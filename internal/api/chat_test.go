package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"api-gateway/internal/auth"
	"api-gateway/internal/store"
)

func newTestSrv(t *testing.T) (*Server, *store.Store, *auth.SessionManager) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	am, err := auth.NewSessionManager("test-secret")
	if err != nil {
		t.Fatal(err)
	}
	return New(s), s, am
}

func doChat(t *testing.T, am *auth.SessionManager, s *store.Store, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	h := am.APIAuth(s)(http.HandlerFunc(New(s).ChatCompletions))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestChatAuthChain(t *testing.T) {
	srv, st, am := newTestSrv(t)
	ctx := context.Background()

	uid, _ := st.CreateUser(ctx, "admin", "hash", "admin")
	plain := "test-token-abc"
	_, _ = st.CreateToken(ctx, uid, "t", auth.HashToken(plain), []string{"deepseek-chat"}, false)

	// 无令牌 → 401
	if rec := doChat(t, am, st, "", `{"model":"deepseek-chat"}`); rec.Code != http.StatusUnauthorized {
		t.Error("无令牌应 401，got", rec.Code)
	}

	// 无效令牌 → 401
	if rec := doChat(t, am, st, "wrong-token", `{"model":"deepseek-chat"}`); rec.Code != http.StatusUnauthorized {
		t.Error("无效令牌应 401，got", rec.Code)
	}

	// 白名单外模型 → 403 model not allowed，且不发上游请求
	rec := doChat(t, am, st, plain, `{"model":"gpt-4o"}`)
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

	// 白名单内模型 → 通过鉴权，返回 501（M3 前桩）
	if rec := doChat(t, am, st, plain, `{"model":"deepseek-chat"}`); rec.Code != http.StatusNotImplemented {
		t.Error("白名单内应 501，got", rec.Code)
	}

	// 禁用令牌 → 401
	tok, _ := st.GetTokenByHash(ctx, auth.HashToken(plain))
	_ = st.SetTokenEnabled(ctx, tok.ID, false)
	if rec := doChat(t, am, st, plain, `{"model":"deepseek-chat"}`); rec.Code != http.StatusUnauthorized {
		t.Error("禁用令牌应 401，got", rec.Code)
	}
	_ = srv
}
