package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Token 访问令牌。明文只生成时展示一次，库中只存 key_hash（sha256）。
type Token struct {
	ID             int64
	UserID         int64
	Name           string
	KeyHash        string
	ModelWhitelist []string
	AllowAll       bool // 显式「允许全部模型」开关，默认关
	Enabled        bool
	CreatedAt      time.Time
	LastUsedAt     sql.NullTime
}

func (s *Store) CreateToken(ctx context.Context, userID int64, name, keyHash string, whitelist []string, allowAll bool) (int64, error) {
	wl, err := json.Marshal(whitelist)
	if err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens(user_id, name, key_hash, model_whitelist, allow_all, enabled, created_at)
		 VALUES(?,?,?,?,?,1,?)`,
		userID, name, keyHash, string(wl), boolToInt(allowAll), now())
	if err != nil {
		return 0, fmt.Errorf("创建令牌: %w", err)
	}
	return res.LastInsertId()
}

func (s *Store) GetTokenByHash(ctx context.Context, keyHash string) (*Token, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, user_id, name, key_hash, model_whitelist, allow_all, enabled, created_at, last_used_at
		FROM tokens WHERE key_hash = ?`, keyHash)
	return scanToken(row)
}

func (s *Store) GetTokenByID(ctx context.Context, id int64) (*Token, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, user_id, name, key_hash, model_whitelist, allow_all, enabled, created_at, last_used_at
		FROM tokens WHERE id = ?`, id)
	return scanToken(row)
}

func (s *Store) ListTokens(ctx context.Context) ([]Token, error) {
	return s.listTokens(ctx, `SELECT id, user_id, name, key_hash, model_whitelist, allow_all, enabled, created_at, last_used_at
		FROM tokens ORDER BY id DESC`)
}

func (s *Store) ListTokensByUser(ctx context.Context, userID int64) ([]Token, error) {
	return s.listTokens(ctx, `SELECT id, user_id, name, key_hash, model_whitelist, allow_all, enabled, created_at, last_used_at
		FROM tokens WHERE user_id = ? ORDER BY id DESC`, userID)
}

func (s *Store) listTokens(ctx context.Context, query string, args ...any) ([]Token, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tokens := []Token{}
	for rows.Next() {
		var t Token
		if err := scanTokenRow(rows, &t); err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

func (s *Store) SetTokenEnabled(ctx context.Context, id int64, enabled bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tokens SET enabled = ? WHERE id = ?`, boolToInt(enabled), id)
	return err
}

// UpdateTokenWhitelist 更新模型白名单与「允许全部模型」开关。
func (s *Store) UpdateTokenWhitelist(ctx context.Context, id int64, whitelist []string, allowAll bool) error {
	wl, err := json.Marshal(whitelist)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE tokens SET model_whitelist = ?, allow_all = ? WHERE id = ?`,
		string(wl), boolToInt(allowAll), id)
	return err
}

func (s *Store) DeleteToken(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tokens WHERE id = ?`, id)
	return err
}

// TouchToken 更新最近使用时间。
func (s *Store) TouchToken(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tokens SET last_used_at = ? WHERE id = ?`, now(), id)
	return err
}

// CountTokens 统计令牌数量。
func (s *Store) CountTokens(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tokens`).Scan(&n)
	return n, err
}

type rowScanner interface{ Scan(...any) error }

func scanToken(row *sql.Row) (*Token, error) {
	var t Token
	if err := scanTokenRow(row, &t); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &t, nil
}

func scanTokenRow(sc rowScanner, t *Token) error {
	var wl string
	var allowAll int
	var enabled int
	if err := sc.Scan(&t.ID, &t.UserID, &t.Name, &t.KeyHash, &wl, &allowAll, &enabled, &t.CreatedAt, &t.LastUsedAt); err != nil {
		return err
	}
	t.AllowAll = allowAll != 0
	t.Enabled = enabled != 0
	if wl != "" {
		_ = json.Unmarshal([]byte(wl), &t.ModelWhitelist)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
