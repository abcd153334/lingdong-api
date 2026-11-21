package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

// Fernet 是 new-api 兼容的对称加密体例（AES-128-CBC + HMAC-SHA256）：
// token = base64url(0x80 ‖ ts(8, 大端) ‖ IV(16) ‖ 密文 ‖ HMAC-SHA256(32))，明文按 PKCS7 填充。
// 用途：channels.keys 密钥池（批量存储）；api_keys.key_cipher 走 AES-GCM，二者不混用（v2.0 §4.14.1）。
// 主密钥必须为 32 字节：前 16 = 签名密钥，后 16 = 加密密钥。

// fernetVersion 是 Fernet token 的版本字节。
const fernetVersion byte = 0x80

// DeriveKey 是全仓唯一的「任意长度口令 → 32 字节主密钥」派生。
//
// AES-GCM（api_keys.key_cipher、user_mfa.secret_enc）与 Fernet（渠道密钥池、
// 存储凭据、/v1/media 能力签名）共用它：两边各写一份的历史后果是
// 「同一个 APP_ENCRYPTION_KEY，一处认 64 字符串、另一处只认 16/24/32 字节」，
// 于是按文档生成的密钥能存渠道密钥、却建不出 API KEY（500）。
//
// 调用方一律传**口令原文**，不得再传本函数的返回值（会二次派生）。
func DeriveKey(secret []byte) []byte {
	sum := sha256.Sum256(secret)
	return sum[:]
}

// FernetDerive 从任意长度的口令派生 32 字节 Fernet 主密钥（避免依赖固定长度口令）。
func FernetDerive(secret string) []byte {
	return DeriveKey([]byte(secret))
}

// FernetEncrypt 使用 Fernet 加密明文，返回 urlsafe-base64 token。
func FernetEncrypt(masterKey, plaintext []byte) (string, error) {
	signKey, encKey, err := splitFernetKey(masterKey)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return "", fmt.Errorf("crypto: fernet 加密密钥非法: %w", err)
	}
	iv := make([]byte, aes.BlockSize)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return "", fmt.Errorf("crypto: fernet 生成 IV 失败: %w", err)
	}

	padded := pkcs7Pad(plaintext, aes.BlockSize)
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)

	out := make([]byte, 0, 1+8+aes.BlockSize+len(ct)+sha256.Size)
	out = append(out, fernetVersion)
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(time.Now().Unix()))
	out = append(out, ts[:]...)
	out = append(out, iv...)
	out = append(out, ct...)

	mac := hmac.New(sha256.New, signKey)
	mac.Write(out)
	out = mac.Sum(out)
	return base64.URLEncoding.EncodeToString(out), nil
}

// FernetDecrypt 校验签名并解密 Fernet token。
func FernetDecrypt(masterKey []byte, token string) ([]byte, error) {
	signKey, encKey, err := splitFernetKey(masterKey)
	if err != nil {
		return nil, err
	}
	raw, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("crypto: fernet token 解码失败: %w", err)
	}
	if len(raw) < 1+8+aes.BlockSize+sha256.Size {
		return nil, fmt.Errorf("crypto: fernet token 长度不足")
	}
	data, sig := raw[:len(raw)-sha256.Size], raw[len(raw)-sha256.Size:]
	mac := hmac.New(sha256.New, signKey)
	mac.Write(data)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, fmt.Errorf("crypto: fernet 签名校验失败")
	}
	if data[0] != fernetVersion {
		return nil, fmt.Errorf("crypto: fernet 版本不支持")
	}
	iv := data[9 : 9+aes.BlockSize]
	ct := data[9+aes.BlockSize:]
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("crypto: fernet 密文长度非法")
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, fmt.Errorf("crypto: fernet 加密密钥非法: %w", err)
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(pt, ct)
	return pkcs7Unpad(pt, aes.BlockSize)
}

func splitFernetKey(key []byte) (sign, enc []byte, err error) {
	if len(key) != 32 {
		return nil, nil, fmt.Errorf("crypto: fernet 主密钥必须为 32 字节（实际 %d）", len(key))
	}
	return key[:16], key[16:], nil
}

func pkcs7Pad(b []byte, size int) []byte {
	n := size - len(b)%size
	return append(append(make([]byte, 0, len(b)+n), b...), bytes.Repeat([]byte{byte(n)}, n)...)
}

func pkcs7Unpad(b []byte, size int) ([]byte, error) {
	if len(b) == 0 || len(b)%size != 0 {
		return nil, fmt.Errorf("crypto: 填充块长度非法")
	}
	n := int(b[len(b)-1])
	if n == 0 || n > size || n > len(b) {
		return nil, fmt.Errorf("crypto: 填充字节非法")
	}
	for _, v := range b[len(b)-n:] {
		if int(v) != n {
			return nil, fmt.Errorf("crypto: 填充校验失败")
		}
	}
	return b[:len(b)-n], nil
}
