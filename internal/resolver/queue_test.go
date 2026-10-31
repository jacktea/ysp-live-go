package resolver

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件覆盖后台刷新队列与 SWR（stale-while-refresh）语义，
// 对应原 enqueue_background_refresh / refresh_worker_loop / ensure_channel 的 SWR 分支。

func TestEnqueueBackgroundRefreshRejectedWhenDisabled(t *testing.T) {
	r, _ := newTestResolver(t, func(c *Config) { c.BackgroundRefreshQueueLimit = 0 })
	defer r.Stop()

	status, reason := r.enqueueBackgroundRefresh("cctv1")
	if status != "rejected" {
		t.Fatalf("队列禁用时应 rejected, got %q", status)
	}
	if !strings.Contains(reason, "background refresh queue disabled") {
		t.Fatalf("拒绝原因文案不符: %q", reason)
	}

	q := r.queue
	q.mu.Lock()
	dropped := q.droppedTotal
	q.mu.Unlock()
	if dropped != 1 {
		t.Fatalf("droppedTotal = %d, want 1", dropped)
	}
}

func TestEnqueueBackgroundRefreshQueueFull(t *testing.T) {
	r, _ := newTestResolver(t, func(c *Config) { c.BackgroundRefreshQueueLimit = 1 })
	defer r.Stop()

	if status, _ := r.enqueueBackgroundRefresh("cctv1"); status != "enqueued" {
		t.Fatalf("首个频道应入队, got %q", status)
	}
	// 同一频道重复入队应视为 already_active。
	if status, _ := r.enqueueBackgroundRefresh("cctv1"); status != "already_active" {
		t.Fatalf("重复入队应 already_active, got %q", status)
	}
	status, reason := r.enqueueBackgroundRefresh("cctv2")
	if status != "rejected" {
		t.Fatalf("队列满时应 rejected, got %q", status)
	}
	if !strings.Contains(reason, "background refresh queue full") {
		t.Fatalf("拒绝原因文案不符: %q", reason)
	}
}

// TestRefreshWorkerLoopProcessesQueue 启动 worker 后入队的频道应被后台刷新。
func TestRefreshWorkerLoopProcessesQueue(t *testing.T) {
	r, _ := newTestResolver(t, func(c *Config) { c.BackgroundRefreshQueueLimit = 4 })
	defer r.Stop()
	if err := r.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	r.Start(context.Background())

	if status, _ := r.enqueueBackgroundRefresh("cctv8"); status != "enqueued" {
		t.Fatalf("入队失败: %q", status)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		r.queue.mu.Lock()
		completed := r.queue.completedTotal
		r.queue.mu.Unlock()
		if completed > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("后台刷新 worker 未处理队列")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, ok := r.Status().Cache["cctv8"]; !ok {
		t.Fatal("后台刷新未写入频道缓存")
	}
}

// TestStaleWhileRefresh 过期但仍在 SWR 窗口内的条目应直接返回并排入后台刷新。
func TestStaleWhileRefresh(t *testing.T) {
	r, m := newTestResolver(t, func(c *Config) {
		c.StaleWhileRefreshTTL = 120
		c.BackgroundRefreshQueueLimit = 0 // 禁用队列，便于断言 rejected 分支
	})
	defer r.Stop()

	// 先正常解析一次，再把条目改成"刚过期"。
	if _, err := r.ChannelPlaylist(context.Background(), "cctv9", "localhost:8767"); err != nil {
		t.Fatalf("首次解析失败: %v", err)
	}
	r.stateLock.Lock()
	entry := r.state.cache["cctv9"]
	entry.ExpiresAt = nowF64() - 1
	entry.LastRefreshFailed = 0
	r.stateLock.Unlock()
	_ = m
	body, err := r.ChannelPlaylist(context.Background(), "cctv9", "localhost:8767")
	if err != nil {
		t.Fatalf("SWR 窗口内不应报错: %v", err)
	}
	if !strings.Contains(body, "#EXTM3U") {
		t.Fatalf("SWR 应返回旧播放列表: %q", body)
	}
	// 队列被禁用时，ensure_channel 应把 reject 原因写回条目。
	r.stateLock.Lock()
	reason := r.state.cache["cctv9"].LastRefreshError
	r.stateLock.Unlock()
	if !strings.Contains(reason, "background refresh queue disabled") {
		t.Fatalf("reject 原因未写回条目: %q", reason)
	}
}

// TestCooldownBlocksEnsureChannel 冷却期内直接返回冷却错误、不再访问上游。
func TestCooldownBlocksEnsureChannel(t *testing.T) {
	r, m := newTestResolver(t, nil)
	defer r.Stop()

	r.stateLock.Lock()
	r.state.channelFailures["cctv1"] = &channelFailure{failedAt: nowF64(), err: "boom", failCount: 1}
	r.stateLock.Unlock()

	before := m.playlistHits()
	_, err := r.ChannelPlaylist(context.Background(), "cctv1", "localhost:8767")
	if err == nil {
		t.Fatal("冷却期内应返回错误")
	}
	if !strings.Contains(err.Error(), "channel cctv1 in cooldown") {
		t.Fatalf("冷却错误文案不符: %v", err)
	}
	if m.playlistHits() != before {
		t.Fatal("冷却期内不应访问上游")
	}

	// 清冷却后应可正常解析。
	r.clearChannelCooldown("cctv1")
	if r.InCooldown("cctv1") {
		t.Fatal("清除后不应处于冷却")
	}
}

// TestMarkChannelFailureAccumulates 失败次数递增 + 冷却时长分级。
func TestMarkChannelFailureAccumulates(t *testing.T) {
	r, _ := newTestResolver(t, nil)
	defer r.Stop()

	for i := 1; i <= 3; i++ {
		r.MarkChannelFailure("cctv10", errorf("boom %d", i))
		_, _, failCount, ok := r.getChannelFailureInfo("cctv10")
		if !ok || failCount != i {
			t.Fatalf("第 %d 次失败后 failCount = %d (ok=%v), want %d", i, failCount, ok, i)
		}
		if !r.InCooldown("cctv10") {
			t.Fatalf("第 %d 次失败后应处于冷却", i)
		}
	}

	// MarkChannelFailure 应丢弃该频道的缓存，避免继续下发失效地址。
	r.stateLock.Lock()
	_, cached := r.state.cache["cctv10"]
	r.stateLock.Unlock()
	if cached {
		t.Fatal("MarkChannelFailure 后应丢弃频道缓存")
	}
}

// TestLoadCacheInvalid 覆盖 load_cache 的容错分支（文件缺失/非法 JSON/无 cache_detail）。
func TestLoadCacheInvalid(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadCache(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("文件缺失应返回错误")
	}

	bad := filepath.Join(dir, "bad.json")
	if err := writeFileSync(bad, []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCache(bad); err == nil {
		t.Fatal("非法 JSON 应返回错误")
	}

	noDetail := filepath.Join(dir, "nodetail.json")
	if err := writeFileSync(noDetail, []byte(`{"cache":{}}`)); err != nil {
		t.Fatal(err)
	}
	cache, err := loadCache(noDetail)
	if err != nil {
		t.Fatalf("无 cache_detail 时不应报错: %v", err)
	}
	if len(cache) != 0 {
		t.Fatalf("无 cache_detail 应返回空 map, got %d", len(cache))
	}
}

// TestNewRecoversFromCorruptMeta 启动时 meta json 损坏不应 panic，只是没有缓存。
func TestNewRecoversFromCorruptMeta(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.MetaJSON = filepath.Join(dir, "meta.json")
	cfg.DeviceJSON = filepath.Join(dir, "dev.json")
	if err := writeFileSync(cfg.MetaJSON, []byte("[]")); err != nil {
		t.Fatal(err)
	}
	r := New(cfg)
	defer r.Stop()
	if len(r.Status().Cache) != 0 {
		t.Fatal("损坏的 meta json 不应恢复出缓存")
	}
}

// TestStatusJSONKeys 校验状态快照的关键 JSON 字段名与原实现一致（运维/诊断依赖）。
func TestStatusJSONKeys(t *testing.T) {
	r, _ := newTestResolver(t, nil)
	defer r.Stop()
	info := r.Status()
	m := statusToMap(info)
	for _, key := range []string{
		"channels", "cache_ttl_seconds", "stale_while_refresh_ttl_seconds",
		"refresh_error_cooldown_seconds", "playlist_cache_ttl_seconds",
		"background_refresh_queue_limit", "http_workers", "http_queue_limit",
		"identity_reset_error_threshold", "identity_reset_cooldown_seconds",
		"refresh_interval_seconds", "heartbeat_interval_seconds",
		"heartbeat_ttl_guard_seconds", "session_ttl_seconds", "meta_json", "device_json",
		"device_state", "background_refresh_queue", "http_queue", "app_session", "cache",
		"playlist_cache", "device_profile_pool_size",
	} {
		if _, ok := m[key]; !ok {
			t.Errorf("状态快照缺少字段 %q", key)
		}
	}
	if _, ok := m["app_session"].(map[string]interface{}); !ok {
		t.Errorf("app_session 应为对象: %T", m["app_session"])
	}
}

// TestNoDeadlockWithBackgroundWorkers 在心跳/刷新 worker 运行的同时并发访问所有对外方法，
// 验证锁顺序一致（controlLock → stateLock → queue.mu）且不会死锁。
func TestNoDeadlockWithBackgroundWorkers(t *testing.T) {
	r, _ := newTestResolver(t, func(c *Config) {
		c.HeartbeatInterval = 120 // 心跳不会真的触发多次，但循环在跑
		c.BackgroundRefreshQueueLimit = 8
		c.StaleWhileRefreshTTL = 60
	})
	defer r.Stop()
	if err := r.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	r.Start(context.Background())

	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 10; j++ {
					r.Status()
					r.WriteMeta()
					r.CacheStats()
					r.DeviceStatus()
					r.MarkChannelFailure("cctv11", errorf("boom"))
					r.InCooldown("cctv11")
					r.clearChannelCooldown("cctv11")
					_, _ = r.enqueueBackgroundRefresh("cctv12")
					_, _ = r.ChannelPlaylist(context.Background(), "cctv13", "localhost:8767")
				}
			}()
		}
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("并发访问出现死锁（30s 未完成）")
	}
}
