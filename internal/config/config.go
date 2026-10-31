// Package config 迁移原项目 main() 的命令行参数与环境变量解析逻辑。
// 保留原有参数名、默认值与优先级，并额外提供结构化配置，便于测试注入。
package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config 运行时配置（原 argparse.Namespace + engine_args 的等价物）。
type Config struct {
	Port           int     // 监听端口（默认 8767）
	Bind           string  // 监听地址（默认 0.0.0.0）
	DataDir        string  // 数据持久化目录
	CacheMB        int     // TS 分片内存缓存（MB，默认 200）
	No4K           bool    // 不启动设备协议
	Only4K         bool    // 仅 4K/8K 走设备协议
	LinksPerDevice int     // 单设备高码率并发上限（默认 6，compose 设 4）
	ZapDebounce    float64 // 换台防抖秒数（默认 10）
	EnginePort     int     // 兼容参数；Go 版为进程内引擎，不再启动伴生进程
	Timeout        float64 // 设备协议 HTTP 超时（默认 10s）
	NoEngine       bool    // 禁用网页版兜底引擎

	// 以下为内部引擎参数（对应原代码中的 engine_args，不可通过命令行调整）。
	CacheTTL               float64
	StaleWhileRefreshTTL   float64
	RefreshErrorCooldown   float64
	PlaylistCacheTTL       float64
	BackgroundRefreshQueue int
	HTTPWorkers            int
	HTTPQueueLimit         int
	IdentityResetThreshold int
	IdentityResetCooldown  float64
	RefreshInterval        float64
	HeartbeatInterval      float64
	HeartbeatTTLGuard      float64
	SessionTTL             float64

	MetaJSON   string // proxy-cache-state-rs.json
	DeviceJSON string // device-state-rs.json
}

// Default 返回与原项目一致的默认配置。
func Default() *Config {
	return &Config{
		Port:           8767,
		Bind:           "0.0.0.0",
		CacheMB:        200,
		LinksPerDevice: envInt("YSP_LINKS_PER_DEVICE", 6),
		ZapDebounce:    envFloat("YSP_ZAP_DEBOUNCE", 10.0),
		EnginePort:     8787,
		Timeout:        10.0,

		CacheTTL:               60.0,
		StaleWhileRefreshTTL:   0.0,
		RefreshErrorCooldown:   5.0,
		PlaylistCacheTTL:       2.0,
		BackgroundRefreshQueue: 4,
		HTTPWorkers:            16,
		HTTPQueueLimit:         1000,
		IdentityResetThreshold: 0,
		IdentityResetCooldown:  300.0,
		RefreshInterval:        1.0,
		HeartbeatInterval:      30.0,
		HeartbeatTTLGuard:      60.0,
		SessionTTL:             7200.0,
	}
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); strings.TrimSpace(v) != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func envFloat(name string, def float64) float64 {
	if v := os.Getenv(name); strings.TrimSpace(v) != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
	}
	return def
}

// Parse 解析命令行参数（与原 argparse 定义保持一致）。
func Parse(args []string) (*Config, error) {
	c := Default()
	fs := flag.NewFlagSet("ysp-live", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.IntVar(&c.Port, "port", c.Port, "监听端口 (默认 8767)")
	fs.StringVar(&c.Bind, "bind", c.Bind, "监听地址 (默认 0.0.0.0)")
	fs.StringVar(&c.DataDir, "data-dir", os.Getenv("YSP_DATA_DIR"), "数据持久化目录")
	fs.IntVar(&c.CacheMB, "cache-mb", c.CacheMB, "TS 分片内存缓存大小 (MB)")
	fs.BoolVar(&c.No4K, "no-4k", false, "不启动设备协议 (仅走 1080p JCE/bkliveinfo)")
	fs.BoolVar(&c.Only4K, "only-4k", false, "仅 4K/8K 频道走设备协议")
	fs.IntVar(&c.LinksPerDevice, "links-per-device", c.LinksPerDevice, "单设备高码率链接配额上限")
	fs.Float64Var(&c.ZapDebounce, "zap-debounce", c.ZapDebounce, "换台防抖秒数")
	fs.IntVar(&c.EnginePort, "engine-port", c.EnginePort, "网页版引擎端口（Go 版为进程内实现，仅为兼容保留）")
	fs.Float64Var(&c.Timeout, "timeout", c.Timeout, "设备协议 HTTP 超时秒数")
	fs.BoolVar(&c.NoEngine, "no-engine", false, "不启用网页版兜底引擎")

	// 原 argparse 允许 "8767 --data-dir /x" 这种选项与位置参数混排，
	// 而 Go 的 flag 包遇到第一个非选项参数就会停止解析。
	// 这里先把位置参数（端口号）摘出来，剩余部分交给 flag 解析，保持两种写法都可用。
	positional, flags, err := splitPositional(args)
	if err != nil {
		return nil, err
	}
	if err := fs.Parse(flags); err != nil {
		return nil, err
	}
	// 兼容 ysp-live.py 的位置参数形式：ysp-live.py 8767
	if len(positional) > 0 {
		n, perr := strconv.Atoi(positional[0])
		if perr != nil {
			return nil, fmt.Errorf("无效的端口参数 %q: %w", positional[0], perr)
		}
		c.Port = n
	}
	if len(positional) > 1 {
		return nil, fmt.Errorf("多余的参数: %v", positional[1:])
	}
	if err := c.finalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// finalize 归一化路径并应用环境变量覆盖。
func (c *Config) finalize() error {
	here, err := os.Getwd()
	if err != nil {
		here = "."
	}
	if exe, err := os.Executable(); err == nil {
		here = filepath.Dir(exe)
	}
	if strings.TrimSpace(c.DataDir) == "" {
		c.DataDir = strings.TrimSpace(os.Getenv("YSP_DATA_DIR"))
	}
	if strings.TrimSpace(c.DataDir) == "" {
		c.DataDir = here
	}
	if c.DataDir != "" {
		_ = os.MkdirAll(c.DataDir, 0o755)
	}
	if c.CacheMB < 0 {
		c.CacheMB = 0
	}
	if c.LinksPerDevice < 0 {
		c.LinksPerDevice = 0
	}
	if c.Timeout < 0.1 {
		c.Timeout = 0.1
	}
	// YSP_ONLY_4K_DEVICE 环境变量优先级同原项目
	v := strings.ToLower(strings.TrimSpace(os.Getenv("YSP_ONLY_4K_DEVICE")))
	if v == "1" || v == "true" || v == "yes" {
		c.Only4K = true
	}
	if c.MetaJSON == "" {
		c.MetaJSON = filepath.Join(c.DataDir, "proxy-cache-state-rs.json")
	}
	if c.DeviceJSON == "" {
		c.DeviceJSON = filepath.Join(c.DataDir, "device-state-rs.json")
	}
	return nil
}

// Addr 返回监听地址。
func (c *Config) Addr() string { return fmt.Sprintf("%s:%d", c.Bind, c.Port) }

// splitPositional 把参数分成"位置参数"与"选项参数"两组。
//
// 需要识别两类选项写法：`-flag`（布尔）与 `-flag value` / `-flag=value`（带值）。
// 带值选项的值即使看起来像数字也不能当作位置参数（例如 --links-per-device 0）。
func splitPositional(args []string) (positional, flags []string, err error) {
	// 需要取值的选项名（其余为布尔选项）
	valueFlags := map[string]bool{
		"port": true, "bind": true, "data-dir": true, "cache-mb": true,
		"links-per-device": true, "zap-debounce": true, "engine-port": true,
		"timeout": true,
	}
	boolFlags := map[string]bool{
		"no-4k": true, "only-4k": true, "no-engine": true, "help": true, "h": true,
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(a) > 0 && a[0] == '-' {
			name := strings.TrimLeft(a, "-")
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				name = name[:eq]
				flags = append(flags, a)
				continue
			}
			flags = append(flags, a)
			if valueFlags[name] {
				// 该选项需要紧随其后的值
				if i+1 >= len(args) {
					return nil, nil, fmt.Errorf("选项 -%s 缺少取值", name)
				}
				i++
				flags = append(flags, args[i])
			} else if !boolFlags[name] {
				// 未知选项交给 flag 包报错
				continue
			}
			continue
		}
		positional = append(positional, a)
	}
	return positional, flags, nil
}
