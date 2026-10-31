package jce

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
)

var (
	guidOnce sync.Once
	guidVal  string
)

// Guid 返回进程级随机 GUID（原模块级 GUID 常量，32 位十六进制）。
func Guid() string {
	guidOnce.Do(func() {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		guidVal = hex.EncodeToString(b)
	})
	return guidVal
}
