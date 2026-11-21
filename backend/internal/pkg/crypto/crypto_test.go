package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"io"
	"strings"
	"testing"
)

// TestEncryptDecryptAnyKeyLength 锁住「口令长度任意」这一契约。
//
// 历史缺陷：newGCM 直接把口令当 AES 密钥，只有 16/24/32 字节能过；而文档教
// `openssl rand -hex 32`（64 字符），于是按文档部署的实例**建不出 API KEY**
// （aes: invalid key size 64），且 2FA 绑定同样 500。
func TestEncryptDecryptAnyKeyLength(t *testing.T) {
	for _, key := range []string{
		"k",
		"0123456789abcdef",                     // 16：旧实现唯一可用的长度
		"0123456789abcdef01234567",             // 24
		"0123456789abcdef0123456789abcdef",     // 32
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", // 64：文档推荐值
	} {
		ct, err := Encrypt([]byte(key), []byte("sk-plaintext"))
		if err != nil {
			t.Fatalf("len=%d Encrypt 失败: %v", len(key), err)
		}
		got, err := Decrypt([]byte(key), ct)
		if err != nil {
			t.Fatalf("len=%d Decrypt 失败: %v", len(key), err)
		}
		if string(got) != "sk-plaintext" {
			t.Fatalf("len=%d 明文不符: %q", len(key), got)
		}
	}
}

// TestDecryptLegacyCiphertext 锁住「旧密文仍可解」。
//
// v2.1 及更早把口令原文直接当 AES 密钥。升级后若只认派生密钥，
// 已绑 2FA 的用户会在登录时拿到 AuthUnavailable，且没有自助恢复手段。
func TestDecryptLegacyCiphertext(t *testing.T) {
	for _, key := range []string{
		"0123456789abcdef",                 // 16
		"0123456789abcdef01234567",         // 24
		"0123456789abcdef0123456789abcdef", // 32
	} {
		legacy := encryptLegacy(t, []byte(key), []byte("JBSWY3DPEHPK3PXP"))
		got, err := Decrypt([]byte(key), legacy)
		if err != nil {
			t.Fatalf("len=%d 旧密文解密失败: %v", len(key), err)
		}
		if string(got) != "JBSWY3DPEHPK3PXP" {
			t.Fatalf("len=%d 旧密文明文不符: %q", len(key), got)
		}
	}
}

// TestDecryptLegacyUnavailableForOddLength 确认回退不会掩盖"密钥本身不合法"：
// 非 16/24/32 的旧密文无从谈起，应返回派生路径的错误而不是静默成功。
func TestDecryptLegacyUnavailableForOddLength(t *testing.T) {
	key := []byte("a-64-char-key-which-old-code-could-never-use-as-an-aes-key-000000")
	ct, err := Encrypt(key, []byte("x"))
	if err != nil {
		t.Fatalf("Encrypt 失败: %v", err)
	}
	other := []byte("another-key-of-same-length-but-different-bytes-111111111111")
	if _, err := Decrypt(other, ct); err == nil {
		t.Fatal("换密钥后仍能解密，说明密钥未参与派生")
	}
}

func TestEncryptRejectsEmptyKey(t *testing.T) {
	if _, err := Encrypt(nil, []byte("x")); err == nil {
		t.Fatal("空密钥必须报错：它会派生出确定的 32 字节，等于所有人的密钥一样")
	} else if !strings.Contains(err.Error(), "密钥为空") {
		t.Fatalf("错误文案应说明密钥为空，实际: %v", err)
	}
}

// encryptLegacy 复刻 v2.1 的加密实现（原文口令直接当 AES 密钥），只用于测试回退路径。
func encryptLegacy(t *testing.T, key, plaintext []byte) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("legacy aes.NewCipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("legacy NewGCM: %v", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		t.Fatalf("legacy nonce: %v", err)
	}
	sealed := gcm.Seal(nonce, nonce, plaintext, nil)
	if bytes.Contains(sealed, plaintext) {
		t.Fatal("密文包含明文，说明 Seal 用错")
	}
	return base64.StdEncoding.EncodeToString(sealed)
}
