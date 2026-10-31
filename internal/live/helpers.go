package live

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// timeZero 用于复位时间字段（等价 Python 里的 0.0）。
var timeZero = time.Time{}

// decodeJSONObject 解析上游 JSON 对象（数字保留为 json.Number 以便安全取整）。
func decodeJSONObject(body []byte) (map[string]interface{}, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("invalid json: %w", err)
	}
	m, ok := v.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid json: expected object")
	}
	return m, nil
}

// jsonString 把 JSON 值安全转为字符串（等价原代码对 str 字段的宽容读取）。
func jsonString(v interface{}) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return val
	case json.Number:
		return val.String()
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(val)
	}
	return ""
}

// jsonInt 把 JSON 值安全转为 int（无法解析时返回 def）。
// 对应原 int(p.get('iretcode', -1))。
func jsonInt(v interface{}, def int) int {
	switch val := v.(type) {
	case nil:
		return def
	case json.Number:
		if n, err := val.Int64(); err == nil {
			return int(n)
		}
		if f, err := val.Float64(); err == nil {
			return int(f)
		}
		return def
	case float64:
		return int(val)
	case int:
		return val
	case string:
		s := strings.TrimSpace(val)
		if s == "" {
			return def
		}
		if n, err := strconv.Atoi(s); err == nil {
			return n
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return int(f)
		}
	}
	return def
}

// parseURL 容错解析 URL。
func parseURL(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, false
	}
	return u, true
}

// trimForLog 截断用于日志/错误的地址，避免日志被超长签名串撑爆。
// 按 rune 截断，保证不会切坏 UTF-8 序列。
func trimForLog(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
