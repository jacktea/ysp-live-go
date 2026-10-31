package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jacktea/ysp-live-go/internal/cache"
	"github.com/jacktea/ysp-live-go/internal/engine"
	"github.com/jacktea/ysp-live-go/internal/gateway"
	"github.com/jacktea/ysp-live-go/internal/model"
)

// fakeDevice 设备层桩。
type fakeDevice struct {
	ready    bool
	cooldown map[string]bool
	playlist map[string]string
	failWith error
	failures int
}

func (f *fakeDevice) Ready() bool { return f.ready }
func (f *fakeDevice) ChannelPlaylist(ctx context.Context, channel, host string) (string, error) {
	if f.failWith != nil {
		return "", f.failWith
	}
	return f.playlist[channel], nil
}
func (f *fakeDevice) InCooldown(channel string) bool               { return f.cooldown[channel] }
func (f *fakeDevice) MarkChannelFailure(channel string, err error) { f.failures++ }

// fakeFallback 1080p 层桩。
type fakeFallback struct {
	playlist map[string]string
	touched  map[string]int
}

func (f *fakeFallback) Playlist(ctx context.Context, channel string) (string, error) {
	if p, ok := f.playlist[channel]; ok {
		return p, nil
	}
	return "", engine.ErrEngine
}
func (f *fakeFallback) Touch(channel string) {
	if f.touched != nil {
		f.touched[channel]++
	}
}

// 供 playlist / health 测试的最小装配。
func newTestServer(t *testing.T) (*Server, *fakeDevice, *fakeFallback) {
	t.Helper()
	dir := model.NewChannelDirectory()
	dev := &fakeDevice{
		ready:    true,
		cooldown: map[string]bool{},
		playlist: map[string]string{"cctv1": "#EXTM3U\n#EXTINF:6,\nhttp://x/a.ts\n"},
	}
	fb := &fakeFallback{
		playlist: map[string]string{"cctv13": "#EXTM3U\n#EXTINF:6,\nhttp://y/b.ts\n"},
		touched:  map[string]int{},
	}
	deviceChans := map[string]bool{"cctv1": true}
	chain := gateway.NewChain(dev, fb, nil, deviceChans)
	srv := New(Options{
		Chain:     chain,
		Dir:       dir,
		TSCache:   cache.NewTsSegmentCache(8, time.Minute),
		StartTime: time.Now(),
	})
	return srv, dev, fb
}

func do(t *testing.T, srv *Server, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.Host = "192.168.1.10:8767"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func TestIndex(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := do(t, srv, http.MethodGet, "/")
	if w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"ysp-live IPTV 直播网关运行中", "/all.m3u", "/health", "/diag"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestAllM3U(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := do(t, srv, http.MethodGet, "/all.m3u")
	if w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "mpegurl") {
		t.Errorf("content-type = %q", ct)
	}
	if n := strings.Count(w.Body.String(), "#EXTINF:"); n != 64 {
		t.Errorf("channels = %d", n)
	}
	if !strings.Contains(w.Body.String(), "http://192.168.1.10:8767/cctv1.m3u8") {
		t.Error("missing host-based url")
	}
}

func TestAllM3UGroupAndK4(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := do(t, srv, http.MethodGet, "/all.m3u?group=%E5%8D%AB%E8%A7%86")
	if n := strings.Count(w.Body.String(), "#EXTINF:"); n != 34 {
		t.Errorf("卫视 = %d", n)
	}
	w = do(t, srv, http.MethodGet, "/all.m3u?k4=1")
	if n := strings.Count(w.Body.String(), "#EXTINF:"); n != 3 {
		t.Errorf("4K = %d", n)
	}
}

func TestHealth(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := do(t, srv, http.MethodGet, "/health")
	if w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("json: %v", err)
	}
	if payload["ok"] != true {
		t.Error("ok should be true")
	}
	if payload["version"] != Version {
		t.Errorf("version = %v", payload["version"])
	}
	if _, exists := payload["ts_cache"]; !exists {
		t.Error("missing ts_cache")
	}
}

func TestChannelDeviceLayer(t *testing.T) {
	srv, dev, fb := newTestServer(t)
	w := do(t, srv, http.MethodGet, "/cctv1.m3u8")
	if w.Code != 200 {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "http://x/a.ts") {
		t.Error("expected device layer playlist")
	}
	if fb.touched["cctv1"] != 0 {
		t.Error("fallback should not be touched when device succeeds")
	}
	_ = dev
}

func TestChannelFallbackLayer(t *testing.T) {
	srv, _, fb := newTestServer(t)
	w := do(t, srv, http.MethodGet, "/cctv13.m3u8")
	if w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "http://y/b.ts") {
		t.Error("expected fallback playlist")
	}
	if fb.touched["cctv13"] != 1 {
		t.Errorf("touch = %d", fb.touched["cctv13"])
	}
}

func TestChannelUnknown(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := do(t, srv, http.MethodGet, "/nosuch.m3u8")
	if w.Code != 404 {
		t.Fatalf("code = %d", w.Code)
	}
}

func TestChannelUnavailable(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// cctv2 既不在设备层也不在 fallback 桩里，且 web 为 nil
	w := do(t, srv, http.MethodGet, "/cctv2.m3u8")
	if w.Code != 503 {
		t.Fatalf("code = %d, want 503", w.Code)
	}
}

func TestChannelAlias(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// anhuiws 是 ahws 的别名，应被识别（这里只验证不是 404）
	w := do(t, srv, http.MethodGet, "/anhuiws.m3u8")
	if w.Code == 404 {
		t.Fatalf("alias should resolve, got 404")
	}
}

func TestProxyTSMissingToken(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := do(t, srv, http.MethodGet, "/proxy.ts")
	if w.Code != 400 {
		t.Fatalf("code = %d", w.Code)
	}
}

func TestProxyTSBadToken(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := do(t, srv, http.MethodGet, "/proxy.ts?ts=%%%")
	if w.Code != 400 {
		t.Fatalf("code = %d", w.Code)
	}
}

func TestEngineProxyRejectsForeignHost(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := do(t, srv, http.MethodGet, "/engine_proxy?url=http%3A%2F%2F127.0.0.1%3A8080%2Fsecret")
	if w.Code != 403 {
		t.Fatalf("code = %d, want 403 (SSRF guard)", w.Code)
	}
}

func TestSegmentRejectsForeignHost(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := do(t, srv, http.MethodGet, "/segment?url=http%3A%2F%2F169.254.169.254%2Flatest")
	if w.Code != 403 {
		t.Fatalf("code = %d, want 403", w.Code)
	}
}

func TestDiag(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := do(t, srv, http.MethodGet, "/diag")
	if w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"诊断报告", "频道状态列表", "分片内存缓存"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	// 修复原项目 bug：诊断报告标题只应出现一次
	if n := strings.Count(body, "ysp-live v9.4.0 诊断报告"); n != 1 {
		t.Errorf("诊断报告标题出现 %d 次，应为 1 次", n)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := do(t, srv, http.MethodPost, "/all.m3u")
	if w.Code != 405 {
		t.Fatalf("code = %d", w.Code)
	}
}

func TestHeadNoBody(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := do(t, srv, http.MethodHead, "/all.m3u")
	if w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD should have empty body, got %d bytes", w.Body.Len())
	}
}

func TestMatchChannelPath(t *testing.T) {
	cases := []struct {
		path string
		slug string
		ok   bool
	}{
		{"/cctv1.m3u8", "cctv1", true},
		{"/live/cctv1.m3u8", "cctv1", true},
		{"/cctv16-4k.m3u8", "cctv16-4k", true},
		{"/a/b.m3u8", "", false},
		{"/cctv1.ts", "", false},
		{"/.m3u8", "", false},
	}
	for _, c := range cases {
		slug, ok := matchChannelPath(c.path)
		if ok != c.ok || slug != c.slug {
			t.Errorf("matchChannelPath(%q) = (%q,%v), want (%q,%v)", c.path, slug, ok, c.slug, c.ok)
		}
	}
}

func TestAllowedUpstreamHost(t *testing.T) {
	allowed := []string{
		"http://liveali.cctv.cn/a.ts",
		"https://vdn.apps.cntv.cn/x.ts",
		"https://live.cctv.com/a.ts",
		"https://x.yangshipin.cn/a.ts",
	}
	for _, u := range allowed {
		if !allowedUpstreamHost(u) {
			t.Errorf("should allow %s", u)
		}
	}
	denied := []string{
		"http://127.0.0.1/a.ts",
		"http://192.168.1.1/a.ts",
		"http://169.254.169.254/latest/meta-data",
		"file:///etc/passwd",
		"ftp://x.cctv.cn/a.ts",
		"http://evil.com/a.ts",
	}
	for _, u := range denied {
		if allowedUpstreamHost(u) {
			t.Errorf("should deny %s", u)
		}
	}
}
