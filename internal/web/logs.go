// 请求日志页：筛选（模型/令牌/状态）+ 分页。
package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"api-gateway/internal/store"
)

const logsPerPage = 20

// logView 日志行视图（附带渠道名/令牌名便于展示）。
type logView struct {
	store.RequestLog
	ChannelName string
	TokenName   string
	StatusText  string // 状态码 + 归类标签
}

// logsPage GET /admin/logs
func (s *Server) logsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	f := store.LogFilter{
		Model:  strings.TrimSpace(r.URL.Query().Get("model")),
		Token:  strings.TrimSpace(r.URL.Query().Get("token")),
		Status: r.URL.Query().Get("status"),
	}
	// 起止日期（YYYY-MM-DD）：from 为当日零点（含），to 为次日零点（排他，含 to 当天）。
	fromStr := strings.TrimSpace(r.URL.Query().Get("from"))
	toStr := strings.TrimSpace(r.URL.Query().Get("to"))
	if fromStr != "" {
		if t, err := time.Parse("2006-01-02", fromStr); err == nil {
			f.From = &t
		}
	}
	if toStr != "" {
		if t, err := time.Parse("2006-01-02", toStr); err == nil {
			end := t.Add(24 * time.Hour)
			f.To = &end
		}
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	total, _ := s.store.CountRequestLogs(ctx, f)
	logs, _ := s.store.ListRequestLogs(ctx, f, logsPerPage, (page-1)*logsPerPage)
	totalPages := (total + logsPerPage - 1) / logsPerPage
	if totalPages == 0 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}

	// 按天×模型用量统计（与列表共用同一筛选，含时间范围）。
	stats, _ := s.store.LogUsageStats(ctx, f)
	var sumReq, sumPrompt, sumCompletion int64
	for _, st := range stats {
		sumReq += int64(st.Requests)
		sumPrompt += st.PromptTokens
		sumCompletion += st.CompletionTokens
	}

	// 令牌名与渠道名映射
	tokens, _ := s.store.ListTokens(ctx)
	tokenNames := map[int64]string{}
	for _, t := range tokens {
		tokenNames[t.ID] = t.Name
	}
	channels, _ := s.store.ListChannels(ctx)
	chNames := map[int64]string{}
	for _, c := range channels {
		chNames[c.ID] = c.Name
	}
	allTokenNames := make([]string, 0, len(tokens))
	for _, t := range tokens {
		allTokenNames = append(allTokenNames, t.Name)
	}

	views := make([]logView, 0, len(logs))
	for _, l := range logs {
		views = append(views, logView{
			RequestLog:  l,
			ChannelName: chNames[l.ChannelID],
			TokenName:   tokenNames[l.TokenID],
			StatusText:  statusLabel(l.Status),
		})
	}

	s.render(w, "logs.html", baseData("请求日志 · 智能 API 网关", "logs", map[string]any{
		"Flash":          s.readFlash(w, r),
		"Logs":           views,
		"Filter":         f,
		"Tokens":         allTokenNames,
		"Page":           page,
		"TotalPages":     totalPages,
		"Total":          total,
		"From":           fromStr,
		"To":             toStr,
		"Stats":          stats,
		"StatsRequests":  sumReq,
		"StatsPrompt":    sumPrompt,
		"StatsCompletion": sumCompletion,
		"StatsTotal":     sumPrompt + sumCompletion,
	}))
}

// statusLabel 状态码归类标签。
func statusLabel(status int) string {
	switch {
	case status >= 200 && status < 300:
		return strconv.Itoa(status) + " 成功"
	case status >= 400 && status < 500:
		return strconv.Itoa(status) + " 客户端错误"
	case status >= 500:
		return strconv.Itoa(status) + " 服务端错误"
	case status == 0:
		return "流中断"
	default:
		return strconv.Itoa(status)
	}
}
