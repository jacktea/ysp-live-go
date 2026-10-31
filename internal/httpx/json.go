package httpx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
)

// CompactJSON 复刻原项目的 json.dumps(separators=(',',':'), ensure_ascii=False, sort_keys=True)：
// 键名升序、无多余空格、不转义非 ASCII、不转义 HTML 字符。
func CompactJSON(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	out := bytes.TrimRight(buf.Bytes(), "\n")
	return SortJSONKeys(out)
}

// SortJSONKeys 递归把 JSON 对象键排序，模拟 Go map 序列化的 Python sort_keys 行为。
func SortJSONKeys(data []byte) ([]byte, error) {
	var v interface{}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := writeSorted(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeSorted(buf *bytes.Buffer, v interface{}) error {
	switch val := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, err := encodeNoEscape(k)
			if err != nil {
				return err
			}
			buf.Write(kb)
			buf.WriteByte(':')
			if err := writeSorted(buf, val[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
		return nil
	case []interface{}:
		buf.WriteByte('[')
		for i, item := range val {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeSorted(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil
	default:
		b, err := encodeNoEscape(val)
		if err != nil {
			return err
		}
		buf.Write(b)
		return nil
	}
}

func encodeNoEscape(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// FormEncode 原项目风格的表单编码（空格 -> +）。
func FormEncode(pairs [][2]string) []byte {
	vals := make([]string, 0, len(pairs))
	for _, p := range pairs {
		vals = append(vals, escapeFormValue(p[0])+"="+escapeFormValue(p[1]))
	}
	return []byte(join(vals, "&"))
}

func escapeFormValue(s string) string {
	var b bytes.Buffer
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

func join(items []string, sep string) string {
	var b bytes.Buffer
	for i, s := range items {
		if i > 0 {
			b.WriteString(sep)
		}
		b.WriteString(s)
	}
	return b.String()
}

// EncodeQueryOrdered 按给定顺序编码查询串（避免 url.Values 的键排序改变签名）。
func EncodeQueryOrdered(pairs [][2]string) string {
	var b bytes.Buffer
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(p[0]))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(p[1]))
	}
	return b.String()
}
