// 本文件实现五子棋规则。
//
// 意图（Why）：
//
//	五子棋是三种棋里唯一"胜负判定无歧义、规则无特例"的棋种，
//	因此作为对弈演示的默认棋种——它测的是模型"能不能连贯地看图并占位"，
//	而不掺入复杂规则的干扰。
//
//	刻意【不】实现禁手（三三禁手、四四禁手等专业规则）：
//	禁手规则在业余对局中本就少用，且会让"模型走了禁手"与"模型看错棋盘"
//	两种失败混在一起，无法区分。演示要的是清晰的观测信号。
//
// 流转（Flow）：
//
//	arena → Lookup(KindGomoku) → 本文件 Validate/Apply/Result
//
// 扩展（Extend）：
//
//	要加"标准/自由"两种规则时，把 gomokuRules 改成带字段的结构体
//	（如 forbidForbidden bool），在 Validate 里分支即可，接口无需改动。
package game

import (
	"errors"
	"fmt"
)

// 五子棋棋盘尺寸固定 15×15。
//
// 不用 19×19：15 路是通用标准，且棋盘越小模型看图定位越准，
// 演示中"能下完一盘"比"棋盘更大"更有价值。
const gomokuSize = 15

// 五子棋的合法性错误。用具名错误而非字符串，便于上层区分处置
// （例如"位置被占"可以重试，而"坐标越界"说明模型没看懂棋盘）。
var (
	// ErrOutOfBoard 坐标越界。
	ErrOutOfBoard = errors.New("game: 坐标超出棋盘")
	// ErrOccupied 该点已有棋子。
	ErrOccupied = errors.New("game: 该位置已有棋子")
)

type gomokuRules struct{}

func (gomokuRules) Kind() Kind { return KindGomoku }

func (gomokuRules) Dims() (int, int) { return gomokuSize, gomokuSize }

// Setup 五子棋开局为空盘。
func (gomokuRules) Setup(*Position) {}

// Validate 校验落子：必须在盘内、且为空点。
func (r gomokuRules) Validate(pos *Position, mv Move) error {
	if mv.Resign {
		return nil
	}
	if mv.Pass {
		return errors.New("game: 五子棋不允许停一手")
	}
	if !pos.InBounds(mv.Point) {
		return fmt.Errorf("%w: (%d,%d) 不在 %d 路盘内", ErrOutOfBoard, mv.Point.X, mv.Point.Y, gomokuSize)
	}
	if pos.At(mv.Point) != ColorNone {
		return fmt.Errorf("%w: (%d,%d) 已被%s方占据", ErrOccupied, mv.Point.X, mv.Point.Y, pos.At(mv.Point).Label())
	}
	return nil
}

// Apply 落子并切换手番。
func (r gomokuRules) Apply(pos *Position, mv Move) *Position {
	next := pos.Clone()
	if mv.Resign {
		// 认输不改棋盘、不切手番：胜负由 Result 依据 Resign 判定，
		// 切手番会让"谁认输"这个信息在状态里丢失。
		next.LastMove = &mv
		return next
	}
	next.SetColor(mv.Point, mv.Color)
	next.LastMove = &mv
	next.ToMove = mv.Color.Opponent()
	return next
}

// Result 判断五子棋胜负：任一方在横 / 竖 / 两斜四个方向连成五子即胜。
//
// 只在"最后一手周围"检查而不是全盘扫描：五子连珠只可能由刚下的那手形成，
// 全盘扫描在 15×15 上虽也够快，但每手都扫全盘是纯粹的浪费，
// 且这个"只看最后一手"的优化顺便表达了规则本身的性质。
func (r gomokuRules) Result(pos *Position) Status {
	if pos.LastMove != nil && pos.LastMove.Resign {
		// 认输：对方胜。
		if pos.LastMove.Color == ColorBlack {
			return StatusWhiteWin
		}
		return StatusBlackWin
	}
	if pos.LastMove == nil {
		return StatusPlaying
	}
	pt := pos.LastMove.Point
	color := pos.LastMove.Color
	if pos.At(pt) != color {
		// 末手不在盘上（理论上不会发生）：保守判为进行中，
		// 避免因状态异常而误判胜负。
		return StatusPlaying
	}
	// 四个方向：横、竖、主对角、副对角。每对方向只需检查一次（正向）即可，
	// 因为"连成五子"是对称性质——从落子点向两侧各数，两边之和 ≥5 即成立。
	dirs := [][2]int{{1, 0}, {0, 1}, {1, 1}, {1, -1}}
	for _, d := range dirs {
		count := 1
		// 正向
		for i := 1; i < 5; i++ {
			if pos.At(Point{pt.X + d[0]*i, pt.Y + d[1]*i}) != color {
				break
			}
			count++
		}
		// 反向
		for i := 1; i < 5; i++ {
			if pos.At(Point{pt.X - d[0]*i, pt.Y - d[1]*i}) != color {
				break
			}
			count++
		}
		if count >= 5 {
			if color == ColorBlack {
				return StatusBlackWin
			}
			return StatusWhiteWin
		}
	}
	return StatusPlaying
}

// MoveFormat 返回给模型的着法格式说明。
func (gomokuRules) MoveFormat() string {
	return "落子格式：列字母+行号，例如 H8 表示第 H 列第 8 行；" +
		"列从左边 A 开始（跳过 I），行号从【上方】1 开始向下递增。"
}
