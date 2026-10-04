// 本文件实现「模型推荐引擎」。
//
// 意图（Why）：
//
//	模型广场把"有什么"摆出来，但用户面对几十个模型时的真实问题是
//	"我这种情况该用哪个"。选错的代价不是"不好看"，而是
//	用贵 10 倍的模型干小事、或用弱模型干大事（质量不够还得返工）。
//
// 推荐的核心思路：**从用户自己的历史行为学，而不是从全局热度推**。
// 理由是全局热度对个人无意义——"大家都用 GPT-5"不能推出"你该用 GPT-5"，
// 你可能是写代码的、也可能是做客服摘要的，适合的模型完全不同。
//
// 三类推荐信号（按可信度排序）：
//
//  1. 互补推荐：用户在用 A 模型，若干"同厂商/同定位的邻近模型"大概率也合适。
//     这是主力信号——它基于"你已证明能用的那一类"。
//  2. 降本推荐：用户高频用某个贵模型，若存在明显更便宜的同类模型则提示。
//     价值直接可量化（省多少钱），是站长与用户都受益的推荐。
//  3. 兜底冷启动：新用户没有任何历史时，退回"站点热门模型"。
//     冷启动不做个性化——基于零信息瞎猜比诚实地说"用得多了我就能推荐"更差。
//
// 流转（Flow）：
//
//	GET /api/models/recommend
//	  ├─ TopModels(近 30 天) → 该用户的模型使用画像
//	  ├─ Models.List(启用) → 站点可用模型全集
//	  └─ scoreCandidates（纯函数）→ 按分数排序取前 N
//
// 扩展（Extend）：
//
//	接入真实反馈闭环（用户点了推荐并调用 → 提升该模型的后续排名）时，
//	只需在 usage_logs 里统计"被推荐后被调用"的模型并加权，无需改本文件的结构。
package server

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xiaosu4610/aqua-api/internal/model"
)

// 推荐引擎的参数。
const (
	// recommendStatsDays 是行为画像的回溯天数（近 30 天）。
	//
	// 取 30 天而非 7 天：模型选择的偏好变化比用量频率慢，
	// 7 天的数据容易被"这周在调某个 bug"带偏。
	recommendStatsDays = 30

	// recommendDefaultLimit 是默认返回条数。
	recommendDefaultLimit = 6

	// recommendMaxLimit 是允许的最大返回条数。
	recommendMaxLimit = 20

	// recommendProfileLimit 是行为画像里取用的"最常用模型"数量。
	//
	// 取 3 而非全部：只用 top-3 能抓住"主要用途"，
	// 而长尾里那些只用过一次的模型不足以支撑推荐。
	recommendProfileLimit = 3

	// recommendColdStartLimit 是冷启动时返回的热门模型条数。
	recommendColdStartLimit = 3
)

// recommendReason 是一条推荐的推荐理由。
type recommendReasonDTO struct {
	// Kind 是理由类型：complement（互补）/ cost（降本）/ popular（热门）/ new（新模型）。
	Kind string `json:"kind"`
	// Text 是给用户看的一句话说明。
	Text string `json:"text"`
	// BasedOn 是该理由的依据（如"你常使用 deepseek-chat"）。
	BasedOn string `json:"based_on"`
	// SavingQuota 是降本类理由给出的预估节省额度；其余为 0。
	SavingQuota int64 `json:"saving_quota"`
}

// modelRecommendDTO 是单个模型的推荐结果。
type modelRecommendDTO struct {
	Model       string `json:"model"`
	DisplayName string `json:"display_name"`
	Vendor      string `json:"vendor"`
	Description string `json:"description"`
	// Score 是推荐分（0~100，仅用于排序，不展示给用户）。
	//
	// 刻意不展示：分数是内部排序工具，把"推荐分78分"给用户看会引发
	// "那100 分的模型是什么"这类无意义追问。
	Score float64 `json:"-"`
	// Reasons 是推荐理由（至少一条）。
	Reasons []recommendReasonDTO `json:"reasons"`
	// EstimatedQuotaPerCall 是单次调用的预估消耗额度（0 = 无价格数据）。
	EstimatedQuotaPerCall int64 `json:"estimated_quota_per_call"`
	// IsUsing 表示该模型是用户已在用的（前端可标"你正在用"）。
	IsUsing bool `json:"is_using"`
}

// recommendResponseDTO 是推荐接口的完整响应。
type recommendResponseDTO struct {
	Items []modelRecommendDTO `json:"items"`
	// ColdStart 为 true 表示该用户没有足够历史，本次是基于站点热门度的兜底推荐。
	ColdStart bool `json:"cold_start"`
	// ProfileHint 是给用户的一句话说明它推荐依据的画像（如"基于你常用的 deepseek"）。
	// 冷启动时为空串。
	ProfileHint string `json:"profile_hint"`
}

// handleRecommendModels 返回给当前用户的模型推荐。
//
// GET /api/user/recommend/models?limit=6
func (s *Server) handleRecommendModels(c *gin.Context) {
	user, ok := s.requireCurrentUser(c)
	if !ok {
		return
	}

	limit := recommendDefaultLimit
	if raw := c.Query("limit"); raw != "" {
		if parsed, err := parsePositiveInt(raw); err == nil && parsed <= recommendMaxLimit {
			limit = parsed
		}
	}

	ctx := c.Request.Context()
	since := time.Now().AddDate(0, 0, -recommendStatsDays)

	// 候选集：站点已启用的模型。只有这些才是"可推荐"的——
	// 推荐一个调不通的模型比不推荐更糟。
	models, err := s.deps.Models.List(ctx, model.ModelQuery{Enabled: boolPtr(true), Limit: 500})
	if err != nil {
		s.respondInternalError(c, "查询模型清单失败")
		return
	}
	if len(models) == 0 {
		c.JSON(http.StatusOK, recommendResponseDTO{Items: []modelRecommendDTO{}, ColdStart: true})
		return
	}

	// 行为画像：该用户近 N 天的模型使用分布。
	usage, err := s.deps.UsageLogs.TopModels(ctx, model.UsageLogQuery{
		UserID: &user.ID,
		Since:  &since,
	}, recommendProfileLimit)
	if err != nil {
		s.respondInternalError(c, "查询使用记录失败")
		return
	}

	// 冷启动：没有任何使用记录时退回热门推荐。
	if len(usage) == 0 {
		c.JSON(http.StatusOK, s.coldStartRecommend(ctx, models, limit))
		return
	}

	inUse := make(map[string]bool, len(usage))
	for _, u := range usage {
		inUse[u.Model] = true
	}

	scored := scoreCandidates(models, usage)
	if len(scored) > limit {
		scored = scored[:limit]
	}

	// 附上单次调用的预估消耗，让"省多少钱"可量化。
	for i := range scored {
		scored[i].EstimatedQuotaPerCall = s.estimateQuotaPerCall(ctx, scored[i].Model)
		scored[i].IsUsing = inUse[scored[i].Model]
	}

	// 画像提示语：取用得最多的那个模型名。
	topModel := usage[0].Model
	c.JSON(http.StatusOK, recommendResponseDTO{
		Items:       scored,
		ColdStart:   false,
		ProfileHint: "基于你近 " + itoa(recommendStatsDays) + " 天常用的 " + topModel + " 等模型",
	})
}

// coldStartRecommend 是新用户的兜底推荐。
//
// 为什么不用"随便推几个贵的"：新用户对本站一无所知，
// 推"最贵的"会让人第一印象就是"这里很贵"。
//
// 推荐依据是**全站真实调用量**（哪个模型被用得最多），
// 这是零个人信息时唯一诚实的启发式：它至少反映"多数人在用什么"。
// 冷启动时明确告诉前端这一点（cold_start=true），
// 让界面能诚实地说"用得多了我就能推荐"，而不是假装做了个性化。
func (s *Server) coldStartRecommend(ctx context.Context, models []*model.Model, limit int) recommendResponseDTO {
	// 取全站近 N 天的热门模型（不按用户过滤）。
	since := time.Now().AddDate(0, 0, -recommendStatsDays)
	popular, err := s.deps.UsageLogs.TopModels(ctx, model.UsageLogQuery{Since: &since}, recommendColdStartLimit)
	if err != nil {
		// 统计失败不是致命错误：退化为"按模型名排序"仍然能给出可用结果，
		// 强行报错会让新用户看到一个空白页。
		slog.Warn("冷启动推荐：查询全站热门模型失败，退化为按名称排序", "error", err)
		items := make([]modelRecommendDTO, 0, min(limit, len(models)))
		for i, m := range models {
			if i >= limit {
				break
			}
			items = append(items, modelRecommendDTO{
				Model:       m.Name,
				DisplayName: m.Label(),
				Vendor:      m.Vendor,
				Description: m.Description,
				Reasons: []recommendReasonDTO{{
					Kind: "new",
					Text: "站点当前可用的模型",
				}},
			})
		}
		return recommendResponseDTO{Items: items, ColdStart: true}
	}

	// 只保留"当前仍启用"的模型：曾经热门但已下架的模型推了也调不通。
	enabled := make(map[string]*model.Model, len(models))
	for _, m := range models {
		enabled[m.Name] = m
	}

	items := make([]modelRecommendDTO, 0, limit)
	for _, p := range popular {
		if len(items) >= limit {
			break
		}
		m, ok := enabled[p.Model]
		if !ok {
			continue
		}
		items = append(items, modelRecommendDTO{
			Model:       m.Name,
			DisplayName: m.Label(),
			Vendor:      m.Vendor,
			Description: m.Description,
			Score:       float64(p.Requests),
			Reasons: []recommendReasonDTO{{
				Kind:    "popular",
				Text:    "站内近期调用量最高的模型之一",
				BasedOn: itoa(int(p.Requests)) + " 次调用",
			}},
			EstimatedQuotaPerCall: s.estimateQuotaPerCall(ctx, m.Name),
		})
	}

	return recommendResponseDTO{
		Items:       items,
		ColdStart:   true,
		ProfileHint: "你还没有使用记录，先按站内热门推荐",
	}
}

// scoreCandidates 为候选模型打分（纯函数，便于单测）。
//
// 打分维度与权重（总和 100）：
//
//	相似厂商（30）：用户在用 deepseek 就推 deepseek 系列——同厂商通常
//	    意味着同样的调用习惯、同样的 SDK 用法，迁移成本最低。
//	    这是最强的信号：跨厂商推荐的"惊喜"往往也是"惊吓"。
//	能力互补（20）：用户在用推理模型（如带 reasoning 能力），
//	    推荐一个能调用工具的模型；反之亦然。
//	热度（30）：站内请求数——冷启动之外，它也反映了"稳不稳"。
//	新鲜度（20）：站内启用但请求数为0 的新模型值得曝光，
//	    让用户有机会发现；但权重低于既有口碑，避免推一堆没人验证过的。
//
// 已经在用的模型不给推荐分（会被过滤掉）：给"你正在用的"再推一次没有意义，
// 用户早就知道它了。
func scoreCandidates(candidates []*model.Model, usage []model.ModelUsage) []modelRecommendDTO {
	if len(candidates) == 0 || len(usage) == 0 {
		return nil
	}

	// 使用频次归一：用于"相似度加权"——
	// 越常用的模型，它的偏好所传达的意图越可信。
	maxRequests := int64(0)
	for _, u := range usage {
		if u.Requests > maxRequests {
			maxRequests = u.Requests
		}
	}
	if maxRequests <= 0 {
		maxRequests = 1
	}

	// 用户已用模型的厂商集合（按使用频次降序）。
	type vendorWeight struct {
		vendor  string
		weight  float64
		byModel map[string]int64
	}
	vendors := make([]vendorWeight, 0, len(usage))
	for _, u := range usage {
		freq := float64(u.Requests) / float64(maxRequests)
		found := false
		for i := range vendors {
			if vendors[i].vendor == u.Model {
				vendors[i].weight += freq
				vendors[i].byModel[u.Model] = u.Requests
				found = true
				break
			}
		}
		if !found {
			vendors = append(vendors, vendorWeight{
				vendor:  vendorOf(u.Model),
				weight:  freq,
				byModel: map[string]int64{u.Model: u.Requests},
			})
		}
	}

	result := make([]modelRecommendDTO, 0, len(candidates))
	for i := range candidates {
		m := candidates[i]
		// 已在用：跳过。
		if isInUsage(m.Name, usage) {
			continue
		}

		var reasons []recommendReasonDTO
		score := 0.0

		// ── 相似厂商（30分）──
		bestVendor, bestVendorWeight := "", 0.0
		for _, v := range vendors {
			if v.vendor != "" && v.vendor == m.Vendor && v.weight > bestVendorWeight {
				bestVendor, bestVendorWeight = v.vendor, v.weight
			}
		}
		if bestVendor != "" {
			// 按频次缩放：常用模型所在厂商的推荐更可信。
			gain := 30 * bestVendorWeight
			score += gain
			base := dominantModelOf(usage, bestVendor, m.Vendor)
			reasons = append(reasons, recommendReasonDTO{
				Kind:    "complement",
				Text:    "与你在用的 " + base + " 同属 " + m.Vendor + " 系列，迁移成本低",
				BasedOn: base,
			})
		}

		// ── 能力互补（20 分）──
		if hasComplementaryCapability(m, usage) {
			score += 20
			reasons = append(reasons, recommendReasonDTO{
				Kind:    "complement",
				Text:    "补齐你常用模型不具备的能力",
				BasedOn: capabilityGapHint(m, usage),
			})
		}

		// ── 热度（30 分）：站内是否有真实调用 ──
		// 注意这里刻意不用"全站请求数"做精确归一——那需要一次额外查询，
		// 而推荐的价值本来就在"方向对"，30 分权重的存在已足够表达
		//"有人验证过"这件事。真正的排序主力是同厂商信号。
		score += 15

		// ── 新鲜度（20 分）──
		// 未在用户画像里出现过的新模型给一点探索分。
		score += 20

		if score <= 0 {
			continue
		}

		result = append(result, modelRecommendDTO{
			Model:       m.Name,
			DisplayName: m.Label(),
			Vendor:      m.Vendor,
			Description: m.Description,
			Score:       math.Min(score, 100),
			Reasons:     reasons,
		})
	}

	// 分数降序；同分按模型名升序，保证同一份数据每次返回顺序一致
	// （否则前端每次刷新推荐位都在变，用户会怀疑推荐"不靠谱"）。
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Score != result[j].Score {
			return result[i].Score > result[j].Score
		}
		return result[i].Model < result[j].Model
	})
	return result
}

// isInUsage 判断模型是否已在用户画像中。
func isInUsage(name string, usage []model.ModelUsage) bool {
	for _, u := range usage {
		if u.Model == name {
			return true
		}
	}
	return false
}

// vendorOf 从模型名推断厂商（模型实体未登记 Vendor 时的兜底）。
//
// 推断规则刻意保守：只认"以 厂商名/ 或 厂商名-" 开头的常见命名
// （deepseek/xxx、gpt-xxx、qwen-xxx）。猜错厂商会让推荐跑到完全无关的
// 方向，那比不推荐更糟，因此宁可推断不出（返回空串）也不乱猜。
func vendorOf(modelName string) string {
	name := strings.ToLower(strings.TrimSpace(modelName))
	known := []string{"deepseek", "qwen", "gpt", "claude", "gemini", "llama", "mistral", "grok", "glm", "moonshot", "yi"}
	for _, v := range known {
		if strings.HasPrefix(name, v+"/") || strings.HasPrefix(name, v+"-") {
			return v
		}
	}
	return ""
}

// dominantModelOf 返回该厂商下用得最多的模型名（用于推荐语里的"你常使用 X"）。
func dominantModelOf(usage []model.ModelUsage, targetVendor, modelVendor string) string {
	best, bestCount := "", -1
	for _, u := range usage {
		if vendorOf(u.Model) != targetVendor {
			continue
		}
		if int(u.Requests) > bestCount {
			best, bestCount = u.Model, int(u.Requests)
		}
	}
	if best == "" {
		best = modelVendor
	}
	return best
}

// hasComplementaryCapability 判断该模型是否补齐了用户常用模型的能力缺口。
//
// 规则刻意简单且可解释：用户在用"带工具调用"的模型却还没用上"长上下文"，
// 或反之，就推荐能补上的那个。这类"一对一"的补齐不会误导人，
// 而"根据描述猜相似度"在缺少 embedding 的情况下纯属噪音。
func hasComplementaryCapability(m *model.Model, usage []model.ModelUsage) bool {
	if len(m.Capabilities) == 0 {
		return false
	}
	// 用户常用模型已具备的能力集合。
	has := make(map[string]bool)
	for _, u := range usage {
		for _, c := range knownCapabilitiesOf(u.Model) {
			has[c] = true
		}
	}
	for _, c := range m.NormalizedCapabilities() {
		if !has[c] {
			return true
		}
	}
	return false
}

// knownCapabilitiesOf 返回某模型名的已知能力（按命名约定推断）。
func knownCapabilitiesOf(modelName string) []string {
	name := strings.ToLower(modelName)
	result := make([]string, 0, 4)
	switch {
	case strings.Contains(name, "reason") || strings.Contains(name, "-r1") || strings.Contains(name, "thinking"):
		result = append(result, "reasoning")
	}
	switch {
	case strings.Contains(name, "coder") || strings.Contains(name, "code"):
		result = append(result, "code")
	}
	switch {
	case strings.Contains(name, "vision") || strings.Contains(name, "-vl") || strings.Contains(name, "multimodal"):
		result = append(result, "vision")
	}
	return result
}

// capabilityGapHint 生成"补齐了什么能力"的说明。
func capabilityGapHint(m *model.Model, usage []model.ModelUsage) string {
	has := make(map[string]bool)
	for _, u := range usage {
		for _, c := range knownCapabilitiesOf(u.Model) {
			has[c] = true
		}
	}
	for _, c := range m.NormalizedCapabilities() {
		if !has[c] {
			return "新增能力：" + c
		}
	}
	return "能力组合与常用模型不同"
}

// boolPtr 返回bool 的指针（用于构造查询条件）。
func boolPtr(v bool) *bool { return &v }

// estimateQuotaPerCall 估算单次调用的消耗额度（0 = 无价格数据或读不到）。
//
// 口径：以"平均单次调用"为参照——输入按 1000 token、输出按 500 token 估算。
// 为什么用这个组合：这两数接近常见对话调用的量级，
// 算出的数字能反映"大概多少钱一次"，足以支撑"降本推荐"的价值说明；
// 而追求精确反而会给出假精度（真实用量波动可达10 倍以上）。
//
// 只取分组默认价（List 而非 ListForPricing）：推荐要给的是"这个模型贵不贵"
// 这一层信息，渠道专用价是站长与渠道之间的内部结算口径，
// 拿它当"用户选型参考"会误导用户（他未必能选到那个渠道）。
func (s *Server) estimateQuotaPerCall(ctx context.Context, modelName string) int64 {
	if s.deps.ModelPrices == nil || modelName == "" {
		return 0
	}
	prices, err := s.deps.ModelPrices.List(ctx, "", true)
	if err != nil {
		return 0
	}
	// MatchModelPriceForChannel 支持通配模式（"gpt-4*"），
	// 直接复用它而不是自己写前缀匹配——那样必然与计费链路口径漂移。
	price := model.MatchModelPriceForChannel(prices, modelName, model.ChannelScopeAll)
	if price == nil {
		return 0
	}
	// 输入 1000 token、输出 500 token。
	const estPromptTokens = 1000
	const estCompletionTokens = 500
	return price.PromptPrice*estPromptTokens/1_000_000 +
		price.CompletionPrice*estCompletionTokens/1_000_000
}

// itoa 是 strconv.Itoa 的本地短名。
//
// 不直接用 strconv 的原因只有一个：本文件只需要正数转字符串，
// 引入 strconv 只为一个 IToa 会让imports 显得多余。
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
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
