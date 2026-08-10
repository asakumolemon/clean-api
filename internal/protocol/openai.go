// OpenAI Chat 协议的解析与序列化：入口请求体 → IR，IR → 上游请求体/对外响应。
package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

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
		ToolChoice:     req.ToolChoice,
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
