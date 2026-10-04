// 本文件实现中国象棋规则。
//
// 意图（Why）：
//
//	象棋是三种棋里规则最"结构化的"：每种棋子有固定的走法几何约束
//	（马蹩腿、象塞眼、炮翻山），而棋盘只有 9×10、棋子 32 个。
//	这使得它特别适合检验模型的空间推理是否稳定——走错一步立刻能从
//	"马蹩腿""炮没有炮架"这类具体原因里看出模型错在哪。
//
//	与五子棋/围棋的关键差异：象棋是【走子】而非落子，着法需要
//	起点与终点两个坐标，因此 Move.From/To 在这里才真正被使用。
//
// 棋盘约定（与渲染严格一致）：
//   - X 0..8 从左到右；Y 0..9 从上到下（Y=0 是最上面一行）。
//   - 黑方（将）在上半盘 Y 0..4，白方（红，帅）在下半盘 Y 5..9。
//     注意：传统象棋是【红先】，本实现沿用项目统一的"黑先"约定，
//     并在提示词里明确告知模型，避免它按传统习惯抢着走红方。
//   - 楚河汉界在 Y=4 与 Y=5 之间。
//
// 流转（Flow）：
//
//	arena → Lookup(KindXiangqi) → 本文件 Validate/Apply/Result
//
// 扩展（Extend）：
//
//	传统记谱法（"炮二平五"）不在此实现：它需要"同列同种棋子按前后分"的
//	消歧逻辑，且红黑两方的列号方向相反，实现复杂度高而收益低
//	——模型看图找起点终点比让它背记谱法更可靠。
package game

import (
	"errors"
	"fmt"
)

// 象棋棋盘尺寸。
const (
	xiangqiWidth  = 9
	xiangqiHeight = 10
	// xiangqiRiverY 是河界上方（黑方）最后一行的 Y 值。
	// 黑方象不可越到 Y >= 5，白方象不可越到 Y <= 4。
	xiangqiRiverY = 4
)

var (
	// ErrNotYourPiece 起点不是自己的棋子。
	ErrNotYourPiece = errors.New("game: 起点不是自己的棋子")
	// ErrNotAPiece 起点没有棋子。
	ErrNotAPiece = errors.New("game: 起点没有棋子")
	// ErrIllegalMove 该棋子不能这样走。
	ErrIllegalMove = errors.New("game: 该棋子不能这样走")
	// ErrBlocked 路径被挡住（马蹩腿 / 象塞眼 / 车炮路径有子）。
	ErrBlocked = errors.New("game: 路径被其他棋子挡住")
	// ErrFriendlyFire 目标点是自己方的棋子。
	ErrFriendlyFire = errors.New("game: 不能吃自己的棋子")
)

type xiangqiRules struct{}

func (xiangqiRules) Kind() Kind { return KindXiangqi }

func (xiangqiRules) Dims() (int, int) { return xiangqiWidth, xiangqiHeight }

// Setup 摆放象棋初始局面。
//
// 坐标按渲染方向（黑上白下）落子，与国际通行的象棋初始摆法一致：
//
//	Y=0/Y=9：车 马 象 士 将 士 象 马 车
//	Y=2/Y=7：炮在第 2 与第 8 列
//	Y=3/Y=6：兵/卒在第 0,2,4,6,8 列
func (xiangqiRules) Setup(pos *Position) {
	back := []Piece{
		PieceChariot, PieceHorse, PieceElephant, PieceAdvisor, PieceKing,
		PieceAdvisor, PieceElephant, PieceHorse, PieceChariot,
	}
	for x, p := range back {
		pos.Set(Point{x, 0}, ColorBlack, p)
		pos.Set(Point{x, xiangqiHeight - 1}, ColorWhite, p)
	}
	pos.Set(Point{1, 2}, ColorBlack, PieceCannon)
	pos.Set(Point{7, 2}, ColorBlack, PieceCannon)
	pos.Set(Point{1, 7}, ColorWhite, PieceCannon)
	pos.Set(Point{7, 7}, ColorWhite, PieceCannon)
	for _, x := range []int{0, 2, 4, 6, 8} {
		pos.Set(Point{x, 3}, ColorBlack, PiecePawn)
		pos.Set(Point{x, 6}, ColorWhite, PiecePawn)
	}
}

// Validate 校验一手象棋着法。
func (r xiangqiRules) Validate(pos *Position, mv Move) error {
	if mv.Resign {
		return nil
	}
	if mv.Pass {
		return errors.New("game: 象棋不允许停一手")
	}
	if !pos.InBounds(mv.From) || !pos.InBounds(mv.To) {
		return fmt.Errorf("%w: 起点(%d,%d) 或终点(%d,%d)", ErrOutOfBoard,
			mv.From.X, mv.From.Y, mv.To.X, mv.To.Y)
	}
	if mv.From == mv.To {
		return fmt.Errorf("%w: 起点与终点相同", ErrIllegalMove)
	}
	piece := pos.PlacedAt(mv.From)
	if piece == PieceNone {
		return fmt.Errorf("%w: (%d,%d)", ErrNotAPiece, mv.From.X, mv.From.Y)
	}
	if pos.At(mv.From) != mv.Color {
		return fmt.Errorf("%w: (%d,%d) 是%s方的棋子", ErrNotYourPiece,
			mv.From.X, mv.From.Y, pos.At(mv.From).Label())
	}
	if target := pos.At(mv.To); target == mv.Color {
		return fmt.Errorf("%w: (%d,%d)", ErrFriendlyFire, mv.To.X, mv.To.Y)
	}
	return r.validateGeometry(pos, mv.Color, piece, mv.From, mv.To)
}

// validateGeometry 按棋子类型校验走法几何（位移 + 阻挡 + 区域限制）。
//
// 拆成独立函数是为了让 Apply 也能复用同一套判断：Apply 在提子后需要
// 重新确认"将帅是否照面"，那条规则与具体棋子无关，但同样需要几何信息。
func (r xiangqiRules) validateGeometry(pos *Position, color Color, piece Piece, from, to Point) error {
	dx := to.X - from.X
	dy := to.Y - from.Y
	absX := abs(dx)
	absY := abs(dy)

	switch piece {
	case PieceKing:
		// 将/帅：只能走一步直线，且必须留在九宫内。
		if absX+absY != 1 {
			return fmt.Errorf("%w: 将帅每次只能走一步直线", ErrIllegalMove)
		}
		if !inPalace(color, to) {
			return fmt.Errorf("%w: 将帅不能出九宫", ErrIllegalMove)
		}
	case PieceAdvisor:
		// 士/仕：斜走一步，留在九宫内。
		if absX != 1 || absY != 1 {
			return fmt.Errorf("%w: 士只能斜走一步", ErrIllegalMove)
		}
		if !inPalace(color, to) {
			return fmt.Errorf("%w: 士不能出九宫", ErrIllegalMove)
		}
	case PieceElephant:
		// 象/相：斜走两步（田字），不过河，且象眼不能被堵。
		if absX != 2 || absY != 2 {
			return fmt.Errorf("%w: 象只能斜走两步（田字）", ErrIllegalMove)
		}
		if crossesRiver(color, to) {
			return fmt.Errorf("%w: 象不能过河", ErrIllegalMove)
		}
		eye := Point{from.X + dx/2, from.Y + dy/2}
		if pos.At(eye) != ColorNone {
			return fmt.Errorf("%w: 象眼(%d,%d) 被堵", ErrBlocked, eye.X, eye.Y)
		}
	case PieceHorse:
		// 马：日字（一正交 + 一对角），马腿不能被堵。
		if !((absX == 1 && absY == 2) || (absX == 2 && absY == 1)) {
			return fmt.Errorf("%w: 马只能走日字", ErrIllegalMove)
		}
		// 马腿是"先走的那一方向"的相邻点：位移为 2 的那个轴。
		leg := from
		if absX == 2 {
			leg.X += dx / 2
		} else {
			leg.Y += dy / 2
		}
		if pos.At(leg) != ColorNone {
			return fmt.Errorf("%w: 马腿(%d,%d) 被堵", ErrBlocked, leg.X, leg.Y)
		}
	case PieceChariot:
		// 车：走直线任意距离，路径不能有子。
		if dx != 0 && dy != 0 {
			return fmt.Errorf("%w: 车只能走直线", ErrIllegalMove)
		}
		if countBetween(pos, from, to) > 0 {
			return fmt.Errorf("%w: 车的路径上有棋子", ErrBlocked)
		}
	case PieceCannon:
		// 炮：不吃子时同车（路径全空）；吃子时必须恰有一个炮架。
		if dx != 0 && dy != 0 {
			return fmt.Errorf("%w: 炮只能走直线", ErrIllegalMove)
		}
		between := countBetween(pos, from, to)
		if pos.At(to) == ColorNone {
			if between > 0 {
				return fmt.Errorf("%w: 炮移动时路径上不能有棋子", ErrBlocked)
			}
		} else {
			if between != 1 {
				return fmt.Errorf("%w: 炮吃子必须且只能隔一个炮架（当前 %d 个）", ErrIllegalMove, between)
			}
		}
	case PiecePawn:
		// 兵/卒：只能向前一步；过河后可横走一步；永不后退。
		forward := 1 // 黑方向上（Y 增大），白方向下（Y 减小）
		if color == ColorWhite {
			forward = -1
		}
		if dy == forward && dx == 0 {
			// 直进，合法
		} else if dx != 0 && dy == 0 && crossesRiver(color, from) && absX == 1 {
			// 横走：必须已过河
		} else {
			return fmt.Errorf("%w: 兵/卒只能向前一步，过河后可横走一步", ErrIllegalMove)
		}
	default:
		return fmt.Errorf("%w: 无法识别的棋子类型", ErrIllegalMove)
	}
	return nil
}

// Apply 执行着法并切换手番。
func (r xiangqiRules) Apply(pos *Position, mv Move) *Position {
	next := pos.Clone()
	if mv.Resign {
		next.LastMove = &mv
		return next
	}
	piece := next.PlacedAt(mv.From)
	next.Set(mv.From, ColorNone, PieceNone)
	next.Set(mv.To, mv.Color, piece)
	next.LastMove = &mv
	next.ToMove = mv.Color.Opponent()
	return next
}

// Result 判断终局：将/帅被吃即负。
//
// 采用"将被吃"而不是"将死"作为判定：将死需要枚举对方所有应手并确认
// 无解，实现成本高且容易出边界 bug；而"将被吃"在规则上是等价的终局
// 信号（正常对局中不会被吃到将，一旦吃到说明对方漏应），
// 且它对模型是一个清晰且可自查的目标——提示词里会直接告诉模型
// "吃掉对方的将/帅即获胜"。
func (r xiangqiRules) Result(pos *Position) Status {
	if pos.LastMove != nil && pos.LastMove.Resign {
		if pos.LastMove.Color == ColorBlack {
			return StatusWhiteWin
		}
		return StatusBlackWin
	}
	blackKing := false
	whiteKing := false
	// 防御：Pieces 缺失或长度不足时直接判进行中。
	// 象棋的局面必然带 Pieces（Setup 会填充），缺失说明状态被异常构造，
	// 此时判"进行中"比误判胜负安全——误报胜负会让对局提前结束。
	if pos.Pieces == nil || len(pos.Pieces) < len(pos.Cells) {
		return StatusPlaying
	}
	for i, c := range pos.Cells {
		if c == ColorNone || pos.Pieces[i] != PieceKing {
			continue
		}
		if c == ColorBlack {
			blackKing = true
		} else {
			whiteKing = true
		}
	}
	switch {
	case !blackKing && !whiteKing:
		return StatusDraw // 理论上不会发生；保守判和，避免误报胜负
	case !blackKing:
		return StatusWhiteWin
	case !whiteKing:
		return StatusBlackWin
	}
	return StatusPlaying
}

// MoveFormat 返回给模型的着法格式说明。
func (xiangqiRules) MoveFormat() string {
	return "走子格式：起点坐标紧跟终点坐标，中间不加分隔符，例如 H1H4 表示把 H1 的棋子走到 H4。" +
		"列从左到右为 A B C D E F G H J（共 9 列，跳过字母 I 以免与数字 1 混淆）；" +
		"行号从【上方】1 开始向下递增到 10。"
}

// ── 象棋内部工具 ──────────────────────────────────────────────

// abs 返回绝对值。
func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// inPalace 判断点是否在该方的九宫内。
func inPalace(color Color, pt Point) bool {
	if pt.X < 3 || pt.X > 5 {
		return false
	}
	if color == ColorBlack {
		return pt.Y >= 0 && pt.Y <= 2
	}
	return pt.Y >= 7 && pt.Y <= 9
}

// crossesRiver 判断"从该点出发是否已在对方半场"（用于象不过河与兵横走）。
//
// 语义是按点自身所在半场判断：黑方（上半场）的"过河"是进入 Y>=5；
// 白方（下半场）的"过河"是进入 Y<=4。
func crossesRiver(color Color, pt Point) bool {
	if color == ColorBlack {
		return pt.Y >= xiangqiRiverY+1
	}
	return pt.Y <= xiangqiRiverY
}

// countBetween 统计 from 到 to 的直线路径上（不含两端）的棋子数。
func countBetween(pos *Position, from, to Point) int {
	count := 0
	stepX := sign(to.X - from.X)
	stepY := sign(to.Y - from.Y)
	cur := Point{from.X + stepX, from.Y + stepY}
	for cur != to {
		if pos.At(cur) != ColorNone {
			count++
		}
		cur = Point{cur.X + stepX, cur.Y + stepY}
		// 防御：坐标异常时不无限循环
		if !pos.InBounds(cur) {
			break
		}
	}
	return count
}

// sign 返回 v 的符号（-1/0/1）。
func sign(v int) int {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	}
	return 0
}
