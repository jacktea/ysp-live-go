// Package server 负责把四个引擎装配成一个服务，并实现 httpapi 所需的适配接口。
//
// 装配关系（对应原项目 main() + Handler）：
//
//	gateway.Chain ──┬── device   适配器 → internal/resolver.Resolver
//	                ├── fallback 适配器 → internal/live.Manager
//	                └── web      适配器 → internal/engine/web.Engine
//	httpapi.Server ── 使用 Chain / TSCache / 各适配器的状态与分片代理能力
package server

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/jacktea/ysp-live-go/internal/cache"
	"github.com/jacktea/ysp-live-go/internal/config"
	"github.com/jacktea/ysp-live-go/internal/gateway"
	"github.com/jacktea/ysp-live-go/internal/httpapi"
	"github.com/jacktea/ysp-live-go/internal/logx"
	"github.com/jacktea/ysp-live-go/internal/model"
)

// App 装配后的应用。
type App struct {
	cfg     *config.Config
	dir     *model.ChannelDirectory
	chain   *gateway.Chain
	tsCache *cache.TsSegmentCache
	api     *httpapi.Server

	device   *deviceAdapter
	fallback *fallbackAdapter
	web      *webAdapter

	closeOnce sync.Once
	closers   []func()
}

// Options 装配参数。
type Options struct {
	Config *config.Config
	Dir    *model.ChannelDirectory
}

// New 创建应用。
//
// 任一引擎初始化失败都不阻止启动：设备协议失败会退化为纯 1080p 模式，
// 网页版引擎失败会退化为三层兜底（与原项目"注册失败快速降级"策略一致）。
func New(o Options) (*App, error) {
	a := &App{cfg: o.Config, dir: o.Dir}

	if o.Config.CacheMB > 0 {
		a.tsCache = cache.NewTsSegmentCache(o.Config.CacheMB, 180*time.Second)
	}

	deviceChans := model.BackendChannels
	if o.Config.Only4K {
		deviceChans = model.True4KChannels
	}

	a.device = newDeviceAdapter(o.Config)
	a.fallback = newFallbackAdapter(o.Config, o.Dir)
	a.web = newWebAdapter(o.Config)

	a.chain = gateway.NewChain(a.device, a.fallback, a.web, deviceChans)

	a.closers = append(a.closers, func() {
		a.device.res.Stop()
		a.fallback.Stop()
		_ = a.web.Close()
	})

	a.api = httpapi.New(httpapi.Options{
		Chain:       a.chain,
		Dir:         o.Dir,
		Segments:    a.device,
		TSCache:     a.tsCache,
		Status:      a.device,
		Catchup:     a.fallback,
		Channels:    a.fallback,
		Web:         a.web,
		DeviceChans: deviceChans,
		StartTime:   time.Now(),
		Only4K:      o.Config.Only4K,
	})
	return a, nil
}

// Handler 返回 HTTP 处理器。
func (a *App) Handler() http.Handler { return a.api.Handler() }

// Start 启动后台引擎。
func (a *App) Start(ctx context.Context) {
	if !a.cfg.No4K {
		a.device.Start(ctx)
	}
	a.fallback.Start(ctx)
	if !a.cfg.NoEngine {
		if err := a.web.Start(ctx); err != nil {
			logx.Log("[引擎] 网页版 WASM 兜底引擎启动失败: "+err.Error()+" (继续使用三层兜底)", 10)
		}
	}
	// 设备协议注册在后台进行，不阻塞 HTTP 服务（原项目 init_resolver 也是后台线程）。
	if !a.cfg.No4K {
		go a.device.Init(ctx)
	}
}

// Close 释放资源。
func (a *App) Close() {
	a.closeOnce.Do(func() {
		for _, fn := range a.closers {
			fn()
		}
	})
}
