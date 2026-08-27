package store

import (
	"context"
	"fmt"
	"time"
)

// DefaultTokenGroupFilter 是日志筛选中的默认分组标记，避免与“全部分组”的空值冲突。
const DefaultTokenGroupFilter = "__default__"

// RequestLog 一条请求日志（异步、可丢）。令牌名称与分组为请求发生时的快照。
type RequestLog struct {
	ID               int64
	TS               time.Time
	RequestID        string
	TokenID          int64
	TokenName        string
	TokenGroup       string
	UserID           int64
	Model            string
	ChannelID        int64
	Status           int
	LatencyMS        int64
	TTFBMS           int64 // 流式：首 token 延迟（首事件写出时刻 - 请求开始）；非流式为 0
	PromptTokens     int
	CompletionTokens int
	CacheHit         bool
	Streaming        bool // 流式请求（成功与已开始输出的错误都标记）
	Interrupted      bool // 流式中断（已开始输出后出错 / 客户端断开）
	Error            string
}

// LogFilter 日志查询筛选（全部字段为空/零值表示不过滤）。
type LogFilter struct {
	Model      string     // 按模型名模糊匹配
	TokenID    int64      // 按令牌 ID 精确匹配（0=全部）
	TokenGroup string     // 按令牌分组匹配（空=全部；DefaultTokenGroupFilter=默认分组）
	Status     string     // all | 2xx | 4xx | 5xx
	UserID     int64      // 0=全部用户
	From       *time.Time // 起（含，UTC）；空=不限
	To         *time.Time // 止（排他，UTC，To 当日 24:00）；空=不限
}

// UsageRow 按天×模型×令牌分组的用量统计行（Day 为本地日期 YYYY-MM-DD，按传入时区分桶）。
type UsageRow struct {
	Day              string
	Model            string
	TokenID          int64
	TokenName        string
	TokenGroup       string
	Requests         int
	CacheHits        int
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
}

// InsertRequestLog 写一条请求日志（调用方负责异步与错误忽略）。
func (s *Store) InsertRequestLog(ctx context.Context, l *RequestLog) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO request_logs(ts, request_id, token_id, token_name, token_group, user_id, model, channel_id, status, latency_ms, ttfb_ms, prompt_tokens, completion_tokens, cache_hit, streaming, interrupted, error)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		l.TS, l.RequestID, l.TokenID, l.TokenName, l.TokenGroup, l.UserID, l.Model, l.ChannelID, l.Status, l.LatencyMS,
		l.TTFBMS, l.PromptTokens, l.CompletionTokens, boolToInt(l.CacheHit), boolToInt(l.Streaming), boolToInt(l.Interrupted), l.Error)
	if err != nil {
		return fmt.Errorf("写入请求日志: %w", err)
	}
	return nil
}

// ListRequestLogs 分页查询日志（按时间倒序）。
func (s *Store) ListRequestLogs(ctx context.Context, f LogFilter, limit, offset int) ([]RequestLog, error) {
	where, args := logFilterWhere(f)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, ts, request_id, COALESCE(token_id,0), COALESCE(token_name,''), COALESCE(token_group,''),
		       COALESCE(user_id,0), COALESCE(model,''), COALESCE(channel_id,0), COALESCE(status,0), COALESCE(latency_ms,0),
		       COALESCE(ttfb_ms,0), COALESCE(prompt_tokens,0), COALESCE(completion_tokens,0), COALESCE(cache_hit,0),
		       COALESCE(streaming,0), COALESCE(interrupted,0), COALESCE(error,'')
		FROM request_logs`+where+` ORDER BY id DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	logs := []RequestLog{}
	for rows.Next() {
		var l RequestLog
		var hit, streaming, interrupted int
		if err := rows.Scan(&l.ID, &l.TS, &l.RequestID, &l.TokenID, &l.TokenName, &l.TokenGroup, &l.UserID, &l.Model,
			&l.ChannelID, &l.Status, &l.LatencyMS, &l.TTFBMS, &l.PromptTokens, &l.CompletionTokens, &hit, &streaming, &interrupted, &l.Error); err != nil {
			return nil, err
		}
		l.CacheHit = hit != 0
		l.Streaming = streaming != 0
		l.Interrupted = interrupted != 0
		logs = append(logs, l)
	}
	return logs, rows.Err()
}

// CountRequestLogs 统计筛选后的日志条数（分页用）。
func (s *Store) CountRequestLogs(ctx context.Context, f LogFilter) (int, error) {
	where, args := logFilterWhere(f)
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM request_logs`+where, args...).Scan(&n)
	return n, err
}

// CountCacheHits 统计筛选条件下的缓存命中条数（命中率 = CountCacheHits/CountRequestLogs）。
func (s *Store) CountCacheHits(ctx context.Context, f LogFilter) (int, error) {
	where, args := logFilterWhere(f)
	q := `SELECT COUNT(*) FROM request_logs`
	if where != "" {
		q += where + " AND cache_hit = 1"
	} else {
		q += " WHERE cache_hit = 1"
	}
	var n int
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
}

// logFilterWhere 拼接筛选条件（含前导空格或空串）。
func logFilterWhere(f LogFilter) (string, []any) {
	conds := []string{}
	args := []any{}
	if f.Model != "" {
		conds = append(conds, "model LIKE ?")
		args = append(args, "%"+f.Model+"%")
	}
	if f.TokenID > 0 {
		conds = append(conds, "token_id = ?")
		args = append(args, f.TokenID)
	}
	if f.TokenGroup == DefaultTokenGroupFilter {
		conds = append(conds, "COALESCE(token_group, '') = '' AND COALESCE(token_name, '') <> ''")
	} else if f.TokenGroup != "" {
		conds = append(conds, "token_group = ?")
		args = append(args, f.TokenGroup)
	}
	switch f.Status {
	case "2xx":
		conds = append(conds, "status >= 200 AND status < 300")
	case "4xx":
		conds = append(conds, "status >= 400 AND status < 500")
	case "5xx":
		conds = append(conds, "status >= 500")
	}
	if f.UserID > 0 {
		conds = append(conds, "user_id = ?")
		args = append(args, f.UserID)
	}
	if f.From != nil {
		conds = append(conds, "ts >= ?")
		args = append(args, *f.From)
	}
	if f.To != nil {
		conds = append(conds, "ts < ?")
		args = append(args, *f.To)
	}
	if len(conds) == 0 {
		return "", nil
	}
	where := " WHERE " + conds[0]
	for _, c := range conds[1:] {
		where += " AND " + c
	}
	return where, args
}

// LogUsageStats 按天×模型×令牌统计请求数与 token 用量（按 loc 时区分桶，默认 UTC）。
// modernc 驱动把 time.Time 以 Go 时间文本写入，SQLite 无法直接解析，因此截取前 19 位后按时区偏移平移。
func (s *Store) LogUsageStats(ctx context.Context, f LogFilter, loc *time.Location) ([]UsageRow, error) {
	where, args := logFilterWhere(f)
	offsetSec := 0
	if loc != nil {
		_, offsetSec = time.Now().In(loc).Zone()
	}
	shift := fmt.Sprintf("%+d seconds", offsetSec)
	rows, err := s.db.QueryContext(ctx, `
		SELECT COALESCE(substr(datetime(substr(ts, 1, 19), printf('%+d seconds', ?)), 1, 10), '') AS day,
		       COALESCE(model,''), COALESCE(token_id,0), COALESCE(token_name,''), COALESCE(token_group,''), COUNT(*),
		       COALESCE(SUM(cache_hit),0), COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0)
		FROM request_logs`+where+`
		GROUP BY day, model, token_id, token_name, token_group
		ORDER BY day DESC, COUNT(*) DESC, model, token_name`, append([]any{shift}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stats := []UsageRow{}
	for rows.Next() {
		var u UsageRow
		if err := rows.Scan(&u.Day, &u.Model, &u.TokenID, &u.TokenName, &u.TokenGroup, &u.Requests, &u.CacheHits, &u.PromptTokens, &u.CompletionTokens); err != nil {
			return nil, err
		}
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
		stats = append(stats, u)
	}
	return stats, rows.Err()
}

// DeleteRequestLogsBefore 删除保留期之前的日志（保留策略清理用）。
func (s *Store) DeleteRequestLogsBefore(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM request_logs WHERE ts < ?`, before)
	if err != nil {
		return 0, fmt.Errorf("清理请求日志: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// CountRequestLogsTotal 全部日志条数（仪表盘用）。
func (s *Store) CountRequestLogsTotal(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM request_logs`).Scan(&n)
	return n, err
}
