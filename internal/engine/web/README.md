# internal/engine/web — 网页版 Web/WASM 兜底引擎

原项目 `ysp-engine.js`（Node，518 行）的 Go 移植，用
[wazero](https://github.com/tetratelabs/wazero) 在进程内执行原 JS 内嵌的两个
Rust/WASM 模块。它是四层兜底链里的**最后一级**（`engine.SourceWeb`）。

## 与 JS 版的差异

| 项 | 原 JS | Go 版 |
|---|---|---|
| 监听端口 | 自己 `http.createServer().listen(PORT)` | **不监听**：提供 `AllM3U` / `SegmentProxy` 由 gateway 统一暴露 |
| 分片代理地址 | 硬编码 `http://127.0.0.1:PORT/segment?url=` | `Config.SegmentProxyPath` 可配置 |
| 结果缓存 | `Map` 无上限 | LRU 128 条 + 30s TTL |
| `/segment` 校验 | 无（可当任意代理 = SSRF） | 域名白名单 + 拒绝裸 IP/回环/私网/link-local |
| `/segment` 响应体 | 无限制 | 32MB 上限 |
| 崩溃处理 | `uncaughtException` 打日志继续跑 | 返回明确错误；`Close` 后拒绝服务 |

## WASM 资源（`wasm/`，由 `gen/gen_assets.py` 从 JS 抽取）

| 文件 | 大小 | 说明 |
|---|---|---|
| `keygen.wasm` | 27669 B | `KEYGEN_B64` 原样解码。导出 `get_token_rnd` / `get_signature` 等，只 import `wbg.*` 三个函数 |
| `ticket.wasm` | 949810 B | `TICKET_B64` 原样解码。导出 `P Q R S T U V W` |
| `ticket_a.wasm` | 671 B | **生成物，不是从 JS 抽的**。ticket.wasm 的 `a` 模块导入项（memory `a.a` 256页 / table `a.b` 743 / 39 个函数 `a.c..a.O`）的实现载体 |

`a` 模块说明：ticket.wasm 把 memory、table、39 个桩函数都当作**导入项**，
和 JS 里 `new WebAssembly.Memory(...)` / `new WebAssembly.Table(...)` + 39 个闭包一一对应。
wazero 的 `HostModuleBuilder` 无法导出 memory/table，因此用一个极小的 wasm 模块
同时导出这三类东西；函数体是转出口，真实逻辑在 Go 的 `a_impl` 宿主模块（`ticket.go`）。

## 并发模型（重要）

两个 WASM 模块都**不需要全局锁**，因为**每次调用都新建实例**，语义严格等于 JS：

- keygen：JS 每次 `runKeygen` 都 `new WebAssembly.Instance`，Go 同样每次实例化（模块只编译一次）。
  状态（heap/values）通过 `context` 传给 `wbg` 宿主函数 ——
  **所有 `exports.Call` 必须使用带状态的 ctx**，否则 WASM 会 `unreachable`。
- ticket：JS 每次 `buildTicket` 都 `new Memory` + `new Table` + `new Instance`。
  ticket.wasm 的数据段/元素段在**实例化时**写入 `a.a`/`a.b`，且 `R()` 是这块 memory 上的分配器。
  因此每次调用用 `experimental.WithImportResolver` 绑定**全新的 a 实例**再实例化 ticket；
  共享 a 实例会让并发调用拿到重叠指针（`out of bounds memory access`）。
  实测 0.24ms/次，无需池化。

## 对外 API（装配层用）

```go
func New(cfg Config) (*Engine, error)   // 编译+自检两个 WASM；失败返回错误，不 panic
func (e *Engine) Close() error
func (e *Engine) Start(ctx context.Context) error
func (e *Engine) Playlist(ctx context.Context, channel string) (*engine.Playlist, error)
func (e *Engine) Supports(channel string) bool
func (e *Engine) AllM3U(baseURL string) string
func (e *Engine) SegmentProxy(ctx context.Context, targetURL, rangeHeader string) (*SegmentResponse, error)
func (e *Engine) Health() []byte

type Config struct {
    UA               string
    UpstreamTimeout  time.Duration
    SegmentProxyPath string // 例如 "/segment?url="
}
type SegmentResponse struct {
    Status        int
    Body          []byte
    ContentType   string
    ContentLength int64
}
```

## 重新生成资源

```bash
python3 internal/engine/web/gen/gen_assets.py /path/to/ysp-engine.js
```

## 测试

```bash
go test -race ./internal/engine/web/
```

覆盖：WASM 实例化、keygen/ticket 与 JS 的逐字节 golden 对齐、并发唯一性、
auth→token→ckey→keygen→ticket→get_live_info→m3u8 全流程（httptest）、
SSRF 白名单、LRU 淘汰、m3u8 改写。
