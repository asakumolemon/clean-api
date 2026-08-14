// Package cache 响应缓存：进程内存 + TTL，按令牌隔离，只缓存非流式成功响应。
// 命中率统计不在此处计数——每请求的命中与否写进请求日志（request_logs.cache_hit），
// 由日志页/仪表盘聚合展示，缓存本身保持无状态。
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"api-gateway/internal/protocol"
)

// Manager 内存响应缓存。key 由调用方用 Key() 生成；值存 ChatResponse 的 JSON
// （IR 字段名序列化，同进程内往返自洽，usage 一并缓存）。
type Manager struct {
	mu      sync.Mutex
	items   map[string]entry
	ttl     time.Duration
	enabled bool
	max     int // 条目上限，超限时清过期后仍超则拒写（防内存膨胀）
}

type entry struct {
	data []byte
	exp  time.Time
}

// New 构造缓存管理器。enabled=false 时 Get 恒未命中、Set 不写入（配置关闭）。
// ttl <= 0 时用默认 300s。
func New(enabled bool, ttl time.Duration) *Manager {
	if ttl <= 0 {
		ttl = 300 * time.Second
	}
	return &Manager{items: map[string]entry{}, ttl: ttl, enabled: enabled, max: 5000}
}

// Enabled 缓存是否启用。
func (m *Manager) Enabled() bool { return m.enabled }

// Key 计算缓存键：tokenID + 请求体（令牌隔离，不同令牌不共享缓存；参数全量入键）。
func Key(tokenID int64, body []byte) string {
	h := sha256.New()
	h.Write([]byte(strconv.FormatInt(tokenID, 10)))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// Get 取缓存响应；过期条目视为未命中并惰性清除。
func (m *Manager) Get(key string) (*protocol.ChatResponse, bool) {
	if !m.enabled {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.items[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.exp) {
		delete(m.items, key)
		return nil, false
	}
	var resp protocol.ChatResponse
	if err := json.Unmarshal(e.data, &resp); err != nil {
		delete(m.items, key)
		return nil, false
	}
	return &resp, true
}

// Set 写入缓存（仅启用时）。条目数达上限时先清过期项，仍超则放弃写入。
func (m *Manager) Set(key string, resp *protocol.ChatResponse) {
	if !m.enabled || resp == nil {
		return
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.items) >= m.max {
		now := time.Now()
		for k, e := range m.items {
			if now.After(e.exp) {
				delete(m.items, k)
			}
		}
		if len(m.items) >= m.max {
			return
		}
	}
	m.items[key] = entry{data: data, exp: time.Now().Add(m.ttl)}
}
