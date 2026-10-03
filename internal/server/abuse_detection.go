// 本文件实现「盗 Key / 滥用检测」的判定逻辑与后台扫描任务。
//
// 意图（Why）：
//
//	访问令牌一旦外泄，损失由站长承担：攻击者会用最低成本刷最贵的模型，
//	而账单上只表现为"某个用户的额度掉得很快"。人工发现通常已经太晚。
//
//	检测的核心思路是【与自身历史比，而不是与绝对阈值比】：
//	同一个用户从"每天 50 次"跳到"每天 5000 次"是异常；
//	另一个用户一直就是"每天 5000 次"（重度使用），那不是异常。
//	用绝对阈值（如"超过 1000次/小时就告警"）必然把重度用户全部误报，
//	而误报多了，整个告警就会被无视——这是风控系统最常见的死法。
//
// 流转（Flow）：
//
//	main 起 goroutine 周期调用 Server.ScanAbuseEvents(ctx)
//	  └─ 对近期活跃用户：
//	       ├─ UserHourlyRequests(近 N 小时) → DetectBurst（纯函数判定）
//	       └─ 命中 → 写 abuse_events（留痕）→ 达冷却阈值则限流
//
// 扩展（Extend）：
//
//	新增检测维度时：在本文件加一个纯函数 + 一条 kind 常量，
//	在 scanUser 里挂上即可。务必坚持"纯函数 + 可单测"的结构——
//	阈值调整频繁，有测试才敢改。
package server

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xiaosu4610/aqua-api/internal/model"
)

// 滥用检测的默认参数。
//
// 全部以常量而非配置项起步：检测阈值需要"用真实数据调"，
// 而配置项一多就没人敢动。这里给的是经验起点，
// 待积累足够样本后再决定哪些值得提到 settings。
const (
	// abuseScanInterval 是后台扫描周期（由 main 的 ticker 使用）。
	abuseScanInterval = 10 * time.Minute
	// abuseHistoryHours 是"自身历史"的回溯窗口（7 天）。
	//
	// 取7 天的理由：一周覆盖了"工作日 vs 周末"的自然波动，
	// 比只看 24 小时更不容易把"周一集中跑批"误判成异常。
	abuseHistoryHours = 24 * 7
	// abuseBurstFactor 是突发倍数阈值：当前小时请求数超过 P99 的多少倍算异常。
	abuseBurstFactor = 5.0
	// abuseBurstMinSamples 是突发判定所需的最少历史桶数。
	//
	// 下限的意义：样本太少时 P99 不可靠（3 个桶的 P99约等于最大值），
	// 一次正常的批量任务就能触发误报。不如不判。
	abuseBurstMinSamples = 24
	// abuseBurstMinRequests 是突发判定所需的最小绝对请求数。
	abuseBurstMinRequests = 50
	// abuseNotifyCooldown 是同类异常的通知冷却（24 小时）。
	abuseNotifyCooldown = 24 * time.Hour
)

// abuseDetectResult 是一次检测的结论。
type abuseDetectResult struct {
	// Triggered 表示是否命中异常。
	Triggered bool
	// Metric 是触发时的度量值（当前小时请求数）。
	Metric float64
	// Threshold 是触发阈值（P99 × factor）。
	Threshold float64
	// Detail 是人类可读说明（已脱敏）。
	Detail string
}

// detectBurst 判断"最近一个整点的请求数"是否相对自身历史异常突增。
//
// 三条判定缺一不可（缺任何一条都会显著提高误报率）：
//  1. 历史桶数足够（≥ abuseBurstMinSamples）：否则 P99 不可信；
//  2. 历史 P99 足够高（≥ abuseBurstMinRequests 的基数）：
//     否则"从 2 次跳到 20 次"会触发告警，而那只是从零星调用变活跃；
//  3. 当前值超过 P99 × factor 且当前值本身达到最小绝对量：
//     同时挡住"低基数放大"与"小抖动"两类误报。
//
// 历史分布用 P99 而非均值：均值会被少数极端小时拉高，
// 导致阈值过高而漏报；P99 天然"允许 1% 的极端值"，正是我们要的。
//
// 注意：调用方传入的桶里最后一个是"当前这个还没走完的整点"，
// 本函数会自行把它排除在历史之外（半个小时的用量拿当历史会低估 P99）。
func detectBurst(buckets []model.HourlyUsage) abuseDetectResult {
	if len(buckets) == 0 {
		return abuseDetectResult{}
	}

	// 切出历史集（排除未走完的当前桶）。
	// 调用方传入的桶按时间升序，最后一个桶就是"当前这个还没走完的整点"——
	// 它必须被排除：半个小时的用量当然低于全天，拿它当历史会低估 P99。
	if len(buckets) < 2 {
		return abuseDetectResult{} // 没有历史可比
	}
	history := buckets[:len(buckets)-1]
	cur := buckets[len(buckets)-1].Requests

	// 历史桶不足 → 样本不够，不判。
	if len(history) < abuseBurstMinSamples {
		return abuseDetectResult{}
	}

	counts := make([]int64, 0, len(history))
	for _, b := range history {
		counts = append(counts, b.Requests)
	}
	p99 := percentileInt64(counts, 0.99)

	// 历史基数太低：突增可能只是"从不用到常用"，不是异常。
	if p99 < abuseBurstMinRequests/2 {
		return abuseDetectResult{}
	}
	// 当前值本身太小：不值得惊动任何人。
	if cur < abuseBurstMinRequests {
		return abuseDetectResult{}
	}

	threshold := float64(p99) * abuseBurstFactor
	if float64(cur) <= threshold {
		return abuseDetectResult{}
	}

	return abuseDetectResult{
		Triggered: true,
		Metric:    float64(cur),
		Threshold: threshold,
		Detail: fmt.Sprintf("最近 1 小时请求 %d 次，超过其历史 P99（%d 次）的 %.1f 倍",
			cur, p99, abuseBurstFactor),
	}
}

// percentileInt64 返回样本的分位数（线性插值）。
//
// 用分位数而不是 max：max 会被单个异常小时拉高，
// 导致"上次已经爆发过一次"之后阈值高到再也检测不到。
func percentileInt64(values []int64, p float64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := make([]int64, len(values))
	copy(sorted, values)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[len(sorted)-1]
	}
	pos := p * float64(len(sorted)-1)
	lower := int(math.Floor(pos))
	upper := int(math.Ceil(pos))
	if lower == upper {
		return sorted[lower]
	}
	frac := pos - float64(lower)
	// 插值：相邻两个样本之间线性取值。请求数是整数，这里保留浮点中间值
	// 是为了不因取整方向而系统性偏松或偏严。
	return int64(math.Round(float64(sorted[lower])*(1-frac) + float64(sorted[upper])*frac))
}

// ScanAbuseEvents 扫描近期活跃用户的异常用量并留痕。
//
// 由 main 起 goroutine 周期调用；ctx 取消后立即返回。
//
// 边界与安全约束：
//   - 只做【记录】，不自动限流用户。自动限流的误伤代价远大于多记一条事件，
//     真需要限流时应由站长在看到事件后手工操作；
//   - 单个用户出错只跳过该用户，不影响其余用户（不中断整轮扫描）。
func (s *Server) ScanAbuseEvents(ctx context.Context) {
	if s.deps.Abuse == nil || s.deps.UsageLogs == nil {
		return
	}

	// 扫描范围 = 近 24 小时调用最频繁的前 N 个用户。
	// 从日志侧取（而不是遍历用户表）：只有"真的在调用"的用户才需要风控，
	// 而用户表里大量注册后从未调用的账号不该占用扫描预算。
	since := time.Now().Add(-24 * time.Hour)
	active, err := s.deps.UsageLogs.TopActiveUsers(ctx, since, abuseScanUserLimit)
	if err != nil {
		slog.Warn("扫描异常用量失败：查询活跃用户出错", "error", err)
		return
	}

	detected := 0
	for _, stat := range active {
		events, err := s.scanUserAbuse(ctx, stat)
		if err != nil {
			slog.Warn("扫描用户异常用量失败", "error", err, "user_id", stat.UserID)
			continue
		}
		detected += events
	}
	if detected > 0 {
		slog.Warn("异常用量扫描完成：发现可疑调用", "events", detected)
	}
}

// abuseScanUserLimit 是单轮扫描的用户数上限。
//
// 存在的理由与 costMaxTags 相同：风控逻辑不该在一次定时任务里打爆数据库。
// 上限意味着大站会"分批覆盖"（每轮扫最活跃的前 N 个用户），
// 对突发检测而言可接受——突发是持续状态，晚一轮发现不改变结论。
const abuseScanUserLimit = 500

// scanUserAbuse 扫描单个用户，返回本轮新发现的事件数。
func (s *Server) scanUserAbuse(ctx context.Context, stat model.ActiveUserStat) (int, error) {
	buckets, err := s.deps.Abuse.UserHourlyRequests(ctx, stat.UserID, abuseHistoryHours)
	if err != nil {
		return 0, fmt.Errorf("server: 查询用户 %d 的小时用量失败: %w", stat.UserID, err)
	}

	result := detectBurst(buckets)
	if !result.Triggered {
		return 0, nil
	}

	// 冷却：同类异常在窗口内已记过就不重复落库。
	// 理由：突发是持续状态（用户每小时都在刷），不冷却的话每轮都会记一条，
	// 几小时后表里全是同一条事实的副本，淹没真正的不同事件。
	since := time.Now().Add(-abuseNotifyCooldown)
	recent, err := s.deps.Abuse.RecentBurstCount(ctx, stat.UserID, model.AbuseKindBurst, since)
	if err != nil {
		return 0, fmt.Errorf("server: 统计用户 %d 的历史异常事件失败: %w", stat.UserID, err)
	}
	if recent > 0 {
		return 0, nil
	}

	// 用户名只用于日志留痕：查不到就显示 ID，不为一行日志中断扫描。
	username := ""
	if u, err := s.deps.Users.GetByID(ctx, stat.UserID); err == nil {
		username = u.Username
	}

	event := &model.AbuseEvent{
		UserID:   stat.UserID,
		Kind:     model.AbuseKindBurst,
		Severity: model.AbuseSeverityWarn,
		// 只观察不处置：见函数头的安全约束。
		Action:    model.AbuseActionObserve,
		Detail:    result.Detail,
		Metric:    result.Metric,
		Threshold: result.Threshold,
		CreatedAt: time.Now(),
	}
	if err := s.deps.Abuse.Create(ctx, event); err != nil {
		return 0, fmt.Errorf("server: 写入异常事件失败: %w", err)
	}

	// 失败占比本身就是重要信号：正常用户偶有失败，
	// 而"拿着被盗 Key 乱试"的成功率会异常低。把它写进说明便于站长判断性质。
	slog.Warn("用户请求量相对自身历史异常突增",
		"user_id", stat.UserID,
		"username", username,
		"detail", result.Detail,
		"window_requests", stat.Requests,
		"window_success", stat.Success)
	return 1, nil
}

// handleListAbuseEvents 返回最近的异常事件（管理后台）。
//
// GET /api/admin/abuse-events?limit=50
func (s *Server) handleListAbuseEvents(c *gin.Context) {
	if s.deps.Abuse == nil {
		s.respondInternalError(c, "异常检测功能未启用")
		return
	}

	limit := 50
	if raw := c.Query("limit"); raw != "" {
		if parsed, err := parsePositiveInt(raw); err == nil && parsed <= 200 {
			limit = parsed
		}
	}

	events, err := s.deps.Abuse.ListRecent(c.Request.Context(), limit)
	if err != nil {
		s.respondInternalError(c, "查询异常事件失败")
		return
	}

	// 一次性解析用户名，避免 N+1 查询（事件列表每行都要显示是谁）。
	usernames := s.loadUsernames(c.Request.Context())

	items := make([]gin.H, 0, len(events))
	for _, e := range events {
		items = append(items, gin.H{
			"id":        e.ID,
			"user_id":   e.UserID,
			"username":  usernames[e.UserID],
			"token_id":  e.TokenID,
			"kind":      e.Kind,
			"kind_text": abuseKindLabel(e.Kind),
			"severity":  e.Severity,
			"action":    e.Action,
			"detail":    e.Detail,
			"metric":    e.Metric,
			"threshold": e.Threshold,
			"created_at": e.CreatedAt.Unix(),
		})
	}
	c.JSON(http.StatusOK, gin.H{"events": items})
}

// abuseKindLabel 把异常类型翻译为中文展示名。
func abuseKindLabel(kind string) string {
	switch kind {
	case model.AbuseKindBurst:
		return "请求量突增"
	case model.AbuseKindFingerprint:
		return "可疑调用指纹"
	case model.AbuseKindKeyLeak:
		return "凭据疑似外泄"
	default:
		return kind
	}
}

// parsePositiveInt 解析正整数，失败返回错误。
func parsePositiveInt(raw string) (int, error) {
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, err
	}
	if v <= 0 {
		return 0, fmt.Errorf("必须为正整数")
	}
	return v, nil
}
