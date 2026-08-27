// Package protocol 统一中间表示（IR）与各入口协议的解析/序列化。
// M3 实现 OpenAI Chat 非流式；M4 起支持流式（统一事件流）与 Responses/Anthropic 入口。
package protocol

import (
	"encoding/json"
	"strings"
)

// ChatRequest 统一请求 IR（REQUIREMENTS §2.3.2，M3 非流式子集）。
type ChatRequest struct {
	Model          string
	Messages       []Message
	Tools          []Tool
	ToolChoice     any // string | object（M4 工具调用转换用）
	Stream         bool
	Temperature    *float64
	MaxTokens      *int
	ResponseFormat any // json_object / json_schema / text
}

// Message 一条消息：role 为 system/user/assistant/tool。
type Message struct {
	Role       string
	Content    []ContentPart
	Name       string // 旧式 function 消息名（OpenAI 兼容保留）
	ToolCallID string // role=tool 消息关联的工具调用 ID（M4 转 Anthropic tool_result 用）
	ToolCalls  []ToolCall
}

// ContentPart 消息内容分片：text / image。
type ContentPart struct {
	Type     string // text | image
	Text     string
	ImageURL string // image 时：data URL 或 http(s) URL
}

// ToolCall 模型发起的工具调用。
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// Tool 可调用工具定义。
type Tool struct {
	Type     string // function
	Function ToolFunction
}

// ToolFunction 函数工具定义。
type ToolFunction struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// ChatResponse 统一响应 IR（非流式）。
type ChatResponse struct {
	ID      string
	Model   string
	Created int64
	Choices []Choice
	Usage   Usage
}

// Choice 单个回复候选。
type Choice struct {
	Index        int
	Message      Message
	FinishReason string // stop | length | tool_calls | content_filter
}

// Usage token 用量。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// 流式事件类型。
const (
	EventTextDelta       = "text_delta"        // Delta 携带文本增量
	EventReasoningDelta  = "reasoning_delta"   // Delta 携带思考增量（DeepSeek reasoning_content 等）
	EventToolCallStart   = "tool_call_start"   // ToolCall 携带新工具调用（Index/ID/Name）
	EventToolCallDelta   = "tool_call_delta"   // ToolCall.Arguments 携带参数增量
	EventToolCallStop    = "tool_call_stop"    // 工具调用结束
	EventDone            = "done"              // 流结束：FinishReason/Usage 可能为空
)

// StreamEvent 统一流式事件（M4 起使用）：上游 SSE 解析为事件流，再翻译成各协议 SSE。
// 工具调用为增量三事件（start/delta/stop），由各协议编码器/客户端自行拼装。
type StreamEvent struct {
	Type         string // 见上方 Event* 常量
	Delta        string
	ToolCall     ToolCallDelta
	FinishReason string // done 事件：stop | length | tool_calls（可能为空）
	Usage        *Usage // done 事件：可能为 nil
}

// ToolCallDelta 工具调用的流式增量片段。
type ToolCallDelta struct {
	Index     int    // 同一响应内工具调用的序号（各协议增量定位用）
	ID        string // start 事件携带
	Name      string // start 事件携带
	Arguments string // 增量片段（start 事件为空串）
}

// FoldSystemIntoUser 把 system 消息文本折叠进首条 user 消息并移除 system 消息
// （上游模型不支持 system 时使用，REQUIREMENTS §2.3.3）。
// 无 user 消息时保持原样。
func FoldSystemIntoUser(req *ChatRequest) {
	var sys []string
	for _, m := range req.Messages {
		if m.Role != "system" {
			continue
		}
		for _, p := range m.Content {
			if p.Type == "text" && p.Text != "" {
				sys = append(sys, p.Text)
			}
		}
	}
	if len(sys) == 0 {
		return
	}
	prefix := strings.Join(sys, "\n\n")
	for i := range req.Messages {
		m := &req.Messages[i]
		if m.Role != "user" {
			continue
		}
		switch {
		case len(m.Content) == 0:
			m.Content = []ContentPart{{Type: "text", Text: prefix}}
		case len(m.Content) == 1 && m.Content[0].Type == "text":
			m.Content[0].Text = prefix + "\n\n" + m.Content[0].Text
		default:
			m.Content = append([]ContentPart{{Type: "text", Text: prefix}}, m.Content...)
		}
		out := req.Messages[:0]
		for _, mm := range req.Messages {
			if mm.Role != "system" {
				out = append(out, mm)
			}
		}
		req.Messages = out
		return
	}
}
