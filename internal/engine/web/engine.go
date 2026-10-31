// Package web 移植原 ysp-engine.js（Node 网页版兜底引擎），
// 用 wazero 在进程内执行它内嵌的两个 Rust/WASM 模块，作为四层兜底里的最后一级。
//
// 与 JS 版本的差异（均由本包在 Go 侧补齐或修复）：
//   - 不自行监听端口：只提供 Playlist/AllM3U/SegmentProxy，由 gateway 统一暴露。
//   - 分片地址改写成可配置前缀（原 JS 硬编码 http://127.0.0.1:PORT/segment?url=）。
//   - 结果缓存有上限并做 LRU（原 JS 的 Map 无上限）。
//   - /segment 做域名白名单校验（原 JS 可被当任意代理，SSRF）。
package web

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// 内嵌的 WASM 二进制（由 gen/gen_assets.py 从 ysp-engine.js 的
// KEYGEN_B64 / TICKET_B64 抽取，ticket_a.wasm 是 buildTicket 里 a 模块的等价物）。
var (
	//go:embed wasm/keygen.wasm
	keygenWasm []byte
	//go:embed wasm/ticket.wasm
	ticketWasm []byte
	//go:embed wasm/ticket_a.wasm
	ticketAWasm []byte
)

// ticketAModuleName ticket.wasm 里 a 模块的名字（见 gen 打印的 import 段）。
const ticketAModuleName = "a"

// 原 ysp-engine.js 的常量。
const (
	// DefaultUA 原 JS 里各请求使用的浏览器 UA。
	DefaultUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"

	// yspAppID 网页版 yspappid（原 JS 的 '519748109'）。
	yspAppID = "519748109"
	// appVersion buildCKey/buildTicket/liveSignBody 用的 'V1.0.0'。
	appVersion = "V1.0.0"
	// cacheTTL 原 JS playlistCache 的 30000ms 有效期。
	cacheTTL = 30 * time.Second
	// cacheMaxEntries 修复原项目 bug: 原 JS 的 playlistCache 是无上限 Map，
	// 频道多时会无限增长；这里限制条目数并做简单 LRU。
	cacheMaxEntries = 128

	// maxSegmentBytes 修复原项目 bug: 原 JS httpGetBuffer 不限制响应体大小，
	// 这里限制到 32MB，避免恶意/异常上游把进程内存打爆。
	maxSegmentBytes = 32 << 20

	// authSalt 原 JS auth 签名后缀 'n@7QKk%YeSjfw%22'。
	authSalt = "n@7QKk%YeSjfw%22"
	// liveSignSalt 原 JS get_live_info 最终签名后缀 '0f$IVHi9Qno?G'。
	liveSignSalt = "0f$IVHi9Qno?G"

	// videoAuthKeyHex buildCKey 的 AES-128 密钥。
	videoAuthKeyHex = "48e5918a74ae21c972b90cce8af6c8be"
	// videoAuthIVHex buildCKey 的 AES-128 IV。
	videoAuthIVHex = "9a7e7d23610266b1d9fbf98581384d92"
)

// 上游接口地址。做成包级变量便于测试注入（原 JS 里是内联字面量）。
var (
	playerAuthURL    = "https://player-api.yangshipin.cn/v1/player/auth"
	openTokenURL     = "https://h5access.yangshipin.cn/web/open/token"
	getLiveInfoURL   = "https://player-api.yangshipin.cn/v1/player/get_live_info"
	yangshipinOrigin = "https://www.yangshipin.cn"
	yangshipinRefer  = "https://www.yangshipin.cn/"
)

// ErrEngine web 引擎的错误基类。
var ErrEngine = errors.New("ysp-engine: web")

// ErrUnsupportedChannel 频道没有网页版元数据。
var ErrUnsupportedChannel = fmt.Errorf("%w: unsupported channel", ErrEngine)

// ErrBlockedSegmentURL /segment 的 URL 不在白名单内。
var ErrBlockedSegmentURL = fmt.Errorf("%w: segment url not allowed", ErrEngine)

// ErrSegmentTooLarge /segment 上游响应体超过 32MB。
var ErrSegmentTooLarge = fmt.Errorf("%w: segment response too large", ErrEngine)

// Config 引擎配置。
type Config struct {
	// UA 请求上游时使用的 User-Agent（为空则用 DefaultUA）。
	UA string
	// UpstreamTimeout 单次上游请求超时（为空则用 20s）。
	UpstreamTimeout time.Duration
	// SegmentProxyPath 分片改写前缀，例如 "/segment?url="。
	// 为空则用原 JS 的语义 "/segment?url="。
	SegmentProxyPath string
}

// Engine 网页版兜底引擎（对应原 JS 的整个 service）。
type Engine struct {
	cfg Config
	now func() time.Time

	runtime       wazero.Runtime
	keygenModule  wazero.CompiledModule
	ticketModule  wazero.CompiledModule
	ticketAModule wazero.CompiledModule

	// tr 是复用的 HTTP Transport（等价原 JS 的 httpsAgent/httpAgent）。
	// 不用 httpx.Client：它固定走 https 语义且无法透传 Range/自定义 Host，
	// 而 /segment 需要支持 http 分片与 Range。
	tr     *http.Transport
	trOnce sync.Once

	mu    sync.Mutex
	cache *lruCache
	// closed 标记 Close 后拒绝继续服务（修复原项目 bug:
	// JS 的 uncaughtException 只打日志继续跑，可能在半初始化状态）。
	closed bool
}

// New 创建引擎：编译并实例化两个 WASM 模块（失败返回错误，不 panic）。
func New(cfg Config) (*Engine, error) {
	if cfg.UA == "" {
		cfg.UA = DefaultUA
	}
	if cfg.UpstreamTimeout <= 0 {
		cfg.UpstreamTimeout = 20 * time.Second
	}
	if cfg.SegmentProxyPath == "" {
		cfg.SegmentProxyPath = "/segment?url="
	}
	e := &Engine{
		cfg:   cfg,
		now:   time.Now,
		cache: newLRUCache(cacheMaxEntries, cacheTTL),
	}
	ctx := context.Background()
	e.runtime = wazero.NewRuntime(ctx)

	if err := e.registerWbgModule(ctx); err != nil {
		_ = e.runtime.Close(ctx)
		return nil, fmt.Errorf("%w: 注册 keygen 宿主模块失败: %v", ErrEngine, err)
	}
	if err := e.registerTicketHostModule(ctx); err != nil {
		_ = e.runtime.Close(ctx)
		return nil, fmt.Errorf("%w: 注册 ticket 宿主模块失败: %v", ErrEngine, err)
	}

	// 编译 ticket_a.wasm（提供 a.a memory + a.b table + 39 个桩函数的转出口）。
	// 注意：这里只编译、不实例化——a 实例必须按调用新建（见 buildTicket 的说明），
	// 否则多个调用会共享同一份 memory/table。
	aCompiled, err := e.runtime.CompileModule(ctx, ticketAWasm)
	if err != nil {
		_ = e.runtime.Close(ctx)
		return nil, fmt.Errorf("%w: 编译 ticket a 模块失败: %v", ErrEngine, err)
	}
	e.ticketAModule = aCompiled
	keygenCompiled, err := e.runtime.CompileModule(ctx, keygenWasm)
	if err != nil {
		_ = e.runtime.Close(ctx)
		return nil, fmt.Errorf("%w: 编译 keygen WASM 失败: %v", ErrEngine, err)
	}
	e.keygenModule = keygenCompiled
	ticketCompiled, err := e.runtime.CompileModule(ctx, ticketWasm)
	if err != nil {
		_ = e.runtime.Close(ctx)
		return nil, fmt.Errorf("%w: 编译 ticket WASM 失败: %v", ErrEngine, err)
	}
	e.ticketModule = ticketCompiled

	// 构造阶段立即做一次自检，确保两个 WASM 真能跑起来。
	// （修复原项目 bug: JS 把模块构造放在顶层，一旦失败进程直接崩或半初始化。）
	if err := e.selfCheck(ctx); err != nil {
		_ = e.runtime.Close(ctx)
		return nil, err
	}
	return e, nil
}

// selfCheck 验证两个 WASM 都能实例化并返回非空结果。
func (e *Engine) selfCheck(ctx context.Context) error {
	rnd, err := e.getRnd(ctx, keygenValues{
		GUID: "selfcheck_0123456789abc", YspAppID: yspAppID, Version: "v1",
		Host: "www.yangshipin.cn", Protocol: "https:",
	})
	if err != nil {
		return fmt.Errorf("%w: keygen 自检失败: %v", ErrEngine, err)
	}
	if rnd == "" {
		return fmt.Errorf("%w: keygen 自检返回空 rnd", ErrEngine)
	}
	ticket, err := e.buildTicket(ctx, "600001859", "1700000000", "2024078201", "selfcheck_0123456789abc")
	if err != nil {
		return fmt.Errorf("%w: ticket 自检失败: %v", ErrEngine, err)
	}
	if len(ticket) == 0 {
		return fmt.Errorf("%w: ticket 自检返回空票据", ErrEngine)
	}
	return nil
}

// Close 释放 WASM 运行时（对应原 JS 的 server.close + exit 语义）。
func (e *Engine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	e.mu.Unlock()
	if e.runtime != nil {
		return e.runtime.Close(context.Background())
	}
	return nil
}

// Name 引擎名称（对应原 JS 的 engine: 'ysp-engine'）。
func (e *Engine) Name() string { return "web" }

// Version 原 JS 的 VERSION 常量。
const Version = "9.4.0"

// canonicalSlug 对应原 JS: ALIAS_MAP[slug] || slug。
func canonicalSlug(slug string) string {
	if alias, ok := aliasMap[slug]; ok {
		return alias
	}
	return slug
}

// Supports 该频道是否有网页版元数据（对应原 JS: CHANNELS[canonical] != null）。
func (e *Engine) Supports(channel string) bool {
	_, ok := channels[canonicalSlug(channel)]
	return ok
}

// instance 里用到的内部接口断言，保证 api.Module 与 wazero.CompiledModule 类型正确。
var _ api.Module = (api.Module)(nil)
