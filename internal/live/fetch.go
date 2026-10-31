package live

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jacktea/ysp-live-go/internal/httpx"
	"github.com/jacktea/ysp-live-go/internal/util"
)

// maxPlaylistBytes 播放列表响应体上限（防御异常上游返回超大内容）。
const maxPlaylistBytes = 8 << 20

// httpStatusError 上游返回非 2xx 状态码（等价 Python urllib.error.HTTPError）。
// 错误文本刻意保留原项目风格 "HTTPError: HTTP 404"，便于上层按 "HTTP 400" 之类片段判断。
type httpStatusError struct {
	code int
}

func (e *httpStatusError) Error() string { return fmt.Sprintf("HTTPError: HTTP %d", e.code) }

// httpFetcher 播列表抓取用的轻量客户端：记录重定向后的最终 URL（原 r.geturl()）。
type httpFetcher struct {
	hc *http.Client
}

func newHTTPFetcher() *httpFetcher {
	tr := &http.Transport{
		Proxy:               nil,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		DisableCompression:  true, // 与原项目一致：不主动协商压缩，收到 gzip 时手动解压
	}
	return &httpFetcher{hc: &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			return nil
		},
	}}
}

// get 执行 GET，返回状态码、重定向后的最终 URL 与响应体。
func (f *httpFetcher) get(rawURL string, headers map[string]string, timeout time.Duration) (int, string, []byte, error) {
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, "", nil, fmt.Errorf("invalid request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := f.hc.Do(req)
	if err != nil {
		return 0, "", nil, httpx.NormalizeNetworkError(err)
	}
	defer resp.Body.Close()

	body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxPlaylistBytes))
	final := rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	if rerr != nil {
		return resp.StatusCode, final, nil, httpx.NormalizeNetworkError(rerr)
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Encoding")), "gzip") && len(body) > 0 {
		if zr, gerr := gzip.NewReader(bytes.NewReader(body)); gerr == nil {
			if plain, perr := io.ReadAll(io.LimitReader(zr, maxPlaylistBytes)); perr == nil {
				body = plain
			}
			_ = zr.Close()
		}
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, final, body, &httpStatusError{code: resp.StatusCode}
	}
	return resp.StatusCode, final, body, nil
}

// waitSlot 上游请求前的全局流控（原 global_flow_limiter.wait_slot()）。
func (m *Manager) waitSlot() {
	if m.limiter == nil {
		return
	}
	m.limiter.Wait()
}

// resolveRef 把相对地址解析为绝对地址（原 urllib.parse.urljoin）。
func resolveRef(baseURL, ref string) string {
	base, err := url.Parse(baseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return base.ResolveReference(r).String()
}

// fetchAbsPlaylist 抓取 m3u8 并把其中的相对分片地址改写为绝对地址。
// 对应原 fetch_abs_playlist(url, depth=0)：遇到 #EXT-X-STREAM-INF 主播放列表时
// 递归进入子列表（depth<2），因此最终返回的必然是分片列表。
func (m *Manager) fetchAbsPlaylist(rawURL string, depth int) (string, error) {
	m.waitSlot()
	headers := map[string]string{
		"User-Agent": "qqlive",
		"Referer":    "https://live.cctv.cn/",
		"Accept":     "application/vnd.apple.mpegurl,application/json,*/*",
	}
	_, final, body, err := m.fetcher.get(rawURL, headers, playlistTimeout)
	if err != nil {
		return "", fmt.Errorf("fetch playlist %s: %w", rawURL, err)
	}
	text := strings.ToValidUTF8(string(body), "\uFFFD")

	if depth < 2 {
		lines := util.RSLines(text)
		for i, ln := range lines {
			if !strings.HasPrefix(strings.TrimSpace(ln), "#EXT-X-STREAM-INF") {
				continue
			}
			for j := i + 1; j < len(lines); j++ {
				s := strings.TrimSpace(lines[j])
				if s != "" && !strings.HasPrefix(s, "#") {
					return m.fetchAbsPlaylist(resolveRef(final, s), depth+1)
				}
			}
			break
		}
	}

	lines := util.RSLines(text)
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		s := strings.TrimSpace(ln)
		if s != "" && !strings.HasPrefix(s, "#") {
			out = append(out, resolveRef(final, s))
		} else {
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n"), nil
}
