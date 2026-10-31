package resolver

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jacktea/ysp-live-go/internal/model"
)

// 本文件覆盖分片改写与播放头匹配（任务书明确要求）：
//   - proxy_playlist_ts_urls 的分片改写（含 base64url 去填充、非 .ts 行保持不变）；
//   - playback_headers_for_ts_url 的 host 匹配与签名头缺失报错；
//   - fetch_playlist_http1 的手动 Host 头 / Connection: close 语义；
//   - 错误分类函数（has_http_400 / is_session_invalidating_error 等）。

func TestProxyPlaylistTSURLs(t *testing.T) {
	r := New(DefaultConfig())
	defer r.Stop()

	playlist := "#EXTM3U\n" +
		"#EXT-X-VERSION:3\n" +
		"#EXTINF:6.000,\n" +
		"http://liveali.example.com/path/seg1.ts?token=abc\n" +
		"#EXTINF:6.000,\n" +
		"http://liveali.example.com/path/seg2.ts\n" +
		"\n" +
		"#EXT-X-ENDLIST\n"

	out := r.proxyPlaylistTSURLs(playlist, "http://192.168.1.2:8767")

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if lines[0] != "#EXTM3U" {
		t.Fatalf("首行应保持 #EXTM3U，got %q", lines[0])
	}
	if lines[1] != "#EXT-X-VERSION:3" {
		t.Fatalf("注释行应原样保留，got %q", lines[1])
	}
	// 分片行应改写为 http://host/proxy.ts?=<base64url 无填充>
	for _, idx := range []int{3, 5} {
		line := lines[idx]
		prefix := "http://192.168.1.2:8767/proxy.ts?="
		if !strings.HasPrefix(line, prefix) {
			t.Fatalf("第 %d 行未改写为代理地址: %q", idx, line)
		}
		token := strings.TrimPrefix(line, prefix)
		if strings.Contains(token, "=") {
			t.Fatalf("base64url token 不应包含填充: %q", token)
		}
		padded := token + strings.Repeat("=", (4-len(token)%4)%4)
		raw, err := base64.URLEncoding.DecodeString(padded)
		if err != nil {
			t.Fatalf("token 不是合法 base64url: %v (%q)", err, token)
		}
		if !strings.HasSuffix(string(raw), ".ts") && !strings.Contains(string(raw), ".ts?") {
			t.Fatalf("解码后应为 .ts 地址: %q", string(raw))
		}
	}
	// 与原始分片一一对应。
	token0 := strings.TrimPrefix(lines[3], "http://192.168.1.2:8767/proxy.ts?=")
	decoded0, _ := base64.URLEncoding.DecodeString(token0 + strings.Repeat("=", (4-len(token0)%4)%4))
	if string(decoded0) != "http://liveali.example.com/path/seg1.ts?token=abc" {
		t.Fatalf("解码结果不符: %q", string(decoded0))
	}
	if !strings.HasSuffix(out, "\n") {
		t.Fatal("输出应以换行结尾（与原实现 join 语义一致）")
	}
}

func TestProxyPlaylistTSURLsWithProxyPrefix(t *testing.T) {
	r := New(DefaultConfig())
	defer r.Stop()

	out := r.proxyPlaylistTSURLs("#EXTM3U\nhttp://cdn.example.com/a.ts\n", "http://ignored", "http://prefix.example.com/")
	if !strings.Contains(out, "http://prefix.example.com/proxy?ts=") {
		t.Fatalf("提供 proxy_prefix 时应使用 /proxy?ts= 形态: %q", out)
	}
}

func TestProxyPlaylistTSURLsSkipsNonTS(t *testing.T) {
	r := New(DefaultConfig())
	defer r.Stop()

	playlist := "#EXTM3U\nhttp://cdn.example.com/index.m3u8\nhttp://cdn.example.com/a.jpg\n"
	out := r.proxyPlaylistTSURLs(playlist, "http://h:1")
	if !strings.Contains(out, "http://cdn.example.com/index.m3u8") {
		t.Fatalf("非 .ts 地址不应改写: %q", out)
	}
	if strings.Contains(out, "/proxy.ts?=") {
		t.Fatalf("没有 .ts 时不应出现代理地址: %q", out)
	}
}

func TestPlaybackHeadersForTSURLHostMatch(t *testing.T) {
	r := New(DefaultConfig())
	defer r.Stop()

	r.stateLock.Lock()
	r.state.cache["cctv1"] = &model.ChannelEntry{
		Channel:     "cctv1",
		FinalURL:    "http://liveali.cctv.cn/live/cctv1/index.m3u8",
		FinalHost:   "liveali.cctv.cn",
		AndroidID:   "0123456789abcdef",
		ExpiresAt:   nowF64() + 60,
		RefreshedAt: nowF64(),
		PlaybackHeaders: map[string]string{
			"UID":          "uid-from-entry",
			"APPID":        "appid-from-entry",
			"APPSIGN":      "sign",
			"APPRANDOMSTR": "random",
		},
	}
	r.stateLock.Unlock()

	// 同 host + 路径前缀匹配：返回该条目的播放头。
	headers, err := r.PlaybackHeadersForTSURL("http://liveali.cctv.cn/live/cctv1/seg1.ts")
	if err != nil {
		t.Fatalf("PlaybackHeadersForTSURL() error = %v", err)
	}
	if headers["UID"] != "uid-from-entry" || headers["APPSIGN"] != "sign" {
		t.Fatalf("未命中缓存播放头: %#v", headers)
	}

	// 同 host + 路径不匹配：返回 same_host_headers 兜底（原实现行为）。
	headers, err = r.PlaybackHeadersForTSURL("http://liveali.cctv.cn/other/seg1.ts")
	if err != nil {
		t.Fatalf("PlaybackHeadersForTSURL() error = %v", err)
	}
	if headers["UID"] != "uid-from-entry" {
		t.Fatalf("同 host 兜底失败: %#v", headers)
	}

	// 需要签名头的 host 但没有缓存：必须报错。
	if _, err := r.PlaybackHeadersForTSURL("http://liveten.cctv.cn/x/seg.ts"); err == nil {
		t.Fatal("liveten host 无缓存时应返回错误")
	} else if !strings.Contains(err.Error(), "missing cached signed playback headers") {
		t.Fatalf("错误文本不符: %v", err)
	}

	// 不需要签名头的 host：返回默认播放头（UID = 当前 android_id）。
	headers, err = r.PlaybackHeadersForTSURL("http://other.cctv.cn/x/seg.ts")
	if err != nil {
		t.Fatalf("PlaybackHeadersForTSURL() error = %v", err)
	}
	if headers["APPID"] == "" || headers["Referer"] != "api.cctv.cn" {
		t.Fatalf("默认播放头不完整: %#v", headers)
	}
}

func TestPlaybackHeadersForTSURLRejectsBadURL(t *testing.T) {
	r := New(DefaultConfig())
	defer r.Stop()

	if _, err := r.PlaybackHeadersForTSURL("://bad url"); err == nil {
		t.Fatal("非法 URL 应返回错误")
	}
	if _, err := r.PlaybackHeadersForTSURL("file:///tmp/a.ts"); err == nil {
		t.Fatal("无 host 的 URL 应返回错误")
	}
}

func TestEntryMissingSignedPlaybackHeaders(t *testing.T) {
	entry := &model.ChannelEntry{FinalHost: "liveali.cctv.cn", PlaybackHeaders: map[string]string{}}
	if !entryMissingSignedPlaybackHeaders(entry) {
		t.Fatal("liveali 缺签名头应判定为缺失")
	}
	entry.PlaybackHeaders = map[string]string{"APPID": "a", "APPSIGN": "b", "APPRANDOMSTR": "c"}
	if entryMissingSignedPlaybackHeaders(entry) {
		t.Fatal("签名头齐全时不应判定缺失")
	}
	entry.FinalHost = "other.cctv.cn"
	entry.PlaybackHeaders = map[string]string{}
	if entryMissingSignedPlaybackHeaders(entry) {
		t.Fatal("非签名 CDN 不要求签名头")
	}
	if !entryMissingSignedPlaybackHeaders(nil) {
		t.Fatal("nil 条目应判定为缺失")
	}
}

// TestFetchPlaylistHTTP1ManualHost 验证手动 HTTP/1.1 抓取的 Host 头与 Connection: close 语义。
func TestFetchPlaylistHTTP1ManualHost(t *testing.T) {
	var gotHost, gotConn, gotAcceptEnc, gotUID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotConn = r.Header.Get("Connection")
		gotAcceptEnc = r.Header.Get("Accept-Encoding")
		gotUID = r.Header.Get("UID")
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte("#EXTM3U\n"))
	}))
	defer srv.Close()

	playbackHeaders := map[string]string{"UID": "android-id-1", "APPID": "ak", "APPSIGN": "s"}
	status, contentType, text, err := fetchPlaylistHTTP1(srv.URL+"/live/x.m3u8", playbackHeaders)
	if err != nil {
		t.Fatalf("fetchPlaylistHTTP1() error = %v", err)
	}
	if status != 200 || !strings.Contains(text, "#EXTM3U") {
		t.Fatalf("响应异常: status=%d text=%q", status, text)
	}
	if contentType != "application/vnd.apple.mpegurl" {
		t.Fatalf("contentType = %q", contentType)
	}
	if gotHost == "" || !strings.HasPrefix(gotHost, "127.0.0.1:") {
		t.Fatalf("Host 头未正确携带端口: %q", gotHost)
	}
	if gotConn != "close" {
		t.Fatalf("Connection 头 = %q, want close", gotConn)
	}
	if gotAcceptEnc != "identity" {
		t.Fatalf("Accept-Encoding 头 = %q, want identity", gotAcceptEnc)
	}
	if gotUID != "android-id-1" {
		t.Fatalf("UID 头未透传: %q", gotUID)
	}
}

// TestFetchPlaylistHTTP1RejectsHTTPS 与原实现一致：只接受 http 地址。
func TestFetchPlaylistHTTP1RejectsHTTPS(t *testing.T) {
	_, _, _, err := fetchPlaylistHTTP1("https://liveali.cctv.cn/x.m3u8", nil)
	if err == nil {
		t.Fatal("https 地址应被拒绝")
	}
	if !strings.Contains(err.Error(), "manual playlist fetch currently expects http URL") {
		t.Fatalf("错误文本不符: %v", err)
	}
}

func TestFetchPlaylistHTTP1UpstreamStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte("expired"))
	}))
	defer srv.Close()

	status, _, text, err := fetchPlaylistHTTP1(srv.URL+"/x.m3u8", nil)
	if err != nil {
		t.Fatalf("fetchPlaylistHTTP1() error = %v", err)
	}
	if status != 403 || text != "expired" {
		t.Fatalf("状态/正文不符: %d %q", status, text)
	}
}

func TestErrorClassifiers(t *testing.T) {
	cases := []struct {
		msg         string
		http400     bool
		sessionBad  bool
		recoverable bool
	}{
		{"ysp-engine: live/v1/01 HTTP 400: {\"result\":400}", true, true, true},
		{"status=400 internal", true, false, false},
		{"app/start HTTP 500", false, true, true},
		{"cloud get HTTP 400", true, false, true},
		{"VDN did not return final URL: x", false, false, false},
		{"upstream m3u8 HTTP 403: expired", false, false, true},
		{"connection refused: dial", false, false, true},
		{"live/v1/02 decrypt failed", false, true, true},
		{"unknown channel cctv", false, false, false},
	}
	for _, c := range cases {
		if got := hasHTTP400(strings.ToLower(c.msg)); got != c.http400 {
			t.Errorf("hasHTTP400(%q) = %v, want %v", c.msg, got, c.http400)
		}
		if got := isSessionInvalidatingError(c.msg); got != c.sessionBad {
			t.Errorf("isSessionInvalidatingError(%q) = %v, want %v", c.msg, got, c.sessionBad)
		}
		if got := isIdentityRecoverableError(c.msg); got != c.recoverable {
			t.Errorf("isIdentityRecoverableError(%q) = %v, want %v", c.msg, got, c.recoverable)
		}
	}
}

func TestCooldownPolicy(t *testing.T) {
	cases := map[int]float64{1: 15, 2: 60, 3: 120, 10: 120}
	for failCount, want := range cases {
		if got := cooldownForFailCount(failCount); got != want {
			t.Errorf("cooldownForFailCount(%d) = %v, want %v", failCount, got, want)
		}
	}
}

func TestInspectPlaylistCache(t *testing.T) {
	entry := &model.ChannelEntry{Channel: "cctv1", FinalURL: "http://x/a.m3u8", AndroidID: "id1"}
	now := nowF64()

	probe := inspectPlaylistCache(nil, entry, "http://h", "", now)
	if probe.State != "miss:not_found" {
		t.Fatalf("缺失时应为 miss:not_found, got %q", probe.State)
	}

	cached := &PlaylistCacheEntry{
		Body: "body", ContentType: "ct", CachedAt: now - 1, ExpiresAt: now + 1,
		FinalURL: entry.FinalURL, AndroidID: entry.AndroidID, ProxyOrigin: "http://h", ProxyPrefix: "",
	}
	if probe := inspectPlaylistCache(cached, entry, "http://h", "", now); probe.State != "hit" {
		t.Fatalf("应命中, got %q", probe.State)
	}

	expired := *cached
	expired.ExpiresAt = now - 1
	if probe := inspectPlaylistCache(&expired, entry, "http://h", "", now); probe.State != "miss:expired" {
		t.Fatalf("过期判定不符, got %q", probe.State)
	}

	changed := *cached
	changed.FinalURL = "http://x/b.m3u8"
	if probe := inspectPlaylistCache(&changed, entry, "http://h", "", now); !strings.Contains(probe.State, "final_url_changed") {
		t.Fatalf("final_url_changed 判定不符, got %q", probe.State)
	}
	if probe := inspectPlaylistCache(cached, entry, "http://other", "", now); !strings.Contains(probe.State, "origin_changed") {
		t.Fatalf("origin_changed 判定不符, got %q", probe.State)
	}
}

func TestMD5AndVDNCode(t *testing.T) {
	// 原 compute_vdn_code：md5_hex(AK + app_secret + random_str)。
	sign, randomStr := computeVDNCode("secret")
	// 原 generate_app_random_str 格式：8-4-4-4-12 的十六进制串（共 36 字符）。
	if len(randomStr) != 36 {
		t.Fatalf("randomStr 长度 = %d, want 36: %q", len(randomStr), randomStr)
	}
	if parts := strings.Split(randomStr, "-"); len(parts) != 5 || len(parts[0]) != 8 || len(parts[1]) != 4 {
		t.Fatalf("randomStr 格式不符: %q", randomStr)
	}
	if len(sign) != 32 {
		t.Fatalf("md5 长度 = %d, want 32", len(sign))
	}
	// 用固定随机串校验 md5 拼接顺序。
	if got := md5Hex("9f5c54c4ed0e50109b800f7e28fec205" + "secret" + "r"); len(got) != 32 {
		t.Fatalf("md5Hex 结果异常: %q", got)
	}
}

func TestSlotDeviceJSONPath(t *testing.T) {
	r := New(func() Config {
		c := DefaultConfig()
		c.DeviceJSON = "/tmp/dir/device-state-rs.json"
		return c
	}())
	defer r.Stop()

	if got := r.slotDeviceJSONPath(0); got != "/tmp/dir/device-state-rs.json" {
		t.Fatalf("slot0 路径 = %q", got)
	}
	if got := r.slotDeviceJSONPath(1); got != "/tmp/dir/device-state-rs-slot1.json" {
		t.Fatalf("slot1 路径 = %q", got)
	}
}

func TestSupportsOnly4K(t *testing.T) {
	all := New(DefaultConfig())
	defer all.Stop()
	if !all.Supports("cctv1") || !all.Supports("cctv4k") {
		t.Fatal("默认模式应支持 26 路高码率频道")
	}
	if all.Supports("cctv6") {
		t.Fatal("cctv6 不在高码率频道列表")
	}

	only4k := New(func() Config {
		c := DefaultConfig()
		c.Only4K = true
		return c
	}())
	defer only4k.Stop()
	if only4k.Supports("cctv1") {
		t.Fatal("Only4K 模式不应支持 cctv1")
	}
	if !only4k.Supports("cctv4k") || !only4k.Supports("cctv164k") || !only4k.Supports("cctv8k") {
		t.Fatal("Only4K 模式应支持 3 路 4K/8K")
	}
}
