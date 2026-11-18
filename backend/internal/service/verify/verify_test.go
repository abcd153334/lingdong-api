package verify

import "testing"

// 本文件是收件人格式与归一化的**唯一**用例集：
// 这两个函数被注册、换绑、找回密码三处共用，规则漂移的表现是
// 「注册能过、找回找不到人」，而两边都不报错。

func TestValidateFormatEmail(t *testing.T) {
	ok := []string{"a@b.com", "x.y@z.co", "user_1@example.cn", "a+b@sub.example.com"}
	for _, s := range ok {
		if err := ValidateFormat(ChannelEmail, s); err != nil {
			t.Errorf("合法邮箱 %q 不应报错: %v", s, err)
		}
	}
	// `a@.com` / `a@b.` / `a@b..com` 是结构坏地址：只判「域名含一个点」会放过它们，
	// 代价是用户注册成功却永远收不到验证码。
	bad := []string{
		"", "abc", "a@b", "a@.com", "a@b.", "a@b..com", "@b.com", "a b@c.com",
		"a@@b.com", "a@b@c.com", "a@b .com", "a@b\t.com",
	}
	for _, s := range bad {
		if err := ValidateFormat(ChannelEmail, s); err == nil {
			t.Errorf("非法邮箱 %q 应报错", s)
		}
	}
}

func TestValidateFormatSMS(t *testing.T) {
	ok := []string{"13800138000", "+8613800138000", "85212345678", "123456"}
	for _, s := range ok {
		if err := ValidateFormat(ChannelSMS, s); err != nil {
			t.Errorf("合法号码 %q 不应报错: %v", s, err)
		}
	}
	// 刻意不按国内号段硬校验（海外号要能用），但字符集与长度必须守。
	bad := []string{"", "12345", "1380013800012345678", "138-0013-8000", "abcdefgh", "+"}
	for _, s := range bad {
		if err := ValidateFormat(ChannelSMS, s); err == nil {
			t.Errorf("非法号码 %q 应报错", s)
		}
	}
}

func TestValidateFormatUnknownChannel(t *testing.T) {
	if err := ValidateFormat("carrier-pigeon", "x@y.com"); err == nil {
		t.Fatal("未知渠道必须报错（否则等于绕过全部格式校验）")
	}
}

// TestNormalize 断言归一化口径。
//
// 归一化是「一个账号只有一份联系方式」的前提：`A@b.com` 与 `a@b.com`
// 若落成两行，唯一索引挡不住，找回密码就会「有时找得到、有时找不到」。
func TestNormalize(t *testing.T) {
	if got := Normalize(ChannelEmail, "  Foo.Bar@Example.COM "); got != "foo.bar@example.com" {
		t.Errorf("邮箱归一化结果 %q", got)
	}
	// `+` 前缀也必须被去掉：E.164 的显示形态不构成号码身份的一部分。
	//
	// 不去掉的后果一个都不报错：`+86…` 与 `86…` 是两个唯一值（可重复注册）、
	// 是两把冷却键（60 秒冷却被绕过）、还会让"注册写 +、找回写裸号"的用户
	// 永远查不到自己的账号（而找回端点恒 200，用户看不到任何提示）。
	for _, in := range []string{
		"13800138000", " 138 0013 8000 ", "138-0013-8000", "(138)0013-8000",
		"+8613800138000", "+86 138 0013 8000",
	} {
		if got := Normalize(ChannelSMS, in); got != "8613800138000" && got != "13800138000" {
			t.Errorf("号码 %q 归一化结果 %q", in, got)
		}
	}
	// 同一号码的两种写法必须归一成**同一个**值（唯一索引才拦得住重复注册）。
	if a, b := Normalize(ChannelSMS, "+8613800138000"), Normalize(ChannelSMS, "8613800138000"); a != b {
		t.Fatalf("带 + 与不带 + 必须归一到同一值：%q vs %q", a, b)
	}
	// 双加号必须一起去掉：去一个会留下一个 `+`，归一化就不幂等，
	// 存进库的值与再算一次的值不同 → 查询永远查不到（且不报错）。
	if got := Normalize(ChannelSMS, "++8613800138000"); got != "8613800138000" {
		t.Fatalf("双加号归一化结果 %q", got)
	}
}

// TestNormalizeIdempotent 断言归一化幂等（二次调用不得再变化）。
//
// 不幂等的后果是「存进去的值」与「查出来的值」不同：注册用一次、登录再归一化一次，
// 两次结果不一致时账号直接登不上，而两次调用看起来都没错。
func TestNormalizeIdempotent(t *testing.T) {
	for _, in := range []string{" Foo@Bar.COM ", "138-0013 8000", "+86 138 0013 8000", "++8613800138000"} {
		for _, ch := range []string{ChannelEmail, ChannelSMS} {
			once := Normalize(ch, in)
			twice := Normalize(ch, once)
			if once != twice {
				t.Errorf("渠道 %s 输入 %q 归一化不幂等：%q → %q", ch, in, once, twice)
			}
		}
	}
}
