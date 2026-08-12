package store

import (
	"context"
	"fmt"
	"time"
)

// RequestLog 一条请求日志（M5 起写入，异步、可丢）。
type RequestLog struct {
	ID               int64
	TS               time.Time
	RequestID        string
	TokenID          int64
	UserID           int64
	Model            string
	ChannelID        int64
	Status           int
	LatencyMS        int64
	PromptTokens     int
	CompletionTokens int
	Error            string
}

// LogFilter 日志查询筛选（全部字段为空/零值表示不过滤）。
type LogFilter struct {
	Model  string // 按模型名模糊匹配
	Token  string // 按令牌名精确匹配（空=全部）
	Status string // all | 2xx | 4xx | 5xx
	UserID int64  // 0=全部用户
	From   *time.Time // 起（含，UTC）；空=不限
	To     *time.Time // 止（排他，UTC，To 当日 24:00）；空=不限
}

// UsageRow 按天×模型分组的用量统计行（Day 为 UTC 日期 YYYY-MM-DD）。
type UsageRow struct {
	Day              string
	Model            string
	Requests         int
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64 // PromptTokens + CompletionTokens
}

// InsertRequestLog 写一条请求日志（调用方负责异步与错误忽略）。
func (s *Store) InsertRequestLog(ctx context.Context, l *RequestLog) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO request_logs(ts, request_id, token_id, user_id, model, channel_id, status, latency_ms, prompt_tokens, completion_tokens, error)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		l.TS, l.RequestID, l.TokenID, l.UserID, l.Model, l.ChannelID, l.Status, l.LatencyMS,
		l.PromptTokens, l.CompletionTokens, l.Error)
	if err != nil {
		return fmt.Errorf("写入请求日志: %w", err)
	}
	return nil
}

// ListRequestLogs 分页查询日志（按时间倒序）。
func (s *Store) ListRequestLogs(ctx context.Context, f LogFilter, limit, offset int) ([]RequestLog, error) {
	where, args := logFilterWhere(f)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, ts, request_id, COALESCE(token_id,0), COALESCE(user_id,0), COALESCE(model,''),
		       COALESCE(channel_id,0), COALESCE(status,0), COALESCE(latency_ms,0),
		       COALESCE(prompt_tokens,0), COALESCE(completion_tokens,0), COALESCE(error,'')
		FROM request_logs`+where+` ORDER BY id DESC LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	logs := []RequestLog{}
	for rows.Next() {
		var l RequestLog
		if err := rows.Scan(&l.ID, &l.TS, &l.RequestID, &l.TokenID, &l.UserID, &l.Model,
			&l.ChannelID, &l.Status, &l.LatencyMS, &l.PromptTokens, &l.CompletionTokens, &l.Error); err != nil {
			return nil, err
		}
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

// logFilterWhere 拼接筛选条件（含前导空格或空串）。
func logFilterWhere(f LogFilter) (string, []any) {
	conds := []string{}
	args := []any{}
	if f.Model != "" {
		conds = append(conds, "model LIKE ?")
		args = append(args, "%"+f.Model+"%")
	}
	if f.Token != "" {
		conds = append(conds, "token_id = (SELECT id FROM tokens WHERE name = ?)")
		args = append(args, f.Token)
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

// LogUsageStats 按天×模型统计请求数与 token 用量（日期按 UTC，即 ts 文本前缀 YYYY-MM-DD）。
// 说明：modernc 驱动默认把 time.Time 以 Go t.String() 文本写入（如 "2026-08-10 12:00:00 +0000 UTC"），
// SQLite 的 date()/strftime() 无法解析该格式（返回 NULL），故取文本前 10 位作为日期，与写入格式一致。
func (s *Store) LogUsageStats(ctx context.Context, f LogFilter) ([]UsageRow, error) {
	where, args := logFilterWhere(f)
	rows, err := s.db.QueryContext(ctx, `
		SELECT COALESCE(substr(ts, 1, 10), '') AS day, COALESCE(model,''), COUNT(*),
		       COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0)
		FROM request_logs`+where+`
		GROUP BY substr(ts, 1, 10), model
		ORDER BY day DESC, COUNT(*) DESC, model`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stats := []UsageRow{}
	for rows.Next() {
		var u UsageRow
		if err := rows.Scan(&u.Day, &u.Model, &u.Requests, &u.PromptTokens, &u.CompletionTokens); err != nil {
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
