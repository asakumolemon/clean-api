package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"api-gateway/internal/protocol"
)

// anthropicResp Anthropic Messages 风格成功响应体。
const anthropicResp = `{
	"id": "msg_01",
	"type": "message",
	"role": "assistant",
	"model": "claude-sonnet-4-20250514",
	"content": [{"type": "text", "text": "你好！"}],
	"stop_reason": "end_turn",
	"usage": {"input_tokens": 5, "output_tokens": 3}
}`

func newTestAnthropicAdapter(t *testing.T, srv *httptest.Server) *AnthropicAdapter {
	t.Helper()
	return NewAnthropic(srv.URL, "sk-anthropic", &http.Client{Timeout: 5 * time.Second})
}

func TestAnthropicChatSuccess(t *testing.T) {
	var gotKey, gotVersion, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		if r.URL.Path != "/v1/messages" {
			t.Error("路径应为 /v1/messages，got", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(anthropicResp))
	}))
	defer srv.Close()

	mt := 128
	resp, err := newTestAnthropicAdapter(t, srv).Chat(context.Background(), &protocol.ChatRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: &mt,
		Messages: []protocol.Message{
			{Role: "system", Content: []protocol.ContentPart{{Type: "text", Text: "你是个助手"}}},
			{Role: "user", Content: []protocol.ContentPart{{Type: "text", Text: "hi"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotKey != "sk-anthropic" || gotVersion != "2023-06-01" {
		t.Errorf("请求头错误: x-api-key=%q version=%q", gotKey, gotVersion)
	}
	// system 提取到顶层字段
	if !strings.Contains(gotBody, `"system":"你是个助手"`) {
		t.Error("system 应提取到顶层 system 字段:", gotBody)
	}
	if !strings.Contains(gotBody, `"max_tokens":128`) {
		t.Error("max_tokens 应传递:", gotBody)
	}
	// 响应解析回 IR
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content[0].Text != "你好！" {
		t.Error("响应解析错误:", resp)
	}
	if resp.Choices[0].FinishReason != "stop" || resp.Usage.PromptTokens != 5 || resp.Usage.CompletionTokens != 3 {
		t.Error("finish_reason/usage 映射错误:", resp)
	}
}

func TestAnthropicChatMaxTokensDefault(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(anthropicResp))
	}))
	defer srv.Close()

	_, err := newTestAnthropicAdapter(t, srv).Chat(context.Background(), &protocol.ChatRequest{
		Model:    "claude-sonnet-4-20250514",
		Messages: []protocol.Message{{Role: "user", Content: []protocol.ContentPart{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"max_tokens":1024`) {
		t.Error("缺 max_tokens 时应给默认值 1024:", gotBody)
	}
}

func TestAnthropicChatHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"参数错误"}}`))
	}))
	defer srv.Close()

	_, err := newTestAnthropicAdapter(t, srv).Chat(context.Background(), &protocol.ChatRequest{
		Model:    "m",
		Messages: []protocol.Message{{Role: "user", Content: []protocol.ContentPart{{Type: "text", Text: "hi"}}}},
	})
	var uperr *Error
	if !errors.As(err, &uperr) {
		t.Fatalf("应返回 *Error，got %v", err)
	}
	if uperr.StatusCode != http.StatusBadRequest || !strings.Contains(uperr.Message, "参数错误") {
		t.Error("4xx 应透传状态码与信息:", uperr)
	}
	if uperr.Retryable {
		t.Error("4xx 不应可重试")
	}
}

// Anthropic 流式：message_start → text_delta → message_delta(usage) → message_stop。
func TestAnthropicChatStream(t *testing.T) {
	sse := `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-4-20250514","content":[],"usage":{"input_tokens":5,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你好"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":5,"output_tokens":3}}

event: message_stop
data: {"type":"message_stop"}
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Error("路径应为 /v1/messages，got", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	var evs []protocol.StreamEvent
	err := newTestAnthropicAdapter(t, srv).ChatStream(context.Background(), &protocol.ChatRequest{
		Model:    "claude-sonnet-4-20250514",
		Messages: []protocol.Message{{Role: "user", Content: []protocol.ContentPart{{Type: "text", Text: "hi"}}}},
	}, func(ev protocol.StreamEvent) error {
		evs = append(evs, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// 事件序列：text_delta + done（带 stop 与 usage）
	var gotText string
	var done *protocol.StreamEvent
	for _, ev := range evs {
		switch ev.Type {
		case protocol.EventTextDelta:
			gotText += ev.Delta
		case protocol.EventDone:
			d := ev
			done = &d
		}
	}
	if gotText != "你好" {
		t.Errorf("文本增量应为「你好」，got %q", gotText)
	}
	if done == nil {
		t.Fatal("应收到 done 事件")
	}
	if done.FinishReason != "stop" {
		t.Errorf("done finish_reason 应为 stop，got %q", done.FinishReason)
	}
	if done.Usage == nil || done.Usage.PromptTokens != 5 || done.Usage.CompletionTokens != 3 {
		t.Error("done 应带 usage:", done.Usage)
	}
}

// Anthropic 流式工具调用：tool_use 块 → start/delta/stop 三事件。
func TestAnthropicChatStreamToolCalls(t *testing.T) {
	sse := `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"北京\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":5,"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	var evs []protocol.StreamEvent
	err := newTestAnthropicAdapter(t, srv).ChatStream(context.Background(), &protocol.ChatRequest{
		Model:    "claude-sonnet-4-20250514",
		Messages: []protocol.Message{{Role: "user", Content: []protocol.ContentPart{{Type: "text", Text: "hi"}}}},
	}, func(ev protocol.StreamEvent) error {
		evs = append(evs, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// 断言 start → delta → stop → done 序列
	types := make([]string, 0, len(evs))
	for _, ev := range evs {
		types = append(types, ev.Type)
	}
	joined := strings.Join(types, ",")
	want := strings.Join([]string{protocol.EventToolCallStart, protocol.EventToolCallDelta, protocol.EventToolCallStop, protocol.EventDone}, ",")
	if joined != want {
		t.Fatalf("工具调用事件序列应为 %s，got %s", want, joined)
	}
	var start, delta *protocol.StreamEvent
	for i := range evs {
		switch evs[i].Type {
		case protocol.EventToolCallStart:
			start = &evs[i]
		case protocol.EventToolCallDelta:
			delta = &evs[i]
		}
	}
	if start.ToolCall.Name != "get_weather" || start.ToolCall.ID != "toolu_01" {
		t.Error("start 事件应带 name/id:", start.ToolCall)
	}
	if !strings.Contains(delta.ToolCall.Arguments, "北京") {
		t.Error("delta 事件应带参数增量:", delta.ToolCall.Arguments)
	}
}
