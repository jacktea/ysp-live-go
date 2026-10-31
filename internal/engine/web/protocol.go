package web

// 本文件移植原 ysp-engine.js 的 resolveChannelLive()（第 293-428 行）与
// 它依赖的 base36 / randStr / buildCKey，是网页版兜底引擎的协议主体。
//
// 调用链（与原 JS 完全一致）：
//   guid = base36(now) + '_' + randStr(13)
//   → POST /v1/player/auth               取 yspPlayerToken(authJson.data.token)、authJson.data.ts
//   → keygen#1 get_token_rnd             → GET h5access/open/token 取 openapi token、ts
//   → buildCKey(cnlid, ts, guid)         AES-128-CBC，得到 cKey
//   → liveSignBody 排序后 md5            → sdkInput = md5 + '-' + guid + '-1-' + reqId
//   → keygen#2 get_signature             → yspsdksign = sign + '-' + sdkInput
//   → buildTicket(livepid, authTs, cnlid, guid) 取 hex 票据
//   → POST /v1/player/get_live_info      取 playurl + extended_param
//   → GET playurl                        校验 #EXTM3U → 改写分片地址

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"github.com/jacktea/ysp-live-go/internal/crypto"
	"github.com/jacktea/ysp-live-go/internal/engine"
	"github.com/jacktea/ysp-live-go/internal/util"
)

// randCharset 原 JS randStr / base36 使用的字符集。
const randCharset = "0123456789abcdefghijklmnopqrstuvwxyz"

// md5Hex 小写十六进制 md5（对应 JS crypto.createHash('md5').update(s).digest('hex')）。
func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// randStr 对应原 JS randStr(n)：从 0-9a-z 里随机取 n 个字符。
func randStr(n int) string {
	return util.RandomFromCharset(n, randCharset)
}

// base36 对应原 JS base36(val)：十进制 → 36 进制小写，val<=0 时返回 "0"。
func base36(val int64) string {
	if val <= 0 {
		return "0"
	}
	return new(big.Int).SetInt64(val).Text(36)
}

// buildCKey 对应原 JS buildCKey(cnlid, ts, guid)：
// 14 段明文用 '|' 连接 → Java hashCode → '|' + hash + 明文 → AES-128-CBC(PKCS#7)
// → "--01" + 大写十六进制密文。
func buildCKey(cnlid, ts, guid string) (string, error) {
	// 原 JS 的 hashSource 是 14 段（首尾各一个空串）拼接。
	hashSource := strings.Join([]string{
		"", cnlid, ts, "mg3c3b04ba", appVersion, guid, "5910204",
		"https://www.yangshipin.c", "mozilla/5.0 (macintosh; ", "", "Mozilla", "Netscape", "MacIntel", "",
	}, "|")
	// 原 JS: hash = ((hash << 5) - hash + charCode) | 0（32 位有符号回绕）
	hash := crypto.JavaStringHashCode(hashSource)
	// 原 JS: const plain = '|' + hash + hashSource;
	plain := "|" + fmt.Sprintf("%d", hash) + hashSource

	key, err := hex.DecodeString(videoAuthKeyHex)
	if err != nil {
		return "", fmt.Errorf("%w: cKey 密钥非法: %v", ErrEngine, err)
	}
	iv, err := hex.DecodeString(videoAuthIVHex)
	if err != nil {
		return "", fmt.Errorf("%w: cKey IV 非法: %v", ErrEngine, err)
	}
	enc, err := crypto.AES128CBCEncryptPKCS7Padded(key, iv, []byte(plain))
	if err != nil {
		return "", fmt.Errorf("%w: cKey 加密失败: %v", ErrEngine, err)
	}
	return "--01" + strings.ToUpper(hex.EncodeToString(enc)), nil
}

// sortKeysLikeJS 复刻原 JS 的两种排序：
//
//   - liveSignBody 用 `sort((a,b)=>a.toLowerCase().localeCompare(b.toLowerCase()) || a.localeCompare(b))`
//     → 忽略大小写比较，相同再按原样比较（本函数 ignoreCase=true）；
//   - 最终签名用裸 `.sort()` → 按 UTF-16 码元（ASCII 下等同 Go 的字节序）。
func sortKeysLikeJS(keys []string, ignoreCase bool) []string {
	out := append([]string(nil), keys...)
	if !ignoreCase {
		sort.Strings(out)
		return out
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		la, lb := strings.ToLower(a), strings.ToLower(b)
		if la != lb {
			return la < lb
		}
		return a < b
	})
	return out
}

// joinSorted 对应 JS 的 `keys.map(k => k + '=' + obj[k]).join('&')`。
func joinSorted(body map[string]string, ignoreCase bool, keys []string) string {
	keys = sortKeysLikeJS(keys, ignoreCase)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+body[k])
	}
	return strings.Join(parts, "&")
}

// openAPIResponse h5access/open/token 的响应体。
type openAPIResponse struct {
	Data struct {
		// Token 等价 JS 的 tokenJson.data.token。
		Token string `json:"token"`
		// TS 等价 JS 的 tokenJson.data.ts —— 可能是字符串也可能是数字，
		// 用 RawMessage 保留原始类型（见 keygenTS 的处理说明）。
		TS json.RawMessage `json:"ts"`
	} `json:"data"`
}

// playerAuthResponse /v1/player/auth 的响应体。
type playerAuthResponse struct {
	Data *struct {
		Token string          `json:"token"`
		TS    json.RawMessage `json:"ts"`
	} `json:"data"`
}

// liveInfoResponse /v1/player/get_live_info 的响应体。
type liveInfoResponse struct {
	Data *struct {
		PlayURL       string `json:"playurl"`
		ExtendedParam string `json:"extended_param"`
	} `json:"data"`
}

// jsString 复刻 JS 的 String(v)（v 来自 JSON 标量）：
// 字符串取原值、数字取十进制字面量、布尔取 true/false、
// null → "null"、缺失(undefined) → "undefined"。
func jsString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "undefined"
	}
	var v interface{}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return ""
	}
	switch val := v.(type) {
	case nil:
		return "null"
	case string:
		return val
	case json.Number:
		return jsNumberString(val.String())
	case bool:
		if val {
			return "true"
		}
		return "false"
	}
	return ""
}

// jsNumberString 复刻 JS 的 String(Number) 语义：
//
//   - 整数字面量原样返回（JSON 里 1700000000 就是 "1700000000"）；
//   - 浮点字面量先归一：JS 里 String(1700000000.0) === "1700000000"，
//     而 Go 若用 json.Number 原样返回会得到 "1700000000.0"，长度就错了；
//   - 不用科学计数法输出 1e9 量级（Go 的 %g 会给出 1.7e+09，与 JS 不同）。
//
// JS 的数字转字符串规则（ECMA-262 Number::toString）：
// |x| < 1e21 用定点表示，否则用指数表示。
func jsNumberString(lit string) string {
	// 纯整数字面量直接返回，避免不必要的浮点精度损失。
	if !strings.ContainsAny(lit, ".eE") {
		return lit
	}
	f, err := strconv.ParseFloat(lit, 64)
	if err != nil {
		return lit
	}
	// NaN/Inf 在 JSON 里不合法，但防御性处理成 JS 的字符串形式。
	if math.IsNaN(f) {
		return "NaN"
	}
	if math.IsInf(f, 1) {
		return "Infinity"
	}
	if math.IsInf(f, -1) {
		return "-Infinity"
	}
	neg := false
	if f < 0 || (f == 0 && math.Signbit(f)) {
		neg = f < 0
		f = -f
	}
	var out string
	switch {
	case f == 0:
		out = "0"
	case f >= 1e-6 && f < 1e21:
		// JS 在 1e-6 <= |x| < 1e21 区间用定点表示；
		// -1 精度让 Go 输出最短且能回读的十进制（与 JS 一致）。
		out = strconv.FormatFloat(f, 'f', -1, 64)
	default:
		// 其余区间用指数表示，JS 形如 1e-7 / 1e+21。
		// Go 的 'e' 会输出 1e-07（两位指数），JS 输出 1e-7，这里去掉指数前导 0。
		out = normalizeExponent(strconv.FormatFloat(f, 'e', -1, 64))
	}
	if neg {
		return "-" + out
	}
	return out
}

// normalizeExponent 把 Go 的 "1e-07"/"1e+21" 规范成 JS 的 "1e-7"/"1e+21"。
func normalizeExponent(s string) string {
	i := strings.IndexAny(s, "eE")
	if i < 0 {
		return s
	}
	mant, exp := s[:i], s[i+1:]
	sign := ""
	if len(exp) > 0 && (exp[0] == '+' || exp[0] == '-') {
		sign, exp = string(exp[0]), exp[1:]
	}
	exp = strings.TrimLeft(exp, "0")
	if exp == "" {
		exp = "0"
	}
	return mant + "e" + sign + exp
}

// jsTruthy 复刻 JS 的真值判断（用于原 JS 的 `tokenJson.data.ts || nowMs`）。
func jsTruthy(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var v interface{}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return false
	}
	switch val := v.(type) {
	case nil:
		return false
	case string:
		return val != ""
	case json.Number:
		f, err := val.Float64()
		return err != nil || f != 0
	case bool:
		return val
	}
	return false
}

// wasmVisibleString 计算 wasm-bindgen 侧真正看到的字符串：
// 原 JS 把这批值原样（不做 String() 转换）塞进 values 映射，
// 而 __wbindgen_string_get 只在 `typeof val === 'string'` 时写回内容，
// 数字/布尔/null 都会被当成非字符串 → 最终写成空串。
func wasmVisibleString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}

// resolveLive 对应原 JS resolveChannelLive(slug)，返回已改写分片的 m3u8 文本。
//
// 返回 (nil, nil) 表示频道没有网页版元数据（等价 JS `return null`）；
// 其余错误都带原项目风格的文字前缀。
func (e *Engine) resolveLive(ctx context.Context, slug string) (*engine.Playlist, error) {
	canonical := canonicalSlug(slug)
	meta, ok := channels[canonical]
	if !ok {
		// 原 JS: if (!meta) return null;
		return nil, nil
	}

	now := e.now()
	// 原 JS: const cached = playlistCache.get(canonical); if (cached && now - cached.time < 30000)
	if cached, ok := e.cache.Get(canonical, now); ok {
		return &engine.Playlist{Body: cached, ContentType: m3u8ContentType, Source: engine.SourceWeb}, nil
	}

	nowMs := now.UnixMilli()
	// 原 JS: const guid = base36(now) + '_' + randStr(13);
	guid := base36(nowMs) + "_" + randStr(13)
	// 原 JS: const cookie = ['guid='+guid, 'versionName=99.99.99', ...].join('; ');
	cookie := strings.Join([]string{
		"guid=" + guid, "versionName=99.99.99", "versionCode=999999", "vplatform=109",
		"platformVersion=Chrome", "deviceModel=148", "newLogin=1", "pc_version=1.1.16",
	}, "; ")

	// ---------- 1) /v1/player/auth ----------
	authBody := map[string]string{
		"pid":      meta.LivePid,
		"guid":     guid,
		"appid":    "ysp_pc",
		"rand_str": randStr(10),
	}
	// 原 JS: 键按 .sort() 升序（默认码元序）拼接，再追加 n@7QKk%YeSjfw%22
	authSorted := joinSorted(authBody, false, []string{"pid", "guid", "appid", "rand_str"}) + authSalt
	// 原 JS: new URLSearchParams(authBody).toString()
	// 按对象插入顺序编码：pid → guid → appid → rand_str → signature
	authPayload := formEncodeOrdered([][2]string{
		{"pid", authBody["pid"]},
		{"guid", guid},
		{"appid", "ysp_pc"},
		{"rand_str", authBody["rand_str"]},
		{"signature", md5Hex(authSorted)},
	})

	authRes, err := e.doRequest(ctx, "POST", playerAuthURL, map[string]string{
		"origin":       yangshipinOrigin,
		"referer":      yangshipinRefer,
		"user-agent":   e.cfg.UA,
		"cookie":       cookie,
		"yspappid":     yspAppID,
		"content-type": "application/x-www-form-urlencoded;charset=UTF-8",
	}, authPayload)
	if err != nil {
		return nil, fmt.Errorf("%w: auth 请求失败: %v", ErrEngine, err)
	}
	if authRes.Status != 200 {
		return nil, fmt.Errorf("%w: auth HTTP %d", ErrEngine, authRes.Status)
	}
	var authJSON playerAuthResponse
	if err = json.Unmarshal(authRes.Body, &authJSON); err != nil {
		return nil, fmt.Errorf("%w: auth 响应不是合法 JSON: %v", ErrEngine, err)
	}
	if authJSON.Data == nil || authJSON.Data.Token == "" {
		// 原 JS: throw new Error('auth failed')
		return nil, fmt.Errorf("%w: auth failed", ErrEngine)
	}
	playerToken := authJSON.Data.Token
	// 原 JS: buildTicket(..., String(authJson.data.ts), ...) —— 显式 String() 转换
	authTS := jsString(authJSON.Data.TS)

	// ---------- 2) keygen#1 → /web/open/token ----------
	kgValues := keygenValues{
		GUID: guid, YspAppID: yspAppID, Version: "v1",
		Host: "www.yangshipin.cn", Protocol: "https:",
	}
	rnd, err := e.getRnd(ctx, kgValues)
	if err != nil {
		return nil, fmt.Errorf("%w: keygen 取 rnd 失败: %v", ErrEngine, err)
	}
	tsValue := fmt.Sprintf("%d", nowMs)
	tokenURL := openTokenURL + "?yspappid=" + yspAppID + "&guid=" + guid +
		"&vappid=59306155&vsecret=b42702bf7309a179d102f3d51b1add2fda0bc7ada64cb801" +
		"&raw=1&version=v1&ts=" + tsValue + "&rnd=" + rnd
	tokenRes, err := e.doRequest(ctx, "GET", tokenURL, map[string]string{
		"referer":    yangshipinRefer,
		"user-agent": e.cfg.UA,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: open/token 请求失败: %v", ErrEngine, err)
	}
	if tokenRes.Status != 200 {
		return nil, fmt.Errorf("%w: open/token HTTP %d", ErrEngine, tokenRes.Status)
	}
	var tokenJSON openAPIResponse
	if err = json.Unmarshal(tokenRes.Body, &tokenJSON); err != nil {
		return nil, fmt.Errorf("%w: open/token 响应不是合法 JSON: %v", ErrEngine, err)
	}
	openAPIToken := tokenJSON.Data.Token
	if openAPIToken == "" {
		return nil, fmt.Errorf("%w: open/token 未返回 token", ErrEngine)
	}
	// 原 JS: ts: tokenJson.data.ts || nowMs
	// 注意两点，缺一不可：
	//  1) `||` 的真值判断：null/""/0/false 都回落到 nowMs；
	//  2) 原 JS 没做 String() 转换，数字类型进到 wasm-bindgen 里
	//     `typeof val === 'string'` 为假 → 实际传给 Rust 的是空串。
	kgValues.Token = openAPIToken
	if !jsTruthy(tokenJSON.Data.TS) {
		kgValues.TS = tsValue
	} else {
		kgValues.TS = wasmVisibleString(tokenJSON.Data.TS)
	}

	// ---------- 3) buildCKey + liveSignBody 签名 ----------
	reqTs := now.Unix() // 原 JS: Math.floor(now / 1000)
	ckey, err := buildCKey(meta.CnLid, fmt.Sprintf("%d", reqTs), guid)
	if err != nil {
		return nil, err
	}
	defn := meta.Defn
	if defn == "" {
		defn = "fhd" // 原 JS: meta.defn || 'fhd'
	}
	// 原 JS liveSignBody 的字段与插入顺序（决定 JSON.stringify 的顺序）。
	liveSignKeys := []string{
		"adjust", "appVer", "app_version", "cKey", "channel", "cmd", "cnlid", "defn",
		"devid", "dtype", "encryptVer", "guid", "livepid", "otype", "platform", "sphttps", "stream",
	}
	liveSignBody := map[string]string{
		"adjust":      "1",
		"appVer":      appVersion,
		"app_version": appVersion,
		"cKey":        ckey,
		"channel":     "ysp_tx",
		"cmd":         "2",
		"cnlid":       meta.CnLid,
		"defn":        defn,
		"devid":       "devid",
		"dtype":       "1",
		"encryptVer":  "8.1",
		"guid":        guid,
		"livepid":     meta.LivePid,
		"otype":       "ojson",
		"platform":    "5910204",
		"sphttps":     "1",
		"stream":      "2",
	}
	// 原 JS: bodyKeys = keys.sort(忽略大小写 localeCompare + 原样兜底)
	sortedPairs := joinSorted(liveSignBody, true, liveSignKeys)
	bodyMd5 := md5Hex(sortedPairs)
	reqID := "999999" + randStr(10) + fmt.Sprintf("%d", nowMs)
	sdkInput := bodyMd5 + "-" + guid + "-1-" + reqID

	// ---------- 4) keygen#2 取签名 ----------
	kgValues.Input = sdkInput
	signHex, err := e.getSign(ctx, kgValues)
	if err != nil {
		return nil, fmt.Errorf("%w: keygen 取签名失败: %v", ErrEngine, err)
	}
	yspsdkSign := signHex + "-" + sdkInput

	// ---------- 5) buildTicket ----------
	ticket, err := e.buildTicket(ctx, meta.LivePid, authTS, meta.CnLid, guid)
	if err != nil {
		return nil, err
	}

	// ---------- 6) /v1/player/get_live_info ----------
	finalKeys := append(append([]string(nil), liveSignKeys...), "rand_str")
	finalBody := make(map[string]string, len(liveSignBody)+2)
	for k, v := range liveSignBody {
		finalBody[k] = v
	}
	finalBody["rand_str"] = randStr(10)
	// 原 JS: 裸 .sort() 升序 + 后缀 0f$IVHi9Qno?G
	finalSignature := md5Hex(joinSorted(finalBody, false, finalKeys) + liveSignSalt)
	finalBody["signature"] = finalSignature

	// 原 JS: const jsonPayload = JSON.stringify(finalBody);
	// 修复原项目 bug: 原 JS 依赖对象插入顺序，若换成"按 key 排序的 JSON"签名会校验失败；
	// 这里显式按 liveSignBody 的插入顺序 + rand_str + signature 手工拼装，绝不依赖 map 遍历。
	jsonPayload := buildOrderedJSON(append(finalKeys, "signature"), finalBody, map[string]bool{"adjust": true})

	liveRes, err := e.doRequest(ctx, "POST", getLiveInfoURL, map[string]string{
		"origin":         yangshipinOrigin,
		"referer":        yangshipinRefer,
		"user-agent":     e.cfg.UA,
		"cookie":         cookie,
		"yspappid":       yspAppID,
		"content-type":   "application/json;charset=UTF-8",
		"yspsdkinput":    bodyMd5,
		"yspsdksign":     yspsdkSign,
		"seqId":          "1",
		"request-id":     reqID,
		"yspPlayerToken": playerToken,
		"yspticket":      ticket,
	}, []byte(jsonPayload))
	if err != nil {
		return nil, fmt.Errorf("%w: get_live_info 请求失败: %v", ErrEngine, err)
	}
	if liveRes.Status != 200 {
		return nil, fmt.Errorf("%w: get_live_info HTTP %d", ErrEngine, liveRes.Status)
	}
	var liveJSON liveInfoResponse
	if err = json.Unmarshal(liveRes.Body, &liveJSON); err != nil {
		return nil, fmt.Errorf("%w: get_live_info 响应不是合法 JSON: %v", ErrEngine, err)
	}
	if liveJSON.Data == nil || liveJSON.Data.PlayURL == "" {
		// 原 JS: throw new Error('missing playurl')
		return nil, fmt.Errorf("%w: missing playurl", ErrEngine)
	}
	playURL := liveJSON.Data.PlayURL
	// 原 JS: if (liveJson.data.extended_param) playurl += extended_param;
	if liveJSON.Data.ExtendedParam != "" {
		playURL += liveJSON.Data.ExtendedParam
	}

	// ---------- 7) 拉 m3u8 并改写分片 ----------
	m3u8Res, err := e.doRequest(ctx, "GET", playURL, map[string]string{
		"User-Agent": e.cfg.UA,
		"Referer":    yangshipinRefer,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: 拉取 playurl 失败: %v", ErrEngine, err)
	}
	if m3u8Res.Status != 200 || !strings.Contains(string(m3u8Res.Body), "#EXTM3U") {
		// 原 JS: throw new Error('invalid upstream m3u8')
		return nil, fmt.Errorf("%w: invalid upstream m3u8", ErrEngine)
	}
	body := e.rewritePlaylist(string(m3u8Res.Body), playURL)

	// 原 JS: playlistCache.set(canonical, {playlist: finalPlaylist, time: now});
	e.cache.Set(canonical, body, now)
	return &engine.Playlist{Body: body, ContentType: m3u8ContentType, Source: engine.SourceWeb}, nil
}

// buildOrderedJSON 按给定键顺序拼装紧凑 JSON。
// numericKeys 里的键按数字字面量输出（对应 JS 里 adjust: 1 是 number 而不是 "1"）。
func buildOrderedJSON(keys []string, body map[string]string, numericKeys map[string]bool) string {
	var b strings.Builder
	b.WriteByte('{')
	for _, k := range keys {
		v, ok := body[k]
		if !ok {
			continue
		}
		if b.Len() > 1 {
			b.WriteByte(',')
		}
		b.WriteString(jsonString(k))
		b.WriteByte(':')
		if numericKeys[k] {
			b.WriteString(v) // 数字字面量
		} else {
			b.WriteString(jsonString(v))
		}
	}
	b.WriteByte('}')
	return b.String()
}

// jsonString 用 JSON 规则转义字符串（等价 JS JSON.stringify 对字符串的处理）。
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// formEncodeOrdered 按给定顺序做 application/x-www-form-urlencoded 编码
// （等价 JS new URLSearchParams(obj).toString()）。
func formEncodeOrdered(pairs [][2]string) []byte {
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, formEncodeValue(p[0])+"="+formEncodeValue(p[1]))
	}
	return []byte(strings.Join(parts, "&"))
}

// formEncodeValue 等价 URLSearchParams 对单个值的编码：
// 字母数字与 *-._ 原样保留，空格编码成 '+'，其余按 UTF-8 字节 %XX。
func formEncodeValue(s string) string {
	return string(util.FormURLEncodeValue(s))
}

// m3u8ContentType /live/<slug>.m3u8 的响应类型（原 JS: application/vnd.apple.mpegurl）。
const m3u8ContentType = "application/vnd.apple.mpegurl"
