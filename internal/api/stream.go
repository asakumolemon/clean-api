package api

import (
	"sync"
	"time"
)

// streamStats 流式请求的可观测性指标（请求日志用）。
// 只统计「已开始输出后」的指标：首 token 延迟（TTFB）与发送的 token 数，
// 用于区分「上游慢（TTFB 高/无首 token）」vs「客户端断（已开始输出后中断）」。
type streamStats struct {
	mu           sync.Mutex
	started      bool      // 是否已发出首事件（首个 chunk）
	firstChunkAt time.Time // 首事件写出时刻（TTFB 基准）
	chunkBytes   int       // 首个 chunk 的字节数（粗略判断首 token 大小）
	sentTokens   int       // 已发送的 text_delta token 数（无细粒度 token 统计时按字符近似）
	totalOutput  int       // 总输出 token（done 事件用量）
}

func newStreamStats() *streamStats {
	return &streamStats{}
}

// markFirstChunk 记录首事件写出（只记录一次）。chunkBytes 为首个 chunk 的字节数。
func (st *streamStats) markFirstChunk(chunkBytes int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.started {
		return
	}
	st.started = true
	st.firstChunkAt = time.Now()
	st.chunkBytes = chunkBytes
}

// addSentToken 累加已发送的 token 数（客户端实际收到的文本增量）。
func (st *streamStats) addSentToken(n int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sentTokens += n
}

// setTotalOutput 记录 done 事件的输出 token 用量（成功日志用）。
func (st *streamStats) setTotalOutput(n int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.totalOutput = n
}

// ttfbMS 首 token 延迟毫秒数；未开始输出返回 0。
func (st *streamStats) ttfbMS(start time.Time) int64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.started {
		return 0
	}
	return st.firstChunkAt.Sub(start).Milliseconds()
}

// sentTokensMS 返回已发送 token 数（线程安全读）。
func (st *streamStats) sentTokensCount() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.sentTokens
}
