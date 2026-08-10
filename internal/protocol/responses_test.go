package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// --- ParseResponsesRequest ---

func TestParseResponsesRequest(t *testing.T) {
	body := `{
		"model": "deepseek-chat",
		"instructions": "你是助手",
		"input": [
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "北京天气"}]},
			{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "get_weather", "arguments": "{\"city\":\"北京\"}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "晴"}
		],
		"tools": [{"type": "function", "name": "get_weather", "description": "查天气", "parameters": {"type": "object"}}],
		"tool_choice": "auto",
		"stream": true,
		"temperature": 0.3,
		"max_output_tokens": 512,
		"text": {"format": {"type": "json_object"}}
	}`
	req, err := ParseResponsesRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "deepseek-chat" || !req.Stream {
		t.Error("顶层字段解析错误:", req.Model, req.Stream)
	}
	if req.Temperature == nil || *req.Temperature != 0.3 {
		t.Error("temperature 解析错误:", req.Temperature)
	}
	if req.MaxTokens == nil || *req.MaxTokens != 512 {
		t.Error("max_output_tokens 应映射到 MaxTokens，got", req.MaxTokens)
	}
	// instructions → system；input 三条目 → user/assistant(tool_calls)/tool
	if len(req.Messages) != 4 {
		t.Fatalf("消息数应为 4（system+user+assistant+tool），got %d", len(req.Messages))
	}
	if m := req.Messages[0]; m.Role != "system" || m.Content[0].Text != "你是助手" {
		t.Error("instructions → system 解析错误:", m)
	}
	if m := req.Messages[1]; m.Role != "user" || m.Content[0].Text != "北京天气" {
		t.Error("input_text 解析错误:", m)
	}
	if m := req.Messages[2]; m.Role != "assistant" || len(m.ToolCalls) != 1 {
		t.Error("function_call → assistant ToolCalls 解析错误:", m)
	} else if tc := m.ToolCalls[0]; tc.ID != "call_1" || tc.Name != "get_weather" || string(tc.Arguments) != `{"city":"北京"}` {
		t.Error("function_call 字段解析错误:", tc)
	}
	if m := req.Messages[3]; m.Role != "tool" || m.ToolCallID != "call_1" || m.Content[0].Text != "晴" {
		t.Error("function_call_output → tool 消息解析错误:", m)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "get_weather" {
		t.Error("tools 解析错误:", req.Tools)
	}
	var format map[string]any
	_ = json.Unmarshal(req.ResponseFormat.(json.RawMessage), &format)
	if format["type"] != "json_object" {
		t.Error("text.format 应映射为 ResponseFormat，got", req.ResponseFormat)
	}
}

// input 为纯字符串也应支持。
func TestParseResponsesStringInput(t *testing.T) {
	req, err := ParseResponsesRequest([]byte(`{"model":"m","input":"直接文本"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" || req.Messages[0].Content[0].Text != "直接文本" {
		t.Error("字符串 input 解析错误:", req.Messages)
	}
}

func TestParseResponsesErrors(t *testing.T) {
	if _, err := ParseResponsesRequest([]byte(`{"input":"hi"}`)); err == nil || !strings.Contains(err.Error(), "model") {
		t.Error("缺 model 应报错，got", err)
	}
}

// --- SerializeResponsesResponse ---

func TestSerializeResponsesResponse(t *testing.T) {
	resp := &ChatResponse{
		ID:    "resp_test",
		Model: "deepseek-chat",
		Choices: []Choice{{
			FinishReason: "stop",
			Message: Message{
				Content: []ContentPart{{Type: "text", Text: "北京晴"}},
				ToolCalls: []ToolCall{{
					ID: "call_1", Name: "get_weather", Arguments: json.RawMessage(`{"city":"北京"}`),
				}},
			},
		}},
		Usage: Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
	out, err := SerializeResponsesResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Text    string `json:"text"`
			Name    string `json:"name"`
			CallID  string `json:"call_id"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Object != "response" || raw.Status != "completed" || raw.ID != "resp_test" {
		t.Error("顶层字段错误:", string(out))
	}
	if len(raw.Output) != 2 {
		t.Fatalf("output 应为 2 项（message+function_call），got %d: %s", len(raw.Output), out)
	}
	if raw.Output[0].Type != "message" || raw.Output[0].Content[0].Type != "output_text" || raw.Output[0].Content[0].Text != "北京晴" {
		t.Error("message 输出项错误:", raw.Output[0])
	}
	if raw.Output[1].Type != "function_call" || raw.Output[1].CallID != "call_1" || raw.Output[1].Name != "get_weather" {
		t.Error("function_call 输出项错误:", raw.Output[1])
	}
	if raw.Usage.TotalTokens != 15 {
		t.Error("usage 错误:", raw.Usage)
	}
}

// --- ResponsesStreamWriter ---

func TestResponsesStreamWriterSequence(t *testing.T) {
	var buf strings.Builder
	w := NewResponsesStreamWriter(&buf, "resp_1", "deepseek-chat", 0)

	evs := []StreamEvent{
		{Type: EventTextDelta, Delta: "北"},
		{Type: EventTextDelta, Delta: "京"},
		{Type: EventToolCallStart, ToolCall: ToolCallDelta{Index: 0, ID: "call_1", Name: "get_weather"}},
		{Type: EventToolCallDelta, ToolCall: ToolCallDelta{Index: 0, Arguments: `{"city":`}},
		{Type: EventDone, FinishReason: "tool_calls", Usage: &Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}},
	}
	for _, ev := range evs {
		if err := w.WriteEvent(ev); err != nil {
			t.Fatal(err)
		}
	}
	got := buf.String()
	for _, want := range []string{
		`"type":"response.output_text.delta"`,
		`"type":"response.output_item.added"`,
		`"type":"response.function_call_arguments.delta"`,
		`"type":"response.completed"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %s：\n%s", want, got)
		}
	}
	if !strings.Contains(got, `"delta":"北"`) || !strings.Contains(got, `"delta":"京"`) {
		t.Errorf("文本增量错误：\n%s", got)
	}
	if !strings.Contains(got, `"type":"function_call"`) || !strings.Contains(got, `"call_id":"call_1"`) || !strings.Contains(got, `"name":"get_weather"`) {
		t.Errorf("function_call 条目错误：\n%s", got)
	}
	if !strings.Contains(got, `"total_tokens":5`) {
		t.Errorf("completed 应带 usage：\n%s", got)
	}
	if strings.Contains(got, "[DONE]") {
		t.Error("Responses 流不应有 [DONE]")
	}
}

// 无 done 直接 Finish：补发 response.completed。
func TestResponsesStreamWriterFinishWithoutDone(t *testing.T) {
	var buf strings.Builder
	w := NewResponsesStreamWriter(&buf, "resp_1", "m", 0)
	_ = w.WriteEvent(StreamEvent{Type: EventTextDelta, Delta: "hi"})
	if err := w.Finish(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"type":"response.completed"`) {
		t.Error("Finish 应补发 completed：", buf.String())
	}
}
