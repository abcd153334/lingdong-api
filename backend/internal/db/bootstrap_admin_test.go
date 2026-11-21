package db

import (
	"strings"
	"testing"
)

// 出厂口令是本轮部署简化里唯一"功能正确性 + 安全语义"同时挂在一条语句上的地方：
// 改错一个字，要么进不去（口令不是 admin123），要么永久弱口令（不再强制改密）。
func TestBootstrapAdminPassword(t *testing.T) {
	t.Run("未设置时用出厂口令并要求改密", func(t *testing.T) {
		t.Setenv(bootstrapAdminEnv, "")
		pw, mustChange := bootstrapAdminPassword()
		if pw != defaultBootstrapPassword {
			t.Fatalf("口令 = %q，期望出厂值 %q", pw, defaultBootstrapPassword)
		}
		if !mustChange {
			t.Fatal("出厂口令必须强制改密（它是公开值，只够进门）")
		}
	})

	t.Run("只有空白视为未设置", func(t *testing.T) {
		t.Setenv(bootstrapAdminEnv, "   ")
		if pw, mustChange := bootstrapAdminPassword(); pw != defaultBootstrapPassword || !mustChange {
			t.Fatalf("空白值被当成显式口令了：pw=%q mustChange=%v", pw, mustChange)
		}
	})

	t.Run("显式指定则照用且不强制改密", func(t *testing.T) {
		t.Setenv(bootstrapAdminEnv, "s3cret-from-env")
		pw, mustChange := bootstrapAdminPassword()
		if pw != "s3cret-from-env" {
			t.Fatalf("口令 = %q，期望照用环境变量", pw)
		}
		if mustChange {
			t.Fatal("运维显式指定口令时不应强制改密（会打断 CI/自动化建环境）")
		}
	})

	t.Run("出厂口令本身要能被口令策略接受", func(t *testing.T) {
		// 强制改密只拦"进门之后"，但 seed 直接写 bcrypt、不过策略校验；
		// 若哪天策略收紧到 8 位以上，admin123 仍是 8 位，这里守住不回归。
		if len(defaultBootstrapPassword) < 8 {
			t.Fatalf("出厂口令 %q 短于 8 位", defaultBootstrapPassword)
		}
		if strings.ContainsAny(defaultBootstrapPassword, " \t") {
			t.Fatalf("出厂口令不应含空白：%q", defaultBootstrapPassword)
		}
	})
}
