package controller

import (
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/pkg/siteurl"
	"lingdong-api/internal/service/pricing"
)

// PortalVendors 处理 GET /api/portal/vendors（公开，设计 §4.3.8 L496）：
// 与 /api/pricing 的 vendors 段**同一个真相源**（pricing.PublicVendors），
// 不另建第二份查询 —— 两份各自查库必然随时间漂移（一份滤空分组、另一份不滤）。
func PortalVendors(c *gin.Context) {
	out, err := pricing.PublicVendors(siteurl.Base(c))
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, out)
}

// contentView 是站点内容正文（按空行分段，设计 §4.3.7 L486）。
type contentView struct {
	Paragraphs []string `json:"paragraphs"`
}

// agreementView 在正文基础上单独下发「生效日期」（设计 §4.3.7 L487 单独强调展示）。
type agreementView struct {
	Paragraphs    []string `json:"paragraphs"`
	EffectiveDate string   `json:"effective_date"`
}

// EffectiveDatePrefix 是协议正文中「生效日期」行的前缀（单独抽离为字段下发）。
const effectiveDatePrefix = "生效日期"

// blankLineSplit 按空行分段（与门户原型 agreement.html 一致）。
var blankLineSplit = regexp.MustCompile(`\n\s*\n`)

// PortalContentAbout 处理 GET /api/portal/content/about（门户 JWT，设计 §4.3.7 L486）。
func PortalContentAbout(c *gin.Context) {
	response.OK(c, contentView{Paragraphs: splitParagraphs(conf.System().String("about_content", ""))})
}

// PortalContentAgreement 处理 GET /api/portal/content/agreement（门户 JWT，设计 §4.3.7 L486-487）。
func PortalContentAgreement(c *gin.Context) {
	body := make([]string, 0, 8)
	effective := ""
	for _, p := range splitParagraphs(conf.System().String("agreement_content", "")) {
		if strings.HasPrefix(p, effectiveDatePrefix) {
			effective = trimEffectiveDate(p)
			continue
		}
		body = append(body, p)
	}
	response.OK(c, agreementView{Paragraphs: body, EffectiveDate: effective})
}

// splitParagraphs 按空行分段并去除空白段（空内容返回空数组而非 nil）。
func splitParagraphs(content string) []string {
	raw := blankLineSplit.Split(content, -1)
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// trimEffectiveDate 从「生效日期：2026-10-01」一类段落中取出日期值。
func trimEffectiveDate(block string) string {
	v := strings.TrimPrefix(block, effectiveDatePrefix)
	v = strings.TrimLeft(v, "：: \t")
	return strings.TrimSpace(v)
}
