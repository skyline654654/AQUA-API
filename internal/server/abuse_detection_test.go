// 本文件是「滥用检测」判定逻辑的单元测试。
//
// 意图（Why）：
//
//	detectBurst 是整个风控模块里唯一"会误伤真实用户"的地方——
//	它决定"是否记一条盗 Key 事件"。阈值一旦偏松就会漏报，
//	偏紧就会把重度用户的正常跑批当成攻击。因此它的每一条边界都必须钉住。
//
//	纯函数 + 表格驱动是刻意的写法：检测逻辑会随站点数据反复调参，
//	没有测试就没法放心改。
package server

import (
	"testing"

	"github.com/xiaosu4610/aqua-api/internal/model"
)

// buildBuckets 造一份"历史平稳 + 最后一桶突增/平稳"的逐小时序列。
//
// 参数：
//   - historyCount：历史桶数量（不含最后一桶）
//   - baseCount：历史各桶的请求数（全部相同）
//   - lastCount：最后一桶（当前小时）的请求数
func buildBuckets(historyCount int, baseCount, lastCount int64) []model.HourlyUsage {
	buckets := make([]model.HourlyUsage, 0, historyCount+1)
	for i := 0; i < historyCount; i++ {
		buckets = append(buckets, model.HourlyUsage{
			HourStart: int64(i) * 3600,
			Requests:  baseCount,
			Success:   baseCount,
		})
	}
	buckets = append(buckets, model.HourlyUsage{
		HourStart: int64(historyCount) * 3600,
		Requests:  lastCount,
		Success:   lastCount,
	})
	return buckets
}

func TestDetectBurst_NoTriggerOnStableUsage(t *testing.T) {
	// 历史稳定在 100 次/小时，当前也是 100 —— 绝不能判为异常。
	// 这是最重要的一条：误伤正常用户会让整个风控失去可信度。
	buckets := buildBuckets(100, 100, 100)
	if got := detectBurst(buckets); got.Triggered {
		t.Fatalf("平稳用量被判为异常: %+v", got)
	}
}

func TestDetectBurst_TriggersOnLargeSpike(t *testing.T) {
	// 历史 100 次/小时（≥25 的历史基数门槛），当前 5000 次 —— 50 倍，应触发。
	buckets := buildBuckets(100, 100, 5000)
	got := detectBurst(buckets)
	if !got.Triggered {
		t.Fatalf("50 倍突增未被检出")
	}
	if got.Metric != 5000 {
		t.Errorf("度量值 = %v, 期望 5000", got.Metric)
	}
	// 阈值应为 P99(100) × 5 = 500
	if got.Threshold != 500 {
		t.Errorf("阈值 = %v, 期望 500", got.Threshold)
	}
	if got.Detail == "" {
		t.Error("触发时必须给出人类可读说明（站长要据此判断）")
	}
}

func TestDetectBurst_InsufficientHistorySamples(t *testing.T) {
	// 只有 5 个历史桶（< 24 的最小样本门槛），即使当前是 100 倍也不判。
	// 理由：样本太少时 P99 约等于最大值，一次正常批量任务就会误报。
	buckets := buildBuckets(5, 10, 10000)
	if got := detectBurst(buckets); got.Triggered {
		t.Fatalf("历史样本不足却判为异常: %+v", got)
	}
}

func TestDetectBurst_LowBaselineNotTriggered(t *testing.T) {
	// 历史只有 5次/小时（< 25 的基数门槛），当前 200 次 —— 40 倍但基数太低。
	// 场景：用户从"偶尔用一下"变成"每天用"，这是活跃度提升不是被盗。
	buckets := buildBuckets(100, 5, 200)
	if got := detectBurst(buckets); got.Triggered {
		t.Fatalf("低基数放大被判为异常: %+v", got)
	}
}

func TestDetectBurst_SmallAbsoluteVolumeNotTriggered(t *testing.T) {
	// 历史 100 次/小时，当前 400 次（4 倍，未达5 倍阈值）—— 判为正常波动。
	buckets := buildBuckets(100, 100, 400)
	if got := detectBurst(buckets); got.Triggered {
		t.Fatalf("未达阈值的波动被判为异常: %+v", got)
	}
}

func TestDetectBurst_CurrentBelowAbsoluteFloor(t *testing.T) {
	// 历史 1000 次/小时，当前 30 次 —— 虽然远低于 P99，但仍达50 的绝对下限。
	// 关键：不判。因为突增检测只看"比历史多"，当前值很小说明用户没在刷。
	// （用例保留是为了钉住"不因'比历史少'而反向触发"这一语义。）
	buckets := buildBuckets(100, 1000, 30)
	if got := detectBurst(buckets); got.Triggered {
		t.Fatalf("当前值低于历史却被判为异常: %+v", got)
	}
}

func TestDetectBurst_EmptyAndSingleBucket(t *testing.T) {
	// 边界：空序列与单桶序列都必须安全返回（不能 panic）。
	if got := detectBurst(nil); got.Triggered {
		t.Error("空输入不应触发")
	}
	if got := detectBurst([]model.HourlyUsage{{Requests: 99999}}); got.Triggered {
		t.Error("单桶输入（无历史可比）不应触发")
	}
}

func TestPercentileInt64(t *testing.T) {
	tests := []struct {
		name   string
		values []int64
		p      float64
		want   int64
	}{
		{"空切片", nil, 0.99, 0},
		{"单元素取p1", []int64{7}, 0.99, 7},
		{"p0取最小", []int64{5, 1, 9}, 0, 1},
		{"p1取最大", []int64{5, 1, 9}, 1, 9},
		// 1..10 取 P99：pos = 0.99×9 = 8.91，
		// 插值 9×(1−0.91) + 10×0.91 = 9.91 → 四舍五入 10
		{"插值", []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.99, 10},
		// 中位数 1..5：pos = 0.5×4 = 2，整数位，插值退化为取该位
		{"中位数", []int64{1, 2, 3, 4, 5}, 0.5, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := percentileInt64(tt.values, tt.p); got != tt.want {
				t.Errorf("percentileInt64 = %v, 期望 %v", got, tt.want)
			}
		})
	}
}

func TestPercentileInt64_DoesNotMutateInput(t *testing.T) {
	// 必须拷贝后排序：调用方（扫描逻辑）拿到的桶序列还要按原顺序使用。
	values := []int64{3, 1, 2}
	_ = percentileInt64(values, 0.5)
	if values[0] != 3 || values[1] != 1 || values[2] != 2 {
		t.Errorf("percentileInt64 破坏了入参顺序: %v", values)
	}
}
