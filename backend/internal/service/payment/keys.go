package payment

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

// 本文件是「运营粘贴的 PEM → 可用的 Go 密钥对象」的唯一转换点。
//
// 为什么集中且必须容错：
//   - 运营从商户平台复制私钥时，常见两种坏形态：整段变成单行（换行成了字面 `\n`）、
//     带 CRLF；直接 pem.Decode 都会失败，而失败信息只有「不是合法 PEM」，无法自诊；
//   - 微信商户私钥是 PKCS#8（`BEGIN PRIVATE KEY`），支付宝工具也可能给 PKCS#1
//     （`BEGIN RSA PRIVATE KEY`），两种都要支持；
//   - 平台证书轮换期会一次给多张，必须能解析 bundle。

// 错误集：调用方靠它决定「配置错」（对客 400 / 保存期拒绝）而不是「网关错」。
var (
	// ErrBadPEM 表示 PEM 语法不可解析（缺头尾、被截断、粘贴了半段）。
	ErrBadPEM = errors.New("payment: PEM 格式不合法")
	// ErrBadKey 表示 PEM 能解析但内容不是期望的密钥类型。
	ErrBadKey = errors.New("payment: 密钥类型不正确")
)

// NormalizePEM 归一化运营粘贴的 PEM 文本。
//
// 处理三件事：单行粘贴（字面 `\n`）→ 真实换行；CRLF/CR → LF；补齐尾换行。
// `pem.Decode` 对结尾换行并不敏感，但补齐能让「多段 PEM 拼接」与肉眼核对都更可靠。
func NormalizePEM(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "\n") && strings.Contains(s, `\n`) {
		s = strings.ReplaceAll(s, `\n`, "\n")
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return s
}

// LoadRSAPrivateKey 解析 RSA 私钥（PKCS#1 或 PKCS#8）。
func LoadRSAPrivateKey(raw string) (*rsa.PrivateKey, error) {
	s := NormalizePEM(raw)
	if s == "" {
		return nil, fmt.Errorf("%w: 内容为空", ErrBadPEM)
	}
	block, _ := pem.Decode([]byte(s))
	if block == nil {
		return nil, fmt.Errorf("%w: 缺少 -----BEGIN ...----- 头或内容被截断", ErrBadPEM)
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: 既不是 PKCS#1 也不是 PKCS#8 私钥", ErrBadPEM)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%w: 只支持 RSA 私钥", ErrBadKey)
	}
	return key, nil
}

// LoadRSAPublicKey 解析 RSA 公钥（PKIX `PUBLIC KEY` / PKCS#1 `RSA PUBLIC KEY`）。
//
// 也接受单张 `CERTIFICATE`：微信平台证书与部分商户习惯直接粘证书，
// 这时取证书里的公钥比报错更符合预期。
func LoadRSAPublicKey(raw string) (*rsa.PublicKey, error) {
	s := NormalizePEM(raw)
	if s == "" {
		return nil, fmt.Errorf("%w: 内容为空", ErrBadPEM)
	}
	block, _ := pem.Decode([]byte(s))
	if block == nil {
		return nil, fmt.Errorf("%w: 缺少 -----BEGIN ...----- 头或内容被截断", ErrBadPEM)
	}
	if pub, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		key, ok := pub.(*rsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("%w: 只支持 RSA 公钥", ErrBadKey)
		}
		return key, nil
	}
	if key, err := x509.ParsePKCS1PublicKey(block.Bytes); err == nil {
		return key, nil
	}
	if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
		key, ok := cert.PublicKey.(*rsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("%w: 证书里的公钥不是 RSA", ErrBadKey)
		}
		return key, nil
	}
	return nil, fmt.Errorf("%w: 既不是 PKIX/PKCS#1 公钥，也不是证书", ErrBadPEM)
}

// LoadCertBundle 解析多段 PEM 证书（平台证书轮换期会同时给新旧两张）。
func LoadCertBundle(raw string) ([]*x509.Certificate, error) {
	s := NormalizePEM(raw)
	if s == "" {
		return nil, fmt.Errorf("%w: 内容为空", ErrBadPEM)
	}
	var out []*x509.Certificate
	rest := []byte(s)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			// 允许用户把私钥/公钥一起粘进来：忽略非证书段，只要有证书就能用。
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: 证书段解析失败", ErrBadPEM)
		}
		out = append(out, cert)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: 没有找到 CERTIFICATE 段", ErrBadPEM)
	}
	return out, nil
}
