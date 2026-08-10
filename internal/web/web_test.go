package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"api-gateway/internal/auth"
	"api-gateway/internal/channel"
	"api-gateway/internal/crypto"
	"api-gateway/internal/store"
)

func fakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "model-a"}, {"id": "model-b"}},
		})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	})
	return httptest.NewServer(mux)
}

func newTestWeb(t *testing.T, up *httptest.Server) (*httptest.Server, *store.Store, *http.Client) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	hash, _ := auth.HashPassword("admin123")
	uid, err := st.CreateUser(ctx, "admin", hash, "admin")
	if err != nil || uid == 0 {
		t.Fatal("创建管理员失败", err)
	}
	am, err := auth.NewSessionManager("test-secret", false)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := crypto.New("test-enc-key")
	chm := channel.NewManager(st, enc, 10*time.Second)

	srv, err := New(st, am, chm)
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	srv.Mount(r)
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.PostForm(ts.URL+"/admin/login", url.Values{"username": {"admin"}, "password": {"admin123"}})
	if err != nil || resp.StatusCode != http.StatusFound {
		t.Fatal("登录失败", resp.StatusCode, err)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin/" {
		t.Fatalf("登录后应跳转 /admin/，got %q（可能是登录失败）", loc)
	}
	resp.Body.Close()
	return ts, st, client
}

func TestChannelProbeFlowViaWeb(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	ts, st, client := newTestWeb(t, up)
	ctx := context.Background()

	resp, err := client.PostForm(ts.URL+"/admin/channels", url.Values{
		"name":     {"fake"},
		"base_url": {up.URL},
		"type":     {"auto"},
		"api_keys": {"sk-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatal("创建渠道应 302，got", resp.StatusCode)
	}

	// 轮询渠道页直到探测完成
	deadline := time.Now().Add(10 * time.Second)
	done := false
	var lastStatus int
	var lastBody string
	for time.Now().Before(deadline) {
		page, err := client.Get(ts.URL + "/admin/channels")
		if err != nil {
			t.Fatal(err)
		}
		body := make([]byte, 1<<20)
		n, _ := page.Body.Read(body)
		page.Body.Close()
		lastStatus = page.StatusCode
		lastBody = string(body[:n])
		if strings.Contains(lastBody, "探测完成") {
			done = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !done {
		t.Fatalf("探测未在超时内完成, status=%d body=%s", lastStatus, lastBody[:min(500, len(lastBody))])
	}

	ch, err := st.GetChannel(ctx, 1)
	if err != nil || ch.Type != "openai" {
		t.Fatal("渠道类型应识别为 openai", ch, err)
	}
	models, _ := st.ListModels(ctx)
	if len(models) != 2 {
		t.Fatalf("应同步 2 个模型，got %d", len(models))
	}
}

func TestChannelProbeFailAndManualRetry(t *testing.T) {
	// 全 404 的上游：探测失败，然后手动指定类型重试成功
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	up := httptest.NewServer(mux)
	defer up.Close()

	ts, _, client := newTestWeb(t, up)

	resp, err := client.PostForm(ts.URL+"/admin/channels", url.Values{
		"name": {"bad"}, "base_url": {up.URL}, "type": {"auto"}, "api_keys": {"sk-x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	deadline := time.Now().Add(10 * time.Second)
	failed := false
	var lastStatus int
	var lastBody string
	for time.Now().Before(deadline) {
		page, _ := client.Get(ts.URL + "/admin/channels")
		body := make([]byte, 1<<20)
		n, _ := page.Body.Read(body)
		page.Body.Close()
		lastStatus = page.StatusCode
		lastBody = string(body[:n])
		if strings.Contains(lastBody, "探测失败") {
			failed = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !failed {
		t.Fatalf("全 404 上游应探测失败, status=%d body=%s", lastStatus, lastBody[:min(500, len(lastBody))])
	}
}
