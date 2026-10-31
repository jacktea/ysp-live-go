package live

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jacktea/ysp-live-go/internal/model"
)

// cstZone 东八区（原 parse_catchup_time_val 里的 datetime.timezone(timedelta(hours=8))）。
var cstZone = time.FixedZone("CST", 8*3600)

// ParseCatchupTimeVal 解析时移时间参数。
// 对应原 parse_catchup_time_val(val_str)：
//   - 先剥离所有非数字字符；
//   - 10 位：秒级时间戳；
//   - 13 位：毫秒级时间戳（整除 1000）；
//   - 14 位：YYYYMMDDHHMMSS（东八区）；
//   - 12 位：YYYYMMDDHHMM（东八区）；
//   - 其余返回 ok=false。
func ParseCatchupTimeVal(valStr string) (int64, bool) {
	val := strings.TrimSpace(valStr)
	if val == "" {
		return 0, false
	}
	digits := make([]byte, 0, len(val))
	for i := 0; i < len(val); i++ {
		c := val[i]
		if c >= '0' && c <= '9' {
			digits = append(digits, c)
		}
	}
	if len(digits) == 0 {
		return 0, false
	}
	s := string(digits)
	switch len(s) {
	case 10:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	case 13:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, false
		}
		return n / 1000, true
	case 14:
		if t, err := time.ParseInLocation("20060102150405", s, cstZone); err == nil {
			return t.Unix(), true
		}
	case 12:
		if t, err := time.ParseInLocation("200601021504", s, cstZone); err == nil {
			return t.Unix(), true
		}
	}
	return 0, false
}

// ParseCatchupQuery 解析时移查询串，返回起止时间戳。
// 对应原 parse_catchup_query(query_str)：
//   - playseek=<start>-<end> 或 playseek=<start>；
//   - 依次回退 start/end、utc/lutc、starttime/endtime；
//   - 只有 start 时，end = max(start+60, min(start+7200, now))。
//
// 返回 ok=false 表示不是时移请求（由 gateway 决定回退直播）。
func ParseCatchupQuery(rawQuery string) (int64, int64, bool) {
	if rawQuery == "" {
		return 0, 0, false
	}
	qs := parseQueryLossy(rawQuery)
	now := time.Now().Unix()

	var startTs, endTs int64
	var hasStart, hasEnd bool

	if ps, ok := firstValue(qs, "playseek"); ok {
		if i := strings.Index(ps, "-"); i >= 0 {
			if v, ok := ParseCatchupTimeVal(ps[:i]); ok {
				startTs, hasStart = v, true
			}
			if v, ok := ParseCatchupTimeVal(ps[i+1:]); ok {
				endTs, hasEnd = v, true
			}
		} else if v, ok := ParseCatchupTimeVal(ps); ok {
			startTs, hasStart = v, true
		}
	}
	if !hasStart {
		if v, ok := firstTime(qs, "start"); ok {
			startTs, hasStart = v, true
		}
	}
	if !hasEnd {
		if v, ok := firstTime(qs, "end"); ok {
			endTs, hasEnd = v, true
		}
	}
	if !hasStart {
		if v, ok := firstTime(qs, "utc"); ok {
			startTs, hasStart = v, true
		}
	}
	if !hasEnd {
		if v, ok := firstTime(qs, "lutc"); ok {
			endTs, hasEnd = v, true
		}
	}
	if !hasStart {
		if v, ok := firstTime(qs, "starttime"); ok {
			startTs, hasStart = v, true
		}
	}
	if !hasEnd {
		if v, ok := firstTime(qs, "endtime"); ok {
			endTs, hasEnd = v, true
		}
	}
	if !hasStart {
		return 0, 0, false
	}
	if !hasEnd || endTs <= startTs {
		endTs = maxInt64(startTs+60, minInt64(startTs+7200, now))
	}
	return startTs, endTs, true
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// parseQueryLossy 宽松解析查询串（原 urllib.parse.parse_qs 语义：
// 保留有值参数、同名参数取第一个、值做 URL 解码）。
func parseQueryLossy(raw string) map[string][]string {
	out := make(map[string][]string, 8)
	if i := strings.Index(raw, "?"); i >= 0 {
		raw = raw[i+1:]
	}
	for _, pair := range strings.Split(raw, "&") {
		if pair == "" {
			continue
		}
		k, v, found := strings.Cut(pair, "=")
		if !found {
			continue
		}
		dk, err := url.QueryUnescape(k)
		if err != nil {
			dk = k
		}
		dv, err := url.QueryUnescape(v)
		if err != nil {
			dv = v
		}
		if strings.TrimSpace(dv) == "" {
			// 原 parse_qs 默认 keep_blank_values=False，空值参数被丢弃。
			continue
		}
		out[dk] = append(out[dk], dv)
	}
	return out
}

func firstValue(qs map[string][]string, name string) (string, bool) {
	vs, ok := qs[name]
	if !ok || len(vs) == 0 {
		return "", false
	}
	return vs[0], true
}

func firstTime(qs map[string][]string, name string) (int64, bool) {
	v, ok := firstValue(qs, name)
	if !ok {
		return 0, false
	}
	return ParseCatchupTimeVal(v)
}

// CatchupPlaylist 生成时移回看播放列表。
// 对应原处理器中的时移分支：
//
//	if catchup_times and canonical in TIMESHIFT_SUPPORTED:
//	    m3u8_url = jce_timeshift_url(ch.pid, ch.sid, start_ts, end_ts, ch.defn)
//	    pl = fetch_abs_playlist(m3u8_url)
//
// 与原实现的差异（task-2 第 5 条）：不在 TimeshiftSupported 的频道不视为错误，
// 返回 (\"\", nil) 由 gateway 决定回退直播；真正的上游错误才返回 error。
// hostVar 为预留的 Host 变量（时移分片改写不需要它，保留以对齐 gateway 调用约定）。
func (m *Manager) CatchupPlaylist(ch *Channel, startTs, endTs int64, hostVar string) (string, error) {
	if ch == nil {
		return "", ErrUnknownChannel
	}
	_ = hostVar // 时移播放列表直接把绝对地址返回给播放器，无需按 Host 改写。
	if !model.TimeshiftSupported[ch.Slug] {
		// 不报错：由 gateway 决定回退到直播链路。
		return "", nil
	}
	rawURL, err := m.jceTimeshiftURL(ch.PID, ch.SID, startTs, endTs, ch.Defn)
	if err != nil {
		logfInterval(10.0, ch.Slug, "时移获取失败: %v，回退直播", err)
		return "", err
	}
	pl, err := m.fetchAbsPlaylist(rawURL, 0)
	if err != nil {
		logfInterval(10.0, ch.Slug, "时移获取失败: %v，回退直播", err)
		return "", err
	}
	if !strings.Contains(pl, "#EXTM3U") {
		return "", fmt.Errorf("时移播放列表无效: %s", trimForLog(rawURL, 80))
	}
	return pl, nil
}

// CatchupQuery 便捷方法：直接从原始查询串生成时移播放列表。
// 返回 ok=false 表示不是时移请求（gateway 应继续走直播逻辑）。
func (m *Manager) CatchupQuery(ch *Channel, rawQuery, hostVar string) (string, bool, error) {
	startTs, endTs, ok := ParseCatchupQuery(rawQuery)
	if !ok {
		return "", false, nil
	}
	pl, err := m.CatchupPlaylist(ch, startTs, endTs, hostVar)
	return pl, true, err
}
