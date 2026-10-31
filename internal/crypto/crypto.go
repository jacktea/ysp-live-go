// Package crypto 迁移原项目 ysp-live.py 中的纯 Python 加密实现：
// AES-256-GCM（自主实现）、AES-128-CBC、RSA-OAEP-SHA256、TEA（JCE/bkliveinfo 用）、
// 以及各类哈希/指纹工具。Go 标准库提供了等价能力，因此这里用标准库实现，
// 并保留原项目用到的全部入口函数签名语义。
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// ErrCrypto 统一的加密错误类型（原项目抛 YsptpError）。
var ErrCrypto = errors.New("ysp-crypto")

// AK 央视频 AppKey。
const AK = "9f5c54c4ed0e50109b800f7e28fec205"

// RSAPublicKeyB64 云端设备注册用的 SPKI 公钥（原 RSA_PUBLIC_KEY_B64）。
const RSAPublicKeyB64 = "MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAkKeLy4ywWLSnBkwRyqYgF3HMIj05V5uuh5HjyEsZOWnu1NHu3jPQv3sr32wwQNYv5qapsNXmNgLUDHtgHZxqPQAYXltjSRc0qhcD286t62wOIHId8zXS3s1Jy4rgU4qjQWzI9rp/1sE0pMsmwTaJa4zuJ5iz8VwF8Av5oJ1k+HxY+/HLnjNlW1hmWLpuDYmkZYuAoTHa1VGeHQh9FEKI8ZcL3GTQphShUoC+Kg3P1hGUVTtCYapmzPS5lkAdwebuzwvTCfGiTErYZCnPBUSeV7BVlgjtLYIi29KvF0a8FHsJMfe/UdHcyW/RihsIYOtDQcRRpFGXyPXbVrzFJse24QIDAQAB"

// NormalizeAESKey 将字符串密钥补齐/截断为 32 字节（原 normalize_aes_key）。
func NormalizeAESKey(key string) []byte {
	raw := []byte(key)
	out := make([]byte, 32)
	copy(out, raw)
	return out
}

// newGCM 构造 AES-256-GCM AEAD。
func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// AESGCMDecrypt AES-256-GCM 解密（nonce + ciphertext + tag）。
func AESGCMDecrypt(key, nonce, ctAndTag, aad []byte) ([]byte, error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(nonce) != aead.NonceSize() {
		return nil, fmt.Errorf("%w: bad nonce length %d", ErrCrypto, len(nonce))
	}
	return aead.Open(nil, nonce, ctAndTag, aad)
}

// AESGCMEncrypt AES-256-GCM 加密，返回 ciphertext+tag。
func AESGCMEncrypt(key, nonce, plaintext, aad []byte) ([]byte, error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(nonce) != aead.NonceSize() {
		return nil, fmt.Errorf("%w: bad nonce length %d", ErrCrypto, len(nonce))
	}
	return aead.Seal(nil, nonce, plaintext, aad), nil
}

// AESGCMDecryptB64 解密 base64(nonce||ct||tag)，密钥为任意长度字符串（补齐到 32 字节）。
func AESGCMDecryptB64(value, key string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", fmt.Errorf("%w: base64 decode failed: %v", ErrCrypto, err)
	}
	if len(raw) <= 12 {
		return "", fmt.Errorf("%w: AES-GCM payload too short", ErrCrypto)
	}
	plain, err := AESGCMDecrypt(NormalizeAESKey(key), raw[:12], raw[12:], nil)
	if err != nil {
		return "", fmt.Errorf("%w: AES-GCM decrypt failed", ErrCrypto)
	}
	return string(plain), nil
}

// AESGCMEncryptB64 使用随机 12 字节 nonce 加密并返回 base64(nonce||ct||tag)。
func AESGCMEncryptB64(value, key string) (string, error) {
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	enc, err := AESGCMEncrypt(NormalizeAESKey(key), nonce, []byte(value), nil)
	if err != nil {
		return "", fmt.Errorf("%w: AES-GCM encrypt failed", ErrCrypto)
	}
	return base64.StdEncoding.EncodeToString(append(nonce, enc...)), nil
}

// AES128CBCEncryptPKCS7Padded aes-128-cbc + PKCS#7 填充（ysp-engine 的 cKey 用）。
func AES128CBCEncryptPKCS7Padded(key, iv, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	bs := block.BlockSize()
	pad := bs - len(plaintext)%bs
	buf := make([]byte, len(plaintext)+pad)
	copy(buf, plaintext)
	for i := len(plaintext); i < len(buf); i++ {
		buf[i] = byte(pad)
	}
	out := make([]byte, len(buf))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, buf)
	return out, nil
}

// RSAPublicKeySPKI 解析内嵌的 SPKI 公钥。
func RSAPublicKeySPKI() (*rsa.PublicKey, error) {
	der, err := base64.StdEncoding.DecodeString(RSAPublicKeyB64)
	if err != nil {
		return nil, fmt.Errorf("%w: RSA public key base64 decode failed: %v", ErrCrypto, err)
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("%w: parse SPKI failed: %v", ErrCrypto, err)
	}
	rp, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%w: not an RSA public key", ErrCrypto)
	}
	return rp, nil
}

// RSAEncryptDeviceID 用 RSA-OAEP-SHA256 分段加密 device id，返回 base64。
func RSAEncryptDeviceID(deviceID string) (string, error) {
	pub, err := RSAPublicKeySPKI()
	if err != nil {
		return "", err
	}
	k := (pub.N.BitLen() + 7) / 8
	chunk := k - 2*sha256.Size - 2
	data := []byte(deviceID)
	var out []byte
	for i := 0; i < len(data); i += chunk {
		end := i + chunk
		if end > len(data) {
			end = len(data)
		}
		enc, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, data[i:end], nil)
		if err != nil {
			return "", fmt.Errorf("%w: RSA encrypt failed: %v", ErrCrypto, err)
		}
		out = append(out, enc...)
	}
	return base64.StdEncoding.EncodeToString(out), nil
}

// SHA1Upper 大写十六进制 sha1（原 sha1_upper）。
func SHA1Upper(value string) string {
	sum := sha1.Sum([]byte(value))
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// SHA256Hex 小写十六进制 sha256。
func SHA256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// JavaStringHashCode 复刻 Java String.hashCode（32 位有符号）。
func JavaStringHashCode(value string) int32 {
	var h int32
	for _, r := range value {
		h = h*31 + int32(r)
	}
	return h
}

// JavaUUIDFromHashes 复刻原 java_uuid_from_hashes：msb/lsb 按无符号拼接成 32 位十六进制。
func JavaUUIDFromHashes(msbHash, lsbHash int32) string {
	msb := uint64(uint32(msbHash))
	lsb := uint64(uint32(lsbHash))
	return fmt.Sprintf("%016x%016x", msb, lsb)
}

// NativeDay0Ms 当日 0 点（东八区）毫秒时间戳（原 native_day0_ms）。
func NativeDay0Ms(nowS int64) int64 {
	return 86400000*((nowS+28800)/86400) - 28800000
}

// ComputeFingerprint 计算 X-Fingerprint 及其时间戳（原 compute_fingerprint）。
func ComputeFingerprint(xUID string, nowMs int64) (fingerprint string, ts int64, day0Ms int64) {
	day0Ms = NativeDay0Ms(nowMs / 1000)
	first := SHA256Hex(fmt.Sprintf("%s%s%d%d", AK, xUID, nowMs, day0Ms))
	return SHA256Hex(first), nowMs, day0Ms
}

// RandomHex 生成 n 位小写十六进制随机串。
func RandomHex(length int) string {
	const chars = "0123456789abcdef"
	buf := make([]byte, length)
	_, _ = rand.Read(buf)
	for i := range buf {
		buf[i] = chars[int(buf[i])%len(chars)]
	}
	return string(buf)
}

// RandomMAC 生成随机 MAC（首字节本地管理位，原 random_mac_address）。
func RandomMAC() string {
	raw := make([]byte, 6)
	_, _ = rand.Read(raw)
	raw[0] = (raw[0] | 2) & 254
	parts := make([]string, 6)
	for i, b := range raw {
		parts[i] = fmt.Sprintf("%02x", b)
	}
	return strings.Join(parts, ":")
}

// GFMult GF(2^128) 乘法（保留供测试与 AES-GCM 自主实现对照）。
func GFMult(x, y *big.Int) *big.Int { return gfMul(x, y) }
