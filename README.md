# ysp-live-go

央视频全频道直播代理（64 路，含 4K / 8K、高码率央视、省级卫视与 7 天回看）的 **Go 重写版**。

原项目 [ysp-live](https://github.com/evanzme/ysp-live) 由 Python 网关（`ysp-live.py`，3374 行）+ Node.js 网页版兜底引擎
（`ysp-engine.js`，含两个内嵌 Rust/WASM 模块）组成，运行需要 Python 3.12 与 Node.js 两个运行时、两个进程、两个端口。

本项目把它完整移植为 **单个静态 Go 二进制**：

- 四层兜底全部内置，不再需要 Python / Node.js
- 网页版 WASM 引擎用 [wazero](https://github.com/tetratelabs/wazero) 在进程内执行，WASM 以 `//go:embed` 打包
- 只监听一个端口（默认 8767），线程模型从 Python 线程改成 Go goroutine
- 修复了原项目若干 bug（见 [修复清单](docs/BUGS.md)）

## 功能对齐

| 能力 | 状态 |
|---|---|
| 64 路频道（央视 30 + 卫视 34） | ✅ 与原项目一致 |
| 四层兜底：设备协议 → 1080p JCE → bkliveinfo → 网页版 WASM | ✅ |
| 4K / 8K 高码率（真 4K：CCTV-4K、8K、16 4K） | ✅ |
| 7 天时移回看（`?playseek=`） | ✅ |
| 换台防抖（先出 1080p，停留后升码率） | ✅ |
| HTTP 400 自动换设备（5 分钟一次 / 每日 5 次熔断） | ✅ |
| 单设备并发池 + 空闲释放 | ✅ |
| SWR 毫秒级秒开 | ✅ |
| TS 分片内存缓存 | ✅ |
| `/all.m3u`、`/<channel>.m3u8`、`/health`、`/diag`、分片代理 | ✅ |
| `/all.m3u?group=卫视`、`?k4=1` | ✅ |
| 设备身份持久化 `device-state-rs.json` | ✅ 与原格式兼容 |

## 快速开始

### 本机运行

```bash
go build -o ysp-live ./cmd/ysp-live
./ysp-live              # 监听 0.0.0.0:8767
./ysp-live 9000         # 指定端口
```

播放器（APTV / TiviMate / OTT Navigator）添加订阅：

```
http://<局域网IP>:8767/all.m3u
```

### Docker

```bash
docker compose up -d --build
docker logs -f ysp-live
```

注意：必须运行在**家庭宽带**下。设备注册会被机房 IP 拒绝，不要部署到 VPS / 云服务器。

## 地址

| 地址 | 内容 |
|---|---|
| `/` | 服务状态与入口（纯文本） |
| `/all.m3u` | 64 路聚合订阅，含 EPG 与 7 天回看 |
| `/all.m3u?group=卫视` | 只订阅某个分组（`央视` / `卫视`） |
| `/all.m3u?k4=1` | 只订阅 3 个真 4K/8K 频道 |
| `/<频道>.m3u8` | 单个频道，如 `/cctv4k.m3u8`、`/cctv13.m3u8` |
| `/health` | JSON 健康信息：运行时间、设备会话、分片缓存命中率 |
| `/diag` | 诊断信息：设备会话状态、分片缓存、各频道刷新情况、最近 30 条日志 |
| `/proxy.ts?<token>` | TS 分片代理（播放列表内部使用） |

## 配置

命令行参数（与原项目同名同义）：

| 参数 | 默认值 | 说明 |
|---|---|---|
| `[port]` | `8767` | 位置参数，监听端口 |
| `--bind` | `0.0.0.0` | 监听地址 |
| `--data-dir` | `$YSP_DATA_DIR` 或可执行文件目录 | 设备身份与状态持久化目录 |
| `--cache-mb` | `200` | TS 分片内存缓存大小，`0` 关闭 |
| `--no-4k` | 关闭 | 不启动设备协议（只走 1080p / 备用线路） |
| `--only-4k` | 关闭 | 只有 3 个真 4K/8K 频道走设备协议 |
| `--links-per-device` | `6` | 单设备高码率并发上限，`0` 不限 |
| `--zap-debounce` | `10` | 换台防抖秒数，`0` 关闭 |
| `--timeout` | `10` | 设备协议 HTTP 超时秒数 |
| `--no-engine` | 关闭 | 禁用网页版 WASM 兜底层 |
| `--engine-port` | — | **已废弃**：Go 版引擎在进程内，不再监听第二个端口（仅为兼容保留） |

环境变量：

| 变量 | 默认值 | 说明 |
|---|---|---|
| `YSP_LINKS_PER_DEVICE` | `6` | 同 `--links-per-device` |
| `YSP_ZAP_DEBOUNCE` | `10` | 同 `--zap-debounce` |
| `YSP_ONLY_4K_DEVICE` | 关闭 | 设为 `1`/`true`/`yes` 等价 `--only-4k` |
| `YSP_DATA_DIR` | 可执行文件目录 | 同 `--data-dir` |
| `TZ` | 系统默认 | 日志时区 |

## 兜底顺序

频道按以下顺序获取，前一级失败自动落到下一级：

1. **设备协议 4K / 高码率**（默认 26 个频道，`--only-4k` 时只有 3 个真 4K）
2. **1080p JCE 解析**（`jacc.ysp.cctv.cn`，时移也走这条）
3. **bkliveinfo 备用线路**（`bkliveinfo.ysp.cctv.cn`）
4. **网页版 WASM 兜底引擎**（原 `ysp-engine.js`，现为进程内 wazero 引擎）

只有前三级都拿不到播放列表时才会用到第 4 层。

## 项目结构

```
cmd/ysp-live/            启动入口（参数解析、装配、优雅退出）
internal/model/          频道表、设备指纹池、设备持久化、JCE 相关数据结构
internal/crypto/         AES-GCM / AES-CBC / RSA-OAEP / TEA(cKey) / 哈希指纹
internal/httpx/          上游 HTTP 客户端、紧凑排序 JSON、表单编码
internal/live/jce/       JCE 编解码（原 W/R 类）与请求封装
internal/live/           1080p JCE 线路、bkliveinfo 备用线路、时移回看
internal/resolver/       设备协议 resolver：注册、心跳、4K 解析、换设备、防抖、并发池
internal/engine/web/     网页版 WASM 兜底引擎（wazero + 内嵌 wasm）
internal/gateway/        四层兜底调度、时移查询解析
internal/httpapi/        HTTP 网关：订阅、播放列表、分片代理、健康与诊断
internal/playlist/       m3u 生成与分片代理令牌
internal/cache/          TS 分片内存缓存
internal/ratelimit/      上游 API 全局流控
internal/logx/           带去抖与环形缓冲的日志
internal/server/         引擎装配与适配层
internal/config/         命令行 / 环境变量
```

## 开发

```bash
go build ./...
go vet ./...
go test ./...              # 全部离线测试
go test -run TestName -v ./internal/resolver/   # 单个测试
```

`docs/PORTING.md` 记录了移植约定与各模块与原实现的对应关系，`docs/BUGS.md` 记录修复的 bug。

## 免责声明

本项目仅用于学习和技术研究。所有流媒体内容版权归原权利方所有，请勿用于商业用途。
与原项目一样，请只在可信的局域网内使用，不要把 8767 端口映射到公网。
