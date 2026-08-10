package web

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"api-gateway/internal/auth"
)

// loginPage GET /admin/login
func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.currentUser(w, r) != nil {
		http.Redirect(w, r, "/admin/", http.StatusFound)
		return
	}
	s.render(w, "login.html", baseData("登录 · 智能 API 网关", "", map[string]any{
		"Flash": s.readFlash(w, r),
	}))
}

// login POST /admin/login
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	username := r.FormValue("username")
	password := r.FormValue("password")
	user, err := s.store.GetUserByUsername(r.Context(), username)
	if err != nil {
		// 用户不存在也走一次 bcrypt 比较，避免用户枚举的时间差异
		auth.CheckPassword("$2a$10$C6UzMDM.H6dfI/f/IKcEeO5h1l0Z8h0z0x0x0x0x0x0x0x0x0x0O", password)
		s.setFlash(w, r, "用户名或密码错误")
		http.Redirect(w, r, "/admin/login", http.StatusFound)
		return
	}
	if !auth.CheckPassword(user.PasswordHash, password) {
		s.setFlash(w, r, "用户名或密码错误")
		http.Redirect(w, r, "/admin/login", http.StatusFound)
		return
	}
	sess, err := s.auth.Get(r)
	if err != nil {
		http.Error(w, "会话创建失败", http.StatusInternalServerError)
		return
	}
	sess.Values["user_id"] = user.ID
	sess.Values["role"] = user.Role
	if err := sess.Save(r, w); err != nil {
		http.Error(w, "会话保存失败", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/", http.StatusFound)
}

// logout POST /admin/logout
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	sess, err := s.auth.Get(r)
	if err == nil {
		sess.Options.MaxAge = -1
		_ = sess.Save(r, w)
	}
	http.Redirect(w, r, "/admin/login", http.StatusFound)
}

// dashboard GET /admin/
func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	users, _ := s.store.CountUsers(ctx)
	tokens, _ := s.store.CountTokens(ctx)
	channels, _ := s.store.CountChannels(ctx)
	models, _ := s.store.CountModels(ctx)
	s.render(w, "dashboard.html", baseData("仪表盘 · 智能 API 网关", "dashboard", map[string]any{
		"Flash":    s.readFlash(w, r),
		"Users":    users,
		"Tokens":   tokens,
		"Channels": channels,
		"Models":   models,
	}))
}

// tokensPage GET /admin/tokens
func (s *Server) tokensPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tokens, _ := s.store.ListTokens(ctx)
	s.render(w, "tokens.html", baseData("令牌管理 · 智能 API 网关", "tokens", map[string]any{
		"Flash":  s.readFlash(w, r),
		"Tokens": tokens,
	}))
}

// createToken POST /admin/tokens
func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	models := splitModels(r.FormValue("models"))
	allowAll := r.FormValue("allow_all") == "on"

	if name == "" {
		s.setFlash(w, r, "请填写令牌名称")
		http.Redirect(w, r, "/admin/tokens", http.StatusFound)
		return
	}
	if !allowAll && len(models) == 0 {
		s.setFlash(w, r, "必须指定至少一个模型；如需放行全部模型请显式勾选「允许全部模型」")
		http.Redirect(w, r, "/admin/tokens", http.StatusFound)
		return
	}

	plain, err := auth.GenerateToken()
	if err != nil {
		s.setFlash(w, r, "生成令牌失败: "+err.Error())
		http.Redirect(w, r, "/admin/tokens", http.StatusFound)
		return
	}
	user := s.currentUser(w, r)
	if _, err := s.store.CreateToken(r.Context(), user.ID, name, auth.HashToken(plain), models, allowAll); err != nil {
		s.setFlash(w, r, "保存令牌失败: "+err.Error())
		http.Redirect(w, r, "/admin/tokens", http.StatusFound)
		return
	}

	ctx := r.Context()
	tokens, _ := s.store.ListTokens(ctx)
	s.render(w, "tokens.html", baseData("令牌管理 · 智能 API 网关", "tokens", map[string]any{
		"Tokens":   tokens,
		"NewToken": plain,
	}))
}

// toggleToken POST /admin/tokens/{id}/toggle
func (s *Server) toggleToken(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	tok, err := s.store.GetTokenByID(r.Context(), id)
	if err != nil {
		s.setFlash(w, r, "令牌不存在")
		http.Redirect(w, r, "/admin/tokens", http.StatusFound)
		return
	}
	if err := s.store.SetTokenEnabled(r.Context(), id, !tok.Enabled); err != nil {
		s.setFlash(w, r, "操作失败: "+err.Error())
	}
	http.Redirect(w, r, "/admin/tokens", http.StatusFound)
}

// revokeToken POST /admin/tokens/{id}/revoke
func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err := s.store.DeleteToken(r.Context(), id); err != nil {
		s.setFlash(w, r, "吊销失败: "+err.Error())
	}
	http.Redirect(w, r, "/admin/tokens", http.StatusFound)
}
