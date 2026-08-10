package channel

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"strings"
	"testing"
	"time"

	"api-gateway/internal/store"
)

// 全部 404 的上游：探测失败，错误信息应包含各协议证据（状态码）。
func TestDetectEvidenceOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "not found")
	}))
	defer srv.Close()

	m := NewManager(nil, nil, 5*time.Second)
	_, err := m.Detect(context.Background(), srv.URL, "sk-1")
	if err == nil {
		t.Fatal("应探测失败")
	}
	msg := err.Error()
	for _, want := range []string{"openai", "anthropic", "responses", "HTTP 404"} {
		if !strings.Contains(msg, want) {
			t.Errorf("探测失败信息应包含证据 %s：%s", want, msg)
		}
	}
	if !strings.Contains(msg, "手动指定类型") {
		t.Error("应提示手动指定类型重试")
	}
}

// 全部 404 且带响应体：证据应包含状态码与响应摘要。
func TestDetectEvidenceWithBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"route not found"}}`)
	}))
	defer srv.Close()

	m := NewManager(nil, nil, 5*time.Second)
	_, err := m.Detect(context.Background(), srv.URL, "sk-1")
	if err == nil {
		t.Fatal("应探测失败")
	}
	if !strings.Contains(err.Error(), "HTTP 404") || !strings.Contains(err.Error(), "route not found") {
		t.Errorf("证据应包含状态码与响应摘要：%s", err.Error())
	}
}

// 网络错误（连接被拒）：证据应包含错误原因。
func TestDetectEvidenceNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := srv.URL
	srv.Close() // 连接必然失败

	m := NewManager(nil, nil, time.Second)
	_, err := m.Detect(context.Background(), deadURL, "sk-1")
	if err == nil {
		t.Fatal("应探测失败")
	}
	if !strings.Contains(err.Error(), "请求失败") {
		t.Errorf("证据应包含网络错误：%s", err.Error())
	}
}

// 命中时无证据要求：OpenAI 正常返回模型列表 → 命中。
func TestDetectHitOpenAI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = io.WriteString(w, `{"data":[{"id":"m1"}]}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	m := NewManager(nil, nil, 5*time.Second)
	typ, err := m.Detect(context.Background(), srv.URL, "sk-1")
	if err != nil || typ != "openai" {
		t.Fatalf("应识别 openai，got %s err=%v", typ, err)
	}
}

// SetCooldown：MarkKeyFailed 的冷却时长应随配置生效。
func TestSetCooldown(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	chID, err := st.CreateChannel(ctx, "渠道", "openai", "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	keyID, err := st.AddChannelKey(ctx, chID, "sk-1")
	if err != nil {
		t.Fatal(err)
	}

	m := NewManager(st, nil, 5*time.Second)
	m.SetCooldown(3 * time.Second)
	m.MarkKeyFailed(ctx, keyID)

	keys, _ := st.ListChannelKeys(ctx, chID)
	if !keys[0].CooldownUntil.Valid {
		t.Fatal("key 应被标记冷却")
	}
	left := time.Until(keys[0].CooldownUntil.Time)
	if left < 2*time.Second || left > 4*time.Second {
		t.Errorf("冷却时长应为 3s，got %v", left)
	}
	// 不调用 SetCooldown 时用默认 60s
	m2 := NewManager(st, nil, 5*time.Second)
	m2.MarkKeyFailed(ctx, keyID)
	keys, _ = st.ListChannelKeys(ctx, chID)
	if left := time.Until(keys[0].CooldownUntil.Time); left < 59*time.Second {
		t.Errorf("默认冷却应为 60s，got %v", left)
	}
}

// 能力探测默认关闭：拉完模型直接入库，能力为保守默认值（system/tools 开，vision/json 关），
// 且不向上游发能力探测请求。
func TestProbeChannelDefaultCapabilities(t *testing.T) {
	// fakeUpstream 只提供 /v1/models；若发探测请求会命中 /v1/chat/completions 并计数
	var chatHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"model-a"},{"id":"model-b"}]}`)
		case "/v1/chat/completions":
			chatHits.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"choices":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	m, st, _ := newTestManager(t, nil)
	ctx := context.Background()
	cid, _ := st.CreateChannel(ctx, "fake", "openai", srv.URL)
	enc, _ := m.enc.Encrypt("sk-1")
	_, _ = st.AddChannelKey(ctx, cid, enc)

	if err := m.ProbeChannel(ctx, cid); err != nil {
		t.Fatal(err)
	}
	models, _ := st.ListModelsByChannel(ctx, cid)
	if len(models) != 2 {
		t.Fatalf("应同步 2 个模型，got %d", len(models))
	}
	caps := models[0].Capabilities
	if !caps.System || !caps.Tools {
		t.Error("默认能力应支持 system/tools，got", caps)
	}
	if caps.Vision || caps.JSONMode {
		t.Error("默认能力应关闭 vision/json_mode，got", caps)
	}
	if chatHits.Load() != 0 {
		t.Errorf("默认不应发能力探测请求，got %d 次", chatHits.Load())
	}
	// 进度消息应提示默认值
	if st2 := m.GetStatus(cid); st2 == nil || !strings.Contains(st2.Message, "默认值") {
		t.Error("进度消息应提示能力使用默认值，got", st2)
	}
}

// 开启 probe_capabilities 后走逐个探测（上游收到最小试调用）。
func TestProbeChannelWithProbing(t *testing.T) {
	var chatHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"model-a"}]}`)
		case "/v1/chat/completions":
			chatHits.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"choices":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	m, st, _ := newTestManager(t, nil)
	m.SetProbeCapabilities(true)
	ctx := context.Background()
	cid, _ := st.CreateChannel(ctx, "fake", "openai", srv.URL)
	enc, _ := m.enc.Encrypt("sk-1")
	_, _ = st.AddChannelKey(ctx, cid, enc)

	if err := m.ProbeChannel(ctx, cid); err != nil {
		t.Fatal(err)
	}
	if chatHits.Load() == 0 {
		t.Error("开启探测后应发能力探测请求")
	}
}

// 手动能力探测：只对库中已有模型探测并更新，不重新拉模型列表。
func TestProbeCapabilitiesOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"choices":[]}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	m, st, _ := newTestManager(t, nil)
	ctx := context.Background()
	cid, _ := st.CreateChannel(ctx, "fake", "openai", srv.URL)
	enc, _ := m.enc.Encrypt("sk-1")
	_, _ = st.AddChannelKey(ctx, cid, enc)
	// 库中已有模型（默认能力：vision 关）
	_, _ = st.SyncModels(ctx, cid, map[string]store.Capabilities{
		"model-a": {System: true, Tools: true},
	}, time.Now().UTC())

	if err := m.ProbeCapabilitiesOnly(ctx, cid); err != nil {
		t.Fatal(err)
	}
	models, _ := st.ListModelsByChannel(ctx, cid)
	if len(models) != 1 {
		t.Fatalf("模型数应不变，got %d", len(models))
	}
	if st2 := m.GetStatus(cid); st2 == nil || !st2.Done || st2.Failed || !strings.Contains(st2.Message, "能力探测完成") {
		t.Error("手动探测状态应完成，got", st2)
	}
}
