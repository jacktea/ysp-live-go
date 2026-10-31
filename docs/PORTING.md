# ysp-live → Go 移植规格（共享约定）

本文件是并行开发时的**唯一契约**。所有包必须遵守。

## 目标
把 `/Users/xiaogang/github/ysp-live/ysp-live.py`（Python 网关，3374 行）与
`/Users/xiaogang/github/ysp-live/ysp-engine.js`（Node 网页版兜底引擎，含 2 个大 WASM blob）
完整移植成 Go 单二进制项目，功能对齐并修复原项目 bug。

**原项目是唯一事实来源**：任何算法细节（请求头、字段顺序、签名、冷却时间、
日志文案、JSON 字段名）都以 Python/JS 源码为准，不要自行"优化"协议行为。

## 已完成的基础包（直接使用，不要重复实现）
| 包 | 内容 |
|---|---|
| `internal/model` | 频道表、设备指纹池、`DeviceState`/`DeviceProfile`/`Identity`/`ChannelEntry`、设备持久化 `DeviceStateStore`、`ComputeXUID`、`BuildIdentity`、`FreshHeaders`、`ReportCommonValue`、`VDNAppCommon`、`ParseResultCode`、`ExtractGUID` 等 |
| `internal/crypto` | AES-GCM、AES-128-CBC、RSA-OAEP、TEA/`CKey`（bkliveinfo）、`SHA1Upper`/`SHA256Hex`/`JavaStringHashCode`/`ComputeFingerprint`/`RandomHex`/`RandomMAC` |
| `internal/httpx` | `Client`（Get/PostBytes/PostJSON/PostForm）、`Response`、`CompactJSON`（等价 Python `sort_keys`+紧凑）、`FormEncode`、`NormalizeNetworkError`、`ReasonOf` |
| `internal/logx` | `Log(msg, minInterval)`、`Recent(n)`（/diag 用） |
| `internal/util` | `RewritePlaylistURLs`、`RSLines`、`IsTSURL`、`HeaderValueClean`、`FormEncode`、`QueryParam`、`GenerateAppRandomStr` 等 |
| `internal/cache` | `TsSegmentCache`（`NewTsSegmentCache(maxMB, maxAge)`、Get/Set/Stats） |
| `internal/ratelimit` | `Global`（700ms+250ms 抖动，等价原 `global_flow_limiter`） |
| `internal/live/jce` | JCE 编解码 `Writer`/`Reader`/`Wrap`/`Unwrap`/`Guid`，已用 Python 生成的字节向量验证 |
| `internal/engine` | 公共类型：`Playlist{Body,ContentType,Source}`、`Source` 常量、`ChannelResolver` 接口、若干哨兵错误 |

## 通用规则
- Go 1.26，模块名 `github.com/jacktea/ysp-live-go`。只用标准库 + `github.com/tetratelabs/wazero`。
- **注释用中文**，关键函数标注对应的原 Python/JS 函数名，例如 `// 对应原 live_v1_01_flow`。
- 错误用 `fmt.Errorf("%w: ...", err)` 包装，保留原项目错误文本片段（如 `"HTTP 400"`、`"内部错误"`、`"device link quota reached"`），
  因为上层靠字符串判断是否换设备 / 冷却。
- 并发：用 `sync.Mutex`/`sync.RWMutex`；**禁止**出现共享 map 无锁读写。
- 每个新包至少一个 `*_test.go`，能离线跑的必须离线（不要依赖真实央视频接口）。需要网络的测试用 `testing.Short()` 跳过。
- 完成后必须 `gofmt -w`、`go build ./...`、`go vet ./...`、`go test ./<你的包>/...` 全绿。
- 不要修改 `internal/model`、`internal/crypto`、`internal/httpx`、`internal/logx`、`internal/util`、
  `internal/cache`、`internal/ratelimit`、`internal/live/jce`、`internal/engine` 这些共享基础包；
  如确实缺能力，在自己包内实现或发消息给 lead。

## 关键常量（来自原项目）
```
AK = 9f5c54c4ed0e50109b800f7e28fec205
APP_ID = 1200013, QMF_APP_ID = 10012, BIZ_ID = 0
VER_NAME = 3.2.7.26212, VER_CODE = 302070, CHAN_ID = 10070
appChannel = dangbei, version = 1.4.1
REFRESH_INTERVAL = 10s, IDLE_TIMEOUT = 120s, LINK_IDLE_TIMEOUT = 15s
WINDOW = 300, MAX_SEGS = 120, BK_URL_TTL = 300
```
