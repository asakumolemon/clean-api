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

func TestSerializeOpenAIChatRequestNormalizesToolChoice(t *testing.T) {
	cases := []struct {
		name  string
		input any
		want  string // 期望 tool_choice 字段的 JSON 值（空串 = 字段应被省略）
	}{
		{"string_auto", "auto", `"auto"`},
		{"string_none", "none", `"none"`},
		{"nil", nil, ""},
		// Anthropic 入口：对象式 {"type":"auto"} 必须归一化为字符串（上游不认新版变体）
		{"anthropic_auto", map[string]any{"type": "auto"}, `"auto"`},
		{"anthropic_any", map[string]any{"type": "any"}, `"auto"`},
		{"anthropic_none", map[string]any{"type": "none"}, `"none"`},
		{"anthropic_required", map[string]any{"type": "required"}, `"required"`},
		{"anthropic_tool", map[string]any{"type": "tool", "name": "get_weather"}, `{"function":{"name":"get_weather"},"type":"function"}`},
		// Responses 入口：{"type":"function","name":X} 补 function 包裹
		{"responses_function_name", map[string]any{"type": "function", "name": "get_weather"}, `{"function":{"name":"get_weather"},"type":"function"}`},
		// 已是旧版对象：原样透传
		{"legacy_function", map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}, `{"function":{"name":"get_weather"},"type":"function"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := &ChatRequest{Model: "m", ToolChoice: c.input,
				Messages: []Message{{Role: "user", Content: []ContentPart{{Type: "text", Text: "hi"}}}}}
			out, err := SerializeOpenAIChatRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			var raw map[string]any
			if err := json.Unmarshal(out, &raw); err != nil {
				t.Fatal(err)
			}
			got, has := raw["tool_choice"]
			if c.want == "" {
				if has {
					t.Errorf("tool_choice 应为省略，got %v", got)
				}
				return
			}
			if !has {
				t.Fatalf("tool_choice 缺失，want %s，body %s", c.want, out)
			}
			gotJSON, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if string(gotJSON) != c.want {
				t.Errorf("tool_choice 归一化错误：got %s，want %s", gotJSON, c.want)
			}
		})
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

// --- OpenAIStreamParser（上游流式解析） ---

func TestOpenAIStreamParserTextAndUsage(t *testing.T) {
	p := NewOpenAIStreamParser()
	feed := func(data string) []StreamEvent {
		t.Helper()
		evs, err := p.Feed(data)
		if err != nil {
			t.Fatal(err)
		}
		return evs
	}
	var all []StreamEvent
	all = append(all, feed(`{"choices":[{"index":0,"delta":{"role":"assistant","content":"你好"},"finish_reason":null}]}`)...)
	all = append(all, feed(`{"choices":[{"index":0,"delta":{"content":"，世界"},"finish_reason":null}]}`)...)
	all = append(all, feed(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)...)

	all = append(all, feed("[DONE]")...)

	// 空 delta 的块不产生事件：2 个 text_delta + 1 个 done
	if len(all) != 3 {
		t.Fatalf("应产出 3 个事件（2 text_delta + done），got %d", len(all))
	}
	if all[0].Type != EventTextDelta || all[0].Delta != "你好" || all[1].Delta != "，世界" {
		t.Error("文本增量错误:", all[:2])
	}
	done := all[2]
	if done.Type != EventDone || done.FinishReason != "stop" {
		t.Error("done 事件错误:", done)
	}
	if done.Usage == nil || done.Usage.TotalTokens != 8 || done.Usage.PromptTokens != 5 {
		t.Error("usage 应缓存到 done 事件:", done.Usage)
	}
}

func TestOpenAIStreamParserToolCalls(t *testing.T) {
	p := NewOpenAIStreamParser()
	feed := func(data string) []StreamEvent {
		t.Helper()
		evs, err := p.Feed(data)
		if err != nil {
			t.Fatal(err)
		}
		return evs
	}
	var all []StreamEvent
	all = append(all, feed(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`)...)
	all = append(all, feed(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":null}]}`)...)
	all = append(all, feed(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"北京\"}"}}]},"finish_reason":null}]}`)...)
	all = append(all, feed(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)...)
	all = append(all, feed("[DONE]")...)

	if len(all) != 4 {
		t.Fatalf("应产出 4 个事件（start+2 delta+done），got %d", len(all))
	}
	start := all[0]
	if start.Type != EventToolCallStart || start.ToolCall.Index != 0 || start.ToolCall.ID != "call_1" || start.ToolCall.Name != "get_weather" {
		t.Error("tool_call_start 错误:", start)
	}
	if all[1].Type != EventToolCallDelta || all[1].ToolCall.Arguments != `{"city":` {
		t.Error("第一个 arguments 增量错误:", all[1])
	}
	if all[2].ToolCall.Arguments != `"北京"}` {
		t.Error("第二个 arguments 增量错误:", all[2])
	}
	if all[3].FinishReason != "tool_calls" {
		t.Error("done 应带 finish_reason=tool_calls:", all[3])
	}
}

func TestOpenAIStreamParserGarbage(t *testing.T) {
	p := NewOpenAIStreamParser()
	if _, err := p.Feed(`not json`); err == nil {
		t.Error("非法行应报错")
	}
}

// --- OpenAIChatStreamWriter（出口流编码） ---

func TestOpenAIChatStreamWriterSequence(t *testing.T) {
	var buf strings.Builder
	w := NewOpenAIChatStreamWriter(&buf, "chatcmpl-1", "deepseek-chat", 1700000000)

	evs := []StreamEvent{
		{Type: EventTextDelta, Delta: "你"},
		{Type: EventToolCallStart, ToolCall: ToolCallDelta{Index: 0, ID: "call_1", Name: "get_weather"}},
		{Type: EventToolCallDelta, ToolCall: ToolCallDelta{Index: 0, Arguments: `{"city":"北京"}`}},
		{Type: EventDone, FinishReason: "tool_calls", Usage: &Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}},
	}
	for _, ev := range evs {
		if err := w.WriteEvent(ev); err != nil {
			t.Fatal(err)
		}
	}
	got := buf.String()
	if !strings.HasSuffix(got, "data: [DONE]\n\n") {
		t.Error("应以 [DONE] 结尾:", got)
	}
	lines := strings.Split(strings.TrimSpace(got), "\n")
	chunks := []string{}
	for _, l := range lines {
		if strings.HasPrefix(l, "data: ") && !strings.HasPrefix(l, "data: [DONE]") {
			chunks = append(chunks, strings.TrimPrefix(l, "data: "))
		}
	}
	if len(chunks) != 4 {
		t.Fatalf("应有 4 个 chunk（text+start+delta+done），got %d", len(chunks))
	}
	var first struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(chunks[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first.ID != "chatcmpl-1" || first.Object != "chat.completion.chunk" || first.Choices[0].Delta.Content != "你" {
		t.Error("chunk 格式错误:", chunks[0])
	}
	// 工具调用 chunk：start 带 id/name，delta 只带 arguments
	if !strings.Contains(chunks[1], `"id":"call_1"`) || !strings.Contains(chunks[1], `"name":"get_weather"`) {
		t.Error("tool_call_start chunk 错误:", chunks[1])
	}
	if !strings.Contains(chunks[2], `"arguments":"{\"city\":\"北京\"}"`) {
		t.Error("tool_call_delta chunk 错误:", chunks[2])
	}
	if !strings.Contains(chunks[3], `"finish_reason":"tool_calls"`) || !strings.Contains(chunks[3], `"total_tokens":5`) {
		t.Error("done chunk 应带 finish_reason 与 usage:", chunks[3])
	}
}

// 无 done 直接 Finish：补写 [DONE] 防客户端挂起。
func TestOpenAIChatStreamWriterFinishWithoutDone(t *testing.T) {
	var buf strings.Builder
	w := NewOpenAIChatStreamWriter(&buf, "x", "m", 0)
	_ = w.WriteEvent(StreamEvent{Type: EventTextDelta, Delta: "hi"})
	if err := w.Finish(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(buf.String(), "data: [DONE]\n\n") {
		t.Error("Finish 应补写 [DONE]:", buf.String())
	}
}

// --- FoldSystemIntoUser ---

func TestFoldSystemIntoUser(t *testing.T) {
	req := &ChatRequest{Messages: []Message{
		{Role: "system", Content: []ContentPart{{Type: "text", Text: "你是助手"}}},
		{Role: "system", Content: []ContentPart{{Type: "text", Text: "第二段"}}},
		{Role: "user", Content: []ContentPart{{Type: "text", Text: "你好"}}},
		{Role: "user", Content: []ContentPart{{Type: "text", Text: "再来"}}},
	}}
	FoldSystemIntoUser(req)
	if len(req.Messages) != 2 {
		t.Fatalf("system 应被移除，剩 2 条，got %d", len(req.Messages))
	}
	first := req.Messages[0]
	if first.Role != "user" || first.Content[0].Text != "你是助手\n\n第二段\n\n你好" {
		t.Error("system 应折叠进首条 user 前缀:", first)
	}
	// 多分片 user：system 前缀应插入到最前（原 2 分片 + 前缀 = 3）
	req2 := &ChatRequest{Messages: []Message{
		{Role: "system", Content: []ContentPart{{Type: "text", Text: "S"}}},
		{Role: "user", Content: []ContentPart{{Type: "text", Text: "看图"}, {Type: "image", ImageURL: "data:image/png;base64,AA"}}},
	}}
	FoldSystemIntoUser(req2)
	c := req2.Messages[0].Content
	if len(c) != 3 || c[0].Text != "S" || c[1].Text != "看图" || c[2].Type != "image" {
		t.Error("多分片 user 折叠错误:", req2.Messages[0])
	}
	// 无 user 消息：原样保留
	req3 := &ChatRequest{Messages: []Message{{Role: "system", Content: []ContentPart{{Type: "text", Text: "S"}}}}}
	FoldSystemIntoUser(req3)
	if len(req3.Messages) != 1 || req3.Messages[0].Role != "system" {
		t.Error("无 user 消息不应折叠:", req3.Messages)
	}
}
