// 本文件把模型的自然语言输出解析成结构化着法。
//
// 意图（Why）：
//
//	这是整个对弈里最容易"看起来能用、实际经常崩"的一环。
//	模型即使被明确要求输出 "MOVE: H8"，实际回复仍可能是：
//	  · 带思路说明的多行文本（"我想占住中间…MOVE: H8"）
//	  · JSON（{"move": "H8"}）或围栏代码块（```\nMOVE: H8\n```）
//	  · 坐标写法变异（h8 / 8H / (7,8) / 第8行H列 / H-8）
//	  · 象棋着法用中文记谱（"炮二平五"）或带分隔符（H1-H4 / H1→H4）
//	  · 认输/停一手的各种说法（resign / 认输 / pass / 停一手 / 虚着）
//
//	因此解析必须是【多策略 + 从严格到宽松】的：先认最规范的写法，
//	再逐步放宽。宽松不是"猜"——每个策略都只接受能唯一确定落点的写法，
//	歧义一律交给上层重试，宁可多问一次也不要下错位置。
//
// 设计原则（重要）：
//
//	**解析器只负责"把文本变成候选坐标"，合法性一律交给规则引擎。**
//	如果解析器也去判合法，两处判据必然漂移，且会出现
//	"解析说合法、规则说不合法"这种互相矛盾、极难排查的状态。
//
// 流转（Flow）：
//
//	arena.go → ParseMove(kind, raw) → 候选 Move（未校验）
//	  → rules.Validate 校验 → 合法则 Apply，非法则带原因重试
//
// 扩展（Extend）：
//
//	遇到新的输出风格：在下方候选提取策略里加一条，
//	并在 parse_test.go 补一个用例。注意保持"从严格到宽松"的顺序，
//	更宽松的策略只能放在后面，否则会把严格写法误解析成别的东西。
package game

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ErrUnparsable 表示完全无法从文本中提取出着法。
//
// 与"提取到了但非法"区分开：前者要提示模型"格式不对"，
// 后者要提示"这个位置不合法，原因是……"，两类反馈的内容完全不同。
var ErrUnparsable = fmt.Errorf("game: 无法从回复中解析出着法")

// resignWords / passWords 是认输与停一手的各种说法。
//
// 覆盖中英文与常见变体：模型在中文语境下可能直接说"认输"，
// 也可能输出它训练时更熟悉的 "resign"。
//
// 中文关键词一律选【多字且语义唯一】的写法，绝不收单字：
// 首版把"过"也列为停一手，结果"我考虑过 H8"被整句判成 PASS
// （TestParseMove_PrefersMoveLineOverMentionedCoordinates 抓到了这个）。
// 单字在中文里几乎总会出现在别的意思里，用它做关键词必然误判。
var (
	resignWords = []string{"resign", "i resign", "认输", "投降", "放弃这局", "我输了"}
	passWords   = []string{"pass", "停一手", "停一着", "虚着", "虚手"}
)

// 坐标相关的正则。
var (
	// 形如 H8 / h8 / H 8（列字母 + 行号）
	reLetterNum = regexp.MustCompile(`(?i)\b([A-HJ-T])\s*[-–—]?\s*(\d{1,2})\b`)
	// 形如 8H（行号 + 列字母）
	reNumLetter = regexp.MustCompile(`(?i)\b(\d{1,2})\s*[-–—]?\s*([A-HJ-T])\b`)
	// 形如 (7,8) 或 7,8 或 7 8
	rePairNum = regexp.MustCompile(`\(?\s*(\d{1,2})\s*[,，]\s*(\d{1,2})\s*\)?`)
	// 形如 "第8行H列" / "H列第8行" / "第 8 行 第 H 列"
	reChineseCoord = regexp.MustCompile(`第?\s*([A-HJ-T])\s*列[^\d]{0,6}(\d{1,2})\s*行|第?\s*(\d{1,2})\s*行[^\dA-HJ-T]{0,6}([A-HJ-T])\s*列`)
	// 形如 "move": "H8" / "着法": "H8" / "move=H8"
	reKeyedMove = regexp.MustCompile(`(?i)["“]?(?:move|moves|着法|落子|走法|下在|坐标)["”]?\s*[:：=]\s*["“]?\s*([A-HJ-T]\s*\d{1,2})`)
	// 象棋双坐标：H1H4 / H1-H4 / H1→H4 / H1 H4 / H1到H4
	reXQDouble = regexp.MustCompile(`(?i)\b([A-HJ-T])\s*(\d{1,2})\s*(?:[-–—>→]|到|至|走|移到|吃)?\s*([A-HJ-T])\s*(\d{1,2})\b`)
)

// ParseResult 是解析结果。
type ParseResult struct {
	// Move 是解析出的着法（未做规则校验）。
	Move Move
	// Kind 是识别出的类型：普通着法 / 停一手 / 认输。
	IsPass   bool
	IsResign bool
	// Strategy 记录命中了哪条策略（便于排查"为什么解析成了这个"）。
	Strategy string
}

// ParseMove 从模型输出里解析着法。
//
// 顺序刻意"严格优先"：显式关键字（MOVE: / 认输）比裸坐标更明确，
// 带标签的坐标（"move": "H8"）比裸坐标更明确。严格策略命中即返回，
// 避免宽松策略把明确的信息解读成别的。
func ParseMove(kind Kind, raw string) (ParseResult, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return ParseResult{}, ErrUnparsable
	}

	// ── 策略 0：认输 / 停一手（显式关键字优先级最高）──
	// 先把代码块与 JSON 外壳剥掉，否则 ``` 里的内容可能干扰关键字识别。
	stripped := stripWrappers(text)
	lower := strings.ToLower(stripped)
	if containsAnyWord(lower, resignWords) {
		return ParseResult{IsResign: true, Strategy: "keyword-resign"}, nil
	}
	// 停一手【对三种棋都识别】，是否允许由规则层决定。
	//
	// 为什么不在这里按棋种过滤：解析器的职责是"识别模型的意图"，
	// 不是"判断该棋种是否允许"。若在这里过滤，五子棋的 PASS 会被报成
	// "无法解析"，而真实原因是"该棋种不允许停一手"——两者对模型的
	// 提示完全不同（前者让它改格式，后者让它换个着法），
	// 报错方向错了会让它反复输出 PASS、白耗重试次数。
	if containsAnyWord(lower, passWords) {
		return ParseResult{IsPass: true, Strategy: "keyword-pass"}, nil
	}

	// ── 策略 1：显式 "MOVE:" 行 ──
	// 只看 MOVE 之后的内容，能避开"思路说明里出现的无关坐标"。
	if after, ok := extractAfterMoveKeyword(stripped); ok {
		if r, err := parseCoordinates(kind, after); err == nil {
			r.Strategy = "move-line/" + r.Strategy
			return r, nil
		}
	}

	// ── 策略 2：带标签的坐标（"move": "H8"）──
	if m := reKeyedMove.FindStringSubmatch(stripped); len(m) > 1 {
		if r, err := parseCoordinates(kind, m[1]); err == nil {
			r.Strategy = "keyed-move"
			return r, nil
		}
	}

	// ── 策略 3：全文里找坐标 ──
	if r, err := parseCoordinates(kind, stripped); err == nil {
		r.Strategy = "bare/" + r.Strategy
		return r, nil
	}

	return ParseResult{}, fmt.Errorf("%w（原文前 120 字：%s）", ErrUnparsable, truncateForPrompt(stripped, 120))
}

// parseCoordinates 从一段文本里提取坐标（棋种相关）。
func parseCoordinates(kind Kind, text string) (ParseResult, error) {
	if kind == KindXiangqi {
		// 象棋优先匹配"双坐标"（起点+终点），否则会把 H1H4 只读出一个点。
		if m := reXQDouble.FindStringSubmatch(text); len(m) == 5 {
			from, err1 := coordFromParts(m[1], m[2])
			to, err2 := coordFromParts(m[3], m[4])
			if err1 == nil && err2 == nil {
				return ParseResult{Move: Move{From: from, To: to}, Strategy: "xq-double"}, nil
			}
		}
		// 退化：模型只给了一个点（常见于它想"落子"式地描述）。
		// 不在这里猜"那是指起点还是终点"——信息不足，交给上层重试。
		return ParseResult{}, fmt.Errorf("%w: 象棋需要起点与终点两个坐标", ErrUnparsable)
	}

	// 落子类：中文坐标 → 字母数字 → 数字对
	if pt, ok := parseChineseCoord(text); ok {
		return ParseResult{Move: Move{Point: pt}, Strategy: "cn-coord"}, nil
	}
	if m := reLetterNum.FindStringSubmatch(text); len(m) == 3 {
		if pt, err := coordFromParts(m[1], m[2]); err == nil {
			return ParseResult{Move: Move{Point: pt}, Strategy: "letter-num"}, nil
		}
	}
	if m := reNumLetter.FindStringSubmatch(text); len(m) == 3 {
		if pt, err := coordFromParts(m[2], m[1]); err == nil {
			return ParseResult{Move: Move{Point: pt}, Strategy: "num-letter"}, nil
		}
	}
	// 数字对 (7,8) 视为【0 基】坐标（与 "x,y" 的编程直觉一致），
	// 而字母数字写法是 1 基行号（与棋盘标注一致）。两种约定不同，
	// 因此在提示词里只推荐字母写法，数字对仅作兜底。
	if m := rePairNum.FindStringSubmatch(text); len(m) == 3 {
		x, err1 := strconv.Atoi(m[1])
		y, err2 := strconv.Atoi(m[2])
		if err1 == nil && err2 == nil {
			return ParseResult{Move: Move{Point: Point{X: x, Y: y}}, Strategy: "num-pair"}, nil
		}
	}
	return ParseResult{}, ErrUnparsable
}

// parseChineseCoord 解析"第8行H列"这类中文坐标。
func parseChineseCoord(text string) (Point, bool) {
	m := reChineseCoord.FindStringSubmatch(text)
	if len(m) < 5 {
		return Point{}, false
	}
	col, row := "", ""
	if m[1] != "" && m[2] != "" {
		col, row = m[1], m[2]
	} else if m[3] != "" && m[4] != "" {
		row, col = m[3], m[4]
	} else {
		return Point{}, false
	}
	pt, err := coordFromParts(col, row)
	if err != nil {
		return Point{}, false
	}
	return pt, true
}

// coordFromParts 把"列字母 + 行号字符串"转成坐标。
//
// 行号是 1 基（与棋盘标注一致）：H8 → Y=7。
func coordFromParts(col, row string) (Point, error) {
	col = strings.TrimSpace(strings.ToUpper(col))
	if len(col) != 1 {
		return Point{}, fmt.Errorf("game: 列标识 %q 非法", col)
	}
	x := colIndex(col[0])
	if x < 0 {
		return Point{}, fmt.Errorf("game: 列标识 %q 超出范围", col)
	}
	n, err := strconv.Atoi(strings.TrimSpace(row))
	if err != nil || n < 1 {
		return Point{}, fmt.Errorf("game: 行号 %q 非法", row)
	}
	return Point{X: x, Y: n - 1}, nil
}

// colIndex 把列字母转成 0 基列号；非法返回 -1。
//
// 跳过字母 I：这与渲染和提示词的约定一致。
// 若某模型仍写了 I（训练数据里常见），把它当作 J 处理不合适
// （会静默落到错误位置），因此这里判为非法，让上层带着原因重试。
func colIndex(ch byte) int {
	const letters = "ABCDEFGHJKLMNOPQRSTUVWXYZ"
	for i := 0; i < len(letters); i++ {
		if letters[i] == ch {
			return i
		}
	}
	return -1
}

// extractAfterMoveKeyword 取出 "MOVE:" / "着法：" 之后的内容。
//
// 只在关键词之后取，而不是在全文里找坐标：
// 模型常在思路里提到多个坐标（"我考虑过 H8，但最终选择 G7"），
// 全文搜索会取到第一个（H8）而不是它真正要走的那个（G7）。
func extractAfterMoveKeyword(text string) (string, bool) {
	lowered := strings.ToLower(text)
	for _, kw := range []string{"move:", "move :", "着法:", "着法：", "落子:", "落子：", "走法:", "走法：", "下在:", "下在：", "坐标:", "坐标："} {
		if idx := strings.Index(lowered, kw); idx >= 0 {
			rest := text[idx+len(kw):]
			// 只取关键词之后的第一行：模型可能在后面继续解释，
			// 而下一行往往又出现别的坐标。
			if nl := strings.IndexAny(rest, "\n\r"); nl >= 0 {
				rest = rest[:nl]
			}
			rest = strings.TrimSpace(rest)
			if rest != "" {
				return rest, true
			}
		}
	}
	return "", false
}

// stripWrappers 剥掉常见的包裹层：围栏代码块、JSON 对象外壳。
//
// 剥壳是为了让后续策略能在"内容"上工作，而不是被 ``` 或 {} 干扰。
// 注意：不做部分 JSON 解析——模型输出的 JSON 经常不合法，
// 正则取字段比 json.Unmarshal 更耐操。
func stripWrappers(text string) string {
	s := text
	// 围栏代码块：取第一段 ``` 之间的内容
	if idx := strings.Index(s, "```"); idx >= 0 {
		rest := s[idx+3:]
		// 跳过语言标识那一行
		if nl := strings.IndexAny(rest, "\n\r"); nl >= 0 {
			rest = rest[nl+1:]
		}
		if end := strings.Index(rest, "```"); end >= 0 {
			rest = rest[:end]
		}
		if strings.TrimSpace(rest) != "" {
			s = rest
		}
	}
	return strings.TrimSpace(s)
}

// containsAnyWord 判断文本是否包含任一关键词（按整词/包含两种方式）。
//
// 英文关键词用词边界判断，避免 "compass" 里命中 "pass"、
// "resignation" 之外的误判；中文没有词边界，用包含即可。
func containsAnyWord(lowered string, words []string) bool {
	for _, w := range words {
		if isASCIIWord(w) {
			if containsBoundedWord(lowered, w) {
				return true
			}
			continue
		}
		if strings.Contains(lowered, w) {
			return true
		}
	}
	return false
}

// isASCIIWord 判断是否为纯 ASCII 单词（决定是否用词边界匹配）。
func isASCIIWord(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}

// containsBoundedWord 用词边界匹配 ASCII 单词。
func containsBoundedWord(text, word string) bool {
	for idx := 0; ; {
		pos := strings.Index(text[idx:], word)
		if pos < 0 {
			return false
		}
		start := idx + pos
		end := start + len(word)
		leftOK := start == 0 || !isWordChar(text[start-1])
		rightOK := end >= len(text) || !isWordChar(text[end])
		if leftOK && rightOK {
			return true
		}
		idx = start + 1
		if idx >= len(text) {
			return false
		}
	}
}

// isWordChar 判断是否为单词字符（字母或数字）。
func isWordChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}
