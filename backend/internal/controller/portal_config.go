package controller

import (
	"github.com/gin-gonic/gin"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/money"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/pkg/sitecfg"
	"lingdong-api/internal/pkg/siteurl"
	"lingdong-api/internal/service/announcement"
	"lingdong-api/internal/service/authcfg"
	"lingdong-api/internal/service/notify"
	"lingdong-api/internal/service/verify"
)

// SiteView 返回门户侧站点品牌视图（唯一模型来自 pkg/sitecfg）。
//
// 这里不再声明第二份 struct：`site` 的字段形状只允许有一个定义，
// 否则新增一个字段（例如 favicon_image）漏改这里，门户就永远拿不到它。
func SiteView() sitecfg.Site {
	s := sitecfg.Load()
	if s.SiteName == "" {
		s.SiteName = "灵动 API"
	}
	// 币种走归一化：库里可能存了小写、空串或未来新增的非法值，
	// 直接透传会让前端拿到 "" 然后拼出「undefined1.44」。
	s.Currency = conf.SiteCurrency()
	return s
}

// capabilitiesPayload 是「接入能力」的唯一投影。
//
// 门户 /api/portal/config 与对客 /api/auth/capabilities 共用它：两处各写一份时，
// 新增一个开关只改一边，就会出现"门户说需要验证、开放接口说不需要"——
// 而第三方正是照着后者写死逻辑的。
//
// 这里**只放准入相关**的字段：品牌、公告、站点正文、价格、币种一律不出现在
// capabilities 里（那是门户渲染需要，不是接入决策需要）。
func capabilitiesPayload() gin.H {
	auth := authcfg.Encode(verify.Ready)
	emailReady, smsReady := notify.EmailReady(), notify.SMSReady()
	return gin.H{
		"register_mode":    auth["register_mode"],
		"invite_required":  auth["register_mode"] == authcfg.RegisterInviteOnly,
		"contact_verify":   auth["contact_verify"],
		"contact_channels": auth["contact_channels"],
		// 生效渠道集合（策略 ∩ 就绪，按优先级排序，首项即默认）。
		// 与上面的 contact_channels（策略白名单）分开下发：注册/找回只该
		// 让用户看见"真的发得出去"的渠道，否则填完必然失败。
		"available_contact_channels": auth["available_contact_channels"],
		// 三个场景的验证码策略：注册与找回恒为 always（代码常量）。
		"captcha_register":     auth["captcha_register"],
		"captcha_login":        auth["captcha_login"],
		"captcha_reset":        auth["captcha_reset"],
		"login_fail_threshold": auth["login_fail_threshold"],
		// 渠道是否**真的能发信**（SMTP / 短信服务已配置并启用）。
		// 与 contact_channels（策略允许用哪些）是两件事：策略开着但 SMTP 没配，
		// 发码必然失败，接入方需要能提前知道。
		"email_enabled": emailReady,
		"sms_enabled":   smsReady,
		// 换绑要求至少有一个渠道真的可用，否则端点必然 400。
		// 口径是**仅就绪**（不含注册策略）：安全换绑不看 auth_register，
		// 前端若用 available_contact_channels 判它，会把后端允许的操作关掉。
		"contact_bind_enabled": emailReady || smsReady,
		"custom_topup_enabled": conf.System().Bool("custom_topup_enabled", true),
		// 兑换码：当前没有"关闭兑换"的开关，恒 true。
		// 保留字段是为了将来加开关时不用改契约 —— 接入方不必为它写死假设。
		"redeem_enabled": true,
		"min_topup":      conf.System().Int("min_topup", money.Scale),
		// 用量查询不传时间时的默认窗口（天）：接入方据此知道"默认能查到多久"。
		"usage_default_window_days": authcfg.UsageDefaultWindowDays(),
	}
}

// AuthCapabilities 处理 GET /api/auth/capabilities（公开）。
//
// 为什么单独一条而不是让接入方读 /api/portal/config：后者同时下发品牌、
// 公告、站点正文、价格口径 —— 那是门户的渲染契约，不是接入契约。
// 让第三方依赖整包，改一次门户文案就可能"看起来像是接口变了"。
func AuthCapabilities(c *gin.Context) {
	response.OK(c, capabilitiesPayload())
}

// PortalConfig 处理 GET /api/portal/config（公开）：门户与登录注册所需的公开配置。
//
// 结算口径（币种/符号/qpc）在这里也下发一份：首屏（含未登录）就要显示金额，
// 而 /api/pricing 有可能因为模型表为空而返回空目录 —— 那时不能让页面
// 退回硬编码的 $ 与 500000。
func PortalConfig(c *gin.Context) {
	// 这份响应含运营模式开关：被浏览器缓存住会让「关闭门户」延迟生效。
	// 前端已有 30s 内存缓存（react-query），这里禁掉 HTTP 层即可。
	c.Header("Cache-Control", "no-store")
	site := SiteView()
	// 对外地址的**唯一**出口：配置 → 站点地址 → 请求 origin，并带来源标记。
	// 前端不猜域名（曾经硬编码过生产域名，自建部署会把别人的地址显示给用户）。
	bases, source := siteurl.DisplayBases(c)
	response.OK(c, gin.H{
		"site": site,
		"api": gin.H{
			"bases":  bases,
			"source": source,
		},
		"currency":        site.Currency,
		"currency_symbol": conf.SiteCurrencySymbol(),
		// 运营模式（唯一出口）：false = 纯服务端，门户站点不可用、仅管理员可登录。
		// 与 middleware 的门户闸同源（conf.PortalEnabled 是唯一判据）。
		"portal_enabled": conf.PortalEnabled(),
		// scale 让首屏（含未登录）就能格式化金额，不必等 /api/pricing。
		"scale":          money.Scale,
		"other_currency": conf.OtherCurrency(site.Currency),
		// 与 /api/pricing 的 cny_per_usd 同类型（float64）：同一个键在两个端点
		// 一个是字符串一个是数字，前端只能写两套解析，迟早漏一处。
		"cny_per_usd":       conf.CnyPerUsd(),
		"about_content":     conf.System().String("about_content", ""),
		"agreement_content": conf.System().String("agreement_content", ""),
		// 认证准入（唯一出口）：注册模式、是否需要联系方式验证、各场景的人机验证策略。
		// 人机验证是自托管图形验证码，**没有任何密钥**需要下发或保管。
		"auth":                 authcfg.Encode(verify.Ready),
		"custom_topup_enabled": conf.System().Bool("custom_topup_enabled", true),
		// 默认值必须与下单校验同源（order.minTopup 的默认是 1 个货币单位）。
		// 这里曾经写死 `1` —— 那是 **1 微**，门户于是显示「最低 ¥0.000001」，
		// 用户看到这个数字根本不知道能不能充。
		"min_topup": conf.System().Int("min_topup", money.Scale),
		// 换绑入口的可用性判据（**仅就绪**，与 auth_register 策略无关）：
		// 后端 BindContact 只要求通道配好，不看注册渠道白名单。
		// 注册/找回的渠道集合在 auth.available_contact_channels 里，两者不要混用。
		"email_enabled": notify.EmailReady(),
		"sms_enabled":   notify.SMSReady(),
	})
}

// portalAnnouncementsPayload 是门户公告的**唯一**响应形状（GET 与 /read 共用）。
//
// 两个端点返回同一份结构，前端拿到就能整份替换缓存，不必写「keys → items」的映射 ——
// 一旦有两套形状，未读徽标与条目状态迟早对不上。
func portalAnnouncementsPayload(enabled bool, items []announcement.ItemView) gin.H {
	if items == nil {
		items = []announcement.ItemView{}
	}
	return gin.H{"enabled": enabled, "items": items}
}

// PortalAnnouncements 处理 GET /api/portal/announcements：公告时间线（含当前用户已读状态）。
func PortalAnnouncements(c *gin.Context) {
	cfg, items, err := announcement.ListFor(c.Request.Context(), db.DB, c.GetUint64("user_id"))
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, portalAnnouncementsPayload(cfg.Enabled, items))
}

// PortalAnnouncementsRead 处理 POST /api/portal/announcements/read：把当前全部公告标记已读。
//
// 不收请求体：要标记哪些公告由服务端从配置派生。让客户端传 keys 等于让它回答
// 「哪些公告存在」，既多一条真相源，也能往已读表里写任意 id。
// 响应直接回新的整份 payload —— 写入成功即「全部已读」，是本操作的后置条件，
// 不需要再查一次已读表（查了反而多一条读路径）。
func PortalAnnouncementsRead(c *gin.Context) {
	uid := c.GetUint64("user_id")
	if uid == 0 {
		response.FailDef(c, errs.Unauthorized)
		return
	}
	cfg := announcement.Load()
	if !cfg.Enabled {
		response.OK(c, portalAnnouncementsPayload(false, nil))
		return
	}
	if err := announcement.MarkReadAll(c.Request.Context(), db.DB, uid, cfg.Items); err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	views := make([]announcement.ItemView, 0, len(cfg.Items))
	for _, it := range cfg.Items {
		views = append(views, announcement.ItemView{Item: it, Read: true})
	}
	response.OK(c, portalAnnouncementsPayload(true, views))
}
