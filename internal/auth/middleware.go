package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"api-gateway/internal/store"
)

type ctxKey int

const (
	ctxKeyToken ctxKey = iota
	ctxKeyUser
)

// TokenFromContext 从请求上下文取鉴权令牌记录。
func TokenFromContext(ctx context.Context) *store.Token {
	t, _ := ctx.Value(ctxKeyToken).(*store.Token)
	return t
}

// UserFromContext 从请求上下文取用户记录。
func UserFromContext(ctx context.Context) *store.User {
	u, _ := ctx.Value(ctxKeyUser).(*store.User)
	return u
}

// CheckModelAllowed 判断令牌是否允许访问目标模型：
// 显式勾选「允许全部模型」才全放行，否则必须在白名单内。
func CheckModelAllowed(t *store.Token, model string) bool {
	if t == nil || model == "" {
		return false
	}
	if t.AllowAll {
		return true
	}
	for _, m := range t.ModelWhitelist {
		if m == model {
			return true
		}
	}
	return false
}

// APIAuth 对外 /v1 接口的令牌鉴权中间件。
// 兼容两种取令牌方式：OpenAI 系客户端的 Authorization: Bearer，
// 以及 Anthropic 系客户端（Claude Code / Cherry Studio 等）的 x-api-key 头。
// 流程：取令牌 → 查哈希 → 存在且启用 → 放行并注入上下文；否则 401。
// 模型白名单校验在具体 handler 内做（需要解析请求体里的 model）。
func (m *SessionManager) APIAuth(s *store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := ""
			if hdr := r.Header.Get("Authorization"); strings.HasPrefix(hdr, "Bearer ") {
				raw = strings.TrimPrefix(hdr, "Bearer ")
			} else {
				raw = r.Header.Get("X-Api-Key")
			}
			if raw == "" {
				WriteAPIError(w, http.StatusUnauthorized, "unauthorized", "缺少 Bearer 令牌或 x-api-key")
				return
			}
			tok, err := s.GetTokenByHash(r.Context(), HashToken(raw))
			if err != nil {
				WriteAPIError(w, http.StatusUnauthorized, "unauthorized", "令牌无效")
				return
			}
			if !tok.Enabled {
				WriteAPIError(w, http.StatusUnauthorized, "token_disabled", "令牌已被禁用")
				return
			}
			user, err := s.GetUserByID(r.Context(), tok.UserID)
			if err != nil {
				WriteAPIError(w, http.StatusUnauthorized, "unauthorized", "用户不存在")
				return
			}
			_ = s.TouchToken(r.Context(), tok.ID)
			ctx := context.WithValue(r.Context(), ctxKeyToken, tok)
			ctx = context.WithValue(ctx, ctxKeyUser, user)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// WriteAPIError 以 OpenAI 兼容格式写错误响应。
func WriteAPIError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    errType,
		},
	})
}
