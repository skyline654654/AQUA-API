// Package model 定义对弈演示的领域实体与仓储契约。
//
// 意图（Why）：
//
//	对弈是"用户发起、系统代其与模型对局"的活动，需要持久化三类事实：
//	对局本身（谁和谁、什么状态）、棋谱（每一手）、以及局面快照（用于快速恢复）。
//	把它们放在 model 层而不是 server 层，是为了让"对局是什么"与
//	"对局怎么通过 HTTP 暴露"分离——前者是领域，后者是传输。
//
// 流转（Flow）：
//
//	server/handler_game → model.GameMatchRepository（接口）
//	  → store/gameRepo（实现）→ SQLite
//
// 扩展（Extend）：
//
//	新增对局属性（如"限时"、"观战人数"）：在 GameMatch 加字段 →
//	在 store/gameRepo 同步列清单与扫描逻辑 → 新开一个迁移脚本。
//	注意迁移【只增不改】：已发布的脚本不再修改。
package model

import (
	"context"
	"errors"
	"time"
)

// 对局模式。
const (
	// GameModeAIVsAI 两个模型互相对弈。
	GameModeAIVsAI = "ai_vs_ai"
	// GameModeHumanVsAI 人机对弈（人类走其中一方）。
	GameModeHumanVsAI = "human_vs_ai"
)

// 对局状态。与 game.Status 的取值保持一致，便于直接透传给前端。
const (
	GameStatusPlaying  = "playing"
	GameStatusBlackWin = "black_win"
	GameStatusWhiteWin = "white_win"
	GameStatusDraw     = "draw"
	// GameStatusAborted 表示"没下完就结束了"（某方给不出合法着法、或调用屡次失败）。
	//
	// 与 draw 严格区分：draw 是"下完了、平局"，aborted 是"没下完"。
	// 前端与统计口径都依赖这个区分——把 aborted 当 draw 会让人
	// 误以为"模型下成了和棋"，与事实完全相反。
	GameStatusAborted = "aborted"
)

// 胜者标识。
const (
	GameWinnerBlack = "black"
	GameWinnerWhite = "white"
	GameWinnerDraw  = "draw"
)

// ErrGameMatchNotFound 对局不存在。
var ErrGameMatchNotFound = errors.New("model: 对局不存在")

// GameMatch 是一局棋。
type GameMatch struct {
	ID     uint64 // 主键
	UserID uint64 // 发起人（对局消耗其额度）
	// Kind 棋种：gomoku / go / xiangqi（与 game.Kind 一致）。
	Kind string
	// Mode 模式：ai_vs_ai / human_vs_ai。
	Mode string
	// BlackModel / WhiteModel 是对应席位上的模型名。
	// 人机模式下人类那一席为空串（人类不出现在模型调用里）。
	BlackModel string
	WhiteModel string
	// HumanColor 是人类执的一方：0=无人类 1=黑 2=白（与 game.Color 一致）。
	HumanColor uint8
	Status     string
	Winner     string // black / white / draw / 空（未分胜负）
	// MoveCount 是已走手数（与棋谱的最后手 seq 一致）。
	MoveCount int
	// ErrorText 是中止原因（人类可读）。仅 Status==aborted 时有意义。
	ErrorText string
	// StateJSON 是局面快照（game.Position 的 JSON）。
	//
	// 存快照而不是每次从棋谱重放：一盘围棋可达数百手，
	// 在"读取对局"这种高频操作上重放全部着法并不划算。
	StateJSON string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsOver 判断对局是否已结束。
func (m *GameMatch) IsOver() bool {
	return m.Status != GameStatusPlaying
}

// ModelForColor 返回某一方应由哪个模型走子。
//
// 返回空串表示该席位是人类——这【不是错误】，因此不用 error 表达，
// 让调用方以"空串即人类"这一条规则判断即可。
func (m *GameMatch) ModelForColor(color uint8) string {
	if color == 1 {
		return m.BlackModel
	}
	return m.WhiteModel
}

// IsHumanTurn 判断当前该不该由人类走。
//
// 注意：判断依据是"该席位是否有人类 + 是否轮到该席位"，
// 而不是"人类执某色"这一静态事实——静态判断会让人类在自己回合之外也能落子。
func (m *GameMatch) IsHumanTurn(toMove uint8) bool {
	return m.HumanColor != 0 && m.HumanColor == toMove
}

// GameMoveRecord 是棋谱里的一手。
//
// 与 game.Move 的区别：game.Move 是"引擎里的着法"（含棋盘语义），
// 本结构是"持久化的一手"（含来源与失败信息）。两者分开是为了让
// 领域模型不必承载 raw_output/attempts 这类排查字段。
type GameMoveRecord struct {
	ID       uint64
	MatchID  uint64
	Seq      int
	Color    uint8
	Notation string // 人类可读着法（H8 / H1H4 / PASS / 认输）
	// RawOutput 是模型原始输出（截断后）。
	//
	// 必须留档：只看解析后的着法无法区分"模型走错了"与"我们解析错了"，
	// 而这两者的处置完全不同（前者是模型能力问题，后者是我们的 bug）。
	RawOutput string
	// Attempts 是这一手尝试了几次才成功（1 表示一次就对）。
	//
	// 它是"模型看图能力"的一个直接量化指标：重试越多说明它越难从图上读出局面。
	Attempts int
	// ErrorText 记录该手最终失败的原因（成功行走时为 empty）。
	ErrorText string
	CreatedAt time.Time
}

// GameMatchRepository 定义对局的持久化操作。
type GameMatchRepository interface {
	// Create 新增对局，返回带 ID 的实体。
	Create(ctx context.Context, match *GameMatch) error
	// GetByID 按主键查询（限定归属用户，防止越权读取他人对局）。
	GetByID(ctx context.Context, userID, id uint64) (*GameMatch, error)
	// ListByUser 列出某用户的对局（按创建时间倒序）。
	ListByUser(ctx context.Context, userID uint64, limit int) ([]*GameMatch, error)
	// Update 全量更新对局（状态机推进后调用）。
	Update(ctx context.Context, match *GameMatch) error
	// Delete 删除对局（级联删除其棋谱）。
	Delete(ctx context.Context, userID, id uint64) error

	// ListMoves 取某对局的棋谱（按手数升序）。
	ListMoves(ctx context.Context, matchID uint64) ([]*GameMoveRecord, error)
	// AdvanceWithMove 在同一事务内追加棋谱并更新对局快照与状态。
	//
	// 为什么要做成一个原子方法而不是"AppendMove + Update"两次调用：
	// 中途崩溃会留下"棋谱有这一手、快照却没有"的不一致状态，
	// 而恢复对局时以快照为准，那一手会被静默丢弃——棋谱与棋盘对不上，
	// 复盘时表现为"某一步莫名其妙没生效"，极难排查。
	AdvanceWithMove(ctx context.Context, match *GameMatch, rec *GameMoveRecord) error
}
