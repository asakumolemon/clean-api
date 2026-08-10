package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Capabilities 模型能力（探测结果，可手动覆盖）。
type Capabilities struct {
	System   bool `json:"system"`
	Tools    bool `json:"tools"`
	Vision   bool `json:"vision"`
	JSONMode bool `json:"json_mode"`
}

// Model 上游模型（自动同步 + 手动覆盖）。
type Model struct {
	ID                 int64
	ChannelID          int64
	Name               string
	Enabled            bool
	Alias              string
	Capabilities       Capabilities
	CapabilityOverride string // 非空表示手动覆盖过（JSON 数组，例如 ["system","tools"]）
	LastSyncAt         time.Time
}

// SyncModels 将上游拉到的模型列表写库：新增的插入，已有的更新能力与同步时间。
// 返回本次新增数量。
func (s *Store) SyncModels(ctx context.Context, channelID int64, models map[string]Capabilities, syncedAt time.Time) (int, error) {
	added := 0
	for name, caps := range models {
		cj, err := json.Marshal(caps)
		if err != nil {
			return added, err
		}
		var exists bool
		if err := s.db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM models WHERE channel_id=? AND name=?)`,
			channelID, name).Scan(&exists); err != nil {
			return added, err
		}
		if exists {
			if _, err := s.db.ExecContext(ctx,
				`UPDATE models SET capabilities=?, last_sync_at=? WHERE channel_id=? AND name=?`,
				string(cj), syncedAt, channelID, name); err != nil {
				return added, fmt.Errorf("更新模型 %s: %w", name, err)
			}
			continue
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO models(channel_id, name, enabled, capabilities, last_sync_at)
			 VALUES(?,?,1,?,?)`,
			channelID, name, string(cj), syncedAt); err != nil {
			return added, fmt.Errorf("新增模型 %s: %w", name, err)
		}
		added++
	}
	return added, nil
}

func (s *Store) GetModel(ctx context.Context, channelID int64, name string) (*Model, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, channel_id, name, enabled, COALESCE(alias,''), capabilities, COALESCE(capability_override,''), last_sync_at
		FROM models WHERE channel_id=? AND name=?`, channelID, name)
	return scanModel(row)
}

func (s *Store) ListModels(ctx context.Context) ([]Model, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, channel_id, name, enabled, COALESCE(alias,''), capabilities, COALESCE(capability_override,''), last_sync_at
		FROM models ORDER BY channel_id, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	models := []Model{}
	for rows.Next() {
		var m Model
		if err := scanModelRow(rows, &m); err != nil {
			return nil, err
		}
		models = append(models, m)
	}
	return models, rows.Err()
}

// ListModelsByChannel 按渠道列模型。
func (s *Store) ListModelsByChannel(ctx context.Context, channelID int64) ([]Model, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, channel_id, name, enabled, COALESCE(alias,''), capabilities, COALESCE(capability_override,''), last_sync_at
		FROM models WHERE channel_id=? ORDER BY name`, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	models := []Model{}
	for rows.Next() {
		var m Model
		if err := scanModelRow(rows, &m); err != nil {
			return nil, err
		}
		models = append(models, m)
	}
	return models, rows.Err()
}

func (s *Store) CountModels(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM models`).Scan(&n)
	return n, err
}

func (s *Store) SetModelEnabled(ctx context.Context, id int64, enabled bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE models SET enabled=? WHERE id=?`, boolToInt(enabled), id)
	return err
}

func (s *Store) SetModelAlias(ctx context.Context, id int64, alias string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE models SET alias=? WHERE id=?`, alias, id)
	return err
}

// OverrideCapabilities 手动覆盖能力：写入覆盖标记（JSON 数组）并更新能力值。
func (s *Store) OverrideCapabilities(ctx context.Context, id int64, caps Capabilities, fields []string) error {
	cj, err := json.Marshal(caps)
	if err != nil {
		return err
	}
	oj, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE models SET capabilities=?, capability_override=? WHERE id=?`,
		string(cj), string(oj), id)
	return err
}

// ModelRoute 模型 → 渠道 的可路由条目（M3 路由分发用）。
type ModelRoute struct {
	ModelName       string // 渠道内真实模型名（按 name 或 alias 命中）
	ChannelID       int64
	ChannelType     string // openai | anthropic | responses | 厂商类型
	BaseURL         string
	BalanceStrategy string // random | round_robin（渠道内 key 轮换策略）
}

// ListChannelsByModel 返回提供指定模型的可用路由：
// models.name 或 models.alias 命中、模型启用、渠道 active。
// alias 为空的模型不会在按别名搜索时误命中。
func (s *Store) ListChannelsByModel(ctx context.Context, name string) ([]ModelRoute, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.name, c.id, c.type, c.base_url, c.balance_strategy
		FROM models m JOIN channels c ON c.id = m.channel_id
		WHERE (m.name = ? OR (m.alias IS NOT NULL AND m.alias != '' AND m.alias = ?))
		  AND m.enabled = 1 AND c.status = 'active'
		ORDER BY c.id`, name, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	routes := []ModelRoute{}
	for rows.Next() {
		var r ModelRoute
		if err := rows.Scan(&r.ModelName, &r.ChannelID, &r.ChannelType, &r.BaseURL, &r.BalanceStrategy); err != nil {
			return nil, err
		}
		routes = append(routes, r)
	}
	return routes, rows.Err()
}

func scanModel(row *sql.Row) (*Model, error) {
	var m Model
	if err := scanModelRow(row, &m); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &m, nil
}

func scanModelRow(sc rowScanner, m *Model) error {
	var caps, override string
	var enabled int
	err := sc.Scan(&m.ID, &m.ChannelID, &m.Name, &enabled, &m.Alias, &caps, &override, &m.LastSyncAt)
	if err != nil {
		return err
	}
	m.Enabled = enabled != 0
	m.CapabilityOverride = override
	if caps != "" {
		_ = json.Unmarshal([]byte(caps), &m.Capabilities)
	}
	return nil
}
