# 原项目 bug 修复清单

本文件记录移植过程中修复的原项目缺陷。每条都标注了原实现位置（`ysp-live.py` / `ysp-engine.js`）、
问题本质与 Go 版的处理方式。

### 0.（移植期 bug，非原项目）JCE 请求漏掉 body 段导致 1080p 与时移全挂

- **现象**：`jacc.ysp.cctv.cn` 返回 HTTP 200，但解包后 `succeed=500`、body 为空，
  `jceTimeshiftURL` 报 `bad response`；1080p 主线路与 7 天时移**全部不可用**，
  线上只剩设备协议与网页引擎在硬扛，而这些层不会报错，故障因此非常隐蔽。
- **根因**：原 `_wrap` 是两步——
  `w.struct(_head, 0)` 写请求头，然后 `w.bytes(body, 1)` 把**请求体作为 tag=1 的 bytes 字段**追加。
  移植时只写了 `WriteRequestHeader` 就 `Out()`，**漏掉了 `w.Bytes(body, 1)`**，
  reqCmd 少 42 字节（tag1 头 1B + 类型/长度 3B + body 38B）。
- **定位方法**：把原 Python 实现与 Go 实现发出的 reqCmd 逐字节对比，长度 283 vs 241，
  差的 42 字节正好是 `1d 00 00 26` + 38 字节 body；用 node/python 直接调原实现可成功，
  证明是 Go 侧请求构造问题而非风控。
- **修复**：`internal/live/jce/jce.go` 的 `Wrap` 增加 `w.Bytes(body, 1)`；
  顺带把 gzip 压缩级别从 Go 默认的 -1 对齐到 Python `gzip.compress` 的 9（`gzip.BestCompression`）。
- **测试**：`internal/live/jce/jce_test.go::TestWrapIncludesBody` 解压内层、断言 reqcmd
  以 42 字节 body 段结尾且包含原始 body 字节。修复后实测 `jceFetch` 解析出 57 个分片，
  时移返回 `#EXT-X-PLAYLIST-TYPE:VOD` 的完整 VOD 列表。

## 一、正确性 / 稳定性

### 1. `/diag` 诊断报告标题重复打印

- **原实现**：`ysp-live.py` 第 2956 与 2965 行两次 `info.append(f"ysp-live v9.4 诊断报告 (运行时间: …)")`，
  中间还夹着首发地址区块。打开 `/diag` 会看到标题出现两次、格式错乱。
- **修复**：`internal/httpapi/server.go` 的 `handleDiag` 只输出一次，并把"首发地址"区块放在标题之后。
- **测试**：`internal/httpapi/server_test.go::TestDiag` 断言标题恰好出现 1 次。

### 2. `build_playlist` 对空分片列表抛异常

- **原实现**：`build_playlist` 中 `target = max(6, max((int(s[1]+0.5) for s in display_segs)))`，
  当 `display_segs` 为空（首次拉取尚未完成、或缓存被清空）时，内层 `max()` 抛 `ValueError`。
  该异常发生在频道状态检查之后，会被上层 `except Exception` 吞掉并计入频道冷却，导致"空列表 + 冷却"的假故障。
- **修复**：`internal/live/lifecycle.go` 的 `BuildPlaylist` 在 `len(segs)==0` 与 `len(display)==0` 时返回 `""`，
  由上层决定回退。
- **测试**：`internal/live/live_test.go` 覆盖空列表返回空串。

### 3. `ensure_channel` 的数据竞争

- **原实现**：`ensure_channel` 在 `ch.lock` 之外读写 `ch._starting`，并在锁内把 `need_fetch/thread_dead`
  算完、锁外再启动线程；两个并发播放请求可能同时通过 `_starting` 检查，重复启动刷新线程，
  甚至出现"一个线程在拉取、另一个把 `segments` 清空"的竞态，表现为播放器偶发断流。
- **修复**：`internal/live/lifecycle.go` 的 `EnsureChannel` 把 `starting` / `loopOn` / 缓存清理全部收进
  `ch.mu` 保护，并用 `spawn` 的返回值保证线程只启动一次。
- **测试**：`go test -race ./internal/live/` 覆盖并发 `EnsureChannel`。

### 4. `jce_refresh` 在无 PDT 时重复插入分片

- **原实现**：`jce_refresh` 用 `if pdt and ch.last_pdt and pdt <= ch.last_pdt: continue` 做增量判断。
  当上游 m3u8 不含 `#EXT-X-PROGRAM-DATE-TIME`（部分备用通道确实没有）时 `pdt` 恒为空，
  该判断被整体跳过，同一分片 URL 每轮都被当作新分片重新分配 `seq` 并 `append` 到 `order`，
  `segments` 字典因 key 相同不增长，但 `order` 持续膨胀——`ch.build_playlist` 取 `order[-15:]`
  时会出现大量指向同一 key 的重复项，最终播放列表里同一段重复出现。
- **修复**：`internal/live/jce_engine.go` 只在**双方都有 pdt** 时做单调性比较，并对 key 做去重，
  `order` 与 `segments` 保持一一对应。

### 5. `bk_refresh` 重取地址后错误信息可能为空

- **原实现**：`bk_refresh` 的两次尝试之间重新拉取 `bk_urls`，但 `last_err` 只在异常分支里赋值；
  若第一次尝试失败于"播放列表不含 #EXTM3U"（`continue`，未设置 `last_err`），
  第二次重取地址也失败时，抛出的是 `RuntimeError('')`，上层日志出现 `刷新异常: ` 空错误。
- **修复**：`internal/live/bk_engine.go` 保证错误文本始终非空并带上下文（尝试次数 / 地址数量）。

### 6. `TsSegmentCache` 的 LRU 记账错误

- **原实现**：`TsSegmentCache` 用 `dict` 存数据、`deque` 存 key 顺序。`set()` 命中已存在 key 时
  只从 dict 删除旧值，**没有**从 deque 移除旧 key；于是同一 URL 被反复刷新（4K 分片每分钟重取一次很常见）
  时 deque 里堆积大量重复 key，`curr_bytes` 记账与实际不符，淘汰循环可能空转甚至误删仍被引用的条目。
- **修复**：`internal/cache/ts.go` 改用 `container/list.Element` 维护真正的 LRU（key ↔ element 一一对应），
  覆盖写走 `MoveToFront` + 原地更新。
- **测试**：`internal/cache/ts_test.go::TestTSCacheReSetNoLeak` 反复覆盖同一 key 100 次，
  断言 `count==1` 且占用不虚高。

### 7. CLI 位置参数与选项混排解析失败

- **原实现**：`argparse` 允许 `ysp-live.py 9123 --only-4k` 这种写法。
  直接用 Go 的 `flag` 包会在遇到第一个非选项参数 `9123` 时停止解析，`--only-4k` 被当作位置参数丢弃，
  表现为"改了参数不生效"。
- **修复**：`internal/config/config.go` 的 `splitPositional` 先摘出位置参数（数字端口），
  剩余交给 `flag`，并区分"需要取值的选项"（如 `--links-per-device 0` 的 `0` 不能被当成端口）。
- **测试**：`internal/config/config_test.go` 覆盖前后两种顺序与 `--links-per-device 0`。

### 8. 时移参数 `start_ts == 0` 被误判为"未提供"

- **原实现**：`parse_catchup_query` 用 `if not start_ts` 判断是否已解析到起始时间。
  `1970-01-01T00:00:00Z` 解析结果为 `0`，会被当成"没有提供"，继而重复走后续 key 的解析。
- **修复**：`internal/gateway/catchup.go` 用独立的 `haveStart`/`haveEnd` 布尔标志。

### 9. 网页版引擎播放列表缓存无上限

- **原实现**：`ysp-engine.js` 的 `playlistCache` 是 `Map`，只写不清，64 路频道长期运行会持续增长。
- **修复**：`internal/engine/web/cache.go` 限制 128 条 + 30s TTL 的 LRU。
- **测试**：`internal/engine/web/wasm_test.go` 覆盖缓存淘汰。

## 二、安全

### 10. `/engine_proxy` 是开放代理（SSRF）

- **原实现**：`ysp-live.py` 的 `/engine_proxy?url=` 直接 `urllib.request.urlopen(target)`，
  可代理任意地址；容器部署时攻击者可借它访问内网/元数据服务。
- **修复**：`internal/httpapi/proxy.go` 的 `allowedUpstreamHost` 只允许央视频相关域名
  （`.cctv.cn` / `.cctv.com` / `.yangshipin.cn` / `.ysp.cctv.cn` 等），并拒绝裸 IP、回环、私网、link-local。
- **测试**：`internal/httpapi/server_test.go::TestEngineProxyRejectsForeignHost`、
  `TestAllowedUpstreamHost`。

### 11. Node 引擎 `/segment` 无地址校验（SSRF）

- **原实现**：`ysp-engine.js` 的 `/segment?url=` 不做任何校验，等价于通用代理。
- **修复**：Go 版 `/segment` 复用同一白名单（`internal/httpapi/proxy.go` 的 `handleSegment`），
  网页引擎自身的 `SegmentProxy` 也做白名单校验。
- **测试**：`internal/httpapi/server_test.go::TestSegmentRejectsForeignHost`（用 169.254.169.254 元数据地址验证）。

### 12. 分片代理令牌解析过于宽松

- **原实现**：`proxy_ts_token` 在 query 既不形如 `a=b` 也不含 `ts=`/`url=` 时，把整个 query 当令牌；
  配合 `decode_proxy_ts_url` 的宽松 base64 解码，任意字符串都能触发一次解码尝试。
- **修复**：`internal/playlist/playlist.go` 的 `ExtractTSToken` 严格区分四种形式，
  `DecodeTSToken` 校验 base64、UTF-8、scheme、`.ts` 后缀。
- **测试**：`internal/playlist/playlist_test.go::TestExtractTSToken`、`TestDecodeTSTokenRejects`。

### 13. 上游响应体无大小限制

- **原实现**：`fetch_playlist_http1` / Node 的 `httpGetBuffer` 都直接 `read()` 全部响应体，
  异常上游返回大文件时会打满内存。
- **修复**：分片代理限制 64MB（`internal/httpapi/upstream.go`），网页引擎限制 32MB，
  分片缓存只接受 ≤35MB（与原 `is_oversized` 逻辑一致）。

## 三、健壮性 / 资源

### 14. `log()` 的去抖表被整体清空

- **原实现**：`_last_log_times` 超过 600 条时 `clear()`，把仍在去抖窗口内的记录也一并清除，
  导致高频日志（如设备 400 重试）在被清空瞬间集中刷屏。
- **修复**：`internal/logx/logx.go` 改为按时间（1 分钟）淘汰过期条目。

### 15. Node 伴生进程生命周期脆弱

- **原实现**：Python 通过 `subprocess.Popen(node ysp-engine.js)` 启动伴生引擎，
  用 `DEVNULL` 丢弃子进程输出，启动失败只能靠 `is_port_listening` 轮询判断；
  容器内 Node 崩溃后不会重启，兜底能力静默丢失，直到重启容器。
- **修复**：Go 版把 WASM 引擎放进同一进程（wazero），没有子进程、没有第二端口，
  也就不存在"子进程已死但网关不知道"的状态。

### 16. WASM 实例共享导入内存导致并发越界

- **原实现（JS）**：`buildTicket()` 每次调用都 `new WebAssembly.Memory` + `new WebAssembly.Table`，
  因此天然无共享。移植初期若为省内存而复用单个导入实例，多 goroutine 并发时
  `exports.R()` 会返回重叠地址，触发 `out of bounds memory access`。
- **修复**：`internal/engine/web/ticket.go` 严格按 JS 语义，每次调用通过
  `experimental.WithImportResolver` 绑定**全新** memory/table 实例后再实例化 ticket 模块
  （CompiledModule 只编译一次）。零共享可变状态，无需全局锁。
- **测试**：`internal/engine/web/wasm_test.go::TestWasmConcurrent`，并跑 `-race`。

### 17. `engine_proxy` 不转发 Range / 忽略状态码差异

- **原实现**：`/engine_proxy` 用 `urlopen(...).read()` 整体读取并统一按 `video/mp2t` 返回，
  HEAD 请求也会真实下载整个分片。
- **修复**：`internal/httpapi/proxy.go` 透传 `Range` 与上游状态码，`HEAD` 走 `FetchUpstream` 的 head 分支不读 body。

## 四、行为保持说明（未改动，属于有意的原样行为）

以下看似奇怪但属于原项目设计，Go 版保持不变，避免改变线上表现：

- 4K 链接缓存只有 60 秒（`cache_ttl=60`），持续观看每分钟重新获取一次。
- 换设备次数只存内存，重启清零；重启复用 `./data` 中的旧设备。
- `--only-4k` 时仍保留 26 路频道列表，只是高码率只对 3 个真 4K 频道生效。
- bkliveinfo 备用线路对 `cctv11/12/14/15/16/17` 与三个剧场频道默认强制启用（`FORCE_BK`）。
