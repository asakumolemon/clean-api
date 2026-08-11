package channel

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"api-gateway/internal/store"
)

// flipServer 状态可切换的模拟上游（fail=true 时返回 500）。
func flipServer(t *testing.T) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &fail
}

func newHealthEnv(t *testing.T, up *httptest.Server, chType string) (*store.Store, *HealthChecker, int64) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	chID, err := st.CreateChannel(ctx, "渠道", chType, up.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddChannelKey(ctx, chID, "sk-1"); err != nil {
		t.Fatal(err)
	}
	chm := NewManager(st, nil, 5*time.Second)
	hc := NewHealthChecker(st, chm, 5*time.Second, 3)
	return st, hc, chID
}

// 连续失败 3 次 → down；恢复后 1 次成功 → active。
func TestHealthCheckMarkDownAndRecover(t *testing.T) {
	up, fail := flipServer(t)
	st, hc, chID := newHealthEnv(t, up, "openai")
	ctx := context.Background()

	// 失败 2 次：仍 active
	fail.Store(true)
	hc.CheckOnce(ctx)
	hc.CheckOnce(ctx)
	ch, _ := st.GetChannel(ctx, chID)
	if ch.Status != "active" {
		t.Fatalf("失败 2 次不应 down，got %s", ch.Status)
	}
	// 第 3 次：down
	hc.CheckOnce(ctx)
	ch, _ = st.GetChannel(ctx, chID)
	if ch.Status != "down" {
		t.Fatalf("连续失败 3 次应 down，got %s", ch.Status)
	}
	// 恢复：1 次成功即回 active
	fail.Store(false)
	hc.CheckOnce(ctx)
	ch, _ = st.GetChannel(ctx, chID)
	if ch.Status != "active" {
		t.Fatalf("恢复后应回 active，got %s", ch.Status)
	}
}

// disabled 与未识别类型（auto）渠道跳过检查。
func TestHealthCheckSkipsInactive(t *testing.T) {
	up, fail := flipServer(t)
	fail.Store(true)
	st, hc, chID := newHealthEnv(t, up, "openai")
	ctx := context.Background()

	// disabled 渠道：失败状态不累计、不标记
	if err := st.SetChannelStatus(ctx, chID, "disabled"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		hc.CheckOnce(ctx)
	}
	ch, _ := st.GetChannel(ctx, chID)
	if ch.Status != "disabled" {
		t.Error("disabled 渠道不应被改写，got", ch.Status)
	}
	// auto 类型渠道同样跳过
	st2, hc2, chID2 := newHealthEnv(t, up, "auto")
	hc2.CheckOnce(ctx)
	ch2, _ := st2.GetChannel(ctx, chID2)
	if ch2.Status != "active" {
		t.Error("auto 类型不应被检查/标记，got", ch2.Status)
	}
}

// 渠道无 key：视为不健康（连续失败累计）。
func TestHealthCheckNoKey(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	chID, _ := st.CreateChannel(ctx, "无 key 渠道", "openai", "https://example.com")
	chm := NewManager(st, nil, 5*time.Second)
	hc := NewHealthChecker(st, chm, 5*time.Second, 2)
	for i := 0; i < 2; i++ {
		hc.CheckOnce(ctx)
	}
	ch, _ := st.GetChannel(ctx, chID)
	if ch.Status != "down" {
		t.Errorf("无 key 渠道连续失败应 down，got %s", ch.Status)
	}
}

// anthropic 渠道健康检查：发 POST /v1/messages（x-api-key 头 + 渠道内启用模型名）。
func TestHealthCheckAnthropicChannel(t *testing.T) {
	var gotMethod, gotPath, gotKey string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotKey = r.Method, r.URL.Path, r.Header.Get("x-api-key")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message"}`)
	}))
	t.Cleanup(srv.Close)

	st, hc, chID := newHealthEnv(t, srv, "anthropic")
	ctx := context.Background()
	// 同步一个启用模型，anthropic ping 需要真实模型名
	if _, err := st.SyncModels(ctx, chID, map[string]store.Capabilities{"claude-sonnet-4-20250514": {}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	hc.CheckOnce(ctx)
	if gotMethod != http.MethodPost || gotPath != "/v1/messages" {
		t.Errorf("anthropic ping 应 POST /v1/messages，got %s %s", gotMethod, gotPath)
	}
	if gotKey != "sk-1" {
		t.Error("anthropic ping 应带 x-api-key 头，got", gotKey)
	}
	if !strings.Contains(gotBody, "claude-sonnet-4-20250514") || !strings.Contains(gotBody, "max_tokens") {
		t.Errorf("anthropic ping 请求体应含模型名与 max_tokens：%s", gotBody)
	}
	// 2xx → 渠道保持 active
	ch, _ := st.GetChannel(ctx, chID)
	if ch.Status != "active" {
		t.Errorf("anthropic 健康渠道应保持 active，got %s", ch.Status)
	}
}
