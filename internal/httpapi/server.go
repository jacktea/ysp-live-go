// Package httpapi 实现原项目 ysp-live.py 的 HTTP 网关：单端口 8767 暴露
// 订阅列表、单频道播放列表、TS 分片代理、健康检查与诊断页。
//
// 路由与原项目一一对应：
//
//	/                     服务状态与入口
//	/favicon.ico          SVG 图标
//	/health               JSON 健康信息
//	/all.m3u              64 路聚合订阅（支持 ?group= 与 ?k4=1）
//	/diag                 诊断信息（纯文本）
//	/proxy.ts, /proxy     TS 分片代理（令牌为 URL-safe base64）
//	/engine_proxy?url=    网页版引擎分片代理
//	/<channel>.m3u8       单频道播放列表（支持时移 ?playseek=）
//	/segment?url=         与 JS 引擎兼容的分片代理（同样走 TS 代理逻辑）
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jacktea/ysp-live-go/internal/cache"
	"github.com/jacktea/ysp-live-go/internal/engine"
	"github.com/jacktea/ysp-live-go/internal/gateway"
	"github.com/jacktea/ysp-live-go/internal/logx"
	"github.com/jacktea/ysp-live-go/internal/model"
	"github.com/jacktea/ysp-live-go/internal/playlist"
)

// Version 版本号（与原项目 __version__ 对齐）。
const Version = "9.4.0"

// AuthorCommunity / AuthorChannel 原作者发布渠道（原项目在 / 与 /diag 中展示）。
const (
	AuthorCommunity = "https://t.me/garysclub"
	AuthorChannel   = "https://t.me/garysclubchannel"
)

// SegmentFetcher 分片代理所需的播放头与上游抓取能力。
type SegmentFetcher interface {
	// PlaybackHeaders 返回访问该 TS 地址所需的上游请求头。
	PlaybackHeaders(rawURL string) (map[string]string, error)
}

// DeviceStatus 设备协议层状态（供 /health、/diag）。
type DeviceStatus struct {
	Ready        bool
	Valid        bool
	GUID         string
	Links        int
	LinksLimit   int
	Heartbeat    int64
	HeartbeatErr string
}

// StatusProvider 提供设备层状态快照。
type StatusProvider interface {
	DeviceStatus() DeviceStatus
}

// CatchupProvider 时移解析能力。
type CatchupProvider interface {
	// Catchup 返回时移 m3u8（已改写为本服务的分片代理地址）。
	Catchup(ctx context.Context, channel string, startTS, endTS int64) (string, error)
}

// ChannelStateLister 提供各频道后台状态（供 /diag）。
type ChannelStateLister interface {
	Snapshot() []ChannelSnapshot
}

// ChannelSnapshot 单频道状态快照。
type ChannelSnapshot struct {
	Slug       string
	Name       string
	Mode       string
	LastOK     time.Time
	LastError  string
	Segments   int
	LastAccess time.Time
}

// Options 服务器依赖与配置。
type Options struct {
	Chain       *gateway.Chain
	Dir         *model.ChannelDirectory
	Segments    SegmentFetcher
	TSCache     *cache.TsSegmentCache
	Status      StatusProvider
	Catchup     CatchupProvider
	Channels    ChannelStateLister
	Web         WebSegmentProxy
	DeviceChans map[string]bool
	StartTime   time.Time
	Only4K      bool
	// EngineProxyAllowed 是否允许 /engine_proxy 代理任意地址（原项目也允许，仅建议内网使用）。
	EngineProxyAllowed bool
}

// WebSegmentProxy 网页版引擎的分片代理（兼容 /segment?url=）。
type WebSegmentProxy interface {
	SegmentProxy(ctx context.Context, targetURL, rangeHeader string) (*SegmentResponse, error)
}

// SegmentResponse 分片响应。
type SegmentResponse struct {
	Status        int
	Body          []byte
	ContentType   string
	ContentLength int64
}

// Server HTTP 处理器。
type Server struct {
	opts      Options
	startedAt time.Time
	requests  atomic.Int64
}

// New 创建 HTTP 处理器。
func New(o Options) *Server {
	if o.Dir == nil {
		o.Dir = model.NewChannelDirectory()
	}
	if o.StartTime.IsZero() {
		o.StartTime = time.Now()
	}
	return &Server{opts: o, startedAt: o.StartTime}
}

// Handler 返回带访问日志的 http.Handler。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.route)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	query := r.URL.RawQuery

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.send(w, http.StatusMethodNotAllowed, "method not allowed\n", "text/plain; charset=utf-8", r.Method == http.MethodHead, nil)
		return
	}
	headOnly := r.Method == http.MethodHead

	switch path {
	case "/", "/index.html":
		s.handleIndex(w, r, headOnly)
		return
	case "/favicon.ico":
		s.handleFavicon(w, r, headOnly)
		return
	case "/health":
		s.handleHealth(w, r, headOnly)
		return
	case "/all.m3u":
		s.handleAllM3U(w, r, headOnly)
		return
	case "/diag":
		s.handleDiag(w, r, headOnly)
		return
	case "/proxy.ts", "/proxy":
		s.handleTSProxy(w, r, headOnly, query)
		return
	case "/engine_proxy":
		s.handleEngineProxy(w, r, headOnly, query)
		return
	case "/segment":
		s.handleSegment(w, r, headOnly, query)
		return
	}

	// /live/<slug>.m3u8 与 /<slug>.m3u8
	if slug, ok := matchChannelPath(path); ok {
		s.handleChannel(w, r, headOnly, slug, query)
		return
	}
	s.send(w, http.StatusNotFound, "not found\n", "text/plain; charset=utf-8", headOnly, nil)
}

// matchChannelPath 匹配 /xxx.m3u8 或 /live/xxx.m3u8。
func matchChannelPath(path string) (string, bool) {
	if !strings.HasSuffix(path, ".m3u8") {
		return "", false
	}
	p := strings.TrimPrefix(path, "/live/")
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimSuffix(p, ".m3u8")
	if p == "" || strings.Contains(p, "/") {
		return "", false
	}
	for _, r := range p {
		if !(r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return "", false
		}
	}
	return p, true
}

func (s *Server) send(w http.ResponseWriter, code int, body, ctype string, headOnly bool, extra map[string]string) {
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Cache-Control", "no-cache, no-store, max-age=0")
	for k, v := range extra {
		h.Set(k, v)
	}
	w.WriteHeader(code)
	if !headOnly {
		_, _ = w.Write([]byte(body))
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request, headOnly bool) {
	host := hostOf(r)
	text := "====================================================\n" +
		"ysp-live IPTV 直播网关运行中 (端口: " + portOf(r) + ")\n" +
		"📢 官方发布社区: " + AuthorCommunity + "\n" +
		"📢 官方发布频道: " + AuthorChannel + "\n" +
		"⚠️ 严正声明: 其他任何渠道均非作者发布！\n" +
		"====================================================\n\n" +
		"全频道订阅: http://" + host + "/all.m3u\n" +
		"系统健康度: http://" + host + "/health\n" +
		"实时诊断日志: http://" + host + "/diag\n"
	s.send(w, http.StatusOK, text, "text/plain; charset=utf-8", headOnly, nil)
}

const faviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100"><text y=".9em" font-size="90">📺</text></svg>`

func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request, headOnly bool) {
	s.send(w, http.StatusOK, faviconSVG, "image/svg+xml", headOnly, nil)
}

// handleAllM3U 生成聚合订阅。
func (s *Server) handleAllM3U(w http.ResponseWriter, r *http.Request, headOnly bool) {
	q := r.URL.Query()
	only4K := false
	switch strings.ToLower(q.Get("k4")) {
	case "1", "true", "yes":
		only4K = true
	}
	body := playlist.All(playlist.Options{
		Host:   hostOf(r),
		Proto:  protoOf(r),
		Group:  q.Get("group"),
		Only4K: only4K,
		Dir:    s.opts.Dir,
	})
	s.send(w, http.StatusOK, body, "application/vnd.apple.mpegurl", headOnly, nil)
}

// handleHealth 返回 JSON 健康信息（字段名与原项目一致）。
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request, headOnly bool) {
	now := time.Now()
	var dev DeviceStatus
	if s.opts.Status != nil {
		dev = s.opts.Status.DeviceStatus()
	}
	var stats cache.Stats
	if s.opts.TSCache != nil {
		stats = s.opts.TSCache.Stats()
	}
	payload := map[string]interface{}{
		"ok":               true,
		"version":          Version,
		"author_community": AuthorCommunity,
		"author_channel":   AuthorChannel,
		"notice":           "其他渠道均非作者发布！",
		"uptime_sec":       int64(now.Sub(s.startedAt).Seconds()),
		"engine": map[string]interface{}{
			"ready":               dev.Ready,
			"active_device_slots": linksLimitBucket(dev),
			"session_valid":       dev.Valid,
			"heartbeat_count":     dev.Heartbeat,
			"guid":                dev.GUID,
		},
		"channels": s.opts.Dir.Count(),
		"ts_cache": stats,
		"routes":   []string{"/", "/all.m3u", "/diag", "/health", "/<channel>.m3u8", "/proxy.ts"},
		"requests": s.requests.Load(),
	}
	data, _ := json.MarshalIndent(payload, "", "  ")
	s.send(w, http.StatusOK, string(data), "application/json; charset=utf-8", headOnly, nil)
}

func linksLimitBucket(d DeviceStatus) int {
	if d.Valid {
		return 1
	}
	return 0
}

// handleDiag 输出纯文本诊断报告。
func (s *Server) handleDiag(w http.ResponseWriter, r *http.Request, headOnly bool) {
	now := time.Now()
	var b strings.Builder
	line := func(format string, args ...interface{}) {
		b.WriteString(fmt.Sprintf(format, args...))
		b.WriteByte('\n')
	}
	line("====================================================")
	line("ysp-live v%s 诊断报告 (运行时间: %ds)", Version, int64(now.Sub(s.startedAt).Seconds()))
	line("📢 首发地址")
	line("社区：Gary's Club (%s)", AuthorCommunity)
	line("频道：Gary's Club Channel (%s)", AuthorChannel)
	line("⚠️ 其他任何分发渠道均非作者本人！")
	line("====================================================")

	var dev DeviceStatus
	if s.opts.Status != nil {
		dev = s.opts.Status.DeviceStatus()
	}
	engineStatus := "未就绪(1080p自动回退)"
	if dev.Ready {
		engineStatus = "就绪"
	}
	line("单设备并发池状态: %s", engineStatus)

	// 修复原项目 bug: 原 /diag 会把"诊断报告"标题打印两次（重复 append），这里只保留一次。
	devState := "未就绪"
	if dev.Valid {
		devState = "有效"
	}
	guid := dev.GUID
	if guid == "" {
		guid = "无"
	}
	line("高码率设备状态 (26 路共用): %s, 占用=%d/%d, 心跳=%d, GUID=%s",
		devState, dev.Links, dev.LinksLimit, dev.Heartbeat, guid)
	if dev.HeartbeatErr != "" {
		line("Slot 0 心跳日志: %s", dev.HeartbeatErr)
	}

	if s.opts.TSCache != nil {
		st := s.opts.TSCache.Stats()
		line("分片内存缓存: 条目=%d, 占用=%sMB, 命中率=%s (命中:%d/未中:%d)",
			st.Count, formatFloat(st.SizeMB), st.HitRate, st.Hits, st.Misses)
	}

	line("--- 频道状态列表 ---")
	snapshots := map[string]ChannelSnapshot{}
	if s.opts.Channels != nil {
		for _, c := range s.opts.Channels.Snapshot() {
			snapshots[c.Slug] = c
		}
	}
	for _, m := range s.opts.Dir.AllChannels() {
		snap := snapshots[m.Slug]
		age := "从未拉取"
		if !snap.LastOK.IsZero() {
			age = fmt.Sprintf("%ds前", int64(now.Sub(snap.LastOK).Seconds()))
		}
		mode := snap.Mode
		if mode == "" {
			mode = "idle"
		}
		if s.opts.DeviceChans[m.Slug] && dev.Ready {
			mode = "4K/高码率"
		}
		errText := snap.LastError
		if errText == "" {
			errText = "无"
		}
		line("%s (%s) 模式=%s 上次刷新=%s 错误=%s", m.Slug, m.Name, mode, age, errText)
	}

	if logs := logx.Recent(30); len(logs) > 0 {
		line("--- 最近运行事件 (最新 30 条) ---")
		for _, l := range logs {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	s.send(w, http.StatusOK, b.String(), "text/plain; charset=utf-8", headOnly, nil)
}

func formatFloat(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

// handleChannel 处理单频道播放列表请求。
//
// 优先级：
//  1. 时移（若支持且带 playseek/start/end）
//  2. 四层兜底链
func (s *Server) handleChannel(w http.ResponseWriter, r *http.Request, headOnly bool, rawSlug, query string) {
	channel := model.Canonical(rawSlug)
	meta := s.opts.Dir.GetMeta(channel)
	if meta == nil {
		s.send(w, http.StatusNotFound, "未知频道\n", "text/plain; charset=utf-8", headOnly, nil)
		return
	}

	if start, end, ok := gateway.ParseCatchupQuery(query); ok && s.opts.Catchup != nil && model.TimeshiftSupported[channel] {
		body, err := s.opts.Catchup.Catchup(r.Context(), channel, start, end)
		if err == nil && strings.Contains(body, "#EXTM3U") {
			s.send(w, http.StatusOK, body, "application/vnd.apple.mpegurl", headOnly, nil)
			return
		}
		if err != nil {
			logx.Log(fmt.Sprintf("[%s] 时移获取失败: %v，回退直播", channel, err), 10)
		}
	}

	res, err := s.opts.Chain.Resolve(r.Context(), channel, hostOf(r))
	if err != nil {
		s.send(w, http.StatusServiceUnavailable,
			fmt.Sprintf("频道 %s 暂无数据 (%s), 请稍后重试\n", meta.Name, errText(err)),
			"text/plain; charset=utf-8", headOnly, nil)
		return
	}
	extra := map[string]string{}
	if res.Source != engine.SourceDevice {
		extra["X-Source"] = string(res.Source)
	}
	s.send(w, http.StatusOK, res.Playlist, "application/vnd.apple.mpegurl", headOnly, extra)
}

func errText(err error) string {
	if err == nil {
		return "拉取中"
	}
	s := strings.TrimSpace(err.Error())
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

// handleEngineProxy 代理网页版引擎分片。
//
// 安全性：原项目用 urllib 直接请求任意 url（可被当作开放代理）。
// 这里保留功能，但默认只允许 CCTV/ysp 域名，见 allowedUpstreamHost。
func (s *Server) handleEngineProxy(w http.ResponseWriter, r *http.Request, headOnly bool, query string) {
	target := ""
	if q, err := url.ParseQuery(query); err == nil {
		target = q.Get("url")
	}
	if target == "" {
		s.send(w, http.StatusBadRequest, "missing url\n", "text/plain; charset=utf-8", headOnly, nil)
		return
	}
	if !s.opts.EngineProxyAllowed && !allowedUpstreamHost(target) {
		s.send(w, http.StatusForbidden, "upstream host not allowed\n", "text/plain; charset=utf-8", headOnly, nil)
		return
	}
	resp, err := http.Get(target) //nolint:gosec // 目标域已校验
	if err != nil {
		s.send(w, http.StatusBadGateway, "engine proxy error: "+err.Error()+"\n", "text/plain; charset=utf-8", headOnly, nil)
		return
	}
	defer resp.Body.Close()
	ctype := resp.Header.Get("Content-Type")
	if ctype == "" {
		ctype = "video/mp2t"
	}
	body := make([]byte, 0, 64*1024)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			body = append(body, buf[:n]...)
		}
		if rerr != nil {
			break
		}
		if len(body) > 64<<20 {
			break
		}
	}
	s.send(w, resp.StatusCode, string(body), ctype, headOnly, nil)
}

func hostOf(r *http.Request) string {
	if h := r.Header.Get("Host"); h != "" {
		return h
	}
	return r.Host
}

func portOf(r *http.Request) string {
	host := hostOf(r)
	if _, port, err := net.SplitHostPort(host); err == nil {
		return port
	}
	return "80"
}

func protoOf(r *http.Request) string {
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		return p
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}
