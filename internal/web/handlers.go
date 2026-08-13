package web

import (
	"context"
	"net/http"
	"sort"
	"strconv"

	"github.com/go-chi/chi/v5"

	"api-gateway/internal/auth"
	"api-gateway/internal/store"
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
	// Get 失败（如重启/多实例后签名密钥变更）也必须发删除指令：
	// 否则残留 cookie 会让 /admin/login 与 /admin/ 判定相反，陷入重定向循环。
	// gorilla Get 解码失败时仍返回可用的空 session，Save 会写出 Max-Age=0 删除 cookie。
	sess, _ := s.auth.Get(r)
	sess.Options.MaxAge = -1
	_ = sess.Save(r, w)
	http.Redirect(w, r, "/admin/login", http.StatusFound)
}

// dashboard GET /admin/
func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	users, _ := s.store.CountUsers(ctx)
	tokens, _ := s.store.CountTokens(ctx)
	channels, _ := s.store.CountChannels(ctx)
	models, _ := s.store.CountModels(ctx)
	logs, _ := s.store.ListRequestLogs(ctx, store.LogFilter{}, 8, 0)
	chNames := map[int64]string{}
	for _, c := range channelsList(ctx, s) {
		chNames[c.ID] = c.Name
	}
	views := make([]logView, 0, len(logs))
	for _, l := range logs {
		views = append(views, logView{
			RequestLog:  l,
			ChannelName: chNames[l.ChannelID],
			StatusText:  statusLabel(l.Status),
		})
	}
	s.render(w, "dashboard.html", baseData("仪表盘 · 智能 API 网关", "dashboard", map[string]any{
		"Flash":    s.readFlash(w, r),
		"Users":    users,
		"Tokens":   tokens,
		"Channels": channels,
		"Models":   models,
		"RecentLogs": views,
	}))
}

// channelsList 渠道列表（忽略错误，供各页拼渠道名映射）。
func channelsList(ctx context.Context, s *Server) []store.Channel {
	list, _ := s.store.ListChannels(ctx)
	return list
}

// tokensPage GET /admin/tokens
func (s *Server) tokensPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tokens, _ := s.store.ListTokens(ctx)
	s.render(w, "tokens.html", baseData("令牌管理 · 智能 API 网关", "tokens", map[string]any{
		"Flash":   s.readFlash(w, r),
		"Tokens":  tokens,
		"Models":  enabledModelNames(ctx, s.store),
		"BaseURL": baseURL(r),
	}))
}

// createToken POST /admin/tokens
func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	allowAll := r.FormValue("allow_all") == "on"

	// 白名单：弹窗多选提交多个 models 值；兼容旧的逗号分隔单字段。
	var models []string
	for _, v := range r.Form["models"] {
		models = append(models, splitModels(v)...)
	}

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
		"Models":   enabledModelNames(ctx, s.store),
		"BaseURL":  baseURL(r),
	}))
}

// baseURL 从请求推导网关对外地址：优先取反向代理的 X-Forwarded-Proto，否则按 TLS 判断。
func baseURL(r *http.Request) string {
	scheme := "http"
	if p := r.Header.Get("X-Forwarded-Proto"); p == "https" {
		scheme = "https"
	} else if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// enabledModelNames 启用模型的对外名（alias 非空用 alias），去重后排序，供令牌白名单弹窗多选。
func enabledModelNames(ctx context.Context, st *store.Store) []string {
	models, err := st.ListModels(ctx)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
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
		out = append(out, name)
	}
	sort.Strings(out)
	return out
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
