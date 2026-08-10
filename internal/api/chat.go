// Package api 对外 HTTP 入口（/v1/*）。M3 起：鉴权 → IR 解析 → 路由分发 → 上游调用。
package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"api-gateway/internal/auth"
	"api-gateway/internal/protocol"
	"api-gateway/internal/router"
	"api-gateway/internal/store"
	"api-gateway/internal/upstream"
)

// Server 对外 /v1 API。
type Server struct {
	store  *store.Store
	router *router.Router
}

func New(s *store.Store, r *router.Router) *Server {
	return &Server{store: s, router: r}
}

// ChatCompletions /v1/chat/completions：鉴权 → OpenAI 请求体解析为 IR →
// 模型白名单校验 → 路由分发到上游 → IR 序列化为 OpenAI 响应。
func (s *Server) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	tok := auth.TokenFromContext(r.Context())
	if tok == nil {
		auth.WriteAPIError(w, http.StatusUnauthorized, "unauthorized", "缺少鉴权上下文")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		auth.WriteAPIError(w, http.StatusBadRequest, "invalid_request", "读取请求体失败")
		return
	}
	if len(body) == 0 {
		auth.WriteAPIError(w, http.StatusBadRequest, "invalid_request", "请求体不能为空")
		return
	}
	req, err := protocol.ParseOpenAIChatRequest(body)
	if err != nil {
		auth.WriteAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Stream {
		auth.WriteAPIError(w, http.StatusNotImplemented, "not_implemented", "流式输出尚未支持（M4 实现），请关闭 stream")
		return
	}
	if !auth.CheckModelAllowed(tok, req.Model) {
		auth.WriteAPIError(w, http.StatusForbidden, "model_not_allowed", "model not allowed")
		return
	}

	resp, err := s.router.Chat(r.Context(), req.Model, req)
	if err != nil {
		s.writeChatError(w, err, req.Model)
		return
	}

	out, err := protocol.SerializeOpenAIChatResponse(resp)
	if err != nil {
		auth.WriteAPIError(w, http.StatusInternalServerError, "internal_error", "序列化响应失败: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// writeChatError 路由错误 → OpenAI 格式错误响应：
// 模型无路由 → 404；上游错误 → 透传状态码与信息；其余 → 500。
func (s *Server) writeChatError(w http.ResponseWriter, err error, model string) {
	if errors.Is(err, router.ErrModelNotFound) {
		auth.WriteAPIError(w, http.StatusNotFound, "model_not_found", "model not found: "+model)
		return
	}
	var uperr *upstream.Error
	if errors.As(err, &uperr) {
		status := uperr.StatusCode
		if status <= 0 {
			status = http.StatusBadGateway // 网络层错误（无 HTTP 响应）→ 502
		}
		auth.WriteAPIError(w, status, uperr.Type, uperr.Message)
		return
	}
	auth.WriteAPIError(w, http.StatusInternalServerError, "internal_error", err.Error())
}

// Models GET /v1/models：返回启用模型的对外名列表（alias 非空用 alias），按名去重。
func (s *Server) Models(w http.ResponseWriter, r *http.Request) {
	models, err := s.store.ListModels(r.Context())
	if err != nil {
		auth.WriteAPIError(w, http.StatusInternalServerError, "internal_error", "查询模型失败: "+err.Error())
		return
	}
	seen := map[string]bool{}
	data := []map[string]any{}
	for _, m := range models {
		if !m.Enabled {
			continue
		}
		name := m.Name
		if m.Alias != "" {
			name = m.Alias
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		data = append(data, map[string]any{"id": name, "object": "model", "owned_by": "gateway"})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}
