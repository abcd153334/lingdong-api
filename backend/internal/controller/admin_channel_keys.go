package controller

import (
	"fmt"
	"strconv"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/audit"
	"lingdong-api/internal/service/channel"
)

// GetAdminChannelKeys 处理 GET /api/admin/channels/:id/keys。
//
// 只回指纹与状态，**永不回明文**：明文只在录入那一刻出现在请求体里，
// 之后任何接口都拿不回来（结构体里没有可放明文的字段）。
func GetAdminChannelKeys(c *gin.Context) {
	id, ok := channelID(c)
	if !ok {
		return
	}
	res, err := channel.Keys(c.Request.Context(), id)
	if err != nil {
		respondChannelErr(c, err)
		return
	}
	response.OK(c, res)
}

// keyOpReq 是密钥池变更的单条操作。
type keyOpReq struct {
	Op      string   `json:"op"`
	Key     string   `json:"key"`
	Keys    []string `json:"keys"`
	KeyID   string   `json:"key_id"`
	Index   *int     `json:"index"`
	Alias   *string  `json:"alias"`
	Enabled *bool    `json:"enabled"`
}

// PatchAdminChannelKeys 处理 PATCH /api/admin/channels/:id/keys。
//
// 一个原子 mutation 覆盖 add / replace / replace_all / remove / meta / reset_cooldown：
// 拆成 6 个 CRUD 端点会让「先删后加」变成两次请求，中间态能被别的请求看到，
// 而密钥池任何中间态都可能让线上请求取不到 key。
func PatchAdminChannelKeys(c *gin.Context) {
	id, ok := channelID(c)
	if !ok {
		return
	}
	var body struct {
		Version int        `json:"version"`
		Ops     []keyOpReq `json:"ops"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	if len(body.Ops) == 0 {
		response.FailDefMsg(c, errs.InvalidParam, "ops 不能为空")
		return
	}
	// 批量粘贴会一次发 N 条 add，这里必须挡住无上限的数组。
	if len(body.Ops) > channel.MaxKeyOpsPerRequest {
		response.FailDefMsg(c, errs.InvalidParam, fmt.Sprintf(
			"单次提交最多 %d 个操作，当前 %d 个，请分批提交",
			channel.MaxKeyOpsPerRequest, len(body.Ops)))
		return
	}
	ops := make([]channel.KeyOp, 0, len(body.Ops))
	for _, op := range body.Ops {
		ops = append(ops, channel.KeyOp{
			Op: op.Op, Key: op.Key, Keys: op.Keys, KeyID: op.KeyID, Index: op.Index,
			Alias: op.Alias, Enabled: op.Enabled,
		})
	}
	res, err := channel.PatchKeys(c.Request.Context(), id, body.Version, ops)
	if err != nil {
		respondChannelErr(c, err)
		return
	}
	// 审计只记计数与版本：**绝不放明文或明文前缀**。
	var added, replaced, rebuilt, removed, metaN, reset int
	for _, op := range body.Ops {
		switch op.Op {
		case "add":
			added++
		case "replace":
			replaced++
		case "replace_all":
			rebuilt++
		case "remove":
			removed++
		case "meta":
			metaN++
		case "reset_cooldown":
			reset++
		}
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionChannelUpdate, "channel",
		strconv.FormatUint(id, 10), gin.H{
			"keys_changed": true, "op_add": added, "op_replace": replaced,
			"op_replace_all": rebuilt, "op_remove": removed,
			"op_meta": metaN, "op_reset": reset, "version": res.Version,
		})
	// 密钥池变化会改变可路由性（全禁用时该渠道不再可用）。
	afterChange(c.Request.Context(), "channel.keys")
	response.OK(c, res)
}
