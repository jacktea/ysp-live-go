// Package ratelimit 迁移原项目 ApiFlowLimiter：所有上游 API 调用共享最小间隔。
package ratelimit

import (
	"math/rand"
	"sync"
	"time"
)

// FlowLimiter 最小间隔 + 随机抖动的全局流控（原 ApiFlowLimiter）。
type FlowLimiter struct {
	mu       sync.Mutex
	minDelay time.Duration
	jitter   time.Duration
	last     time.Time
}

// New 创建流控器，minIntervalMs 最小间隔毫秒，jitterMs 随机抖动上限毫秒。
func New(minIntervalMs, jitterMs int) *FlowLimiter {
	return &FlowLimiter{
		minDelay: time.Duration(minIntervalMs) * time.Millisecond,
		jitter:   time.Duration(jitterMs) * time.Millisecond,
	}
}

// Wait 阻塞直到可以发起下一次调用。
func (l *FlowLimiter) Wait() {
	if l == nil {
		return
	}
	l.mu.Lock()
	target := l.minDelay
	if l.jitter > 0 {
		target += time.Duration(rand.Int63n(int64(l.jitter) + 1))
	}
	since := time.Since(l.last)
	if !l.last.IsZero() && since < target {
		wait := target - since
		l.mu.Unlock()
		time.Sleep(wait)
		l.mu.Lock()
	}
	l.last = time.Now()
	l.mu.Unlock()
}

// Global 全局流控实例（原 global_flow_limiter，700ms + 250ms 抖动）。
var Global = New(700, 250)
