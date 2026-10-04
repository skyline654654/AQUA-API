// 本文件是「真实模型」的对弈链路验证（默认跳过，需显式开启）。
//
// 意图（Why）：
//
//	其余测试都用桩模型，它们能证明"链路正确"，但证明不了这个功能真正想回答的问题：
//	**模型能不能从一张渲染出来的棋盘图里稳定读出局面并给出合法着法。**
//	这正是整个多模态演示存在的理由，也只能用真实模型来回答。
//
//	因此本测试刻意分成两层结论，避免把两类完全不同的问题混为一谈：
//	  · 【管道层】请求是否真的发出、响应是否被正确解析 —— 失败一律 hard fail，
//	    因为那是我们代码的问题；
//	  · 【模型层】模型给出的着法是否合法 —— 只记录、不 hard fail，
//	    因为模型走错是"观测结果"而不是缺陷（提示词已尽力，剩下的是模型能力）。
//	    唯一例外：连续多手一个合法着法都给不出，那说明这个模型根本不具备
//	    看图下棋的能力，测试会明确失败并打印它的原始输出。
//
//	为什么默认跳过：它会发起真实的模型调用（花钱、依赖网络与上游可用性），
//	不适合放进日常 `go test ./...`。用环境变量显式开启：
//
//	AQUA_LIVE_VISION=1 \
//	AQUA_LIVE_BASE_URL=http://127.0.0.1:18787/zen/go/v1 \
//	AQUA_LIVE_API_KEY=sk-xxx \
//	AQUA_LIVE_MODEL=deepseek-v4-flash-vision-exp \
//	go test -run TestLiveVisionGame -v ./internal/server/
//
// 流转（Flow）：
//
//	建库迁移 → 建渠道（指向真实上游）→ 建 relay 与 server
//	  → 建局（AI 对 AI）→ 逐手 step（每次都是一次真实的多模态调用）
//
// 扩展（Extend）：
//
//	想验证其他棋种：改 liveGameKind。想验证人机对弈：在 step 时带上 move 字段。
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/xiaosu4610/aqua-api/internal/crypto"
	"github.com/xiaosu4610/aqua-api/internal/model"
	"github.com/xiaosu4610/aqua-api/internal/relay"
	"github.com/xiaosu4610/aqua-api/internal/store"
)

// liveGameKind 是本测试使用的棋种。
//
// 选五子棋：规则最简单、坐标格式最直观，能把"看图能力"从"规则理解难度"里
// 剥离出来。若连五子棋都给不出合法着法，换围棋只会更差、无法定位原因。
const liveGameKind = "gomoku"

// liveGameTurns 是本次验证实际推进的手数（每手一次真实调用，控制费用）。
const liveGameTurns = 2

func TestLiveVisionGame(t *testing.T) {
	if os.Getenv("AQUA_LIVE_VISION") != "1" {
		t.Skip("未设置 AQUA_LIVE_VISION=1，跳过真实模型验证（会产生真实调用）")
	}
	baseURL := os.Getenv("AQUA_LIVE_BASE_URL")
	apiKey := os.Getenv("AQUA_LIVE_API_KEY")
	modelName := os.Getenv("AQUA_LIVE_MODEL")
	if baseURL == "" || apiKey == "" || modelName == "" {
		t.Fatal("需要 AQUA_LIVE_BASE_URL / AQUA_LIVE_API_KEY / AQUA_LIVE_MODEL")
	}

	gin.SetMode(gin.TestMode)
	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "live.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	cipher, err := crypto.New("live-vision-test-key-material")
	if err != nil {
		t.Fatalf("构造加密器失败: %v", err)
	}
	ctx := context.Background()
	channels := store.NewChannelRepository(st.DB(), cipher)
	// 建一个真实渠道：分组必须是 relay 的默认分组（default），否则选不到。
	ch := &model.Channel{
		Name:    "live-vision",
		Type:    1,
		BaseURL: baseURL,
		APIKey:  apiKey,
		Models:  []string{modelName},
		Group:   "default",
		Groups:  []string{"default"},
		Status:  model.ChannelStatusEnabled,
		Weight:  1,
	}
	if err := channels.Create(ctx, ch); err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}

	users := store.NewUserRepository(st.DB())
	user := &model.User{
		Username: "live-vision", Email: "live@example.test",
		PasswordHash: "test-hash", Quota: model.QuotaUnlimited,
		Status: model.UserStatusEnabled, Role: model.UserRoleUser,
	}
	if err := users.Create(ctx, user); err != nil {
		t.Fatalf("创建测试用户失败: %v", err)
	}

	engine := gin.New()
	s := &Server{
		engine: engine,
		deps: Deps{
			Channels:    channels,
			Users:       users,
			UsageLogs:   store.NewUsageLogRepository(st.DB(), st.Dialect()),
			GameMatches: store.NewGameMatchRepository(st.DB()),
			Relay:       relay.New(channels, relay.Options{}),
			// 每手最多试 2 次：既能看到重试路径，也把真实调用次数控制在 4 次以内。
			GameArenaAttempts: 2,
		},
	}
	auth := func(c *gin.Context) {
		c.Set(testUserCtxKey, user)
		c.Next()
	}
	engine.POST("/api/user/games", auth, s.handleCreateGame)
	engine.POST("/api/user/games/:id/step", auth, s.handleStepGame)
	engine.GET("/api/user/games/:id", auth, s.handleGetGame)

	// 1) 建局：AI 对 AI，双方用同一个模型（本测试只关心"能不能下"）
	rec := doJSON(t, engine, http.MethodPost, "/api/user/games", map[string]any{
		"kind": liveGameKind, "mode": "ai_vs_ai",
		"black_model": modelName, "white_model": modelName,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("建局失败：HTTP %d %s", rec.Code, rec.Body.String())
	}
	var created gameMatchDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("解析建局响应失败: %v", err)
	}
	t.Logf("已建对局 #%d（%s，模型 %s）", created.ID, liveGameKind, modelName)

	// 2) 逐手推进：每一次 step 都是一次真实的多模态调用
	legalMoves := 0
	callFailures := 0
	for turn := 1; turn <= liveGameTurns; turn++ {
		rec := doJSON(t, engine, http.MethodPost,
			"/api/user/games/"+itoa64(created.ID)+"/step", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 手请求失败：HTTP %d %s", turn, rec.Code, rec.Body.String())
		}
		var stepped gameStepResultForTest
		if err := json.Unmarshal(rec.Body.Bytes(), &stepped); err != nil {
			t.Fatalf("解析第 %d 手响应失败: %v", turn, err)
		}
		switch {
		case stepped.CallFailed:
			// 管道层失败：请求根本没到达模型，或用例无法解析它的响应 —— 这是我们的问题
			callFailures++
			t.Errorf("第 %d 手调用失败（管道层问题，需修）：%s", turn, stepped.Error)
		case len(stepped.Moves) > 0:
			mv := stepped.Moves[0]
			legalMoves++
			t.Logf("第 %d 手：%s %s（尝试 %d 次）｜模型原话：%s",
				turn, mv.ColorText, mv.Notation, mv.Attempts, truncateForLog(mv.Raw, 160))
		default:
			// 模型层结果：给不出合法着法。记录而不判失败 —— 这是有价值的观测。
			t.Logf("第 %d 手：模型未能给出合法着法（%s）｜原话：%s",
				turn, stepped.Error, truncateForLog(rawOfAbort(stepped), 200))
		}
		if stepped.Over || stepped.Aborted {
			t.Logf("对局在第 %d 手结束：%s", turn, stepped.Match.StatusText)
			break
		}
	}

	// 3) 汇总：管道必须通（否则是我们代码的问题），模型至少要给出一手合法着法
	if callFailures > 0 {
		t.Fatalf("有 %d 手在管道层失败 —— 说明转发或解析链路有问题", callFailures)
	}
	if legalMoves == 0 {
		t.Fatalf("真实模型在 %d 手里一个合法着法都没给出 —— 该模型可能不具备看图下棋能力"+
			"（提示词与解析器本身已由桩模型测试覆盖，此处是模型能力问题）", liveGameTurns)
	}
	t.Logf("✓ 管道层通过：%d/%d 手成功产出合法着法", legalMoves, liveGameTurns)

	// 4) 棋谱必须落库（含模型原始输出，供复盘"模型错在哪"）
	rec = doJSON(t, engine, http.MethodGet, "/api/user/games/"+itoa64(created.ID), nil)
	var detail gameDetailDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("解析详情失败: %v", err)
	}
	if len(detail.Moves) != legalMoves {
		t.Errorf("棋谱手数 = %d，期望 %d（与成功手数一致）", len(detail.Moves), legalMoves)
	}
	for _, m := range detail.Moves {
		if m.Raw == "" {
			t.Errorf("第 %d 手缺少模型原始输出（复盘时无法区分'模型走错'与'我们解析错'）", m.Seq)
		}
	}
}

// truncateForLog 截断文本用于测试日志（按 rune 截断，避免切碎多字节字符）。
func truncateForLog(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// rawOfAbort 从中止响应里尽量取出模型原话（便于观察它到底说了什么）。
func rawOfAbort(stepped gameStepResultForTest) string {
	if len(stepped.Moves) > 0 {
		return stepped.Moves[0].Raw
	}
	return stepped.Error
}
