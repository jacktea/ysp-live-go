package crypto

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
// TEA / cKey / ck_guard —— 复刻原项目 bkliveinfo 备用线路的签名算法。
// 对应 Python: _tea_blk / _cksum / _tea_pkt / _ck_guard / _ckey / bk_playurls
// ============================================================================

const (
	ckPlatform = 4330403
	ckAppVer   = "V8.22.1035.3031"
)

var (
	ckTEA  = mustHex("59b2f7cf725ef43c34fdd7c123411ed3")
	ckGTEA = mustHex("110DBEC10C23E7D2E56A1CAD6914EF1B")
	ckXOR  = []byte{132, 46, 237, 8, 240, 102, 230, 234, 72, 180, 202, 169, 145, 237, 111, 243}
	ckGXOR = []byte{179, 201, 83, 160, 105, 19, 173, 77}
)

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// teaBlk TEA 单块加密（大端 2xuint32 输入输出）。
func teaBlk(blk, key []byte) []byte {
	y := binary.BigEndian.Uint32(blk[0:4])
	z := binary.BigEndian.Uint32(blk[4:8])
	k0 := binary.BigEndian.Uint32(key[0:4])
	k1 := binary.BigEndian.Uint32(key[4:8])
	k2 := binary.BigEndian.Uint32(key[8:12])
	k3 := binary.BigEndian.Uint32(key[12:16])
	var sum uint32
	for i := 0; i < 16; i++ {
		sum += 2654435769
		y += ((z << 4) + k0) ^ (z + sum) ^ ((z >> 5) + k1)
		z += ((y << 4) + k2) ^ (y + sum) ^ ((y >> 5) + k3)
	}
	out := make([]byte, 8)
	binary.BigEndian.PutUint32(out[0:4], y)
	binary.BigEndian.PutUint32(out[4:8], z)
	return out
}

// cksum 复刻原 _cksum：v = 131*v + b (mod 2^31-1)。
func cksum(buf []byte) uint32 {
	var v int64
	for _, b := range buf {
		v = (131*v + int64(b)) & 0x7FFFFFFF
	}
	return uint32(v)
}

// teaPkt TEA-CBC 打包（前后各带 8 字节空 IV，随机填充）。
func teaPkt(data, key []byte) []byte {
	pad := (8 - (len(data)+10)%8) % 8
	plain := make([]byte, 0, 1+pad+2+len(data)+7)
	rb := make([]byte, 1)
	_, _ = rand.Read(rb)
	plain = append(plain, rb[0]&248|byte(pad))
	rnd := make([]byte, pad+2)
	_, _ = rand.Read(rnd)
	plain = append(plain, rnd...)
	plain = append(plain, data...)
	plain = append(plain, make([]byte, 7)...)

	pp := make([]byte, 8)
	pc := make([]byte, 8)
	out := make([]byte, 0, len(plain))
	for off := 0; off < len(plain); off += 8 {
		blk := plain[off : off+8]
		mixed := make([]byte, 8)
		for i := 0; i < 8; i++ {
			mixed[i] = blk[i] ^ pc[i]
		}
		enc := teaBlk(mixed, key)
		cipherBlk := make([]byte, 8)
		for i := 0; i < 8; i++ {
			cipherBlk[i] = enc[i] ^ pp[i]
		}
		out = append(out, cipherBlk...)
		pp, pc = mixed, cipherBlk
	}
	return out
}

// lp 长度前缀（大端 uint16）。
func lp(s string) []byte {
	out := make([]byte, 2+len(s))
	binary.BigEndian.PutUint16(out[0:2], uint16(len(s)))
	copy(out[2:], s)
	return out
}

func u32be(v uint32) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, v)
	return out
}

func tail5(v string) string {
	if len(v) >= 5 {
		return v[len(v)-5:]
	}
	return ""
}

// ckGuard 生成 cKey 的 guard 字段。
func ckGuard(ts int64, guid string) string {
	var body []byte
	body = append(body, u32be(uint32(ts))...)
	body = append(body, lp(tail5(guid))...)
	body = append(body, lp(tail5("null"))...)
	body = append(body, lp(tail5("null"))...)
	body = append(body, lp("-1")...)

	plain := lp(string(body))
	enc := append(teaPkt(plain, ckGTEA), u32be(cksum(plain))...)
	for i := range enc {
		enc[i] ^= ckGXOR[i&7]
	}
	return strings.ToUpper(hex.EncodeToString(enc))
}

// CKey 生成 bkliveinfo 请求所需的 cKey / guid / ts / flowId。
func CKey(channelID string) map[string]string {
	ts := time.Now().Unix()
	guidBytes := make([]byte, 16)
	_, _ = rand.Read(guidBytes)
	guid := hex.EncodeToString(guidBytes)
	guard := ckGuard(ts, guid)
	uidBytes := make([]byte, 4)
	_, _ = rand.Read(uidBytes)
	uid := strings.ToUpper(hex.EncodeToString(uidBytes))

	var body []byte
	body = append(body, mustHex("0000004200000004000004d2")...)
	body = append(body, u32be(ckPlatform)...)
	body = append(body, u32be(0)...)
	body = append(body, u32be(uint32(ts))...)
	body = append(body, lp("dcgh")...)
	body = append(body, lp("_zj1A5Gh6QYcxWjIUGos2w==")...)
	body = append(body, lp(ckAppVer)...)
	body = append(body, lp(channelID)...)
	body = append(body, lp(guid)...)
	body = append(body, u32be(1)...)
	body = append(body, u32be(1)...)
	body = append(body, lp(uid)...)
	body = append(body, lp("nil")...)
	body = append(body, lp("57eab0c4-2c58-44c6-8ae9-dd2757525dc5")...)
	body = append(body, lp("nil")...)
	body = append(body, lp("v0.1.000")...)
	body = append(body, lp("com.cctv.yangshipin.app.iphone")...)
	body = append(body, lp(strconv.Itoa(ckPlatform))...)
	body = append(body, lp("ex_json_bus")...)
	body = append(body, lp("ex_json_vs")...)
	body = append(body, lp(guard)...)

	pkt := make([]byte, 2+len(body))
	binary.BigEndian.PutUint16(pkt[0:2], uint16(len(body)))
	copy(pkt[2:], body)
	binary.BigEndian.PutUint32(pkt[18:22], cksum(pkt))

	enc := append(teaPkt(pkt, ckTEA), u32be(cksum(pkt))...)
	for i := range enc {
		enc[i] ^= ckXOR[i&15]
	}
	b64 := base64.StdEncoding.EncodeToString(enc)
	b64 = strings.ReplaceAll(b64, "+", "_")
	b64 = strings.ReplaceAll(b64, "/", "-")
	b64 = strings.TrimRight(b64, "=")

	return map[string]string{
		"cKey":   "--01" + b64,
		"guid":   guid,
		"ts":     strconv.FormatInt(ts, 10),
		"flowId": fmt.Sprintf("%s_%d", strings.ToUpper(RandomHex(32)), ckPlatform),
	}
}

// BKPlayURLs 通过 bkliveinfo 接口获取备用播放地址列表。
// 对应原 bk_playurls 的 URL 构造部分（HTTP 请求由调用方执行，便于注入 client）。
func BKPlayURLsQuery(channelID, livePID, defn, cKey, guid, ts, flowID string) string {
	h264 := base64.StdEncoding.EncodeToString([]byte("H(30:1080,60:1080|30:1080,60:1080)"))
	q := url.Values{}
	// 注意：原项目用 urllib.parse.urlencode 构造且保持插入顺序，这里用有序切片保证签名一致。
	pairs := [][2]string{
		{"atime", "120"},
		{"livepid", livePID},
		{"cnlid", channelID},
		{"appVer", ckAppVer},
		{"app_version", "300090"},
		{"caplv", "1"},
		{"cmd", "2"},
		{"defn", defn},
		{"device", "iPhone"},
		{"encryptVer", "4.2"},
		{"getpreviewinfo", "0"},
		{"hevclv", "0"},
		{"lang", "zh-Hans_CN"},
		{"livequeue", "0"},
		{"logintype", "1"},
		{"nettype", "1"},
		{"newnettype", "1"},
		{"newplatform", strconv.Itoa(ckPlatform)},
		{"platform", strconv.Itoa(ckPlatform)},
		{"sdtfrom", "v3021"},
		{"spacode", "23"},
		{"spaudio", "1"},
		{"spdemuxer", "6"},
		{"spdrm", "2"},
		{"spdynamicrange", "1"},
		{"spflv", "1"},
		{"spflvaudio", "1"},
		{"sphdrfps", "60"},
		{"sphttps", "1"},
		{"spvcode", h264},
		{"spvideo", "4"},
		{"stream", "1"},
		{"system", "1"},
		{"sysver", "ios18.2.1"},
		{"uhd_flag", "0"},
		{"cKey", cKey},
		{"guid", guid},
		{"fntick", ts},
		{"flowid", flowID},
		{"playbacktime", "0"},
	}
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(p[0]))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(p[1]))
	}
	_ = q
	return b.String()
}
