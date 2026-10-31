// Package gateway 实现四层兜底调度：设备协议 → 1080p JCE → bkliveinfo → 网页版 WASM 引擎。
//
// 对应原项目 Handler.do_GET 中 /<channel>.m3u8 分支的分级逻辑。
package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jacktea/ysp-live-go/internal/engine"
	"github.com/jacktea/ysp-live-go/internal/logx"
	"github.com/jacktea/ysp-live-go/internal/model"
)

// DeviceProvider 设备协议层（internal/resolver.Resolver 适配）。
type DeviceProvider interface {
	Ready() bool
	// ChannelPlaylist 返回已改写分片的 m3u8；("", nil) 表示换台防抖阶段，应回退到下层。
	ChannelPlaylist(ctx context.Context, channel, hostHeader string) (string, error)
	InCooldown(channel string) bool
	MarkChannelFailure(channel string, err error)
}

// FallbackProvider 1080p JCE + bkliveinfo 层（internal/live.Manager 适配）。
type FallbackProvider interface {
	// Playlist 返回该频道的 1080p / 备用线路 m3u8。
	Playlist(ctx context.Context, channel string) (string, error)
	// Touch 标记频道正在被观看，维持后台刷新循环。
	Touch(channel string)
}

// WebProvider 网页版兜底层（internal/engine/web.Engine 适配）。
type WebProvider interface {
	Playlist(ctx context.Context, channel string) (*engine.Playlist, error)
}

// Chain 四层兜底调度器。
type Chain struct {
	Device   DeviceProvider
	Fallback FallbackProvider
	Web      WebProvider

	// DeviceChannels 允许走设备协议的频道（26 路或仅 3 路 4K）。
	DeviceChannels map[string]bool
	// Timeshift 支持时移的频道集合。
	Timeshift map[string]bool

	// PlaylistTTL 播放列表缓存时间（对应原 ch.last_built_playlist 的 15s SWR）。
	PlaylistTTL time.Duration

	mu       sync.Mutex
	lastGood map[string]cachedPlaylist
	source   map[string]engine.Source
}

type cachedPlaylist struct {
	body string
	at   time.Time
	src  engine.Source
}

// NewChain 创建兜底链。
func NewChain(device DeviceProvider, fallback FallbackProvider, web WebProvider, deviceChannels map[string]bool) *Chain {
	return &Chain{
		Device:         device,
		Fallback:       fallback,
		Web:            web,
		DeviceChannels: deviceChannels,
		Timeshift:      model.TimeshiftSupported,
		PlaylistTTL:    15 * time.Second,
		lastGood:       make(map[string]cachedPlaylist),
		source:         make(map[string]engine.Source),
	}
}

// Result 一次解析结果。
type Result struct {
	Playlist string
	Source   engine.Source
	// Fallback1080p 表示当前返回的是 1080p 兜底（供日志/诊断）。
	Fallback1080p bool
}

// Resolve 按四层顺序解析频道 m3u8。
//
// 与原项目的差异（bug 修复）：
//   - 原实现把 choke 在 Handler 里手写，且 device 层失败时重复累计冷却计数；
//     这里统一在 Chain 内做一次计数，避免一次播放请求累计多次失败。
//   - 原实现在 device 层抛错后仍会继续用旧缓存，可能给播放器下发已失效地址；
//     这里改为失败后明确回退，但保留 SWR 的"最后一次可用"保护（见 stale 分支）。
func (c *Chain) Resolve(ctx context.Context, channel, hostHeader string) (*Result, error) {
	channel = model.Canonical(channel)
	now := time.Now()

	// 第 1 层：设备协议（4K / 高码率）
	if c.Device != nil && c.DeviceChannels[channel] && c.Device.Ready() && !c.Device.InCooldown(channel) {
		body, err := c.Device.ChannelPlaylist(ctx, channel, hostHeader)
		switch {
		case err == nil && body != "":
			return c.remember(channel, body, engine.SourceDevice)
		case err == nil && body == "":
			// 换台防抖：先出 1080p，后台已排入高码率预取。
			logx.Log(fmt.Sprintf("[%s] 换台防抖: 先出 1080p, 停留后自动升码率", channel), 10)
		default:
			// 设备层失败：记录一次失败（原实现在 Handler 里记录，这里集中处理）。
			c.Device.MarkChannelFailure(channel, err)
			logx.Log(fmt.Sprintf("[%s] 高码率播放暂时受限 (%s)，平滑回退 1080p", channel, cleanErr(err)), 5)
		}
	}

	// 第 2/3 层：1080p JCE / bkliveinfo
	if c.Fallback != nil {
		c.Fallback.Touch(channel)
		body, err := c.Fallback.Playlist(ctx, channel)
		if err == nil && strings.Contains(body, "#EXTM3U") {
			res, rerr := c.remember(channel, body, engine.SourceJCE)
			if rerr != nil {
				return nil, rerr
			}
			res.Fallback1080p = true
			return res, nil
		}
		if err == nil {
			err = errors.New("playlist missing #EXTM3U")
		}
		logx.Log(fmt.Sprintf("[%s] 1080p 线路不可用: %s", channel, cleanErr(err)), 8)
	}

	// 第 4 层：网页版 WASM 兜底引擎
	if c.Web != nil {
		pl, err := c.Web.Playlist(ctx, channel)
		if err == nil && pl != nil && strings.Contains(pl.Body, "#EXTM3U") {
			res, rerr := c.remember(channel, pl.Body, engine.SourceWeb)
			if rerr != nil {
				return nil, rerr
			}
			res.Fallback1080p = true
			return res, nil
		}
		if err != nil {
			logx.Log(fmt.Sprintf("[%s] 网页版兜底引擎失败: %s", channel, cleanErr(err)), 8)
		}
	}

	// 全部失败：如果最近 15s 内有过可用列表，继续下发（避免播放器直接断流）。
	c.mu.Lock()
	cached, ok := c.lastGood[channel]
	c.mu.Unlock()
	if ok && now.Sub(cached.at) < c.PlaylistTTL {
		return &Result{Playlist: cached.body, Source: cached.src, Fallback1080p: true}, nil
	}
	return nil, fmt.Errorf("%w: %s 所有线路均不可用", engine.ErrEngine, channel)
}

// remember 缓存并返回结果，同时在来源切换时插入 #EXT-X-DISCONTINUITY。
//
// 对应原项目：从 device 切到 bk 或反之都插入 discontinuity，避免播放器把两种码流拼在一起。
func (c *Chain) remember(channel, body string, src engine.Source) (*Result, error) {
	c.mu.Lock()
	prev, hadPrev := c.source[channel]
	c.source[channel] = src
	c.lastGood[channel] = cachedPlaylist{body: body, at: time.Now(), src: src}
	c.mu.Unlock()

	if hadPrev && prev != src {
		body = insertDiscontinuity(body)
	}
	return &Result{Playlist: body, Source: src}, nil
}

// insertDiscontinuity 在 #EXTM3U 之后插入 #EXT-X-DISCONTINUITY（仅插一次）。
func insertDiscontinuity(body string) string {
	if strings.Contains(body, "#EXT-X-DISCONTINUITY") {
		return body
	}
	return strings.Replace(body, "#EXTM3U", "#EXTM3U\n#EXT-X-DISCONTINUITY", 1)
}

// CachedSource 返回频道最近一次成功解析的来源。
func (c *Chain) CachedSource(channel string) engine.Source {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.source[model.Canonical(channel)]
}

func cleanErr(err error) string {
	if err == nil {
		return ""
	}
	s := strings.TrimSpace(err.Error())
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}
