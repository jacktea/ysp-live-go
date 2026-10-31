package resolver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jacktea/ysp-live-go/internal/httpapi"
	"github.com/jacktea/ysp-live-go/internal/httpx"
	"github.com/jacktea/ysp-live-go/internal/model"
)

// 本文件对应原 Resolver 的状态快照与持久化：
// status_json / write_meta / load_cache。

// CacheEntryStatus 状态快照里的单频道缓存视图（原 status_json 里的 cache[k]）。
type CacheEntryStatus struct {
	LiveID            string  `json:"live_id"`
	Fresh             bool    `json:"fresh"`
	AgeSeconds        float64 `json:"age_seconds"`
	ExpiresInSeconds  float64 `json:"expires_in_seconds"`
	Rate              string  `json:"rate"`
	RateName          string  `json:"rate_name"`
	FinalHost         string  `json:"final_host"`
	Generation        int     `json:"generation"`
	SessionGeneration int     `json:"session_generation"`
	LastRefreshError  string  `json:"last_refresh_error"`
}

// PlaylistCacheStatus 状态快照里的播放列表缓存视图。
type PlaylistCacheStatus struct {
	Fresh            bool    `json:"fresh"`
	AgeSeconds       float64 `json:"age_seconds"`
	ExpiresInSeconds float64 `json:"expires_in_seconds"`
	Bytes            int     `json:"bytes"`
	ContentType      string  `json:"content_type"`
	ProxyOrigin      string  `json:"proxy_origin"`
	ProxyPrefix      string  `json:"proxy_prefix"`
}

// RefreshQueueStatus 后台刷新队列视图。
type RefreshQueueStatus struct {
	Limit             int      `json:"limit"`
	QueuedDepth       int      `json:"queued_depth"`
	QueuedChannels    []string `json:"queued_channels"`
	RunningChannel    string   `json:"running_channel"`
	RunningAgeSeconds *float64 `json:"running_age_seconds"`
	EnqueuedTotal     int64    `json:"enqueued_total"`
	DroppedTotal      int64    `json:"dropped_total"`
	CompletedTotal    int64    `json:"completed_total"`
	FailedTotal       int64    `json:"failed_total"`
	LastEnqueuedAge   *float64 `json:"last_enqueued_age_seconds"`
	LastFinishedAge   *float64 `json:"last_finished_age_seconds"`
	LastDroppedAge    *float64 `json:"last_dropped_age_seconds"`
	LastDropReason    string   `json:"last_drop_reason"`
}

// HTTPQueueStatus HTTP 队列视图。
type HTTPQueueStatus struct {
	Workers          int      `json:"workers"`
	Limit            int      `json:"limit"`
	QueuedDepth      int      `json:"queued_depth"`
	ActiveWorkers    int      `json:"active_workers"`
	EnqueuedTotal    int64    `json:"enqueued_total"`
	RejectedTotal    int64    `json:"rejected_total"`
	CompletedTotal   int64    `json:"completed_total"`
	PanickedTotal    int64    `json:"panicked_total"`
	LastEnqueuedAge  *float64 `json:"last_enqueued_age_seconds"`
	LastCompletedAge *float64 `json:"last_completed_age_seconds"`
	LastRejectedAge  *float64 `json:"last_rejected_age_seconds"`
}

// StatusInfo /diag 与 /health 需要的状态快照（原 status_json 的返回结构）。
//
// JSON 字段名与原实现保持一致，便于运维对照。
type StatusInfo struct {
	Channels                     []string                       `json:"channels"`
	CacheTTLSeconds              float64                        `json:"cache_ttl_seconds"`
	StaleWhileRefreshTTLSeconds  float64                        `json:"stale_while_refresh_ttl_seconds"`
	RefreshErrorCooldownSeconds  float64                        `json:"refresh_error_cooldown_seconds"`
	PlaylistCacheTTLSeconds      float64                        `json:"playlist_cache_ttl_seconds"`
	BackgroundRefreshQueueLimit  int                            `json:"background_refresh_queue_limit"`
	HTTPWorkers                  int                            `json:"http_workers"`
	HTTPQueueLimit               int                            `json:"http_queue_limit"`
	IdentityResetErrorThreshold  int                            `json:"identity_reset_error_threshold"`
	IdentityResetCooldownSeconds float64                        `json:"identity_reset_cooldown_seconds"`
	RefreshIntervalSeconds       float64                        `json:"refresh_interval_seconds"`
	ControlStepJitterMinMs       int                            `json:"control_step_jitter_min_ms"`
	ControlStepJitterMaxMs       int                            `json:"control_step_jitter_max_ms"`
	HeartbeatIntervalSeconds     float64                        `json:"heartbeat_interval_seconds"`
	HeartbeatTTLGuardSeconds     float64                        `json:"heartbeat_ttl_guard_seconds"`
	SessionTTLSeconds            float64                        `json:"session_ttl_seconds"`
	MetaJSON                     string                         `json:"meta_json"`
	DeviceJSON                   string                         `json:"device_json"`
	DeviceState                  map[string]interface{}         `json:"device_state"`
	RefreshingChannel            string                         `json:"refreshing_channel"`
	BackgroundRefreshing         []string                       `json:"background_refreshing"`
	BackgroundRefreshQueue       RefreshQueueStatus             `json:"background_refresh_queue"`
	HTTPQueue                    HTTPQueueStatus                `json:"http_queue"`
	Generation                   int                            `json:"generation"`
	AppSession                   map[string]interface{}         `json:"app_session"`
	LastBusinessEndAt            float64                        `json:"last_business_end_at"`
	LastError                    string                         `json:"last_error"`
	IdentityResetErrorCount      int                            `json:"identity_reset_error_count"`
	IdentityResetCount           int                            `json:"identity_reset_count"`
	LastIdentityResetAt          float64                        `json:"last_identity_reset_at"`
	LastIdentityResetReason      string                         `json:"last_identity_reset_reason"`
	AdDisplay                    bool                           `json:"ad_display"`
	DeviceProfilePoolSize        int                            `json:"device_profile_pool_size"`
	Cache                        map[string]CacheEntryStatus    `json:"cache"`
	PlaylistCache                map[string]PlaylistCacheStatus `json:"playlist_cache"`
	// Ready / LinksLimit 便于 /health 直接使用（原实现由调用方另取）。
	Ready        bool   `json:"ready"`
	LinksLimit   int    `json:"links_limit"`
	LinksUsed    int    `json:"links_used"`
	Heartbeat    int64  `json:"heartbeat_count"`
	GUID         string `json:"guid"`
	SessionValid bool   `json:"session_valid"`
	SlotCount    int    `json:"slot_count"`
}

// Status 返回状态快照（原 status_json）。
func (r *Resolver) Status() StatusInfo {
	now := nowF64()

	r.stateLock.Lock()
	cacheSnapshot := make(map[string]*model.ChannelEntry, len(r.state.cache))
	for k, v := range r.state.cache {
		cacheSnapshot[k] = cloneEntry(v)
	}
	playlistSnapshot := make(map[string]*PlaylistCacheEntry, len(r.state.playlistCache))
	for k, v := range r.state.playlistCache {
		playlistSnapshot[k] = v
	}
	backgroundRefreshing := make([]string, 0, len(r.state.backgroundRefreshing))
	for k := range r.state.backgroundRefreshing {
		backgroundRefreshing = append(backgroundRefreshing, k)
	}
	sort.Strings(backgroundRefreshing)
	sess := r.appSessionLocked()
	generation := r.state.generation
	sessionGeneration := r.state.sessionGeneration
	refreshingChannel := r.state.refreshingChannel
	lastError := r.state.lastError
	lastBusinessEndAt := r.state.lastBusinessEndAt
	identityResetErrorCount := r.state.identityResetErrorCount
	identityResetCount := r.state.identityResetCount
	lastIdentityResetAt := r.state.lastIdentityResetAt
	lastIdentityResetReason := r.state.lastIdentityResetReason
	adDisplay := r.state.adDisplay
	var appSession map[string]interface{}
	if sess != nil {
		appSession = r.appSessionJSON(sess, now)
	} else {
		appSession = map[string]interface{}{"active": false, "generation": sessionGeneration}
	}
	r.stateLock.Unlock()

	cache := make(map[string]CacheEntryStatus, len(cacheSnapshot))
	for k, v := range cacheSnapshot {
		cache[k] = CacheEntryStatus{
			LiveID:            v.LiveID,
			Fresh:             v.Fresh(now),
			AgeSeconds:        rustRound((now-v.RefreshedAt)*1000.0) / 1000.0,
			ExpiresInSeconds:  rustRound((v.ExpiresAt-now)*1000.0) / 1000.0,
			Rate:              v.Rate,
			RateName:          v.RateName,
			FinalHost:         v.FinalHost,
			Generation:        v.Generation,
			SessionGeneration: v.SessionGeneration,
			LastRefreshError:  v.LastRefreshError,
		}
	}
	playlistCache := make(map[string]PlaylistCacheStatus, len(playlistSnapshot))
	for k, v := range playlistSnapshot {
		playlistCache[k] = PlaylistCacheStatus{
			Fresh:            now < v.ExpiresAt,
			AgeSeconds:       rustRound((now-v.CachedAt)*1000.0) / 1000.0,
			ExpiresInSeconds: rustRound((v.ExpiresAt-now)*1000.0) / 1000.0,
			Bytes:            len(v.Body),
			ContentType:      v.ContentType,
			ProxyOrigin:      v.ProxyOrigin,
			ProxyPrefix:      v.ProxyPrefix,
		}
	}

	q := r.queue
	q.mu.Lock()
	refreshQueue := RefreshQueueStatus{
		Limit:          r.cfg.BackgroundRefreshQueueLimit,
		QueuedDepth:    len(q.queued),
		QueuedChannels: append([]string(nil), q.queued...),
		RunningChannel: q.runningChannel,
		EnqueuedTotal:  q.enqueuedTotal,
		DroppedTotal:   q.droppedTotal,
		CompletedTotal: q.completedTotal,
		FailedTotal:    q.failedTotal,
		LastDropReason: q.lastDropReason,
	}
	if q.runningStartedAt > 0 {
		age := rustRound((now-q.runningStartedAt)*1000.0) / 1000.0
		refreshQueue.RunningAgeSeconds = &age
	}
	refreshQueue.LastEnqueuedAge = agePointer(now, q.lastEnqueuedAt)
	refreshQueue.LastFinishedAge = agePointer(now, q.lastFinishedAt)
	refreshQueue.LastDroppedAge = agePointer(now, q.lastDroppedAt)
	q.mu.Unlock()

	hq := r.httpQueue
	hq.mu.Lock()
	httpQueueStatus := HTTPQueueStatus{
		Workers:          r.cfg.HTTPWorkers,
		Limit:            r.cfg.HTTPQueueLimit,
		QueuedDepth:      0,
		ActiveWorkers:    hq.activeWorkers,
		EnqueuedTotal:    hq.enqueuedTotal,
		RejectedTotal:    hq.rejectedTotal,
		CompletedTotal:   hq.completedTotal,
		PanickedTotal:    hq.panickedTotal,
		LastEnqueuedAge:  agePointer(now, hq.lastEnqueuedAt),
		LastCompletedAge: agePointer(now, hq.lastCompletedAt),
		LastRejectedAge:  agePointer(now, hq.lastRejectedAt),
	}
	hq.mu.Unlock()

	channels := make([]string, len(deviceChannelOrder))
	copy(channels, deviceChannelOrder)

	return StatusInfo{
		Channels:                     channels,
		CacheTTLSeconds:              r.cfg.CacheTTL,
		StaleWhileRefreshTTLSeconds:  r.cfg.StaleWhileRefreshTTL,
		RefreshErrorCooldownSeconds:  r.cfg.RefreshErrorCooldown,
		PlaylistCacheTTLSeconds:      r.cfg.PlaylistCacheTTL,
		BackgroundRefreshQueueLimit:  r.cfg.BackgroundRefreshQueueLimit,
		HTTPWorkers:                  r.cfg.HTTPWorkers,
		HTTPQueueLimit:               r.cfg.HTTPQueueLimit,
		IdentityResetErrorThreshold:  r.cfg.IdentityResetErrorThreshold,
		IdentityResetCooldownSeconds: r.cfg.IdentityResetCooldown,
		RefreshIntervalSeconds:       r.cfg.RefreshInterval,
		ControlStepJitterMinMs:       r.cfg.ControlStepJitterMinMs,
		ControlStepJitterMaxMs:       r.cfg.ControlStepJitterMaxMs,
		HeartbeatIntervalSeconds:     r.cfg.HeartbeatInterval,
		HeartbeatTTLGuardSeconds:     r.cfg.HeartbeatTTLGuard,
		SessionTTLSeconds:            r.cfg.SessionTTL,
		MetaJSON:                     r.cfg.MetaJSON,
		DeviceJSON:                   r.cfg.DeviceJSON,
		DeviceState:                  r.deviceStateJSON(),
		RefreshingChannel:            refreshingChannel,
		BackgroundRefreshing:         backgroundRefreshing,
		BackgroundRefreshQueue:       refreshQueue,
		HTTPQueue:                    httpQueueStatus,
		Generation:                   generation,
		AppSession:                   appSession,
		LastBusinessEndAt:            lastBusinessEndAt,
		LastError:                    lastError,
		IdentityResetErrorCount:      identityResetErrorCount,
		IdentityResetCount:           identityResetCount,
		LastIdentityResetAt:          lastIdentityResetAt,
		LastIdentityResetReason:      lastIdentityResetReason,
		AdDisplay:                    adDisplay,
		DeviceProfilePoolSize:        len(model.DeviceProfilePool),
		Cache:                        cache,
		PlaylistCache:                playlistCache,
	}
}

// agePointer 计算"距现在多少秒"，0 值返回 nil（原 speed 里的条件表达式）。
func agePointer(now, at float64) *float64 {
	if at <= 0 {
		return nil
	}
	age := rustRound((now-at)*1000.0) / 1000.0
	return &age
}

// deviceStateJSON 读取 device json 生成状态视图（原 status_json 的 device_state 分支）。
func (r *Resolver) deviceStateJSON() map[string]interface{} {
	if info, err := os.Stat(r.cfg.DeviceJSON); err != nil || info.IsDir() {
		return map[string]interface{}{"exists": false}
	}
	store := model.NewDeviceStateStore(r.cfg.DeviceJSON)
	state, existed := store.Load()
	if !existed {
		return map[string]interface{}{"exists": false}
	}
	return map[string]interface{}{
		"exists":             true,
		"android_id":         state.Profile.AndroidID,
		"mac":                state.Profile.MAC,
		"brand":              state.Profile.Brand,
		"model":              state.Profile.Model,
		"screen_param":       state.ScreenParam,
		"x_uid":              state.XUID,
		"cloud_guid_present": state.CloudGUID != "",
		"registered_at":      state.RegisteredAt,
		"updated_at":         state.UpdatedAt,
	}
}

// appSessionJSON 生成会话状态视图（原 status_json 的 app_session 分支）。
// 调用方必须持有 stateLock。
func (r *Resolver) appSessionJSON(sess *AppSession, now float64) map[string]interface{} {
	var expiresIn interface{}
	if r.cfg.SessionTTL > 0 {
		expiresIn = r.cfg.SessionTTL - (now - sess.CreatedAt)
	} else {
		expiresIn = nil
	}
	return map[string]interface{}{
		"active":                     true,
		"generation":                 sess.Generation,
		"age_seconds":                rustRound((now-sess.CreatedAt)*1000.0) / 1000.0,
		"fresh":                      r.sessionFresh(sess),
		"expires_in_seconds":         expiresIn,
		"android_id":                 sess.Profile.AndroidID,
		"mac":                        sess.Profile.MAC,
		"brand":                      sess.Profile.Brand,
		"model":                      sess.Profile.Model,
		"screen_param":               sess.ScreenParam,
		"cast_model":                 sess.CastModel,
		"x_uid":                      sess.Identity.XUID,
		"cloud_guid_present":         sess.CloudGUID != "",
		"fingerprint_timestamp_ms":   sess.Identity.FingerprintTimestamp,
		"heartbeat_count":            sess.HeartbeatCount,
		"last_heartbeat_age_seconds": rustRound((now-sess.LastHeartbeatAt)*1000.0) / 1000.0,
		"last_heartbeat_result":      sess.LastHeartbeatResult,
		"last_heartbeat_error":       sess.LastHeartbeatError,
	}
}

// CacheStats 供 /health 与 /diag 使用：
// slots 有效会话数、links 已占用名额、guid、sessionValid、heartbeat 次数。
func (r *Resolver) CacheStats() (slots int, links int, guid string, sessionValid bool, heartbeat int64) {
	r.stateLock.Lock()
	defer r.stateLock.Unlock()
	slots = len(r.state.sessions)
	if sess := r.appSessionLocked(); sess != nil {
		links = sess.linkedChannelCount()
		guid = sess.CloudGUID
		sessionValid = r.sessionFresh(sess)
		heartbeat = sess.HeartbeatCount
	}
	return
}

// WriteMeta 把状态快照写入 MetaJSON（原 write_meta）。
//
// 原实现：先写 status_json，再补 cache_detail（完整 ChannelEntry）与两条说明字段，
// 以 2 空格缩进 + 键排序 + 末尾换行落盘（tmp + fsync + rename）。
func (r *Resolver) WriteMeta() {
	if strings.TrimSpace(r.cfg.MetaJSON) == "" {
		return
	}
	payload := statusToMap(r.Status())

	r.stateLock.Lock()
	cacheDetail := make(map[string]*model.ChannelEntry, len(r.state.cache))
	for k, v := range r.state.cache {
		cacheDetail[k] = cloneEntry(v)
	}
	r.stateLock.Unlock()
	payload["cache_detail"] = cacheDetail
	payload["session_detail_note"] = "Session secrets are intentionally not persisted for reuse; Python process keeps live session in memory only."
	payload["device_state_note"] = "Randomized TV device identity is persisted in device-state-rs.json after cloud registration succeeds; a new identity reset creates a new profile and MAC."

	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	// 原实现用 json.dumps(..., indent=2, sort_keys=True)：递归按键名排序后再缩进。
	// json.MarshalIndent 只会对 map 排序，结构体字段保持定义顺序，因此这里显式递归排序。
	sorted, err := httpx.SortJSONKeys(raw)
	if err != nil {
		return
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, sorted, "", "  "); err != nil {
		return
	}
	data := append(buf.Bytes(), '\n')

	path := r.cfg.MetaJSON
	parent := filepath.Dir(path)
	if parent != "" && parent != "." {
		_ = os.MkdirAll(parent, 0o755)
	}
	tmpPath := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := writeFileSync(tmpPath, data); err != nil {
		_ = os.Remove(tmpPath)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
	}
}

// statusToMap 把 StatusInfo 转成 map（用于追加 cache_detail 后统一序列化）。
func statusToMap(info StatusInfo) map[string]interface{} {
	data, err := json.Marshal(info)
	if err != nil {
		return map[string]interface{}{}
	}
	var out map[string]interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		return map[string]interface{}{}
	}
	return out
}

// DeviceStatus 返回 httpapi 需要的设备状态快照（供 internal/server 适配层直接调用）。
//
// 返回类型与 internal/httpapi.DeviceStatus 完全一致，因此 *Resolver 可直接满足
// internal/server.DeviceEngine 接口，无需额外类型转换。
func (r *Resolver) DeviceStatus() httpapi.DeviceStatus {
	_, links, guid, valid, hb := r.CacheStats()
	var lastErr string
	r.stateLock.Lock()
	if sess := r.appSessionLocked(); sess != nil {
		lastErr = sess.LastHeartbeatError
	}
	r.stateLock.Unlock()
	return httpapi.DeviceStatus{
		Ready:        r.ready.Load(),
		Valid:        valid,
		GUID:         guid,
		Links:        links,
		LinksLimit:   r.cfg.LinksPerDevice,
		Heartbeat:    hb,
		HeartbeatErr: lastErr,
	}
}

// Slots 返回当前会话数量（供 /health 的 active_device_slots）。
func (r *Resolver) Slots() int {
	r.stateLock.Lock()
	defer r.stateLock.Unlock()
	return len(r.state.sessions)
}

// ---------------------------------------------------------------------------
// 缓存持久化（原 load_cache / write_meta 的 cache_detail）
// ---------------------------------------------------------------------------

// loadCache 从 MetaJSON 恢复 cache_detail（原 load_cache）。
func loadCache(path string) (map[string]*model.ChannelEntry, error) {
	if strings.TrimSpace(path) == "" {
		return map[string]*model.ChannelEntry{}, nil
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return map[string]*model.ChannelEntry{}, err
	}
	var root map[string]interface{}
	if err := json.Unmarshal(text, &root); err != nil {
		return map[string]*model.ChannelEntry{}, err
	}
	detail, ok := root["cache_detail"].(map[string]interface{})
	if !ok {
		return map[string]*model.ChannelEntry{}, nil
	}
	out := make(map[string]*model.ChannelEntry, len(detail))
	for k, raw := range detail {
		item, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		entry := channelEntryFromMap(item)
		if entry != nil {
			out[k] = entry
		}
	}
	return out, nil
}

// channelEntryFromMap 从 JSON 对象恢复 ChannelEntry（原 ChannelEntry.from_dict）。
func channelEntryFromMap(m map[string]interface{}) *model.ChannelEntry {
	data, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	var entry model.ChannelEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil
	}
	if entry.PlaybackHeaders == nil {
		entry.PlaybackHeaders = map[string]string{}
	}
	return &entry
}

// writeFileSync 原子写文件的辅助（原 save_device_state 里的 flush + fsync）。
func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}
