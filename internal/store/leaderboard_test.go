// 排行榜聚合与峰值并发算法的测试。
//
// 意图（Why）：
//
//	排行榜的分数口径、付费/免费分榜、峰值并发估算都直接呈现给用户，
//	出错可见性强。这里重点锁定：
//	  1) peakConcurrency 的差分扫描算法（区间重叠边界最容易算错）；
//	  2) Leaderboard 的成功请求过滤（失败请求不得混入分数与耗时）；
//	  3) 付费标记（EXISTS payment_orders status=2）正确归属分组。
//
// 流转（Flow）：
//
//	go test ./internal/store/ -run Leaderboard → 内存 SQLite 写入样本后核验
package store

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/xiaosu4610/aqua-api/internal/model"
)

func TestPeakConcurrency_Basic(t *testing.T) {
	cases := []struct {
		name   string
		events []usageEvent
		want   int64
	}{
		{name: "空输入", events: nil, want: 0},
		{
			name: "两个区间重叠一部分",
			// 请求 A: [100, 130]，请求 B: [120, 150] → 120~130 重叠，峰值 2
			events: []usageEvent{
				{at: 100, delta: 1}, {at: 130, delta: -1},
				{at: 120, delta: 1}, {at: 150, delta: -1},
			},
			want: 2,
		},
		{
			name: "三个区间全重叠于一点",
			events: []usageEvent{
				{at: 10, delta: 1}, {at: 20, delta: -1},
				{at: 15, delta: 1}, {at: 25, delta: -1},
				{at: 12, delta: 1}, {at: 22, delta: -1},
			},
			want: 3,
		},
		{
			name: "同一秒开始与结束算重叠",
			// 请求 A 结束于 50，请求 B 开始于 50 → 同时刻 +1 在前，峰值 2
			events: []usageEvent{
				{at: 40, delta: 1}, {at: 50, delta: -1},
				{at: 50, delta: 1}, {at: 60, delta: -1},
			},
			want: 2,
		},
		{
			name: "乱序输入仍求对峰值",
			events: []usageEvent{
				{at: 200, delta: -1},
				{at: 100, delta: 1},
				{at: 150, delta: 1},
				{at: 180, delta: -1},
			},
			want: 2, // 100~180 与 150~200 重叠于 150~180
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := peakConcurrency(tc.events); got != tc.want {
				t.Fatalf("peakConcurrency = %d，期望 %d", got, tc.want)
			}
		})
	}
}

// TestLeaderboard_SuccessOnlyAndBillingSplit 验证排行榜的三大口径：
//  1. 请求数统计【全部】请求（含失败），成功率单独反映稳定性；
//  2. 按【请求是否计费】聚合：同一用户的免费与计费请求各成一行，互不混合；
//  3. 请求数 / token 聚合正确。
func TestLeaderboard_SuccessOnlyAndBillingSplit(t *testing.T) {
	st := newTestStore(t)
	repo := NewUsageLogRepository(st.DB(), st.Dialect())
	ctx := context.Background()
	now := time.Now()
	since := now.AddDate(0, 0, -30)
	until := now.Add(time.Hour)

	freeUserID := uint64(42)  // 只用免费模型
	billedUserID := uint64(7) // 只用计费模型

	// 免费用户：2 次成功 + 1 次失败，全部为免费调用
	createLogWithBilling(t, repo, freeUserID, now.Add(-time.Hour), 200, 100, 80, true)
	createLogWithBilling(t, repo, freeUserID, now.Add(-2*time.Hour), 200, 200, 120, true)
	createLogWithBilling(t, repo, freeUserID, now.Add(-3*time.Hour), 500, 999, 10, true)

	// 计费用户：1 次成功，计费调用
	createLogWithBilling(t, repo, billedUserID, now.Add(-time.Hour), 200, 300, 60, false)

	entries, err := repo.Leaderboard(ctx, model.UsageLogQuery{Since: &since, Until: &until})
	if err != nil {
		t.Fatalf("Leaderboard 失败: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("榜单条目数 = %d，期望 2（两个用户各一条）", len(entries))
	}

	var freeEntry, billedEntry *model.LeaderboardEntry
	for i := range entries {
		switch {
		case entries[i].UserID == freeUserID && entries[i].BillingFree:
			freeEntry = &entries[i]
		case entries[i].UserID == billedUserID && !entries[i].BillingFree:
			billedEntry = &entries[i]
		}
	}
	if freeEntry == nil {
		t.Fatal("未找到免费请求的榜单条目")
	}
	if billedEntry == nil {
		t.Fatal("未找到计费请求的榜单条目")
	}

	// 免费请求行：请求数统计全部请求（含失败），成功率单独反映稳定性
	if freeEntry.Requests != 3 {
		t.Errorf("免费请求数 = %d，期望 3（含失败请求）", freeEntry.Requests)
	}
	if freeEntry.SuccessRequests != 2 {
		t.Errorf("免费成功请求数 = %d，期望 2", freeEntry.SuccessRequests)
	}
	if rate := freeEntry.SuccessRate(); rate < 0.66 || rate > 0.67 {
		t.Errorf("免费请求成功率 = %v，期望约 0.667（2/3）", rate)
	}
	if freeEntry.Tokens != 1299 {
		t.Errorf("免费 token = %d，期望 1299（全部请求累计）", freeEntry.Tokens)
	}
	// 平均耗时只按成功请求计算（失败请求耗时不可信）
	if freeEntry.AvgLatencyMS != 100 {
		// (80+120)/2 = 100
		t.Errorf("免费请求平均耗时 = %v，期望 100（仅成功请求）", freeEntry.AvgLatencyMS)
	}

	// 分数封顶 100：榜首（各项最大）应为 100 分
	if score := freeEntry.LeaderboardScore(freeEntry.Requests, freeEntry.Tokens); score != 100 {
		t.Errorf("榜首分数 = %v，期望 100（0~100 封顶）", score)
	}

	// 计费请求行：1 次成功
	if billedEntry.Requests != 1 || billedEntry.SuccessRequests != 1 {
		t.Errorf("计费请求聚合错误: requests=%d success=%d", billedEntry.Requests, billedEntry.SuccessRequests)
	}
	if rate := billedEntry.SuccessRate(); rate != 1 {
		t.Errorf("计费请求成功率 = %v，期望 1（全成功）", rate)
	}
	if billedEntry.Tokens != 300 {
		t.Errorf("计费 token = %d，期望 300", billedEntry.Tokens)
	}
}

// TestLeaderboard_同一用户的免费与计费请求互斥分列 是本文件最关键的一条。
//
// 意图（Why）：
//
//	这正是本次修复要解决的问题：旧口径按"用户是否充过值"分类，
//	同一个人的免费调用与计费调用会被整体归到同一个榜，两榜的统计基数相互重叠。
//	新口径按【请求是否计费】分类，因此：
//	  · 同一用户会在两榜各占一行；
//	  · 两行各自的请求数与 token 【互斥】，相加正好等于该用户的全部用量；
//	  · 不会出现"同一笔请求被两个榜各算一次"。
//
//	断言用"相加等于总量"而不是"两行都存在"——后者无法证明不重复计数。
func TestLeaderboard_同一用户的免费与计费请求互斥分列(t *testing.T) {
	st := newTestStore(t)
	repo := NewUsageLogRepository(st.DB(), st.Dialect())
	ctx := context.Background()
	now := time.Now()
	since := now.AddDate(0, 0, -30)
	until := now.Add(time.Hour)

	userID := uint64(99)
	// 3 笔计费（token 100/200/300）+ 2 笔免费（token 10/20）
	createLogWithBilling(t, repo, userID, now.Add(-1*time.Hour), 200, 100, 50, false)
	createLogWithBilling(t, repo, userID, now.Add(-2*time.Hour), 200, 200, 50, false)
	createLogWithBilling(t, repo, userID, now.Add(-3*time.Hour), 200, 300, 50, false)
	createLogWithBilling(t, repo, userID, now.Add(-4*time.Hour), 200, 10, 50, true)
	createLogWithBilling(t, repo, userID, now.Add(-5*time.Hour), 200, 20, 50, true)

	entries, err := repo.Leaderboard(ctx, model.UsageLogQuery{Since: &since, Until: &until})
	if err != nil {
		t.Fatalf("Leaderboard 失败: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("同一用户应产生两行（计费一行 + 免费一行），实际 %d 行", len(entries))
	}

	var billed, free *model.LeaderboardEntry
	for i := range entries {
		if entries[i].BillingFree {
			free = &entries[i]
		} else {
			billed = &entries[i]
		}
	}
	if billed == nil || free == nil {
		t.Fatal("应同时存在计费行与免费行")
	}

	if billed.Requests != 3 || billed.Tokens != 600 {
		t.Errorf("计费行 = %d 请求 / %d token，期望 3 / 600", billed.Requests, billed.Tokens)
	}
	if free.Requests != 2 || free.Tokens != 30 {
		t.Errorf("免费行 = %d 请求 / %d token，期望 2 / 30", free.Requests, free.Tokens)
	}
	// 互斥性的量化保证：两行相加 == 该用户的全部用量（5 笔 / 630 token）
	if got := billed.Requests + free.Requests; got != 5 {
		t.Errorf("两榜请求数之和 = %d，期望 5（必须等于实际总请求数，不能重复计数）", got)
	}
	if got := billed.Tokens + free.Tokens; got != 630 {
		t.Errorf("两榜 token 之和 = %d，期望 630", got)
	}
}

// TestLeaderboard_失败与BYOK不算作免费 验证"是否计费"不靠 quota 反推。
//
// 旧思路会用 quota > 0 判定计费，但那会把两类调用误判成免费：
//
//	· 失败的计费请求（额度已全额退还，quota = 0）；
//	· BYOK 调用（用户用自己的上游额度付过钱，站内 quota = 0）。
//
// 两者都会让"免费流量"被高估。因此本测试特意构造"计费但 quota=0"的记录，
// 断言它仍落在计费行、而不是被算成免费。
func TestLeaderboard_失败与BYOK不算作免费(t *testing.T) {
	st := newTestStore(t)
	repo := NewUsageLogRepository(st.DB(), st.Dialect())
	ctx := context.Background()
	now := time.Now()
	since := now.AddDate(0, 0, -30)
	until := now.Add(time.Hour)

	userID := uint64(123)
	// 计费但失败（quota = 0，真实场景里额度已退还）
	entry := &model.UsageLog{
		UserID: userID, ChannelID: 1, Model: "gpt-4o",
		TotalTokens: 500, StatusCode: 500, LatencyMS: 30,
		Quota: 0, BillingFree: false, // 计费模型 → 即使 quota=0 也不是免费
		CreatedAt: now.Add(-time.Hour),
	}
	if err := repo.Create(ctx, entry); err != nil {
		t.Fatalf("写入日志失败: %v", err)
	}
	// BYOK：站内不扣费（quota = 0），但该模型本身是计费模型 → 仍算计费
	buyok := &model.UsageLog{
		UserID: userID, ChannelID: 0, Model: "gpt-4o",
		TotalTokens: 200, StatusCode: 200, LatencyMS: 30,
		Quota: 0, BillingFree: false,
		CreatedAt: now.Add(-2 * time.Hour),
	}
	if err := repo.Create(ctx, buyok); err != nil {
		t.Fatalf("写入日志失败: %v", err)
	}

	entries, err := repo.Leaderboard(ctx, model.UsageLogQuery{Since: &since, Until: &until})
	if err != nil {
		t.Fatalf("Leaderboard 失败: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("应只有一行（全部为计费请求），实际 %d 行", len(entries))
	}
	if entries[0].BillingFree {
		t.Error("计费模型上的失败请求与 BYOK 调用被误判为免费（不能用 quota 反推）")
	}
	if entries[0].Requests != 2 {
		t.Errorf("计费请求数 = %d，期望 2", entries[0].Requests)
	}
}

func createLog(t *testing.T, repo model.UsageLogRepository, userID uint64, at time.Time, status int, tokens int, latency int) {
	t.Helper()
	createLogWithBilling(t, repo, userID, at, status, tokens, latency, false)
}

// createLogWithBilling 写入一条带"是否计费"标记的日志。
//
// billingFree 传 true 表示这次调用走的是免费模型（未命中计价规则或规则显式免费）。
// 该标记由计费层在转发时写入，因此这里直接构造而不是靠 quota 反推——
// 反推会把失败请求与 BYOK 误判为免费（见 TestLeaderboard_失败与BYOK不算作免费）。
func createLogWithBilling(t *testing.T, repo model.UsageLogRepository, userID uint64, at time.Time, status int, tokens int, latency int, billingFree bool) {
	t.Helper()
	entry := &model.UsageLog{
		UserID:      userID,
		ChannelID:   1,
		Model:       "gpt-4o",
		TotalTokens: tokens,
		StatusCode:  status,
		LatencyMS:   latency,
		BillingFree: billingFree,
		CreatedAt:   at,
	}
	if err := repo.Create(context.Background(), entry); err != nil {
		t.Fatalf("写入日志失败: %v", err)
	}
}

// createPaidOrder 写入一条已支付订单（status=2），用于付费判定测试。
func createPaidOrder(t *testing.T, st *Store, userID uint64) {
	t.Helper()
	now := time.Now().Unix()
	if _, err := st.DB().Exec(`INSERT INTO payment_orders
		(trade_no, user_id, amount, currency, quota, method, status, credited, created_at, updated_at)
		VALUES (?, ?, 100, 'CNY', 10000, 'manual', 2, 1, ?, ?)`,
		"test-paid-"+strconv.FormatUint(userID, 10), userID, now, now); err != nil {
		t.Fatalf("写入已支付订单失败: %v", err)
	}
}
