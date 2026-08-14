package web

import (
	"context"
	"encoding/json"
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
	s.render(w, r, "login.html", baseData("登录 · 智能 API 网关", "", map[string]any{
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
	s.render(w, r, "dashboard.html", baseData("仪表盘 · 智能 API 网关", "dashboard", map[string]any{
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
	s.render(w, r, "tokens.html", baseData("令牌管理 · 智能 API 网关", "tokens", map[string]any{
		"Flash":   s.readFlash(w, r),
		"Tokens":  tokenViews(tokens),
		"Models":  modelOptions(ctx, s.store),
		"BaseURL": baseURL(r),
	}))
}

// tokenView 令牌行视图（附白名单 JSON，供编辑弹窗预勾选）。
type tokenView struct {
	store.Token
	WhitelistJSON string
}

// tokenViews 转换令牌列表为视图，序列化白名单 JSON。
func tokenViews(tokens []store.Token) []tokenView {
	views := make([]tokenView, 0, len(tokens))
	for _, t := range tokens {
		views = append(views, tokenView{Token: t, WhitelistJSON: tokenWhitelistJSON(t)})
	}
	return views
}

func tokenWhitelistJSON(t store.Token) string {
	b, err := json.Marshal(t.ModelWhitelist)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// createToken POST /admin/tokens
// 支持两种来源：令牌页表单（name + models 多选 / allow_all），
// 以及模型页「建令牌」一键直达（model 参数预填白名单，名称为空时自动命名）。
func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	allowAll := r.FormValue("allow_all") == "on"

	// 白名单：弹窗多选提交多个 models 值；兼容旧的逗号分隔单字段。
	var models []string
	for _, v := range r.Form["models"] {
		models = append(models, splitModels(v)...)
	}
	if len(models) == 0 && r.FormValue("model") != "" {
		models = []string{r.FormValue("model")}
		if name == "" {
			name = r.FormValue("model") + " 令牌"
		}
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
	s.render(w, r, "tokens.html", baseData("令牌管理 · 智能 API 网关", "tokens", map[string]any{
		"Tokens":   tokenViews(tokens),
		"NewToken": plain,
		"Models":   modelOptions(ctx, s.store),
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

// modelOption 令牌白名单弹窗的模型选项视图：对外名 + 渠道覆盖（总渠道/健康渠道）+ 能力汇总。
type modelOption struct {
	Name         string
	ChannelCount int
	ActiveCount  int
	Caps         store.Capabilities
}

// modelOptions 启用模型的对外名选项：按对外名（alias 非空用 alias）去重分组，
// 统计提供该模型的渠道数与健康（active）渠道数，能力取各渠道并集，排序后返回。
// 供令牌白名单弹窗展示「多渠道提供同一模型」的覆盖情况，避免配上无可用渠道的模型。
func modelOptions(ctx context.Context, st *store.Store) []modelOption {
	models, err := st.ListModels(ctx)
	if err != nil {
		return nil
	}
	chans, _ := st.ListChannels(ctx)
	active := make(map[int64]bool, len(chans))
	for _, c := range chans {
		active[c.ID] = c.Status == "active"
	}
	byName := map[string]*modelOption{}
	for _, m := range models {
		if !m.Enabled {
			continue
		}
		name := m.Name
		if m.Alias != "" {
			name = m.Alias
		}
		opt := byName[name]
		if opt == nil {
			opt = &modelOption{Name: name}
			byName[name] = opt
		}
		opt.ChannelCount++
		if active[m.ChannelID] {
			opt.ActiveCount++
		}
		opt.Caps.System = opt.Caps.System || m.Capabilities.System
		opt.Caps.Tools = opt.Caps.Tools || m.Capabilities.Tools
		opt.Caps.Vision = opt.Caps.Vision || m.Capabilities.Vision
		opt.Caps.JSONMode = opt.Caps.JSONMode || m.Capabilities.JSONMode
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]modelOption, 0, len(names))
	for _, n := range names {
		out = append(out, *byName[n])
	}
	return out
}

// updateTokenWhitelist POST /admin/tokens/{id}/whitelist：编辑令牌模型白名单（弹窗多选提交）。
func (s *Server) updateTokenWhitelist(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if _, err := s.store.GetTokenByID(r.Context(), id); err != nil {
		s.setFlash(w, r, "令牌不存在")
		http.Redirect(w, r, "/admin/tokens", http.StatusFound)
		return
	}
	allowAll := r.FormValue("allow_all") == "on"
	var models []string
	for _, v := range r.Form["models"] {
		models = append(models, splitModels(v)...)
	}
	if !allowAll && len(models) == 0 {
		s.setFlash(w, r, "必须指定至少一个模型；如需放行全部模型请显式勾选「允许全部模型」")
		http.Redirect(w, r, "/admin/tokens", http.StatusFound)
		return
	}
	if err := s.store.UpdateTokenWhitelist(r.Context(), id, models, allowAll); err != nil {
		s.setFlash(w, r, "保存白名单失败: "+err.Error())
	} else {
		s.setFlash(w, r, "模型白名单已更新")
	}
	http.Redirect(w, r, "/admin/tokens", http.StatusFound)
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
