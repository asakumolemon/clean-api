package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Channel 上游渠道。
type Channel struct {
	ID              int64
	Name            string
	Type            string // auto（待探测）| openai | anthropic | responses | 厂商类型
	BaseURL         string
	Status          string // active | down | disabled
	Weight          int
	BalanceStrategy string // random | round_robin
	CreatedAt       time.Time
}

// ChannelKey 渠道下的一个 API key（加密存储）。
type ChannelKey struct {
	ID            int64
	ChannelID     int64
	KeyEnc        string
	CooldownUntil sql.NullTime
}

func (s *Store) CreateChannel(ctx context.Context, name, chType, baseURL string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO channels(name, type, base_url, status, weight, balance_strategy, created_at)
		 VALUES(?,?,?,'active',1,'random',?)`,
		name, chType, baseURL, now())
	if err != nil {
		return 0, fmt.Errorf("创建渠道: %w", err)
	}
	return res.LastInsertId()
}

func (s *Store) GetChannel(ctx context.Context, id int64) (*Channel, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, type, base_url, status, weight, balance_strategy, created_at
		 FROM channels WHERE id = ?`, id)
	return scanChannel(row)
}

func (s *Store) ListChannels(ctx context.Context) ([]Channel, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, type, base_url, status, weight, balance_strategy, created_at
		 FROM channels ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	channels := []Channel{}
	for rows.Next() {
		var c Channel
		if err := scanChannelRow(rows, &c); err != nil {
			return nil, err
		}
		channels = append(channels, c)
	}
	return channels, rows.Err()
}

// UpdateChannel 更新渠道基础字段。
func (s *Store) UpdateChannel(ctx context.Context, id int64, name, chType, baseURL, status string, weight int, strategy string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE channels SET name=?, type=?, base_url=?, status=?, weight=?, balance_strategy=? WHERE id=?`,
		name, chType, baseURL, status, weight, strategy, id)
	if err != nil {
		return fmt.Errorf("更新渠道: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetChannelStatus 更新渠道状态（active/down/disabled）。
func (s *Store) SetChannelStatus(ctx context.Context, id int64, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE channels SET status=? WHERE id=?`, status, id)
	return err
}

// DeleteChannel 删除渠道及其 key 与模型。
func (s *Store) DeleteChannel(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM channel_keys WHERE channel_id=?`, id); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM models WHERE channel_id=?`, id); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM channels WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CountChannels(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM channels`).Scan(&n)
	return n, err
}

// AddChannelKey 添加一个（已加密）key。
func (s *Store) AddChannelKey(ctx context.Context, channelID int64, keyEnc string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO channel_keys(channel_id, key_enc) VALUES(?,?)`, channelID, keyEnc)
	if err != nil {
		return 0, fmt.Errorf("添加渠道 key: %w", err)
	}
	return res.LastInsertId()
}

func (s *Store) ListChannelKeys(ctx context.Context, channelID int64) ([]ChannelKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, channel_id, key_enc, cooldown_until FROM channel_keys WHERE channel_id=? ORDER BY id`,
		channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []ChannelKey{}
	for rows.Next() {
		var k ChannelKey
		if err := rows.Scan(&k.ID, &k.ChannelID, &k.KeyEnc, &k.CooldownUntil); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// ReplaceChannelKeys 整体替换渠道的 key 列表（编辑渠道用）。
func (s *Store) ReplaceChannelKeys(ctx context.Context, channelID int64, keyEncs []string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM channel_keys WHERE channel_id=?`, channelID); err != nil {
		return err
	}
	for _, k := range keyEncs {
		if _, err := s.AddChannelKey(ctx, channelID, k); err != nil {
			return err
		}
	}
	return nil
}

// SetKeyCooldown 设置 key 冷却截止时间；nil 表示清除冷却。
func (s *Store) SetKeyCooldown(ctx context.Context, keyID int64, until *time.Time) error {
	var v any
	if until == nil {
		v = nil
	} else {
		v = *until
	}
	_, err := s.db.ExecContext(ctx, `UPDATE channel_keys SET cooldown_until=? WHERE id=?`, v, keyID)
	return err
}

func scanChannel(row *sql.Row) (*Channel, error) {
	var c Channel
	if err := scanChannelRow(row, &c); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &c, nil
}

func scanChannelRow(sc rowScanner, c *Channel) error {
	return sc.Scan(&c.ID, &c.Name, &c.Type, &c.BaseURL, &c.Status, &c.Weight, &c.BalanceStrategy, &c.CreatedAt)
}
