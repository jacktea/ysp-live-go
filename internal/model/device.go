package model

import (
	"crypto/rand"
	"encoding/json"
	mathrand "math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jacktea/ysp-live-go/internal/crypto"
)

// 常量（原项目顶部常量）。
const (
	DefaultLiveUserID     = "BAEBFF2B-C516-4F34-ABC0-A824A6461CBD"
	DefaultDeviceName     = "央视频电视投屏助手"
	VDNAppName            = "央视频电视投屏助手"
	ReportAppKey          = "1178c84d-4818-44ff-b415-02106e87e144"
	CollectSDKVersion     = "1.0.0"
	DefaultAcceptLanguage = "zh-CN,zh;q=0.8"
	VdnAppVer             = "1.4.1"
	AppChannel            = "dangbei"

	ResultOK                  = 0
	ResultNeedsRegister       = 601
	ResultGetMissingOrInvalid = 2
	ResultRegisteredElsewhere = 694
	ResultRegisterRetryLater  = 695
)

// Endpoints 央视频各接口地址（原常量）。
const (
	CloudGetURL      = "https://ytpcloudws.cctv.cn/cloudps/wssapi/device/v2/get"
	CloudRegisterURL = "https://ytpcloudws.cctv.cn/cloudps/wssapi/device/v2/register"
	AppStartURL      = "https://ytpaddr.cctv.cn/gsnw/api/app/start/v1/01"
	DRMConfigURL     = "https://ytpaddr.cctv.cn/gsnw/drm/config/obtain/v1"
	VersionConfigURL = "https://ytpaddr.cctv.cn/gsnw/version/config/obtain/v1"
	DictionaryURL    = "https://ytpaddr.cctv.cn/gsnw/player/dictionary/obtain/v1"
	IndexURL         = "https://ytpaddr.cctv.cn/gsnw/api/index/v1/01"
	ReportSingleURL  = "https://ytpdata.cctv.cn/das/app/data/message/single"
	CollectReportURL = "https://collect.cctv.cn/cctvmobileinf/rest/cctv/receive/new/app"
	LiveV101URL      = "https://ytpaddr.cctv.cn/gsnw/api/live/v1/01"
	LiveV102URL      = "https://ytpaddr.cctv.cn/gsnw/api/live/v1/02"
	VDNGetStreamURL  = "https://ytpvdn.cctv.cn/cctvmobileinf/rest/cctv/videoliveUrl/getstream"
	JaccURL          = "https://jacc.ysp.cctv.cn"
	BKLiveInfoURL    = "https://bkliveinfo.ysp.cctv.cn/"
)

// DeviceProfile 伪造的电视设备指纹（原 DeviceProfile dataclass）。
type DeviceProfile struct {
	AndroidID    string `json:"android_id"`
	MAC          string `json:"mac"`
	Hardware     string `json:"hardware"`
	Board        string `json:"board"`
	Brand        string `json:"brand"`
	Device       string `json:"device"`
	Manufacturer string `json:"manufacturer"`
	Model        string `json:"model"`
	Product      string `json:"product"`
	Tags         string `json:"tags"`
	BuildType    string `json:"build_type"`
	User         string `json:"user"`
	Resolution   string `json:"resolution"`
	Display      string `json:"display"`
	VersionID    string `json:"version_id"`
	Host         string `json:"host"`
	Fingerprint  string `json:"fingerprint"`
	ReportModel  string `json:"report_model"`
}

// DeviceState 持久化的设备身份（原 device-state-rs.json）。
type DeviceState struct {
	SchemaVersion int           `json:"schema_version"`
	ProfileSource string        `json:"profile_source"`
	Profile       DeviceProfile `json:"profile"`
	ScreenParam   string        `json:"screen_param"`
	CastModel     string        `json:"cast_model"`
	XUID          string        `json:"x_uid"`
	CloudGUID     string        `json:"cloud_guid"`
	RegisteredAt  float64       `json:"registered_at"`
	UpdatedAt     float64       `json:"updated_at"`
}

// Identity 请求标识（原 Identity）。
type Identity struct {
	XUID                 string            `json:"x_uid"`
	XFingerprint         string            `json:"x_fingerprint"`
	FingerprintTimestamp int64             `json:"fingerprint_timestamp_ms"`
	Headers              map[string]string `json:"headers"`
}

// ChannelEntry 高码率频道解析结果（原 ChannelEntry）。
type ChannelEntry struct {
	Channel           string            `json:"channel"`
	LiveID            string            `json:"live_id"`
	FinalURL          string            `json:"final_url"`
	PlaybackHeaders   map[string]string `json:"playback_headers"`
	AndroidID         string            `json:"android_id"`
	XUID              string            `json:"x_uid"`
	Rate              string            `json:"rate"`
	RateName          string            `json:"rate_name"`
	RawLiveHost       string            `json:"raw_live_host"`
	FinalHost         string            `json:"final_host"`
	RefreshedAt       float64           `json:"refreshed_at"`
	ExpiresAt         float64           `json:"expires_at"`
	Generation        int               `json:"generation"`
	LastRefreshError  string            `json:"last_refresh_error"`
	LastRefreshFailed float64           `json:"last_refresh_failed_at"`
	SessionGeneration int               `json:"session_generation"`
}

// Fresh 是否仍在有效期内。
func (e *ChannelEntry) Fresh(now float64) bool {
	return e != nil && e.FinalURL != "" && now < e.ExpiresAt
}

// StaleUsable 过期后是否仍可在 SWR 窗口内使用。
func (e *ChannelEntry) StaleUsable(now, ttl float64) bool {
	return e != nil && e.FinalURL != "" && ttl > 0 && now < e.ExpiresAt+ttl
}

// NowF64 当前 Unix 时间（秒，浮点）。
func NowF64() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// CurrentTimeMs 当前毫秒时间戳。
func CurrentTimeMs() int64 { return time.Now().UnixNano() / 1e6 }

// SanitizeProfileID 只保留 ASCII 字母数字，其余折叠为单个下划线（原 sanitize_profile_id）。
func SanitizeProfileID(value string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range value {
		if r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			b.WriteRune(toLower(r))
			lastUnderscore = false
		} else if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	return strings.Trim(b.String(), "_")
}

func toLower(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r + ('a' - 'A')
	}
	return r
}

// ResolutionFromScreenParam 由 screen_param 推导分辨率字符串。
func ResolutionFromScreenParam(screenParam string) string {
	parts := strings.Split(screenParam, "-")
	if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
		return parts[0] + "*" + parts[1]
	}
	return "3840*2160"
}

// ProfileFromTemplate 由模板生成设备指纹（原 device_profile_from_template）。
func ProfileFromTemplate(t DeviceTemplate, androidID, mac string) DeviceProfile {
	brandID := SanitizeProfileID(t.Brand)
	modelID := SanitizeProfileID(t.Model)
	device := brandID + "_" + modelID
	return DeviceProfile{
		AndroidID:    androidID,
		MAC:          mac,
		Hardware:     t.Hardware,
		Board:        t.Board,
		Brand:        t.Brand,
		Device:       device,
		Manufacturer: t.Manufacturer,
		Model:        t.Model,
		Product:      device,
		Tags:         "release-keys",
		BuildType:    "user",
		User:         "build",
		Resolution:   ResolutionFromScreenParam(t.ScreenParam),
		Display:      t.Model + "-user 13 " + t.VersionID + " 2024 release-keys",
		VersionID:    t.VersionID,
		Host:         brandID + "-tv-build",
		Fingerprint:  t.Manufacturer + "/" + device + "/" + device + ":13/" + t.VersionID + "/2024:user/release-keys",
		ReportModel:  t.ReportModel,
	}
}

// RandomDeviceTemplate 随机选一个 8K 设备模板（原 random_device_template）。
func RandomDeviceTemplate() DeviceTemplate {
	var pool []DeviceTemplate
	for _, t := range DeviceProfilePool {
		if strings.Contains(t.Source, "8k") {
			pool = append(pool, t)
		}
	}
	if len(pool) == 0 {
		pool = DeviceProfilePool
	}
	return pool[mathrand.Intn(len(pool))]
}

// DefaultDeviceState 生成全新随机设备身份（原 default_device_state）。
func DefaultDeviceState() *DeviceState {
	t := RandomDeviceTemplate()
	profile := ProfileFromTemplate(t, crypto.RandomHex(16), crypto.RandomMAC())
	return &DeviceState{
		SchemaVersion: 1,
		ProfileSource: t.Source,
		Profile:       profile,
		ScreenParam:   t.ScreenParam,
		CastModel:     t.CastModel,
		XUID:          ComputeXUID(profile),
		RegisteredAt:  0,
		UpdatedAt:     NowF64(),
	}
}

// ComputeXUID 计算 X-Uid（原 compute_x_uid）。
func ComputeXUID(p DeviceProfile) string {
	build := "1698" + p.Hardware + p.Board + p.Brand + p.Device + p.Manufacturer +
		p.Model + p.Product + p.Tags + p.BuildType + p.User + p.Resolution + p.MAC
	msb := crypto.JavaStringHashCode(build)
	lsb := crypto.JavaStringHashCode(p.Model)
	uuidPart := crypto.JavaUUIDFromHashes(msb, lsb)
	return crypto.SHA1Upper(p.AndroidID + "|" + uuidPart)
}

// InferOSVersion 由 fingerprint 推导 Android 版本（原 infer_os_version）。
func InferOSVersion(p DeviceProfile) string {
	if i := strings.Index(p.Fingerprint, ":"); i >= 0 {
		tail := p.Fingerprint[i+1:]
		if j := strings.Index(tail, "/"); j >= 0 {
			return tail[:j]
		}
	}
	return ""
}

// InferSDKInt 由 Android 版本推导 SDK 号。
func InferSDKInt(p DeviceProfile) string {
	osv := InferOSVersion(p)
	if osv == "" {
		return ""
	}
	major := strings.SplitN(osv, ".", 2)[0]
	return map[string]string{
		"13": "33", "12": "31", "11": "30", "10": "29", "9": "28",
		"8": "26", "7": "24", "6": "23",
	}[major]
}

// BuildIdentity 构造请求标识与基础请求头（原 build_identity）。
func BuildIdentity(p DeviceProfile, appChannel, version string) *Identity {
	xUID := ComputeXUID(p)
	fp, ts, _ := crypto.ComputeFingerprint(xUID, CurrentTimeMs())
	return &Identity{
		XUID:                 xUID,
		XFingerprint:         fp,
		FingerprintTimestamp: ts,
		Headers: map[string]string{
			"Accept":          "application/json",
			"Accept-Language": DefaultAcceptLanguage,
			"Referer":         "api.cctv.cn",
			"User-Agent":      "cctv_app_tv",
			"UID":             p.AndroidID,
			"appChannel":      appChannel,
			"X-Uid":           xUID,
			"X-Fingerprint":   fp,
			"X-Version":       version,
			"Content-Type":    "application/json; charset=utf-8",
			"Connection":      "Keep-Alive",
			"Accept-Encoding": "gzip",
			"Cache-Control":   "no-cache",
		},
	}
}

// FreshHeaders 生成带时间戳/随机串的新鲜请求头（原 fresh_headers）。
func FreshHeaders(template map[string]string, contentType string, accept string, forceTs *int64) map[string]string {
	h := make(map[string]string, len(template)+4)
	if accept != "" {
		h["Accept"] = accept
	}
	ts := CurrentTimeMs()
	if forceTs != nil {
		ts = *forceTs
	}
	h["X-Timestamp"] = strconv.FormatInt(ts, 10)
	h["X-Nonce"] = NewUUID()
	for _, key := range []string{"Accept-Language", "Referer", "User-Agent", "UID", "appChannel", "X-Uid", "X-Fingerprint", "X-Version"} {
		if v, ok := template[key]; ok {
			h[key] = v
		}
	}
	if contentType != "" {
		h["Content-Type"] = contentType
	}
	for _, key := range []string{"Connection", "Accept-Encoding", "Cache-Control"} {
		if v, ok := template[key]; ok {
			h[key] = v
		}
	}
	return h
}

// RootHeaders 字典接口使用的 ROOT 头（原 root_headers）。
func RootHeaders(version string) map[string]string {
	return map[string]string{
		"X-Uid":           "ROOT",
		"X-Fingerprint":   "ROOT",
		"X-Nonce":         NewUUID(),
		"X-Timestamp":     strconv.FormatInt(CurrentTimeMs(), 10),
		"X-Version":       version,
		"UID":             "ROOT",
		"Referer":         "api.cctv.cn",
		"User-Agent":      "cctv_app_tv",
		"appChannel":      "ROOT",
		"Connection":      "Keep-Alive",
		"Accept-Encoding": "gzip",
	}
}

// NewUUID 生成 v4 UUID 字符串。
func NewUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	const hexc = "0123456789abcdef"
	out := make([]byte, 36)
	p := 0
	for i, v := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out[p] = '-'
			p++
		}
		out[p] = hexc[v>>4]
		out[p+1] = hexc[v&0x0f]
		p += 2
	}
	return string(out)
}

// ReportCommonValue 构造上报公共字段（原 build_report_common_value）。
func ReportCommonValue(p DeviceProfile, xUID, appChannel, version, sdkVersion string, dataTimeMs int64) map[string]interface{} {
	model := p.ReportModel
	if model == "" {
		model = strings.ReplaceAll(strings.ReplaceAll(p.Model, p.Manufacturer, ""), " ", "")
	}
	cut := func(v string, n int) string {
		if n >= 0 && len(v) > n {
			return v[:n]
		}
		return v
	}
	return map[string]interface{}{
		"cctv_id":             cut(xUID, 64),
		"device_id":           cut(p.AndroidID, 64),
		"idfa":                "",
		"idfv":                "",
		"user_id":             "",
		"app_key":             cut(ReportAppKey, 64),
		"imei":                "",
		"android_id":          cut(p.AndroidID, 64),
		"mac":                 cut(p.MAC, 64),
		"device_builder_type": cut(p.BuildType, 64),
		"device_hardware":     cut(p.Hardware, 64),
		"device_board":        cut(p.Board, 64),
		"device_brand":        cut(p.Brand, 64),
		"device_params":       cut(p.Device, 64),
		"device_display":      cut(p.Display, 64),
		"device_version_id":   cut(p.VersionID, 64),
		"device_host":         cut(p.Host, 128),
		"device_product":      cut(p.Product, 64),
		"device_tags":         cut(p.Tags, 64),
		"device_user":         cut(p.User, 30),
		"device_fingerprint":  cut(p.Fingerprint, 128),
		"device_manufacturer": cut(p.Manufacturer, 64),
		"device_model":        cut(model, 50),
		"device_resolution":   cut(p.Resolution, 20),
		"system_type":         "Android",
		"device_type":         "TV",
		"app_language":        "CHINESE",
		"app_version":         cut(version, 30),
		"sdk_version":         cut(sdkVersion, 30),
		"os_version":          cut(InferOSVersion(p), 20),
		"app_channel":         cut(appChannel, 50),
		"data_time":           cut(strconv.FormatInt(dataTimeMs, 10), 13),
	}
}

// AppStartBody 构造 app/start 请求体。
func AppStartBody(p DeviceProfile, xUID, appChannel, version string, dataTimeMs int64) map[string]interface{} {
	return map[string]interface{}{
		"key":   "app_start_d1",
		"value": ReportCommonValue(p, xUID, appChannel, version, "", dataTimeMs),
	}
}

// CollectHeaders 采集上报请求头（原 build_collect_headers）。
func CollectHeaders(p DeviceProfile) map[string]string {
	release := InferOSVersion(p)
	if release == "" {
		release = "Android"
	}
	return map[string]string{
		"Content-type":    "application/x-www-form-urlencoded",
		"Charset":         "UTF-8",
		"User-Agent":      "Dalvik/2.1.0 (Linux; U; Android " + release + "; " + p.Model + " Build/" + p.VersionID + ")",
		"Connection":      "Keep-Alive",
		"Accept-Encoding": "gzip",
	}
}

// ParseResultCode 递归解析 result/code 字段（原 parse_result_code）。
func ParseResultCode(value interface{}) (int, bool) {
	m, ok := value.(map[string]interface{})
	if !ok {
		return 0, false
	}
	for _, key := range []string{"result", "code", "errCode", "errcode", "ret"} {
		raw, exists := m[key]
		if !exists {
			continue
		}
		switch v := raw.(type) {
		case bool:
			continue
		case float64:
			return int(v), true
		case int:
			return v, true
		case json.Number:
			if n, err := v.Int64(); err == nil {
				return int(n), true
			}
		case string:
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				return n, true
			}
		}
	}
	for _, key := range []string{"data", "error", "response"} {
		if raw, exists := m[key]; exists {
			if n, ok := ParseResultCode(raw); ok {
				return n, true
			}
		}
	}
	return 0, false
}

// ExtractGUID 从响应中取 data.guid。
func ExtractGUID(value interface{}) string {
	m, ok := value.(map[string]interface{})
	if !ok {
		return ""
	}
	data, ok := m["data"].(map[string]interface{})
	if !ok {
		return ""
	}
	if s, ok := data["guid"].(string); ok {
		return s
	}
	return ""
}

// VDNAppCommon 构造 vdn 接口的 appcommon 字段（原 build_vdn_appcommon，需保持键序）。
func VDNAppCommon(version string) string {
	return `{"adid":"","an":"` + VDNAppName + `","ap":"cctv_app_tv","av":"` + version + `"}`
}

// DeviceStateStore 设备身份持久化（带文件锁，原 load/save_device_state）。
type DeviceStateStore struct {
	mu   sync.Mutex
	path string
}

// NewDeviceStateStore 创建设备状态存储。
func NewDeviceStateStore(path string) *DeviceStateStore {
	return &DeviceStateStore{path: path}
}

// Path 返回存储路径。
func (s *DeviceStateStore) Path() string { return s.path }

// Load 读取设备状态；文件不存在或损坏时返回随机新身份与 false。
func (s *DeviceStateStore) Load() (*DeviceState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return loadDeviceStateLocked(s.path)
}

func loadDeviceStateLocked(path string) (*DeviceState, bool) {
	text, err := os.ReadFile(path)
	if err != nil {
		return DefaultDeviceState(), false
	}
	var state DeviceState
	if err := json.Unmarshal(text, &state); err != nil {
		return DefaultDeviceState(), false
	}
	if len(state.Profile.AndroidID) != 16 || state.Profile.MAC == "" {
		return DefaultDeviceState(), false
	}
	state.XUID = ComputeXUID(state.Profile)
	if state.ScreenParam == "" {
		state.ScreenParam = "7680-4320-280"
	}
	if state.CastModel == "" {
		state.CastModel = state.Profile.Model
	}
	return &state, true
}

// Save 原子写入设备状态（tmp + fsync + rename）。
func (s *DeviceStateStore) Save(state *DeviceState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return saveDeviceStateLocked(s.path, state)
}

func saveDeviceStateLocked(path string, state *DeviceState) error {
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := writeFileSync(tmp, data); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

// CreateAndSaveNewDevice 生成并保存全新设备身份（原 create_and_save_new_device）。
func CreateAndSaveNewDevice(store *DeviceStateStore) *DeviceState {
	state := DefaultDeviceState()
	state.UpdatedAt = NowF64()
	_ = store.Save(state)
	return state
}
