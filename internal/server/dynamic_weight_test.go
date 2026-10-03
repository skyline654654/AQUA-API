// 本文件是「动态权重」核心计算的单元测试。
//
// 意图（Why）：
//
//	healthScore / advanceEWMA / computeDynamicWeight 三个纯函数直接决定
//	"哪个渠道承接多少流量"。它们出错的后果是全站流量分配失衡——
//	要么好渠道被冷落（浪费钱），要么坏渠道承接大量流量（拖慢所有请求）。
//	而这类错误在生产上往往要到"某个渠道账单异常"才被发现。
//
//	因此把每个数值边界都钉成测试，让调参变成"改常量 + 看测试是否仍绿"。
package server

import (
	"math"
	"testing"

	"github.com/xiaosu4610/aqua-api/internal/model"
)

func TestHealthScore_PerfectChannel(t *testing.T) {
	// 全部成功、零延迟、零成本 —— 应该是满分 1.0。
	// 注意零成本会得到 costPart = 1/(1+0) = 1，延迟 0 得到 latencyPart = 1。
	//
	// 断言用近似而非精确相等：0.7+0.2+0.1 在 IEEE754 下是 0.9999999999999999，
	// 精确比较会制造一个与业务无关的假失败。
	sample := &model.ChannelSample{Requests: 100, Success: 100, LatencySumMS: 0, Quota: 0}
	if got := healthScore(sample); math.Abs(got-1.0) > 1e-9 {
		t.Errorf("完美渠道健康分 = %v, 期望 1.0", got)
	}
}

func TestHealthScore_AllFailedIsZero(t *testing.T) {
	// 全部失败必须【严格为 0】，不能是"接近 0"。
	//
	// 这条比"数值小"更强：早先的加权实现（0.7×成功率 + 0.2×延迟 + 0.1×成本）
	// 会让全失败渠道靠"延迟快"拿到 0.3 分，换算成权重仍在承接流量——
	// 一个必然失败的渠道本该只保留最低存在感。改成成功率乘性后才是 0。
	sample := &model.ChannelSample{Requests: 100, Success: 0, LatencySumMS: 0, Quota: 0}
	if got := healthScore(sample); got != 0 {
		t.Errorf("全失败渠道健康分 = %v, 期望严格为 0（乘性因子的核心保证）", got)
	}
}

func TestHealthScore_SuccessDominatesLatency(t *testing.T) {
	// 两个渠道：成功但极慢vs 失败但很快。
	// 前者必须得分更高——站长最不能接受的是"钱花了没拿到服务"，
	// "慢一点"只是体验问题。这条断言钉住"成功率权重 0.7 > 延迟权重 0.2"。
	slowButOK := healthScore(&model.ChannelSample{Requests: 100, Success: 100, LatencySumMS: 100 * 10000})
	fastButFailing := healthScore(&model.ChannelSample{Requests: 100, Success: 50, LatencySumMS: 100 * 10})
	if slowButOK <= fastButFailing {
		t.Errorf("慢但成功(%.4f) 未优于快但半数失败(%.4f)", slowButOK, fastButFailing)
	}
}

func TestHealthScore_NoRequestsIsNeutral(t *testing.T) {
	// 无请求返回 1（"没有负面证据"），而不是 0。
	// 若返回 0，空闲渠道的 EWMA 会被一路压到底，
	// 等它真正需要用时权重已经是最小值。
	if got := healthScore(&model.ChannelSample{Requests: 0}); got != 1.0 {
		t.Errorf("无请求的健康分 = %v, 期望 1.0", got)
	}
	if got := healthScore(nil); got != 1.0 {
		t.Errorf("nil 采样的健康分 = %v, 期望 1.0", got)
	}
}

func TestHealthScore_ClampedToUnitRange(t *testing.T) {
	// 构造一个"不可能但若出现也不得越界"的输入：成功率>1（脏数据）。
	// 实现里已把success 夹逼到 ≤ requests，此处再验证夹逼后的结果不越界。
	weird := &model.ChannelSample{Requests: 10, Success: 10, LatencySumMS: -500, Quota: -100}
	got := healthScore(weird)
	if got < 0 || got > 1 {
		t.Errorf("健康分越界: %v", got)
	}
}

func TestAdvanceEWMA_FirstSampleInitializesDirectly(t *testing.T) {
	// 首次样本应直接取该值，而不是从 0 慢慢爬。
	// 从 0 爬意味着新渠道前几轮几乎拿不到流量（权重≈0）。
	state := &dynamicWeightState{}
	advanceEWMA(state, 0.8, dynamicWeightAlpha)
	if !state.Inited {
		t.Fatal("首次推进后应标记为已初始化")
	}
	if state.Score != 0.8 {
		t.Errorf("首次分数 = %v, 期望 0.8（直接取样本值）", state.Score)
	}
}

func TestAdvanceEWMA_ConvergesToNewValue(t *testing.T) {
	// 连续推入 20 次同一个值，应收敛到该值附近。
	// 这是 EWMA 的核心性质：不收敛的平滑器会让权重永远滞后于真实健康度。
	state := &dynamicWeightState{}
	for i := 0; i < 20; i++ {
		advanceEWMA(state, 0.9, dynamicWeightAlpha)
	}
	if diff := state.Score - 0.9; diff > 0.01 || diff < -0.01 {
		t.Errorf("20 次同值推进后分数 = %v, 期望接近 0.9", state.Score)
	}
}

func TestAdvanceEWMA_SmoothsAbruptChange(t *testing.T) {
	// 从 1.0 突然跌到 0.0，一次推进后不应直接归零——
	// 这正是用 EWMA 而非滑动窗口的意义：避免单次抖动造成权重剧烈跳变。
	state := &dynamicWeightState{}
	advanceEWMA(state, 1.0, dynamicWeightAlpha) // 初始化为 1.0
	advanceEWMA(state, 0.0, dynamicWeightAlpha) // 一次崩溃样本
	//期望 = 0.3×0 + 0.7×1.0 = 0.7
	if diff := state.Score - 0.7; diff > 0.001 || diff < -0.001 {
		t.Errorf("单次崩溃后分数 = %v, 期望 0.7（应平滑而非归零）", state.Score)
	}
}

func TestAdvanceEWMA_NilStateIsSafe(t *testing.T) {
	// nil 状态必须安全返回（不 panic）——后台任务不该因边缘输入崩掉。
	advanceEWMA(nil, 0.5, dynamicWeightAlpha)
}

func TestComputeDynamicWeight_LinearMapping(t *testing.T) {
	tests := []struct {
		score float64
		want  int
	}{
		// 健康分 1 → Ceiling（压倒性优先）
		{1.0, dynamicWeightCeiling},
		// 健康分 0.5 → 区间中点
		{0.5, (dynamicWeightFloor + dynamicWeightCeiling) / 2},
		// 健康分 0 → Floor（与其它渠道同等的最低存在感，而非完全消失）
		{0.0, dynamicWeightFloor},
	}
	for _, tt := range tests {
		if got := computeDynamicWeight(tt.score); got != tt.want {
			t.Errorf("computeDynamicWeight(%v) = %d, 期望 %d", tt.score, got, tt.want)
		}
	}
}

// TestComputeDynamicWeight_FullRangeIsUsable 钉住"映射区间必须有区分度"。
//
// 早先的实现是 weight = Base × score（Base=10、score≤1），
// 于是权重恒在 [0, 10]、永远够不到 Max=100，Max 成了死代码；
// 而"最健康"与"被夹逼的最差"都会落在同一量级，动态调节形同虚设。
// 这条测试防止同类退化再次发生。
func TestComputeDynamicWeight_FullRangeIsUsable(t *testing.T) {
	best := computeDynamicWeight(1.0)
	worst := computeDynamicWeight(0.0)
	if best <= worst {
		t.Errorf("最健康(%d) 未优于最差(%d)：映射区间失去区分度", best, worst)
	}
	if best != dynamicWeightMax {
		t.Errorf("健康分满分应映射到 Max(%d)，实际 %d（说明上界是死代码）",
			dynamicWeightMax, best)
	}
}

func TestComputeDynamicWeight_ClampsOutOfRange(t *testing.T) {
	// 越界输入必须被夹逼，绝不能返回超出边界的权重
	// （超过 Max 会让一次误判的后果被放大；低于 Floor 则低于设计下限）。
	//
	// 注意 score=-0.5 的期望是 Floor（不是 Min）：先把健康分夹到 [0,1]，
	// 再线性映射到 [Floor, Ceiling] —— 两步顺序决定了负值落到区间下端。
	if got := computeDynamicWeight(1.5); got != dynamicWeightMax {
		t.Errorf("score=1.5 → %d, 期望夹逼到 %d", got, dynamicWeightMax)
	}
	if got := computeDynamicWeight(-0.5); got != dynamicWeightFloor {
		t.Errorf("score=-0.5 → %d, 期望夹逼到 %d", got, dynamicWeightFloor)
	}
}

func TestSortChannelSamples_AscendingByBucket(t *testing.T) {
	items := []*model.ChannelSample{
		{BucketStart: 300},
		{BucketStart: 100},
		{BucketStart: 200},
	}
	sortChannelSamples(items)
	if items[0].BucketStart != 100 || items[1].BucketStart != 200 || items[2].BucketStart != 300 {
		t.Errorf("排序结果 = %v, 期望按桶起点升序", items)
	}
}
