// 渠道健康检查（M5）：定时向 active 渠道发最小请求，连续失败 N 次标记 down，
// 成功 1 次恢复 active。路由层已按 status='active' 过滤，down 渠道自动绕开。
package channel

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"

	"api-gateway/internal/store"
)

// HealthChecker 渠道健康检查器。
type HealthChecker struct {
	store       *store.Store
	chm         *Manager // 取渠道真实 key（用假 key 会被上游 401 误判）
	client      *http.Client
	maxFailures int
	failCount   map[int64]int // 渠道连续失败次数（内存态）
}

// NewHealthChecker 构造健康检查器。
func NewHealthChecker(st *store.Store, chm *Manager, timeout time.Duration, maxFailures int) *HealthChecker {
	return &HealthChecker{
		store:       st,
		chm:         chm,
		client:      &http.Client{Timeout: timeout},
		maxFailures: maxFailures,
		failCount:   map[int64]int{},
	}
}

// Start 启动定时检查（每 interval 一轮），ctx 取消时退出。
func (h *HealthChecker) Start(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	h.CheckOnce(ctx) // 启动时先跑一轮
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.CheckOnce(ctx)
		}
	}
}

// CheckOnce 检查一轮所有 active/down 渠道（disabled 与未识别类型跳过）。
func (h *HealthChecker) CheckOnce(ctx context.Context) {
	channels, err := h.store.ListChannels(ctx)
	if err != nil {
		slog.Error("健康检查：读取渠道失败", "error", err)
		return
	}
	for _, ch := range channels {
		// 只检查 active/down 且已识别类型的渠道（auto/空 是未探测的，跳过）
		if (ch.Status != "active" && ch.Status != "down") || ch.Type == "" || ch.Type == "auto" {
			delete(h.failCount, ch.ID)
			continue
		}
		if h.ping(ctx, ch) {
			if h.failCount[ch.ID] > 0 {
				slog.Info("健康检查：渠道恢复", "channel_id", ch.ID, "channel", ch.Name)
			}
			h.failCount[ch.ID] = 0
			if ch.Status != "active" {
				// down → 恢复 active（路由层自动重新纳入）
				if err := h.store.SetChannelStatus(ctx, ch.ID, "active"); err != nil {
					slog.Error("健康检查：恢复渠道失败", "channel_id", ch.ID, "error", err)
				} else {
					slog.Info("健康检查：渠道恢复为 active", "channel_id", ch.ID, "channel", ch.Name)
				}
			}
			continue
		}
		h.failCount[ch.ID]++
		if h.failCount[ch.ID] >= h.maxFailures {
			if err := h.store.SetChannelStatus(ctx, ch.ID, "down"); err != nil {
				slog.Error("健康检查：标记渠道 down 失败", "channel_id", ch.ID, "error", err)
			} else {
				slog.Warn("健康检查：渠道连续失败，标记 down", "channel_id", ch.ID, "channel", ch.Name, "连续失败", h.failCount[ch.ID])
			}
			delete(h.failCount, ch.ID)
		}
	}
}

// ping 用渠道真实 key 发最小请求（GET /v1/models），2xx 视为健康。
func (h *HealthChecker) ping(ctx context.Context, ch store.Channel) bool {
	key, _, err := h.chm.SelectKey(ctx, &ch)
	if err != nil {
		return false // 无可用 key（未配置/全冷却）视为不健康
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, normalizeBase(ch.BaseURL)+"/v1/models", nil)
	if err != nil {
		return false
	}
	switch ch.Type {
	case "anthropic":
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	default: // openai / responses
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
