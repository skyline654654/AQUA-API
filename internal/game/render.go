// 本文件把棋局渲染成 PNG 图片——这是整个演示"多模态"之所以成立的一环。
//
// 意图（Why）：
//
//	把棋盘画成图片而不是交给模型一个数字矩阵，是本功能的核心决定：
//	给矩阵测的是"模型会不会读数组"，任何模型都能应付；给图片测的是
//	它能否从像素里认出棋子、位置与坐标——这才是多模态的真实能力边界。
//
//	渲染必须【完全自行绘制，不调用图片生成模型】：
//	  · 棋盘是规则图形，生成模型会在格线数量、棋子位置上出错，
//	    而一格之差就会让模型的合法着法被判非法，整盘棋作废；
//	  · 每手都要出图，用生成模型意味着每手多一次调用与费用；
//	  · 确定性渲染可复现——同一局面永远得到同一张图，
//	    这样"模型这手为什么走错"才是可排查的。
//
// 字体方案（为什么自绘 5×7 位图字体）：
//
//	渲染需要画坐标字母与象棋棋子标识，但本仓库约束"只能引入纯 Go 依赖"，
//	而 CJK 字体动辄数 MB、也不该为一张演示图进仓库。
//	因此这里内置一套自制 5×7 位图字体，只用得上 29 个字形
//	（列字母 A..T 去掉 I、数字 0..9）。
//
//	象棋棋子用【拉丁字母标识】（K/A/E/H/R/C/P）而不是汉字：
//	汉字需要 CJK 字体，而拉丁标识是象棋程序界的通行做法（同 FEN），
//	且提示词里会给出完整对照表，模型不需要任何"认字"能力。
//
// 流转（Flow）：
//
//	arena → RenderPNG(pos, header)
//	  → drawBoard（格线 / 星位 / 九宫 / 河界）
//	    → drawStones（棋子）
//	      → drawCoords（坐标标注，四边）
//	        → 2 倍超采样后降采样（抗锯齿）
//
// 扩展（Extend）：
//
//	新增棋种：在 metricsFor 里补画布参数、在 drawBoard 补该棋种的格线分支、
//	在 glyphFor 补棋子字形（若新棋种有棋子类型）。
//	调整视觉：改下方的调色板常量；注意这些颜色是【渲染进图片】给模型看的，
//	与前端 UI 的 Design Token 无关，不受"禁止硬编码颜色"那条前端规则约束
//	（那条规则针对的是界面主题一致性，而这里是图像内容本身）。
package game

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
)

// 渲染调色板。
//
// 选择依据是"视觉模型读图的对比度"，而不是好看：
// 棋盘用浅木色、格线用深棕，黑白子在浅底上都有足够反差，
// 且白子加深色描边——否则白子在浅色棋盘上会"糊"进背景，
// 模型会漏看不存在的空位（这是最初版本实测到的问题）。
var (
	colorBoardBG   = color.RGBA{R: 0xF0, G: 0xDF, B: 0xC0, A: 0xFF} // 棋盘底色：浅木色
	colorGridLine  = color.RGBA{R: 0x5A, G: 0x3E, B: 0x22, A: 0xFF} // 格线：深棕
	colorStoneBlk  = color.RGBA{R: 0x14, G: 0x14, B: 0x18, A: 0xFF} // 黑子
	colorStoneWht  = color.RGBA{R: 0xFA, G: 0xFA, B: 0xF8, A: 0xFF} // 白子
	colorStoneEdge = color.RGBA{R: 0x30, G: 0x30, B: 0x34, A: 0xFF} // 白子描边
	colorMarker    = color.RGBA{R: 0xD1, G: 0x1E, B: 0x2E, A: 0xFF} // 末手标记：正红
	colorHeaderBG  = color.RGBA{R: 0x1C, G: 0x24, B: 0x30, A: 0xFF} // 顶部信息条底色
	colorHeaderFG  = color.RGBA{R: 0xF2, G: 0xF5, B: 0xFA, A: 0xFF} // 顶部信息条文字
	colorLabelFG   = color.RGBA{R: 0x3A, G: 0x2A, B: 0x18, A: 0xFF} // 坐标标注文字
	colorXQBlack   = color.RGBA{R: 0x18, G: 0x18, B: 0x1C, A: 0xFF} // 象棋黑方棋子字色
	colorXQRed     = color.RGBA{R: 0xB3, G: 0x1B, B: 0x22, A: 0xFF} // 象棋红方棋子字色
)

// renderScale 是超采样倍数。
//
// 先按 2 倍尺寸绘制再降采样，等于免费的 2×2 抗锯齿。
// 对视觉模型而言，平滑的边缘能显著降低"把棋子看成两个"或
// "把交叉点看成有子"的误判——比在绘制时做复杂抗锯齿简单得多。
const renderScale = 2

// canvas 描述一次渲染的画布几何（单位：最终像素）。
type canvas struct {
	// MarginX 是棋盘左侧留给【行号】的空白。
	// MarginY 是棋盘上方（信息条之下）留给【列字母】的空白。
	//
	// 两个方向分开算而不是用一个 margin：列字母只需要高度，
	// 行号只需要宽度，而象棋的棋子半径（24）远大于字母高度，
	// 合用一个值就得取两者的最大值，白白浪费另一边的空间。
	MarginX int
	MarginY int
	// Cell 是相邻两条格线的间距。
	Cell int
	// Cols / Rows 是格线数量（围棋 19 路即 19 条线）。
	Cols, Rows int
	// HeaderH 是顶部信息条高度；0 表示不画。
	HeaderH int
	// StoneRadius 是棋子半径（三种棋都用它作为"棋子占据范围"的统一口径）。
	StoneRadius int
}

// labelScale 是坐标标注的字形放大倍数。
const labelScale = 2

// labelHeight 是坐标标注的实际像素高度。
const labelHeight = glyphHeight * labelScale

// labelGap 是标注与棋子之间的最小间距。
//
// 必须有这个间距：首版没留，结果 A 列的行号被棋子圆盘整块盖住，
// 图上只剩零星几个行号（表现为"有些行没有编号"，极难一眼看出是遮挡）。
const labelGap = 8

// metricsFor 返回棋种的画布参数。
//
// 边距由【棋子半径 + 标注尺寸】反推，而不是手填常数：
// 手填的常数在改棋子大小后会立刻失效，而失效形式是"标注被盖住"——
// 一种不会报错、只能靠肉眼发现的 bug。
func metricsFor(kind Kind) canvas {
	var cell, cols, rows, radius int
	switch kind {
	case KindGomoku:
		cell, cols, rows, radius = 40, 15, 15, 18
	case KindGo:
		cell, cols, rows, radius = 40, 19, 19, 18
	case KindXiangqi:
		// 象棋格子更大：格内要放"圆盘 + 字母"，太小则字母糊成一团
		cell, cols, rows, radius = 54, 9, 10, 24
	default:
		cell, cols, rows, radius = 40, 15, 15, 18
	}
	// 最长的行号（如 "19"）决定左侧需要多宽
	maxRowLabelW := textWidth(strings.Repeat("9", len(itoa(rows))), labelScale)
	return canvas{
		MarginX:     radius + labelGap + maxRowLabelW + 4,
		MarginY:     radius + labelGap + labelHeight,
		Cell:        cell,
		Cols:        cols,
		Rows:        rows,
		HeaderH:     40,
		StoneRadius: radius,
	}
}

// itoa 是整数转字符串的本地实现（避免为一次转换引入 strconv）。
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [12]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// RenderPNG 把局面渲染为 PNG 字节流。
//
// header 会画在顶部信息条上；调用方用它传递"棋种 / 该谁走 / 第几手"
// 这类模型需要但图片本身表达不了的信息。
func RenderPNG(pos *Position, header string) ([]byte, error) {
	if pos == nil {
		return nil, fmt.Errorf("game: 渲染失败：局面为空")
	}
	m := metricsFor(pos.Kind)

	// 画布尺寸：格线区域 = (cols-1) 个格距；
	// 右侧与下方各留 margin 的一半作为对称留白（标注只画在左/上，
	// 但不留白会让棋盘紧贴图片边缘，视觉上难以判断边界）。
	innerW := (m.Cols - 1) * m.Cell
	innerH := (m.Rows - 1) * m.Cell
	width := innerW + m.MarginX + m.MarginX/2
	height := innerH + m.MarginY + m.MarginY/2 + m.HeaderH
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("game: 渲染失败：画布尺寸非法 %dx%d", width, height)
	}

	// 在放大后的画布上绘制，最后降采样
	big := image.NewRGBA(image.Rect(0, 0, width*renderScale, height*renderScale))
	c := &pen{img: big, scale: renderScale, m: m, w: width, h: height}

	c.fillAll(colorBoardBG)
	if m.HeaderH > 0 && header != "" {
		c.fillRect(0, 0, width, m.HeaderH, colorHeaderBG)
		c.drawText(header, 12, m.HeaderH/2-glyphHeight, 2, colorHeaderFG)
	}
	c.drawCoords(pos, m)
	c.drawGrid(pos)
	c.drawStones(pos)

	final := downsample(big, renderScale)
	var buf bytes.Buffer
	if err := png.Encode(&buf, final); err != nil {
		return nil, fmt.Errorf("game: PNG 编码失败: %w", err)
	}
	return buf.Bytes(), nil
}

// pen 是绘制上下文。
//
// 所有坐标都按【最终像素】传入，由 pen 内部乘以 scale——
// 这样上层绘制逻辑只关心逻辑坐标，不必到处写 *renderScale。
type pen struct {
	img   *image.RGBA
	scale int
	m     canvas
	w, h  int
}

// px 把最终坐标转换为放大画布上的坐标。
func (p *pen) px(x, y int) (int, int) { return x * p.scale, y * p.scale }

// fillAll 用纯色铺满整张画布。
func (p *pen) fillAll(c color.RGBA) {
	p.fillRect(0, 0, p.w, p.h, c)
}

// fillRect 填充矩形（最终坐标）。
func (p *pen) fillRect(x, y, w, h int, c color.RGBA) {
	x0, y0 := p.px(x, y)
	x1, y1 := p.px(x+w, y+h)
	for yy := y0; yy < y1; yy++ {
		if yy < 0 || yy >= p.img.Bounds().Dy() {
			continue
		}
		for xx := x0; xx < x1; xx++ {
			if xx < 0 || xx >= p.img.Bounds().Dx() {
				continue
			}
			p.img.SetRGBA(xx, yy, c)
		}
	}
}

// drawLineThick 画一条有宽度的线段（最终坐标）。
//
// 用"沿主方向逐点推进 + 垂直方向铺开"的朴素算法而不是 Bresenham：
// 棋盘上的线只有水平与垂直两种（象棋九宫斜线也是 45°），
// 朴素算法在轴上更直观，且厚度控制比 Bresenham 自然。
func (p *pen) drawLineThick(x0, y0, x1, y1, thick int, c color.RGBA) {
	if thick < 1 {
		thick = 1
	}
	dx := x1 - x0
	dy := y1 - y0
	steps := abs(dx)
	if abs(dy) > steps {
		steps = abs(dy)
	}
	if steps == 0 {
		p.fillRect(x0, y0, thick, thick, c)
		return
	}
	// 半厚向两侧铺开，使线以给定坐标为中心
	half := thick / 2
	for i := 0; i <= steps; i++ {
		x := x0 + dx*i/steps
		y := y0 + dy*i/steps
		if dx == 0 {
			// 垂直线
			p.fillRect(x-half, y, thick, 1, c)
		} else if dy == 0 {
			// 水平线
			p.fillRect(x, y-half, 1, thick, c)
		} else {
			// 斜线（象棋九宫）：按方形笔头铺开
			p.fillRect(x-half, y-half, thick, thick, c)
		}
	}
}

// fillCircle 填充圆形（最终坐标）。
func (p *pen) fillCircle(cx, cy, r int, c color.RGBA) {
	if r <= 0 {
		return
	}
	x0, y0 := p.px(cx-r, cy-r)
	x1, y1 := p.px(cx+r, cy+r)
	rr := r * p.scale
	ccx, ccy := cx*p.scale, cy*p.scale
	for yy := y0; yy <= y1; yy++ {
		if yy < 0 || yy >= p.img.Bounds().Dy() {
			continue
		}
		for xx := x0; xx <= x1; xx++ {
			if xx < 0 || xx >= p.img.Bounds().Dx() {
				continue
			}
			ddx := xx - ccx
			ddy := yy - ccy
			if ddx*ddx+ddy*ddy <= rr*rr {
				p.img.SetRGBA(xx, yy, c)
			}
		}
	}
}

// strokeCircle 画圆环（用于白子描边与象棋棋子的双圈）。
func (p *pen) strokeCircle(cx, cy, r, thick int, c color.RGBA) {
	if r <= 0 {
		return
	}
	x0, y0 := p.px(cx-r-thick, cy-r-thick)
	x1, y1 := p.px(cx+r+thick, cy+r+thick)
	outer := (r + thick) * p.scale
	inner := (r - thick) * p.scale
	if inner < 0 {
		inner = 0
	}
	ccx, ccy := cx*p.scale, cy*p.scale
	for yy := y0; yy <= y1; yy++ {
		if yy < 0 || yy >= p.img.Bounds().Dy() {
			continue
		}
		for xx := x0; xx <= x1; xx++ {
			if xx < 0 || xx >= p.img.Bounds().Dx() {
				continue
			}
			ddx := xx - ccx
			ddy := yy - ccy
			d2 := ddx*ddx + ddy*ddy
			if d2 <= outer*outer && d2 >= inner*inner {
				p.img.SetRGBA(xx, yy, c)
			}
		}
	}
}

// drawText 用内置位图字体绘制 ASCII 文本（最终坐标，左上角定位）。
//
// scale 是字形放大倍数（1 表示原生 5×7 像素）。
func (p *pen) drawText(text string, x, y, scale int, c color.RGBA) {
	if scale < 1 {
		scale = 1
	}
	cursor := x
	for _, r := range text {
		glyph, ok := bitmapFont[r]
		if !ok {
			// 未知字符画成方框：比静默跳过好——坐标标签缺一个字母
			// 会让模型整列定位错位，画个框至少看得出来"这里没渲染出来"。
			glyph = bitmapFont['?']
		}
		for row := 0; row < glyphHeight; row++ {
			bits := glyph[row]
			for col := 0; col < glyphWidth; col++ {
				// 位 4 是最左列
				if bits&(1<<(glyphWidth-1-col)) != 0 {
					p.fillRect(cursor+col*scale, y+row*scale, scale, scale, c)
				}
			}
		}
		cursor += (glyphWidth + 1) * scale
	}
}

// textWidth 估算文本渲染后的像素宽度（用于居中）。
func textWidth(text string, scale int) int {
	if len(text) == 0 {
		return 0
	}
	return len(text)*(glyphWidth+1)*scale - scale
}

// origin 返回第 (col,row) 条格线的画布坐标（最终像素，棋盘交点）。
func (m canvas) origin(col, row int) (int, int) {
	return m.MarginX + col*m.Cell, m.HeaderH + m.MarginY + row*m.Cell
}

// maxLabelWidth 返回本棋盘最长行号的像素宽度（迭代求，避免依赖 rows 的具体位数）。
func (m canvas) maxLabelWidth() int {
	return textWidth(itoa(m.Rows), labelScale)
}

// drawGrid 按棋种绘制格线（含星位 / 九宫 / 河界）。
func (p *pen) drawGrid(pos *Position) {
	m := p.m
	thick := 2
	// 外框加粗：让模型能立刻分辨棋盘边界，尤其在图片被缩放后
	borderThick := 3

	// 横线（三种棋都画满）
	for row := 0; row < m.Rows; row++ {
		x0, y := m.origin(0, row)
		x1, _ := m.origin(m.Cols-1, row)
		t := thick
		if row == 0 || row == m.Rows-1 {
			t = borderThick
		}
		p.drawLineThick(x0, y, x1, y, t, colorGridLine)
	}

	// 竖线
	for col := 0; col < m.Cols; col++ {
		x, y0 := m.origin(col, 0)
		_, y1 := m.origin(col, m.Rows-1)
		t := thick
		if col == 0 || col == m.Cols-1 {
			t = borderThick
		}
		if pos.Kind == KindXiangqi && col != 0 && col != m.Cols-1 {
			// 象棋河界：中间列在楚河汉界处断开（传统画法）
			_, riverTop := m.origin(col, xiangqiRiverY)
			_, riverBottom := m.origin(col, xiangqiRiverY+1)
			p.drawLineThick(x, y0, x, riverTop, t, colorGridLine)
			p.drawLineThick(x, riverBottom, x, y1, t, colorGridLine)
			continue
		}
		p.drawLineThick(x, y0, x, y1, t, colorGridLine)
	}

	switch pos.Kind {
	case KindGo:
		p.drawStarPoints(m)
	case KindXiangqi:
		p.drawPalaces(m)
	}
}

// drawStarPoints 画围棋星位（9 个）。
func (p *pen) drawStarPoints(m canvas) {
	stars := [][2]int{
		{3, 3}, {9, 3}, {15, 3},
		{3, 9}, {9, 9}, {15, 9},
		{3, 15}, {9, 15}, {15, 15},
	}
	for _, s := range stars {
		x, y := m.origin(s[0], s[1])
		p.fillCircle(x, y, 4, colorGridLine)
	}
}

// drawPalaces 画象棋九宫的两条斜线。
func (p *pen) drawPalaces(m canvas) {
	// 黑方九宫（上）：X 3..5，Y 0..2
	x3, y0 := m.origin(3, 0)
	x5, y2 := m.origin(5, 2)
	p.drawLineThick(x3, y0, x5, y2, 2, colorGridLine)
	p.drawLineThick(x5, y0, x3, y2, 2, colorGridLine)
	// 红方九宫（下）：X 3..5，Y 7..9
	_, y7 := m.origin(3, 7)
	_, y9 := m.origin(5, 9)
	p.drawLineThick(x3, y7, x5, y9, 2, colorGridLine)
	p.drawLineThick(x5, y7, x3, y9, 2, colorGridLine)
}

// drawStones 绘制棋子与末手标记。
func (p *pen) drawStones(pos *Position) {
	m := p.m
	for row := 0; row < m.Rows; row++ {
		for col := 0; col < m.Cols; col++ {
			pt := Point{col, row}
			c := pos.At(pt)
			if c == ColorNone {
				continue
			}
			x, y := m.origin(col, row)
			if pos.Kind == KindXiangqi {
				p.drawXiangqiPiece(x, y, m.StoneRadius, pos, pt, c)
			} else {
				p.drawStone(x, y, m.StoneRadius, c)
			}
		}
	}
	// 末手标记最后画，保证盖在棋子上
	if pos.LastMove != nil && !pos.LastMove.Resign && !pos.LastMove.Pass {
		target := pos.LastMove.Point
		if pos.Kind == KindXiangqi {
			target = pos.LastMove.To
		}
		if pos.InBounds(target) {
			x, y := m.origin(target.X, target.Y)
			p.drawLastMoveMarker(x, y, m.StoneRadius)
		}
	}
}

// drawStone 画一个落子类棋子（五子棋 / 围棋）。
func (p *pen) drawStone(cx, cy, r int, c Color) {
	if c == ColorWhite {
		// 白子先描边再填白：浅色棋盘上不描边的白子会与背景糊在一起，
		// 模型会把它当成空位（实测问题）。
		p.strokeCircle(cx, cy, r, 2, colorStoneEdge)
		p.fillCircle(cx, cy, r-1, colorStoneWht)
		return
	}
	p.fillCircle(cx, cy, r, colorStoneBlk)
}

// drawLastMoveMarker 用红色方框标记最后一手。
//
// 用方框而不是"高亮棋子本身"：末手可能是吃子（原棋子已不在原位）
// 也可能是走子（原位置已空），方框能同时覆盖"落点"与"走到这里"两种语义。
func (p *pen) drawLastMoveMarker(cx, cy, radius int) {
	size := radius + 4
	p.drawLineThick(cx-size, cy-size, cx+size, cy-size, 2, colorMarker)
	p.drawLineThick(cx-size, cy+size, cx+size, cy+size, 2, colorMarker)
	p.drawLineThick(cx-size, cy-size, cx-size, cy+size, 2, colorMarker)
	p.drawLineThick(cx+size, cy-size, cx+size, cy+size, 2, colorMarker)
}

// drawXiangqiPiece 画一个象棋棋子（圆盘 + 标准汉字）。
//
// 棋子必须写汉字而不是拉丁字母：象棋的辨识习惯就是"看字认子"，
// 换成 K/A/E 会让人第一眼认不出这是象棋。汉字字形见 glyph_cjk.go
// （内嵌 32×32 灰度位图——Go 侧拿不到 CJK 字体，见该文件说明）。
func (p *pen) drawXiangqiPiece(cx, cy, r int, pos *Position, pt Point, c Color) {
	body := colorXQBlack
	if c == ColorWhite {
		body = colorXQRed
	}
	// 棋子底：浅色圆盘，保证汉字有足够对比度（深底黑字会糊成一团）
	p.fillCircle(cx, cy, r, colorStoneWht)
	p.strokeCircle(cx, cy, r, 2, body)
	// 内圈：象棋棋子的传统样式，也让模型更容易识别"这是一个棋子"
	p.strokeCircle(cx, cy, r-5, 1, body)

	ch := xiangqiGlyph(pos.PlacedAt(pt), c)
	if ch == 0 {
		return
	}
	// 字形尺寸按【最终像素】给：1 表示"一个字形像素占一个最终像素"，
	// 于是整个字是 32px，正好落在直径 48 的圆盘内并留出 8px 边距。
	//
	// 注意单位必须与 cx/cy 一致（它们都是最终像素，见 canvas.origin）。
	// 首版误把这里当成"放大画布的倍数"传了 2，导致字形放大一倍且偏移。
	// 传 1 还有个额外好处：因为先按 renderScale 放大绘制再降采样，
	// 每个字形像素恰好对应整数个放大像素，降采样后边缘干净不糊。
	p.drawGlyphGray(ch, cx, cy, 1, body)
}

// drawGlyphGray 以 (cx,cy) 为中心、按覆盖度 alpha 混合绘制内嵌灰度字形。
//
// scale 的含义是【一个字形像素占几个最终像素】，与 cx/cy 同单位。
//
// 用 alpha 混合而不是"覆盖度过半就填实"：中文笔画在 32px 下只有 1~2 像素宽，
// 二值化会随机切断笔画（实测「士」的横、「车」的竖都会被切断），
// 而按覆盖度混合能保住笔画的连续性。
func (p *pen) drawGlyphGray(ch rune, cx, cy, scale int, ink color.RGBA) {
	rows, ok := cjkGlyph[ch]
	if !ok || scale < 1 {
		return
	}
	w := cjkGlyphSize * scale
	h := cjkGlyphSize * scale
	left := cx - w/2
	top := cy - h/2
	for r := 0; r < cjkGlyphSize; r++ {
		line := rows[r]
		for c := 0; c < cjkGlyphSize && c < len(line); c++ {
			v := hexValue(line[c])
			if v == 0 {
				continue
			}
			// 4bit 覆盖度 → 8bit alpha（0→0，15→255）
			alpha := uint8(v * 17)
			p.fillRectBlend(left+c*scale, top+r*scale, scale, scale, ink, alpha)
		}
	}
}

// hexValue 解析单个十六进制字符（非法字符返回 0）。
func hexValue(ch byte) int {
	switch {
	case ch >= '0' && ch <= '9':
		return int(ch - '0')
	case ch >= 'a' && ch <= 'f':
		return int(ch-'a') + 10
	case ch >= 'A' && ch <= 'F':
		return int(ch-'A') + 10
	}
	return 0
}

// fillRectBlend 用给定 alpha 把 ink 混合到现有像素上。
//
// 与 fillRect 的区别：fillRect 直接覆盖，这里是半透明叠加。
// 字形抗锯齿必须走这条路径，否则边缘会是硬台阶。
func (p *pen) fillRectBlend(x, y, w, h int, ink color.RGBA, alpha uint8) {
	if alpha == 0 {
		return
	}
	if alpha == 255 {
		p.fillRect(x, y, w, h, ink)
		return
	}
	x0, y0 := p.px(x, y)
	x1, y1 := p.px(x+w, y+h)
	inv := 255 - int(alpha)
	a := int(alpha)
	bounds := p.img.Bounds()
	for yy := y0; yy < y1; yy++ {
		if yy < 0 || yy >= bounds.Dy() {
			continue
		}
		for xx := x0; xx < x1; xx++ {
			if xx < 0 || xx >= bounds.Dx() {
				continue
			}
			dst := p.img.RGBAAt(xx, yy)
			p.img.SetRGBA(xx, yy, color.RGBA{
				R: uint8((int(dst.R)*inv + int(ink.R)*a) / 255),
				G: uint8((int(dst.G)*inv + int(ink.G)*a) / 255),
				B: uint8((int(dst.B)*inv + int(ink.B)*a) / 255),
				A: 0xFF,
			})
		}
	}
}

// drawCoords 在棋盘上方 / 左侧绘制坐标标注。
//
// 只画上边与左边（不画四边）：四边标注会让图更乱，
// 且模型只需两个方向的参照就能定位；上/左与"行号从上方起"的约定一致。
//
// 位置严格避开棋子占据范围（stoneRadius + labelGap）——
// 首版没避，A 列行号被棋子盖住，图上只剩零星几个编号。
func (p *pen) drawCoords(_ *Position, m canvas) {
	// 列字母：底边贴着"首行棋子之上"
	for col := 0; col < m.Cols; col++ {
		label := string(coordLetter(col))
		x0, y0 := m.origin(col, 0)
		labelY := y0 - m.StoneRadius - labelGap - labelHeight
		p.drawText(label, x0-textWidth(label, labelScale)/2, labelY, labelScale, colorLabelFG)
	}
	// 行号：右边界贴着"首列棋子之左"
	for row := 0; row < m.Rows; row++ {
		label := itoa(row + 1)
		x0, y0 := m.origin(0, row)
		labelX := x0 - m.StoneRadius - labelGap - textWidth(label, labelScale)
		p.drawText(label, labelX, y0-labelHeight/2, labelScale, colorLabelFG)
	}
}

// downsample 以平均值降采样（盒子滤波），实现抗锯齿。
func downsample(src *image.RGBA, factor int) *image.RGBA {
	if factor <= 1 {
		return src
	}
	b := src.Bounds()
	dstW := b.Dx() / factor
	dstH := b.Dy() / factor
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	for y := 0; y < dstH; y++ {
		for x := 0; x < dstW; x++ {
			var rs, gs, bs, as, n int
			for dy := 0; dy < factor; dy++ {
				for dx := 0; dx < factor; dx++ {
					r, g, bl, a := src.At(b.Min.X+x*factor+dx, b.Min.Y+y*factor+dy).RGBA()
					rs += int(r >> 8)
					gs += int(g >> 8)
					bs += int(bl >> 8)
					as += int(a >> 8)
					n++
				}
			}
			if n == 0 {
				continue
			}
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(rs / n), G: uint8(gs / n), B: uint8(bs / n), A: uint8(as / n),
			})
		}
	}
	return dst
}

// coordLetter 返回第 col 列的标注字母（跳过 I）。
func coordLetter(col int) rune {
	const letters = "ABCDEFGHJKLMNOPQRSTUVWXYZ"
	if col < 0 || col >= len(letters) {
		return '?'
	}
	return rune(letters[col])
}

// xiangqiGlyph 返回棋子应写的汉字。
//
// 红黑两方的写法不同，这是象棋的既有习惯（也是辨认双方的直观依据）：
//
//	黑方：将 士 象 马 车 砲 卒
//	红方：帅 仕 相 马 车 炮 兵
//
// 其中「砲/炮」「兵/卒」的红黑之分是传统写法：
// 红方用火字旁的「炮」，黑方用石字旁的「砲」。
// 现代简化字常把两方都写作「炮」，但保留这个区分更贴近实体棋具，
// 也让模型多一个"红黑"的视觉线索。
func xiangqiGlyph(p Piece, c Color) rune {
	black := c == ColorBlack
	switch p {
	case PieceKing:
		if black {
			return '将'
		}
		return '帅'
	case PieceAdvisor:
		if black {
			return '士'
		}
		return '仕'
	case PieceElephant:
		if black {
			return '象'
		}
		return '相'
	case PieceHorse:
		return '马'
	case PieceChariot:
		return '车'
	case PieceCannon:
		if black {
			return '砲'
		}
		return '炮'
	case PiecePawn:
		if black {
			return '卒'
		}
		return '兵'
	}
	return 0
}
