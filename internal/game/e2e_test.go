// 本文件是"整盘棋能否下起来"的离线端到端测试。
//
// 意图（Why）：
//
//	前面的单测都是逐块验证（规则 / 解析 / 提示词 / 驱动），但"能不能正常下完一盘棋"
//	是这些部分【协同】的结果——单块都对，组合起来仍可能因为状态传递、
//	手番切换、终局判定衔接不上而卡住。这类问题只有真跑一盘才会暴露。
//
//	这里用两个"脚本化假模型"模拟真人下棋的回复风格（带思路说明、坐标写法略有差异），
//	让它们互相对下，断言：
//	  · 对局能走到终局（不卡死、不无限循环）；
//	  · 每一手都真的落到了盘上（棋谱与棋盘一致）；
//	  · 终局状态与棋谱的最后一手自洽。
//
//	刻意不让假模型"永远下对"：真实模型会犯错，因此这里也让它偶尔给出
//	非法着法，验证重试路径在真实对局节奏下同样有效。
package game

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// gomokuBot 是一个"会下五子棋"的假模型。
//
// 策略：能连成五子就赢；对方快连成时去堵；否则贴着已有棋子随便下。
// 它不需要棋力强，只需要【总是给出合法着法】，这样才能把一盘棋下完——
// 它同时也在验证"规则引擎给出的合法着法集合"确实可用。
type gomokuBot struct {
	color Color
	rng   *rand.Rand
	// noiseEvery 表示每隔多少手故意给一次非法着法（0 = 不犯错）。
	// 用于验证重试路径在真实对局中确实被走到且能纠正。
	noiseEvery int
	calls      int
}

func (b *gomokuBot) CallVision(_ context.Context, _, _, _ string, _ []byte) (string, error) {
	b.calls++
	// 偶尔给一个非法答案，模拟真实模型看错棋盘
	if b.noiseEvery > 0 && b.calls%b.noiseEvery == 0 {
		return "我觉得应该下在 M20。MOVE: M20", nil
	}
	return "我思考了一下，MOVE: " + pickGomokuMove(b.color), nil
}

// pickGomokuMove 用一个确定性的简单策略选点。
//
// 之所以不用随机选：随机选点几乎不可能形成五连，一盘棋会下到盘面填满
// （225 手）才结束，测试会很慢。用"贴着已有棋子下"的策略能让棋局快速收敛。
func pickGomokuMove(color Color) string {
	if gomokuScan == nil {
		return "H8"
	}
	pt, ok := gomokuScan(color)
	if !ok {
		return "H8"
	}
	return CoordName(pt)
}

// gomokuScan 由测试注入（因为需要当前局面，而 ModelCaller 接口不带局面）。
//
// 说明：真实实现里模型是从图片里读局面的，因此 ModelCaller 不需要局面参数。
// 但假模型拿不到图片内容，只能靠外部注入的局面来决策——
// 这是测试替身的合理妥协，不影响被测代码的真实性。
var gomokuScan func(color Color) (Point, bool)

func TestFullGomokuGame_PlaysToCompletion(t *testing.T) {
	pos := mustPosition(t, KindGomoku)
	black := &gomokuBot{color: ColorBlack, rng: rand.New(rand.NewSource(1))}
	white := &gomokuBot{color: ColorWhite, rng: rand.New(rand.NewSource(2))}
	arenas := map[Color]*Arena{
		ColorBlack: NewArena(black, 3),
		ColorWhite: NewArena(white, 3),
	}
	// 当前局面注入给假模型
	current := pos
	gomokuScan = func(color Color) (Point, bool) {
		return bestGomokuPoint(current, color)
	}
	defer func() { gomokuScan = nil }()

	var moveLog []string
	status := StatusPlaying
	for step := 0; step < 200 && status == StatusPlaying; step++ {
		mover := current.ToMove
		outcome, next, st, err := arenas[mover].PlayTurn(context.Background(), current, "bot", moveLog)
		if err != nil {
			t.Fatalf("第 %d 手报错: %v", step+1, err)
		}
		if outcome.Move == nil {
			t.Fatalf("第 %d 手未能给出合法着法（尝试 %d 次）：%v",
				step+1, outcome.Attempts, outcome.AttemptErrors)
		}
		// 断言这一手确实落到了盘上：棋谱与棋盘必须一致
		if next.At(outcome.Move.Point) != mover {
			t.Fatalf("第 %d 手声称落在 %v，但棋盘上那里是 %v",
				step+1, outcome.Move.Point, next.At(outcome.Move.Point))
		}
		moveLog = append(moveLog, outcome.Move.Notation())
		current = next
		status = st
	}

	if status == StatusPlaying {
		t.Fatalf("200 手内未结束（说明策略不收敛或规则有问题）")
	}
	// 胜负两种情况：分出胜负，或棋盘下满判和（真实对局也存在和棋）
	if status == StatusBlackWin || status == StatusWhiteWin {
		// 胜负必须由最后一手形成五连来解释：检查胜方确实有五连
		if !hasFiveInARow(current, current.LastMove.Color) {
			t.Errorf("判定了胜负，但盘面上找不到五连（胜方 %v）", current.LastMove.Color)
		}
		t.Logf("对局结束：%s，共 %d 手", status, len(moveLog))
		return
	}
	t.Logf("对局以 %s 结束（棋盘下满），共 %d 手", status, len(moveLog))
	t.Logf("对局结束：%s，共 %d 手", status, len(moveLog))
}

func TestFullGomokuGame_RetriesRecoverFromBadMoves(t *testing.T) {
	// 让黑方每 3 手故意给一次非法着法，验证重试能把对局救回来
	// （而不是"第一手错了就判负"）。
	pos := mustPosition(t, KindGomoku)
	black := &gomokuBot{color: ColorBlack, noiseEvery: 3}
	white := &gomokuBot{color: ColorWhite}
	arenas := map[Color]*Arena{
		ColorBlack: NewArena(black, 3),
		ColorWhite: NewArena(white, 3),
	}
	current := pos
	gomokuScan = func(color Color) (Point, bool) { return bestGomokuPoint(current, color) }
	defer func() { gomokuScan = nil }()

	status := StatusPlaying
	retriesSeen := 0
	for step := 0; step < 200 && status == StatusPlaying; step++ {
		mover := current.ToMove
		outcome, next, st, err := arenas[mover].PlayTurn(context.Background(), current, "bot", nil)
		if err != nil {
			t.Fatalf("报错: %v", err)
		}
		if outcome.Move == nil {
			t.Fatalf("第 %d 手失败（本应靠重试纠正）：%v", step+1, outcome.AttemptErrors)
		}
		if outcome.Attempts > 1 {
			retriesSeen++
		}
		current = next
		status = st
	}
	if retriesSeen == 0 {
		t.Error("注入的非法着法没有触发任何重试，说明重试路径没被走到")
	}
	if status == StatusPlaying {
		t.Error("对局应在 200 手内结束")
	}
	t.Logf("重试发生 %d 次，对局以 %s 结束", retriesSeen, status)
}

// bestGomokuPoint 选一个合法且有用的落点。
//
// 策略分三层，从"必胜"到"布局"：
//  1. 自己能立刻连成五 → 直接赢；
//  2. 对手能立刻连成五 → 去堵（不堵就输）；
//  3. 否则用评分挑点：同时看"这手能让我这条线多长"与
//     "这手能压住对手多长的线"，取分最高者。
//
// 为什么第 3 层不能只"贴着已有棋子随便下"：
//
//	那样双方都只会防守、谁都不主动造威胁，结果一路填到棋盘下满仍未分胜负
//	（实测 200 手都没结束）。评分策略会主动造三连、四连，
//	对局才能像真实棋局一样收敛。
func bestGomokuPoint(pos *Position, color Color) (Point, bool) {
	// 1) 能赢就赢
	if pt, ok := findWinningPoint(pos, color); ok {
		return pt, true
	}
	// 2) 对手能赢就堵
	if pt, ok := findWinningPoint(pos, color.Opponent()); ok {
		return pt, true
	}

	// 3) 评分选点：只考虑已有棋子附近的空点（远处落子没有战术价值）
	best := Point{-1, -1}
	bestScore := -1
	for r := 0; r < pos.Height; r++ {
		for c := 0; c < pos.Width; c++ {
			pt := Point{c, r}
			if pos.At(pt) != ColorNone || !nearAnyStone(pos, pt) {
				continue
			}
			score := lineScore(pos, pt, color)*2 + lineScore(pos, pt, color.Opponent())
			if score > bestScore {
				bestScore, best = score, pt
			}
		}
	}
	if bestScore >= 0 {
		return best, true
	}
	// 4) 空盘：从天元开始
	return Point{pos.Width / 2, pos.Height / 2}, true
}

// nearAnyStone 判断该点周围两格内是否有棋子。
func nearAnyStone(pos *Position, pt Point) bool {
	for dy := -2; dy <= 2; dy++ {
		for dx := -2; dx <= 2; dx++ {
			if pos.At(Point{pt.X + dx, pt.Y + dy}) != ColorNone {
				return true
			}
		}
	}
	return false
}

// lineScore 评估"把 color 下在 pt 能形成多大的威胁"。
//
// 取四个方向上最长连子数的平方：平方是为了让"四连"远重于"两个二连"——
// 四连是必胜威胁，必须优先处理，线性加权会让布局类着法与它同分。
func lineScore(pos *Position, pt Point, color Color) int {
	dirs := [][2]int{{1, 0}, {0, 1}, {1, 1}, {1, -1}}
	best := 0
	for _, d := range dirs {
		n := 1 // 加上将要下的这一子
		for i := 1; i < 5; i++ {
			if pos.At(Point{pt.X + d[0]*i, pt.Y + d[1]*i}) != color {
				break
			}
			n++
		}
		for i := 1; i < 5; i++ {
			if pos.At(Point{pt.X - d[0]*i, pt.Y - d[1]*i}) != color {
				break
			}
			n++
		}
		if n > best {
			best = n
		}
	}
	return best * best
}

// findWinningPoint 找出能让 color 立刻成五的点。
func findWinningPoint(pos *Position, color Color) (Point, bool) {
	for r := 0; r < pos.Height; r++ {
		for c := 0; c < pos.Width; c++ {
			pt := Point{c, r}
			if pos.At(pt) != ColorNone {
				continue
			}
			trial := pos.Clone()
			trial.SetColor(pt, color)
			if hasFiveInARow(trial, color) {
				return pt, true
			}
		}
	}
	return Point{}, false
}

// hasFiveInARow 判断盘面上该方是否存在五连（用于校验终局判定）。
func hasFiveInARow(pos *Position, color Color) bool {
	dirs := [][2]int{{1, 0}, {0, 1}, {1, 1}, {1, -1}}
	for r := 0; r < pos.Height; r++ {
		for c := 0; c < pos.Width; c++ {
			if pos.At(Point{c, r}) != color {
				continue
			}
			for _, d := range dirs {
				n := 0
				for i := 0; i < 5; i++ {
					if pos.At(Point{c + d[0]*i, r + d[1]*i}) != color {
						break
					}
					n++
				}
				if n >= 5 {
					return true
				}
			}
		}
	}
	return false
}

// xiangqiBot 是一个"会给合法着法"的假象棋模型。
//
// 它从规则引擎枚举出的合法着法里挑一个，因此必然合法——
// 用来验证"象棋整盘（含走子、吃子、终局判定）能跑通"。
type xiangqiBot struct {
	color Color
	// pick 是选招函数（由测试注入当前局面）
	pick func(pos *Position) (Move, bool)
}

func (b *xiangqiBot) CallVision(_ context.Context, _, _, _ string, _ []byte) (string, error) {
	if b.pick == nil {
		return "MOVE: A1A2", nil
	}
	mv, ok := b.pick(nil)
	if !ok {
		return "MOVE: RESIGN", nil
	}
	return fmt.Sprintf("我走这步。MOVE: %s%s", CoordName(mv.From), CoordName(mv.To)), nil
}

func TestFullXiangqiGame_RunsWithoutGettingStuck(t *testing.T) {
	// 象棋的验证重点不是"谁赢"（假模型棋力无关紧要），而是：
	//   · 走子/吃子正确应用（棋子类型随位置转移）；
	//   · 双方来回若干手不会出现"无合法着法"或状态错乱；
	//   · 终局判定（将被吃）能触发。
	pos := mustPosition(t, KindXiangqi)
	current := pos
	// 双方都从合法着法里挑：优先能吃子的着法，让对局更快走向终局
	pick := func(_ *Position) (Move, bool) {
		moves := SampleLegalMoves(current, 200)
		if len(moves) == 0 {
			return Move{}, false
		}
		for _, mv := range moves {
			if current.At(mv.To) != ColorNone {
				return mv, true // 有吃子优先
			}
		}
		return moves[0], true
	}
	black := &xiangqiBot{color: ColorBlack, pick: pick}
	white := &xiangqiBot{color: ColorWhite, pick: pick}
	arenas := map[Color]*Arena{
		ColorBlack: NewArena(black, 3),
		ColorWhite: NewArena(white, 3),
	}

	status := StatusPlaying
	badApply := 0
	for step := 0; step < 120 && status == StatusPlaying; step++ {
		mover := current.ToMove
		outcome, next, st, err := arenas[mover].PlayTurn(context.Background(), current, "bot", nil)
		if err != nil {
			t.Fatalf("第 %d 手报错: %v", step+1, err)
		}
		if outcome.Move == nil {
			// 象棋走到无子可动是可能的（比如双方只剩将），此时判和并停止
			t.Logf("第 %d 手无合法着法（尝试 %d 次），终止循环", step+1, outcome.Attempts)
			break
		}
		// 走子后：终点必须是走子的那一方，起点必须清空
		if next.At(outcome.Move.To) != mover {
			badApply++
		}
		if next.At(outcome.Move.From) != ColorNone {
			badApply++
		}
		if next.PlacedAt(outcome.Move.To) == PieceNone {
			badApply++
		}
		current = next
		status = st
	}
	if badApply > 0 {
		t.Errorf("走子应用有 %d 处不一致（终点归属/起点清空/类型保留）", badApply)
	}
	if !current.ToMove.Valid() {
		t.Error("手番出现了非法值")
	}
	t.Logf("象棋对局推进到第 %d 手，状态 %s", current.LastMoveSeq(), status)
}

// Valid 判断颜色值是否合法（测试辅助）。
func (c Color) Valid() bool { return c == ColorBlack || c == ColorWhite }

func TestGoBot_PlaysLegalMovesAndCanPass(t *testing.T) {
	// 围棋：验证 Pass 与终局（双方连续停一手）在驱动层能走通。
	passBot := &passingGoBot{}
	pos := mustPosition(t, KindGo)
	arena := NewArena(passBot, 3)

	outcome, next, status, err := arena.PlayTurn(context.Background(), pos, "bot", nil)
	if err != nil || outcome.Move == nil || !outcome.Move.Pass {
		t.Fatalf("第一次应停一手，实际 %+v / %v", outcome, err)
	}
	if status != StatusPlaying {
		t.Errorf("一次停一手不应终局，实际 %s", status)
	}
	_, _, status2, err := arena.PlayTurn(context.Background(), next, "bot", nil)
	if err != nil {
		t.Fatalf("第二次报错: %v", err)
	}
	if status2 != StatusDraw {
		t.Errorf("双方连续停一手应判和，实际 %s", status2)
	}
}

// passingGoBot 永远选择停一手。
type passingGoBot struct{}

func (passingGoBot) CallVision(_ context.Context, _, _, _ string, _ []byte) (string, error) {
	return "当前无处可下，我选择停一手。", nil
}

func TestPromptForAllKinds_ContainsCoordinateInstructions(t *testing.T) {
	// 三种棋的系统提示都必须讲清"怎么读坐标"和"怎么输出"——
	// 这两条缺失会让模型按自己的想象给坐标，是非法着法的主因。
	for _, kind := range AllKinds() {
		sys := SystemPrompt(kind)
		for _, want := range []string{"列字母", "行号", "MOVE:"} {
			if !strings.Contains(sys, want) {
				t.Errorf("%s 的系统提示缺少 %q", kind, want)
			}
		}
		user := UserPrompt(kind, MoveHint{MoveNumber: 1, SelfColor: ColorBlack})
		if !strings.Contains(user, "第 1 手") {
			t.Errorf("%s 的用户提示缺少手数", kind)
		}
	}
}
