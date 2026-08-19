// Package store 提供 SQLite 持久层：建库建表 + 各实体的 CRUD。
// 使用 database/sql + modernc.org/sqlite（纯 Go，无 CGO）。
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Store 封装 SQLite 连接。
type Store struct {
	db *sql.DB
}

// Open 打开（或创建）SQLite 数据库，并执行建表迁移。
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建数据目录: %w", err)
		}
	}
	dsn := fmt.Sprintf("%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化数据库: %w", err)
	}
	return s, nil
}

// Close 关闭数据库连接。
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users(
			id INTEGER PRIMARY KEY,
			username TEXT UNIQUE NOT NULL,
			password_hash TEXT NOT NULL,
			role TEXT NOT NULL DEFAULT 'user',
			created_at DATETIME
		);`,
		`CREATE TABLE IF NOT EXISTS tokens(
			id INTEGER PRIMARY KEY,
			user_id INTEGER REFERENCES users(id),
			name TEXT,
			key_hash TEXT UNIQUE NOT NULL,
			model_whitelist TEXT,
			allow_all INTEGER DEFAULT 0,
			enabled INTEGER DEFAULT 1,
			` + "`group`" + ` TEXT DEFAULT '',
			created_at DATETIME,
			last_used_at DATETIME
		);`,
		`CREATE TABLE IF NOT EXISTS channels(
			id INTEGER PRIMARY KEY,
			name TEXT,
			type TEXT,
			base_url TEXT,
			status TEXT DEFAULT 'active',
			weight INTEGER DEFAULT 1,
			balance_strategy TEXT DEFAULT 'random',
			created_at DATETIME
		);`,
		`CREATE TABLE IF NOT EXISTS channel_keys(
			id INTEGER PRIMARY KEY,
			channel_id INTEGER REFERENCES channels(id),
			key_enc TEXT NOT NULL,
			cooldown_until DATETIME
		);`,
		`CREATE TABLE IF NOT EXISTS models(
			id INTEGER PRIMARY KEY,
			channel_id INTEGER REFERENCES channels(id),
			name TEXT,
			enabled INTEGER DEFAULT 1,
			alias TEXT,
			capabilities TEXT,
			capability_override TEXT,
			last_sync_at DATETIME,
			UNIQUE(channel_id, name)
		);`,
		`CREATE TABLE IF NOT EXISTS request_logs(
			id INTEGER PRIMARY KEY,
			ts DATETIME,
			request_id TEXT,
			token_id INTEGER,
			user_id INTEGER,
			model TEXT,
			channel_id INTEGER,
			status INTEGER,
			latency_ms INTEGER,
			prompt_tokens INTEGER,
				completion_tokens INTEGER,
				cache_hit INTEGER DEFAULT 0,
				token_name TEXT,
				token_group TEXT,
				error TEXT
		);`,
		// 请求日志查询索引（M5 日志页筛选/分页用）
		`CREATE INDEX IF NOT EXISTS idx_request_logs_ts ON request_logs(ts);`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_token ON request_logs(token_id);`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_status ON request_logs(status);`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}
	// 老库补列：ensureColumn 幂等，可在同一迁移里链式调用。
	if err := s.ensureColumn("request_logs", "cache_hit", "cache_hit INTEGER DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn("request_logs", "token_name", "token_name TEXT"); err != nil {
		return err
	}
	if err := s.ensureColumn("request_logs", "token_group", "token_group TEXT"); err != nil {
		return err
	}
	if err := s.ensureColumn("tokens", "group", "`group` TEXT DEFAULT ''"); err != nil {
		return err
	}
	// 旧日志按当前令牌补齐可取得的快照；已删除令牌的历史记录保留为空。
	_, err := s.db.Exec(`
		UPDATE request_logs
		SET token_name = COALESCE((SELECT name FROM tokens WHERE tokens.id = request_logs.token_id), token_name, ''),
		    token_group = COALESCE((SELECT ` + "`group`" + ` FROM tokens WHERE tokens.id = request_logs.token_id), token_group, '')
		WHERE COALESCE(token_name, '') = '' AND COALESCE(token_group, '') = ''`)
	return err
}

// ensureColumn 检测表是否已有指定列，缺失则 ALTER TABLE ADD COLUMN 补上（幂等）。
// PRAGMA table_info 列：cid, name, type, notnull, dflt_value, pk。
func (s *Store) ensureColumn(table, column, ddl string) error {
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	_, err = s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + ddl)
	return err
}

func now() time.Time { return time.Now().UTC() }
