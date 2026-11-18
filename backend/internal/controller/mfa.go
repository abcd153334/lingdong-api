package controller

import (
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/db"
	"lingdong-api/internal/middleware"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/authcookie"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/auth"
	"lingdong-api/internal/service/authevent"
	"lingdong-api/internal/service/mfa"
	"lingdong-api/internal/service/notify"
	"lingdong-api/internal/service/session"
)

// MFAStatus 处理 GET /api/portal/auth/mfa/status。
//
// 这个端点在「策略要求管理员绑定」时**必须放行**（见 middleware.AdminAuth 白名单）：
// 它正是被拦的管理员用来判断「我该不该去绑定」的唯一依据。
func MFAStatus(c *gin.Context) {
	uid := c.GetUint64("user_id")
	st, err := mfa.New(db.DB).Status(c.Request.Context(), uid)
	if err != nil {
		respondErr(c, err)
		return
	}
	response.OK(c, gin.H{
		"enabled":           st.Enabled,
		"policy":            st.Policy,
		"backup_codes_left": st.BackupCodesLeft,
	})
}

// MFASetup 处理 POST /api/portal/auth/mfa/setup：生成（或覆盖）未启用的密钥。
//
// 返回的 secret 是明文，**只此一次**：用户扫不出码时可以手抄，
// 之后库里只有密文，任何接口都不会再回显它。
func MFASetup(c *gin.Context) {
	req, ok := bindMFAReq(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	uid := c.GetUint64("user_id")
	if err := auth.New(db.DB).Reauth(ctx, uid, c.ClientIP(), req.Password); err != nil {
		respondErr(c, err)
		return
	}
	username := c.GetString("username")
	secret, uri, err := mfa.New(db.DB).Setup(ctx, uid, username, notify.SiteName())
	if err != nil {
		respondErr(c, err)
		return
	}
	authevent.Record(ctx, authevent.Event{
		UID: uid, Account: username, Event: authevent.EventMFASetup,
		Result: authevent.ResultSuccess, IP: c.ClientIP(),
	})
	response.OK(c, gin.H{"secret": secret, "otpauth_uri": uri})
}

// MFAEnable 处理 POST /api/portal/auth/mfa/enable {code}。
//
// 成功后返回备份码明文（同样只此一次），并踢掉**除当前**外的会话：
// 刚完成绑定就把自己踢下线体验极差，而其他设备可能正是被入侵的那台。
func MFAEnable(c *gin.Context) {
	req, ok := bindMFAReq(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	uid := c.GetUint64("user_id")
	if err := auth.New(db.DB).Reauth(ctx, uid, c.ClientIP(), req.Password); err != nil {
		respondErr(c, err)
		return
	}
	codes, err := mfa.New(db.DB).Enable(ctx, uid, req.Code)
	if err != nil {
		respondErr(c, err)
		return
	}
	_, _ = session.New(db.DB).RevokeAll(ctx, uid, model.SessionRevokeMFAChanged, middleware.SessionID(c))
	authevent.Record(ctx, authevent.Event{
		UID: uid, Account: c.GetString("username"), Event: authevent.EventMFAEnable,
		Result: authevent.ResultSuccess, IP: c.ClientIP(), SessionID: middleware.SessionID(c),
	})
	response.OK(c, gin.H{"backup_codes": codes})
}

// MFADisable 处理 POST /api/portal/auth/mfa/disable {code}。
//
// 关闭后撤销**全部**会话（含当前）并清 Cookie：关闭 2FA 是降低安全等级的操作，
// 让「可能已泄露的会话」继续活着，等于把这次降级也一起送给了攻击者。
func MFADisable(c *gin.Context) { mfaDisable(c, true) }

// UserMFADisable 是 /api/user/mfa/disable 的入口（不下发/清理 Cookie）。
// 理由同 UserResetPassword：`/api/user/*` 是不碰 Cookie 的一组。
func UserMFADisable(c *gin.Context) { mfaDisable(c, false) }

func mfaDisable(c *gin.Context, clearCookie bool) {
	var req mfaCodeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	ctx := c.Request.Context()
	uid := c.GetUint64("user_id")
	if err := mfa.New(db.DB).Disable(ctx, uid, req.Code); err != nil {
		respondErr(c, err)
		return
	}
	_, _ = session.New(db.DB).RevokeAll(ctx, uid, model.SessionRevokeMFAChanged, 0)
	if clearCookie {
		authcookie.Clear(c)
	}
	authevent.Record(ctx, authevent.Event{
		UID: uid, Account: c.GetString("username"), Event: authevent.EventMFADisable,
		Result: authevent.ResultSuccess, IP: c.ClientIP(), SessionID: middleware.SessionID(c),
	})
	response.OK(c, nil)
}

// MFABackupCodesRegenerate 处理 POST /api/portal/auth/mfa/backup-codes/regenerate {code}。
func MFABackupCodesRegenerate(c *gin.Context) {
	var req mfaCodeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	ctx := c.Request.Context()
	uid := c.GetUint64("user_id")
	codes, err := mfa.New(db.DB).Regenerate(ctx, uid, req.Code)
	if err != nil {
		respondErr(c, err)
		return
	}
	authevent.Record(ctx, authevent.Event{
		UID: uid, Account: c.GetString("username"), Event: authevent.EventBackupCodeUsed,
		Result: authevent.ResultSuccess, Reason: "regenerated", IP: c.ClientIP(),
	})
	response.OK(c, gin.H{"backup_codes": codes})
}

// mfaCodeReq 是「第二因素」的通用请求体：动态码或备份码，二者同一个输入框。
// bindMFAReq 解析「绑定 / 开启」的请求体，并挡掉空密码。
//
// 空密码不能走到密码比对里：那会把「参数没带」显示成「用户名或密码错误」（401），
// 前端看到 401 还会以为会话出了问题。缺参数就是 400。
func bindMFAReq(c *gin.Context) (mfaBindReq, bool) {
	var req mfaBindReq
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Password) == "" {
		response.FailDef(c, errs.InvalidParam)
		return req, false
	}
	return req, true
}

type mfaCodeReq struct {
	Code string `json:"code"`
}

// mfaBindReq 是「绑定 / 开启」两次请求的请求体，都要带当前账户密码。
//
// 为什么这两步要重认证：会话被窃取后，攻击者最值钱的动作就是「绑一个自己的
// 验证器」——那会把一次临时的会话劫持变成永久的账号接管（连改密都还要过他的码）。
// 关闭 / 重生成备份码已经要求动态码（本身就是第二因素），不需要再问密码。
type mfaBindReq struct {
	Code     string `json:"code"`
	Password string `json:"password"`
}
