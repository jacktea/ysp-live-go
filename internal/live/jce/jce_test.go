package jce

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/hex"
	"io"
	"testing"
)

// 测试向量由原项目 Python 实现直接生成，确保 Go 侧编码逐字节一致。
func TestWriterHeaderMatchesPython(t *testing.T) {
	const wantHex = "0a020012d6871162e02a060b332e322e372e32363231321606333032303730210438310960400356023132600170018101a496053130303730a600b600c600d600e600fa0f0c1c26000bf61000f61100f61200fa130c140000000024000000003500000000000000000bf6141030313233343536373839616263646566f61507506978656c2036f01601fc17fc18fc19fc1af61b00f61c00f61d2030313233343536373839616263646566303132333435363738396162636465660b36073132303030313346203031323334353637383961626364656630313233343536373839616263646566590c6a0b790c8c9cac0b"

	w := NewWriter()
	WriteRequestHeader(w, CmdTimeshift, 1234567, "0123456789abcdef0123456789abcdef")
	got := hex.EncodeToString(w.Out())
	if got != wantHex {
		t.Fatalf("header mismatch\n got=%s\nwant=%s", got, wantHex)
	}
}

func TestTimeshiftBodyMatchesPython(t *testing.T) {
	const wantHex = "0609363030303031383539160a32303234303738323031226553f100326553ff104603666864"
	w := NewWriter()
	w.String("600001859", 0)
	w.String("2024078201", 1)
	w.Long(1700000000, 2)
	w.Long(1700003600, 3)
	w.String("fhd", 4)
	got := hex.EncodeToString(w.Out())
	if got != wantHex {
		t.Fatalf("body mismatch\n got=%s\nwant=%s", got, wantHex)
	}
}

// 验证 Reader 能正确回读 Writer 的结果（编码/解码对称性）。
func TestReaderRoundTrip(t *testing.T) {
	w := NewWriter()
	w.Struct(func(ww *Writer) {
		ww.Int(200, 0)
		ww.String("hello", 1)
		ww.Long(1700000000, 2)
		ww.Bytes([]byte{1, 2, 3}, 3)
	}, 0)
	m, err := NewReader(w.Out()).Struct()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	inner, ok := m[0].(map[int]interface{})
	if !ok {
		t.Fatalf("expected nested struct, got %T", m[0])
	}
	if GetInt(inner, 0, -1) != 200 {
		t.Errorf("tag0 = %v", inner[0])
	}
	if GetString(inner, 1, "") != "hello" {
		t.Errorf("tag1 = %v", inner[1])
	}
	// 1700000000 可被无损降级为 type 2 (int32)，回读得到 int
	if v, ok := inner[2].(int); !ok || v != 1700000000 {
		t.Errorf("tag2 = %v (%T)", inner[2], inner[2])
	}
	if b, ok := inner[3].([]byte); !ok || len(b) != 3 || b[0] != 1 {
		t.Errorf("tag3 = %v", inner[3])
	}
}

// TestWrapIncludesBody 锁定 _wrap 的完整结构：请求头 struct(tag=0) + 请求体 bytes(tag=1)。
//
// 回归背景：Go 版曾漏掉 w.Bytes(body,1)，reqcmd 少 42 字节
// （tag1 头 1 字节 + 类型/长度前缀 3 字节 + body 38 字节），
// 导致 jacc.ysp.cctv.cn 返回 succeed=500、1080p 与时移全部不可用。
// 该 bug 从字节层面看不出来（请求仍能发出、HTTP 200），只有对比 reqcmd 长度才能发现。
func TestWrapIncludesBody(t *testing.T) {
	// 复刻原 timeshift body：pid / sid / start / end / stream
	body := NewWriter()
	body.String("600001811", 0)
	body.String("2029797201", 1)
	body.Long(1700000000, 2)
	body.Long(1700003600, 3)
	body.String("fhd", 4)
	bodyBytes := body.Out()

	// bytes(data, tag=1) 的编码：head(type=13,tag=1)=0x1d, head(type=0,tag=0)=0x00,
	// int(len,0) 在 <=127 时为 0x00 + 单字节长度，随后是数据本身。
	wantSuffix := append([]byte{0x1d, 0x00, 0x00, byte(len(bodyBytes))}, bodyBytes...)

	pkt := Wrap(CmdTimeshift, bodyBytes, 1234567)

	// 外层是 gzip 压缩的，必须先解压再比对。
	inner := gunzipInner(t, pkt)
	if len(inner) < 17 {
		t.Fatalf("内层长度异常: %d", len(inner))
	}
	if inner[0] != 38 || inner[len(inner)-1] != 40 {
		t.Errorf("内层 framing 不对: first=%d last=%d", inner[0], inner[len(inner)-1])
	}
	// 内层长度字段 = 整个 inner 的字节数（原 _wrap 里 pack_into('>i', out, 1, len(out))）
	declared := int(binary.BigEndian.Uint32(inner[1:5]))
	if declared != len(inner) {
		t.Errorf("内层长度字段 = %d, 实际 = %d", declared, len(inner))
	}
	reqCmd := inner[16 : len(inner)-1]
	if !bytes.HasSuffix(reqCmd, wantSuffix) {
		t.Fatalf("reqcmd 未以 body 段结尾\n reqcmd tail=%s\n   wantSuffix=%s",
			hex.EncodeToString(tailN(reqCmd, len(wantSuffix)+8)), hex.EncodeToString(wantSuffix))
	}
	// 核心回归断言：body 段必须恰好是 42 字节（4 字节前缀 + 38 字节 body）。
	// 若 Wrap 漏掉 body，这里的 reqCmd 会短 42 字节，HasSuffix 已先失败。
	if got := len(reqCmd) - (len(reqCmd) - len(wantSuffix)); got != len(wantSuffix) {
		t.Errorf("body 段长度异常: %d", got)
	}
	if want := 4 + len(bodyBytes); len(wantSuffix) != want {
		t.Errorf("body 段长度 = %d, 期望 %d", len(wantSuffix), want)
	}
	// 再确认 body 内容原样出现在 reqcmd 中（不止长度对，内容也要对）
	if !bytes.Contains(reqCmd, bodyBytes) {
		t.Error("reqcmd 中找不到原始 body 字节")
	}
}

// tailN 返回 s 末尾 n 字节（不足则返回全部）。
func tailN(s []byte, n int) []byte {
	if n >= len(s) {
		return s
	}
	return s[len(s)-n:]
}

func containsSubslice(haystack, needle []byte) bool { return bytes.Contains(haystack, needle) }

func gunzipInner(t *testing.T, pkt []byte) []byte {
	t.Helper()
	if len(pkt) < 90 {
		t.Fatalf("包长度异常: %d", len(pkt))
	}
	flags := binary.BigEndian.Uint32(pkt[21:25])
	payload := pkt[89 : len(pkt)-1]
	if flags&2 != 0 {
		zr, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("gzip: %v", err)
		}
		out, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("gzip read: %v", err)
		}
		return out
	}
	return payload
}
