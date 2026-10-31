package web

import (
	"context"
	"testing"
	"time"
)

// TestTicketGoldenJSPinned 用固定时钟与 JS 侧逐字节对齐验证 buildTicket。
//
// golden 值由原 ysp-engine.js 的 buildTicket 在同一固定时间戳（1700000000.123s）
// 下生成：jscheck_fixed.js 把 Date.now 固定为 1700000000123 后调用
// buildTicket('600001859','1700000000','2024078201','m9x_abcdefghijklm')。
//
// 这条测试的价值：ticket 的后 20 位十六进制依赖 Wasm 桩函数 a.d/a.i/a.j 注入的时间，
// 若桩函数的时间语义（秒/微秒/纳秒、写入位置）与 JS 有任何偏差，这里会立刻失败。
// 前 98 位则覆盖 WASM 数据段初始化、内存分配与 T(pid,auth_ts,cnlid,guid,yspappid,app_ver,out) 调用。
func TestTicketGoldenJSPinned(t *testing.T) {
	e, err := New(Config{UpstreamTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	// JS: Math.floor(Date.now()/1000) = 1700000000，(Date.now()%1000)*1000 = 123000 (us)
	//     (Date.now()%1000)*1e6 = 123000000 (ns)
	fixed := time.Unix(1700000000, 123*int64(time.Millisecond))
	e.now = func() time.Time { return fixed }

	got, err := e.buildTicket(context.Background(), "600001859", "1700000000", "2024078201", "m9x_abcdefghijklm")
	if err != nil {
		t.Fatalf("buildTicket: %v", err)
	}
	const want = "5c40d99c304df40d750e99bacd1221eefb56797becee841927993551d68faaf2fc8d8fc01e63e5910fbe84f313b72916d69083b8edbd36d7e9164e"
	if got != want {
		t.Fatalf("票据与 JS golden 不一致\n got=%s\nwant=%s", got, want)
	}
}

// TestTicketLengthFormula 校验 outLen 公式与 JS 完全一致：
// outLen = len(livepid) + len(auth_ts) + len(guid) + len("519748109") + 14，输出为 hex 故长度 *2。
func TestTicketLengthFormula(t *testing.T) {
	e, err := New(Config{UpstreamTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	cases := []struct{ pid, authTS, cnlid, guid string }{
		{"600001859", "1700000000", "2024078201", "m9x_abcdefghijklm"},
		{"600002264", "1700000000123", "2029810301", "abcdefghijklmnopqrstuvwxyz"},
	}
	for _, c := range cases {
		got, err := e.buildTicket(context.Background(), c.pid, c.authTS, c.cnlid, c.guid)
		if err != nil {
			t.Fatalf("buildTicket(%v): %v", c, err)
		}
		outLen := len(c.pid) + len(c.authTS) + len(c.guid) + len(yspAppID) + 14
		if want := outLen * 2; len(got) != want {
			t.Errorf("ticket 长度 = %d, 期望 %d (outLen=%d)", len(got), want, outLen)
		}
	}
}
