package game

import (
	"os"
	"testing"
)

// TestRenderSmoke 生成三种棋的示例图，用于人工目视校验渲染是否正确。
// 这是必要的：渲染错了（格线数量不对、坐标错位、棋子画在错误交点）
// 单元断言很难表达，但一眼就能看出来。
func TestRenderSmoke(t *testing.T) {
	out := os.Getenv("RENDER_OUT")
	if out == "" {
		t.Skip("未设置 RENDER_OUT，跳过目视校验")
	}
	type tc struct {
		kind   Kind
		header string
		setup  func(p *Position)
	}
	cases := []tc{
		{KindGomoku, "GOMOKU  TO MOVE: WHITE", func(p *Position) {
			for i := 0; i < 4; i++ {
				p.SetColor(Point{3 + i, 3 + i}, ColorBlack)
				p.SetColor(Point{3 + i, 4 + i}, ColorWhite)
			}
			mv := Move{Color: ColorWhite, Seq: 8, Point: Point{6, 7}}
			p.SetColor(mv.Point, ColorWhite)
			p.LastMove = &mv
			p.ToMove = ColorBlack
		}},
		{KindGo, "GO  TO MOVE: BLACK  CAPTURED B0/W2", func(p *Position) {
			// 一小片布局 + 一处被围吃的白子
			pts := [][3]int{{3, 3, 1}, {4, 3, 1}, {5, 3, 1}, {3, 4, 2}, {4, 4, 2}, {5, 4, 2}}
			for _, q := range pts {
				c := ColorBlack
				if q[2] == 2 {
					c = ColorWhite
				}
				p.SetColor(Point{q[0], q[1]}, c)
			}
			p.CapturedBlack, p.CapturedWhite = 0, 2
			mv := Move{Color: ColorBlack, Seq: 7, Point: Point{4, 5}}
			p.SetColor(mv.Point, ColorBlack)
			p.LastMove = &mv
			p.ToMove = ColorWhite
		}},
		{KindXiangqi, "XIANGQI  TO MOVE: WHITE", func(p *Position) {
			// 初始局面 + 走一步红车
			mv := Move{Color: ColorBlack, Seq: 1, From: Point{0, 0}, To: Point{0, 1}}
			piece := p.PlacedAt(mv.From)
			p.Set(mv.From, ColorNone, PieceNone)
			p.Set(mv.To, ColorBlack, piece)
			p.LastMove = &mv
			p.ToMove = ColorWhite
		}},
	}
	for _, c := range cases {
		pos, err := NewPosition(c.kind)
		if err != nil {
			t.Fatalf("%s: %v", c.kind, err)
		}
		c.setup(pos)
		data, err := RenderPNG(pos, c.header)
		if err != nil {
			t.Fatalf("%s 渲染失败: %v", c.kind, err)
		}
		path := out + "/" + string(c.kind) + ".png"
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("写入 %s 失败: %v", path, err)
		}
		t.Logf("%s -> %s (%d bytes)", c.kind, path, len(data))
	}
}
