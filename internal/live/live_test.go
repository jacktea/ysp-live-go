package live

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacktea/ysp-live-go/internal/live/jce"
	"github.com/jacktea/ysp-live-go/internal/model"
	"github.com/jacktea/ysp-live-go/internal/ratelimit"
)

// testConfig 返回不依赖真实央视频接口、且关闭全局流控的测试配置。
func testConfig(now func() time.Time) Config {
	return Config{
		UA:              "test-agent",
		Window:          DefaultWindow,
		MaxSegs:         DefaultMaxSegs,
		RefreshInterval: 10 * time.Millisecond,
		IdleTimeout:     60 * time.Second,
		BKURLTTL:        300 * time.Second,
		Limiter:         ratelimit.New(0, 0),
		Now:             now,
	}
}

// buildJCEResponse 构造一个 jce.Unwrap 可解析的响应包：
// 外层 struct 的 tag1 是内层 body（jce_timeshift_url 返回值的结构体）。
func buildJCEResponse(innerBody []byte) []byte {
	w := jce.NewWriter()
	w.Struct(func(ww *jce.Writer) {
		ww.Int(123, 0)
		ww.Int(jce.CmdTimeshift, 1)
	}, 0)
	w.Bytes(innerBody, 1)
	reqCmd := w.Out()

	payload := make([]byte, 0, 16+len(reqCmd)+1)
	payload = append(payload, '&')
	payload = append(payload, make([]byte, 15)...)
	payload = append(payload, reqCmd...)
	payload = append(payload, '(')

	out := make([]byte, 89+len(payload)+1)
	out[0] = 0x13
	copy(out[89:], payload)
	out[len(out)-1] = 0x03
	return out
}

// timeshiftBody 构造内层结构体：tag0=err、tag1=errMsg、tag2=url。
func timeshiftBody(errCode int, errMsg, url string) []byte {
	w := jce.NewWriter()
	if errCode != 0 {
		w.Int(errCode, 0)
	}
	if errMsg != "" {
		w.String(errMsg, 1)
	}
	if url != "" {
		w.String(url, 2)
	}
	return w.Out()
}

// m3u8Text 生成带 PDT 的 m3u8 文本。
func m3u8Text(prefix string, n int, startPDT time.Time) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:6\n")
	for i := 0; i < n; i++ {
		b.WriteString("#EXT-X-PROGRAM-DATE-TIME:")
		b.WriteString(startPDT.Add(time.Duration(i) * 6 * time.Second).Format("2006-01-02T15:04:05.000-07:00"))
		b.WriteString("\n#EXTINF:6.000,\n")
		b.WriteString(prefix)
		b.WriteString("-")
		b.WriteString(itoa(i))
		b.WriteString(".ts\n")
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	p := len(buf)
	for n > 0 {
		p--
		buf[p] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[p:])
}

// fakeUpstream 模拟 jacc 时移接口 + m3u8 CDN。
type fakeUpstream struct {
	srv        *httptest.Server
	mu         sync.Mutex
	errCode    int
	errMsg     string
	deadHost   bool
	m3u8       string
	segHits    int32
	masterPl   string
	subPl      string
	requestCnt int32
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{}
	mux := http.NewServeMux()
	mux.HandleFunc("/jacc", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.requestCnt, 1)
		f.mu.Lock()
		code, msg, dead := f.errCode, f.errMsg, f.deadHost
		f.mu.Unlock()
		var body []byte
		switch {
		case dead:
			body = timeshiftBody(0, "", "http://liverecord.video.cloud.cctv.com/live/abc.m3u8")
		case code != 0:
			body = timeshiftBody(code, msg, "")
		default:
			body = timeshiftBody(0, "", f.srv.URL+"/live.m3u8")
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(buildJCEResponse(body))
	})
	mux.HandleFunc("/live.m3u8", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.segHits, 1)
		f.mu.Lock()
		text := f.m3u8
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte(text))
	})
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		text := f.masterPl
		f.mu.Unlock()
		_, _ = w.Write([]byte(text))
	})
	mux.HandleFunc("/sub/index.m3u8", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		text := f.subPl
		f.mu.Unlock()
		_, _ = w.Write([]byte(text))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// 覆盖 .cctv. 路径：bk 地址过滤器按 ".cctv." 子串判断，测试用路径承载。
		f.mu.Lock()
		text := f.m3u8
		f.mu.Unlock()
		_, _ = w.Write([]byte(text))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) setM3U8(text string) {
	f.mu.Lock()
	f.m3u8 = text
	f.mu.Unlock()
}

// ---------------------------------------------------------------------------
// 时移参数解析（原 parse_catchup_time_val / parse_catchup_query）
// ---------------------------------------------------------------------------

func TestParseCatchupTimeVal(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"1700000000", 1700000000, true},            // 10 位：秒级时间戳
		{"1700000000000", 1700000000, true},         // 13 位：毫秒级时间戳
		{"20231114223320", 1699972400, true},        // 14 位：YYYYMMDDHHMMSS（东八区）
		{"202311142233", 1699972380, true},          // 12 位：YYYYMMDDHHMM（东八区）
		{"  1700000000  ", 1700000000, true},        // 前后空白
		{"1700000000-1700003600", 1700000000, true}, // 剥离非数字后 20 位 → 失败?
		{"", 0, false},               // 空
		{"abc", 0, false},            // 无数字
		{"20231314223320", 0, false}, // 非法月份（14 位解析失败）
		{"1234", 0, false},           // 位数不支持
	}
	// "1700000000-1700003600" 剥数字后是 20 位，原实现返回 None。
	cases[5].ok = false
	cases[5].want = 0

	for _, c := range cases {
		got, ok := ParseCatchupTimeVal(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("ParseCatchupTimeVal(%q) = (%d,%v), want (%d,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestParseCatchupQuery(t *testing.T) {
	// playseek 区间：20231114223320 ~ 20231114233320（东八区）→ 1699972400 ~ 1699976000
	start, end, ok := ParseCatchupQuery("playseek=20231114223320-20231114233320")
	if !ok {
		t.Fatalf("playseek 区间解析失败")
	}
	if start != 1699972400 || end != 1699976000 {
		t.Errorf("playseek = (%d,%d), want (1699972400,1699976000)", start, end)
	}

	// playseek 单值：end 取 max(start+60, min(start+7200, now))
	start, end, ok = ParseCatchupQuery("playseek=1700000000")
	if !ok || start != 1700000000 {
		t.Fatalf("playseek 单值解析失败: %d %v", start, ok)
	}
	// now 远大于 start+7200，因此 end = min(start+7200, now) = start+7200
	if end != start+7200 {
		t.Errorf("end = %d, want %d（过去时间取 start+7200）", end, start+7200)
	}

	// start/end 参数
	start, end, ok = ParseCatchupQuery("start=1700000000&end=1700003600")
	if !ok || start != 1700000000 || end != 1700003600 {
		t.Errorf("start/end = (%d,%d,%v)", start, end, ok)
	}

	// utc/lutc 回退
	start, end, ok = ParseCatchupQuery("utc=1700000000&lutc=1700003600")
	if !ok || start != 1700000000 || end != 1700003600 {
		t.Errorf("utc/lutc = (%d,%d,%v)", start, end, ok)
	}

	// starttime/endtime 回退
	start, end, ok = ParseCatchupQuery("starttime=20231114223320&endtime=20231114233320")
	if !ok || start != 1699972400 || end != 1699976000 {
		t.Errorf("starttime/endtime = (%d,%d,%v)", start, end, ok)
	}

	// end <= start 时按默认窗口修正
	start, end, ok = ParseCatchupQuery("start=1700000000&end=1700000000")
	if !ok || end != start+7200 {
		t.Errorf("end<=start 修正失败: (%d,%d,%v)", start, end, ok)
	}

	// 未来时间的 end 上限为 now
	future := time.Now().Add(24 * time.Hour).Unix()
	_, end, ok = ParseCatchupQuery("start=" + itoa64(future))
	if !ok {
		t.Fatalf("未来时间解析失败")
	}
	// min(start+7200, now) = now，max(start+60, now) = start+60
	if end != future+60 {
		t.Errorf("未来时间 end = %d, want %d", end, future+60)
	}

	// 非时移请求
	if _, _, ok := ParseCatchupQuery(""); ok {
		t.Errorf("空查询不应判定为时移请求")
	}
	if _, _, ok := ParseCatchupQuery("foo=bar"); ok {
		t.Errorf("无时移参数不应判定为时移请求")
	}
}

func itoa64(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ---------------------------------------------------------------------------
// jce_timeshift_url / jce_fetch / jce_refresh
// ---------------------------------------------------------------------------

func TestJCETimeshiftURL(t *testing.T) {
	f := newFakeUpstream(t)
	cfg := testConfig(nil)
	cfg.JaccURL = f.srv.URL + "/jacc"
	m := New(cfg)
	defer m.Stop()

	got, err := m.jceTimeshiftURL("600001859", "2024078201", 1700000000, 1700003600, "fhd")
	if err != nil {
		t.Fatalf("jceTimeshiftURL: %v", err)
	}
	if got != f.srv.URL+"/live.m3u8" {
		t.Errorf("url = %q", got)
	}
}

func TestJCETimeshiftURLErrCode(t *testing.T) {
	f := newFakeUpstream(t)
	f.errCode = 10086
	f.errMsg = "内部错误"
	cfg := testConfig(nil)
	cfg.JaccURL = f.srv.URL + "/jacc"
	m := New(cfg)
	defer m.Stop()

	_, err := m.jceTimeshiftURL("600001859", "2024078201", 0, 1, "fhd")
	if err == nil {
		t.Fatalf("期望错误，实际成功")
	}
	if !strings.Contains(err.Error(), "内部错误") {
		t.Errorf("错误文本应保留接口 errMsg，实际: %v", err)
	}
}

func TestJCETimeshiftURLDeadHost(t *testing.T) {
	f := newFakeUpstream(t)
	f.deadHost = true
	cfg := testConfig(nil)
	cfg.JaccURL = f.srv.URL + "/jacc"
	m := New(cfg)
	defer m.Stop()

	_, err := m.jceTimeshiftURL("600001859", "2024078201", 0, 1, "fhd")
	if err == nil || !strings.Contains(err.Error(), ErrDeadHost.Error()) {
		t.Fatalf("期望 DeadHost 错误，实际: %v", err)
	}
}

func TestJCEFetchParse(t *testing.T) {
	f := newFakeUpstream(t)
	base := time.Date(2023, 11, 15, 12, 0, 0, 0, cstZone)
	f.setM3U8(m3u8Text("seg", 5, base))
	cfg := testConfig(nil)
	cfg.JaccURL = f.srv.URL + "/jacc"
	m := New(cfg)
	defer m.Stop()

	ch := m.Channel("cctv1")
	if ch == nil {
		t.Fatalf("cctv1 不存在")
	}
	segs, err := m.jceFetch(ch)
	if err != nil {
		t.Fatalf("jceFetch: %v", err)
	}
	if len(segs) != 5 {
		t.Fatalf("分片数 = %d, want 5", len(segs))
	}
	if segs[0].dur != 6.0 {
		t.Errorf("dur = %v, want 6", segs[0].dur)
	}
	if !strings.Contains(segs[0].pdt, "2023-11-15T12:00:00") {
		t.Errorf("pdt = %q", segs[0].pdt)
	}
	// 相对分片地址应转成绝对地址
	if !strings.HasPrefix(segs[0].url, f.srv.URL) || !strings.HasSuffix(segs[0].url, "/seg-0.ts") {
		t.Errorf("分片地址未转绝对: %q", segs[0].url)
	}
}

// 初始化只取最后 25 片。
func TestJCERefreshInitTakesLast25(t *testing.T) {
	f := newFakeUpstream(t)
	base := time.Date(2023, 11, 15, 12, 0, 0, 0, cstZone)
	f.setM3U8(m3u8Text("seg", 40, base))
	cfg := testConfig(nil)
	cfg.JaccURL = f.srv.URL + "/jacc"
	m := New(cfg)
	defer m.Stop()

	ch := m.Channel("cctv1")
	if err := m.jceRefresh(ch); err != nil {
		t.Fatalf("jceRefresh: %v", err)
	}
	if n := ch.SegmentCount(); n != 25 {
		t.Errorf("初始化分片数 = %d, want 25", n)
	}
	state := ch.State()
	if !strings.Contains(state.Mode, ModeJCE) {
		t.Errorf("mode = %q", state.Mode)
	}
}

// 增量按 PDT 去重，不重复插入；MAX_SEGS 淘汰最旧。
func TestJCERefreshDedupAndEvict(t *testing.T) {
	f := newFakeUpstream(t)
	base := time.Date(2023, 11, 15, 12, 0, 0, 0, cstZone)
	f.setM3U8(m3u8Text("seg", 4, base))
	cfg := testConfig(nil)
	cfg.JaccURL = f.srv.URL + "/jacc"
	cfg.MaxSegs = 5
	m := New(cfg)
	defer m.Stop()

	ch := m.Channel("cctv1")
	if err := m.jceRefresh(ch); err != nil {
		t.Fatalf("首次刷新: %v", err)
	}
	if n := ch.SegmentCount(); n != 4 {
		t.Fatalf("首次分片数 = %d, want 4", n)
	}
	// 重复拉取同一份列表：不应重复插入
	if err := m.jceRefresh(ch); err != nil {
		t.Fatalf("二次刷新: %v", err)
	}
	if n := ch.SegmentCount(); n != 4 {
		t.Errorf("去重后分片数 = %d, want 4", n)
	}
	// 推进窗口：新增 3 片，总数超过 MaxSegs=5，淘汰最旧
	f.setM3U8(m3u8Text("seg2", 3, base.Add(24*time.Second)))
	if err := m.jceRefresh(ch); err != nil {
		t.Fatalf("三次刷新: %v", err)
	}
	if n := ch.SegmentCount(); n != 5 {
		t.Errorf("淘汰后分片数 = %d, want 5", n)
	}
}

// pdt 为空时不得用 pdt 过滤，且同一分片不重复插入（bug 2 的回归测试）。
func TestJCERefreshNoPDTDoesNotDuplicate(t *testing.T) {
	f := newFakeUpstream(t)
	// 无 PDT 的列表
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n")
	for i := 0; i < 3; i++ {
		b.WriteString("#EXTINF:6.000,\n")
		b.WriteString("nopdt-" + itoa(i) + ".ts\n")
	}
	f.setM3U8(b.String())
	cfg := testConfig(nil)
	cfg.JaccURL = f.srv.URL + "/jacc"
	m := New(cfg)
	defer m.Stop()

	ch := m.Channel("cctv1")
	for i := 0; i < 3; i++ {
		if err := m.jceRefresh(ch); err != nil {
			t.Fatalf("第 %d 次刷新: %v", i, err)
		}
	}
	if n := ch.SegmentCount(); n != 3 {
		t.Errorf("无 PDT 时分片数 = %d, want 3（不应重复插入）", n)
	}
}

// JCE 命中受限 CDN 时自动切 bk（对应原 DeadHostError 分支）。
func TestRefreshOnceSwitchesToBKOnDeadHost(t *testing.T) {
	f := newFakeUpstream(t)
	f.deadHost = true
	m3u8 := "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:6.000,\nhttp://127.0.0.1:1/a.ts\n"
	f.setM3U8(m3u8)
	cfg := testConfig(nil)
	cfg.JaccURL = f.srv.URL + "/jacc"
	cfg.BKLiveInfoURL = f.srv.URL + "/bk/"
	// bk 的 JSON 接口在 /bk/ 上返回 m3u8 文本，无法解析成 JSON，这里只验证模式切换。
	m := New(cfg)
	defer m.Stop()

	ch := m.Channel("cctv1")
	ok := m.refreshOnce(ch)
	if ok {
		t.Fatalf("bk JSON 不可解析时 refreshOnce 应返回 false")
	}
	if got := ch.CurrentMode(); got != ModeBK {
		t.Errorf("mode = %q, want %q", got, ModeBK)
	}
	if ch.State().LastError == "" {
		t.Errorf("失败后 LastError 不应为空")
	}
}

// ---------------------------------------------------------------------------
// build_playlist
// ---------------------------------------------------------------------------

func TestBuildPlaylist(t *testing.T) {
	m := New(testConfig(nil))
	defer m.Stop()
	ch := m.Channel("cctv1")

	// 空列表返回 ""（bug 4 的回归测试：原实现会 panic）
	if got := m.BuildPlaylist(ch, true); got != "" {
		t.Errorf("空分片时应返回空串，实际: %q", got)
	}

	// 手工灌入 6 片，验证 holdback / TARGETDURATION / MEDIA-SEQUENCE
	ch.mu.Lock()
	base := time.Date(2023, 11, 15, 12, 0, 0, 0, cstZone)
	for i := 0; i < 6; i++ {
		key := segKey("http://x/"+itoa(i)+".ts", base.Add(time.Duration(i)*6*time.Second).String())
		ch.seq++
		ch.segments[key] = &segment{
			seq: ch.seq,
			dur: 6.0,
			pdt: base.Add(time.Duration(i) * 6 * time.Second).Format("2006-01-02T15:04:05.000-07:00"),
			url: "http://x/" + itoa(i) + ".ts",
		}
		ch.order = append(ch.order, key)
	}
	ch.mu.Unlock()

	pl := m.BuildPlaylist(ch, true)
	if !strings.Contains(pl, "#EXTM3U\n") {
		t.Fatalf("缺少 #EXTM3U: %q", pl)
	}
	if !strings.Contains(pl, "#EXT-X-VERSION:3") {
		t.Errorf("缺少 VERSION:3")
	}
	if !strings.Contains(pl, "#EXT-X-TARGETDURATION:6") {
		t.Errorf("TARGETDURATION 错误: %s", pl)
	}
	if !strings.Contains(pl, "#EXT-X-MEDIA-SEQUENCE:1") {
		t.Errorf("MEDIA-SEQUENCE 应为首片 seq=1: %s", pl)
	}
	// holdback: 6 片中显示 5 片
	if n := strings.Count(pl, "#EXTINF:"); n != 5 {
		t.Errorf("holdback 后分片数 = %d, want 5", n)
	}
	if strings.Contains(pl, "http://x/5.ts") {
		t.Errorf("holdback 应丢弃最后一片 /5.ts: %s", pl)
	}
	// 不足 4 片时不 holdback
	pl = m.BuildPlaylist(ch, false)
	if n := strings.Count(pl, "#EXTINF:"); n != 6 {
		t.Errorf("holdback=false 分片数 = %d, want 6", n)
	}
}

// ---------------------------------------------------------------------------
// bkliveinfo（原 bk_playurls）
// ---------------------------------------------------------------------------

func newBKServer(t *testing.T, respond func(w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "qqlive" {
			t.Errorf("User-Agent = %q, want qqlive", r.Header.Get("User-Agent"))
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		if !strings.Contains(r.URL.RawQuery, "cKey=--01") {
			t.Errorf("查询串缺少 cKey: %s", r.URL.RawQuery)
		}
		respond(w)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestBKPlayurlsParse(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "playurl + backurl_list 列表",
			body: `{"iretcode":0,"playurl":"http://a.cctv.com/live.m3u8",
			        "backurl_list":["http://b.cctv.com/x.m3u8",{"url":"http://c.cctv.com/y.m3u8"}]}`,
			want: []string{"http://a.cctv.com/live.m3u8", "http://b.cctv.com/x.m3u8", "http://c.cctv.com/y.m3u8"},
		},
		{
			name: "backurl 字符串形态（; 分隔）",
			body: `{"iretcode":0,"playurl":"http://a.cctv.com/live.m3u8",
			        "backurl":"http://b.cctv.com/x.m3u8;http://c.cctv.com/y.m3u8"}`,
			want: []string{"http://a.cctv.com/live.m3u8", "http://b.cctv.com/x.m3u8", "http://c.cctv.com/y.m3u8"},
		},
		{
			name: "bklive- 优先排序 + 过滤非 cctv 域名",
			body: `{"iretcode":0,"backurl_list":["http://z.cctv.com/z.m3u8","http://evil.example.com/x.m3u8","http://bklive-a.cctv.com/a.m3u8"]}`,
			want: []string{"http://bklive-a.cctv.com/a.m3u8", "http://z.cctv.com/z.m3u8"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newBKServer(t, func(w http.ResponseWriter) {
				_, _ = w.Write([]byte(c.body))
			})
			cfg := testConfig(nil)
			cfg.BKLiveInfoURL = srv.URL + "/"
			m := New(cfg)
			defer m.Stop()

			got, err := m.bkPlayurls("2024078201", "600001859", "fhd")
			if err != nil {
				t.Fatalf("bkPlayurls: %v", err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("地址数 = %d, want %d: %v", len(got), len(c.want), got)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Errorf("地址[%d] = %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestBKPlayurlsErrCode(t *testing.T) {
	srv := newBKServer(t, func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"iretcode":-2,"errinfo":"签名错误"}`))
	})
	cfg := testConfig(nil)
	cfg.BKLiveInfoURL = srv.URL + "/"
	m := New(cfg)
	defer m.Stop()

	_, err := m.bkPlayurls("2024078201", "600001859", "fhd")
	if err == nil {
		t.Fatalf("iretcode != 0 应报错")
	}
	if !strings.Contains(err.Error(), "iretcode=-2") || !strings.Contains(err.Error(), "签名错误") {
		t.Errorf("错误文本应包含 iretcode 与 errinfo，实际: %v", err)
	}
}

func TestBKPlayurlsNoPlayurl(t *testing.T) {
	srv := newBKServer(t, func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"iretcode":0,"backurl_list":["http://not-cctv.example/x.m3u8"]}`))
	})
	cfg := testConfig(nil)
	cfg.BKLiveInfoURL = srv.URL + "/"
	m := New(cfg)
	defer m.Stop()

	_, err := m.bkPlayurls("2024078201", "600001859", "fhd")
	if err == nil || !strings.Contains(err.Error(), "no playurl") {
		t.Fatalf("期望 no playurl 错误，实际: %v", err)
	}
}

// bk_refresh 成功路径：地址过期后重取一次并保存播放列表。
func TestBKRefreshSuccess(t *testing.T) {
	f := newFakeUpstream(t)
	m3u8 := "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:6.000,\na.ts\n"
	f.setM3U8(m3u8)

	var calls int32
	srv := newBKServer(t, func(w http.ResponseWriter) {
		atomic.AddInt32(&calls, 1)
		pl := f.srv.URL + "/.cctv./live.m3u8"
		_, _ = w.Write([]byte(`{"iretcode":0,"playurl":"` + pl + `"}`))
	})
	cfg := testConfig(nil)
	cfg.BKLiveInfoURL = srv.URL + "/"
	m := New(cfg)
	defer m.Stop()

	ch := m.Channel("cctv11") // ForceBK 频道
	if err := m.bkRefresh(ch); err != nil {
		t.Fatalf("bkRefresh: %v", err)
	}
	st := ch.State()
	if !st.HasBKPlaylist {
		t.Errorf("bk 播放列表未保存")
	}
	if st.LastError != "" {
		t.Errorf("成功后 LastError 应为空，实际 %q", st.LastError)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("bkliveinfo 调用次数 = %d, want 1", calls)
	}
	// TTL 内二次刷新不应重取地址
	if err := m.bkRefresh(ch); err != nil {
		t.Fatalf("二次 bkRefresh: %v", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("TTL 内不应重取地址，调用次数 = %d", calls)
	}
}

// ---------------------------------------------------------------------------
// fetch_abs_playlist
// ---------------------------------------------------------------------------

func TestFetchAbsPlaylistMasterRecursion(t *testing.T) {
	f := newFakeUpstream(t)
	f.masterPl = "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=2000000\nsub/index.m3u8\n"
	f.subPl = "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:6.000,\nsegA.ts\n#EXTINF:6.000,\nhttp://other.cctv.com/segB.ts\n"
	cfg := testConfig(nil)
	m := New(cfg)
	defer m.Stop()

	pl, err := m.fetchAbsPlaylist(f.srv.URL+"/master.m3u8", 0)
	if err != nil {
		t.Fatalf("fetchAbsPlaylist: %v", err)
	}
	if !strings.Contains(pl, f.srv.URL+"/sub/segA.ts") {
		t.Errorf("相对分片未转绝对: %s", pl)
	}
	if !strings.Contains(pl, "http://other.cctv.com/segB.ts") {
		t.Errorf("绝对分片被改写: %s", pl)
	}
	if strings.Contains(pl, "#EXT-X-STREAM-INF") {
		t.Errorf("应递归进入子播放列表，不应返回主列表: %s", pl)
	}
}

// ---------------------------------------------------------------------------
// 频道 / 并发
// ---------------------------------------------------------------------------

func TestChannelLookupAndForceBK(t *testing.T) {
	m := New(testConfig(nil))
	defer m.Stop()

	if m.Channel("不存在的频道") != nil {
		t.Errorf("未知频道应返回 nil")
	}
	// 别名解析
	if ch := m.Channel("cctv5plus"); ch == nil || ch.Slug != "cctv5p" {
		t.Errorf("别名解析失败: %+v", ch)
	}
	// 强制 bk 频道初始模式
	if ch := m.Channel("cctv11"); ch == nil || ch.CurrentMode() != ModeBK {
		t.Errorf("cctv11 初始 mode 应为 bk")
	}
	if ch := m.Channel("cctv1"); ch == nil || ch.CurrentMode() != ModeJCE {
		t.Errorf("cctv1 初始 mode 应为 jce")
	}
	if got := len(m.Snapshot()); got != len(model.DefaultChannels) {
		t.Errorf("Snapshot 条目数 = %d, want %d", got, len(model.DefaultChannels))
	}
}

// ensure_channel 并发调用不产生数据竞争（配合 go test -race 验证）。
func TestEnsureChannelConcurrent(t *testing.T) {
	f := newFakeUpstream(t)
	f.setM3U8(m3u8Text("seg", 8, time.Now().Add(-time.Minute)))
	cfg := testConfig(nil)
	cfg.JaccURL = f.srv.URL + "/jacc"
	m := New(cfg)
	defer m.Stop()

	ch := m.Channel("cctv1")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.EnsureChannel(ch)
			_ = m.BuildPlaylist(ch, true)
			_ = ch.State()
		}()
	}
	wg.Wait()

	if ch.SegmentCount() == 0 {
		t.Errorf("并发 ensure 后应有分片")
	}
	// 同频道并发刷新只应发起一次上游请求（初始化 1 次 + 后台循环可能若干次）
	if hits := atomic.LoadInt32(&f.segHits); hits < 1 {
		t.Errorf("未发起上游请求")
	}
}

// Stop 可重复调用且能结束后台循环。
func TestStopIdempotent(t *testing.T) {
	cfg := testConfig(nil)
	m := New(cfg)
	m.Stop()
	m.Stop()
	// 停止后 ensure 不应再启动 goroutine，也不 panic
	ch := m.Channel("cctv1")
	m.EnsureChannel(ch)
	if got := m.BuildPlaylist(ch, true); got != "" {
		t.Errorf("停止后无分片应返回空串，实际 %q", got)
	}
}

// 时移：不在 TimeshiftSupported 的频道不报错（bug 5）。
func TestCatchupUnsupportedChannelNoError(t *testing.T) {
	m := New(testConfig(nil))
	defer m.Stop()

	ch := m.Channel("cctv11") // 不在 TIMESHIFT_SUPPORTED
	if model.TimeshiftSupported[ch.Slug] {
		t.Skip("cctv11 意外成为时移频道")
	}
	pl, err := m.CatchupPlaylist(ch, 1700000000, 1700000600, "example.com")
	if err != nil {
		t.Errorf("不支持的时移频道不应报错: %v", err)
	}
	if pl != "" {
		t.Errorf("不支持的时移频道应返回空播放列表，实际 %q", pl)
	}
}

// 时移成功路径：jacc 返回地址后抓取 m3u8 并返回绝对分片地址。
func TestCatchupPlaylistSuccess(t *testing.T) {
	f := newFakeUpstream(t)
	f.setM3U8(m3u8Text("seg", 3, time.Now().Add(-time.Hour)))
	cfg := testConfig(nil)
	cfg.JaccURL = f.srv.URL + "/jacc"
	m := New(cfg)
	defer m.Stop()

	ch := m.Channel("cctv1")
	pl, err := m.CatchupPlaylist(ch, 1700000000, 1700000600, "example.com")
	if err != nil {
		t.Fatalf("CatchupPlaylist: %v", err)
	}
	if !strings.Contains(pl, "#EXTM3U") {
		t.Fatalf("返回内容不是 m3u8: %q", pl)
	}
	if !strings.Contains(pl, f.srv.URL+"/seg-0.ts") {
		t.Errorf("时移分片未转绝对地址: %s", pl)
	}
}

// 时移查询入口：非时移请求返回 ok=false。
func TestCatchupQuery(t *testing.T) {
	m := New(testConfig(nil))
	defer m.Stop()
	ch := m.Channel("cctv1")

	if _, ok, _ := m.CatchupQuery(ch, "foo=bar", "h"); ok {
		t.Errorf("非时移请求应返回 ok=false")
	}
}

// segKey 行为：有 pdt 用 pdt，无 pdt 用 scheme://host/path。
func TestSegKey(t *testing.T) {
	if got := segKey("http://h/a.ts", "20231115T120000"); got != "pdt:20231115T120000" {
		t.Errorf("segKey(pdt) = %q", got)
	}
	if got := segKey("http://h/a.ts?token=1", ""); got != "http://h/a.ts" {
		t.Errorf("segKey(url) = %q, want http://h/a.ts", got)
	}
	if segKey("http://h/a.ts?token=1", "") != segKey("http://h/a.ts?token=2", "") {
		t.Errorf("同一分片不同签名应视为同一 key（去重依赖此语义）")
	}
	if segKey("http://h/a.ts", "") == segKey("http://h/b.ts", "") {
		t.Errorf("不同分片不应视为同一 key")
	}
}

// defn_label
func TestDefnLabel(t *testing.T) {
	cases := map[string]string{"fhd": "1080p", "shd": "720p", "FHD": "1080p", "": "1080p", "uhd": "uhd"}
	for in, want := range cases {
		if got := defnLabel(in); got != want {
			t.Errorf("defnLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// bk_refresh 重试与错误文本（bug 3 的回归测试）
// ---------------------------------------------------------------------------

// 第一轮全部地址失败 → 重取地址 → 第二轮成功。
func TestBKRefreshRetriesWithFreshURLs(t *testing.T) {
	var mu sync.Mutex
	var listed int
	var cdnHits int32

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "application/json" {
			// bkliveinfo 接口
			mu.Lock()
			listed++
			n := listed
			mu.Unlock()
			// 第一次给一个失效地址，第二次给可用地址
			var pl string
			if n == 1 {
				pl = "http://" + r.Host + "/.cctv./dead.m3u8"
			} else {
				pl = "http://" + r.Host + "/.cctv./ok.m3u8"
			}
			_, _ = w.Write([]byte(`{"iretcode":0,"playurl":"` + pl + `"}`))
			return
		}
		// CDN
		atomic.AddInt32(&cdnHits, 1)
		if strings.Contains(r.URL.Path, "dead") {
			http.Error(w, "gone", http.StatusGone)
			return
		}
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:6.000,\na.ts\n"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := testConfig(nil)
	cfg.BKLiveInfoURL = srv.URL + "/"
	m := New(cfg)
	defer m.Stop()

	ch := m.Channel("cctv11")
	if err := m.bkRefresh(ch); err != nil {
		t.Fatalf("bkRefresh 应在重取地址后成功: %v", err)
	}
	if !ch.State().HasBKPlaylist {
		t.Errorf("未保存播放列表")
	}
	mu.Lock()
	gotListed := listed
	mu.Unlock()
	if gotListed != 2 {
		t.Errorf("bkliveinfo 调用次数 = %d, want 2（首轮失败后重取一次）", gotListed)
	}
	if atomic.LoadInt32(&cdnHits) < 2 {
		t.Errorf("CDN 请求次数 = %d, want >=2", cdnHits)
	}
}

// 全部失败时错误文本必须非空且带上下文（原实现可能抛出空的 last_err）。
func TestBKRefreshFailureHasContext(t *testing.T) {
	srv := newBKServer(t, func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"iretcode":0,"playurl":"http://127.0.0.1:1/.cctv./x.m3u8"}`))
	})
	cfg := testConfig(nil)
	cfg.BKLiveInfoURL = srv.URL + "/"
	m := New(cfg)
	defer m.Stop()

	ch := m.Channel("cctv11")
	err := m.bkRefresh(ch)
	if err == nil {
		t.Fatalf("全部失败应返回错误")
	}
	msg := err.Error()
	if !strings.Contains(msg, "bk playlist failed") {
		t.Errorf("错误文本应带上下文: %v", err)
	}
	if len(strings.TrimSpace(strings.TrimPrefix(msg, "bk playlist failed:"))) == 0 {
		t.Errorf("错误详情不能为空: %v", err)
	}
	// 失败后地址缓存被清空，便于下次重新获取
	st := ch.State()
	if st.BKURLs != 0 {
		t.Errorf("失败后应清空地址缓存，实际 %d", st.BKURLs)
	}
}

// ---------------------------------------------------------------------------
// coalesce_refresh 单飞
// ---------------------------------------------------------------------------

// 同一频道并发刷新只执行一次回调。
func TestCoalesceRefreshSingleFlight(t *testing.T) {
	m := New(testConfig(nil))
	defer m.Stop()

	var runs int32
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	fn := func() {
		atomic.AddInt32(&runs, 1)
		started <- struct{}{}
		<-release
	}

	done := make(chan struct{})
	go func() {
		m.coalesceRefresh("cctv1", fn, 2*time.Second)
		close(done)
	}()
	<-started

	// 第二个调用方应作为等待者，不执行回调
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.coalesceRefresh("cctv1", fn, 2*time.Second)
		}()
	}
	time.Sleep(30 * time.Millisecond)
	close(release)
	wg.Wait()
	<-done

	if got := atomic.LoadInt32(&runs); got != 1 {
		t.Errorf("回调执行次数 = %d, want 1（同频道并发刷新应合并）", got)
	}
}

// 等待方最多等 timeout 指定的时长。
func TestCoalesceRefreshWaiterTimeout(t *testing.T) {
	m := New(testConfig(nil))
	defer m.Stop()

	release := make(chan struct{})
	started := make(chan struct{}, 1)
	go m.coalesceRefresh("cctv2", func() {
		started <- struct{}{}
		<-release
	}, 5*time.Second)
	<-started

	begin := time.Now()
	m.coalesceRefresh("cctv2", func() { t.Errorf("等待方不应执行回调") }, 50*time.Millisecond)
	elapsed := time.Since(begin)
	close(release)

	if elapsed > time.Second {
		t.Errorf("等待方耗时 %v，应约为 50ms（受 timeout 限制）", elapsed)
	}
}

// ---------------------------------------------------------------------------
// refresh_loop 空闲超时
// ---------------------------------------------------------------------------

func TestRefreshLoopStopsWhenIdle(t *testing.T) {
	f := newFakeUpstream(t)
	f.setM3U8(m3u8Text("seg", 4, time.Now()))
	cfg := testConfig(nil)
	cfg.JaccURL = f.srv.URL + "/jacc"
	cfg.IdleTimeout = 40 * time.Millisecond
	cfg.RefreshInterval = 10 * time.Millisecond
	m := New(cfg)
	defer m.Stop()

	ch := m.Channel("cctv1")
	ch.Touch()
	stopped := make(chan struct{})
	go func() {
		m.refreshLoop(ch)
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatalf("空闲超时后 refreshLoop 未退出")
	}
	if ch.State().LoopRunning {
		t.Errorf("循环结束后 LoopRunning 应为 false")
	}
}

// 后台刷新失败 3 次后切换到 30s 慢速间隔（不阻塞 Stop）。
func TestRefreshLoopBackoffAndStop(t *testing.T) {
	cfg := testConfig(nil)
	cfg.JaccURL = "http://127.0.0.1:1/jacc" // 必然失败
	cfg.IdleTimeout = 10 * time.Minute
	cfg.RefreshInterval = 10 * time.Millisecond
	m := New(cfg)

	ch := m.Channel("cctv1")
	ch.Touch()
	m.spawn(func() { m.refreshLoop(ch) })
	time.Sleep(120 * time.Millisecond)

	begin := time.Now()
	m.Stop() // 慢速间隔期间也必须能被 Stop 立刻打断
	if elapsed := time.Since(begin); elapsed > 2*time.Second {
		t.Errorf("Stop 耗时 %v，应立即打断后台循环", elapsed)
	}
}

// ---------------------------------------------------------------------------
// 门面方法：Playlist / Touch / Catchup（gateway 适配层使用）
// ---------------------------------------------------------------------------

// Playlist 冷启动应拉取并返回可下发 m3u8。
func TestPlaylistColdStart(t *testing.T) {
	f := newFakeUpstream(t)
	f.setM3U8(m3u8Text("seg", 8, time.Now().Add(-time.Minute)))
	cfg := testConfig(nil)
	cfg.JaccURL = f.srv.URL + "/jacc"
	m := New(cfg)
	defer m.Stop()

	pl, err := m.Playlist(context.Background(), "cctv1")
	if err != nil {
		t.Fatalf("Playlist: %v", err)
	}
	if !strings.Contains(pl, "#EXTM3U") {
		t.Fatalf("返回内容不是 m3u8: %q", pl)
	}
	if !strings.Contains(pl, "#EXT-X-TARGETDURATION:6") {
		t.Errorf("缺少 TARGETDURATION: %s", pl)
	}
}

// Playlist 未知频道返回 ErrUnknownChannel。
func TestPlaylistUnknownChannel(t *testing.T) {
	m := New(testConfig(nil))
	defer m.Stop()
	if _, err := m.Playlist(context.Background(), "no-such-channel"); !errors.Is(err, ErrUnknownChannel) {
		t.Fatalf("err = %v, want ErrUnknownChannel", err)
	}
}

// Playlist 暂无数据时返回 ("", nil)，不阻断 gateway 的下层兜底。
func TestPlaylistNoDataReturnsEmptyNoError(t *testing.T) {
	f := newFakeUpstream(t)
	f.setM3U8("#EXTM3U\n#EXT-X-VERSION:3\n") // 没有分片
	cfg := testConfig(nil)
	cfg.JaccURL = f.srv.URL + "/jacc"
	cfg.BKLiveInfoURL = f.srv.URL + "/bkjson/"
	m := New(cfg)
	defer m.Stop()

	pl, err := m.Playlist(context.Background(), "cctv1")
	if err != nil {
		t.Fatalf("暂无数据不应返回 error: %v", err)
	}
	if pl != "" {
		t.Errorf("暂无数据应返回空串，实际 %q", pl)
	}
}

// Playlist 的 SWR：15s 内复用上次结果，超过 6s 触发后台刷新且不再同步打上游。
func TestPlaylistSWR(t *testing.T) {
	f := newFakeUpstream(t)
	f.setM3U8(m3u8Text("seg", 8, time.Now().Add(-time.Minute)))
	cfg := testConfig(nil)
	cfg.JaccURL = f.srv.URL + "/jacc"
	m := New(cfg)
	defer m.Stop()

	ch := m.Channel("cctv1")
	first, err := m.Playlist(context.Background(), "cctv1")
	if err != nil || first == "" {
		t.Fatalf("首次 Playlist: %q %v", first, err)
	}
	hitsAfterFirst := atomic.LoadInt32(&f.segHits)

	// 15s 内再次请求：命中 SWR，返回同一份内容
	second, err := m.Playlist(context.Background(), "cctv1")
	if err != nil {
		t.Fatalf("二次 Playlist: %v", err)
	}
	if second != first {
		t.Errorf("SWR 应复用上次构建的播放列表")
	}

	// 把构建时间回拨到 7s 前：应触发后台刷新（上游请求数增加），但仍先返回缓存。
	ch.mu.Lock()
	ch.builtTime = m.now().Add(-7 * time.Second)
	ch.mu.Unlock()
	if _, err := m.Playlist(context.Background(), "cctv1"); err != nil {
		t.Fatalf("三次 Playlist: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&f.segHits) > hitsAfterFirst {
			return // 后台刷新已发生
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("缓存超过 6s 后应触发后台刷新（上游命中数 %d → %d）", hitsAfterFirst, atomic.LoadInt32(&f.segHits))
}

// Playlist 在 context 取消时立即返回。
func TestPlaylistContextCanceled(t *testing.T) {
	m := New(testConfig(nil))
	defer m.Stop()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Playlist(ctx, "cctv1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// Touch 刷新 last_access 并拉起后台循环。
func TestTouchStartsLoopAndUpdatesAccess(t *testing.T) {
	cfg := testConfig(nil)
	cfg.JaccURL = "http://127.0.0.1:1/jacc"
	m := New(cfg)
	defer m.Stop()

	ch := m.Channel("cctv1")
	ch.mu.Lock()
	old := time.Now().Add(-time.Hour)
	ch.LastAccess = old
	ch.mu.Unlock()

	m.Touch("cctv1")
	st := ch.State()
	if !st.LastAccess.After(old) {
		t.Errorf("Touch 未刷新 LastAccess")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if ch.State().LoopRunning {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("Touch 应启动后台 refresh_loop")
}

// Touch 对未知频道应安全无副作用。
func TestTouchUnknownChannel(t *testing.T) {
	m := New(testConfig(nil))
	defer m.Stop()
	m.Touch("no-such-channel") // 不应 panic
}

// Catchup 走时移链路并返回绝对地址的 m3u8。
func TestCatchupFacadeSuccess(t *testing.T) {
	f := newFakeUpstream(t)
	f.setM3U8(m3u8Text("seg", 3, time.Now().Add(-time.Hour)))
	cfg := testConfig(nil)
	cfg.JaccURL = f.srv.URL + "/jacc"
	m := New(cfg)
	defer m.Stop()

	pl, err := m.Catchup(context.Background(), "cctv1", 1700000000, 1700000600)
	if err != nil {
		t.Fatalf("Catchup: %v", err)
	}
	if !strings.Contains(pl, "#EXTM3U") {
		t.Fatalf("不是 m3u8: %q", pl)
	}
	if !strings.Contains(pl, f.srv.URL+"/seg-0.ts") {
		t.Errorf("分片地址应为绝对地址: %s", pl)
	}
}

// Catchup 对不支持的频道返回 ("", nil)（bug 5：交由 gateway 回退直播）。
func TestCatchupFacadeUnsupported(t *testing.T) {
	m := New(testConfig(nil))
	defer m.Stop()
	pl, err := m.Catchup(context.Background(), "cctv11", 1700000000, 1700000600)
	if err != nil {
		t.Errorf("不支持的时移频道不应报错: %v", err)
	}
	if pl != "" {
		t.Errorf("应返回空串，实际 %q", pl)
	}
}

// Catchup 未知频道返回 ErrUnknownChannel；上游失败返回 error（gateway 据此回退直播）。
func TestCatchupFacadeErrors(t *testing.T) {
	m := New(testConfig(nil))
	defer m.Stop()
	if _, err := m.Catchup(context.Background(), "no-such", 1, 2); !errors.Is(err, ErrUnknownChannel) {
		t.Errorf("err = %v, want ErrUnknownChannel", err)
	}

	cfg := testConfig(nil)
	cfg.JaccURL = "http://127.0.0.1:1/jacc"
	m2 := New(cfg)
	defer m2.Stop()
	if _, err := m2.Catchup(context.Background(), "cctv1", 1700000000, 1700000600); err == nil {
		t.Errorf("上游失败应返回 error")
	}
}

// ChannelState 必须含 /diag 依赖的字段（编译期契约检查）。
func TestChannelStateFields(t *testing.T) {
	m := New(testConfig(nil))
	defer m.Stop()
	snap := m.Snapshot()
	if len(snap) == 0 {
		t.Fatalf("Snapshot 为空")
	}
	st := snap[0]
	var _ string = st.Slug
	var _ string = st.Name
	var _ string = st.Mode
	var _ time.Time = st.LastOK
	var _ string = st.LastError
	var _ int = st.Segments
	if st.Slug == "" || st.Name == "" || st.Mode == "" {
		t.Errorf("Snapshot 字段缺失: %+v", st)
	}
	// LastOK 用零值表示"从未拉取"
	if !st.LastOK.IsZero() {
		t.Errorf("未拉取过的频道 LastOK 应为零值，实际 %v", st.LastOK)
	}
}
