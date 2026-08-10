// Package protocol 统一中间表示（IR）与各入口协议的解析/序列化。
// M3 仅实现 OpenAI Chat 非流式；Responses/Anthropic 与流式翻译为 M4 范围。
package protocol

import "encoding/json"

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
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}
