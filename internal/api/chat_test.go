package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"api-gateway/internal/auth"
	"api-gateway/internal/channel"
	"api-gateway/internal/router"
	"api-gateway/internal/store"
)

func newTestSrv(t *testing.T) (*Server, *store.Store, *auth.SessionManager) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	am, err := auth.NewSessionManager("test-secret", false)
	if err != nil {
		t.Fatal(err)
	}
	chm := channel.NewManager(s, nil, 5*time.Second) // enc=nil → key 明文
	rt := router.New(s, chm, "random", 5*time.Second)
	return New(s, rt), s, am
}

func doChat(t *testing.T, srv *Server, am *auth.SessionManager, s *store.Store, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	h := am.APIAuth(s)(http.HandlerFunc(srv.ChatCompletions))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// chatUpstream 启动一个始终 200 的 OpenAI Chat 模拟上游，记录请求体。
func chatUpstream(t *testing.T, respondContent string) (*httptest.Server, *atomic.Value) {
	t.Helper()
	var lastBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		lastBody.Store(string(b))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"chatcmpl-1","created":1700000000,"model":"deepseek-chat","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`, respondContent)
	}))
	t.Cleanup(srv.Close)
	return srv, &lastBody
}

func addTestChannel(t *testing.T, s *store.Store, baseURL string) {
	t.Helper()
	ctx := context.Background()
	chID, err := s.CreateChannel(ctx, "测试渠道", "openai", baseURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddChannelKey(ctx, chID, "sk-test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SyncModels(ctx, chID, map[string]store.Capabilities{"deepseek-chat": {}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func TestChatAuthChain(t *testing.T) {
	srv, st, am := newTestSrv(t)
	ctx := context.Background()

	uid, _ := st.CreateUser(ctx, "admin", "hash", "admin")
	plain := "test-token-abc"
	_, _ = st.CreateToken(ctx, uid, "t", auth.HashToken(plain), []string{"deepseek-chat"}, false)

	// 无令牌 → 401
	if rec := doChat(t, srv, am, st, "", `{"model":"deepseek-chat"}`); rec.Code != http.StatusUnauthorized {
		t.Error("无令牌应 401，got", rec.Code)
	}

	// 无效令牌 → 401
	if rec := doChat(t, srv, am, st, "wrong-token", `{"model":"deepseek-chat"}`); rec.Code != http.StatusUnauthorized {
		t.Error("无效令牌应 401，got", rec.Code)
	}

	// 白名单外模型 → 403 model not allowed，且不发上游请求
	rec := doChat(t, srv, am, st, plain, `{"model":"gpt-4o"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatal("白名单外应 403，got", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	msg := resp["error"].(map[string]any)["message"]
	if msg != "model not allowed" {
		t.Error("403 文案应为 model not allowed，got", msg)
	}

	// 白名单内模型 → 通过鉴权，进入路由层（无渠道 → 404 model not found）
	if rec := doChat(t, srv, am, st, plain, `{"model":"deepseek-chat"}`); rec.Code != http.StatusNotFound {
		t.Error("白名单内无渠道应 404，got", rec.Code)
	}

	// 禁用令牌 → 401
	tok, _ := st.GetTokenByHash(ctx, auth.HashToken(plain))
	_ = st.SetTokenEnabled(ctx, tok.ID, false)
	if rec := doChat(t, srv, am, st, plain, `{"model":"deepseek-chat"}`); rec.Code != http.StatusUnauthorized {
		t.Error("禁用令牌应 401，got", rec.Code)
	}
	_ = srv
}

// 完整闭环：令牌 → 白名单 → 路由 → 上游 → OpenAI 响应。
func TestChatCompletionsFullFlow(t *testing.T) {
	srv, st, am := newTestSrv(t)
	ctx := context.Background()
	upstream, lastBody := chatUpstream(t, "你好！")

	uid, _ := st.CreateUser(ctx, "admin", "hash", "admin")
	plain := "test-token-abc"
	_, _ = st.CreateToken(ctx, uid, "t", auth.HashToken(plain), []string{"deepseek-chat"}, false)
	addTestChannel(t, st, upstream.URL)

	rec := doChat(t, srv, am, st, plain, `{"model":"deepseek-chat","messages":[{"role":"user","content":"你好"}],"temperature":0.5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Error("Content-Type 应为 application/json，got", ct)
	}
	var resp struct {
		Object  string `json:"object"`
		ID      string `json:"id"`
		Choices []struct {
			Index        int `json:"index"`
			Message      struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Object != "chat.completion" || resp.ID != "chatcmpl-1" {
		t.Error("响应顶层字段错误:", rec.Body.String())
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "你好！" || resp.Choices[0].FinishReason != "stop" {
		t.Error("choices 错误:", rec.Body.String())
	}
	if resp.Usage.TotalTokens != 8 {
		t.Error("usage 错误:", rec.Body.String())
	}
	// 上游应收到原始模型名与参数
	var sent map[string]any
	_ = json.Unmarshal([]byte(lastBody.Load().(string)), &sent)
	if sent["model"] != "deepseek-chat" || sent["temperature"] != 0.5 {
		t.Error("上游请求体错误:", lastBody.Load())
	}
}

// 请求参数往返：tools / tool 消息 / image_url 应原样到达上游。
func TestChatCompletionsRoundTrip(t *testing.T) {
	srv, st, am := newTestSrv(t)
	ctx := context.Background()
	upstream, lastBody := chatUpstream(t, "ok")

	uid, _ := st.CreateUser(ctx, "admin", "hash", "admin")
	plain := "test-token-abc"
	_, _ = st.CreateToken(ctx, uid, "t", auth.HashToken(plain), []string{"deepseek-chat"}, true) // 允许全部模型
	addTestChannel(t, st, upstream.URL)

	body := `{
		"model": "deepseek-chat",
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "看图"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,AAAA"}}
			]},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "c1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"北京\"}"}}
			]},
			{"role": "tool", "tool_call_id": "c1", "content": "晴"}
		],
		"tools": [{"type": "function", "function": {"name": "get_weather", "description": "查天气", "parameters": {"type": "object"}}}],
		"response_format": {"type": "json_object"}
	}`
	if rec := doChat(t, srv, am, st, plain, body); rec.Code != http.StatusOK {
		t.Fatalf("应 200，got %d: %s", rec.Code, rec.Body.String())
	}
	sent := lastBody.Load().(string)
	for _, want := range []string{`"image_url"`, `"tool_call_id":"c1"`, `"tool_calls"`, `"response_format"`, `"json_object"`} {
		if !strings.Contains(sent, want) {
			t.Errorf("上游请求体缺少 %s：%s", want, sent)
		}
	}
}

// stream=true → 501（流式 M4 实现），不发上游请求。
func TestChatStreamNotSupported(t *testing.T) {
	srv, st, am := newTestSrv(t)
	ctx := context.Background()
	upstream, _ := chatUpstream(t, "x")

	uid, _ := st.CreateUser(ctx, "admin", "hash", "admin")
	plain := "test-token-abc"
	_, _ = st.CreateToken(ctx, uid, "t", auth.HashToken(plain), []string{"deepseek-chat"}, true)
	addTestChannel(t, st, upstream.URL)

	rec := doChat(t, srv, am, st, plain, `{"model":"deepseek-chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusNotImplemented {
		t.Error("stream 应 501，got", rec.Code)
	}
}

// 上游 400 → 透传状态码与错误信息。
func TestChatUpstreamErrorPassthrough(t *testing.T) {
	srv, st, am := newTestSrv(t)
	ctx := context.Background()
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error": {"message": "参数错误", "type": "invalid_request_error"}}`)
	}))
	t.Cleanup(srv2.Close)

	uid, _ := st.CreateUser(ctx, "admin", "hash", "admin")
	plain := "test-token-abc"
	_, _ = st.CreateToken(ctx, uid, "t", auth.HashToken(plain), []string{"deepseek-chat"}, true)
	addTestChannel(t, st, srv2.URL)

	rec := doChat(t, srv, am, st, plain, `{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("应透传 400，got %d", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if got := resp["error"].(map[string]any)["message"]; got != "参数错误" {
		t.Error("应透传上游错误信息，got", got)
	}
}

// 请求体非法 → 400。
func TestChatInvalidBody(t *testing.T) {
	srv, st, am := newTestSrv(t)
	uid, _ := st.CreateUser(context.Background(), "admin", "hash", "admin")
	plain := "test-token-abc"
	_, _ = st.CreateToken(context.Background(), uid, "t", auth.HashToken(plain), []string{"deepseek-chat"}, true)

	if rec := doChat(t, srv, am, st, plain, `not json`); rec.Code != http.StatusBadRequest {
		t.Error("非法 JSON 应 400，got", rec.Code)
	}
	if rec := doChat(t, srv, am, st, plain, `{"messages":[]}`); rec.Code != http.StatusBadRequest {
		t.Error("缺 model 应 400，got", rec.Code)
	}
}

// GET /v1/models：只返回启用模型，alias 非空显示 alias，去重。
func TestModelsList(t *testing.T) {
	srv, st, am := newTestSrv(t)
	ctx := context.Background()

	uid, _ := st.CreateUser(ctx, "admin", "hash", "admin")
	plain := "test-token-abc"
	_, _ = st.CreateToken(ctx, uid, "t", auth.HashToken(plain), nil, true) // 允许全部模型

	chID, err := st.CreateChannel(ctx, "渠道", "openai", "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SyncModels(ctx, chID, map[string]store.Capabilities{"deepseek-chat": {}, "ds-coder": {}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	models, _ := st.ListModelsByChannel(ctx, chID)
	if err := st.SetModelAlias(ctx, models[1].ID, "deepseek-chat"); err != nil { // ds-coder → 别名撞名
		t.Fatal(err)
	}
	if err := st.SetModelEnabled(ctx, models[0].ID, false); err != nil { // deepseek-chat 禁用
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	h := am.APIAuth(st)(http.HandlerFunc(srv.Models))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，got %d", rec.Code)
	}
	var resp struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Object != "list" {
		t.Error("object 应为 list")
	}
	if len(resp.Data) != 1 || resp.Data[0].ID != "deepseek-chat" {
		t.Errorf("应只返回别名 deepseek-chat，got %+v", resp.Data)
	}
}
