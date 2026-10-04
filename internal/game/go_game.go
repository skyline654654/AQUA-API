// 本文件实现围棋规则（含提子与劫争）。
//
// 意图（Why）：
//
//	围棋是对弈演示里最有价值的棋种：它的每一手都需要模型做一次
//	"数气"的空间推理（这块棋有几口气、下这里能不能提子），
//	而这正是多模态模型最容易翻车也最容易观察的地方。
//
//	本实现刻意【只做基本规则】（气、提子、禁自杀、劫争），不做终局数子：
//	完整数子需要处理双活、官子、贴目等大量边界，而这些与"模型能不能看懂棋"
//	无关。终局改为"连续两次停一手即和棋"，把这个简化在提示词里
//	明确告诉模型，避免它以为要下到棋盘填满。
//
// 流转（Flow）：
//
//	arena → Lookup(KindGo) → 本文件 Validate/Apply/Result
//
// 扩展（Extend）：
//
//	要做完整规则：在 Result 里接入数子（用 floodFill 算territory），
//	并在 Position 增加贴目字段。注意劫争的"超级劫"（superko）
//	需要保存历史局面哈希，那会改变 Position 的结构，属较大改动。
package game

import (
	"errors"
	"fmt"
)

// 围棋棋盘固定 19 路。
const goBoardSize = 19

var (
	// ErrSuicide 禁止自杀：落子后自己这块棋无气。
	ErrSuicide = errors.New("game: 该位置落子后自己无气（禁自杀）")
	// ErrKo 劫争禁着：不能立即回提，形成循环。
	ErrKo = errors.New("game: 劫争禁着（不能立即回提）")
)

// goRules 是围棋规则实现。
type goRules struct{}

func (goRules) Kind() Kind { return KindGo }

func (goRules) Dims() (int, int) { return goBoardSize, goBoardSize }

// Setup 围棋开局为空盘。
func (goRules) Setup(*Position) {}

// Validate 校验落子。
//
// 顺序很重要且刻意如此：
//  1. 盘内 → 2. 空点 → 3. 劫争禁着 → 4. 自杀。
//
// 为什么先判劫争再判自杀：劫争禁着点上"落子会提掉对方一子"，因此它
// 【不是】自杀；若顺序反了，劫争点会先被自杀规则判掉，
// 于是报出的理由是"禁自杀"——而真实原因是打劫，排错方向完全不同。
func (goRules) Validate(pos *Position, mv Move) error {
	if mv.Resign || mv.Pass {
		return nil
	}
	if !pos.InBounds(mv.Point) {
		return fmt.Errorf("%w: (%d,%d) 不在 %d 路盘内", ErrOutOfBoard, mv.Point.X, mv.Point.Y, goBoardSize)
	}
	if pos.At(mv.Point) != ColorNone {
		return fmt.Errorf("%w: (%d,%d) 已被%s方占据", ErrOccupied, mv.Point.X, mv.Point.Y, pos.At(mv.Point).Label())
	}
	if pos.KoPoint != nil && *pos.KoPoint == mv.Point {
		return fmt.Errorf("%w: (%d,%d) 是上一手刚提子的位置", ErrKo, mv.Point.X, mv.Point.Y)
	}
	// 自杀判定：试探落子并结算提子后，若自己这块棋无气则为自杀。
	trial := pos.Clone()
	trial.SetColor(mv.Point, mv.Color)
	removed := resolveCaptures(trial, mv.Point, mv.Color)
	if len(removed) == 0 && !hasLiberty(trial, mv.Point) {
		return fmt.Errorf("%w: (%d,%d)", ErrSuicide, mv.Point.X, mv.Point.Y)
	}
	return nil
}

// Apply 落子、提子、设置劫争禁着点并切换手番。
func (goRules) Apply(pos *Position, mv Move) *Position {
	next := pos.Clone()
	if mv.Resign {
		next.LastMove = &mv
		return next
	}
	if mv.Pass {
		next.ConsecutivePasses++
		next.KoPoint = nil // 停一手不产生劫争
		next.LastMove = &mv
		next.ToMove = mv.Color.Opponent()
		return next
	}

	next.SetColor(mv.Point, mv.Color)
	removed := resolveCaptures(next, mv.Point, mv.Color)

	// 统计被提子数（记在"被提方"名下）。
	// 注意是记"被提掉多少"，因此提子方为黑时，增加的是白方被提数。
	for range removed {
		if mv.Color == ColorBlack {
			next.CapturedWhite++
		} else {
			next.CapturedBlack++
		}
	}

	// 劫争判定：恰好提掉对方一子、且自己落下的这一子成为"单子一气"时，
	// 对方若能立刻回提就形成循环。此处只处理最简单的一手劫（占实际对局的绝大多数）。
	next.KoPoint = nil
	if len(removed) == 1 {
		if liberties := libertyCount(next, mv.Point); liberties == 1 && isSingleStone(next, mv.Point) {
			ko := removed[0]
			next.KoPoint = &ko
		}
	}

	next.ConsecutivePasses = 0
	next.LastMove = &mv
	next.ToMove = mv.Color.Opponent()
	return next
}

// Result 判断终局。
//
// 这里的胜负只有两种来源：认输，或连续两次停一手（和棋）。
// 见文件头说明——不做数子。
func (goRules) Result(pos *Position) Status {
	if pos.LastMove != nil && pos.LastMove.Resign {
		if pos.LastMove.Color == ColorBlack {
			return StatusWhiteWin
		}
		return StatusBlackWin
	}
	if pos.ConsecutivePasses >= 2 {
		return StatusDraw
	}
	return StatusPlaying
}

// MoveFormat 返回给模型的着法格式说明。
func (goRules) MoveFormat() string {
	return "落子格式：列字母+行号，例如 Q16 表示第 Q 列第 16 行；" +
		"列从左边 A 开始（跳过 I），行号从【上方】1 开始向下递增。" +
		"若你认为当前无处可下或应当终局，输出 PASS。"
}

// ── 围棋内部算法（气与提子）──────────────────────────────────────

// neighbors 返回某点上下左右四个相邻点。
func neighbors(pt Point) [4]Point {
	return [4]Point{
		{pt.X - 1, pt.Y},
		{pt.X + 1, pt.Y},
		{pt.X, pt.Y - 1},
		{pt.X, pt.Y + 1},
	}
}

// group 返回与 pt 相连的同色棋块及其气数。
//
// 用迭代（栈）而不是递归：19 路盘上一条大龙可以有数百子，
// 递归在极端局面下有栈溢出风险，而迭代版本没有这个上限。
func group(pos *Position, pt Point) (stones []Point, liberties int) {
	color := pos.At(pt)
	if color == ColorNone {
		return nil, 0
	}
	visited := make(map[Point]bool)
	libertySet := make(map[Point]bool)
	stack := []Point{pt}
	visited[pt] = true

	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		stones = append(stones, cur)

		for _, nb := range neighbors(cur) {
			if !pos.InBounds(nb) {
				continue
			}
			switch pos.At(nb) {
			case ColorNone:
				libertySet[nb] = true
			case color:
				if !visited[nb] {
					visited[nb] = true
					stack = append(stack, nb)
				}
			}
		}
	}
	return stones, len(libertySet)
}

// hasLiberty 判断 pt 所在棋块是否有气。
func hasLiberty(pos *Position, pt Point) bool {
	_, liberties := group(pos, pt)
	return liberties > 0
}

// libertyCount 返回 pt 所在棋块的气数。
func libertyCount(pos *Position, pt Point) int {
	_, liberties := group(pos, pt)
	return liberties
}

// isSingleStone 判断 pt 是否为孤子（周围无同色）。
func isSingleStone(pos *Position, pt Point) bool {
	for _, nb := range neighbors(pt) {
		if pos.At(nb) == pos.At(pt) {
			return false
		}
	}
	return true
}

// resolveCaptures 结算刚落下的这手所造成的提子，返回被提掉的点。
//
// 只检查刚落子点周围的【对方】棋块：一手棋不可能造成别处的棋块被提
// （除自己这块以外，其他棋块的气不受影响）——这是围棋的基本性质，
// 也是本函数能保持 O(局部) 的原因。
func resolveCaptures(pos *Position, pt Point, color Color) []Point {
	opponent := color.Opponent()
	var removed []Point
	// 用 set 去重：一个对方棋块可能同时与落子点相邻两个方向，
	// 不去重会把同一块棋提两次、导致计数虚高。
	handled := make(map[Point]bool)

	for _, nb := range neighbors(pt) {
		if pos.At(nb) != opponent || handled[nb] {
			continue
		}
		stones, liberties := group(pos, nb)
		for _, s := range stones {
			handled[s] = true
		}
		if liberties > 0 {
			continue
		}
		for _, s := range stones {
			pos.Set(s, ColorNone, PieceNone)
			removed = append(removed, s)
		}
	}
	return removed
}
