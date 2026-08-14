// Package api 对外 HTTP 入口（/v1/*）。M4 起支持三个协议入口（流式+非流式）：
// OpenAI Chat / Responses / Anthropic Messages，内部统一转 IR 再路由到上游。
// M5 起：每请求生成 request_id（X-Request-Id）并异步写入请求日志（失败可丢）。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"api-gateway/internal/auth"
	"api-gateway/internal/cache"
	"api-gateway/internal/protocol"
	"api-gateway/internal/router"
	"api-gateway/internal/store"
	"api-gateway/internal/upstream"
)

// entryProtocol 入口协议（决定请求解析、响应序列化与错误格式）。
type entryProtocol int

const (
	entryOpenAI entryProtocol = iota
	entryAnthropic
	entryResponses
)

// Server 对外 /v1 API。
type Server struct {
	store  *store.Store
	router *router.Router
	cache  *cache.Manager
}

func New(s *store.Store, r *router.Router, c *cache.Manager) *Server {
	return &Server{store: s, router: r, cache: c}
}

// ChatCompletions POST /v1/chat/completions（OpenAI Chat 入口）。
func (s *Server) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	s.handle(w, r, entryOpenAI)
}

// Messages POST /v1/messages（Anthropic Messages 入口，Claude Code / Cursor）。
func (s *Server) Messages(w http.ResponseWriter, r *http.Request) {
	s.handle(w, r, entryAnthropic)
}

// Responses POST /v1/responses（OpenAI Responses 入口，新版 OpenAI SDK）。
func (s *Server) Responses(w http.ResponseWriter, r *http.Request) {
	s.handle(w, r, entryResponses)
}

// handle 公共流水线：鉴权上下文 → 入口解析为 IR → 模型白名单校验 →
// 非流式（router.Chat + 出口序列化）或流式（SSE 头 + router.ChatStream + 出口编码）。
func (s *Server) handle(w http.ResponseWriter, r *http.Request, entry entryProtocol) {
	tok := auth.TokenFromContext(r.Context())
	if tok == nil {
		s.writeEntryError(w, entry, http.StatusUnauthorized, "unauthorized", "缺少鉴权上下文")
		return
	}
	requestID := protocol.RandHex(12)
	w.Header().Set("X-Request-Id", requestID)
	start := time.Now()

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		s.writeEntryError(w, entry, http.StatusBadRequest, "invalid_request", "读取请求体失败")
		return
	}
	if len(body) == 0 {
		s.writeEntryError(w, entry, http.StatusBadRequest, "invalid_request", "请求体不能为空")
		return
	}
	req, err := parseEntry(entry, body)
	if err != nil {
		s.writeEntryError(w, entry, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !auth.CheckModelAllowed(tok, req.Model) {
		s.writeEntryError(w, entry, http.StatusForbidden, "model_not_allowed", "model not allowed")
		return
	}
	// 空对话拦截：白名单校验之后再查，避免把鉴权语义让位给校验错误。
	// 空 messages 的上游会 400（Empty input messages），且空对话本身无法补全，
	// 入口直接给出各协议格式的 400 更清晰。
	if len(req.Messages) == 0 {
		s.writeEntryError(w, entry, http.StatusBadRequest, "invalid_request", "请求消息为空（messages/input 不能为空）")
		return
	}

	// 响应缓存（非流式）：命中直接返回缓存响应，不调上游。key 含 tokenID 与请求体
	// （令牌隔离 + 参数全量入键），白名单已先行校验；model_redirects 为静态映射，
	// 同一对外名恒映射同一实际名，用对外名作 key 安全。命中仍写请求日志（cache_hit=1）。
	cacheKey := ""
	if !req.Stream && s.cache.Enabled() {
		cacheKey = cache.Key(tok.ID, body)
		if resp, ok := s.cache.Get(cacheKey); ok {
			if out, err := serializeResponse(entry, resp); err == nil {
				s.logRequest(r, req.Model, 0, start, requestID, http.StatusOK, "",
					resp.Usage.PromptTokens, resp.Usage.CompletionTokens, true)
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(out)
				return
			}
		}
	}

	if !req.Stream {
		s.handleNonStream(w, r, entry, req, requestID, start, cacheKey)
		return
	}
	s.handleStream(w, r, entry, req, requestID, start)
}

func parseEntry(entry entryProtocol, body []byte) (*protocol.ChatRequest, error) {
	switch entry {
	case entryAnthropic:
		return protocol.ParseAnthropicMessagesRequest(body)
	case entryResponses:
		return protocol.ParseResponsesRequest(body)
	default:
		return protocol.ParseOpenAIChatRequest(body)
	}
}

// handleNonStream 非流式：router.Chat → 出口序列化 → 200 JSON。成功后写响应缓存（cacheKey 非空时）。
func (s *Server) handleNonStream(w http.ResponseWriter, r *http.Request, entry entryProtocol, req *protocol.ChatRequest, requestID string, start time.Time, cacheKey string) {
	res, err := s.router.Chat(r.Context(), req.Model, req)
	if err != nil {
		chID := int64(0)
		if res != nil {
			chID = res.ChannelID // 错误时也携带实际尝试渠道（请求日志用）
		}
		s.logRequest(r, req.Model, chID, start, requestID, errorStatus(err), err.Error(), 0, 0, false)
		s.writeRouteError(w, entry, err, req.Model)
		return
	}
	out, err := serializeResponse(entry, res.Resp)
	if err != nil {
		s.writeEntryError(w, entry, http.StatusInternalServerError, "internal_error", "序列化响应失败: "+err.Error())
		return
	}
	if cacheKey != "" {
		s.cache.Set(cacheKey, res.Resp) // 只缓存成功响应
	}
	s.logRequest(r, req.Model, res.ChannelID, start, requestID, http.StatusOK, "",
		res.Resp.Usage.PromptTokens, res.Resp.Usage.CompletionTokens, false)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// serializeResponse 按入口协议序列化非流式响应。
func serializeResponse(entry entryProtocol, resp *protocol.ChatResponse) ([]byte, error) {
	switch entry {
	case entryAnthropic:
		return protocol.SerializeAnthropicMessagesResponse(resp)
	case entryResponses:
		return protocol.SerializeResponsesResponse(resp)
	default:
		return protocol.SerializeOpenAIChatResponse(resp)
	}
}

// handleStream 流式：SSE 头 → 首个事件时写 200 → router.ChatStream（emit 写出口事件）。
// 首事件前的路由错误仍可返回正常 HTTP 错误状态；已开始输出后的错误写协议 error 事件。
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request, entry entryProtocol, req *protocol.ChatRequest, requestID string, start time.Time) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	writer := newStreamWriter(entry, w, req.Model)
	started := false
	var usage *protocol.Usage
	chID, err := s.router.ChatStream(r.Context(), req.Model, req, func(ev protocol.StreamEvent) error {
		if !started {
			started = true
			w.WriteHeader(http.StatusOK)
		}
		if ev.Type == protocol.EventDone && ev.Usage != nil {
			usage = ev.Usage // 缓存 done 事件的用量，成功日志用
		}
		return writer.WriteEvent(ev)
	})
	if err != nil {
		if !started {
			// 路由层错误（模型不存在/上游 4xx 等）：HTTP 头未发，可写正常 JSON 错误
			status := errorStatus(err)
			s.logRequest(r, req.Model, chID, start, requestID, status, err.Error(), 0, 0, false)
			s.writeRouteError(w, entry, err, req.Model)
			return
		}
		_ = writer.WriteError(err) // 流中错误：协议 error 事件
		s.logRequest(r, req.Model, chID, start, requestID, 0, err.Error(), 0, 0, false)
		return
	}
	_ = writer.Finish() // 收尾（未收到 done 时补发结束事件，防客户端挂起）
	// 流式成功补请求日志（REQUIREMENTS §2.6 每请求记录；用量取自 done 事件）
	pt, ct := 0, 0
	if usage != nil {
		pt, ct = usage.PromptTokens, usage.CompletionTokens
	}
	s.logRequest(r, req.Model, chID, start, requestID, http.StatusOK, "", pt, ct, false)
}

// logRequest 异步写请求日志（失败忽略，不影响主流程，符合 REQUIREMENTS §2.6）。
func (s *Server) logRequest(r *http.Request, model string, channelID int64, start time.Time, requestID string, status int, errText string, promptTokens, completionTokens int, cacheHit bool) {
	tok := auth.TokenFromContext(r.Context())
	user := auth.UserFromContext(r.Context())
	var tokID, userID int64
	if tok != nil {
		tokID = tok.ID
	}
	if user != nil {
		userID = user.ID
	} else if tok != nil {
		userID = tok.UserID
	}
	log := &store.RequestLog{
		TS:               time.Now().UTC(),
		RequestID:        requestID,
		TokenID:          tokID,
		UserID:           userID,
		Model:            model,
		ChannelID:        channelID,
		Status:           status,
		LatencyMS:        time.Since(start).Milliseconds(),
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		CacheHit:         cacheHit,
		Error:            truncateLog(errText, 300),
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.store.InsertRequestLog(ctx, log)
	}()
}

// errorStatus 路由错误 → 日志用状态码。
func errorStatus(err error) int {
	if errors.Is(err, router.ErrModelNotFound) {
		return http.StatusNotFound
	}
	var uperr *upstream.Error
	if errors.As(err, &uperr) {
		if uperr.StatusCode > 0 {
			return uperr.StatusCode
		}
		return http.StatusBadGateway // 网络层错误
	}
	return http.StatusInternalServerError
}

func truncateLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// newStreamWriter 按入口构造流式出口编码器（ID 由网关合成，客户端不校验格式）。
func newStreamWriter(entry entryProtocol, w http.ResponseWriter, model string) streamWriter {
	id := ""
	switch entry {
	case entryAnthropic:
		id = "msg_" + protocol.RandHex(8)
		return protocol.NewAnthropicStreamWriter(w, id, model)
	case entryResponses:
		id = "resp_" + protocol.RandHex(8)
		return protocol.NewResponsesStreamWriter(w, id, model, 0)
	default:
		id = "chatcmpl-" + protocol.RandHex(8)
		return protocol.NewOpenAIChatStreamWriter(w, id, model, 0)
	}
}

// streamWriter 出口流编码器接口（WriteEvent/WriteError/Finish）。
type streamWriter interface {
	WriteEvent(ev protocol.StreamEvent) error
	WriteError(err error) error
	Finish() error
}

// writeRouteError 路由错误 → 入口协议格式的错误响应。
func (s *Server) writeRouteError(w http.ResponseWriter, entry entryProtocol, err error, model string) {
	if errors.Is(err, router.ErrModelNotFound) {
		typ := "model_not_found"
		if entry == entryAnthropic {
			typ = "not_found_error" // Anthropic 风格错误类型
		}
		s.writeEntryError(w, entry, http.StatusNotFound, typ, "model not found: "+model)
		return
	}
	var uperr *upstream.Error
	if errors.As(err, &uperr) {
		status := uperr.StatusCode
		if status <= 0 {
			status = http.StatusBadGateway // 网络层错误（无 HTTP 响应）→ 502
		}
		typ := uperr.Type
		if entry == entryAnthropic {
			typ = anthropicErrorType(status) // Anthropic 错误类型按状态码映射
		}
		s.writeEntryError(w, entry, status, typ, uperr.Message)
		return
	}
	s.writeEntryError(w, entry, http.StatusInternalServerError, "internal_error", err.Error())
}

// writeEntryError 按入口写错误响应：OpenAI/Responses 用 {error:{...}}，Anthropic 用 {type:"error",error:{...}}。
func (s *Server) writeEntryError(w http.ResponseWriter, entry entryProtocol, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if entry == entryAnthropic {
		if errType == "" {
			errType = anthropicErrorType(status)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    errType,
				"message": msg,
			},
		})
		return
	}
	if errType == "" {
		errType = "invalid_request_error"
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    errType,
		},
	})
}

// anthropicErrorType Anthropic 风格错误类型（按状态码）。
func anthropicErrorType(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	}
	if status >= 500 {
		return "api_error"
	}
	return "invalid_request_error"
}

// Models GET /v1/models：返回启用模型的对外名列表（alias 非空用 alias），按名去重，
// 并按当前令牌白名单过滤——只返回该令牌可用的模型，供客户端模型选择器使用。
func (s *Server) Models(w http.ResponseWriter, r *http.Request) {
	tok := auth.TokenFromContext(r.Context())
	if tok == nil {
		auth.WriteAPIError(w, http.StatusUnauthorized, "unauthorized", "缺少鉴权上下文")
		return
	}
	models, err := s.store.ListModels(r.Context())
	if err != nil {
		auth.WriteAPIError(w, http.StatusInternalServerError, "internal_error", "查询模型失败: "+err.Error())
		return
	}
	seen := map[string]bool{}
	data := []map[string]any{}
	for _, m := range models {
		if !m.Enabled {
			continue
		}
		name := m.Name
		if m.Alias != "" {
			name = m.Alias
		}
		if !auth.CheckModelAllowed(tok, name) {
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		data = append(data, map[string]any{"id": name, "object": "model", "owned_by": "gateway"})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}
