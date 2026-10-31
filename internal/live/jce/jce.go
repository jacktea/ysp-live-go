// Package jce 迁移原项目 ysp-live.py 中的 JCE 编码/解码（W/R 两个类）与
// jacc.ysp.cctv.cn 时移接口封装。
package jce

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// 常量（原 VER_NAME / VER_CODE / APP_ID / QMF_* / BIZ_ID / CHAN_ID）。
const (
	VerName      = "3.2.7.26212"
	VerCode      = "302070"
	AppID        = "1200013"
	QMFAppID     = 10012
	QMFPlatform  = 1
	BizID        = 0
	ChanID       = "10070"
	CmdTimeshift = 25312
)

// ErrJCE JCE 编解码错误。
var ErrJCE = errors.New("jce")

// Writer JCE 编码器（原 W）。
type Writer struct{ b bytes.Buffer }

// NewWriter 创建编码器。
func NewWriter() *Writer { return &Writer{} }

func (w *Writer) head(typ, tag int) {
	if tag < 15 {
		w.b.WriteByte(byte((tag&15)<<4 | typ&15))
	} else {
		w.b.WriteByte(byte(240 | typ&15))
		w.b.WriteByte(byte(tag))
	}
}

// Byte 写 int8。
func (w *Writer) Byte(v int, tag int) {
	if v == 0 {
		w.head(12, tag)
		return
	}
	w.head(0, tag)
	w.b.WriteByte(byte(int8(v)))
}

// Short 写 int16（区间内自动降级为 Byte）。
func (w *Writer) Short(v, tag int) {
	if v >= -128 && v <= 127 {
		w.Byte(v, tag)
		return
	}
	w.head(1, tag)
	var buf [2]byte
	binary.BigEndian.PutUint16(buf[:], uint16(int16(v)))
	w.b.Write(buf[:])
}

// Int 写 int32（区间内自动降级为 Short）。
func (w *Writer) Int(v, tag int) {
	if v >= -32768 && v <= 32767 {
		w.Short(v, tag)
		return
	}
	w.head(2, tag)
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], uint32(int32(v)))
	w.b.Write(buf[:])
}

// Long 写 int64。
func (w *Writer) Long(v int64, tag int) {
	if v >= math.MinInt32 && v <= math.MaxInt32 {
		w.Int(int(v), tag)
		return
	}
	w.head(3, tag)
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(v))
	w.b.Write(buf[:])
}

// Float 写 float32。
func (w *Writer) Float(v float32, tag int) {
	w.head(4, tag)
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], math.Float32bits(v))
	w.b.Write(buf[:])
}

// Double 写 float64。
func (w *Writer) Double(v float64, tag int) {
	w.head(5, tag)
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], math.Float64bits(v))
	w.b.Write(buf[:])
}

// String 写字符串（>255 字节自动用 type 7）。
func (w *Writer) String(s string, tag int) {
	if s == "" {
		w.head(6, tag)
		w.b.WriteByte(0)
		return
	}
	data := []byte(s)
	if len(data) > 255 {
		w.head(7, tag)
		var buf [4]byte
		binary.BigEndian.PutUint32(buf[:], uint32(len(data)))
		w.b.Write(buf[:])
		w.b.Write(data)
		return
	}
	w.head(6, tag)
	w.b.WriteByte(byte(len(data)))
	w.b.Write(data)
}

// Bytes 写二进制数据。
func (w *Writer) Bytes(data []byte, tag int) {
	w.head(13, tag)
	w.head(0, 0)
	w.Int(len(data), 0)
	w.b.Write(data)
}

// Struct 写结构体（回调内写入字段，自动补结束标记）。
func (w *Writer) Struct(fn func(*Writer), tag int) {
	w.head(10, tag)
	fn(w)
	w.head(11, 0)
}

// List 写列表头（仅长度，元素由调用方写入）。
func (w *Writer) List(n, tag int) {
	w.head(9, tag)
	w.Int(n, 0)
}

// Out 返回编码结果。
func (w *Writer) Out() []byte { return w.b.Bytes() }

// Reader JCE 解码器（原 R）。
type Reader struct {
	d []byte
	p int
}

// NewReader 创建解码器。
func NewReader(data []byte) *Reader { return &Reader{d: data} }

func (r *Reader) rem() int { return len(r.d) - r.p }

func (r *Reader) get(n int) ([]byte, error) {
	if r.p+n > len(r.d) {
		return nil, io.ErrUnexpectedEOF
	}
	b := r.d[r.p : r.p+n]
	r.p += n
	return b, nil
}

func (r *Reader) u8() (byte, error) {
	b, err := r.get(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (r *Reader) head() (typ, tag int, err error) {
	b, err := r.u8()
	if err != nil {
		return 0, 0, err
	}
	typ = int(b & 15)
	tag = int((b & 240) >> 4)
	if tag == 15 {
		t, err := r.u8()
		if err != nil {
			return 0, 0, err
		}
		tag = int(t)
	}
	return typ, tag, nil
}

func (r *Reader) value(typ int) (interface{}, error) {
	switch typ {
	case 0:
		b, err := r.get(1)
		if err != nil {
			return nil, err
		}
		return int(int8(b[0])), nil
	case 1:
		b, err := r.get(2)
		if err != nil {
			return nil, err
		}
		return int(int16(binary.BigEndian.Uint16(b))), nil
	case 2:
		b, err := r.get(4)
		if err != nil {
			return nil, err
		}
		return int(int32(binary.BigEndian.Uint32(b))), nil
	case 3:
		b, err := r.get(8)
		if err != nil {
			return nil, err
		}
		return int64(binary.BigEndian.Uint64(b)), nil
	case 4:
		b, err := r.get(4)
		if err != nil {
			return nil, err
		}
		return math.Float32frombits(binary.BigEndian.Uint32(b)), nil
	case 5:
		b, err := r.get(8)
		if err != nil {
			return nil, err
		}
		return math.Float64frombits(binary.BigEndian.Uint64(b)), nil
	case 6:
		n, err := r.u8()
		if err != nil {
			return nil, err
		}
		b, err := r.get(int(n))
		if err != nil {
			return nil, err
		}
		return string(b), nil
	case 7:
		b, err := r.get(4)
		if err != nil {
			return nil, err
		}
		n := binary.BigEndian.Uint32(b)
		body, err := r.get(int(n))
		if err != nil {
			return nil, err
		}
		return string(body), nil
	case 8:
		n, err := r.readInt()
		if err != nil {
			return nil, err
		}
		m := make(map[interface{}]interface{}, n)
		for i := 0; i < n; i++ {
			k, err := r.fv()
			if err != nil {
				return nil, err
			}
			v, err := r.fv()
			if err != nil {
				return nil, err
			}
			m[k] = v
		}
		return m, nil
	case 9:
		n, err := r.readInt()
		if err != nil {
			return nil, err
		}
		list := make([]interface{}, 0, n)
		for i := 0; i < n; i++ {
			v, err := r.fv()
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
		return list, nil
	case 10:
		return r.Struct()
	case 11:
		return nil, nil
	case 12:
		return 0, nil
	case 13:
		if _, _, err := r.head(); err != nil {
			return nil, err
		}
		n, err := r.readInt()
		if err != nil {
			return nil, err
		}
		return r.get(n)
	}
	return nil, fmt.Errorf("%w: unknown type %d", ErrJCE, typ)
}

func (r *Reader) fv() (interface{}, error) {
	t, _, err := r.head()
	if err != nil {
		return nil, err
	}
	return r.value(t)
}

func (r *Reader) readInt() (int, error) {
	t, _, err := r.head()
	if err != nil {
		return 0, err
	}
	v, err := r.value(t)
	if err != nil {
		return 0, err
	}
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	}
	return 0, fmt.Errorf("%w: not an int", ErrJCE)
}

// Struct 解码一个结构体（到结束标记 11 为止）。
func (r *Reader) Struct() (map[int]interface{}, error) {
	m := make(map[int]interface{})
	for r.rem() > 0 {
		t, tag, err := r.head()
		if err != nil {
			return nil, err
		}
		if t == 11 {
			break
		}
		v, err := r.value(t)
		if err != nil {
			return nil, err
		}
		m[tag] = v
	}
	return m, nil
}

// GetInt 从结构体取 int（缺省返回 def）。
func GetInt(m map[int]interface{}, tag, def int) int {
	if v, ok := m[tag]; ok {
		if n, ok := v.(int); ok {
			return n
		}
	}
	return def
}

// GetString 从结构体取 string（缺省返回 def）。
func GetString(m map[int]interface{}, tag int, def string) string {
	if v, ok := m[tag]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}

// WriteRequestHeader 写 JCE 请求头（原 _head / _qua 的组合）。
func WriteRequestHeader(w *Writer, cmd, reqID int, guid string) {
	w.Struct(func(ww *Writer) {
		ww.Int(reqID, 0)
		ww.Int(cmd, 1)
		ww.Struct(func(q *Writer) {
			q.String(VerName, 0)
			q.String(VerCode, 1)
			q.Int(1080, 2)
			q.Int(2400, 3)
			q.Int(3, 4)
			q.String("12", 5)
			q.Int(1, 6)
			q.Int(1, 7)
			q.Int(420, 8)
			q.String(ChanID, 9)
			for i := 10; i < 15; i++ {
				q.String("", i)
			}
			q.Struct(func(s *Writer) {
				s.Int(0, 0)
				s.Byte(0, 1)
				s.String("", 2)
			}, 15)
			q.String("", 16)
			q.String("", 17)
			q.String("", 18)
			q.Struct(func(s *Writer) {
				s.Int(0, 0)
				s.Float(0, 1)
				s.Float(0, 2)
				s.Double(0, 3)
			}, 19)
			q.String(guid[:min(16, len(guid))], 20)
			q.String("Pixel 6", 21)
			q.Int(1, 22)
			for i := 23; i < 27; i++ {
				q.Int(0, i)
			}
			q.String("", 27)
			q.String("", 28)
			q.String(guid, 29)
		}, 2)
		w.String(AppID, 3)
		w.String(guid, 4)
		w.List(0, 5)
		w.Struct(func(*Writer) {}, 6)
		w.List(0, 7)
		w.Int(0, 8)
		w.Int(0, 9)
		w.Int(0, 10)
	}, 0)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Wrap 封装完整 JCE 请求包（原 _wrap）。
//
// 原实现：
//
//	w = W()
//	w.struct(lambda ww: _head(ww, cmd, reqid), 0)   # 请求头，tag=0 的结构体
//	w.bytes(body, 1)                                # 请求体，tag=1 的 bytes
//	reqcmd = w.out()
//
// 修复移植 bug（关键）：早期 Go 版只写了请求头就取 out()，漏掉 tag=1 的 body 段，
// 导致 reqcmd 少 42 字节（tag/类型/长度前缀 4 字节 + body 38 字节），
// jacc.ysp.cctv.cn 返回 succeed=500（空 body），时移与 1080p 主线路全部不可用。
func Wrap(cmd int, body []byte, reqID int) []byte {
	w := NewWriter()
	WriteRequestHeader(w, cmd, reqID, Guid())
	w.Bytes(body, 1) // 关键：请求体作为 tag=1 的 bytes 字段追加在请求头之后
	reqCmd := w.Out()

	var inner bytes.Buffer
	inner.WriteByte(38)
	var l4 [4]byte
	binary.BigEndian.PutUint32(l4[:], uint32(len(reqCmd)+17))
	inner.Write(l4[:])
	inner.WriteByte(1)
	inner.Write(make([]byte, 10))
	inner.Write(reqCmd)
	inner.WriteByte(40)

	var gz bytes.Buffer
	// Python 侧 gzip.compress 默认压缩级别为 9，Go 的 NewWriter 默认是 -1（约等于 6）。
	// 这里显式对齐 9，使请求体字节分布与原实现一致，避免个别网关对差异敏感。
	zw, gzErr := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	if gzErr != nil {
		return nil
	}
	_, _ = zw.Write(inner.Bytes())
	_ = zw.Close()
	comp := gz.Bytes()

	var out bytes.Buffer
	out.WriteByte(19)
	binary.Write(&out, binary.BigEndian, int32(0))
	binary.Write(&out, binary.BigEndian, uint16(2))
	binary.Write(&out, binary.BigEndian, uint16(65281))
	binary.Write(&out, binary.BigEndian, uint16(cmd))
	binary.Write(&out, binary.BigEndian, uint16(0))
	binary.Write(&out, binary.BigEndian, int64(reqID))
	binary.Write(&out, binary.BigEndian, int32(531))
	binary.Write(&out, binary.BigEndian, int32(QMFAppID))
	binary.Write(&out, binary.BigEndian, int64(BizID))
	g := []byte(Guid())[:min32(32)]
	padded := make([]byte, 32)
	copy(padded, g)
	out.Write(padded)
	out.WriteByte(byte(QMFPlatform))
	binary.Write(&out, binary.BigEndian, int32(atoiOr(VerCode, 0)))
	out.Write(make([]byte, 6))
	out.WriteByte(0)
	binary.Write(&out, binary.BigEndian, uint16(0))
	binary.Write(&out, binary.BigEndian, uint16(0))
	binary.Write(&out, binary.BigEndian, int32(len(inner.Bytes())))
	out.Write(comp)
	out.WriteByte(3)

	raw := out.Bytes()
	binary.BigEndian.PutUint32(raw[1:5], uint32(len(raw)))
	return raw
}

func min32(n int) int {
	if n > 32 {
		return 32
	}
	return n
}

func atoiOr(s string, def int) int {
	var v int
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil {
		return def
	}
	return v
}

// Unwrap 解析 JCE 响应包，返回内层 body（原 _unwrap）。
func Unwrap(data []byte) ([]byte, error) {
	if len(data) < 90 || data[0] != 0x13 {
		return nil, fmt.Errorf("%w: bad response header", ErrJCE)
	}
	flags := binary.BigEndian.Uint32(data[21:25])
	payload := data[89 : len(data)-1]
	if flags&2 != 0 {
		zr, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("%w: gzip: %v", ErrJCE, err)
		}
		payload, err = io.ReadAll(zr)
		if err != nil {
			return nil, fmt.Errorf("%w: gzip: %v", ErrJCE, err)
		}
	}
	if len(payload) < 17 || payload[0] != '&' || payload[len(payload)-1] != '(' {
		return nil, fmt.Errorf("%w: bad payload framing", ErrJCE)
	}
	m, err := NewReader(payload[16 : len(payload)-1]).Struct()
	if err != nil {
		return nil, err
	}
	if v, ok := m[1].([]byte); ok {
		return v, nil
	}
	return nil, nil
}
