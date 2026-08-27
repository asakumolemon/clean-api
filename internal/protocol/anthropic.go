// Anthropic Messages 协议的解析与序列化（入口/出口，M4）。
// 上游适配器只支持 OpenAI 类型渠道；本文件负责「Anthropic 客户端 ⇄ IR」互转。
package protocol

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// --- 入口解析（Anthropic Messages → IR） ---

type anthropicRequest struct {
	Model       string          `json:"model"`
	MaxTokens   *int            `json:"max_tokens"`
	Temperature *float64        `json:"temperature"`
	System      json.RawMessage `json:"system"` // string | [{type:text,text}] | null
	Messages    []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"` // string | 内容块数组
	} `json:"messages"`
	Tools []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	} `json:"tools"`
	ToolChoice any  `json:"tool_choice"`
	Stream     bool `json:"stream"`
}

// ParseAnthropicMessagesRequest 解析 Anthropic Messages 请求体为 IR。
func ParseAnthropicMessagesRequest(body []byte) (*ChatRequest, error) {
	var raw anthropicRequest
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("请求体 JSON 解析失败: %w", err)
	}
	if raw.Model == "" {
		return nil, errors.New("缺少 model 字段")
	}
	if raw.MaxTokens == nil {
		return nil, errors.New("缺少 max_tokens 字段")
	}
	req := &ChatRequest{
		Model:       raw.Model,
		Stream:      raw.Stream,
		Temperature: raw.Temperature,
		MaxTokens:   raw.MaxTokens,
		ToolChoice:  raw.ToolChoice,
	}
	if sysText := parseAnthropicSystem(raw.System); sysText != "" {
		req.Messages = append(req.Messages, Message{
			Role:    "system",
			Content: []ContentPart{{Type: "text", Text: sysText}},
		})
	}
	for i, m := range raw.Messages {
		ims, err := parseAnthropicMessage(m.Role, m.Content)
		if err != nil {
			return nil, fmt.Errorf("第 %d 条消息解析失败: %w", i+1, err)
		}
		req.Messages = append(req.Messages, ims...)
	}
	for _, t := range raw.Tools {
		req.Tools = append(req.Tools, Tool{
			Type: "function",
			Function: ToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}
	return req, nil
}

// parseAnthropicSystem system 参数：字符串或 [{type:text}] 数组 → 拼接文本。
func parseAnthropicSystem(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var arr []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &arr) != nil {
		return ""
	}
	var texts []string
	for _, b := range arr {
		if b.Type == "text" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n\n")
}

// anthropicBlock Anthropic 内容块。
type anthropicBlock struct {
	Type      string          `json:"type"` // text | image | tool_use | tool_result
	Text      string          `json:"text"`
	Source    *anthropicImage `json:"source"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"` // tool_result：string | 内容块数组
}

type anthropicImage struct {
	Type      string `json:"type"` // base64 | url
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}

// parseAnthropicMessage 一条 Anthropic 消息可能产出 0..n 条 IR 消息：
// 文本 → user/assistant 文本；image → 图片分片；tool_use → assistant ToolCalls；
// tool_result → 独立 IR tool 消息（Anthropic 中它在 user 消息里）。
func parseAnthropicMessage(role string, content json.RawMessage) ([]Message, error) {
	if len(content) == 0 || string(content) == "null" {
		return []Message{{Role: role}}, nil
	}
	var s string
	if json.Unmarshal(content, &s) == nil {
		return []Message{{Role: role, Content: []ContentPart{{Type: "text", Text: s}}}}, nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil, errors.New("content 既不是字符串也不是内容块数组")
	}
	var out []Message
	var textParts []ContentPart
	var toolCalls []ToolCall
	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, ContentPart{Type: "text", Text: b.Text})
		case "image":
			img, err := imageToDataURL(b.Source)
			if err != nil {
				return nil, err
			}
			textParts = append(textParts, ContentPart{Type: "image", ImageURL: img})
		case "tool_use":
			if role == "assistant" {
				toolCalls = append(toolCalls, ToolCall{
					ID:        b.ID,
					Name:      b.Name,
					Arguments: b.Input,
				})
			}
		case "tool_result":
			txt := parseAnthropicBlockContent(b.Content)
			out = append(out, Message{
				Role:       "tool",
				ToolCallID: b.ToolUseID,
				Content:    []ContentPart{{Type: "text", Text: txt}},
			})
		default:
			return nil, fmt.Errorf("不支持的内容块类型: %s", b.Type)
		}
	}
	if len(textParts) > 0 {
		out = append(out, Message{Role: role, Content: textParts})
	}
	if len(toolCalls) > 0 {
		if len(out) > 0 && out[len(out)-1].Role == role {
			out[len(out)-1].ToolCalls = toolCalls
		} else {
			out = append(out, Message{Role: role, ToolCalls: toolCalls})
		}
	}
	return out, nil
}

// imageToDataURL Anthropic image 块 → data URL（与 OpenAI image_url 统一）。
func imageToDataURL(img *anthropicImage) (string, error) {
	if img == nil {
		return "", errors.New("image 块缺少 source")
	}
	switch img.Type {
	case "url":
		return img.URL, nil
	case "base64":
		if img.Data == "" {
			return "", errors.New("image 块缺少 base64 data")
		}
		mt := img.MediaType
		if mt == "" {
			mt = "image/png"
		}
		return "data:" + mt + ";base64," + img.Data, nil
	default:
		return "", fmt.Errorf("不支持的 image source 类型: %s", img.Type)
	}
}

// parseAnthropicBlockContent tool_result 的 content（string | 内容块数组）→ 文本。
func parseAnthropicBlockContent(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []anthropicBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var texts []string
	for _, b := range blocks {
		if b.Type == "text" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// --- 出口序列化（IR → Anthropic Messages 响应，非流式） ---

type anthropicResponseOut struct {
	ID         string            `json:"id"`
	Type       string            `json:"type"`
	Role       string            `json:"role"`
	Model      string            `json:"model"`
	Content    []any             `json:"content"`
	StopReason string            `json:"stop_reason"`
	Usage      anthropicUsageOut `json:"usage"`
}

type anthropicUsageOut struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// SerializeAnthropicMessagesResponse 把 IR 响应序列化为 Anthropic Messages 格式。
func SerializeAnthropicMessagesResponse(resp *ChatResponse) ([]byte, error) {
	if resp.ID == "" {
		resp.ID = "msg_" + RandHex(8)
	}
	out := anthropicResponseOut{
		ID:         resp.ID,
		Type:       "message",
		Role:       "assistant",
		Model:      resp.Model,
		StopReason: anthropicStopReason(firstFinishReason(resp)),
		Usage: anthropicUsageOut{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
		},
	}
	for _, c := range resp.Choices {
		for _, p := range c.Message.Content {
			if p.Type == "text" && p.Text != "" {
				out.Content = append(out.Content, map[string]any{"type": "text", "text": p.Text})
			}
		}
		for _, tc := range c.Message.ToolCalls {
			input := map[string]any{}
			if len(tc.Arguments) > 0 {
				_ = json.Unmarshal(tc.Arguments, &input) // 非法 JSON 退化为空对象
			}
			out.Content = append(out.Content, map[string]any{
				"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": input,
			})
		}
	}
	return json.Marshal(out)
}

// anthropicStopReason OpenAI finish_reason → Anthropic stop_reason。
func anthropicStopReason(finish string) string {
	switch finish {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default: // "" | stop | content_filter
		return "end_turn"
	}
}

func firstFinishReason(resp *ChatResponse) string {
	if len(resp.Choices) > 0 {
		return resp.Choices[0].FinishReason
	}
	return ""
}

// --- 出口流式（StreamEvent → Anthropic SSE） ---

// AnthropicStreamWriter Anthropic Messages 流式出口编码器。
// 事件序列：message_start → [content_block_start → content_block_delta* → content_block_stop]* →
// message_delta → message_stop。文本块与工具块共享按出现顺序递增的块序号。
type AnthropicStreamWriter struct {
	w          *bufio.Writer
	under      io.Writer
	id         string
	model      string
	started    bool        // message_start 是否已发
	blockSeq   int         // 下一个内容块序号
	textBlock  int         // 当前文本块序号，-1 表示未开
	toolBlocks map[int]int // 工具调用 index → 内容块序号
	openBlocks []int       // 已开未关的块序号（按打开顺序）
	done       bool
}

func NewAnthropicStreamWriter(w io.Writer, id, model string) *AnthropicStreamWriter {
	return &AnthropicStreamWriter{
		w: bufio.NewWriter(w), under: w, id: id, model: model,
		textBlock: -1, toolBlocks: map[int]int{},
	}
}

// WriteEvent 写一个事件。done 事件后流结束，后续事件忽略。
func (aw *AnthropicStreamWriter) WriteEvent(ev StreamEvent) error {
	if aw.done {
		return nil
	}
	if err := aw.writeMessageStartIfNeeded(); err != nil {
		return err
	}
	switch ev.Type {
	case EventTextDelta:
		if err := aw.ensureTextBlock(); err != nil {
			return err
		}
		err := aw.writeEvent(map[string]any{
			"type": "content_block_delta", "index": aw.textBlock,
			"delta": map[string]any{"type": "text_delta", "text": ev.Delta},
		})
		if err != nil {
			return err
		}
	case EventReasoningDelta:
		// Anthropic 思考内容：映射为 thinking_delta（上游为 Anthropic 生态时透传思考过程）
		if err := aw.ensureTextBlock(); err != nil {
			return err
		}
		err := aw.writeEvent(map[string]any{
			"type": "content_block_delta", "index": aw.textBlock,
			"delta": map[string]any{"type": "thinking_delta", "thinking": ev.Delta},
		})
		if err != nil {
			return err
		}
	case EventToolCallStart:
		if aw.textBlock >= 0 {
			// 内容块必须顺序出现：开新工具块前先关闭打开的文本块
			if err := aw.closeBlock(aw.textBlock); err != nil {
				return err
			}
		}
		idx := aw.blockSeq
		aw.blockSeq++
		aw.toolBlocks[ev.ToolCall.Index] = idx
		aw.openBlocks = append(aw.openBlocks, idx)
		id := ev.ToolCall.ID
		if id == "" {
			id = fmt.Sprintf("toolu_%d", idx)
		}
		err := aw.writeEvent(map[string]any{
			"type": "content_block_start", "index": idx,
			"content_block": map[string]any{"type": "tool_use", "id": id, "name": ev.ToolCall.Name, "input": map[string]any{}},
		})
		if err != nil {
			return err
		}
	case EventToolCallDelta:
		idx, ok := aw.toolBlocks[ev.ToolCall.Index]
		if !ok {
			return fmt.Errorf("工具调用增量先于 start 到达: index=%d", ev.ToolCall.Index)
		}
		err := aw.writeEvent(map[string]any{
			"type": "content_block_delta", "index": idx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": ev.ToolCall.Arguments},
		})
		if err != nil {
			return err
		}
	case EventToolCallStop:
		idx, ok := aw.toolBlocks[ev.ToolCall.Index]
		if !ok {
			return nil // 未开过则忽略
		}
		if err := aw.closeBlock(idx); err != nil {
			return err
		}
	case EventDone:
		return aw.finishMessage(ev)
	}
	return flushWriter(aw.w, aw.under)
}

// WriteError 流中错误：写 Anthropic error 事件（终止流）。
func (aw *AnthropicStreamWriter) WriteError(err error) error {
	if aw.done {
		return nil
	}
	_ = aw.writeEvent(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "api_error", "message": err.Error()},
	})
	aw.done = true
	return flushWriter(aw.w, aw.under)
}

// Finish 流收尾：未收到 done（上游未发 [DONE]）时补发 message_delta + message_stop。
func (aw *AnthropicStreamWriter) Finish() error {
	if aw.done {
		return nil
	}
	return aw.finishMessage(StreamEvent{Type: EventDone})
}

func (aw *AnthropicStreamWriter) writeMessageStartIfNeeded() error {
	if aw.started {
		return nil
	}
	aw.started = true
	return aw.writeEvent(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": aw.id, "type": "message", "role": "assistant", "model": aw.model,
			"content": []any{}, "stop_reason": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

// ensureTextBlock 打开文本内容块（首个文本增量前）。
func (aw *AnthropicStreamWriter) ensureTextBlock() error {
	if aw.textBlock >= 0 {
		return nil
	}
	idx := aw.blockSeq
	aw.blockSeq++
	aw.textBlock = idx
	aw.openBlocks = append(aw.openBlocks, idx)
	return aw.writeEvent(map[string]any{
		"type": "content_block_start", "index": idx,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
}

// closeBlock 关闭内容块（按打开顺序）。
func (aw *AnthropicStreamWriter) closeBlock(idx int) error {
	for i, open := range aw.openBlocks {
		if open == idx {
			aw.openBlocks = append(aw.openBlocks[:i], aw.openBlocks[i+1:]...)
			if aw.textBlock == idx {
				aw.textBlock = -1
			}
			return aw.writeEvent(map[string]any{"type": "content_block_stop", "index": idx})
		}
	}
	return nil
}

// finishMessage 写 message_delta（stop_reason + usage）与 message_stop。
func (aw *AnthropicStreamWriter) finishMessage(ev StreamEvent) error {
	if err := aw.writeMessageStartIfNeeded(); err != nil {
		return err
	}
	// 按打开顺序关闭所有未关内容块
	for _, idx := range append([]int(nil), aw.openBlocks...) {
		if err := aw.closeBlock(idx); err != nil {
			return err
		}
	}
	usage := map[string]any{"input_tokens": 0, "output_tokens": 0}
	if ev.Usage != nil {
		usage = map[string]any{"input_tokens": ev.Usage.PromptTokens, "output_tokens": ev.Usage.CompletionTokens}
	}
	if err := aw.writeEvent(map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": anthropicStopReason(ev.FinishReason)},
		"usage": usage,
	}); err != nil {
		return err
	}
	if err := aw.writeEvent(map[string]any{"type": "message_stop"}); err != nil {
		return err
	}
	aw.done = true
	return flushWriter(aw.w, aw.under)
}

// writeEvent 写 event: + data: 两行 SSE。
func (aw *AnthropicStreamWriter) writeEvent(payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := aw.w.WriteString("event: " + payload["type"].(string) + "\n"); err != nil {
		return err
	}
	_, err = aw.w.WriteString("data: " + string(b) + "\n\n")
	return err
}

// --- 上游序列化（IR → Anthropic Messages 请求体） ---

// anthropicRequestOut 上游 Anthropic Messages 请求体（与 anthropicRequest 对称，
// 由 IR 填充；system 与 messages 分开，Anthropic 强制 max_tokens）。
type anthropicRequestOut struct {
	Model       string                `json:"model"`
	MaxTokens   int                   `json:"max_tokens"`
	Temperature *float64              `json:"temperature,omitempty"`
	System      string                `json:"system,omitempty"`
	Messages    []anthropicMessageOut `json:"messages"`
	Tools       []anthropicToolOut    `json:"tools,omitempty"`
	ToolChoice  any                   `json:"tool_choice,omitempty"`
	Stream      bool                  `json:"stream,omitempty"`
}

type anthropicMessageOut struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string | 内容块数组
}

type anthropicToolOut struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// SerializeAnthropicMessagesRequest 把 IR 请求序列化为 Anthropic Messages 请求体。
// system 从 Messages 中 role=system 提取；max_tokens 缺失时给默认值（Anthropic 强制该字段）。
func SerializeAnthropicMessagesRequest(req *ChatRequest) ([]byte, error) {
	out := anthropicRequestOut{
		Model:       req.Model,
		Temperature: req.Temperature,
		Stream:      req.Stream,
		Messages:    []anthropicMessageOut{},
	}
	if req.MaxTokens != nil {
		out.MaxTokens = *req.MaxTokens
	} else {
		out.MaxTokens = 1024 // Anthropic 必填，缺省给保守默认值
	}
	var sysTexts []string
	for _, m := range req.Messages {
		if m.Role == "system" {
			for _, p := range m.Content {
				if p.Type == "text" {
					sysTexts = append(sysTexts, p.Text)
				}
			}
			continue
		}
		out.Messages = append(out.Messages, anthropicMessageOut{Role: m.Role, Content: anthropicContentToAny(m)})
	}
	out.System = strings.Join(sysTexts, "\n\n")
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, anthropicToolOut{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
		})
	}
	out.ToolChoice = normalizeAnthropicToolChoice(req.ToolChoice)
	return json.Marshal(out)
}

// anthropicContentToAny IR 消息 content → Anthropic 内容块。规则：
//   - role=tool → user 消息内的 tool_result 块（Anthropic 无独立 tool 角色）
//   - assistant 带 ToolCalls → tool_use 块 + 文本块
//   - 文本单分片退化为字符串，多分片/图片保持数组
func anthropicContentToAny(m Message) any {
	if m.Role == "tool" {
		txt := ""
		if len(m.Content) > 0 && m.Content[0].Type == "text" {
			txt = m.Content[0].Text
		}
		return []any{map[string]any{
			"type":        "tool_result",
			"tool_use_id": m.ToolCallID,
			"content":     txt,
		}}
	}
	out := make([]any, 0, len(m.Content)+len(m.ToolCalls))
	for _, p := range m.Content {
		switch p.Type {
		case "text":
			out = append(out, map[string]any{"type": "text", "text": p.Text})
		case "image":
			out = append(out, map[string]any{
				"type":   "image",
				"source": imageSourceFromDataURL(p.ImageURL),
			})
		}
	}
	for _, tc := range m.ToolCalls {
		input := map[string]any{}
		if len(tc.Arguments) > 0 {
			_ = json.Unmarshal(tc.Arguments, &input) // 非法 JSON 退化为空对象
		}
		out = append(out, map[string]any{
			"type":  "tool_use",
			"id":    tc.ID,
			"name":  tc.Name,
			"input": input,
		})
	}
	// 单文本块退化为字符串（Anthropic 兼容）
	if len(out) == 1 {
		if text, ok := out[0].(map[string]any); ok && text["type"] == "text" {
			return text["text"]
		}
	}
	if len(out) == 0 {
		return ""
	}
	return out
}

// imageSourceFromDataURL 把 data URL 拆成 Anthropic image source（base64/url 两种）。
func imageSourceFromDataURL(dataURL string) map[string]any {
	if strings.HasPrefix(dataURL, "data:") {
		// data:[<mediatype>][;base64],<data>
		rest := strings.TrimPrefix(dataURL, "data:")
		comma := strings.Index(rest, ",")
		if comma > 0 {
			meta, data := rest[:comma], rest[comma+1:]
			mediaType := "image/png"
			base64 := false
			if strings.HasSuffix(meta, ";base64") {
				base64 = true
				mediaType = strings.TrimSuffix(meta, ";base64")
			} else if parts := strings.Split(meta, ";"); len(parts) > 0 && parts[0] != "" {
				mediaType = parts[0]
			}
			if base64 {
				return map[string]any{"type": "base64", "media_type": mediaType, "data": data}
			}
			return map[string]any{"type": "url", "url": "data:" + meta + "," + data}
		}
	}
	return map[string]any{"type": "url", "url": dataURL}
}

// normalizeAnthropicToolChoice 把 IR 透传的 tool_choice 归一化为 Anthropic Messages 格式：
// "auto"→{"type":"auto"}、"none"→{"type":"none"}"required"→{"type":"any"}、
// OpenAI 函数对象 → {"type":"tool","name":X}；nil/对象原样。
func normalizeAnthropicToolChoice(tc any) any {
	switch v := tc.(type) {
	case string:
		switch v {
		case "auto":
			return map[string]any{"type": "auto"}
		case "none":
			return map[string]any{"type": "none"}
		case "required":
			return map[string]any{"type": "any"}
		default:
			return map[string]any{"type": "auto"}
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		switch typ {
		case "tool", "auto", "any", "none":
			return v // 已是 Anthropic 对象式
		case "function":
			// OpenAI 旧版对象：取 function.name（或其 name 字段）
			if fn, ok := v["function"].(map[string]any); ok {
				if name, _ := fn["name"].(string); name != "" {
					return map[string]any{"type": "tool", "name": name}
				}
			}
			if name, _ := v["name"].(string); name != "" {
				return map[string]any{"type": "tool", "name": name}
			}
			return map[string]any{"type": "auto"}
		default:
			return v
		}
	default:
		return tc
	}
}

// --- 上游响应解析（Anthropic Messages → IR） ---

type anthropicResponse struct {
	ID         string           `json:"id"`
	Type       string           `json:"type"`
	Role       string           `json:"role"`
	Model      string           `json:"model"`
	Content    []anthropicBlock `json:"content"`
	StopReason string           `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// ParseAnthropicMessagesResponse 解析 Anthropic Messages 响应体为 IR。
func ParseAnthropicMessagesResponse(body []byte) (*ChatResponse, error) {
	var raw anthropicResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析上游 Anthropic 响应失败: %w", err)
	}
	resp := &ChatResponse{ID: raw.ID, Model: raw.Model}
	choice := Choice{Index: 0, FinishReason: irFinishReason(raw.StopReason)}
	for _, b := range raw.Content {
		switch b.Type {
		case "text":
			choice.Message.Content = append(choice.Message.Content, ContentPart{Type: "text", Text: b.Text})
		case "tool_use":
			choice.Message.ToolCalls = append(choice.Message.ToolCalls, ToolCall{
				ID:        b.ID,
				Name:      b.Name,
				Arguments: b.Input,
			})
		}
	}
	resp.Choices = []Choice{choice}
	resp.Usage = Usage{
		PromptTokens:     raw.Usage.InputTokens,
		CompletionTokens: raw.Usage.OutputTokens,
		TotalTokens:      raw.Usage.InputTokens + raw.Usage.OutputTokens,
	}
	return resp, nil
}

// irFinishReason Anthropic stop_reason → IR finish_reason。
func irFinishReason(stop string) string {
	switch stop {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default: // end_turn | stop_sequence | ""
		return "stop"
	}
}

// --- 上游流式解析（Anthropic SSE → StreamEvent） ---

// anthropicStreamEvent Anthropic 上游 SSE 事件（event: + data: 两行）。
// 目标形态与 OpenAIStreamParser 一致：text 增量 → EventTextDelta，
// tool_use → 增量三事件（start/delta/stop），message_stop → EventDone（带 stop_reason/usage）。
type anthropicStreamEvent struct {
	Type         string          `json:"type"`
	Index        int             `json:"index"`
	Message      json.RawMessage `json:"message"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// AnthropicStreamParser 上游 Anthropic Messages 流式响应解析器（状态在实例内）：
// 逐行喂入 SSE data 内容；文本/工具增量按 content_block index 累积。
// 每个工具调用块在 content_block_start 时发 start 事件（记 index/id/name），
// input_json_delta 发 delta，content_block_stop 发 stop 事件。
// stop_reason 与 usage 在 message_delta 中缓存，message_stop 时随 done 事件一起发出。
type AnthropicStreamParser struct {
	toolBlocks   map[int]bool // content_block index → 是否为 tool_use 块
	started      bool
	finishReason string
	usage        *Usage
}

func NewAnthropicStreamParser() *AnthropicStreamParser {
	return &AnthropicStreamParser{toolBlocks: map[int]bool{}}
}

// Feed 喂入一行 data 内容（不含 "data: " 前缀）。返回 0..n 个事件。
// 收到 message_stop 后流结束，后续事件返回 nil（幂等）。
func (p *AnthropicStreamParser) Feed(data string) ([]StreamEvent, error) {
	var ev anthropicStreamEvent
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return nil, fmt.Errorf("解析 Anthropic 流式块失败: %w", err)
	}
	if p.started {
		return nil, nil // message_stop 已到，忽略后续
	}
	var out []StreamEvent
	switch ev.Type {
	case "message_start":
		// 记录 message id/model（done 事件不带，适配器负责组装 id）
	case "content_block_start":
		if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
			idx := ev.Index
			p.toolBlocks[idx] = true
			out = append(out, StreamEvent{Type: EventToolCallStart,
				ToolCall: ToolCallDelta{Index: idx, ID: ev.ContentBlock.ID, Name: ev.ContentBlock.Name}})
		}
	case "content_block_delta":
		switch ev.Delta.Type {
		case "text_delta":
			out = append(out, StreamEvent{Type: EventTextDelta, Delta: ev.Delta.Text})
		case "input_json_delta":
			idx := ev.Index
			if p.toolBlocks[idx] {
				out = append(out, StreamEvent{Type: EventToolCallDelta,
					ToolCall: ToolCallDelta{Index: idx, Arguments: ev.Delta.PartialJSON}})
			}
		}
	case "content_block_stop":
		if p.toolBlocks[ev.Index] {
			out = append(out, StreamEvent{Type: EventToolCallStop, ToolCall: ToolCallDelta{Index: ev.Index}})
		}
	case "message_delta":
		if ev.Delta.StopReason != "" {
			p.finishReason = ev.Delta.StopReason
		}
		if ev.Usage.InputTokens > 0 || ev.Usage.OutputTokens > 0 {
			p.usage = &Usage{PromptTokens: ev.Usage.InputTokens, CompletionTokens: ev.Usage.OutputTokens}
		}
	case "message_stop":
		p.started = true
		out = append(out, StreamEvent{
			Type:         EventDone,
			FinishReason: irFinishReason(p.finishReason),
			Usage:        p.usage,
		})
	case "error":
		msg := "Anthropic 上游流式错误"
		if ev.Error != nil && ev.Error.Message != "" {
			msg = ev.Error.Message
		}
		return nil, errors.New(msg)
	case "ping":
		// 心跳事件，忽略
	}
	return out, nil
}
