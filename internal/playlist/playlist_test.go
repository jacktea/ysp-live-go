package playlist

import (
	"strings"
	"testing"

	"github.com/jacktea/ysp-live-go/internal/model"
)

func TestAllM3UStructure(t *testing.T) {
	dir := model.NewChannelDirectory()
	body := All(Options{Host: "192.168.1.1:8767", Dir: dir})

	if !strings.HasPrefix(body, "#EXTM3U ") {
		t.Fatalf("bad header: %q", firstLine(body))
	}
	for _, want := range []string{
		`#EXT-X-APTV-PREVIEW: FALSE`,
		`#EXT-X-APTV-LATENCY: FALSE`,
		`#EXT-X-APTV-LOGO: FALSE`,
		`tvg-id="CCTV1"`,
		`catchup="append" catchup-days="7"`,
		`http://192.168.1.1:8767/cctv1.m3u8`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	// 64 路频道 = 64 行 #EXTINF + 64 行 URL + 4 行头部
	if got := strings.Count(body, "#EXTINF:"); got != 64 {
		t.Errorf("channel count = %d, want 64", got)
	}
}

func TestAllM3UGroupFilter(t *testing.T) {
	dir := model.NewChannelDirectory()
	body := All(Options{Host: "h:8767", Group: "卫视", Dir: dir})
	if strings.Count(body, "#EXTINF:") != 34 {
		t.Errorf("卫视 count = %d, want 34", strings.Count(body, "#EXTINF:"))
	}
	if strings.Contains(body, "CCTV-1 综合") {
		t.Error("央视频道不应出现在卫视过滤结果里")
	}
}

func TestAllM3UOnly4K(t *testing.T) {
	dir := model.NewChannelDirectory()
	body := All(Options{Host: "h:8767", Only4K: true, Dir: dir})
	if got := strings.Count(body, "#EXTINF:"); got != 3 {
		t.Errorf("only4k count = %d, want 3", got)
	}
}

func TestTSTokenRoundTrip(t *testing.T) {
	raw := "http://liveali.cctv.cn/path/seg-1.ts?auth=abc"
	tok := EncodeTSToken(raw)
	got, err := DecodeTSToken(tok)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != raw {
		t.Fatalf("roundtrip = %q, want %q", got, raw)
	}
}

func TestDecodeTSTokenRejects(t *testing.T) {
	cases := map[string]string{
		"not base64!!!": EncodeTSToken("ftp://x/y.ts"),
		"http scheme":   EncodeTSToken("ftp://a.cctv.cn/a.ts"),
		"not ts":        EncodeTSToken("http://a.cctv.cn/a.m3u8"),
	}
	for name, tok := range cases {
		if _, err := DecodeTSToken(tok); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, err := DecodeTSToken(""); err == nil {
		t.Error("empty token should fail")
	}
}

func TestExtractTSToken(t *testing.T) {
	cases := []struct{ query, want string }{
		{"=abc", "abc"},
		{"ts=abc", "abc"},
		{"url=abc", "abc"},
		{"abc", "abc"},
		{"", ""},
	}
	for _, c := range cases {
		if got := ExtractTSToken("/proxy.ts", c.query); got != c.want {
			t.Errorf("ExtractTSToken(%q) = %q, want %q", c.query, got, c.want)
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
