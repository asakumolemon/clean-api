package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"api-gateway/internal/protocol"
)

// testResp OpenAI 风格的成功响应体。
const testResp = `{
	"id": "chatcmpl-test",
	"object": "chat.completion",
	"created": 1700000000,
	"model": "deepseek-chat",
	"choices": [{"index": 0, "message": {"role": "assistant", "content": "你好！"}, "finish_reason": "stop"}],
	"usage": {"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8}
}`

func newTestAdapter(t *testing.T, srv *httptest.Server) *OpenAIAdapter {
	t.Helper()
	return NewOpenAI(srv.URL, "sk-test", &http.Client{Timeout: 5 * time.Second})
}

func TestOpenAIChatSuccess(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/chat/completions" {
			t.Error("路径应为 /v1/chat/completions，got", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(testResp))
	}))
	defer srv.Close()

	tm := 0.5
	mt := 128
	resp, err := newTestAdapter(t, srv).Chat(context.Background(), &protocol.ChatRequest{
		Model:       "deepseek-chat",
		Temperature: &tm,
		MaxTokens:   &mt,
		Messages:    []protocol.Message{{Role: "user", Content: []protocol.ContentPart{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer sk-test" {
		t.Error("Authorization 头错误:", gotAuth)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["model"] != "deepseek-chat" || sent["temperature"] != 0.5 || sent["max_tokens"] != float64(128) {
		t.Error("请求体字段错误:", gotBody)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content[0].Text != "你好！" {
		t.Error("响应解析错误:", resp)
	}
	if resp.Usage.TotalTokens != 8 {
		t.Error("usage 解析错误:", resp.Usage)
	}
}

// 上游 4xx 应透传：Status 保留、Retryable=false、错误信息取自上游 body。
func TestOpenAIError4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error": {"message": "参数错误", "type": "invalid_request_error"}}`))
	}))
	defer srv.Close()

	_, err := newTestAdapter(t, srv).Chat(context.Background(), &protocol.ChatRequest{Model: "m"})
	var uperr *Error
	if !errors.As(err, &uperr) {
		t.Fatalf("应返回 *Error，got %v", err)
	}
	if uperr.StatusCode != http.StatusBadRequest || uperr.Retryable {
		t.Error("4xx 应非 Retryable 且保留状态码:", uperr)
	}
	if uperr.Type != "invalid_request_error" || uperr.Message != "参数错误" {
		t.Error("错误信息应取自上游 body:", uperr)
	}
}

// 上游 429 应保留状态码（路由器据此做 key 冷却）。
func TestOpenAIError429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	_, err := newTestAdapter(t, srv).Chat(context.Background(), &protocol.ChatRequest{Model: "m"})
	var uperr *Error
	if !errors.As(err, &uperr) {
		t.Fatal("应返回 *Error")
	}
	if uperr.StatusCode != http.StatusTooManyRequests || uperr.Retryable {
		t.Error("429 应保留状态码且非 Retryable:", uperr)
	}
}

// 上游 5xx 应标记 Retryable。
func TestOpenAIError5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	_, err := newTestAdapter(t, srv).Chat(context.Background(), &protocol.ChatRequest{Model: "m"})
	var uperr *Error
	if !errors.As(err, &uperr) {
		t.Fatal("应返回 *Error")
	}
	if uperr.StatusCode != http.StatusBadGateway || !uperr.Retryable {
		t.Error("5xx 应 Retryable:", uperr)
	}
}

// 网络错误（连接被拒）应标记 Retryable 且 StatusCode=0。
func TestOpenAINetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srvURL := srv.URL
	srv.Close() // 立即关闭，连接必然失败

	_, err := NewOpenAI(srvURL, "sk-test", &http.Client{Timeout: time.Second}).Chat(context.Background(), &protocol.ChatRequest{Model: "m"})
	var uperr *Error
	if !errors.As(err, &uperr) {
		t.Fatal("应返回 *Error")
	}
	if uperr.Retryable == false || uperr.StatusCode != 0 {
		t.Error("网络错误应 Retryable 且 StatusCode=0:", uperr)
	}
	if !strings.Contains(uperr.Message, "调用上游失败") {
		t.Error("网络错误信息不清晰:", uperr.Message)
	}
}

// 上游返回 200 但响应体非法 → 视为可重试。
func TestOpenAIUnparsable200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>502 网关错误</html>`))
	}))
	defer srv.Close()

	_, err := newTestAdapter(t, srv).Chat(context.Background(), &protocol.ChatRequest{Model: "m"})
	var uperr *Error
	if !errors.As(err, &uperr) {
		t.Fatal("应返回 *Error")
	}
	if !uperr.Retryable {
		t.Error("200 但响应不可解析应 Retryable")
	}
}

// base_url 规范化：不带协议的地址应补 https、去尾斜杠。
func TestNormalizeBase(t *testing.T) {
	var gotPath string
	var gotAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(testResp))
	}))
	defer srv.Close()

	base := srv.URL + "/" // 带尾斜杠，验证会被去除
	adapter := NewOpenAI(base, "sk-1", &http.Client{Timeout: time.Second})
	if _, err := adapter.Chat(context.Background(), &protocol.ChatRequest{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Error("base_url 规范化后路径错误:", gotPath)
	}
}
