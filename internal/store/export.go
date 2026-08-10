package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// 配置导入导出（M5）：全量导出 users/tokens/channels/channel_keys/models 为单个 JSON，
// 导入为替换式（单事务清空重建，保留原始 id）。
// 注意：token 只含 key_hash，明文不可恢复；key 为加密密文，迁移需同一 GATEWAY_ENC_KEY。

// ExportData 导出文件结构（对外可读的独立 DTO，字段小写）。
type ExportData struct {
	Version     int               `json:"version"`
	ExportedAt  time.Time         `json:"exported_at"`
	Users       []exportUser      `json:"users"`
	Tokens      []exportToken     `json:"tokens"`
	Channels    []exportChannel   `json:"channels"`
	ChannelKeys []exportChannelKey `json:"channel_keys"`
	Models      []exportModel     `json:"models"`
}

type exportUser struct {
	ID           int64     `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"password_hash"`
	Role         string    `json:"role"`
	CreatedAt    time.Time `json:"created_at"`
}

type exportToken struct {
	ID            int64     `json:"id"`
	UserID        int64     `json:"user_id"`
	Name          string    `json:"name"`
	KeyHash       string    `json:"key_hash"`
	ModelWhitelist []string `json:"model_whitelist"`
	AllowAll      bool      `json:"allow_all"`
	Enabled       bool      `json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
	LastUsedAt    *time.Time `json:"last_used_at,omitempty"`
}

type exportChannel struct {
	ID              int64     `json:"id"`
	Name            string    `json:"name"`
	Type            string    `json:"type"`
	BaseURL         string    `json:"base_url"`
	Status          string    `json:"status"`
	Weight          int       `json:"weight"`
	BalanceStrategy string    `json:"balance_strategy"`
	CreatedAt       time.Time `json:"created_at"`
}

type exportChannelKey struct {
	ID        int64  `json:"id"`
	ChannelID int64  `json:"channel_id"`
	KeyEnc    string `json:"key_enc"`
}

type exportModel struct {
	ID                 int64     `json:"id"`
	ChannelID          int64     `json:"channel_id"`
	Name               string    `json:"name"`
	Enabled            bool      `json:"enabled"`
	Alias              string    `json:"alias"`
	Capabilities       Capabilities `json:"capabilities"`
	CapabilityOverride string    `json:"capability_override"`
	LastSyncAt         time.Time `json:"last_sync_at"`
}

// ExportAll 导出全部配置为 JSON 字节。
func (s *Store) ExportAll(ctx context.Context) ([]byte, error) {
	users, err := s.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	tokens, err := s.ListTokens(ctx)
	if err != nil {
		return nil, err
	}
	channels, err := s.ListChannels(ctx)
	if err != nil {
		return nil, err
	}
	models, err := s.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	data := ExportData{Version: 1, ExportedAt: now()}
	for _, u := range users {
		data.Users = append(data.Users, exportUser{
			ID: u.ID, Username: u.Username, PasswordHash: u.PasswordHash, Role: u.Role, CreatedAt: u.CreatedAt,
		})
	}
	for _, t := range tokens {
		var last *time.Time
		if t.LastUsedAt.Valid {
			v := t.LastUsedAt.Time
			last = &v
		}
		data.Tokens = append(data.Tokens, exportToken{
			ID: t.ID, UserID: t.UserID, Name: t.Name, KeyHash: t.KeyHash,
			ModelWhitelist: t.ModelWhitelist, AllowAll: t.AllowAll, Enabled: t.Enabled,
			CreatedAt: t.CreatedAt, LastUsedAt: last,
		})
	}
	for _, c := range channels {
		data.Channels = append(data.Channels, exportChannel{
			ID: c.ID, Name: c.Name, Type: c.Type, BaseURL: c.BaseURL, Status: c.Status,
			Weight: c.Weight, BalanceStrategy: c.BalanceStrategy, CreatedAt: c.CreatedAt,
		})
	}
	for _, k := range data.Channels {
		keys, err := s.ListChannelKeys(ctx, k.ID)
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			// 冷却状态是瞬态，导出时清空
			data.ChannelKeys = append(data.ChannelKeys, exportChannelKey{
				ID: key.ID, ChannelID: key.ChannelID, KeyEnc: key.KeyEnc,
			})
		}
	}
	for _, m := range models {
		data.Models = append(data.Models, exportModel{
			ID: m.ID, ChannelID: m.ChannelID, Name: m.Name, Enabled: m.Enabled, Alias: m.Alias,
			Capabilities: m.Capabilities, CapabilityOverride: m.CapabilityOverride, LastSyncAt: m.LastSyncAt,
		})
	}
	return json.MarshalIndent(data, "", "  ")
}

// ImportAll 导入配置（替换式）：单事务内清空全部表并按原 id 重建。
// 任一环节失败则整体回滚。
func (s *Store) ImportAll(ctx context.Context, data []byte) error {
	var in ExportData
	if err := json.Unmarshal(data, &in); err != nil {
		return fmt.Errorf("解析导入文件: %w", err)
	}
	if in.Version != 1 {
		return fmt.Errorf("不支持的导出文件版本: %d", in.Version)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// 先删子表再删父表（外键顺序）
	for _, stmt := range []string{
		`DELETE FROM channel_keys`, `DELETE FROM models`, `DELETE FROM channels`,
		`DELETE FROM tokens`, `DELETE FROM users`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("清空数据: %w", err)
		}
	}
	for _, u := range in.Users {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO users(id, username, password_hash, role, created_at) VALUES(?,?,?,?,?)`,
			u.ID, u.Username, u.PasswordHash, u.Role, u.CreatedAt); err != nil {
			return fmt.Errorf("导入用户 %s: %w", u.Username, err)
		}
	}
	for _, t := range in.Tokens {
		wl, err := json.Marshal(t.ModelWhitelist)
		if err != nil {
			return err
		}
		var last any
		if t.LastUsedAt != nil {
			last = *t.LastUsedAt
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tokens(id, user_id, name, key_hash, model_whitelist, allow_all, enabled, created_at, last_used_at)
			 VALUES(?,?,?,?,?,?,?,?,?)`,
			t.ID, t.UserID, t.Name, t.KeyHash, string(wl), boolToInt(t.AllowAll), boolToInt(t.Enabled), t.CreatedAt, last); err != nil {
			return fmt.Errorf("导入令牌 %s: %w", t.Name, err)
		}
	}
	for _, c := range in.Channels {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO channels(id, name, type, base_url, status, weight, balance_strategy, created_at)
			 VALUES(?,?,?,?,?,?,?,?)`,
			c.ID, c.Name, c.Type, c.BaseURL, c.Status, c.Weight, c.BalanceStrategy, c.CreatedAt); err != nil {
			return fmt.Errorf("导入渠道 %s: %w", c.Name, err)
		}
	}
	for _, k := range in.ChannelKeys {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO channel_keys(id, channel_id, key_enc) VALUES(?,?,?)`,
			k.ID, k.ChannelID, k.KeyEnc); err != nil {
			return fmt.Errorf("导入渠道 key（渠道 %d）: %w", k.ChannelID, err)
		}
	}
	for _, m := range in.Models {
		caps, err := json.Marshal(m.Capabilities)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO models(id, channel_id, name, enabled, alias, capabilities, capability_override, last_sync_at)
			 VALUES(?,?,?,?,?,?,?,?)`,
			m.ID, m.ChannelID, m.Name, boolToInt(m.Enabled), m.Alias, string(caps), m.CapabilityOverride, m.LastSyncAt); err != nil {
			return fmt.Errorf("导入模型 %s: %w", m.Name, err)
		}
	}
	return tx.Commit()
}
