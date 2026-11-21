package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
)

// Encrypt 使用 AES-GCM 加密明文，返回 base64(nonce||ciphertext)。
//
// key 是**口令原文**（任意长度），由 newGCM 内的 DeriveKey 派生 32 字节主密钥。
// 仅作工具函数（供 api_keys.key_cipher、user_mfa.secret_enc 使用，见 v2.0 §4.14.1）。
func Encrypt(key, plaintext []byte) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("crypto: 生成 nonce 失败: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt 解密 Encrypt 产生的密文。
//
// 兼容 v2.1 之前的密文：那时 key 被**直接**当 AES 密钥用，只有 16/24/32 字节的
// 口令能工作。派生密钥解不开时回退原始口令再试一次 —— 否则已绑 2FA 的用户
// 会在升级后全部登录失败（mfa.decryptSecret 把解密错误归为 AuthUnavailable），
// 而这些人手里没有任何自助恢复手段。
//
// 回退只影响「读」，Encrypt 永远写派生格式；密钥轮换或重绑时旧密文自然淘汰。
// 不构成额外风险：两条路径用的是同一个 env 口令，攻击者拿到口令本来就能解开。
func Decrypt(key []byte, ciphertext string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return nil, fmt.Errorf("crypto: base64 解码失败: %w", err)
	}
	plain, derivedErr := openWith(newGCM, key, raw)
	if derivedErr == nil {
		return plain, nil
	}
	if plain, legacyErr := openWith(legacyGCM, key, raw); legacyErr == nil {
		return plain, nil
	}
	return nil, derivedErr
}

// openWith 用给定的 AEAD 构造器解一次；构造失败与解密失败都返回错误。
func openWith(build func([]byte) (cipher.AEAD, error), key, raw []byte) ([]byte, error) {
	gcm, err := build(key)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(raw) < ns {
		return nil, fmt.Errorf("crypto: 密文长度不足")
	}
	return gcm.Open(nil, raw[:ns], raw[ns:], nil)
}

// newGCM 是当前的唯一构造路径：任意长度口令 → SHA-256 → 32 字节 → AES-256-GCM。
func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) == 0 {
		return nil, fmt.Errorf("crypto: 密钥为空")
	}
	block, err := aes.NewCipher(DeriveKey(key))
	if err != nil {
		return nil, fmt.Errorf("crypto: 派生密钥非法: %w", err)
	}
	return cipher.NewGCM(block)
}

// legacyGCM 只用于解密旧密文：把口令原文直接当 AES 密钥（16/24/32 字节）。
// 新代码不得用它加密 —— 写进去的密文在密钥治理上属于"未派生"，无法轮换。
func legacyGCM(key []byte) (cipher.AEAD, error) {
	switch len(key) {
	case 16, 24, 32:
	default:
		return nil, fmt.Errorf("crypto: 无旧格式密文可解（口令长度 %d 不是 16/24/32）", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
