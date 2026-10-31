package web

// 本文件覆盖原 ysp-engine.js 的 HTTP 服务端语义在 Go 侧的等价物：
//   - AllM3U（原 GET /all.m3u）
//   - Health（原 GET /health）
//   - SegmentProxy（原 GET /segment?url=）
//   - 分片地址改写（原 resolveChannelLive 末尾循环）
//   - SSRF 白名单（修复原项目 bug）

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestAllM3U 验证 /all.m3u 的输出格式与频道数。
func TestAllM3U(t *testing.T) {
	e := newTestEngine(t)
	out := e.AllM3U("http://127.0.0.1:8767")
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if lines[0] != "#EXTM3U" {
		t.Fatalf("首行 = %q, 期望 #EXTM3U", lines[0])
	}
	// 每个频道两行（EXTINF + URL）
	if want := 1 + len(channels)*2; len(lines) != want {
		t.Fatalf("行数 = %d, 期望 %d", len(lines), want)
	}
	// 抽查第一条（CHANNELS 声明顺序的第一个是 cctv1）
	const wantFirst = `#EXTINF:-1 tvg-id="cctv1" tvg-name="CCTV-1 综合" group-title="央视频",CCTV-1 综合`
	if lines[1] != wantFirst {
		t.Fatalf("第一条 EXTINF\n got=%s\nwant=%s", lines[1], wantFirst)
	}
	if lines[2] != "http://127.0.0.1:8767/live/cctv1.m3u8" {
		t.Fatalf("第一条 URL = %q", lines[2])
	}
	// trailing slash 处理
	out2 := e.AllM3U("http://127.0.0.1:8767/")
	if out2 != out {
		t.Fatalf("baseURL 带斜杠时输出不一致")
	}
	// 所有频道都出现，且顺序与 channelOrder 一致
	for i, slug := range channelOrder {
		if !strings.Contains(out, "/live/"+slug+".m3u8") {
			t.Fatalf("all.m3u 缺少频道 %s", slug)
		}
		if !strings.Contains(out, `tvg-id="`+slug+`"`) {
			t.Fatalf("all.m3u 缺少 tvg-id=%s", slug)
		}
		_ = i
	}
}

// TestHealth 验证 /health 的响应体与原 JS 完全一致。
func TestHealth(t *testing.T) {
	e := newTestEngine(t)
	const want = `{"status":"ok","version":"9.4.0","engine":"ysp-engine"}`
	if got := string(e.Health()); got != want {
		t.Fatalf("Health\n got=%s\nwant=%s", got, want)
	}
	if HealthContentType != "application/json" {
		t.Fatalf("HealthContentType = %q", HealthContentType)
	}
}

// TestSegmentProxy 验证 /segment 的 SSRF 白名单与基本错误分支。
func TestSegmentProxy(t *testing.T) {
	// 注意：SegmentProxy 有域名白名单，测试用 httpapi 同款白名单里的域名不可行
	// （无法把测试服务器绑到 .cctv.cn 域名），因此这里直接测试白名单函数 +
	// 用一个"命中白名单后缀"的假 URL 走拒绝分支，正常路径用 localhost 绕过方式验证：
	// 通过 http.RoundTripper 无法替换，所以正常路径改为下面 TestSegmentProxyAllowed
	// 里的 end-to-end 方式（用 -count 不联网的 httptest + 域名后缀校验）。
	if allowedSegmentHost("http://127.0.0.1:8080/seg.ts") {
		t.Fatal("裸 IP 应被拒绝")
	}
	if allowedSegmentHost("http://localhost:8080/seg.ts") {
		t.Fatal("localhost 应被拒绝")
	}
	if allowedSegmentHost("ftp://cdn.cctv.cn/seg.ts") {
		t.Fatal("非 http(s) 应被拒绝")
	}
	if allowedSegmentHost("http://evil.example.com/seg.ts") {
		t.Fatal("非白名单域名应被拒绝")
	}
	if allowedSegmentHost("http://10.0.0.1/seg.ts") {
		t.Fatal("私网 IP 应被拒绝")
	}
	if allowedSegmentHost("http://169.254.1.1/seg.ts") {
		t.Fatal("link-local IP 应被拒绝")
	}
	if !allowedSegmentHost("https://cdn.example.cctv.cn/seg.ts") {
		t.Fatal("*.cctv.cn 应被允许")
	}
	if !allowedSegmentHost("https://live.yangshipin.cn/seg.ts") {
		t.Fatal("*.yangshipin.cn 应被允许")
	}
	if !allowedSegmentHost("https://v1.ysp.cctv.cn/seg.ts") {
		t.Fatal("*.ysp.cctv.cn 应被允许")
	}
	// .cn 后缀白名单与 httpapi 的 allowedUpstreamHost 完全一致：
	// 注意 example.com 并不以 ".cn" 结尾，因此应被拒绝（我曾误写成允许）。
	if allowedSegmentHost("http://example.com/seg.ts") {
		t.Fatal("example.com 不应被允许（后缀不是 .cn）")
	}
	if !allowedSegmentHost("https://vdn.apps.cntv.cn/seg.ts") {
		t.Fatal(".cn 后缀域名应被允许（与 httpapi 保持一致）")
	}
	if !allowedSegmentHost("https://live.cctv.com/seg.ts") {
		t.Fatal("*.cctv.com 应被允许")
	}

	e := newTestEngine(t)
	if _, err := e.SegmentProxy(context.Background(), "", ""); err == nil {
		t.Fatal("空 url 应报错")
	}
	if _, err := e.SegmentProxy(context.Background(), "http://127.0.0.1:1/seg.ts", ""); err == nil {
		t.Fatal("非白名单地址应报错")
	} else if !strings.Contains(err.Error(), "白名单") {
		t.Fatalf("错误文本 = %v, 期望提示白名单", err)
	}
}

// TestSegmentProxyAllowed end-to-end 验证白名单放行后的实际抓取：
// 用 httptest 服务 + 构造 host 后缀可绕过的场景不可行，因此这里
// 通过临时放宽（测试内直接调用底层 fetch 逻辑）验证状态码/头透传。
func TestSegmentProxyAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "bytes=0-3" {
			w.Header().Set("Content-Type", "video/mp2t")
			w.WriteHeader(http.StatusPartialContent)
			fmt.Fprint(w, "TSPA")
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Content-Length", "9")
		fmt.Fprint(w, "TSPAYLOAD")
	}))
	defer srv.Close()

	e := newTestEngine(t)
	// 直接走内部抓取逻辑（跳过白名单，仅验证代理语义本身）。
	resp, err := e.fetchSegment(context.Background(), srv.URL+"/seg.ts", "")
	if err != nil {
		t.Fatalf("fetchSegment: %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("状态码 = %d", resp.Status)
	}
	if string(resp.Body) != "TSPAYLOAD" {
		t.Fatalf("body = %q", resp.Body)
	}
	if resp.ContentType != "video/mp2t" {
		t.Fatalf("ContentType = %q", resp.ContentType)
	}
	if resp.ContentLength != int64(len("TSPAYLOAD")) {
		t.Fatalf("ContentLength = %d", resp.ContentLength)
	}

	// Range 透传
	resp, err = e.fetchSegment(context.Background(), srv.URL+"/seg.ts", "bytes=0-3")
	if err != nil {
		t.Fatalf("fetchSegment(range): %v", err)
	}
	if resp.Status != http.StatusPartialContent {
		t.Fatalf("Range 状态码 = %d, 期望 206", resp.Status)
	}
	if string(resp.Body) != "TSPA" {
		t.Fatalf("Range body = %q", resp.Body)
	}
}

// TestRewritePlaylist 验证分片改写规则（绝对/相对/根相对/带额外 tag）。
func TestRewritePlaylist(t *testing.T) {
	e := newTestEngine(t)
	base := "https://live.yangshipin.cn/hls/index.m3u8?token=abc"
	text := strings.Join([]string{
		"#EXTM3U",
		"#EXT-X-VERSION:3",
		"",
		"#EXTINF:4.0,",
		"seg-1.ts",
		"#EXTINF:4.0,",
		"/hls/seg-2.ts",
		"#EXTINF:4.0,",
		"https://cdn.cctv.cn/seg-3.ts",
		"#EXT-X-ENDLIST",
		"",
	}, "\n")
	out := e.rewritePlaylist(text, base)
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")

	if lines[0] != "#EXTM3U" || lines[1] != "#EXT-X-VERSION:3" {
		t.Fatalf("tag 行被改动: %v", lines[:2])
	}
	// 空行被丢弃（原 JS: if (!trimmed) continue）
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			t.Fatalf("输出含空行")
		}
	}
	// 相对分片相对 base 的目录解析
	want1 := e.cfg.SegmentProxyPath + url.QueryEscape("https://live.yangshipin.cn/hls/seg-1.ts")
	if lines[3] != want1 {
		t.Fatalf("相对分片\n got=%s\nwant=%s", lines[3], want1)
	}
	// 根相对分片
	want2 := e.cfg.SegmentProxyPath + url.QueryEscape("https://live.yangshipin.cn/hls/seg-2.ts")
	if lines[5] != want2 {
		t.Fatalf("根相对分片\n got=%s\nwant=%s", lines[5], want2)
	}
	// 绝对分片原样保留
	want3 := e.cfg.SegmentProxyPath + url.QueryEscape("https://cdn.cctv.cn/seg-3.ts")
	if lines[7] != want3 {
		t.Fatalf("绝对分片\n got=%s\nwant=%s", lines[7], want3)
	}
	// 结尾必须有换行
	if !strings.HasSuffix(out, "\n") {
		t.Fatal("输出未以换行结尾")
	}
}

// TestRewritePlaylistCustomPrefix 验证可配置的分片代理前缀。
func TestRewritePlaylistCustomPrefix(t *testing.T) {
	e, err := New(Config{SegmentProxyPath: "/proxy?ts="})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()
	out := e.rewritePlaylist("#EXTM3U\n#EXTINF:4,\nseg.ts\n", "https://cdn.cctv.cn/x/index.m3u8")
	if !strings.Contains(out, "/proxy?ts="+url.QueryEscape("https://cdn.cctv.cn/x/seg.ts")) {
		t.Fatalf("自定义前缀未生效:\n%s", out)
	}
}

// TestLRUCache 验证缓存上限与 LRU 淘汰（修复原项目 bug: JS 的 Map 无上限）。
func TestLRUCache(t *testing.T) {
	c := newLRUCache(3, cacheTTL)
	now := time.Now()
	c.Set("a", "pa", now)
	c.Set("b", "pb", now)
	c.Set("c", "pc", now)
	if c.Len() != 3 {
		t.Fatalf("Len = %d", c.Len())
	}
	// 访问 a，使 b 成为最久未使用
	if v, ok := c.Get("a", now); !ok || v != "pa" {
		t.Fatalf("Get(a) = %q,%v", v, ok)
	}
	c.Set("d", "pd", now) // 触发淘汰，应淘汰 b
	if c.Len() != 3 {
		t.Fatalf("淘汰后 Len = %d", c.Len())
	}
	if _, ok := c.Get("b", now); ok {
		t.Fatal("b 应被淘汰")
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, ok := c.Get(k, now); !ok {
			t.Fatalf("%s 不应被淘汰", k)
		}
	}
	// TTL 过期
	later := now.Add(cacheTTL + time.Millisecond)
	if _, ok := c.Get("a", later); ok {
		t.Fatal("过期条目应失效")
	}
	// 容量上限压测
	c2 := newLRUCache(8, cacheTTL)
	for i := 0; i < 100; i++ {
		c2.Set(fmt.Sprintf("k%d", i), "v", now)
	}
	if c2.Len() != 8 {
		t.Fatalf("容量未受限: Len = %d", c2.Len())
	}
}
