package payment

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
)

/* -------------------- PEM 归一化与解析 -------------------- */

func TestNormalizePEMSingleLinePaste(t *testing.T) {
	// 运营从网页复制时常拿到「整段一行、换行是字面 \n」的形态。
	oneLine := `-----BEGIN PRIVATE KEY-----\nMIIB\n-----END PRIVATE KEY-----`
	got := NormalizePEM(oneLine)
	if strings.Contains(got, `\n`) {
		t.Fatalf("字面 \\n 必须换成真实换行: %q", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Fatal("应补齐尾换行")
	}
	if strings.Contains(NormalizePEM("a\r\nb"), "\r") {
		t.Fatal("CRLF 必须归一为 LF")
	}
	if NormalizePEM("   ") != "" {
		t.Fatal("空白应归一为空串")
	}
}

func TestLoadRSAKeysAcceptBothEncodings(t *testing.T) {
	key := wxTestKey(t)
	priv := pemPrivate(t, key) // PKCS#8
	if _, err := LoadRSAPrivateKey(priv); err != nil {
		t.Fatalf("PKCS#8 私钥应可解析: %v", err)
	}
	// 同样内容用 PKCS#1 头再走一遍。
	pkcs1 := string(mustPEMBlock(t, "RSA PRIVATE KEY", mustPKCS1(t, key)))
	if _, err := LoadRSAPrivateKey(pkcs1); err != nil {
		t.Fatalf("PKCS#1 私钥应可解析: %v", err)
	}
	pub := pemPublic(t, key)
	if _, err := LoadRSAPublicKey(pub); err != nil {
		t.Fatalf("PKIX 公钥应可解析: %v", err)
	}
	if _, err := LoadRSAPrivateKey("not a pem"); err == nil {
		t.Fatal("非 PEM 应报错")
	}
	if _, err := LoadRSAPrivateKey(pub); err == nil {
		t.Fatal("把公钥当私钥必须报错")
	}
}

/* -------------------- 渠道参数自洽性 -------------------- */

func TestValidateChannelShapeOnlyChecksNonEmpty(t *testing.T) {
	// 存量渠道（新字段全为空串）必须无条件通过。
	ch := Channel{ID: "epay", Name: "易支付", Protocol: string(ProtocolEpay), Enabled: true,
		Params: ChannelParams{PayAddress: "https://p.x.cn", PID: "1", Key: "k"}}
	if err := ValidateChannelShape(ch); err != nil {
		t.Fatalf("存量渠道不应被判不合规: %v", err)
	}

	// APIv3 密钥长度错一位就该报（它是解密回调的密钥，差一位全解不开）。
	bad := ch
	bad.Protocol = string(ProtocolWxpay)
	bad.Params = ChannelParams{MchID: "1", APIv3Key: "short"}
	if err := ValidateChannelShape(bad); err == nil || !strings.Contains(err.Error(), "32 位") {
		t.Fatalf("APIv3 密钥长度必须校验，实际: %v", err)
	}

	// 把私钥粘到公钥框：必须在保存期就拦住（而不是等订单失败）。
	key := wxTestKey(t)
	swapped := ch
	swapped.Protocol = string(ProtocolAlipay)
	swapped.Params = ChannelParams{AppID: "1", PublicKey: pemPrivate(t, key)}
	if err := ValidateChannelShape(swapped); err == nil || !strings.Contains(err.Error(), "公钥") {
		t.Fatalf("公钥框里塞私钥必须报错，实际: %v", err)
	}

	// 序列号格式错（把整段私钥粘进来）。
	serial := ch
	serial.Protocol = string(ProtocolWxpay)
	serial.Params = ChannelParams{MchID: "1", CertSerialNo: "-----BEGIN PRIVATE KEY-----"}
	if err := ValidateChannelShape(serial); err == nil {
		t.Fatal("序列号格式错必须报错")
	}
}

func TestValidateChannelRequiredUsesSchema(t *testing.T) {
	// 微信：缺 AppID 必须被点名。
	ch := Channel{ID: "wx", Name: "微信直连", Protocol: string(ProtocolWxpay), Enabled: true,
		Params: ChannelParams{
			MchID: "1600000001", CertSerialNo: "5157F09EFDC096DE15EBE81A47057A7232F1B8E1",
			APIv3Key: "0123456789abcdef0123456789abcdef", PrivateKey: "x",
			PublicKey: "y", PublicKeyID: "PUB_KEY_ID_1",
		}}
	err := ValidateChannelRequired(ch)
	if err == nil || !strings.Contains(err.Error(), "AppID") {
		t.Fatalf("缺 AppID 应被点名，实际: %v", err)
	}

	// 验签材料二选一：公钥与公钥 ID 只填一个必须拒。
	half := ch
	half.Params.AppID = "wx1"
	half.Params.PublicKey = ""
	if err := ValidateChannelRequired(half); err == nil || !strings.Contains(err.Error(), "成对") {
		t.Fatalf("公钥与公钥 ID 必须成对，实际: %v", err)
	}

	// 换成平台证书则通过（老商户形态）。
	cert := ch
	cert.Params.AppID = "wx1"
	cert.Params.PublicKey = ""
	cert.Params.PublicKeyID = ""
	cert.Params.PlatformCert = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----"
	if err := ValidateChannelRequired(cert); err != nil {
		t.Fatalf("平台证书形态应通过: %v", err)
	}
}

// TestValidateWxpayVerifyMaterialTruthTable 锁死微信验签材料的六种输入组合。
//
// 关键语义：**允许两组同时存在**。出站 usePublicKeyMode() 在公钥完整时走公钥，
// 入站按回调头 Wechatpay-Serial 前缀自适应；两组都配是换证窗口的正确配置，
// 清空任一都会让某一类回调 500。
func TestValidateWxpayVerifyMaterialTruthTable(t *testing.T) {
	const certPEM = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----"
	cases := []struct {
		name string
		pub  string
		id   string
		cert string
		want string // 期望错误里包含的关键字；空串表示应通过
	}{
		{"全空", "", "", "", "缺少验签材料"},
		{"只有证书", "", "", certPEM, ""},
		{"只有公钥", "pub", "", "", "成对"},
		{"只有公钥ID", "", "PUB_KEY_ID_1", "", "成对"},
		{"公钥成对", "pub", "PUB_KEY_ID_1", "", ""},
		{"公钥成对且带证书", "pub", "PUB_KEY_ID_1", certPEM, ""},
		{"公钥有ID空但带证书仍算不成对", "pub", "", certPEM, "成对"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateWxpayVerifyMaterial("微信直连", ChannelParams{
				PublicKey: c.pub, PublicKeyID: c.id, PlatformCert: c.cert,
			})
			if c.want == "" {
				if err != nil {
					t.Fatalf("应通过，实际报错: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("应报「%s」，实际: %v", c.want, err)
			}
		})
	}
}

func TestTruncateUTF8Bytes(t *testing.T) {
	// 微信退款 reason 上限 80 **字节**：80 个汉字是 240 字节，必须按字节截断且不切碎 rune。
	s := strings.Repeat("退", 40) // 每个汉字 3 字节，共 120 字节
	got := TruncateUTF8Bytes(s, 80)
	// 80 不是 3 的倍数，必须退到最近的 rune 边界（78），不能切出半个字符。
	if len(got) > 80 || len(got) < 78 {
		t.Fatalf("应截到不超过 80 字节且贴合 rune 边界，实际 %d", len(got))
	}
	if !strings.HasPrefix(s, got) {
		t.Fatal("截断结果必须是原串前缀（不能出现半个字符）")
	}
	for _, r := range got {
		if r == '\uFFFD' {
			t.Fatal("不能切出非法 UTF-8")
		}
	}
	if TruncateUTF8Bytes("abc", 10) != "abc" {
		t.Fatal("未超限应原样返回")
	}
}

/* -------------------- 测试用小工具 -------------------- */

func mustPKCS1(t *testing.T, k *rsa.PrivateKey) []byte {
	t.Helper()
	return x509.MarshalPKCS1PrivateKey(k)
}

func mustPEMBlock(t *testing.T, typ string, der []byte) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

// TestValidateChannelShapeRejectsWrongSettleCurrency 结算币种的两种错法都必须被拒。
//
// ① 单币种协议（易支付等）填了别的币种：它们的报文里没有币种参数，金额恒按 CNY
// 解释。若放行，这条渠道会在美元站的收银台出现 —— 用户付人民币、系统按美元记额度。
// 字段已从这些协议的 schema 移除，但 ChannelParams 全量回写，直接调 API 仍能写进去。
//
// ② 多币种协议（Stripe）填了不支持的币种（"RMB"/"USDT"）：写错一个字母不会报错，
// 只是永远匹配不上 —— 运营看到的现象是「这个支付方式莫名其妙没了」。
func TestValidateChannelShapeRejectsWrongSettleCurrency(t *testing.T) {
	epay := Channel{ID: "epay", Name: "易支付", Protocol: string(ProtocolEpay), Enabled: true,
		Params: ChannelParams{PayAddress: "https://p.x.cn", PID: "1", Key: "k"}}
	for _, cur := range []string{"USD", "usd", " USD "} {
		epay.Params.SettleCurrency = cur
		err := ValidateChannelShape(epay)
		if err == nil || !strings.Contains(err.Error(), "固定按 CNY 结算") {
			t.Fatalf("易支付填 %q 应被拒，实际: %v", cur, err)
		}
	}
	// 空值（改字段之前存下的数据）与显式 CNY 都要通过。
	for _, cur := range []string{"", "CNY", "cny"} {
		epay.Params.SettleCurrency = cur
		if err := ValidateChannelShape(epay); err != nil {
			t.Fatalf("易支付结算币种 %q 应合法，实际: %v", cur, err)
		}
	}

	stripe := Channel{ID: "stripe", Name: "Stripe", Protocol: string(ProtocolStripe), Enabled: true,
		Params: ChannelParams{SecretKey: "sk", WebhookSecret: "whsec"}}
	stripe.Params.SettleCurrency = "RMB"
	if err := ValidateChannelShape(stripe); err == nil || !strings.Contains(err.Error(), "结算币种") {
		t.Fatalf("Stripe 填 RMB 应被拒，实际: %v", err)
	}
	for _, cur := range []string{"USD", "cny", " CNY "} {
		stripe.Params.SettleCurrency = cur
		if err := ValidateChannelShape(stripe); err != nil {
			t.Fatalf("Stripe 结算币种 %q 应合法，实际: %v", cur, err)
		}
	}
}
