package live

import (
	"context"
	"strings"
	"time"
)

// 本文件是给上层 gateway / server 适配层用的门面方法：
// 把原 Handler 中 "/<channel>.m3u8" 分支的编排逻辑（SWR 缓存判断 + ensure_channel +
// build_playlist、时移分支）收敛到 Manager 内，避免适配层直接操作 Channel 内部状态。

// SWR 相关常量（原 Handler 中的硬编码值）。
const (
	// swrFreshTTL 上次构建的播放列表在该时长内可直接复用（原 cached_age < 15.0）。
	swrFreshTTL = 15 * time.Second
	// swrRefreshAfter 缓存超过该时长即触发后台刷新（原 cached_age > 6.0）。
	swrRefreshAfter = 6 * time.Second
)

// Playlist 按 slug 取 1080p / 备用线路的可下发 m3u8。
//
// 对应原 Handler 的 "/<channel>.m3u8" 分支：
//
//	ch.last_access = now_ts
//	if cached_pl and age < 15.0 and '#EXTM3U' in cached_pl:   # SWR 命中
//	    if age > 6.0: trigger_background_refresh(ch)
//	    return cached_pl
//	ensure_channel(ch)
//	pl = build_playlist(ch, holdback=True)
//
// 语义约定（与 gateway 的兜底链配合）：
//   - 拿到数据时返回非空 m3u8 与 nil error；
//   - 暂无数据（例如上游还没拉取到分片）时返回 ("", nil)，由 gateway 继续尝试下一层兜底，
//     因此这里**不**把"没数据"当成 error；
//   - 只有频道不存在才返回 ErrUnknownChannel。
func (m *Manager) Playlist(ctx context.Context, slug string) (string, error) {
	if err := ctxErr(ctx); err != nil {
		return "", err
	}
	ch := m.Channel(slug)
	if ch == nil {
		return "", ErrUnknownChannel
	}

	now := m.now()

	ch.mu.Lock()
	ch.LastAccess = now
	cached := ch.builtPl
	cachedAt := ch.builtTime
	ch.mu.Unlock()

	// --- 原 Handler 的 SWR 快速路径：15s 内直接复用，超过 6s 顺手触发后台刷新 ---
	if cached != "" && strings.Contains(cached, "#EXTM3U") && !cachedAt.IsZero() {
		age := now.Sub(cachedAt)
		if age < 0 {
			// 时钟回拨：原实现算出负年龄同样满足 cached_age < 15.0，视为新鲜。
			age = 0
		}
		if age < swrFreshTTL {
			// 复用缓存时也要保证后台循环活着，否则缓存过期后无人刷新。
			m.startLoop(ch)
			if age > swrRefreshAfter {
				m.TriggerBackgroundRefresh(ch)
			}
			return cached, nil
		}
	}

	// --- 冷路径：确保缓存/后台线程就绪，再构建一次 ---
	m.startLoop(ch)
	m.EnsureChannel(ch)
	if err := ctxErr(ctx); err != nil {
		return "", err
	}

	pl := m.BuildPlaylist(ch, true)
	if pl != "" && strings.Contains(pl, "#EXTM3U") {
		return pl, nil
	}
	// 暂无数据：交给 gateway 决定是否继续下层兜底，不返回 error。
	return "", nil
}

// Touch 标记频道正在被观看：刷新 last_access 并在需要时启动后台 refresh_loop。
// 对应原 Handler 中的 `ch.last_access = now_ts` 加上 ensure_channel 启动线程的副作用；
// 原项目的 refresh_loop 靠 last_access 判断空闲超时，所以观看期间必须持续刷新该字段。
func (m *Manager) Touch(slug string) {
	ch := m.Channel(slug)
	if ch == nil {
		return
	}
	now := m.now()
	ch.mu.Lock()
	ch.LastAccess = now
	ch.mu.Unlock()
	// 后台循环可能因空闲超时退出，观看时重新拉起。
	m.startLoop(ch)
}

// Catchup 按 slug 做时移回看重取，返回可直接下发的 m3u8。
// 对应原 Handler 中时移分支：
//
//	if catchup_times and canonical in TIMESHIFT_SUPPORTED:
//	    m3u8_url = jce_timeshift_url(ch.pid, ch.sid, start_ts, end_ts, ch.defn)
//	    pl = fetch_abs_playlist(m3u8_url)
//
// 返回的播放列表分片地址已经是绝对地址（原实现直接下发，不经过本服务的 /proxy.ts）。
//
// 返回值为 ("", nil) 的情形：
//   - 频道不在 model.TimeshiftSupported（原项目 bug 修复第 5 条：交由 gateway 回退直播，
//     不当作错误上报）；
//   - 上游返回空列表。
//
// 上游/解析失败才返回 error，由 gateway 决定"回退直播"。
func (m *Manager) Catchup(ctx context.Context, slug string, startTS, endTS int64) (string, error) {
	if err := ctxErr(ctx); err != nil {
		return "", err
	}
	ch := m.Channel(slug)
	if ch == nil {
		return "", ErrUnknownChannel
	}
	pl, err := m.CatchupPlaylist(ch, startTS, endTS, "")
	if err != nil {
		return "", err
	}
	if err := ctxErr(ctx); err != nil {
		return "", err
	}
	if pl != "" && !strings.Contains(pl, "#EXTM3U") {
		return "", nil
	}
	return pl, nil
}

// ctxErr 归一化 context 错误（保持 context.Canceled / DeadlineExceeded 可用 errors.Is 判断）。
func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
