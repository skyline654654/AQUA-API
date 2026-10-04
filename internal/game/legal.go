// 本文件为每个棋种枚举"示例合法着法"。
//
// 意图（Why）：
//
//	模型看图找着法时最容易出的错不是"不懂规则"，而是"没找到可走的子/可下的点"——
//	尤其象棋：它必须先从图里认出自己有哪些棋子，再想"这子能走到哪"，
//	两步都靠视觉。给它几条真实存在的合法着法作为示例，能把这个链条缩短，
//	显著降低"编一个不存在的坐标"的概率。
//
//	本文件只负责"给示例"，不负责"判断模型给的对不对"（那是规则引擎的事）。
//
// 流转（Flow）：
//
//	prompt.go buildUserPrompt → SampleLegalMoves(pos, n) → 写进提示词
//	  → 模型从示例里得到"坐标长什么样、有哪些子可动"的具体感
//
// 扩展（Extend）：
//
//	要更强的提示（如"这几手里哪手最好"）：可以在此按简单启发式排序
//	（如五子棋优先连子附近、象棋优先能吃子的着法）。
//	当前刻意保持中立：不做任何"建议"，避免模型的判断被我们的启发式带偏——
//	演示要观测的是模型自己的能力，不是我们替它想好的答案。
package game

// SampleLegalMoves 返回至多 limit 条当前局面下的合法着法（黑先白的实际手番由其决定）。
//
// 返回值已按规则校验过，因此可以直接作为提示词里的"示例"，
// 不会出现"示例本身就非法"这种自相矛盾的情况。
func SampleLegalMoves(pos *Position, limit int) []Move {
	if pos == nil || limit <= 0 {
		return nil
	}
	rules, err := Lookup(pos.Kind)
	if err != nil {
		return nil
	}
	color := pos.ToMove
	if color == ColorNone {
		color = ColorBlack
	}
	var out []Move
	seq := 0

	switch pos.Kind {
	case KindGomoku, KindGo:
		out = samplePlacementMoves(pos, rules, color, limit)
	case KindXiangqi:
		out = sampleXiangqiMoves(pos, rules, color, limit)
	}
	for i := range out {
		seq++
		out[i].Seq = seq
	}
	return out
}

// samplePlacementMoves 为落子类棋种挑示例点。
//
// 优先取"已有棋子附近"的空点：这些点既最可能是当前该考虑的位置，
// 也让模型看到"棋子周围是空的、可以下"，比随机散布在全盘更有指导性。
func samplePlacementMoves(pos *Position, rules RuleSet, color Color, limit int) []Move {
	var out []Move
	seen := make(map[Point]bool)

	// 先收集所有棋子的邻居空点
	for y := 0; y < pos.Height && len(out) < limit; y++ {
		for x := 0; x < pos.Width && len(out) < limit; x++ {
			if pos.At(Point{x, y}) == ColorNone {
				continue
			}
			for _, nb := range neighbors(Point{x, y}) {
				pt := nb
				if !pos.InBounds(pt) || pos.At(pt) != ColorNone || seen[pt] {
					continue
				}
				mv := Move{Color: color, Point: pt}
				if rules.Validate(pos, mv) != nil {
					continue
				}
				seen[pt] = true
				out = append(out, mv)
				if len(out) >= limit {
					break
				}
			}
		}
	}
	// 棋盘还空着（或邻居点不够）时补"中心附近"的点：
	// 空盘时没有任何棋子的邻居可取样，必须另给起点，否则提示词里没有示例。
	if len(out) == 0 || (pos.Kind == KindGo && pos.Empty()) {
		cx, cy := pos.Width/2, pos.Height/2
		for r := 1; r <= 3 && len(out) < limit; r++ {
			for _, d := range []Point{{0, 0}, {r, 0}, {0, r}, {-r, 0}, {0, -r}} {
				pt := Point{cx + d.X, cy + d.Y}
				if !pos.InBounds(pt) || pos.At(pt) != ColorNone || seen[pt] {
					continue
				}
				mv := Move{Color: color, Point: pt}
				if rules.Validate(pos, mv) != nil {
					continue
				}
				seen[pt] = true
				out = append(out, mv)
			}
		}
	}
	return out
}

// sampleXiangqiMoves 为象棋挑示例着法。
//
// 遍历己方所有棋子 × 全部终点，取前 limit 条合法着法。
// 复杂度是 16 子 × 90 点 = 1440 次校验，每次校验最坏 O(9)（车炮数路径），
// 单次调用在微秒级——对每手一次的频率而言完全不必优化。
func sampleXiangqiMoves(pos *Position, rules RuleSet, color Color, limit int) []Move {
	var out []Move
	for y := 0; y < pos.Height; y++ {
		for x := 0; x < pos.Width; x++ {
			from := Point{x, y}
			if pos.At(from) != color {
				continue
			}
			for ty := 0; ty < pos.Height; ty++ {
				for tx := 0; tx < pos.Width; tx++ {
					to := Point{tx, ty}
					if to == from || pos.At(to) == color {
						continue
					}
					mv := Move{Color: color, From: from, To: to}
					if rules.Validate(pos, mv) != nil {
						continue
					}
					out = append(out, mv)
					if len(out) >= limit {
						return out
					}
				}
			}
		}
	}
	return out
}
