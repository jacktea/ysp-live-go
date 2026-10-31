package web

// 本文件移植原 ysp-engine.js 的 buildTicket()（第 87-196 行）：
// 用 wazero 执行内嵌的 ticket.wasm（Rust 编译产物，导出 P/Q/R/S/T/U/V/W）。
//
// 关于 a 模块（原 JS imports.a）：它同时需要 memory（a.a，256 页、max 256）、
// funcref table（a.b，743、max 743）以及 39 个桩函数（a.c..a.O）。
// wazero 的 HostModuleBuilder 只能导出函数、不能导出 memory/table，
// 因此用一个由 gen/gen_assets.py 生成的小 wasm 模块（ticket_a.wasm）承担
// "导出 memory + table + 39 个函数转出口" 的角色：这些函数自身从宿主模块
// a_impl 导入，而 a_impl 的实现就在本文件 registerTicketHostModule 里，
// 逐条对应原 JS imports.a 的 39 个桩。

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

// ticketStubStateKey 一次 buildTicket 调用的时钟状态。
type ticketStubStateKey struct{}

// ticketStubState 保存本次 buildTicket 调用使用的时钟，
// 对应原 JS 桩函数里直接读取的 Date.now()。
type ticketStubState struct {
	now func() time.Time
}

// newTicketStubState 构造时钟状态（now 为 nil 时用 time.Now）。
func newTicketStubState(now func() time.Time) *ticketStubState {
	if now == nil {
		now = time.Now
	}
	return &ticketStubState{now: now}
}

// unixSec 当前秒级时间戳（对应 JS Math.floor(Date.now()/1000)）。
func (s *ticketStubState) unixSec() uint32 { return uint32(s.now().Unix()) }

// registerTicketHostModule 注册 a_impl 宿主模块：原 JS imports.a 里的 39 个桩函数。
//
// 每个桩的签名严格照抄 ticket.wasm 的 import 段（见 gen/ 里打印的类型表），
// 否则 wazero 会在实例化时报 signature mismatch。
func (e *Engine) registerTicketHostModule(ctx context.Context) error {
	b := e.runtime.NewHostModuleBuilder("a_impl")
	add := func(name string, fn interface{}) {
		b.NewFunctionBuilder().WithFunc(fn).Export(name)
	}

	// a.c: (i32,i32,i32) => 0（原 JS: c: () => 0）
	add("c", func(ctx context.Context, m api.Module, _, _, _ uint32) uint32 { return 0 })
	// a.d: (ptr) => 写当前秒级时间戳到 memory[ptr>>2] 并返回
	add("d", func(ctx context.Context, m api.Module, ptr uint32) uint32 {
		now := ticketStateFrom(ctx).unixSec()
		if ptr != 0 {
			writeU32Le(m.Memory(), ptr, now)
		}
		return now
	})
	// a.e: () => 42（原 JS: e: () => 42）
	add("e", func(ctx context.Context, m api.Module) uint32 { return 42 })
	// a.f / a.g: (i32) => 0
	add("f", func(ctx context.Context, m api.Module, _ uint32) uint32 { return 0 })
	add("g", func(ctx context.Context, m api.Module, _ uint32) uint32 { return 0 })
	// a.h: () => 0
	add("h", func(ctx context.Context, m api.Module) uint32 { return 0 })
	// a.i: (ptr, _) => 写 (秒, 微秒) 到 memory[ptr]，返回 0
	add("i", func(ctx context.Context, m api.Module, ptr, _ uint32) uint32 {
		now := ticketStateFrom(ctx).now()
		writeU32Le(m.Memory(), ptr, uint32(now.Unix()))
		writeU32Le(m.Memory(), ptr+4, uint32(now.Nanosecond()/1000))
		return 0
	})
	// a.j: (clk, tp) => 写 (秒, 纳秒) 到 memory[tp]，返回 0
	add("j", func(ctx context.Context, m api.Module, _, tp uint32) uint32 {
		now := ticketStateFrom(ctx).now()
		writeU32Le(m.Memory(), tp, uint32(now.Unix()))
		writeU32Le(m.Memory(), tp+4, uint32(now.Nanosecond()))
		return 0
	})
	// a.k / a.l: (i32,i32) => 0
	add("k", func(ctx context.Context, m api.Module, _, _ uint32) uint32 { return 0 })
	add("l", func(ctx context.Context, m api.Module, _, _ uint32) uint32 { return 0 })
	// a.m: (i32,i32,i32,i32) => 0
	add("m", func(ctx context.Context, m api.Module, _, _, _, _ uint32) uint32 { return 0 })
	// a.n: (i32,i32,i32) => 0
	add("n", func(ctx context.Context, m api.Module, _, _, _ uint32) uint32 { return 0 })
	// a.o: (i32,i32) => -1
	add("o", func(ctx context.Context, m api.Module, _, _ uint32) uint32 { return ^uint32(0) })
	// a.p: (i32,i32,i32) => 0
	add("p", func(ctx context.Context, m api.Module, _, _, _ uint32) uint32 { return 0 })
	// a.q: (i32,i32,i32) => -1
	add("q", func(ctx context.Context, m api.Module, _, _, _ uint32) uint32 { return ^uint32(0) })
	// a.r: (i32,i32) => 0
	add("r", func(ctx context.Context, m api.Module, _, _ uint32) uint32 { return 0 })
	// a.s: (fd, low, high, whence, out) => 把 out 指向的两个 i32 清零，返回 0
	add("s", func(ctx context.Context, m api.Module, _, _, _, _, out uint32) uint32 {
		writeU32Le(m.Memory(), out, 0)
		writeU32Le(m.Memory(), out+4, 0)
		return 0
	})
	// a.t: (dst, src, len) => 等价 Uint8Array.copyWithin(dst, src, src+len)，返回 dst
	add("t", func(ctx context.Context, m api.Module, dst, src, length uint32) uint32 {
		mem := m.Memory()
		if length > 0 {
			if buf, ok := mem.Read(src, length); ok {
				// copyWithin 的源/目标可能重叠，先拷出一份再写回，避免别名覆盖。
				tmp := make([]byte, len(buf))
				copy(tmp, buf)
				mem.Write(dst, tmp)
			}
		}
		return dst
	})
	// a.u: (i32) => 0
	add("u", func(ctx context.Context, m api.Module, _ uint32) uint32 { return 0 })
	// a.v: (fd, pbuf) => memory[pbuf] = 4，返回 0
	add("v", func(ctx context.Context, m api.Module, _, pbuf uint32) uint32 {
		m.Memory().WriteByte(pbuf, 4)
		return 0
	})
	// a.w: (i32,i32) => 0
	add("w", func(ctx context.Context, m api.Module, _, _ uint32) uint32 { return 0 })
	// a.x: (cnt, sz) => 把 cnt/sz 指向的两个 i32 清零，返回 0
	add("x", func(ctx context.Context, m api.Module, cntPtr, szPtr uint32) uint32 {
		writeU32Le(m.Memory(), cntPtr, 0)
		writeU32Le(m.Memory(), szPtr, 0)
		return 0
	})
	// a.y: (i32,i32,i32) => 0
	add("y", func(ctx context.Context, m api.Module, _, _, _ uint32) uint32 { return 0 })
	// a.z..a.C: () => 0
	add("z", func(ctx context.Context, m api.Module) uint32 { return 0 })
	add("A", func(ctx context.Context, m api.Module) uint32 { return 0 })
	add("B", func(ctx context.Context, m api.Module) uint32 { return 0 })
	add("C", func(ctx context.Context, m api.Module) uint32 { return 0 })
	// a.D: (i32,i32,i32,i32) => 0
	add("D", func(ctx context.Context, m api.Module, _, _, _, _ uint32) uint32 { return 0 })
	// a.E / a.F: (i32,i32) => 0
	add("E", func(ctx context.Context, m api.Module, _, _ uint32) uint32 { return 0 })
	add("F", func(ctx context.Context, m api.Module, _, _ uint32) uint32 { return 0 })
	// a.G: () => void
	add("G", func(ctx context.Context, m api.Module) {})
	// a.H: (i32,i32,i32) => 0
	add("H", func(ctx context.Context, m api.Module, _, _, _ uint32) uint32 { return 0 })
	// a.I: (i32,i32) => 0
	add("I", func(ctx context.Context, m api.Module, _, _ uint32) uint32 { return 0 })
	// a.J..a.M: (i32) => 0
	add("J", func(ctx context.Context, m api.Module, _ uint32) uint32 { return 0 })
	add("K", func(ctx context.Context, m api.Module, _ uint32) uint32 { return 0 })
	add("L", func(ctx context.Context, m api.Module, _ uint32) uint32 { return 0 })
	add("M", func(ctx context.Context, m api.Module, _ uint32) uint32 { return 0 })
	// a.N: (i32,i32) => 0
	add("N", func(ctx context.Context, m api.Module, _, _ uint32) uint32 { return 0 })
	// a.O: () => 1（原 JS: O: () => 1）
	add("O", func(ctx context.Context, m api.Module) uint32 { return 1 })

	_, err := b.Instantiate(ctx)
	return err
}

// ticketStateFrom 从 context 取本次调用的时钟状态（缺省用 time.Now）。
func ticketStateFrom(ctx context.Context) *ticketStubState {
	st, _ := ctx.Value(ticketStubStateKey{}).(*ticketStubState)
	if st == nil {
		return newTicketStubState(nil)
	}
	return st
}

// buildTicket 对应原 JS buildTicket(pid, auth_ts, cnlid, guid)：
// 实例化 ticket WASM → P() → 依次分配 6 个 C 字符串 → T(...) → 从 outPtr 读 outLen 字节 → hex。
//
// 并发策略（关键）：原 JS 每次 buildTicket 都会 new 一个 Memory/Table 给 a 模块，
// 再 new WebAssembly.Instance(ticket)。ticket.wasm 的数据段(datas)和元素段(elems)
// 是在**实例化时**写进 a.a memory / a.b table 的，因此：
//   - 若共用一个 ticket 实例 → 后续调用的静态数据不会重新初始化，行为偏离 JS；
//   - 若共用同一个 a 实例的 memory → 多个 goroutine 的 R() 分配器会互相踩地址（越界）。
//
// 正确做法与 JS 完全一致：每次调用都用 ImportResolver 绑定一个全新的 a 实例
// （全新 memory + table），再实例化 ticket。这样没有共享可变状态，无需全局串行化。
func (e *Engine) buildTicket(ctx context.Context, pid, authTS, cnlid, guid string) (string, error) {
	st := newTicketStubState(e.now)
	callCtx := context.WithValue(ctx, ticketStubStateKey{}, st)

	// 原 JS: const memory = new WebAssembly.Memory({initial:256, maximum:256});
	//        const table = new WebAssembly.Table({element:'anyfunc', initial:743, maximum:743});
	// 这里实例化一个匿名 a 模块，它导出的 a.a(memory)/a.b(table) 就是此调用私有的。
	aInstance, err := e.runtime.InstantiateModule(callCtx, e.ticketAModule,
		wazero.NewModuleConfig().WithName("").WithStartFunctions())
	if err != nil {
		return "", fmt.Errorf("实例化 ticket a 模块失败: %w", err)
	}
	defer aInstance.Close(ctx)

	// 原 JS: const instance = new WebAssembly.Instance(TICKET_MODULE, imports);
	// 用 ImportResolver 把本次调用私有的 a 实例绑定给 ticket 的 a 导入。
	instCtx := experimental.WithImportResolver(callCtx, func(name string) api.Module {
		if name == ticketAModuleName {
			return aInstance
		}
		return nil
	})
	instance, err := e.runtime.InstantiateModule(instCtx, e.ticketModule,
		wazero.NewModuleConfig().WithName("").WithStartFunctions())
	if err != nil {
		return "", fmt.Errorf("实例化 ticket WASM 失败: %w", err)
	}
	defer instance.Close(ctx)

	// 原 JS: instance.exports.P();
	pFn := instance.ExportedFunction("P")
	if pFn == nil {
		return "", fmt.Errorf("ticket WASM 缺少导出 P")
	}
	if _, err = pFn.Call(callCtx); err != nil {
		return "", fmt.Errorf("ticket WASM P() 失败: %w", err)
	}

	allocFn := instance.ExportedFunction("R")
	tFn := instance.ExportedFunction("T")
	if allocFn == nil || tFn == nil {
		return "", fmt.Errorf("ticket WASM 缺少导出 R/T")
	}
	mem := instance.Memory()
	if mem == nil {
		return "", fmt.Errorf("ticket WASM 的 a.a memory 不可用")
	}

	// alloc 对应原 JS alloc(str)：R(len+1) 分配，写入字符串，末尾补 0。
	alloc := func(s string) (uint32, error) {
		b := []byte(s)
		res, err := allocFn.Call(callCtx, uint64(len(b)+1))
		if err != nil {
			return 0, fmt.Errorf("ticket WASM R(%d) 分配失败: %w", len(b)+1, err)
		}
		if len(res) == 0 {
			return 0, fmt.Errorf("ticket WASM R(%d) 无返回值", len(b)+1)
		}
		p := uint32(res[0])
		if !mem.Write(p, b) {
			return 0, fmt.Errorf("ticket WASM 写入字符串越界 (ptr=%d len=%d)", p, len(b))
		}
		mem.WriteByte(p+uint32(len(b)), 0)
		return p, nil
	}

	// 原 JS: const yspappid = '519748109'; const app_ver = 'V1.0.0';
	p1, err := alloc(pid)
	if err != nil {
		return "", err
	}
	p2, err := alloc(authTS)
	if err != nil {
		return "", err
	}
	p3, err := alloc(cnlid)
	if err != nil {
		return "", err
	}
	p4, err := alloc(guid)
	if err != nil {
		return "", err
	}
	p5, err := alloc(yspAppID)
	if err != nil {
		return "", err
	}
	p6, err := alloc(appVersion)
	if err != nil {
		return "", err
	}

	// 原 JS: outLen = pid.length + auth_ts.length + guid.length + yspappid.length + 14
	outLen := len(pid) + len(authTS) + len(guid) + len(yspAppID) + 14
	outRes, err := allocFn.Call(callCtx, uint64(outLen))
	if err != nil {
		return "", fmt.Errorf("ticket WASM R(outLen=%d) 分配失败: %w", outLen, err)
	}
	if len(outRes) == 0 {
		return "", fmt.Errorf("ticket WASM R(outLen=%d) 无返回值", outLen)
	}
	outPtr := uint32(outRes[0])

	// 原 JS: instance.exports.T(p1..p6, outPtr)
	if _, err = tFn.Call(callCtx,
		uint64(p1), uint64(p2), uint64(p3), uint64(p4), uint64(p5), uint64(p6), uint64(outPtr),
	); err != nil {
		return "", fmt.Errorf("ticket WASM T() 失败: %w", err)
	}

	// 原 JS: Buffer.from(memory.buffer, outPtr, outLen).toString('hex')
	raw, ok := mem.Read(outPtr, uint32(outLen))
	if !ok {
		return "", fmt.Errorf("ticket WASM 输出区越界 (ptr=%d len=%d)", outPtr, outLen)
	}
	out := make([]byte, len(raw))
	copy(out, raw)
	return hex.EncodeToString(out), nil
}
