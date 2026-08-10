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
	"strings"
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
	rt := New(st, chm, strategy, 5*time.Second, nil)
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

	res, err := rt.Chat(context.Background(), "deepseek-chat", chatReq("deepseek-chat"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Resp.Choices) != 1 || res.Resp.Choices[0].Message.Content[0].Text != "你好！" {
		t.Error("响应内容错误:", res.Resp)
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

	res, err := rt.Chat(context.Background(), "m", chatReq("m"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Resp.Choices[0].Message.Content[0].Text != "第二个 key 成功" {
		t.Error("应换 key 后成功，got", res.Resp)
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

	res, err := rt.Chat(context.Background(), "m", chatReq("m"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Resp.Choices[0].Message.Content[0].Text != "渠道 B 应答" {
		t.Error("应切换到渠道 B，got", res.Resp)
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

	res, err := rt.Chat(context.Background(), "m", chatReq("m"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Resp.Choices[0].Message.Content[0].Text != "重试成功" {
		t.Error("应重试同一渠道，got", res.Resp)
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
	c1 := first.Resp.Choices[0].Message.Content[0].Text
	c2 := second.Resp.Choices[0].Message.Content[0].Text
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

// 渠道类型非 openai（M4 后续才支持）→ 501。
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

// --- ChatStream 流式路由 ---

// streamMock 返回固定 SSE 内容的上游。
func streamMock(t *testing.T, sse string, recordBody func(string)) *countedServer {
	return newCountedServer(t, func(n int64, w http.ResponseWriter, r *http.Request) {
		if recordBody != nil {
			b, _ := io.ReadAll(r.Body)
			recordBody(string(b))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	})
}

const streamSSE = `data: {"choices":[{"index":0,"delta":{"content":"你"},"finish_reason":null}]}
data: {"choices":[{"index":0,"delta":{"content":"好"},"finish_reason":null}]}
data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}
data: [DONE]
`

// 流式首事件前的 429：冷却 key 后换 key 重试，成功。
func TestChatStreamRetryBeforeEmit(t *testing.T) {
	st, rt := newTestEnv(t, "random")
	mock := newCountedServer(t, func(n int64, w http.ResponseWriter, r *http.Request) {
		if n == 1 { // 第一个 key → 429（未发任何事件）
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, streamSSE)
	})
	chID := addChannel(t, st, "主渠道", "openai", mock.srv.URL, []string{"sk-1", "sk-2"}, map[string]store.Capabilities{"m": {}})
	if err := st.UpdateChannel(context.Background(), chID, "主渠道", "openai", mock.srv.URL, "active", 1, "round_robin"); err != nil {
		t.Fatal(err)
	}

	var got []string
	_, err := rt.ChatStream(context.Background(), "m", chatReq("m"), func(ev protocol.StreamEvent) error {
		got = append(got, ev.Type+":"+ev.Delta)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "text_delta:你" || got[2] != "done:" {
		t.Error("事件序列错误:", got)
	}
	if mock.reqs.Load() != 2 {
		t.Error("应请求 2 次（key1 429 + key2 成功），got", mock.reqs.Load())
	}
	keys, _ := st.ListChannelKeys(context.Background(), chID)
	if !keys[0].CooldownUntil.Valid {
		t.Error("key1 应被标记冷却")
	}
}

// 已 emit 后出错：不再换渠道重试（上游只被调用 1 次）。
func TestChatStreamNoRetryAfterEmit(t *testing.T) {
	st, rt := newTestEnv(t, "round_robin") // round_robin 保证先试坏渠道（index 0）
	// 第一个渠道：先发一个 chunk，再发非法行触发流中错误
	bad := newCountedServer(t, func(n int64, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: not-json\n\n")
	})
	good := chatMock(t, "不应被调用")
	addChannel(t, st, "坏渠道", "openai", bad.srv.URL, []string{"sk-1"}, map[string]store.Capabilities{"m": {}})
	addChannel(t, st, "好渠道", "openai", good.srv.URL, []string{"sk-2"}, map[string]store.Capabilities{"m": {}})

	var got []string
	_, err := rt.ChatStream(context.Background(), "m", chatReq("m"), func(ev protocol.StreamEvent) error {
		got = append(got, ev.Type)
		return nil
	})
	if err == nil {
		t.Fatal("流中错误应返回")
	}
	if len(got) != 1 || got[0] != "text_delta" {
		t.Error("应只收到 1 个事件后中断，got", got)
	}
	if bad.reqs.Load() != 1 || good.reqs.Load() != 0 {
		t.Error("已 emit 后不应重试：坏渠道", bad.reqs.Load(), "好渠道", good.reqs.Load())
	}
}

// 流式 4xx：首事件前直接透传，不重试。
func TestChatStreamPassThrough4xx(t *testing.T) {
	st, rt := newTestEnv(t, "random")
	mock := newCountedServer(t, func(n int64, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error": {"message": "参数错误", "type": "invalid_request_error"}}`)
	})
	addChannel(t, st, "渠道", "openai", mock.srv.URL, []string{"sk-1"}, map[string]store.Capabilities{"m": {}})

	_, err := rt.ChatStream(context.Background(), "m", chatReq("m"), func(ev protocol.StreamEvent) error { return nil })
	var uperr *upstream.Error
	if !errors.As(err, &uperr) || uperr.StatusCode != http.StatusBadRequest {
		t.Fatalf("应透传 400，got %v", err)
	}
	if mock.reqs.Load() != 1 {
		t.Error("4xx 不应重试，got", mock.reqs.Load())
	}
}

// system 折叠：模型能力不支持 system 时，折叠进首条 user；支持时原样透传。
func TestChatSystemFold(t *testing.T) {
	ctx := context.Background()
	newReq := func() *protocol.ChatRequest {
		return &protocol.ChatRequest{
			Model: "m",
			Messages: []protocol.Message{
				{Role: "system", Content: []protocol.ContentPart{{Type: "text", Text: "你是助手"}}},
				{Role: "user", Content: []protocol.ContentPart{{Type: "text", Text: "你好"}}},
			},
		}
	}
	checkBody := func(t *testing.T, body string, wantSystem bool) {
		t.Helper()
		var sent struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal([]byte(body), &sent); err != nil {
			t.Fatal(err)
		}
		if wantSystem {
			// 支持 system：原样透传 system + user 两条
			if len(sent.Messages) != 2 || sent.Messages[0].Role != "system" {
				t.Errorf("支持 system 时应原样透传，got %d 条: %s", len(sent.Messages), body)
			}
		} else {
			// 不支持 system：折叠后只剩 1 条 user（前缀含 system 文本）
			if len(sent.Messages) != 1 || sent.Messages[0].Role != "user" {
				t.Errorf("不支持 system 时应折叠为 1 条 user，got %d 条: %s", len(sent.Messages), body)
			}
			if !strings.Contains(sent.Messages[0].Content, "你是助手") {
				t.Error("system 文本应折叠进首条 user:", sent.Messages[0].Content)
			}
		}
	}

	// 能力不支持 system → 折叠
	t.Run("不支持 system", func(t *testing.T) {
		st, rt := newTestEnv(t, "random")
		var got string
		mock := streamMock(t, streamSSE, func(b string) { got = b })
		addChannel(t, st, "渠道", "openai", mock.srv.URL, []string{"sk-1"},
			map[string]store.Capabilities{"m": {System: false, Tools: true, Vision: false, JSONMode: true}})
		if _, err := rt.ChatStream(ctx, "m", newReq(), func(ev protocol.StreamEvent) error { return nil }); err != nil {
			t.Fatal(err)
		}
		checkBody(t, got, false)
	})
	// 能力支持 system → 原样
	t.Run("支持 system", func(t *testing.T) {
		st, rt := newTestEnv(t, "random")
		var got string
		mock := streamMock(t, streamSSE, func(b string) { got = b })
		addChannel(t, st, "渠道", "openai", mock.srv.URL, []string{"sk-1"},
			map[string]store.Capabilities{"m": {System: true}})
		if _, err := rt.ChatStream(ctx, "m", newReq(), func(ev protocol.StreamEvent) error { return nil }); err != nil {
			t.Fatal(err)
		}
		checkBody(t, got, true)
	})
}

// 全局模型重定向：请求名 → 实际模型名后再路由。
func TestChatRedirect(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	chm := channel.NewManager(st, nil, 5*time.Second)
	rt := New(st, chm, "random", 5*time.Second, map[string]string{"deepseek-chat": "ds-main"})

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
	addChannel(t, st, "渠道", "openai", mock.srv.URL, []string{"sk-1"}, map[string]store.Capabilities{"ds-main": {}})

	// 请求 "deepseek-chat"（无此模型名）→ 重定向到 ds-main 路由成功
	res, err := rt.Chat(context.Background(), "deepseek-chat", chatReq("deepseek-chat"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Resp.Choices[0].Message.Content[0].Text != "ok" {
		t.Error("重定向后应成功", res.Resp)
	}
	if gotModel != "ds-main" {
		t.Error("上游应收到重定向后的模型名 ds-main，got", gotModel)
	}
	// 未命中重定向的模型名照常 404
	if _, err := rt.Chat(context.Background(), "gpt-4o", chatReq("gpt-4o")); !errors.Is(err, ErrModelNotFound) {
		t.Error("未重定向模型应 404，got", err)
	}
}

// ChatResult 携带命中渠道 ID（请求日志用）。
func TestChatResultChannelID(t *testing.T) {
	st, rt := newTestEnv(t, "random")
	mock := chatMock(t, "x")
	chID := addChannel(t, st, "渠道", "openai", mock.srv.URL, []string{"sk-1"}, map[string]store.Capabilities{"m": {}})
	res, err := rt.Chat(context.Background(), "m", chatReq("m"))
	if err != nil {
		t.Fatal(err)
	}
	if res.ChannelID != chID {
		t.Errorf("ChannelID 应为 %d，got %d", chID, res.ChannelID)
	}
	// 流式同样返回渠道 ID
	got, err := rt.ChatStream(context.Background(), "m", chatReq("m"), func(ev protocol.StreamEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if got != chID {
		t.Errorf("流式 ChannelID 应为 %d，got %d", chID, got)
	}
}
