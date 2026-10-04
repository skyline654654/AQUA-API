// 本文件定义「成本归因」「滥用检测」「用量预警」的领域模型。
//
// 意图（Why）：
//
//	这三件事解决的是同一个问题的三个侧面：钱花在哪、谁在偷、还能撑多久。
//	它们共享 usage_logs 这一个数据源，但关注点不同，故分文件但同属运营域：
//	  - 归因（CostAttribution）：按场景标签切分用量，回答"优化哪儿"；
//	  - 滥用（AbuseEvent）：识别异常用量，回答"亏在谁身上"；
//	  - 预警（AlertNotification）：预测余额与流量趋势，主动告知用户。
//
// 流转（Flow）：
//
//	relay 落 usage_logs（带 tag）
//	  → 归因：按 (tag) 聚合 quota / tokens / requests
//	  → 滥用：按 (user_id, token_id) 的历史分布比对当期
//	  → 预警：按近 N 日均消耗 × 剩余额度 推算耗尽天数
//
// 扩展（Extend）：
//
//	新增统计维度时（如按 API Key、按 IP），在 UsageLogQuery 加过滤条件即可，
//	三个能力共用同一套聚合查询，不需要各自造轮子。
package model

import (
	"context"
	"errors"
	"strings"
	"time"
)

// 场景标签的缺省值。
//
// 为什么缺省不是空串而是具名常量：归因看板需要能区分"用户主动打了标签"
// 和"客户端没传标签"，否则"未标注"这一栏会把多个场景混在一起，
// 看板上就无法回答"是不是我的 Playground 在烧钱"。
const (
	// TagUntagged 表示客户端未传标签。
	TagUntagged = "untagged"
	// TagPlayground 表示来自站内游乐场（服务端内部注入，客户端无法伪造）。
	TagPlayground = "playground"
)

// MaxTagLength 是场景标签的最大长度。
//
// 上限 32 的理由：标签用于聚合维度而非展示长文本，过长只会让索引变大、
// 界面上撑破卡片；32 足够表达"支付网关-商户回调"这类场景。
const MaxTagLength = 32

// 归一化场景标签：去空白、截断超长、空值归为 untagged。
//
// 为什么放在 model 层：写入 usage_logs（relay）与读出归因（server）两处都要用同
// 一套规则，若各写一份必然漂移（一个截断、一个不截断，归因就对不上）。
func NormalizeTag(tag string) string {
	trimmed := strings.TrimSpace(tag)
	if trimmed == "" {
		return TagUntagged
	}
	if len(trimmed) > MaxTagLength {
		trimmed = trimmed[:MaxTagLength]
	}
	return trimmed
}

// CostAttribution 是按场景标签聚合的成本视图。
//
// 语义：Requests/Tokens/Quota 都是该标签下的**合计值**，
// Share 是该标签占总量的比例（0~1），由 store 层在聚合时算出。
type CostAttribution struct {
	Tag       string  // 场景标签
	Requests  int64   // 请求数
	Tokens    int64   // Token 消耗
	Quota     int64   // 消耗额度（内部计费单位）
	Share     float64 // 占总消耗的比例（0~1）
	AvgTokens float64 // 单次请求平均 Token
}

// CostRepository 定义成本归因所需的聚合查询。
type CostRepository interface {
	// CostByTag 聚合某用户在时间窗内、按场景标签切分的用量。
	// 窗口为 [since, until)；until 传零值表示"至今"。
	//
	// 为什么要按用户过滤：归因是用户视角的功能（"我的钱花在哪"），
	// 管理员看全站是另一个聚合层级（跨用户），不混在同一个接口里。
	CostByTag(ctx context.Context, userID uint64, since, until time.Time) ([]CostAttribution, error)
}

// 异常类型（abuse kind）。
//
// 为什么用字符串常量而不是数字枚举：这些值会直接出现在告警邮件与用户界面里，
// 可读性比存储密度重要；与项目内既有做法（如 UsageLog.Status 用字符串语义值）一致。
const (
	// AbuseKindBurst 表示请求量突增（相对该用户自身历史 P99）。
	AbuseKindBurst = "burst"
	// AbuseKindFingerprint 表示调用指纹可疑（无 UA / 高频失败重试等）。
	AbuseKindFingerprint = "fingerprint"
	// AbuseKindKeyLeak 表示凭据疑似外泄。
	AbuseKindKeyLeak = "key_leak"
)

// 严重级别。
const (
	// AbuseSeverityInfo 提示：记录留痕，不打扰。
	AbuseSeverityInfo = 1
	// AbuseSeverityWarn 警告：已自动降权或限流。
	AbuseSeverityWarn = 2
	// AbuseSeverityCritical 严重：需人工介入。
	AbuseSeverityCritical = 3
)

// 处置动作。
//
// 把"发现了什么"（kind/detail）与"做了什么"（action）分开记录：
// 调整阈值时不丢历史，也能回答"当时到底限没限"。
const (
	// AbuseActionObserve 仅记录，未处置。
	AbuseActionObserve = "observe"
	// AbuseActionLimit 已自动限流。
	AbuseActionLimit = "limit"
	// AbuseActionNotify 已通知站长。
	AbuseActionNotify = "notify"
)

// AbuseEvent 是一次异常用量的检测结果。
//
// 设计取舍：一条 event 对应"一个用户 + 一类异常在一个时间点"，
// 不做持续状态的更新——历史是流水账，便于事后逐条复盘。
type AbuseEvent struct {
	ID       uint64
	UserID   uint64
	TokenID  uint64
	Kind     string
	Severity int
	Action   string
	// Detail 是人类可读说明（已脱敏，不含任何凭据片段）。
	Detail string
	// Metric / Threshold 是触发时的度量值与阈值，成对保存便于调参复盘：
	// 只留度量值会看不出"是不是阈值太严"。
	Metric    float64
	Threshold float64
	CreatedAt time.Time
}

// AbuseRepository 定义异常事件的持久化。
type AbuseRepository interface {
	// Create 落一条异常事件。
	Create(ctx context.Context, event *AbuseEvent) error

	// RecentBurstCount 统计该用户在该类型下最近 window 内的事件数。
	//
	// 用途：告警冷却——同一异常在窗口内重复触发 N 次才通知站长，
	// 避免"一次抖动就发邮件"导致的告警疲劳。
	RecentBurstCount(ctx context.Context, userID uint64, kind string, since time.Time) (int, error)

	// ListRecent 返回最近的异常事件（按时间倒序），供后台展示。
	ListRecent(ctx context.Context, limit int) ([]*AbuseEvent, error)

	// UserHourlyRequests 返回该用户近 hours 小时逐小时的请求数。
	//
	// 滥用检测的输入：与历史分布比对即可发现"突增"。
	UserHourlyRequests(ctx context.Context, userID uint64, hours int) ([]HourlyUsage, error)
}

// HourlyUsage 是某用户某小时的用量桶。
type HourlyUsage struct {
	HourStart int64 // 桶起点（Unix 秒，对齐整点）
	Requests  int64
	Success   int64
	Tokens    int64
	Quota     int64
}

// 预警类型（alert kind）。
const (
	// AlertQuotaLow 表示余额不足以支撑预计的后续调用。
	AlertQuotaLow = "quota_low"
	// AlertQuotaDrain 表示按当前速率余额将在数日内耗尽。
	AlertQuotaDrain = "quota_drain"
	// AlertUsageSpike 表示用量相较历史突增。
	AlertUsageSpike = "usage_spike"
)

// AlertNotification 是一条已发出的预警记录。
//
// 为什么要落库而不是发完即弃：
//   - 冷却：同一 (用户, 类型, 窗口) 已有记录就不再发，内存态重启即失效；
//   - 举证：用户问"为什么收到这封邮件"时能还原当时的内容；
//   - 复盘：预警发得太晚说明阈值需要收紧。
type AlertNotification struct {
	ID     uint64
	UserID uint64
	Kind   string
	// Severity 1=提示 2=警告 3=紧急。
	Severity int
	// WindowBucket 是冷却窗口的量化键：同一 (user, kind, bucket)
	// 已有记录则跳过本轮发送（唯一索引保证）。
	WindowBucket int64
	// Title / Body 是邮件内容的快照（模板会改，历史要能复现）。
	Title string
	Body  string
	// EstimatedDaysLeft 是预计余额可支撑天数（趋势预警有值，紧急预警为 0）。
	EstimatedDaysLeft int
	// Delivered 1=已发出；0=发送失败（保留记录以便下轮重试）。
	Delivered int
	CreatedAt time.Time
}

// ErrAlertAlreadySent 表示该冷却窗口内已发过同类预警。
var ErrAlertAlreadySent = errors.New("model: 该冷却窗口内已发送过同类预警")

// AlertRepository 定义预警记录的持久化。
type AlertRepository interface {
	// Create 落一条预警记录。
	//
	// 冷却语义：若 (user_id, kind, window_bucket) 已存在，
	// 返回 ErrAlertAlreadySent，调用方据此跳过发信。
	Create(ctx context.Context, alert *AlertNotification) error

	// MarkDelivered 标记记录已成功发出。
	MarkDelivered(ctx context.Context, id uint64) error

	// ListByUser 返回某用户最近的预警（时间倒序），供门户预警中心展示。
	ListByUser(ctx context.Context, userID uint64, limit int) ([]*AlertNotification, error)
}
