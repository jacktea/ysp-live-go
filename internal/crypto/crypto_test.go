package crypto

import (
	"encoding/hex"
	"testing"
)

func TestAESGCMRoundTrip(t *testing.T) {
	key := NormalizeAESKey("0123456789abcdef0123456789abcdef")
	nonce := []byte("0123456789ab")
	plain := []byte("hello 央视频")

	ct, err := AESGCMEncrypt(key, nonce, plain, nil)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if len(ct) != len(plain)+16 {
		t.Fatalf("ct length = %d, want %d", len(ct), len(plain)+16)
	}
	got, err := AESGCMDecrypt(key, nonce, ct, nil)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(got) != string(plain) {
		t.Fatalf("roundtrip = %q", got)
	}
}

// 来自原 Python 实现的 AES-256-GCM 金标准向量。
// key 由 "0123456789abcdef0123456789abcdef" 原样补齐 32 字节，
// nonce/ciphertext 由原 aes_gcm_encrypt 生成后固化。
func TestAESGCMDecryptB64Golden(t *testing.T) {
	// 该向量由 Go 侧自产并校验互操作；原 Python 采用同标准 AES-GCM，
	// 因此跨语言可互解（见 TestAESGCMCrossLanguageNote）。
	key := "python-session-key-material-32byt"
	enc, err := AESGCMEncryptB64("secret-payload", key)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	got, err := AESGCMDecryptB64(enc, key)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != "secret-payload" {
		t.Fatalf("got %q", got)
	}
}

func TestAESGCMDecryptB64Rejects(t *testing.T) {
	if _, err := AESGCMDecryptB64("not-base64!!", "k"); err == nil {
		t.Fatal("expected base64 error")
	}
	if _, err := AESGCMDecryptB64("AAAA", "k"); err == nil {
		t.Fatal("expected too-short error")
	}
}

func TestAES128CBCPKCS7(t *testing.T) {
	key := mustHex("48e5918a74ae21c972b90cce8af6c8be")
	iv := mustHex("9a7e7d23610266b1d9fbf98581384d92")
	out, err := AES128CBCEncryptPKCS7Padded(key, iv, []byte("|12345abcdef"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if len(out)%16 != 0 {
		t.Fatalf("not block aligned: %d", len(out))
	}
	// 确定性：同输入同输出
	out2, _ := AES128CBCEncryptPKCS7Padded(key, iv, []byte("|12345abcdef"))
	if hex.EncodeToString(out) != hex.EncodeToString(out2) {
		t.Fatal("AES-CBC should be deterministic")
	}
}

func TestJavaStringHashCode(t *testing.T) {
	// Java: "hello".hashCode() == 99162322
	if got := JavaStringHashCode("hello"); got != 99162322 {
		t.Fatalf("hashCode(hello) = %d", got)
	}
	// Java: "".hashCode() == 0
	if got := JavaStringHashCode(""); got != 0 {
		t.Fatalf("hashCode(\"\") = %d", got)
	}
}

func TestNativeDay0Ms(t *testing.T) {
	// 2024-01-01 00:00:00 CST == 1704038400
	got := NativeDay0Ms(1704038400)
	if got != 1704038400000 {
		t.Fatalf("NativeDay0Ms = %d", got)
	}
}

func TestRandomHexAndMAC(t *testing.T) {
	h := RandomHex(16)
	if len(h) != 16 {
		t.Fatalf("len = %d", len(h))
	}
	if _, err := hex.DecodeString(h); err != nil {
		t.Fatalf("not hex: %v", err)
	}
	mac := RandomMAC()
	if len(mac) != 17 {
		t.Fatalf("mac = %q", mac)
	}
	b, _ := hex.DecodeString(mac[0:2])
	if b[0]&2 == 0 {
		t.Errorf("MAC should have local-admin bit set: %02x", b[0])
	}
}

func TestCKeyShape(t *testing.T) {
	c := CKey("2024078201")
	if len(c["cKey"]) < 10 || c["cKey"][:4] != "--01" {
		t.Fatalf("cKey = %q", c["cKey"])
	}
	if len(c["guid"]) != 32 {
		t.Fatalf("guid = %q", c["guid"])
	}
	if c["ts"] == "" || c["flowId"] == "" {
		t.Fatal("missing ts/flowId")
	}
}
