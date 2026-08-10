// OpenAI 兼容上游适配器：IR → OpenAI Chat 请求，解析响应回 IR。
package upstream

import (
	"bufio"
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

// ChatStream 流式对话：IR → OpenAI 请求（stream:true）→ SSE 逐行解析 → StreamEvent 回调。
// emit 返回错误（客户端断连）时立即中止。HTTP 非 2xx 返回 *Error（同 Chat）。
func (a *OpenAIAdapter) ChatStream(ctx context.Context, req *protocol.ChatRequest, emit func(protocol.StreamEvent) error) error {
	ir := *req
	ir.Stream = true
	body, err := protocol.SerializeOpenAIChatRequest(&ir)
	if err != nil {
		return &Error{Message: "序列化上游请求失败: " + err.Error()}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return &Error{Message: "构造上游请求失败: " + err.Error()}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return &Error{Message: "调用上游失败: " + err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		return parseUpstreamError(resp.StatusCode, raw)
	}

	parser := protocol.NewOpenAIStreamParser()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20) // 单行（chunk）上限 1MB
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue // 忽略空行/注释行
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
