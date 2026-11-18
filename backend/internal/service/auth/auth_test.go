package auth

import (
	"strings"
	"testing"
)

// 本文件只测**纯函数**（格式/强度/投影）。凡是需要 DB 或 Redis 的路径
// （注册并发、登录失败退避、验证码消费）都放在集成用例里，靠 LD_TEST_DSN 开启。

func TestValidateUsername(t *testing.T) {
	valid := []string{"abc", "user_1", "A1b2C3", "_x_", "zhangsan_2024"}
	for _, s := range valid {
		if err := ValidateUsername(s); err != nil {
			t.Errorf("合法用户名 %q 不应报错: %v", s, err)
		}
	}
	// 关键回归：纯数字必须被拒（改造前用 ^[a-zA-Z0-9_-]{3,20}$，`123456` 能注册成功，
	// 结果是一个和用户 id 视觉上无法区分的账号名）。
	invalid := []string{"", "ab", "123456", "a b", "用户", "abc-def", "aaaaaaaaaaaaaaaaaaaaa"}
	for _, s := range invalid {
		if err := ValidateUsername(s); err == nil {
			t.Errorf("非法用户名 %q 应报错", s)
		}
	}
}

func TestPasswordStrength(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"abcdefgh", 1},  // 仅小写
		{"abcdefg1", 2},  // 小写+数字
		{"Abcdefg1", 3},  // 大小写+数字
		{"Abcdefg1!", 4}, // 四类全中
		{"密码密码密码密码", 1},  // 非 ASCII 一律计为符号
		{"", 0},          //
	}
	for _, c := range cases {
		if got := PasswordStrength(c.in); got != c.want {
			t.Errorf("PasswordStrength(%q) = %d，期望 %d", c.in, got, c.want)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	valid := []string{"abc12345", "Abcdefg1", "P@ssw0rd!", "密码12345678"}
	for _, s := range valid {
		if err := ValidatePassword(s); err != nil {
			t.Errorf("合法密码 %q 不应报错: %v", s, err)
		}
	}
	invalid := []string{
		"",                       // 空
		"abc1234",                // 7 位
		"abcdefgh",               // 仅一类
		"12345678",               // 仅一类
		strings.Repeat("a1", 40), // 80 字节，超过 bcrypt 的 72 字节硬上限
	}
	for _, s := range invalid {
		if err := ValidatePassword(s); err == nil {
			t.Errorf("非法密码 %q 应报错", s)
		}
	}
	// 72 字节边界：bcrypt 会静默截断，必须在入口就拒，否则「密码后半段无效」
	// 在用户看来等于「密码偶尔不对」。
	if err := ValidatePassword(strings.Repeat("a1", 36)); err != nil {
		t.Errorf("恰好 72 字节应放行: %v", err)
	}
	if err := ValidatePassword(strings.Repeat("a1", 37)); err == nil {
		t.Error("73 字节必须拒绝（bcrypt 截断）")
	}
}
