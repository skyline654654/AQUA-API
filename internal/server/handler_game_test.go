// 本文件是对弈 HTTP 全链路的集成测试。
//
// 意图（Why）：
//
//	单测验证了规则、解析、驱动，但"接口能不能真的用"取决于另一层东西：
//	路由是否注册、鉴权上下文是否正确传递、DTO 字段是否齐全、
//	棋盘图片接口是否真的返回 PNG、以及最要紧的——**额度闸门是否生效**。
//	这些只有把真实的 gin 路由 + 真实 SQLite + 桩模型装配起来跑一遍才能确认。
//
//	桩模型（stubGameCaller）替代真实模型：AI 那一步之外的链路与模型无关，
//	注入桩之后整条链路可在进程内完整验证，且不花钱、不依赖上游可用性。
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/xiaosu4610/aqua-api/internal/model"
	"github.com/xiaosu4610/aqua-api/internal/store"
)

// 登录用户在上下文中的键名（与 SessionAuth 写入的一致）。
const testUserCtxKey = "aqua.context.user"

// stubGameCaller 是一个脚本化的模型调用器。
type stubGameCaller struct {
	replies []string
	calls   int
	// gotImages 记录每次调用附带的图片字节数（用于断言"确实发了图"）
	gotImages []int
	// failWith 非空时模拟"调用失败"
	failWith error
}

func (s *stubGameCaller) CallVision(_ context.Context, _, _, _ string, png []byte) (string, error) {
	if s.failWith != nil {
		return "", s.failWith
	}
	s.gotImages = append(s.gotImages, len(png))
	idx := s.calls
	s.calls++
	if idx < len(s.replies) {
		return s.replies[idx], nil
	}
	return "MOVE: RESIGN", nil
}

// newGameTestEnv 搭好一套可用的对弈测试环境。
func newGameTestEnv(t *testing.T, caller *stubGameCaller, userQuota int64) (http.Handler, uint64) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "game_test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	users := store.NewUserRepository(st.DB())
	user := &model.User{
		Username: "game-test-user",
		Email:    "game@example.test",
		// 仓储会校验口令哈希非空（防直接构造用户对象绕过服务层），
		// 对弈链路用不到口令，给一个占位值即可。
		PasswordHash: "test-hash",
		Quota:        userQuota,
		Status:       model.UserStatusEnabled,
		Role:         model.UserRoleUser,
	}
	if err := users.Create(context.Background(), user); err != nil {
		t.Fatalf("创建测试用户失败: %v", err)
	}

	engine := gin.New()
	s := &Server{
		engine: engine,
		deps: Deps{
			Users:       users,
			GameMatches: store.NewGameMatchRepository(st.DB()),
			GameCaller:  caller,
			// 重试固定 2 次，让"重试用尽"的用例跑得快
			GameArenaAttempts: 2,
		},
	}
	// 模拟 SessionAuth：注入登录用户后进入真实处理器
	auth := func(c *gin.Context) {
		c.Set(testUserCtxKey, user)
		c.Next()
	}
	engine.POST("/api/user/games", auth, s.handleCreateGame)
	engine.GET("/api/user/games", auth, s.handleListGames)
	engine.GET("/api/user/games/:id", auth, s.handleGetGame)
	engine.DELETE("/api/user/games/:id", auth, s.handleDeleteGame)
	engine.POST("/api/user/games/:id/step", auth, s.handleStepGame)
	engine.GET("/api/user/games/:id/board.png", auth, s.handleGameBoard)
	lastGameTestServer = s
	return engine, user.ID
}

// doJSON 发一个 JSON 请求并返回响应。
func doJSON(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// 测试用：无限制额度的用户（不触发额度闸门）。
const unlimitedQuota = model.QuotaUnlimited

func TestGameAPI_HumanVsAI_FullFlow(t *testing.T) {
	// 桩模型执白，人类执黑先走。
	caller := &stubGameCaller{replies: []string{"我要占角。MOVE: J10"}}
	h, _ := newGameTestEnv(t, caller, unlimitedQuota)

	// 1) 创建人机对局（人类执黑）
	rec := doJSON(t, h, http.MethodPost, "/api/user/games", map[string]any{
		"kind": "gomoku", "mode": "human_vs_ai",
		"white_model": "stub-model", "human_color": 1,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建对局失败：HTTP %d %s", rec.Code, rec.Body.String())
	}
	var created gameMatchDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("解析创建响应失败: %v", err)
	}
	if created.Mode != "human_vs_ai" || created.HumanColor != 1 {
		t.Fatalf("创建结果不对：%+v", created)
	}
	if !created.AwaitingHuman {
		t.Error("人类执黑且轮黑先走，应处于等待人类落子")
	}

	// 2) 人类落子 H8（不带 MOVE 前缀也应能识别）
	rec = doJSON(t, h, http.MethodPost,
		"/api/user/games/"+itoa64(created.ID)+"/step", map[string]any{"move": "h8"})
	if rec.Code != http.StatusOK {
		t.Fatalf("人类落子失败：HTTP %d %s", rec.Code, rec.Body.String())
	}
	var stepped gameStepResultForTest
	if err := json.Unmarshal(rec.Body.Bytes(), &stepped); err != nil {
		t.Fatalf("解析落子响应失败: %v", err)
	}
	if stepped.Match.MoveCount != 1 {
		t.Errorf("手数 = %d，期望 1", stepped.Match.MoveCount)
	}
	// 落子必须真的写到盘上
	if got := stepped.Match.Position.Cells[7*15+7]; got != 1 {
		t.Errorf("H8 应有黑子，实际 color=%d", got)
	}
	if stepped.Match.AwaitingHuman {
		t.Error("人类走完后应轮到 AI，不再等待人类")
	}

	// 3) 轮到 AI：再 step 一次，后端去问模型（桩返回 J10）
	rec = doJSON(t, h, http.MethodPost,
		"/api/user/games/"+itoa64(created.ID)+"/step", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("AI 走子失败：HTTP %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &stepped); err != nil {
		t.Fatalf("解析 AI 响应失败: %v", err)
	}
	if stepped.Match.MoveCount != 2 {
		t.Errorf("手数 = %d，期望 2", stepped.Match.MoveCount)
	}
	// J10 → X=8, Y=9
	if got := stepped.Match.Position.Cells[9*15+8]; got != 2 {
		t.Errorf("J10 应有白子，实际 color=%d", got)
	}
	// 关键：必须真的把棋盘图片发给了模型（这是"多模态"的实质）
	if len(caller.gotImages) != 1 || caller.gotImages[0] == 0 {
		t.Errorf("调用模型时应附带棋盘图片，实际 %v", caller.gotImages)
	}

	// 4) 读取对局详情：棋谱应有两手
	rec = doJSON(t, h, http.MethodGet, "/api/user/games/"+itoa64(created.ID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("读取对局失败：HTTP %d", rec.Code)
	}
	var detail gameDetailDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("解析详情失败: %v", err)
	}
	if len(detail.Moves) != 2 {
		t.Fatalf("棋谱应有 2 手，实际 %d", len(detail.Moves))
	}
	if detail.Moves[0].Notation == "" || detail.Moves[1].Notation == "" {
		t.Error("棋谱写法不应为空")
	}

	// 5) 棋盘图片接口必须返回真实 PNG
	rec = doJSON(t, h, http.MethodGet, "/api/user/games/"+itoa64(created.ID)+"/board.png", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("取棋盘图失败：HTTP %d", rec.Code)
	}
	body := rec.Body.Bytes()
	if len(body) < 8 || string(body[1:4]) != "PNG" {
		t.Errorf("返回的不是 PNG（前 8 字节 %v）", body[:min(8, len(body))])
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "image/png") {
		t.Errorf("Content-Type = %q，期望 image/png", ct)
	}
}

func TestGameAPI_AIVsAI_BothSidesCallModel(t *testing.T) {
	caller := &stubGameCaller{replies: []string{"MOVE: H8", "MOVE: J10"}}
	h, _ := newGameTestEnv(t, caller, unlimitedQuota)

	rec := doJSON(t, h, http.MethodPost, "/api/user/games", map[string]any{
		"kind": "gomoku", "mode": "ai_vs_ai",
		"black_model": "model-a", "white_model": "model-b",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建失败：HTTP %d %s", rec.Code, rec.Body.String())
	}
	var created gameMatchDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// 连走两手：黑、白各一次，每次都应调用模型
	for i := 0; i < 2; i++ {
		rec = doJSON(t, h, http.MethodPost, "/api/user/games/"+itoa64(created.ID)+"/step", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次推进失败：HTTP %d %s", i+1, rec.Code, rec.Body.String())
		}
	}
	if caller.calls != 2 {
		t.Errorf("AI 对 AI 走两手应调用模型 2 次，实际 %d", caller.calls)
	}
}

func TestGameAPI_RejectsHumanMoveOnAITurn(t *testing.T) {
	// AI 回合时不该接受人类落子——否则用户能替模型下棋，演示就失去意义。
	caller := &stubGameCaller{replies: []string{"MOVE: H8"}}
	h, _ := newGameTestEnv(t, caller, unlimitedQuota)

	rec := doJSON(t, h, http.MethodPost, "/api/user/games", map[string]any{
		"kind": "gomoku", "mode": "ai_vs_ai",
		"black_model": "a", "white_model": "b",
	})
	var created gameMatchDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// 带 move 请求推进：AI 回合下这个 move 应被忽略（而不是替 AI 落子）
	rec = doJSON(t, h, http.MethodPost,
		"/api/user/games/"+itoa64(created.ID)+"/step", map[string]any{"move": "A1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d", rec.Code)
	}
	var stepped gameStepResultForTest
	_ = json.Unmarshal(rec.Body.Bytes(), &stepped)
	// 实际落子应是模型给的 H8，而不是请求里的 A1
	if got := stepped.Match.Position.Cells[7*15+7]; got != 1 {
		t.Errorf("应落在模型给出的 H8，实际 H8 格 color=%d", got)
	}
	if got := stepped.Match.Position.Cells[0]; got != 0 {
		t.Error("AI 回合不应采纳请求里携带的人类着法 A1")
	}
}

func TestGameAPI_QuotaGateBlocksWhenExhausted(t *testing.T) {
	// 额度为 0 时必须拦住——否则用户能靠"开一局棋"绕过额度墙白刷模型。
	// 这是本功能最需要守住的一条：它 Commit 了一条不经过鉴权中间件的调用路径。
	caller := &stubGameCaller{replies: []string{"MOVE: H8"}}
	h, _ := newGameTestEnv(t, caller, 0) // 总额度 0，已用 0 → 可用 0

	rec := doJSON(t, h, http.MethodPost, "/api/user/games", map[string]any{
		"kind": "gomoku", "mode": "ai_vs_ai",
		"black_model": "a", "white_model": "b",
	})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("零额度建局应被拒（429），实际 HTTP %d %s", rec.Code, rec.Body.String())
	}
	if caller.calls != 0 {
		t.Error("被额度拦住时不应调用任何模型")
	}
}

func TestGameAPI_QuotaGateAlsoGuardsEachStep(t *testing.T) {
	// 建局时有额度、中途额度耗尽的情况也要拦住。
	caller := &stubGameCaller{replies: []string{"MOVE: H8", "MOVE: J10"}}
	h, _ := newGameTestEnv(t, caller, unlimitedQuota)

	rec := doJSON(t, h, http.MethodPost, "/api/user/games", map[string]any{
		"kind": "gomoku", "mode": "ai_vs_ai",
		"black_model": "a", "white_model": "b",
	})
	var created gameMatchDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// 第一次推进正常
	rec = doJSON(t, h, http.MethodPost, "/api/user/games/"+itoa64(created.ID)+"/step", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("首次推进应成功，实际 %d", rec.Code)
	}

	// 把额度改成 0（模拟用户额度被别的请求耗尽），再推进应被拦
	srv := testServerFromHandler(t, h)
	if srv == nil {
		t.Skip("无法取回 Server 实例，跳过本用例")
	}
	if err := setUserQuota(t, srv, 0); err != nil {
		t.Skipf("调整额度失败，跳过：%v", err)
	}
	before := caller.calls
	rec = doJSON(t, h, http.MethodPost, "/api/user/games/"+itoa64(created.ID)+"/step", nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("中途额度耗尽应被拦（429），实际 HTTP %d", rec.Code)
	}
	if caller.calls != before {
		t.Error("被额度拦住时不应继续调用模型")
	}
}

func TestGameAPI_AbortsWhenModelCannotProduceLegalMove(t *testing.T) {
	// 模型连续给非法着法 → 对局必须【中止】并如实说明原因，
	// 而不是判它认输（那是与事实相反的结论），也不能一直挂着。
	caller := &stubGameCaller{replies: []string{
		"我随便下 MOVE: Z99", "还是 MOVE: Z99", "又是 MOVE: Z99",
	}}
	h, _ := newGameTestEnv(t, caller, unlimitedQuota)

	rec := doJSON(t, h, http.MethodPost, "/api/user/games", map[string]any{
		"kind": "gomoku", "mode": "ai_vs_ai",
		"black_model": "a", "white_model": "b",
	})
	var created gameMatchDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	rec = doJSON(t, h, http.MethodPost, "/api/user/games/"+itoa64(created.ID)+"/step", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d %s", rec.Code, rec.Body.String())
	}
	var stepped gameStepResultForTest
	_ = json.Unmarshal(rec.Body.Bytes(), &stepped)
	if !stepped.Aborted {
		t.Error("应标记为中止")
	}
	if stepped.Match.Status != model.GameStatusAborted {
		t.Errorf("状态应为 aborted，实际 %q", stepped.Match.Status)
	}
	if stepped.Match.Winner != "" {
		t.Errorf("中止时不应有胜者，实际 %q（把中止当认输是错的）", stepped.Match.Winner)
	}
	if !strings.Contains(stepped.Match.ErrorText, "未能给出合法着法") {
		t.Errorf("中止原因应说明是给不出合法着法，实际 %q", stepped.Match.ErrorText)
	}
}

func TestGameAPI_CallFailureAbortsButIsNotAModelLoss(t *testing.T) {
	// 调用失败（网络/上游 5xx）要中止对局，且【不能】写成模型认输。
	caller := &stubGameCaller{failWith: errUpstreamDown}
	h, _ := newGameTestEnv(t, caller, unlimitedQuota)

	rec := doJSON(t, h, http.MethodPost, "/api/user/games", map[string]any{
		"kind": "gomoku", "mode": "ai_vs_ai",
		"black_model": "a", "white_model": "b",
	})
	var created gameMatchDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	rec = doJSON(t, h, http.MethodPost, "/api/user/games/"+itoa64(created.ID)+"/step", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d", rec.Code)
	}
	var stepped gameStepResultForTest
	_ = json.Unmarshal(rec.Body.Bytes(), &stepped)
	if !stepped.Aborted {
		t.Error("调用失败应中止对局")
	}
	if stepped.Match.Status == model.GameStatusBlackWin || stepped.Match.Status == model.GameStatusWhiteWin {
		t.Errorf("调用失败不应判出胜负，实际 %q", stepped.Match.Status)
	}
	if !strings.Contains(stepped.Match.ErrorText, "调用失败") {
		t.Errorf("中止原因应说明是调用失败，实际 %q", stepped.Match.ErrorText)
	}
}

func TestGameAPI_CreateValidatesModeAndSeats(t *testing.T) {
	h, _ := newGameTestEnv(t, &stubGameCaller{}, unlimitedQuota)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"AI对AI缺模型", map[string]any{"kind": "gomoku", "mode": "ai_vs_ai", "black_model": "a"}},
		{"AI对AI却指定人类执子", map[string]any{
			"kind": "gomoku", "mode": "ai_vs_ai", "black_model": "a", "white_model": "b", "human_color": 1}},
		{"人机未指定人类执子", map[string]any{
			"kind": "gomoku", "mode": "human_vs_ai", "white_model": "b"}},
		{"人机人类执黑却配了黑方模型", map[string]any{
			"kind": "gomoku", "mode": "human_vs_ai", "human_color": 1, "black_model": "a", "white_model": "b"}},
		{"人机缺AI模型", map[string]any{
			"kind": "gomoku", "mode": "human_vs_ai", "human_color": 1}},
		{"未知棋种", map[string]any{
			"kind": "chess", "mode": "ai_vs_ai", "black_model": "a", "white_model": "b"}},
		{"未知模式", map[string]any{
			"kind": "gomoku", "mode": "x", "black_model": "a", "white_model": "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, h, http.MethodPost, "/api/user/games", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("应返回 400，实际 %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestGameAPI_RejectsIllegalHumanMoveWithReason(t *testing.T) {
	h, _ := newGameTestEnv(t, &stubGameCaller{}, unlimitedQuota)

	rec := doJSON(t, h, http.MethodPost, "/api/user/games", map[string]any{
		"kind": "gomoku", "mode": "human_vs_ai", "white_model": "m", "human_color": 1,
	})
	var created gameMatchDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// 人类给出无法解析的着法
	rec = doJSON(t, h, http.MethodPost,
		"/api/user/games/"+itoa64(created.ID)+"/step", map[string]any{"move": "随便下"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("非法输入应返回 400，实际 %d", rec.Code)
	}

	// 人类给越界坐标
	rec = doJSON(t, h, http.MethodPost,
		"/api/user/games/"+itoa64(created.ID)+"/step", map[string]any{"move": "Z99"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("越界坐标应返回 400，实际 %d", rec.Code)
	}
}

func TestGameAPI_BoardImageForAllThreeKinds(t *testing.T) {
	// 三种棋的棋盘图都必须能渲染出来（渲染路径按棋种分支，容易漏分支）。
	for _, kind := range []string{"gomoku", "go", "xiangqi"} {
		t.Run(kind, func(t *testing.T) {
			caller := &stubGameCaller{}
			h, _ := newGameTestEnv(t, caller, unlimitedQuota)
			rec := doJSON(t, h, http.MethodPost, "/api/user/games", map[string]any{
				"kind": kind, "mode": "ai_vs_ai", "black_model": "a", "white_model": "b",
			})
			if rec.Code != http.StatusCreated {
				t.Fatalf("创建 %s 对局失败：%d %s", kind, rec.Code, rec.Body.String())
			}
			var created gameMatchDTO
			_ = json.Unmarshal(rec.Body.Bytes(), &created)

			rec = doJSON(t, h, http.MethodGet,
				"/api/user/games/"+itoa64(created.ID)+"/board.png", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s 棋盘图渲染失败：HTTP %d", kind, rec.Code)
			}
			if body := rec.Body.Bytes(); len(body) < 1000 {
				t.Errorf("%s 棋盘图过小（%d 字节），可能没画出内容", kind, len(body))
			}
		})
	}
}

func TestGameAPI_DeleteRemovesMatchAndMoves(t *testing.T) {
	caller := &stubGameCaller{replies: []string{"MOVE: H8"}}
	h, _ := newGameTestEnv(t, caller, unlimitedQuota)

	rec := doJSON(t, h, http.MethodPost, "/api/user/games", map[string]any{
		"kind": "gomoku", "mode": "ai_vs_ai", "black_model": "a", "white_model": "b",
	})
	var created gameMatchDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	_ = doJSON(t, h, http.MethodPost, "/api/user/games/"+itoa64(created.ID)+"/step", nil)

	rec = doJSON(t, h, http.MethodDelete, "/api/user/games/"+itoa64(created.ID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("删除失败：HTTP %d", rec.Code)
	}
	// 删除后应查不到
	rec = doJSON(t, h, http.MethodGet, "/api/user/games/"+itoa64(created.ID), nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("删除后读取应返回 404，实际 %d", rec.Code)
	}
	// 列表里也不该再有
	rec = doJSON(t, h, http.MethodGet, "/api/user/games", nil)
	var list struct {
		Matches []gameMatchDTO `json:"matches"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Matches) != 0 {
		t.Errorf("删除后列表应为空，实际 %d 条", len(list.Matches))
	}
}

// ── 测试辅助 ──────────────────────────────────────────────────

// gameStepResultForTest 是推进接口响应的测试用结构。
type gameStepResultForTest struct {
	Match   gameMatchDTO  `json:"match"`
	Moves   []gameMoveDTO `json:"moves"`
	Status  string        `json:"status"`
	Over    bool          `json:"over"`
	Aborted bool          `json:"aborted"`
	Error   string        `json:"error"`
	// CallFailed 表示模型【调用】失败（网络/额度/上游 5xx），
	// 与"模型给不出合法着法"是两类完全不同的失败，必须分开断言。
	CallFailed bool `json:"call_failed"`
}

var errUpstreamDown = &testError{"上游 503 Service Unavailable"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

// itoa64 把 ID 转成路径片段。
func itoa64(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// testServerFromHandler 尝试从已构造的 handler 取回 *Server（用于中途改额度。
//
// 这里用的是 gin.Engine 的反射式取回不可行，因此改为在测试环境里
// 把 Server 指针存进一个包级变量（见 newGameTestEnv）。
func testServerFromHandler(t *testing.T, _ http.Handler) *Server {
	t.Helper()
	return lastGameTestServer
}

// lastGameTestServer 记录最近一次构造的测试 Server（仅测试用）。
var lastGameTestServer *Server

// setUserQuota 直接改测试用户的额度，模拟"额度被别处耗尽"。
func setUserQuota(t *testing.T, s *Server, quota int64) error {
	t.Helper()
	if s == nil || s.deps.Users == nil {
		return &testError{"缺少用户仓储"}
	}
	u, err := s.deps.Users.GetByID(context.Background(), 1)
	if err != nil {
		return err
	}
	u.Quota = quota
	return s.deps.Users.Update(context.Background(), u)
}
