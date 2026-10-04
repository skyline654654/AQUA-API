// 本文件是排行榜接口的端到端测试。
//
// 意图（Why）：
//
//	本次修复的是「两榜口径重叠」——同一笔流量可能同时落入两个榜，
//	导致两个榜都无法解释。这类问题的正确性无法靠"看代码"确认，
//	必须用一组能算清总数的数据把契约钉住：
//
//	  两榜请求数之和 == 全站总请求数（且各自内部无重复用户）
//
//	只要这条成立，就不可能出现"同一笔请求被算两次"。
//	断言用"相加等于总量"而不是"两榜都非空"——后者完全无法排除重复计数。
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xiaosu4610/aqua-api/internal/model"
	"github.com/xiaosu4610/aqua-api/internal/store"
)

// leaderboardTestEnv 搭一个只挂排行榜路由的环境，并返回记账用的仓储。
func leaderboardTestEnv(t *testing.T) (http.Handler, model.UsageLogRepository, *model.User) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "lb_test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	repo := store.NewUsageLogRepository(st.DB(), st.Dialect())
	me := &model.User{ID: 1, Username: "me"}

	engine := gin.New()
	s := &Server{engine: engine, deps: Deps{UsageLogs: repo}}
	engine.GET("/api/user/leaderboard", func(c *gin.Context) {
		// 绕过鉴权中间件，直接注入登录用户
		c.Set(testUserCtxKey, me)
		c.Next()
	}, s.handleLeaderboard)
	return engine, repo, me
}

// seedLog 写入一条用量日志。
func seedLog(t *testing.T, repo model.UsageLogRepository, userID uint64, tokens int, billingFree bool, at time.Time) {
	t.Helper()
	entry := &model.UsageLog{
		UserID:      userID,
		ChannelID:   1,
		Model:       "gpt-4o",
		TotalTokens: tokens,
		StatusCode:  200,
		LatencyMS:   100,
		BillingFree: billingFree,
		CreatedAt:   at,
	}
	if err := repo.Create(context.Background(), entry); err != nil {
		t.Fatalf("写入日志失败: %v", err)
	}
}

// fetchLeaderboard 取一次榜单。
func fetchLeaderboard(t *testing.T, h http.Handler, days int) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		"/api/user/leaderboard?days="+itoaInt(days), bytes.NewReader(nil))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("取榜单失败：HTTP %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析榜单响应失败: %v", err)
	}
	return out
}

// sectionRows 从响应里取出某个榜的行集合。
func sectionRows(t *testing.T, payload map[string]any, key string) []map[string]any {
	t.Helper()
	section, ok := payload[key].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 %q 段（实际字段：%v）", key, keysOf(payload))
	}
	raw, _ := section["items"].([]any)
	rows := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			rows = append(rows, m)
		}
	}
	return rows
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func itoaInt(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [12]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func numOf(t *testing.T, m map[string]any, key string) int64 {
	t.Helper()
	v, ok := m[key].(float64)
	if !ok {
		return 0
	}
	return int64(v)
}

// TestLeaderboardAPI_两榜互斥且与全站一致 是本文件的核心契约测试。
func TestLeaderboardAPI_两榜互斥且与全站一致(t *testing.T) {
	h, repo, _ := leaderboardTestEnv(t)
	now := time.Now()

	// 构造三类用户，覆盖所有情形：
	//   A：只用免费模型（3 笔）
	//   B：只用计费模型（2 笔）
	//   C：两种都用（计费 4 笔 + 免费 1 笔）—— 旧口径下它的两种流量会被混在一起
	seedLog(t, repo, 100, 10, true, now.Add(-1*time.Hour))
	seedLog(t, repo, 100, 10, true, now.Add(-2*time.Hour))
	seedLog(t, repo, 100, 10, true, now.Add(-3*time.Hour))
	seedLog(t, repo, 200, 20, false, now.Add(-1*time.Hour))
	seedLog(t, repo, 200, 20, false, now.Add(-2*time.Hour))
	for i := 0; i < 4; i++ {
		seedLog(t, repo, 300, 30, false, now.Add(-time.Duration(i+1)*time.Hour))
	}
	seedLog(t, repo, 300, 30, true, now.Add(-10*time.Hour))

	payload := fetchLeaderboard(t, h, 30)
	billed := sectionRows(t, payload, "billed")
	free := sectionRows(t, payload, "free")

	// 1) billed 段必须存在（旧实现叫 paid，改名后若前端/测试没跟上会静默取到空）
	if _, ok := payload["billed"]; !ok {
		t.Fatalf("响应应含 billed 段，实际字段：%v", keysOf(payload))
	}

	// 2) 两榜请求数互斥且等于全站总量
	split, _ := payload["split"].(map[string]any)
	billedTotal := numOf(t, split, "billed_requests")
	freeTotal := numOf(t, split, "free_requests")
	totals, _ := payload["totals"].(map[string]any)
	grandTotal := numOf(t, totals, "requests")

	if billedTotal != 6 {
		t.Errorf("计费请求数 = %d，期望 6（B 的 2 笔 + C 的 4 笔）", billedTotal)
	}
	if freeTotal != 4 {
		t.Errorf("免费请求数 = %d，期望 4（A 的 3 笔 + C 的 1 笔）", freeTotal)
	}
	if billedTotal+freeTotal != grandTotal {
		t.Errorf("两榜之和 = %d，但全站总请求 = %d —— 存在重复计数或漏计",
			billedTotal+freeTotal, grandTotal)
	}

	// 3) 榜内不得出现重复用户（用 user_id 去重检查）
	assertNoDuplicateUsers(t, billed, "计费榜")
	assertNoDuplicateUsers(t, free, "免费榜")

	// 4) A 只应在免费榜、B 只应在计费榜
	if containsUser(billed, 100) {
		t.Error("只用免费模型的用户不应出现在计费榜")
	}
	if !containsUser(free, 100) {
		t.Error("只用免费模型的用户应出现在免费榜")
	}
	if !containsUser(billed, 200) {
		t.Error("只用计费模型的用户应出现在计费榜")
	}
	if containsUser(free, 200) {
		t.Error("只用计费模型的用户不应出现在免费榜")
	}

	// 5) C 两种都用 → 两榜各占一行，且数字互斥
	billedC := findUser(billed, 300)
	freeC := findUser(free, 300)
	if billedC == nil {
		t.Fatal("两种都用的用户应出现在计费榜")
	}
	if freeC == nil {
		t.Fatal("两种都用的用户应出现在免费榜")
	}
	br, fr := numOf(t, billedC, "requests"), numOf(t, freeC, "requests")
	if br != 4 {
		t.Errorf("C 在计费榜的请求数 = %d，期望 4（只含计费那部分）", br)
	}
	if fr != 1 {
		t.Errorf("C 在免费榜的请求数 = %d，期望 1（只含免费那部分）", fr)
	}
	if br+fr != 5 {
		t.Errorf("C 在两榜的请求数之和 = %d，期望 5（等于它的实际总请求数）", br+fr)
	}
	// token 同理必须互斥：4×30 + 1×30 = 150
	if bt, ft := numOf(t, billedC, "tokens"), numOf(t, freeC, "tokens"); bt+ft != 150 {
		t.Errorf("C 在两榜的 token 之和 = %d，期望 150", bt+ft)
	}
}

// TestLeaderboardAPI_只有一种流量时另一榜为空 验证空榜不会伪造出条目。
func TestLeaderboardAPI_只有一种流量时另一榜为空(t *testing.T) {
	h, repo, _ := leaderboardTestEnv(t)
	now := time.Now()
	seedLog(t, repo, 500, 100, false, now.Add(-time.Hour))

	payload := fetchLeaderboard(t, h, 30)
	if rows := sectionRows(t, payload, "billed"); len(rows) != 1 {
		t.Errorf("计费榜应有 1 条，实际 %d 条", len(rows))
	}
	if rows := sectionRows(t, payload, "free"); len(rows) != 0 {
		t.Errorf("无免费流量时免费榜应为空，实际 %d 条", len(rows))
	}
	// 全站总量仍应等于计费榜的量
	totals, _ := payload["totals"].(map[string]any)
	if got := numOf(t, totals, "requests"); got != 1 {
		t.Errorf("全站总请求 = %d，期望 1", got)
	}
}

// TestLeaderboardAPI_我的名次按所在榜计算 验证 my_rank 用对了榜。
func TestLeaderboardAPI_我的名次按所在榜计算(t *testing.T) {
	h, repo, me := leaderboardTestEnv(t)
	now := time.Now()
	// 我在免费榜有量，在计费榜无量
	seedLog(t, repo, me.ID, 10, true, now.Add(-time.Hour))
	seedLog(t, repo, 900, 999, true, now.Add(-time.Hour))

	payload := fetchLeaderboard(t, h, 30)
	freeSection, _ := payload["free"].(map[string]any)
	billedSection, _ := payload["billed"].(map[string]any)

	if rank := numOf(t, freeSection, "my_rank"); rank != 2 {
		t.Errorf("免费榜我的名次 = %d，期望 2（用量少排第二）", rank)
	}
	// 我没在计费榜有量 → 名次应为 0
	if rank := numOf(t, billedSection, "my_rank"); rank != 0 {
		t.Errorf("计费榜我的名次 = %d，期望 0（未上榜）", rank)
	}
}

// ── 断言辅助 ────────────────────────────────────────────────

func assertNoDuplicateUsers(t *testing.T, rows []map[string]any, label string) {
	t.Helper()
	seen := make(map[float64]bool)
	for _, row := range rows {
		id, _ := row["user_id"].(float64)
		if seen[id] {
			t.Errorf("%s出现重复用户 user_id=%v（榜内同一用户只能有一行）", label, id)
		}
		seen[id] = true
	}
}

func containsUser(rows []map[string]any, userID uint64) bool {
	return findUser(rows, userID) != nil
}

func findUser(rows []map[string]any, userID uint64) map[string]any {
	for _, row := range rows {
		if id, ok := row["user_id"].(float64); ok && uint64(id) == userID {
			return row
		}
	}
	return nil
}
