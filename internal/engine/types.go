// Package engine 定义四层兜底引擎之间的公共类型与接口，避免各引擎互相 import。
package engine

import (
	"context"
	"errors"
)

// 统一的引擎错误（原 YsptpError）。
var (
	ErrEngine         = errors.New("ysp-engine")
	ErrHTTP400        = errors.New("ysp-engine: HTTP 400")
	ErrCooldown       = errors.New("ysp-engine: channel in cooldown")
	ErrQuotaReached   = errors.New("ysp-engine: device link quota reached")
	ErrUnknownChannel = errors.New("ysp-engine: unknown channel")
	ErrNoUsableURL    = errors.New("ysp-engine: no usable URL")
	ErrMissingSigned  = errors.New("ysp-engine: missing cached signed playback headers")
)

// Playlist 一个可直接下发给播放器的 m3u8 文本。
type Playlist struct {
	Body        string
	ContentType string
	// Source 标识本播放列表来自哪一层兜底，用于日志与 /diag。
	Source Source
}

// Source 播放列表来源。
type Source string

const (
	SourceDevice Source = "device" // 设备协议（4K / 高码率）
	SourceJCE    Source = "jce"    // 1080p JCE 解析
	SourceBK     Source = "bk"     // bkliveinfo 备用线路
	SourceWeb    Source = "web"    // 网页版兜底引擎
)

// ChannelResolver 任意一层引擎需要实现的最小接口。
type ChannelResolver interface {
	// Name 引擎名称，用于日志。
	Name() string
	// Supports 该引擎是否可能提供此频道。
	Supports(channel string) bool
	// Playlist 获取该频道的 m3u8（已把分片地址改写为指向本服务的代理地址）。
	Playlist(ctx context.Context, channel string) (*Playlist, error)
}
