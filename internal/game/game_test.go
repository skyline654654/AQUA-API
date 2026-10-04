// 本文件是三种棋规则引擎的单元测试。
//
// 意图（Why）：
//
//		规则引擎是整个对弈演示的地基——它判错一步，后面"模型走得好不好"的
//		所有结论都不可信。而且规则错误往往不会崩，只会静默地放过非法着法，
//		表现为"模型看起来很强，其实在乱走"，这种错误极难从对局日志里看出来。
//
//		因此这里对每种棋都钉住三类边界：
//		  1. 正常着法必须被接受；
//		  2. 几何上非法的着法必须被拒绝（且拒绝理由要具体）；
//	 3. 胜负判定只在真正成形时触发（提前触发会让对局草草结束）。
package game

import (
	"errors"
	"testing"
)

// mustPosition 创建指定棋种的空局面（测试辅助）。
func mustPosition(t *testing.T, kind Kind) *Position {
	t.Helper()
	pos, err := NewPosition(kind)
	if err != nil {
		t.Fatalf("创建 %s 局面失败: %v", kind, err)
	}
	return pos
}

// place 在局面上放子（测试辅助，绕过合法性校验以构造特定局面）。
func place(pos *Position, x, y int, c Color, p Piece) {
	pos.Set(Point{x, y}, c, p)
}

// ── 通用 ────────────────────────────────────────────────────

func TestNewPosition_AllKindsHaveCorrectDims(t *testing.T) {
	want := map[Kind][2]int{
		KindGomoku:  {15, 15},
		KindGo:      {19, 19},
		KindXiangqi: {9, 10},
	}
	for kind, dims := range want {
		pos := mustPosition(t, kind)
		if pos.Width != dims[0] || pos.Height != dims[1] {
			t.Errorf("%s 尺寸 = %dx%d，期望 %dx%d", kind, pos.Width, pos.Height, dims[0], dims[1])
		}
		if pos.ToMove != ColorBlack {
			t.Errorf("%s 首手应为黑方，实际 %v", kind, pos.ToMove)
		}
	}
}

func TestParseKind_RejectsUnknown(t *testing.T) {
	// 拼错棋种必须报错而不是回退到默认值：静默回退会让"下错棋"无法被发现。
	if _, err := ParseKind("chess"); err == nil {
		t.Error("未知棋种应返回错误")
	}
	if k, err := ParseKind("  GOMOKU  "); err != nil || k != KindGomoku {
		t.Errorf("应容忍大小写与空白，得到 %v / %v", k, err)
	}
}

func TestPosition_CloneIsDeep(t *testing.T) {
	// 克隆必须是深拷贝：模型走子会反复试探着法，浅拷贝会污染真实局面。
	pos := mustPosition(t, KindGomoku)
	pos.SetColor(Point{3, 3}, ColorBlack)
	clone := pos.Clone()
	clone.SetColor(Point{4, 4}, ColorWhite)

	if pos.At(Point{4, 4}) != ColorNone {
		t.Error("修改克隆影响到了原局面（浅拷贝）")
	}
	// Pieces 也要独立
	if len(pos.Pieces) > 0 && len(clone.Pieces) > 0 {
		clone.Pieces[0] = PieceKing
		if pos.Pieces[0] == PieceKing {
			t.Error("Pieces 未深拷贝")
		}
	}
}

func TestPosition_AtOutOfBoundsReturnsNone(t *testing.T) {
	// 越界必须返回空而不是 panic：气计算与象棋路径枚举会大量探测界外点。
	pos := mustPosition(t, KindGo)
	for _, pt := range []Point{{-1, 0}, {0, -1}, {19, 0}, {0, 19}, {-5, -5}} {
		if got := pos.At(pt); got != ColorNone {
			t.Errorf("At(%v) = %v，期望 ColorNone", pt, got)
		}
		if got := pos.PlacedAt(pt); got != PieceNone {
			t.Errorf("PlacedAt(%v) = %v，期望 PieceNone", pt, got)
		}
	}
}

func TestCoordName_SkipsIAndCountsFromTop(t *testing.T) {
	tests := []struct {
		pt   Point
		want string
	}{
		{Point{0, 0}, "A1"},
		{Point{7, 0}, "H1"},
		{Point{8, 0}, "J1"}, // 跳过 I
		{Point{0, 8}, "A9"},
		{Point{18, 18}, "T19"},
	}
	for _, tt := range tests {
		if got := CoordName(tt.pt); got != tt.want {
			t.Errorf("CoordName(%v) = %q，期望 %q", tt.pt, got, tt.want)
		}
	}
}

// ── 五子棋 ──────────────────────────────────────────────────

func TestGomoku_ValidateRejectsOutOfBoardAndOccupied(t *testing.T) {
	rules, _ := Lookup(KindGomoku)
	pos := mustPosition(t, KindGomoku)

	if err := rules.Validate(pos, Move{Color: ColorBlack, Point: Point{15, 0}}); !errors.Is(err, ErrOutOfBoard) {
		t.Errorf("越界应返回 ErrOutOfBoard，实际 %v", err)
	}
	place(pos, 5, 5, ColorBlack, PieceStone)
	err := rules.Validate(pos, Move{Color: ColorWhite, Point: Point{5, 5}})
	if !errors.Is(err, ErrOccupied) {
		t.Errorf("占位应返回 ErrOccupied，实际 %v", err)
	}
	// 停一手在五子棋里非法
	if err := rules.Validate(pos, Move{Color: ColorBlack, Pass: true}); err == nil {
		t.Error("五子棋不应允许停一手")
	}
}

func TestGomoku_DetectsFiveInAllFourDirections(t *testing.T) {
	rules, _ := Lookup(KindGomoku)
	cases := []struct {
		name string
		pts  []Point
	}{
		{"横向", []Point{{2, 7}, {3, 7}, {4, 7}, {5, 7}, {6, 7}}},
		{"纵向", []Point{{7, 2}, {7, 3}, {7, 4}, {7, 5}, {7, 6}}},
		{"主对角", []Point{{2, 2}, {3, 3}, {4, 4}, {5, 5}, {6, 6}}},
		{"副对角", []Point{{6, 2}, {5, 3}, {4, 4}, {3, 5}, {2, 6}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pos := mustPosition(t, KindGomoku)
			var last Move
			for i, pt := range tc.pts {
				mv := Move{Color: ColorBlack, Seq: i + 1, Point: pt}
				if err := rules.Validate(pos, mv); err != nil {
					t.Fatalf("第 %d 手不应非法: %v", i+1, err)
				}
				pos = rules.Apply(pos, mv)
				last = mv
			}
			if got := rules.Result(pos); got != StatusBlackWin {
				t.Errorf("%s 五连应判黑胜，实际 %s（末手 %v）", tc.name, got, last.Point)
			}
		})
	}
}

func TestGomoku_SixInARowStillWins(t *testing.T) {
	// 长连（六子）也是胜：不做禁手规则，因此不应要求"恰好五子"。
	rules, _ := Lookup(KindGomoku)
	pos := mustPosition(t, KindGomoku)
	for i := 0; i < 6; i++ {
		mv := Move{Color: ColorBlack, Seq: i + 1, Point: Point{2 + i, 7}}
		pos = rules.Apply(pos, mv)
	}
	if got := rules.Result(pos); got != StatusBlackWin {
		t.Errorf("六连应判黑胜，实际 %s", got)
	}
}

func TestGomoku_FourInARowIsNotWin(t *testing.T) {
	// 四连绝不能提前判胜——提前触发会让对局草草结束且结论错误。
	rules, _ := Lookup(KindGomoku)
	pos := mustPosition(t, KindGomoku)
	for i := 0; i < 4; i++ {
		pos = rules.Apply(pos, Move{Color: ColorBlack, Seq: i + 1, Point: Point{2 + i, 7}})
	}
	if got := rules.Result(pos); got != StatusPlaying {
		t.Errorf("四连不应判胜，实际 %s", got)
	}
}

func TestGomoku_BrokenLineIsNotWin(t *testing.T) {
	// 中间断开的五个子不是五连。
	rules, _ := Lookup(KindGomoku)
	pos := mustPosition(t, KindGomoku)
	for _, x := range []int{2, 3, 4, 6, 7} { // 缺 x=5
		pos = rules.Apply(pos, Move{Color: ColorBlack, Seq: x, Point: Point{x, 7}})
	}
	if got := rules.Result(pos); got != StatusPlaying {
		t.Errorf("断开的五子不应判胜，实际 %s", got)
	}
}

func TestGomoku_ResignGivesOpponentWin(t *testing.T) {
	rules, _ := Lookup(KindGomoku)
	pos := mustPosition(t, KindGomoku)
	pos = rules.Apply(pos, Move{Color: ColorBlack, Seq: 1, Resign: true})
	if got := rules.Result(pos); got != StatusWhiteWin {
		t.Errorf("黑认输应判白胜，实际 %s", got)
	}
}

// ── 围棋 ────────────────────────────────────────────────────

func TestGo_RejectsSuicide(t *testing.T) {
	// 经典自杀形：白子被完全包围的角上，白再下角即无气。
	rules, _ := Lookup(KindGo)
	pos := mustPosition(t, KindGo)
	// 把 (0,0) 四周用黑子围死：仅 (1,0) 与 (0,1)
	place(pos, 1, 0, ColorBlack, PieceStone)
	place(pos, 0, 1, ColorBlack, PieceStone)

	err := rules.Validate(pos, Move{Color: ColorWhite, Point: Point{0, 0}})
	if !errors.Is(err, ErrSuicide) {
		t.Errorf("(0,0) 落白子应为自杀，实际 %v", err)
	}
	// 但黑方下这里不会被判自杀吗？黑下 (0,0) 有气（自身连成大块），应合法。
	if err := rules.Validate(pos, Move{Color: ColorBlack, Point: Point{0, 0}}); err != nil {
		t.Errorf("同一位置黑子落子应合法（会连成有气的棋块），实际 %v", err)
	}
}

func TestGo_CapturesOpponentGroup(t *testing.T) {
	// 白单子被黑三面围，黑下最后一面即提掉该白子。
	rules, _ := Lookup(KindGo)
	pos := mustPosition(t, KindGo)
	place(pos, 5, 5, ColorWhite, PieceStone)
	place(pos, 4, 5, ColorBlack, PieceStone)
	place(pos, 6, 5, ColorBlack, PieceStone)
	place(pos, 5, 4, ColorBlack, PieceStone)

	mv := Move{Color: ColorBlack, Seq: 1, Point: Point{5, 6}}
	if err := rules.Validate(pos, mv); err != nil {
		t.Fatalf("提子手不应非法: %v", err)
	}
	next := rules.Apply(pos, mv)
	if next.At(Point{5, 5}) != ColorNone {
		t.Error("被围的白子应被提掉")
	}
	if next.CapturedWhite != 1 {
		t.Errorf("白方被提数 = %d，期望 1", next.CapturedWhite)
	}
}

func TestGo_KoIsForbiddenImmediately(t *testing.T) {
	// 构造标准劫争形。
	//
	// 劫争的成立条件是【对称】的，缺一不可：
	//   白子 P 有且仅有一口气 L（P 的三面是黑子）；
	//   黑子落到 L 提掉 P 后，L 自己也只剩一口气 P（L 的三面必须是白子）。
	// 两个条件同时满足，双方才能互相回提、形成循环。
	// 只满足前者会让黑提子后自己还有别的气，那就不是劫、回提也不该被禁。
	//
	//	局面向（X 黑、O 白、. 空）：
	//	  Y=0:  .  X  .  .
	//	  Y=1:  X  O  X  .      P=(1,1)
	//	  Y=2:  O  .  O  .      L=(1,2)
	//	  Y=3:  .  O  .  .
	rules, _ := Lookup(KindGo)
	pos := mustPosition(t, KindGo)

	// P=(1,1) 周围三面放黑
	place(pos, 1, 0, ColorBlack, PieceStone)
	place(pos, 0, 1, ColorBlack, PieceStone)
	place(pos, 2, 1, ColorBlack, PieceStone)
	// L=(1,2) 周围三面放白
	place(pos, 0, 2, ColorWhite, PieceStone)
	place(pos, 2, 2, ColorWhite, PieceStone)
	place(pos, 1, 3, ColorWhite, PieceStone)
	// 被围的白子
	place(pos, 1, 1, ColorWhite, PieceStone)

	// 前置断言：白子 P 当前确实只剩一口气，否则后面的劫争判定不成立
	if lib := libertyCount(pos, Point{1, 1}); lib != 1 {
		t.Fatalf("构造前提失败：白子 (1,1) 气数 = %d，期望 1", lib)
	}

	mv := Move{Color: ColorBlack, Seq: 1, Point: Point{1, 2}}
	if err := rules.Validate(pos, mv); err != nil {
		t.Fatalf("黑提子手不应非法: %v", err)
	}
	next := rules.Apply(pos, mv)
	if next.At(Point{1, 1}) != ColorNone {
		t.Fatal("被围的白子应被提掉")
	}
	if next.CapturedWhite != 1 {
		t.Fatalf("应提掉 1 个白子，实际 %d", next.CapturedWhite)
	}
	if next.KoPoint == nil {
		t.Fatal("提单子后应设置劫争禁着点")
	}
	if *next.KoPoint != (Point{1, 1}) {
		t.Errorf("禁着点 = %v，期望 (1,1)", *next.KoPoint)
	}

	// 白立刻回提必须被拒绝
	err := rules.Validate(next, Move{Color: ColorWhite, Seq: 2, Point: Point{1, 1}})
	if !errors.Is(err, ErrKo) {
		t.Errorf("白回提应触发劫争禁着，实际 %v", err)
	}
}

func TestGo_NonKoCaptureDoesNotSetKoPoint(t *testing.T) {
	// 对照：普通提子（不是劫）不应设禁着点。
	// 若实现不区分"提单子"与"提成劫"，正常提子也是提单子，
	// 会导致大量合法回提被误禁——对局会卡住。
	rules, _ := Lookup(KindGo)
	pos := mustPosition(t, KindGo)
	place(pos, 5, 5, ColorWhite, PieceStone)
	place(pos, 4, 5, ColorBlack, PieceStone)
	place(pos, 6, 5, ColorBlack, PieceStone)
	place(pos, 5, 4, ColorBlack, PieceStone)

	next := rules.Apply(pos, Move{Color: ColorBlack, Seq: 1, Point: Point{5, 6}})
	if next.At(Point{5, 5}) != ColorNone {
		t.Fatal("白子应被提掉")
	}
	// 黑子 (5,6) 有气（下方与两侧），不是"单子一气"，因此不构成劫
	if next.KoPoint != nil {
		t.Errorf("普通提子不应设禁着点，实际 %v", *next.KoPoint)
	}
}

func TestGo_TwoPassesEndGameAsDraw(t *testing.T) {
	rules, _ := Lookup(KindGo)
	pos := mustPosition(t, KindGo)
	pos = rules.Apply(pos, Move{Color: ColorBlack, Seq: 1, Pass: true})
	if got := rules.Result(pos); got != StatusPlaying {
		t.Errorf("一次停一手不应终局，实际 %s", got)
	}
	pos = rules.Apply(pos, Move{Color: ColorWhite, Seq: 2, Pass: true})
	if got := rules.Result(pos); got != StatusDraw {
		t.Errorf("连续两次停一手应判和，实际 %s", got)
	}
}

func TestGo_CaptureResetsPassedCounter(t *testing.T) {
	// 停一手后若又有实际落子，连续计数必须清零——
	// 否则"停一手→落子→停一手"会被误判为终局。
	rules, _ := Lookup(KindGo)
	pos := mustPosition(t, KindGo)
	pos = rules.Apply(pos, Move{Color: ColorBlack, Seq: 1, Pass: true})
	pos = rules.Apply(pos, Move{Color: ColorWhite, Seq: 2, Point: Point{3, 3}})
	if pos.ConsecutivePasses != 0 {
		t.Errorf("落子后连续停一手计数 = %d，期望 0", pos.ConsecutivePasses)
	}
	if got := rules.Result(pos); got != StatusPlaying {
		t.Errorf("不应终局，实际 %s", got)
	}
}

// ── 象棋 ────────────────────────────────────────────────────

func TestXiangqi_SetupHasExpectedPieceCount(t *testing.T) {
	// 初始局面每方 16 子：车马象士将士象马车(9) + 炮 2 + 兵 5。
	pos := mustPosition(t, KindXiangqi)
	if got := pos.StoneCount(ColorBlack); got != 16 {
		t.Errorf("黑方初始子数 = %d，期望 16", got)
	}
	if got := pos.StoneCount(ColorWhite); got != 16 {
		t.Errorf("白方初始子数 = %d，期望 16", got)
	}
	// 将/帅在正确位置
	if pos.PlacedAt(Point{4, 0}) != PieceKing || pos.At(Point{4, 0}) != ColorBlack {
		t.Error("黑将应在 (4,0)")
	}
	if pos.PlacedAt(Point{4, 9}) != PieceKing || pos.At(Point{4, 9}) != ColorWhite {
		t.Error("白帅应在 (4,9)")
	}
}

func TestXiangqi_HorseLegBlocked(t *testing.T) {
	// 马腿被堵时必须拒绝：这是象棋最容易写错也最容易观察的规则。
	rules, _ := Lookup(KindXiangqi)
	pos := mustPosition(t, KindXiangqi)
	// 清空一匹马的周边，只留马腿
	// 黑马在 (1,0)。马腿为位移 2 的轴方向相邻点。
	// 目标 (2,2)：dx=1, dy=2 → 马腿在 (1,1)
	place(pos, 1, 1, ColorBlack, PiecePawn) // 堵马腿
	err := rules.Validate(pos, Move{Color: ColorBlack, From: Point{1, 0}, To: Point{2, 2}})
	if !errors.Is(err, ErrBlocked) {
		t.Errorf("马腿被堵应返回 ErrBlocked，实际 %v", err)
	}
	// 撤掉马腿后应合法
	place(pos, 1, 1, ColorNone, PieceNone)
	if err := rules.Validate(pos, Move{Color: ColorBlack, From: Point{1, 0}, To: Point{2, 2}}); err != nil {
		t.Errorf("马腿通畅时应合法，实际 %v", err)
	}
}

func TestXiangqi_ElephantEyeBlocked(t *testing.T) {
	rules, _ := Lookup(KindXiangqi)
	pos := mustPosition(t, KindXiangqi)
	// 黑象在 (2,0)，目标 (4,2)，象眼在 (3,1)
	place(pos, 3, 1, ColorBlack, PiecePawn)
	err := rules.Validate(pos, Move{Color: ColorBlack, From: Point{2, 0}, To: Point{4, 2}})
	if !errors.Is(err, ErrBlocked) {
		t.Errorf("象眼被堵应返回 ErrBlocked，实际 %v", err)
	}
}

func TestXiangqi_ElephantCannotCrossRiver(t *testing.T) {
	rules, _ := Lookup(KindXiangqi)
	pos := mustPosition(t, KindXiangqi)
	// 黑象从 (2,4) 到 (4,6) 需要过河（Y>=5），必须拒绝
	place(pos, 2, 4, ColorBlack, PieceElephant)
	err := rules.Validate(pos, Move{Color: ColorBlack, From: Point{2, 4}, To: Point{4, 6}})
	if err == nil {
		t.Error("象过河应被拒绝")
	}
}

func TestXiangqi_CannonNeedsExactlyOneScreen(t *testing.T) {
	rules, _ := Lookup(KindXiangqi)
	pos := mustPosition(t, KindXiangqi)

	// 在 (1,6) 放一个白卒作为炮的目标（初始局面该点是空的）。
	// 炮在 (1,2)，路径 (1,3)(1,4)(1,5) 全空 → 无炮架 → 不能吃。
	place(pos, 1, 6, ColorWhite, PiecePawn)
	err := rules.Validate(pos, Move{Color: ColorBlack, From: Point{1, 2}, To: Point{1, 6}})
	if err == nil {
		t.Error("无炮架时炮不应能吃掉对方棋子")
	}

	// 加一个炮架（黑卒在 (1,5)）→ 恰好一个，可以吃
	place(pos, 1, 5, ColorBlack, PiecePawn)
	if err := rules.Validate(pos, Move{Color: ColorBlack, From: Point{1, 2}, To: Point{1, 6}}); err != nil {
		t.Errorf("恰好一个炮架时炮应能吃子，实际 %v", err)
	}

	// 再加一个炮架（黑卒在 (1,4)）→ 两个，不能吃
	place(pos, 1, 4, ColorBlack, PiecePawn)
	if err := rules.Validate(pos, Move{Color: ColorBlack, From: Point{1, 2}, To: Point{1, 6}}); err == nil {
		t.Error("两个炮架时炮不应能吃子")
	}
}

func TestXiangqi_CannonMovesLikeChariotWhenNotCapturing(t *testing.T) {
	// 炮不吃子时必须像车一样走（路径不能有子）。
	rules, _ := Lookup(KindXiangqi)
	pos := mustPosition(t, KindXiangqi)
	// 炮 (1,2) 平移到 (1,1)（空点，路径为空）→ 合法
	if err := rules.Validate(pos, Move{Color: ColorBlack, From: Point{1, 2}, To: Point{1, 1}}); err != nil {
		t.Errorf("炮走到空点应合法，实际 %v", err)
	}
	// 炮想吃 (0,3) 自己的卒 → 友好火力
	if err := rules.Validate(pos, Move{Color: ColorBlack, From: Point{1, 2}, To: Point{0, 3}}); err == nil {
		t.Error("炮不应能吃自己的棋子")
	}
}

func TestXiangqi_PawnCannotMoveBackward(t *testing.T) {
	rules, _ := Lookup(KindXiangqi)
	pos := mustPosition(t, KindXiangqi)
	// 黑卒在 (4,3)，向后是 Y 减小
	err := rules.Validate(pos, Move{Color: ColorBlack, From: Point{4, 3}, To: Point{4, 2}})
	if err == nil {
		t.Error("兵/卒不应能后退")
	}
	// 向前合法
	if err := rules.Validate(pos, Move{Color: ColorBlack, From: Point{4, 3}, To: Point{4, 4}}); err != nil {
		t.Errorf("兵/卒向前应合法，实际 %v", err)
	}
}

func TestXiangqi_PawnSidewaysOnlyAfterRiver(t *testing.T) {
	rules, _ := Lookup(KindXiangqi)
	pos := mustPosition(t, KindXiangqi)

	// 黑卒在 (4,3)（未过河），横走非法
	if err := rules.Validate(pos, Move{Color: ColorBlack, From: Point{4, 3}, To: Point{5, 3}}); err == nil {
		t.Error("未过河的兵不应能横走")
	}
	// 把黑卒搬到 (4,5)（已过河），横走合法
	place(pos, 4, 3, ColorNone, PieceNone)
	place(pos, 4, 5, ColorBlack, PiecePawn)
	if err := rules.Validate(pos, Move{Color: ColorBlack, From: Point{4, 5}, To: Point{5, 5}}); err != nil {
		t.Errorf("过河后的兵应能横走，实际 %v", err)
	}
}

func TestXiangqi_KingStaysInPalace(t *testing.T) {
	rules, _ := Lookup(KindXiangqi)
	pos := mustPosition(t, KindXiangqi)
	// 黑将 (4,0) 走到 (4,1)（九宫内、且初始为空点）→ 合法。
	// 注意不能选 (3,0)——那里初始有自家士，会先撞上"不能吃自己子"。
	if err := rules.Validate(pos, Move{Color: ColorBlack, From: Point{4, 0}, To: Point{4, 1}}); err != nil {
		t.Errorf("将帅在九宫内移动应合法，实际 %v", err)
	}
	// 走到 (2,0) 出宫（且那里有自家象）→ 必须被拒
	if err := rules.Validate(pos, Move{Color: ColorBlack, From: Point{4, 0}, To: Point{2, 0}}); err == nil {
		t.Error("将帅出九宫应被拒绝")
	}
	// 走两步同向也非法（即便仍在宫内）
	if err := rules.Validate(pos, Move{Color: ColorBlack, From: Point{4, 0}, To: Point{4, 2}}); err == nil {
		t.Error("将帅不应能一次走两步")
	}
}

func TestXiangqi_AdvisorStaysInPalaceAndMovesDiagonal(t *testing.T) {
	rules, _ := Lookup(KindXiangqi)
	pos := mustPosition(t, KindXiangqi)
	// 黑士 (3,0) 斜走到 (4,1)（九宫内空点）→ 合法
	if err := rules.Validate(pos, Move{Color: ColorBlack, From: Point{3, 0}, To: Point{4, 1}}); err != nil {
		t.Errorf("士斜走一步应合法，实际 %v", err)
	}
	// 士直走非法
	if err := rules.Validate(pos, Move{Color: ColorBlack, From: Point{3, 0}, To: Point{3, 1}}); err == nil {
		t.Error("士不应能直走")
	}
}

func TestXiangqi_CannotCaptureOwnPiece(t *testing.T) {
	rules, _ := Lookup(KindXiangqi)
	pos := mustPosition(t, KindXiangqi)
	// 黑车 (0,0) 吃自家黑马 (1,0)
	err := rules.Validate(pos, Move{Color: ColorBlack, From: Point{0, 0}, To: Point{1, 0}})
	if !errors.Is(err, ErrFriendlyFire) {
		t.Errorf("吃自己棋子应返回 ErrFriendlyFire，实际 %v", err)
	}
}

func TestXiangqi_CannotMoveOpponentPiece(t *testing.T) {
	rules, _ := Lookup(KindXiangqi)
	pos := mustPosition(t, KindXiangqi)
	// 黑方试图移动白方的车 (0,9)
	err := rules.Validate(pos, Move{Color: ColorBlack, From: Point{0, 9}, To: Point{0, 8}})
	if !errors.Is(err, ErrNotYourPiece) {
		t.Errorf("移动对方棋子应返回 ErrNotYourPiece，实际 %v", err)
	}
}

func TestXiangqi_ChariotPathBlocked(t *testing.T) {
	rules, _ := Lookup(KindXiangqi)
	pos := mustPosition(t, KindXiangqi)
	// 黑车 (0,0) 想吃 (0,9) 的白车，但中间有自己的卒 (0,3)
	err := rules.Validate(pos, Move{Color: ColorBlack, From: Point{0, 0}, To: Point{0, 9}})
	if !errors.Is(err, ErrBlocked) {
		t.Errorf("车路径被挡应返回 ErrBlocked，实际 %v", err)
	}
	// 吃掉挡路的自己棋子不允许（friendly fire）
	// 但车走到 (0,1) 空点应合法
	if err := rules.Validate(pos, Move{Color: ColorBlack, From: Point{0, 0}, To: Point{0, 1}}); err != nil {
		t.Errorf("车走到空点应合法，实际 %v", err)
	}
}

func TestXiangqi_KingCaptureEndsGame(t *testing.T) {
	// 将被吃应判负——这是本实现的终局信号（见 Result 的说明）。
	rules, _ := Lookup(KindXiangqi)
	pos := mustPosition(t, KindXiangqi)
	// 直接把白帅拿走，模拟"被吃"
	place(pos, 4, 9, ColorNone, PieceNone)
	if got := rules.Result(pos); got != StatusBlackWin {
		t.Errorf("白帅不在盘上应判黑胜，实际 %s", got)
	}
}

func TestXiangqi_ApplyPreservesPieceType(t *testing.T) {
	// 走子必须保留棋子类型：若只搬颜色不搬类型，车会变成"无类型的子"，
	// 后续所有几何校验都会失效（表现为模型可以乱走）。
	rules, _ := Lookup(KindXiangqi)
	pos := mustPosition(t, KindXiangqi)
	next := rules.Apply(pos, Move{Color: ColorBlack, From: Point{0, 0}, To: Point{0, 1}})

	if next.PlacedAt(Point{0, 1}) != PieceChariot {
		t.Errorf("走子后类型 = %v，期望车", next.PlacedAt(Point{0, 1}))
	}
	if next.PlacedAt(Point{0, 0}) != PieceNone {
		t.Error("起点应被清空")
	}
	if next.At(Point{0, 0}) != ColorNone {
		t.Error("起点颜色应被清空")
	}
	if next.ToMove != ColorWhite {
		t.Errorf("手番应切换到白方，实际 %v", next.ToMove)
	}
}

func TestXiangqi_ApplyCaptureChangesStoneCount(t *testing.T) {
	rules, _ := Lookup(KindXiangqi)
	pos := mustPosition(t, KindXiangqi)
	// 黑车吃掉 (0,9) 的白车需要清空路径
	for y := 1; y <= 8; y++ {
		place(pos, 0, y, ColorNone, PieceNone)
	}
	before := pos.StoneCount(ColorWhite)
	next := rules.Apply(pos, Move{Color: ColorBlack, From: Point{0, 0}, To: Point{0, 9}})
	if got := next.StoneCount(ColorWhite); got != before-1 {
		t.Errorf("白方子数 = %d，期望 %d", got, before-1)
	}
	if next.PlacedAt(Point{0, 9}) != PieceChariot || next.At(Point{0, 9}) != ColorBlack {
		t.Error("黑车应占据 (0,9)")
	}
}

// ── 合法性错误必须是可分类的具名错误 ──────────────────────────

func TestErrors_AreClassifiable(t *testing.T) {
	// 上层要靠错误类型决定处置（占位可重试、越界说明模型没看懂棋盘）。
	// 若错误退化成普通 fmt.Errorf，这些分支会全部失效。
	gomoku, _ := Lookup(KindGomoku)
	goRules, _ := Lookup(KindGo)
	xiangqi, _ := Lookup(KindXiangqi)

	occ := mustPosition(t, KindGomoku)
	place(occ, 1, 1, ColorBlack, PieceStone)
	if err := gomoku.Validate(occ, Move{Color: ColorWhite, Point: Point{1, 1}}); !errors.Is(err, ErrOccupied) {
		t.Error("五子棋占位错误不可分类")
	}

	ko := mustPosition(t, KindGo)
	if err := goRules.Validate(ko, Move{Color: ColorBlack, Point: Point{99, 99}}); !errors.Is(err, ErrOutOfBoard) {
		t.Error("围棋越界错误不可分类")
	}

	xq := mustPosition(t, KindXiangqi)
	if err := xiangqi.Validate(xq, Move{Color: ColorBlack, From: Point{0, 0}, To: Point{0, 0}}); !errors.Is(err, ErrIllegalMove) {
		t.Error("象棋原地不动错误不可分类")
	}
}
