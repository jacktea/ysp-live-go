package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacktea/ysp-live-go/internal/crypto"
)

// 本文件用 httptest.Server 模拟央视频全部接口，离线验证：
//   - bootstrap 成功路径（app/start → cloud register → heartbeat → 三级取流 → m3u8）；
//   - 播放列表缓存与 403 过期重取；
//   - 冷却计数与换设备熔断；
//   - 高码率并发名额；
//   - 换台防抖与后台预取。

// 测试用固定凭据。
const (
	testSessionKey = "test-session-key-0123456789abcdef"
	testAppSecret  = "test-app-secret"
	testGUID       = "test-cloud-guid"
	m3u8Body       = "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:6\n#EXTINF:6.000,\nseg1.ts\n#EXTINF:6.000,\nseg2.ts\n"
)

// mockUpstream 央视频接口模拟器。
type mockUpstream struct {
	srv       *httptest.Server
	baseURL   string
	mu        sync.Mutex
	liveCalls int
	playlist  []int // 依次返回的状态码；用完后返回 200
	plHits    int
	// live400 为 true 时 live/v1/01 返回 400（用于换设备测试）。
	live400 bool
	// vdnMissingURL 为 true 时 VDN 不返回 url（用于错误路径测试）。
	vdnMissingURL bool
}

// newMockUpstream 创建模拟上游服务。
func newMockUpstream(t *testing.T) *mockUpstream {
	t.Helper()
	m := &mockUpstream{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.handle(w, r)
	}))
	m.baseURL = m.srv.URL
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockUpstream) config(dir string) Config {
	cfg := DefaultConfig()
	cfg.Timeout = 3
	cfg.RefreshInterval = 0
	cfg.ZapDebounce = 0
	cfg.PlaylistCacheTTL = 2
	cfg.CacheTTL = 60
	cfg.HeartbeatInterval = 60
	cfg.InitMaxAttempts = 1
	cfg.InitBackoff = 0.01
	cfg.InitBackoffMax = 0.02
	cfg.MetaJSON = filepath.Join(dir, "proxy-cache-state-rs.json")
	cfg.DeviceJSON = filepath.Join(dir, "device-state-rs.json")
	cfg.AppStartURL = m.baseURL + "/gsnw/api/app/start/v1/01"
	cfg.CloudGetURL = m.baseURL + "/cloudps/wssapi/device/v2/get"
	cfg.CloudRegisterURL = m.baseURL + "/cloudps/wssapi/device/v2/register"
	cfg.CollectReportURL = m.baseURL + "/cctvmobileinf/rest/cctv/receive/new/app"
	cfg.DictionaryURL = m.baseURL + "/gsnw/player/dictionary/obtain/v1"
	cfg.IndexURL = m.baseURL + "/gsnw/api/index/v1/01"
	cfg.ReportSingleURL = m.baseURL + "/das/app/data/message/single"
	cfg.LiveV101URL = m.baseURL + "/gsnw/api/live/v1/01"
	cfg.LiveV102URL = m.baseURL + "/gsnw/api/live/v1/02"
	cfg.VDNGetStreamURL = m.baseURL + "/cctvmobileinf/rest/cctv/videoliveUrl/getstream"
	cfg.DRMConfigURL = m.baseURL + "/gsnw/drm/config/obtain/v1"
	cfg.VersionConfigURL = m.baseURL + "/gsnw/version/config/obtain/v1"
	return cfg
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// handle 按路径分发。
func (m *mockUpstream) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/gsnw/api/app/start/v1/01":
		// 原实现：session_key 用 X-Fingerprint[:32] 作为 AES-GCM 密钥加密返回。
		xfp := r.Header.Get("X-Fingerprint")
		key := xfp
		if len(key) > 32 {
			key = key[:32]
		}
		enc, err := crypto.AESGCMEncryptB64(testSessionKey, key)
		if err != nil {
			writeJSON(w, 500, map[string]interface{}{"result": 500})
			return
		}
		writeJSON(w, 200, map[string]interface{}{"data": map[string]interface{}{"key": enc}})
	case "/cloudps/wssapi/device/v2/get", "/cloudps/wssapi/device/v2/register":
		writeJSON(w, 200, map[string]interface{}{
			"result": 0,
			"data":   map[string]interface{}{"guid": testGUID},
		})
	case "/das/app/data/message/single":
		writeJSON(w, 200, map[string]interface{}{"result": 0})
	case "/cctvmobileinf/rest/cctv/receive/new/app":
		writeJSON(w, 200, map[string]interface{}{"result": 0})
	case "/gsnw/player/dictionary/obtain/v1":
		writeJSON(w, 200, map[string]interface{}{"result": 0})
	case "/gsnw/api/index/v1/01":
		writeJSON(w, 200, map[string]interface{}{"result": 0})
	case "/gsnw/drm/config/obtain/v1", "/gsnw/version/config/obtain/v1":
		writeJSON(w, 200, map[string]interface{}{"result": 0})
	case "/gsnw/api/live/v1/01":
		m.mu.Lock()
		m.liveCalls++
		live400 := m.live400
		m.mu.Unlock()
		if live400 {
			writeJSON(w, 400, map[string]interface{}{"result": 400, "msg": "内部错误"})
			return
		}
		// 固定返回 36p 高码率条目（指向 mock 自己的 http m3u8，满足 http1 抓取限制）。
		writeJSON(w, 200, map[string]interface{}{
			"data": map[string]interface{}{
				"videoList": []interface{}{
					map[string]interface{}{"url": m.baseURL + "/live/test.m3u8", "rate": "36p", "rateName": "超高清"},
				},
			},
		})
	case "/gsnw/api/live/v1/02":
		enc, err := crypto.AESGCMEncryptB64(testAppSecret, testSessionKey)
		if err != nil {
			writeJSON(w, 500, map[string]interface{}{"result": 500})
			return
		}
		writeJSON(w, 200, map[string]interface{}{"data": map[string]interface{}{"appSecret": enc}})
	case "/cctvmobileinf/rest/cctv/videoliveUrl/getstream":
		m.mu.Lock()
		missing := m.vdnMissingURL
		m.mu.Unlock()
		if missing {
			writeJSON(w, 200, map[string]interface{}{"succeed": "0", "message": "内部错误"})
			return
		}
		writeJSON(w, 200, map[string]interface{}{
			"succeed": "1",
			"url":     m.baseURL + "/live/test.m3u8",
		})
	case "/live/test.m3u8":
		m.mu.Lock()
		m.plHits++
		status := 200
		if len(m.playlist) > 0 {
			status = m.playlist[0]
			m.playlist = m.playlist[1:]
		}
		m.mu.Unlock()
		if status != 200 {
			writeText(w, status, "expired")
			return
		}
		writeText(w, 200, m3u8Body)
	default:
		writeJSON(w, 404, map[string]interface{}{"result": 404, "path": r.URL.Path})
	}
}

func (m *mockUpstream) setPlaylistStatuses(statuses ...int) {
	m.mu.Lock()
	m.playlist = append([]int(nil), statuses...)
	m.mu.Unlock()
}

func (m *mockUpstream) playlistHits() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.plHits
}

func (m *mockUpstream) setLive400(v bool) {
	m.mu.Lock()
	m.live400 = v
	m.mu.Unlock()
}

// newTestResolver 创建指向 mock 的 Resolver（不启动后台 worker）。
func newTestResolver(t *testing.T, mut func(*Config)) (*Resolver, *mockUpstream) {
	t.Helper()
	m := newMockUpstream(t)
	cfg := m.config(t.TempDir())
	if mut != nil {
		mut(&cfg)
	}
	return New(cfg), m
}

// TestInitAndChannelPlaylist 覆盖 bootstrap 成功路径 + 三级取流 + m3u8 分片改写。
func TestInitAndChannelPlaylist(t *testing.T) {
	r, m := newTestResolver(t, nil)
	defer r.Stop()

	if err := r.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if !r.Ready() {
		t.Fatal("Init 成功后 Ready() 应为 true")
	}

	body, err := r.ChannelPlaylist(context.Background(), "cctv1", "192.168.1.10:8767")
	if err != nil {
		t.Fatalf("ChannelPlaylist() error = %v", err)
	}
	if !strings.Contains(body, "#EXTM3U") {
		t.Fatalf("播放列表缺少 #EXTM3U: %q", body)
	}
	if !strings.Contains(body, "http://192.168.1.10:8767/proxy.ts?=") {
		t.Fatalf("分片未改写为 /proxy.ts 代理地址: %q", body)
	}
	if strings.Contains(body, "seg1.ts\n") {
		t.Fatalf("仍存在未改写分片地址: %q", body)
	}

	// 二级缓存：第二次请求应命中 playlist_cache，不再访问上游 m3u8。
	hits := m.playlistHits()
	if _, err := r.ChannelPlaylist(context.Background(), "cctv1", "192.168.1.10:8767"); err != nil {
		t.Fatalf("第二次 ChannelPlaylist() error = %v", err)
	}
	if got := m.playlistHits(); got != hits {
		t.Fatalf("播放列表缓存未命中：上游 m3u8 请求数 %d -> %d", hits, got)
	}

	// 设备状态与健康数据。
	status := r.Status()
	if status.AppSession["cloud_guid_present"] != true {
		t.Fatalf("会话状态缺少 cloud guid: %#v", status.AppSession)
	}
	if _, ok := status.Cache["cctv1"]; !ok {
		t.Fatal("Status().Cache 缺少 cctv1")
	}
	slots, links, guid, sessionValid, hb := r.CacheStats()
	if slots != 1 || links != 1 {
		t.Fatalf("CacheStats slots=%d links=%d, want 1/1", slots, links)
	}
	if guid != testGUID || !sessionValid || hb != 1 {
		t.Fatalf("CacheStats guid=%q valid=%v hb=%d", guid, sessionValid, hb)
	}
	ds := r.DeviceStatus()
	if !ds.Ready || !ds.Valid || ds.Links != 1 || ds.LinksLimit != 6 || ds.GUID != testGUID || ds.Heartbeat != 1 {
		t.Fatalf("DeviceStatus 不符: %+v", ds)
	}
}

// TestChannelPlaylistRefreshExpired 覆盖上游 m3u8 403 后强制重取。
func TestChannelPlaylistRefreshExpired(t *testing.T) {
	r, m := newTestResolver(t, nil)
	defer r.Stop()
	m.setPlaylistStatuses(403, 200)

	body, err := r.ChannelPlaylist(context.Background(), "cctv2", "localhost:8767")
	if err != nil {
		t.Fatalf("ChannelPlaylist() error = %v", err)
	}
	if !strings.Contains(body, "#EXTM3U") {
		t.Fatalf("403 后重取失败: %q", body)
	}
	if hits := m.playlistHits(); hits != 2 {
		t.Fatalf("上游 m3u8 请求次数 = %d, want 2", hits)
	}
}

// TestRotateDeviceOnHTTP400 覆盖 400 换设备与熔断。
func TestRotateDeviceOnHTTP400(t *testing.T) {
	r, m := newTestResolver(t, nil)
	defer r.Stop()
	if err := r.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	before := deviceAndroidID(r)
	if before == "" {
		t.Fatal("注册后应有持久化设备身份")
	}
	m.setLive400(true)

	_, err := r.ChannelPlaylist(context.Background(), "cctv1", "localhost:8767")
	if err == nil {
		t.Fatal("400 时 ChannelPlaylist 应返回错误")
	}
	if !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("错误文本应包含 HTTP 400: %v", err)
	}

	// 换设备后设备文件应被重建（android_id 变化）。
	r.stateLock.Lock()
	wipes := r.state.deviceWipeCount
	r.stateLock.Unlock()
	if wipes != 1 {
		t.Fatalf("deviceWipeCount = %d, want 1", wipes)
	}
	after := deviceAndroidID(r)
	if before == after {
		t.Fatalf("换设备后 android_id 未变化: %s", after)
	}

	// 5 分钟内熔断：再次换设备应被拒绝。
	if r.rotateDeviceOn400("test") {
		t.Fatal("5 分钟内第二次换设备应被熔断拒绝")
	}

	// 失败应进入冷却。
	if !r.InCooldown("cctv1") {
		t.Fatal("失败后频道应进入冷却")
	}
}

// TestLinkQuota 覆盖高码率并发名额与 device link quota reached 错误文本。
func TestLinkQuota(t *testing.T) {
	r, _ := newTestResolver(t, func(c *Config) { c.LinksPerDevice = 1 })
	defer r.Stop()

	if _, err := r.ChannelPlaylist(context.Background(), "cctv1", "localhost:8767"); err != nil {
		t.Fatalf("首个频道解析失败: %v", err)
	}
	_, err := r.ChannelPlaylist(context.Background(), "cctv2", "localhost:8767")
	if err == nil {
		t.Fatal("名额耗尽时应返回错误")
	}
	if !strings.Contains(err.Error(), "device link quota reached") {
		t.Fatalf("错误文本应包含 device link quota reached: %v", err)
	}
}

// TestZapDebounce 覆盖换台防抖：首次返回空列表，防抖后后台预取完成。
func TestZapDebounce(t *testing.T) {
	r, _ := newTestResolver(t, func(c *Config) {
		c.ZapDebounce = 0.15
		c.LinksPerDevice = 6
	})
	defer r.Stop()

	body, err := r.ChannelPlaylist(context.Background(), "cctv3", "localhost:8767")
	if err != nil {
		t.Fatalf("防抖命中时不应返回错误: %v", err)
	}
	if body != "" {
		t.Fatalf("防抖命中应返回空播放列表，got %q", body)
	}

	// 等待后台预取把频道写入缓存。
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, ok := r.Status().Cache["cctv3"]
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("换台防抖后的后台预取未完成")
		}
		time.Sleep(20 * time.Millisecond)
	}

	body, err = r.ChannelPlaylist(context.Background(), "cctv3", "localhost:8767")
	if err != nil {
		t.Fatalf("预取后 ChannelPlaylist() error = %v", err)
	}
	if !strings.Contains(body, "#EXTM3U") {
		t.Fatalf("预取后应返回有效播放列表: %q", body)
	}
}

// TestVDNFailure 覆盖 VDN 不回 URL 的错误文案。
func TestVDNFailure(t *testing.T) {
	r, m := newTestResolver(t, nil)
	defer r.Stop()
	m.mu.Lock()
	m.vdnMissingURL = true
	m.mu.Unlock()

	_, err := r.ChannelPlaylist(context.Background(), "cctv4", "localhost:8767")
	if err == nil {
		t.Fatal("VDN 失败时应返回错误")
	}
	if !strings.Contains(err.Error(), "VDN did not return final URL") {
		t.Fatalf("错误文本不符: %v", err)
	}
}

// TestStatusAndWriteMeta 覆盖状态快照与 proxy-cache-state-rs.json 持久化/恢复。
func TestStatusAndWriteMeta(t *testing.T) {
	r, _ := newTestResolver(t, nil)
	defer r.Stop()

	if _, err := r.ChannelPlaylist(context.Background(), "cctv5", "localhost:8767"); err != nil {
		t.Fatalf("ChannelPlaylist() error = %v", err)
	}
	r.WriteMeta()

	cache, err := loadCache(r.cfg.MetaJSON)
	if err != nil {
		t.Fatalf("loadCache() error = %v", err)
	}
	entry, ok := cache["cctv5"]
	if !ok {
		t.Fatal("cache_detail 缺少 cctv5")
	}
	if entry.FinalURL == "" || len(entry.PlaybackHeaders) == 0 {
		t.Fatalf("恢复的条目不完整: %+v", entry)
	}

	// 新 Resolver 应从 meta json 恢复缓存。
	r2 := New(r.cfg)
	defer r2.Stop()
	if _, ok := r2.Status().Cache["cctv5"]; !ok {
		t.Fatal("新 Resolver 未从 meta json 恢复 cache_detail")
	}
}

// TestConcurrentAccess 并发调用对外方法，配合 -race 验证并发安全。
func TestConcurrentAccess(t *testing.T) {
	r, _ := newTestResolver(t, nil)
	defer r.Stop()

	if _, err := r.ChannelPlaylist(context.Background(), "cctv7", "localhost:8767"); err != nil {
		t.Fatalf("ChannelPlaylist() error = %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				_ = r.Status()
				_, _, _, _, _ = r.CacheStats()
				_ = r.InCooldown("cctv7")
				_ = r.Ready()
				_ = r.DeviceStatus()
				_, _ = r.ChannelPlaylist(context.Background(), "cctv7", "localhost:8767")
			}
		}()
	}
	wg.Wait()
}

// TestHeartbeatLoop 覆盖后台心跳循环（Start 启动，不阻塞）。
func TestHeartbeatLoop(t *testing.T) {
	r, _ := newTestResolver(t, func(c *Config) { c.HeartbeatInterval = 0.2 })
	defer r.Stop()

	if err := r.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	r.Start(context.Background())
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, _, _, _, hb := r.CacheStats()
		if hb >= 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("心跳次数未增长: %d", hb)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestInitFailure 覆盖 Init 失败路径（不阻塞，返回最后一次错误）。
func TestInitFailure(t *testing.T) {
	r, _ := newTestResolver(t, func(c *Config) {
		c.InitMaxAttempts = 2
		c.InitBackoff = 0.01
		c.AppStartURL = "http://127.0.0.1:1/gsnw/api/app/start/v1/01"
	})
	defer r.Stop()

	err := r.Init(context.Background())
	if err == nil {
		t.Fatal("上游不可用时应返回错误")
	}
	if r.Ready() {
		t.Fatal("初始化失败后 Ready() 应为 false")
	}
	_ = fmt.Sprint(err)
}

// deviceAndroidID 从状态快照里取持久化设备 android_id。
func deviceAndroidID(r *Resolver) string {
	if v, ok := r.Status().DeviceState["android_id"].(string); ok {
		return v
	}
	return ""
}
