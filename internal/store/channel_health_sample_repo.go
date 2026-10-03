// 本文件是 model.ChannelHealthSampleRepository 的 SQL 实现。
//
// 意图（Why）：
//
//	这张表是动态权重的「输入端」。它有两个与其他表截然不同的约束：
//	  1. 写在【转发热路径】上——每条 usage_logs 落库时顺手累加一次，
//	     因此实现必须是单条 upsert，绝不能先查再写（那是竞态且多一次往返）；
//	  2. 只增不减——跑久了会累积成百万行，把「读一行」的性能优势吃光，
//	     所以必须配套清理（DeleteBefore）。
//
// 流转（Flow）：
//
//	relay 每条调用 → Accumulate（累加到当前 5 分钟桶）
//	  → 后台任务 ListCompleted(已封口桶) → EWMA → 写回 channels.weight
//	    → DeleteBefore(过老桶) 回收空间
//
// 扩展（Extend）：
//
//	需要按模型细分采样时：加model 列 + 改唯一索引为 (channel_id, model, bucket_start)，
//	Accumulate 的 ON CONFLICT 子句同步加一列即可，其余逻辑不变。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/xiaosu4610/aqua-api/internal/model"
)

// channelHealthSampleRepository 是渠道健康采样的 SQL 实现。
type channelHealthSampleRepository struct{ db *sql.DB }

// NewChannelHealthSampleRepository 创建渠道健康采样仓储。
func NewChannelHealthSampleRepository(db *sql.DB) model.ChannelHealthSampleRepository {
	return &channelHealthSampleRepository{db: db}
}

// Accumulate 向 (channel_id, bucket_start) 桶累加一次调用指标。
//
// 用INSERT ... ON CONFLICT DO UPDATE（upsert）而非"先SELECT 再INSERT/UPDATE"：
//   - 正确性：多核并发下"先查后写"会丢失累加（A 和 B 同时读到 0，各自写入 1，
//     结果桶里是 1 而不是 2），而upsert 由数据库保证原子累加；
//   - 性能：热路径上省掉一次 SELECT 往返。
//
// 为什么允许桶内数据不精确：EWMA 本身就是统计平滑，桶内少算一次请求的影响
// 远小于"漏掉整次累加"对权重趋势的破坏。
func (r *channelHealthSampleRepository) Accumulate(ctx context.Context, sample *model.ChannelSample) error {
	if sample == nil || sample.ChannelID == 0 {
		// 渠道 ID 为 0 表示系统级调用（无渠道），不参与渠道健康统计。
		return nil
	}

	now := time.Now()
	//桶起点对齐：写入前统一向下取整，避免调用方各自算一遍导致同一桶被拆成多行。
	bucket := sample.BucketStart
	if bucket <= 0 {
		bucket = now.Unix()
	}
	bucket = bucket - bucket%model.ChannelSampleBucketSeconds

	// 成功数做上界夹逼：调用方可能传入 requests=0 且success=1（脏数据），
	// 存进去会让成功率 > 1，进而让 EWMA 算出荒谬的健康分。
	success := sample.Success
	if success < 0 {
		success = 0
	}
	if success > sample.Requests && sample.Requests > 0 {
		success = sample.Requests
	}
	latency := sample.LatencySumMS
	if latency < 0 {
		latency = 0
	}

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO channel_health_samples
			(channel_id, bucket_start, requests, success, latency_sum_ms, quota, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (channel_id, bucket_start) DO UPDATE SET
			requests       = requests + excluded.requests,
			success        = success + excluded.success,
			latency_sum_ms = latency_sum_ms + excluded.latency_sum_ms,
			quota          = quota + excluded.quota,
			updated_at     = excluded.updated_at`,
		sample.ChannelID, bucket, sample.Requests, success, latency, sample.Quota, now.Unix(), now.Unix())
	if err != nil {
		return fmt.Errorf("store: 累加渠道健康采样失败: %w", err)
	}
	return nil
}

// ListCompleted 返回 bucket_start <= before 的桶（桶起点升序）。
//
// before 传入的是"当前时间 - 1 个桶"：只有这样才能保证读到的桶已经封口，
// 不会把正在写入的桶读进来（那会让成功率偏高，形成"越检查越健康"的假象）。
func (r *channelHealthSampleRepository) ListCompleted(ctx context.Context, before time.Time, limit int) ([]*model.ChannelSample, error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT channel_id, bucket_start, requests, success, latency_sum_ms, quota, updated_at
		FROM channel_health_samples
		WHERE bucket_start <= ?
		ORDER BY bucket_start ASC, channel_id ASC
		LIMIT ?`, before.Unix(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: 查询渠道健康采样失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make([]*model.ChannelSample, 0, limit)
	for rows.Next() {
		item := &model.ChannelSample{}
		var updatedAt int64
		if err := rows.Scan(&item.ChannelID, &item.BucketStart, &item.Requests,
			&item.Success, &item.LatencySumMS, &item.Quota, &updatedAt); err != nil {
			return nil, fmt.Errorf("store: 读取渠道健康采样失败: %w", err)
		}
		item.UpdatedAt = time.Unix(updatedAt, 0)
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历渠道健康采样失败: %w", err)
	}
	return result, nil
}

// DeleteBefore 清理早于 before 的桶，返回删除行数。
func (r *channelHealthSampleRepository) DeleteBefore(ctx context.Context, before time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		"DELETE FROM channel_health_samples WHERE bucket_start < ?", before.Unix())
	if err != nil {
		return 0, fmt.Errorf("store: 清理渠道健康采样失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		// 拿不到行数不是错误：调用方只用它打日志，不参与控制流。
		return 0, nil
	}
	return affected, nil
}
