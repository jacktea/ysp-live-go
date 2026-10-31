// Package live 迁移原项目 ysp-live.py 中的 1080p JCE 主线路、bkliveinfo 备用线路
// 与 7 天时移回看（原 Channel 类、jce_fetch / jce_refresh / bk_playurls / bk_refresh /
// refresh_once / refresh_loop / ensure_channel / build_playlist / coalesce_refresh /
// trigger_background_refresh / parse_catchup_* 等函数）。
//
// 并发约定：所有可变状态都在 Channel.mu 保护下读写；对外导出的 Channel 字段只是
// 便于 /diag 直接读取的快照，诊断请优先使用 State() / Snapshot()（内部加锁）。
package live

import (
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jacktea/ysp-live-go/internal/httpx"
	"github.com/jacktea/ysp-live-go/internal/model"
	"github.com/jacktea/ysp-live-go/internal/ratelimit"
)

// 与原项目一致的默认常量（WINDOW / MAX_SEGS / REFRESH_INTERVAL / IDLE_TIMEOUT / BK_URL_TTL）。
const (
	DefaultWindow          = 300
	DefaultMaxSegs         = 120
	DefaultRefreshInterval = 10 * time.Second
	DefaultIdleTimeout     = 120 * time.Second
	DefaultBKURLTTL        = 300 * time.Second

	// DefaultUA 原项目 UA 常量。
	DefaultUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36"
	// DefaultJaccURL JCE 时移接口（原 jce_timeshift_url 中的硬编码地址）。
	DefaultJaccURL = "https://jacc.ysp.cctv.cn"
	// DefaultBKLiveInfoURL bkliveinfo 备用接口（原 bk_playurls 中的硬编码地址）。
	DefaultBKLiveInfoURL = "https://bkliveinfo.ysp.cctv.cn/"
)

// 各上游请求的超时，与原项目各自 urlopen(timeout=…) 保持一致。
const (
	jaccTimeout     = 15 * time.Second
	cdnTimeout      = 20 * time.Second
	bkTimeout       = 5 * time.Second
	playlistTimeout = 10 * time.Second
)

// 线路模式（原 ch.mode 的取值）。
const (
	ModeJCE = "jce" // 1080p JCE 主线路
	ModeBK  = "bk"  // bkliveinfo 备用线路
)

// 错误哨兵。
var (
	// ErrDeadHost JCE 返回的 m3u8 落在受限 CDN 域名（原 DeadHostError），上层据此切 bk。
	ErrDeadHost = errors.New("dead cdn host")
	// ErrUnknownChannel 频道不存在（原 get_channel_instance 返回 None）。
	ErrUnknownChannel = errors.New("unknown channel")
)

// Config Manager 配置。零值字段由 New 填充为与原项目一致的默认值。
type Config struct {
	UA              string        // 请求 UA
	Window          int           // JCE 时移窗口秒数（默认 300）
	MaxSegs         int           // 单频道最多缓存分片数（默认 120）
	RefreshInterval time.Duration // 后台拉取间隔（默认 10s）
	IdleTimeout     time.Duration // 空闲停止后台拉取（默认 120s）
	BKURLTTL        time.Duration // bkliveinfo 地址缓存时间（默认 300s）

	// 以下字段用于测试/私有部署注入，留空即使用原项目默认值。
	JaccURL       string                 // JCE 接口地址覆盖
	BKLiveInfoURL string                 // bkliveinfo 接口地址覆盖
	Limiter       *ratelimit.FlowLimiter // 全局流控覆盖；nil 用 ratelimit.Global
	Now           func() time.Time       // 时钟覆盖（测试用）
}

// segment 一个分片（原 ch.segments[key] = [seq, dur, pdt, url]）。
type segment struct {
	seq int64
	dur float64
	pdt string
	url string
}

// Channel 一路频道的 1080p/备用流状态（原 class Channel）。
// 导出字段仅作为诊断快照使用，跨 goroutine 读取请走 State()。
type Channel struct {
	Slug string
	Name string
	SID  string // 时移/回看频道 ID
	PID  string // 直播频道 ID
	Defn string // fhd / shd

	// 以下字段在 mu 保护下读写。
	Mode       string    // jce / bk
	LastOK     time.Time // 零值表示从未成功拉取
	LastError  string
	LastAccess time.Time
	Segments   int // 当前缓存分片数量

	mu        sync.Mutex
	segments  map[string]*segment
	order     []string // FIFO，最旧在前
	seq       int64
	lastPDT   string
	bkURLs    []string
	bkURLsAt  time.Time
	bkPl      string
	starting  bool
	loopOn    bool
	builtPl   string
	builtTime time.Time
	stopReq   bool
}

// Manager 全部频道的管理器（原 CHANNEL_MAP + 后台线程池 + 全局流控）。
type Manager struct {
	cfg     Config
	ua      string
	window  int
	maxSegs int
	refresh time.Duration
	idle    time.Duration
	bkTTL   time.Duration
	jaccURL string
	bkURL   string
	nowFn   func() time.Time
	limiter *ratelimit.FlowLimiter

	jaccClient *httpx.Client // jacc.ysp.cctv.cn（JCE 时移接口）
	bkClient   *httpx.Client // bkliveinfo（备用线路）
	fetcher    *httpFetcher  // m3u8 抓取（保留重定向后的最终 URL）

	channels map[string]*Channel
	order    []string // 按 model.DefaultChannels 顺序，Snapshot 输出用

	mu       sync.Mutex
	inflight map[string]*inflightEvent
	stopCh   chan struct{}
	stopped  bool
	wg       sync.WaitGroup
	stopOnce sync.Once
}

// inflightEvent 同一频道并发刷新合并的等待事件（原 _inflight_refresh[slug]）。
type inflightEvent struct {
	done chan struct{}
}

func (e *inflightEvent) wait(d time.Duration) {
	if d <= 0 {
		<-e.done
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-e.done:
	case <-t.C:
	}
}

// New 创建 Manager，并按 DEFAULT_CHANNELS 预建全部频道（FORCE_BK 频道初始为 bk 模式）。
func New(cfg Config) *Manager {
	if cfg.UA == "" {
		cfg.UA = DefaultUA
	}
	if cfg.Window <= 0 {
		cfg.Window = DefaultWindow
	}
	if cfg.MaxSegs <= 0 {
		cfg.MaxSegs = DefaultMaxSegs
	}
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = DefaultRefreshInterval
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = DefaultIdleTimeout
	}
	if cfg.BKURLTTL <= 0 {
		cfg.BKURLTTL = DefaultBKURLTTL
	}
	if cfg.JaccURL == "" {
		cfg.JaccURL = DefaultJaccURL
	}
	if cfg.BKLiveInfoURL == "" {
		cfg.BKLiveInfoURL = DefaultBKLiveInfoURL
	}
	if cfg.Limiter == nil {
		cfg.Limiter = ratelimit.Global
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	m := &Manager{
		cfg:        cfg,
		ua:         cfg.UA,
		window:     cfg.Window,
		maxSegs:    cfg.MaxSegs,
		refresh:    cfg.RefreshInterval,
		idle:       cfg.IdleTimeout,
		bkTTL:      cfg.BKURLTTL,
		jaccURL:    cfg.JaccURL,
		bkURL:      cfg.BKLiveInfoURL,
		nowFn:      cfg.Now,
		limiter:    cfg.Limiter,
		jaccClient: httpx.New(jaccTimeout.Seconds(), false),
		bkClient:   httpx.New(bkTimeout.Seconds(), false),
		fetcher:    newHTTPFetcher(),
		channels:   make(map[string]*Channel, len(model.DefaultChannels)),
		order:      make([]string, 0, len(model.DefaultChannels)),
		inflight:   make(map[string]*inflightEvent),
		stopCh:     make(chan struct{}),
	}
	for _, c := range model.DefaultChannels {
		ch := &Channel{
			Slug:     c.Slug,
			Name:     c.Name,
			SID:      c.SID,
			PID:      c.PID,
			Defn:     c.Defn,
			Mode:     ModeJCE,
			segments: make(map[string]*segment),
		}
		if model.ForceBK[c.Slug] {
			// 原项目：FORCE_BK 频道初始 mode 直接为 bk。
			ch.Mode = ModeBK
		}
		m.channels[c.Slug] = ch
		m.order = append(m.order, c.Slug)
	}
	return m
}

// now 返回当前时间（测试可注入）。
func (m *Manager) now() time.Time {
	if m.nowFn != nil {
		return m.nowFn()
	}
	return time.Now()
}

// Channel 按 slug（支持别名/.m3u8 后缀）取频道，不存在返回 nil。
// 对应原 get_channel_instance。
func (m *Manager) Channel(slug string) *Channel {
	m.mu.Lock()
	ch := m.channels[model.Canonical(slug)]
	m.mu.Unlock()
	return ch
}

// Channels 返回全部频道（按默认顺序），供订阅列表使用。
func (m *Manager) Channels() []*Channel {
	out := make([]*Channel, 0, len(m.order))
	for _, slug := range m.order {
		if ch := m.Channel(slug); ch != nil {
			out = append(out, ch)
		}
	}
	return out
}

// ChannelState 一路频道的诊断快照（/diag 用）。
type ChannelState struct {
	Slug          string
	Name          string
	Defn          string
	Mode          string
	LastOK        time.Time
	LastError     string
	LastAccess    time.Time
	Segments      int
	BKURLs        int
	HasBKPlaylist bool
	LoopRunning   bool
}

// Snapshot 返回全部频道的诊断快照（按默认频道顺序）。
func (m *Manager) Snapshot() []ChannelState {
	out := make([]ChannelState, 0, len(m.order))
	for _, slug := range m.order {
		if ch := m.Channel(slug); ch != nil {
			out = append(out, ch.State())
		}
	}
	return out
}

// State 返回该频道的加锁快照。
func (c *Channel) State() ChannelState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return ChannelState{
		Slug:          c.Slug,
		Name:          c.Name,
		Defn:          c.Defn,
		Mode:          c.Mode,
		LastOK:        c.LastOK,
		LastError:     c.LastError,
		LastAccess:    c.LastAccess,
		Segments:      len(c.segments),
		BKURLs:        len(c.bkURLs),
		HasBKPlaylist: c.bkPl != "",
		LoopRunning:   c.loopOn,
	}
}

// SegmentCount 当前缓存分片数量（加锁读取）。
func (c *Channel) SegmentCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.segments)
}

// CurrentMode 当前线路模式（加锁读取）。
func (c *Channel) CurrentMode() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Mode
}

// Touch 记录一次访问时间（对应处理器里的 ch.last_access = now）。
func (c *Channel) Touch() {
	c.mu.Lock()
	c.LastAccess = time.Now()
	c.mu.Unlock()
}

// setMode 切换线路模式（加锁）。
func (c *Channel) setMode(mode string) {
	c.mu.Lock()
	c.Mode = mode
	c.mu.Unlock()
}

// Stop 停止全部后台拉取循环并等待其退出（可重复调用）。
func (m *Manager) Stop() {
	m.stopOnce.Do(func() {
		m.mu.Lock()
		m.stopped = true
		close(m.stopCh)
		m.mu.Unlock()
	})
	m.wg.Wait()
}

// sleepOrStop 可被 Stop 打断的等待；返回 false 表示已停止。
func (m *Manager) sleepOrStop(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-m.stopCh:
		return false
	case <-t.C:
		return true
	}
}

// defnLabel 清晰度中文标签（原 defn_label）：fhd→1080p，shd→720p，其余原样返回。
func defnLabel(defn string) string {
	switch strings.ToLower(strings.TrimSpace(defn)) {
	case "fhd":
		return "1080p"
	case "shd":
		return "720p"
	}
	if strings.TrimSpace(defn) == "" {
		return "1080p"
	}
	return defn
}
