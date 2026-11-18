// Package notify 是通知通道（SMTP 邮件 / 短信）的统一发送层（S10，step-10 §⑤）。
// 统一语义：通道未启用或参数不完整 → errs.NotifyChannelDisabled（调用方按「未配置即禁用」分支处理）。
package notify

import (
	"context"
	"encoding/json"
	"strings"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/pkg/errs"
)

// 验证码场景（与 service/auth 的 scene 常量一致；此处为字符串常量，避免反向依赖）。
const (
	SceneEmailBind     = "email_bind"
	ScenePhoneBind     = "phone_bind"
	ScenePasswordReset = "password_reset"
)

// EmailSettings 是 `email` 配置键的结构（v2.0 §4.2.6）。
type EmailSettings struct {
	Enabled    bool              `json:"enabled"`
	SMTPHost   string            `json:"smtp_host"`
	SMTPPort   int               `json:"smtp_port"`
	Encryption string            `json:"encryption"`
	Username   string            `json:"username"`
	Password   string            `json:"password"`
	FromAddr   string            `json:"from_addr"`
	SenderName string            `json:"sender_name"`
	Scenes     map[string]bool   `json:"scenes"`
	Templates  map[string]TplCfg `json:"templates"`
}

// TplCfg 是单场景邮件模板。
type TplCfg struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// SMSSettings 是 `sms` 配置键的结构（v2.0 §4.2.6）。
type SMSSettings struct {
	Enabled    bool                      `json:"enabled"`
	Active     string                    `json:"active"`
	DailyLimit int64                     `json:"daily_limit"`
	Channels   map[string]map[string]any `json:"channels"`
}

// Email 返回当前生效的邮件配置。
func Email() EmailSettings {
	var s EmailSettings
	_ = conf.System().Unmarshal("email", &s)
	return s
}

// SMS 返回当前生效的短信配置。
func SMS() SMSSettings {
	var s SMSSettings
	_ = conf.System().Unmarshal("sms", &s)
	return s
}

// EmailReady 判断邮件通道是否可用于发送（总闸 + SMTP 关键参数齐备）。
func EmailReady() bool {
	e := Email()
	return e.Enabled && e.SMTPHost != "" && e.FromAddr != ""
}

// SMSReady 判断短信通道是否可用于发送（总闸 + 选中渠道参数齐备）。
func SMSReady() bool {
	s := SMS()
	if !s.Enabled || s.Channels == nil {
		return false
	}
	ch := s.Channels[s.Active]
	if ch == nil {
		return false
	}
	for _, f := range smsSchemes[s.Active] {
		if v, ok := ch[f.Key].(string); !ok || v == "" {
			return false
		}
	}
	return true
}

// SiteName 读取站点名（邮件模板变量兜底）。
func SiteName() string {
	var site struct {
		SiteName string `json:"site_name"`
	}
	_ = conf.System().Unmarshal("site", &site)
	if site.SiteName == "" {
		return "灵动 API"
	}
	return site.SiteName
}

// SendCode 按场景分发验证码通知：邮箱类场景走 SMTP（reset_password 模板），手机号场景走短信。
// 事务性验证码不受 email.scenes 总闸约束（scenes 仅约束通知类邮件）。
func SendCode(ctx context.Context, scene, target, code string) error {
	switch scene {
	case SceneEmailBind, ScenePasswordReset:
		return sendTemplateMail(ctx, "reset_password", target, map[string]string{"code": code})
	case ScenePhoneBind:
		return sendSMS(ctx, target, code)
	default:
		return errs.NotifyChannelDisabled
	}
}

// SendTestEmail 发送一封测试邮件（管理端「测试发送」；不走 scenes 总闸）。
func SendTestEmail(ctx context.Context, to string) error {
	e := Email()
	if !EmailReady() {
		return errs.NotifyChannelDisabled
	}
	subject := "【" + SiteName() + "】邮件通道测试"
	body := "这是一封来自 " + SiteName() + " 的测试邮件。\n\n收到本邮件说明 SMTP 通道配置正确。"
	return sendMail(ctx, e, to, subject, body)
}

// SendTestSMS 发送一条测试短信（管理端「测试发送」）。
func SendTestSMS(ctx context.Context, phone string) error {
	if !SMSReady() {
		return errs.NotifyChannelDisabled
	}
	return sendSMS(ctx, phone, "123456")
}

// sendTemplateMail 渲染指定场景模板并发送；模板缺失或通道未就绪时返回 NotifyChannelDisabled。
func sendTemplateMail(ctx context.Context, scene, to string, vars map[string]string) error {
	e := Email()
	if !EmailReady() {
		return errs.NotifyChannelDisabled
	}
	tpl, ok := e.Templates[scene]
	if !ok || tpl.Subject == "" || tpl.Body == "" {
		return errs.NotifyChannelDisabled
	}
	vars["site_name"] = SiteName()
	return sendMail(ctx, e, to, render(tpl.Subject, vars), render(tpl.Body, vars))
}

// render 替换模板中的 {{key}} 变量。
func render(tpl string, vars map[string]string) string {
	out := tpl
	for k, v := range vars {
		out = strings.ReplaceAll(out, "{{"+k+"}}", v)
	}
	return out
}

// smsChannelField 是渠道专属参数 schema 的展示项（校验/就绪判定共用）。
type smsChannelField struct{ Key, Label string }

var smsSchemes = map[string][]smsChannelField{
	"aliyun": {
		{"access_key_id", "AccessKey ID"}, {"access_key_secret", "AccessKey Secret"},
		{"sign_name", "短信签名"}, {"template_code", "模板 CODE"},
	},
	"tencent": {
		{"secret_id", "SecretId"}, {"secret_key", "SecretKey"},
		{"sdk_app_id", "SDK App ID"}, {"sign_name", "短信签名"}, {"template_id", "模板 ID"},
	},
}

// jsonString 读取渠道参数中的字符串项。
func jsonString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// marshalJSON 序列化任意值为 JSON 字符串（短信 TemplateParam）。
func marshalJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
