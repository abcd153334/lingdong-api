package controller

// 本文件是**应用托管密钥**的端点：
//
//	GET  /api/user/apps                    （对外面：列出我已接入的应用）
//	GET  /api/portal/apps                  已删除：浏览器改由密钥列表直出 app_slug
//	POST /api/user/apps/{slug}/hosted-key  （对外面 Bearer 会话票）
//	POST /api/portal/apps/{slug}/hosted-key（浏览器 Cookie + CSRF）
//
// 签发/轮换双面挂载（同一 handler，不同凭证面）：`/api/user/*` 刻意不读 Cookie，
// 浏览器用它只会拿到 401 —— 而前端把 401 当「会话失效」处理（清登录态 + 守卫
// 跳登录），于是门户一调就被踢下线。不要试图把两个凭证面并进一个路由组
// （那等于给 Cookie 请求开一条无 CSRF 的路）。
//
// 为什么签发只认会话票（user 面 SessionTokenAuth、portal 面即 Cookie 会话）：
// 这两个动作一个要读应用清单、一个要造凭证。API KEY 能签发 API KEY 是一条
// 自我复制链 —— 一把泄露的密钥可以无限繁殖新密钥，而"撤销"只能靠人发现。
// 会话票背后是真人登录（且可挂 2FA），是唯一能承担"发放凭证"这个动作的凭证。
//
// 为什么不给一个"应用注册"端点：应用的新建/停用是平台侧动作（谁是应用、
// 哪个应用能接入），由管理员在后台配置。让任何登录用户都能注册应用，
// 等于把平台的接入白名单交给公众。

import (
	"errors"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/keyscope"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/apikey"
	"lingdong-api/internal/service/authevent"
)

// appSlugPattern 是应用标识的格式约束：小写字母、数字、连字符，1-32 位。
//
// 与 apps.slug 的列宽一致。用连字符而不是下划线，是因为 slug 会出现在 URL 路径里
// （/api/user/apps/{slug}/hosted-key），保留下划线会让它和"下划线在部分终端里
// 被吞掉"的历史问题纠缠。
var appSlugPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// hostedKeyReq 是签发入参。空体等价于 {"rotate": false}。
type hostedKeyReq struct {
	// Rotate 表达"想换一把新的"这个意图，**不参与授权判定**。
	//
	// 是否属于"首次接入"由库中是否已存在该 (app,user) 的托管密钥决定。
	// 用请求参数做授权锚点等于让调用方自己声明"我不是新用户"——
	// 一个 {"rotate":true} 就能绕过 allow_self_issue 凭空首次签发。
	Rotate bool `json:"rotate"`
	// Name 只在首次签发时生效（轮换不改名）。
	Name string `json:"name"`
}

// UserHostedKeyIssue 处理 POST /api/user/apps/{slug}/hosted-key 与 /api/portal/apps/{slug}/hosted-key。
//
// 幂等契约：
//   - 首次调用：返回一次性明文（`plaintext` 非空），此后平台**不再**提供回显；
//   - 再次调用（rotate=false）：返回同一把密钥的元数据，`plaintext` 为空串；
//   - rotate=true：新明文立即生效，旧明文进入重叠窗口（默认 72h，可配 0–720），
//     窗口内旧明文仍可用（见 service/apikey 的窗口说明）。
//
// 授权锚点是**库中是否存在这把钥**，不是请求体里的 rotate：
//   - 无既有钥 = 首次接入 → 受应用停用与 allow_self_issue 双重约束；
//   - 有既有钥 = 已接入 → 不受这两者约束（应用停用只拦新接入，不拦已跑流量的轮换，
//     否则开关一关就让接入方在下一次轮换日彻底断服）。
func UserHostedKeyIssue(c *gin.Context) {
	slug := strings.TrimSpace(c.Param("slug"))
	if !appSlugPattern.MatchString(slug) {
		response.FailDef(c, errs.AppNotFound)
		return
	}
	var req hostedKeyReq
	// 空体是合法的（rotate=false）：接入方第一次拿到的是"给我一把密钥"，
	// 不该被迫构造一个 {} 才能调用。
	//
	// 判据是 io.EOF 而不是 ContentLength：chunked 编码（HTTP/1.1 流式、
	// 部分反向代理的默认行为）下 ContentLength 是 -1，用 `> 0` 判断会把
	// 一个**带体**的请求当成空体 —— 于是 rotate=true 被静默忽略，调用方
	// 拿到 plaintext="" 还以为"没有新明文，继续用旧钥"。
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	// 名称长度与自建密钥同一口径（api_key.go 的 keyReq 校验）：列宽 64，
	// 但 30 个字符已经够辨识。不校验的表现是超长直接 500（PG 22001）。
	if name := strings.TrimSpace(req.Name); len([]rune(name)) > 30 {
		response.FailDef(c, errs.InvalidParam.WithMessage("密钥备注名不得超过 30 个字符"))
		return
	}
	ctx := c.Request.Context()

	var app model.App
	if err := db.DB.WithContext(ctx).Where("slug = ?", slug).First(&app).Error; err != nil {
		response.FailDef(c, errs.AppNotFound)
		return
	}

	uid := c.GetUint64("user_id")
	var existing model.APIKey
	err := db.DB.WithContext(ctx).
		Where("app_id = ? AND user_id = ? AND kind = ?", app.ID, uid, model.KeyKindHosted).
		First(&existing).Error
	var existingPtr *model.APIKey
	switch {
	case err == nil:
		existingPtr = &existing
	case !errors.Is(err, gorm.ErrRecordNotFound):
		response.FailDef(c, errs.Internal)
		return
	}

	if existingPtr == nil {
		// 首次接入才受这两条约束。
		if app.Status != model.AppStatusEnabled {
			// 与「不存在」合并成同一个码：分开会让调用方用两次请求就能判断
			// "这个 slug 存在但被停用了"，而 slug 是无随机化的人类可读串，
			// 枚举成本只有一次请求。对真正接了该应用的一方，'应用不可用'
			// 这一条信息足够（他去找平台管理员即可）。
			response.FailDef(c, errs.AppNotFound)
			return
		}
		if !app.AllowSelfIssue {
			response.FailDef(c, errs.Forbidden.WithMessage("该应用已关闭自助签发，请联系运营获取接入码"))
			return
		}
	}

	res, err := apikey.CreateHosted(ctx, apikey.HostedInput{
		AppID: app.ID, UserID: uid,
		Name: strings.TrimSpace(req.Name), Rotate: req.Rotate,
		Existing: existingPtr,
	})
	if err != nil {
		// 映射表只有 apikey.ErrDef 一份；这里唯一的显式覆盖是**封禁**：
		// 调用方带的是会话票且完全有效，被封的是"目标那把钥"。用 401 表达会让
		// 浏览器端把"目标钥被封"当成"你的登录掉了"，点一次轮换就被踢回登录页。
		def := apikey.DefOr(err, errs.Internal)
		if errors.Is(err, apikey.ErrKeyDisabled) {
			def = errs.KeyRotateBlocked
		}
		response.FailDef(c, def)
		return
	}

	// 托管密钥是"应用能替用户花钱"的凭证，签发与轮换必须留痕。
	recordKeyEvent(c, authevent.EventKeyIssue, res.Key.ID, map[string]any{
		"kind": model.KeyKindHosted, "app_id": app.ID, "app_slug": app.Slug,
		"rotate": res.Plaintext != "" && !res.Created,
		// 重叠窗口记入审计：事后追溯"某次断服"时，能对上"是不是旧钥窗口用完了"。
		"previous_expires_at": formatTimePtr(res.PreviousExpiresAt),
	})
	response.OK(c, gin.H{
		"app": gin.H{
			"id": app.ID, "slug": app.Slug, "name": app.Name,
		},
		"key": hostedKeyIssueView{
			hostedKeyView: newHostedKeyView(res.Key, res.PreviousExpiresAt),
			Created:       res.Created,
			Rotated:       res.Plaintext != "" && !res.Created,
			IssuedAt:      res.Key.CreatedAt,
		},
		// 空串表示"没有新明文"（命中已有密钥且未轮换）。
		// 客户端必须据此决定是否覆盖自己存的凭证 —— 用空串覆盖会让应用永久失效。
		"plaintext": res.Plaintext,
	})
}

// formatTimePtr 把可空时间转成审计用的 RFC3339 字符串（nil 返回空串）。
func formatTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339)
}

// hostedAppItem 是「我已接入的应用」一行。
type hostedAppItem struct {
	AppID     uint64 `json:"app_id"`
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	KeyID     uint64 `json:"key_id"`
	KeyPrefix string `json:"key_prefix"`
	// Scopes 是这把托管密钥实际带的权限点（文档与排障用）。
	Scopes    []string   `json:"scopes"`
	CreatedAt time.Time  `json:"created_at"`
	RotatedAt *time.Time `json:"rotated_at"`
}

// UserAppList 处理 GET /api/user/apps：列出**我已持有托管密钥**的应用。
//
// 只有对外（Bearer）面：浏览器侧的密钥列表已直出 app_slug/app_name，
// 不再需要单独请求应用清单（那条 Cookie 读端点已删除）。
//
// 为什么不返回平台的全部应用：应用清单是平台拓扑，接入方只需要知道自己那一个。
// 而且用户 A 在应用 X 下有没有密钥，与应用 Y 无关 —— 把 Y 的 slug 和名字
// 告诉他，等于让任何一个终端用户能枚举平台接入了哪些产品。
//
// 因此"首次接入"必须先调签发端点（slug 来自应用自己的部署配置），
// 之后本端点用于：拿 app_id 做用量过滤、确认密钥前缀、看权限集。
func UserAppList(c *gin.Context) {
	type row struct {
		AppID     uint64
		Slug      string
		Name      string
		KeyID     uint64
		KeyPrefix string
		Scopes    model.JSON
		CreatedAt time.Time
		ResetAt   *time.Time
	}
	var rows []row
	q := db.DB.WithContext(c.Request.Context()).
		Table("apps AS a").
		Select("a.id AS app_id, a.slug, a.name, k.id AS key_id, k.key_prefix, k.scopes, k.created_at, k.reset_secret_at AS reset_at").
		Joins("JOIN api_keys AS k ON k.app_id = a.id AND k.user_id = ? AND k.kind = ? AND k.deleted_at IS NULL",
			c.GetUint64("user_id"), model.KeyKindHosted)
	// 托管密钥只能看到**自己这一个应用**。
	//
	// 不加这一条时，应用 A 的密钥会拿到该用户在 B/C 下的 slug、应用名、
	// key_id、密钥前缀与权限集 —— 平台接入拓扑与跨应用归属全部外泄。
	// 会话票/自建 KEY 仍返回全部（那是用户本人的视角）。
	if s, ok := keyscope.Of(c); ok && s.Hosted {
		q = q.Where("k.id = ?", s.KeyID)
	}
	err := q.Order("a.slug").Scan(&rows).Error
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	items := make([]hostedAppItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, hostedAppItem{
			AppID: r.AppID, Slug: r.Slug, Name: r.Name,
			KeyID: r.KeyID, KeyPrefix: r.KeyPrefix,
			Scopes:    apikey.DecodeStrings(r.Scopes),
			CreatedAt: r.CreatedAt, RotatedAt: r.ResetAt,
		})
	}
	response.OK(c, gin.H{"items": items})
}
