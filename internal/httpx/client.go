// Package httpx 提供与原项目 HttpClient 等价的 HTTP 客户端：
// 固定超时、CookieJar、gzip 解压、错误归一化文本。
package httpx

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

// Response 统一响应（原 HttpResponse）。
type Response struct {
	Status  int
	Reason  string
	Headers http.Header
	Body    []byte
}

// Text 将响应体按 UTF-8 解析（非法字节替换）。
func (r *Response) Text() string {
	return strings.ToValidUTF8(string(r.Body), "\uFFFD")
}

// Client 封装 http.Client（原 HttpClient）。
type Client struct {
	hc      *http.Client
	timeout time.Duration
}

// New 创建客户端；insecureTLS 为 true 时跳过证书校验。
func New(timeout float64, insecureTLS bool) *Client {
	if timeout < 0.1 {
		timeout = 0.1
	}
	jar, _ := cookiejar.New(nil)
	tr := &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   time.Duration(timeout * float64(time.Second)),
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: insecureTLS}, //nolint:gosec
		DisableCompression:  true,                                         // 自行处理 gzip，避免与请求头冲突
	}
	return &Client{
		timeout: time.Duration(timeout * float64(time.Second)),
		hc: &http.Client{
			Transport: tr,
			Jar:       jar,
			Timeout:   time.Duration(timeout * float64(time.Second)),
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("stopped after 10 redirects")
				}
				return nil
			},
		},
	}
}

var ctx = context.Background()

// Send 执行一次请求（原 _send），网络错误会被归一化为原项目风格的文字。
func (c *Client) Send(method, rawURL string, headers map[string]string, data []byte) (*Response, error) {
	var body io.Reader
	if data != nil {
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// 与原项目一致：不自动解压时手动处理 gzip
	req.Header.Del("Proxy-Connection")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, NormalizeNetworkError(err)
	}
	defer resp.Body.Close()

	var raw []byte
	if resp.ContentLength > 0 && resp.ContentLength <= 64<<20 {
		raw = make([]byte, 0, resp.ContentLength)
	}
	buf, rerr := io.ReadAll(io.LimitReader(resp.Body, 128<<20))
	if rerr != nil {
		return nil, NormalizeNetworkError(rerr)
	}
	raw = append(raw, buf...)

	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Encoding")), "gzip") && len(raw) > 0 {
		if zr, err := gzip.NewReader(bytes.NewReader(raw)); err == nil {
			if plain, err := io.ReadAll(zr); err == nil {
				raw = plain
			}
			_ = zr.Close()
		}
	}
	return &Response{Status: resp.StatusCode, Reason: strings.TrimSpace(strings.TrimPrefix(resp.Status, fmt.Sprint(resp.StatusCode))), Headers: resp.Header, Body: raw}, nil
}

// Get 发送 GET。
func (c *Client) Get(rawURL string, headers map[string]string) (*Response, error) {
	return c.Send(http.MethodGet, rawURL, headers, nil)
}

// PostBytes 发送原始字节 POST。
func (c *Client) PostBytes(rawURL string, headers map[string]string, data []byte) (*Response, error) {
	return c.Send(http.MethodPost, rawURL, headers, data)
}

// PostJSON 以紧凑+键排序 JSON 发送 POST（等价原 post_json）。
func (c *Client) PostJSON(rawURL string, headers map[string]string, body interface{}) (*Response, error) {
	data, err := CompactJSON(body)
	if err != nil {
		return nil, err
	}
	return c.Send(http.MethodPost, rawURL, headers, data)
}

// PostForm 以原项目编码方式发送表单 POST。
func (c *Client) PostForm(rawURL string, headers map[string]string, pairs [][2]string) (*Response, error) {
	return c.Send(http.MethodPost, rawURL, headers, FormEncode(pairs))
}

// ReasonOf 返回状态描述文字（原 http_status_text）。
func ReasonOf(status int, reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Sprint(status)
	}
	return fmt.Sprintf("%d %s", status, reason)
}

// NormalizeNetworkError 把 Go 网络错误归一化为原项目的中文/英文提示文本。
func NormalizeNetworkError(err error) error {
	if err == nil {
		return nil
	}
	var dnsErr *net.DNSError
	var netErr net.Error
	switch {
	case errors.As(err, &dnsErr):
		return fmt.Errorf("failed to lookup address: %v", err)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("operation timed out: %v", err)
	case errors.As(err, &netErr) && netErr.Timeout():
		return fmt.Errorf("operation timed out: %v", err)
	case strings.Contains(err.Error(), "connection refused"):
		return fmt.Errorf("connection refused: %v", err)
	case strings.Contains(err.Error(), "connection reset"):
		return fmt.Errorf("connection reset by peer: %v", err)
	}
	return fmt.Errorf("network error: %v", err)
}

// ParseURL 容错解析 URL。
func ParseURL(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, false
	}
	return u, true
}
