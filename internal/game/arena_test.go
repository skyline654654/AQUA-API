// 本文件是对局驱动的单元测试，重点验证"三类失败被正确区分"。
//
// 意图（Why）：
//
//	把失败混为一谈是本功能最危险的缺陷类型。想象三种情形：
//	  · 上游 5xx 导致调用失败 → 若当成"模型下错棋"，会判它负；
//	  · 模型输出格式乱 → 若直接判负，等于把可修复的格式问题当棋力问题；
//	  · 模型给了非法着法 → 若静默接受，棋谱就错了。
//	三者对用户的意义完全不同，因此必须各测一遍。
package game

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// scriptedCaller 按预设脚本依次返回回复，用于精确驱动重试路径。
type scriptedCaller struct {
	replies []string
	errs    []error
	calls   int
	// lastSystem / lastUser 记录最后一次收到的提示词（用于断言提示词内容）
	lastSystem string
	lastUser   string
	gotPNG     bool
}

func (c *scriptedCaller) CallVision(_ context.Context, _ string, system, user string, png []byte) (string, error) {
	c.lastSystem, c.lastUser = system, user
	c.gotPNG = len(png) > 0
	idx := c.calls
	c.calls++
	if idx < len(c.errs) && c.errs[idx] != nil {
		return "", c.errs[idx]
	}
	if idx < len(c.replies) {
		return c.replies[idx], nil
	}
	return "", errors.New("scriptedCaller: 脚本已用尽")
}

func TestPlayTurn_HappyPath(t *testing.T) {
	pos := mustPosition(t, KindGomoku)
	caller := &scriptedCaller{replies: []string{"MOVE: H8"}}
	arena := NewArena(caller, 3)

	out, next, status, err := arena.PlayTurn(context.Background(), pos, "test-model", nil)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if out.Move == nil {
		t.Fatal("应成功走出一手")
	}
	if out.Move.Point != (Point{7, 7}) {
		t.Errorf("落子位置 %v，期望 H8=(7,7)", out.Move.Point)
	}
	if out.Attempts != 1 {
		t.Errorf("尝试次数 %d，期望 1", out.Attempts)
	}
	if next == nil || next.At(Point{7, 7}) != ColorBlack {
		t.Error("新局面应已落下黑子")
	}
	if next.ToMove != ColorWhite {
		t.Error("手番应切换到白方")
	}
	if status != StatusPlaying {
		t.Errorf("状态应为进行中，实际 %s", status)
	}
	// 原局面不能被修改（Position 按值语义使用）
	if pos.At(Point{7, 7}) != ColorNone {
		t.Error("原局面被修改了——PlayTurn 不应改动入参")
	}
	if !caller.gotPNG {
		t.Error("调用模型时必须附带棋盘图片")
	}
}

func TestPlayTurn_RetriesOnIllegalMoveWithSpecificReason(t *testing.T) {
	// 模型第一次给了一个被占的位置，第二次给对。
	// 关键断言：第二次的提示词里必须带上【具体原因】，
	// 否则模型不知道错在哪，只能盲猜。
	pos := mustPosition(t, KindGomoku)
	pos.SetColor(Point{7, 7}, ColorBlack) // H8 被占
	pos.ToMove = ColorWhite
	pos.LastMove = &Move{Color: ColorBlack, Seq: 1, Point: Point{7, 7}}

	caller := &scriptedCaller{replies: []string{"MOVE: H8", "MOVE: H9"}}
	arena := NewArena(caller, 3)

	out, next, _, err := arena.PlayTurn(context.Background(), pos, "m", nil)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if out.Move == nil || out.Move.Point != (Point{7, 8}) {
		t.Fatalf("应最终落在 H9，实际 %+v", out.Move)
	}
	if out.Attempts != 2 {
		t.Errorf("尝试次数 %d，期望 2", out.Attempts)
	}
	if len(out.AttemptErrors) != 1 {
		t.Errorf("应记录 1 条失败原因，实际 %d", len(out.AttemptErrors))
	}
	// 提示词里必须出现"已有棋子"这类具体原因，而不是泛泛的"请重试"
	if !strings.Contains(caller.lastUser, "已有棋子") {
		t.Errorf("重试提示词缺少具体原因，实际为：\n%s", caller.lastUser)
	}
	// 也应回显模型上次的错误输出，让它看到自己写了什么
	if !strings.Contains(caller.lastUser, "H8") {
		t.Error("重试提示词应回显上次输出")
	}
	if next == nil {
		t.Fatal("应返回新局面")
	}
}

func TestPlayTurn_RetriesOnUnparsableOutput(t *testing.T) {
	// 第一次输出完全无法解析，第二次规范输出。
	pos := mustPosition(t, KindGomoku)
	caller := &scriptedCaller{replies: []string{
		"我觉得这局很难，让我再想想。", // 无坐标
		"MOVE: D4",
	}}
	arena := NewArena(caller, 3)

	out, _, _, err := arena.PlayTurn(context.Background(), pos, "m", nil)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if out.Move == nil || out.Move.Point != (Point{3, 3}) {
		t.Fatalf("应最终落在 D4，实际 %+v", out.Move)
	}
	if !strings.Contains(caller.lastUser, "无法从回复中解析") {
		t.Errorf("重试提示词应说明是解析问题，实际：\n%s", caller.lastUser)
	}
}

func TestPlayTurn_CallFailureIsNotTreatedAsModelError(t *testing.T) {
	// 调用失败（如上游 5xx）必须原样上报，而不是判模型下错棋。
	// 若误判，用户会看到"模型认输"，与事实完全不符。
	pos := mustPosition(t, KindGomoku)
	caller := &scriptedCaller{errs: []error{errors.New("上游 503")}}
	arena := NewArena(caller, 3)

	out, next, _, err := arena.PlayTurn(context.Background(), pos, "m", nil)
	if err != nil {
		t.Fatalf("调用失败应以 outcome 形式返回而不是 error: %v", err)
	}
	if !out.CallFailed {
		t.Error("应标记为调用失败")
	}
	if out.AttemptsExhausted() {
		t.Error("调用失败不应被算作'重试用尽'")
	}
	if next != nil {
		t.Error("调用失败不应产生新局面")
	}
	if out.Attempts != 1 {
		t.Errorf("调用失败不应重试，实际尝试 %d 次", out.Attempts)
	}
	if !strings.Contains(out.Error, "503") {
		t.Errorf("应保留原始错误信息，实际 %q", out.Error)
	}
}

func TestPlayTurn_ExhaustedAfterMaxAttempts(t *testing.T) {
	// 连续给非法着法，用尽重试后应标记用完，且不产生新局面。
	pos := mustPosition(t, KindGomoku)
	pos.SetColor(Point{7, 7}, ColorBlack)
	pos.LastMove = &Move{Color: ColorBlack, Seq: 1, Point: Point{7, 7}}
	pos.ToMove = ColorWhite

	caller := &scriptedCaller{replies: []string{"MOVE: H8", "MOVE: H8", "MOVE: H8"}}
	arena := NewArena(caller, 3)

	out, next, _, err := arena.PlayTurn(context.Background(), pos, "m", nil)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if !out.AttemptsExhausted() {
		t.Error("三次非法后应标记重试用尽")
	}
	if next != nil {
		t.Error("用尽后不应产生新局面")
	}
	if out.Attempts != 3 {
		t.Errorf("尝试次数 %d，期望 3", out.Attempts)
	}
	if len(out.AttemptErrors) != 3 {
		t.Errorf("应记录 3 条失败原因，实际 %d", len(out.AttemptErrors))
	}
}

func TestPlayTurn_ResignEndsGame(t *testing.T) {
	pos := mustPosition(t, KindGomoku)
	caller := &scriptedCaller{replies: []string{"我认输了。MOVE: RESIGN"}}
	arena := NewArena(caller, 3)

	out, next, status, err := arena.PlayTurn(context.Background(), pos, "m", nil)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if out.Move == nil || !out.Move.Resign {
		t.Fatalf("应记录认输，实际 %+v", out.Move)
	}
	// 黑方认输 → 白胜
	if status != StatusWhiteWin {
		t.Errorf("黑认输应判白胜，实际 %s", status)
	}
	if next == nil {
		t.Fatal("应返回新局面（终局局面）")
	}
}

func TestPlayTurn_GoPassAllowedAndEndsOnDoublePass(t *testing.T) {
	pos := mustPosition(t, KindGo)
	// 黑停一手
	caller := &scriptedCaller{replies: []string{"MOVE: PASS"}}
	arena := NewArena(caller, 3)

	out, next, status, err := arena.PlayTurn(context.Background(), pos, "m", nil)
	if err != nil || out.Move == nil || !out.Move.Pass {
		t.Fatalf("围棋应允许停一手，实际 %+v / %v", out, err)
	}
	if status != StatusPlaying {
		t.Errorf("一次停一手不应终局，实际 %s", status)
	}
	// 白再停一手 → 和棋
	caller2 := &scriptedCaller{replies: []string{"MOVE: PASS"}}
	arena2 := NewArena(caller2, 3)
	_, _, status2, err := arena2.PlayTurn(context.Background(), next, "m", nil)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if status2 != StatusDraw {
		t.Errorf("双方连续停一手应判和，实际 %s", status2)
	}
}

func TestPlayTurn_GomokuPassIsRejected(t *testing.T) {
	// 五子棋没有停一手规则。若不拦，模型可以靠不断 PASS 让对局永不结束。
	pos := mustPosition(t, KindGomoku)
	caller := &scriptedCaller{replies: []string{"MOVE: PASS", "MOVE: PASS", "MOVE: PASS"}}
	arena := NewArena(caller, 3)

	out, _, _, err := arena.PlayTurn(context.Background(), pos, "m", nil)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if out.Move != nil {
		t.Error("五子棋不应接受停一手")
	}
	if !out.AttemptsExhausted() {
		t.Error("应判为重试用尽")
	}
	if len(out.AttemptErrors) == 0 || !strings.Contains(out.AttemptErrors[0], "不允许停一手") {
		t.Errorf("失败原因应说明不许停一手，实际 %v", out.AttemptErrors)
	}
}

func TestPlayTurn_XiangqiMoveIsAppliedCorrectly(t *testing.T) {
	pos := mustPosition(t, KindXiangqi)
	caller := &scriptedCaller{replies: []string{"MOVE: A1A2"}}
	arena := NewArena(caller, 3)

	out, next, _, err := arena.PlayTurn(context.Background(), pos, "m", nil)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if out.Move == nil {
		t.Fatal("应成功走出着法")
	}
	// A1 的车走到 A2（A1 是 (0,0)，A2 是 (0,1)）
	if next.PlacedAt(Point{0, 1}) != PieceChariot || next.At(Point{0, 1}) != ColorBlack {
		t.Error("车应移动到 A2")
	}
	if next.At(Point{0, 0}) != ColorNone {
		t.Error("A1 应被清空")
	}
}

func TestPlayTurn_XiangqiRejectsOpponentPiece(t *testing.T) {
	// 模型试图移动对方的棋子 → 必须被拒并带着"不是你的棋子"重试。
	pos := mustPosition(t, KindXiangqi)
	caller := &scriptedCaller{replies: []string{"MOVE: A10A9", "MOVE: A1A2"}}
	arena := NewArena(caller, 3)

	out, _, _, err := arena.PlayTurn(context.Background(), pos, "m", nil)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if out.Attempts != 2 {
		t.Fatalf("尝试次数 %d，期望 2", out.Attempts)
	}
	if !strings.Contains(caller.lastUser, "不是自己的棋子") {
		t.Errorf("重试提示应说明棋子归属错误，实际：\n%s", caller.lastUser)
	}
}

func TestPlayTurn_PromptContainsEssentials(t *testing.T) {
	// 提示词必须让模型知道：怎么读坐标、必须输出 MOVE 行、以及示例着法。
	// 这三样缺任何一样，非法着法率都会显著上升。
	pos := mustPosition(t, KindXiangqi)
	caller := &scriptedCaller{replies: []string{"MOVE: A1A2"}}
	arena := NewArena(caller, 3)
	if _, _, _, err := arena.PlayTurn(context.Background(), pos, "m", nil); err != nil {
		t.Fatal(err)
	}

	sys := caller.lastSystem
	for _, want := range []string{"MOVE:", "列字母", "行号", "九宫", "马", "炮", "将"} {
		if !strings.Contains(sys, want) {
			t.Errorf("系统提示词缺少 %q", want)
		}
	}
	user := caller.lastUser
	if !strings.Contains(user, "第 1 手") {
		t.Errorf("用户提示应说明手数，实际：\n%s", user)
	}
	if !strings.Contains(user, "合法") {
		t.Error("用户提示应包含合法着法示例")
	}
}

func TestPlayTurn_ImageIsRenderedPerTurn(t *testing.T) {
	// 每手都必须重新渲染局面图——这是"多模态"的核心：
	// 模型看到的是当前局面的图片，而不是累积的棋谱文本。
	pos := mustPosition(t, KindGomoku)
	pos.SetColor(Point{3, 3}, ColorBlack)
	pos.LastMove = &Move{Color: ColorBlack, Seq: 1, Point: Point{3, 3}}
	pos.ToMove = ColorWhite

	var captured []byte
	caller := &pngCapturingCaller{reply: "MOVE: D5", sink: &captured}
	arena := NewArena(caller, 1)
	if _, _, _, err := arena.PlayTurn(context.Background(), pos, "m", nil); err != nil {
		t.Fatal(err)
	}
	// PNG 魔数校验：确认真的是一张 PNG，而不是空字节或其它格式
	if len(captured) < 8 || string(captured[1:4]) != "PNG" {
		t.Errorf("附带的图片不是合法 PNG（前 8 字节：%v）", captured[:min(8, len(captured))])
	}
}

// pngCapturingCaller 记录收到的图片字节，用于断言图片确实被渲染并附带。
type pngCapturingCaller struct {
	reply string
	sink  *[]byte
}

func (c *pngCapturingCaller) CallVision(_ context.Context, _, _, _ string, png []byte) (string, error) {
	*c.sink = append([]byte(nil), png...)
	return c.reply, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
