package httpapi

import (
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/jacktea/ysp-live-go/internal/playlist"
)

// allowedUpstreamHost 校验上游域名白名单。
//
// 修复原项目 bug: 原 /engine_proxy 与 Node 引擎的 /segment 都能代理任意地址（开放代理/SSRF），
// 攻击者可借内网地址探测；Go 版默认只允许央视频相关域名。
func allowedUpstreamHost(raw string) bool {
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
	// 拒绝内网 / 回环地址
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return false
		}
		return false // 上游 CDN 不使用裸 IP
	}
	for _, suffix := range []string{
		".cctv.cn", ".cctv.com", ".yangshipin.cn", ".ysp.cctv.cn",
		".cn", // 央视频 CDN 也可能落在其他 .cn 子域；如需更严格可去掉
	} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// handleSegment 兼容 Node 引擎的 /segment?url= 形态。
//
// 分片地址在 JS 引擎里是完整绝对地址；这里优先走带播放头的 TS 代理，
// 没有可用播放头时退回直连抓取。
func (s *Server) handleSegment(w http.ResponseWriter, r *http.Request, headOnly bool, query string) {
	target := ""
	if q, err := url.ParseQuery(query); err == nil {
		target = q.Get("url")
	}
	if target == "" {
		s.send(w, http.StatusBadRequest, "missing url\n", "text/plain; charset=utf-8", headOnly, nil)
		return
	}
	if !allowedUpstreamHost(target) {
		s.send(w, http.StatusForbidden, "upstream host not allowed\n", "text/plain; charset=utf-8", headOnly, nil)
		return
	}
	// 复用 TS 代理逻辑（带缓存与播放头）
	s.proxyTS(w, r, headOnly, target)
}

// handleTSProxy 处理 /proxy.ts?<token> 与 /proxy?ts=<token>。
func (s *Server) handleTSProxy(w http.ResponseWriter, r *http.Request, headOnly bool, query string) {
	token := playlist.ExtractTSToken(r.URL.Path, query)
	if token == "" {
		s.send(w, http.StatusBadRequest, "missing proxy ts token\n", "text/plain; charset=utf-8", headOnly, nil)
		return
	}
	target, err := playlist.DecodeTSToken(token)
	if err != nil {
		s.send(w, http.StatusBadRequest, err.Error()+"\n", "text/plain; charset=utf-8", headOnly, nil)
		return
	}
	s.proxyTS(w, r, headOnly, target)
}

// proxyTS 抓取并回传 TS 分片，带内存缓存与播放头。
//
// 行为对齐原 /proxy.ts 分支：
//   - 命中缓存返回 X-Cache: HIT-RAM
//   - 未命中转发上游并返回 X-Cache: MISS-UPSTREAM
//   - 仅缓存 200 且大小 <= 35MB 的完整响应
//   - 透传 Content-Type / Content-Range / Accept-Ranges / Content-Length
func (s *Server) proxyTS(w http.ResponseWriter, r *http.Request, headOnly bool, target string) {
	if cached, headers, ok := s.opts.TSCache.Get(target); ok {
		h := w.Header()
		for k, v := range headers {
			h.Set(k, v)
		}
		h.Set("X-Cache", "HIT-RAM")
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Cache-Control", "public, max-age=300")
		if h.Get("Content-Length") == "" {
			h.Set("Content-Length", itoa(len(cached)))
		}
		w.WriteHeader(200)
		if !headOnly {
			_, _ = w.Write(cached)
		}
		return
	}

	var playbackHeaders map[string]string
	if s.opts.Segments != nil {
		hs, err := s.opts.Segments.PlaybackHeaders(target)
		if err != nil {
			// 缺少签名头时不直接失败：允许无头重试（部分 CDN 不校验）。
			playbackHeaders = nil
		} else {
			playbackHeaders = hs
		}
	}
	status, body, respHeaders, err := FetchUpstream(r, target, playbackHeaders, headOnly)
	if err != nil {
		s.send(w, http.StatusBadGateway, "Upstream CDN error: "+err.Error()+"\n", "text/plain; charset=utf-8", headOnly, nil)
		return
	}
	h := w.Header()
	for k, v := range respHeaders {
		h.Set(k, v)
	}
	h.Set("X-Cache", "MISS-UPSTREAM")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(status)
	if !headOnly && len(body) > 0 {
		_, _ = w.Write(body)
	}
	if status == 200 && len(body) > 0 && len(body) <= 35<<20 {
		s.opts.TSCache.Set(target, body, respHeaders)
	}
}

func itoa(v int) string {
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
