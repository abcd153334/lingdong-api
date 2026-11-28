package controller

import (
	"errors"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/audit"
	"lingdong-api/internal/service/presets"
)

// restorePresetsReq 是「恢复出厂」的请求体。
//
// Confirm 必须是字面量 RESTORE：这是唯一会覆盖运营改名/改价的动作，
// 不能靠"点了一个红色按钮"当作确认（API 也可能被脚本误调）。
type restorePresetsReq struct {
	Group       string `json:"group"`
	DryRun      bool   `json:"dry_run"`
	KeepEnabled *bool  `json:"keep_enabled"`
	Confirm     string `json:"confirm"`
}

// RestorePresets 恢复内置预设（dry_run=true 只预览并回滚）。
func RestorePresets(c *gin.Context) {
	var req restorePresetsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	if !req.DryRun && req.Confirm != "RESTORE" {
		response.FailDefMsg(c, errs.InvalidParam, `恢复出厂会覆盖内置渠道/模型配置：请传 confirm:"RESTORE"`)
		return
	}
	keep := true
	if req.KeepEnabled != nil {
		keep = *req.KeepEnabled
	}
	res, err := presets.Restore(db.DB, presets.RestoreOptions{
		Group: req.Group, DryRun: req.DryRun, KeepEnabled: keep,
	})
	if err != nil {
		if errors.Is(err, presets.ErrPresetsDisabled) {
			response.FailDefMsg(c, errs.InvalidParam, "本实例已关闭内置预设（LD_BUILTIN_PRESETS=0），恢复出厂不可用")
			return
		}
		if errors.Is(err, presets.ErrUnknownPresetGroup) {
			response.FailDefMsg(c, errs.InvalidParam, "未知的内置预设组："+req.Group)
			return
		}
		response.FailDef(c, errs.Internal)
		return
	}
	if !req.DryRun {
		counts := gin.H{}
		for _, g := range res.Groups {
			counts[g.Group] = gin.H{
				"channels": g.Channels, "models": g.Models, "bindings": g.Bindings,
			}
		}
		audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionPresetRestore, "preset", req.Group, gin.H{
			"group": req.Group, "keep_enabled": keep, "counts": counts,
		})
	}
	response.OK(c, res)
}
