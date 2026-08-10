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
			error TEXT
		);`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func now() time.Time { return time.Now().UTC() }
