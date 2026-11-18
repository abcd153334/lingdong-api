package controller

// 应用级公告（终端可见）。与站点公告 `system_config.announcements` 完全独立：
// 那一套面向门户登录用户且有已读状态，这一套面向接入方终端、按 app_id 隔离。

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

// adminAppNoticeReq 是公告的创建/更新体。
//
// 时间用 *time.Time 直接接 RFC3339（encoding/json 原生支持）：
// 前端把 `datetime-local` 的值补上本地时区转成 ISO 再提交，
// 后端不做"猜时区"这种事 —— 服务器时区与浏览器时区不一致时必然错 8 小时。
type adminAppNoticeReq struct {
	Title    string     `json:"title"`
	Content  string     `json:"content"`
	Level    string     `json:"level"`
	Enabled  bool       `json:"enabled"`
	StartsAt *time.Time `json:"starts_at"`
	EndsAt   *time.Time `json:"ends_at"`
}

// adminAppNoticeItem 是管理台列表的一行。
type adminAppNoticeItem struct {
	ID       uint64  `json:"id"`
	Title    string  `json:"title"`
	Content  string  `json:"content"`
	Level    string  `json:"level"`
	Enabled  bool    `json:"enabled"`
	StartsAt *string `json:"starts_at"`
	EndsAt   *string `json:"ends_at"`
	// Active 是"此刻是否会下发"的派生值：运营最常问的就是这句话，
	// 让前端自己用浏览器时间去算会与服务器判据产生偏差。
	Active    bool   `json:"active"`
	CreatedAt string `json:"created_at"`
}

func rfc3339OrNil(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

func noticeItemOf(r model.AppNotice, now time.Time) adminAppNoticeItem {
	return adminAppNoticeItem{
		ID: r.ID, Title: r.Title, Content: r.Content, Level: r.Level, Enabled: r.Enabled,
		StartsAt: rfc3339OrNil(r.StartsAt), EndsAt: rfc3339OrNil(r.EndsAt),
		// 与公开面共用同一份判据（service/app.ActiveOf），不让前端拿浏览器时钟另算一份。
		Active:    appsvc.ActiveOf(r, now),
		CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// AdminAppNoticeList 处理 GET /api/admin/apps/{id}/notices。
func AdminAppNoticeList(c *gin.Context) {
	a, ok := appOf(c)
	if !ok {
		return
	}
	p, err := pagination.Parse(c)
	if err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	rows, total, err := appsvc.ListNotices(c.Request.Context(), db.DB, a.ID, p.Offset(), p.Limit())
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	now := time.Now()
	items := make([]adminAppNoticeItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, noticeItemOf(r, now))
	}
	response.OK(c, pagination.New(items, total, p))
}

// AdminAppNoticeCreate 处理 POST /api/admin/apps/{id}/notices。
func AdminAppNoticeCreate(c *gin.Context) {
	a, ok := appOf(c)
	if !ok {
		return
	}
	var req adminAppNoticeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	in, msg := appsvc.NormalizeNotice(appsvc.NoticeInput{
		Title: req.Title, Content: req.Content, Level: req.Level, Enabled: req.Enabled,
		StartsAt: req.StartsAt, EndsAt: req.EndsAt,
	})
	if msg != "" {
		response.FailDef(c, errs.InvalidParam.WithMessage(msg))
		return
	}
	row, err := appsvc.CreateNotice(c.Request.Context(), db.DB, a.ID, in)
	if err != nil {
		failAppErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAppNoticeCreate, "app_notice",
		strconv.FormatUint(row.ID, 10), map[string]any{"app_id": a.ID, "title": row.Title})
	response.OK(c, noticeItemOf(*row, time.Now()))
}

// AdminAppNoticeUpdate 处理 PUT /api/admin/apps/{id}/notices/{nid}。
func AdminAppNoticeUpdate(c *gin.Context) {
	a, ok := appOf(c)
	if !ok {
		return
	}
	nid, ok := uintParam(c, "nid")
	if !ok {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	var req adminAppNoticeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	in, msg := appsvc.NormalizeNotice(appsvc.NoticeInput{
		Title: req.Title, Content: req.Content, Level: req.Level,
		StartsAt: req.StartsAt, EndsAt: req.EndsAt,
		// Enabled 刻意不传：上下架只由 PATCH /status 改。全量更新也写这一列时，
		// 运营 A 刚下架、运营 B 的旧弹窗一保存就会把它重新上架并对外下发。
	})
	if msg != "" {
		response.FailDef(c, errs.InvalidParam.WithMessage(msg))
		return
	}
	if err := appsvc.UpdateNotice(c.Request.Context(), db.DB, a.ID, nid, in); err != nil {
		failAppErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAppNoticeUpdate, "app_notice",
		strconv.FormatUint(nid, 10), map[string]any{"app_id": a.ID, "title": in.Title})
	response.OK(c, nil)
}

// adminAppNoticeStatusReq 用指针区分"没传"与"显式 false"。
type adminAppNoticeStatusReq struct {
	Enabled *bool `json:"enabled"`
}

// AdminAppNoticeStatus 处理 PATCH /api/admin/apps/{id}/notices/{nid}/status。
func AdminAppNoticeStatus(c *gin.Context) {
	a, ok := appOf(c)
	if !ok {
		return
	}
	nid, ok := uintParam(c, "nid")
	if !ok {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	var req adminAppNoticeStatusReq
	if err := c.ShouldBindJSON(&req); err != nil || req.Enabled == nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	if err := appsvc.SetNoticeEnabled(c.Request.Context(), db.DB, a.ID, nid, *req.Enabled); err != nil {
		failAppErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAppNoticeUpdate, "app_notice",
		strconv.FormatUint(nid, 10), map[string]any{"app_id": a.ID, "enabled": *req.Enabled})
	response.OK(c, nil)
}

// AdminAppNoticeDelete 处理 DELETE /api/admin/apps/{id}/notices/{nid}。
//
// 删除前把整行读出来写进审计：公告是物理删除，审计是唯一的事后依据。
// 只记 id/title 的话，"误删后重建"这件事根本做不到（content/level/时间窗都丢了）。
func AdminAppNoticeDelete(c *gin.Context) {
	a, ok := appOf(c)
	if !ok {
		return
	}
	nid, ok := uintParam(c, "nid")
	if !ok {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	var row model.AppNotice
	if err := db.DB.WithContext(c.Request.Context()).
		Where("id = ? AND app_id = ?", nid, a.ID).
		Limit(1).Find(&row).Error; err != nil || row.ID == 0 {
		response.FailDef(c, errs.AppNoticeNotFound)
		return
	}
	detail := map[string]any{
		"app_id": a.ID, "title": row.Title, "content": row.Content, "level": row.Level,
		"enabled": row.Enabled, "starts_at": row.StartsAt, "ends_at": row.EndsAt,
		"created_at": row.CreatedAt,
	}
	if err := appsvc.DeleteNotice(c.Request.Context(), db.DB, a.ID, nid); err != nil {
		failAppErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAppNoticeDelete, "app_notice",
		strconv.FormatUint(nid, 10), detail)
	response.OK(c, nil)
}
