package live

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/jacktea/ysp-live-go/internal/live/jce"
	"github.com/jacktea/ysp-live-go/internal/util"
)

// jceTimeshiftURL 调用 jacc.ysp.cctv.cn 的时移接口换取一条 m3u8 地址。
// 对应原 jce_timeshift_url(pid, sid, start, end, stream='fhd')。
//
// 返回的错误：
//   - 接口 tag0 非 0 时包含原项目的 errMsg / "errCode=%s"；
//   - m3u8 落在 liverecord.video.cloud.cctv.com 时返回 ErrDeadHost，
//     上层 refresh_once 据此自动切 bkliveinfo 备用通道。
func (m *Manager) jceTimeshiftURL(pid, sid string, start, end int64, stream string) (string, error) {
	if stream == "" {
		stream = "fhd"
	}
	w := jce.NewWriter()
	w.String(pid, 0)
	w.String(sid, 1)
	w.Long(start, 2)
	w.Long(end, 3)
	w.String(stream, 4)
	body := w.Out()

	reqID := int(m.now().UnixMilli()) & 2147483647
	packet := jce.Wrap(jce.CmdTimeshift, body, reqID)

	m.waitSlot()
	headers := map[string]string{"Content-Type": "application/octet-stream"}
	resp, err := m.jaccClient.PostBytes(m.jaccURL, headers, packet)
	if err != nil {
		return "", fmt.Errorf("jce timeshift: %w", err)
	}
	if resp.Status < 200 || resp.Status >= 300 {
		// 保持原项目 HTTPError 文本形态（"HTTPError: HTTP 404"），供上层识别。
		return "", fmt.Errorf("jce timeshift: %w", &httpStatusError{code: resp.Status})
	}
	inner, err := jce.Unwrap(resp.Body)
	if err != nil {
		return "", fmt.Errorf("jce timeshift: %w", err)
	}
	if len(inner) == 0 {
		return "", fmt.Errorf("jce timeshift: bad response")
	}
	fields, err := jce.NewReader(inner).Struct()
	if err != nil {
		return "", fmt.Errorf("jce timeshift: %w", err)
	}
	if errCode := jce.GetInt(fields, 0, 0); errCode != 0 {
		msg := jce.GetString(fields, 1, "")
		if msg == "" {
			msg = fmt.Sprintf("errCode=%d", errCode)
		}
		return "", fmt.Errorf("jce timeshift: %s", msg)
	}
	rawURL := jce.GetString(fields, 2, "")
	if rawURL == "" {
		return "", fmt.Errorf("jce timeshift: empty m3u8")
	}
	if strings.Contains(rawURL, "liverecord.video.cloud.cctv.com") {
		return "", ErrDeadHost
	}
	return rawURL, nil
}

// jceSeg 一个解析出的分片（原 jce_fetch 返回的 (dur, pdt, url) 三元组）。
type jceSeg struct {
	dur float64
	pdt string
	url string
}

// jceFetch 拉取 1080p JCE 的 m3u8 并解析分片。
// 对应原 jce_fetch(ch)：先按 now-WINDOW..now 取地址，再解析 #EXTINF /
// #EXT-X-PROGRAM-DATE-TIME 与分片地址；分片地址用 urljoin 转绝对地址。
func (m *Manager) jceFetch(ch *Channel) ([]jceSeg, error) {
	now := m.now().Unix()
	m3u8URL, err := m.jceTimeshiftURL(ch.PID, ch.SID, now-int64(m.window), now, ch.Defn)
	if err != nil {
		return nil, err
	}
	m.waitSlot()
	headers := map[string]string{"User-Agent": m.ua}
	// 与原实现一致：分片相对地址以请求用的 m3u8 地址为基准（不用重定向后的最终地址）。
	_, _, body, ferr := m.fetcher.get(m3u8URL, headers, cdnTimeout)
	if ferr != nil {
		return nil, fmt.Errorf("jce fetch: %w", ferr)
	}
	text := strings.ToValidUTF8(string(body), "\uFFFD")

	segs := make([]jceSeg, 0, 32)
	dur := 6.0
	pdt := ""
	for _, line := range util.RSLines(text) {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "#EXTINF:"):
			rest := line[len("#EXTINF:"):]
			if i := strings.Index(rest, ","); i >= 0 {
				rest = rest[:i]
			}
			// 与原实现一致：解析失败时回落为 6.0（Python 的 except ValueError 分支）。
			if v, perr := strconv.ParseFloat(strings.TrimSpace(rest), 64); perr == nil {
				dur = v
			} else {
				dur = 6.0
			}
		case strings.HasPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:"):
			pdt = line[len("#EXT-X-PROGRAM-DATE-TIME:"):]
		case line != "" && !strings.HasPrefix(line, "#"):
			segs = append(segs, jceSeg{dur: dur, pdt: pdt, url: resolveRef(m3u8URL, line)})
			pdt = ""
		}
	}
	if len(segs) == 0 {
		return nil, fmt.Errorf("empty playlist")
	}
	return segs, nil
}

// segKey 分片去重键（原 seg_key）：有 pdt 用 pdt，否则用 scheme://host/path。
func segKey(rawURL, pdt string) string {
	if pdt != "" {
		return "pdt:" + pdt
	}
	if u, ok := parseURL(rawURL); ok && u.Host != "" {
		return u.Scheme + "://" + u.Host + u.Path
	}
	// 地址解析失败时退化为原串，保证不同分片不会互相覆盖。
	return "url:" + rawURL
}

// jceRefresh 拉取并合并 1080p 分片。
// 对应原 jce_refresh(ch)：
//   - 首次（缓存为空）只保留最后 25 片；
//   - 增量时按 PDT 过滤旧分片、按 key 去重；
//   - 超过 MaxSegs 时从最旧开始淘汰。
//
// 修复原项目 bug（task-2 第 2 条）：原实现 `if pdt and ch.last_pdt and pdt <= ch.last_pdt`
// 在仅一侧有 pdt 时逻辑脆弱；这里明确只在双方都有 pdt 时比较，pdt 为空的分片按 key 去重，
// 不会出现同一分片反复插入且 lastPDT 不推进的问题。
func (m *Manager) jceRefresh(ch *Channel) error {
	segs, err := m.jceFetch(ch)
	if err != nil {
		return err
	}

	ch.mu.Lock()
	added := 0
	isInit := len(ch.order) == 0

	// insertLocked 按 key 去重插入一个分片（返回是否新增）。
	insertLocked := func(s jceSeg) bool {
		key := segKey(s.url, s.pdt)
		if old, ok := ch.segments[key]; ok {
			// 已存在：只刷新地址（与原实现一致），不重复追加 order，
			// 修复原项目在 pdt 为空时同一分片被重复插入的问题。
			old.url = s.url
			return false
		}
		ch.seq++
		ch.segments[key] = &segment{seq: ch.seq, dur: s.dur, pdt: s.pdt, url: s.url}
		ch.order = append(ch.order, key)
		if s.pdt != "" {
			ch.lastPDT = s.pdt
		}
		added++
		return true
	}

	if isInit {
		initSegs := segs
		if len(initSegs) > initSegCount {
			initSegs = initSegs[len(initSegs)-initSegCount:]
		}
		for _, s := range initSegs {
			insertLocked(s)
		}
	} else {
		for _, s := range segs {
			// 修复原项目 bug：只在双方都有 pdt 时才做"旧分片"过滤，
			// 避免 pdt 缺失时整段增量被误判丢弃。
			if s.pdt != "" && ch.lastPDT != "" && s.pdt <= ch.lastPDT {
				continue
			}
			insertLocked(s)
		}
	}
	for len(ch.order) > m.maxSegs {
		oldest := ch.order[0]
		ch.order = ch.order[1:]
		delete(ch.segments, oldest)
	}
	ch.LastError = ""
	ch.Segments = len(ch.segments)
	ch.mu.Unlock()

	if isInit && added > 0 {
		total := ch.SegmentCount()
		// 原文案：`[slug] 1080p 备用流缓冲就绪 (N 片)`，N 为当前缓存总分片数。
		logf(ch.Slug, "%s 备用流缓冲就绪 (%d 片)", defnLabel(ch.Defn), total)
	}
	return nil
}
