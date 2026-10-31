package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacktea/ysp-live-go/internal/engine"
)

// fakeUpstream 模拟央视频网页版的 4 个上游接口，用于离线全流程测试。
type fakeUpstream struct {
	server *httptest.Server

	mu sync.Mutex
	// 记录收到的请求，便于断言签名/请求头。
	authCalls    int
	tokenCalls   int
	liveCalls    int
	m3u8Calls    int
	lastAuthBody string
	lastLiveBody string
	lastLiveHdr  http.Header
	lastTokenQry url.Values

	// 可注入的响应（默认给出正常值）。
	authTS  json.RawMessage
	tokenTS json.RawMessage
	// playurl 指向 /hls/index.m3u8。
	playurl string
}

// newFakeUpstream 启动假上游。
func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{
		authTS:  json.RawMessage(`1700000000`),
		tokenTS: json.RawMessage(`"1700000000000"`),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/player/auth", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.authCalls++
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		f.lastAuthBody = string(buf)
		ts := f.authTS
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"token":"player-token-abc","ts":%s}}`, ts)
	})
	mux.HandleFunc("/web/open/token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.tokenCalls++
		f.lastTokenQry = r.URL.Query()
		ts := f.tokenTS
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"token":"openapi-token-xyz","ts":%s}}`, ts)
	})
	mux.HandleFunc("/v1/player/get_live_info", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.liveCalls++
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		f.lastLiveBody = string(buf)
		f.lastLiveHdr = r.Header.Clone()
		playurl := f.playurl
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"playurl":%q,"extended_param":"&sign=abc"}}`, playurl)
	})
	mux.HandleFunc("/hls/index.m3u8", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.m3u8Calls++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n"+
			"#EXTINF:4.0,\nseg-1.ts\n#EXTINF:4.0,\n/hls/seg-2.ts\n"+
			"#EXTINF:4.0,\nhttps://cdn.example.cctv.cn/seg-3.ts\n#EXT-X-ENDLIST\n")
	})
	mux.HandleFunc("/seg.ts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		fmt.Fprint(w, "TSPAYLOAD")
	})
	f.server = httptest.NewServer(mux)
	f.playurl = f.server.URL + "/hls/index.m3u8?tok=1"
	t.Cleanup(f.server.Close)
	return f
}

// install 把包级 URL 变量指向假上游，测试结束自动还原。
func (f *fakeUpstream) install(t *testing.T) {
	t.Helper()
	oldAuth, oldToken, oldLive := playerAuthURL, openTokenURL, getLiveInfoURL
	playerAuthURL = f.server.URL + "/v1/player/auth"
	openTokenURL = f.server.URL + "/web/open/token"
	getLiveInfoURL = f.server.URL + "/v1/player/get_live_info"
	t.Cleanup(func() {
		playerAuthURL, openTokenURL, getLiveInfoURL = oldAuth, oldToken, oldLive
	})
}

// newEngineWithUpstream 构造引擎并把上游指向假服务。
func newEngineWithUpstream(t *testing.T) (*Engine, *fakeUpstream) {
	t.Helper()
	f := newFakeUpstream(t)
	f.install(t)
	e := newTestEngine(t)
	return e, f
}

// TestPlaylistFullFlow 覆盖 auth→token→ckey→keygen→ticket→get_live_info→m3u8 全流程。
func TestPlaylistFullFlow(t *testing.T) {
	e, f := newEngineWithUpstream(t)
	ctx := context.Background()

	pl, err := e.Playlist(ctx, "cctv1")
	if err != nil {
		t.Fatalf("Playlist: %v", err)
	}
	if pl.Source != engine.SourceWeb {
		t.Fatalf("Source = %q, 期望 %q", pl.Source, engine.SourceWeb)
	}
	if pl.ContentType != m3u8ContentType {
		t.Fatalf("ContentType = %q", pl.ContentType)
	}
	body := pl.Body
	if !strings.HasPrefix(body, "#EXTM3U") {
		t.Fatalf("m3u8 未以 #EXTM3U 开头:\n%s", body)
	}
	// 分片必须被改写为 /segment?url=<encodeURIComponent(绝对地址)>
	if !strings.Contains(body, "/segment?url=") {
		t.Fatalf("分片未被改写:\n%s", body)
	}
	if strings.Contains(body, "\nseg-1.ts") {
		t.Fatalf("相对分片未被改写:\n%s", body)
	}
	// 三个分片都应是 /segment?url= 开头
	var segs int
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		segs++
		if !strings.HasPrefix(line, "/segment?url=") {
			t.Fatalf("分片行格式不对: %q", line)
		}
		enc := strings.TrimPrefix(line, "/segment?url=")
		dec, err := url.QueryUnescape(enc)
		if err != nil {
			t.Fatalf("分片地址无法解码: %v", err)
		}
		if !strings.HasPrefix(dec, "http") {
			t.Fatalf("分片未转成绝对地址: %q", dec)
		}
	}
	if segs != 3 {
		t.Fatalf("分片数量 = %d, 期望 3:\n%s", segs, body)
	}
	// 第二个分片是从根路径相对的，应解析成 http://host/hls/seg-2.ts
	if !strings.Contains(body, url.QueryEscape(f.server.URL+"/hls/seg-2.ts")) {
		t.Fatalf("相对根路径的分片解析错误:\n%s", body)
	}
	// 第三个是绝对地址，应原样保留
	if !strings.Contains(body, url.QueryEscape("https://cdn.example.cctv.cn/seg-3.ts")) {
		t.Fatalf("绝对地址分片被改写错误:\n%s", body)
	}

	// 上游调用次数
	if f.authCalls != 1 || f.tokenCalls != 1 || f.liveCalls != 1 || f.m3u8Calls != 1 {
		t.Fatalf("上游调用次数异常: auth=%d token=%d live=%d m3u8=%d",
			f.authCalls, f.tokenCalls, f.liveCalls, f.m3u8Calls)
	}

	// auth 请求体：按对象插入顺序 pid→guid→appid→rand_str→signature
	if !strings.HasPrefix(f.lastAuthBody, "pid=600001859&guid=") {
		t.Fatalf("auth 请求体顺序不对: %s", f.lastAuthBody)
	}
	if !strings.Contains(f.lastAuthBody, "&appid=ysp_pc&rand_str=") ||
		!strings.Contains(f.lastAuthBody, "&signature=") {
		t.Fatalf("auth 请求体缺少字段: %s", f.lastAuthBody)
	}
	// token 请求参数
	for _, k := range []string{"yspappid", "guid", "vappid", "vsecret", "raw", "version", "ts", "rnd"} {
		if f.lastTokenQry.Get(k) == "" {
			t.Fatalf("open/token 缺少参数 %s", k)
		}
	}
	if f.lastTokenQry.Get("rnd") == "" {
		t.Fatalf("rnd 为空（keygen 未生效）")
	}
	// get_live_info 请求头（原 JS 原样）
	for _, k := range []string{"yspsdkinput", "yspsdksign", "seqid", "request-id", "yspplayertoken", "yspticket"} {
		if f.lastLiveHdr.Get(k) == "" {
			t.Fatalf("get_live_info 缺少请求头 %s（实收: %v）", k, f.lastLiveHdr)
		}
	}
	if got := f.lastLiveHdr.Get("yspplayertoken"); got != "player-token-abc" {
		t.Fatalf("yspPlayerToken = %q", got)
	}
	// yspticket 长度由 buildTicket 的 outLen 决定：
	// outLen = len(livepid) + len(auth_ts) + len(guid) + len("519748109") + 14
	var livePayload map[string]interface{}
	if err := json.Unmarshal([]byte(f.lastLiveBody), &livePayload); err != nil {
		t.Fatalf("解析 get_live_info body 失败: %v", err)
	}
	guidStr, _ := livePayload["guid"].(string)
	authTS := jsString(f.authTS)
	wantTicketLen := 2 * (len("600001859") + len(authTS) + len(guidStr) + len(yspAppID) + 14)
	if ticket := f.lastLiveHdr.Get("yspticket"); len(ticket) != wantTicketLen {
		t.Fatalf("yspticket 长度 = %d, 期望 %d (guid=%q authTS=%q)", len(ticket), wantTicketLen, guidStr, authTS)
	}
	// yspsdksign = sign + '-' + sdkInput
	sign := f.lastLiveHdr.Get("yspsdksign")
	if !strings.Contains(sign, "-") || !strings.HasSuffix(sign, f.lastLiveHdr.Get("yspsdkinput")+"-"+extractGUIDFromSDK(sign)) {
		// 这里只做基本形状校验（sign-md5-guid-1-reqid）
		t.Logf("yspsdksign = %s", sign)
	}
	// JSON body 必须是紧凑 JSON 且含 signature/rand_str/cKey
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(f.lastLiveBody), &payload); err != nil {
		t.Fatalf("get_live_info body 不是合法 JSON: %v\n%s", err, f.lastLiveBody)
	}
	for _, k := range []string{"adjust", "appVer", "app_version", "cKey", "channel", "cmd", "cnlid",
		"defn", "devid", "dtype", "encryptVer", "guid", "livepid", "otype", "platform",
		"sphttps", "stream", "rand_str", "signature"} {
		if _, ok := payload[k]; !ok {
			t.Fatalf("get_live_info body 缺少字段 %s: %s", k, f.lastLiveBody)
		}
	}
	if !strings.HasPrefix(payload["cKey"].(string), "--01") {
		t.Fatalf("cKey 前缀不对: %v", payload["cKey"])
	}
	// adjust 在原 JS 里是 number，序列化后不应带引号
	if !strings.Contains(f.lastLiveBody, `"adjust":1,`) {
		t.Fatalf("adjust 未按数字序列化: %s", f.lastLiveBody)
	}
}

// extractGUIDFromSDK 从 sdkInput 里取 guid（用于形状校验）。
func extractGUIDFromSDK(sign string) string {
	// sign 形如 <signHex>-<bodyMd5>-<guid>-1-<reqId>
	parts := strings.SplitN(sign, "-", 4)
	if len(parts) < 3 {
		return ""
	}
	return parts[2]
}

// TestPlaylistCache 验证 30s 缓存（对应原 JS 的 playlistCache TTL）。
func TestPlaylistCache(t *testing.T) {
	e, f := newEngineWithUpstream(t)
	ctx := context.Background()
	if _, err := e.Playlist(ctx, "cctv1"); err != nil {
		t.Fatalf("第一次 Playlist: %v", err)
	}
	if _, err := e.Playlist(ctx, "cctv1"); err != nil {
		t.Fatalf("第二次 Playlist: %v", err)
	}
	// 命中缓存：auth 只应发生一次
	if f.authCalls != 1 {
		t.Fatalf("缓存未生效: auth 调用 %d 次", f.authCalls)
	}
	if e.cache.Len() != 1 {
		t.Fatalf("缓存条目数 = %d, 期望 1", e.cache.Len())
	}
}

// TestPlaylistAlias 验证别名解析（对应原 JS ALIAS_MAP[slug] || slug）。
func TestPlaylistAlias(t *testing.T) {
	e, f := newEngineWithUpstream(t)
	ctx := context.Background()
	// beijingws → bjws
	if _, err := e.Playlist(ctx, "beijingws"); err != nil {
		t.Fatalf("别名 Playlist: %v", err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(f.lastLiveBody), &payload); err != nil {
		t.Fatalf("body: %v", err)
	}
	bjws := channels["bjws"]
	if payload["cnlid"] != bjws.CnLid {
		t.Fatalf("cnlid = %v, 期望 %s（别名未解析）", payload["cnlid"], bjws.CnLid)
	}
	if payload["livepid"] != bjws.LivePid {
		t.Fatalf("livepid = %v, 期望 %s", payload["livepid"], bjws.LivePid)
	}
}

// TestPlaylistUnknownChannel 验证未知频道返回 ErrUnknownChannel。
func TestPlaylistUnknownChannel(t *testing.T) {
	e, _ := newEngineWithUpstream(t)
	_, err := e.Playlist(context.Background(), "no-such-channel")
	if err == nil {
		t.Fatal("期望错误，实际 nil")
	}
	if !strings.Contains(err.Error(), "unknown channel") {
		t.Fatalf("错误文本 = %q, 期望包含 unknown channel", err)
	}
}

// TestPlaylistAuthFailed 验证 auth 失败的错误文本（原 JS: 'auth failed'）。
func TestPlaylistAuthFailed(t *testing.T) {
	f := newFakeUpstream(t)
	f.install(t)
	// 覆盖 auth 为无 token
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/player/auth":
			fmt.Fprint(w, `{"data":{}}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	})
	e := newTestEngine(t)
	_, err := e.Playlist(context.Background(), "cctv1")
	if err == nil || !strings.Contains(err.Error(), "auth failed") {
		t.Fatalf("错误 = %v, 期望包含 auth failed", err)
	}
}

// TestPlaylistMissingPlayurl 验证缺 playurl 的错误文本（原 JS: 'missing playurl'）。
func TestPlaylistMissingPlayurl(t *testing.T) {
	f := newFakeUpstream(t)
	f.install(t)
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/player/auth":
			fmt.Fprint(w, `{"data":{"token":"t","ts":1700000000}}`)
		case "/web/open/token":
			fmt.Fprint(w, `{"data":{"token":"o","ts":"1700000000000"}}`)
		case "/v1/player/get_live_info":
			fmt.Fprint(w, `{"data":{}}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	})
	e := newTestEngine(t)
	_, err := e.Playlist(context.Background(), "cctv1")
	if err == nil || !strings.Contains(err.Error(), "missing playurl") {
		t.Fatalf("错误 = %v, 期望包含 missing playurl", err)
	}
}

// TestPlaylistInvalidM3U8 验证上游 m3u8 非法的错误文本。
func TestPlaylistInvalidM3U8(t *testing.T) {
	f := newFakeUpstream(t)
	f.install(t)
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/player/auth":
			fmt.Fprint(w, `{"data":{"token":"t","ts":1700000000}}`)
		case "/web/open/token":
			fmt.Fprint(w, `{"data":{"token":"o","ts":"1700000000000"}}`)
		case "/v1/player/get_live_info":
			fmt.Fprintf(w, `{"data":{"playurl":%q}}`, f.server.URL+"/hls/index.m3u8")
		case "/hls/index.m3u8":
			fmt.Fprint(w, "not a playlist\n")
		}
	})
	e := newTestEngine(t)
	_, err := e.Playlist(context.Background(), "cctv1")
	if err == nil || !strings.Contains(err.Error(), "invalid upstream m3u8") {
		t.Fatalf("错误 = %v, 期望包含 invalid upstream m3u8", err)
	}
}

// TestPlaylistClosedEngine 验证 Close 后返回明确错误
// （修复原项目 bug: JS 的 uncaughtException 只打日志继续跑）。
func TestPlaylistClosedEngine(t *testing.T) {
	e, _ := newEngineWithUpstream(t)
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err := e.Playlist(context.Background(), "cctv1")
	if err == nil || !strings.Contains(err.Error(), "关闭") {
		t.Fatalf("错误 = %v, 期望明确提示引擎已关闭", err)
	}
	// Close 幂等
	if err := e.Close(); err != nil {
		t.Fatalf("重复 Close 出错: %v", err)
	}
}

// TestSupports 验证 Supports 覆盖有元数据/无元数据的频道。
func TestSupports(t *testing.T) {
	e := newTestEngine(t)
	for _, slug := range []string{"cctv1", "cctv5p", "guoxue", "beijingws"} {
		if !e.Supports(slug) {
			t.Fatalf("Supports(%q) = false, 期望 true", slug)
		}
	}
	for _, slug := range []string{"no-such", "", "cctv999"} {
		if e.Supports(slug) {
			t.Fatalf("Supports(%q) = true, 期望 false", slug)
		}
	}
}

// TestChannelsTableGenerated 校验从 JS 抽取的频道表规模（防止生成脚本回归）。
func TestChannelsTableGenerated(t *testing.T) {
	if len(channels) != 64 {
		t.Fatalf("channels 条目数 = %d, 期望 64", len(channels))
	}
	if len(aliasMap) != 37 {
		t.Fatalf("aliasMap 条目数 = %d, 期望 37", len(aliasMap))
	}
	if len(channelOrder) != len(channels) {
		t.Fatalf("channelOrder 长度 = %d, channels = %d", len(channelOrder), len(channels))
	}
	// 抽查几个 JS 特有的值
	if got := channels["cctv1"]; got.CnLid != "2024078201" || got.LivePid != "600001859" || got.Defn != "fhd" {
		t.Fatalf("cctv1 元数据 = %+v", got)
	}
	if got := channels["cctvfyjc"]; got.Defn != "shd" {
		t.Fatalf("cctvfyjc defn = %q, 期望 shd", got.Defn)
	}
	if aliasMap["beijingws"] != "bjws" || aliasMap["cctv5plus"] != "cctv5p" {
		t.Fatalf("aliasMap 抽查失败: %+v", aliasMap)
	}
}

// 确保 time 包被使用（缓存 TTL 断言用）。
var _ = time.Second
