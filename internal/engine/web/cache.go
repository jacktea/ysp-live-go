package web

// 本文件实现原 ysp-engine.js 的 playlistCache（第 291 行起）的 Go 版：
//
//	const playlistCache = new Map();
//	if (cached && now - cached.time < 30000) return cached.playlist;
//	...
//	playlistCache.set(canonical, { playlist: finalPlaylist, time: now });
//
// 修复原项目 bug: 原 JS 的 playlistCache 是无上限 Map，频道数量多/被刷 slug 时
// 会无限增长；Go 版限制条目数（cacheMaxEntries=128）并按最近使用顺序淘汰（LRU）。

import (
	"container/list"
	"sync"
	"time"
)

// cacheEntry 一条缓存记录（对应原 JS {playlist, time}）。
type cacheEntry struct {
	key      string
	playlist string
	time     time.Time
}

// lruCache 带 TTL + 容量上限的 LRU 缓存。
type lruCache struct {
	mu       sync.Mutex
	maxItems int
	ttl      time.Duration
	order    *list.List               // 头部为最近使用
	items    map[string]*list.Element // key -> element(值为 *cacheEntry)
}

// newLRUCache 构造 LRU 缓存。
func newLRUCache(maxItems int, ttl time.Duration) *lruCache {
	if maxItems <= 0 {
		maxItems = cacheMaxEntries
	}
	return &lruCache{
		maxItems: maxItems,
		ttl:      ttl,
		order:    list.New(),
		items:    make(map[string]*list.Element, maxItems),
	}
}

// Get 取缓存（对应原 JS: cached && now-cached.time < 30000）。
func (c *lruCache) Get(key string, now time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return "", false
	}
	ent := el.Value.(*cacheEntry)
	if now.Sub(ent.time) >= c.ttl {
		// 过期即淘汰，避免过期项长期占用容量。
		c.order.Remove(el)
		delete(c.items, key)
		return "", false
	}
	c.order.MoveToFront(el)
	return ent.playlist, true
}

// Set 写缓存并在超容量时淘汰最久未使用的条目。
func (c *lruCache) Set(key, playlist string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		ent := el.Value.(*cacheEntry)
		ent.playlist = playlist
		ent.time = now
		c.order.MoveToFront(el)
		return
	}
	el := c.order.PushFront(&cacheEntry{key: key, playlist: playlist, time: now})
	c.items[key] = el
	for c.order.Len() > c.maxItems {
		back := c.order.Back()
		if back == nil {
			break
		}
		c.order.Remove(back)
		delete(c.items, back.Value.(*cacheEntry).key)
	}
}

// Len 当前条目数（测试与 /diag 用）。
func (c *lruCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
