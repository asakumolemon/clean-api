// Package api 对外 HTTP 入口（/v1/*）。M1 阶段仅为鉴权桩。
package api

import (
	"encoding/json"
	"io"
	"net/http"

	"api-gateway/internal/auth"
	"api-gateway/internal/store"
)

// Server 对外 /v1 API。
type Server struct {
	store *store.Store
}

func New(s *store.Store) *Server {
	return &Server{store: s}
}

// ChatCompletions /v1/chat/completions 桩：走完鉴权 + 模型白名单校验后，
// 返回 501（协议转换在 M3 实现）。
func (s *Server) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	tok := auth.TokenFromContext(r.Context())
	if tok == nil {
		auth.WriteAPIError(w, http.StatusUnauthorized, "unauthorized", "缺少鉴权上下文")
		return
	}

	var req struct {
		Model string `json:"model"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		auth.WriteAPIError(w, http.StatusBadRequest, "invalid_request", "读取请求体失败")
		return
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			auth.WriteAPIError(w, http.StatusBadRequest, "invalid_request", "请求体 JSON 解析失败")
			return
		}
	}
	if req.Model == "" {
		auth.WriteAPIError(w, http.StatusBadRequest, "invalid_request", "缺少 model 字段")
		return
	}

	if !auth.CheckModelAllowed(tok, req.Model) {
		auth.WriteAPIError(w, http.StatusForbidden, "model_not_allowed", "model not allowed")
		return
	}

	auth.WriteAPIError(w, http.StatusNotImplemented, "not_implemented", "协议转换尚未实现（M3）")
}
