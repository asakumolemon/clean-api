package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// panic 应被恢复：返回 500 JSON 而非崩溃。
func TestRecoverMW(t *testing.T) {
	h := recoverMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panic 应返回 500，got %d", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	errObj, ok := resp["error"].(map[string]any)
	if !ok || errObj["type"] != "internal_error" {
		t.Error("错误响应应为 OpenAI 格式:", rec.Body.String())
	}
}

// 正常请求不受影响。
func TestRecoverMWPassThrough(t *testing.T) {
	h := recoverMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatal("正常请求应原样通过")
	}
}
