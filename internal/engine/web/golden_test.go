package web

// 本文件用"在原始 Node 里跑一遍"得到的 golden 值锁定协议细节，
// 防止后续重构悄悄改变签名/排序/编码行为。
// golden 的生成脚本见 gen/golden_reference.js（直接 require 原 ysp-engine.js 的等价实现）。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// TestBuildCKeyGolden 用 node 生成的 golden 值验证 buildCKey。
func TestBuildCKeyGolden(t *testing.T) {
	cases := []struct {
		cnlid, ts, guid, expect string
	}{
		{
			"2024078201", "1700000000", "testguid_0123456789abcd",
			"--013E7EA1E22BCC8871E4A709923AFC4E932B3E06C2BA42F9A86BAD041295BF1FC69C89A1F03C483AECDC7FD6A7106F2E0BD026F0A96354D5564121F7B2A77EC4564ACF92D1B11CE0AAA7291B0D0FC7348942A2A111E48D69C1760D4769B1FA0B810ADABBC27E9A9307F967E54EACA88D17F14E02086B3B25ADB80192F0AA757D7EF6972ACFC2860DB5EF52C135BA931E2BCC3B6DEA2DDC3815E9321CEC0D92378400A8F1914A5E7E58539E3F6095743361",
		},
		{
			// 这一例的 Java hashCode 为负，覆盖 int32 回绕（原 JS 的 |0 语义）。
			"2029360403", "1893456000", "zzz_qwertyuiopasd",
			"--01896F1463BADD464FC60ED587F2F7395C1E6D8370362ADDE6AD9348FE2BF6BA180AA319DECB2A49AD3C832FDDE3DC6B4B84A1291E09201B047692A6235AAC1AC0F48E59D21587054396BD6A1FA5C44A7B5409C12DBE48C42E7370BFC4CCA4912DDD58B605BB87CD1BB8C398F3A10AE7FB9F6A052EBB3EE154E90F095A491723EDF6AD6289C2E3DF42CFD20B5E54581B5FF1C62876C16F313A02889CBC94DB964E",
		},
	}
	for _, c := range cases {
		got, err := buildCKey(c.cnlid, c.ts, c.guid)
		if err != nil {
			t.Fatalf("buildCKey(%s,%s,%s): %v", c.cnlid, c.ts, c.guid, err)
		}
		if got != c.expect {
			t.Fatalf("buildCKey(%s,%s,%s)\n got=%s\nwant=%s", c.cnlid, c.ts, c.guid, got, c.expect)
		}
		if !strings.HasPrefix(got, "--01") {
			t.Fatalf("cKey 前缀不是 --01: %s", got)
		}
	}
}

// TestSortKeysLikeJSGolden 验证两种排序与原 JS 完全一致。
func TestSortKeysLikeJSGolden(t *testing.T) {
	liveKeys := []string{
		"adjust", "appVer", "app_version", "cKey", "channel", "cmd", "cnlid", "defn",
		"devid", "dtype", "encryptVer", "guid", "livepid", "otype", "platform", "sphttps", "stream",
	}
	// 原 JS: sort((a,b)=>a.toLowerCase().localeCompare(b.toLowerCase()) || a.localeCompare(b))
	wantIgnoreCase := []string{
		"adjust", "app_version", "appVer", "channel", "cKey", "cmd", "cnlid", "defn",
		"devid", "dtype", "encryptVer", "guid", "livepid", "otype", "platform", "sphttps", "stream",
	}
	got := sortKeysLikeJS(liveKeys, true)
	if strings.Join(got, ",") != strings.Join(wantIgnoreCase, ",") {
		t.Fatalf("忽略大小写排序\n got=%v\nwant=%v", got, wantIgnoreCase)
	}
	// 原 JS 最终签名用裸 .sort()：ASCII 码元序（大写在小写前）
	finalKeys := append(append([]string(nil), liveKeys...), "rand_str")
	wantFinal := []string{
		"adjust", "appVer", "app_version", "cKey", "channel", "cmd", "cnlid", "defn",
		"devid", "dtype", "encryptVer", "guid", "livepid", "otype", "platform", "rand_str", "sphttps", "stream",
	}
	gotFinal := sortKeysLikeJS(finalKeys, false)
	if strings.Join(gotFinal, ",") != strings.Join(wantFinal, ",") {
		t.Fatalf("裸排序\n got=%v\nwant=%v", gotFinal, wantFinal)
	}
}

// TestSignBodyGolden 验证 liveSignBody / 最终签名 / auth 签名三处 md5。
func TestSignBodyGolden(t *testing.T) {
	// --- liveSignBody（忽略大小写排序）---
	body := map[string]string{
		"adjust": "1", "appVer": appVersion, "app_version": appVersion, "cKey": "X",
		"channel": "ysp_tx", "cmd": "2", "cnlid": "2024078201", "defn": "fhd",
		"devid": "devid", "dtype": "1", "encryptVer": "8.1", "guid": "g",
		"livepid": "600001859", "otype": "ojson", "platform": "5910204",
		"sphttps": "1", "stream": "2",
	}
	keys := []string{
		"adjust", "appVer", "app_version", "cKey", "channel", "cmd", "cnlid", "defn",
		"devid", "dtype", "encryptVer", "guid", "livepid", "otype", "platform", "sphttps", "stream",
	}
	joined := joinSorted(body, true, keys)
	const wantJoined = "adjust=1&app_version=V1.0.0&appVer=V1.0.0&channel=ysp_tx&cKey=X&cmd=2&cnlid=2024078201&defn=fhd&devid=devid&dtype=1&encryptVer=8.1&guid=g&livepid=600001859&otype=ojson&platform=5910204&sphttps=1&stream=2"
	if joined != wantJoined {
		t.Fatalf("liveSignBody 拼接\n got=%s\nwant=%s", joined, wantJoined)
	}
	if got := md5Hex(joined); got != "9feccc70d636378badd6e06021baa446" {
		t.Fatalf("liveSignBody md5 = %s", got)
	}

	// --- 最终签名（裸排序 + 后缀）---
	finalBody := make(map[string]string, len(body)+1)
	for k, v := range body {
		finalBody[k] = v
	}
	finalBody["rand_str"] = "0123456789"
	finalKeys := append(append([]string(nil), keys...), "rand_str")
	finalJoined := joinSorted(finalBody, false, finalKeys) + liveSignSalt
	const wantFinal = "adjust=1&appVer=V1.0.0&app_version=V1.0.0&cKey=X&channel=ysp_tx&cmd=2&cnlid=2024078201&defn=fhd&devid=devid&dtype=1&encryptVer=8.1&guid=g&livepid=600001859&otype=ojson&platform=5910204&rand_str=0123456789&sphttps=1&stream=20f$IVHi9Qno?G"
	if finalJoined != wantFinal {
		t.Fatalf("最终签名串\n got=%s\nwant=%s", finalJoined, wantFinal)
	}
	if got := md5Hex(finalJoined); got != "a026d2c7b66581e8bb6f22e38bab0fa1" {
		t.Fatalf("最终签名 md5 = %s", got)
	}

	// --- auth 签名（裸排序 + n@7QKk%YeSjfw%22）---
	authBody := map[string]string{
		"pid": "600001859", "guid": "testguid_0123456789abcd",
		"appid": "ysp_pc", "rand_str": "0123456789",
	}
	authJoined := joinSorted(authBody, false, []string{"pid", "guid", "appid", "rand_str"}) + authSalt
	const wantAuth = "appid=ysp_pc&guid=testguid_0123456789abcd&pid=600001859&rand_str=0123456789n@7QKk%YeSjfw%22"
	if authJoined != wantAuth {
		t.Fatalf("auth 签名串\n got=%s\nwant=%s", authJoined, wantAuth)
	}
	if got := md5Hex(authJoined); got != "3480d0d4031ed9e943e423bd652f0263" {
		t.Fatalf("auth md5 = %s", got)
	}
}

// TestBase36Golden 验证 base36 与 node 的 BigInt 实现一致。
func TestBase36Golden(t *testing.T) {
	cases := map[int64]string{
		1700000000000: "loyw3v28",
		1:             "1",
		35:            "z",
		36:            "10",
		1295:          "zz",
		0:             "0",
		-5:            "0",
	}
	for in, want := range cases {
		if got := base36(in); got != want {
			t.Fatalf("base36(%d) = %q, 期望 %q", in, got, want)
		}
	}
}

// TestJSONPayloadOrderGolden 验证 get_live_info 的 JSON 字节顺序与原 JS 一致。
// 原 JS 依赖对象插入顺序，顺序错了上游签名校验会失败。
func TestJSONPayloadOrderGolden(t *testing.T) {
	keys := []string{
		"adjust", "appVer", "app_version", "cKey", "channel", "cmd", "cnlid", "defn",
		"devid", "dtype", "encryptVer", "guid", "livepid", "otype", "platform",
		"sphttps", "stream", "rand_str", "signature",
	}
	body := map[string]string{
		"adjust": "1", "appVer": "V1.0.0", "app_version": "V1.0.0", "cKey": "--01AB",
		"channel": "ysp_tx", "cmd": "2", "cnlid": "2024078201", "defn": "fhd",
		"devid": "devid", "dtype": "1", "encryptVer": "8.1", "guid": "g",
		"livepid": "600001859", "otype": "ojson", "platform": "5910204",
		"sphttps": "1", "stream": "2", "rand_str": "0123456789", "signature": "deadbeef",
	}
	got := buildOrderedJSON(keys, body, map[string]bool{"adjust": true})
	const want = `{"adjust":1,"appVer":"V1.0.0","app_version":"V1.0.0","cKey":"--01AB","channel":"ysp_tx","cmd":"2","cnlid":"2024078201","defn":"fhd","devid":"devid","dtype":"1","encryptVer":"8.1","guid":"g","livepid":"600001859","otype":"ojson","platform":"5910204","sphttps":"1","stream":"2","rand_str":"0123456789","signature":"deadbeef"}`
	if got != want {
		t.Fatalf("JSON 顺序\n got=%s\nwant=%s", got, want)
	}
}

// TestKeygenStatePerCall 验证「同一个 Engine 用不同 keygenValues 调用，结果随输入变化」。
//
// 这条测试专门防止 wbg 宿主函数从错误的 context 取状态（例如误用外层 ctx），
// 那种 bug 会让 getRnd/getSign 变成与输入无关的恒定值，非常隐蔽。
func TestKeygenStatePerCall(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	base := keygenValues{
		GUID: "guid-a_0123456789", YspAppID: yspAppID, Version: "v1",
		Host: "www.yangshipin.cn", Protocol: "https:", TS: "1700000000000",
	}
	rndA, err := e.getRnd(ctx, base)
	if err != nil {
		t.Fatalf("getRnd A: %v", err)
	}
	// 换 guid + ts：get_token_rnd 的输入变化，输出必须变化
	other := base
	other.GUID = "guid-b_9876543210"
	other.TS = "1800000000000"
	rndB, err := e.getRnd(ctx, other)
	if err != nil {
		t.Fatalf("getRnd B: %v", err)
	}
	if rndA == rndB {
		t.Fatalf("不同输入得到相同 rnd（%s）——说明 keygen 状态没有随 ctx 传递", rndA)
	}

	// get_signature 依赖 token/input，换 input 必须换签名
	s1 := base
	s1.Token = "token-1"
	s1.Input = "input-one"
	signA, err := e.getSign(ctx, s1)
	if err != nil {
		t.Fatalf("getSign A: %v", err)
	}
	s2 := s1
	s2.Input = "input-two"
	signB, err := e.getSign(ctx, s2)
	if err != nil {
		t.Fatalf("getSign B: %v", err)
	}
	if signA == signB {
		t.Fatalf("不同 input 得到相同签名（%s）——说明 keygen 状态没有随 ctx 传递", signA)
	}
	// 相同输入必须稳定
	signA2, err := e.getSign(ctx, s1)
	if err != nil {
		t.Fatalf("getSign A2: %v", err)
	}
	if signA != signA2 {
		t.Fatalf("相同输入签名不稳定: %s != %s", signA, signA2)
	}
}

// TestTicketUniquePerGUID 并发回归测试：
// N 个 goroutine 同时 buildTicket（guid 不同），每个输出长度正确且互不相同。
//
// 这条测试锁定"每次调用新建 a 实例"的设计：只要有人把 a/ticket 改回共享单实例，
// 轻则输出互相污染/相同，重则直接 out of bounds。
func TestTicketUniquePerGUID(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	const n = 16
	type result struct {
		idx int
		out string
		err error
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			guid := fmt.Sprintf("conc%02d_0123456789ab", i)
			out, err := e.buildTicket(ctx, "600001859", "1700000000", "2024078201", guid)
			results[i] = result{idx: i, out: out, err: err}
		}(i)
	}
	wg.Wait()

	seen := make(map[string]int, n)
	// 期望长度：outLen = len(pid)+len(auth_ts)+len(guid)+len(yspappid)+14，
	// 这里 guid 形如 "conc00_0123456789ab"，共 19 字符，结果再转 hex（×2）。
	guidLen := len("conc00_0123456789ab")
	wantLen := 2 * (len("600001859") + len("1700000000") + guidLen + len(yspAppID) + 14)
	for _, r := range results {
		if r.err != nil {
			t.Fatalf("goroutine %d buildTicket: %v", r.idx, r.err)
		}
		if len(r.out) != wantLen {
			t.Fatalf("goroutine %d ticket 长度 = %d, 期望 %d", r.idx, len(r.out), wantLen)
		}
		if prev, dup := seen[r.out]; dup {
			t.Fatalf("goroutine %d 与 %d 输出了相同票据（共享了 WASM 实例内存）", r.idx, prev)
		}
		seen[r.out] = r.idx
	}
	if len(seen) != n {
		t.Fatalf("唯一票据数 = %d, 期望 %d", len(seen), n)
	}
}

// TestJSStringSemantics 验证 jsString 与原 JS `String(v)` 的语义一致。
//
// 覆盖 get_live_info 上游可能返回的 auth/data.ts 三种形态：
// 数字 1700000000、字符串 "1700000000"、浮点 1700000000.0 ——
// 三者经 String() 后都必须是 "1700000000"（不能是 "1.7e+09"、不能带引号）。
func TestJSStringSemantics(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		// 数字
		{`1700000000`, "1700000000"},
		// 字符串
		{`"1700000000"`, "1700000000"},
		// 浮点（JS: String(1700000000.0) === "1700000000"）
		{`1700000000.0`, "1700000000"},
		{`1700000000123.0`, "1700000000123"},
		// 其它标量
		{`0`, "0"},
		{`-5`, "-5"},
		{`1.5`, "1.5"},
		{`1e21`, "1e+21"},
		{`1e-7`, "1e-7"},
		{`true`, "true"},
		{`false`, "false"},
		{`null`, "null"},
		{`"中文"`, "中文"},
		// 缺失（undefined）
		{``, "undefined"},
	}
	for _, c := range cases {
		got := jsString(json.RawMessage(c.raw))
		if got != c.want {
			t.Fatalf("jsString(%s) = %q, 期望 %q", c.raw, got, c.want)
		}
	}
	// 浮点形态必须与整数形态长度一致（防止 1.7e+09 这种科学计数法渗入）
	if len(jsString(json.RawMessage(`1700000000.0`))) != len("1700000000") {
		t.Fatal("浮点 ts 转成的字符串长度与 JS String() 不一致")
	}
}

// TestJSTruthySemantics 验证 `tokenJson.data.ts || nowMs` 的真值判断。
func TestJSTruthySemantics(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{`1700000000`, true},
		{`"1700000000000"`, true},
		{`0`, false},
		{`""`, false},
		{`null`, false},
		{`false`, false},
		{``, false}, // undefined
	}
	for _, c := range cases {
		if got := jsTruthy(json.RawMessage(c.raw)); got != c.want {
			t.Fatalf("jsTruthy(%s) = %v, 期望 %v", c.raw, got, c.want)
		}
	}
}

// TestWasmVisibleString 验证非字符串标量进 wasm 后为空串的语义。
func TestWasmVisibleString(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{`"1700000000000"`, "1700000000000"},
		{`1700000000000`, ""}, // 数字：typeof !== 'string' → wasm 侧空串
		{`null`, ""},          // 同上
		{`true`, ""},          // 同上
	}
	for _, c := range cases {
		if got := wasmVisibleString(json.RawMessage(c.raw)); got != c.want {
			t.Fatalf("wasmVisibleString(%s) = %q, 期望 %q", c.raw, got, c.want)
		}
	}
}
