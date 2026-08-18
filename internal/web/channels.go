package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"api-gateway/internal/store"
)

// channelView 渠道列表页一行：渠道 + key 掩码 + 探测状态 + 模型数。
type channelView struct {
	store.Channel
	Keys       []keyView
	ProbeStep  string
	Probing    bool
	ProbeDone  bool
	ProbeFail  bool
	ProbeMsg   string
	ModelCount int
	Polling    bool
}

type keyView struct {
	ID            int64
	Masked        string
	InCooldown    bool
	CooldownUntil string
}

// channelsPage GET /admin/channels
func (s *Server) channelsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	chs, _ := s.store.ListChannels(ctx)
	views := make([]channelView, 0, len(chs))
	polling := false
	for _, ch := range chs {
		v := channelView{Channel: ch}
		keys, _ := s.store.ListChannelKeys(ctx, ch.ID)
		for _, k := range keys {
			kv := keyView{ID: k.ID, Masked: s.chm.MaskKey(k.KeyEnc)}
			if k.CooldownUntil.Valid {
				kv.InCooldown = true
				kv.CooldownUntil = k.CooldownUntil.Time.In(s.loc).Format("01-02 15:04")
			}
			v.Keys = append(v.Keys, kv)
		}
		if models, err := s.store.ListModelsByChannel(ctx, ch.ID); err == nil {
			v.ModelCount = len(models)
		}
		if st := s.chm.GetStatus(ch.ID); st != nil {
			v.Probing = st.Running
			v.ProbeStep = st.Step
			v.ProbeDone = st.Done
			v.ProbeFail = st.Failed
			v.ProbeMsg = st.Message
			if st.Running {
				polling = true
			}
		}
		views = append(views, v)
	}
	s.render(w, r, "channels.html", baseData("渠道管理 · 智能 API 网关", "channels", map[string]any{
		"Flash":    s.readFlash(w, r),
		"Channels": views,
		"Polling":  polling,
	}))
}

// createChannel POST /admin/channels
func (s *Server) createChannel(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	baseURL := strings.TrimSpace(r.FormValue("base_url"))
	chType := strings.TrimSpace(r.FormValue("type"))
	if chType == "" {
		chType = "auto"
	}
	keys := splitKeys(r.FormValue("api_keys"))

	if name == "" || baseURL == "" {
		s.setFlash(w, r, "请填写渠道名称与 base_url")
		http.Redirect(w, r, "/admin/channels", http.StatusFound)
		return
	}
	if len(keys) == 0 {
		s.setFlash(w, r, "请至少填写一个 API key")
		http.Redirect(w, r, "/admin/channels", http.StatusFound)
		return
	}

	cid, err := s.store.CreateChannel(r.Context(), name, chType, baseURL)
	if err != nil {
		s.setFlash(w, r, "创建渠道失败: "+err.Error())
		http.Redirect(w, r, "/admin/channels", http.StatusFound)
		return
	}
	for _, k := range keys {
		enc, err := s.chm.EncryptKey(k)
		if err != nil {
			s.setFlash(w, r, "加密 key 失败: "+err.Error())
			http.Redirect(w, r, "/admin/channels", http.StatusFound)
			return
		}
		if _, err := s.store.AddChannelKey(r.Context(), cid, enc); err != nil {
			s.setFlash(w, r, "保存 key 失败: "+err.Error())
			http.Redirect(w, r, "/admin/channels", http.StatusFound)
			return
		}
	}
	s.chm.StartProbe(cid)
	s.setFlash(w, r, "渠道已创建，开始自动探测…")
	http.Redirect(w, r, "/admin/channels", http.StatusFound)
}

// channelEditPage GET /admin/channels/{id}/edit
func (s *Server) channelEditPage(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	ch, err := s.store.GetChannel(r.Context(), id)
	if err != nil {
		s.setFlash(w, r, "渠道不存在")
		http.Redirect(w, r, "/admin/channels", http.StatusFound)
		return
	}
	s.render(w, r, "channel_edit.html", baseData("编辑渠道 · 智能 API 网关", "channels", map[string]any{
		"Flash": s.readFlash(w, r),
		"Ch":    ch,
	}))
}

// updateChannel POST /admin/channels/{id}/edit
func (s *Server) updateChannel(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	name := strings.TrimSpace(r.FormValue("name"))
	baseURL := strings.TrimSpace(r.FormValue("base_url"))
	chType := strings.TrimSpace(r.FormValue("type"))
	if chType == "" {
		chType = "auto"
	}
	weight, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("weight")))
	if weight <= 0 {
		weight = 1
	}
	strategy := strings.TrimSpace(r.FormValue("balance_strategy"))
	if strategy == "" {
		strategy = "random"
	}

	ch, err := s.store.GetChannel(r.Context(), id)
	if err != nil {
		s.setFlash(w, r, "渠道不存在")
		http.Redirect(w, r, "/admin/channels", http.StatusFound)
		return
	}
	if err := s.store.UpdateChannel(r.Context(), id, name, chType, baseURL, ch.Status, weight, strategy); err != nil {
		s.setFlash(w, r, "保存失败: "+err.Error())
		http.Redirect(w, r, "/admin/channels", http.StatusFound)
		return
	}

	// 填写了新 key 则整体替换，否则保留现有
	if newKeys := splitKeys(r.FormValue("api_keys")); len(newKeys) > 0 {
		encs := make([]string, 0, len(newKeys))
		for _, k := range newKeys {
			enc, err := s.chm.EncryptKey(k)
			if err != nil {
				s.setFlash(w, r, "加密 key 失败: "+err.Error())
				http.Redirect(w, r, "/admin/channels", http.StatusFound)
				return
			}
			encs = append(encs, enc)
		}
		if err := s.store.ReplaceChannelKeys(r.Context(), id, encs); err != nil {
			s.setFlash(w, r, "更新 key 失败: "+err.Error())
			http.Redirect(w, r, "/admin/channels", http.StatusFound)
			return
		}
	}

	// 类型/地址变更后自动重新探测
	if ch.Type != chType || ch.BaseURL != baseURL {
		s.chm.StartProbe(id)
		s.setFlash(w, r, "已保存，开始重新探测…")
	} else {
		s.setFlash(w, r, "已保存")
	}
	http.Redirect(w, r, "/admin/channels", http.StatusFound)
}

// toggleChannel POST /admin/channels/{id}/toggle
func (s *Server) toggleChannel(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	ch, err := s.store.GetChannel(r.Context(), id)
	if err != nil {
		s.setFlash(w, r, "渠道不存在")
	} else {
		status := "disabled"
		if ch.Status == "disabled" {
			status = "active"
		}
		if err := s.store.SetChannelStatus(r.Context(), id, status); err != nil {
			s.setFlash(w, r, "操作失败: "+err.Error())
		}
	}
	http.Redirect(w, r, "/admin/channels", http.StatusFound)
}

// resyncChannel POST /admin/channels/{id}/resync
func (s *Server) resyncChannel(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	s.chm.StartProbe(id)
	s.setFlash(w, r, "已触发重新探测（协议识别 + 模型同步）")
	http.Redirect(w, r, "/admin/channels", http.StatusFound)
}

// probeCapsChannel POST /admin/channels/{id}/probe-caps：
// 仅对该渠道全部模型执行能力探测（手动触发）。
func (s *Server) probeCapsChannel(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	s.chm.StartCapabilitiesProbe(id)
	s.setFlash(w, r, "已触发能力探测，页面将自动刷新进度")
	http.Redirect(w, r, "/admin/channels", http.StatusFound)
}

// deleteChannel POST /admin/channels/{id}/delete
func (s *Server) deleteChannel(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err := s.store.DeleteChannel(r.Context(), id); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.setFlash(w, r, "删除失败: "+err.Error())
	}
	http.Redirect(w, r, "/admin/channels", http.StatusFound)
}

// splitKeys 解析表单里的 key 列表（逗号或换行分隔）。
func splitKeys(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' }) {
		if k := strings.TrimSpace(part); k != "" {
			out = append(out, k)
		}
	}
	return out
}
