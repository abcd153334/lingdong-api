package totp

import (
	"strings"
	"testing"
	"time"
)

// RFC 6238 附录 B 的种子（ASCII "12345678901234567890" 的 base32 形式）。
const rfcSecret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"

// RFC 6238 的官方测试向量（原文是 8 位，本项目固定 6 位 → 取后 6 位）。
//
// 钉住它们而不是「自己算一遍再比」：自算等于把实现的错误抄进断言里，
// 而这几组是标准文件给的，能同时验出截断偏移、字节序与模数三处错。
func TestRFC6238Vectors(t *testing.T) {
	cases := []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	}
	for _, c := range cases {
		got, err := Code(rfcSecret, time.Unix(c.unix, 0))
		if err != nil {
			t.Fatalf("t=%d 计算出错: %v", c.unix, err)
		}
		if got != c.want {
			t.Fatalf("t=%d 期望 %s，实际 %s", c.unix, c.want, got)
		}
	}
}

func TestValidateSkewWindow(t *testing.T) {
	now := time.Unix(1234567890, 0)
	cur := StepOf(now)

	prev, _ := CodeAt(rfcSecret, cur-1)
	next, _ := CodeAt(rfcSecret, cur+1)
	far, _ := CodeAt(rfcSecret, cur+2)

	if _, ok := Validate(rfcSecret, prev, now); !ok {
		t.Fatal("上一步（-30s）应可用：手机与服务器时钟常有几十秒偏差")
	}
	if step, ok := Validate(rfcSecret, next, now); !ok || step != cur+1 {
		t.Fatalf("下一步（+30s）应可用且返回 step=%d，实际 %d/%v", cur+1, step, ok)
	}
	if _, ok := Validate(rfcSecret, far, now); ok {
		t.Fatal("+2 步必须拒绝：再放宽等于把爆破窗口翻倍")
	}
	if _, ok := Validate(rfcSecret, "000000", now); ok {
		t.Fatal("错误口令必须拒绝")
	}
}

func TestValidateRejectsMalformed(t *testing.T) {
	now := time.Unix(1234567890, 0)
	good, _ := Code(rfcSecret, now)
	for _, bad := range []string{"", "12345", "1234567", "12 34", "abcdef", "12a456", "12345 6 7"} {
		if _, ok := Validate(rfcSecret, bad, now); ok {
			t.Fatalf("畸形口令 %q 不该通过", bad)
		}
	}
	// 带空格的正常码要能通过：App 常常显示成「005 924」。
	spaced := good[:3] + " " + good[3:]
	if _, ok := Validate(rfcSecret, spaced, now); !ok {
		t.Fatalf("带空格的正确口令 %q 应通过（用户粘贴时会连空格一起带上）", spaced)
	}
}

func TestNormalizeSecret(t *testing.T) {
	// 用户从后台/Authenticator 复制时常见三种形态：小写、带空格分组、带 padding。
	want := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	for _, in := range []string{
		"gezdgnbvgy3tqojqgezdgnbvgy3tqojq",
		"GEZD GNBV GY3T QOJQ GEZD GNBV GY3T QOJQ",
		"GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ====",
		"  gezd-gnbv-gy3t-qojq-gezd-gnbv-gy3t-qojq  ",
	} {
		if got := Normalize(in); got != want {
			t.Fatalf("Normalize(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestNewSecretIsUsable(t *testing.T) {
	a, err := NewSecret()
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	b, _ := NewSecret()
	if a == b {
		t.Fatal("两次生成的密钥不能相同")
	}
	if strings.Contains(a, "=") {
		t.Fatalf("密钥不应带 padding（二维码里多一个 = 就会被某些 App 截断）: %q", a)
	}
	now := time.Now()
	code, err := Code(a, now)
	if err != nil {
		t.Fatalf("用自己的密钥算码失败: %v", err)
	}
	if _, ok := Validate(a, code, now); !ok {
		t.Fatal("自己生成的密钥必须能验证自己算出的码")
	}
}

func TestURIContainsBothIssuers(t *testing.T) {
	uri := URI("灵动 API", "alice", rfcSecret)
	for _, want := range []string{"otpauth://totp/", "secret=" + rfcSecret, "issuer=", "digits=6", "period=30"} {
		if !strings.Contains(uri, want) {
			t.Fatalf("绑定 URI 缺少 %q: %s", want, uri)
		}
	}
	// issuer 必须出现两次：label 前缀（新版 App 读）与 query（老版读）。
	if strings.Count(uri, "灵动") != 2 {
		t.Fatalf("issuer 必须在 label 与 query 各出现一次: %s", uri)
	}
}
