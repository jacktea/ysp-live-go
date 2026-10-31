// Package playlist 生成 m3u 订阅列表与分片代理令牌。
//
// 对应原项目 /all.m3u 分支与 proxy_ts_token / decode_proxy_ts_url。
package playlist

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/jacktea/ysp-live-go/internal/model"
)

// EPGURLs 订阅头里的 EPG 地址（沿用原项目）。
const (
	EPGURL1 = "https://epg.zsdc.eu.org/t.xml.gz"
	EPGURL2 = "https://epg.pw/xmltv/epg_CN.xml.gz"
)

// ErrBadToken 分片代理令牌非法。
var ErrBadToken = errors.New("invalid proxy ts token")

// Options /all.m3u 生成参数。
type Options struct {
	Host   string // 请求的 Host 头
	Proto  string // http / https
	Group  string // 分组过滤（央视 / 卫视），空表示全部
	Only4K bool   // 仅 4K/8K 频道
	Dir    *model.ChannelDirectory
}

// All 生成聚合订阅 m3u。
//
// 与原实现完全一致的输出结构：
//
//	#EXTM3U url-tvg="..." x-tvg-url="..."
//	#EXT-X-APTV-PREVIEW: FALSE
//	#EXT-X-APTV-LATENCY: FALSE
//	#EXT-X-APTV-LOGO: FALSE
//	#EXTINF:-1 tvg-id=... tvg-name=... tvg-logo=... group-title=... [catchup 属性],名称
//	http://host/slug.m3u8
func All(o Options) string {
	proto := o.Proto
	if proto == "" {
		proto = "http"
	}
	var b strings.Builder
	b.WriteString(`#EXTM3U url-tvg="` + EPGURL1 + "," + EPGURL2 + `" x-tvg-url="` + EPGURL1 + `"`)
	b.WriteString("\n")
	b.WriteString("#EXT-X-APTV-PREVIEW: FALSE\n")
	b.WriteString("#EXT-X-APTV-LATENCY: FALSE\n")
	b.WriteString("#EXT-X-APTV-LOGO: FALSE\n")

	for _, m := range o.Dir.AllChannels() {
		if o.Group != "" && !strings.Contains(m.Group, o.Group) {
			continue
		}
		if o.Only4K && !model.True4KChannels[m.Slug] {
			continue
		}
		b.WriteString(`#EXTINF:-1 tvg-id="` + m.TVGID + `" tvg-name="` + m.Name +
			`" tvg-logo="` + m.Logo + `" group-title="` + m.Group + `"`)
		if model.TimeshiftSupported[m.Slug] {
			b.WriteString(` catchup="append" catchup-days="7" catchup-source="?playseek=${(b)yyyyMMddHHmmss}-${(e)yyyyMMddHHmmss}"`)
		}
		b.WriteString("," + m.Name + "\n")
		b.WriteString(proto + "://" + o.Host + "/" + m.Slug + ".m3u8\n")
	}
	return b.String()
}

// EncodeTSToken 把真实分片地址编码为 URL-safe base64（无填充）。
func EncodeTSToken(rawURL string) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString([]byte(rawURL)), "=")
}

// DecodeTSToken 解码并校验分片令牌。
//
// 原项目 decode_proxy_ts_url 的校验规则：
//   - 必须是合法 base64url（自动补齐 =）
//   - 解码后必须是 UTF-8
//   - scheme 必须是 http/https
//   - path 必须以 .ts 结尾
//
// 修复原项目 bug：原 `/proxy.ts?=token` 的解析在 query 为空或形如 `a=b&=`  时
// 会把整个 query 当 token，导致把任意字符串当 base64 解码；这里统一严格解析。
func DecodeTSToken(token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", fmt.Errorf("%w: empty token", ErrBadToken)
	}
	padded := token
	if m := len(padded) % 4; m != 0 {
		padded += strings.Repeat("=", 4-m)
	}
	raw, err := base64.URLEncoding.DecodeString(padded)
	if err != nil {
		return "", fmt.Errorf("%w: base64: %v", ErrBadToken, err)
	}
	value := string(raw)
	if !utf8Valid(value) {
		return "", fmt.Errorf("%w: not UTF-8", ErrBadToken)
	}
	u, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("%w: not a URL: %v", ErrBadToken, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%w: scheme %q not allowed", ErrBadToken, u.Scheme)
	}
	if !strings.HasSuffix(strings.ToLower(u.Path), ".ts") {
		return "", fmt.Errorf("%w: not a .ts segment", ErrBadToken)
	}
	return value, nil
}

// ExtractTSToken 从请求路径+查询串中提取令牌。
// 兼容原项目三种形式：`?ts=<tok>`、`?url=<tok>`、`?=<tok>`、以及裸 query。
func ExtractTSToken(path, rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	if strings.HasPrefix(rawQuery, "=") {
		return rawQuery[1:]
	}
	q, err := url.ParseQuery(rawQuery)
	if err == nil {
		if v := q.Get("ts"); v != "" {
			return v
		}
		if v := q.Get("url"); v != "" {
			return v
		}
	}
	// 裸 query（无 '='）
	if !strings.Contains(rawQuery, "=") {
		return rawQuery
	}
	return ""
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '\uFFFD' {
			return false
		}
	}
	return true
}
