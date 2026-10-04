// 本文件驱动"某一方走一步"：渲染 → 提问 → 解析 → 校验 → 必要时重试。
//
// 意图（Why）：
//
//	这是把"模型能力"接进"棋类规则"的地方，也是整个功能里最需要谨慎的一段：
//	模型会给出无法解析的输出、非法的着法、或者干脆调用失败。
//	三种情况的处置【完全不同】，混在一起处理就会出现"把网络抖动当成模型下错棋"
//	这种误判，进而错误地判负——而用户看到的是"模型认输了"，与事实不符。
//
//	因此本文件的中心是把失败分成三类，分别处置：
//	  1. 调用失败（网络/额度/无可用渠道）：不是模型的错，直接上抛，由上层中止对局；
//	  2. 输出无法解析：格式问题，带着"你上次输出的是…"重试；
//	  3. 着法非法：规则问题，带着【具体规则原因】重试（如"马腿被堵"）。
//
//	重试用尽后才判定"该方无法给出合法着法"，并把原始输出留档——
//	这本身就是一个有价值的观测结果（"模型看这张图时彻底走不动了"），
//	不是需要隐藏的错误。
//
// 流转（Flow）：
//
//	server/handler_game → Arena.PlayTurn
//	  ├─ RenderPNG（局面图）
//	  ├─ SystemPrompt / UserPrompt（提示词）
//	  ├─ ModelCaller.CallVision（真正调用模型，实现见 server 层）
//	  ├─ ParseMove（解析）
//	  └─ rules.Validate / Apply（规则）
//	     → 返回本次结果或"该方无法给出合法着法"
//
// 扩展（Extend）：
//
//	想换更强的重试策略（如"非法时把某几手合法着法直接喂给它"）：
//	在 MoveHint 里加字段并在 playOneAttempt 组装时填入。
//	想支持"人类落子"：人类走的那一步不走本文件，直接由上层
//	用 ParseMove + Validate 处理（人类的输入同样可以容错解析）。
package game

import (
	"context"
	"fmt"
	"strings"
)

// 默认重试次数。
//
// 取 3：实测模型第一次给非法着法多半是"看错了一格"或"格式不对"，
// 带着具体原因重问一次通常就能改对；两次仍不对说明它确实读不懂这张图，
// 再问下去只是浪费额度与时间。
const defaultMaxAttempts = 3

// ModelCaller 是"让模型看图回答"所需的最小能力。
//
// 在消费方（game）定义接口、由 server 层实现，是为了让本包不依赖
// relay / HTTP 那一层——规则与提示词的正确性可以完全离线单测。
type ModelCaller interface {
	// CallVision 发起一次带图片的多模态对话，返回模型回复的纯文本。
	//
	// 返回 error 表示【调用层面】失败（网络、额度、无可用渠道等），
	// 这类失败与"模型下错棋"性质不同，不应触发着法重试。
	CallVision(ctx context.Context, modelName, systemPrompt, userPrompt string, boardPNG []byte) (string, error)
}

// TurnOutcome 一方的走子结果。
type TurnOutcome struct {
	// Move 是最终确定的着法；CallFailed 或 AttemptsExhausted 时为 nil。
	Move *Move
	// Raw 是模型最后一次的原始输出（排查用，已截断）。
	Raw string
	// Attempts 是实际尝试次数（含成功那次）。
	Attempts int
	// CallFailed 为 true 表示模型【调用】失败（与棋力无关）。
	CallFailed bool
	// Error 是失败原因（CallFailed 或 AttemptsExhausted 时非空）。
	Error string
	// AttemptErrors 记录每一次尝试的失败原因（供前端展示"它错在哪"）。
	AttemptErrors []string
}

// AttemptsExhausted 判断是否"重试用尽仍拿不到合法着法"。
func (o TurnOutcome) AttemptsExhausted() bool { return o.Move == nil && !o.CallFailed }

// Arena 驱动对局中的走子。
type Arena struct {
	caller      ModelCaller
	maxAttempts int
}

// NewArena 创建对局驱动器。maxAttempts <= 0 时使用默认值。
func NewArena(caller ModelCaller, maxAttempts int) *Arena {
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxAttempts
	}
	return &Arena{caller: caller, maxAttempts: maxAttempts}
}

// PlayTurn 让 modelName 在当前局面走一步。
//
// 返回的 *Position 是走完后的新局面（成功时非 nil）；Status 是走完后的对局状态。
// 注意：本方法【不修改】传入的 pos（Position 全程按值传递语义使用）。
func (a *Arena) PlayTurn(ctx context.Context, pos *Position, modelName string, recentMoves []string) (TurnOutcome, *Position, Status, error) {
	if pos == nil {
		return TurnOutcome{}, nil, StatusPlaying, fmt.Errorf("game: 局面为空")
	}
	rules, err := Lookup(pos.Kind)
	if err != nil {
		return TurnOutcome{}, nil, StatusPlaying, err
	}
	if a.caller == nil {
		return TurnOutcome{}, nil, StatusPlaying, fmt.Errorf("game: 未配置模型调用器")
	}
	if pos.ToMove == ColorNone {
		return TurnOutcome{}, nil, StatusPlaying, fmt.Errorf("game: 局面缺少轮次信息")
	}

	// 系统提示每手都重建：开销可忽略（纯字符串拼接），
	// 换来的是"提示词随代码演进不会出现新旧不一致"。
	system := SystemPrompt(pos.Kind)

	// 局面图每手都要重渲染——这正是"多模态"的含义：
	// 模型看到的是当前局面的【图片】，而不是我们喂给它的棋谱文本。
	png, err := RenderPNG(pos, renderHeader(pos))
	if err != nil {
		return TurnOutcome{}, nil, StatusPlaying, err
	}

	out := TurnOutcome{}
	hint := MoveHint{
		MoveNumber:  pos.LastMoveSeq() + 1,
		SelfColor:   pos.ToMove,
		RecentMoves: recentMoves,
	}

	for attempt := 0; attempt < a.maxAttempts; attempt++ {
		hint.RetryCount = attempt
		hint.LegalExamples = legalExampleStrings(pos, 6)

		user := UserPrompt(pos.Kind, hint)
		raw, callErr := a.caller.CallVision(ctx, modelName, system, user, png)
		if callErr != nil {
			// 调用失败：不是模型的棋力问题，不重试、不判负。
			out.CallFailed = true
			out.Attempts = attempt + 1
			out.Error = callErr.Error()
			return out, nil, StatusPlaying, nil
		}
		out.Attempts = attempt + 1
		out.Raw = truncateForPrompt(raw, 300)

		mv, res, parseErr := a.interpret(pos, rules, raw)
		if parseErr != nil {
			hint.LastError = parseErr.Error()
			hint.LastRaw = out.Raw
			out.AttemptErrors = append(out.AttemptErrors,
				fmt.Sprintf("第 %d 次：%s", attempt+1, parseErr.Error()))
			continue
		}

		// 认输：直接采纳，对局由规则判定终局。
		if res.IsResign {
			mv.Color = pos.ToMove
			mv.Resign = true
			mv.Note = out.Raw
			next := rules.Apply(pos, mv)
			out.Move = &mv
			return out, next, rules.Result(next), nil
		}

		// 落子 / 走子：校验通过才应用。
		if err := rules.Validate(pos, mv); err != nil {
			hint.LastError = err.Error()
			hint.LastRaw = out.Raw
			out.AttemptErrors = append(out.AttemptErrors,
				fmt.Sprintf("第 %d 次：着法 %s 不合法——%s", attempt+1, mv.Notation(), err.Error()))
			continue
		}
		mv.Note = out.Raw
		next := rules.Apply(pos, mv)
		out.Move = &mv
		return out, next, rules.Result(next), nil
	}

	out.Error = fmt.Sprintf("连续 %d 次未能给出合法着法", a.maxAttempts)
	return out, nil, StatusPlaying, nil
}

// interpret 把模型输出解释成着法（含 Pass 的处理）。
//
// 与 ParseMove 的分工：ParseMove 只管"文本 → 坐标"，
// 本函数负责"坐标 → 这一手在该棋种下讲不讲得通"（如五子棋不许停一手）。
func (a *Arena) interpret(pos *Position, rules RuleSet, raw string) (Move, ParseResult, error) {
	res, err := ParseMove(pos.Kind, raw)
	if err != nil {
		return Move{}, res, err
	}
	mv := res.Move
	mv.Color = pos.ToMove
	mv.Seq = pos.LastMoveSeq() + 1

	if res.IsPass {
		// 停一手只在围棋合法。五子棋/象棋没有这个规则，
		// 若不拦，模型可以靠"停一手"无限拖延对局。
		if pos.Kind != KindGo {
			return Move{}, res, fmt.Errorf("该棋种不允许停一手（PASS 仅围棋可用）")
		}
		mv.Pass = true
		return mv, res, nil
	}
	return mv, res, nil
}

// renderHeader 构造图片顶部信息条的文字。
//
// 信息条是图片的一部分，因此必须只放【模型能读懂且必要】的内容：
// 棋种、该谁走、第几手。刻意不放对局 ID、模型名之类的内部信息——
// 那会干扰模型对"我在下什么棋"的判断。
func renderHeader(pos *Position) string {
	kindName := map[Kind]string{
		KindGomoku:  "GOMOKU",
		KindGo:      "GO",
		KindXiangqi: "XIANGQI",
	}[pos.Kind]
	if kindName == "" {
		kindName = strings.ToUpper(string(pos.Kind))
	}
	mover := "BLACK"
	if pos.ToMove == ColorWhite {
		mover = "WHITE"
	}
	header := fmt.Sprintf("%s  TO MOVE: %s  (MOVE %d)", kindName, mover, pos.LastMoveSeq()+1)
	// 围棋额外标注提子数与连续停一手次数：这两项在棋盘图片上【看不出来】，
	// 而它们直接影响模型对局势与终局的判断。
	if pos.Kind == KindGo {
		header += fmt.Sprintf("  CAPTURED B%d/W%d", pos.CapturedBlack, pos.CapturedWhite)
		if pos.ConsecutivePasses > 0 {
			header += fmt.Sprintf("  PASSES %d", pos.ConsecutivePasses)
		}
	}
	return header
}

// legalExampleStrings 生成给模型看的合法着法示例文本。
func legalExampleStrings(pos *Position, limit int) []string {
	moves := SampleLegalMoves(pos, limit)
	if len(moves) == 0 {
		return nil
	}
	out := make([]string, 0, len(moves))
	for _, mv := range moves {
		out = append(out, moveNotationForModel(pos.Kind, mv))
	}
	return out
}

// moveNotationForModel 把着法写成提示词里推荐的格式（与输出格式一致）。
func moveNotationForModel(kind Kind, mv Move) string {
	if kind == KindXiangqi {
		return CoordName(mv.From) + CoordName(mv.To)
	}
	return CoordName(mv.Point)
}
