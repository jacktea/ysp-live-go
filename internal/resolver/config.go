// Package resolver 迁移原项目 ysp-live.py 中的 Resolver（央视频设备协议 / 4K 高码率解析层）。
//
// 覆盖内容：
//   - ResolverState / AppSession / RefreshQueue / HttpQueue 状态结构；
//   - 设备身份注册（app/start、云端注册、心跳、遥测上报、字典、index、warmup）；
//   - live/v1/01 + live/v1/02 + VDN getstream 三级取流；
//   - 频道级 SWR 缓存、失败冷却、HTTP 400 自动换设备、换台防抖；
//   - 高码率并发名额（links_per_device）与空闲释放；
//   - 分片地址改写（/proxy.ts?=BASE64）与播放头匹配；
//   - 后台刷新队列、状态快照与 proxy-cache-state-rs.json 持久化。
//
// 设计约束（见 docs/PORTING.md）：
//   - 注释用中文，并标注对应的原 Python 函数名；
//   - 协议细节（请求头、字段、签名、错误文案、日志文案）与原实现保持一致；
//   - 所有对外方法并发安全。
package resolver

import (
	"strings"

	"github.com/jacktea/ysp-live-go/internal/model"
)

// Config 是 Resolver 的配置。字段与默认值对应原项目 main() 里的 engine_args
// （见 ysp-live.py 第 3333-3345 行）。
//
// 所有以 URL 结尾的字段都用于测试注入：为空时使用 internal/model 定义的官方地址。
type Config struct {
	// Timeout 设备协议 HTTP 超时秒数（原 args.timeout，默认 10）。
	Timeout float64
	// CacheTTL 频道解析结果有效期秒数（原 args.cache_ttl，默认 60）。
	CacheTTL float64
	// StaleWhileRefreshTTL 过期后仍可使用的 SWR 窗口秒数（原 args.stale_while_refresh_ttl，默认 0）。
	StaleWhileRefreshTTL float64
	// RefreshErrorCooldown 后台刷新失败后的重试冷却秒数（原 args.refresh_error_cooldown，默认 5）。
	RefreshErrorCooldown float64
	// PlaylistCacheTTL 播放列表缓存秒数（原 args.playlist_cache_ttl，默认 2；0 表示不缓存）。
	PlaylistCacheTTL float64
	// BackgroundRefreshQueueLimit 后台刷新队列长度上限（原 args.background_refresh_queue_limit，默认 4；0 表示禁用）。
	BackgroundRefreshQueueLimit int
	// RefreshInterval 业务请求最小间隔秒数（原 args.refresh_interval，默认 1）。
	RefreshInterval float64
	// HeartbeatInterval 心跳间隔秒数（原 args.heartbeat_interval，默认 30；实现上不小于 1 秒）。
	HeartbeatInterval float64
	// HeartbeatTTLGuard 会话 TTL 剩余低于该值时提前重建（原 args.heartbeat_ttl_guard，默认 60）。
	HeartbeatTTLGuard float64
	// SessionTTL 设备会话有效期秒数（原 args.session_ttl，默认 7200；<=0 表示永不过期）。
	SessionTTL float64
	// LinksPerDevice 单设备高码率并发名额（原 args.links_per_device，默认 6；0 表示不限制）。
	LinksPerDevice int
	// ZapDebounce 换台防抖秒数（原 args.zap_debounce，默认 10；<=0 关闭防抖）。
	ZapDebounce float64
	// Only4K 仅 4K/8K 频道走设备协议（原 only_4k / YSP_ONLY_4K_DEVICE）。
	Only4K bool
	// MetaJSON 状态持久化文件（proxy-cache-state-rs.json）。
	MetaJSON string
	// DeviceJSON 设备身份持久化文件（device-state-rs.json）。
	DeviceJSON string
	// HTTPWorkers 并发 HTTP worker 数（原 args.http_workers，仅用于状态展示）。
	HTTPWorkers int
	// HTTPQueueLimit HTTP 队列长度上限（原 args.http_queue_limit，仅用于状态展示）。
	HTTPQueueLimit int
	// IdentityResetErrorThreshold 身份重置错误阈值（原 args.identity_reset_error_threshold，默认 0 关闭）。
	IdentityResetErrorThreshold int
	// IdentityResetCooldown 身份重置冷却秒数（原 args.identity_reset_cooldown，默认 300）。
	IdentityResetCooldown float64
	// ControlStepJitterMinMs / ControlStepJitterMaxMs 控制面请求随机抖动毫秒（原 args.control_step_jitter_*，默认 0）。
	ControlStepJitterMinMs int
	ControlStepJitterMaxMs int

	// LinkIdleTimeout 高码率名额空闲释放秒数（原常量 LINK_IDLE_TIMEOUT，默认 15）。
	LinkIdleTimeout float64
	// ServerPort 未传 Host 头时生成分片代理地址使用的端口（原全局 server_port，默认 8767）。
	ServerPort int

	// InitMaxAttempts Init 最多尝试次数（原 init_resolver 是无限重试；Go 版由调用方控制，默认 3）。
	InitMaxAttempts int
	// InitBackoff 首次重试等待秒数（原 15s，指数增长上限 60s）。
	InitBackoff float64
	// InitBackoffMax 重试等待上限秒数（原 60s）。
	InitBackoffMax float64

	// 以下为接口地址注入（测试用；为空使用 model 中的官方地址）。
	AppStartURL      string
	CloudGetURL      string
	CloudRegisterURL string
	CollectReportURL string
	DictionaryURL    string
	IndexURL         string
	ReportSingleURL  string
	LiveV101URL      string
	LiveV102URL      string
	VDNGetStreamURL  string
	DRMConfigURL     string
	VersionConfigURL string
}

// DefaultConfig 返回与原项目 engine_args 一致的默认配置。
func DefaultConfig() Config {
	return Config{
		Timeout:                     10.0,
		CacheTTL:                    60.0,
		StaleWhileRefreshTTL:        0.0,
		RefreshErrorCooldown:        5.0,
		PlaylistCacheTTL:            2.0,
		BackgroundRefreshQueueLimit: 4,
		RefreshInterval:             1.0,
		HeartbeatInterval:           30.0,
		HeartbeatTTLGuard:           60.0,
		SessionTTL:                  7200.0,
		LinksPerDevice:              6,
		ZapDebounce:                 10.0,
		MetaJSON:                    "proxy-cache-state-rs.json",
		DeviceJSON:                  "device-state-rs.json",
		HTTPWorkers:                 16,
		HTTPQueueLimit:              1000,
		IdentityResetErrorThreshold: 0,
		IdentityResetCooldown:       300.0,
		LinkIdleTimeout:             15.0,
		ServerPort:                  8767,
		InitMaxAttempts:             3,
		InitBackoff:                 15.0,
		InitBackoffMax:              60.0,
	}
}

// normalize 补齐零值字段，保证 New(Config{}) 也能安全运行。
func (c *Config) normalize() {
	if c.Timeout < 0.1 {
		c.Timeout = 10.0
	}
	if c.CacheTTL <= 0 {
		c.CacheTTL = 60.0
	}
	if c.StaleWhileRefreshTTL < 0 {
		c.StaleWhileRefreshTTL = 0
	}
	if c.PlaylistCacheTTL < 0 {
		c.PlaylistCacheTTL = 0
	}
	if c.BackgroundRefreshQueueLimit < 0 {
		c.BackgroundRefreshQueueLimit = 0
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = 30.0
	}
	if c.HeartbeatTTLGuard <= 0 {
		c.HeartbeatTTLGuard = 60.0
	}
	if c.LinksPerDevice < 0 {
		c.LinksPerDevice = 0
	}
	if c.MetaJSON == "" {
		c.MetaJSON = "proxy-cache-state-rs.json"
	}
	if c.DeviceJSON == "" {
		c.DeviceJSON = "device-state-rs.json"
	}
	if c.HTTPWorkers <= 0 {
		c.HTTPWorkers = 16
	}
	if c.HTTPQueueLimit <= 0 {
		c.HTTPQueueLimit = 1000
	}
	if c.IdentityResetCooldown <= 0 {
		c.IdentityResetCooldown = 300.0
	}
	if c.LinkIdleTimeout <= 0 {
		c.LinkIdleTimeout = 15.0
	}
	if c.ServerPort <= 0 {
		c.ServerPort = 8767
	}
	if c.InitMaxAttempts <= 0 {
		c.InitMaxAttempts = 3
	}
	if c.InitBackoff <= 0 {
		c.InitBackoff = 15.0
	}
	if c.InitBackoffMax <= 0 {
		c.InitBackoffMax = 60.0
	}
}

// endpoints 实际使用的接口地址（Config 为空时回落官方地址）。
type endpoints struct {
	appStart      string
	cloudGet      string
	cloudRegister string
	collectReport string
	dictionary    string
	index         string
	reportSingle  string
	liveV101      string
	liveV102      string
	vdnGetStream  string
	drmConfig     string
	versionConfig string
}

func (c *Config) endpoints() endpoints {
	return endpoints{
		appStart:      orDefault(c.AppStartURL, model.AppStartURL),
		cloudGet:      orDefault(c.CloudGetURL, model.CloudGetURL),
		cloudRegister: orDefault(c.CloudRegisterURL, model.CloudRegisterURL),
		collectReport: orDefault(c.CollectReportURL, model.CollectReportURL),
		dictionary:    orDefault(c.DictionaryURL, model.DictionaryURL),
		index:         orDefault(c.IndexURL, model.IndexURL),
		reportSingle:  orDefault(c.ReportSingleURL, model.ReportSingleURL),
		liveV101:      orDefault(c.LiveV101URL, model.LiveV101URL),
		liveV102:      orDefault(c.LiveV102URL, model.LiveV102URL),
		vdnGetStream:  orDefault(c.VDNGetStreamURL, model.VDNGetStreamURL),
		drmConfig:     orDefault(c.DRMConfigURL, model.DRMConfigURL),
		versionConfig: orDefault(c.VersionConfigURL, model.VersionConfigURL),
	}
}

func orDefault(value, def string) string {
	if strings.TrimSpace(value) == "" {
		return def
	}
	return value
}
