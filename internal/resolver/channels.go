package resolver

import (
	"fmt"
	"strings"
	"time"

	"github.com/jacktea/ysp-live-go/internal/logx"
	"github.com/jacktea/ysp-live-go/internal/model"
)

// 本文件对应原 Resolver 的频道级解析与缓存部分：
// ensure_channel / refresh_channel_controlled / refresh_channel_background /
// refresh_worker_loop（队列部分见 resolver.go）/ enqueue_background_refresh /
// is_channel_in_cooldown / get_channel_failure_info / clear_channel_cooldown /
// evict_idle_links / get_slot_for_channel / zap_debounced / _zap_resolve_worker.

// cloneEntry 复制一份 ChannelEntry（含 playback_headers map）。
//
// 原实现直接返回缓存里的对象引用；Go 版返回副本，避免调用方在锁外读取的同时
// 后台刷新线程修改 LastRefreshError / LastRefreshFailed 造成数据竞争。
func cloneEntry(entry *model.ChannelEntry) *model.ChannelEntry {
	if entry == nil {
		return nil
	}
	clone := *entry
	if entry.PlaybackHeaders != nil {
		clone.PlaybackHeaders = make(map[string]string, len(entry.PlaybackHeaders))
		for k, v := range entry.PlaybackHeaders {
			clone.PlaybackHeaders[k] = v
		}
	}
	return &clone
}

// ensureChannel 确保频道有可用的 ChannelEntry（原 ensure_channel）。
//
// 行为顺序与原实现一致：
//  1. 非强制刷新时，锁内快速返回新鲜缓存（含"签名播放头缺失"判定）；
//  2. SWR：stale_while_refresh_ttl 内返回旧值并排入后台刷新；
//  3. 冷却命中直接报错（文案保留 "channel xxx in cooldown (Ns/Ms): err"）；
//  4. 控制面锁内强制刷新；若失败文本含 "HTTP 400" 且换设备成功，则重试一次；
//  5. 失败记录失败次数与错误，SWR 窗口内仍可返回旧值。
func (r *Resolver) ensureChannel(channel string, force bool) (*model.ChannelEntry, error) {
	now := nowF64()
	if !force {
		r.stateLock.Lock()
		entry := r.state.cache[channel]
		if entry != nil {
			if entry.Fresh(now) && !entryMissingSignedPlaybackHeaders(entry) {
				clone := cloneEntry(entry)
				r.stateLock.Unlock()
				return clone, nil
			}
			if entry.StaleUsable(now, r.cfg.StaleWhileRefreshTTL) && !entryMissingSignedPlaybackHeaders(entry) {
				retryAt := entry.LastRefreshFailed + r.cfg.RefreshErrorCooldown
				started, hasStarted := r.state.backgroundRefreshing[channel]
				shouldRefresh := now >= retryAt && (!hasStarted || now-started > 300.0)
				clone := cloneEntry(entry)
				if shouldRefresh {
					r.state.backgroundRefreshing[channel] = now
					r.stateLock.Unlock()
					status, reason := r.enqueueBackgroundRefresh(channel)
					if status == "rejected" {
						r.stateLock.Lock()
						delete(r.state.backgroundRefreshing, channel)
						if e2 := r.state.cache[channel]; e2 != nil {
							e2.LastRefreshError = reason
							e2.LastRefreshFailed = now
						}
						r.stateLock.Unlock()
					}
					return clone, nil
				}
				r.stateLock.Unlock()
				return clone, nil
			}
		}
		if fail := r.state.channelFailures[channel]; fail != nil {
			cooldown := cooldownForFailCount(fail.failCount)
			elapsed := now - fail.failedAt
			if elapsed < cooldown {
				r.stateLock.Unlock()
				return nil, errorf("channel %s in cooldown (%ds/%ds): %s", channel, int(elapsed), int(cooldown), fail.err)
			}
		}
		r.stateLock.Unlock()
	}

	r.controlLock.Lock()
	defer r.controlLock.Unlock()

	now = nowF64()
	if !force {
		r.stateLock.Lock()
		if entry := r.state.cache[channel]; entry != nil && entry.Fresh(now) && !entryMissingSignedPlaybackHeaders(entry) {
			clone := cloneEntry(entry)
			r.stateLock.Unlock()
			return clone, nil
		}
		r.stateLock.Unlock()
	}

	entry, err := r.refreshChannelControlledLocked(channel)
	if err != nil {
		errText := err.Error()
		if strings.Contains(errText, "HTTP 400") && r.rotateDeviceOn400(channel+":"+truncateString(errText, 60)) {
			if retry, retryErr := r.refreshChannelControlledLocked(channel); retryErr == nil {
				r.clearRecoverableErrorCount()
				r.WriteMeta()
				return retry, nil
			}
		}
		r.stateLock.Lock()
		r.state.lastError = errText
		r.state.lastErrorAt = nowF64()
		lastError := r.state.lastError
		lastErrorAt := r.state.lastErrorAt
		prev := r.state.channelFailures[channel]
		failCount := 1
		if prev != nil {
			failCount = prev.failCount + 1
		}
		r.state.channelFailures[channel] = &channelFailure{failedAt: lastErrorAt, err: lastError, failCount: failCount}
		var clone *model.ChannelEntry
		if e := r.state.cache[channel]; e != nil && e.FinalURL != "" && e.StaleUsable(lastErrorAt, r.cfg.StaleWhileRefreshTTL) {
			e.LastRefreshError = lastError
			e.LastRefreshFailed = lastErrorAt
			clone = cloneEntry(e)
		}
		r.stateLock.Unlock()

		didReset := r.recordRecoverableError(errText)
		if !didReset {
			r.WriteMeta()
		}
		if didReset || clone == nil {
			return nil, err
		}
		return clone, nil
	}
	r.clearRecoverableErrorCount()
	r.WriteMeta()
	return entry, nil
}

// refreshChannelControlled 受控刷新（原 refresh_channel_controlled，外部入口）。
//
// 原实现要求调用方已持有 control_lock；Go 版公开入口自行加锁，内部逻辑在 *Locked 版本里。
func (r *Resolver) refreshChannelControlled(channel string) (*model.ChannelEntry, error) {
	r.controlLock.Lock()
	defer r.controlLock.Unlock()
	return r.refreshChannelControlledLocked(channel)
}

// refreshChannelControlledLocked 假定已持有 controlLock（原 refresh_channel_controlled）。
func (r *Resolver) refreshChannelControlledLocked(channel string) (*model.ChannelEntry, error) {
	r.stateLock.Lock()
	wait := r.cfg.RefreshInterval - (nowF64() - r.state.lastBusinessEndAt)
	if wait > 0 {
		r.stateLock.Unlock()
		time.Sleep(time.Duration(wait * float64(time.Second)))
		r.stateLock.Lock()
	}
	r.state.refreshingChannel = channel
	r.state.generation++
	generation := r.state.generation
	r.stateLock.Unlock()

	logx.Log(fmt.Sprintf("[%s] 正在向央视频协议引擎请求直播源…", channel), 10)

	entry, err := r.resolveChannelControlledBody(channel, generation)
	if err != nil {
		r.stateLock.Lock()
		r.state.lastBusinessEndAt = nowF64()
		r.state.refreshingChannel = ""
		r.stateLock.Unlock()
		return nil, asResolverErr(err)
	}

	r.stateLock.Lock()
	r.state.lastBusinessEndAt = nowF64()
	r.state.refreshingChannel = ""
	r.state.cache[channel] = entry
	delete(r.state.playlistCache, channel)
	delete(r.state.channelFailures, channel)
	r.state.identityResetErrorCount = 0
	r.state.lastError = ""
	rateLabel := rateLabelFor(channel, entry)
	r.stateLock.Unlock()

	logx.Log(fmt.Sprintf("[%s] 直播源获取成功 (清晰度: %s)", channel, rateLabel), 10)
	return entry, nil
}

// resolveChannelControlledBody 对应原 refresh_channel_controlled 的 try 块：
// 名额检查 → live_id 查询 → resolve_channel_once。
func (r *Resolver) resolveChannelControlledBody(channel string, generation int) (*model.ChannelEntry, error) {
	slot := r.getSlotForChannel(channel)
	limit := r.cfg.LinksPerDevice
	sess, err := r.ensureSlotSessionLockedE(slot)
	if err != nil {
		return nil, err
	}

	r.stateLock.Lock()
	linked := sess.linkedChannelCount()
	alreadyLinked := sess.linkedChannels[channel]
	if limit > 0 && !alreadyLinked && linked >= limit {
		r.stateLock.Unlock()
		r.evictIdleLinks(sess)
		r.stateLock.Lock()
		linked = sess.linkedChannelCount()
		if linked >= limit {
			r.stateLock.Unlock()
			return nil, errorf("device link quota reached (%d), fallback to 1080p", limit)
		}
	}
	r.stateLock.Unlock()

	liveID, ok := model.LiveIDs[channel]
	if !ok {
		return nil, errorf("unknown channel %s", channel)
	}
	entry, err := r.resolveChannelOnce(sess, channel, liveID, generation)
	if err != nil {
		return nil, err
	}
	r.stateLock.Lock()
	sess.linkedChannels[channel] = true
	r.stateLock.Unlock()
	return entry, nil
}

// rateLabelFor 生成日志里的清晰度文案（原 refresh_channel_controlled 里的 rate_label 分支）。
func rateLabelFor(channel string, entry *model.ChannelEntry) string {
	var rateLabel string
	switch {
	case channel == "cctv4k" || channel == "cctv164k" || channel == "cctv8k":
		switch entry.Rate {
		case "36p":
			rateLabel = "4K超高清 (36M超高码率)"
		case "10p":
			rateLabel = "4K (10M码率)"
		default:
			rateLabel = "4K超高清"
		}
	case entry.Rate == "10p":
		rateLabel = "高码率 (10M码率)"
	case entry.Rate == "36p":
		rateLabel = "超高码率 (36M码率)"
	default:
		rateLabel = entry.RateName
		if rateLabel == "" {
			rateLabel = entry.Rate
		}
		if rateLabel == "" {
			rateLabel = "高码率"
		}
	}
	if (channel == "cctv4k" || channel == "cctv164k" || channel == "cctv8k") && strings.Contains(rateLabel, "8K") {
		rateLabel = "4K超高清 (36M超高码率)"
	}
	return rateLabel
}

// refreshChannelBackground 后台刷新（原 refresh_channel_background）。
//
// 原实现是 "成功/失败两条路径各自清理后台标记"；Go 版用 defer 统一清理，
// 避免任何提前返回（含 panic 恢复）漏放控制锁。
func (r *Resolver) refreshChannelBackground(channel string) (ok bool) {
	r.controlLock.Lock()
	defer r.controlLock.Unlock()
	defer func() {
		if rec := recover(); rec != nil {
			ok = false
		}
		r.stateLock.Lock()
		delete(r.state.backgroundRefreshing, channel)
		r.stateLock.Unlock()
	}()

	now := nowF64()
	r.stateLock.Lock()
	entry := r.state.cache[channel]
	if entry != nil && entry.Fresh(now) && entry.ExpiresAt-now > 180.0 && !entryMissingSignedPlaybackHeaders(entry) {
		r.stateLock.Unlock()
		return true
	}
	r.stateLock.Unlock()

	if _, err := r.refreshChannelControlledLocked(channel); err == nil {
		r.WriteMeta()
		return true
	} else {
		errText := err.Error()
		r.stateLock.Lock()
		r.state.lastError = errText
		r.state.lastErrorAt = nowF64()
		lastError := r.state.lastError
		lastErrorAt := r.state.lastErrorAt
		prev := r.state.channelFailures[channel]
		failCount := 1
		if prev != nil {
			failCount = prev.failCount + 1
		}
		r.state.channelFailures[channel] = &channelFailure{failedAt: lastErrorAt, err: lastError, failCount: failCount}
		if e := r.state.cache[channel]; e != nil {
			e.LastRefreshError = lastError
			e.LastRefreshFailed = lastErrorAt
		}
		if isSessionInvalidatingError(errText) {
			// 原实现：self.state.app_session = None（仅清 slot 0）。
			delete(r.state.sessions, 0)
		}
		r.stateLock.Unlock()

		if !r.recordRecoverableError(errText) {
			r.WriteMeta()
		}
		return false
	}
}

// enqueueBackgroundRefresh 把频道排入后台刷新队列（原 enqueue_background_refresh）。
//
// 返回 (status, reason)，status 取值："already_active" / "enqueued" / "rejected"。
func (r *Resolver) enqueueBackgroundRefresh(channel string) (string, string) {
	now := nowF64()
	q := r.queue
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.runningChannel == channel {
		return "already_active", ""
	}
	if _, exists := q.queuedChannels[channel]; exists {
		return "already_active", ""
	}
	limit := r.cfg.BackgroundRefreshQueueLimit
	if limit == 0 || len(q.queued) >= limit {
		q.droppedTotal++
		q.lastDroppedAt = now
		if limit == 0 {
			q.lastDropReason = fmt.Sprintf("background refresh queue disabled; channel=%s", channel)
		} else {
			q.lastDropReason = fmt.Sprintf("background refresh queue full (%d/%d); channel=%s", len(q.queued), limit, channel)
		}
		return "rejected", q.lastDropReason
	}
	q.queued = append(q.queued, channel)
	q.queuedChannels[channel] = now
	q.enqueuedTotal++
	q.lastEnqueuedAt = now
	select {
	case q.signal <- struct{}{}:
	default:
	}
	return "enqueued", ""
}

// isChannelInCooldown 频道是否在冷却中（原 is_channel_in_cooldown）。
func (r *Resolver) isChannelInCooldown(channel string) bool {
	r.stateLock.Lock()
	defer r.stateLock.Unlock()
	fail := r.state.channelFailures[channel]
	if fail == nil {
		return false
	}
	return (nowF64() - fail.failedAt) < cooldownForFailCount(fail.failCount)
}

// InCooldown 对外：频道是否在冷却中（原 is_channel_in_cooldown）。
func (r *Resolver) InCooldown(channel string) bool {
	return r.isChannelInCooldown(model.Canonical(channel))
}

// getChannelFailureInfo 返回 (failedAt, err, failCount, ok)（原 get_channel_failure_info）。
func (r *Resolver) getChannelFailureInfo(channel string) (float64, string, int, bool) {
	r.stateLock.Lock()
	defer r.stateLock.Unlock()
	fail := r.state.channelFailures[channel]
	if fail == nil {
		return 0, "", 0, false
	}
	return fail.failedAt, fail.err, fail.failCount, true
}

// clearChannelCooldown 清除频道失败记录（原 clear_channel_cooldown）。
func (r *Resolver) clearChannelCooldown(channel string) {
	r.stateLock.Lock()
	delete(r.state.channelFailures, channel)
	r.stateLock.Unlock()
}

// MarkChannelFailure 上层（gateway）在设备层失败后调用，累计冷却计数。
//
// 对应原 Handler 里失败分支的状态更新：记录 last_error 并累加 channel_failures。
func (r *Resolver) MarkChannelFailure(channel string, err error) {
	channel = model.Canonical(channel)
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if msg == "" {
		msg = "device layer failure"
	}
	now := nowF64()
	r.stateLock.Lock()
	// 对齐原 Handler 失败分支：丢弃该频道的频道缓存与播放列表缓存，
	// 避免下一次请求继续下发已失效地址（SWR 由 ensureChannel 内部负责）。
	delete(r.state.cache, channel)
	delete(r.state.playlistCache, channel)
	r.state.lastError = msg
	r.state.lastErrorAt = now
	prev := r.state.channelFailures[channel]
	failCount := 1
	if prev != nil {
		failCount = prev.failCount + 1
	}
	r.state.channelFailures[channel] = &channelFailure{failedAt: now, err: msg, failCount: failCount}
	r.stateLock.Unlock()
}

// evictIdleLinks 释放空闲超时的高码率名额（原 evict_idle_links）。
func (r *Resolver) evictIdleLinks(sess *AppSession) {
	now := nowF64()
	r.stateLock.Lock()
	defer r.stateLock.Unlock()
	if sess == nil {
		return
	}
	for channel := range sess.linkedChannels {
		if now-r.state.channelLastAccess[channel] > r.cfg.LinkIdleTimeout {
			delete(sess.linkedChannels, channel)
		}
	}
}

// getSlotForChannel 频道使用的设备槽位（原 get_slot_for_channel，恒为 0）。
func (r *Resolver) getSlotForChannel(string) int { return 0 }

// ---------------------------------------------------------------------------
// 换台防抖（原 zap_debounced / _zap_resolve_worker）
// ---------------------------------------------------------------------------

// zapDebounced 换台防抖（原 zap_debounced）。
//
// 返回 true 表示本次应先返回 1080p（并已在 debounce 后启动高码率预取）。
func (r *Resolver) zapDebounced(channel string) bool {
	if model.True4KChannels[channel] {
		return false
	}
	debounce := r.cfg.ZapDebounce
	if debounce <= 0 {
		return false
	}
	now := nowF64()

	r.stateLock.Lock()
	entry := r.state.cache[channel]
	if entry != nil && entry.Fresh(now) && !entryMissingSignedPlaybackHeaders(entry) {
		r.stateLock.Unlock()
		return false
	}
	if r.state.zapChannel != channel {
		r.state.zapChannel = channel
		r.state.zapTime = now
		r.state.zapPending = ""
	}
	if r.state.zapPending == channel {
		r.stateLock.Unlock()
		return true
	}
	if now-r.state.zapTime < debounce {
		r.state.zapPending = channel
		r.state.zapGeneration++
		gen := r.state.zapGeneration
		delay := maxF(0.1, debounce-(now-r.state.zapTime))
		r.stateLock.Unlock()
		r.goBackground(func() { r.zapResolveWorker(channel, gen, delay) })
		return true
	}
	r.stateLock.Unlock()
	return false
}

// zapResolveWorker 防抖到期后强制解析频道（原 _zap_resolve_worker）。
func (r *Resolver) zapResolveWorker(channel string, gen int, delay float64) {
	// 用 workCtx 使 Stop() 能立即打断等待中的防抖预取，避免退出时被 ZapDebounce 拖住。
	if err := sleepCtx(r.workCtx, time.Duration(delay*float64(time.Second))); err != nil {
		return
	}

	r.stateLock.Lock()
	if gen != r.state.zapGeneration || r.state.zapChannel != channel {
		r.stateLock.Unlock()
		return
	}
	r.stateLock.Unlock()

	_, err := r.ensureChannel(channel, true)
	if err != nil {
		logx.Log(fmt.Sprintf("[%s] 高码率后台预取失败: %v, 继续 1080p", channel, err), 10)
	} else {
		logx.Log(fmt.Sprintf("[%s] 高码率后台预取完成, 继续停留将自动升级", channel), 10)
	}

	r.stateLock.Lock()
	if r.state.zapPending == channel {
		r.state.zapPending = ""
	}
	r.stateLock.Unlock()
}

// deviceChannelOrder 设备协议支持的 26 路频道（顺序与原项目 channels() 一致）。
//
// 原实现 status_json 的 channels 字段直接来自 channels() 列表顺序，
// 因此这里用固定切片而不是遍历 model.LiveIDs（map 无序）。
var deviceChannelOrder = []string{
	"cctv1", "cctv2", "cctv3", "cctv4", "cctv5", "cctv5p", "cctv7", "cctv8",
	"cctv9", "cctv10", "cctv11", "cctv12", "cctv13", "cctv14", "cctv15", "cctv16",
	"cctv17", "cctv4k", "cctv8k", "cctv164k", "cgtn", "cgtnfr", "cgtnru", "cgtnar",
	"cgtnes", "cgtndoc",
}
