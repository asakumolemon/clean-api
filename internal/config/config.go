// Package config 负责加载网关自身配置：config.json + 环境变量覆盖。
package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// Config 网关自身配置。
type Config struct {
	Addr                   string            `json:"addr"`                          // HTTP 监听地址
	DBPath                 string            `json:"db_path"`                       // SQLite 文件路径
	SessionSecret          string            `json:"session_secret"`                // 管理面 session 签名密钥，缺省随机生成（重启后失效）
	LogLevel               string            `json:"log_level"`                     // debug|info|warn|error
	LogRetentionDays       int               `json:"log_retention_days"`            // 请求日志保留天数，默认 7
	DefaultTimeoutSec      int               `json:"default_timeout_seconds"`       // 上游请求默认超时，默认 120
	AdminUsername          string            `json:"admin_username"`                // 首次启动创建的管理员用户名
	AdminPassword          string            `json:"admin_password"`                // 首次启动创建的管理员密码（建议用环境变量，勿入库）
	EncKey                 string            `json:"enc_key"`                       // 上游 API key 的 AES-GCM 加密密钥（M2 起使用）
	SessionSecure          bool              `json:"session_secure"`                // 管理面 cookie 加 Secure（仅 HTTPS 时开启，默认关）
	RoutingStrategy        string            `json:"routing_strategy"`              // 模型→渠道选择策略：random|round_robin（默认 random，M3 起使用）
	HealthCheckEnabled     bool              `json:"health_check_enabled"`          // 渠道健康检查开关（默认开，M5 起使用）
	HealthCheckIntervalSec int               `json:"health_check_interval_seconds"` // 健康检查间隔秒，默认 300
	HealthCheckMaxFailures int               `json:"health_check_max_failures"`     // 连续失败 N 次标记 down，默认 3
	KeyCooldownSec         int               `json:"key_cooldown_seconds"`          // 单 key 冷却时长秒（429/401 后，默认 60，M6 起使用）
	ProbeCapabilities      bool              `json:"probe_capabilities"`            // 添加渠道时是否对每个模型发最小试调用探测能力（默认关：省时省配额，能力用保守默认值，可在模型管理页手动调整；M6 起使用）
	ModelRedirects         map[string]string `json:"model_redirects"`               // 全局模型重定向：请求模型名 → 实际模型名（M5 起使用）
	CacheEnabled           *bool             `json:"cache_enabled"`                 // 响应缓存开关（默认开；重复的非流式请求直接返回缓存响应，省上游调用；M7 后起使用）
	CacheTTLSec            int               `json:"cache_ttl_seconds"`             // 响应缓存 TTL 秒（默认 300，M7 后起使用）
	Timezone               string            `json:"timezone"`                      // 管理面时间展示时区（IANA 名，如 Asia/Shanghai；默认服务器本地时区，M7 后起使用）
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
	if c.RoutingStrategy == "" {
		c.RoutingStrategy = "random"
	}
	if c.HealthCheckIntervalSec == 0 {
		c.HealthCheckIntervalSec = 300
	}
	if c.HealthCheckMaxFailures == 0 {
		c.HealthCheckMaxFailures = 3
	}
	if c.KeyCooldownSec == 0 {
		c.KeyCooldownSec = 60
	}
	if c.CacheEnabled == nil {
		enabled := true
		c.CacheEnabled = &enabled
	}
	if c.CacheTTLSec == 0 {
		c.CacheTTLSec = 300
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
	if v := get("ROUTING_STRATEGY"); v != "" {
		c.RoutingStrategy = v
	}
	if v := get("HEALTH_CHECK_ENABLED"); v != "" {
		c.HealthCheckEnabled = v == "1" || v == "true"
	}
	if v := get("HEALTH_CHECK_INTERVAL_SECONDS"); v != "" {
		fmt.Sscanf(v, "%d", &c.HealthCheckIntervalSec)
	}
	if v := get("HEALTH_CHECK_MAX_FAILURES"); v != "" {
		fmt.Sscanf(v, "%d", &c.HealthCheckMaxFailures)
	}
	if v := get("KEY_COOLDOWN_SECONDS"); v != "" {
		fmt.Sscanf(v, "%d", &c.KeyCooldownSec)
	}
	if v := get("PROBE_CAPABILITIES"); v != "" {
		c.ProbeCapabilities = v == "1" || v == "true"
	}
	if v := get("CACHE_ENABLED"); v != "" {
		enabled := v == "1" || v == "true"
		c.CacheEnabled = &enabled
	}
	if v := get("CACHE_TTL_SECONDS"); v != "" {
		fmt.Sscanf(v, "%d", &c.CacheTTLSec)
	}
	if v := get("TIMEZONE"); v != "" {
		c.Timezone = v
	}
}
