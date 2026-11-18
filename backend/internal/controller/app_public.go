package controller

// 应用公开面：终端在用户登录**之前**就要展示品牌、公告与"检查更新"，
// 此时没有任何凭证。这是唯一免凭证的应用信息面，返回体是白名单
// （见 service/app/public.go）。
//
// 为什么是三条端点而不是一条聚合 —— 判据是「变更频率 × 鉴权等级 × 缓存策略」：
//   - 品牌 + 维护态：同出 apps 一行、同属人工运营动作、同一缓存档，且冷启动
//     时两样都要 → 合在 profile 是正确聚合，不是妥协；
//   - 公告：最易变、体积最大（最坏约 40KB）、判据是精确时间窗 → 单独一条；
//   - 版本：几乎不变、检查更新是独立场景（1 次/天） → 单独一条。
//
// 具体的缓存秒数与限流额度**不写在这里**：它们是契约里的 PublicPolicy，
// 由路由、Cache-Control 与文档生成器共同读取（写在这里必然漂移）。
//
// 合成一条的代价不是"字段多"，而是**只能取最严的那个**：公告必须新鲜 ⇒ 整条
// no-store ⇒ 周级变更的品牌每次冷调用都要重查一遍（实测 5 条 SQL，其中
// usages 的 24h 聚合只为算一个本就不该公开的"在线状态"）。

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/apicontract"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
	appsvc "lingdong-api/internal/service/app"
)

// 三条端点的客户端缓存时长来自契约（apicontract 的 PublicPolicy），
// 不在这里另写常量：文档里的 "max-age=60" 由同一份声明生成，
// 改数值不需要再记得同步三处。

// AppProfilePublic 处理 GET /api/apps/{slug}/profile。
func AppProfilePublic(c *gin.Context) {
	a, ok := publicAppOf(c)
	if !ok {
		return
	}
	// 维护态走 MaintenanceOf：与 /v1 的 503 闸共用同一份快照，两处不会打架。
	msg, blocked := appsvc.MaintenanceOf(c.Request.Context(), a.ID)
	respondCached(
		c,
		appsvc.ProfileOf(a, logoURLOf(c, a), msg, blocked),
		apicontract.MustPublicPolicy("apps.profile").MaxAge,
	)
}

// AppNoticesPublic 处理 GET /api/apps/{slug}/notices。
func AppNoticesPublic(c *gin.Context) {
	a, ok := publicAppOf(c)
	if !ok {
		return
	}
	data, err := appsvc.NoticesOf(c.Request.Context(), db.DB, a.ID, time.Now())
	if err != nil {
		// 不返回空数组伪装成功：终端会把"没有公告"当成真相，运营则完全看不到故障。
		response.FailDef(c, errs.Internal)
		return
	}
	respondCached(c, data, apicontract.MustPublicPolicy("apps.notices").MaxAge)
}

// AppReleasePublic 处理 GET /api/apps/{slug}/release。
func AppReleasePublic(c *gin.Context) {
	a, ok := publicAppOf(c)
	if !ok {
		return
	}
	data, err := appsvc.ReleaseOf(c.Request.Context(), db.DB, a.ID)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	respondCached(c, data, apicontract.MustPublicPolicy("apps.release").MaxAge)
}

// publicAppOf 是三条端点的唯一入口：slug 格式校验 + 行快照查表。
//
// 停用（status=2）**不**拦这里：停用的语义是"拒绝新签发，已有密钥仍可用"
// （model/app.go），若公开面回 404，终端就会出现"能对话、界面显示应用不存在"。
// 要断流量用维护模式，不是停用。
func publicAppOf(c *gin.Context) (*model.App, bool) {
	slug := strings.TrimSpace(c.Param("slug"))
	// 格式先挡：不合规的 slug 与"不存在"同码，避免把参数格式当成信息泄露面。
	if !appSlugPattern.MatchString(slug) {
		response.FailDef(c, errs.AppNotFound)
		return nil, false
	}
	a, err := appsvc.AppBySlug(c.Request.Context(), slug)
	if err != nil {
		if errors.Is(err, appsvc.ErrNotFound) {
			response.FailDef(c, errs.AppNotFound)
			return nil, false
		}
		response.FailDef(c, errs.Internal)
		return nil, false
	}
	return a, true
}

// respondCached 是三条端点的唯一出口：编码 → 弱 ETag → 304 或写字节。
//
// 为什么弱 ETag：响应会经过全局 gzip（带 Vary: Accept-Encoding），同一 ETag
// 对应两种传输表示，强验证器不成立。理由见 writeETag 的注释。
func respondCached(c *gin.Context, payload any, maxAge int) {
	raw, err := response.Encode(payload)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	if writeETag(c, raw, maxAge, false, true) {
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", raw)
}
