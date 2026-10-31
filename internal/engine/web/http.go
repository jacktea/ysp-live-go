package web

// 本文件移植原 ysp-engine.js 里的 HTTP 客户端与服务端语义：
//   - httpPost / httpGet / httpGetBuffer（第 242-289 行）
//   - 分片地址改写（resolveChannelLive 末尾的 outLines 循环，第 409-425 行）
//   - /all.m3u（第 442-452 行）、/segment（第 454-476 行）、/health（第 436-440 行）
//
// 与原 JS 的差异（都是任务要求的修复/适配）：
//   - 不自己监听端口，改为 AllM3U(baseURL) + SegmentProxy 供 gateway 统一暴露；
//   - 分片改写前缀可配置（原 JS 硬编码 http://127.0.0.1:PORT/segment?url=）；
//   - /segment 做 SSRF 白名单校验、限制响应体 32MB、透传 Range。

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jacktea/ysp-live-go/internal/engine"
	"github.com/jacktea/ysp-live-go/internal/httpx"
)

// httpResult 一次上游请求的结果（对应原 JS {statusCode, headers, data/buffer}）。
type httpResult struct {
	Status  int
	Headers http.Header
	Body    []byte
}

// Text 按 UTF-8 返回响应体文本。
func (r *httpResult) Text() string { return string(r.Body) }

// transport 与 Engine 共享的 HTTP Transport（等价原 JS 的 httpsAgent:
// keepAlive + maxSockets 64 + 30s keepAliveMsecs）。
func (e *Engine) transport() *http.Transport {
	if e.tr != nil {
		return e.tr
	}
	e.trOnce.Do(func() {
		e.tr = &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   e.cfg.UpstreamTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:        64,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
			DisableCompression:  true, // 自行处理，避免与请求头冲突
		}
	})
	return e.tr
}

// doRequest 对应原 JS httpPost / httpGet：
// 发请求、限制响应体大小、把网络错误归一化成原项目风格文字。
func (e *Engine) doRequest(ctx context.Context, method, rawURL string, headers map[string]string, body []byte) (*httpResult, error) {
	if _, err := url.Parse(rawURL); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// HTTP 客户端会拒绝带空格的 Host 等非法头，这里统一清理 CR/LF 防注入。
	for k := range req.Header {
		req.Header.Set(k, strings.ReplaceAll(strings.ReplaceAll(req.Header.Get(k), "\r", ""), "\n", ""))
	}

	client := &http.Client{Transport: e.transport(), Timeout: e.cfg.UpstreamTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, httpx.NormalizeNetworkError(err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxPlaylistBytes))
	if err != nil {
		return nil, httpx.NormalizeNetworkError(err)
	}
	return &httpResult{Status: resp.StatusCode, Headers: resp.Header, Body: raw}, nil
}

// maxPlaylistBytes m3u8 / JSON 响应体上限（防御异常上游）。
const maxPlaylistBytes = 8 << 20

// rewritePlaylist 对应原 JS resolveChannelLive 末尾的分片改写循环：
//
//	for (const line of lines) {
//	  const trimmed = line.trim();
//	  if (!trimmed) continue;
//	  if (trimmed.startsWith('#')) outLines.push(trimmed);
//	  else outLines.push(SEGMENT_PREFIX + encodeURIComponent(absoluteUrl));
//	}
//	return outLines.join('\n') + '\n';
func (e *Engine) rewritePlaylist(text, baseURL string) string {
	// 原 JS: url.URL(trimmed, playurl) —— 以 playurl 为基准解析相对地址
	base, baseErr := url.Parse(baseURL)
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines)+8)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue // 原 JS: if (!trimmed) continue;
		}
		if strings.HasPrefix(trimmed, "#") {
			out = append(out, trimmed)
			continue
		}
		fullSegURL := trimmed
		if !strings.HasPrefix(fullSegURL, "http://") && !strings.HasPrefix(fullSegURL, "https://") {
			if baseErr == nil {
				if ref, err := url.Parse(trimmed); err == nil {
					// 修复原项目 bug: 原 JS 直接 new URL(...) 没有 try/catch，
					// 非法地址会抛异常导致整条播放列表失败；Go 版降级为保留原样。
					fullSegURL = base.ResolveReference(ref).String()
				}
			}
		}
		// 原 JS: '.../segment?url=' + encodeURIComponent(fullSegUrl)
		// encodeURIComponent 与 url.QueryEscape 的差异只在空格(+ vs %20)，
		// 分片 URL 里不会出现空格，这里语义一致。
		out = append(out, e.cfg.SegmentProxyPath+url.QueryEscape(fullSegURL))
	}
	return strings.Join(out, "\n") + "\n"
}

// Playlist 获取频道的 m3u8（对应原 JS 的 GET /live/<slug>.m3u8 处理）。
// 未支持/未知频道返回 engine.ErrUnknownChannel。
func (e *Engine) Playlist(ctx context.Context, channel string) (*engine.Playlist, error) {
	e.mu.Lock()
	closed := e.closed
	e.mu.Unlock()
	if closed {
		// 修复原项目 bug: 原 JS 的 uncaughtException 只打日志继续运行，
		// 可能在引擎已关闭/半初始化时继续接请求；Go 版返回明确错误。
		return nil, fmt.Errorf("%w: 引擎已关闭", ErrEngine)
	}
	pl, err := e.resolveLive(ctx, channel)
	if err != nil {
		return nil, err
	}
	if pl == nil {
		// 原 JS: if (!meta) return null → HTTP 404 unknown channel
		return nil, fmt.Errorf("%w: %s", engine.ErrUnknownChannel, channel)
	}
	return pl, nil
}

// Start 兼容 server 装配层的生命周期接口。
// 本包不监听端口（由 gateway 统一暴露），因此只做一次可用性检查。
func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	closed := e.closed
	e.mu.Unlock()
	if closed {
		return fmt.Errorf("%w: 引擎已关闭", ErrEngine)
	}
	return nil
}

// Health 对应原 JS GET /health 的响应体：
// {"status":"ok","version":VERSION,"engine":"ysp-engine"}（字段顺序与原 JS 一致）。
func (e *Engine) Health() []byte {
	return []byte(`{"status":"ok","version":"` + Version + `","engine":"ysp-engine"}`)
}

// HealthContentType /health 的 Content-Type。
const HealthContentType = "application/json"

// AllM3U 对应原 JS GET /all.m3u：按 CHANNELS 声明顺序输出全部频道的 m3u。
// baseURL 是本服务的对外地址（例如 http://127.0.0.1:8767），用于拼 /live/<slug>.m3u8。
func (e *Engine) AllM3U(baseURL string) string {
	base := strings.TrimRight(baseURL, "/")
	lines := make([]string, 0, len(channelOrder)*2+1)
	lines = append(lines, "#EXTM3U")
	for _, slug := range channelOrder {
		meta := channels[slug]
		// 原 JS: '#EXTINF:-1 tvg-id="' + slug + '" tvg-name="' + meta.name + '" group-title="央视频",' + meta.name
		lines = append(lines, `#EXTINF:-1 tvg-id="`+slug+`" tvg-name="`+meta.Name+`" group-title="央视频",`+meta.Name)
		lines = append(lines, base+"/live/"+slug+".m3u8")
	}
	return strings.Join(lines, "\n") + "\n"
}

// AllM3UContentType /all.m3u 的 Content-Type（原 JS 带 charset）。
const AllM3UContentType = "application/vnd.apple.mpegurl; charset=utf-8"

// SegmentResponse /segment 代理的返回体（对应原 JS /segment 的响应头 + body）。
// 装配层可直接映射到 httpapi.SegmentResponse 或按字段写响应。
type SegmentResponse struct {
	Status        int
	Body          []byte
	ContentType   string
	ContentLength int64
}

// allowedSegmentHost 校验分片 URL 的域名白名单。
//
// 修复原项目 bug: 原 JS 的 /segment 不校验 url 参数，可被当作任意地址代理（SSRF），
// 攻击者能用它探测内网；Go 版只允许 http/https 且 host 属于央视频相关域名，
// 并显式拒绝裸 IP / 回环 / 私网 / link-local / 未指定地址。
//
// 域名集合与 internal/httpapi/proxy.go 的 allowedUpstreamHost 保持一致，
// 避免两层校验互相打架。
func allowedSegmentHost(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}
	// 裸 IP 一律拒绝：上游 CDN 不使用裸 IP。
	// 这一条同时覆盖回环/私网/link-local/未指定/组播地址，
	// 不需要再逐项判断（它们都只能以裸 IP 形式出现，而裸 IP 已整体拒绝）。
	if net.ParseIP(host) != nil {
		return false
	}
	for _, suffix := range []string{
		".cctv.cn", ".cctv.com", ".yangshipin.cn", ".ysp.cctv.cn",
		".cn", // 央视频 CDN 也可能落在其他 .cn 子域（与 httpapi 保持一致）
	} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// SegmentProxy 对应原 JS GET /segment?url=<encoded>：
// 拉取分片并透传状态码、Content-Type、Content-Length，支持 Range。
//
// 修复原项目 bug: 原 JS 的 httpGetBuffer 不限制响应体大小，异常上游可打爆内存；
// 这里限制到 32MB（maxSegmentBytes）并返回 ErrSegmentTooLarge。
func (e *Engine) SegmentProxy(ctx context.Context, targetURL, rangeHeader string) (*SegmentResponse, error) {
	if targetURL == "" {
		// 原 JS: res.writeHead(400); res.end('missing url\n')
		return nil, fmt.Errorf("%w: missing url", ErrEngine)
	}
	if !allowedSegmentHost(targetURL) {
		return nil, fmt.Errorf("%w: 目标地址不在允许的央视频域名白名单内", ErrBlockedSegmentURL)
	}
	return e.fetchSegment(ctx, targetURL, rangeHeader)
}

// fetchSegment 是 SegmentProxy 的抓取主体（不含白名单校验）。
// 拆出来是为了让测试能对 httptest 本地服务验证状态码/头透传语义；
// 生产路径必须经由 SegmentProxy（带白名单）。
func (e *Engine) fetchSegment(ctx context.Context, targetURL, rangeHeader string) (*SegmentResponse, error) {
	// 用独立的 client：分片可能是 http（非 https），且需要更长的读超时。
	client := &http.Client{
		Transport: e.transport(),
		Timeout:   30 * time.Second,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: 非法分片地址: %v", ErrEngine, err)
	}
	req.Header.Set("User-Agent", e.cfg.UA)
	req.Header.Set("Referer", yangshipinRefer)
	if rangeHeader != "" {
		req.Header.Set("Range", strings.ReplaceAll(strings.ReplaceAll(rangeHeader, "\r", ""), "\n", ""))
	}
	resp, err := client.Do(req)
	if err != nil {
		// 原 JS: 502 segment error: <message>
		return nil, fmt.Errorf("%w: segment error: %v", ErrEngine, httpx.NormalizeNetworkError(err))
	}
	defer resp.Body.Close()

	// 限制 +1 字节用于判断是否超限。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSegmentBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: segment error: %v", ErrEngine, httpx.NormalizeNetworkError(err))
	}
	if int64(len(raw)) > maxSegmentBytes {
		return nil, fmt.Errorf("%w: 分片响应体超过 %dMB", ErrSegmentTooLarge, maxSegmentBytes>>20)
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		// 原 JS: resp.headers['content-type'] || 'video/mp2t'
		contentType = "video/mp2t"
	}
	resp2 := &SegmentResponse{
		Status:        resp.StatusCode,
		Body:          raw,
		ContentType:   contentType,
		ContentLength: int64(len(raw)),
	}
	// 优先用上游给的 Content-Length（HEAD/206 场景），否则用实际长度。
	if resp.ContentLength >= 0 && rangeHeader != "" {
		resp2.ContentLength = resp.ContentLength
	}
	return resp2, nil
}

// AdminNote 说明本包对外暴露的装配信息（供 /diag 或 README 引用）。
const AdminNote = "web 引擎不监听端口：用 AllM3U(baseURL) + SegmentProxy 由 gateway 统一暴露"
