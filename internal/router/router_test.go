package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"api-gateway/internal/channel"
	"api-gateway/internal/protocol"
	"api-gateway/internal/store"
	"api-gateway/internal/upstream"
)

// --- 测试设施 ---

// countedServer 带请求计数的模拟上游。
type countedServer struct {
	srv  *httptest.Server
	reqs atomic.Int64
}

func newCountedServer(t *testing.T, handler func(n int64, w http.ResponseWriter, r *http.Request)) *countedServer {
	t.Helper()
	c := &countedServer{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(c.reqs.Add(1), w, r)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func respJSON(content string) string {
	return fmt.Sprintf(`{"id":"chatcmpl-1","created":1700000000,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, content)
}

// chatMock 返回始终 200 的模拟上游，respondContent 标识应答来源。
func chatMock(t *testing.T, respondContent string) *countedServer {
	return newCountedServer(t, func(n int64, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respJSON(respondContent))
	})
}

func newTestEnv(t *testing.T, strategy string) (*store.Store, *Router) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	chm := channel.NewManager(st, nil, 5*time.Second) // enc=nil → key 明文
	rt := New(st, chm, strategy, 5*time.Second)
	return st, rt
}

func addChannel(t *testing.T, st *store.Store, name, chType, baseURL string, keys []string, models map[string]store.Capabilities) int64 {
	t.Helper()
	ctx := context.Background()
	chID, err := st.CreateChannel(ctx, name, chType, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if _, err := st.AddChannelKey(ctx, chID, k); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.SyncModels(ctx, chID, models, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	return chID
}

func chatReq(model string) *protocol.ChatRequest {
	return &protocol.ChatRequest{
		Model:    model,
		Messages: []protocol.Message{{Role: "user", Content: []protocol.ContentPart{{Type: "text", Text: "hi"}}}},
	}
}

// --- 用例 ---

func TestChatSuccess(t *testing.T) {
	st, rt := newTestEnv(t, "random")
	mock := chatMock(t, "你好！")
	addChannel(t, st, "主渠道", "openai", mock.srv.URL, []string{"sk-1"}, map[string]store.Capabilities{"deepseek-chat": {}})

	resp, err := rt.Chat(context.Background(), "deepseek-chat", chatReq("deepseek-chat"))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content[0].Text != "你好！" {
		t.Error("响应内容错误:", resp)
	}
	if mock.reqs.Load() != 1 {
		t.Error("上游应只被调用 1 次，got", mock.reqs.Load())
	}
}

// key 冷却：429 后标记冷却并换下一个 key，最终成功。
func TestChatKeyCooldown(t *testing.T) {
	st, rt := newTestEnv(t, "random")
	mock := newCountedServer(t, func(n int64, w http.ResponseWriter, r *http.Request) {
		if n == 1 { // 第一个 key 命中 → 429
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respJSON("第二个 key 成功"))
	})
	// 渠道内 key 轮换策略设为 round_robin，保证第一次选 key1、第二次选 key2
	chID := addChannel(t, st, "主渠道", "openai", mock.srv.URL, []string{"sk-1", "sk-2"}, map[string]store.Capabilities{"m": {}})
	if err := st.UpdateChannel(context.Background(), chID, "主渠道", "openai", mock.srv.URL, "active", 1, "round_robin"); err != nil {
		t.Fatal(err)
	}

	resp, err := rt.Chat(context.Background(), "m", chatReq("m"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Choices[0].Message.Content[0].Text != "第二个 key 成功" {
		t.Error("应换 key 后成功，got", resp)
	}
	// key1 应处于冷却中
	keys, _ := st.ListChannelKeys(context.Background(), chID)
	if !keys[0].CooldownUntil.Valid || keys[1].CooldownUntil.Valid {
		t.Error("key1 应冷却、key2 不应冷却:", keys)
	}
}

// 5xx 换渠道重试：渠道 A 失败后应命中渠道 B。
func TestChatFailoverChannel(t *testing.T) {
	st, rt := newTestEnv(t, "random")
	bad := newCountedServer(t, func(n int64, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	good := chatMock(t, "渠道 B 应答")
	addChannel(t, st, "坏渠道", "openai", bad.srv.URL, []string{"sk-1"}, map[string]store.Capabilities{"m": {}})
	addChannel(t, st, "好渠道", "openai", good.srv.URL, []string{"sk-2"}, map[string]store.Capabilities{"m": {}})

	resp, err := rt.Chat(context.Background(), "m", chatReq("m"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Choices[0].Message.Content[0].Text != "渠道 B 应答" {
		t.Error("应切换到渠道 B，got", resp)
	}
}

// 单渠道 5xx：无其他渠道可换时，重试同一家一次。
func TestChatRetrySameChannel(t *testing.T) {
	st, rt := newTestEnv(t, "random")
	mock := newCountedServer(t, func(n int64, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respJSON("重试成功"))
	})
	addChannel(t, st, "单渠道", "openai", mock.srv.URL, []string{"sk-1"}, map[string]store.Capabilities{"m": {}})

	resp, err := rt.Chat(context.Background(), "m", chatReq("m"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Choices[0].Message.Content[0].Text != "重试成功" {
		t.Error("应重试同一渠道，got", resp)
	}
	if mock.reqs.Load() != 2 {
		t.Error("应恰好请求 2 次，got", mock.reqs.Load())
	}
}

// 4xx 直接透传，不重试。
func TestChatPassThrough4xx(t *testing.T) {
	st, rt := newTestEnv(t, "random")
	mock := newCountedServer(t, func(n int64, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error": {"message": "参数错误", "type": "invalid_request_error"}}`)
	})
	addChannel(t, st, "渠道", "openai", mock.srv.URL, []string{"sk-1"}, map[string]store.Capabilities{"m": {}})

	_, err := rt.Chat(context.Background(), "m", chatReq("m"))
	var uperr *upstream.Error
	if !errors.As(err, &uperr) {
		t.Fatalf("应返回 *upstream.Error，got %v", err)
	}
	if uperr.StatusCode != http.StatusBadRequest || uperr.Message != "参数错误" {
		t.Error("4xx 应透传状态码与信息:", uperr)
	}
	if mock.reqs.Load() != 1 {
		t.Error("4xx 不应重试，got", mock.reqs.Load())
	}
}

// 模型无路由 / 模型禁用 / 渠道 down → ErrModelNotFound。
func TestChatModelNotFound(t *testing.T) {
	st, rt := newTestEnv(t, "random")
	ctx := context.Background()
	mock := chatMock(t, "x")
	chID := addChannel(t, st, "渠道", "openai", mock.srv.URL, []string{"sk-1"}, map[string]store.Capabilities{"m": {}})

	// 未同步的模型
	if _, err := rt.Chat(ctx, "不存在", chatReq("不存在")); !errors.Is(err, ErrModelNotFound) {
		t.Error("未知模型应 ErrModelNotFound，got", err)
	}
	// 模型被禁用
	models, _ := st.ListModelsByChannel(ctx, chID)
	if err := st.SetModelEnabled(ctx, models[0].ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Chat(ctx, "m", chatReq("m")); !errors.Is(err, ErrModelNotFound) {
		t.Error("禁用模型应 ErrModelNotFound，got", err)
	}
	// 渠道 down
	if err := st.SetModelEnabled(ctx, models[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if err := st.SetChannelStatus(ctx, chID, "down"); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Chat(ctx, "m", chatReq("m")); !errors.Is(err, ErrModelNotFound) {
		t.Error("渠道 down 应 ErrModelNotFound，got", err)
	}
}

// round_robin 策略：多请求轮换渠道。
func TestChatRoundRobin(t *testing.T) {
	st, rt := newTestEnv(t, "round_robin")
	a := chatMock(t, "渠道A")
	b := chatMock(t, "渠道B")
	addChannel(t, st, "A", "openai", a.srv.URL, []string{"sk-1"}, map[string]store.Capabilities{"m": {}})
	addChannel(t, st, "B", "openai", b.srv.URL, []string{"sk-2"}, map[string]store.Capabilities{"m": {}})

	first, err := rt.Chat(context.Background(), "m", chatReq("m"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := rt.Chat(context.Background(), "m", chatReq("m"))
	if err != nil {
		t.Fatal(err)
	}
	c1 := first.Choices[0].Message.Content[0].Text
	c2 := second.Choices[0].Message.Content[0].Text
	if c1 == c2 {
		t.Errorf("round_robin 应轮换渠道，两次都是 %q", c1)
	}
	if a.reqs.Load() != 1 || b.reqs.Load() != 1 {
		t.Error("两个渠道应各被调用 1 次:", a.reqs.Load(), b.reqs.Load())
	}
}

// alias 命中：请求对外名，上游收到渠道内真实模型名。
func TestChatAlias(t *testing.T) {
	st, rt := newTestEnv(t, "random")
	var gotModel string
	mock := newCountedServer(t, func(n int64, w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		gotModel = req.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respJSON("ok"))
	})
	chID := addChannel(t, st, "渠道", "openai", mock.srv.URL, []string{"sk-1"}, map[string]store.Capabilities{"ds-internal": {}})
	models, _ := st.ListModelsByChannel(context.Background(), chID)
	if err := st.SetModelAlias(context.Background(), models[0].ID, "deepseek-chat"); err != nil {
		t.Fatal(err)
	}

	if _, err := rt.Chat(context.Background(), "deepseek-chat", chatReq("deepseek-chat")); err != nil {
		t.Fatal(err)
	}
	if gotModel != "ds-internal" {
		t.Error("上游应收到内部模型名 ds-internal，got", gotModel)
	}
}

// 渠道类型非 openai（M4 才支持）→ 501。
func TestChatUnsupportedType(t *testing.T) {
	st, rt := newTestEnv(t, "random")
	mock := chatMock(t, "x")
	addChannel(t, st, "渠道", "anthropic", mock.srv.URL, []string{"sk-1"}, map[string]store.Capabilities{"m": {}})

	_, err := rt.Chat(context.Background(), "m", chatReq("m"))
	var uperr *upstream.Error
	if !errors.As(err, &uperr) {
		t.Fatalf("应返回 *upstream.Error，got %v", err)
	}
	if uperr.StatusCode != http.StatusNotImplemented {
		t.Error("非 openai 渠道应 501，got", uperr.StatusCode)
	}
	if mock.reqs.Load() != 0 {
		t.Error("不应发上游请求")
	}
}
