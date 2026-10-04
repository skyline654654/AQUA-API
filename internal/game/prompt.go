// 本文件构造给模型看的提示词（系统提示 + 每手的用户提示）。
//
// 意图（Why）：
//
//	模型能否"正常下完一盘棋"几乎完全取决于提示词，而不是模型本身。
//	缺少坐标约定 → 它按自己想象的编号给坐标；缺少着法格式 → 输出自然语言
//	（"我走马到中间"）而无法解析；缺少合法性要求 → 它给一个不存在的位置，
//	每一步都要重试。这三类问题都表现为"模型很笨"，但根因都在提示词。
//
//	因此本文件的目标很明确：**把"模型需要猜的东西"全部写清楚**。
//	具体包括六块，缺一块都会明显拉高非法着法率：
//	  1. 任务与角色的明确陈述；
//	  2. 图上坐标如何读（列在上方、行在左侧、如何拼成 H8）；
//	  3. 该棋种的完整规则（够用即可，不做竞赛级完备）；
//	  4. 严格的输出格式（可解析是硬要求）；
//	  5. 合法着法示例（降低"编造坐标"的概率）；
//	  6. 上一手若非法，明确的失败原因与重试要求。
//
// 流转（Flow）：
//
//	arena.go → SystemPrompt(kind) 一次（对局开始）
//	         → UserPrompt(pos, hint) 每手一次
//	           → 与 render.go 产出的 PNG 一起发给模型
//
// 扩展（Extend）：
//
//	新增棋种：在 SystemPrompt 的 switch 里补该棋种的 RulesText；
//	若要调整风格（更啰嗦/更精简），改 buildUserPrompt 的组装顺序即可，
//	但【不要删掉六块中的任何一块】——每一块都对应一类实测到的失败。
package game

import (
	"fmt"
	"strings"
)

// 三种棋的规则说明（写给模型看的，因此用词要具体、避免术语歧义）。
const (
	gomokuRulesText = `五子棋规则：
- 棋盘 15×15 个交叉点。黑方先手，双方轮流在【空点】落子。
- 先在横、竖、或任一斜方向上连成【五子或更多】的一方获胜。
- 棋子落下后不再移动，也不能被吃掉。
- 若棋盘下满仍未连成五子，判和。`

	goRulesText = `围棋规则：
- 棋盘 19×19 个交叉点。黑方先手，双方轮流在【空点】落子。
- 一块棋的「气」是它紧邻的空点数（上下左右四个方向）。
- 若某块棋落下后没有气，该块棋被【提掉】（从棋盘上移除）。
- 不允许「自杀」：落子后若自己这块棋没有气、且没有提掉对方任何子，则该着法非法。
- 不允许「劫争回提」：若你上一手刚提掉对方一子，对方不能立即在刚被提掉的位置回提。
- 停一手（PASS）是合法的；双方连续两次停一手即终局和棋。
- 本演示不进行终局数子，胜负通过认输（RESIGN）决定。`

	xiangqiRulesText = `中国象棋规则：
- 棋盘 9 列 × 10 行，棋子放在【交叉点】上。黑方在上半盘，红方在下半盘。
- 【重要】本演示约定黑方先手（传统象棋是红先，这里以本演示为准）。
- 各棋子走法（"格"指相邻交叉点）：
  · 将/帅：每次走一步直线（上下左右），且不能走出九宫（3×3 的宫格）。
  · 士/仕：每次斜走一步，且不能走出九宫。
  · 象/相：斜走两步（走"田"字），不能被"塞象眼"（田字中心有子则不能走），且不能过河。
  · 马：走"日"字（先直一步再斜一步），不能被"蹩马腿"（直行方向紧邻处有子则不能走）。
  · 车：沿直线任意距离行走，路径上不能有棋子。
  · 炮/砲：不吃子时与车相同（路径必须全空）；吃子时必须【恰好隔一个棋子】跳过去。
  · 兵/卒：每次向前一步；过河后还可左右各走一步；永远不能后退。
- 不能吃自己的棋子，也不能走到棋盘外。
- 吃掉对方的将/帅即获胜（本演示以"将被吃"作为终局，不判将死/应将）。
- 认输（RESIGN）即对方获胜。`
)

// OutputInstruction 返回该棋种的输出格式要求。
//
// 输出格式是整个提示词里最硬的一条：解析器只认这几种写法，
// 因此必须把"允许写什么"列全，而不是只说"输出着法"。
func OutputInstruction(kind Kind) string {
	var moveFmt string
	switch kind {
	case KindGomoku:
		moveFmt = "落子坐标，形如 H8（H 为列字母，8 为行号）"
	case KindGo:
		moveFmt = "落子坐标，形如 Q16；若要停一手则写 PASS"
	case KindXiangqi:
		moveFmt = "起点坐标紧跟终点坐标，形如 H1H4（也可以写成 H1-H4）"
	}
	return fmt.Sprintf(`【输出格式（必须严格遵守）】
你的回复必须包含且只包含一行着法，格式为：

MOVE: <着法>

其中 <着法> 是%s。

允许的额外内容：在 MOVE 行之前可以最多写两句简短的中文思路说明（不超过 60 字）。
禁止：不要输出多行候选着法、不要用 JSON 包裹整段回复、不要写围栏代码块、不要复述棋盘。

其他约定：
- 认为自己必败或不想继续时，输出 MOVE: RESIGN
- 你必须给出【合法】着法。若被判为非法，你会收到具体的失败原因，需要重新给出着法。`, moveFmt)
}

// SystemPrompt 构造某棋种的系统提示词。
//
// 这是"给模型设定角色与规则"的部分，一局只发一次（作为 system 消息），
// 之后每手只发用户提示与棋盘图 —— 既省 token，也让规则描述保持一致。
func SystemPrompt(kind Kind) string {
	var sb strings.Builder
	sb.WriteString("你是一位高水平棋手，正在与另一位棋手对弈。\n\n")

	sb.WriteString("【局面如何观察】\n")
	sb.WriteString("你会收到一张棋盘的图片。请严格按图片判断局面，不要凭记忆或想象。\n")
	sb.WriteString("图片上方的字母是列号（从左到右依次为 A B C D E F G H J K ...，")
	sb.WriteString("其中跳过了字母 I，以免与数字 1 混淆）；\n")
	sb.WriteString("图片左侧的数字是行号（从最上方开始为 1，向下递增）。\n")
	sb.WriteString("因此一个交叉点用「列字母 + 行号」表示，例如 H8 表示 H 列第 8 行。\n")
	sb.WriteString("注意：行号是从【最上方】开始数的，与图片上的视觉顺序一致。\n")
	if kind == KindXiangqi {
		sb.WriteString("象棋棋子写的是汉字：红方为 帅 仕 相 马 车 炮 兵，黑方为 将 士 象 马 车 砲 卒；\n")
		sb.WriteString("深色字是黑方，红色字是红方。棋盘上的红框标记了对方刚刚走的那一手。\n")
	} else {
		sb.WriteString("黑子是深色圆点，白子是白色圆点；棋盘上的红框标记了对方刚刚下出的那一手。\n")
	}

	sb.WriteString("\n")
	switch kind {
	case KindGomoku:
		sb.WriteString(gomokuRulesText)
	case KindGo:
		sb.WriteString(goRulesText)
	case KindXiangqi:
		sb.WriteString(xiangqiRulesText)
	}
	sb.WriteString("\n\n")
	sb.WriteString(OutputInstruction(kind))
	sb.WriteString("\n\n【对局要求】\n")
	sb.WriteString("请认真对待每一步：先看清图片上的局面，再选择一手对你自己最有利的着法。\n")
	sb.WriteString("不要随机落子，也不要在无关位置消耗棋子。\n")
	return sb.String()
}

// MoveHint 是给模型的一手提示信息（可带上一手的失败原因）。
type MoveHint struct {
	// MoveNumber 是当前是第几手（从 1 开始）。
	MoveNumber int
	// SelfColor / OpponentColor 用于说明该谁走。
	SelfColor Color
	// RecentMoves 是最近几手的着法描述（帮助模型对齐上下文）。
	RecentMoves []string
	// LastError 非空时表示上一手非法，这里是具体原因。
	LastError string
	// LastRaw 是上一手的原始输出（让模型看到自己写错了什么）。
	LastRaw string
	// LegalExamples 是几条真实合法的着法示例。
	LegalExamples []string
	// RetryCount 是本次用户的第几次重试（0 表示首次）。
	RetryCount int
}

// UserPrompt 构造某一手的用户提示词。
//
// 与系统提示分工：系统提示讲"规则与格式"（不变的），
// 用户提示讲"现在轮到你、第几手、上一手出了什么问题"（每手变的）。
func UserPrompt(kind Kind, h MoveHint) string {
	var sb strings.Builder

	who := "黑方"
	if h.SelfColor == ColorWhite {
		who = "白方"
	}
	if kind == KindXiangqi && h.SelfColor == ColorWhite {
		who = "红方"
	}
	sb.WriteString(fmt.Sprintf("现在是第 %d 手，轮到你（%s）行棋。\n", h.MoveNumber, who))

	if len(h.RecentMoves) > 0 {
		// 只带最近几手：更早的局面图片已经体现，重复列出既费 token
		// 又可能与我方对局状态不一致（模型会以文字为准而忽略图片）。
		sb.WriteString("最近几手：" + strings.Join(h.RecentMoves, "，") + "\n")
	}

	// 上一手非法：必须给出【具体原因】而不是笼统的"请重试"。
	// 原因里带着坐标与规则名，模型才能针对性修正
	// （是"位置被占"就换点，是"马腿被堵"就换子）。
	if strings.TrimSpace(h.LastError) != "" {
		sb.WriteString("\n【上一手不合法】\n")
		sb.WriteString("原因：" + h.LastError + "\n")
		if strings.TrimSpace(h.LastRaw) != "" {
			sb.WriteString("你上次的输出是：" + truncateForPrompt(h.LastRaw, 200) + "\n")
		}
		if h.RetryCount > 0 {
			sb.WriteString(fmt.Sprintf("这已经是第 %d 次尝试，请务必给出一个新的、合法的着法。\n", h.RetryCount+1))
		}
		sb.WriteString("请改正后重新输出。\n")
	}

	if len(h.LegalExamples) > 0 {
		sb.WriteString("\n参考：当前局面下若干【确实合法】的着法示例（仅供参考，你可以选择其他合法着法）：\n")
		sb.WriteString("  " + strings.Join(h.LegalExamples, "、") + "\n")
	}

	sb.WriteString("\n")
	switch kind {
	case KindGomoku, KindGo:
		sb.WriteString("请在图片上找一个【空的交叉点】落子，然后按格式输出。\n")
	case KindXiangqi:
		sb.WriteString("请从图片上选择一个【你自己】的棋子，把它走到一个合法位置，然后按格式输出。\n")
	}
	return sb.String()
}

// truncateForPrompt 截断过长文本（保留头部）。
//
// 保留头部而不是尾部：模型通常第一句就说出了它想走的着法，
// 后面才是解释；而"格式错乱"往往就发生在头部，因此头部信息量更大。
func truncateForPrompt(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…（已截断）"
}
