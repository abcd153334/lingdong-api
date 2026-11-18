package app

import (
	"context"
	"strings"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/service/iconref"
)

// BrandInput 是对外品牌信息（终端应用可见）。
//
// 与 apps.Name/Description 的区别：那两个只用于后台辨识，改了不影响接入方；
// 这里的字段会通过公开接口下发，改动等于换品牌。
type BrandInput struct {
	DisplayName  string
	Tagline      string
	Intro        string
	LogoIcon     string
	ContactName  string
	ContactEmail string
	SupportURL   string
	PrivacyURL   string
	TermsURL     string
}

// NormalizeBrand 去空白并校验，返回归一化后的入参与错误文案（"" = 通过）。
//
// 返回文案而不是 error：校验失败是**字段级**的，前端要把它显示在对应输入框下，
// 用 error 串起来最后只能 toast 一句"参数不合法"，运营不知道该改哪。
func NormalizeBrand(in BrandInput) (BrandInput, string) {
	in.DisplayName = strings.TrimSpace(in.DisplayName)
	in.Tagline = strings.TrimSpace(in.Tagline)
	in.Intro = strings.TrimSpace(in.Intro)
	in.LogoIcon = strings.TrimSpace(in.LogoIcon)
	in.ContactName = strings.TrimSpace(in.ContactName)
	in.ContactEmail = strings.TrimSpace(in.ContactEmail)
	in.SupportURL = strings.TrimSpace(in.SupportURL)
	in.PrivacyURL = strings.TrimSpace(in.PrivacyURL)
	in.TermsURL = strings.TrimSpace(in.TermsURL)

	switch {
	case in.DisplayName == "":
		return in, "展示名称必填"
	case !maxRunes(in.DisplayName, 64):
		return in, "展示名称不超过 64 个字符"
	case !maxRunes(in.Tagline, 120):
		return in, "标语不超过 120 个字符"
	case !maxRunes(in.Intro, 500):
		return in, "简介不超过 500 个字符"
	case !maxRunes(in.ContactName, 64):
		return in, "联系人不超过 64 个字符"
	case !maxRunes(in.ContactEmail, 128):
		return in, "联系邮箱不超过 128 个字符"
	case in.ContactEmail != "" && !strings.Contains(in.ContactEmail, "@"):
		return in, "联系邮箱格式不正确"
	case !maxRunes(in.SupportURL, 512), !maxRunes(in.PrivacyURL, 512), !maxRunes(in.TermsURL, 512):
		return in, "链接长度不超过 512 个字符"
	case !validHTTPURL(in.SupportURL):
		return in, "支持链接必须是 http(s) 地址"
	case !validHTTPURL(in.PrivacyURL):
		return in, "隐私政策必须是 http(s) 地址"
	case !validHTTPURL(in.TermsURL):
		return in, "服务条款必须是 http(s) 地址"
	case !maxRunes(in.LogoIcon, 64):
		return in, "图标标识不合法"
	case in.LogoIcon != "" && iconref.Path(in.LogoIcon) == "":
		// 令牌写错（拼错 key / 手填任意字符串）时，iconref 会把它派生成空地址：
		// 保存成功、公开面却静默没有 logo，而运营以为已经换好了。
		// 这里直接拒绝，让错误在保存那一刻暴露。
		return in, "图标标识不合法（应为 builtin:<内置图标> 或 upload:<上传ID>）"
	}
	return in, ""
}

// SaveBrand 全量写品牌列。
//
// 用 map 而不是 Save：Save 会把结构体零值写回（例如 Status/AllowSelfIssue），
// 而本方法只应触碰品牌那 9 列 —— 与 PUT /admin/apps/{id}（身份字段）互不干扰。
func SaveBrand(ctx context.Context, gdb *gorm.DB, id uint64, in BrandInput) error {
	res := gdb.WithContext(ctx).Model(&model.App{}).Where("id = ?", id).Updates(map[string]any{
		"display_name":  in.DisplayName,
		"tagline":       in.Tagline,
		"intro":         in.Intro,
		"logo_icon":     in.LogoIcon,
		"contact_name":  in.ContactName,
		"contact_email": in.ContactEmail,
		"support_url":   in.SupportURL,
		"privacy_url":   in.PrivacyURL,
		"terms_url":     in.TermsURL,
	})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		// 全量提交与库内值完全相同时也可能 0 行；用存在性判断区分"没这条"。
		var n int64
		if err := gdb.WithContext(ctx).Model(&model.App{}).Where("id = ?", id).Count(&n).Error; err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
	}
	return nil
}

// BrandView 是对外品牌视图（logo 已是可直接渲染的绝对地址）。
//
// 公开面与官网都读这一份投影：品牌字段只有一处定义，加字段就只改这里
// （此前 BasicView 另有一份扁平副本，两处一分叉就会出现"卡片叫 A、弹窗叫 B"）。
type BrandView struct {
	DisplayName  string `json:"display_name"`
	Tagline      string `json:"tagline"`
	Intro        string `json:"intro"`
	LogoURL      string `json:"logo_url"`
	ContactName  string `json:"contact_name"`
	ContactEmail string `json:"contact_email"`
	SupportURL   string `json:"support_url"`
	PrivacyURL   string `json:"privacy_url"`
	TermsURL     string `json:"terms_url"`
}

// AdminBrandView 是品牌的管理端形状：BrandView + 图标令牌。
//
// 令牌只给后台：它编码了存储位置（`upload:<id>` / `builtin:<key>`），
// 终端拿到也没有用，下发只会多一个需要长期兼容的内部格式。
type AdminBrandView struct {
	BrandView
	LogoIcon string `json:"logo_icon"`
}

// DisplayNameOf 是"展示名称"的唯一回落规则：空则用后台名称。
//
// 只此一份：列表卡片、概览、公开面全部经它取值。
func DisplayNameOf(a *model.App) string {
	if n := strings.TrimSpace(a.DisplayName); n != "" {
		return n
	}
	// 终端至少要有个能显示的名字，所以回落到后台名称。
	return a.Name
}

// BrandOf 把应用行投影成品牌视图。logoURL 由调用方用 iconref 派生
// （service 层不依赖 gin，也不该知道站点基址从哪来）。
func BrandOf(a *model.App, logoURL string) BrandView {
	return BrandView{
		DisplayName:  DisplayNameOf(a),
		Tagline:      a.Tagline,
		Intro:        a.Intro,
		LogoURL:      logoURL,
		ContactName:  a.ContactName,
		ContactEmail: a.ContactEmail,
		SupportURL:   a.SupportURL,
		PrivacyURL:   a.PrivacyURL,
		TermsURL:     a.TermsURL,
	}
}

// AdminBrandOf 是管理端品牌视图（含图标令牌，供表单回填与保存回显）。
func AdminBrandOf(a *model.App, logoURL string) AdminBrandView {
	return AdminBrandView{BrandView: BrandOf(a, logoURL), LogoIcon: a.LogoIcon}
}
