#!/usr/bin/env python3
"""从原项目 ysp-engine.js 抽取资源，生成 Go 移植所需的二进制/代码资产。

用法：
    python3 internal/engine/web/gen/gen_assets.py [ysp-engine.js 路径]

生成物（相对本脚本所在目录）：
    ../wasm/keygen.wasm   KEYGEN_B64 解码后的 keygen 模块（原样，未改动）
    ../wasm/ticket.wasm   TICKET_B64 解码后的 ticket 模块（原样，未改动）
    ../wasm/ticket_a.wasm ticket.wasm 的 a 导入模块的"实现载体"（约 671 字节）。
                          ticket.wasm 把 a 模块的 **memory a.a(256页,max256)**、
                          **funcref table a.b(743,max743)** 和 39 个函数 a.c..a.O
                          全部当作"导入项"，即这些资源必须由我们（宿主侧）提供。
                          原 JS 用 new WebAssembly.Memory/Table + 39 个 JS 闭包提供它们。
                          wazero 的 HostModuleBuilder 只能导出函数、无法导出
                          memory/table，所以这里生成一个极小的 wasm 模块来同时导出
                          memory + table + 39 个函数；39 个函数体本身只是"转出口"，
                          真正的桩逻辑在 Go 的 a_impl 宿主模块里（见 ticket.go）。
                          注意签名不是手抄的：本脚本解析 ticket.wasm 的 import 段
                          得到精确签名（wazero 对导入签名是严格匹配的）。
    ../channels_gen.go    CHANNELS / ALIAS_MAP 生成的 Go 表
"""
import base64
import json
import os
import re
import struct
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
WEB = os.path.dirname(HERE)

JS_PATH = sys.argv[1] if len(sys.argv) > 1 else "/Users/xiaogang/github/ysp-live/ysp-engine.js"


def read_const(name):
    src = open(JS_PATH, encoding="utf-8").read()
    m = re.search(r"^const " + name + r" = (.*);\s*$", src, re.M)
    if not m:
        raise SystemExit("找不到常量 " + name)
    return m.group(1)


# ---------- wasm 编码小工具 ----------
def uleb(n):
    out = b""
    while True:
        b = n & 0x7F
        n >>= 7
        if n:
            out += bytes([b | 0x80])
        else:
            return out + bytes([b])


def vec(items):
    return uleb(len(items)) + b"".join(items)


def wasm_name(s):
    b = s.encode("utf-8")
    return uleb(len(b)) + b


def section(sid, payload):
    return bytes([sid]) + uleb(len(payload)) + payload


# ---------- ticket 的宿主模块 a ----------
# 桩函数签名不手抄，直接从 ticket.wasm 的 import 段解析，保证与 wasm 完全一致
# （wazero 对导入函数的签名是严格匹配的，差一个参数都会实例化失败）。


def parse_ticket_a_imports(ticket_bytes):
    """解析 ticket.wasm 的 import 段，返回 (type 列表, [(name, 函数类型), ...])。

    返回的项只包含来自模块 a 的 39 个函数导入（跳过 a.a memory / a.b table）。
    """
    b = ticket_bytes
    assert b[:4] == b"\0asm"
    i = 8
    types, a_funcs = [], []
    while i < len(b):
        sid = b[i]
        i += 1
        size, i = read_leb(b, i)
        end = i + size
        if sid == 1:
            cnt, j = read_leb(b, i)
            for _ in range(cnt):
                assert b[j] == 0x60
                j += 1
                np, j = read_leb(b, j)
                ps = list(b[j:j + np])
                j += np
                nr, j = read_leb(b, j)
                rs = list(b[j:j + nr])
                j += nr
                types.append((ps, rs))
        elif sid == 2:
            cnt, j = read_leb(b, i)
            for _ in range(cnt):
                mod, j = read_name(b, j)
                nm, j = read_name(b, j)
                kind = b[j]
                j += 1
                if kind == 0:
                    tidx, j = read_leb(b, j)
                    if mod == "a" and not nm in ("a", "b"):
                        a_funcs.append((nm, tidx))
                elif kind == 1:
                    j += 1
                    flags, j = read_leb(b, j)
                    _, j = read_leb(b, j)
                    if flags & 1:
                        _, j = read_leb(b, j)
                elif kind == 2:
                    flags, j = read_leb(b, j)
                    _, j = read_leb(b, j)
                    if flags & 1:
                        _, j = read_leb(b, j)
                elif kind == 3:
                    j += 2
        i = end
    return types, a_funcs


def read_leb(b, i):
    r, s = 0, 0
    while True:
        x = b[i]
        i += 1
        r |= (x & 0x7F) << s
        if not x & 0x80:
            return r, i
        s += 7


def read_name(b, i):
    n, i = read_leb(b, i)
    return b[i:i + n].decode("utf-8", "replace"), i + n


def build_ticket_a(ticket_bytes):
    """生成 ticket.wasm 的 a 模块：导出 memory a + table b + 39 个函数转出口。"""
    types, a_funcs = parse_ticket_a_imports(ticket_bytes)
    # 去重类型表
    uniq, type_index = [], {}
    for nm, tidx in a_funcs:
        sig = (tuple(types[tidx][0]), tuple(types[tidx][1]))
        if sig not in type_index:
            type_index[sig] = len(uniq)
            uniq.append(sig)
    type_sec = section(1, vec([
        bytes([0x60]) + vec([bytes([p]) for p in ps]) + vec([bytes([r]) for r in rs])
        for ps, rs in uniq
    ]))
    # import 段：39 个函数全部来自宿主模块 a_impl，导出名与 wasm 期望的一致
    imports = []
    for nm, tidx in a_funcs:
        sig = (tuple(types[tidx][0]), tuple(types[tidx][1]))
        imports.append(wasm_name("a_impl") + wasm_name(nm) + bytes([0x00]) + uleb(type_index[sig]))
    import_sec = section(2, vec(imports))
    # table 段：funcref min=743 max=743（对应 JS 的 Table({initial:743, maximum:743})）
    table_sec = section(4, vec([bytes([0x70, 0x01]) + uleb(743) + uleb(743)]))
    # memory 段：min=256 max=256 页（对应 JS 的 Memory({initial:256, maximum:256})）
    mem_sec = section(5, vec([bytes([0x01]) + uleb(256) + uleb(256)]))
    # export 段：memory a、table b、39 个函数 c..O
    exports = [wasm_name("a") + bytes([0x02]) + uleb(0),
               wasm_name("b") + bytes([0x01]) + uleb(0)]
    for i, (nm, _) in enumerate(a_funcs):
        exports.append(wasm_name(nm) + bytes([0x00]) + uleb(i))
    export_sec = section(7, vec(exports))
    return b"\0asm" + struct.pack("<I", 1) + type_sec + import_sec + table_sec + mem_sec + export_sec


def go_string(s):
    # 生成 Go 双引号字符串字面量（保留中文，转义反斜杠/引号）
    return json.dumps(s, ensure_ascii=False)


def main():
    wasm_dir = os.path.join(WEB, "wasm")
    os.makedirs(wasm_dir, exist_ok=True)

    keygen = base64.b64decode(read_const("KEYGEN_B64"))
    ticket = base64.b64decode(read_const("TICKET_B64"))
    open(os.path.join(wasm_dir, "keygen.wasm"), "wb").write(keygen)
    open(os.path.join(wasm_dir, "ticket.wasm"), "wb").write(ticket)
    a_module = build_ticket_a(ticket)
    open(os.path.join(wasm_dir, "ticket_a.wasm"), "wb").write(a_module)
    print("keygen.wasm   %8d bytes" % len(keygen))
    print("ticket.wasm   %8d bytes" % len(ticket))
    print("ticket_a.wasm %8d bytes" % len(a_module))

    channels = json.loads(read_const("CHANNELS"))
    alias = json.loads(read_const("ALIAS_MAP"))

    lines = [
        "// Code generated by gen/gen_assets.py from ysp-engine.js; DO NOT EDIT.",
        "",
        "package web",
        "",
        "// channelMeta 网页版接口需要的频道元数据（对应原 ysp-engine.js 的 CHANNELS）。",
        "type channelMeta struct {",
        "\tName    string // 频道中文名（/all.m3u 用）",
        "\tCnLid   string // 时移/直播串流 ID",
        "\tLivePid string // 直播频道 ID",
        "\tDefn    string // fhd/shd",
        "}",
        "",
        "// channels 原 ysp-engine.js 的 CHANNELS 表，键为规范 slug。",
        "var channels = map[string]channelMeta{",
    ]
    for slug, meta in channels.items():
        lines.append('\t%s: {%s, %s, %s, %s},' % (
            go_string(slug), go_string(meta["name"]), go_string(meta["cnlid"]),
            go_string(meta["livepid"]), go_string(meta["defn"])))
    lines += [
        "}",
        "",
        "// channelOrder CHANNELS 的声明顺序（/all.m3u 按此顺序输出）。",
        "var channelOrder = []string{",
    ]
    for i, slug in enumerate(channels.keys()):
        lines.append("\t%s," % go_string(slug))
        if i % 6 == 5:
            pass
    lines += [
        "}",
        "",
        "// aliasMap 原 ysp-engine.js 的 ALIAS_MAP（别名 → 规范 slug）。",
        "var aliasMap = map[string]string{",
    ]
    for k, v in alias.items():
        lines.append("\t%s: %s," % (go_string(k), go_string(v)))
    lines += ["}", ""]
    open(os.path.join(WEB, "channels_gen.go"), "w", encoding="utf-8").write("\n".join(lines))
    print("channels_gen.go  %d channels / %d aliases" % (len(channels), len(alias)))


if __name__ == "__main__":
    main()
