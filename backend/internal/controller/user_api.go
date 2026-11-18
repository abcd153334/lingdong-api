package controller

// 本文件是**对外开放用户 API**（`/api/user/*`）的控制器。
//
// 它与 `/api/portal/*` 的关系，一句话：**同一套 service，不同的凭证面**。
//   - `/api/portal/*`：浏览器，凭证是 HttpOnly Cookie，整组挂 CSRF 双提交；
//   - `/api/user/*`  ：脚本/第三方服务，凭证是 `Authorization: Bearer`，不读 Cookie。
//
// 因此这里刻意**没有一行是"另一套业务逻辑"**：注册走 auth.Register、登录走
// auth.Login、改密走 auth.ChangePassword、设备走 session.Service。这里只负责
// 「把凭证换成响应体的 token、把错误换成统一错误体」。
//
// 为什么不在门户 handler 里加一个 `if 是 API 请求 { 回 token } else { 发 Cookie }`：
// 那种写法会让「发不发 Cookie」变成一个运行期分支，而 CSRF 是**路由组的静态属性**。
// 分支一旦写反，浏览器拿到的 Cookie 就出现在一个没有 CSRF 校验的组里。

import (
	"github.com/gin-gonic/gin"

	"lingdong-api/internal/db"
	"lingdong-api/internal/middleware"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/keyscope"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/auth"
	"lingdong-api/internal/service/authcfg"
	"lingdong-api/internal/service/authevent"
	"lingdong-api/internal/service/session"
)

// UserRegister 处理 POST /api/user/register。
//
// 语义与 `/api/portal/auth/register` 完全一致（同一 service），区别只有两点：
// 验证码 id 可以从请求体传（无 Cookie 罐），且**响应里没有任何 Cookie**。
func UserRegister(c *gin.Context) {
	var req registerReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	u, err := auth.New(db.DB).Register(c.Request.Context(), auth.RegisterInput{
		Username:        req.Username,
		Channel:         req.Channel,
		Target:          req.Target,
		Code:            req.Code,
		Password:        req.Password,
		ConfirmPassword: req.ConfirmPassword,
		AffCode:         req.AffCode,
		Agreed:          req.Agreed,
		CaptchaID:       req.CaptchaID,
		CaptchaAnswer:   req.CaptchaAnswer,
		IP:              c.ClientIP(),
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	bindAffiliate(c.Request.Context(), u.ID, req.AffCode)
	response.OK(c, gin.H{"id": u.ID, "username": u.Username})
}

// UserLogin 处理 POST /api/user/login：**只在响应体里回 token，不发 Cookie**。
//
// 这既是它存在的理由，也是它最大的风险，必须写清楚：
//   - 响应体里的 token 是明文，JS 可读（不像 HttpOnly Cookie）。所以**浏览器页面
//     绝不应该用它** —— 门户前端必须继续走 `/api/portal/auth/login`。
//   - 它面向的是 CLI、移动端、第三方后端，这些客户端本来就没有 HttpOnly 的容器。
//
// 会话固定防护仍然成立：若客户端带着旧票（`Authorization: Bearer lds_...`）重新登录，
// 旧会话会在新会话建立时被撤销 —— 与 Cookie 路径共用 `auth.Login` 的同一段逻辑。
func UserLogin(c *gin.Context) {
	var req loginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	// 只把会话票当作"旧票"：Bearer 里若是一把 API KEY，它不是会话，谈不上取代。
	oldTicket := middleware.BearerToken(c)
	if !session.IsTicket(oldTicket) {
		oldTicket = ""
	}
	res, err := auth.New(db.DB).Login(c.Request.Context(), auth.LoginInput{
		Account:       req.Account,
		Password:      req.Password,
		IP:            c.ClientIP(),
		UA:            c.Request.UserAgent(),
		CaptchaID:     req.CaptchaID,
		CaptchaAnswer: req.CaptchaAnswer,
		CurrentTicket: oldTicket,
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	respondUserToken(c, res)
}

// UserLoginMFA 处理 POST /api/user/login/mfa：登录第二步（同 Cookie 路径的语义）。
func UserLoginMFA(c *gin.Context) {
	var req loginMFAReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	res, err := auth.New(db.DB).LoginMFA(c.Request.Context(), req.Challenge, req.Code,
		c.ClientIP(), c.Request.UserAgent())
	if err != nil {
		respondErr(c, err)
		return
	}
	respondUserToken(c, res)
}

// respondUserToken 是 token 形态登录的统一出口。
//
// `csrf_token` 刻意不下发：它对 Bearer 客户端没有任何用处（CSRF 只与
// "浏览器自动附带凭证"有关），发出去只会诱使有人把它当第二个密钥存起来。
func respondUserToken(c *gin.Context, res *auth.LoginResult) {
	if res.MFARequired {
		// 与门户一致：2FA 未完成时**不发放任何凭证**，只回挑战。
		response.OK(c, gin.H{
			"mfa_required": true,
			"challenge":    res.Challenge,
			"expires_in":   res.ExpiresIn,
		})
		return
	}
	ttl := int(authcfg.Session().AbsoluteTTL.Seconds())
	response.OK(c, gin.H{
		"token":      res.Ticket,
		"token_type": "Bearer",
		"expires_in": ttl,
		"user":       auth.ViewOf(res.User),
	})
}

// UserLogout 处理 POST /api/user/logout：撤销**当前这张票**。
//
// 只能拿到会话票的请求（SessionTokenAuth），所以这里不会有 API KEY 走空的情况。
func UserLogout(c *gin.Context) {
	ctx := c.Request.Context()
	uid := c.GetUint64("user_id")
	sid := middleware.SessionID(c)
	if sid != 0 {
		_ = session.New(db.DB).Revoke(ctx, sid, model.SessionRevokeLogout)
	}
	// 不清会话 Cookie：这条路径不读 Cookie，清它反而会误伤
	// 同一浏览器上的门户会话（用户可能只是想注销脚本那把票）。
	authevent.Record(ctx, authevent.Event{
		UID: uid, Event: authevent.EventLogout, Result: authevent.ResultSuccess,
		IP: c.ClientIP(), SessionID: sid,
	})
	response.OK(c, nil)
}

// UserSelf 处理 GET /api/user/self：账户概览（与门户 /me 同一投影）。
func UserSelf(c *gin.Context) {
	// 用 API KEY 访问时，这里读的是**账户级**余额，必须尊重建 KEY 时的
	// 「允许查询用户额度」开关。整个端点就是账户，所以只能直接拒；
	// 只读列表类端点则不拒，改为把余额字段置零（见 PortalTransactionList）。
	if !quotaVisible(c) {
		response.FailDef(c, errs.Forbidden.WithMessage("该 API KEY 未开启账户额度查询权限"))
		return
	}
	uid := c.GetUint64("user_id")
	var u model.User
	if err := db.DB.WithContext(c.Request.Context()).First(&u, uid).Error; err != nil {
		response.FailDef(c, errs.SessionExpired)
		return
	}
	payload, err := selfPayload(c.Request.Context(), &u)
	if err != nil {
		respondErr(c, err)
		return
	}
	// 托管密钥代表的是**应用**，不是用户本人。
	//
	// 余额/冻结必须保留：应用要靠它提示用户充值，拿掉等于让接入方无法判断
	// "还能不能用"。但邮箱/手机号/头像属于 PII，不该因为一把应用密钥泄露
	// 就跟着泄露 —— 那会把"接口凭证泄露"升级成"用户身份泄露"。
	// 会话票与自建 KEY 不受影响（同一个投影，门户仍要展示完整资料）。
	if s, _ := keyscope.Of(c); s.Hosted {
		payload["email"] = ""
		payload["phone"] = ""
		payload["avatar_url"] = ""
	}
	// 让调用方知道"这次用的是哪一类凭证"（读能力一致，写能力不同）。
	payload["auth_channel"] = c.GetString("auth_channel")
	response.OK(c, payload)
}

// UserChangePassword 处理 POST /api/user/password/change（仅会话票）。
//
// 成功后该用户**全部会话失效**（含当前）：改密的动机通常就是怀疑凭证泄露，
// 留一张旧票等于没改。客户端应当提示用户重新登录。
func UserChangePassword(c *gin.Context) {
	var req changePasswordReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	uid := c.GetUint64("user_id")
	if err := auth.New(db.DB).ChangePassword(c.Request.Context(), uid, c.ClientIP(),
		req.OldPassword, req.NewPassword); err != nil {
		respondErr(c, err)
		return
	}
	authevent.Record(c.Request.Context(), authevent.Event{
		UID: uid, Event: authevent.EventPasswordChange, Result: authevent.ResultSuccess,
		IP: c.ClientIP(), SessionID: middleware.SessionID(c),
	})
	response.OK(c, gin.H{"revoked_all": true})
}

// UserRevokeSession 处理 POST /api/user/sessions/:id/revoke（token 凭证，不动 Cookie）。
func UserRevokeSession(c *gin.Context) { revokeOwnedSession(c, false) }

// UserRevokeAllSessions 处理 POST /api/user/sessions/revoke-all（token 凭证）。
func UserRevokeAllSessions(c *gin.Context) { revokeAllSessions(c, false) }
