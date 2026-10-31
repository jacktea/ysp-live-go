package cache

import (
	"testing"
	"time"
)

func TestTSCacheHitMiss(t *testing.T) {
	c := NewTsSegmentCache(1, time.Minute)
	if _, _, ok := c.Get("missing"); ok {
		t.Fatal("expected miss")
	}
	c.Set("k", []byte("hello"), map[string]string{"Content-Type": "video/mp2t"})
	data, headers, ok := c.Get("k")
	if !ok || string(data) != "hello" || headers["Content-Type"] != "video/mp2t" {
		t.Fatalf("got %q ok=%v headers=%v", data, ok, headers)
	}
	st := c.Stats()
	if st.Hits != 1 || st.Misses != 1 || st.Count != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestTSCacheExpiry(t *testing.T) {
	c := NewTsSegmentCache(1, 10*time.Millisecond)
	c.Set("k", []byte("v"), nil)
	time.Sleep(20 * time.Millisecond)
	if _, _, ok := c.Get("k"); ok {
		t.Fatal("expected expired")
	}
}

func TestTSCacheLRUEviction(t *testing.T) {
	c := NewTsSegmentCache(1, time.Minute)
	// 1MB 上限，写入 3 个 400KB 应淘汰最旧的
	c.Set("a", make([]byte, 400*1024), nil)
	c.Set("b", make([]byte, 400*1024), nil)
	c.Set("c", make([]byte, 400*1024), nil)
	if _, _, ok := c.Get("a"); ok {
		t.Error("a should be evicted")
	}
	// b/c 仍在（c 触发淘汰 a）
	if _, _, ok := c.Get("c"); !ok {
		t.Error("c should be present")
	}
}

func TestTSCacheOversizeSkipped(t *testing.T) {
	c := NewTsSegmentCache(1, time.Minute)
	c.Set("big", make([]byte, 2<<20), nil)
	if _, _, ok := c.Get("big"); ok {
		t.Fatal("oversized entry should not be cached")
	}
}

func TestTSCacheReSetNoLeak(t *testing.T) {
	c := NewTsSegmentCache(4, time.Minute)
	// 反复覆盖同一个 key 不应让 size 统计虚高（原项目 deque 残留 bug）
	for i := 0; i < 100; i++ {
		c.Set("same", make([]byte, 100*1024), nil)
	}
	st := c.Stats()
	if st.Count != 1 {
		t.Fatalf("count = %d, want 1", st.Count)
	}
	if st.SizeMB > 0.2 {
		t.Fatalf("size = %v MB, want ~0.10 MB", st.SizeMB)
	}
}
