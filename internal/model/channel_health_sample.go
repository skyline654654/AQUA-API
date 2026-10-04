// 本文件定义「渠道健康采样」的领域模型与仓储接口。
//
// 意图（Why）：
//
//	渠道权重目前是人工静态配置的：设错就得手动调，而且「调完就定住」。
//	上游渠道的质量是随时间漂移的——密钥被限流、上游涨价、上游故障、
//	甚至同一渠道在不同时段的承载能力都不同。人工权重无法跟随这种漂移，
//	结果就是「好渠道没被优先用、坏渠道还在承接流量」。
//
//	动态权重需要持续采样才能算，但采样方式有讲究：
//	EWMA（指数加权移动平均）只需要【最近窗口】的统计，
//	而usage_logs 是逐请求明细（增长极快），每轮调权重都扫明细会越来越慢。
//	因此把窗口聚合【前置到写入路径】：每次调用落日志时顺手往采样表加一次，
//	查询就变成「读一行」，天然支撑「低频后台任务 + 高频选路」的分离。
//
// 流转（Flow）：
//
//	relay 落usage_logs（同一事务外，失败不阻塞主链路）
//	  → Sample.Accumulate 累加到 (channel_id, 5分钟桶)
//	    → 后台任务每轮 ListCompleted 读已封口的桶
//	      → 计算 EWMA（成功率 / 延迟 / 成本三因子）
//	        → 写回 channels.weight（受最小/最大权重夹逼保护）
//
// 扩展（Extend）：
//
//	需要更细的粒度（如按模型分别采样）时，为Sample 加 Model 字段并调整唯一索引；
//	采样周期要改时，只改 BucketSeconds 一个常量即可（写入与读取共用它）。
package model

import (
	"context"
	"time"
)

// ChannelSampleBucketSeconds 是采样桶的时间粒度（秒）。
//
// 为什么是 5 分钟：
//   - 太短（如 1 分钟）：单个桶样本太少，一次网络抖动就能把 EWMA 打飞，
//     权重会剧烈抖动，反而伤害稳定性；
//   - 太长（如 1 小时）：反应太慢，渠道已经坏了却要一小时后才降权。
//
// 5 分钟是「抖动不至于主导」与「故障及时止损」之间的折中，
// 且与后台任务周期（10 分钟）形成 2:1 的采样/消费比例，保证每轮都有完整桶可读。
const ChannelSampleBucketSeconds = 5 * 60

// ChannelSample 是某个渠道在某个时间桶内的累计健康指标。
//
// 语义：桶内只做累加（请求数、成功数、延迟和、额度消耗），
// 不在桶内计算比率——比率要跨桶才有意义（EWMA 正是跨桶的）。
type ChannelSample struct {
	ChannelID uint64
	// BucketStart 是桶起点（Unix 秒，必然对齐到 ChannelSampleBucketSeconds 的整数倍）。
	BucketStart int64
	Requests    int64
	Success     int64
	// LatencySumMS 是桶内所有请求的总耗时之和。
	// 存「和」而不是「平均」：平均不可累加（两个桶平均的平均不等于总平均），
	// 只有存和才能在跨桶聚合时正确还原整体平均延迟。
	LatencySumMS int64
	// Quota 是桶内消耗额度，作为「成本」维度的代理指标。
	Quota     int64
	UpdatedAt time.Time
}

// SuccessRate 返回桶内成功率（0~1）；无请求时返回 1。
//
// 无请求返回 1 而非 0 是刻意选择：EWMA 里「没数据」应当表示「没有负面证据」，
// 若返回 0 会让空闲渠道的权重被一路压到底。
func (s *ChannelSample) SuccessRate() float64 {
	if s == nil || s.Requests <= 0 {
		return 1
	}
	return float64(s.Success) / float64(s.Requests)
}

// AvgLatencyMS 返回桶内平均延迟（毫秒）；无请求时返回 0。
func (s *ChannelSample) AvgLatencyMS() float64 {
	if s == nil || s.Requests <= 0 {
		return 0
	}
	return float64(s.LatencySumMS) / float64(s.Requests)
}

// ChannelHealthSampleRepository 定义渠道健康采样的持久化操作。
type ChannelHealthSampleRepository interface {
	// Accumulate 向 (channelID, bucketStart) 桶累加一次调用指标。
	//
	// 幂等性说明：这是【累加】而非【覆盖】，因此不能靠重放去重——
	// 调用方每条日志只能累加一次。本方法在转发热路径上被调用，
	// 实现必须走唯一索引 upsert（单条 INSERT ... ON CONFLICT DO UPDATE），
	// 不允许先SELECT 再决定写不写（那是竞态）。
	Accumulate(ctx context.Context, sample *ChannelSample) error

	// ListCompleted 返回 bucketStart <= before 的桶（按桶起点升序）。
	//
	// 语义是「已封口」：只有整个桶都过去了才算完整，
	// 否则会把正在写入的桶读进来，得到的成功率是偏高的假象。
	ListCompleted(ctx context.Context, before time.Time, limit int) ([]*ChannelSample, error)

	// DeleteBefore 清理早于 before 的桶，返回删除行数。
	//
	// 必须有清理：采样表只增不减，跑上几个月就会累积成百万行，
	// 把「读一行」的优势吃光。默认保留 7 天足够 EWMA 收敛。
	DeleteBefore(ctx context.Context, before time.Time) (int64, error)
}
