package gateway

import (
	"strconv"
	"strings"
	"time"
)

// 时移查询解析。对应原 parse_catchup_time_val / parse_catchup_query。
//
// 支持的写法（与原项目一致）：
//   ?playseek=20240101120000-20240101130000
//   ?playseek=20240101120000
//   ?start=...&end=... / ?utc=...&lutc=... / ?starttime=...&endtime=...

// cstZone 东八区。
var cstZone = time.FixedZone("CST", 8*3600)

// ParseCatchupTimeVal 解析单侧时间值，支持 10/13/12/14 位数字。
//
// 原实现先做 `"".join(c for c in val if c.isdigit())`：会把分隔符全部剔除，
// 因此 `2024-01-01 12:00:00` 这类写法也能解析（14 位）。
func ParseCatchupTimeVal(val string) (int64, bool) {
	val = strings.TrimSpace(val)
	if val == "" {
		return 0, false
	}
	var digits strings.Builder
	for _, r := range val {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	d := digits.String()
	switch len(d) {
	case 10:
		n, err := strconv.ParseInt(d, 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	case 13:
		n, err := strconv.ParseInt(d, 10, 64)
		if err != nil {
			return 0, false
		}
		return n / 1000, true
	case 14:
		if t, err := time.ParseInLocation("20060102150405", d, cstZone); err == nil {
			return t.Unix(), true
		}
	case 12:
		if t, err := time.ParseInLocation("200601021504", d, cstZone); err == nil {
			return t.Unix(), true
		}
	}
	return 0, false
}

// ParseCatchupQuery 解析时移区间。
//
// 修复原项目 bug：原实现用 `if not start_ts` 判断，导致 start_ts==0
// （1970-01-01）被当成"未提供"重复解析；这里改用 ok 标志判断。
func ParseCatchupQuery(query string) (startTS, endTS int64, ok bool) {
	if query == "" {
		return 0, 0, false
	}
	q := parseQuery(query)
	now := time.Now().Unix()

	var haveStart, haveEnd bool
	if ps := q["playseek"]; ps != "" {
		if s, e, found := strings.Cut(ps, "-"); found {
			startTS, haveStart = ParseCatchupTimeVal(s)
			endTS, haveEnd = ParseCatchupTimeVal(e)
		} else {
			startTS, haveStart = ParseCatchupTimeVal(ps)
		}
	}
	if !haveStart {
		if v := q["start"]; v != "" {
			startTS, haveStart = ParseCatchupTimeVal(v)
		}
	}
	if !haveEnd {
		if v := q["end"]; v != "" {
			endTS, haveEnd = ParseCatchupTimeVal(v)
		}
	}
	if !haveStart {
		if v := q["utc"]; v != "" {
			startTS, haveStart = ParseCatchupTimeVal(v)
		}
	}
	if !haveEnd {
		if v := q["lutc"]; v != "" {
			endTS, haveEnd = ParseCatchupTimeVal(v)
		}
	}
	if !haveStart {
		if v := q["starttime"]; v != "" {
			startTS, haveStart = ParseCatchupTimeVal(v)
		}
	}
	if !haveEnd {
		if v := q["endtime"]; v != "" {
			endTS, haveEnd = ParseCatchupTimeVal(v)
		}
	}
	if !haveStart {
		return 0, 0, false
	}
	if !haveEnd || endTS <= startTS {
		// 原实现：end = max(start+60, min(start+7200, now))
		endTS = startTS + 60
		if upper := startTS + 7200; upper < endTS {
			endTS = upper
		}
		if now < endTS {
			endTS = now
		}
		if endTS <= startTS {
			endTS = startTS + 60
		}
	}
	return startTS, endTS, true
}

// parseQuery 极简 query 解析（保留原项目"取第一个值"的语义）。
func parseQuery(raw string) map[string]string {
	out := make(map[string]string, 8)
	for _, pair := range strings.Split(raw, "&") {
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		if _, exists := out[k]; exists {
			continue
		}
		out[k] = unescapeQuery(v)
	}
	return out
}

func unescapeQuery(s string) string {
	if !strings.ContainsAny(s, "%+") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '+':
			b.WriteByte(' ')
		case '%':
			if i+2 < len(s) {
				if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
					b.WriteByte(byte(v))
					i += 2
					continue
				}
			}
			b.WriteByte('%')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
