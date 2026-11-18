package controller

// 应用版本发布（单通道）。状态机在 service 层唯一实现，这里只做参数与审计。

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/pkg/response"
	appsvc "lingdong-api/internal/service/app"
	"lingdong-api/internal/service/audit"
)

// adminAppReleaseReq 是版本的创建/更新体。
type adminAppReleaseReq struct {
	Version      string `json:"version"`
	ReleaseNotes string `json:"release_notes"`
	DownloadURL  string `json:"download_url"`
	ForceUpdate  bool   `json:"force_update"`
}

// adminAppReleaseItem 是管理台版本列表的一行。
type adminAppReleaseItem struct {
	ID           uint64  `json:"id"`
	Version      string  `json:"version"`
	ReleaseNotes string  `json:"release_notes"`
	DownloadURL  string  `json:"download_url"`
	ForceUpdate  bool    `json:"force_update"`
	Status       int     `json:"status"`
	PublishedAt  *string `json:"published_at"`
	CreatedAt    string  `json:"created_at"`
}

// releaseItemOf 把版本行投影成管理台列表项。
//
// 时间一律输出 RFC3339（UTC）：`"2006-01-02 15:04:05"` 这种无时区串会被浏览器
// 按本地时区解析，跨时区部署整体偏 8 小时。DTO 里转成 UTC，由前端按用户时区展示。
func releaseItemOf(r model.AppRelease) adminAppReleaseItem {
	item := adminAppReleaseItem{
		ID: r.ID, Version: r.Version, ReleaseNotes: r.ReleaseNotes,
		DownloadURL: r.DownloadURL, ForceUpdate: r.ForceUpdate, Status: int(r.Status),
		CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339),
	}
	if r.PublishedAt != nil {
		s := r.PublishedAt.UTC().Format(time.RFC3339)
		item.PublishedAt = &s
	}
	return item
}

// AdminAppReleaseList 处理 GET /api/admin/apps/{id}/releases。
func AdminAppReleaseList(c *gin.Context) {
	a, ok := appOf(c)
	if !ok {
		return
	}
	p, err := pagination.Parse(c)
	if err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	var status int16
	if raw := c.Query("status"); raw != "" {
		v, perr := strconv.Atoi(raw)
		if perr != nil || (v != int(model.AppReleaseDraft) && v != int(model.AppReleasePublished) && v != int(model.AppReleaseRevoked)) {
			response.FailDef(c, errs.InvalidParam)
			return
		}
		status = int16(v)
	}
	rows, total, err := appsvc.ListReleases(c.Request.Context(), db.DB, a.ID, status, p.Offset(), p.Limit())
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	items := make([]adminAppReleaseItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, releaseItemOf(r))
	}
	// 「当前对外下发」的版本：同一应用允许并存多个已发布（例如热修回填），
	// 对外取 published_at 最新的那个。不告诉前端是哪一个，运营只能靠猜 ——
	// 而猜错的后果是撤回掉正在生效的版本。
	cur, err := appsvc.LatestPublishedOf(c.Request.Context(), db.DB, a.ID)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	var curID *uint64
	if cur != nil {
		curID = &cur.ID
	}
	response.OK(c, adminAppReleasePage{
		Result:           pagination.New(items, total, p),
		CurrentReleaseID: curID,
	})
}

// adminAppReleasePage 是版本列表响应：分页字段 + 当前下发版本。
type adminAppReleasePage struct {
	pagination.Result
	CurrentReleaseID *uint64 `json:"current_release_id"`
}

// AdminAppReleaseCreate 处理 POST /api/admin/apps/{id}/releases。
//
// 一律建草稿：先建后发，避免"填错下载地址直接推给全量终端"。
func AdminAppReleaseCreate(c *gin.Context) {
	a, ok := appOf(c)
	if !ok {
		return
	}
	var req adminAppReleaseReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	in, msg := appsvc.NormalizeRelease(appsvc.ReleaseInput{
		Version: req.Version, ReleaseNotes: req.ReleaseNotes,
		DownloadURL: req.DownloadURL, ForceUpdate: req.ForceUpdate,
	})
	if msg != "" {
		response.FailDef(c, errs.InvalidParam.WithMessage(msg))
		return
	}
	row, err := appsvc.CreateRelease(c.Request.Context(), db.DB, a.ID, in)
	if err != nil {
		failAppErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAppReleaseCreate, "app_release",
		strconv.FormatUint(row.ID, 10), map[string]any{"app_id": a.ID, "version": row.Version})
	response.OK(c, releaseItemOf(*row))
}

// AdminAppReleaseUpdate 处理 PUT /api/admin/apps/{id}/releases/{rid}。
func AdminAppReleaseUpdate(c *gin.Context) {
	a, ok := appOf(c)
	if !ok {
		return
	}
	rid, ok := uintParam(c, "rid")
	if !ok {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	var req adminAppReleaseReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	in, msg := appsvc.NormalizeRelease(appsvc.ReleaseInput{
		Version: req.Version, ReleaseNotes: req.ReleaseNotes,
		DownloadURL: req.DownloadURL, ForceUpdate: req.ForceUpdate,
	})
	if msg != "" {
		response.FailDef(c, errs.InvalidParam.WithMessage(msg))
		return
	}
	if _, err := appsvc.UpdateRelease(c.Request.Context(), db.DB, a.ID, rid, in); err != nil {
		failAppErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAppReleaseUpdate, "app_release",
		strconv.FormatUint(rid, 10), map[string]any{"app_id": a.ID, "version": in.Version})
	response.OK(c, nil)
}

// adminAppReleaseStatusReq 只接受 2（发布）/3（撤回）。
type adminAppReleaseStatusReq struct {
	Status *int `json:"status"`
}

// AdminAppReleaseStatus 处理 PATCH /api/admin/apps/{id}/releases/{rid}/status。
func AdminAppReleaseStatus(c *gin.Context) {
	a, ok := appOf(c)
	if !ok {
		return
	}
	rid, ok := uintParam(c, "rid")
	if !ok {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	var req adminAppReleaseStatusReq
	if err := c.ShouldBindJSON(&req); err != nil || req.Status == nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	// 必须先在 int 上校验再转换：`int16(65538)` 会被截成 2，
	// 于是一个越界值就能把草稿"发布"出去，审计里记的还是截断后的 2。
	if *req.Status != int(model.AppReleasePublished) && *req.Status != int(model.AppReleaseRevoked) {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	next := int16(*req.Status)
	if err := appsvc.SetReleaseStatus(c.Request.Context(), db.DB, a.ID, rid, next); err != nil {
		failAppErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAppReleaseStatus, "app_release",
		strconv.FormatUint(rid, 10), map[string]any{"app_id": a.ID, "status": next})
	response.OK(c, nil)
}

// AdminAppReleaseDelete 处理 DELETE /api/admin/apps/{id}/releases/{rid}（仅草稿）。
func AdminAppReleaseDelete(c *gin.Context) {
	a, ok := appOf(c)
	if !ok {
		return
	}
	rid, ok := uintParam(c, "rid")
	if !ok {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	row, err := appsvc.DeleteRelease(c.Request.Context(), db.DB, a.ID, rid)
	if err != nil {
		failAppErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAppReleaseDelete, "app_release",
		strconv.FormatUint(rid, 10), map[string]any{"app_id": a.ID, "version": row.Version})
	response.OK(c, nil)
}
