package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// --- ParseOpenAIChatRequest ---

func TestParseOpenAIChatRequest(t *testing.T) {
	body := `{
		"model": "deepseek-chat",
		"messages": [
			{"role": "system", "content": "你是个助手"},
			{"role": "user", "content": "你好"},
			{"role": "user", "content": [
				{"type": "text", "text": "看看这张图"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,AAAA"}}
			]},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"北京\"}"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "晴"}
		],
		"tools": [{"type": "function", "function": {"name": "get_weather", "description": "查天气", "parameters": {"type": "object"}}}],
		"tool_choice": "auto",
		"stream": false,
		"temperature": 0.7,
		"max_tokens": 256,
		"response_format": {"type": "json_object"}
	}`
	req, err := ParseOpenAIChatRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "deepseek-chat" {
		t.Error("model 解析错误:", req.Model)
	}
	if len(req.Messages) != 5 {
		t.Fatalf("消息数应为 5，got %d", len(req.Messages))
	}
	// system：字符串 content → 单 text 分片
	if m := req.Messages[0]; m.Role != "system" || len(m.Content) != 1 || m.Content[0].Text != "你是个助手" {
		t.Error("system 消息解析错误:", m)
	}
	// user：内容数组 → text + image 分片
	if m := req.Messages[2]; len(m.Content) != 2 || m.Content[1].Type != "image" || m.Content[1].ImageURL != "data:image/png;base64,AAAA" {
		t.Error("image_url 分片解析错误:", m)
	}
	// assistant：tool_calls
	if m := req.Messages[3]; len(m.ToolCalls) != 1 {
		t.Fatal("tool_calls 应解析出 1 个")
	} else if tc := m.ToolCalls[0]; tc.ID != "call_1" || tc.Name != "get_weather" || string(tc.Arguments) != `{"city":"北京"}` {
		t.Error("tool_calls 解析错误:", tc)
	}
	// tool 消息：tool_call_id 保留
	if m := req.Messages[4]; m.Role != "tool" || m.ToolCallID != "call_1" {
		t.Error("tool 消息解析错误:", m)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "get_weather" {
		t.Error("tools 解析错误:", req.Tools)
	}
	if req.ToolChoice != "auto" {
		t.Error("tool_choice 解析错误:", req.ToolChoice)
	}
	if req.Stream {
		t.Error("stream 应为 false")
	}
	if req.Temperature == nil || *req.Temperature != 0.7 {
		t.Error("temperature 解析错误:", req.Temperature)
	}
	if req.MaxTokens == nil || *req.MaxTokens != 256 {
		t.Error("max_tokens 解析错误:", req.MaxTokens)
	}
	if req.ResponseFormat == nil {
		t.Error("response_format 应为非空")
	}
}

func TestParseOpenAIChatRequestErrors(t *testing.T) {
	if _, err := ParseOpenAIChatRequest([]byte(`{"messages":[]}`)); err == nil || !strings.Contains(err.Error(), "model") {
		t.Error("缺 model 应报错，got", err)
	}
	if _, err := ParseOpenAIChatRequest([]byte(`not json`)); err == nil {
		t.Error("非法 JSON 应报错")
	}
	if _, err := ParseOpenAIChatRequest([]byte(`{"model":"m","messages":[{"role":"user","content":{"bad":1}}]}`)); err == nil {
		t.Error("非法 content 类型应报错")
	}
}

// --- 请求序列化往返 ---

func TestSerializeOpenAIChatRequest(t *testing.T) {
	tm := 0.2
	mt := 64
	req := &ChatRequest{
		Model:    "m",
		Stream:   true,
		Temperature: &tm,
		MaxTokens:   &mt,
		Messages: []Message{
			{Role: "user", Content: []ContentPart{{Type: "text", Text: "hi"}}},
			{Role: "user", Content: []ContentPart{{Type: "text", Text: "看图"}, {Type: "image", ImageURL: "data:image/png;base64,BBBB"}}},
			{Role: "tool", ToolCallID: "c1", Content: []ContentPart{{Type: "text", Text: "晴"}}},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "get_weather", Arguments: json.RawMessage(`{"city":"北京"}`)}}},
		},
		Tools: []Tool{{Type: "function", Function: ToolFunction{Name: "get_weather", Description: "查天气", Parameters: json.RawMessage(`{"type":"object"}`)}}},
	}
	out, err := SerializeOpenAIChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseOpenAIChatRequest(out)
	if err != nil {
		t.Fatalf("序列化结果应能被重新解析: %v\n%s", err, out)
	}
	// 单文本分片退化为字符串；多分片保持数组
	if len(parsed.Messages[0].Content) != 1 || parsed.Messages[0].Content[0].Text != "hi" {
		t.Error("单文本分片应退化回字符串")
	}
	if len(parsed.Messages[1].Content) != 2 || parsed.Messages[1].Content[1].Type != "image" {
		t.Error("多分片应保持数组")
	}
	if parsed.Messages[2].ToolCallID != "c1" {
		t.Error("tool 消息 tool_call_id 丢失")
	}
	if len(parsed.Messages[3].ToolCalls) != 1 || string(parsed.Messages[3].ToolCalls[0].Arguments) != `{"city":"北京"}` {
		t.Error("tool_calls 往返丢失")
	}
	if !parsed.Stream || *parsed.Temperature != 0.2 || *parsed.MaxTokens != 64 {
		t.Error("顶层参数往返丢失")
	}
	if len(parsed.Tools) != 1 || parsed.Tools[0].Function.Description != "查天气" {
		t.Error("tools 往返丢失")
	}
}

func TestSerializeOpenAIChatRequestOmitEmpty(t *testing.T) {
	req := &ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: []ContentPart{{Type: "text", Text: "hi"}}}}}
	out, err := SerializeOpenAIChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, bad := range []string{`"stream"`, `"temperature"`, `"max_tokens"`, `"tools"`, `"tool_choice"`} {
		if strings.Contains(s, bad) {
			t.Errorf("nil 字段不应输出 %s：%s", bad, s)
		}
	}
}

// --- 响应解析与序列化 ---

func TestParseAndSerializeOpenAIChatResponse(t *testing.T) {
	body := `{
		"id": "chatcmpl-abc",
		"object": "chat.completion",
		"created": 1700000000,
		"model": "deepseek-chat",
		"choices": [{
			"index": 0,
			"message": {"role": "assistant", "content": "你好！"},
			"finish_reason": "stop"
		}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	}`
	resp, err := ParseOpenAIChatResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "chatcmpl-abc" || resp.Model != "deepseek-chat" || resp.Created != 1700000000 {
		t.Error("顶层字段解析错误:", resp)
	}
	if len(resp.Choices) != 1 {
		t.Fatal("choices 应为 1 个")
	}
	c := resp.Choices[0]
	if c.Index != 0 || c.FinishReason != "stop" || len(c.Message.Content) != 1 || c.Message.Content[0].Text != "你好！" {
		t.Error("choice 解析错误:", c)
	}
	if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 5 || resp.Usage.TotalTokens != 15 {
		t.Error("usage 解析错误:", resp.Usage)
	}

	out, err := SerializeOpenAIChatResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["object"] != "chat.completion" || raw["id"] != "chatcmpl-abc" {
		t.Error("序列化顶层字段错误:", string(out))
	}
	choices := raw["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["role"] != "assistant" || msg["content"] != "你好！" {
		t.Error("序列化 message 错误:", string(out))
	}
	if raw["usage"].(map[string]any)["total_tokens"] != float64(15) {
		t.Error("序列化 usage 错误:", string(out))
	}
}

func TestSerializeEmptyChoices(t *testing.T) {
	out, err := SerializeOpenAIChatResponse(&ChatResponse{ID: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	if choices, ok := raw["choices"].([]any); !ok || len(choices) != 0 {
		t.Error("空 choices 应输出空数组:", string(out))
	}
}
