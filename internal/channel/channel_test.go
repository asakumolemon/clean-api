package channel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"api-gateway/internal/crypto"
	"api-gateway/internal/store"
)

func newTestManager(t *testing.T, srv *httptest.Server) (*Manager, *store.Store, *httptest.Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	enc, _ := crypto.New("test-enc-key")
	m := NewManager(st, enc, 10*time.Second)
	return m, st, srv
}

// fakeUpstream 模拟三种协议的上游。
func fakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	// OpenAI 兼容
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-123" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "model-a"},
				{"id": "model-b"},
			},
		})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	})
	return httptest.NewServer(mux)
}

func TestDetectOpenAI(t *testing.T) {
	srv := fakeUpstream(t)
	defer srv.Close()
	m, _, _ := newTestManager(t, nil)
	typ, err := m.Detect(context.Background(), srv.URL, "sk-123")
	if err != nil {
		t.Fatal(err)
	}
	if typ != "openai" {
		t.Error("应识别为 openai，got", typ)
	}
}

func TestDetectAnthropic(t *testing.T) {
	mux := http.NewServeMux()
	var called bool
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Header.Get("x-api-key") != "sk-ant-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("anthropic-version") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	m, _, _ := newTestManager(t, nil)
	typ, err := m.Detect(context.Background(), srv.URL, "sk-ant-1")
	if err != nil {
		t.Fatal(err)
	}
	if typ != "anthropic" || !called {
		t.Error("应识别为 anthropic", typ, called)
	}
}

func TestDetectFailed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	m, _, _ := newTestManager(t, nil)
	if _, err := m.Detect(context.Background(), srv.URL, "sk-x"); err == nil {
		t.Error("全 404 应探测失败")
	}
}

func TestSyncModels(t *testing.T) {
	srv := fakeUpstream(t)
	defer srv.Close()
	m, _, _ := newTestManager(t, nil)
	names, err := m.SyncModels(context.Background(), "openai", srv.URL, "sk-123")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "model-a" {
		t.Error("模型列表不正确", names)
	}
}

func TestProbeChannelFull(t *testing.T) {
	srv := fakeUpstream(t)
	defer srv.Close()
	m, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	cid, err := st.CreateChannel(ctx, "fake", "auto", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := m.enc.Encrypt("sk-123")
	_, _ = st.AddChannelKey(ctx, cid, enc)

	if err := m.ProbeChannel(ctx, cid); err != nil {
		t.Fatal(err)
	}

	ch, _ := st.GetChannel(ctx, cid)
	if ch.Type != "openai" {
		t.Error("渠道类型应更新为 openai，got", ch.Type)
	}
	models, _ := st.ListModelsByChannel(ctx, cid)
	if len(models) != 2 {
		t.Fatalf("应同步 2 个模型，got %d", len(models))
	}
	if !models[0].Capabilities.System {
		t.Error("能力探测应识别 system 支持")
	}
	st2 := m.GetStatus(cid)
	if st2 == nil || !st2.Done || st2.Failed {
		t.Error("探测状态应标记完成", st2)
	}
}

func TestSelectKeyRotationAndCooldown(t *testing.T) {
	m, st, _ := newTestManager(t, nil)
	ctx := context.Background()
	cid, _ := st.CreateChannel(ctx, "c", "openai", "https://x.com")
	enc, _ := m.enc.Encrypt("sk-1")
	k1, _ := st.AddChannelKey(ctx, cid, enc)
	enc2, _ := m.enc.Encrypt("sk-2")
	_, _ = st.AddChannelKey(ctx, cid, enc2)

	// 用 round_robin 保证短抽样内覆盖全部 key（random 短抽样可能随机不到某个 key，造成 flaky）。
	if err := st.UpdateChannel(ctx, cid, "c", "openai", "https://x.com", "active", 1, "round_robin"); err != nil {
		t.Fatal(err)
	}
	ch, _ := st.GetChannel(ctx, cid)
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		plain, _, err := m.SelectKey(ctx, ch)
		if err != nil {
			t.Fatal(err)
		}
		seen[plain] = true
	}
	if !seen["sk-1"] || !seen["sk-2"] {
		t.Error("随机轮换应能选到所有 key", seen)
	}

	// 全部冷却后报错
	future := time.Now().Add(time.Hour)
	_ = st.SetKeyCooldown(ctx, k1, &future)
	keys, _ := st.ListChannelKeys(ctx, cid)
	for _, k := range keys {
		f := time.Now().Add(time.Hour)
		_ = st.SetKeyCooldown(ctx, k.ID, &f)
	}
	if _, _, err := m.SelectKey(ctx, ch); err == nil {
		t.Error("全部冷却时应返回错误")
	}

	// 单个冷却被跳过
	_ = st.SetKeyCooldown(ctx, k1, nil)
	keys, _ = st.ListChannelKeys(ctx, cid)
	for _, k := range keys {
		if k.ID != k1 {
			f := time.Now().Add(time.Hour)
			_ = st.SetKeyCooldown(ctx, k.ID, &f)
		}
	}
	plain, _, err := m.SelectKey(ctx, ch)
	if err != nil || plain != "sk-1" {
		t.Error("应跳过冷却 key 选到 sk-1，got", plain, err)
	}
}
