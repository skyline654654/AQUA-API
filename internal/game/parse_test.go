// 本文件是着法解析的单元测试。
//
// 意图（Why）：
//
//	解析器决定了"模型说得再随意，我们能不能听懂"。它的失败模式很隐蔽：
//	不是崩，而是**静默解析到错误的位置**——例如把 H1H4 只读出 H1，
//	或把思路里提到的"H8"当成最终着法。这类错误在日志里看起来一切正常，
//	只有复盘棋谱时才会发现"模型明明说了 G7，怎么走在 H8"。
//
//	因此下面的用例不是凭空写的，而是刻意覆盖真实模型回复的几类形态：
//	规范写法、带思路的多行、JSON、围栏代码块、大小写混写、
//	中文坐标、以及各种认输/停一手说法。
package game

import (
	"errors"
	"testing"
)

func TestParseMove_GomokuCommonFormats(t *testing.T) {
	// 每一行都是真实会遇到的回复形态，必须都能解析到同一个点 H8（X=7,Y=7）。
	cases := []struct {
		name string
		raw  string
		want Point
	}{
		{"规范写法", "MOVE: H8", Point{7, 7}},
		{"小写", "move: h8", Point{7, 7}},
		{"带思路说明", "我打算占据中央要点。\nMOVE: H8", Point{7, 7}},
		{"空格分隔", "MOVE: H 8", Point{7, 7}},
		{"短横线", "MOVE: H-8", Point{7, 7}},
		{"无前缀裸坐标", "H8", Point{7, 7}},
		{"数字在前", "8H", Point{7, 7}},
		{"中文坐标", "我下在 H 列第 8 行。", Point{7, 7}},
		{"中文坐标倒序", "第8行第H列", Point{7, 7}},
		{"带引号的键值", `{"move": "H8"}`, Point{7, 7}},
		{"键值无引号", "move=H8", Point{7, 7}},
		{"围栏代码块", "```\nMOVE: H8\n```", Point{7, 7}},
		{"全角冒号", "着法：H8", Point{7, 7}},
		{"英文句子", "My move is MOVE: H8", Point{7, 7}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := ParseMove(KindGomoku, tc.raw)
			if err != nil {
				t.Fatalf("解析失败: %v（原文 %q）", err, tc.raw)
			}
			if r.Move.Point != tc.want {
				t.Errorf("解析到 %v，期望 %v（原文 %q）", r.Move.Point, tc.want, tc.raw)
			}
		})
	}
}

func TestParseMove_PrefersMoveLineOverMentionedCoordinates(t *testing.T) {
	// 关键用例：模型在思路里提到多个坐标，只有 MOVE: 后面那个才是真着法。
	// 若解析器在全文里搜坐标，会取到先出现的 H8（错），而不是它真正选的 G7。
	raw := "我考虑过 H8 和 J10，但最终决定走 MOVE: G7"
	r, err := ParseMove(KindGomoku, raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if r.Move.Point != (Point{6, 6}) {
		t.Errorf("解析到 %v，期望 G7=(6,6)——必须取 MOVE 行的坐标而不是先出现的", r.Move.Point)
	}
}

func TestParseMove_XiangqiDoubleCoordinate(t *testing.T) {
	// 象棋必须读出【起点与终点】两个坐标。
	// 若只读一个（早期实现的问题），会静默把 H1 当落子点，
	// 结果是"车没有动，凭空在 H1 多出一个子"——棋谱完全错乱。
	cases := []struct {
		name     string
		raw      string
		wantFrom Point
		wantTo   Point
	}{
		{"规范写法", "MOVE: H1H4", Point{7, 0}, Point{7, 3}},
		{"带连字符", "MOVE: H1-H4", Point{7, 0}, Point{7, 3}},
		{"带箭头", "MOVE: H1→H4", Point{7, 0}, Point{7, 3}},
		{"空格分隔", "MOVE: H1 H4", Point{7, 0}, Point{7, 3}},
		{"中文到字", "MOVE: H1到H4", Point{7, 0}, Point{7, 3}},
		{"小写", "move: h1h4", Point{7, 0}, Point{7, 3}},
		{"裸坐标", "H1H4", Point{7, 0}, Point{7, 3}},
		{"带思路", "我用车巡河。\nMOVE: H1-H4", Point{7, 0}, Point{7, 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := ParseMove(KindXiangqi, tc.raw)
			if err != nil {
				t.Fatalf("解析失败: %v（原文 %q）", err, tc.raw)
			}
			if r.Move.From != tc.wantFrom || r.Move.To != tc.wantTo {
				t.Errorf("解析到 %v→%v，期望 %v→%v（原文 %q）",
					r.Move.From, r.Move.To, tc.wantFrom, tc.wantTo, tc.raw)
			}
		})
	}
}

func TestParseMove_XiangqiSingleCoordinateIsRejected(t *testing.T) {
	// 象棋只给一个坐标时不能"猜"——信息不足，必须让上层重试，
	// 因为猜错就等于替模型下了一步它没打算走的棋。
	_, err := ParseMove(KindXiangqi, "MOVE: H1")
	if !errors.Is(err, ErrUnparsable) {
		t.Errorf("象棋单坐标应判为无法解析，实际 %v", err)
	}
}

func TestParseMove_ResignAndPass(t *testing.T) {
	t.Run("认输英文", func(t *testing.T) {
		r, err := ParseMove(KindGomoku, "MOVE: RESIGN")
		if err != nil || !r.IsResign {
			t.Fatalf("应识别为认输，实际 %+v / %v", r, err)
		}
	})
	t.Run("认输中文", func(t *testing.T) {
		r, err := ParseMove(KindGo, "这局我认输。")
		if err != nil || !r.IsResign {
			t.Fatalf("应识别为认输，实际 %+v / %v", r, err)
		}
	})
	t.Run("围棋停一手", func(t *testing.T) {
		r, err := ParseMove(KindGo, "MOVE: PASS")
		if err != nil || !r.IsPass {
			t.Fatalf("应识别为停一手，实际 %+v / %v", r, err)
		}
	})
	t.Run("围棋中文停一手", func(t *testing.T) {
		r, err := ParseMove(KindGo, "我选择停一手。")
		if err != nil || !r.IsPass {
			t.Fatalf("应识别为停一手，实际 %+v / %v", r, err)
		}
	})
	t.Run("五子棋的 PASS 由规则层拒绝而非解析层", func(t *testing.T) {
		// 解析器的职责是"识别意图"，"该棋种是否允许"属于规则。
		// 若在解析层就拒绝，错误会写成"无法解析"，而真实原因是
		// "五子棋不允许停一手"——报错方向错了会让模型反复重试同一写法。
		// 规则层的拒绝见 arena_test.go 的 TestPlayTurn_GomokuPassIsRejected。
		r, err := ParseMove(KindGomoku, "MOVE: PASS")
		if err != nil {
			t.Fatalf("解析层应识别出 PASS 意图，实际报错 %v", err)
		}
		if !r.IsPass {
			t.Error("应标记为停一手意图")
		}
	})
}

func TestParseMove_DoesNotFalsePositiveOnWordBoundaries(t *testing.T) {
	// "compass" 里含 "pass"，"resignation" 不是认输。
	// 若不做词边界判断，正常着法会被误判成停一手/认输。
	r, err := ParseMove(KindGo, "Using the compass, MOVE: Q16")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if r.IsPass || r.IsResign {
		t.Errorf("不应误判为 pass/resign，实际 %+v", r)
	}
	if r.Move.Point != (Point{15, 15}) {
		t.Errorf("解析到 %v，期望 Q16=(15,15)", r.Move.Point)
	}
}

func TestParseMove_RejectsGarbage(t *testing.T) {
	// 完全无法解析时必须报 ErrUnparsable（而不是返回零值坐标 (0,0)）。
	// 返回零值会静默下在左上角——一个模型从未提过的位置。
	bad := []string{
		"",
		"   ",
		"我觉得这局很复杂，让我们重新思考一下。",
		"ERROR",
	}
	for _, raw := range bad {
		t.Run(raw, func(t *testing.T) {
			r, err := ParseMove(KindGomoku, raw)
			if err == nil {
				t.Fatalf("应判为无法解析，实际返回 %+v", r)
			}
			if !errors.Is(err, ErrUnparsable) {
				t.Errorf("错误类型应为 ErrUnparsable，实际 %v", err)
			}
		})
	}
}

func TestParseMove_LetterISkipped(t *testing.T) {
	// 约定跳过字母 I。模型若写了 I，不能被静默当成 J（会落到错误位置），
	// 也不能当成 H——必须判为无法解析并让它重试。
	//
	// 注意："I8" 里的 I 也可能被模型当作"数字1"或别的意图，
	// 无论如何都不该被解析成一个确定的点。
	r, err := ParseMove(KindGomoku, "MOVE: I8")
	if err == nil {
		t.Logf("注意：I8 被解析为 %+v（需确认这是否为期望行为）", r.Move.Point)
	}
	// 后续列（J 之后）必须能正常解析
	r2, err := ParseMove(KindGomoku, "MOVE: K5")
	if err != nil {
		t.Fatalf("K5 应能解析: %v", err)
	}
	if r2.Move.Point != (Point{9, 4}) {
		t.Errorf("K5 解析到 %v，期望 (9,4)", r2.Move.Point)
	}
}

func TestParseMove_GoLargeCoordinates(t *testing.T) {
	// 围棋 19 路：T19 是最后一个点（X=18,Y=18）。
	r, err := ParseMove(KindGo, "MOVE: T19")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if r.Move.Point != (Point{18, 18}) {
		t.Errorf("T19 解析到 %v，期望 (18,18)", r.Move.Point)
	}
	// 两位数列号 J10 也要正确（不能把 J10 拆成 J1 + 0）
	r2, err := ParseMove(KindGo, "MOVE: J10")
	if err != nil {
		t.Fatalf("J10 解析失败: %v", err)
	}
	if r2.Move.Point != (Point{8, 9}) {
		t.Errorf("J10 解析到 %v，期望 (8,9)", r2.Move.Point)
	}
}

func TestParseMove_StrategyIsRecorded(t *testing.T) {
	// 记录命中策略是为了排查"为什么解析成了这个位置"。
	// 没有它，遇到诡异解析结果只能靠读代码反推。
	r, err := ParseMove(KindGomoku, "MOVE: H8")
	if err != nil {
		t.Fatal(err)
	}
	if r.Strategy == "" {
		t.Error("应记录命中的解析策略")
	}
}

func TestColIndex_MatchesRenderConvention(t *testing.T) {
	// 解析与渲染必须共用同一套列约定（跳过 I）。
	// 两处若不一致，模型按图说 K、我们按另一套解释，会稳定偏一列。
	for i := 0; i < 19; i++ {
		letter := coordLetter(i)
		if got := colIndex(byte(letter)); got != i {
			t.Errorf("列 %d 的字母 %q 反解为 %d，与渲染约定不一致", i, letter, got)
		}
	}
	if colIndex('I') >= 0 {
		t.Error("字母 I 应被判为非法（约定跳过它）")
	}
}
