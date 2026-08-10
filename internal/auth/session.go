package auth

import (
	"net/http"

	"github.com/gorilla/sessions"
)

// SessionName 管理面登录会话名。
const SessionName = "gateway_admin"

// SessionManager 封装签名 cookie session 的读写。
type SessionManager struct {
	store *sessions.CookieStore
}

// NewSessionManager 创建会话管理器。secret 为空时自动生成随机密钥
//（此时重启服务后所有登录态失效）。
func NewSessionManager(secret string) (*SessionManager, error) {
	if secret == "" {
		gen, err := GenerateToken()
		if err != nil {
			return nil, err
		}
		secret = gen
	}
	cs := sessions.NewCookieStore([]byte(secret))
	cs.Options.HttpOnly = true
	cs.Options.SameSite = http.SameSiteLaxMode
	return &SessionManager{store: cs}, nil
}

// Get 读取会话。
func (m *SessionManager) Get(r *http.Request) (*sessions.Session, error) {
	return m.store.Get(r, SessionName)
}
