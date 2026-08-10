// Package upstream 上游适配器：把 IR 按上游协议序列化并发起调用。
// M3 仅实现 OpenAI 兼容适配器的非流式 Chat；ChatStream/Models/Ping 为 M4 扩展点。
package upstream

import (
	"context"
	"fmt"

	"api-gateway/internal/protocol"
)

// Upstream 上游服务统一接口。
type Upstream interface {
	// Chat 非流式对话。
	Chat(ctx context.Context, req *protocol.ChatRequest) (*protocol.ChatResponse, error)
	// M4 扩展：ChatStream(ctx, *ChatRequest, func(StreamEvent) error) error、Models()、Ping()
}

// Error 上游调用错误：状态码 + OpenAI 风格错误信息。
// Retryable=true 表示可换渠道重试（5xx/网络错误）；429/401 由路由器做 key 冷却后换 key 重试。
type Error struct {
	StatusCode int    // 0 表示网络层错误（无 HTTP 响应）
	Type       string // OpenAI 风格错误类型，尽量带上游原值
	Message    string
	Retryable  bool
}

func (e *Error) Error() string {
	if e.StatusCode > 0 {
		return fmt.Sprintf("上游 HTTP %d: %s", e.StatusCode, e.Message)
	}
	return e.Message
}
