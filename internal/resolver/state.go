package resolver

import (
	"strings"
	"sync"
	"time"

	"github.com/jacktea/ysp-live-go/internal/httpx"
	"github.com/jacktea/ysp-live-go/internal/model"
)

// ---------------------------------------------------------------------------
// 状态结构（对应原 ResolverState / AppSession / RefreshQueue / HttpQueue）
// ---------------------------------------------------------------------------

// AppSession 一个设备槽位的会话（原 AppSession dataclass）。
//
// 可变字段（linkedChannels / Heartbeat* / SessionKey）由 Resolver.stateLock 保护：
// 原实现中 linked_channels 是 Python set，在 state_lock 内读写、在 control_lock 外读取，
// Go 版统一收敛到 stateLock 之下，避免数据竞争。
type AppSession struct {
	Profile     model.DeviceProfile
	Identity    *model.Identity
	Client      *httpx.Client
	SessionKey  string
	CloudGUID   string
	Version     string
	ScreenParam string
	CastModel   string

	CreatedAt           float64
	Generation          int
	LastHeartbeatAt     float64
	HeartbeatCount      int64
	LastHeartbeatResult string
	LastHeartbeatError  string

	// linkedChannels 已占用高码率名额的频道集合（原 linked_channels）。
	linkedChannels map[string]bool
}

// newAppSession 初始化会话内部集合。
func newAppSession(s *AppSession) *AppSession {
	if s.linkedChannels == nil {
		s.linkedChannels = make(map[string]bool)
	}
	return s
}

// linkedChannelCount 已占用的高码率名额数（原 len(app_session.linked_channels)）。
// 调用方必须持有 Resolver.stateLock。
func (s *AppSession) linkedChannelCount() int {
	if s == nil {
		return 0
	}
	return len(s.linkedChannels)
}

// RefreshQueue 后台刷新队列（原 RefreshQueue）。
type RefreshQueue struct {
	mu               sync.Mutex
	queued           []string
	queuedChannels   map[string]float64
	runningChannel   string
	runningStartedAt float64

	enqueuedTotal  int64
	droppedTotal   int64
	completedTotal int64
	failedTotal    int64
	lastEnqueuedAt float64
	lastFinishedAt float64
	lastDroppedAt  float64
	lastDropReason string

	// signal 唤醒 refresh_worker_loop（等价原 cond.notify）。
	signal chan struct{}
}

// newRefreshQueue 创建刷新队列。
func newRefreshQueue() *RefreshQueue {
	return &RefreshQueue{
		queuedChannels: make(map[string]float64),
		signal:         make(chan struct{}, 1),
	}
}

// HttpQueue HTTP 队列统计（原 HttpQueue）。
//
// 原实现只把 HttpQueue 用于 status_json 展示（没有真正入队/出队逻辑），
// Go 版保留结构并记录同样维度的统计，行为对齐。
type HttpQueue struct {
	mu              sync.Mutex
	activeWorkers   int
	enqueuedTotal   int64
	rejectedTotal   int64
	completedTotal  int64
	panickedTotal   int64
	lastEnqueuedAt  float64
	lastCompletedAt float64
	lastRejectedAt  float64
}

// newHttpQueue 创建 HTTP 队列统计。
func newHttpQueue() *HttpQueue { return &HttpQueue{} }

// PlaylistCacheEntry 播放列表缓存条目（原 PlaylistCacheEntry）。
type PlaylistCacheEntry struct {
	Body        string
	ContentType string
	CachedAt    float64
	ExpiresAt   float64
	FinalURL    string
	AndroidID   string
	ProxyOrigin string
	ProxyPrefix string
}

// PlaylistCacheProbe 播放列表缓存探测结果（原 PlaylistCacheProbe）。
type PlaylistCacheProbe struct {
	State string
	AgeMs *int64
}

// AppStartResult app/start 返回（原 AppStartResult）。
type AppStartResult struct {
	SessionKey string
}

// Live01Result live/v1/01 返回（原 Live01Result）。
type Live01Result struct {
	LiveURL  string
	Rate     string
	RateName string
}

// VdnGetStreamResult VDN getstream 返回（原 VdnGetStreamResult）。
type VdnGetStreamResult struct {
	FinalURL     string
	AppSign      string
	AppRandomStr string
}

// channelFailure 频道失败记录（原 state.channel_failures 的值：failed_at / err / fail_cnt）。
type channelFailure struct {
	failedAt  float64
	err       string
	failCount int
}

// resolverState Resolver 的可变状态（原 ResolverState）。
//
// 除 RefreshQueue / HttpQueue 自带锁外，其余字段统一由 Resolver.stateLock 保护。
type resolverState struct {
	cache                map[string]*model.ChannelEntry
	playlistCache        map[string]*PlaylistCacheEntry
	backgroundRefreshing map[string]float64
	channelFailures      map[string]*channelFailure

	generation        int
	sessionGeneration int
	sessions          map[int]*AppSession
	channelSlotMap    map[string]int
	slotLastReset     map[int]float64

	lastControlRequestAt float64
	lastBusinessEndAt    float64
	refreshingChannel    string
	lastError            string
	lastErrorAt          float64

	identityResetErrorCount int
	channelLastAccess       map[string]float64
	identityResetCount      int
	lastIdentityResetAt     float64
	lastIdentityResetReason string
	adDisplay               bool

	// 换台防抖
	zapChannel    string
	zapTime       float64
	zapPending    string
	zapGeneration int

	// 换设备熔断
	lastDeviceWipeAt float64
	deviceWipeDay    string
	deviceWipeCount  int
}

// newResolverState 创建状态（原 ResolverState.__init__）。
func newResolverState() *resolverState {
	return &resolverState{
		cache:                make(map[string]*model.ChannelEntry),
		playlistCache:        make(map[string]*PlaylistCacheEntry),
		backgroundRefreshing: make(map[string]float64),
		channelFailures:      make(map[string]*channelFailure),
		sessions:             make(map[int]*AppSession),
		channelSlotMap:       make(map[string]int),
		slotLastReset:        make(map[int]float64),
		channelLastAccess:    make(map[string]float64),
	}
}

// inspectPlaylistCache 判断播放列表缓存是否命中（原 inspect_playlist_cache）。
func inspectPlaylistCache(cached *PlaylistCacheEntry, entry *model.ChannelEntry, proxyOrigin, proxyPrefix string, now float64) PlaylistCacheProbe {
	if cached == nil {
		return PlaylistCacheProbe{State: "miss:not_found"}
	}
	ageMs := rustRoundInt(maxF(0, now-cached.CachedAt) * 1000.0)
	probe := PlaylistCacheProbe{AgeMs: &ageMs}
	fresh := now < cached.ExpiresAt
	sameFinalURL := cached.FinalURL == entry.FinalURL
	sameAndroidID := cached.AndroidID == entry.AndroidID
	sameOrigin := cached.ProxyOrigin == proxyOrigin
	samePrefix := cached.ProxyPrefix == proxyPrefix
	if fresh && sameFinalURL && sameAndroidID && sameOrigin && samePrefix {
		probe.State = "hit"
		return probe
	}
	var reasons []string
	if !fresh {
		reasons = append(reasons, "expired")
	}
	if !sameFinalURL {
		reasons = append(reasons, "final_url_changed")
	}
	if !sameAndroidID {
		reasons = append(reasons, "android_id_changed")
	}
	if !sameOrigin {
		reasons = append(reasons, "origin_changed")
	}
	if !samePrefix {
		reasons = append(reasons, "prefix_changed")
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "unknown")
	}
	probe.State = "miss:" + strings.Join(reasons, "+")
	return probe
}

// rustRoundInt 复刻原 rust_round（math.floor(x+0.5)）的整数取整语义。
func rustRoundInt(x float64) int64 { return int64(x + 0.5) }

// rustRound 复刻原 rust_round 的浮点语义。
func rustRound(x float64) float64 { return float64(rustRoundInt(x)) }

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// nowF64 当前 Unix 秒（浮点）（原 now_f64）。
func nowF64() float64 { return model.NowF64() }

// dayString 当日日期字符串（原 time.strftime('%Y-%m-%d', time.localtime(now))）。
func dayString(now float64) string {
	return time.Unix(int64(now), 0).Format("2006-01-02")
}

// appSessionLocked 返回 slot 0 会话，缺失时退到任一槽位（原 ResolverState.app_session 属性）。
// 调用方必须持有 Resolver.stateLock。
func (r *Resolver) appSessionLocked() *AppSession {
	if sess := r.state.sessions[0]; sess != nil {
		return sess
	}
	for _, sess := range r.state.sessions {
		return sess
	}
	return nil
}

// sessionFresh 会话是否仍在 TTL 内（原 app_session_fresh）。
func (r *Resolver) sessionFresh(s *AppSession) bool {
	if s == nil {
		return false
	}
	if r.cfg.SessionTTL <= 0 {
		return true
	}
	return nowF64()-s.CreatedAt < r.cfg.SessionTTL
}

// sessionTTLRemaining 会话剩余 TTL 秒；SessionTTL<=0 时返回 nil（原 app_session_ttl_remaining）。
// 调用方必须持有 Resolver.stateLock（只读字段）。
func (r *Resolver) sessionTTLRemaining(s *AppSession) *float64 {
	if r.cfg.SessionTTL <= 0 || s == nil {
		return nil
	}
	rem := r.cfg.SessionTTL - (nowF64() - s.CreatedAt)
	return &rem
}
