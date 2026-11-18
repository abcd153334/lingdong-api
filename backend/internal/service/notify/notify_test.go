package notify

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/pkg/errs"
)

func snapshotSystem(t *testing.T) {
	t.Helper()
	prev := conf.System().All()
	t.Cleanup(func() { conf.System().Reload(prev) })
}

// TestChannelDisabled 未配置（或未启用）时统一返回「未配置即禁用」，不发任何网络请求。
func TestChannelDisabled(t *testing.T) {
	snapshotSystem(t)
	conf.System().Reload(map[string]json.RawMessage{})
	cases := []struct {
		name string
		call func() error
	}{
		{"验证码-邮箱", func() error { return SendCode(context.Background(), SceneEmailBind, "a@b.cn", "123456") }},
		{"验证码-手机", func() error { return SendCode(context.Background(), ScenePhoneBind, "13800000000", "123456") }},
		{"测试邮件", func() error { return SendTestEmail(context.Background(), "a@b.cn") }},
		{"测试短信", func() error { return SendTestSMS(context.Background(), "13800000000") }},
		{"未知场景", func() error { return SendCode(context.Background(), "weird", "a@b.cn", "1") }},
	}
	for _, c := range cases {
		if err := c.call(); !errors.Is(err, errs.NotifyChannelDisabled) {
			t.Errorf("%s 应返回 NotifyChannelDisabled，实际: %v", c.name, err)
		}
	}
}

// TestEmailSceneTemplateRequired 邮件模板缺失时视为通道不可用（不外发）。
func TestEmailSceneTemplateRequired(t *testing.T) {
	snapshotSystem(t)
	conf.System().Reload(map[string]json.RawMessage{
		"email": json.RawMessage(`{"enabled":true,"smtp_host":"smtp.x.cn","smtp_port":465,"username":"u","password":"p","from_addr":"a@b.cn","templates":{}}`),
	})
	if err := SendCode(context.Background(), ScenePasswordReset, "a@b.cn", "123456"); !errors.Is(err, errs.NotifyChannelDisabled) {
		t.Fatalf("模板缺失应视为不可用，实际: %v", err)
	}
}

// TestSMSReadyRequiresChannelParams 短信就绪判定须覆盖选中渠道的全部必填参数。
func TestSMSReadyRequiresChannelParams(t *testing.T) {
	snapshotSystem(t)
	conf.System().Reload(map[string]json.RawMessage{
		"sms": json.RawMessage(`{"enabled":true,"active":"aliyun","channels":{"aliyun":{"access_key_id":"id"}}}`),
	})
	if SMSReady() {
		t.Fatalf("参数不全时不应就绪")
	}
	conf.System().Set("sms", json.RawMessage(`{"enabled":true,"active":"aliyun","daily_limit":0,"channels":{"aliyun":{"access_key_id":"id","access_key_secret":"sk","sign_name":"S","template_code":"T"}}}`))
	if !SMSReady() {
		t.Fatalf("参数齐备时应就绪")
	}
}

// TestRender 模板变量替换。
func TestRender(t *testing.T) {
	got := render("【{{site_name}}】验证码 {{code}}", map[string]string{"site_name": "灵动 API", "code": "8888"})
	if got != "【灵动 API】验证码 8888" {
		t.Fatalf("渲染不符: %s", got)
	}
}

// TestAliyunSign 阿里云 RPC 签名：确定性 + 参数名升序 + 密钥敏感。
func TestAliyunSign(t *testing.T) {
	params := map[string]string{
		"Timestamp": "2026-10-20T00:00:00Z", "Action": "SendSms",
		"PhoneNumbers": "13800000000", "SignatureNonce": "abc123", "AccessKeyId": "key-id",
	}
	canonical, sig := aliyunSign(params, "secret")
	canonical2, sig2 := aliyunSign(params, "secret")
	if canonical != canonical2 || sig != sig2 {
		t.Fatalf("签名应确定: %s/%s vs %s/%s", canonical, sig, canonical2, sig2)
	}
	if !strings.HasPrefix(canonical, "AccessKeyId=key-id&") {
		t.Fatalf("规范化查询串须按参数名升序: %s", canonical)
	}
	if len(sig) != 28 {
		t.Fatalf("HMAC-SHA1 的 base64 签名应为 28 字符，实际 %d", len(sig))
	}
	if _, other := aliyunSign(params, "secret2"); other == sig {
		t.Fatalf("不同密钥应得不同签名")
	}
}

// TestTencentAuth TC3 授权头结构（Credential/scope/SignedHeaders 齐备且随密钥变化）。
func TestTencentAuth(t *testing.T) {
	auth := tencentAuth("sid", "skey", "sms.tencentcloudapi.com", "2026-10-20", "1792425600", `{"a":1}`)
	for _, want := range []string{"TC3-HMAC-SHA256", "Credential=sid/2026-10-20/sms/tc3_request", "SignedHeaders=content-type;host", "Signature="} {
		if !strings.Contains(auth, want) {
			t.Fatalf("授权头缺少 %q: %s", want, auth)
		}
	}
	if other := tencentAuth("sid", "skey2", "sms.tencentcloudapi.com", "2026-10-20", "1792425600", `{"a":1}`); other == auth {
		t.Fatalf("不同密钥应得不同签名")
	}
}

// TestNormalizeCNPhone 腾讯云要求 E.164。
func TestNormalizeCNPhone(t *testing.T) {
	if got := normalizeCNPhone("13800000000"); got != "+8613800000000" {
		t.Fatalf("归一不符: %s", got)
	}
	if got := normalizeCNPhone("+8613800000000"); got != "+8613800000000" {
		t.Fatalf("已带前缀应保持: %s", got)
	}
}
