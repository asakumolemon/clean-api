// Package web 管理面：html/template + Pico.css，服务端渲染。
package web

import (
	"html/template"
	"io/fs"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"api-gateway/internal/auth"
	"api-gateway/internal/store"
	webassets "api-gateway/web"
)

// Server 管理面 handler 集合。
type Server struct {
	store *store.Store
	auth  *auth.SessionManager
	tpl   *template.Template
}

// New 解析内嵌模板并构造管理面服务。
func New(s *store.Store, am *auth.SessionManager) (*Server, error) {
	tpl, err := template.ParseFS(webassets.FS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{store: s, auth: am, tpl: tpl}, nil
}

// Mount 注册 /admin 路由。
func (s *Server) Mount(r chi.Router) {
	r.Route("/admin", func(r chi.Router) {
		r.Get("/login", s.loginPage)
		r.Post("/login", s.login)
		r.Post("/logout", s.logout)
		r.Handle("/static/*", s.static())

		r.Group(func(r chi.Router) {
			r.Use(s.adminOnly)
			r.Get("/", s.dashboard)
			r.Get("/tokens", s.tokensPage)
			r.Post("/tokens", s.createToken)
			r.Post("/tokens/{id}/toggle", s.toggleToken)
			r.Post("/tokens/{id}/revoke", s.revokeToken)
		})
	})
}

func (s *Server) static() http.Handler {
	sub, err := fs.Sub(webassets.FS, "static")
	if err != nil {
		panic(err)
	}
	return http.StripPrefix("/admin/static/", http.FileServer(http.FS(sub)))
}

// render 渲染指定模板。
func (s *Server) render(w http.ResponseWriter, name string, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "模板渲染失败: "+err.Error(), http.StatusInternalServerError)
	}
}

// baseData 拼装模板公共字段。
func baseData(title, active string, extra map[string]any) map[string]any {
	m := map[string]any{"Title": title, "Active": active}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// setFlash 写入一条一次性提示信息。
func (s *Server) setFlash(w http.ResponseWriter, r *http.Request, msg string) {
	sess, err := s.auth.Get(r)
	if err != nil {
		return
	}
	sess.AddFlash(msg)
	_ = sess.Save(r, w)
}

// readFlash 读取并清除一次性提示信息。
func (s *Server) readFlash(w http.ResponseWriter, r *http.Request) string {
	sess, err := s.auth.Get(r)
	if err != nil {
		return ""
	}
	flashes := sess.Flashes()
	if len(flashes) == 0 {
		return ""
	}
	_ = sess.Save(r, w)
	if m, ok := flashes[0].(string); ok {
		return m
	}
	return ""
}

// adminOnly 管理面登录态校验：未登录/非 admin 一律跳登录页。
func (s *Server) adminOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := s.currentUser(w, r)
		if user == nil || user.Role != "admin" {
			http.Redirect(w, r, "/admin/login", http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// currentUser 从会话取当前用户，无效返回 nil。
func (s *Server) currentUser(w http.ResponseWriter, r *http.Request) *store.User {
	sess, err := s.auth.Get(r)
	if err != nil {
		return nil
	}
	id, ok := sess.Values["user_id"].(int64)
	if !ok {
		return nil
	}
	user, err := s.store.GetUserByID(r.Context(), id)
	if err != nil {
		return nil
	}
	return user
}

func splitModels(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if m := strings.TrimSpace(part); m != "" {
			out = append(out, m)
		}
	}
	return out
}
