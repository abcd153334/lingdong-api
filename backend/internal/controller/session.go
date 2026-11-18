package controller

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/db"
	"lingdong-api/internal/middleware"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/authcookie"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/authevent"
	"lingdong-api/internal/service/session"
)

// sessionItem 是设备列表的一项（**不包含**票据与哈希）。
type sessionItem struct {
	ID         uint64 `json:"id"`
	Current    bool   `json:"current"`
	IP         string `json:"ip"`
	UA         string `json:"ua"`
	CreatedAt  string `json:"created_at"`
	LastSeenAt string `json:"last_seen_at"`
	ExpiresAt  string `json:"expires_at"`
}

// ListSessions 处理 GET /api/portal/auth/sessions。
//
// 只回「能让人认出是哪台设备」的信息：IP、UA、创建与最近活跃时间。
// 绝不回票据或哈希 —— 设备列表的作用是让用户撤销，不是让他复制凭证。
func ListSessions(c *gin.Context) {
	uid := c.GetUint64("user_id")
	current := middleware.SessionID(c)
	rows, err := session.New(db.DB).List(c.Request.Context(), uid)
	if err != nil {
		respondErr(c, err)
		return
	}
	response.OK(c, gin.H{"sessions": sessionItems(rows, current)})
}

// sessionItems 是设备列表的唯一投影（门户与 `/api/user/sessions` 共用）。
//
// 必须是 make 出来的非 nil 切片：nil 会序列化成 JSON null，
// 前端对 null 调 .length 会整页崩（HTTP 200，后端日志干净）。
func sessionItems(rows []model.UserSession, current uint64) []sessionItem {
	out := make([]sessionItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, sessionItem{
			ID:         r.ID,
			Current:    r.ID == current,
			IP:         r.IP,
			UA:         r.UA,
			CreatedAt:  r.CreatedAt.Format(time.RFC3339),
			LastSeenAt: r.LastSeenAt.Format(time.RFC3339),
			ExpiresAt:  r.ExpiresAt.Format(time.RFC3339),
		})
	}
	return out
}

// RevokeSession 处理 POST /api/portal/auth/sessions/:id/revoke（Cookie 凭证）。
func RevokeSession(c *gin.Context) { revokeOwnedSession(c, true) }

// RevokeAllSessions 处理 POST /api/portal/auth/sessions/revoke-all（退出全部设备）。
func RevokeAllSessions(c *gin.Context) { revokeAllSessions(c, true) }

// revokeOwnedSession 是「撤销自己的某一台设备」的唯一实现。
//
// clearCookie 只在门户（凭证是 Cookie）为 true：开放 API 客户端没有 Cookie，
// 给它发 Set-Cookie 既无意义、又会让「一个 JSON 客户端被写进浏览器 Cookie」
// 这种莫名其妙的状态出现。
func revokeOwnedSession(c *gin.Context, clearCookie bool) {
	uid := c.GetUint64("user_id")
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	// 必须带 uid 条件：id 是自增的，只按 id 撤销等于允许任何登录用户
	// 踢掉别人的所有设备。
	n, err := session.New(db.DB).RevokeOwned(c.Request.Context(), uid, id, model.SessionRevokeUserRevoked)
	if err != nil {
		respondErr(c, err)
		return
	}
	if n == 0 {
		// 不属于自己 / 已失效：都按"不存在"处理，不区分（区分等于探测他人会话 id）。
		response.FailDef(c, errs.NotFound)
		return
	}
	authevent.Record(c.Request.Context(), authevent.Event{
		UID: uid, Event: authevent.EventSessionRevoke, Result: authevent.ResultSuccess,
		IP: c.ClientIP(), SessionID: id,
	})
	if clearCookie && id == middleware.SessionID(c) {
		// 撤销的是自己：顺手清 Cookie，前端不用再猜发生了什么。
		clearSessionCookie(c)
	}
	response.OK(c, nil)
}

// revokeAllSessions 是「退出全部设备」的唯一实现（两个凭证面共用）。
func revokeAllSessions(c *gin.Context, clearCookie bool) {
	uid := c.GetUint64("user_id")
	n, err := session.New(db.DB).RevokeAll(c.Request.Context(), uid, model.SessionRevokeUserRevoked, 0)
	if err != nil {
		respondErr(c, err)
		return
	}
	authevent.Record(c.Request.Context(), authevent.Event{
		UID: uid, Event: authevent.EventSessionRevokeAll, Result: authevent.ResultSuccess,
		IP: c.ClientIP(),
	})
	if clearCookie {
		clearSessionCookie(c)
	}
	response.OK(c, gin.H{"revoked": n})
}

// clearSessionCookie 封装 Cookie 清理（业务代码里只表达「清掉登录态」）。
func clearSessionCookie(c *gin.Context) {
	authcookie.Clear(c)
}
