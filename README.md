# ysp-live-go

[![CI](https://github.com/jacktea/ysp-live-go/actions/workflows/ci.yml/badge.svg)](https://github.com/jacktea/ysp-live-go/actions/workflows/ci.yml)
[![Release](https://github.com/jacktea/ysp-live-go/actions/workflows/release.yml/badge.svg)](https://github.com/jacktea/ysp-live-go/actions/workflows/release.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

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

### 预编译二进制（推荐）

从 [Releases](https://github.com/jacktea/ysp-live-go/releases) 下载对应平台的文件：

| 平台 | 文件后缀 |
|---|---|
| Linux x86_64 | `linux-amd64` |
| Linux ARM64 | `linux-arm64` |
| Linux ARMv7 / ARMv6（树莓派、软路由） | `linux-armv7` / `linux-armv6` |
| macOS Intel / Apple Silicon | `darwin-amd64` / `darwin-arm64` |
| Windows x86_64 | `windows-amd64.exe` |

```bash
chmod +x ysp-live-<版本>-linux-amd64
./ysp-live-<版本>-linux-amd64     # 监听 0.0.0.0:8767
```

下载后可用同批次的 `checksums.txt` 校验：

```bash
shasum -a 256 -c checksums.txt
```

### 从源码构建

```bash
go build -o ysp-live ./cmd/ysp-live
./ysp-live              # 监听 0.0.0.0:8767
./ysp-live 9000         # 指定端口
```

自己编译时想把版本号写进二进制（发布会自动做这一步）：

```bash
go build -ldflags "-X github.com/jacktea/ysp-live-go/internal/httpapi.Version=1.0.0" -o ysp-live ./cmd/ysp-live
```

播放器（APTV / TiviMate / OTT Navigator）添加订阅：

```
http://<局域网IP>:8767/all.m3u
```

### Docker

镜像发布在 GHCR，多架构（amd64 / arm64 / armv7）自动匹配：

```bash
# 本地构建（compose 内已配好数据卷、TZ、并发数）
docker compose up -d --build

# 只想拉发布镜像：删掉 compose 里的 build: 一行后
docker compose pull && docker compose up -d

# 或者不用 compose 直接跑（把 1.2.3 换成实际版本号；镜像 tag 不带 v 前缀）
docker run -d --name ysp-live --network host --restart unless-stopped \
  -v "$PWD/data:/app/data" \
  -e TZ=Asia/Shanghai \
  ghcr.io/jacktea/ysp-live-go:1.2.3

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

## 发布

打 tag 即触发 [`.github/workflows/release.yml`](.github/workflows/release.yml)：

```bash
git tag v1.0.0
git push origin v1.0.0
```

流水线先跑 gofmt + `go vet` + `go test -race` 作为门禁，通过后产出：

- **GitHub Release**：7 个二进制（linux/amd64、linux/arm64、linux/armv6、linux/armv7、
  windows/amd64、darwin/amd64、darwin/arm64）+ `checksums.txt`。
  版本号通过 `-ldflags -X` 注入二进制，`/health` 的 `version` 字段与启动横幅可见。
- **容器镜像**：`ghcr.io/<owner>/ysp-live-go`，多架构 `linux/amd64` / `linux/arm64` / `linux/arm/v7`，
  标签为 `1.2.3`（tag 去掉 `v`）、`1.2`、`sha-<sha>`，正式版额外打 `latest`
  （带 `-` 的 tag 如 `v1.0.0-rc1` 视为预发布，不打 `latest`）。

日常 push / PR 走 [`.github/workflows/ci.yml`](.github/workflows/ci.yml)：ubuntu + macOS 双平台构建测试、
`-race` 检测、go.mod 一致性、gofmt，以及 Dockerfile 构建 + `/health` 冒烟测试。

## 许可

[MIT](LICENSE)。可自由使用、修改、分发（含商业用途），只需保留版权声明与许可声明。

## 免责声明

本项目仅用于学习和技术研究。所有流媒体内容版权归原权利方所有，请勿用于商业用途。
与原项目一样，请只在可信的局域网内使用，不要把 8767 端口映射到公网。
