// 请求日志页：按模型、令牌、令牌分组、状态与日期筛选并分页展示。
package web

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"api-gateway/internal/store"
)

const logsPerPage = 20

// logView 日志行视图（附带渠道名便于展示）。
type logView struct {
	store.RequestLog
	ChannelName string
	StatusText  string
	TTFBText    string // TTFB 展示文本（成功流式显示毫秒，其余 —）
}

// tokenLogOption 日志筛选用令牌选项。
type tokenLogOption struct {
	ID    int64
	Name  string
	Group string
}

// logsPage GET /admin/logs
func (s *Server) logsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tokenID, _ := strconv.ParseInt(r.URL.Query().Get("token_id"), 10, 64)
	f := store.LogFilter{
		Model:      strings.TrimSpace(r.URL.Query().Get("model")),
		TokenID:    tokenID,
		TokenGroup: strings.TrimSpace(r.URL.Query().Get("group")),
		Status:     r.URL.Query().Get("status"),
	}
	// 起止日期（YYYY-MM-DD）：按管理面时区解析为本地零点，再转 UTC 与存储比对。
	// from 为当日零点（含），to 为次日零点（排他，含 to 当天）。
	fromStr := strings.TrimSpace(r.URL.Query().Get("from"))
	toStr := strings.TrimSpace(r.URL.Query().Get("to"))
	if fromStr != "" {
		if t, err := time.ParseInLocation("2006-01-02", fromStr, s.loc); err == nil {
			utc := t.UTC()
			f.From = &utc
		}
	}
	if toStr != "" {
		if t, err := time.ParseInLocation("2006-01-02", toStr, s.loc); err == nil {
			end := t.Add(24 * time.Hour).UTC()
			f.To = &end
		}
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	total, _ := s.store.CountRequestLogs(ctx, f)
	totalPages := (total + logsPerPage - 1) / logsPerPage
	if totalPages == 0 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}
	logs, _ := s.store.ListRequestLogs(ctx, f, logsPerPage, (page-1)*logsPerPage)

	// 按天×模型×令牌用量统计（与列表共用同一筛选，含时间范围；按管理面时区分桶）。
	stats, _ := s.store.LogUsageStats(ctx, f, s.loc)
	var sumReq, sumPrompt, sumCompletion, sumHits int64
	for _, st := range stats {
		sumReq += int64(st.Requests)
		sumHits += int64(st.CacheHits)
		sumPrompt += st.PromptTokens
		sumCompletion += st.CompletionTokens
	}
	hitRate := "-"
	if sumReq > 0 {
		hitRate = fmt.Sprintf("%.1f%%", float64(sumHits)/float64(sumReq)*100)
	}

	tokens, _ := s.store.ListTokens(ctx)
	tokenOptions := make([]tokenLogOption, 0, len(tokens))
	groupSet := map[string]bool{}
	for _, t := range tokens {
		tokenOptions = append(tokenOptions, tokenLogOption{ID: t.ID, Name: t.Name, Group: t.Group})
		if t.Group != "" {
			groupSet[t.Group] = true
		}
	}
	sort.Slice(tokenOptions, func(i, j int) bool {
		if tokenOptions[i].Name == tokenOptions[j].Name {
			return tokenOptions[i].ID < tokenOptions[j].ID
		}
		return tokenOptions[i].Name < tokenOptions[j].Name
	})
	groups := make([]string, 0, len(groupSet))
	for group := range groupSet {
		groups = append(groups, group)
	}
	sort.Strings(groups)

	channels, _ := s.store.ListChannels(ctx)
	chNames := map[int64]string{}
	for _, c := range channels {
		chNames[c.ID] = c.Name
	}
	views := make([]logView, 0, len(logs))
	for _, l := range logs {
		views = append(views, logView{
			RequestLog:  l,
			ChannelName: chNames[l.ChannelID],
			StatusText:  statusLabel(l),
			TTFBText:    ttfbLabel(l),
		})
	}

	s.render(w, r, "logs.html", baseData("请求日志 · 智能 API 网关", "logs", map[string]any{
		"Flash":           s.readFlash(w, r),
		"Logs":            views,
		"Filter":          f,
		"Tokens":          tokenOptions,
		"TokenGroups":     groups,
		"DefaultGroup":    store.DefaultTokenGroupFilter,
		"Page":            page,
		"TotalPages":      totalPages,
		"Total":           total,
		"From":            fromStr,
		"To":              toStr,
		"Stats":           stats,
		"StatsRequests":   sumReq,
		"StatsHits":       sumHits,
		"HitRate":         hitRate,
		"StatsPrompt":     sumPrompt,
		"StatsCompletion": sumCompletion,
		"StatsTotal":      sumPrompt + sumCompletion,
	}))
}

// statusLabel 状态码归类标签。流式中断（status 0 且已开始输出）显示中断标记。
func statusLabel(l store.RequestLog) string {
	status := l.Status
	if status == 0 && l.Streaming {
		return "流中断"
	}
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

// ttfbLabel TTFB 展示：成功流式且已记录首 token 延迟才显示毫秒数，其余 —。
func ttfbLabel(l store.RequestLog) string {
	if l.Streaming && l.TTFBMS > 0 {
		return strconv.FormatInt(l.TTFBMS, 10) + "ms"
	}
	return "—"
}
