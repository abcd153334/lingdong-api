package controller

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/apikey"
	"lingdong-api/internal/service/audit"
	"lingdong-api/internal/service/authevent"
)

// 本文件是管理台对 API KEY 的兜底启停（唯一人工恢复通道）。
//
// 为什么必须有：status=2 的托管密钥唯一来源是管理员动作（强制重置全禁 /
// 本端点禁用），而门户对 hosted 的启停一律 409 —— 没有这里，误处置就只能改库。

// adminKeyStatusReq 是 PATCH /api/admin/keys/:id/status 的请求体。
type adminKeyStatusReq struct {
	// Status 用指针区分「没传」与「传 0」；1 启用 / 2 禁用。
	Status *int `json:"status"`
}

// AdminKeyStatus 处理 PATCH /api/admin/keys/:id/status。
//
// 执行流程：解析校验 → 读 key（软删自动 404）→ 单行主键更新 → 管理审计 +
// 用户侧安全事件（by_admin）。KEY 级并发无歧义：单行主键 UPDATE 原子。
func AdminKeyStatus(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		response.FailDef(c, errs.NotFound)
		return
	}
	var req adminKeyStatusReq
	if err := c.ShouldBindJSON(&req); err != nil || req.Status == nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	if *req.Status != int(apikey.KeyStatusEnabled) && *req.Status != int(apikey.KeyStatusDisabled) {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	var k model.APIKey
	if err := db.DB.WithContext(c.Request.Context()).First(&k, id).Error; err != nil {
		// First 自带软删过滤：deleted_at 非空的 KEY 查不到，统一 404。
		response.FailDef(c, errs.NotFound)
		return
	}
	// 解禁时顺带刷新 last_used_at：禁用期间密钥不会产生使用记录，若禁用超过
	// 闲置期（默认 180 天），解禁后首次请求仍会被「真闲置过期」判死 —— 解锁通道
	// 就成了半截。刷新后由 Authenticate 的救回路径自动续期 expired_at，
	// 不做第二次写入（保持 expired_at 只有 renewExpiredAt 一个写点）。
	//
	// disabled_reason 与 status 必须同写（唯一判据见 model.APIKey.CanRotateRecover）：
	//   - 禁用 → admin_block：管理台是**人**的判定，任何自助轮换都不得覆盖它；
	//   - 启用 → 清空：恢复信任后，下一次轮换应按正常路径自愈。
	updates := map[string]any{"status": *req.Status}
	if *req.Status == int(apikey.KeyStatusEnabled) {
		updates["last_used_at"] = time.Now()
		updates["disabled_reason"] = ""
	} else {
		updates["disabled_reason"] = model.KeyDisabledAdminBlock
	}
	res := db.DB.WithContext(c.Request.Context()).Model(&model.APIKey{}).
		Where("id = ?", id).Updates(updates)
	if res.Error != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	if res.RowsAffected == 0 {
		response.FailDef(c, errs.NotFound)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionKeyAdminStatus, "api_key",
		strconv.FormatUint(id, 10), map[string]any{"status": *req.Status, "kind": k.Kind, "user_id": k.UserID})
	// 用户侧安全日志：让「我的 KEY 怎么被禁了/解禁了」能对到管理员操作。
	// 用独立事件而不是复用 key_revoke/key_issue：后者会让用户以为
	// 有人替他签发了新钥匙（自助签发与平台处置必须能一眼区分）。
	event := authevent.EventKeyAdminDisable
	if *req.Status == int(apikey.KeyStatusEnabled) {
		event = authevent.EventKeyAdminEnable
	}
	authevent.Record(c.Request.Context(), authevent.Event{
		UID: k.UserID, Event: event, Result: authevent.ResultSuccess,
		Detail: map[string]any{"key_id": k.ID, "kind": k.Kind, "by_admin": true},
	})
	response.OK(c, nil)
}
