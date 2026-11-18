package controller

// 维护模式开关。

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
	appsvc "lingdong-api/internal/service/app"
	"lingdong-api/internal/service/audit"
)

// adminAppMaintenanceReq 用指针区分"没传"与"显式 false"。
type adminAppMaintenanceReq struct {
	Enabled *bool  `json:"enabled"`
	Message string `json:"message"`
}

// AdminAppMaintenance 处理 PATCH /api/admin/apps/{id}/maintenance。
//
// 与 PATCH .../status 严格分工：
//   - status=2 只拦"新签发"，存量流量照跑（既有语义）；
//   - maintenance=true 只拦"存量流量"，不影响签发。
//
// 两者不联动，因为一次点击不该同时完成两件事。
func AdminAppMaintenance(c *gin.Context) {
	a, ok := appOf(c)
	if !ok {
		return
	}
	var req adminAppMaintenanceReq
	if err := c.ShouldBindJSON(&req); err != nil || req.Enabled == nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	msg := strings.TrimSpace(req.Message)
	if utf8.RuneCountInString(msg) > 200 {
		response.FailDef(c, errs.InvalidParam.WithMessage("维护提示不超过 200 个字符"))
		return
	}
	if err := db.DB.WithContext(c.Request.Context()).Model(&model.App{}).Where("id = ?", a.ID).
		Updates(map[string]any{
			"maintenance_mode":    *req.Enabled,
			"maintenance_message": msg,
		}).Error; err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	// 本实例立即生效；其余实例最多 10s 收敛（见 service/app/maintenance.go）。
	// 公开面 profile 读的就是这份快照，所以这里只需失效一处。
	appsvc.InvalidateMaintenance()
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAppMaintenance, "app",
		strconv.FormatUint(a.ID, 10), map[string]any{"enabled": *req.Enabled, "message": msg})
	response.OK(c, gin.H{"maintenance_mode": *req.Enabled, "maintenance_message": msg})
}
