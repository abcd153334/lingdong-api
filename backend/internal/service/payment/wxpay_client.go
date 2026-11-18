package payment

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"

	"github.com/wechatpay-apiv3/wechatpay-go/core"
	"github.com/wechatpay-apiv3/wechatpay-go/core/auth"
	"github.com/wechatpay-apiv3/wechatpay-go/core/auth/verifiers"
	"github.com/wechatpay-apiv3/wechatpay-go/core/option"
)

// 本文件负责「渠道参数 → 可用的 SDK 客户端 / 验签器」，是微信侧唯一构造入口。
//
// 两条硬约束：
//  1. 出站 HTTP 客户端**必须**由 netguard 提供（禁重定向 + 建连期 IP 校验）。
//     SDK 有 `WithWechatPayAutoAuthCipher` 与 `core/downloader` 会自动出网下载平台证书，
//     那条路径绕开 netguard，因此**禁止使用**（CI 有静态断言）。
//  2. 每个商户号一套 `core.Client`。它内部持有 signer/validator，构造有成本，
//     按指纹缓存；配置改了（指纹变了）就整条替换，不累积旧凭据。

// wxCreds 是一个渠道解析后的凭证集合。
type wxCreds struct {
	MchID  string
	AppID  string
	Serial string
	Priv   *rsa.PrivateKey
	// 公钥模式材料（新商户）
	PubKeyID  string
	PublicKey *rsa.PublicKey
	// 平台证书模式材料（老商户，支持多张覆盖轮换窗口）
	Certs []*x509.Certificate
	// APIv3Key 用于解密回调
	APIv3Key string
}

// usePublicKeyMode 判断出站签名走哪套。
//
// 规则是**单一确定**的：两套材料都配了就以公钥模式为准。
// 不做「按回调头动态选」——出站请求没有回调头，动态规则只会让行为不可预测。
func (c *wxCreds) usePublicKeyMode() bool {
	return c.PubKeyID != "" && c.PublicKey != nil
}

// parseWxpayChannel 解析并校验渠道参数。
func parseWxpayChannel(ch Channel) (*wxCreds, error) {
	p := ch.Params
	mchID := strings.TrimSpace(p.MchID)
	serial := strings.TrimSpace(p.CertSerialNo)
	apiV3 := strings.TrimSpace(p.APIv3Key)
	if mchID == "" {
		return nil, fmt.Errorf("%w: 缺少商户号", ErrNotConfigured)
	}
	if serial == "" {
		return nil, fmt.Errorf("%w: 缺少商户证书序列号", ErrNotConfigured)
	}
	if len(apiV3) != 32 {
		return nil, fmt.Errorf("%w: APIv3 密钥必须是 32 位", ErrNotConfigured)
	}
	priv, err := LoadRSAPrivateKey(p.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("%w: 商户私钥不可用: %v", ErrNotConfigured, err)
	}
	c := &wxCreds{
		MchID:    mchID,
		AppID:    strings.TrimSpace(p.AppID),
		Serial:   serial,
		Priv:     priv,
		APIv3Key: apiV3,
		PubKeyID: strings.TrimSpace(p.PublicKeyID),
	}
	if strings.TrimSpace(p.PublicKey) != "" {
		pub, perr := LoadRSAPublicKey(p.PublicKey)
		if perr != nil {
			return nil, fmt.Errorf("%w: 微信支付公钥不可用: %v", ErrNotConfigured, perr)
		}
		c.PublicKey = pub
	}
	if strings.TrimSpace(p.PlatformCert) != "" {
		certs, cerr := LoadCertBundle(p.PlatformCert)
		if cerr != nil {
			return nil, fmt.Errorf("%w: 平台证书不可用: %v", ErrNotConfigured, cerr)
		}
		c.Certs = certs
	}
	if !c.usePublicKeyMode() && len(c.Certs) == 0 {
		return nil, fmt.Errorf("%w: 缺少验签材料（微信支付公钥+公钥ID 或 平台证书）", ErrNotConfigured)
	}
	return c, nil
}

// fingerprint 是缓存失效判据：任何一项凭据变了都必须重建 client。
func (c *wxCreds) fingerprint() string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, s := range parts {
			h.Write([]byte(s))
			h.Write([]byte{0})
		}
	}
	write("mch", c.MchID, "app", c.AppID, "serial", c.Serial, "pbid", c.PubKeyID, "v3", c.APIv3Key)
	if c.Priv != nil {
		write("priv", fmt.Sprintf("%x", c.Priv.N.Bytes()), c.Priv.D.String())
	}
	if c.PublicKey != nil {
		write("pub", fmt.Sprintf("%x", c.PublicKey.N.Bytes()))
	}
	for _, cert := range c.Certs {
		write("cert", cert.SerialNumber.String())
	}
	return hex.EncodeToString(h.Sum(nil))
}

type wxEntry struct {
	fp     string
	client *core.Client
}

var wxPool = struct {
	sync.Mutex
	m map[string]*wxEntry
}{m: make(map[string]*wxEntry)}

// wxClient 返回该渠道的 SDK 客户端（按商户号缓存，按指纹失效）。
func wxClient(ch Channel) (*core.Client, *wxCreds, error) {
	c, err := parseWxpayChannel(ch)
	if err != nil {
		return nil, nil, err
	}
	fp := c.fingerprint()

	wxPool.Lock()
	if e, ok := wxPool.m[c.MchID]; ok && e.fp == fp {
		client := e.client
		wxPool.Unlock()
		return client, c, nil
	}
	wxPool.Unlock()

	client, err := buildWxClient(c)
	if err != nil {
		return nil, nil, err
	}

	wxPool.Lock()
	wxPool.m[c.MchID] = &wxEntry{fp: fp, client: client}
	wxPool.Unlock()
	return client, c, nil
}

// buildWxClient 构造 client。凭证模式决定 option 组合：
//
//	公钥模式   WithWechatPayPublicKeyAuthCipher 一条同时给出 Signer + Validator + Cipher
//	平台证书   WithMerchantCredential（Signer）+ WithWechatPayCertificate（Validator）
//
// 不能用 `WithWechatPayAuthCipher`：它已 deprecated，且语义是「证书列表 + 一键 cipher」，
// 与上面两条组合出来的行为不一致（升级时最容易踩的坑）。
func buildWxClient(c *wxCreds) (*core.Client, error) {
	opts := []core.ClientOption{
		// timeout=0：整体超时由每次调用的 context 控制，连接池才能复用。
		option.WithHTTPClient(outboundClient(0, outboundPolicy)),
	}
	if c.usePublicKeyMode() {
		opts = append(opts, option.WithWechatPayPublicKeyAuthCipher(
			c.MchID, c.Serial, c.Priv, c.PubKeyID, c.PublicKey))
	} else {
		opts = append(opts,
			option.WithMerchantCredential(c.MchID, c.Serial, c.Priv),
			option.WithWechatPayCertificate(c.Certs))
	}
	client, err := core.NewClient(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("%w: 微信支付客户端初始化失败: %v", ErrNotConfigured, err)
	}
	return client, nil
}

// resolveNotifyVerifier 按回调头 `Wechatpay-Serial` 选验签器。
//
// 为什么不「有公钥就用公钥」：回调头里的 serial 才是**上游实际用来签名的**那把。
//   - `PUB_KEY_ID_...` → 微信支付公钥模式（老商户不会出现这种头）；
//   - 十六进制序列号 → 平台证书，按 serial 精确匹配（bundle 里可能有新旧两张）。
//
// 公钥验签器内部还会校验 keyID == serial，配错会直接失败而不是「用错密钥验过」。
func resolveNotifyVerifier(c *wxCreds, serial string) (auth.Verifier, error) {
	serial = strings.TrimSpace(serial)
	if strings.HasPrefix(serial, "PUB_KEY_ID_") {
		if c.PubKeyID == "" || c.PublicKey == nil {
			return nil, fmt.Errorf("%w: 回调使用微信支付公钥签名，但渠道没有配置公钥", ErrNotConfigured)
		}
		return verifiers.NewSHA256WithRSAPubkeyVerifier(c.PubKeyID, *c.PublicKey), nil
	}
	if len(c.Certs) == 0 {
		return nil, fmt.Errorf("%w: 回调使用平台证书签名，但渠道没有配置平台证书", ErrNotConfigured)
	}
	return verifiers.NewSHA256WithRSAVerifier(core.NewCertificateMapWithList(c.Certs)), nil
}
