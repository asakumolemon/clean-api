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
	rt := router.New(s, chm, "random", 5*time.Second, nil)
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
	addTestChannelType(t, s, "openai", baseURL)
}

// addTestChannelType 建指定类型的测试渠道（openai/anthropic）。
func addTestChannelType(t *testing.T, s *store.Store, chType, baseURL string) {
	t.Helper()
	ctx := context.Background()
	chID, err := s.CreateChannel(ctx, "测试渠道", chType, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddChannelKey(ctx, chID, "sk-test"); err != nil {
		t.Fatal(err)
	}
	// 模拟真实探测结果：全能力支持（system 折叠行为由 router 单测覆盖）
	if _, err := s.SyncModels(ctx, chID, map[string]store.Capabilities{
		"deepseek-chat": {System: true, Tools: true, Vision: true, JSONMode: true},
	}, time.Now().UTC()); err != nil {
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
	if rec := doChat(t, srv, am, st, plain, `{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`); rec.Code != http.StatusNotFound {
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
			Index   int `json:"index"`
			Message struct {
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

// OpenAI 入口流式：SSE 头 + chunks + [DONE]。
func TestChatStreamFlow(t *testing.T) {
	srv, st, am := newTestSrv(t)
	ctx := context.Background()
	sse := `data: {"choices":[{"index":0,"delta":{"content":"你"},"finish_reason":null}]}
data: {"choices":[{"index":0,"delta":{"content":"好"},"finish_reason":null}]}
data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}
data: [DONE]
`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	t.Cleanup(upstream.Close)

	uid, _ := st.CreateUser(ctx, "admin", "hash", "admin")
	plain := "test-token-abc"
	_, _ = st.CreateToken(ctx, uid, "t", auth.HashToken(plain), []string{"deepseek-chat"}, true)
	addTestChannel(t, st, upstream.URL)

	rec := doChat(t, srv, am, st, plain, `{"model":"deepseek-chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Error("Content-Type 应为 text/event-stream，got", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"delta":{"content":"你"}`) || !strings.Contains(body, `"delta":{"content":"好"}`) {
		t.Error("应包含文本增量 chunk:", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) || !strings.Contains(body, `"total_tokens":3`) {
		t.Error("最后 chunk 应带 finish_reason 与 usage:", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Error("应以 [DONE] 结尾:", body)
	}
}

// 流式首事件前路由失败（模型不存在）→ 正常 HTTP 错误（非 SSE）。
func TestChatStreamModelNotFound(t *testing.T) {
	srv, st, am := newTestSrv(t)
	ctx := context.Background()
	uid, _ := st.CreateUser(ctx, "admin", "hash", "admin")
	plain := "test-token-abc"
	_, _ = st.CreateToken(ctx, uid, "t", auth.HashToken(plain), []string{"deepseek-chat"}, true)

	rec := doChat(t, srv, am, st, plain, `{"model":"deepseek-chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusNotFound {
		t.Error("无渠道流式请求应 404，got", rec.Code)
	}
}

// 流式成功也要落请求日志（此前流式成功完全不写日志）；用量取自 done 事件。
func TestChatStreamSuccessLogged(t *testing.T) {
	srv, st, am := newTestSrv(t)
	ctx := context.Background()
	sse := `data: {"choices":[{"index":0,"delta":{"content":"好"},"finish_reason":null}]}
data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}
data: [DONE]
`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	t.Cleanup(upstream.Close)

	uid, _ := st.CreateUser(ctx, "admin", "hash", "admin")
	plain := "test-token-abc"
	_, _ = st.CreateToken(ctx, uid, "t", auth.HashToken(plain), []string{"deepseek-chat"}, true)
	chID := addTestChannelReturnID(t, st, upstream.URL)

	rec := doChat(t, srv, am, st, plain, `{"model":"deepseek-chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，got %d: %s", rec.Code, rec.Body.String())
	}
	logs := waitLogs(t, st, 1)
	l := logs[0]
	if l.Status != http.StatusOK || l.ChannelID != chID {
		t.Errorf("流式成功日志应记录 200 与渠道 %d: %+v", chID, l)
	}
	if l.PromptTokens != 2 || l.CompletionTokens != 1 {
		t.Errorf("流式成功日志应记录 done 事件用量: %+v", l)
	}
	if l.Error != "" {
		t.Errorf("成功日志不应有错误信息: %+v", l)
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

// --- 三入口通用工具 ---

// doV1 对任意 /v1 handler 发起带令牌请求。
func doV1(t *testing.T, handler http.HandlerFunc, am *auth.SessionManager, s *store.Store, token, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	h := am.APIAuth(s)(handler)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// upstreamCapture 记录请求体并可返回自定义响应的 OpenAI 模拟上游。
func upstreamCapture(t *testing.T, respond func(gotBody string) (int, string)) (*httptest.Server, *atomic.Value) {
	t.Helper()
	var lastBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		lastBody.Store(string(b))
		status, respBody := respond(string(b))
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv, &lastBody
}

// chatResp 非流式 OpenAI 响应体（可选工具调用）。
func chatResp(content string, toolCalls string) string {
	msg := `{"role":"assistant","content":"` + content + `"`
	if toolCalls != "" {
		msg += `,"tool_calls":[` + toolCalls + `]`
	}
	msg += `}`
	return `{"id":"chatcmpl-1","created":1700000000,"model":"deepseek-chat","choices":[{"index":0,"message":` + msg + `,"finish_reason":"` + tern(toolCalls != "", "tool_calls", "stop") + `"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
}

func tern(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

func newUserToken(t *testing.T, st *store.Store, model string) string {
	t.Helper()
	uid, _ := st.CreateUser(context.Background(), "admin", "hash", "admin")
	plain := "test-token-abc"
	_, _ = st.CreateToken(context.Background(), uid, "t", auth.HashToken(plain), []string{model}, true)
	return plain
}

// --- Anthropic Messages 入口 ---

// Claude Code 风格调用（非流式 + 工具调用）：上游收到 OpenAI 格式，出口为 Anthropic 格式。
func TestMessagesNonStreamToolCalls(t *testing.T) {
	srv, st, am := newTestSrv(t)
	upstream, lastBody := upstreamCapture(t, func(got string) (int, string) {
		return 200, chatResp("查好了", `{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}`)
	})
	plain := newUserToken(t, st, "deepseek-chat")
	addTestChannel(t, st, upstream.URL)

	body := `{
		"model": "deepseek-chat",
		"max_tokens": 1024,
		"system": "你是助手",
		"messages": [{"role": "user", "content": "北京天气？"}],
		"tools": [{"name": "get_weather", "description": "查天气", "input_schema": {"type": "object"}}]
	}`
	rec := doV1(t, srv.Messages, am, st, plain, http.MethodPost, "/v1/messages", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，got %d: %s", rec.Code, rec.Body.String())
	}
	// 上游请求：system → role=system 消息，tools → function 格式
	sent := lastBody.Load().(string)
	for _, want := range []string{`"role":"system"`, `"content":"你是助手"`, `"name":"get_weather"`, `"parameters":{"type":"object"}`} {
		if !strings.Contains(sent, want) {
			t.Errorf("上游请求缺少 %s：%s", want, sent)
		}
	}
	// 出口响应：Anthropic 格式（tool_use 块 + stop_reason=tool_use + usage 映射）
	var out struct {
		Type       string `json:"type"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string         `json:"type"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Type != "message" || out.StopReason != "tool_use" {
		t.Error("顶层字段错误:", rec.Body.String())
	}
	if len(out.Content) != 2 || out.Content[1].Type != "tool_use" || out.Content[1].ID != "call_1" || out.Content[1].Name != "get_weather" {
		t.Error("tool_use 块错误:", rec.Body.String())
	}
	if out.Content[1].Input["city"] != "北京" {
		t.Error("tool_use input 错误:", out.Content[1].Input)
	}
	if out.Usage.InputTokens != 10 || out.Usage.OutputTokens != 5 {
		t.Error("usage 应映射为 input/output:", out.Usage)
	}
}

// Anthropic 入口流式：上游 OpenAI SSE → 出口 Anthropic SSE（message_start/content_block_delta/message_stop）。
func TestMessagesStreaming(t *testing.T) {
	srv, st, am := newTestSrv(t)
	sse := `data: {"choices":[{"index":0,"delta":{"content":"你好"},"finish_reason":null}]}
data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}
data: [DONE]
`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	t.Cleanup(upstream.Close)
	plain := newUserToken(t, st, "deepseek-chat")
	addTestChannel(t, st, upstream.URL)

	body := `{"model":"deepseek-chat","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := doV1(t, srv.Messages, am, st, plain, http.MethodPost, "/v1/messages", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，got %d: %s", rec.Code, rec.Body.String())
	}
	got := rec.Body.String()
	for _, want := range []string{
		`event: message_start`,
		`event: content_block_start`,
		`event: content_block_delta`,
		`"delta":{"text":"你好","type":"text_delta"}`,
		`event: message_delta`,
		`"stop_reason":"end_turn"`,
		`event: message_stop`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Anthropic SSE 缺少 %s：\n%s", want, got)
		}
	}
	if strings.Contains(got, "[DONE]") {
		t.Error("Anthropic SSE 不应有 [DONE]")
	}
}

// Anthropic 错误格式：{type:"error",error:{...}}，且状态码映射为 not_found_error。
func TestMessagesErrorFormat(t *testing.T) {
	srv, st, am := newTestSrv(t)
	plain := newUserToken(t, st, "deepseek-chat")

	body := `{"model":"deepseek-chat","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
	rec := doV1(t, srv.Messages, am, st, plain, http.MethodPost, "/v1/messages", body)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("应 404，got %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["type"] != "error" {
		t.Error("Anthropic 错误应带 type=error:", rec.Body.String())
	}
	errObj := out["error"].(map[string]any)
	if errObj["type"] != "not_found_error" {
		t.Error("错误类型应为 not_found_error:", errObj["type"])
	}
}

// 缺 max_tokens → 400 invalid_request_error。
func TestMessagesMissingMaxTokens(t *testing.T) {
	srv, st, am := newTestSrv(t)
	plain := newUserToken(t, st, "deepseek-chat")

	rec := doV1(t, srv.Messages, am, st, plain, http.MethodPost, "/v1/messages", `{"model":"m","messages":[]}`)
	if rec.Code != http.StatusBadRequest {
		t.Error("缺 max_tokens 应 400，got", rec.Code)
	}
}

// Anthropic 入口带 tool_choice:{"type":"auto"}（对象式）→ 上游应收到旧版字符串 "auto"，
// 而不是新版 {"type":"auto"}（OpencodeGo/Console Go 等上游只认旧格式，直接 400）。
func TestMessagesToolChoiceAutoNormalized(t *testing.T) {
	srv, st, am := newTestSrv(t)
	upstream, lastBody := upstreamCapture(t, func(got string) (int, string) {
		return 200, chatResp("查好了", "")
	})
	plain := newUserToken(t, st, "deepseek-chat")
	addTestChannel(t, st, upstream.URL)

	body := `{
		"model": "deepseek-chat",
		"max_tokens": 1024,
		"tool_choice": {"type": "auto"},
		"messages": [{"role": "user", "content": "你好"}]
	}`
	rec := doV1(t, srv.Messages, am, st, plain, http.MethodPost, "/v1/messages", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，got %d: %s", rec.Code, rec.Body.String())
	}
	sent := lastBody.Load().(string)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(sent), &parsed); err != nil {
		t.Fatal(err)
	}
	tc, ok := parsed["tool_choice"].(string)
	if !ok || tc != "auto" {
		t.Errorf("tool_choice 应归一化为字符串 auto，got %v（body %s）", parsed["tool_choice"], sent)
	}
}

// 空对话拦截：messages 为空在入口直接 400，不透传 "messages":[] 给上游。
func TestMessagesEmptyInputRejected(t *testing.T) {
	srv, st, am := newTestSrv(t)
	plain := newUserToken(t, st, "deepseek-chat")

	body := `{"model":"deepseek-chat","max_tokens":100,"messages":[]}`
	rec := doV1(t, srv.Messages, am, st, plain, http.MethodPost, "/v1/messages", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空 messages 应 400，got %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["type"] != "error" {
		t.Error("Anthropic 错误应带 type=error:", rec.Body.String())
	}
	errObj := out["error"].(map[string]any)
	if errObj["type"] != "invalid_request" {
		t.Errorf("错误类型应为 invalid_request（与入口解析错误一致），got %v", errObj["type"])
	}
}

// Responses 入口 input 为空数组 → 入口直接 400（上游同样会拒 "messages":[]）。
func TestResponsesEmptyInputRejected(t *testing.T) {
	srv, st, am := newTestSrv(t)
	plain := newUserToken(t, st, "deepseek-chat")

	rec := doV1(t, srv.Responses, am, st, plain, http.MethodPost, "/v1/responses", `{"model":"deepseek-chat","input":[]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空 input 应 400，got %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["error"]; !ok {
		t.Error("Responses 错误应带 error 对象:", rec.Body.String())
	}
}

// --- Anthropic 原生上游（渠道类型 anthropic） ---

// 下游 OpenAI Chat 入口 → anthropic 类型渠道：上游收到 Messages 格式请求，出口 OpenAI 格式。
func TestChatToAnthropicUpstream(t *testing.T) {
	srv, st, am := newTestSrv(t)
	var gotKey, gotPath, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"你好"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3}}`)
	}))
	t.Cleanup(upstream.Close)

	plain := newUserToken(t, st, "deepseek-chat")
	addTestChannelType(t, st, "anthropic", upstream.URL)

	rec := doChat(t, srv, am, st, plain, `{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，got %d: %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/messages" {
		t.Errorf("anthropic 渠道应发 /v1/messages，got %s", gotPath)
	}
	if gotKey != "sk-test" {
		t.Error("anthropic 渠道应带 x-api-key 头，got", gotKey)
	}
	// 上游收到 Messages 格式（model 用渠道内真实模型名）
	if !strings.Contains(gotBody, `"model":"deepseek-chat"`) || !strings.Contains(gotBody, `"max_tokens"`) {
		t.Errorf("上游应收到 Anthropic 格式请求体：%s", gotBody)
	}
	// 出口仍为 OpenAI 格式
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	obj, _ := out["object"].(string)
	if obj != "chat.completion" {
		t.Errorf("出口应为 OpenAI 格式，got %s", rec.Body.String())
	}
}

// 下游 Anthropic 入口 → anthropic 类型渠道：全链路 Anthropic 格式互通。
func TestAnthropicToAnthropicUpstream(t *testing.T) {
	srv, st, am := newTestSrv(t)
	var gotPath, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_02","type":"message","role":"assistant","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"你好"}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":2}}`)
	}))
	t.Cleanup(upstream.Close)

	plain := newUserToken(t, st, "deepseek-chat")
	addTestChannelType(t, st, "anthropic", upstream.URL)

	body := `{"model":"deepseek-chat","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
	rec := doV1(t, srv.Messages, am, st, plain, http.MethodPost, "/v1/messages", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，got %d: %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/messages" {
		t.Errorf("应发 /v1/messages，got %s", gotPath)
	}
	// 上游收到 Anthropic 格式（含 max_tokens 透传）
	if !strings.Contains(gotBody, `"max_tokens":100`) {
		t.Errorf("max_tokens 应透传：%s", gotBody)
	}
	// 出口 Anthropic 格式
	if !strings.Contains(rec.Body.String(), `"type":"message"`) {
		t.Errorf("出口应为 Anthropic 格式：%s", rec.Body.String())
	}
}

// --- Responses 入口 ---
// Responses 非流式：instructions/function_call_output → 上游 OpenAI 格式；出口为 Responses 格式。
func TestResponsesNonStream(t *testing.T) {
	srv, st, am := newTestSrv(t)
	upstream, lastBody := upstreamCapture(t, func(got string) (int, string) {
		return 200, chatResp("北京晴", "")
	})
	plain := newUserToken(t, st, "deepseek-chat")
	addTestChannel(t, st, upstream.URL)

	body := `{
		"model": "deepseek-chat",
		"instructions": "你是助手",
		"input": [
			{"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": "{\"city\":\"北京\"}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "晴"}
		]
	}`
	rec := doV1(t, srv.Responses, am, st, plain, http.MethodPost, "/v1/responses", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，got %d: %s", rec.Code, rec.Body.String())
	}
	// 上游：instructions → system 消息；function_call_output → role=tool 消息
	sent := lastBody.Load().(string)
	for _, want := range []string{`"role":"system"`, `"role":"tool"`, `"tool_call_id":"call_1"`, `"name":"get_weather"`} {
		if !strings.Contains(sent, want) {
			t.Errorf("上游请求缺少 %s：%s", want, sent)
		}
	}
	// 出口：Responses 格式
	var out struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "response" || out.Status != "completed" {
		t.Error("顶层字段错误:", rec.Body.String())
	}
	if len(out.Output) != 1 || out.Output[0].Content[0].Text != "北京晴" {
		t.Error("output 条目错误:", rec.Body.String())
	}
}

// Responses 入口流式：output_text.delta + response.completed。
func TestResponsesStreaming(t *testing.T) {
	srv, st, am := newTestSrv(t)
	sse := `data: {"choices":[{"index":0,"delta":{"content":"你好"},"finish_reason":null}]}
data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}
data: [DONE]
`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	t.Cleanup(upstream.Close)
	plain := newUserToken(t, st, "deepseek-chat")
	addTestChannel(t, st, upstream.URL)

	body := `{"model":"deepseek-chat","stream":true,"input":"hi"}`
	rec := doV1(t, srv.Responses, am, st, plain, http.MethodPost, "/v1/responses", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，got %d: %s", rec.Code, rec.Body.String())
	}
	got := rec.Body.String()
	for _, want := range []string{
		`"type":"response.output_text.delta"`,
		`"delta":"你好"`,
		`"type":"response.completed"`,
		`"total_tokens":3`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Responses SSE 缺少 %s：\n%s", want, got)
		}
	}
	if strings.Contains(got, "[DONE]") {
		t.Error("Responses SSE 不应有 [DONE]")
	}
}

// 白名单外模型 → 403（Anthropic 入口同样校验）。
func TestMessagesWhitelist(t *testing.T) {
	srv, st, am := newTestSrv(t)
	uid, _ := st.CreateUser(context.Background(), "admin", "hash", "admin")
	plain := "test-token-abc"
	_, _ = st.CreateToken(context.Background(), uid, "t", auth.HashToken(plain), []string{"deepseek-chat"}, false)

	rec := doV1(t, srv.Messages, am, st, plain, http.MethodPost, "/v1/messages", `{"model":"gpt-4o","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusForbidden {
		t.Error("白名单外应 403，got", rec.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["type"] != "error" {
		t.Error("403 也应按 Anthropic 错误格式:", rec.Body.String())
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

// --- M5：请求日志与 request_id ---

// waitLogs 轮询等待异步日志落库（最多 2s）。
func waitLogs(t *testing.T, s *store.Store, want int) []store.RequestLog {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		logs, _ := s.ListRequestLogs(context.Background(), store.LogFilter{}, 20, 0)
		if len(logs) >= want {
			return logs
		}
		time.Sleep(20 * time.Millisecond)
	}
	logs, _ := s.ListRequestLogs(context.Background(), store.LogFilter{}, 20, 0)
	t.Fatalf("日志未在超时内写入：got %d，want %d", len(logs), want)
	return nil
}

// 完整对话后：X-Request-Id 响应头 + 异步落库（模型/状态/渠道/tokens）。
func TestChatRequestLogAndID(t *testing.T) {
	srv, st, am := newTestSrv(t)
	ctx := context.Background()
	upstream, _ := chatUpstream(t, "你好！")

	uid, _ := st.CreateUser(ctx, "admin", "hash", "admin")
	plain := "test-token-abc"
	_, _ = st.CreateToken(ctx, uid, "t", auth.HashToken(plain), []string{"deepseek-chat"}, true)
	chID := addTestChannelReturnID(t, st, upstream.URL)

	rec := doChat(t, srv, am, st, plain, `{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，got %d", rec.Code)
	}
	requestID := rec.Header().Get("X-Request-Id")
	if requestID == "" {
		t.Fatal("响应应带 X-Request-Id")
	}

	logs := waitLogs(t, st, 1)
	l := logs[0]
	if l.RequestID != requestID {
		t.Error("日志 request_id 应与响应头一致")
	}
	if l.Model != "deepseek-chat" || l.Status != 200 || l.ChannelID != chID {
		t.Errorf("日志字段错误: %+v", l)
	}
	if l.TokenID == 0 || l.UserID != uid {
		t.Errorf("日志应记录令牌与用户: %+v", l)
	}
	if l.PromptTokens != 5 || l.CompletionTokens != 3 {
		t.Errorf("日志应记录 tokens: %+v", l)
	}
	if l.LatencyMS < 0 {
		t.Error("延迟不应为负")
	}
}

// 路由错误（模型不存在）同样落库，状态 404。
func TestChatErrorLogged(t *testing.T) {
	srv, st, am := newTestSrv(t)
	ctx := context.Background()
	uid, _ := st.CreateUser(ctx, "admin", "hash", "admin")
	plain := "test-token-abc"
	_, _ = st.CreateToken(ctx, uid, "t", auth.HashToken(plain), []string{"deepseek-chat"}, true)

	rec := doChat(t, srv, am, st, plain, `{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("应 404，got %d", rec.Code)
	}
	logs := waitLogs(t, st, 1)
	if logs[0].Status != http.StatusNotFound || !strings.Contains(logs[0].Error, "model not found") {
		t.Errorf("错误日志应记录 404 与原因: %+v", logs[0])
	}
}

// addTestChannelReturnID 建测试渠道并返回渠道 ID。
func addTestChannelReturnID(t *testing.T, s *store.Store, baseURL string) int64 {
	t.Helper()
	ctx := context.Background()
	chID, err := s.CreateChannel(ctx, "测试渠道", "openai", baseURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddChannelKey(ctx, chID, "sk-test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SyncModels(ctx, chID, map[string]store.Capabilities{
		"deepseek-chat": {System: true, Tools: true, Vision: true, JSONMode: true},
	}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	return chID
}
