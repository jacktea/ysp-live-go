package resolver

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jacktea/ysp-live-go/internal/httpapi"
	"github.com/jacktea/ysp-live-go/internal/httpx"
	"github.com/jacktea/ysp-live-go/internal/logx"
	"github.com/jacktea/ysp-live-go/internal/model"
)

// Resolver 央视频设备协议解析器（原 Python 类 Resolver）。
//
// 一个设备身份（slot 0）承担全部 26 路高码率频道，因此所有状态集中在本结构里：
//
//	stateLock     保护 resolverState 全部可变字段（原 state_lock）
//	controlLock   设备身份/频道刷新等控制面临界区（原 control_lock）
//	playlistLocks 按频道串行化播放列表抓取（原 playlist_locks）
//
// 对外方法全部并发安全。
type Resolver struct {
	cfg Config
	eps endpoints

	stateLock sync.Mutex
	state     *resolverState

	// controlLock 控制面锁。原实现用 threading.RLock 允许重入；Go 版改为
	// "公开方法加锁 + *Locked 私有方法假定已持锁" 的写法，避免重入死锁。
	controlLock sync.Mutex

	playlistLocksLock sync.Mutex
	playlistLocks     map[string]*sync.Mutex

	queue     *RefreshQueue
	httpQueue *HttpQueue

	genericClient *httpx.Client

	store      *model.DeviceStateStore
	devicePath string

	ready atomic.Bool
	// statusSlot 当前正在刷新的频道（供状态快照；已由 stateLock 保护，此处仅为文档说明）。
	workCtx    context.Context
	workCancel context.CancelFunc
	startOnce  sync.Once
	stopOnce   sync.Once
	wg         sync.WaitGroup
	// bgWg 跟踪遥测/防抖预取等可选的短生命周期后台任务，Stop 时等待它们结束，
	// 避免进程退出（或测试清理临时目录）时仍有 goroutine 写文件。
	bgWg sync.WaitGroup
}

// New 创建 Resolver 并从 MetaJSON 恢复缓存（原 Resolver.__init__ + load_cache）。
func New(cfg Config) *Resolver {
	cfg.normalize()
	r := &Resolver{
		cfg:           cfg,
		eps:           cfg.endpoints(),
		state:         newResolverState(),
		playlistLocks: make(map[string]*sync.Mutex),
		queue:         newRefreshQueue(),
		httpQueue:     newHttpQueue(),
		store:         model.NewDeviceStateStore(cfg.DeviceJSON),
		devicePath:    cfg.DeviceJSON,
	}
	r.genericClient = httpx.New(cfg.Timeout, false)
	// 原实现启动时恢复 cache_detail（频道级缓存），但不会恢复会话密钥（只在内存中）。
	if cached, err := loadCache(cfg.MetaJSON); err == nil {
		for k, v := range cached {
			r.state.cache[k] = v
		}
	}
	r.workCtx, r.workCancel = context.WithCancel(context.Background())
	return r
}

// Config 返回当前生效配置（副本）。
func (r *Resolver) Config() Config { return r.cfg }

// Name 引擎名称（满足 engine.ChannelResolver 风格的日志需求）。
func (r *Resolver) Name() string { return "device" }

// Supports 该频道是否可能由设备协议提供。
//
// 对应原项目 active_backend_channels 的选择逻辑：Only4K 时仅 4K/8K 三路，
// 否则 26 路高码率频道（model.BackendChannels）。
func (r *Resolver) Supports(channel string) bool {
	channel = model.Canonical(channel)
	if r.cfg.Only4K {
		return model.True4KChannels[channel]
	}
	return model.BackendChannels[channel]
}

// Ready 设备身份是否就绪（原 set_resolver_ready / resolver_ready 的全局状态）。
func (r *Resolver) Ready() bool { return r.ready.Load() }

// Start 启动心跳与后台刷新 worker（原 start_heartbeat + start_refresh_worker，不阻塞）。
//
// 重复调用是安全的（只启动一次）。传入的 ctx 取消时两个后台循环退出。
func (r *Resolver) Start(ctx context.Context) {
	r.startOnce.Do(func() {
		if ctx == nil {
			ctx = context.Background()
		}
		// 后台循环同时跟随调用方 ctx 与 Stop()（r.workCancel）。
		runCtx, cancel := context.WithCancel(ctx)
		r.wg.Add(3)
		go func() {
			defer r.wg.Done()
			<-r.workCtx.Done()
			cancel()
		}()
		go r.heartbeatLoop(runCtx)
		go r.refreshWorkerLoop(runCtx)
	})
}

// Stop 停止后台循环（Go 版新增，供进程优雅退出）。
func (r *Resolver) Stop() {
	r.stopOnce.Do(func() {
		r.workCancel()
	})
	r.wg.Wait()
	r.bgWg.Wait()
}

// Init 初始化设备身份（原 init_resolver 的重试循环）。
//
// 与原实现的差异：原实现是无限重试的守护线程；Go 版按 InitMaxAttempts 重试，
// 失败返回最后一次错误（不阻塞调用方永远不返回），ready 保持 false，上层自动降级 1080p。
func (r *Resolver) Init(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	logx.Log("[系统] 正在初始化设备协议并发池…", 0)
	var lastErr error
	for attempt := 1; attempt <= r.cfg.InitMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.initOnce(ctx); err != nil {
			lastErr = err
			logx.Log(fmt.Sprintf("[系统] 设备协议初始化第 %d 次重试中: %v，4K/高码率频道暂走 1080p 回退", attempt, err), 10)
			r.ready.Store(false)
			if strings.Contains(err.Error(), "HTTP 400") {
				r.rotateDeviceOn400("init:" + truncateString(err.Error(), 60))
			}
			if attempt >= r.cfg.InitMaxAttempts {
				break
			}
			// 原实现：backoff = min(15.0 * 1.5 ** min(retry_count-1, 4), 60.0)
			exp := attempt - 1
			if exp > 4 {
				exp = 4
			}
			backoff := r.cfg.InitBackoff * math.Pow(1.5, float64(exp))
			if backoff > r.cfg.InitBackoffMax {
				backoff = r.cfg.InitBackoffMax
			}
			if err := sleepCtx(ctx, time.Duration(backoff*float64(time.Second))); err != nil {
				return lastErr
			}
			continue
		}
		r.ready.Store(true)
		logx.Log("[系统] 设备身份就绪，高码率并发池已激活 (全部 26 路按需解析)", 0)
		return nil
	}
	return lastErr
}

// initOnce 单次初始化：确保 slot 0 会话可用。
func (r *Resolver) initOnce(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := r.ensureSlotSessionE(0); err != nil {
		return err
	}
	r.ready.Store(true)
	return nil
}

// SetReady 显式设置就绪状态（测试与上层降级用）。
func (r *Resolver) SetReady(v bool) { r.ready.Store(v) }

// ---------------------------------------------------------------------------
// 后台循环（原 start_heartbeat / start_refresh_worker）
// ---------------------------------------------------------------------------

// heartbeatLoop 心跳守护循环（原 start_heartbeat 里的 loop）。
func (r *Resolver) heartbeatLoop(ctx context.Context) {
	defer r.wg.Done()
	interval := r.cfg.HeartbeatInterval
	if interval < 1.0 {
		interval = 1.0
	}
	ticker := time.NewTicker(time.Duration(interval * float64(time.Second)))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.sendHeartbeat()
		}
	}
}

// refreshWorkerLoop 后台刷新 worker（原 refresh_worker_loop）。
func (r *Resolver) refreshWorkerLoop(ctx context.Context) {
	defer r.wg.Done()
	q := r.queue
	for {
		// 等待队列非空或退出。
		select {
		case <-ctx.Done():
			return
		case <-q.signal:
		}
		for {
			q.mu.Lock()
			if len(q.queued) == 0 {
				q.mu.Unlock()
				break
			}
			channel := q.queued[0]
			q.queued = q.queued[1:]
			delete(q.queuedChannels, channel)
			q.runningChannel = channel
			q.runningStartedAt = nowF64()
			q.mu.Unlock()

			if ctx.Err() != nil {
				return
			}
			ok := r.refreshChannelBackground(channel)

			q.mu.Lock()
			q.runningChannel = ""
			q.runningStartedAt = 0
			q.lastFinishedAt = nowF64()
			if ok {
				q.completedTotal++
			} else {
				q.failedTotal++
			}
			q.mu.Unlock()
		}
	}
}

// sendHeartbeat 向后台上报心跳并刷新会话（原 send_heartbeat）。
//
// 与原实现一致：拿不到控制锁时直接返回（不阻塞业务请求）。
func (r *Resolver) sendHeartbeat() {
	if !r.controlLock.TryLock() {
		return
	}
	defer r.controlLock.Unlock()
	r.sendHeartbeatLocked()
}

// sendHeartbeatLocked 假定已持有 controlLock（原 send_heartbeat 主体）。
func (r *Resolver) sendHeartbeatLocked() {
	r.stateLock.Lock()
	slots := make([]int, 0, len(r.state.sessions))
	for slot := range r.state.sessions {
		slots = append(slots, slot)
	}
	r.stateLock.Unlock()
	if len(slots) == 0 {
		return
	}
	for _, slot := range slots {
		r.stateLock.Lock()
		sess := r.state.sessions[slot]
		r.stateLock.Unlock()
		if sess == nil {
			continue
		}
		// 原实现：not app_session_fresh(s) or (app_session_ttl_remaining(s) or 0.0) <= 300.0
		// （注意这里是硬编码 300s，与 ensure_slot_session 的阈值一致；
		//  SessionTTL<=0 时 ttl_remaining 为 None，Python 视为 0.0，因此每次心跳都会重建会话。）
		if !r.sessionFresh(sess) || ttlOrDefault(r.sessionTTLRemaining(sess)) <= 300.0 {
			var err error
			sess, err = r.ensureSlotSessionLockedE(slot)
			if err != nil {
				r.stateLock.Lock()
				if cur := r.state.sessions[slot]; cur != nil {
					cur.LastHeartbeatError = err.Error()
				}
				r.state.lastError = fmt.Sprintf("heartbeat failed on slot %d: %v", slot, err)
				r.state.lastErrorAt = nowF64()
				r.stateLock.Unlock()
				continue
			}
		}
		r.stateLock.Lock()
		wait := r.cfg.RefreshInterval - (nowF64() - r.state.lastBusinessEndAt)
		r.stateLock.Unlock()
		if wait > 0 {
			time.Sleep(time.Duration(wait * float64(time.Second)))
		}
		code, err := r.heartbeatFlow(sess.Profile, sess.Identity, "dangbei", sess.Version, sess.CloudGUID)
		r.stateLock.Lock()
		r.state.lastBusinessEndAt = nowF64()
		if err != nil {
			if cur := r.state.sessions[slot]; cur != nil {
				cur.LastHeartbeatError = err.Error()
			}
			r.state.lastError = fmt.Sprintf("heartbeat failed on slot %d: %v", slot, err)
			r.state.lastErrorAt = nowF64()
		} else if cur := r.state.sessions[slot]; cur != nil {
			cur.LastHeartbeatAt = nowF64()
			cur.HeartbeatCount++
			cur.LastHeartbeatResult = code
			cur.LastHeartbeatError = ""
		}
		r.stateLock.Unlock()
	}
	r.ready.Store(true)
}

// ttlOrDefault 把 nil TTL（SessionTTL<=0）视为 0.0，对齐原实现 `ttl or 0.0`。
func ttlOrDefault(rem *float64) float64 {
	if rem == nil {
		return 0.0
	}
	return *rem
}

// ---------------------------------------------------------------------------
// 控制面请求节流（原 control_request_slot / control_step_jitter_duration）
// ---------------------------------------------------------------------------

// controlRequestSlot 在满足最小间隔（RefreshInterval）与随机抖动后执行一次控制面请求。
func (r *Resolver) controlRequestSlot(action func() error) error {
	if r.cfg.RefreshInterval > 0 {
		for {
			r.stateLock.Lock()
			last := r.state.lastControlRequestAt
			r.stateLock.Unlock()
			wait := 0.0
			if last > 0 {
				wait = r.cfg.RefreshInterval - (nowF64() - last)
			}
			if wait <= 0 {
				break
			}
			time.Sleep(time.Duration(wait * float64(time.Second)))
		}
	}
	if jitter := r.controlStepJitter(); jitter > 0 {
		time.Sleep(time.Duration(jitter * float64(time.Second)))
	}
	err := action()
	r.stateLock.Lock()
	r.state.lastControlRequestAt = nowF64()
	r.stateLock.Unlock()
	return err
}

// controlStepJitter 控制面请求随机抖动（原 control_step_jitter_duration，单位秒）。
func (r *Resolver) controlStepJitter() float64 {
	minMs := r.cfg.ControlStepJitterMinMs
	maxMs := r.cfg.ControlStepJitterMaxMs
	if maxMs == 0 {
		return 0
	}
	upper := maxMs
	if minMs > upper {
		upper = minMs
	}
	span := upper - minMs
	extra := 0
	if span > 0 {
		extra = rand.Intn(span + 1)
	}
	return float64(minMs+extra) / 1000.0
}

// newYtpaddrClient 创建新的 HTTP 客户端（原 new_ytpaddr_client）。
func (r *Resolver) newYtpaddrClient() *httpx.Client {
	return httpx.New(r.cfg.Timeout, false)
}

// ---------------------------------------------------------------------------
// 生命周期辅助
// ---------------------------------------------------------------------------

// goBackground 启动一个被 Stop 等待的后台任务。
func (r *Resolver) goBackground(fn func()) {
	r.bgWg.Add(1)
	go func() {
		defer r.bgWg.Done()
		defer func() { _ = recover() }()
		fn()
	}()
}

// sleepCtx 可被 ctx 取消的 sleep。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// truncateString 按字节截断（原实现用 Python 切片截断字符串）。
func truncateString(s string, n int) string {
	if n < 0 || len(s) <= n {
		return s
	}
	return s[:n]
}

// asResolverErr 保证错误带有本包 Err 前缀（原 YsptpError 统一类型）。
func asResolverErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, Err) {
		return err
	}
	return fmt.Errorf("%w: %v", Err, err)
}

// 编译期接口断言：*Resolver 必须满足 internal/server 的 DeviceEngine 契约
// （server 包定义了同名窄接口；这里用等价接口本地断言，避免 import 环）。
//   - internal/gateway.DeviceProvider: Ready/ChannelPlaylist/InCooldown/MarkChannelFailure
//   - internal/httpapi.SegmentFetcher: PlaybackHeaders
//   - internal/server.DeviceEngine: 上述 + Init + DeviceStatus
var (
	_ interface {
		Init(ctx context.Context) error
		Ready() bool
		ChannelPlaylist(ctx context.Context, channel, hostHeader string) (string, error)
		InCooldown(channel string) bool
		MarkChannelFailure(channel string, err error)
		PlaybackHeaders(rawURL string) (map[string]string, error)
		DeviceStatus() httpapi.DeviceStatus
	} = (*Resolver)(nil)
)
