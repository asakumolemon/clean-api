// Package router 模型路由分发：模型名 → 渠道选择（策略）+ 故障切换 + key 轮换。
// M3 仅支持 OpenAI 兼容上游；Responses/Anthropic 上游为 M4 范围。
package router

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"api-gateway/internal/channel"
	"api-gateway/internal/protocol"
	"api-gateway/internal/store"
	"api-gateway/internal/upstream"
)

// ErrModelNotFound 没有可用渠道提供该模型（无路由 / 渠道 down / 模型禁用）。
var ErrModelNotFound = errors.New("model not found")

// ChatResult 非流式对话结果：响应 + 实际命中渠道（请求日志用）。
type ChatResult struct {
	Resp      *protocol.ChatResponse
	ChannelID int64
}

// Router 负责把一次对话请求分发到具体渠道。
type Router struct {
	store     *store.Store
	chm       *channel.Manager
	strategy  string            // random | round_robin（模型 → 渠道的选择策略，来自配置）
	redirects map[string]string // 全局模型重定向：请求模型名 → 实际模型名（M5）
	client    *http.Client

	rrMu sync.Mutex
	rr   map[string]int // 按模型名记数，round_robin 策略用
}

// New 构造路由器。redirects 为 nil 时不启用全局重定向。
func New(st *store.Store, chm *channel.Manager, strategy string, timeout time.Duration, redirects map[string]string) *Router {
	if strategy == "" {
		strategy = "random"
	}
	return &Router{
		store:     st,
		chm:       chm,
		strategy:  strategy,
		redirects: redirects,
		client:    &http.Client{Timeout: timeout},
		rr:        make(map[string]int),
	}
}

// applyRedirect 应用全局模型重定向（未命中映射时原样返回）。
func (r *Router) applyRedirect(model string) string {
	if r.redirects != nil {
		if target, ok := r.redirects[model]; ok && target != "" {
			return target
		}
	}
	return model
}

// Chat 分发一次非流式对话：
// 查可用渠道 → 按策略选起点 → 渠道内多 key 轮换（429/401 冷却换 key）→
// 5xx/网络错误换渠道重试（最多一轮）→ 4xx 直接透传。
// 返回响应与实际命中渠道 ID（请求日志用）。
func (r *Router) Chat(ctx context.Context, model string, req *protocol.ChatRequest) (*ChatResult, error) {
	model = r.applyRedirect(model)
	routes, seq, maxAttempts, err := r.resolveRoutes(ctx, model)
	if err != nil {
		return nil, err
	}

	attempts := 0
	var lastErr error = ErrModelNotFound
	var lastChannelID int64
	for _, idx := range seq {
		if attempts >= maxAttempts {
			break
		}
		route := routes[idx]
		lastChannelID = route.ChannelID
		ch := &store.Channel{ID: route.ChannelID, BalanceStrategy: route.BalanceStrategy}

	keyLoop:
		for attempts < maxAttempts {
			attempts++
			key, keyID, err := r.chm.SelectKey(ctx, ch)
			if err != nil {
				lastErr = err // 无可用 key（未配置 / 全部冷却中）→ 换渠道
				break keyLoop
			}
			if route.ChannelType != "" && route.ChannelType != "openai" {
				return &ChatResult{ChannelID: lastChannelID}, unsupportedTypeError(route.ChannelType)
			}
			ir := r.prepareReq(req, route)
			resp, err := upstream.NewOpenAI(route.BaseURL, key, r.client).Chat(ctx, &ir)
			if err == nil {
				return &ChatResult{Resp: resp, ChannelID: lastChannelID}, nil
			}
			lastErr = err
			var uperr *upstream.Error
			if !errors.As(err, &uperr) {
				return &ChatResult{ChannelID: lastChannelID}, err // 本地异常（key 解密失败等）直接返回
			}
			switch {
			case uperr.StatusCode == http.StatusTooManyRequests || uperr.StatusCode == http.StatusUnauthorized:
				r.chm.MarkKeyFailed(ctx, keyID) // 冷却该 key，继续换下一个 key
			case uperr.Retryable:
				break keyLoop // 5xx/网络错误：换渠道（下一轮）
			default:
				return &ChatResult{ChannelID: lastChannelID}, err // 4xx 不重试，直接透传
			}
		}
	}
	return &ChatResult{ChannelID: lastChannelID}, lastErr
}

// ChatStream 分发一次流式对话：语义与 Chat 相同，唯一差异——
// 重试只在首个事件 emit 前进行；一旦开始输出（emitted），任何错误直接中断，
// 避免客户端收到重复/错乱数据。返回最终渠道 ID（请求日志用，失败时为最后尝试渠道）。
func (r *Router) ChatStream(ctx context.Context, model string, req *protocol.ChatRequest, emit func(protocol.StreamEvent) error) (int64, error) {
	model = r.applyRedirect(model)
	var lastChannelID int64
	routes, seq, maxAttempts, err := r.resolveRoutes(ctx, model)
	if err != nil {
		return lastChannelID, err
	}

	attempts := 0
	emitted := false
	var lastErr error = ErrModelNotFound
	emitWrapped := func(ev protocol.StreamEvent) error {
		emitted = true
		return emit(ev)
	}
	for _, idx := range seq {
		if attempts >= maxAttempts {
			break
		}
		route := routes[idx]
		lastChannelID = route.ChannelID
		ch := &store.Channel{ID: route.ChannelID, BalanceStrategy: route.BalanceStrategy}

	keyLoop:
		for attempts < maxAttempts {
			attempts++
			key, keyID, err := r.chm.SelectKey(ctx, ch)
			if err != nil {
				lastErr = err // 无可用 key → 换渠道
				break keyLoop
			}
			if route.ChannelType != "" && route.ChannelType != "openai" {
				return lastChannelID, unsupportedTypeError(route.ChannelType)
			}
			ir := r.prepareReq(req, route)
			err = upstream.NewOpenAI(route.BaseURL, key, r.client).ChatStream(ctx, &ir, emitWrapped)
			if err == nil {
				return lastChannelID, nil
			}
			lastErr = err
			var uperr *upstream.Error
			if !errors.As(err, &uperr) {
				return lastChannelID, err // 本地异常直接返回
			}
			if emitted {
				return lastChannelID, err // 已开始输出：中断，不重试
			}
			switch {
			case uperr.StatusCode == http.StatusTooManyRequests || uperr.StatusCode == http.StatusUnauthorized:
				r.chm.MarkKeyFailed(ctx, keyID) // 冷却该 key，继续换下一个 key
			case uperr.Retryable:
				break keyLoop // 5xx/网络错误：换渠道
			default:
				return lastChannelID, err // 4xx 直接透传
			}
		}
	}
	return lastChannelID, lastErr
}

// resolveRoutes 查可用路由并给出渠道尝试序列（两轮轮换顺序）与总尝试上限。
func (r *Router) resolveRoutes(ctx context.Context, model string) ([]store.ModelRoute, []int, int, error) {
	routes, err := r.store.ListChannelsByModel(ctx, model)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("查询模型路由: %w", err)
	}
	if len(routes) == 0 {
		return nil, nil, 0, ErrModelNotFound
	}
	// 渠道尝试序列 = 两轮轮换顺序（覆盖 5xx/网络错误「重试 1 次换渠道」；单渠道时重试同一家）。
	order := r.channelOrder(model, len(routes))
	seq := make([]int, 0, 2*len(routes))
	seq = append(seq, order...)
	seq = append(seq, order...)
	// 总尝试上限：每渠道最多 2 次（1 次 key 轮换 + 1 次渠道重试），最多 4 次，防雪崩。
	maxAttempts := 2 * len(routes)
	if maxAttempts > 4 {
		maxAttempts = 4
	}
	return routes, seq, maxAttempts, nil
}

// prepareReq 复制请求并适配当前渠道：外部模型名 → 渠道内真实名；
// 上游不支持 system 时折叠 system 消息进首条 user。
func (r *Router) prepareReq(req *protocol.ChatRequest, route store.ModelRoute) protocol.ChatRequest {
	ir := *req
	ir.Model = route.ModelName
	if !route.Caps.System {
		protocol.FoldSystemIntoUser(&ir)
	}
	return ir
}

func unsupportedTypeError(chType string) error {
	return &upstream.Error{
		StatusCode: http.StatusNotImplemented,
		Type:       "not_implemented",
		Message:    fmt.Sprintf("渠道类型 %s 暂不支持（后续版本实现）", chType),
	}
}

// channelOrder 返回渠道下标的尝试顺序：
// random 随机打乱；round_robin 从模型计数起点循环（多请求轮换起点）。
func (r *Router) channelOrder(model string, n int) []int {
	perm := make([]int, n)
	for i := range perm {
		perm[i] = i
	}
	start := 0
	if r.strategy == "round_robin" {
		r.rrMu.Lock()
		start = r.rr[model] % n
		r.rr[model]++
		r.rrMu.Unlock()
	} else {
		rand.Shuffle(n, func(i, j int) { perm[i], perm[j] = perm[j], perm[i] })
	}
	out := make([]int, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, perm[(start+i)%n])
	}
	return out
}
