package server

import (
	"context"
	"time"

	"github.com/jacktea/ysp-live-go/internal/config"
	"github.com/jacktea/ysp-live-go/internal/engine"
	"github.com/jacktea/ysp-live-go/internal/engine/web"
	"github.com/jacktea/ysp-live-go/internal/httpapi"
	"github.com/jacktea/ysp-live-go/internal/live"
	"github.com/jacktea/ysp-live-go/internal/model"
	"github.com/jacktea/ysp-live-go/internal/resolver"
)

// 本文件把三个具体引擎适配到 gateway / httpapi 需要的窄接口。
// 引擎实现变化时只需改这里，HTTP 路由层保持稳定。

// ---------------------------------------------------------------------------
// 设备协议层（internal/resolver）
// ---------------------------------------------------------------------------

// deviceAdapter 把 *resolver.Resolver 适配成 gateway.DeviceProvider 等接口。
type deviceAdapter struct {
	res *resolver.Resolver
	cfg *config.Config
}

func newDeviceAdapter(cfg *config.Config) *deviceAdapter {
	rc := resolver.DefaultConfig()
	// 逐项对齐原项目 main() 里传给 Resolver 的 engine_args
	rc.Timeout = cfg.Timeout
	rc.CacheTTL = cfg.CacheTTL
	rc.StaleWhileRefreshTTL = cfg.StaleWhileRefreshTTL
	rc.RefreshErrorCooldown = cfg.RefreshErrorCooldown
	rc.PlaylistCacheTTL = cfg.PlaylistCacheTTL
	rc.BackgroundRefreshQueueLimit = cfg.BackgroundRefreshQueue
	rc.RefreshInterval = cfg.RefreshInterval
	rc.HeartbeatInterval = cfg.HeartbeatInterval
	rc.HeartbeatTTLGuard = cfg.HeartbeatTTLGuard
	rc.SessionTTL = cfg.SessionTTL
	rc.LinksPerDevice = cfg.LinksPerDevice
	rc.ZapDebounce = cfg.ZapDebounce
	rc.Only4K = cfg.Only4K
	rc.MetaJSON = cfg.MetaJSON
	rc.DeviceJSON = cfg.DeviceJSON
	rc.HTTPWorkers = cfg.HTTPWorkers
	rc.HTTPQueueLimit = cfg.HTTPQueueLimit
	rc.IdentityResetErrorThreshold = cfg.IdentityResetThreshold
	rc.IdentityResetCooldown = cfg.IdentityResetCooldown
	rc.ServerPort = cfg.Port
	return &deviceAdapter{res: resolver.New(rc), cfg: cfg}
}

func (d *deviceAdapter) Start(ctx context.Context) { d.res.Start(ctx) }

func (d *deviceAdapter) Init(ctx context.Context) error { return d.res.Init(ctx) }

func (d *deviceAdapter) Ready() bool { return d.res.Ready() }

func (d *deviceAdapter) ChannelPlaylist(ctx context.Context, channel, hostHeader string) (string, error) {
	return d.res.ChannelPlaylist(ctx, channel, hostHeader)
}

func (d *deviceAdapter) InCooldown(channel string) bool { return d.res.InCooldown(channel) }

func (d *deviceAdapter) MarkChannelFailure(channel string, err error) {
	d.res.MarkChannelFailure(channel, err)
}

func (d *deviceAdapter) PlaybackHeaders(rawURL string) (map[string]string, error) {
	return d.res.PlaybackHeaders(rawURL)
}

func (d *deviceAdapter) DeviceStatus() httpapi.DeviceStatus {
	s := d.res.DeviceStatus()
	return httpapi.DeviceStatus{
		Ready:        s.Ready,
		Valid:        s.Valid,
		GUID:         s.GUID,
		Links:        s.Links,
		LinksLimit:   s.LinksLimit,
		Heartbeat:    s.Heartbeat,
		HeartbeatErr: s.HeartbeatErr,
	}
}

// ---------------------------------------------------------------------------
// 1080p / bkliveinfo 层（internal/live）
// ---------------------------------------------------------------------------

// fallbackAdapter 把 *live.Manager 适配成 gateway.FallbackProvider 等接口。
type fallbackAdapter struct {
	mgr *live.Manager
	cfg *config.Config
}

func newFallbackAdapter(cfg *config.Config, dir *model.ChannelDirectory) *fallbackAdapter {
	return &fallbackAdapter{
		cfg: cfg,
		mgr: live.New(live.Config{
			RefreshInterval: 10 * time.Second,
			IdleTimeout:     120 * time.Second,
			BKURLTTL:        300 * time.Second,
			Window:          300,
			MaxSegs:         120,
		}),
	}
}

func (f *fallbackAdapter) Start(ctx context.Context) {}

func (f *fallbackAdapter) Playlist(ctx context.Context, channel string) (string, error) {
	return f.mgr.Playlist(ctx, channel)
}

func (f *fallbackAdapter) Touch(channel string) { f.mgr.Touch(channel) }

func (f *fallbackAdapter) Catchup(ctx context.Context, channel string, startTS, endTS int64) (string, error) {
	return f.mgr.Catchup(ctx, channel, startTS, endTS)
}

func (f *fallbackAdapter) Snapshot() []httpapi.ChannelSnapshot {
	states := f.mgr.Snapshot()
	out := make([]httpapi.ChannelSnapshot, 0, len(states))
	for _, s := range states {
		out = append(out, httpapi.ChannelSnapshot{
			Slug:       s.Slug,
			Name:       s.Name,
			Mode:       s.Mode,
			LastOK:     s.LastOK,
			LastError:  s.LastError,
			Segments:   s.Segments,
			LastAccess: s.LastAccess,
		})
	}
	return out
}

func (f *fallbackAdapter) Stop() { f.mgr.Stop() }

// ---------------------------------------------------------------------------
// 网页版 WASM 层（internal/engine/web）
// ---------------------------------------------------------------------------

// webAdapter 把 *web.Engine 适配成 gateway.WebProvider 等接口。
type webAdapter struct {
	eng  *web.Engine
	cfg  *config.Config
	init error
}

func newWebAdapter(cfg *config.Config) *webAdapter {
	a := &webAdapter{cfg: cfg}
	if cfg.NoEngine {
		return a
	}
	eng, err := web.New(web.Config{
		UpstreamTimeout: time.Duration(cfg.Timeout * float64(time.Second)),
		// 分片地址改写为本服务的 /segment?url=，与 Node 版语义一致
		SegmentProxyPath: "/segment?url=",
	})
	if err != nil {
		a.init = err
		return a
	}
	a.eng = eng
	return a
}

func (w *webAdapter) Start(ctx context.Context) error {
	if w.init != nil {
		return w.init
	}
	if w.eng == nil {
		return nil
	}
	return w.eng.Start(ctx)
}

func (w *webAdapter) Playlist(ctx context.Context, channel string) (*engine.Playlist, error) {
	if w.eng == nil {
		return nil, engine.ErrEngine
	}
	return w.eng.Playlist(ctx, channel)
}

func (w *webAdapter) SegmentProxy(ctx context.Context, targetURL, rangeHeader string) (*httpapi.SegmentResponse, error) {
	if w.eng == nil {
		return nil, engine.ErrEngine
	}
	resp, err := w.eng.SegmentProxy(ctx, targetURL, rangeHeader)
	if err != nil {
		return nil, err
	}
	return &httpapi.SegmentResponse{
		Status:        resp.Status,
		Body:          resp.Body,
		ContentType:   resp.ContentType,
		ContentLength: resp.ContentLength,
	}, nil
}

func (w *webAdapter) Close() error {
	if w.eng == nil {
		return nil
	}
	return w.eng.Close()
}
