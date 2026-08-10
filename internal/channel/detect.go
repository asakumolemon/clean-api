package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"api-gateway/internal/store"
)

// Detect 探测上游协议类型，命中即停。
// 顺序：GET /v1/models → OpenAI；POST /v1/messages → Anthropic；POST /v1/responses → Responses。
func (m *Manager) Detect(ctx context.Context, baseURL, apiKey string) (string, error) {
	base := normalizeBase(baseURL)

	if ok, _ := m.detectOpenAI(ctx, base, apiKey); ok {
		return "openai", nil
	}
	if ok, _ := m.detectAnthropic(ctx, base, apiKey); ok {
		return "anthropic", nil
	}
	if ok, _ := m.detectResponses(ctx, base, apiKey); ok {
		return "responses", nil
	}
	return "", fmt.Errorf("协议探测失败：候选协议（openai/anthropic/responses）均无有效响应，可在编辑时手动指定类型")
}

// detectOpenAI GET {base}/v1/models 返回模型数组即视为 OpenAI 兼容。
func (m *Manager) detectOpenAI(ctx context.Context, base, key string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := m.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var parsed struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Data == nil {
		return false, nil
	}
	return true, nil
}

// detectAnthropic POST {base}/v1/messages：路径存在（非 404/405）即视为 Anthropic。
func (m *Manager) detectAnthropic(ctx context.Context, base, key string) (bool, error) {
	payload := `{"model":"claude-sonnet-4-20250514","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/messages", strings.NewReader(payload))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := m.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	return resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed, nil
}

// detectResponses POST {base}/v1/responses：路径存在（非 404/405）即视为 Responses。
func (m *Manager) detectResponses(ctx context.Context, base, key string) (bool, error) {
	payload := `{"model":"gpt-4o-mini","input":"hi"}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/responses", strings.NewReader(payload))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := m.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	return resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed, nil
}

// SyncModels 拉取上游模型列表，返回模型名数组。
func (m *Manager) SyncModels(ctx context.Context, chType, baseURL, apiKey string) ([]string, error) {
	base := normalizeBase(baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	switch chType {
	case "anthropic":
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	default: // openai / responses
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("拉取模型列表失败: HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("解析模型列表失败: %w", err)
	}
	names := make([]string, 0, len(parsed.Data))
	for _, d := range parsed.Data {
		if d.ID != "" {
			names = append(names, d.ID)
		}
	}
	return names, nil
}

// probeCapabilities 探测单个模型的四项能力（system/tools/vision/json_mode），
// 每项发一次最小试调用，2xx 视为支持。anthropic 走 messages 协议，其余走 chat completions。
func (m *Manager) probeCapabilities(ctx context.Context, chType, baseURL, apiKey, model string) store.Capabilities {
	if chType == "anthropic" {
		return m.probeAnthropicCaps(ctx, baseURL, apiKey, model)
	}
	return m.probeOpenAICaps(ctx, baseURL, apiKey, model)
}

func (m *Manager) probeOpenAICaps(ctx context.Context, base, key, model string) store.Capabilities {
	probe := func(build func(map[string]any)) bool {
		p := map[string]any{
			"model":      model,
			"max_tokens": 1,
			"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		}
		build(p)
		body, _ := json.Marshal(p)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(body))
		if err != nil {
			return false
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := m.client.Do(req)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode >= 200 && resp.StatusCode < 300
	}
	var caps store.Capabilities
	caps.System = probe(func(p map[string]any) {
		p["messages"] = []any{
			map[string]any{"role": "system", "content": "be terse"},
			map[string]any{"role": "user", "content": "hi"},
		}
	})
	caps.Tools = probe(func(p map[string]any) {
		p["tools"] = []any{map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "get_weather", "description": "获取天气", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}},
		}}
	})
	caps.Vision = probe(func(p map[string]any) {
		p["messages"] = []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "what is this"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,iVBORw0KGgo="}},
		}}}
	})
	caps.JSONMode = probe(func(p map[string]any) {
		p["response_format"] = map[string]any{"type": "json_object"}
	})
	return caps
}

func (m *Manager) probeAnthropicCaps(ctx context.Context, base, key, model string) store.Capabilities {
	probe := func(build func(map[string]any)) bool {
		p := map[string]any{
			"model":      model,
			"max_tokens": 1,
			"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		}
		build(p)
		body, _ := json.Marshal(p)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/messages", bytes.NewReader(body))
		if err != nil {
			return false
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
		resp, err := m.client.Do(req)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode >= 200 && resp.StatusCode < 300
	}
	var caps store.Capabilities
	caps.System = probe(func(p map[string]any) { p["system"] = "be terse" })
	caps.Tools = probe(func(p map[string]any) {
		p["tools"] = []any{map[string]any{
			"name":         "get_weather",
			"description":  "获取天气",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
		}}
	})
	caps.Vision = probe(func(p map[string]any) {
		p["messages"] = []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "what is this"},
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgo="}},
		}}}
	})
	caps.JSONMode = probe(func(p map[string]any) {})
	return caps
}

// normalizeBase 规范化 base_url：去尾斜杠、补 https 前缀。
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
