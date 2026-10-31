// Package logx 复刻原项目的日志行为：带时间戳、按消息内容去抖、环形缓冲供 /diag 使用。
package logx

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// RingSize 环形缓冲容量（原 global_log_ring = deque(maxlen=120)）。
const RingSize = 120

type logger struct {
	mu        sync.Mutex
	ring      []string
	lastTimes map[string]time.Time
}

var std = &logger{
	ring:      make([]string, 0, RingSize),
	lastTimes: make(map[string]time.Time),
}

// Log 输出一行日志。minInterval<=0 时使用默认 1.5s 去抖（同一消息内容）。
//
// 原项目缺陷修复：原实现在 _last_log_times 超过 600 条时整体 clear()，
// 会顺带清掉仍在去抖窗口内的记录；这里改为按时间淘汰。
func Log(msg interface{}, minInterval float64) {
	s := strings.TrimSpace(fmt.Sprint(msg))
	if s == "" {
		return
	}
	now := time.Now()
	interval := minInterval
	if interval <= 0 {
		interval = 1.5
	}

	std.mu.Lock()
	if last, ok := std.lastTimes[s]; ok && now.Sub(last).Seconds() < interval {
		std.mu.Unlock()
		return
	}
	if len(std.lastTimes) > 600 {
		cutoff := now.Add(-time.Minute)
		for k, t := range std.lastTimes {
			if t.Before(cutoff) {
				delete(std.lastTimes, k)
			}
		}
	}
	std.lastTimes[s] = now
	line := fmt.Sprintf("[%s] %s", now.Format("15:04:05"), s)
	std.ring = append(std.ring, line)
	if len(std.ring) > RingSize {
		std.ring = std.ring[len(std.ring)-RingSize:]
	}
	std.mu.Unlock()

	fmt.Fprintln(os.Stdout, line)
}

// Logf 带格式化的日志。
func Logf(minInterval float64, format string, args ...interface{}) {
	Log(fmt.Sprintf(format, args...), minInterval)
}

// Recent 返回最近 n 条日志（用于 /diag）。
func Recent(n int) []string {
	std.mu.Lock()
	defer std.mu.Unlock()
	if n <= 0 || n > len(std.ring) {
		n = len(std.ring)
	}
	out := make([]string, n)
	copy(out, std.ring[len(std.ring)-n:])
	return out
}

// ResetTimes 清空去抖表（测试用）。
func ResetTimes() {
	std.mu.Lock()
	std.lastTimes = make(map[string]time.Time)
	std.mu.Unlock()
}
