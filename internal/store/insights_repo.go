// 本文件是「成本归因」「滥用检测」「用量预警」三者的 SQL 实现。
//
// 意图（Why）：
//
//	这三类查询都围绕 usage_logs，但查询形态不同：
//	  - 归因：按标签 GROUP BY，需要算占比（要拿到总量）；
//	  - 滥用：按小时分桶返回时序，用于与历史分布比对；
//	  - 预警：按用户列出预警记录，需要跨表读。
//	因此放在同一文件——它们共享同一份数据源与同一批索引，
//	拆成三个文件只会让"这几个查询为什么总是一起改"这件事更难看出来。
//
// 流转（Flow）：
//
//	relay 落 usage_logs（带 tag）
//	  → CostByTag / UserHourlyRequests 读明细做聚合
//	    → server 层归因看板 / 滥用检测 / 预警服务消费
//
// 扩展（Extend）：
//
//	新增时间维度（按周/按月）时复用同一套 SQL，仅改 GROUP BY 的时间表达式。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/xiaosu4610/aqua-api/internal/model"
)

// 三类运营查询共用 *sql.DB 与同一组索引，但方法名会撞车
// （AbuseEvent 与 AlertNotification 都有 Create/ListRecent）。
// 因此拆成三个独立实现体，各自只实现一个接口——
// 这样每个文件读起来也只有一件事，符合本项目「单一职责」的分层纪律。
type (
	// costRepository 实现成本归因。
	costRepository struct{ db *sql.DB }
	// abuseRepository 实现滥用检测的事件存储。
	abuseRepository struct{ db *sql.DB }
	// alertRepository 实现预警记录存储。
	alertRepository struct{ db *sql.DB }
)

// NewCostRepository 创建成本归因仓储。
func NewCostRepository(db *sql.DB) model.CostRepository { return &costRepository{db: db} }

// NewAbuseRepository 创建滥用事件仓储。
func NewAbuseRepository(db *sql.DB) model.AbuseRepository { return &abuseRepository{db: db} }

// NewAlertRepository 创建预警记录仓储。
func NewAlertRepository(db *sql.DB) model.AlertRepository { return &alertRepository{db: db} }

/* ───────────────────────── 成本归因 ───────────────────────── */

// CostByTag 聚合用户在时间窗内按场景标签切分的用量。
//
// 占比需要两个聚合（分组 + 总量），这里用一次查询取回分组结果后在 Go 内算：
// 总量 = 各组之和，与 SQL 再跑一次的结果一致，但省一次往返，
// 且避免两次查询之间数据变动导致分母分子口径不一致。
func (r *costRepository) CostByTag(ctx context.Context, userID uint64, since, until time.Time) ([]model.CostAttribution, error) {
	sb := `SELECT tag, COUNT(1),
			COALESCE(SUM(total_tokens), 0),
			COALESCE(SUM(quota), 0)
		FROM usage_logs
		WHERE user_id = ? AND created_at >= ?`
	args := []any{userID, since.Unix()}
	if !until.IsZero() {
		sb += " AND created_at < ?"
		args = append(args, until.Unix())
	}
	// 按额度倒序：站长最先要看到的是"最烧钱的场景"。
	// 额度相同时按标签升序，保证结果稳定可复现（避免同值行每次刷新顺序抖动）。
	sb += " GROUP BY tag ORDER BY SUM(quota) DESC, tag ASC"

	rows, err := r.db.QueryContext(ctx, sb, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 成本归因查询失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make([]model.CostAttribution, 0, 8)
	var totalQuota int64
	for rows.Next() {
		var item model.CostAttribution
		if err := rows.Scan(&item.Tag, &item.Requests, &item.Tokens, &item.Quota); err != nil {
			return nil, fmt.Errorf("store: 读取成本归因结果失败: %w", err)
		}
		totalQuota += item.Quota
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历成本归因结果失败: %w", err)
	}

	// 回填占比与单次均量。
	for i := range result {
		if totalQuota > 0 {
			result[i].Share = float64(result[i].Quota) / float64(totalQuota)
		}
		if result[i].Requests > 0 {
			result[i].AvgTokens = float64(result[i].Tokens) / float64(result[i].Requests)
		}
	}
	return result, nil
}

/* ───────────────────────── 滥用检测 ───────────────────────── */

// Create 落一条异常事件。
func (r *abuseRepository) Create(ctx context.Context, event *model.AbuseEvent) error {
	if event == nil {
		return errors.New("store: 异常事件不能为空")
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now()
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO abuse_events (user_id, token_id, kind, severity, action, detail, metric, threshold, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.UserID, event.TokenID, event.Kind, event.Severity, event.Action,
		event.Detail, event.Metric, event.Threshold, event.CreatedAt.Unix())
	if err != nil {
		return fmt.Errorf("store: 写入异常事件失败: %w", err)
	}
	if id, idErr := res.LastInsertId(); idErr == nil {
		event.ID = uint64(id)
	}
	return nil
}

// RecentBurstCount 统计该用户在该类型下最近 window 内的事件数。
func (r *abuseRepository) RecentBurstCount(ctx context.Context, userID uint64, kind string, since time.Time) (int, error) {
	var count int
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(1) FROM abuse_events
		WHERE user_id = ? AND kind = ? AND created_at >= ?`,
		userID, kind, since.Unix()).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: 统计异常事件数失败: %w", err)
	}
	return count, nil
}

// ListRecent 返回最近的异常事件（时间倒序）。
func (r *abuseRepository) ListRecent(ctx context.Context, limit int) ([]*model.AbuseEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, user_id, token_id, kind, severity, action, detail, metric, threshold, created_at
		FROM abuse_events ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 查询异常事件失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make([]*model.AbuseEvent, 0, limit)
	for rows.Next() {
		var (
			event     model.AbuseEvent
			severity  int
			createdAt int64
		)
		if err := rows.Scan(&event.ID, &event.UserID, &event.TokenID, &event.Kind,
			&severity, &event.Action, &event.Detail, &event.Metric, &event.Threshold, &createdAt); err != nil {
			return nil, fmt.Errorf("store: 读取异常事件失败: %w", err)
		}
		event.Severity = severity
		event.CreatedAt = time.Unix(createdAt, 0)
		result = append(result, &event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历异常事件失败: %w", err)
	}
	return result, nil
}

// UserHourlyRequests 返回该用户近 hours 小时的逐小时用量桶。
//
// 为什么按整点分桶：突发检测要比对"每小时请求量"的分布，
// 用自然小时对齐才能跨天可比；若按"最近 N 小时滑动窗口"分桶，
// 相邻两次统计的桶边界不同，分布会被切碎。
func (r *abuseRepository) UserHourlyRequests(ctx context.Context, userID uint64, hours int) ([]model.HourlyUsage, error) {
	if hours <= 0 || hours > 24*30 {
		hours = 24 // 缺省看一天；上限 30 天，避免慢查询拖垮后台
	}
	// 对齐整点：SQLite 的 strftime('%s', ..., 'unixepoch') 取整点时间戳。
	since := time.Now().Add(-time.Duration(hours) * time.Hour).Truncate(time.Hour)

	rows, err := r.db.QueryContext(ctx, `
		SELECT CAST(strftime('%s', created_at, 'unixepoch', 'start of hour') AS INTEGER) AS hour_start,
		       COUNT(1),
		       COALESCE(SUM(CASE WHEN status_code >= 200 AND status_code < 400 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(total_tokens), 0),
		       COALESCE(SUM(quota), 0)
		FROM usage_logs
		WHERE user_id = ? AND created_at >= ?
		GROUP BY hour_start
		ORDER BY hour_start ASC`, userID, since.Unix())
	if err != nil {
		return nil, fmt.Errorf("store: 查询用户小时用量失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make([]model.HourlyUsage, 0, hours)
	for rows.Next() {
		var item model.HourlyUsage
		if err := rows.Scan(&item.HourStart, &item.Requests, &item.Success, &item.Tokens, &item.Quota); err != nil {
			return nil, fmt.Errorf("store: 读取用户小时用量失败: %w", err)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历用户小时用量失败: %w", err)
	}
	return result, nil
}

/* ───────────────────────── 用量预警 ───────────────────────── */

// Create 落一条预警记录（冷却语义：同 (user, kind, window) 已存在则拒绝）。
func (r *alertRepository) Create(ctx context.Context, alert *model.AlertNotification) error {
	if alert == nil {
		return errors.New("store: 预警记录不能为空")
	}
	if alert.CreatedAt.IsZero() {
		alert.CreatedAt = time.Now()
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO alert_notifications (user_id, kind, severity, window_bucket, title, body,
			estimated_days_left, delivered, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		alert.UserID, alert.Kind, alert.Severity, alert.WindowBucket,
		alert.Title, alert.Body, alert.EstimatedDaysLeft, alert.Delivered, alert.CreatedAt.Unix())
	if err != nil {
		if isUniqueViolation(err) {
			// 唯一索引冲突 = 该冷却窗口内已发过同类预警。
			// 归一成领域错误：调用方据此安静跳过，不打扰用户。
			return model.ErrAlertAlreadySent
		}
		return fmt.Errorf("store: 写入预警记录失败: %w", err)
	}
	if id, idErr := res.LastInsertId(); idErr == nil {
		alert.ID = uint64(id)
	}
	return nil
}

// MarkDelivered 标记记录已成功发出。
func (r *alertRepository) MarkDelivered(ctx context.Context, id uint64) error {
	if _, err := r.db.ExecContext(ctx,
		"UPDATE alert_notifications SET delivered = 1 WHERE id = ?", id); err != nil {
		return fmt.Errorf("store: 标记预警已送达失败: %w", err)
	}
	return nil
}

// ListByUser 返回某用户最近的预警（时间倒序）。
func (r *alertRepository) ListByUser(ctx context.Context, userID uint64, limit int) ([]*model.AlertNotification, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, user_id, kind, severity, window_bucket, title, body,
		       estimated_days_left, delivered, created_at
		FROM alert_notifications
		WHERE user_id = ?
		ORDER BY created_at DESC, id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 查询用户预警记录失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make([]*model.AlertNotification, 0, limit)
	for rows.Next() {
		var (
			alert     model.AlertNotification
			severity  int
			delivered int
			createdAt int64
		)
		if err := rows.Scan(&alert.ID, &alert.UserID, &alert.Kind, &severity,
			&alert.WindowBucket, &alert.Title, &alert.Body,
			&alert.EstimatedDaysLeft, &delivered, &createdAt); err != nil {
			return nil, fmt.Errorf("store: 读取预警记录失败: %w", err)
		}
		alert.Severity = severity
		alert.Delivered = delivered
		alert.CreatedAt = time.Unix(createdAt, 0)
		result = append(result, &alert)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历预警记录失败: %w", err)
	}
	return result, nil
}
