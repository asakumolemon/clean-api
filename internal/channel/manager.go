// Package channel 渠道管理：协议自动识别、模型列表同步、能力探测、多 key 轮换与冷却。
package channel

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"api-gateway/internal/crypto"
	"api-gateway/internal/store"
)

// 默认探测参数。
const (
	ProbeTimeout   = 5 * time.Minute  // 单渠道完整探测总超时
	RequestTimeout = 30 * time.Second // 单次探测请求超时
	DefaultCooldown = 60 * time.Second // 单 key 默认冷却时长（429/401 后，M6 起可配）
)

// ProbeStatus 探测进度（内存态，供管理页轮询渲染）。
type ProbeStatus struct {
	ChannelID  int64
	Running    bool
	Step       string
	Type       string
	ModelCount int
	Done       bool
	Failed     bool
	Message    string
	UpdatedAt  time.Time
}

// Manager 渠道探测与 key 管理。
type Manager struct {
	store    *store.Store
	enc      *crypto.Cipher
	client   *http.Client
	cooldown time.Duration // 单 key 冷却时长（MarkKeyFailed 用，默认 DefaultCooldown）
	probeCaps bool         // 是否对每个模型发最小试调用探测能力（默认关，config probe_capabilities）

	mu     sync.Mutex
	probes map[int64]*ProbeStatus

	rrMu sync.Mutex
	rr   map[int64]int
}

func NewManager(st *store.Store, enc *crypto.Cipher, timeout time.Duration) *Manager {
	return &Manager{
		store:    st,
		enc:      enc,
		client:   &http.Client{Timeout: timeout},
		cooldown: DefaultCooldown,
		probes:   make(map[int64]*ProbeStatus),
		rr:       make(map[int64]int),
	}
}

// SetCooldown 设置 key 冷却时长（M6：来自配置 key_cooldown_seconds）。
func (m *Manager) SetCooldown(d time.Duration) {
	if d > 0 {
		m.cooldown = d
	}
}

// SetProbeCapabilities 开关能力探测（M6：默认关闭，避免免费模型配额被打爆；
// 需要时由配置 probe_capabilities 开启，或渠道页手动触发）。
func (m *Manager) SetProbeCapabilities(b bool) {
	m.probeCaps = b
}

// defaultCapabilities 能力探测关闭时的保守默认值：
// 主流模型普遍支持 system 与工具调用，视觉/JSON mode 默认关（可在模型管理页手动调整）。
func defaultCapabilities(chType string) store.Capabilities {
	return store.Capabilities{System: true, Tools: true}
}

// --- 探测进度 ---

func (m *Manager) updateStatus(chID int64, mut func(*ProbeStatus)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.probes[chID]
	if st == nil {
		st = &ProbeStatus{ChannelID: chID}
		m.probes[chID] = st
	}
	mut(st)
	st.UpdatedAt = time.Now().UTC()
}

func (m *Manager) setStep(chID int64, step string) {
	m.updateStatus(chID, func(st *ProbeStatus) { st.Step = step })
}

// GetStatus 查询渠道最近一次探测进度（未探测过返回 nil）。
func (m *Manager) GetStatus(chID int64) *ProbeStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.probes[chID]
}

// StartProbe 异步触发完整探测（协议识别 → 模型同步 → 能力探测）。
// 同一渠道正在探测中时忽略重复触发。
func (m *Manager) StartProbe(chID int64) {
	m.mu.Lock()
	if st := m.probes[chID]; st != nil && st.Running {
		m.mu.Unlock()
		return
	}
	m.probes[chID] = &ProbeStatus{ChannelID: chID, Running: true, Step: "排队中…", UpdatedAt: time.Now().UTC()}
	m.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), ProbeTimeout)
		defer cancel()
		if err := m.ProbeChannel(ctx, chID); err != nil {
			slog.Error("渠道探测失败", "channel_id", chID, "error", err)
			m.updateStatus(chID, func(st *ProbeStatus) {
				st.Running = false
				st.Done = true
				st.Failed = true
				st.Message = err.Error()
			})
		}
	}()
}

// ProbeChannel 同步执行完整探测并更新渠道类型与模型库。
func (m *Manager) ProbeChannel(ctx context.Context, chID int64) error {
	m.setStep(chID, "开始探测…")
	ch, err := m.store.GetChannel(ctx, chID)
	if err != nil {
		return err
	}
	key, _, err := m.SelectKey(ctx, ch)
	if err != nil {
		return err
	}

	// 1. 协议识别（仅 type=auto 时）
	chType := ch.Type
	if chType == "" || chType == "auto" {
		m.setStep(chID, "协议识别中…")
		detected, err := m.Detect(ctx, ch.BaseURL, key)
		if err != nil {
			return err
		}
		chType = detected
		if err := m.store.UpdateChannel(ctx, ch.ID, ch.Name, chType, ch.BaseURL, ch.Status, ch.Weight, ch.BalanceStrategy); err != nil {
			return err
		}
		m.setStep(chID, "已识别为 "+chType)
	}

	// 2. 拉取模型列表
	m.setStep(chID, "同步模型列表…")
	names, err := m.SyncModels(ctx, chType, ch.BaseURL, key)
	if err != nil {
		return err
	}

	// 3. 能力：默认按渠道类型给保守值（不消耗上游配额）；
	//    probe_capabilities 开启时逐个最小试调用探测。
	var caps map[string]store.Capabilities
	capNote := "能力使用默认值，可在模型管理页调整"
	if m.probeCaps {
		m.setStep(chID, "探测模型能力…")
		caps = make(map[string]store.Capabilities, len(names))
		for _, name := range names {
			m.setStep(chID, "探测能力 "+name)
			caps[name] = m.probeCapabilities(ctx, chType, ch.BaseURL, key, name)
		}
		capNote = "已完成能力探测"
	} else {
		def := defaultCapabilities(chType)
		caps = make(map[string]store.Capabilities, len(names))
		for _, name := range names {
			caps[name] = def
		}
	}

	added, err := m.store.SyncModels(ctx, chID, caps, time.Now().UTC())
	if err != nil {
		return err
	}

	m.updateStatus(chID, func(st *ProbeStatus) {
		st.Running = false
		st.Done = true
		st.Failed = false
		st.Type = chType
		st.ModelCount = len(names)
		st.Message = "探测完成：识别为 " + chType + "，同步 " + itoa(len(names)) + " 个模型（新增 " + itoa(added) + "），" + capNote
	})
	return nil
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

// StartCapabilitiesProbe 异步触发该渠道全部模型的能力探测（渠道页手动按钮，M6）。
// 同一渠道正在探测中时忽略重复触发。
func (m *Manager) StartCapabilitiesProbe(chID int64) {
	m.mu.Lock()
	if st := m.probes[chID]; st != nil && st.Running {
		m.mu.Unlock()
		return
	}
	m.probes[chID] = &ProbeStatus{ChannelID: chID, Running: true, Step: "排队中…", UpdatedAt: time.Now().UTC()}
	m.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), ProbeTimeout)
		defer cancel()
		if err := m.ProbeCapabilitiesOnly(ctx, chID); err != nil {
			slog.Error("能力探测失败", "channel_id", chID, "error", err)
			m.updateStatus(chID, func(st *ProbeStatus) {
				st.Running = false
				st.Done = true
				st.Failed = true
				st.Message = err.Error()
			})
		}
	}()
}

// ProbeCapabilitiesOnly 仅对该渠道全部模型执行能力探测（读取库中已有模型，逐个最小试调用）。
func (m *Manager) ProbeCapabilitiesOnly(ctx context.Context, chID int64) error {
	m.setStep(chID, "探测模型能力…")
	ch, err := m.store.GetChannel(ctx, chID)
	if err != nil {
		return err
	}
	key, _, err := m.SelectKey(ctx, ch)
	if err != nil {
		return err
	}
	models, err := m.store.ListModelsByChannel(ctx, chID)
	if err != nil {
		return err
	}
	if len(models) == 0 {
		m.updateStatus(chID, func(st *ProbeStatus) {
			st.Running = false
			st.Done = true
			st.Failed = false
			st.Message = "渠道暂无模型，请先同步模型列表"
		})
		return nil
	}
	caps := make(map[string]store.Capabilities, len(models))
	for _, md := range models {
		m.setStep(chID, "探测能力 "+md.Name)
		caps[md.Name] = m.probeCapabilities(ctx, ch.Type, ch.BaseURL, key, md.Name)
	}
	if _, err := m.store.SyncModels(ctx, chID, caps, time.Now().UTC()); err != nil {
		return err
	}
	m.updateStatus(chID, func(st *ProbeStatus) {
		st.Running = false
		st.Done = true
		st.Failed = false
		st.Message = "能力探测完成：" + itoa(len(models)) + " 个模型已更新"
	})
	return nil
}

// SelectKey 按渠道策略选择一个未冷却的 key 并解密，返回明文与 key ID。
// 策略：random（默认）| round_robin。
func (m *Manager) SelectKey(ctx context.Context, ch *store.Channel) (string, int64, error) {
	keys, err := m.store.ListChannelKeys(ctx, ch.ID)
	if err != nil {
		return "", 0, err
	}
	if len(keys) == 0 {
		return "", 0, errors.New("渠道未配置 API key")
	}
	now := time.Now().UTC()
	avail := make([]store.ChannelKey, 0, len(keys))
	for _, k := range keys {
		if !k.CooldownUntil.Valid || k.CooldownUntil.Time.Before(now) {
			avail = append(avail, k)
		}
	}
	if len(avail) == 0 {
		return "", 0, errors.New("渠道所有 key 均在冷却中")
	}
	var pick store.ChannelKey
	switch ch.BalanceStrategy {
	case "round_robin":
		m.rrMu.Lock()
		i := m.rr[ch.ID] % len(avail)
		m.rr[ch.ID]++
		m.rrMu.Unlock()
		pick = avail[i]
	default: // random
		pick = avail[rand.Intn(len(avail))]
	}
	plain, err := m.enc.Decrypt(pick.KeyEnc)
	if err != nil {
		return "", 0, err
	}
	return plain, pick.ID, nil
}

// MarkKeyFailed 将 key 标记冷却（429/401 时调用，M3 路由使用）。
func (m *Manager) MarkKeyFailed(ctx context.Context, keyID int64) {
	until := time.Now().UTC().Add(m.cooldown)
	_ = m.store.SetKeyCooldown(ctx, keyID, &until)
}

// MaskKey 返回 key 掩码用于页面展示（不解密完整明文）。
func (m *Manager) MaskKey(keyEnc string) string {
	plain, err := m.enc.Decrypt(keyEnc)
	if err != nil {
		return "(解密失败)"
	}
	return maskSecret(plain)
}

// EncryptKey 加密明文 key 用于入库。
func (m *Manager) EncryptKey(plain string) (string, error) {
	return m.enc.Encrypt(plain)
}

func maskSecret(s string) string {
	if len(s) <= 8 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + "…" + s[len(s)-4:]
}
