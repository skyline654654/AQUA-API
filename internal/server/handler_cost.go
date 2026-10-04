// 本文件实现「成本归因」接口。
//
// 意图（Why）：
//
//	站长与用户真正想知道的不是"我花了多少"，而是"钱花在哪"——
//
// 是站内游乐场试玩、还是某个插件在后台静默烧、还是某个脚本在刷。
// 缺少这个维度，所有优化都只能靠猜。
//
// 归因的维度选择（为什么是"场景标签"而不是别的）：
//   - 按令牌切：能回答"哪把 Key 花的"，但同一把 Key 往往被多个场景复用，
//     仍然回答不了"哪个场景"；
//   - 按模型切：能回答"哪个模型贵"，但试玩和线上用同一个模型时无法区分；
//   - 按时间切：是"什么时候"，不是"为了什么"；
//   - 按场景标签切：正交于前两者，能把"同模型不同用途"分开。
//
// 流转（Flow）：
//
//	客户端 → X-Aqua-Tag 头 → middleware.CaptureTag → reqctx.Tag
//	  → relay 落 usage_logs.tag
//	    → GET /api/user/cost/attribution 按 (user_id, tag) 聚合
//
// 扩展（Extend）：
//
//	需要更多维度时（按 IP / 按模型 × 标签二维交叉）：
//	在 model.CostRepository 加方法并配一条 GROUP BY，接口层加一个 query 参数。
//	二维交叉要小心组合爆炸——务必保留"至少按额度倒序 + LIMIT"的约束。
package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xiaosu4610/aqua-api/internal/model"
)

// 成本归因的时间窗约束。
const (
	// costDefaultDays 是默认回溯天数（近 30 天）。
	//
	// 为什么默认一个月而不是 7 天：成本优化的决策周期以月计——
	// 7 天内的波动可能只是一次调试，"少用某模型"这种结论需要更长样本支撑。
	costDefaultDays = 30
	// costMaxDays 是允许的最大回溯天数（近 180 天）。
	//
	// 上限存在的理由：usage_logs 会很大，无界的时间窗能让一次页面刷新
	// 变成全表扫描，把"看板"变成"压测工具"。
	costMaxDays = 180
	// costMaxTags 是返回的标签数量上限。
	//
	// 归一后的标签基数理论上无界（客户端可自由声明），不设上限会让
	// 恶意客户端用随机标签把接口打成返回几万个分组。
	costMaxTags = 50
)

// costAttributionDTO 是单个场景的成本视图。
type costAttributionDTO struct {
	Tag string `json:"tag"`
	// TagLabel 是便于直接展示的中文名（未标注/站内游乐场有专门文案，
	// 自定义标签原样返回）。
	TagLabel string `json:"tag_label"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
	Quota    int64  `json:"quota"`
	// Share 是该场景占总额度的比例（0~1），前端直接乘100 显示。
	Share float64 `json:"share"`
	// AvgTokens 是单次请求平均 token 数（衡量"这个场景有多重"）。
	AvgTokens float64 `json:"avg_tokens"`
}

// costAttributionSummaryDTO 是总量汇总（看板顶部的四个数字）。
type costAttributionSummaryDTO struct {
	Requests int64 `json:"requests"`
	Tokens   int64 `json:"tokens"`
	Quota    int64 `json:"quota"`
	// TagCount 是出现过的场景数（被 LIMIT 截断时，实际标签数可能更多，
	// 因此这里报的是"返回的标签数"而非精确总数）。
	TagCount int `json:"tag_count"`
	// Truncated 表示结果是否因超过上限而被截断（前端据此提示"仅显示前 N 项"）。
	Truncated bool `json:"truncated"`
	Days      int  `json:"days"`
}

// handleCostAttribution 返回按场景标签切分的成本归因。
//
// GET /api/user/cost/attribution?days=30
func (s *Server) handleCostAttribution(c *gin.Context) {
	user, ok := s.requireCurrentUser(c)
	if !ok {
		return
	}
	if s.deps.Costs == nil {
		s.respondInternalError(c, "成本归因功能未启用")
		return
	}

	days := costDefaultDays
	if raw := c.Query("days"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= costMaxDays {
			days = parsed
		}
	}
	since := time.Now().Add(-time.Duration(days) * time.Hour)

	items, err := s.deps.Costs.CostByTag(c.Request.Context(), user.ID, since, time.Time{})
	if err != nil {
		s.respondInternalError(c, "查询成本归因失败")
		return
	}

	// 截断到上限：按额度倒序（仓储已排序），保留最烧钱的那些场景。
	truncated := len(items) > costMaxTags
	if truncated {
		items = items[:costMaxTags]
	}
	// 截断后需要重算占比：分母（总额度）必须与实际展示的部分一致，
	// 否则前 N 项的占比加起来会小于 100%，前端显示的饼图会有"缺失的一块"。
	var totalQuota int64
	for _, it := range items {
		totalQuota += it.Quota
	}

	summary := costAttributionSummaryDTO{TagCount: len(items), Truncated: truncated, Days: days}
	result := make([]costAttributionDTO, 0, len(items))
	for _, it := range items {
		if totalQuota > 0 {
			it.Share = float64(it.Quota) / float64(totalQuota)
		}
		summary.Requests += it.Requests
		summary.Tokens += it.Tokens
		summary.Quota += it.Quota
		result = append(result, costAttributionDTO{
			Tag:       it.Tag,
			TagLabel:  costTagLabel(it.Tag),
			Requests:  it.Requests,
			Tokens:    it.Tokens,
			Quota:     it.Quota,
			Share:     it.Share,
			AvgTokens: it.AvgTokens,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"items":   result,
		"summary": summary,
	})
}

// costTagLabel 把标签翻译为便于展示的名称。
//
// 只对内置标签做翻译，自定义标签原样返回——用户自己写的 tag
// 猜错含义比原样显示更糟。
func costTagLabel(tag string) string {
	switch tag {
	case model.TagUntagged:
		return "未标注（客户端未传标签）"
	case model.TagPlayground:
		return "站内游乐场"
	default:
		return tag
	}
}
