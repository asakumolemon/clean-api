// OpenAI Chat 协议的解析与序列化：入口请求体 → IR，IR → 上游请求体/对外响应。
package protocol

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// RandHex 随机十六进制串（合成响应 ID 用，客户端不校验格式）。
func RandHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// flushWriter 刷新缓冲并把底层 io.Writer 的 Flush 能力透传（SSE 即时送达）。
func flushWriter(bw *bufio.Writer, w io.Writer) error {
	if err := bw.Flush(); err != nil {
		return err
	}
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
	return nil
}

// --- OpenAI 请求体解析（入口 → IR） ---

// openaiRequest OpenAI 请求体原样结构（解析用）。
type openaiRequest struct {
	Model          string            `json:"model"`
	Messages       []openaiMessage   `json:"messages"`
	Tools          []openaiTool      `json:"tools"`
	ToolChoice     any               `json:"tool_choice"`
	Stream         bool              `json:"stream"`
	Temperature    *float64          `json:"temperature"`
	MaxTokens      *int              `json:"max_tokens"`
	ResponseFormat any               `json:"response_format"`
}

type openaiMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"` // string | 内容数组 | null
	Name       string          `json:"name"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"` // JSON 字符串
		} `json:"function"`
	} `json:"tool_calls"`
}

type openaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// ParseOpenAIChatRequest 解析 OpenAI Chat 请求体为 IR。
func ParseOpenAIChatRequest(body []byte) (*ChatRequest, error) {
	var raw openaiRequest
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("请求体 JSON 解析失败: %w", err)
	}
	if raw.Model == "" {
		return nil, errors.New("缺少 model 字段")
	}
	req := &ChatRequest{
		Model:          raw.Model,
		Stream:         raw.Stream,
		Temperature:    raw.Temperature,
		MaxTokens:      raw.MaxTokens,
		ToolChoice:     raw.ToolChoice,
		ResponseFormat: raw.ResponseFormat,
	}
	for i, m := range raw.Messages {
		im, err := parseOpenAIMessage(m)
		if err != nil {
			return nil, fmt.Errorf("第 %d 条消息解析失败: %w", i+1, err)
		}
		req.Messages = append(req.Messages, im)
	}
	for _, t := range raw.Tools {
		tool := Tool{Type: t.Type}
		if tool.Type == "" {
			tool.Type = "function"
		}
		tool.Function = ToolFunction{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		}
		req.Tools = append(req.Tools, tool)
	}
	return req, nil
}

func parseOpenAIMessage(m openaiMessage) (Message, error) {
	im := Message{Role: m.Role, Name: m.Name, ToolCallID: m.ToolCallID}
	parts, err := parseOpenAIContent(m.Content)
	if err != nil {
		return im, err
	}
	im.Content = parts
	for _, tc := range m.ToolCalls {
		im.ToolCalls = append(im.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: json.RawMessage(tc.Function.Arguments),
		})
	}
	return im, nil
}

// parseOpenAIContent 兼容 content 为字符串/内容数组/null 三种形态。
func parseOpenAIContent(raw json.RawMessage) ([]ContentPart, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []ContentPart{{Type: "text", Text: s}}, nil
	}
	var arr []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, errors.New("content 既不是字符串也不是内容数组")
	}
	parts := make([]ContentPart, 0, len(arr))
	for _, p := range arr {
		switch p.Type {
		case "text":
			parts = append(parts, ContentPart{Type: "text", Text: p.Text})
		case "image_url":
			parts = append(parts, ContentPart{Type: "image", ImageURL: p.ImageURL.URL})
		default:
			return nil, fmt.Errorf("不支持的消息内容类型: %s", p.Type)
		}
	}
	return parts, nil
}

// --- OpenAI 请求体序列化（IR → 上游） ---

type openaiRequestOut struct {
	Model          string         `json:"model"`
	Messages       []messageOut   `json:"messages"`
	Tools          []toolOut      `json:"tools,omitempty"`
	ToolChoice     any            `json:"tool_choice,omitempty"`
	Stream         bool           `json:"stream,omitempty"`
	Temperature    *float64       `json:"temperature,omitempty"`
	MaxTokens      *int           `json:"max_tokens,omitempty"`
	ResponseFormat any            `json:"response_format,omitempty"`
}

type messageOut struct {
	Role       string        `json:"role"`
	Content    any           `json:"content"`
	Name       string        `json:"name,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	ToolCalls  []toolCallOut `json:"tool_calls,omitempty"`
}

type toolCallOut struct {
	ID       string      `json:"id"`
	Type     string      `json:"type"`
	Function functionOut `json:"function"`
}

type functionOut struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON 字符串
}

type toolOut struct {
	Type     string      `json:"type"`
	Function functionDef `json:"function,omitempty"`
}

type functionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// SerializeOpenAIChatRequest 把 IR 请求序列化为 OpenAI Chat 请求体。
func SerializeOpenAIChatRequest(req *ChatRequest) ([]byte, error) {
	out := openaiRequestOut{
		Model:          req.Model,
		Stream:         req.Stream,
		Temperature:    req.Temperature,
		MaxTokens:      req.MaxTokens,
		ToolChoice:     normalizeToolChoice(req.ToolChoice),
		ResponseFormat: req.ResponseFormat,
		Messages:       []messageOut{},
	}
	for _, m := range req.Messages {
		out.Messages = append(out.Messages, messageOutToOpenAI(m))
	}
	for _, t := range req.Tools {
		tool := toolOut{Type: t.Type}
		if tool.Type == "" {
			tool.Type = "function"
		}
		tool.Function = functionDef{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		}
		out.Tools = append(out.Tools, tool)
	}
	return json.Marshal(out)
}

// normalizeToolChoice 把各入口透传的 tool_choice 归一化为 OpenAI Chat 上游兼容格式。
// 上游（如 OpenCodeGo/Console Go）只认旧版两种形态：裸字符串 "auto"/"none"/"required"，或
// {"type":"function","function":{...}}；新版 {"type":"auto"} 变体与 Anthropic 的 {"type":"tool"}
// 会直接 400（unknown variant）。规则：
//   - string / nil：原样（nil 由 omitempty 省略）
//   - {"type":"auto"} / {"type":"any"} → "auto"
//   - {"type":"none"} → "none"；{"type":"required"} → "required"
//   - {"type":"tool","name":X}（Anthropic）与 {"type":"function","name":X}（Responses）→
//     {"type":"function","function":{"name":X}}
//   - {"type":"function","function":{...}}（已是旧版对象）→ 原样
func normalizeToolChoice(tc any) any {
	switch v := tc.(type) {
	case map[string]any:
		typ, _ := v["type"].(string)
		switch typ {
		case "auto", "any":
			return "auto"
		case "none":
			return "none"
		case "required":
			return "required"
		case "tool":
			// Anthropic 格式：{"type":"tool","name":X} → OpenAI 旧版函数对象
			name, _ := v["name"].(string)
			if name == "" {
				return nil
			}
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		case "function":
			// 已是旧版对象 {"type":"function","function":{...}} → 原样；
			// Responses 的 {"type":"function","name":X} → 补 function 包裹
			if _, ok := v["function"]; ok {
				return v
			}
			name, _ := v["name"].(string)
			if name == "" {
				return nil
			}
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
		return v // 其他对象（如自定义 tool_choice）原样透传
	default:
		return tc
	}
}

func messageOutToOpenAI(m Message) messageOut {
	out := messageOut{Role: m.Role, Name: m.Name, ToolCallID: m.ToolCallID}
	out.Content = contentPartsToAny(m.Content)
	for _, tc := range m.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, toolCallOut{
			ID:   tc.ID,
			Type: "function",
			Function: functionOut{
				Name:      tc.Name,
				Arguments: string(tc.Arguments),
			},
		})
	}
	return out
}

// contentPartsToAny 单文本分片退化为字符串，其余输出内容数组。
func contentPartsToAny(parts []ContentPart) any {
	if len(parts) == 0 {
		return nil
	}
	if len(parts) == 1 && parts[0].Type == "text" {
		return parts[0].Text
	}
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "text":
			out = append(out, map[string]any{"type": "text", "text": p.Text})
		case "image":
			out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": p.ImageURL}})
		}
	}
	return out
}

// --- OpenAI 响应解析（上游 → IR） ---

type openaiResponse struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int           `json:"index"`
		Message      openaiMessage `json:"message"`
		FinishReason string        `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// ParseOpenAIChatResponse 解析 OpenAI Chat 响应体为 IR。
func ParseOpenAIChatResponse(body []byte) (*ChatResponse, error) {
	var raw openaiResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析上游响应失败: %w", err)
	}
	resp := &ChatResponse{ID: raw.ID, Model: raw.Model, Created: raw.Created}
	for _, c := range raw.Choices {
		m, err := parseOpenAIMessage(c.Message)
		if err != nil {
			return nil, err
		}
		resp.Choices = append(resp.Choices, Choice{
			Index:        c.Index,
			Message:      m,
			FinishReason: c.FinishReason,
		})
	}
	resp.Usage = Usage{
		PromptTokens:     raw.Usage.PromptTokens,
		CompletionTokens: raw.Usage.CompletionTokens,
		TotalTokens:      raw.Usage.TotalTokens,
	}
	return resp, nil
}

// --- OpenAI 响应序列化（IR → 对外） ---

type openaiResponseOut struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []choiceOut `json:"choices"`
	Usage   usageOut    `json:"usage"`
}

type choiceOut struct {
	Index        int        `json:"index"`
	Message      messageOut `json:"message"`
	FinishReason string     `json:"finish_reason"`
}

type usageOut struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// SerializeOpenAIChatResponse 把 IR 响应序列化为 OpenAI Chat 格式。
func SerializeOpenAIChatResponse(resp *ChatResponse) ([]byte, error) {
	if resp.ID == "" {
		resp.ID = "chatcmpl-" + RandHex(8)
	}
	out := openaiResponseOut{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: resp.Created,
		Model:   resp.Model,
		Choices: []choiceOut{},
		Usage: usageOut{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		},
	}
	for _, c := range resp.Choices {
		out.Choices = append(out.Choices, choiceOut{
			Index:        c.Index,
			Message:      messageOutToOpenAI(c.Message),
			FinishReason: c.FinishReason,
		})
	}
	return json.Marshal(out)
}

// --- OpenAI 流式（上游解析 + 出口编码） ---

// openaiStreamChunk OpenAI 流式 chunk（choices[0].delta 增量）。
type openaiStreamChunk struct {
	Choices []struct {
		Index        int `json:"index"`
		Delta        struct {
			Content           string `json:"content"`
			ReasoningContent  string `json:"reasoning_content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
}

// OpenAIStreamParser 上游 OpenAI 流式响应解析器（状态在实例内）：
// 逐行喂入 `data:` 内容；tool_calls 增量按 index 累积（新 index → start 事件）；
// finish_reason 与 usage 缓存到 [DONE] 时随 done 事件一起发出。
type OpenAIStreamParser struct {
	toolStarted  map[int]bool
	finishReason string
	usage        *Usage
}

func NewOpenAIStreamParser() *OpenAIStreamParser {
	return &OpenAIStreamParser{toolStarted: map[int]bool{}}
}

// Feed 喂入一行 data 内容（不含 "data: " 前缀）。返回 0..n 个事件。
func (p *OpenAIStreamParser) Feed(data string) ([]StreamEvent, error) {
	if data == "[DONE]" {
		ev := StreamEvent{Type: EventDone, FinishReason: p.finishReason, Usage: p.usage}
		return []StreamEvent{ev}, nil
	}
	var chunk openaiStreamChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return nil, fmt.Errorf("解析流式块失败: %w", err)
	}
	if chunk.Usage != nil {
		p.usage = chunk.Usage
	}
	var evs []StreamEvent
	for _, c := range chunk.Choices {
		if c.FinishReason != "" {
			p.finishReason = c.FinishReason
		}
		if c.Delta.Content != "" {
			evs = append(evs, StreamEvent{Type: EventTextDelta, Delta: c.Delta.Content})
		}
		if c.Delta.ReasoningContent != "" {
			evs = append(evs, StreamEvent{Type: EventReasoningDelta, Delta: c.Delta.ReasoningContent})
		}
		for _, tc := range c.Delta.ToolCalls {
			if !p.toolStarted[tc.Index] {
				p.toolStarted[tc.Index] = true
				evs = append(evs, StreamEvent{Type: EventToolCallStart,
					ToolCall: ToolCallDelta{Index: tc.Index, ID: tc.ID, Name: tc.Function.Name}})
			}
			if tc.Function.Arguments != "" {
				evs = append(evs, StreamEvent{Type: EventToolCallDelta,
					ToolCall: ToolCallDelta{Index: tc.Index, Arguments: tc.Function.Arguments}})
			}
		}
	}
	return evs, nil
}

// OpenAIChatStreamWriter OpenAI Chat 流式出口编码器：StreamEvent → SSE chunk。
// done 事件写最后 chunk（finish_reason + usage）并追加 [DONE]。
type OpenAIChatStreamWriter struct {
	w       *bufio.Writer
	under   io.Writer
	id      string
	model   string
	created int64
	done    bool
}

func NewOpenAIChatStreamWriter(w io.Writer, id, model string, created int64) *OpenAIChatStreamWriter {
	return &OpenAIChatStreamWriter{w: bufio.NewWriter(w), under: w, id: id, model: model, created: created}
}

// WriteEvent 写一个事件。done 事件后流结束，后续事件忽略。
func (sw *OpenAIChatStreamWriter) WriteEvent(ev StreamEvent) error {
	if sw.done {
		return nil
	}
	var err error
	switch ev.Type {
	case EventTextDelta:
		err = sw.writeChunk(map[string]any{
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": ev.Delta}, "finish_reason": nil,
			}},
		})
	case EventReasoningDelta:
		err = sw.writeChunk(map[string]any{
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"reasoning_content": ev.Delta}, "finish_reason": nil,
			}},
		})
	case EventToolCallStart:
		err = sw.writeChunk(map[string]any{
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": ev.ToolCall.Index, "id": ev.ToolCall.ID, "type": "function",
					"function": map[string]any{"name": ev.ToolCall.Name, "arguments": ""},
				}}},
				"finish_reason": nil,
			}},
		})
	case EventToolCallDelta:
		err = sw.writeChunk(map[string]any{
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": ev.ToolCall.Index, "type": "function",
					"function": map[string]any{"arguments": ev.ToolCall.Arguments},
				}}},
				"finish_reason": nil,
			}},
		})
	case EventToolCallStop:
		return nil // OpenAI 无工具调用 stop 事件
	case EventDone:
		chunk := map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": anyNil(ev.FinishReason)}},
		}
		if ev.Usage != nil {
			chunk["usage"] = ev.Usage
		}
		if err := sw.writeChunk(chunk); err != nil {
			return err
		}
		_, err = sw.w.WriteString("data: [DONE]\n\n")
		sw.done = true
	}
	if err != nil {
		return err
	}
	return flushWriter(sw.w, sw.under)
}

// WriteError 流中错误：写 OpenAI 风格 error chunk 并收尾。
func (sw *OpenAIChatStreamWriter) WriteError(err error) error {
	if sw.done {
		return nil
	}
	_ = sw.writeChunk(map[string]any{"error": map[string]any{"message": err.Error(), "type": "upstream_error"}})
	_, _ = sw.w.WriteString("data: [DONE]\n\n")
	sw.done = true
	return flushWriter(sw.w, sw.under)
}

// Finish 流收尾：若上游未发 done（未收到 [DONE]），补写 [DONE] 防客户端挂起。
func (sw *OpenAIChatStreamWriter) Finish() error {
	if sw.done {
		return nil
	}
	_, _ = sw.w.WriteString("data: [DONE]\n\n")
	sw.done = true
	return flushWriter(sw.w, sw.under)
}

func (sw *OpenAIChatStreamWriter) writeChunk(payload map[string]any) error {
	payload["id"] = sw.id
	payload["object"] = "chat.completion.chunk"
	payload["created"] = sw.created
	payload["model"] = sw.model
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = sw.w.WriteString("data: " + string(b) + "\n\n")
	return err
}

// anyNil 空串转 nil（JSON null），否则原值。
func anyNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}
