package resolver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jacktea/ysp-live-go/internal/crypto"
	"github.com/jacktea/ysp-live-go/internal/httpx"
	"github.com/jacktea/ysp-live-go/internal/logx"
	"github.com/jacktea/ysp-live-go/internal/model"
)

// 本文件对应原项目 Resolver 的设备身份与会话管理部分：
// bootstrap_session / ensure_slot_session / app_start_flow / cloud_registration_flow /
// heartbeat_flow / collect_report / dictionary_obtain / device_info_report_flow /
// index_flow / warmup_flow / rotate_device_on_400 / record_recoverable_error。

// 设备协议固定参数（原 bootstrap_session 里的 app_channel / version 字面量）。
const (
	appChannel = "dangbei"
	appVersion = "1.4.1"
)

// bootstrapSession 完整注册一次设备会话（原 bootstrap_session）。
//
// 参数：
//   - generation：本次会话代号（原 state.session_generation）；
//   - deviceState：已有设备身份（nil 表示从磁盘加载或新建）；
//   - saveToDisk：是否把注册结果写回 device json；
//   - deviceJSON：设备 json 路径（空则用 cfg.DeviceJSON）。
func (r *Resolver) bootstrapSession(generation int, deviceState *model.DeviceState, saveToDisk bool, deviceJSON string) (*AppSession, error) {
	targetJSON := deviceJSON
	if strings.TrimSpace(targetJSON) == "" {
		targetJSON = r.cfg.DeviceJSON
	}
	store := model.NewDeviceStateStore(targetJSON)

	if deviceState == nil {
		loaded, existed := store.Load()
		deviceState = loaded
		if (!existed || deviceState.RegisteredAt <= 0) && saveToDisk {
			// 原实现：没有可用身份时生成新身份并落盘（注册成功后会再次覆盖保存）。
			deviceState = model.DefaultDeviceState()
			deviceState.UpdatedAt = nowF64()
			_ = store.Save(deviceState)
		}
	}

	profile := deviceState.Profile
	identity := model.BuildIdentity(profile, appChannel, appVersion)
	deviceState.XUID = identity.XUID

	client := r.newYtpaddrClient()

	appStart, err := r.appStartFlow(client, profile, identity, appChannel, appVersion)
	if err != nil {
		return nil, err
	}
	sessionKey := appStart.SessionKey

	cloudGUID, err := r.cloudRegistrationFlow(identity, identity.XUID)
	if err != nil {
		return nil, errorf("cloud device registration failed: %v", err)
	}
	if cloudGUID == "" && deviceState.CloudGUID != "" {
		cloudGUID = deviceState.CloudGUID
	}
	if cloudGUID == "" {
		return nil, errorf("cloud device registration failed: empty guid, fallback to 1080p")
	}

	now := nowF64()
	deviceState.XUID = identity.XUID
	deviceState.UpdatedAt = now
	deviceState.CloudGUID = cloudGUID
	if deviceState.RegisteredAt <= 0 {
		deviceState.RegisteredAt = now
	}
	if saveToDisk {
		_ = store.Save(deviceState)
	}

	hb, hbErr := r.heartbeatFlow(profile, identity, appChannel, appVersion, cloudGUID)
	hbResult := hb
	hbError := ""
	if hbErr != nil {
		hbResult = ""
		hbError = hbErr.Error()
	}

	// 遥测与预热是后台任务，不阻塞会话建立（原 _bg_telemetry 线程）。
	r.goBackground(func() { r.backgroundTelemetry(client, profile, identity, cloudGUID) })

	sess := newAppSession(&AppSession{
		Profile:             profile,
		Identity:            identity,
		Client:              client,
		SessionKey:          sessionKey,
		CloudGUID:           cloudGUID,
		Version:             appVersion,
		ScreenParam:         deviceState.ScreenParam,
		CastModel:           deviceState.CastModel,
		CreatedAt:           nowF64(),
		Generation:          generation,
		LastHeartbeatAt:     nowF64(),
		HeartbeatCount:      1,
		LastHeartbeatResult: hbResult,
		LastHeartbeatError:  hbError,
	})
	return sess, nil
}

// backgroundTelemetry 后台遥测/预热（原 bootstrap_session 里的 _bg_telemetry）。
func (r *Resolver) backgroundTelemetry(client *httpx.Client, profile model.DeviceProfile, identity *model.Identity, cloudGUID string) {
	body := map[string]interface{}{
		"key":   "app_start_d1",
		"value": model.ReportCommonValue(profile, identity.XUID, appChannel, appVersion, model.CollectSDKVersion, model.CurrentTimeMs()),
	}
	_ = r.collectReport(profile, body)
	_ = r.dictionaryObtain(appVersion)
	if cloudGUID != "" {
		_ = r.deviceInfoReportFlow(profile, identity, appChannel, appVersion, cloudGUID)
	}
	_ = r.indexFlow(client, identity, appChannel)
	_ = r.warmupFlow(client, identity, appVersion)
}

// ensureSlotSessionE 确保指定槽位的会话可用并返回（原 ensure_slot_session，外部入口）。
func (r *Resolver) ensureSlotSessionE(slot int) (*AppSession, error) {
	r.controlLock.Lock()
	defer r.controlLock.Unlock()
	return r.ensureSlotSessionLockedE(slot)
}

// ensureSlotSessionLockedE 假定已持有 controlLock（原 ensure_slot_session 主体）。
//
// 逻辑：
//  1. 锁内快速路径：会话存在且新鲜、剩余 TTL > 300s 直接复用；
//  2. 否则递增 session_generation，在控制面锁下重新 bootstrap；
//  3. 若并发方已经建立了更新代际的会话，则采用它。
func (r *Resolver) ensureSlotSessionLockedE(slot int) (*AppSession, error) {
	r.stateLock.Lock()
	if sess := r.state.sessions[slot]; sess != nil && r.sessionFresh(sess) {
		if rem := r.sessionTTLRemaining(sess); rem == nil || *rem > 300.0 {
			r.stateLock.Unlock()
			return sess, nil
		}
	}
	r.state.sessionGeneration++
	gen := r.state.sessionGeneration
	targetJSON := r.slotDeviceJSONPath(slot)
	r.stateLock.Unlock()

	sess, err := r.bootstrapSession(gen, nil, true, targetJSON)
	if err != nil {
		return nil, asResolverErr(err)
	}

	r.stateLock.Lock()
	cur := r.state.sessions[slot]
	if cur == nil || !r.sessionFresh(cur) || cur.Generation < gen {
		r.state.sessions[slot] = sess
	} else {
		sess = cur
	}
	r.stateLock.Unlock()
	return sess, nil
}

// slotDeviceJSONPath 槽位对应的设备 json 路径（原 slot_device_json_path）。
func (r *Resolver) slotDeviceJSONPath(slot int) string {
	base := r.cfg.DeviceJSON
	if slot == 0 {
		return base
	}
	// 对应 os.path.splitext(base)
	idx := strings.LastIndex(base, ".")
	if idx <= strings.LastIndex(base, "/") {
		return fmt.Sprintf("%s-slot%d", base, slot)
	}
	return fmt.Sprintf("%s-slot%d%s", base[:idx], slot, base[idx:])
}

// appStartFlow app/start 换取 session_key（原 app_start_flow）。
//
// 最多尝试 2 次（第 2 次前 sleep 1s）。与原实现一致：
// 网络/解密错误直接上抛，不重试；只有"响应非 2xx 或缺少加密 session_key"才进入下一次尝试。
func (r *Resolver) appStartFlow(client *httpx.Client, profile model.DeviceProfile, identity *model.Identity, channel, version string) (*AppStartResult, error) {
	last := ""
	for attempt := 1; attempt <= 2; attempt++ {
		if attempt > 1 {
			time.Sleep(time.Duration(attempt-1) * time.Second)
		}
		var (
			result   *AppStartResult
			fatalErr error
		)
		err := r.controlRequestSlot(func() error {
			tokenTime := model.CurrentTimeMs()
			xfp, ts, _ := crypto.ComputeFingerprint(identity.XUID, tokenTime)
			identity.XFingerprint = xfp
			identity.FingerprintTimestamp = ts
			identity.Headers["X-Fingerprint"] = xfp

			body := model.AppStartBody(profile, identity.XUID, channel, version, model.CurrentTimeMs())
			payload, err := compactJSON(body, true)
			if err != nil {
				fatalErr = err
				return err
			}
			headers := model.FreshHeaders(identity.Headers, "application/json", "application/json", &ts)
			headers["UID"] = ""
			resp, err := client.PostBytes(r.eps.appStart, headers, payload)
			if err != nil {
				fatalErr = err
				return err
			}
			status, text, value := responseJSON(resp)
			if status >= 200 && status < 300 {
				// 原实现：
				//   data = value.get('data') if isinstance(value, dict) else None
				//   if isinstance(data, dict): key_val = data.get('key', data); encrypted = key_val if str else ''
				//   elif isinstance(data, str): encrypted = data
				encrypted := ""
				if root, ok := value.(map[string]interface{}); ok {
					switch data := root["data"].(type) {
					case map[string]interface{}:
						if keyVal, exists := data["key"]; exists {
							if s, ok := keyVal.(string); ok {
								encrypted = s
							}
						}
					case string:
						encrypted = data
					}
				}
				if encrypted != "" {
					sessionKey, derr := crypto.AESGCMDecryptB64(encrypted, truncateString(identity.XFingerprint, 32))
					if derr != nil {
						fatalErr = derr
						return derr
					}
					result = &AppStartResult{SessionKey: sessionKey}
					return nil
				}
			}
			last = fmt.Sprintf("app/start HTTP %d: %s", status, truncateString(text, 300))
			return nil
		})
		if err != nil {
			return nil, asResolverErr(fatalErr)
		}
		if result != nil {
			return result, nil
		}
	}
	return nil, errorf("%s", last)
}

// cloudRegistrationFlow 云端设备注册（原 cloud_registration_flow）。
//
// 返回 cloud guid；注册失败返回错误（guid 为空由调用方决定是否回退）。
func (r *Resolver) cloudRegistrationFlow(identity *model.Identity, cloudDeviceID string) (string, error) {
	encryptedID, err := crypto.RSAEncryptDeviceID(cloudDeviceID)
	if err != nil {
		return "", asResolverErr(err)
	}
	body := map[string]interface{}{
		"device_name": model.DefaultDeviceName,
		"device_id":   encryptedID,
	}

	var value interface{}
	err = r.controlRequestSlot(func() error {
		resp, err := r.genericClient.PostJSON(r.eps.cloudGet, model.FreshHeaders(identity.Headers, "application/json", "application/json", nil), body)
		if err != nil {
			return err
		}
		_, _, value = responseJSON(resp)
		return nil
	})
	if err != nil {
		return "", asResolverErr(err)
	}
	// 原实现：
	//   result = parse_result_code(value)   # 解析失败返回 None
	//   if result == RESULT_OK: return extract_guid(value)
	//   if result != RESULT_NEEDS_REGISTER and result != RESULT_GET_MISSING_OR_INVALID: return ''
	// 即"解析失败"与"其他错误码"都直接返回空 guid，不进入注册流程。
	result, ok := model.ParseResultCode(value)
	if !ok {
		return "", nil
	}
	if result == model.ResultOK {
		return model.ExtractGUID(value), nil
	}
	if result != model.ResultNeedsRegister && result != model.ResultGetMissingOrInvalid {
		return "", nil
	}

	lastResult := -1
	guid := ""
	for i := 0; i < 2; i++ {
		var v interface{}
		err = r.controlRequestSlot(func() error {
			resp, err := r.genericClient.PostJSON(r.eps.cloudRegister, model.FreshHeaders(identity.Headers, "application/json", "application/json", nil), body)
			if err != nil {
				return err
			}
			_, _, v = responseJSON(resp)
			return nil
		})
		if err != nil {
			return "", asResolverErr(err)
		}
		lastResult, _ = model.ParseResultCode(v)
		guid = model.ExtractGUID(v)
		if guid != "" || lastResult != model.ResultRegisterRetryLater {
			break
		}
	}
	if guid != "" {
		return guid, nil
	}
	if lastResult == model.ResultOK || lastResult == model.ResultRegisteredElsewhere || lastResult == model.ResultGetMissingOrInvalid {
		var v interface{}
		err = r.controlRequestSlot(func() error {
			resp, err := r.genericClient.PostJSON(r.eps.cloudGet, model.FreshHeaders(identity.Headers, "application/json", "application/json", nil), body)
			if err != nil {
				return err
			}
			_, _, v = responseJSON(resp)
			return nil
		})
		if err != nil {
			return "", asResolverErr(err)
		}
		return model.ExtractGUID(v), nil
	}
	return "", nil
}

// collectReport 采集上报（原 collect_report）。
func (r *Resolver) collectReport(profile model.DeviceProfile, body map[string]interface{}) error {
	headers := model.CollectHeaders(profile)
	info, err := compactJSON(body, false)
	if err != nil {
		return asResolverErr(err)
	}
	resp, err := r.genericClient.PostForm(r.eps.collectReport, headers, [][2]string{{"info", string(info)}})
	if err != nil {
		return asResolverErr(err)
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return errorf("collect report HTTP %s", httpStatusText(resp.Status, resp.Reason))
	}
	return nil
}

// dictionaryObtain 拉取播放器字典（原 dictionary_obtain）。
func (r *Resolver) dictionaryObtain(version string) error {
	var resp *httpx.Response
	err := r.controlRequestSlot(func() error {
		var err error
		resp, err = r.genericClient.PostBytes(r.eps.dictionary, model.RootHeaders(version), []byte{})
		return err
	})
	if err != nil {
		return asResolverErr(err)
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return errorf("dictionary HTTP %s", httpStatusText(resp.Status, resp.Reason))
	}
	return nil
}

// deviceInfoReportFlow 设备信息上报（原 device_info_report_flow）。
func (r *Resolver) deviceInfoReportFlow(profile model.DeviceProfile, identity *model.Identity, channel, version, cloudGUID string) error {
	var resp *httpx.Response
	err := r.controlRequestSlot(func() error {
		value := model.ReportCommonValue(profile, identity.XUID, channel, version, "", model.CurrentTimeMs())
		sdkInt := model.InferSDKInt(profile)
		systemInfo := model.InferOSVersion(profile)
		if sdkInt != "" {
			if systemInfo == "" {
				systemInfo = sdkInt
			} else {
				systemInfo = systemInfo + "/" + sdkInt
			}
		}
		value["version"] = version
		value["network_status"] = "WiFi"
		value["device_info"] = profile.Brand + "-" + profile.Model
		value["manufacturer"] = profile.Manufacturer
		value["cpu_info"] = ""
		value["chip_info"] = profile.Hardware
		value["ram_info"] = ""
		value["memory_info"] = ""
		value["system_info"] = systemInfo
		value["guid"] = cloudGUID
		body := map[string]interface{}{"key": "app_device_info", "value": value}
		var err error
		resp, err = r.genericClient.PostJSON(r.eps.reportSingle, model.FreshHeaders(identity.Headers, "application/json", "application/json", nil), body)
		return err
	})
	if err != nil {
		return asResolverErr(err)
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return errorf("device info HTTP %s", httpStatusText(resp.Status, resp.Reason))
	}
	return nil
}

// heartbeatFlow 心跳上报，返回 result code 字符串（原 heartbeat_flow）。
func (r *Resolver) heartbeatFlow(profile model.DeviceProfile, identity *model.Identity, channel, version, cloudGUID string) (string, error) {
	var resp *httpx.Response
	err := r.controlRequestSlot(func() error {
		value := model.ReportCommonValue(profile, identity.XUID, channel, version, "", model.CurrentTimeMs())
		value["network_type"] = "WiFi"
		value["guid"] = cloudGUID
		value["other"] = ""
		body := map[string]interface{}{"key": "app_heartbeat", "value": value}
		var err error
		resp, err = r.genericClient.PostJSON(r.eps.reportSingle, model.FreshHeaders(identity.Headers, "application/json", "application/json", nil), body)
		return err
	})
	if err != nil {
		return "", asResolverErr(err)
	}
	status, _, value := responseJSON(resp)
	if status < 200 || status >= 300 {
		return "", errorf("heartbeat HTTP %d", status)
	}
	code, ok := model.ParseResultCode(value)
	if !ok {
		return "", nil
	}
	return strconv.Itoa(code), nil
}

// indexFlow 上报 index（原 index_flow）。
func (r *Resolver) indexFlow(client *httpx.Client, identity *model.Identity, channel string) error {
	body := map[string]interface{}{"channel": channel, "source": "application"}
	var resp *httpx.Response
	err := r.controlRequestSlot(func() error {
		var err error
		resp, err = client.PostJSON(r.eps.index, model.FreshHeaders(identity.Headers, "application/json", "application/json", nil), body)
		return err
	})
	if err != nil {
		return asResolverErr(err)
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return errorf("index HTTP %s", httpStatusText(resp.Status, resp.Reason))
	}
	return nil
}

// warmupFlow 预热 DRM/版本配置（原 warmup_flow）。
func (r *Resolver) warmupFlow(client *httpx.Client, identity *model.Identity, version string) error {
	appcommon := model.VDNAppCommon(version)
	var drmResp *httpx.Response
	err := r.controlRequestSlot(func() error {
		var err error
		drmResp, err = client.PostForm(r.eps.drmConfig,
			model.FreshHeaders(identity.Headers, "application/x-www-form-urlencoded", "", nil),
			[][2]string{{"appcommon", appcommon}})
		return err
	})
	if err != nil {
		return asResolverErr(err)
	}
	if drmResp.Status < 200 || drmResp.Status >= 300 {
		return errorf("drm config HTTP %s", httpStatusText(drmResp.Status, drmResp.Reason))
	}

	url := r.eps.versionConfig + "?appcommon=" + formURLEncodeValue(appcommon)
	var verResp *httpx.Response
	err = r.controlRequestSlot(func() error {
		headers := model.FreshHeaders(identity.Headers, "", "", nil)
		delete(headers, "Content-Type")
		var err error
		verResp, err = client.Get(url, headers)
		return err
	})
	if err != nil {
		return asResolverErr(err)
	}
	if verResp.Status < 200 || verResp.Status >= 300 {
		return errorf("version config HTTP %s", httpStatusText(verResp.Status, verResp.Reason))
	}
	return nil
}

// rotateDeviceOn400 检测到 400 后换新设备身份（原 rotate_device_on_400）。
//
// 熔断策略（原样）：5 分钟内最多一次；同一自然日最多 5 次。
func (r *Resolver) rotateDeviceOn400(reason string) bool {
	now := nowF64()
	r.stateLock.Lock()
	if now-r.state.lastDeviceWipeAt < 300.0 {
		r.stateLock.Unlock()
		return false
	}
	day := dayString(now)
	if r.state.deviceWipeDay != day {
		r.state.deviceWipeDay = day
		r.state.deviceWipeCount = 0
	}
	if r.state.deviceWipeCount >= 5 {
		r.stateLock.Unlock()
		logx.Log("[设备] 24小时内已自动换新身份 5 次，停止自动更换，请手动检查网络或设备", 60)
		return false
	}
	r.state.lastDeviceWipeAt = now
	r.state.deviceWipeDay = day
	r.state.deviceWipeCount++
	wipeNo := r.state.deviceWipeCount
	target := r.slotDeviceJSONPath(0)
	delete(r.state.sessions, 0)
	r.state.cache = make(map[string]*model.ChannelEntry)
	r.state.playlistCache = make(map[string]*PlaylistCacheEntry)
	r.state.channelFailures = make(map[string]*channelFailure)
	r.state.channelLastAccess = make(map[string]float64)
	r.stateLock.Unlock()

	// 原实现：create_and_save_new_device(target)（生成新身份并落盘）。
	store := model.NewDeviceStateStore(target)
	model.CreateAndSaveNewDevice(store)

	logx.Log(fmt.Sprintf("[设备] 检测到 400 (%s)，旧身份已废弃，换新设备身份 (5分钟内仅一次，今日第 %d 次)", reason, wipeNo), 0)
	return true
}

// recordRecoverableError 累计可恢复错误（原 record_recoverable_error）。
//
// 原实现里 identity_reset_error_threshold 默认 0（关闭），因此该函数几乎总是返回 false；
// 这里保持完全一致的语义（含 write_meta 副作用）。
func (r *Resolver) recordRecoverableError(reason string) bool {
	if !isIdentityRecoverableError(reason) {
		return false
	}
	threshold := r.cfg.IdentityResetErrorThreshold
	if threshold == 0 {
		return false
	}
	r.stateLock.Lock()
	if r.state.lastIdentityResetAt > 0 && nowF64()-r.state.lastIdentityResetAt < r.cfg.IdentityResetCooldown {
		r.stateLock.Unlock()
		return false
	}
	if r.state.identityResetErrorCount < 1<<31-1 {
		r.state.identityResetErrorCount++
	}
	r.stateLock.Unlock()
	r.WriteMeta()
	return false
}

// clearRecoverableErrorCount 清零可恢复错误计数（原 clear_recoverable_error_count）。
func (r *Resolver) clearRecoverableErrorCount() {
	r.stateLock.Lock()
	r.state.identityResetErrorCount = 0
	r.stateLock.Unlock()
}

// ---------------------------------------------------------------------------
// JSON / 编码辅助
// ---------------------------------------------------------------------------

// compactJSON 等价 Python compact_json_bytes：紧凑 + 键排序 + 可选转义斜杠。
func compactJSON(v interface{}, escapeForwardSlashes bool) ([]byte, error) {
	data, err := httpx.CompactJSON(v)
	if err != nil {
		return nil, err
	}
	if escapeForwardSlashes {
		data = bytes.ReplaceAll(data, []byte("/"), []byte(`\/`))
	}
	return data, nil
}

// responseJSON 解析响应为 (status, text, jsonValue)（原 response_json）。
func responseJSON(resp *httpx.Response) (int, string, interface{}) {
	if resp == nil {
		return 0, "", nil
	}
	text := resp.Text()
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var value interface{}
	if err := dec.Decode(&value); err != nil {
		value = nil
	}
	return resp.Status, text, value
}

// formURLEncodeValue 复刻原 form_urlencode_value（空格 +，保留 *-._）。
func formURLEncodeValue(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '*', c == '-', c == '.', c == '_':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0f])
		}
	}
	return b.String()
}
