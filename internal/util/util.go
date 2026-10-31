// Package util 通用工具：URL 处理、m3u8 改写、表单编码、Java/指纹兼容函数。
package util

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
)

// HeaderValueClean 去除 CR/LF，避免头部注入（原 header_value_clean）。
func HeaderValueClean(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "\r", ""), "\n", "")
}

// URLHost 返回 URL 的 host（不含端口），解析失败返回空串。
func URLHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// IsTSURL 判断是否为 .ts 分片地址。
func IsTSURL(value string) bool {
	u, err := url.Parse(value)
	if err != nil {
		return false
	}
	return strings.HasSuffix(strings.ToLower(u.Path), ".ts")
}

// RSLines 按行切分，保留原项目对 \r\n / 尾部换行的处理语义。
func RSLines(text string) []string {
	norm := strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(norm, "\n")
	if strings.HasSuffix(norm, "\n") && len(lines) > 0 {
		lines = lines[:len(lines)-1]
	}
	for i, ln := range lines {
		lines[i] = strings.TrimSuffix(ln, "\r")
	}
	return lines
}

// RewritePlaylistURLs 把 m3u8 中的相对地址改写为绝对地址（原 rewrite_playlist_urls）。
func RewritePlaylistURLs(text, baseURL string) string {
	base, err := url.Parse(baseURL)
	hasBase := err == nil && base.Scheme != "" && base.Host != ""
	var out strings.Builder
	for _, line := range RSLines(text) {
		stripped := strings.TrimSpace(line)
		if stripped == "" || strings.HasPrefix(stripped, "#") {
			out.WriteString(line)
		} else if hasBase {
			if ref, err := url.Parse(stripped); err == nil {
				out.WriteString(base.ResolveReference(ref).String())
			} else {
				out.WriteString(stripped)
			}
		} else {
			out.WriteString(stripped)
		}
		out.WriteString("\n")
	}
	return out.String()
}

// FormURLEncodeValue 复刻原项目的表单值编码（空格用 +，保留 *-._）。
func FormURLEncodeValue(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '*', c == '-', c == '.', c == '_':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// FormEncode 按原项目顺序编码 key=value 对。
func FormEncode(pairs [][2]string) []byte {
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, FormURLEncodeValue(p[0])+"="+FormURLEncodeValue(p[1]))
	}
	return []byte(strings.Join(parts, "&"))
}

// QueryParam 取查询参数（原 query_param，不做额外解码）。
func QueryParam(rawURL, name string) string {
	idx := strings.Index(rawURL, "?")
	if idx < 0 {
		return ""
	}
	for _, pair := range strings.Split(rawURL[idx+1:], "&") {
		k, v, found := strings.Cut(pair, "=")
		if found && k == name {
			return v
		}
	}
	return ""
}

// RandomFromCharset 从字符集随机取样生成长度为 n 的字符串。
func RandomFromCharset(n int, charset string) string {
	out := make([]byte, n)
	if charset == "" {
		return ""
	}
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	for i := range buf {
		out[i] = charset[int(buf[i])%len(charset)]
	}
	return string(out)
}

// RandUint32 返回加密随机 uint32。
func RandUint32() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}

// GenerateAppRandomStr 复刻原 generate_app_random_str。
func GenerateAppRandomStr() string {
	return fmt.Sprintf("%08x-0000-%04x-0000-00000000%04x", RandUint32(), RandUint32()&0xFFFF, RandUint32()&0xFFFF)
}
