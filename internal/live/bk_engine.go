package live

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jacktea/ysp-live-go/internal/crypto"
	"github.com/jacktea/ysp-live-go/internal/logx"
)

// logf 输出中文日志（等价原 log(msg, min_interval)）。
func logf(slug, format string, args ...interface{}) {
	logx.Logf(1.5, "["+slug+"] "+format, args...)
}

// logfInterval 指定去抖间隔的日志。
func logfInterval(interval float64, slug, format string, args ...interface{}) {
	logx.Logf(interval, "["+slug+"] "+format, args...)
}

// bkPlayurls 通过 bkliveinfo 接口获取备用播放地址列表。
// 对应原 bk_playurls(channel_id, live_pid, defn='fhd')：
//   - 用 crypto.CKey 生成 cKey/guid/ts/flowId，用 crypto.BKPlayURLsQuery 构造有序查询串；
//   - 请求头固定 User-Agent=qqlive、Accept=application/json；
//   - iretcode != 0 时抛 "iretcode=%s %s" 风格错误（错误文本原样保留）；
//   - 合并 playurl 与 backurl_list/backurlList/backurl（字符串或列表两种形态），
//     去重、只保留含 ".cctv." 的地址、排序时 bklive- 优先。
func (m *Manager) bkPlayurls(channelID, livePID, defn string) ([]string, error) {
	if defn == "" {
		defn = "fhd"
	}
	ck := crypto.CKey(channelID)
	query := crypto.BKPlayURLsQuery(channelID, livePID, defn, ck["cKey"], ck["guid"], ck["ts"], ck["flowId"])

	target := m.bkURL
	if !strings.Contains(target, "?") {
		target += "?"
	}
	target += query

	m.waitSlot()
	headers := map[string]string{
		"User-Agent": "qqlive",
		"Accept":     "application/json",
	}
	resp, err := m.bkClient.Get(target, headers)
	if err != nil {
		return nil, fmt.Errorf("bk_playurls: %w", err)
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return nil, fmt.Errorf("bk_playurls: %w", &httpStatusError{code: resp.Status})
	}
	payload, perr := decodeJSONObject(resp.Body)
	if perr != nil {
		return nil, fmt.Errorf("bk_playurls: %w", perr)
	}
	if code := jsonInt(payload["iretcode"], -1); code != 0 {
		return nil, fmt.Errorf("iretcode=%v %v", payload["iretcode"], jsonString(payload["errinfo"]))
	}

	raw := make([]string, 0, 4)
	if u := jsonString(payload["playurl"]); u != "" {
		raw = append(raw, u)
	}
	if bu, ok := payload["backurl_list"]; ok {
		raw = append(raw, flattenBackURL(bu)...)
	} else if bu, ok := payload["backurlList"]; ok {
		raw = append(raw, flattenBackURL(bu)...)
	} else if bu, ok := payload["backurl"]; ok {
		raw = append(raw, flattenBackURL(bu)...)
	}

	// 去重（保序）→ 过滤 .cctv.
	seen := make(map[string]bool, len(raw))
	urls := make([]string, 0, len(raw))
	for _, u := range raw {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		if !strings.Contains(u, ".cctv.") {
			continue
		}
		urls = append(urls, u)
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("no playurl")
	}
	// 与原实现一致：bklive- 前缀优先，其次按地址字典序。
	sort.SliceStable(urls, func(i, j int) bool {
		pi, pj := 1, 1
		if strings.Contains(urls[i], "bklive-") {
			pi = 0
		}
		if strings.Contains(urls[j], "bklive-") {
			pj = 0
		}
		if pi != pj {
			return pi < pj
		}
		return urls[i] < urls[j]
	})
	return urls, nil
}

// flattenBackURL 展开 backurl 字段：支持字符串（[,;] 分隔）与列表（字符串或对象）两种形态。
// 对应原 bk_playurls 中对 bu 的 isinstance(list)/isinstance(str) 分支。
func flattenBackURL(v interface{}) []string {
	switch val := v.(type) {
	case nil:
		return nil
	case string:
		out := make([]string, 0, 2)
		for _, part := range strings.FieldsFunc(val, func(r rune) bool {
			return r == ';' || r == ',' || r == '['
		}) {
			part = strings.TrimSpace(strings.Trim(part, "]"))
			if part != "" {
				out = append(out, part)
			}
		}
		return out
	case []interface{}:
		out := make([]string, 0, len(val))
		for _, it := range val {
			switch item := it.(type) {
			case string:
				out = append(out, item)
			case map[string]interface{}:
				for _, k := range []string{"url", "playurl"} {
					if s := jsonString(item[k]); s != "" {
						out = append(out, s)
						break
					}
				}
			}
		}
		return out
	case map[string]interface{}:
		for _, k := range []string{"url", "playurl"} {
			if s := jsonString(val[k]); s != "" {
				return []string{s}
			}
		}
	}
	return nil
}

// bkRefresh 拉取并缓存备用线路播放列表。
// 对应原 bk_refresh(ch)：
//   - BK_URL_TTL 内复用地址，过期则重新获取；
//   - 逐个候选地址尝试 fetchAbsPlaylist，成功即保存；
//   - 第一轮全失败时重新取一次地址再试（原实现的"地址已过期，已重新拉取"）；
//   - 彻底失败时清空地址缓存并返回带上下文的非空错误。
//
// 修复原项目 bug（task-2 第 3 条）：原实现第二轮重取地址后没有重置 last_err，
// 若第一轮 last_err 从未赋值（例如候选列表为空）会抛出 "bk playlist failed" 这类
// 丢失细节的错误。Go 版保证错误文本始终非空且带上下文。
func (m *Manager) bkRefresh(ch *Channel) error {
	ch.mu.Lock()
	urls := append([]string(nil), ch.bkURLs...)
	urlsAt := ch.bkURLsAt
	ch.mu.Unlock()

	now := m.now()
	if len(urls) == 0 || now.Sub(urlsAt) > m.bkTTL {
		fresh, err := m.bkPlayurls(ch.SID, ch.PID, ch.Defn)
		if err != nil {
			return err
		}
		ch.mu.Lock()
		ch.bkURLs = fresh
		ch.bkURLsAt = now
		urls = append([]string(nil), fresh...)
		ch.mu.Unlock()
		logfInterval(60.0, ch.Slug, "bkliveinfo 备用通道就绪 (%d 地址)", len(fresh))
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		for _, u := range urls {
			pl, err := m.fetchAbsPlaylist(u, 0)
			if err != nil {
				lastErr = err
				continue
			}
			if !strings.Contains(pl, "#EXTM3U") {
				lastErr = fmt.Errorf("playlist missing #EXTM3U: %s", trimForLog(u, 80))
				continue
			}
			ch.mu.Lock()
			ch.bkPl = pl
			ch.LastError = ""
			ch.mu.Unlock()
			return nil
		}
		if attempt != 0 {
			break
		}
		refreshed, err := m.bkPlayurls(ch.SID, ch.PID, ch.Defn)
		if err != nil {
			// 重取失败：保留原始错误为上下文，避免错误信息被清空。
			if lastErr == nil {
				lastErr = err
			} else {
				lastErr = fmt.Errorf("%w (重取地址失败: %v)", lastErr, err)
			}
			break
		}
		ch.mu.Lock()
		ch.bkURLs = refreshed
		ch.bkURLsAt = m.now()
		urls = append([]string(nil), refreshed...)
		ch.mu.Unlock()
		logfInterval(10.0, ch.Slug, "%s 地址已过期，已重新拉取", defnLabel(ch.Defn))
	}

	ch.mu.Lock()
	ch.bkURLs = nil
	ch.bkURLsAt = timeZero
	ch.mu.Unlock()

	if lastErr == nil {
		lastErr = fmt.Errorf("bk playlist 无可用备用地址")
	}
	msg := lastErr.Error()
	if r := []rune(msg); len(r) > maxErrLen {
		msg = string(r[:maxErrLen])
	}
	return fmt.Errorf("bk playlist failed: %s", msg)
}
