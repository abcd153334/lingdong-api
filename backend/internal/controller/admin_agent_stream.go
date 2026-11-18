package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/agent"
)

// 本文件是 `POST /api/admin/agent/sessions/:id/messages`：发一条用户消息并跑一轮编排（SSE）。
//
// 三条实现要点：
//  1. **首帧之前先写响应头**。等 SSE 事件再写头会让前端在"连接建立"和
//     "第一个字"之间无法区分——那段时间恰好是最长的（首包延迟）。
//  2. 每帧后 Flush。不 Flush 的话 gin/nginx 会攒够缓冲区才发，前端看到的是
//     "卡住 8 秒然后一次吐一大段"。
//  3. 客户端断开 ≠ 服务端失败。断开时要把已生成内容落库（aborted），
//     并让 Run 自己收尾——直接 return 会把 token 记账一起丢掉。

// maxAgentBodyBytes 是发消息请求体的上限（一条需求不会超过 256KB）。
const maxAgentBodyBytes = 256 * 1024

// AgentSessionMessage 处理 POST /api/admin/agent/sessions/:id/messages。
func AgentSessionMessage(c *gin.Context) {
	sessionID, ok := agentID(c)
	if !ok {
		return
	}
	var req struct {
		Content string   `json:"content"`
		ModelID string   `json:"model_id"`
		DocIDs  []uint64 `json:"doc_ids"`
		// Scope 是入口的自我约束（如从「文档」页进来 = ["doc"]）。
		// 工具层用它拦住「顺手改别的实体」，与 apply 时的 EnforceScope 同一套词表。
		Scope []string `json:"scope"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAgentBodyBytes)
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		response.FailDef(c, errs.InvalidParam.WithMessage("请输入内容"))
		return
	}
	// 先确认会话存在：不存在时必须在写响应头之前回 404，
	// 否则前端拿到 200 + 一个 error 事件，会把它当成"模型报错"。
	if _, err := agent.GetSession(c.Request.Context(), sessionID); err != nil {
		respondAgentErr(c, err)
		return
	}
	// 同一会话同时只允许一轮：两轮并发会让 messages.seq 交错、上下文互相污染。
	release, got := agent.TakeLock(c.Request.Context(), sessionID)
	if !got {
		// 分流：正在等回答 ≠ 忙。"忙"要求用户等，"等回答"要求用户去答上一题 ——
		// 报同一个 agent_busy，前端只能显示"正在处理，请稍候"，而实际上它在等人。
		if cur, err := agent.PendingAsk(c.Request.Context(), sessionID); err == nil && cur != nil {
			response.FailDef(c, errs.AgentAwaitingAnswer)
			return
		}
		response.FailDef(c, errs.AgentBusy)
		return
	}
	defer release()

	userMsg, err := agent.AppendMessage(c.Request.Context(), sessionID, "user",
		strings.TrimSpace(req.Content), nil, agent.MessageMeta{Status: "ok"})
	if err != nil {
		respondAgentErr(c, err)
		return
	}

	// 首帧之前把头写掉（含 X-Accel-Buffering: no，防 nginx 缓冲）。
	h := c.Writer.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)
	c.Writer.Flush()

	emit := func(ev agent.Event) error {
		// ping 是等待回答期间的心跳：一律写成 SSE **注释帧**。
		// 用注释而不是事件：前端 EventSource 层不会把它当数据，`switch(type)`
		// 也不需要多一个分支；而代理与浏览器都认注释帧是"连接还活着"。
		if ev.Type == "ping" {
			if _, err := fmt.Fprintf(c.Writer, ": ping %d\n\n", ev.MS); err != nil {
				return err
			}
			c.Writer.Flush()
			return nil
		}
		buf, err := json.Marshal(ev)
		if err != nil {
			return nil
		}
		if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", buf); err != nil {
			return err
		}
		c.Writer.Flush()
		return nil
	}

	// runCtx 就是请求 ctx：客户端断开时 runCtx 被取消，Run 会把已生成内容落库
	// （aborted）后收尾；落库与 finishRun 用的是 Run 内部的 persistCtx（WithoutCancel），
	// 不受断线影响 —— 所以这里不需要也不能 WithoutCancel（保留取消才能及时止损）。
	runCtx := c.Request.Context()
	err = agent.Run(runCtx, agent.RunInput{
		SessionID: sessionID, MessageID: userMsg.ID, AdminID: c.GetUint64("user_id"),
		UserText: userMsg.Content, ModelID: req.ModelID, DocIDs: req.DocIDs,
		Scope: req.Scope,
		// 装配只回灌 seq < UserSeq 的历史：这是并发写（锁未就绪时放行）的物理隔离，
		// 不依赖 Redis 锁是否真的生效。
		UserSeq: userMsg.Seq,
	}, emit)
	if err != nil {
		// Run 已经发过 error 事件；这里只补一个注释帧，避免代理把连接当异常。
		_, _ = fmt.Fprintf(c.Writer, ": run error: %s\n\n", clipLine(err.Error()))
		c.Writer.Flush()
	}
}

// clipLine 把错误文本压成单行（SSE 注释帧里出现换行会破坏帧结构）。
func clipLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	if r := []rune(s); len(r) > 300 {
		return string(r[:300])
	}
	return s
}

// AgentDryRun 处理 POST /api/admin/agent/dry-run（零成本探针）。
//
// 限流在路由层做（每管理员 6 次/分钟）：探针会出网，被脚本刷起来就是一个
// 打别人家的工具。这里不重复实现窗口，避免两处阈值不一致。
func AgentDryRun(c *gin.Context) {
	var req struct {
		AdapterID string          `json:"adapter_id"`
		BaseURL   string          `json:"base_url"`
		APIKey    string          `json:"api_key"`
		Spec      json.RawMessage `json:"spec"`
		Probe     string          `json:"probe"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	res, err := agent.DryRun(ctx, agent.DryRunInput{
		AdapterID: req.AdapterID, BaseURL: req.BaseURL, APIKey: req.APIKey,
		Spec: req.Spec, Probe: req.Probe,
	})
	if err != nil {
		switch {
		case errors.Is(err, agent.ErrDryRunAdapter), errors.Is(err, agent.ErrDryRunBlocked):
			response.FailDef(c, errs.AgentProposalState.WithMessage(err.Error()))
		default:
			response.FailDefMsg(c, errs.AgentDryRunFailed, err.Error())
		}
		return
	}
	response.OK(c, res)
}
