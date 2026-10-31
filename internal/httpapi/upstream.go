package httpapi

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jacktea/ysp-live-go/internal/util"
)

// FetchUpstream 抓取 TS 分片。
//
// 对应原 /proxy.ts 分支里的 http.client 逻辑：
//   - 手动设置 Host 头
//   - 只透传 Content-Type / Content-Range / Accept-Ranges / Content-Length
//   - 透传客户端 Range
//   - 30s 超时
func FetchUpstream(r *http.Request, target string, playbackHeaders map[string]string, headOnly bool) (int, []byte, map[string]string, error) {
	u, err := url.Parse(target)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("invalid target url: %w", err)
	}
	host := u.Hostname()
	if host == "" {
		return 0, nil, nil, fmt.Errorf("invalid ts host")
	}
	isHTTPS := u.Scheme == "https"
	port := u.Port()
	if port == "" {
		if isHTTPS {
			port = "443"
		} else {
			port = "80"
		}
	}

	headers := make(map[string]string, len(playbackHeaders)+4)
	for _, key := range []string{"UID", "APPID", "APPRANDOMSTR", "Referer", "User-Agent", "APPSIGN"} {
		if v, ok := playbackHeaders[key]; ok && strings.TrimSpace(v) != "" {
			headers[key] = util.HeaderValueClean(v)
		}
	}
	headers["Accept"] = "*/*"
	headers["Accept-Encoding"] = "identity"
	headers["Connection"] = "close"
	hostHeader := host
	if u.Port() != "" {
		hostHeader = net.JoinHostPort(host, u.Port())
	}
	headers["Host"] = hostHeader
	if rg := r.Header.Get("Range"); rg != "" {
		headers["Range"] = util.HeaderValueClean(rg)
	}

	method := http.MethodGet
	if headOnly {
		method = http.MethodHead
	}
	transport := &http.Transport{
		DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // 央视频 CDN 证书链偶有问题，与原实现一致
		},
		DisableCompression:    true,
		MaxIdleConnsPerHost:   4,
		ResponseHeaderTimeout: 20 * time.Second,
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}

	req, err := http.NewRequest(method, target, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	// http.Request 不允许直接设置 Host 头到 Header map，需用 req.Host
	req.Host = hostHeader
	for k, v := range headers {
		if k == "Host" {
			continue
		}
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()

	pass := make(map[string]string, 4)
	for _, key := range []string{"Content-Type", "Content-Range", "Accept-Ranges", "Content-Length"} {
		if v := resp.Header.Get(key); v != "" {
			pass[key] = v
		}
	}
	if pass["Content-Length"] == "" && resp.ContentLength >= 0 {
		pass["Content-Length"] = strconv.FormatInt(resp.ContentLength, 10)
	}
	if headOnly {
		return resp.StatusCode, nil, pass, nil
	}
	// 限制单分片最大 64MB，避免异常上游把内存打满。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return resp.StatusCode, body, pass, nil
	}
	return resp.StatusCode, body, pass, nil
}
