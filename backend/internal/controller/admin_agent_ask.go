package controller

import (
	"errors"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/agent"
)

// 本文件是 `POST /api/admin/agent/asks/:id/answer`：回答助手提出的问题。
//
// 三条实现要点（改之前先读）：
//  1. **绝不取会话锁**。发消息端点会 TakeLock（同一会话只跑一轮），而作答是
//     "正在跑的那一轮的输入"——取锁必然 409，功能等于不存在。替代防线是
//     行状态 + CAS + run 存活三道（见 service/agent/ask.go 的 AnswerAsk）。
//  2. 三种失败必须分开报：已答（409，别再点）/ 已随本轮结束（410，重新发需求）/
//     值不合法（400）。合成一个"操作失败"，运营只能反复点同一个按钮。
//  3. 作答成功后**立刻**返回，不等模型继续：前端把卡片切成已答态，
//     后续内容仍然从原来那条 SSE 连接流进来。

// AgentAnswerAsk 处理 POST /api/admin/agent/asks/:id/answer。
func AgentAnswerAsk(c *gin.Context) {
	id, err := strconv.ParseUint(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id == 0 {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	var req struct {
		Selected []string `json:"selected"`
		FreeText string   `json:"free_text"`
		Skipped  bool     `json:"skipped"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	row, err := agent.AnswerAsk(c.Request.Context(), id, c.GetUint64("user_id"), agent.AskAnswer{
		Selected: req.Selected, FreeText: req.FreeText, Skipped: req.Skipped,
	})
	if err != nil {
		switch {
		case errors.Is(err, agent.ErrAskNotFound):
			response.FailDef(c, errs.NotFound)
		case errors.Is(err, agent.ErrAskAnswered):
			response.FailDef(c, errs.AgentAskAnswered)
		case errors.Is(err, agent.ErrAskExpired):
			response.FailDef(c, errs.AgentAskExpired)
		case errors.Is(err, agent.ErrAskInvalid):
			response.FailDef(c, errs.AgentAskInvalid)
		default:
			response.FailDef(c, errs.Internal)
		}
		return
	}
	response.OK(c, gin.H{"id": row.ID, "status": row.Status})
}
