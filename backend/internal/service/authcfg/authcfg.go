// Package authcfg 是认证策略的**唯一读取点**。
//
// 为什么要有这一层：登录阈值、会话 TTL、验证码场景、注册准入这四类参数
// 原先散落在 auth / binding / portal_config / 前端四处各自 conf.System().Get，
// 于是「配置改了但某条路径没读」这种缺陷无法被发现——它不报错，只是行为不一致。
// 现在任何 handler 都不许直接读 auth_* 键，只能调本包。
//
// 另外两个不属于 auth_* 但同属「账号侧资源约束」的键（key_limit_per_user、
// usage_default_window_days）也在这里读：它们的默认值、回落策略与登录阈值
// 是同一类问题，分散到各 handler 会出现"改一处忘一处"。
//
// 默认值的唯一来源是 db/seed.go；本包的 def* 常量**只在键缺失时防 panic**
// （正常启动流程里 Seed 必然先写入）。缺失本身由 Validate 在启动期拦下。
package authcfg

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"lingdong-api/internal/conf"
)

// 验证码场景。注册/登录/找回之外的场景（换绑）走登录后链路，单独命名。
const (
	SceneRegister      = "register"
	SceneLogin         = "login"
	ScenePasswordReset = "password_reset"
	SceneContactBind   = "contact_bind"
)

// 场景的人机验证策略取值。
const (
	SceneAlways     = "always"      // 每次都要
	SceneAfterFails = "after_fails" // 仅连续失败后要（只对 login 有意义）
	SceneOff        = "off"         // 从不要（注册场景被校验禁止）
)

// 注册模式。
const (
	RegisterOpen       = "open"        // 任何人可注册
	RegisterInviteOnly = "invite_only" // 必须带有效邀请码
	RegisterClosed     = "closed"      // 关闭注册
)

// 联系方式验证策略。
const (
	ContactVerifyRequired = "required"
	ContactVerifyOff      = "off"
)

// 兜底常量：仅用于配置键缺失时避免除零/永久锁定，取值与 seed 默认值一致。
const (
	defSessionAbsolute  = 7 * 24 * time.Hour
	defSessionIdle      = 12 * time.Hour
	defSessionTouch     = 5 * time.Minute
	defSessionMaxActive = 10

	defLoginFailThreshold = 5
	defLoginIPFailLimit   = 20
	defLoginIPWindow      = 10 * time.Minute
	defLoginDelayMax      = 800 * time.Millisecond

	defEventRetentionDays = 90

	// defKeyLimitPerUser 是单用户的用户自建 KEY 上限。
	// 与 auth_* 无关，但它和登录策略一样属于「账号侧资源约束」，
	// 放在这个包是为了让"账号相关阈值"只有一个读取入口。
	defKeyLimitPerUser = 20
	// defUsageWindowDays 是用量查询的默认时间窗（天）。
	defUsageWindowDays = 30
)

// CaptchaPolicy 是人机验证策略。
//
// 只有登录可配。注册与找回恒为「每次都要」（代码常量，不进配置）：
// 这两个入口一个是不需要凭据的建号口、一个会真的发一条短信/邮件，
// 给运维一个「关掉」的开关只会换来一次事故。
//
// 这里**没有**失败阈值：阈值只有一个来源 authcfg.Login().FailThreshold。
// 历史上这里另有一个 max_fails_before_captcha，两个阈值不一致时会出现
// 「服务端要求验证、但不告诉前端」，前端不亮图、用户提交必失败。
type CaptchaPolicy struct {
	Login string
}

// Captcha 读取人机验证策略。
func Captcha() CaptchaPolicy {
	var v struct {
		Scenes map[string]string `json:"scenes"`
	}
	_ = conf.System().Unmarshal("auth_captcha", &v)
	return CaptchaPolicy{Login: sceneOr(v.Scenes[SceneLogin], SceneAfterFails)}
}

// KeyLimitPerUser 返回单用户可保留的用户自建 KEY 上限（0 = 不限制）。
//
// 缺失或脏值时回落 20：这个值只影响"能不能再建一把"，回落到一个具体数字
// 比回落到"不限制"安全（后者等于没配就敞开）。
func KeyLimitPerUser() int {
	raw, ok := conf.System().Get("key_limit_per_user")
	if !ok {
		return defKeyLimitPerUser
	}
	var n int
	if json.Unmarshal(raw, &n) != nil || n < 0 {
		return defKeyLimitPerUser
	}
	return n
}

// UsageDefaultWindowDays 返回用量查询不传时间时的默认窗口天数（0 = 不加窗口）。
func UsageDefaultWindowDays() int {
	raw, ok := conf.System().Get("usage_default_window_days")
	if !ok {
		return defUsageWindowDays
	}
	var n int
	if json.Unmarshal(raw, &n) != nil || n < 0 {
		return defUsageWindowDays
	}
	return n
}

func sceneOr(v, def string) string {
	switch v {
	case SceneAlways, SceneAfterFails, SceneOff:
		return v
	}
	return def
}

// LoginPolicy 是登录失败处置策略。
type LoginPolicy struct {
	// FailThreshold 是「转人机验证」的失败次数阈值。
	FailThreshold int
	// Backoff 是失败计数的窗口阶梯（第 1..n 次失败对应第 1..n 档，超出取末档）。
	Backoff []time.Duration
	// IPFailLimit / IPFailWindow 是单 IP 的硬闸（唯一会返回 429 的地方）。
	IPFailLimit  int
	IPFailWindow time.Duration
	// DelayMax 是达阈值后服务端随机退避的上限（真实 sleep，不是前端假等）。
	DelayMax time.Duration
}

// Login 读取登录失败策略。
func Login() LoginPolicy {
	var v struct {
		FailThreshold   int   `json:"fail_threshold"`
		BackoffSeconds  []int `json:"backoff_seconds"`
		IPFailLimit     int   `json:"ip_fail_limit"`
		IPFailWindowSec int   `json:"ip_fail_window_sec"`
		DelayMaxMS      int   `json:"delay_max_ms"`
	}
	_ = conf.System().Unmarshal("auth_login", &v)

	out := LoginPolicy{
		FailThreshold: v.FailThreshold,
		IPFailLimit:   v.IPFailLimit,
		IPFailWindow:  time.Duration(v.IPFailWindowSec) * time.Second,
		DelayMax:      time.Duration(v.DelayMaxMS) * time.Millisecond,
	}
	if out.FailThreshold < 1 {
		out.FailThreshold = defLoginFailThreshold
	}
	if out.IPFailLimit < 1 {
		out.IPFailLimit = defLoginIPFailLimit
	}
	if out.IPFailWindow <= 0 {
		out.IPFailWindow = defLoginIPWindow
	}
	if out.DelayMax < 0 {
		out.DelayMax = defLoginDelayMax
	}
	out.Backoff = make([]time.Duration, 0, len(v.BackoffSeconds))
	for _, s := range v.BackoffSeconds {
		if s > 0 {
			out.Backoff = append(out.Backoff, time.Duration(s)*time.Second)
		}
	}
	if len(out.Backoff) == 0 {
		out.Backoff = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}
	}
	return out
}

// BackoffFor 返回「第 n 次失败」对应的计数窗口。
func (p LoginPolicy) BackoffFor(n int) time.Duration {
	if len(p.Backoff) == 0 {
		return time.Minute
	}
	if n < 1 {
		n = 1
	}
	if n > len(p.Backoff) {
		n = len(p.Backoff)
	}
	return p.Backoff[n-1]
}

// SessionPolicy 是会话生命周期策略。
type SessionPolicy struct {
	AbsoluteTTL   time.Duration
	IdleTTL       time.Duration
	TouchInterval time.Duration
	MaxActive     int
}

// Session 读取会话策略。
func Session() SessionPolicy {
	var v struct {
		AbsoluteTTLSec   int `json:"absolute_ttl_sec"`
		IdleTTLSec       int `json:"idle_ttl_sec"`
		MaxActive        int `json:"max_active"`
		TouchIntervalSec int `json:"touch_interval_sec"`
	}
	_ = conf.System().Unmarshal("auth_session", &v)

	out := SessionPolicy{
		AbsoluteTTL:   time.Duration(v.AbsoluteTTLSec) * time.Second,
		IdleTTL:       time.Duration(v.IdleTTLSec) * time.Second,
		TouchInterval: time.Duration(v.TouchIntervalSec) * time.Second,
		MaxActive:     v.MaxActive,
	}
	if out.AbsoluteTTL <= 0 {
		out.AbsoluteTTL = defSessionAbsolute
	}
	if out.IdleTTL <= 0 {
		out.IdleTTL = defSessionIdle
	}
	// 空闲超时必须小于绝对有效期，否则空闲判定永远不触发。
	if out.IdleTTL >= out.AbsoluteTTL {
		out.IdleTTL = out.AbsoluteTTL / 2
	}
	if out.TouchInterval <= 0 {
		out.TouchInterval = defSessionTouch
	}
	if out.MaxActive < 1 {
		out.MaxActive = defSessionMaxActive
	}
	return out
}

// RegisterPolicy 是注册准入策略。
type RegisterPolicy struct {
	Mode          string
	ContactVerify string
	Channels      []string
}

// Register 读取注册准入策略。
func Register() RegisterPolicy {
	var v struct {
		Mode            string   `json:"mode"`
		ContactVerify   string   `json:"contact_verify"`
		ContactChannels []string `json:"contact_channels"`
	}
	_ = conf.System().Unmarshal("auth_register", &v)

	mode := v.Mode
	switch mode {
	case RegisterOpen, RegisterInviteOnly, RegisterClosed:
	default:
		mode = RegisterOpen
	}
	verify := v.ContactVerify
	if verify != ContactVerifyRequired {
		verify = ContactVerifyOff
	}
	channels := make([]string, 0, len(v.ContactChannels))
	for _, ch := range v.ContactChannels {
		if ch == "email" || ch == "sms" {
			channels = append(channels, ch)
		}
	}
	return RegisterPolicy{Mode: mode, ContactVerify: verify, Channels: channels}
}

// MethodEnabled 判断某个联系方式渠道是否在该策略内被允许。
func (p RegisterPolicy) MethodEnabled(channel string) bool {
	for _, ch := range p.Channels {
		if ch == channel {
			return true
		}
	}
	return false
}

// channelPriority 是渠道优先级的**唯一来源**：短信优先于邮箱。
//
// 下发数组的顺序即优先级（首项即默认），客户端不得重排 —— 前端再排一次
// 就是第二个真相源，两边一旦不一致，用户看到的默认渠道与后端校验的渠道会分叉。
var channelPriority = [2]string{"sms", "email"}

// AvailableChannels 返回「策略白名单 ∩ 传输就绪」的渠道，按优先级排序，恒非 nil。
//
// ready 由调用方注入（controller / main 传 verify.Ready）：本包是纯策略层，
// 不 import notify/verify，依赖方向保持单向。
//
// 两个条件缺一不可：
//   - 策略允许（auth_register.contact_channels）—— 运营愿意用它；
//   - 传输就绪（SMTP / 短信商真的配好）—— 用了也发得出去。
//
// 只判策略会让用户填进一个必然发不出码的输入框；只判就绪会绕开运营的开关。
func (p RegisterPolicy) AvailableChannels(ready func(channel string) bool) []string {
	out := make([]string, 0, len(channelPriority))
	for _, ch := range channelPriority {
		if p.MethodEnabled(ch) && ready(ch) {
			out = append(out, ch)
		}
	}
	return out
}

// 二次验证策略取值。
const (
	MFAPolicyOff              = "off"                // 不检查，也不建议绑定
	MFAPolicyOptional         = "optional"           // 自愿绑定
	MFAPolicyRequiredForAdmin = "required_for_admin" // 管理员必须绑定，否则 /admin/** 403
)

// MFAPolicy 返回二次验证策略，取值见上面的三个常量。
//
// 未知/缺失一律按 optional 处理：这里返回的字符串决定「管理台要不要拦」，
// 把脏值当成 required_for_admin 会把所有人关在门外，当成 off 又会让运营
// 以为开关还在生效。optional 是「不拦」且与 seed 默认值一致。
func MFAPolicy() string {
	var v struct {
		Policy string `json:"policy"`
	}
	_ = conf.System().Unmarshal("auth_mfa", &v)
	switch v.Policy {
	case MFAPolicyOff, MFAPolicyOptional, MFAPolicyRequiredForAdmin:
		return v.Policy
	default:
		return MFAPolicyOptional
	}
}

// EventRetentionDays 返回认证事件保留天数。
func EventRetentionDays() int {
	n := int(conf.System().Int("auth_event_retention_days", defEventRetentionDays))
	if n < 1 {
		return defEventRetentionDays
	}
	return n
}

// requiredKeys 是认证功能依赖的配置键。缺任何一个都说明 seed 没跑或被人删了。
var requiredKeys = []string{
	"auth_captcha", "auth_login", "auth_session", "auth_register",
	"auth_mfa", "auth_event_retention_days",
}

// Validate 是启动期校验（LoadSystemConfig 之后调用）。
//
// 这里只做「配置自相矛盾就无法正常服务」的判定，不做业务判断。
// 返回 error 时调用方必须拒绝启动：带着矛盾配置运行的表现是
// 「注册永远 400 / 所有人都登不上」，而日志里没有任何线索。
//
// ready 传 verify.Ready（= SMTP host/凭证、短信商参数是否齐备）。
// 刻意**不**只看 auth_register 之外的 enabled 布尔：`email.enabled=true`
// 但 smtp_host 为空时，开关是亮的、发信是必然失败的 —— 那种配置必须
// 在启动期就被拦下，否则运行时表现为「注册页要求联系方式、却没有可用渠道」。
func Validate(ready func(channel string) bool) error {
	for _, k := range requiredKeys {
		if _, ok := conf.System().Get(k); !ok {
			return fmt.Errorf("authcfg: 缺少认证配置键 %s（seed 未执行或被误删）", k)
		}
	}

	sp := Session()
	if sp.AbsoluteTTL <= sp.IdleTTL {
		return fmt.Errorf("authcfg: 会话绝对有效期必须大于空闲超时")
	}

	rp := Register()
	if rp.ContactVerify == ContactVerifyRequired && len(rp.AvailableChannels(ready)) == 0 {
		return fmt.Errorf("authcfg: 注册要求联系方式验证，但所选渠道(%s)均不可用（未启用或缺少发送参数）",
			strings.Join(rp.Channels, ","))
	}
	return nil
}

// Encode 是给前端下发的认证配置（不含任何密钥）。
//
// 与 portal_config 的其它字段同一出口：前端只拿「能做什么」，
// 拿不到「用什么密钥做」。ready 传 verify.Ready，与 Validate 同一判据 ——
// 「启动时怎么判」与「下发给前端怎么判」必须是同一套，否则前端会展示
// 一个后端认为不可用的渠道。
func Encode(ready func(channel string) bool) map[string]any {
	cp := Captcha()
	rp := Register() // 只读一次：两次读之间若发生配置热更，两个字段会自相矛盾
	login := Login()
	return map[string]any{
		"register_mode":  rp.Mode,
		"contact_verify": rp.ContactVerify,
		// 策略白名单：运营允许用哪些渠道（不代表已经配好发送服务）。
		"contact_channels": rp.Channels,
		// 生效集合：白名单 ∩ 就绪，按优先级排序（sms 在前），首项即默认。
		// 注册/找回页只读这个，否则会渲染一个必然发不出码的输入框。
		"available_contact_channels": rp.AvailableChannels(ready),
		// 注册/找回是常量（恒 always），下发只为让前端知道"要渲染验证码"。
		"captcha_register":     SceneAlways,
		"captcha_login":        cp.Login,
		"captcha_reset":        SceneAlways,
		"login_fail_threshold": login.FailThreshold,
	}
}
