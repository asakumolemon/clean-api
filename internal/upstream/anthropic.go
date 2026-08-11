// Anthropic Messages 兼容上游适配器：IR → Anthropic 请求 → 解析响应回 IR。
// 支持非流式与流式（SSE）；请求头用 x-api-key + anthropic-version。
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"

	"api-gateway/internal/protocol"
)

// anthropicVersion Anthropic API 版本头（与 channel 探测一致）。
const anthropicVersion = "2023-06-01"

// AnthropicAdapter 面向 Anthropic Messages 兼容上游（Claude 及各类 Anthropic 兼容网关）。
type AnthropicAdapter struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

// NewAnthropic 构造适配器。baseURL 自动补全规范形式。
func NewAnthropic(baseURL, apiKey string, client *http.Client) *AnthropicAdapter {
	return &AnthropicAdapter{baseURL: normalizeBase(baseURL), apiKey: apiKey, client: client}
}

// Chat 非流式对话：IR → Anthropic 请求体 → POST {base}/v1/messages → 解析回 IR。
func (a *AnthropicAdapter) Chat(ctx context.Context, req *protocol.ChatRequest) (*protocol.ChatResponse, error) {
	body, err := protocol.SerializeAnthropicMessagesRequest(req)
	if err != nil {
		return nil, &Error{Message: "序列化上游请求失败: " + err.Error()}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, &Error{Message: "构造上游请求失败: " + err.Error()}
	}
	a.setHeaders(httpReq)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, &Error{Message: "调用上游失败: " + err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, parseUpstreamError(resp.StatusCode, raw)
	}
	ir, err := protocol.ParseAnthropicMessagesResponse(raw)
	if err != nil {
		// 2xx 但响应无法解析：按可重试处理（可能是网关类上游的异常返回）
		return nil, &Error{Message: err.Error(), Retryable: true}
	}
	return ir, nil
}

// ChatStream 流式对话：IR → Anthropic 请求（stream:true）→ SSE 逐行解析 → StreamEvent 回调。
// emit 返回错误（客户端断连）时立即中止。Anthropic SSE 无 [DONE]，以 message_stop 收尾。
func (a *AnthropicAdapter) ChatStream(ctx context.Context, req *protocol.ChatRequest, emit func(protocol.StreamEvent) error) error {
	ir := *req
	ir.Stream = true
	body, err := protocol.SerializeAnthropicMessagesRequest(&ir)
	if err != nil {
		return &Error{Message: "序列化上游请求失败: " + err.Error()}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return &Error{Message: "构造上游请求失败: " + err.Error()}
	}
	a.setHeaders(httpReq)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return &Error{Message: "调用上游失败: " + err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		return parseUpstreamError(resp.StatusCode, raw)
	}

	parser := protocol.NewAnthropicStreamParser()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20) // 单行（chunk）上限 1MB
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue // 忽略空行/注释行/event: 行
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		evs, err := parser.Feed(data)
		if err != nil {
			return &Error{Message: "解析上游流式响应失败: " + err.Error()}
		}
		for _, ev := range evs {
			if err := emit(ev); err != nil {
				return err // 客户端断连等
			}
		}
	}
	if err := sc.Err(); err != nil {
		return &Error{Message: "读取上游流式响应失败: " + err.Error(), Retryable: true}
	}
	return nil
}

// setHeaders 设置 Anthropic Messages 请求头（x-api-key + 版本 + JSON）。
func (a *AnthropicAdapter) setHeaders(httpReq *http.Request) {
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)
}
