package controller

// 应用品牌信息（对外展示）。与 PUT /admin/apps/{id}（身份字段）分开：
// 一次保存只改一组语义，避免"改个客服邮箱"顺带把名称/自助签发开关写回去。

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
	appsvc "lingdong-api/internal/service/app"
	"lingdong-api/internal/service/audit"
)

// adminAppBrandReq 是品牌全量提交体（不含 logo 的绝对 URL，只收令牌）。
type adminAppBrandReq struct {
	DisplayName  string `json:"display_name"`
	Tagline      string `json:"tagline"`
	Intro        string `json:"intro"`
	LogoIcon     string `json:"logo_icon"`
	ContactName  string `json:"contact_name"`
	ContactEmail string `json:"contact_email"`
	SupportURL   string `json:"support_url"`
	PrivacyURL   string `json:"privacy_url"`
	TermsURL     string `json:"terms_url"`
}

// AdminAppBrandGet 处理 GET /api/admin/apps/{id}/brand。
//
// 与 PUT 同形（含 logo_icon 与 logo_url）：品牌页签只需要这 10 个字段，
// 不必为了回填表单去拉一次近 7 日聚合。
func AdminAppBrandGet(c *gin.Context) {
	a, ok := appOf(c)
	if !ok {
		return
	}
	response.OK(c, appsvc.AdminBrandOf(a, logoURLOf(c, a)))
}

// AdminAppBrandUpdate 处理 PUT /api/admin/apps/{id}/brand。
func AdminAppBrandUpdate(c *gin.Context) {
	a, ok := appOf(c)
	if !ok {
		return
	}
	var req adminAppBrandReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	in, msg := appsvc.NormalizeBrand(appsvc.BrandInput{
		DisplayName: req.DisplayName, Tagline: req.Tagline, Intro: req.Intro,
		LogoIcon: req.LogoIcon, ContactName: req.ContactName, ContactEmail: req.ContactEmail,
		SupportURL: req.SupportURL, PrivacyURL: req.PrivacyURL, TermsURL: req.TermsURL,
	})
	if msg != "" {
		response.FailDef(c, errs.InvalidParam.WithMessage(msg))
		return
	}
	if err := appsvc.SaveBrand(c.Request.Context(), db.DB, a.ID, in); err != nil {
		failAppErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAppBrandUpdate, "app",
		strconv.FormatUint(a.ID, 10), map[string]any{"display_name": in.DisplayName})
	// 公开面的应用行快照要立刻失效，否则品牌最多 10 秒后才会在公开接口上变化
	// （再叠加客户端 max-age，运营会认为"保存了没生效"）。
	appsvc.InvalidateAppPublic()
	// 回显归一化后的值：前端据此更新预览，也避免"提交了什么、存了什么"两套。
	a.DisplayName, a.Tagline, a.Intro, a.LogoIcon = in.DisplayName, in.Tagline, in.Intro, in.LogoIcon
	a.ContactName, a.ContactEmail = in.ContactName, in.ContactEmail
	a.SupportURL, a.PrivacyURL, a.TermsURL = in.SupportURL, in.PrivacyURL, in.TermsURL
	response.OK(c, appsvc.AdminBrandOf(a, logoURLOf(c, a)))
}
