package live

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// 生命周期相关常量。
const (
	// cacheStaleTTL 缓存超过该时长即视为过期，需要同步重拉（原 ensure_channel 的 30.0）。
	cacheStaleTTL = 30 * time.Second
	// ensureRefreshTimeout ensure_channel 等待同步刷新完成的上限（原 coalesce_refresh timeout=5.0）。
	ensureRefreshTimeout = 5 * time.Second
	// coalesceDefaultTimeout coalesce_refresh 默认等待上限（原实现在 timeout=None 时等 6s）。
	coalesceDefaultTimeout = 6 * time.Second
	// refreshFailBackoff 连续失败 3 次后的慢速间隔（原 30 秒）。
	refreshFailBackoff = 30 * time.Second
	// holdbackMinSegs 分片数达到该值才启用 holdback（原实现 >=4）。
	holdbackMinSegs = 4
	// playlistWindowSegs 每次构建播放列表取最后 N 片（原实现 [-15:]）。
	playlistWindowSegs = 15
	// initSegCount 首次初始化只取最后 N 片（原实现 [-25:]）。
	initSegCount = 25
	// maxErrLen 错误文本截断长度（原实现 [:120]）。
	maxErrLen = 120
)

// spawn 启动一个受 Stop 管理的后台 goroutine；返回 false 表示 Manager 已停止。
func (m *Manager) spawn(fn func()) bool {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return false
	}
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		fn()
	}()
	return true
}

// coalesceRefresh 同一频道的并发刷新只执行一次，其余调用方等待完成或超时。
// 对应原 coalesce_refresh(slug, refresh_fn, timeout=None)。
// 返回 true 表示本次调用是"执行者"，false 表示只是等待者。
func (m *Manager) coalesceRefresh(slug string, fn func(), timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = coalesceDefaultTimeout
	}
	m.mu.Lock()
	if ev, ok := m.inflight[slug]; ok {
		m.mu.Unlock()
		ev.wait(timeout)
		return false
	}
	ev := &inflightEvent{done: make(chan struct{})}
	m.inflight[slug] = ev
	m.mu.Unlock()

	started := m.spawn(func() {
		defer func() {
			m.mu.Lock()
			// 只有当前事件仍是自己时才删除，避免误删后来的刷新。
			if cur, ok := m.inflight[slug]; ok && cur == ev {
				delete(m.inflight, slug)
			}
			m.mu.Unlock()
			close(ev.done)
		}()
		fn()
	})
	if !started {
		m.mu.Lock()
		if cur, ok := m.inflight[slug]; ok && cur == ev {
			delete(m.inflight, slug)
		}
		m.mu.Unlock()
		close(ev.done)
		return false
	}
	ev.wait(timeout)
	return true
}

// refreshOnce 执行一次刷新：按当前模式走 JCE 或 bkliveinfo，失败时记录错误。
// 对应原 refresh_once(ch)。
func (m *Manager) refreshOnce(ch *Channel) bool {
	ch.mu.Lock()
	mode := ch.Mode
	ch.mu.Unlock()

	var err error
	switch mode {
	case ModeBK:
		err = m.bkRefresh(ch)
	default:
		if jerr := m.jceRefresh(ch); jerr != nil {
			if errors.Is(jerr, ErrDeadHost) {
				// 原 DeadHostError 分支：JCE 域名受限，永久切到备用通道。
				ch.setMode(ModeBK)
				logfInterval(10.0, ch.Slug, "JCE 域名受限，自动切换 bkliveinfo 备用通道")
				err = m.bkRefresh(ch)
			} else {
				err = jerr
			}
		}
	}
	if err != nil {
		msg := truncErr(err)
		ch.mu.Lock()
		ch.LastError = msg
		ch.mu.Unlock()
		logfInterval(15.0, ch.Slug, "%s 刷新异常: %s", defnLabel(ch.Defn), msg)
		return false
	}
	ch.mu.Lock()
	ch.LastOK = m.now()
	ch.mu.Unlock()
	return true
}

// truncErr 生成与原项目一致的错误摘要（原实现为 "异常类名: 文本" 截断 120 字符）。
// 偏差说明：Go 无异常类名，这里改用错误文本本身，保证内容非空且可读。
func truncErr(err error) string {
	if err == nil {
		return "unknown error"
	}
	msg := strings.TrimSpace(err.Error())
	if msg == "" {
		msg = "unknown error"
	}
	if r := []rune(msg); len(r) > maxErrLen {
		msg = string(r[:maxErrLen])
	}
	return msg
}

// refreshLoop 后台周期拉取，直到空闲超时或被 Stop 打断。
// 对应原 refresh_loop(ch)。
func (m *Manager) refreshLoop(ch *Channel) {
	ch.mu.Lock()
	mode := ch.Mode
	ch.mu.Unlock()
	logfInterval(10.0, ch.Slug, "%s 后台拉取启动 [%s]", defnLabel(ch.Defn), mode)

	fails := 0
	for {
		ch.mu.Lock()
		lastAccess := ch.LastAccess
		ch.mu.Unlock()
		if m.now().Sub(lastAccess) >= m.idle {
			break
		}
		ok := m.refreshOnce(ch)
		if ok {
			m.BuildPlaylist(ch, true)
			fails = 0
		} else {
			fails++
		}
		interval := m.refresh
		if fails >= 3 {
			interval = refreshFailBackoff
		}
		if !m.sleepOrStop(interval) {
			break
		}
	}

	ch.mu.Lock()
	ch.loopOn = false
	ch.mu.Unlock()
	logfInterval(10.0, ch.Slug, "空闲超时，已停止后台拉取")
}

// startLoop 幂等启动频道的后台拉取循环（调用方不持锁）。
// 对应原 ensure_channel 中 `ch.thread = threading.Thread(target=refresh_loop, ...)` 的分支。
func (m *Manager) startLoop(ch *Channel) {
	ch.mu.Lock()
	if ch.loopOn {
		ch.mu.Unlock()
		return
	}
	ch.loopOn = true
	ch.mu.Unlock()
	if !m.spawn(func() { m.refreshLoop(ch) }) {
		// Manager 已 Stop：回滚标记，避免频道被误认为仍在拉取。
		ch.mu.Lock()
		ch.loopOn = false
		ch.mu.Unlock()
	}
}

// EnsureChannel 确保频道有可用的后台拉取线程与缓存，必要时同步刷新一次。
// 对应原 ensure_channel(ch)。
//
// 修复原项目 bug（task-2 第 1 条）：原实现把 last_access 写在锁外、
// `_starting` 标志在读锁外判断并在 finally 中锁外复位，存在数据竞争；
// Go 版所有状态读写都在 ch.mu 内完成，刷新/启动线程期间只持锁做标记。
func (m *Manager) EnsureChannel(ch *Channel) {
	if ch == nil {
		return
	}
	now := m.now()

	ch.mu.Lock()
	ch.LastAccess = now
	if ch.starting {
		ch.mu.Unlock()
		return
	}
	threadDead := !ch.loopOn
	cacheStale := ch.LastOK.IsZero() || now.Sub(ch.LastOK) > cacheStaleTTL ||
		(len(ch.segments) == 0 && ch.bkPl == "")
	if cacheStale {
		ch.segments = make(map[string]*segment)
		ch.order = nil
		ch.lastPDT = ""
		ch.bkPl = ""
		ch.Segments = 0
	}
	if !cacheStale && !threadDead {
		ch.mu.Unlock()
		return
	}
	ch.starting = true
	ch.mu.Unlock()

	if cacheStale {
		m.coalesceRefresh(ch.Slug, func() { m.refreshOnce(ch) }, ensureRefreshTimeout)
	}

	if threadDead {
		m.startLoop(ch)
	}

	ch.mu.Lock()
	ch.starting = false
	ch.mu.Unlock()
}

// TriggerBackgroundRefresh 在后台触发一次刷新（若同频道已有刷新在进行则跳过）。
// 对应原 trigger_background_refresh(ch)。
func (m *Manager) TriggerBackgroundRefresh(ch *Channel) {
	if ch == nil {
		return
	}
	m.mu.Lock()
	_, busy := m.inflight[ch.Slug]
	m.mu.Unlock()
	if busy {
		return
	}
	m.spawn(func() {
		m.coalesceRefresh(ch.Slug, func() {
			if m.refreshOnce(ch) {
				m.BuildPlaylist(ch, true)
			}
		}, coalesceDefaultTimeout)
	})
}

// BuildPlaylist 构建可下发的 m3u8 文本。
// 对应原 build_playlist(ch, holdback=True)：
//   - bk 模式直接返回备用播放列表；
//   - jce 模式取最后 15 片，holdback 且分片数 >= 4 时丢弃最后一片（等分片写完整）；
//   - TARGETDURATION = max(6, max(round(dur)))，MEDIA-SEQUENCE 取首片 seq。
//
// 修复原项目 bug（task-2 第 4 条）：原实现对空分片列表执行
// `max(6, max(int(dur+0.5) for ...))` 会抛 ValueError；Go 版空列表返回 ""。
func (m *Manager) BuildPlaylist(ch *Channel, holdback bool) string {
	if ch == nil {
		return ""
	}

	ch.mu.Lock()
	if ch.Mode == ModeBK {
		res := ch.bkPl
		if res != "" {
			ch.builtPl = res
			ch.builtTime = m.now()
		}
		ch.mu.Unlock()
		return res
	}
	keys := ch.order
	if len(keys) > playlistWindowSegs {
		keys = keys[len(keys)-playlistWindowSegs:]
	}
	segs := make([]segment, 0, len(keys))
	for _, k := range keys {
		if s, ok := ch.segments[k]; ok && s != nil {
			segs = append(segs, *s)
		}
	}
	ch.mu.Unlock()

	if len(segs) == 0 {
		// 修复空列表 panic：原项目此处会抛异常。
		return ""
	}
	display := segs
	if holdback && len(segs) >= holdbackMinSegs {
		display = segs[:len(segs)-1]
	}
	if len(display) == 0 {
		return ""
	}
	target := 6
	for _, s := range display {
		if v := int(s.dur + 0.5); v > target {
			target = v
		}
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	fmt.Fprintf(&b, "#EXT-X-VERSION:3\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", target)
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", display[0].seq)
	for _, s := range display {
		if s.pdt != "" {
			b.WriteString("#EXT-X-PROGRAM-DATE-TIME:")
			b.WriteString(s.pdt)
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n", s.dur)
		b.WriteString(s.url)
		b.WriteByte('\n')
	}
	res := b.String()

	ch.mu.Lock()
	ch.builtPl = res
	ch.builtTime = m.now()
	ch.mu.Unlock()
	return res
}

// CachedPlaylist 返回上次构建的播放列表及其构建时间（供 gateway 做 SWR 判断）。
func (m *Manager) CachedPlaylist(ch *Channel) (string, time.Time) {
	if ch == nil {
		return "", time.Time{}
	}
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return ch.builtPl, ch.builtTime
}

// InvalidateCachedPlaylist 让缓存的播放列表失效（供 gateway 在切换线路后调用）。
func (m *Manager) InvalidateCachedPlaylist(ch *Channel) {
	if ch == nil {
		return
	}
	ch.mu.Lock()
	ch.builtPl = ""
	ch.builtTime = time.Time{}
	ch.mu.Unlock()
}
