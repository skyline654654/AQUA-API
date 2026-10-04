// 本文件实现对弈所需的多模态模型调用（game.ModelCaller 的服务端实现）。
//
// 意图（Why）：
//
//	对弈要"代替用户去问模型"，而这个调用必须走网关自己的转发链路，
//	不能另起一套。理由是转发链路上挂着大量必须复用的横切能力：
//	渠道选择与故障转移、密钥池调度、BYOK 自备密钥、模型映射、
//	额度扣费、调用日志、语料采样、错误脱敏。
//	另起一套 HTTP 客户端意味着这些能力全部要重做一遍，而且必然与主链路漂移。
//
//	因此这里构造一个"标准的多模态 chat/completions 请求"，
//	直接交给 relay 的同一个处理器（ServeChatCompletions）执行，
//	只是把响应写进内存而不是网络连接。
//
// 与真实客户端请求的差异（刻意的）：
//
//	不经过 TokenAuth 中间件，因此不会创建额度【预留】。
//	这意味着结算走的是"调用后扣费"路径（见 relay.settleQuota 的说明），
//	行为正确但少了一道"事前额度墙"。因此 handler_game 在每次走子前
//	显式做额度检查（ensureGameQuota），把这道闸门补回来——
//	否则零额度用户可以靠"开一局棋"绕过额度限制白刷模型。
//
// 流转（Flow）：
//
//	game.Arena.PlayTurn → relayModelCaller.CallVision
//	  ├─ 组装 messages（system + user[text + image_url data URI]）
//	  ├─ reqctx 注入身份（用户 / 令牌 / 分组），供计费与日志使用
//	  ├─ relay.ServeChatCompletions（同一处理器，写进内存 ResponseRecorder）
//	  └─ 解析 OpenAI 响应取出正文
//
// 扩展（Extend）：
//
//	若要支持流式（让前端看到模型"边想边下"）：把 stream 置 true 并解析 SSE。
//	当前刻意不流式：一手棋只需要最后那个坐标，流式会显著增加解析与
//	错误处理的复杂度，而对弈的观感提升有限（思考过程本身不是重点）。
package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/xiaosu4610/aqua-api/internal/relay"
	"github.com/xiaosu4610/aqua-api/internal/reqctx"
)

// relayModelCaller 通过网关自身的转发链路调用模型。
type relayModelCaller struct {
	relay *relay.Relay
}

// NewRelayModelCaller 创建基于转发引擎的模型调用器。
func NewRelayModelCaller(r *relay.Relay) *relayModelCaller {
	return &relayModelCaller{relay: r}
}

// visionCall 是一次多模态调用的输入。
type visionCall struct {
	// Model 是对外模型名（会被网关按映射解析到上游名）。
	Model string
	// System 是系统提示词。
	System string
	// User 是用户提示词（文字部分）。
	User string
	// BoardPNG 是棋盘图片（会被编码成 data URI 附在 user 消息里）。
	BoardPNG []byte
	// Identity 是调用者身份（用于计费与日志归属）。
	Identity reqctx.Identity
	// Group 是计费与路由分组。
	Group string
}

// CallVision 实现 game.ModelCaller。
func (c *relayModelCaller) CallVision(ctx context.Context, modelName, systemPrompt, userPrompt string, boardPNG []byte) (string, error) {
	identity, _ := reqctx.IdentityFrom(ctx)
	return c.call(ctx, visionCall{
		Model:    modelName,
		System:   systemPrompt,
		User:     userPrompt,
		BoardPNG: boardPNG,
		Identity: identity,
		Group:    reqctx.Group(ctx),
	})
}

// call 组装并执行一次多模态对话。
//
// 单独抽出来是为了让"组装请求"与"发起请求"可分别测试：
// 组装逻辑（尤其是多模态消息体的形状）是最容易写错的部分，
// 而它完全可以用纯函数的方式断言。
func (c *relayModelCaller) call(ctx context.Context, in visionCall) (string, error) {
	if c == nil || c.relay == nil {
		return "", fmt.Errorf("game: 转发引擎未就绪")
	}
	body, err := buildVisionRequest(in)
	if err != nil {
		return "", err
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// 身份与分组必须挂到 context 上：relay 从中取计费身份与路由分组。
	// 这一步不能省——缺了身份，调用会记成"匿名"，用量无法归属到用户。
	reqCtx := reqctx.WithIdentity(ctx, in.Identity)
	if in.Group != "" {
		reqCtx = reqctx.WithGroup(reqCtx, in.Group)
	}
	req = req.WithContext(reqCtx)

	rec := httptest.NewRecorder()
	c.relay.ServeChatCompletions(rec, req)

	respBody, _ := io.ReadAll(rec.Result().Body)
	if rec.Code >= http.StatusBadRequest {
		return "", fmt.Errorf("模型调用失败（HTTP %d）：%s", rec.Code, extractErrorMessage(respBody))
	}
	text, err := extractAssistantText(respBody)
	if err != nil {
		return "", err
	}
	return text, nil
}

// buildVisionRequest 组装 OpenAI 兼容的多模态请求体。
//
// 消息结构（这是 OpenAI 多模态的标准形状，不能改）：
//
//	{"model":"...","messages":[
//	   {"role":"system","content":"<规则与格式>"},
//	   {"role":"user","content":[
//	      {"type":"text","text":"<本手提示>"},
//	      {"type":"image_url","image_url":{"url":"data:image/png;base64,..."}}
//	   ]}
//	],"stream":false}
//
// 为什么把图片放在 user 消息【后半段】（文字在前）：
// 多数视觉模型对"先读到任务要求、再看到图"的顺序理解更好；
// 反过来（图在前）在部分模型上会先描述图片内容、再才处理指令。
func buildVisionRequest(in visionCall) ([]byte, error) {
	if strings.TrimSpace(in.Model) == "" {
		return nil, fmt.Errorf("game: 未指定模型")
	}
	if len(in.BoardPNG) == 0 {
		return nil, fmt.Errorf("game: 缺少棋盘图片")
	}

	userContent := []map[string]any{
		{"type": "text", "text": in.User},
		{"type": "image_url", "image_url": map[string]any{
			"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(in.BoardPNG),
		}},
	}
	messages := make([]map[string]any, 0, 2)
	if strings.TrimSpace(in.System) != "" {
		messages = append(messages, map[string]any{"role": "system", "content": in.System})
	}
	messages = append(messages, map[string]any{"role": "user", "content": userContent})

	payload := map[string]any{
		"model":    in.Model,
		"messages": messages,
		// 明确非流式：一手棋只要最终那个坐标。
		"stream": false,
		// 温度取 0.3 而不是 0：
		//
		// 取 0 会让模型在"同一个局面下反复给出同一个非法着法"——
		// 重试时它每次都输出完全一样的内容，重试机制形同虚设。
		// 留一点随机性，重试才有机会得到不同的候选着法。
		// 也不取高值：下棋需要稳定判断，高温度会明显拉低棋力。
		"temperature": 0.3,
	}
	return json.Marshal(payload)
}

// extractAssistantText 从 OpenAI 兼容响应里取出正文。
//
// content 有两种形态（都会遇到）：
//   - 字符串："content": "MOVE: H8"
//   - 内容数组："content": [{"type":"text","text":"MOVE: H8"}]（部分模型/网关如此返回）
//
// 只取第一种会让第二种情况全部变成"模型没有回复"，而实际它有回复。
func extractAssistantText(body []byte) (string, error) {
	var parsed struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
				// 部分推理模型把正文放在 reasoning_content 之外，
				// 但极少数实现会只填 reasoning_content，这里作为兜底。
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			Text string `json:"text"` // 少数实现用顶层 text
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("game: 无法解析模型响应: %w（原文前 200 字：%s）",
			err, truncateText(string(body), 200))
	}
	// 有些实现即使 HTTP 200 也在体内返回 error 对象
	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", fmt.Errorf("模型返回错误：%s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("game: 模型响应没有 choices（原文前 200 字：%s）", truncateText(string(body), 200))
	}

	choice := parsed.Choices[0]
	if text := decodeContent(choice.Message.Content); strings.TrimSpace(text) != "" {
		return text, nil
	}
	if text := strings.TrimSpace(choice.Text); text != "" {
		return text, nil
	}
	if text := strings.TrimSpace(choice.Message.ReasoningContent); text != "" {
		return text, nil
	}
	return "", fmt.Errorf("game: 模型响应内容为空")
}

// decodeContent 解析 content 字段的两种形态。
func decodeContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// 形态一：字符串
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// 形态二：内容数组
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	}
	return ""
}

// extractErrorMessage 从错误响应体里取出可读信息。
//
// 兼容本站统一错误结构（{"error":{"message":...}}）与纯文本错误体，
// 两者都要能读出信息——读不出来时至少给出原文片段，
// 否则排查时只剩"模型调用失败"这一句，没有任何线索。
func extractErrorMessage(body []byte) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error.Message != "" {
		return parsed.Error.Message
	}
	return truncateText(string(body), 300)
}

// truncateText 截断文本用于错误信息与日志。
func truncateText(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
