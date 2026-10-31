package resolver

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jacktea/ysp-live-go/internal/httpx"
	"github.com/jacktea/ysp-live-go/internal/logx"
	"github.com/jacktea/ysp-live-go/internal/model"
	"github.com/jacktea/ysp-live-go/internal/util"
)

// 本文件对应原 Resolver 的播放列表面：
// fetch_channel_playlist / fetch_playlist / proxy_playlist_ts_urls /
// playback_headers_for_ts_url / fetch_playlist_http1 / _playback_request_headers。

// ChannelPlaylist 获取频道 m3u8（原 fetch_channel_playlist），返回已改写为 /proxy.ts?=BASE64 的文本。
//
// 特殊约定：换台防抖命中时返回 ("", nil)，表示"先走下层的 1080p"。
func (r *Resolver) ChannelPlaylist(ctx context.Context, channel, hostHeader string) (string, error) {
	channel = model.Canonical(channel)
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return "", err
		}
	}
	r.stateLock.Lock()
	r.state.channelLastAccess[channel] = nowF64()
	r.stateLock.Unlock()

	if r.zapDebounced(channel) {
		logx.Log(fmt.Sprintf("[%s] 换台防抖: 先出 1080p, 停留后自动升码率", channel), 10)
		return "", nil
	}
	host := strings.TrimSpace(hostHeader)
	if host == "" {
		host = fmt.Sprintf("localhost:%d", r.cfg.ServerPort)
	}
	proxyOrigin := "http://" + host

	entry, err := r.ensureChannel(channel, false)
	if err != nil {
		return "", err
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		body, _, _, ferr := r.fetchPlaylist(entry, proxyOrigin, "")
		if ferr == nil {
			r.clearChannelCooldown(channel)
			return body, nil
		}
		lastErr = ferr
		if isExpiredPlaylistError(ferr.Error()) && attempt == 0 {
			logx.Log(fmt.Sprintf("[%s] 高码率播放地址已过期，正在极速重取新地址…", channel), 4)
			r.stateLock.Lock()
			delete(r.state.cache, channel)
			delete(r.state.playlistCache, channel)
			r.stateLock.Unlock()
			if entry, err = r.ensureChannel(channel, true); err != nil {
				return "", err
			}
			continue
		}
		break
	}
	return "", lastErr
}

// fetchPlaylist 抓取并改写播放列表，带短时缓存（原 fetch_playlist）。
//
// 与原实现一致：命中缓存直接返回；miss 时按频道加锁双击检查；
// 上游失败时若 20 秒内有旧缓存则降级返回旧值。
func (r *Resolver) fetchPlaylist(entry *model.ChannelEntry, proxyOrigin, proxyPrefix string) (string, string, PlaylistCacheProbe, error) {
	now := nowF64()
	r.stateLock.Lock()
	probe := inspectPlaylistCache(r.state.playlistCache[entry.Channel], entry, proxyOrigin, proxyPrefix, now)
	if probe.State == "hit" {
		cached := r.state.playlistCache[entry.Channel]
		body, ctype := cached.Body, cached.ContentType
		r.stateLock.Unlock()
		return body, ctype, probe, nil
	}
	r.stateLock.Unlock()
	missProbe := probe

	lock := r.playlistLock(entry.Channel)
	lock.Lock()
	defer lock.Unlock()

	now = nowF64()
	r.stateLock.Lock()
	probe = inspectPlaylistCache(r.state.playlistCache[entry.Channel], entry, proxyOrigin, proxyPrefix, now)
	if probe.State == "hit" {
		cached := r.state.playlistCache[entry.Channel]
		body, ctype := cached.Body, cached.ContentType
		r.stateLock.Unlock()
		return body, ctype, probe, nil
	}
	r.stateLock.Unlock()
	missProbe = probe

	playbackHeaders := cctvPlaybackHeadersForEntry(entry)
	status, contentType, text, err := fetchPlaylistHTTP1(entry.FinalURL, playbackHeaders)
	if err != nil {
		r.recordRecoverableError(err.Error())
		if body, ctype, ok := r.recentPlaylistCache(entry.Channel, 20.0); ok {
			return body, ctype, missProbe, nil
		}
		return "", "", missProbe, asResolverErr(err)
	}
	if status < 200 || status >= 300 {
		fetchErr := errorf("upstream m3u8 HTTP %d: %s", status, truncateString(text, 300))
		r.recordRecoverableError(fetchErr.Error())
		if body, ctype, ok := r.recentPlaylistCache(entry.Channel, 20.0); ok {
			return body, ctype, missProbe, nil
		}
		return "", "", missProbe, fetchErr
	}
	r.clearRecoverableErrorCount()

	body := util.RewritePlaylistURLs(text, entry.FinalURL)
	body = r.proxyPlaylistTSURLs(body, proxyOrigin, proxyPrefix)

	if r.cfg.PlaylistCacheTTL > 0 {
		now = nowF64()
		cached := &PlaylistCacheEntry{
			Body:        body,
			ContentType: contentType,
			CachedAt:    now,
			ExpiresAt:   now + r.cfg.PlaylistCacheTTL,
			FinalURL:    entry.FinalURL,
			AndroidID:   entry.AndroidID,
			ProxyOrigin: proxyOrigin,
			ProxyPrefix: proxyPrefix,
		}
		r.stateLock.Lock()
		r.state.playlistCache[entry.Channel] = cached
		r.stateLock.Unlock()
	}
	return body, contentType, missProbe, nil
}

// recentPlaylistCache 取最近 20 秒内的播放列表缓存（原 fetch_playlist 的降级分支）。
func (r *Resolver) recentPlaylistCache(channel string, maxAge float64) (string, string, bool) {
	r.stateLock.Lock()
	defer r.stateLock.Unlock()
	cached := r.state.playlistCache[channel]
	if cached != nil && nowF64()-cached.CachedAt < maxAge {
		return cached.Body, cached.ContentType, true
	}
	return "", "", false
}

// playlistLock 取频道级播放列表锁（原 playlist_locks）。
func (r *Resolver) playlistLock(channel string) *sync.Mutex {
	r.playlistLocksLock.Lock()
	defer r.playlistLocksLock.Unlock()
	lock := r.playlistLocks[channel]
	if lock == nil {
		lock = &sync.Mutex{}
		r.playlistLocks[channel] = lock
	}
	return lock
}

// proxyPlaylistTSURLs 把 m3u8 中的 .ts 分片改写成 /proxy.ts?=BASE64（原 proxy_playlist_ts_urls）。
//
// proxyPrefix 非空时使用 "/proxy?ts=" 前缀与原项目兼容；否则使用 "/proxy.ts?="。
func (r *Resolver) proxyPlaylistTSURLs(playlist, proxyOrigin string, proxyPrefix ...string) string {
	// 原实现用 proxy_prefix=None 表示"不分片前缀"，此时 origin 取 proxy_origin、查询串为 "?="。
	// Go 版用空字符串表达 None，因此这里把空前缀同样视为未提供。
	prefixProvided := len(proxyPrefix) > 0 && strings.TrimSpace(proxyPrefix[0]) != ""
	prefix := proxyOrigin
	proxyQuery := "/proxy.ts?="
	if prefixProvided {
		prefix = proxyPrefix[0]
		proxyQuery = "/proxy?ts="
	}
	origin := strings.TrimRight(prefix, "/")

	var out strings.Builder
	for _, line := range util.RSLines(playlist) {
		stripped := strings.TrimSpace(line)
		switch {
		case stripped == "" || strings.HasPrefix(stripped, "#"):
			out.WriteString(line)
		case util.IsTSURL(stripped):
			encoded := base64.URLEncoding.EncodeToString([]byte(stripped))
			encoded = strings.TrimRight(encoded, "=")
			out.WriteString(origin)
			out.WriteString(proxyQuery)
			out.WriteString(encoded)
		default:
			out.WriteString(line)
		}
		out.WriteString("\n")
	}
	return out.String()
}

// PlaybackHeadersForTSURL 为分片地址匹配缓存的播放头（原 playback_headers_for_ts_url）。
func (r *Resolver) PlaybackHeadersForTSURL(rawURL string) (map[string]string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, errorf("invalid proxy ts URL: %v", err)
	}
	host := u.Hostname()
	if host == "" {
		return nil, errorf("proxy ts URL missing host")
	}
	tsPath := u.Path

	var sameHostHeaders map[string]string
	r.stateLock.Lock()
	// 遍历顺序固定（按频道名排序）：Python dict 保序，Go map 无序，
	// 固定顺序可保证同 host 兜底时选中的条目稳定、可复现。
	channels := make([]string, 0, len(r.state.cache))
	for channel := range r.state.cache {
		channels = append(channels, channel)
	}
	r.stateLock.Unlock()
	sort.Strings(channels)

	r.stateLock.Lock()
	for _, channel := range channels {
		entry := r.state.cache[channel]
		if entry == nil || entryMissingSignedPlaybackHeaders(entry) {
			continue
		}
		entryHost := entry.FinalHost
		if entryHost == "" {
			entryHost = util.URLHost(entry.FinalURL)
		}
		if !strings.EqualFold(entryHost, host) {
			continue
		}
		headers := cctvPlaybackHeadersForEntry(entry)
		if tsPathMatchesFinalURL(tsPath, entry.FinalURL) {
			r.stateLock.Unlock()
			return headers, nil
		}
		if sameHostHeaders == nil {
			sameHostHeaders = headers
		}
	}
	r.stateLock.Unlock()

	if sameHostHeaders != nil {
		return sameHostHeaders, nil
	}
	if playbackHostNeedsSignedHeaders(host) {
		return nil, errorf("missing cached signed playback headers for %s", host)
	}
	uid := r.currentCCTVAndroidID()
	if uid == "" {
		// 原实现：current_cctv_android_id 找不到身份时 raise YsptpError('missing cctv android_id')。
		return nil, errorf("missing cctv android_id")
	}
	return defaultCCTVPlaybackHeaders(uid), nil
}

// PlaybackHeaders 兼容 httpapi.SegmentFetcher 接口的方法名。
func (r *Resolver) PlaybackHeaders(rawURL string) (map[string]string, error) {
	return r.PlaybackHeadersForTSURL(rawURL)
}

// currentCCTVAndroidID 当前使用的 android_id（原 current_cctv_android_id）。
func (r *Resolver) currentCCTVAndroidID() string {
	// 原实现（current_cctv_android_id）：
	//   1) os.path.exists(device_json) 时用文件里的 android_id；
	//   2) 否则退到 slot0 会话；
	//   3) 再退到任一缓存条目的 android_id；
	//   4) 都取不到才 raise 'missing cctv android_id'。
	// 原实现只要 device json 存在就用其中（或 Load 失败时随机生成的）android_id，
	// 不额外要求"解析成功"标志，这里保持一致。
	if path := strings.TrimSpace(r.cfg.DeviceJSON); path != "" {
		if _, err := os.Stat(path); err == nil {
			store := model.NewDeviceStateStore(path)
			if state, _ := store.Load(); state != nil && state.Profile.AndroidID != "" {
				return state.Profile.AndroidID
			}
		}
	}
	r.stateLock.Lock()
	defer r.stateLock.Unlock()
	if sess := r.appSessionLocked(); sess != nil && sess.Profile.AndroidID != "" {
		return sess.Profile.AndroidID
	}
	for _, entry := range r.state.cache {
		if entry.AndroidID != "" {
			return entry.AndroidID
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 手动 HTTP/1.1 播放列表抓取（原 fetch_playlist_http1 / _playback_request_headers）
// ---------------------------------------------------------------------------

// playbackRequestHeaders 构造播放请求头（原 _playback_request_headers）。
func playbackRequestHeaders(playbackHeaders map[string]string, extra map[string]string) map[string]string {
	headers := make(map[string]string, 10)
	for _, key := range []string{"UID", "APPID", "APPRANDOMSTR", "Referer", "User-Agent", "APPSIGN"} {
		if value, ok := playbackHeaders[key]; ok && strings.TrimSpace(value) != "" {
			headers[key] = util.HeaderValueClean(value)
		}
	}
	headers["Accept"] = "*/*"
	headers["Accept-Encoding"] = "identity"
	headers["Connection"] = "close"
	for k, v := range extra {
		headers[k] = v
	}
	return headers
}

// fetchPlaylistHTTP1 手动发 HTTP/1.1 请求上游 m3u8。
//
// 与原实现一致：
//   - 只接受 http:// 地址（原实现 raise "manual playlist fetch currently expects http URL"）；
//   - 手动设置 Host 头（不能用 net/http 的默认行为，因为签名头要求 Host 与 CDN 一致）；
//   - Connection: close，超时 20s。
//
// 实现上用 net/http + 关闭 HTTP/2（Transport.ForceAttemptHTTP2=false）并显式设置 Host 头，
// 语义与 Python http.client.HTTPConnection 一致，同时复用连接池与错误归一化。
func fetchPlaylistHTTP1(finalURL string, playbackHeaders map[string]string) (int, string, string, error) {
	u, err := url.Parse(finalURL)
	if err != nil {
		return 0, "", "", errorf("invalid final URL: %v", err)
	}
	if u.Scheme != "http" {
		return 0, "", "", errorf("manual playlist fetch currently expects http URL, got %s", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return 0, "", "", errorf("final URL missing host")
	}
	port := u.Port()
	if port == "" {
		port = "80"
	}
	hostHeader := host
	if u.Port() != "" {
		hostHeader = host + ":" + u.Port()
	}
	target := u.Path
	if target == "" {
		target = "/"
	}
	if u.RawQuery != "" {
		target += "?" + u.RawQuery
	}

	headers := playbackRequestHeaders(playbackHeaders, map[string]string{"Host": hostHeader})

	transport := &http.Transport{
		Proxy:              nil,
		DisableCompression: true,
		ForceAttemptHTTP2:  false, // 仅 HTTP/1.1，与原 http.client.HTTPConnection 一致
		DialContext: (&net.Dialer{
			Timeout:   20 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   20 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	req, err := http.NewRequest(http.MethodGet, "http://"+hostHeader+target, nil)
	if err != nil {
		return 0, "", "", asResolverErr(err)
	}
	for k, v := range headers {
		if strings.EqualFold(k, "Host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", "", asResolverErr(httpx.NormalizeNetworkError(err))
	}
	defer resp.Body.Close()

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/vnd.apple.mpegurl"
	}
	buf := make([]byte, 0, 64*1024)
	tmp := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if len(buf) > 16<<20 {
				// 播放列表不应这么大；截断保护避免异常上游拖垮内存。
				break
			}
		}
		if rerr != nil {
			if rerr.Error() == "EOF" {
				break
			}
			return 0, "", "", asResolverErr(httpx.NormalizeNetworkError(rerr))
		}
	}
	return resp.StatusCode, contentType, strings.ToValidUTF8(string(buf), "\uFFFD"), nil
}
