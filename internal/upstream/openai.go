// OpenAI 兼容上游适配器：IR → OpenAI Chat 请求，解析响应回 IR。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"api-gateway/internal/protocol"
)

// OpenAIAdapter 面向 OpenAI Chat 兼容上游（DeepSeek、OpenRouter 等）。
type OpenAIAdapter struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

// NewOpenAI 构造适配器。baseURL 自动补全规范形式（去尾斜杠、补 https）。
func NewOpenAI(baseURL, apiKey string, client *http.Client) *OpenAIAdapter {
	return &OpenAIAdapter{baseURL: normalizeBase(baseURL), apiKey: apiKey, client: client}
}

// Chat 非流式对话：IR → OpenAI 请求体 → POST {base}/v1/chat/completions → 解析回 IR。
func (a *OpenAIAdapter) Chat(ctx context.Context, req *protocol.ChatRequest) (*protocol.ChatResponse, error) {
	body, err := protocol.SerializeOpenAIChatRequest(req)
	if err != nil {
		return nil, &Error{Message: "序列化上游请求失败: " + err.Error()}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, &Error{Message: "构造上游请求失败: " + err.Error()}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, &Error{Message: "调用上游失败: " + err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, parseUpstreamError(resp.StatusCode, raw)
	}
	ir, err := protocol.ParseOpenAIChatResponse(raw)
	if err != nil {
		// 2xx 但响应无法解析：按可重试处理（可能是网关类上游的异常返回）
		return nil, &Error{Message: err.Error(), Retryable: true}
	}
	return ir, nil
}

// parseUpstreamError 从上游错误响应提取 {error:{message,type}}，取不到则用响应体摘要。
func parseUpstreamError(status int, raw []byte) *Error {
	msg, typ := "", ""
	var parsed struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &parsed) == nil && parsed.Error.Message != "" {
		msg, typ = parsed.Error.Message, parsed.Error.Type
	} else {
		msg = truncate(string(raw), 300)
	}
	if typ == "" {
		typ = defaultErrorType(status)
	}
	return &Error{StatusCode: status, Type: typ, Message: msg, Retryable: status >= 500}
}

// defaultErrorType 上游未给错误类型时的兜底分类。
func defaultErrorType(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusNotFound:
		return "model_not_found"
	case http.StatusBadRequest:
		return "invalid_request_error"
	}
	if status >= 500 {
		return "upstream_error"
	}
	return "invalid_request_error"
}

// normalizeBase 规范化 base_url：去尾斜杠、补 https 前缀（与 channel 包保持同一规则）。
func normalizeBase(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "/")
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		s = "https://" + s
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
