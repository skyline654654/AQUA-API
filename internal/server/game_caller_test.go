// 本文件测试对弈的服务端装配：多模态请求体的形状、响应解析、额度闸门。
//
// 意图（Why）：
//
//	请求体形状是最容易写错的一处——OpenAI 的多模态消息是嵌套数组，
//	缩进一层、字段名写错、或把 base64 编码方式写错，都会让上游直接 400。
//	而这类错误在本地"看起来没问题"（代码能编译、服务能起），
//	只有真正调用时才暴露，代价是一次完整的排查。
//	因此这里对组装结果做结构化断言。
package server

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/xiaosu4610/aqua-api/internal/game"
)

func TestBuildVisionRequest_ShapeIsOpenAICompatible(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	raw, err := buildVisionRequest(visionCall{
		Model:    "gpt-4o",
		System:   "你是棋手",
		User:     "轮到你走",
		BoardPNG: png,
	})
	if err != nil {
		t.Fatalf("组装请求失败: %v", err)
	}

	var req struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("组装出的不是合法 JSON: %v", err)
	}
	if req.Model != "gpt-4o" {
		t.Errorf("model = %q", req.Model)
	}
	if req.Stream {
		t.Error("对弈不需要流式，stream 应为 false")
	}
	if len(req.Messages) != 2 {
		t.Fatalf("应有 system + user 两条消息，实际 %d", len(req.Messages))
	}
	if req.Messages[0].Role != "system" {
		t.Errorf("第一条应为 system，实际 %q", req.Messages[0].Role)
	}

	// system 的 content 是普通字符串
	var sysText string
	if err := json.Unmarshal(req.Messages[0].Content, &sysText); err != nil {
		t.Fatalf("system content 应为字符串: %v", err)
	}
	if sysText != "你是棋手" {
		t.Errorf("system 内容 = %q", sysText)
	}

	// user 的 content 必须是【内容数组】：text 在前、image_url 在后
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(req.Messages[1].Content, &parts); err != nil {
		t.Fatalf("user content 应为内容数组: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("user 应有两段内容，实际 %d", len(parts))
	}
	if parts[0].Type != "text" || parts[0].Text != "轮到你走" {
		t.Errorf("第一段应为文本，实际 %+v", parts[0])
	}
	if parts[1].Type != "image_url" {
		t.Errorf("第二段应为图片，实际 %q", parts[1].Type)
	}
	// data URI 前缀与 base64 内容都要对：前缀错了上游会当成 URL 去抓取
	wantURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	if parts[1].ImageURL.URL != wantURL {
		t.Errorf("图片 data URI 不正确\n得到 %q\n期望 %q", parts[1].ImageURL.URL, wantURL)
	}
}

func TestBuildVisionRequest_RejectsIncompleteInput(t *testing.T) {
	// 缺模型或缺图片时必须报错，而不是发出一个注定失败的请求
	// （那会在上游产生一次无意义的调用与一条错误日志）。
	if _, err := buildVisionRequest(visionCall{User: "x", BoardPNG: []byte{1}}); err == nil {
		t.Error("缺少模型应报错")
	}
	if _, err := buildVisionRequest(visionCall{Model: "m", User: "x"}); err == nil {
		t.Error("缺少图片应报错")
	}
}

func TestExtractAssistantText_StringContent(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"MOVE: H8"}}]}`)
	got, err := extractAssistantText(body)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got != "MOVE: H8" {
		t.Errorf("得到 %q", got)
	}
}

func TestExtractAssistantText_ArrayContent(t *testing.T) {
	// 部分模型/网关把 content 返回成内容数组。只处理字符串形态的话，
	// 这些模型会全部表现为"没有回复"，而实际上它答了。
	body := []byte(`{"choices":[{"message":{"content":[{"type":"text","text":"MOVE: "},{"type":"text","text":"H8"}]}}]}`)
	got, err := extractAssistantText(body)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got != "MOVE: H8" {
		t.Errorf("得到 %q", got)
	}
}

func TestExtractAssistantText_ErrorInsideSuccessfulResponse(t *testing.T) {
	// 有些实现即使 HTTP 200 也在响应体里返回 error 对象。
	// 不识别它就会把 error 当成空回复，用户看到的是"模型没有回复"。
	body := []byte(`{"error":{"message":"model overloaded"}}`)
	_, err := extractAssistantText(body)
	if err == nil {
		t.Fatal("应识别出体内的 error 对象")
	}
	if !strings.Contains(err.Error(), "overloaded") {
		t.Errorf("错误信息应包含原始原因，实际 %v", err)
	}
}

func TestExtractAssistantText_ReportsUnparsableBody(t *testing.T) {
	// 响应不是 JSON 时，错误信息里要带上原文片段——
	// 否则排查时只剩"解析失败"，没有任何线索。
	_, err := extractAssistantText([]byte("<html>502 Bad Gateway</html>"))
	if err == nil {
		t.Fatal("非 JSON 响应应报错")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("错误信息应保留原文片段，实际 %v", err)
	}
}

func TestExtractErrorMessage_HandlesBothShapes(t *testing.T) {
	// 本站统一错误结构
	got := extractErrorMessage([]byte(`{"error":{"message":"额度不足"}}`))
	if got != "额度不足" {
		t.Errorf("标准结构解析失败：%q", got)
	}
	// 纯文本错误体
	got = extractErrorMessage([]byte("upstream timeout"))
	if !strings.Contains(got, "upstream timeout") {
		t.Errorf("纯文本错误体解析失败：%q", got)
	}
}

func TestDecodeContent_EmptyAndInvalid(t *testing.T) {
	if got := decodeContent(nil); got != "" {
		t.Errorf("空内容应返回空串，实际 %q", got)
	}
	if got := decodeContent(json.RawMessage(`123`)); got != "" {
		t.Errorf("非法形态应返回空串，实际 %q", got)
	}
}

func TestWinnerOf_DistinguishesAbortedFromDraw(t *testing.T) {
	// aborted 与 draw 必须给出不同的 winner 值：
	//   draw → "draw"（真的和棋）
	//   aborted → ""（没下完，没有胜者）
	// 若 aborted 也填 "draw"，统计口径会把"模型走不动了"算成"和棋"，
	// 这是与事实相反的结论。
	if got := winnerOf(game.StatusDraw); got != "draw" {
		t.Errorf("和棋应返回 draw，实际 %q", got)
	}
	if got := winnerOf(game.StatusPlaying); got != "" {
		t.Errorf("未分胜负应返回空串，实际 %q", got)
	}
}

func TestKindLabelAndModeLabel(t *testing.T) {
	cases := map[string]string{
		"gomoku": "五子棋", "go": "围棋", "xiangqi": "象棋", "unknown": "unknown",
	}
	for in, want := range cases {
		if got := kindLabel(in); got != want {
			t.Errorf("kindLabel(%q) = %q，期望 %q", in, got, want)
		}
	}
	if got := modeLabel("ai_vs_ai"); got != "AI 对 AI" {
		t.Errorf("modeLabel = %q", got)
	}
	if got := modeLabel("human_vs_ai"); got != "人机对弈" {
		t.Errorf("modeLabel = %q", got)
	}
}
