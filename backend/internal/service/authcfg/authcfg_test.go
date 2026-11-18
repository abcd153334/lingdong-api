package authcfg

import (
	"slices"
	"testing"
	"time"
)

// 本文件只测纯函数与「配置缺失时的兜底」。
//
// 兜底值本身不参与业务判断（正常启动 seed 一定会写入），但它们是
// 「配置被误删 / 新环境未 seed」时唯一挡住 panic 与除零的东西：
// 这些用例保证兜底至少是自洽的（空闲 < 绝对、退避阶梯非空且单调）。

func TestSessionFallbackIsCoherent(t *testing.T) {
	sp := Session()
	if sp.AbsoluteTTL <= 0 || sp.IdleTTL <= 0 || sp.TouchInterval <= 0 {
		t.Fatalf("会话兜底值不得为零：%+v", sp)
	}
	// 空闲超时必须严格小于绝对有效期，否则「闲置自动退出」永远不触发。
	if sp.IdleTTL >= sp.AbsoluteTTL {
		t.Fatalf("空闲超时(%v)必须小于绝对有效期(%v)", sp.IdleTTL, sp.AbsoluteTTL)
	}
	if sp.MaxActive < 1 {
		t.Fatalf("活跃会话上限必须 >= 1，实际 %d", sp.MaxActive)
	}
}

func TestLoginFallbackIsCoherent(t *testing.T) {
	lp := Login()
	if lp.FailThreshold < 3 {
		// 阈值 1~2 等于「正常用户打错两次密码就被要求验证码」
		t.Fatalf("转验证码阈值过小：%d", lp.FailThreshold)
	}
	if lp.IPFailLimit < 1 || lp.IPFailWindow <= 0 {
		t.Fatalf("IP 维度硬闸必须有效：limit=%d window=%v", lp.IPFailLimit, lp.IPFailWindow)
	}
	if len(lp.Backoff) == 0 {
		t.Fatal("退避阶梯不得为空（否则失败计数窗口为 0，等于不计数）")
	}
	for i := 1; i < len(lp.Backoff); i++ {
		if lp.Backoff[i] <= lp.Backoff[i-1] {
			t.Fatalf("退避阶梯必须严格递增：%v", lp.Backoff)
		}
	}
}

// TestBackoffForClamps 断言越界次数落在合法档位上。
//
// 这是失败计数的核心索引：次数从 Redis 读出来可能是 0（键刚过期）或
// 大于阶梯长度（长时间攻击），任何一种越界都会 panic 或取到 0 时长。
func TestBackoffForClamps(t *testing.T) {
	p := LoginPolicy{Backoff: []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}}
	cases := map[int]time.Duration{
		-1: time.Minute,
		0:  time.Minute,
		1:  time.Minute,
		2:  5 * time.Minute,
		3:  15 * time.Minute,
		4:  15 * time.Minute,
		99: 15 * time.Minute,
	}
	for n, want := range cases {
		if got := p.BackoffFor(n); got != want {
			t.Errorf("BackoffFor(%d) = %v，期望 %v", n, got, want)
		}
	}
	// 空阶梯必须给出非零兜底（0 = 键永不过期 = 永久计数，用户再也登不上）
	if got := (LoginPolicy{}).BackoffFor(1); got <= 0 {
		t.Fatalf("空阶梯兜底必须为正，实际 %v", got)
	}
}

func TestSceneOr(t *testing.T) {
	if got := sceneOr("always", SceneOff); got != SceneAlways {
		t.Errorf("合法值不应被覆盖，实际 %q", got)
	}
	if got := sceneOr("", SceneAfterFails); got != SceneAfterFails {
		t.Errorf("非法/空值应回落默认，实际 %q", got)
	}
	if got := sceneOr("ALWAYS", SceneOff); got != SceneOff {
		t.Errorf("大小写不匹配应视为非法，实际 %q", got)
	}
}

func TestRegisterPolicyMethodEnabled(t *testing.T) {
	p := RegisterPolicy{Channels: []string{"email"}}
	if !p.MethodEnabled("email") {
		t.Fatal("email 在允许列表内应可用")
	}
	if p.MethodEnabled("sms") {
		t.Fatal("sms 不在列表内必须不可用（否则注册会走一条没配好的通道）")
	}
}

// TestAvailableChannels 锁住「生效渠道集合」的两个不变量：
//
//  1. 顺序 = 优先级（sms 在 email 之前），前端直接取首项作默认值；
//  2. 恒返回非 nil —— 对外 JSON 必须是 [] 而不是 null（前端再 .length 判空）。
//
// 同时锁住「策略」与「就绪」的合取语义：任何一边不满足都不出现在结果里。
// 少判一边的后果分别是「用户填进必然发不出码的输入框」与「绕开运营的开关」。
func TestAvailableChannels(t *testing.T) {
	readyAll := func(string) bool { return true }
	readyNone := func(string) bool { return false }
	onlyEmail := func(ch string) bool { return ch == "email" }
	onlySMS := func(ch string) bool { return ch == "sms" }

	cases := []struct {
		name     string
		channels []string
		ready    func(string) bool
		want     []string
	}{
		{"双渠都就绪时短信优先", []string{"email", "sms"}, readyAll, []string{"sms", "email"}},
		{"只有邮箱就绪", []string{"sms", "email"}, onlyEmail, []string{"email"}},
		{"只有短信就绪", []string{"email", "sms"}, onlySMS, []string{"sms"}},
		{"策略只允许邮箱", []string{"email"}, readyAll, []string{"email"}},
		{"策略只允许短信", []string{"sms"}, readyAll, []string{"sms"}},
		{"策略与就绪都为空", nil, readyNone, []string{}},
	}
	for _, c := range cases {
		got := RegisterPolicy{Channels: c.channels}.AvailableChannels(c.ready)
		if got == nil {
			t.Fatalf("%s：必须返回非 nil 切片（对外 JSON 是 [] 不是 null）", c.name)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s：got %v want %v", c.name, got, c.want)
		}
	}
}

// TestRegisterFallbackMode 断言注册模式兜底为 open 而不是 closed。
//
// 反过来兜底的后果是「配置读不到 → 所有人注册不了」，而且没有任何报错。
// 真正的关闭必须来自显式配置（closed）。
func TestRegisterFallbackMode(t *testing.T) {
	rp := Register()
	switch rp.Mode {
	case RegisterOpen, RegisterInviteOnly, RegisterClosed:
	default:
		t.Fatalf("注册模式兜底值非法：%q", rp.Mode)
	}
	if rp.Mode != RegisterOpen {
		t.Fatalf("缺配置时应兜底 open，实际 %q", rp.Mode)
	}
	switch rp.ContactVerify {
	case ContactVerifyRequired, ContactVerifyOff:
	default:
		t.Fatalf("联系方式验证兜底值非法：%q", rp.ContactVerify)
	}
}
