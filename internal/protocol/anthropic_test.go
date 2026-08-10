package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// --- ParseAnthropicMessagesRequest ---

func TestParseAnthropicMessagesRequest(t *testing.T) {
	body := `{
		"model": "claude-sonnet-4-20250514",
		"max_tokens": 1024,
		"temperature": 0.5,
		"system": [{"type": "text", "text": "系统提示"}],
		"messages": [
			{"role": "user", "content": "你好"},
			{"role": "assistant", "content": [
				{"type": "text", "text": "我来查天气"},
				{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "北京"}}
			]},
			{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_1", "content": "晴，25度"}]}
		],
		"tools": [{"name": "get_weather", "description": "查天气", "input_schema": {"type": "object"}}],
		"tool_choice": {"type": "auto"},
		"stream": false
	}`
	req, err := ParseAnthropicMessagesRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "claude-sonnet-4-20250514" {
		t.Error("model 解析错误:", req.Model)
	}
	if req.MaxTokens == nil || *req.MaxTokens != 1024 {
		t.Error("max_tokens 解析错误:", req.MaxTokens)
	}
	if req.Temperature == nil || *req.Temperature != 0.5 {
		t.Error("temperature 解析错误:", req.Temperature)
	}
	// system 数组 → 首条 system 消息
	if len(req.Messages) != 4 {
		t.Fatalf("消息数应为 4（system+3），got %d", len(req.Messages))
	}
	if m := req.Messages[0]; m.Role != "system" || m.Content[0].Text != "系统提示" {
		t.Error("system 解析错误:", m)
	}
	// assistant：text + tool_use → text 分片 + ToolCalls
	if m := req.Messages[2]; m.Role != "assistant" {
		t.Error("assistant 消息缺失")
	} else if len(m.Content) != 1 || m.Content[0].Text != "我来查天气" {
		t.Error("assistant 文本解析错误:", m.Content)
	} else if len(m.ToolCalls) != 1 {
		t.Fatal("tool_use 应解析为 1 个 ToolCall")
	} else if tc := m.ToolCalls[0]; tc.ID != "toolu_1" || tc.Name != "get_weather" || string(tc.Arguments) != `{"city": "北京"}` {
		t.Error("tool_use 解析错误:", tc)
	}
	// tool_result → IR tool 消息
	if m := req.Messages[3]; m.Role != "tool" || m.ToolCallID != "toolu_1" || m.Content[0].Text != "晴，25度" {
		t.Error("tool_result 解析错误:", m)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "get_weather" || req.Tools[0].Function.Description != "查天气" {
		t.Error("tools 解析错误:", req.Tools)
	}
	if req.ToolChoice == nil {
		t.Error("tool_choice 应为非空")
	}
}

func TestParseAnthropicMessagesRequestErrors(t *testing.T) {
	if _, err := ParseAnthropicMessagesRequest([]byte(`{"model":"m","messages":[]}`)); err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Error("缺 max_tokens 应报错，got", err)
	}
	if _, err := ParseAnthropicMessagesRequest([]byte(`{"max_tokens":1}`)); err == nil || !strings.Contains(err.Error(), "model") {
		t.Error("缺 model 应报错，got", err)
	}
}

// system 为字符串、image base64 块也应正确解析。
func TestParseAnthropicImageAndStringSystem(t *testing.T) {
	body := `{
		"model": "m",
		"max_tokens": 10,
		"system": "字符串系统提示",
		"messages": [{"role": "user", "content": [
			{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "AAAA"}}
		]}]
	}`
	req, err := ParseAnthropicMessagesRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Messages[0].Content[0].Text != "字符串系统提示" {
		t.Error("字符串 system 解析错误")
	}
	img := req.Messages[1].Content[0]
	if img.Type != "image" || img.ImageURL != "data:image/png;base64,AAAA" {
		t.Error("image 块应转为 data URL:", img)
	}
}

// --- SerializeAnthropicMessagesResponse ---

func TestSerializeAnthropicMessagesResponse(t *testing.T) {
	resp := &ChatResponse{
		ID:    "msg_test",
		Model: "deepseek-chat",
		Choices: []Choice{{
			FinishReason: "tool_calls",
			Message: Message{
				Content: []ContentPart{{Type: "text", Text: "查好了"}},
				ToolCalls: []ToolCall{{
					ID: "call_1", Name: "get_weather",
					Arguments: json.RawMessage(`{"city":"北京"}`),
				}},
			},
		}},
		Usage: Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
	out, err := SerializeAnthropicMessagesResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Role       string `json:"role"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Type != "message" || raw.Role != "assistant" || raw.ID != "msg_test" {
		t.Error("顶层字段错误:", string(out))
	}
	if raw.StopReason != "tool_use" {
		t.Error("tool_calls 应映射为 tool_use，got", raw.StopReason)
	}
	if len(raw.Content) != 2 {
		t.Fatalf("content 应为 2 块（text+tool_use），got %d: %s", len(raw.Content), out)
	}
	if raw.Content[0].Type != "text" || raw.Content[0].Text != "查好了" {
		t.Error("text 块错误:", raw.Content[0])
	}
	if raw.Content[1].Type != "tool_use" || raw.Content[1].ID != "call_1" || raw.Content[1].Name != "get_weather" {
		t.Error("tool_use 块错误:", raw.Content[1])
	}
	if string(raw.Content[1].Input) != `{"city":"北京"}` {
		t.Error("tool_use input 应为对象:", string(raw.Content[1].Input))
	}
	if raw.Usage.InputTokens != 10 || raw.Usage.OutputTokens != 5 {
		t.Error("usage 映射错误:", raw.Usage)
	}
}

// 纯文本响应：stop → end_turn。
func TestSerializeAnthropicStopReasonMapping(t *testing.T) {
	resp := &ChatResponse{Choices: []Choice{{FinishReason: "stop", Message: Message{
		Content: []ContentPart{{Type: "text", Text: "hi"}}}}}}
	out, _ := SerializeAnthropicMessagesResponse(resp)
	var raw struct {
		StopReason string `json:"stop_reason"`
	}
	_ = json.Unmarshal(out, &raw)
	if raw.StopReason != "end_turn" {
		t.Error("stop 应映射为 end_turn，got", raw.StopReason)
	}
}

// --- AnthropicStreamWriter ---

func TestAnthropicStreamWriterSequence(t *testing.T) {
	var buf strings.Builder
	w := NewAnthropicStreamWriter(&buf, "msg_1", "deepseek-chat")

	evs := []StreamEvent{
		{Type: EventTextDelta, Delta: "你"},
		{Type: EventTextDelta, Delta: "好"},
		{Type: EventToolCallStart, ToolCall: ToolCallDelta{Index: 0, ID: "call_1", Name: "get_weather"}},
		{Type: EventToolCallDelta, ToolCall: ToolCallDelta{Index: 0, Arguments: `{"city":`}},
		{Type: EventToolCallDelta, ToolCall: ToolCallDelta{Index: 0, Arguments: `"北京"}`}},
		{Type: EventToolCallStop, ToolCall: ToolCallDelta{Index: 0}},
		{Type: EventDone, FinishReason: "tool_calls", Usage: &Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}},
	}
	for _, ev := range evs {
		if err := w.WriteEvent(ev); err != nil {
			t.Fatal(err)
		}
	}
	got := buf.String()

	// 事件序列
	for _, want := range []string{
		`event: message_start`,
		`event: content_block_start`,
		`event: content_block_delta`,
		`event: content_block_stop`,
		`event: message_delta`,
		`event: message_stop`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %s：\n%s", want, got)
		}
	}
	// 文本块：index 0，两段 text_delta（断言不依赖 JSON 键顺序）
	if !strings.Contains(got, `"type":"content_block_start"`) || !strings.Contains(got, `"type":"text"`) || !strings.Contains(got, `"index":0`) {
		t.Errorf("文本块 start 错误：\n%s", got)
	}
	if !strings.Contains(got, `"text":"你"`) || !strings.Contains(got, `"type":"text_delta"`) ||
		!strings.Contains(got, `"text":"好"`) {
		t.Errorf("text_delta 事件错误：\n%s", got)
	}
	// 工具块：index 1，tool_use + input_json_delta 增量
	if !strings.Contains(got, `"type":"tool_use"`) || !strings.Contains(got, `"id":"call_1"`) ||
		!strings.Contains(got, `"name":"get_weather"`) || !strings.Contains(got, `"index":1`) {
		t.Errorf("tool_use 块 start 错误：\n%s", got)
	}
	if !strings.Contains(got, `"partial_json":"{\"city\":"`) || !strings.Contains(got, `"type":"input_json_delta"`) {
		t.Errorf("input_json_delta 增量错误：\n%s", got)
	}
	// 结束：stop_reason=tool_use + usage
	if !strings.Contains(got, `"delta":{"stop_reason":"tool_use"}`) ||
		!strings.Contains(got, `"usage":{"input_tokens":10,"output_tokens":5}`) {
		t.Errorf("message_delta 错误：\n%s", got)
	}
}

// 无事件直接 Finish：应补发 message_start + message_delta + message_stop。
func TestAnthropicStreamWriterFinishWithoutDone(t *testing.T) {
	var buf strings.Builder
	w := NewAnthropicStreamWriter(&buf, "msg_1", "m")
	if err := w.WriteEvent(StreamEvent{Type: EventTextDelta, Delta: "hi"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Finish(); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, `event: message_stop`) {
		t.Error("Finish 应补发 message_stop：", got)
	}
	if !strings.Contains(got, `"stop_reason":"end_turn"`) {
		t.Error("无 finish_reason 应默认 end_turn：", got)
	}
	// done 后再 Finish 不应重复输出
	n := len(buf.String())
	if err := w.Finish(); err != nil {
		t.Fatal(err)
	}
	if buf.String() != got {
		t.Error("重复 Finish 不应追加输出")
	}
	_ = n
}

// 工具调用缺 id：自动合成。
func TestAnthropicStreamWriterSynthesizeToolID(t *testing.T) {
	var buf strings.Builder
	w := NewAnthropicStreamWriter(&buf, "msg_1", "m")
	_ = w.WriteEvent(StreamEvent{Type: EventToolCallStart, ToolCall: ToolCallDelta{Index: 0, Name: "f"}})
	if !strings.Contains(buf.String(), `"id":"toolu_0"`) {
		t.Error("缺 id 时应合成 toolu_0：", buf.String())
	}
}
