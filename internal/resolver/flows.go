package resolver

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/jacktea/ysp-live-go/internal/crypto"
	"github.com/jacktea/ysp-live-go/internal/httpx"
	"github.com/jacktea/ysp-live-go/internal/model"
	"github.com/jacktea/ysp-live-go/internal/util"
)

// 本文件对应原 Resolver 的取流三级协议：
// resolve_channel_once / live_v1_01_flow / live_v1_02_flow / vdn_getstream_flow，
// 以及播放头相关的辅助函数（default_cctv_playback_headers / cctv_playback_headers_for_entry /
// entry_missing_signed_playback_headers / ts_path_matches_final_url / compute_vdn_code）。

// resolveChannelOnce 解析一次频道，返回可缓存的 ChannelEntry（原 resolve_channel_once）。
func (r *Resolver) resolveChannelOnce(sess *AppSession, channel, liveID string, generation int) (*model.ChannelEntry, error) {
	live01, err := r.liveV101Flow(sess, liveID)
	if err != nil {
		return nil, err
	}
	appSecret, err := r.liveV102Flow(sess)
	if err != nil {
		return nil, err
	}
	vdn, err := r.vdnGetStreamFlow(sess.Identity, sess.Version, live01.LiveURL, appSecret)
	if err != nil {
		return nil, err
	}
	now := nowF64()
	playbackHeaders := defaultCCTVPlaybackHeaders(sess.Profile.AndroidID)
	playbackHeaders["APPRANDOMSTR"] = vdn.AppRandomStr
	playbackHeaders["APPSIGN"] = vdn.AppSign

	r.stateLock.Lock()
	sess.linkedChannels[channel] = true
	r.stateLock.Unlock()

	displayRateName := live01.RateName
	if displayRateName == "" {
		displayRateName = "高码率"
	}
	if channel == "cctv4k" || channel == "cctv164k" || channel == "cctv8k" {
		if strings.Contains(displayRateName, "8K") || displayRateName == "8K高清" || displayRateName == "8K超高清" {
			displayRateName = "4K超高清"
		}
	} else {
		switch displayRateName {
		case "8K高清", "4K高清", "8K超高清", "4K超高清":
			displayRateName = "高码率"
		}
	}

	return &model.ChannelEntry{
		Channel:           channel,
		LiveID:            liveID,
		FinalURL:          vdn.FinalURL,
		PlaybackHeaders:   playbackHeaders,
		AndroidID:         sess.Profile.AndroidID,
		XUID:              sess.Identity.XUID,
		Rate:              live01.Rate,
		RateName:          displayRateName,
		RawLiveHost:       util.URLHost(live01.LiveURL),
		FinalHost:         util.URLHost(vdn.FinalURL),
		RefreshedAt:       now,
		ExpiresAt:         now + r.cfg.CacheTTL,
		Generation:        generation,
		LastRefreshError:  "",
		LastRefreshFailed: 0,
		SessionGeneration: sess.Generation,
	}, nil
}

// liveV101Flow 调用 live/v1/01 取播放地址（原 live_v1_01_flow）。
//
// 选择策略（原样）：优先 rate == "36p"，否则取第一个可用条目。
func (r *Resolver) liveV101Flow(sess *AppSession, liveID string) (*Live01Result, error) {
	body := map[string]interface{}{
		"screenParam": sess.ScreenParam,
		"rate":        "",
		"systemType":  "ios",
		"model":       sess.CastModel,
		"id":          liveID,
		"userId":      model.DefaultLiveUserID,
		"clientSign":  "cctvVideo",
		"deviceId":    map[string]interface{}{"serial": "", "imei": "", "android_id": ""},
	}
	var resp *httpx.Response
	err := r.controlRequestSlot(func() error {
		headers := model.FreshHeaders(sess.Identity.Headers, "application/json", "application/json", nil)
		raw, err := sess.Client.PostJSON(r.eps.liveV101, headers, body)
		if err != nil {
			return err
		}
		resp = raw
		return nil
	})
	if err != nil {
		return nil, asResolverErr(err)
	}
	status, text, value := responseJSON(resp)
	if status < 200 || status >= 300 {
		return nil, errorf("live/v1/01 HTTP %d: %s", status, truncateString(text, 300))
	}

	// 原实现：videos = data.get('videoList') or data.get('videos')，
	// 空列表同样视为 falsy，因此这里只在非空时采用 videoList。
	var videos []interface{}
	if root, ok := value.(map[string]interface{}); ok {
		if data, ok := root["data"].(map[string]interface{}); ok {
			if list, ok := data["videoList"].([]interface{}); ok && len(list) > 0 {
				videos = list
			} else if list, ok := data["videos"].([]interface{}); ok {
				videos = list
			}
		}
	}
	if videos == nil {
		return nil, errorf("live/v1/01 missing videos")
	}

	var selected, fallback map[string]interface{}
	for _, item := range videos {
		video, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		rawURL, ok := video["url"].(string)
		if !ok || rawURL == "" {
			continue
		}
		if fallback == nil {
			fallback = video
		}
		if rate, _ := video["rate"].(string); rate == "36p" {
			selected = video
			break
		}
	}
	video := selected
	if video == nil {
		video = fallback
	}
	if video == nil {
		return nil, errorf("live/v1/01 no usable URL")
	}
	rawURL, _ := video["url"].(string)
	liveURL := rawURL
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		decrypted, derr := crypto.AESGCMDecryptB64(rawURL, sess.SessionKey)
		if derr != nil {
			return nil, asResolverErr(derr)
		}
		liveURL = decrypted
	}
	rate, _ := video["rate"].(string)
	rateName, _ := video["rateName"].(string)
	return &Live01Result{LiveURL: liveURL, Rate: rate, RateName: rateName}, nil
}

// liveV102Flow 调用 live/v1/02 取 appSecret（原 live_v1_02_flow）。
func (r *Resolver) liveV102Flow(sess *AppSession) (string, error) {
	var resp *httpx.Response
	err := r.controlRequestSlot(func() error {
		encryptedGUID, err := crypto.AESGCMEncryptB64("", sess.SessionKey)
		if err != nil {
			return err
		}
		body := map[string]interface{}{"guid": encryptedGUID}
		headers := model.FreshHeaders(sess.Identity.Headers, "application/json", "application/json", nil)
		raw, err := sess.Client.PostJSON(r.eps.liveV102, headers, body)
		if err != nil {
			return err
		}
		resp = raw
		return nil
	})
	if err != nil {
		return "", asResolverErr(err)
	}
	status, text, value := responseJSON(resp)
	if status < 200 || status >= 300 {
		return "", errorf("live/v1/02 HTTP %d: %s", status, truncateString(text, 300))
	}
	encrypted := ""
	if root, ok := value.(map[string]interface{}); ok {
		switch data := root["data"].(type) {
		case map[string]interface{}:
			for _, key := range []string{"appSecret", "app_secret"} {
				if s, ok := data[key].(string); ok {
					encrypted = s
					break
				}
			}
		case string:
			encrypted = data
		}
	}
	if encrypted == "" {
		return "", errorf("live/v1/02 missing appSecret")
	}
	secret, derr := crypto.AESGCMDecryptB64(encrypted, sess.SessionKey)
	if derr != nil {
		return "", asResolverErr(derr)
	}
	return secret, nil
}

// vdnGetStreamFlow 调用 VDN getstream 取最终播放地址（原 vdn_getstream_flow）。
func (r *Resolver) vdnGetStreamFlow(identity *model.Identity, version, liveURL, appSecret string) (*VdnGetStreamResult, error) {
	var resp *httpx.Response
	appSign, randomStr := computeVDNCode(appSecret)
	appcommon := model.VDNAppCommon(version)
	err := r.controlRequestSlot(func() error {
		headers := model.FreshHeaders(identity.Headers, "application/x-www-form-urlencoded", "", nil)
		headers["APPID"] = crypto.AK
		headers["APPSIGN"] = appSign
		headers["APPRANDOMSTR"] = randomStr
		raw, err := r.genericClient.PostForm(r.eps.vdnGetStream, headers, [][2]string{
			{"appcommon", appcommon},
			{"url", liveURL},
		})
		if err != nil {
			return err
		}
		resp = raw
		return nil
	})
	if err != nil {
		return nil, asResolverErr(err)
	}
	status, text, value := responseJSON(resp)
	if status < 200 || status >= 300 {
		return nil, errorf("VDN HTTP %d: %s", status, truncateString(text, 300))
	}
	if root, ok := value.(map[string]interface{}); ok {
		succeedStr := succeedToString(root["succeed"])
		if succeedStr == "1" {
			if url, ok := root["url"].(string); ok {
				return &VdnGetStreamResult{FinalURL: url, AppSign: appSign, AppRandomStr: randomStr}, nil
			}
		}
	}
	return nil, errorf("VDN did not return final URL: %s", truncateString(text, 400))
}

// succeedToString 复刻原 succeed 字段的字符串化逻辑：
//
//	succeed.strip('"') if isinstance(succeed, str)
//	else json.dumps(succeed, separators=(',',':')).strip('"') if succeed is not None else ''
func succeedToString(v interface{}) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return strings.Trim(val, `"`)
	case bool:
		if val {
			return "true"
		}
		return "false"
	case json.Number:
		return val.String()
	case float64:
		return fmt.Sprintf("%g", val)
	case int:
		return fmt.Sprint(val)
	default:
		return strings.Trim(compactString(v), `"`)
	}
}

// ---------------------------------------------------------------------------
// 播放头辅助（原 default_cctv_playback_headers 等）
// ---------------------------------------------------------------------------

// defaultCCTVPlaybackHeaders 默认播放头（原 default_cctv_playback_headers）。
func defaultCCTVPlaybackHeaders(uid string) map[string]string {
	return map[string]string{
		"UID":        uid,
		"APPID":      crypto.AK,
		"Referer":    "api.cctv.cn",
		"User-Agent": "cctv_app_tv",
	}
}

// cctvPlaybackHeadersForEntry 由缓存条目构造播放头（原 cctv_playback_headers_for_entry）。
func cctvPlaybackHeadersForEntry(entry *model.ChannelEntry) map[string]string {
	headers := defaultCCTVPlaybackHeaders(entry.AndroidID)
	for _, key := range []string{"UID", "APPID", "APPRANDOMSTR", "Referer", "User-Agent", "APPSIGN"} {
		if value := entry.PlaybackHeaders[key]; value != "" {
			headers[key] = value
		}
	}
	return headers
}

// entryMissingSignedPlaybackHeaders 该条目是否缺少必需的签名播放头
// （原 entry_missing_signed_playback_headers）。
func entryMissingSignedPlaybackHeaders(entry *model.ChannelEntry) bool {
	if entry == nil {
		return true
	}
	host := entry.FinalHost
	if host == "" {
		host = util.URLHost(entry.FinalURL)
	}
	if !playbackHostNeedsSignedHeaders(host) {
		return false
	}
	for _, key := range []string{"APPID", "APPSIGN", "APPRANDOMSTR"} {
		if strings.TrimSpace(entry.PlaybackHeaders[key]) == "" {
			return true
		}
	}
	return false
}

// tsPathMatchesFinalURL 分片路径是否属于该条目的 m3u8 目录（原 ts_path_matches_final_url）。
func tsPathMatchesFinalURL(tsPath, finalURL string) bool {
	u, err := url.Parse(finalURL)
	if err != nil {
		return false
	}
	prefix := strings.TrimSuffix(u.Path, ".m3u8")
	return prefix != "" && strings.HasPrefix(tsPath, prefix)
}

// computeVDNCode 计算 VDN 的 APPSIGN（原 compute_vdn_code：md5_hex(AK + app_secret + random_str)）。
func computeVDNCode(appSecret string) (string, string) {
	randomStr := util.GenerateAppRandomStr()
	return md5Hex(crypto.AK + appSecret + randomStr), randomStr
}

// md5Hex 小写十六进制 MD5（原 md5_hex）。
func md5Hex(value string) string {
	sum := md5.Sum([]byte(value))
	return hex.EncodeToString(sum[:])
}

// compactString 紧凑 JSON（键排序），用于 succeed 字段字符串化。
func compactString(v interface{}) string {
	data, err := httpx.CompactJSON(v)
	if err != nil {
		return ""
	}
	return string(data)
}
