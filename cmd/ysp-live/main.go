// Package main ysp-live-go 启动入口。
//
// 与原项目 ysp-live.py 的 main() 对齐：
//   - 解析命令行参数 / 环境变量
//   - 初始化四层兜底引擎（设备协议 / 1080p JCE / bkliveinfo / 网页版 WASM）
//   - 在单端口暴露 HTTP 网关（默认 8767）
//
// 与原项目的关键差异（同时是移植收益）：
//   - 原项目需要一个独立 Node.js 进程跑网页版引擎，通过 8787 端口回环通信；
//     Go 版把 WASM 引擎内置进同一进程，不再需要 Node，也不再占用第二个端口。
//   - 原项目 /engine_proxy 与 Node /segment 是开放代理；Go 版有域名白名单。
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jacktea/ysp-live-go/internal/config"
	"github.com/jacktea/ysp-live-go/internal/httpapi"
	"github.com/jacktea/ysp-live-go/internal/logx"
	"github.com/jacktea/ysp-live-go/internal/model"
	"github.com/jacktea/ysp-live-go/internal/server"
)

// Version 二进制版本（可通过 -ldflags -X 注入，见 internal/httpapi.Version 的说明）。
var Version = httpapi.Version

func main() {
	cfg, err := config.Parse(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "参数错误:", err)
		os.Exit(2)
	}
	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "启动失败:", err)
		os.Exit(1)
	}
}

func run(cfg *config.Config) error {
	dir := model.NewChannelDirectory()
	warnDeprecated(cfg)

	app, err := server.New(server.Options{Config: cfg, Dir: dir})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	app.Start(ctx)

	ln, err := net.Listen("tcp", cfg.Addr())
	if err != nil {
		app.Close()
		return fmt.Errorf("监听 %s 失败: %w", cfg.Addr(), err)
	}

	srv := &http.Server{
		Handler:           app.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		// 分片代理可能长时间传输，不设 ReadTimeout/WriteTimeout。
		IdleTimeout: 120 * time.Second,
		ErrorLog:    nil,
	}

	printBanner(cfg, dir)

	errCh := make(chan error, 1)
	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		logx.Log("[系统] 收到退出信号，正在停止…", 0)
	case serveErr := <-errCh:
		if serveErr != nil {
			app.Close()
			return serveErr
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	app.Close()
	logx.Log("[系统] 服务已停止", 0)
	return nil
}

// warnDeprecated 提示已不生效的兼容参数。
//
// 原项目用 --engine-port / -no-engine 控制 Node 伴生进程；Go 版引擎在进程内，
// 这两个参数仅为兼容旧脚本保留，显式使用时给出提示。
func warnDeprecated(cfg *config.Config) {
	if cfg.NoEngine {
		logx.Log("[系统] --no-engine 已指定：网页版 WASM 兜底引擎将被禁用 (仅剩三层兜底)", 0)
	}
	if cfg.EnginePort != 8787 {
		logx.Log("[系统] 提示: Go 版内置 WASM 引擎，不再监听 --engine-port，该参数已忽略", 0)
	}
}

func printBanner(cfg *config.Config, dir *model.ChannelDirectory) {
	cctv, weishi := 0, 0
	for _, m := range dir.AllChannels() {
		if m.Group == "央视" {
			cctv++
		} else {
			weishi++
		}
	}
	links := cfg.LinksPerDevice
	mode := fmt.Sprintf("全部 %d 个高码率频道走单设备并发池", len(activeBackendChannels(cfg)))
	if cfg.Only4K {
		mode = "仅 4K/8K 频道走设备协议"
	}
	logx.Log("====================================================================", 0)
	logx.Log(fmt.Sprintf("ysp-live v%s 启动成功: %d 个独立频道 (央视 %d 路, 卫视 %d 路)", Version, dir.Count(), cctv, weishi), 0)
	logx.Log(fmt.Sprintf("高码率模式: %s，并发上限 %d 路，换台防抖 %.0fs", mode, links, cfg.ZapDebounce), 0)
	logx.Log(fmt.Sprintf("全频道订阅:   http://localhost:%d/all.m3u", cfg.Port), 0)
	logx.Log(fmt.Sprintf("系统健康度:   http://localhost:%d/health", cfg.Port), 0)
	logx.Log(fmt.Sprintf("实时诊断日志: http://localhost:%d/diag", cfg.Port), 0)
	logx.Log(fmt.Sprintf("持久化目录:   %s", cfg.DataDir), 0)
	logx.Log(fmt.Sprintf("📢 首发地址  社区: %s  频道: %s", httpapi.AuthorCommunity, httpapi.AuthorChannel), 0)
	logx.Log("⚠️ 其他任何分发渠道均非作者本人！", 0)
	logx.Log("====================================================================", 0)
}

func activeBackendChannels(cfg *config.Config) map[string]bool {
	if cfg.Only4K {
		out := make(map[string]bool, len(model.True4KChannels))
		for k, v := range model.True4KChannels {
			out[k] = v
		}
		return out
	}
	return model.BackendChannels
}
