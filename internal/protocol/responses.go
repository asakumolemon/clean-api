// OpenAI Responses 协议的解析与序列化（入口/出口，M4）。
// 上游适配器只支持 OpenAI 类型渠道；本文件负责「Responses 客户端 ⇄ IR」互转。
package protocol

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// --- 入口解析（Responses → IR） ---

type responsesRequest struct {
	Model           string          `json:"model"`
	Instructions    string          `json:"instructions"`
	Input           json.RawMessage `json:"input"` // string | 条目数组
	Tools           []struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"tools"`
	ToolChoice      any             `json:"tool_choice"`
	Stream          bool            `json:"stream"`
	Temperature     *float64        `json:"temperature"`
	MaxOutputTokens *int            `json:"max_output_tokens"`
	Text            json.RawMessage `json:"text"` // {format:{type:"json_object"}} 等
}

// ParseResponsesRequest 解析 OpenAI Responses 请求体为 IR。
func ParseResponsesRequest(body []byte) (*ChatRequest, error) {
	var raw responsesRequest
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("请求体 JSON 解析失败: %w", err)
	}
	if raw.Model == "" {
		return nil, errors.New("缺少 model 字段")
	}
	req := &ChatRequest{
		Model:       raw.Model,
		Stream:      raw.Stream,
		Temperature: raw.Temperature,
		MaxTokens:   raw.MaxOutputTokens,
		ToolChoice:  raw.ToolChoice,
	}
	if raw.Instructions != "" {
		req.Messages = append(req.Messages, Message{
			Role:    "system",
			Content: []ContentPart{{Type: "text", Text: raw.Instructions}},
		})
	}
	ims, err := parseResponsesInput(raw.Input)
	if err != nil {
		return nil, err
	}
	req.Messages = append(req.Messages, ims...)
	for _, t := range raw.Tools {
		tool := Tool{Type: t.Type, Function: ToolFunction{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
		}}
		if tool.Type == "" {
			tool.Type = "function"
		}
		req.Tools = append(req.Tools, tool)
	}
	// text.format 提取为 ResponseFormat
	var text struct {
		Format json.RawMessage `json:"format"`
	}
	if len(raw.Text) > 0 && json.Unmarshal(raw.Text, &text) == nil && len(text.Format) > 0 && string(text.Format) != "null" {
		req.ResponseFormat = text.Format
	}
	return req, nil
}

// parseResponsesInput input：字符串 或 条目数组。
func parseResponsesInput(raw json.RawMessage) ([]Message, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []Message{{Role: "user", Content: []ContentPart{{Type: "text", Text: s}}}}, nil
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, errors.New("input 既不是字符串也不是条目数组")
	}
	var out []Message
	for i, item := range items {
		typ, _ := item["type"].(string)
		if typ == "" {
			// type 是可选字段（Cherry Studio 等客户端省略）：按字段推断条目类型
			if _, hasRole := item["role"]; hasRole {
				typ = "message"
			} else if _, hasOutput := item["output"]; hasOutput {
				typ = "function_call_output"
			} else if _, hasArgs := item["arguments"]; hasArgs {
				typ = "function_call"
			}
		}
		switch typ {
		case "message":
			role, _ := item["role"].(string)
			if role == "" {
				role = "user"
			}
			content, err := parseResponsesContent(item["content"])
			if err != nil {
				return nil, fmt.Errorf("input[%d] 消息内容解析失败: %w", i, err)
			}
			out = append(out, Message{Role: role, Content: content})
		case "function_call_output":
			callID, _ := item["call_id"].(string)
			output, _ := item["output"].(string)
			out = append(out, Message{
				Role:       "tool",
				ToolCallID: callID,
				Content:    []ContentPart{{Type: "text", Text: output}},
			})
		case "function_call":
			args, _ := item["arguments"].(string)
			name, _ := item["name"].(string)
			// 用 call_id 作为工具调用 ID：function_call_output 按它回传
			callID, _ := item["call_id"].(string)
			if callID == "" {
				callID, _ = item["id"].(string)
			}
			out = append(out, Message{Role: "assistant", ToolCalls: []ToolCall{
				{ID: callID, Name: name, Arguments: json.RawMessage(args)},
			}})
		default:
			// reasoning 等其他条目：忽略
		}
	}
	return out, nil
}

// parseResponsesContent message 条目 content：字符串或 [{type:input_text|input_image|output_text,...}]。
func parseResponsesContent(raw any) ([]ContentPart, error) {
	if raw == nil {
		return nil, nil
	}
	if s, ok := raw.(string); ok {
		return []ContentPart{{Type: "text", Text: s}}, nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, errors.New("content 既不是字符串也不是数组")
	}
	var parts []ContentPart
	for _, it := range arr {
		obj, ok := it.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := obj["type"].(string)
		switch typ {
		case "input_text", "output_text":
			text, _ := obj["text"].(string)
			parts = append(parts, ContentPart{Type: "text", Text: text})
		case "input_image":
			url, _ := obj["image_url"].(string)
			if url == "" {
				url, _ = obj["url"].(string)
			}
			if url == "" {
				if data, _ := obj["data"].(string); data != "" {
					mt, _ := obj["media_type"].(string)
					if mt == "" {
						mt = "image/png"
					}
					url = "data:" + mt + ";base64," + data
				}
			}
			if url != "" {
				parts = append(parts, ContentPart{Type: "image", ImageURL: url})
			}
		default:
			// 其他类型忽略
		}
	}
	return parts, nil
}

// --- 出口序列化（IR → Responses 响应，非流式） ---

type responsesResponseOut struct {
	ID        string            `json:"id"`
	Object    string            `json:"object"`
	CreatedAt int64             `json:"created_at"`
	Status    string            `json:"status"`
	Model     string            `json:"model"`
	Output    []any             `json:"output"`
	Usage     responsesUsageOut `json:"usage"`
}

type responsesUsageOut struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// SerializeResponsesResponse 把 IR 响应序列化为 OpenAI Responses 格式。
func SerializeResponsesResponse(resp *ChatResponse) ([]byte, error) {
	if resp.ID == "" {
		resp.ID = "resp_" + RandHex(8)
	}
	created := resp.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	out := responsesResponseOut{
		ID:        resp.ID,
		Object:    "response",
		CreatedAt: created,
		Status:    "completed",
		Model:     resp.Model,
		Output:    []any{},
		Usage: responsesUsageOut{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
			TotalTokens:  resp.Usage.TotalTokens,
		},
	}
	itemSeq := 0
	for _, c := range resp.Choices {
		var text strings.Builder
		for _, p := range c.Message.Content {
			if p.Type == "text" {
				text.WriteString(p.Text)
			}
		}
		if text.Len() > 0 {
			itemSeq++
			out.Output = append(out.Output, map[string]any{
				"type": "message", "id": fmt.Sprintf("msg_%d", itemSeq), "role": "assistant",
				"status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": text.String(), "annotations": []any{}}},
			})
		}
		for _, tc := range c.Message.ToolCalls {
			itemSeq++
			out.Output = append(out.Output, map[string]any{
				"type": "function_call",
				"id":   fmt.Sprintf("fc_%d", itemSeq),
				"call_id": tc.ID, "name": tc.Name, "arguments": string(tc.Arguments),
			})
		}
	}
	return json.Marshal(out)
}

// --- 出口流式（StreamEvent → Responses SSE） ---

// ResponsesStreamWriter OpenAI Responses 流式出口编码器。
// 事件序列：output_item.added → output_text.delta / function_call_arguments.delta →
// response.completed（无 [DONE]）。
type ResponsesStreamWriter struct {
	w           *bufio.Writer
	under       io.Writer
	id          string
	model       string
	created     int64
	itemSeq     int
	msgItemID   string // 文本消息条目 ID（首个文本增量前生成）
	callItemIDs map[int]string
	textBuf     strings.Builder
	output      []any // 汇总输出条目（response.completed 用）
	done        bool
}

func NewResponsesStreamWriter(w io.Writer, id, model string, created int64) *ResponsesStreamWriter {
	if created == 0 {
		created = time.Now().Unix()
	}
	return &ResponsesStreamWriter{
		w: bufio.NewWriter(w), under: w, id: id, model: model, created: created,
		callItemIDs: map[int]string{},
	}
}

// WriteEvent 写一个事件。done 事件后流结束，后续事件忽略。
func (rw *ResponsesStreamWriter) WriteEvent(ev StreamEvent) error {
	if rw.done {
		return nil
	}
	switch ev.Type {
	case EventTextDelta:
		if rw.msgItemID == "" {
			rw.itemSeq++
			rw.msgItemID = fmt.Sprintf("msg_%d", rw.itemSeq)
			if err := rw.write(map[string]any{
				"type": "response.output_item.added", "output_index": rw.itemSeq - 1,
				"item": map[string]any{
					"id": rw.msgItemID, "type": "message", "role": "assistant", "status": "in_progress",
					"content": []any{},
				},
			}); err != nil {
				return err
			}
		}
		rw.textBuf.WriteString(ev.Delta)
		if err := rw.write(map[string]any{
			"type": "response.output_text.delta", "item_id": rw.msgItemID,
			"output_index": rw.itemSeq - 1, "content_index": 0, "delta": ev.Delta,
		}); err != nil {
			return err
		}
	case EventToolCallStart:
		rw.itemSeq++
		idx := rw.itemSeq - 1
		id := ev.ToolCall.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", rw.itemSeq)
		}
		itemID := fmt.Sprintf("fc_%d", rw.itemSeq)
		rw.callItemIDs[ev.ToolCall.Index] = itemID
		rw.output = append(rw.output, map[string]any{
			"type": "function_call", "id": itemID, "call_id": id,
			"name": ev.ToolCall.Name, "arguments": "",
		})
		return rw.write(map[string]any{
			"type": "response.output_item.added", "output_index": idx,
			"item": rw.output[len(rw.output)-1],
		})
	case EventToolCallDelta:
		itemID, ok := rw.callItemIDs[ev.ToolCall.Index]
		if !ok {
			return fmt.Errorf("工具调用增量先于 start 到达: index=%d", ev.ToolCall.Index)
		}
		if err := rw.write(map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": itemID,
			"output_index": 0, "delta": ev.ToolCall.Arguments,
		}); err != nil {
			return err
		}
	case EventToolCallStop:
		return nil // Responses 无工具调用 stop 事件
	case EventDone:
		return rw.finish(ev)
	}
	return flushWriter(rw.w, rw.under)
}

// WriteError 流中错误：写 Responses error 事件。
func (rw *ResponsesStreamWriter) WriteError(err error) error {
	if rw.done {
		return nil
	}
	_ = rw.write(map[string]any{"type": "error", "error": map[string]any{"message": err.Error()}})
	rw.done = true
	return flushWriter(rw.w, rw.under)
}

// Finish 流收尾：未收到 done 时补发 response.completed。
func (rw *ResponsesStreamWriter) Finish() error {
	if rw.done {
		return nil
	}
	return rw.finish(StreamEvent{Type: EventDone})
}

func (rw *ResponsesStreamWriter) finish(ev StreamEvent) error {
	if rw.msgItemID != "" {
		// 补一条完整 message 条目到输出汇总
		rw.output = append([]any{map[string]any{
			"type": "message", "id": rw.msgItemID, "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": rw.textBuf.String(), "annotations": []any{}}},
		}}, rw.output...)
	}
	usage := map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	if ev.Usage != nil {
		usage = map[string]any{
			"input_tokens": ev.Usage.PromptTokens, "output_tokens": ev.Usage.CompletionTokens,
			"total_tokens": ev.Usage.TotalTokens,
		}
	}
	if err := rw.write(map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id": rw.id, "object": "response", "created_at": rw.created,
			"status": "completed", "model": rw.model, "output": rw.output, "usage": usage,
		},
	}); err != nil {
		return err
	}
	rw.done = true
	return flushWriter(rw.w, rw.under)
}

func (rw *ResponsesStreamWriter) write(payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = rw.w.WriteString("data: " + string(b) + "\n\n")
	return err
}
