// 测试台（Playground）：页面上选模型直接聊天，调内部路由，不走令牌/白名单。
package web

import (
	"net/http"
	"strings"

	"api-gateway/internal/protocol"
)

// playgroundModel 下拉模型视图（对外名优先 alias）。
type playgroundModel struct {
	Name string
}

// playgroundPage GET /admin/playground
func (s *Server) playgroundPage(w http.ResponseWriter, r *http.Request) {
	s.renderPlayground(w, r, "", "", "", "", "")
}

// playgroundChat POST /admin/playground/chat
func (s *Server) playgroundChat(w http.ResponseWriter, r *http.Request) {
	model := r.FormValue("model")
	system := r.FormValue("system")
	message := r.FormValue("message")
	if model == "" {
		s.renderPlayground(w, r, model, system, message, "请选择模型", "")
		return
	}
	if strings.TrimSpace(message) == "" {
		s.renderPlayground(w, r, model, system, message, "请输入消息内容", "")
		return
	}
	req := &protocol.ChatRequest{Model: model}
	if strings.TrimSpace(system) != "" {
		req.Messages = append(req.Messages, protocol.Message{
			Role:    "system",
			Content: []protocol.ContentPart{{Type: "text", Text: system}},
		})
	}
	req.Messages = append(req.Messages, protocol.Message{
		Role:    "user",
		Content: []protocol.ContentPart{{Type: "text", Text: message}},
	})
	res, err := s.router.Chat(r.Context(), model, req)
	if err != nil {
		s.renderPlayground(w, r, model, system, message, "对话失败: "+err.Error(), "")
		return
	}
	result := ""
	for _, c := range res.Resp.Choices {
		for _, p := range c.Message.Content {
			if p.Type == "text" {
				result += p.Text
			}
		}
	}
	s.renderPlayground(w, r, model, system, message, "", result)
}

// renderPlayground 渲染测试台页面（回显输入与结果，不重定向）。
func (s *Server) renderPlayground(w http.ResponseWriter, r *http.Request, model, system, message, errMsg, result string) {
	ctx := r.Context()
	all, _ := s.store.ListModels(ctx)
	models := []playgroundModel{}
	seen := map[string]bool{}
	for _, m := range all {
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
		models = append(models, playgroundModel{Name: name})
	}
	s.render(w, r, "playground.html", baseData("测试台 · 智能 API 网关", "playground", map[string]any{
		"Flash":   s.readFlash(w, r),
		"Models":  models,
		"Model":   model,
		"System":  system,
		"Message": message,
		"Error":   errMsg,
		"Result":  result,
	}))
}
