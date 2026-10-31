package resolver

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// 真实上游端到端测试（默认跳过）。
//
// 运行方式：YSP_E2E=1 go test ./internal/resolver/ -run TestE2ERealUpstream -v
// 必须显式设置 YSP_E2E 才执行，避免 CI/离线环境依赖真实央视频接口。
func TestE2ERealUpstream(t *testing.T) {
	if os.Getenv("YSP_E2E") != "1" {
		t.Skip("需要真实网络：设置 YSP_E2E=1 后运行")
	}
	if testing.Short() {
		t.Skip("short mode")
	}

	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Timeout = 10
	cfg.MetaJSON = dir + "/proxy-cache-state-rs.json"
	cfg.DeviceJSON = dir + "/device-state-rs.json"
	cfg.InitMaxAttempts = 1
	cfg.ZapDebounce = 0
	r := New(cfg)
	defer r.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := r.Init(ctx); err != nil {
		t.Fatalf("Init 失败: %v", err)
	}
	if !r.Ready() {
		t.Fatal("Init 成功后 Ready 应为 true")
	}

	// 常规高码率频道 + 两路 4K 频道。
	for _, channel := range []string{"cctv1", "cctv4k", "cctv164k"} {
		body, err := r.ChannelPlaylist(ctx, channel, "127.0.0.1:8767")
		if err != nil {
			t.Fatalf("[%s] ChannelPlaylist 失败: %v", channel, err)
		}
		if !strings.Contains(body, "#EXTM3U") {
			t.Fatalf("[%s] 播放列表缺少 #EXTM3U", channel)
		}
	}

	// 分片代理链路：token 解码 → 播放头匹配 → 真实抓取 TS。
	body, err := r.ChannelPlaylist(ctx, "cctv1", "127.0.0.1:8767")
	if err != nil {
		t.Fatalf("ChannelPlaylist 失败: %v", err)
	}
	token := ""
	for _, line := range strings.Split(body, "\n") {
		if i := strings.Index(line, "/proxy.ts?="); i >= 0 {
			token = strings.TrimPrefix(line[i:], "/proxy.ts?=")
			break
		}
	}
	if token == "" {
		t.Fatal("m3u8 中没有 proxy.ts token")
	}
	padded := token + strings.Repeat("=", (4-len(token)%4)%4)
	decoded, err := base64.URLEncoding.DecodeString(padded)
	if err != nil {
		t.Fatalf("token 解码失败: %v", err)
	}
	segURL := string(decoded)

	headers, err := r.PlaybackHeadersForTSURL(segURL)
	if err != nil {
		t.Fatalf("PlaybackHeadersForTSURL 失败: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, segURL, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("抓取分片失败: %v", err)
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		t.Fatalf("分片状态码 = %d", resp.StatusCode)
	}
	if len(buf) < 188 || buf[0] != 0x47 {
		t.Fatalf("返回内容不是合法 TS 分片: len=%d", len(buf))
	}
	t.Logf("设备协议端到端成功：%d 字节分片, headers=%v", len(buf), headers)
}
