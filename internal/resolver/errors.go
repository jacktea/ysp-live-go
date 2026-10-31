package resolver

import (
	"errors"
	"fmt"
	"strings"
)

// Err 是本包所有协议错误的基类（原 YsptpError）。
//
// 上层（gateway）依靠错误文本片段判断是否换设备 / 冷却，因此错误文案必须保留原实现的关键片段：
// "HTTP 400"、"内部错误"、"device link quota reached"、"no usable URL" 等。
var Err = errors.New("ysp-engine")

// errorf 构造带 Err 前缀的错误（原 raise YsptpError(msg)）。
func errorf(format string, args ...interface{}) error {
	msg := fmt.Sprintf(format, args...)
	return fmt.Errorf("%w: %s", Err, msg)
}

// 识别函数（原 has_http_400 / is_session_core_http_400 / is_control_plane_http_400 等）。
//
// 原实现全部基于小写文本子串判断，这里保持同样的语义（调用方可能已转小写）。

// hasHTTP400 文本是否包含 HTTP 400（原 has_http_400）。
func hasHTTP400(lower string) bool {
	return strings.Contains(lower, "http 400") || strings.Contains(lower, "status=400")
}

// isSessionCoreHTTP400 是否是会话核心接口的 400（原 is_session_core_http_400）。
func isSessionCoreHTTP400(lower string) bool {
	if !hasHTTP400(lower) {
		return false
	}
	for _, needle := range []string{"app/start", "live/v1/01", "live/v1/02", "vdn", "heartbeat"} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// isControlPlaneHTTP400 是否是控制面接口的 400（原 is_control_plane_http_400）。
func isControlPlaneHTTP400(lower string) bool {
	if !hasHTTP400(lower) {
		return false
	}
	for _, needle := range []string{
		"app/start", "dictionary", "app event", "page event", "cloud_get", "cloud get",
		"cloud_register", "cloud register", "device info", "heartbeat", "index",
		"drm config", "version config", "live/v1/01", "live/v1/02", "vdn", "collect report",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// isSessionInvalidatingError 该错误是否意味着会话（session_key/身份）已不可用（原 is_session_invalidating_error）。
func isSessionInvalidatingError(message string) bool {
	lower := strings.ToLower(message)
	if strings.Contains(lower, "app/start") ||
		strings.Contains(lower, "decrypt") ||
		strings.Contains(lower, "session_key") ||
		strings.Contains(lower, "missing encrypted session key") ||
		isSessionCoreHTTP400(lower) {
		return true
	}
	for _, needle := range []string{
		"live/v1/01 http 400", "live/v1/01 http 401", "live/v1/01 http 403",
		"live/v1/02 http 400", "live/v1/02 http 401", "live/v1/02 http 403",
		"vdn http 400", "vdn http 401", "vdn http 403",
		"内部错误", "no usable url", "missing videos",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// isIdentityRecoverableError 该错误是否可通过重置身份/会话恢复（原 is_identity_recoverable_error）。
func isIdentityRecoverableError(message string) bool {
	lower := strings.ToLower(message)
	if isSessionInvalidatingError(message) || isControlPlaneHTTP400(lower) {
		return true
	}
	for _, needle := range []string{
		"upstream m3u8 http 400", "upstream m3u8 http 401", "upstream m3u8 http 403",
		"upstream m3u8 http 404", "playlist response missing", "invalid playlist status",
		"connection reset", "connection refused", "operation timed out", "timed out",
		"network is unreachable", "nodename nor servname", "failed to lookup address",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// hasHTTPStatus 文本是否包含给定的状态码片段（如 "HTTP 403"）。
func hasHTTPStatus(message string, status int) bool {
	return strings.Contains(message, fmt.Sprintf("HTTP %d", status)) ||
		strings.Contains(message, fmt.Sprintf("status=%d", status))
}

// isExpiredPlaylistError 播放列表地址过期类错误，需要强制重取（原 fetch_channel_playlist 的判断）。
func isExpiredPlaylistError(message string) bool {
	return hasHTTPStatus(message, 403) || hasHTTPStatus(message, 404) ||
		hasHTTPStatus(message, 401) || strings.Contains(message, "upstream m3u8")
}

// cooldownForFailCount 失败次数对应的冷却秒数（原 is_channel_in_cooldown 的分支）。
func cooldownForFailCount(failCount int) float64 {
	switch {
	case failCount <= 1:
		return 15.0
	case failCount == 2:
		return 60.0
	default:
		return 120.0
	}
}

// httpStatusText 状态码 + 原因（原 http_status_text）。
func httpStatusText(status int, reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Sprint(status)
	}
	return fmt.Sprintf("%d %s", status, reason)
}

// playbackHostNeedsSignedHeaders 该播放域名是否要求签名头（原 live_playback_host_needs_signed_headers）。
func playbackHostNeedsSignedHeaders(host string) bool {
	lower := strings.ToLower(host)
	return strings.Contains(lower, "liveali") || strings.Contains(lower, "liveten")
}
