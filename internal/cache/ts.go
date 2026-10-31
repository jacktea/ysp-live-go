// Package cache 迁移原项目的两类缓存：
//  1. TsSegmentCache —— TS 分片内存缓存（LRU + TTL，/engine_proxy 与 /proxy.ts 共用）
//  2. 通用 SWR 缓存语义由各引擎自行实现
package cache

import (
	"container/list"
	"sync"
	"time"
)

// Segment 一个缓存的分片。
type Segment struct {
	Data      []byte
	Headers   map[string]string
	ExpiresAt time.Time
}

// TsSegmentCache TS 分片内存缓存（原 TsSegmentCache）。
//
// 原项目缺陷修复：原实现在淘汰时把 key 同时保存在 deque 与 dict 中，
// 重复 set 同一个 key 会让 deque 里残留旧节点，导致 curr_bytes 统计偏大、
// 淘汰循环可能空转；这里改用 container/list 维护真正的 LRU。
type TsSegmentCache struct {
	mu       sync.Mutex
	maxBytes int64
	maxAge   time.Duration
	items    map[string]*list.Element
	order    *list.List // front = 最近使用
	curr     int64
	hits     int64
	misses   int64
}

type cacheItem struct {
	key string
	seg Segment
}

// NewTsSegmentCache 创建 TS 分片缓存，maxMB 为 0 表示不缓存。
func NewTsSegmentCache(maxMB int, maxAge time.Duration) *TsSegmentCache {
	if maxAge <= 0 {
		maxAge = 180 * time.Second
	}
	return &TsSegmentCache{
		maxBytes: int64(maxMB) * 1024 * 1024,
		maxAge:   maxAge,
		items:    make(map[string]*list.Element),
		order:    list.New(),
	}
}

// Get 取缓存；命中时把条目移到队首。
func (c *TsSegmentCache) Get(key string) ([]byte, map[string]string, bool) {
	if c == nil {
		return nil, nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		c.misses++
		return nil, nil, false
	}
	it := el.Value.(*cacheItem)
	if time.Now().After(it.seg.ExpiresAt) {
		c.removeElement(el)
		c.misses++
		return nil, nil, false
	}
	c.order.MoveToFront(el)
	c.hits++
	return it.seg.Data, it.seg.Headers, true
}

// Set 写入缓存，必要时按 LRU 淘汰。
func (c *TsSegmentCache) Set(key string, data []byte, headers map[string]string) {
	if c == nil || c.maxBytes <= 0 {
		return
	}
	if int64(len(data)) > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		it := el.Value.(*cacheItem)
		c.curr -= int64(len(it.seg.Data))
		c.order.MoveToFront(el)
		it.seg = Segment{Data: data, Headers: headers, ExpiresAt: time.Now().Add(c.maxAge)}
		c.curr += int64(len(data))
		return
	}
	for c.curr+int64(len(data)) > c.maxBytes && c.order.Len() > 0 {
		c.removeElement(c.order.Back())
	}
	el := c.order.PushFront(&cacheItem{key: key, seg: Segment{
		Data: data, Headers: headers, ExpiresAt: time.Now().Add(c.maxAge),
	}})
	c.items[key] = el
	c.curr += int64(len(data))
}

func (c *TsSegmentCache) removeElement(el *list.Element) {
	if el == nil {
		return
	}
	it := el.Value.(*cacheItem)
	c.curr -= int64(len(it.seg.Data))
	c.order.Remove(el)
	delete(c.items, it.key)
}

// Stats 缓存统计（原 stats）。
type Stats struct {
	Count   int     `json:"count"`
	SizeMB  float64 `json:"size_mb"`
	Hits    int64   `json:"hits"`
	Misses  int64   `json:"misses"`
	HitRate string  `json:"hit_rate"`
}

// Stats 返回当前统计信息。
func (c *TsSegmentCache) Stats() Stats {
	if c == nil {
		return Stats{HitRate: "0%"}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	total := c.hits + c.misses
	if total == 0 {
		total = 1
	}
	return Stats{
		Count:   c.order.Len(),
		SizeMB:  round2(float64(c.curr) / (1024 * 1024)),
		Hits:    c.hits,
		Misses:  c.misses,
		HitRate: formatPct(float64(c.hits) / float64(total) * 100),
	}
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

func formatPct(v float64) string {
	// 与原项目 "%.1f%%" 等价
	whole := int64(v * 10)
	neg := ""
	if whole < 0 {
		neg = "-"
		whole = -whole
	}
	return neg + itoa(whole/10) + "." + itoa(whole%10) + "%"
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
