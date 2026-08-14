// Package web 管理面：html/template + Tailwind（Play CDN），服务端渲染。
package web

import (
	"html/template"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"api-gateway/internal/auth"
	"api-gateway/internal/channel"
	"api-gateway/internal/router"
	"api-gateway/internal/store"
	webassets "api-gateway/web"
)

// Server 管理面 handler 集合。
type Server struct {
	store   *store.Store
	auth    *auth.SessionManager
	chm     *channel.Manager
	router  *router.Router
	version string
	tpl     *template.Template
}

// New 解析内嵌模板并构造管理面服务。version 展示在侧栏（-ldflags 注入）。
func New(s *store.Store, am *auth.SessionManager, chm *channel.Manager, rt *router.Router, version string) (*Server, error) {
	tpl, err := template.New("").Funcs(template.FuncMap{
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
	}).ParseFS(webassets.FS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	if version == "" {
		version = "dev"
	}
	return &Server{store: s, auth: am, chm: chm, router: rt, version: version, tpl: tpl}, nil
}

// Mount 注册 /admin 路由。
// 角色分级：任意已登录角色（含 user）可访问只读页（仪表盘/日志/测试台），
// 管理页（令牌/渠道/模型/用户/导入导出）仅 admin。
func (s *Server) Mount(r chi.Router) {
	r.Route("/admin", func(r chi.Router) {
		r.Get("/login", s.loginPage)
		r.Post("/login", s.login)
		r.Post("/logout", s.logout)

		// 只读页：任意登录角色（user 可访问）
		r.Group(func(r chi.Router) {
			r.Use(s.authOnly)
			r.Get("/", s.dashboard)
			r.Get("/logs", s.logsPage)
			r.Get("/playground", s.playgroundPage)
			r.Post("/playground/chat", s.playgroundChat)
		})

		// 管理页：仅 admin
		r.Group(func(r chi.Router) {
			r.Use(s.adminOnly)
			r.Get("/tokens", s.tokensPage)
			r.Post("/tokens", s.createToken)
			r.Post("/tokens/{id}/toggle", s.toggleToken)
			r.Post("/tokens/{id}/whitelist", s.updateTokenWhitelist)
			r.Post("/tokens/{id}/revoke", s.revokeToken)
			r.Get("/channels", s.channelsPage)
			r.Post("/channels", s.createChannel)
			r.Get("/channels/{id}/edit", s.channelEditPage)
			r.Post("/channels/{id}/edit", s.updateChannel)
			r.Post("/channels/{id}/toggle", s.toggleChannel)
			r.Post("/channels/{id}/resync", s.resyncChannel)
			r.Post("/channels/{id}/probe-caps", s.probeCapsChannel)
			r.Post("/channels/{id}/delete", s.deleteChannel)
			r.Get("/models", s.modelsPage)
			r.Post("/models/{id}/toggle", s.toggleModel)
			r.Post("/models/{id}/alias", s.setModelAlias)
			r.Post("/models/{id}/override", s.overrideModel)
			r.Get("/users", s.usersPage)
			r.Post("/users", s.createUser)
			r.Post("/users/{id}/role", s.setUserRole)
			r.Post("/users/{id}/password", s.resetUserPassword)
			r.Post("/users/{id}/delete", s.deleteUser)
			r.Get("/export", s.exportPage)
			r.Get("/export/download", s.exportConfig)
			r.Post("/import", s.importConfig)
		})
	})
}

// render 渲染指定模板。统一注入 Version 与当前用户 Role（侧栏按角色显示导航）。
func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data map[string]any) {
	data["Version"] = s.version // 侧栏版本展示
	if u := s.currentUser(w, r); u != nil {
		data["Role"] = u.Role
	} else {
		data["Role"] = ""
	}
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

// authOnly 管理面登录态校验（任意角色）：未登录跳登录页。user 角色只读页用。
func (s *Server) authOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.currentUser(w, r) == nil {
			http.Redirect(w, r, "/admin/login", http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// adminOnly 管理页权限校验：未登录跳登录页；已登录但非 admin 提示无权限并回仪表盘
// （user 可访问 /admin/，避免跳登录页造成「被登出」的错觉）。
func (s *Server) adminOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := s.currentUser(w, r)
		if user == nil {
			http.Redirect(w, r, "/admin/login", http.StatusFound)
			return
		}
		if user.Role != "admin" {
			s.setFlash(w, r, "无权限：该页面仅管理员可访问")
			http.Redirect(w, r, "/admin/", http.StatusFound)
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
