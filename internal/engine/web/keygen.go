package web

// 本文件移植原 ysp-engine.js 的 runKeygen()（第 21-85 行）：
// 用 wazero 执行内嵌的 keygen.wasm（Rust/wasm-bindgen 编译产物），
// 并通过 wbg 宿主模块复刻原 JS 里手写的那套 wbg 导入桩。
//
// 原 JS 每次调用 runKeygen 都会：
//  1) 新建 heap 数组（129/130/131 预置 'null'/'true'/'false'，其余 null），
//  2) 新建一个 WebAssembly.Instance（因此 keygen 的内存/堆天然按调用隔离），
//  3) 调用导出 get_token_rnd / get_signature 取字符串。
//
// Go 版选择"编译一次 + 每次调用新建实例"（等价于 JS 的每次 new Instance），
// 因此 keygen 无需加锁、天然并发安全；每次调用的状态通过 context 传给宿主函数。

import (
	"context"
	"fmt"
	"strings"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// keygenHeapBase JS 的 heap 从 132 开始 push（0..131 是 wasm-bindgen 的保留索引）。
const (
	keygenHeapReserved = 132
	keygenHeapNull     = 129
	keygenHeapTrue     = 130
	keygenHeapFalse    = 131
)

// keygenStateKey 每次 runKeygen 调用的私有状态（对应 JS 闭包里的 heap/values）。
type keygenStateKey struct{}

// keygenState 等价 JS runKeygen 内的 heap 与 values。
type keygenState struct {
	heap   []string
	values map[string]string
}

// cloneStrings 复制一份字符串切片，避免宿主回调之间共享底层数组造成数据竞争。
func cloneStrings(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// newKeygenState 构造一次 keygen 调用的状态（对应 JS 里 heap 的初始化）。
func newKeygenState(values map[string]string) *keygenState {
	heap := make([]string, keygenHeapReserved)
	heap[keygenHeapNull] = "null"
	heap[keygenHeapTrue] = "true"
	heap[keygenHeapFalse] = "false"
	return &keygenState{heap: heap, values: values}
}

// push 对应 JS 的 heap.push(val) + return heap.length - 1。
func (s *keygenState) push(val string) int32 {
	s.heap = append(s.heap, val)
	return int32(len(s.heap) - 1)
}

// get 对应 JS 的 heap[idx]（越界/空槽都视为空串，与原 JS 的 undefined 语义一致）。
func (s *keygenState) get(idx int32) (string, bool) {
	if idx < 0 || int(idx) >= len(s.heap) {
		return "", false
	}
	return s.heap[idx], true
}

// dropRef 对应 JS 的 __wbindgen_object_drop_ref：仅对 idx >= 132 的槽位置空。
func (s *keygenState) dropRef(idx int32) {
	if idx < keygenHeapReserved || idx < 0 {
		return
	}
	// 修复原项目 bug: JS 的 heap[idx] = null 在 idx 超出数组长度时会拉长数组并留空洞，
	// Go 版显式补齐，避免后续 heap[idx] 读到"空洞"的歧义语义。
	for int(idx) >= len(s.heap) {
		s.heap = append(s.heap, "")
	}
	s.heap[idx] = ""
}

// registerWbgModule 注册 wbg 宿主模块（原 JS imports.wbg 的三个函数）。
func (e *Engine) registerWbgModule(ctx context.Context) error {
	_, err := e.runtime.NewHostModuleBuilder("wbg").
		// 原 JS: __wbg_get_9c1840f7ecd81363(ptr, len) => 从 wasm 内存读 key，查 values，push 进 heap
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module, ptr, n uint32) uint32 {
			st := keygenStateFrom(ctx)
			if st == nil {
				return 0
			}
			var key string
			if b, ok := m.Memory().Read(ptr, n); ok {
				key = string(b)
			}
			// 原 JS: const val = values[key] || '';
			val := st.values[key]
			return uint32(st.push(val))
		}).
		Export("__wbg_get_9c1840f7ecd81363").
		// 原 JS: __wbindgen_string_get(out_ptr, obj_idx) => 把 heap 里的字符串写回 wasm 内存
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module, outPtr, objIdx uint32) {
			st := keygenStateFrom(ctx)
			if st == nil {
				return
			}
			var (
				ptr uint32
				n   uint32
			)
			if val, ok := st.get(int32(objIdx)); ok && len(val) > 0 {
				b := []byte(val)
				malloc := m.ExportedFunction("__wbindgen_malloc")
				if malloc == nil {
					return
				}
				res, err := malloc.Call(ctx, uint64(len(b)), 1)
				if err != nil || len(res) == 0 {
					return
				}
				ptr = uint32(res[0])
				if !m.Memory().Write(ptr, b) {
					return
				}
				n = uint32(len(b))
			}
			// 原 JS: i32[out_ptr>>2] = ptr; i32[(out_ptr>>2)+1] = len;
			writeU32Le(m.Memory(), outPtr, ptr)
			writeU32Le(m.Memory(), outPtr+4, n)
		}).
		Export("__wbindgen_string_get").
		// 原 JS: __wbindgen_object_drop_ref(idx) => if (idx >= 132) heap[idx] = null
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, idx uint32) {
			if st := keygenStateFrom(ctx); st != nil {
				st.dropRef(int32(idx))
			}
		}).
		Export("__wbindgen_object_drop_ref").
		Instantiate(ctx)
	return err
}

// keygenStateFrom 从 context 取出本次调用的 keygen 状态。
func keygenStateFrom(ctx context.Context) *keygenState {
	st, _ := ctx.Value(keygenStateKey{}).(*keygenState)
	return st
}

// writeU32Le 向 wasm 内存写一个 LE uint32（对应 JS 的 Int32Array 下标赋值）。
func writeU32Le(mem api.Memory, offset, value uint32) {
	if mem == nil {
		return
	}
	var buf [4]byte
	buf[0] = byte(value)
	buf[1] = byte(value >> 8)
	buf[2] = byte(value >> 16)
	buf[3] = byte(value >> 24)
	mem.Write(offset, buf[:])
}

// readU32Le 从 wasm 内存读一个 LE uint32。
func readU32Le(mem api.Memory, offset uint32) (uint32, bool) {
	b, ok := mem.Read(offset, 4)
	if !ok {
		return 0, false
	}
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24, true
}

// keygenValues 原 JS runKeygen 的 values 对象，state 来自网页版全局状态。
type keygenValues struct {
	GUID     string
	YspAppID string
	Version  string
	Host     string
	Protocol string
	Token    string
	Input    string
	TS       string
}

// toMap 转成原 JS values 的字面量映射。
func (v keygenValues) toMap() map[string]string {
	return map[string]string{
		"cctvh5openapi.state.guid":     v.GUID,
		"cctvh5openapi.state.yspappid": v.YspAppID,
		"cctvh5openapi.state.version":  v.Version,
		"window.location.host":         v.Host,
		"window.location.protocol":     v.Protocol,
		"cctvh5openapi.state.token":    v.Token,
		"cctvh5openapi.state.input":    v.Input,
		"cctvh5openapi.state.ts":       v.TS,
	}
}

// runKeygen 对应原 JS runKeygen(state)：新建实例、注入状态、返回两个取值函数。
// 返回的闭包只在同一次调用内使用（实例在 Close 前一直有效）。
//
// 注意：宿主函数(imports.wbg)的状态是随 context 传进来的，因此所有
// 对导出函数的 Call 都必须使用带状态的 ctx（call.ctx），不能用外层 ctx。
type keygenCall struct {
	engine   *Engine
	instance api.Module
	state    *keygenState
	ctx      context.Context
}

// newKeygenCall 等价 JS: wasmInstance = new WebAssembly.Instance(KEYGEN_MODULE, imports)。
func (e *Engine) newKeygenCall(ctx context.Context, values keygenValues) (*keygenCall, error) {
	state := newKeygenState(values.toMap())
	callCtx := context.WithValue(ctx, keygenStateKey{}, state)
	instance, err := e.runtime.InstantiateModule(callCtx, e.keygenModule,
		wazero.NewModuleConfig().WithName("").WithStartFunctions())
	if err != nil {
		return nil, fmt.Errorf("实例化 keygen WASM 失败: %w", err)
	}
	return &keygenCall{engine: e, instance: instance, state: state, ctx: callCtx}, nil
}

// Close 释放本次调用创建的实例。
func (c *keygenCall) Close(ctx context.Context) {
	if c.instance != nil {
		_ = c.instance.Close(ctx)
		c.instance = nil
	}
}

// callStringExport 对应原 JS callStringExport(fn)：
// 在栈上开 16 字节返回值区，调用导出函数，读回 (ptr,len)，恢复栈指针并释放内存。
func (c *keygenCall) callStringExport(ctx context.Context, name string) (string, error) {
	// 统一使用携带本次调用状态的 ctx，宿主 wbg 函数依赖它取 heap/values。
	ctx = c.ctx
	fn := c.instance.ExportedFunction(name)
	if fn == nil {
		return "", fmt.Errorf("keygen WASM 缺少导出 %s", name)
	}
	addStack := c.instance.ExportedFunction("__wbindgen_add_to_stack_pointer")
	free := c.instance.ExportedFunction("__wbindgen_free")
	if addStack == nil || free == nil {
		return "", fmt.Errorf("keygen WASM 缺少 wasm-bindgen 栈/内存导出")
	}
	mem := c.instance.Memory()
	if mem == nil {
		return "", fmt.Errorf("keygen WASM 未导出 memory")
	}

	// 原 JS: __wbindgen_add_to_stack_pointer(-16)。wasm 里是 i32，Go 侧用回绕后的 uint32 承载。
	res, err := addStack.Call(ctx, uint64(^uint32(15)))
	if err != nil || len(res) == 0 {
		return "", fmt.Errorf("keygen __wbindgen_add_to_stack_pointer(-16) 失败: %w", err)
	}
	retPtr := uint32(res[0])
	if retPtr == 0 {
		return "", fmt.Errorf("keygen 栈指针返回空地址")
	}
	if _, err = fn.Call(ctx, uint64(retPtr)); err != nil {
		return "", fmt.Errorf("keygen %s 调用失败: %w", name, err)
	}
	ptr, okPtr := readU32Le(mem, retPtr)
	length, okLen := readU32Le(mem, retPtr+4)
	if !okPtr || !okLen {
		return "", fmt.Errorf("keygen %s 返回值越界", name)
	}
	var out string
	if length > 0 {
		b, ok := mem.Read(ptr, length)
		if !ok {
			return "", fmt.Errorf("keygen %s 返回字符串越界 (ptr=%d len=%d)", name, ptr, length)
		}
		out = strings.ToValidUTF8(string(b), "\uFFFD")
	}
	if _, err = addStack.Call(ctx, 16); err != nil {
		return "", fmt.Errorf("keygen 恢复栈指针失败: %w", err)
	}
	// 原 JS: if (ptr > 0 && len >= 0) __wbindgen_free(ptr, len, 1)
	// 修复原项目 bug: JS 的 `len >= 0` 恒为真（无符号长度），Go 版直接按 ptr>0 释放，
	// 避免 len 为负数(long)时的隐式转换歧义。
	if ptr > 0 && length > 0 {
		if _, err = free.Call(ctx, uint64(ptr), uint64(length), 1); err != nil {
			return "", fmt.Errorf("keygen __wbindgen_free 失败: %w", err)
		}
	}
	return out, nil
}

// GetRnd 等价 JS kg.getRnd()：调用 get_token_rnd 取随机串。
func (c *keygenCall) GetRnd(ctx context.Context) (string, error) {
	return c.callStringExport(ctx, "get_token_rnd")
}

// GetSign 等价 JS kg.getSign()：调用 get_signature 取签名。
func (c *keygenCall) GetSign(ctx context.Context) (string, error) {
	return c.callStringExport(ctx, "get_signature")
}

// getRnd 是 runKeygen + getRnd 的便捷封装（对应原 JS: runKeygen(state).getRnd()）。
func (e *Engine) getRnd(ctx context.Context, v keygenValues) (string, error) {
	call, err := e.newKeygenCall(ctx, v)
	if err != nil {
		return "", err
	}
	defer call.Close(ctx)
	return call.GetRnd(ctx)
}

// getSign 是 runKeygen + getSign 的便捷封装（对应原 JS: runKeygen(state).getSign()）。
func (e *Engine) getSign(ctx context.Context, v keygenValues) (string, error) {
	call, err := e.newKeygenCall(ctx, v)
	if err != nil {
		return "", err
	}
	defer call.Close(ctx)
	return call.GetSign(ctx)
}
