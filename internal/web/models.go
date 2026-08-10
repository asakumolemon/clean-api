package web

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"api-gateway/internal/store"
)

// modelView 模型列表页一行：模型 + 渠道名 + 能力标签。
type modelView struct {
	store.Model
	ChannelName string
}

// modelsPage GET /admin/models
func (s *Server) modelsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	models, _ := s.store.ListModels(ctx)
	chs, _ := s.store.ListChannels(ctx)
	chName := map[int64]string{}
	for _, ch := range chs {
		chName[ch.ID] = ch.Name
	}
	views := make([]modelView, 0, len(models))
	for _, m := range models {
		views = append(views, modelView{Model: m, ChannelName: chName[m.ChannelID]})
	}
	s.render(w, "models.html", baseData("模型管理 · 智能 API 网关", "models", map[string]any{
		"Flash":  s.readFlash(w, r),
		"Models": views,
	}))
}

// toggleModel POST /admin/models/{id}/toggle
func (s *Server) toggleModel(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	models, _ := s.store.ListModels(r.Context())
	var cur *store.Model
	for i := range models {
		if models[i].ID == id {
			cur = &models[i]
			break
		}
	}
	if cur == nil {
		s.setFlash(w, r, "模型不存在")
	} else if err := s.store.SetModelEnabled(r.Context(), id, !cur.Enabled); err != nil {
		s.setFlash(w, r, "操作失败: "+err.Error())
	}
	http.Redirect(w, r, "/admin/models", http.StatusFound)
}

// setModelAlias POST /admin/models/{id}/alias
func (s *Server) setModelAlias(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	alias := strings.TrimSpace(r.FormValue("alias"))
	if err := s.store.SetModelAlias(r.Context(), id, alias); err != nil {
		s.setFlash(w, r, "设置别名失败: "+err.Error())
	} else if alias != "" {
		s.setFlash(w, r, "别名已设置："+alias)
	}
	http.Redirect(w, r, "/admin/models", http.StatusFound)
}

// overrideModel POST /admin/models/{id}/override
func (s *Server) overrideModel(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	form := func(f string) bool { return r.FormValue(f) == "on" }
	caps := store.Capabilities{
		System:   form("system"),
		Tools:    form("tools"),
		Vision:   form("vision"),
		JSONMode: form("json_mode"),
	}
	var fields []string
	for name, ok := range map[string]bool{"system": caps.System, "tools": caps.Tools, "vision": caps.Vision, "json_mode": caps.JSONMode} {
		if ok {
			fields = append(fields, name)
		}
	}
	if len(fields) == 0 {
		s.setFlash(w, r, "请至少勾选一项能力")
		http.Redirect(w, r, "/admin/models", http.StatusFound)
		return
	}
	if err := s.store.OverrideCapabilities(r.Context(), id, caps, fields); err != nil {
		s.setFlash(w, r, "覆盖能力失败: "+err.Error())
	} else {
		s.setFlash(w, r, "能力已手动覆盖")
	}
	http.Redirect(w, r, "/admin/models", http.StatusFound)
}
