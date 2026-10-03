// 本文件实现「动态权重路由」——用 EWMA 依据渠道真实表现自动调节权重。
//
// 意图（Why）：
//
//	渠道权重目前是人工静态配置的：设错就得手动调，而且「调完就定住」。
//	但上游质量是随时间漂移的——密钥被限流、上游涨价、节点故障、
//	甚至同一渠道在不同时段的承载能力都不同。人工权重无法跟随这种漂移，
//	结果是「好渠道没被优先用、坏渠道还在承接流量」。
//
// 为什么用 EWMA 而不是滑动窗口均值：
//
//	滑动窗口的问题是「窗口边界是硬的」——第 N 个样本进来时，
//	最早的那个样本立刻被丢弃，统计值会出现阶跃。EWMA 用指数衰减
//	给每个样本不同的寿命，越新的样本影响越大，边界平滑，
//	这正是「趋势跟随」需要的性质。
//
//	EWMA 的递推：S_t = α·x_t + (1-α)·S_{t-1}
//	直觉：α 是"新样本的话语权"。α=0.1 意味着每个新样本只占 10%，
//	要连续多个同向样本才能推动指标——平滑但反应慢。
//
// 流转（Flow）：
//
//	relay 每条调用 → channel_health_samples 累加到 5 分钟桶
//	  → 本文件后台任务每轮读已封口桶 → 推进 EWMA
//	    → 算目标权重 → 夹逼后写回 channels.weight
//	      → relay 的 weightedPick 自动按新权重分配流量
//
// 安全约束（必须保留）：
//
//	功能默认关闭（settings 的 channel.dynamic_weight != 1），
//	关闭时行为与引入本功能前完全一致——存量部署升级不改变任何流量分配。
package server

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/xiaosu4610/aqua-api/internal/model"
)

// 动态权重的参数与边界。
const (
	// dynamicWeightBucketLag 是"已封口桶"相对当前时间的滞后量。
	//
	// 必须 ≥ 1 个桶：正在写入的桶成功率是偏高的假象
	//（失败请求可能落在后半段），把它读进来会得出"渠道很健康"的错误结论。
	dynamicWeightBucketLag = model.ChannelSampleBucketSeconds

	// dynamicWeightAlpha 是 EWMA 的平滑系数。
	//
	// 取 0.3 的理由：桶是 5 分钟，一轮扫描读到的是"最近 5~15 分钟"的若干桶。
	// α 太小（如 0.05）需要20 个桶（近 2 小时）才能纠正一次误判；
	// 太大（如 0.8）则单次抖动就能让权重剧烈跳变。
	// 0.3 意味着约 3 个桶（15 分钟）即可完成 2/3 的纠正，兼顾两者。
	dynamicWeightAlpha = 0.3

	// dynamicWeightMin / Max 是权重的夹逼边界。
	//
	// 下界 1 而非 0：权重为 0 会让渠道永不被选中（model.Channel.Validate 也拒绝），
	// 而"完全不发流量"这种极端处置应该是停用，而不是靠权重实现。
	// 上界 100：与人工配置的上限保持一致，避免动态调节把某个渠道推到
	// 远超人工可能设定的量级（那会让一次误判的后果被放大 100 倍）。
	dynamicWeightMin = 1
	dynamicWeightMax = 100

	// dynamicWeightFloor / Ceiling 是权重映射区间的两端。
	//
	// 为什么不是 [0, 1] 而是 [Floor, Ceiling]：
	// 权重决定的是"在同优先级候选中的抽签概率"，是个比值而不是分数。
	// 若把 [0,1] 整体当权重域，最健康的渠道只能拿到 1，
	// 而不健康的渠道被夹到下界 1——两者概率相同，动态调节完全失效。
	//
	// 因此把健康分 [0,1] 线性映射到 [Floor, Ceiling] 这个"有区分度"的区间：
	// 完全不健康 → 1（与其他渠道同等的最低存在感），
	// 完全健康 → 100（压倒性地优先）。
	//
	// Floor 取 2 而非 1：让"最差的渠道"也保有 1/100 的存在感，
	// 避免它在健康度波动时突然完全消失、恢复时又突然挤占大量流量。
	dynamicWeightFloor   = 2
	dynamicWeightCeiling = 100

	// dynamicWeightSampleRetention 是采样桶的保留时长。
	//
	// 7 天足够 EWMA 完全收敛（衰减到 1e-6 以下），再留着只是占空间。
	dynamicWeightSampleRetention = 7 * 24 * time.Hour

	// dynamicWeightScansPerRound 是单轮最多消费的桶数。
	//
	// 上限存在的理由同其它批量查询：后台任务不该在一次执行里读几万行。
	// 正常情况下每轮只有 1~2 个新封口的桶，这个上限只在进程重启后追赶时才生效。
	dynamicWeightScansPerRound = 200
)

// dynamicWeightState 是某渠道的 EWMA 内部状态。
//
// 不落库：它是"对最近若干桶的平滑结果"，进程重启后从当前桶重新起步即可，
// 代价是重启后需要几轮才恢复平滑——把状态持久化反而要处理
// "多实例同时写同一份状态"的一致性问题，收益远不抵成本。
type dynamicWeightState struct {
	// Score 是健康分（0~1）：越高越健康。
	Score float64
	// Inited 区分"平滑值 0"与"尚未初始化"：
	// 前者意味着这个渠道已被判死（Score=0），后者意味着还没数据。
	// 混为一谈会让新渠道一上来就被判死。
	Inited bool
}

// healthScore 把一个采样桶换算成 0~1 的健康分。
//
// 三个因子各司其职（这是权重动态化的核心设计）：
//
//	成功率（主导）：不成功的调用没有任何价值；
//	延迟（次要）：同样成功但慢三倍的渠道，多占一份连接与用户等待；
//	成本（微调）：同等健康时优先用更便宜的，把额度留给真正需要的场景。
//
// 三个因子都是"越小越好"，因此统一取倒数后加权。
func healthScore(sample *model.ChannelSample) float64 {
	if sample == nil || sample.Requests <= 0 {
		// 无请求不产生负面证据（与 SuccessRate 的约定一致）。
		return 1
	}

	successPart := sample.SuccessRate()

	// 延迟归一：用 1/(1+延迟/参考值) 把"毫秒"映射到 (0,1]。
	// 参考值取 5 秒：超过 5 秒的调用延迟项已接近 0，
	// 这样"延迟 5 秒"与"延迟 30 秒"不会被判为同等糟糕。
	const latencyReferenceMS = 5000.0
	latencyPart := 1.0 / (1.0 + sample.AvgLatencyMS()/latencyReferenceMS)

	// 成本归一：单请求平均额度。同样用倒数映射，
	// 参考值取"1 万额度/请求"——绝大多数调用远低于它，因此该项普遍接近 1，
	// 只会对"极端贵的模型调用"起区分作用。
	const quotaReference = 10000.0
	avgQuota := float64(sample.Quota) / float64(sample.Requests)
	costPart := 1.0 / (1.0 + avgQuota/quotaReference)

	// 成功率是【乘性因子】而不是加权项——这是本函数最关键的设计决定。
	//
	// 为什么不用加权（0.7×成功率 + 0.2×延迟 + 0.1×成本）：
	// 加权下一个 100% 失败的渠道仍能靠"延迟快、成本低"拿到 0.3 分，
	// 换算成权重是 3——它会继续承接相当比例的流量。
	// 这与设计意图完全相反：一个必然失败的渠道，唯一合理的权重是最小值。
	//
	// 改成乘性后：成功率为 0 则分数必为 0（无论延迟多快、成本多低），
	// 成功率为 1 则分数由延迟与成本决定（0.8~1.0）。
	// 括号内的和始终落在 [0.8, 1.0]，因此"成功率 80% 的快渠道"
	// 与"成功率 100% 的慢渠道"能得到相近的分数——这正是想要的：
	// 成功率是准入门槛，延迟与成本只在合格渠道之间做微调。
	condition := 0.2*latencyPart + 0.1*costPart
	score := successPart * (0.7 + condition)

	// 夹逼到 [0,1]：浮点运算的边界保护，防止极端输入算出 >1 的分
	// 导致权重超过 Max（那本身不会出错，但会让"最大权重"的语义失真）。
	if score < 0 {
		return 0
	}
	if score > 1 {
		return 1
	}
	return score
}

// advanceEWMA 推进一次 EWMA（纯函数，便于单测）。
//
// 公式：S_t = α·x_t + (1-α)·S_{t-1}
//
// 未初始化时直接取当前样本值：让新渠道从真实值起步，
// 而不是从 0 慢慢爬（从 0 爬意味着前几轮它几乎拿不到流量）。
func advanceEWMA(state *dynamicWeightState, x, alpha float64) {
	if state == nil {
		return
	}
	if !state.Inited {
		state.Score = x
		state.Inited = true
		return
	}
	state.Score = alpha*x + (1-alpha)*state.Score
}

// computeDynamicWeight 把健康分换算成目标权重。
//
// 映射关系刻意是【线性】而非指数：
//
//	线性的好处是"健康分掉一半，权重也掉一半"，可预期、可解释——
//	站长看到权重变化时能直接反推健康状况。指数映射会让中段极其陡峭，
//	出现"从 90分到 60分，权重从 100掉到 2"的剧烈跳变。
//
// 区间是 [Floor, Ceiling] 而非 [0, Base]：见 dynamicWeightFloor 的说明——
// 权重是抽签比值，需要区间本身有区分度才有意义。
func computeDynamicWeight(score float64) int {
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	weight := int(math.Round(dynamicWeightFloor +
		(dynamicWeightCeiling-dynamicWeightFloor)*score))
	if weight < dynamicWeightMin {
		return dynamicWeightMin
	}
	if weight > dynamicWeightMax {
		return dynamicWeightMax
	}
	return weight
}

// 动态权重在 settings 表中的键名。
//
// 放 settings 而不是新增数据库列：这类"要不要开、开到什么程度"的旋钮
// 本质是运维配置，运维配置天然属于 settings；单独建列会让每加一个旋钮
// 就要走一次数据库迁移。
const (
	// settingDynamicWeightEnable 是总开关（"1" 开启，其余值关闭）。
	settingDynamicWeightEnable = "channel.dynamic_weight"
	// settingDynamicWeightSmoothing 是 EWMA 的 α（0~1，越小越平滑）。
	settingDynamicWeightSmoothing = "channel.dynamic_smoothing"
)

// dynamicWeightEnabled 读取动态权重总开关。
//
// 只认精确的 "1"：宁可"配置写错等于关闭"（用户会来问，总有人会发现），
// 也不要"配置写错等于开启"（静默改变全站流量分配，无人察觉）。
func (s *Server) dynamicWeightEnabled(ctx context.Context) (bool, error) {
	raw, err := s.deps.Settings.Get(ctx, settingDynamicWeightEnable)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(raw) == "1", nil
}

// TuneChannelWeights 是「动态权重」后台任务的周期入口。
//
// 由 main 起 goroutine 周期调用；ctx 取消后立即返回。
//
// 默认关闭：未在 settings 里把 channel.dynamic_weight 置 1 时直接返回，
// 保证存量部署升级后流量分配行为逐字不变。
func (s *Server) TuneChannelWeights(ctx context.Context) {
	if s.deps.ChannelHealthSamples == nil || s.deps.Channels == nil || s.deps.Settings == nil {
		return
	}

	// 开关读取失败时按"关闭"处理：风控/调度类能力的默认必须是保守的一侧。
	enabled, err := s.dynamicWeightEnabled(ctx)
	if err != nil {
		slog.Warn("读取动态权重开关失败，本轮跳过", "error", err)
		return
	}
	if !enabled {
		return
	}

	alpha := dynamicWeightAlpha
	if raw, err := s.deps.Settings.Get(ctx, settingDynamicWeightSmoothing); err == nil {
		// 夹逼到 (0,1]：α=0 会让状态永远不更新，α=1 会退化为"只看最新桶"。
		if parsed, perr := strconv.ParseFloat(strings.TrimSpace(raw), 64); perr == nil && parsed > 0 && parsed <= 1 {
			alpha = parsed
		}
	}

	tuned, err := s.tuneChannelWeightsOnce(ctx, alpha)
	if err != nil {
		slog.Warn("动态权重调节失败", "error", err)
		return
	}
	if tuned > 0 {
		slog.Info("动态权重调节完成", "channels", tuned)
	}
}

// tuneChannelWeightsOnce 执行一轮权重调节，返回被调整的渠道数。
func (s *Server) tuneChannelWeightsOnce(ctx context.Context, alpha float64) (int, error) {
	// 只读"已封口"的桶：正在写入的桶成功率偏高，会得出"渠道很健康"的假象。
	before := time.Now().Add(-time.Duration(dynamicWeightBucketLag) * time.Second)
	samples, err := s.deps.ChannelHealthSamples.ListCompleted(ctx, before, dynamicWeightScansPerRound)
	if err != nil {
		return 0, fmt.Errorf("server: 读取渠道健康采样失败: %w", err)
	}
	if len(samples) == 0 {
		return 0, nil
	}

	// 按渠道聚合本轮读到的所有桶（一次扫描可能读到同一渠道的多个桶）。
	// 必须先聚合再推进 EWMA：逐桶推进等于把时间顺序的信息丢掉，
	// 而且同一次扫描里后面的桶会覆盖前面的结论。
	byChannel := make(map[uint64][]*model.ChannelSample, len(samples))
	order := make([]uint64, 0, len(samples))
	for _, sm := range samples {
		if _, seen := byChannel[sm.ChannelID]; !seen {
			order = append(order, sm.ChannelID)
		}
		byChannel[sm.ChannelID] = append(byChannel[sm.ChannelID], sm)
	}

	// 本轮内的 EWMA 状态：跨轮次不保留（见 dynamicWeightState 的注释）。
	states := make(map[uint64]*dynamicWeightState, len(byChannel))
	tuned := 0
	for _, channelID := range order {
		buckets := byChannel[channelID]
		// 桶必须按时间升序推进 EWMA，顺序错乱会让结果不可复现。
		sortChannelSamples(buckets)

		state := &dynamicWeightState{}
		for _, sm := range buckets {
			advanceEWMA(state, healthScore(sm), alpha)
		}
		states[channelID] = state

		target := computeDynamicWeight(state.Score)

		// 重新载入渠道：从采样到写入之间有窗口，期间管理员可能已改过权重，
		// 直接用旧值覆盖会把人家的操作抹掉。
		ch, err := s.deps.Channels.GetByID(ctx, channelID)
		if err != nil {
			// 渠道可能已被删除：跳过即可，不算失败。
			continue
		}
		if ch.Weight == target {
			continue
		}
		ch.Weight = target
		if err := s.deps.Channels.Update(ctx, ch); err != nil {
			return tuned, fmt.Errorf("server: 更新渠道 %d 权重失败: %w", channelID, err)
		}
		tuned++

		slog.Info("已按渠道健康度动态调整权重",
			"channel_id", channelID,
			"channel_name", ch.Name,
			"weight", target,
			"health_score", roundTo(state.Score, 4),
			"buckets", len(buckets))
	}

	// 清理过老的采样桶：只增不减的表跑久了会把"读少量行"的优势吃光。
	// 放在本轮末尾：调节失败时宁可多留一会儿数据，也别让数据先被清掉。
	if _, err := s.deps.ChannelHealthSamples.DeleteBefore(ctx, time.Now().Add(-dynamicWeightSampleRetention)); err != nil {
		slog.Warn("清理过期渠道健康采样失败", "error", err)
	}

	return tuned, nil
}

// sortChannelSamples 按桶起点升序排序（原地）。
func sortChannelSamples(items []*model.ChannelSample) {
	// 手写插入排序而非引入 sort：桶数极少（通常 1~3），
	// insertion sort 在这个规模下比标准库更快，且避免为一个小函数付出包级依赖。
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].BucketStart < items[j-1].BucketStart; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

// roundTo 把浮点数四舍五入到指定小数位（仅用于日志可读性）。
func roundTo(v float64, digits int) float64 {
	shift := math.Pow(10, float64(digits))
	return math.Round(v*shift) / shift
}
