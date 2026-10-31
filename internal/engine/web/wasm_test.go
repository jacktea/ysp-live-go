package web

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestWasmKeygen 验证 keygen WASM 能实例化并返回非空字符串。
func TestWasmKeygen(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	rnd, err := e.getRnd(ctx, keygenValues{
		GUID: "testguid_0123456789", YspAppID: yspAppID, Version: "v1",
		Host: "www.yangshipin.cn", Protocol: "https:", TS: "1700000000000",
	})
	if err != nil {
		t.Fatalf("getRnd: %v", err)
	}
	t.Logf("getRnd = %q (len=%d)", rnd, len(rnd))
	if rnd == "" {
		t.Fatal("getRnd 返回空字符串")
	}
	sign, err := e.getSign(ctx, keygenValues{
		GUID: "testguid_0123456789", YspAppID: yspAppID, Version: "v1",
		Host: "www.yangshipin.cn", Protocol: "https:", Token: "tok", Input: "input-md5", TS: "1700000000000",
	})
	if err != nil {
		t.Fatalf("getSign: %v", err)
	}
	t.Logf("getSign = %q (len=%d)", sign, len(sign))
	if sign == "" {
		t.Fatal("getSign 返回空字符串")
	}
}

// TestWasmTicketGolden 用 node 跑原始 JS 生成的 golden 值验证 buildTicket 完全一致。
// golden 由 /tmp 的脚本用原 ysp-engine.js buildTicket 生成（固定时钟除外，
// ticket 输出依赖装置时钟，因此 golden 无法固定；这里验证结构与长度，
// 并额外验证相同输入的确定性）。
func TestWasmTicketBasic(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	out, err := e.buildTicket(ctx, "600001859", "1700000000", "2024078201", "m9x_abcdefghijklm")
	if err != nil {
		t.Fatalf("buildTicket: %v", err)
	}
	t.Logf("ticket = %s (len=%d)", out, len(out))
	// 原 JS: outLen = pid+auth_ts+guid+yspappid+14 = 9+10+17+9+14 = 59 字节 → 118 hex
	if len(out) != 118 {
		t.Fatalf("ticket hex 长度 = %d, 期望 118", len(out))
	}
	for _, c := range out {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("ticket 含非 hex 字符 %q", c)
		}
	}
	e2 := newTestEngine(t)
	out2, err := e2.buildTicket(ctx, "600001859", "1700000000", "2024078201", "m9x_abcdefghijklm")
	if err != nil {
		t.Fatalf("buildTicket 第二次: %v", err)
	}
	if out[:20] != out2[:20] {
		t.Fatalf("相同输入的票据前缀不稳定:\n  %s\n  %s", out, out2)
	}
}

// TestWasmConcurrent 验证并发调用不会触发 WASM 单实例的数据竞争。
func TestWasmConcurrent(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	const n = 8
	errs := make(chan error, n*2)
	for i := 0; i < n; i++ {
		go func(i int) {
			_, err := e.buildTicket(ctx, "600001859", "1700000000", "2024078201", fmt.Sprintf("guid%02d_0123456789", i))
			errs <- err
		}(i)
		go func() {
			_, err := e.getRnd(ctx, keygenValues{GUID: "g_0123456789abc", YspAppID: yspAppID, Version: "v1", Host: "www.yangshipin.cn", Protocol: "https:"})
			errs <- err
		}()
	}
	for i := 0; i < n*2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("并发调用失败: %v", err)
		}
	}
}

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := New(Config{UpstreamTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}
