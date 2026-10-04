// Package game 实现对弈演示的棋类规则引擎与模型驱动。
//
// 意图（Why）：
//   AQUA-API 是模型网关，但"网关能力"很难被直观看见——用户看到的是价格表和
//   调用日志，看不见"模型有多强、多模态到底能不能看图"。本包提供一个可见的
//   演示：让两个模型下一盘棋，且棋盘以【图片】形式发给模型。
//
//   为什么必须发图片而不是文本矩阵：
//     如果给模型一个数字矩阵，测的是"模型会不会读数组"，任何模型都能应付；
//     而把棋盘渲染成图，测的是模型能否从像素里认出棋子与坐标——这才是多模态
//     的真实能力边界，也正是本演示要回答的问题。代价是模型更容易走错，
//     但这恰恰是有价值的观测结果，不是缺陷。
//
// 流转（Flow）：
//   server/handler_game.go → arena.go（对局推进）
//     → render.go 把局面画成 PNG
//       → prompt.go 组装多模态提示词（图片 + 规则 + 着法格式）
//         → relay 转发给模型（走既有计费 / BYOK / 审计链路）
//           → parse.go 防御式解析着法
//             → 本包各规则实现校验并应用
//
// 扩展（Extend）：
//   新增棋种：实现 RuleSet 接口 → 在 registry 注册 → 补 render.go 的棋盘绘制
//   分支与坐标约定 → 在 prompt.go 补该棋种的规则说明与着法格式。
//   注意三处必须同时改：规则、渲染、提示词——缺一处模型就会持续走非法着法。
package game

import (
	"fmt"
	"strings"
)

// Kind 是棋种标识。
type Kind string

const (
	// KindGomoku 五子棋：规则最简单、胜负有唯一答案，作为演示的默认棋种。
	KindGomoku Kind = "gomoku"
	// KindGo 围棋：规则最复杂（提子与劫争），最能体现"模型会不会数气"。
	KindGo Kind = "go"
	// KindXiangqi 象棋：着法用"起点-终点"表达，与落子类棋种不同。
	KindXiangqi Kind = "xiangqi"
)

// AllKinds 返回全部已支持的棋种（顺序即前端展示顺序）。
func AllKinds() []Kind { return []Kind{KindGomoku, KindGo, KindXiangqi} }

// ParseKind 解析棋种字符串；无法识别时返回 error 而非默认值——
// 静默回退会让"拼错棋种"表现为"下了一盘别的棋"，排查成本极高。
func ParseKind(raw string) (Kind, error) {
	switch Kind(strings.ToLower(strings.TrimSpace(raw))) {
	case KindGomoku:
		return KindGomoku, nil
	case KindGo:
		return KindGo, nil
	case KindXiangqi:
		return KindXiangqi, nil
	}
	return "", fmt.Errorf("game: 未知棋种 %q", raw)
}

// Color 表示一方。刻意不用 bool：bool 需要额外注释说明"谁先手"，
// 而具名常量让"黑先"这件事在代码里自解释。
type Color uint8

const (
	// ColorNone 表示该点为空（棋盘的第三态）。
	ColorNone Color = 0
	// ColorBlack 黑方，先手。
	ColorBlack Color = 1
	// ColorWhite 白方，后手。
	ColorWhite Color = 2
)

// Opponent 返回对方颜色；ColorNone 原样返回。
func (c Color) Opponent() Color {
	switch c {
	case ColorBlack:
		return ColorWhite
	case ColorWhite:
		return ColorBlack
	}
	return ColorNone
}

// Label 返回中文名（用于日志与前端）。
func (c Color) Label() string {
	switch c {
	case ColorBlack:
		return "黑"
	case ColorWhite:
		return "白"
	}
	return "空"
}

// Status 是对局状态。
type Status string

const (
	// StatusPlaying 进行中。
	StatusPlaying Status = "playing"
	// StatusBlackWin / StatusWhiteWin 胜负已分。
	StatusBlackWin Status = "black_win"
	StatusWhiteWin Status = "white_win"
	// StatusDraw 和棋（围棋双方停一手、或达到手数上限且未分胜负）。
	StatusDraw Status = "draw"
	// StatusAborted 对局被中止（模型连续无法给出合法着法、或超出预算）。
	//
	// 与 Draw 区分开很重要：Draw 是"下完了、平局"，Aborted 是"没下完、
	// 其中一方出了问题"。报告里把二者混为一谈会让人误以为模型"下成了和棋"。
	StatusAborted Status = "aborted"
)

// IsOver 判断对局是否已结束。
func (s Status) IsOver() bool { return s != StatusPlaying }

// Point 是棋盘坐标（0 起，原点在左上）。
type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// IsZero 判断是否为零值点。
func (p Point) IsZero() bool { return p.X == 0 && p.Y == 0 }

// Move 是一手棋。
//
// 同时容纳落子类（五子棋 / 围棋）与走子类（象棋）两种语义：
// 前者只用 Point，后者用 From→To。用一个结构体而不是两个是为了让
// 对局的着法序列可以用同一种类型存储（棋谱、前端渲染、持久化都只有一条路径）。
type Move struct {
	// Color 是下这一手的颜色。
	Color Color `json:"color"`
	// Seq 是手数（从 1 开始），用于校验顺序与前端展示。
	Seq int `json:"seq"`
	// Point 是落子位置（五子棋 / 围棋）。
	Point Point `json:"point"`
	// From / To 是走子的起点与终点（象棋）。
	From Point `json:"from"`
	To   Point `json:"to"`
	// Pass 表示停一手（仅围棋有意义）。
	Pass bool `json:"pass"`
	// Resign 表示认输（模型主动放弃时使用）。
	Resign bool `json:"resign"`
	// Note 是该手的原始输出（模型原话），保留下来供排查与展示——
	// 只看解析结果无法判断"模型是走错了还是输出格式没解析对"。
	Note string `json:"note,omitempty"`
}

// Notation 返回该手的人类可读写法（用于日志；棋种无关的简化格式）。
func (m Move) Notation() string {
	switch {
	case m.Resign:
		return m.Color.Label() + "认输"
	case m.Pass:
		return m.Color.Label() + "停一手"
	case !m.From.IsZero() || !m.To.IsZero():
		return fmt.Sprintf("%s %s->%s", m.Color.Label(), CoordName(m.From), CoordName(m.To))
	default:
		return fmt.Sprintf("%s %s", m.Color.Label(), CoordName(m.Point))
	}
}

// Piece 是棋子类型（仅象棋需要；五子棋与围棋的所有棋子都是 PieceStone）。
//
// 为什么把"类型"与"颜色"分开存而不是合成一个"红车/黑马"枚举：
//   两种棋种（五子棋/围棋）只有颜色没有类型，合成枚举会让它们被迫
//   在 14 个取值里挑一个，规则代码里到处是"忽略类型"的分支。
//   分开存则前者完全不感知 Pieces 字段，后者按需读取，各取所需。
type Piece uint8

const (
	// PieceNone 表示该点无子。
	PieceNone Piece = 0
	// PieceStone 是落子类棋种的棋子（五子棋 / 围棋）。
	PieceStone Piece = 1
	// PieceKing 将 / 帅。
	PieceKing Piece = 2
	// PieceAdvisor 士 / 仕。
	PieceAdvisor Piece = 3
	// PieceElephant 象 / 相。
	PieceElephant Piece = 4
	// PieceHorse 马。
	PieceHorse Piece = 5
	// PieceChariot 车。
	PieceChariot Piece = 6
	// PieceCannon 炮。
	PieceCannon Piece = 7
	// PiecePawn 兵 / 卒。
	PiecePawn Piece = 8
)

// Label 返回棋子中文名（随颜色不同而不同：车的名称两方一致，
// 而将/帅、士/仕、象/相、兵/卒在红黑两方写法不同，这是象棋的既有习惯）。
func (p Piece) Label(c Color) string {
	switch p {
	case PieceStone:
		return "子"
	case PieceKing:
		if c == ColorBlack {
			return "将"
		}
		return "帅"
	case PieceAdvisor:
		if c == ColorBlack {
			return "士"
		}
		return "仕"
	case PieceElephant:
		if c == ColorBlack {
			return "象"
		}
		return "相"
	case PieceHorse:
		return "马"
	case PieceChariot:
		return "车"
	case PieceCannon:
		return "炮"
	case PiecePawn:
		if c == ColorBlack {
			return "卒"
		}
		return "兵"
	}
	return ""
}

// Position 是一个棋局局面。
//
// 用值类型 Cells 的一维切片而非二维：一维在克隆时只需一次 copy，
// 而二维需要逐行分配，在高频克隆（模型走子会反复试探）下差异明显。
type Position struct {
	Kind   Kind   `json:"kind"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	// Cells 是行主序的颜色格，索引 = Y*Width + X。
	Cells []Color `json:"cells"`
	// Pieces 是行主序的类型格，与 Cells 等长同序。
	//
	// 与 Cells 拆成两个切片而不是合成一个结构体切片：五子棋与围棋的
	// 核心算法（气计算、连子判定）只需要颜色，拆开后它们完全不读 Pieces，
	// 新增棋种也不会让既有棋种的算法被迫感知棋子类型。
	Pieces []Piece `json:"pieces,omitempty"`
	// ToMove 是下一手该谁走。
	ToMove Color `json:"to_move"`
	// KoPoint 是围棋的劫争禁着点；nil 表示无禁着。
	//
	// 为什么用指针而不是"哨兵坐标"：棋盘上的 (0,0) 是合法位置，
	// 用哨兵值会让"禁止下在左上角"这种正常规则变成无法表达的情况。
	KoPoint *Point `json:"ko_point,omitempty"`
	// CapturedBlack / CapturedWhite 是各方被提掉的子数（仅围棋统计）。
	//
	// 记的是"被提"而不是"提了对方多少"：前者可由棋盘状态直接重算校验，
	// 后者在打劫回提等场景下容易出现不一致。
	CapturedBlack int `json:"captured_black"`
	CapturedWhite int `json:"captured_white"`
	// LastMove 是上一手（渲染时高亮用）。
	LastMove *Move `json:"last_move,omitempty"`
	// ConsecutivePasses 是连续停一手次数（围棋终局判定）。
	ConsecutivePasses int `json:"consecutive_passes"`
}

// NewPosition 按棋种创建空局面。
func NewPosition(kind Kind) (*Position, error) {
	rules, err := Lookup(kind)
	if err != nil {
		return nil, err
	}
	w, h := rules.Dims()
	pos := &Position{
		Kind:   kind,
		Width:  w,
		Height: h,
		Cells:  make([]Color, w*h),
		Pieces: make([]Piece, w*h),
		ToMove: ColorBlack, // 三种棋都是黑先
	}
	rules.Setup(pos)
	return pos, nil
}

// At 返回某点的颜色；越界返回 ColorNone（而非 panic）。
//
// 越界返回空而不是报错：气计算、象棋着法枚举等都会大量探测边界外的点，
// 每次都判边界会让规则代码充满噪音，且它们本来就该把界外当作"空"。
func (p *Position) At(pt Point) Color {
	if pt.X < 0 || pt.Y < 0 || pt.X >= p.Width || pt.Y >= p.Height {
		return ColorNone
	}
	return p.Cells[pt.Y*p.Width+pt.X]
}

// PlacedAt 返回某点的棋子类型；越界或空点返回 PieceNone。
func (p *Position) PlacedAt(pt Point) Piece {
	if pt.X < 0 || pt.Y < 0 || pt.X >= p.Width || pt.Y >= p.Height {
		return PieceNone
	}
	if p.Pieces == nil {
		return PieceNone
	}
	return p.Pieces[pt.Y*p.Width+pt.X]
}

// Set 在棋盘上放置一个棋子（内部使用，不做合法性判断）。
//
// piece 传 PieceNone 表示清除该点（提子 / 吃子）。同时维护颜色与类型
// 两个切片，避免出现"有色无类"或"有类无色"的不一致状态。
func (p *Position) Set(pt Point, c Color, piece Piece) {
	if pt.X < 0 || pt.Y < 0 || pt.X >= p.Width || pt.Y >= p.Height {
		return
	}
	idx := pt.Y*p.Width + pt.X
	p.Cells[idx] = c
	if p.Pieces != nil {
		if c == ColorNone {
			p.Pieces[idx] = PieceNone
		} else {
			p.Pieces[idx] = piece
		}
	}
}

// SetColor 是 Set 的便捷形式，用于落子类棋种（类型固定为 PieceStone）。
func (p *Position) SetColor(pt Point, c Color) {
	if c == ColorNone {
		p.Set(pt, ColorNone, PieceNone)
		return
	}
	p.Set(pt, c, PieceStone)
}

// InBounds 判断点是否在棋盘内。
func (p *Position) InBounds(pt Point) bool {
	return pt.X >= 0 && pt.Y >= 0 && pt.X < p.Width && pt.Y < p.Height
}

// Clone 深拷贝局面。
//
// 必须深拷贝 Cells 与 Pieces：模型走子时会先"试探"若干着法（枚举合法着法、
// 判断提子结果），若共享底层数组，一次试探就会污染真实局面。
func (p *Position) Clone() *Position {
	cp := *p
	cp.Cells = make([]Color, len(p.Cells))
	copy(cp.Cells, p.Cells)
	if p.Pieces != nil {
		cp.Pieces = make([]Piece, len(p.Pieces))
		copy(cp.Pieces, p.Pieces)
	}
	if p.KoPoint != nil {
		ko := *p.KoPoint
		cp.KoPoint = &ko
	}
	if p.LastMove != nil {
		mv := *p.LastMove
		cp.LastMove = &mv
	}
	return &cp
}

// Empty 判断棋盘是否为空（用于渲染与提示词的"开局"判断）。
func (p *Position) Empty() bool {
	for _, c := range p.Cells {
		if c != ColorNone {
			return false
		}
	}
	return true
}

// StoneCount 统计一方在盘面上的子数。
func (p *Position) StoneCount(c Color) int {
	n := 0
	for _, cell := range p.Cells {
		if cell == c {
			n++
		}
	}
	return n
}

// CoordName 把坐标渲染成人类可读名（如 H8）。
//
// 坐标约定（三种棋统一，且与渲染出的图片严格一致）：
//   - 列用字母，从最左列起为 A（跳过 I，国际惯例，避免与数字 1 混淆）；
//   - 行用数字，从【最上行】起为 1，向下递增。
//
// 为什么行号从上方起而不是沿用围棋/象棋"从己方底线数"的惯例：
//   模型看到的是渲染出的图片，图上第 1 行就在最上面。若坐标名与图上的
//   视觉顺序相反，就会出现"模型说 H8、人和代码都以为在最下面"的经典翻转错位，
//   而这种错位在排查时极难被发现（它会表现为"模型总走错位置"）。
//   演示场景下"人与模型看到同一套编号"比"符合传统记谱"重要得多。
//
// 注意：本函数只用于日志与人工复盘；模型是按图片判断位置的，
// 坐标名不参与模型决策，也就不存在"模型需要理解这套编号"的问题。
func CoordName(pt Point) string {
	const letters = "ABCDEFGHJKLMNOPQRSTUVWXYZ"
	col := ""
	if pt.X >= 0 && pt.X < len(letters) {
		col = string(letters[pt.X])
	} else {
		col = fmt.Sprintf("x%d", pt.X)
	}
	return fmt.Sprintf("%s%d", col, pt.Y+1)
}

// RuleSet 是棋种规则接口。
//
// 为什么把"渲染"与"提示词"排除在接口之外：它们与规则无关（同一套规则可以
// 有完全不同的画法），混在一起会让新增棋种被迫复制无关代码。
// 渲染见 render.go 的 switch，提示词见 prompt.go。
type RuleSet interface {
	// Kind 返回棋种。
	Kind() Kind
	// Dims 返回棋盘宽高。
	Dims() (width, height int)
	// Setup 做开局布置（多数棋种为空盘，象棋需要摆子）。
	Setup(pos *Position)
	// Validate 校验一手是否合法（含提子/吃子等结果性判断）。
	//
	// 上层保证"轮次正确"（由 pos.ToMove 决定谁走），因此 Validate 只关心
	// "这个着法在该棋种下是否成立"，不重复判轮次。
	Validate(pos *Position, mv Move) error
	// Apply 应用一手并返回新局面（不改动入参）。
	Apply(pos *Position, mv Move) *Position
	// Result 判断当前局面的终局状态。
	Result(pos *Position) Status
	// MoveFormat 返回面向模型的着法格式说明。
	MoveFormat() string
}

// registry 是棋种到规则的映射。用 map 而非 switch 便于新增棋种时只改一处。
var registry = map[Kind]RuleSet{
	KindGomoku:  gomokuRules{},
	KindGo:      goRules{},
	KindXiangqi: xiangqiRules{},
}

// Lookup 取得棋种规则。
func Lookup(kind Kind) (RuleSet, error) {
	rules, ok := registry[kind]
	if !ok {
		return nil, fmt.Errorf("game: 棋种 %q 尚未实现规则", kind)
	}
	return rules, nil
}
