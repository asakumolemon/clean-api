// Package config 负责加载网关自身配置：config.json + 环境变量覆盖。
package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// Config 网关自身配置。
type Config struct {
	Addr              string `json:"addr"`                    // HTTP 监听地址
	DBPath            string `json:"db_path"`                 // SQLite 文件路径
	SessionSecret     string `json:"session_secret"`          // 管理面 session 签名密钥，缺省随机生成（重启后失效）
	LogLevel          string `json:"log_level"`               // debug|info|warn|error
	LogRetentionDays  int    `json:"log_retention_days"`      // 请求日志保留天数，默认 7
	DefaultTimeoutSec int    `json:"default_timeout_seconds"` // 上游请求默认超时，默认 120
	AdminUsername     string `json:"admin_username"`          // 首次启动创建的管理员用户名
	AdminPassword     string `json:"admin_password"`          // 首次启动创建的管理员密码（建议用环境变量，勿入库）
	EncKey            string `json:"enc_key"`                 // 上游 API key 的 AES-GCM 加密密钥（M2 起使用）
	SessionSecure     bool   `json:"session_secure"`          // 管理面 cookie 加 Secure（仅 HTTPS 时开启，默认关）
}

// Load 从 JSON 文件读取配置并叠加默认值与环境变量覆盖。
func Load(path string) (*Config, error) {
	c := &Config{}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取配置 %s: %w", path, err)
		}
		if err := json.Unmarshal(data, c); err != nil {
			return nil, fmt.Errorf("解析配置 %s: %w", path, err)
		}
	}
	c.setDefaults()
	c.applyEnv()
	return c, nil
}

func (c *Config) setDefaults() {
	if c.Addr == "" {
		c.Addr = ":8080"
	}
	if c.DBPath == "" {
		c.DBPath = "data/gateway.db"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.LogRetentionDays == 0 {
		c.LogRetentionDays = 7
	}
	if c.DefaultTimeoutSec == 0 {
		c.DefaultTimeoutSec = 120
	}
	if c.AdminUsername == "" {
		c.AdminUsername = "admin"
	}
}

// applyEnv 用 GATEWAY_* 环境变量覆盖配置项。
func (c *Config) applyEnv() {
	get := func(key string) string { return os.Getenv("GATEWAY_" + key) }
	if v := get("ADDR"); v != "" {
		c.Addr = v
	}
	if v := get("DB_PATH"); v != "" {
		c.DBPath = v
	}
	if v := get("SESSION_SECRET"); v != "" {
		c.SessionSecret = v
	}
	if v := get("LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	if v := get("ADMIN_USERNAME"); v != "" {
		c.AdminUsername = v
	}
	if v := get("ADMIN_PASSWORD"); v != "" {
		c.AdminPassword = v
	}
	if v := get("ENC_KEY"); v != "" {
		c.EncKey = v
	}
	if v := get("SESSION_SECURE"); v != "" {
		c.SessionSecure = v == "1" || v == "true"
	}
}
