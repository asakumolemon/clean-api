package channel

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
