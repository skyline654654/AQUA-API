// 本文件实现「用量预测 + 邮件预警」。
//
// 意图（Why）：
//
//	用户对额度的感知总是滞后的：等到调用返回"额度不足"才知道用完了，
//	这时候业务已经中断了。预警的价值就是【把中断提前到还有时间补救的时刻】。
//
//	为什么必须发到注册邮箱而不是只做站内提示：
//	  站内提示的前提是"用户会来看"。而真正需要预警的时刻，
//	  用户往往正在等一个模型返回、根本没打开控制台——
//	  站内提示会恰好在最需要的时候不被看到。
//	  邮箱是用户注册时主动留下的、且不在站内的那条通道。
//
// 流转（Flow）：
//
//	main 起 goroutine 周期调用 Server.ScanQuotaAlerts(ctx)
//	  └─ 找出"近 N 天有消耗且额度非无限"的用户
//	    → estimateDaysLeft（纯函数：剩余额度 ÷ 日均消耗）
//	    → 命中阈值 → Alerts.Create（冷却去重，唯一索引兜底）
//	      → mailer.Send 到注册邮箱 → Alerts.MarkDelivered
//
// 关键设计：预测是【纯函数 + 可单测】，发信是【副作用】，两者分开。
// 阈值一定会随站点实际数据调整，纯函数部分能独立验证是正确性的前提。
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xiaosu4610/aqua-api/internal/mailer"
	"github.com/xiaosu4610/aqua-api/internal/model"
)

// 用量预警的参数。
const (
	// quotaAlertInterval 是后台扫描周期。
	//
	// 取 6 小时的理由：预警本身也有冷却窗口（12 小时），
	// 扫得比冷却密只是空转；6 小时让"最晚一班扫描"与"冷却结束"基本对齐。
	quotaAlertInterval = 6 * time.Hour

	// quotaAlertStatsDays 是计算日均消耗的回溯天数。
	//
	// 取 7 天而不是 3 天：3 天容易被"周末不用、周一集中跑"带偏，
	// 预测出的耗尽日期会吓到用户并造成不必要的充值。
	quotaAlertStatsDays = 7

	// quotaAlertLowDays 是"余额不足"预警的阈值（预计剩余天数 ≤ 3 天）。
	quotaAlertLowDays = 3

	// quotaAlertDrainDays 是"即将耗尽"预警的阈值（预计剩余天数 ≤ 1 天）。
	quotaAlertDrainDays = 1

	// quotaAlertMinDailyQuota 是参与预测的最小日均消耗。
	//
	// 存在的理由：日均消耗 10额度的用户，"还能用 300 天"这种预测毫无意义，
	// 给他发预警只是噪音。低于此值直接跳过。
	quotaAlertMinDailyQuota = 50

	// quotaAlertUserLimit 是单轮扫描的用户数上限（防风控查询打爆数据库）。
	quotaAlertUserLimit = 500
)

// 预警的冷却窗口（小时）。
//
// 两种预警给不同的窗口：
//   - 余额不足（3 天）：用户有充裕时间慢慢处理，12 小时一次足够；
//   - 即将耗尽（1 天）：需要更频繁地提醒，6 小时一次。
//
// 为什么紧急的反而更频繁：紧急预警的价值恰恰在于"反复确认"——
// 用户可能第一次没看到，6 小时后再说一次能显著提高被看到的概率。
const (
	quotaAlertLowCooldownHours   = 12
	quotaAlertDrainCooldownHours = 6
)

// estimateDaysLeft 估算余额还能支撑多少天（纯函数）。
//
// 输入是"剩余额度"与"日均消耗"，输出是向上取整的天数。
//
// 三条边界约定（每条都有具体的踩坑场景）：
//  1. 剩余额度 <= 0 返回 0：已经没钱了，不是"还能用 0 天"而是"已耗尽"；
//  2. 日均消耗 <= 0 返回 -1：表示"无法预测"（没有消耗数据），
//     而不是返回一个大数——把"不知道"说成"一百年"是危险的误导；
//  3. 向上取整：宁可少报一点余量，也不要让用户以为"还够用 2 天"结果当天就没了。
//
// 返回 -1 的语义是"不可预测"，调用方必须显式处理它，
// 不能当 0 用（那会误发"已耗尽"预警）。
func estimateDaysLeft(remainQuota, dailyQuota int64) int {
	if remainQuota <= 0 {
		return 0
	}
	if dailyQuota <= 0 {
		return -1
	}
	days := remainQuota / dailyQuota
	if remainQuota%dailyQuota != 0 {
		days++
	}
	return int(days)
}

// alertCooldownHours 返回某类预警的冷却窗口小时数。
func alertCooldownHours(kind string) int {
	if kind == model.AlertQuotaDrain {
		return quotaAlertDrainCooldownHours
	}
	return quotaAlertLowCooldownHours
}

// alertWindowBucket 把当前时间量化成冷却窗口的桶键。
//
// 语义：同一个桶内不重复发信，跨桶后可再发。
// 用整数除法实现"每N 小时一个桶"是最省事且无状态的方案——
// 不需要额外存储，也天然容忍进程重启。
func alertWindowBucket(kind string, now time.Time) int64 {
	hours := alertCooldownHours(kind)
	return now.Unix() / int64(hours*3600)
}

// ScanQuotaAlerts 扫描额度不足的用户并发邮件预警。
//
// 由 main 起 goroutine 周期调用；ctx 取消后立即返回。
//
// 绝不 panic：预警能力的故障不该拖垮主服务。
func (s *Server) ScanQuotaAlerts(ctx context.Context) {
	if s.deps.Alerts == nil || s.deps.Users == nil || s.deps.UsageLogs == nil {
		return
	}
	// 未配置邮件通道时静默跳过：不是错误，是"这个能力没开"。
	// 仍然落库记录吗？——不落。没有投递通道时落库只会让门户的预警中心
	// 显示一堆"用户其实没收到"的记录，反而误导。
	if s.deps.Mailer == nil || !s.deps.Mailer.Configured() {
		return
	}

	siteName := s.siteDisplayName(ctx)

	// 扫描范围：近 7 天有消耗的用户。没消耗的用户不需要预警。
	since := time.Now().AddDate(0, 0, -quotaAlertStatsDays)
	active, err := s.deps.UsageLogs.TopActiveUsers(ctx, since, quotaAlertUserLimit)
	if err != nil {
		slog.Warn("扫描额度预警失败：查询活跃用户出错", "error", err)
		return
	}

	sent := 0
	for _, stat := range active {
		ok, err := s.alertUserIfNeeded(ctx, stat.UserID, siteName)
		if err != nil {
			slog.Warn("处理用户额度预警失败", "error", err, "user_id", stat.UserID)
			continue
		}
		if ok {
			sent++
		}
	}
	if sent > 0 {
		slog.Info("额度预警扫描完成", "sent", sent)
	}
}

// alertUserIfNeeded 判断某用户是否需要预警，需要则落库并发信。
// 返回值表示"本轮是否真的发出了一封"。
func (s *Server) alertUserIfNeeded(ctx context.Context, userID uint64, siteName string) (bool, error) {
	user, err := s.deps.Users.GetByID(ctx, userID)
	if err != nil {
		// 用户可能已注销：跳过即可，不算失败。
		return false, nil
	}
	// 不限额度的人永远耗尽不了余额，不该收这类邮件。
	if user.Quota == model.QuotaUnlimited {
		return false, nil
	}
	// 未注册邮箱的用户无处可发：门户预警中心仍会记录，但不发邮件。
	// 这里直接跳过整个流程（含落库），避免门户显示"发过邮件"却没有邮件。
	email := ""
	if user.Email != "" {
		email = user.Email
	}
	if email == "" {
		return false, nil
	}

	remain := user.Quota - user.UsedQuota
	// 已经是负数（透支）时不发"即将耗尽"：那类用户需要的是直接充值，
	// 而不是预警。透支本身会在调用时被拦下，无需邮件。
	if remain <= 0 {
		return false, nil
	}

	dailyQuota, topModel, topModelQuota, err := s.userConsumptionProfile(ctx, userID)
	if err != nil {
		return false, err
	}
	// 消耗太低（还没形成稳定用量）不预测：给刚注册的人发"预计 300 天后耗尽"
	// 只会显得荒唐。
	if dailyQuota < quotaAlertMinDailyQuota {
		return false, nil
	}

	days := estimateDaysLeft(remain, dailyQuota)
	kind := ""
	switch {
	case days >= 0 && days <= quotaAlertDrainDays:
		kind = model.AlertQuotaDrain
	case days >= 0 && days <= quotaAlertLowDays:
		kind = model.AlertQuotaLow
	default:
		return false, nil
	}

	now := time.Now()
	bucket := alertWindowBucket(kind, now)

	// 先落库再发信：唯一索引在这里充当"发信闸门"。
	// 顺序不能反（先发信再落库）：并发时两轮扫描会同时通过"未发过"检查，
	// 导致用户收到两封完全相同的预警。
	subject, body := mailer.QuotaAlertEmail(mailer.QuotaAlertEmailInput{
		SiteName:      siteName,
		Username:      user.Username,
		Kind:          kind,
		RemainQuota:   remain,
		DailyQuota:    dailyQuota,
		DaysLeft:      days,
		TopModel:      topModel,
		TopModelQuota: topModelQuota,
		PortalURL:     s.portalURL(ctx),
	})

	alert := &model.AlertNotification{
		UserID:           userID,
		Kind:             kind,
		Severity:         alertSeverity(kind),
		WindowBucket:     bucket,
		Title:            subject,
		Body:             body,
		EstimatedDaysLeft: days,
		Delivered:        0,
		CreatedAt:        now,
	}
	if err := s.deps.Alerts.Create(ctx, alert); err != nil {
		if errors.Is(err, model.ErrAlertAlreadySent) {
			// 该冷却窗口内已发过：安静跳过，这是设计中的正常路径。
			return false, nil
		}
		return false, err
	}

	// 投递失败保留 delivered=0，下轮冷却结束后会重试——
	// 这正是"落库而非发完即弃"的第一个价值。
	if err := s.deps.Mailer.Send(ctx, email, subject, body); err != nil {
		slog.Warn("发送额度预警邮件失败，将在下轮重试",
			"error", err, "user_id", userID, "kind", kind)
		return false, nil
	}
	if err := s.deps.Alerts.MarkDelivered(ctx, alert.ID); err != nil {
		slog.Warn("标记预警已送达失败", "error", err, "alert_id", alert.ID)
	}

	slog.Info("已发送额度预警邮件",
		"user_id", userID, "username", user.Username,
		"kind", kind, "days_left", days, "remain_quota", remain)
	return true, nil
}

// userConsumptionProfile 返回该用户近 N 天的 (日均消耗, 消耗最多的模型, 该模型消耗)。
func (s *Server) userConsumptionProfile(ctx context.Context, userID uint64) (int64, string, int64, error) {
	// 优先用仓储已有的"近N 天消耗汇总"，避免为预警再写一套聚合。
	since := time.Now().AddDate(0, 0, -quotaAlertStatsDays)
	summary, err := s.deps.UsageLogs.Summary(ctx, model.UsageLogQuery{
		UserID: &userID,
		Since:  &since,
	})
	if err != nil {
		return 0, "", 0, fmt.Errorf("server: 统计用户 %d 的消耗失败: %w", userID, err)
	}

	days := float64(quotaAlertStatsDays)
	var daily int64
	if summary.Requests > 0 {
		daily = int64(float64(summary.Quota) / days)
	}

	// TopModel 是"钱花在哪"的第一层答案，能让预警邮件不止于"你快没钱了"。
	topModel := ""
	var topQuota int64
	if models, err := s.deps.UsageLogs.TopModels(ctx, model.UsageLogQuery{
		UserID: &userID,
		Since:  &since,
	}, 1); err == nil && len(models) > 0 {
		topModel = models[0].Model
		topQuota = models[0].Quota
	}

	return daily, topModel, topQuota, nil
}

// alertSeverity 返回预警的严重级别（1 提示 / 2 警告 / 3 紧急）。
func alertSeverity(kind string) int {
	if kind == model.AlertQuotaDrain {
		return 3
	}
	return 2
}

// handleMyAlerts 返回当前用户的预警记录（门户「预警中心」）。
//
// 为什么要给用户看这个列表：
//   - 邮件会被删、被归进垃圾箱、也可能根本没送达；
//     只靠邮件的预警在"用户真的需要知道"这件事上是不可靠的单一通道。
//   - 用户问"为什么我收到这封邮件"时，需要一个能自查的入口。
// 注意这里返回的是邮件【正文的纯文本快照】而非 HTML：
// 前端要展示成列表文案，直接塞 HTML 会有 XSS 风险。
//
// GET /api/user/alerts?limit=20
func (s *Server) handleMyAlerts(c *gin.Context) {
	user, ok := s.requireCurrentUser(c)
	if !ok {
		return
	}
	if s.deps.Alerts == nil {
		s.respondInternalError(c, "预警功能未启用")
		return
	}

	limit := 20
	if raw := c.Query("limit"); raw != "" {
		if parsed, err := parsePositiveInt(raw); err == nil && parsed <= 100 {
			limit = parsed
		}
	}

	alerts, err := s.deps.Alerts.ListByUser(c.Request.Context(), user.ID, limit)
	if err != nil {
		s.respondInternalError(c, "查询预警记录失败")
		return
	}

	items := make([]gin.H, 0, len(alerts))
	for _, a := range alerts {
		items = append(items, gin.H{
			"id":              a.ID,
			"kind":            a.Kind,
			"kind_text":       alertKindLabel(a.Kind),
			"severity":        a.Severity,
			"title":           a.Title,
			"summary":         alertSummary(a),
			"days_left":       a.EstimatedDaysLeft,
			"delivered":       a.Delivered == 1,
			"window_bucket":   a.WindowBucket,
			"created_at":      a.CreatedAt.Unix(),
		})
	}
	c.JSON(http.StatusOK, gin.H{"alerts": items})
}

// alertSummary 从邮件正文里抽出可安全展示的纯文本摘要。
//
// 为什么不存一份独立的 summary 字段：那样每次发信要维护两份文案，
// 迟早不一致。直接从已发送的正文里提取标题行（<h1>…</h1>），
// 保证"门户显示的"与"邮件里实际写的"永远是同一句话。
func alertSummary(a *model.AlertNotification) string {
	const openTag = "<h1 style=\"margin:0 0 8px;font-size:18px;font-weight:600;color:#0f172a;\">"
	const closeTag = "</h1>"
	start := strings.Index(a.Body, openTag)
	if start < 0 {
		// 模板变更时兜底：直接给类型说明，而不是空白。
		return alertKindLabel(a.Kind)
	}
	rest := a.Body[start+len(openTag):]
	end := strings.Index(rest, closeTag)
	if end < 0 {
		return alertKindLabel(a.Kind)
	}
	return rest[:end]
}

// alertKindLabel 把预警类型翻译为中文展示名。
func alertKindLabel(kind string) string {
	switch kind {
	case model.AlertQuotaLow:
		return "余额不足"
	case model.AlertQuotaDrain:
		return "即将耗尽"
	case model.AlertUsageSpike:
		return "用量异常增长"
	default:
		return kind
	}
}

// siteDisplayName 读取站点显示名（读不到时用兜底名）。
//
// 复用 model.LoadSiteSettings 而不是自己 Get("site.name")：
// 站点设置的键名与默认值只在该函数里定义一处，
// 各处自己拼键名必然出现"改了一处、另一处读不到"的漂移。
func (s *Server) siteDisplayName(ctx context.Context) string {
	if s.deps.Settings == nil {
		return "AQUA-API"
	}
	settings, err := model.LoadSiteSettings(ctx, s.deps.Settings)
	if err != nil || settings.SiteName == "" {
		return "AQUA-API"
	}
	return settings.SiteName
}

// portalURL 返回门户地址（空串表示邮件里不显示充值按钮）。
//
// 从 settings 的 site.url 读，而不是从 config 读：
// 站点地址是【运营随时会改】的东西（换域名、加CDN），
// 放settings 才能不发版就改；放 config 意味着每次换域名都要重启进程。
//
// 读不到就返回空串（不显示按钮）：宁可少一个按钮，
// 也不要给用户一个点不开的链接——那比没有链接更让人困惑。
func (s *Server) portalURL(ctx context.Context) string {
	if s.deps.Settings == nil {
		return ""
	}
	raw, err := s.deps.Settings.Get(ctx, "site.url")
	if err != nil {
		return ""
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	// 必须以 http(s) 开头：否则拼进 href 会变成 javascript: 之类的协议，
	// 虽然值来自自家后台，但仍应做最小校验——邮件客户端不会替我们把关。
	if !strings.HasPrefix(trimmed, "http://") && !strings.HasPrefix(trimmed, "https://") {
		return ""
	}
	return strings.TrimRight(trimmed, "/")
}
