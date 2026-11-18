package controller

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/middleware"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/authcookie"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/money"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/apikey"
	"lingdong-api/internal/service/auth"
	"lingdong-api/internal/service/authcfg"
	"lingdong-api/internal/service/authevent"
	"lingdong-api/internal/service/captcha"
	"lingdong-api/internal/service/pricing"
	"lingdong-api/internal/service/session"
	"lingdong-api/internal/service/verify"
)

// respondErr 把服务层错误映射为统一错误响应。
//
// 必须用 errs.AsDef（errors.As）而不是类型断言：任何被包装过的 errs.Def
// （fmt.Errorf("%w")、事务回滚时的包装）在类型断言下都会退化成 500，
// 于是"用户名已存在"变成"服务器内部错误"。
func respondErr(c *gin.Context, err error) {
	if def, ok := errs.AsDef(err); ok {
		response.FailDef(c, def)
		return
	}
	response.FailDef(c, errs.Internal)
}

// ---- 注册 ----

type registerReq struct {
	Username        string `json:"username"`
	Channel         string `json:"channel"`
	Target          string `json:"target"`
	Code            string `json:"code"`
	Password        string `json:"password"`
	ConfirmPassword string `json:"confirm_password"`
	AffCode         string `json:"aff_code"`
	Agreed          bool   `json:"agreed"`
	// 图形验证码：先 GET /api/portal/auth/captcha?scene=register 拿 {id, image}，
	// 用户填图上的 5 位数字后，把 id 与答案一起回传。
	CaptchaID     string `json:"captcha_id"`
	CaptchaAnswer string `json:"captcha_answer"`
}

// Register 处理 POST /api/portal/auth/register。
//
// 注册成功**不自动登录**：多一步登录换来的是一条清晰的边界
// （注册只负责建号，登录只负责建会话），也避免「批量注册后立刻可调用」。
func Register(c *gin.Context) {
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
	// 邀请绑定：非法邀请码 / 返利关闭时静默忽略，不影响注册结果。
	bindAffiliate(c.Request.Context(), u.ID, req.AffCode)
	response.OK(c, gin.H{"id": u.ID, "username": u.Username})
}

type contactCodeReq struct {
	Channel string `json:"channel"`
	Target  string `json:"target"`
	// 找回密码的发码端点要图形验证码；注册发码不需要（见 RegisterContactCode）。
	CaptchaID     string `json:"captcha_id"`
	CaptchaAnswer string `json:"captcha_answer"`
}

// RegisterContactCode 处理 POST /api/portal/auth/register/contact-code。
func RegisterContactCode(c *gin.Context) {
	var req contactCodeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	channel := normalChannel(req.Channel)

	// 策略闸门：与注册提交（service/auth 的 MethodEnabled 判定）必须一致。
	// 少了这一步，运营在后台取消勾选某个渠道后，这个端点仍能真的发出短信 ——
	// 既产生费用，也让「关掉的渠道」变成一条可用的滥用通道。
	// 必须放在 verify.Issue **之前**：Issue 会写冷却键与验证码，策略不允许时
	// 不能留下任何副作用（否则用户回头改用合法渠道时会先撞 60 秒冷却）。
	if !authcfg.Register().MethodEnabled(channel) {
		response.FailDef(c, errs.BindMethodDisabled)
		return
	}

	// 这里**不要求**图形验证码。
	//
	// 原因不是"省一道闸"，而是图形码一次性的：发码用掉一张，注册提交就必然
	// 拿不到，用户得填两次。发码端点已有的防护是「同收件人 60 秒冷却 +
	// 同 IP 每小时 20 次 + 整组 60rpm/IP」（service/verify），它不产生账号，
	// 只花一条短信；真正的闸门在 POST /register。

	expiresIn, err := verify.Issue(c.Request.Context(), verify.IssueInput{
		Scene: verify.SceneRegister, Channel: channel, Target: req.Target, IP: c.ClientIP(),
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	response.OK(c, gin.H{"expires_in": expiresIn})
}

// ---- 登录 / 登出 ----

type loginReq struct {
	Account  string `json:"account"`
	Password string `json:"password"`
	// 图形验证码；只有策略要求时才需要（after_fails 达阈值 / always）。
	CaptchaID     string `json:"captcha_id"`
	CaptchaAnswer string `json:"captcha_answer"`
}

// Login 处理 POST /api/portal/auth/login。
func Login(c *gin.Context) {
	var req loginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	res, err := auth.New(db.DB).Login(c.Request.Context(), auth.LoginInput{
		Account:       req.Account,
		Password:      req.Password,
		IP:            c.ClientIP(),
		UA:            c.Request.UserAgent(),
		CaptchaID:     req.CaptchaID,
		CaptchaAnswer: req.CaptchaAnswer,
		// 请求携带的旧票（会话固定防护）。这里**不能**用 middleware.SessionID(c)：
		// 登录路由在公开组，没有 UserAuth，那个值恒为 0。
		CurrentTicket: authcookie.Ticket(c),
	})
	if err != nil {
		respondErr(c, err)
		return
	}

	// 第一步只回挑战，**不发任何 Cookie**：2FA 未完成就发凭证，
	// 等于把「第二因素」降级成一个可以跳过的提示。
	if res.MFARequired {
		response.OK(c, gin.H{
			"mfa_required": true,
			"challenge":    res.Challenge,
			"expires_in":   res.ExpiresIn,
		})
		return
	}
	mfaSetCookies(c, res)
}

// mfaSetCookies 统一发会话 Cookie 并回用户（登录两步共用）。
//
// 门户端关闭时这里是**唯一**能拿到门户会话的地方（会话层闸门只挡已有凭证），
// 所以「只有管理员能登录」的判定必须落在这里。
func mfaSetCookies(c *gin.Context, res *auth.LoginResult) {
	// 非管理员在门户关闭时不许拿到凭证。放在发券点而不是校验密码之前：
	// 凭据阶段按角色拒绝，等于给登录页一个「这个账号是不是管理员」的探测口。
	if int(res.User.Role) != 0 && !conf.PortalEnabled() {
		// 会话已经在 auth.Login/LoginMFA 里落库了：不撤销的话每拒绝一次就多一条
		// 活会话（设备列表里出现从未发出去的「幽灵设备」，还占 max_active 名额）。
		_ = session.New(db.DB).Revoke(c.Request.Context(), res.Session.ID, model.SessionRevokePortalClosed)
		response.FailDef(c, errs.PortalDisabled)
		return
	}
	ttl := int(authcfg.Session().AbsoluteTTL.Seconds())
	authcookie.SetSession(c, res.Ticket, res.CSRF, ttl)
	response.OK(c, gin.H{
		"user":       auth.ViewOf(res.User),
		"expires_in": ttl,
	})
}

// LoginMFA 处理 POST /api/portal/auth/login/mfa：登录第二步。
//
// 挑战令牌是一次性的、绑出口 IP 的，且试错次数在 Redis 里原子累加 ——
// 6 位码只有 5 次机会，且不能换 IP 分散爆破。
func LoginMFA(c *gin.Context) {
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
	mfaSetCookies(c, res)
}

// Logout 处理 POST /api/portal/auth/logout：撤销当前会话并清除 Cookie。
func Logout(c *gin.Context) {
	ctx := c.Request.Context()
	uid := c.GetUint64("user_id")
	sid := middleware.SessionID(c)
	if sid != 0 {
		_ = session.New(db.DB).Revoke(ctx, sid, model.SessionRevokeLogout)
	}
	authcookie.Clear(c)
	authevent.Record(ctx, authevent.Event{
		UID: uid, Event: authevent.EventLogout, Result: authevent.ResultSuccess,
		IP: c.ClientIP(), SessionID: sid,
	})
	response.OK(c, nil)
}

// ---- 密码 ----

// loginMFAReq 是登录第二步的请求体。
type loginMFAReq struct {
	Challenge string `json:"challenge"`
	Code      string `json:"code"`
}

type changePasswordReq struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

// ChangePassword 处理 POST /api/portal/auth/password/change。
//
// 成功后该用户全部会话失效（含当前）：改密的动机通常就是"怀疑凭证泄露"，
// 留一个旧会话等于没改。
func ChangePassword(c *gin.Context) {
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
	authcookie.Clear(c)
	authevent.Record(c.Request.Context(), authevent.Event{
		UID: uid, Event: authevent.EventPasswordChange, Result: authevent.ResultSuccess,
		IP: c.ClientIP(), SessionID: middleware.SessionID(c),
	})
	response.OK(c, nil)
}

// ResetContactCode 处理 POST /api/portal/auth/password/reset/contact-code。
//
// **枚举防护**：账号不存在时也返回 200，且写入同样的限流键。
// 若不存在分支什么都不写，攻击者用「连续发两次，第二次是否 429」
// 就能判断某个邮箱是否注册过。
func ResetContactCode(c *gin.Context) {
	var req contactCodeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	channel := normalChannel(req.Channel)
	ctx := c.Request.Context()

	// 找回密码的发码端点**恒要求**图形验证码：它没有后续的"提交建号"环节，
	// 发码就是整个流程的闸门。
	if err := captcha.Verify(ctx, req.CaptchaID, req.CaptchaAnswer); err != nil {
		respondErr(c, err)
		return
	}

	target := verify.Normalize(channel, req.Target)
	if err := verify.ValidateFormat(channel, target); err != nil {
		respondErr(c, err)
		return
	}
	// 渠道未就绪时明确报错，不假装已发送。
	if !verify.Ready(channel) {
		response.FailDef(c, errs.BindMethodDisabled)
		return
	}

	rp := authcfg.Register()
	if !rp.MethodEnabled(channel) {
		response.FailDef(c, errs.BindMethodDisabled)
		return
	}

	var u model.User
	col := "email"
	if channel == verify.ChannelSMS {
		col = "phone"
	}
	err := db.DB.WithContext(ctx).Where(col+" = ?", target).First(&u).Error
	if err != nil || u.ID == 0 || u.Status == 2 {
		// 不存在或已禁用：与成功分支完全等时（写同样的限流键），恒返回 200。
		_ = verify.MarkSent(ctx, verify.IssueInput{
			Scene: verify.SceneReset, Channel: channel, Target: target, IP: c.ClientIP(),
		})
		response.OK(c, gin.H{"expires_in": int((10 * time.Minute).Seconds())})
		return
	}

	expiresIn, err := verify.Issue(ctx, verify.IssueInput{
		Scene: verify.SceneReset, Channel: channel, Target: target, UID: u.ID, IP: c.ClientIP(),
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	response.OK(c, gin.H{"expires_in": expiresIn})
}

type resetVerifyReq struct {
	Channel string `json:"channel"`
	Target  string `json:"target"`
	Code    string `json:"code"`
}

// ResetVerify 处理 POST /api/auth/password/reset/verify：校验验证码并换一次性凭据。
//
// **只校验不消费**：用户还需要在第 3 步设置新密码，若这一步就烧掉验证码，
// 密码不符合强度要求时用户必须重新等 60 秒收码。
func ResetVerify(c *gin.Context) {
	var req resetVerifyReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	channel := normalChannel(req.Channel)
	ctx := c.Request.Context()

	if err := verify.Consume(ctx, verify.SceneReset, channel, req.Target, req.Code); err != nil {
		respondErr(c, err)
		return
	}
	target := verify.Normalize(channel, req.Target)
	col := "email"
	if channel == verify.ChannelSMS {
		col = "phone"
	}
	var u model.User
	if err := db.DB.WithContext(ctx).Where(col+" = ?", target).First(&u).Error; err != nil {
		// 验证码对但账号刚被删：按凭据无效处理，不泄露"账号曾经存在"。
		response.FailDef(c, errs.VerifyCodeInvalid)
		return
	}
	ticket, expiresIn, err := verify.IssueTicket(ctx, u.ID)
	if err != nil {
		respondErr(c, err)
		return
	}
	response.OK(c, gin.H{"reset_ticket": ticket, "expires_in": expiresIn})
}

type resetPasswordReq struct {
	ResetTicket string `json:"reset_ticket"`
	NewPassword string `json:"new_password"`
}

// ResetPassword 处理 POST /api/portal/auth/password/reset。
func ResetPassword(c *gin.Context) { resetPassword(c, true) }

// UserResetPassword 是 /api/user/password/reset 的入口。
//
// 与门户的差别只有一处：**不下发/清理 Cookie**。`/api/user/*` 全组不解析
// 也不写 Cookie（凭证只走 Authorization），一条端点擅自 Set-Cookie 会破坏
// 这个静态不变式 —— 今天清的是自己的 cookie，明天有人把 SetSession 挪进
// 共用 handler 时就没有任何机制能拦住。
// 先例：session.revokeOwnedSession(c, clearCookie bool)，用户面传 false。
func UserResetPassword(c *gin.Context) { resetPassword(c, false) }

func resetPassword(c *gin.Context, clearCookie bool) {
	var req resetPasswordReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	ctx := c.Request.Context()
	uid, err := verify.RedeemTicket(ctx, req.ResetTicket)
	if err != nil {
		respondErr(c, err)
		return
	}
	if err := auth.New(db.DB).ResetPassword(ctx, uid, req.NewPassword); err != nil {
		respondErr(c, err)
		return
	}
	if clearCookie {
		authcookie.Clear(c)
	}
	authevent.Record(ctx, authevent.Event{
		UID: uid, Event: authevent.EventPasswordReset, Result: authevent.ResultSuccess,
		IP: c.ClientIP(),
	})
	response.OK(c, nil)
}

// ---- 联系方式 ----

// ContactCode 处理 POST /api/portal/auth/contact/code（已登录换绑发码）。
func ContactCode(c *gin.Context) {
	var req contactCodeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	expiresIn, err := auth.New(db.DB).SendContactCode(c.Request.Context(),
		c.GetUint64("user_id"), normalChannel(req.Channel), req.Target, c.ClientIP())
	if err != nil {
		respondErr(c, err)
		return
	}
	response.OK(c, gin.H{"expires_in": expiresIn})
}

type contactBindReq struct {
	Channel string `json:"channel"`
	Target  string `json:"target"`
	Code    string `json:"code"`
}

// ContactBind 处理 POST /api/portal/auth/contact/bind。
func ContactBind(c *gin.Context) {
	var req contactBindReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	email, phone, revokedKeys, err := auth.New(db.DB).BindContact(c.Request.Context(),
		c.GetUint64("user_id"), middleware.SessionID(c),
		normalChannel(req.Channel), req.Target, req.Code)
	if err != nil {
		respondErr(c, err)
		return
	}
	// revoked_keys 必须回给调用方：换绑会禁用全部 API KEY（账号接管的止血动作），
	// 应用端若不知道这件事，会以为"接口成功所以我的密钥还能用"，然后在下次调用
	// 收到 401 时把它当成服务端故障。数字是 0 时前端不必提示。
	response.OK(c, gin.H{"email": email, "phone": phone, "revoked_keys": revokedKeys})
}

// ---- 当前用户 ----

// Me 处理 GET /api/portal/me。
func Me(c *gin.Context) {
	uid := c.GetUint64("user_id")
	var u model.User
	if err := db.DB.First(&u, uid).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 会话还在但用户已被删除：这不是 500，而是「这个会话已经没有主人」。
			// 返回 500 会让前端只看到「请求失败」，用户反复刷新也回不到登录页；
			// 按会话失效处理并清 Cookie，前端据此正常回到登录。
			authcookie.Clear(c)
			response.FailDef(c, errs.SessionExpired)
			return
		}
		respondErr(c, err)
		return
	}
	payload, err := selfPayload(c.Request.Context(), &u)
	if err != nil {
		respondErr(c, err)
		return
	}
	response.OK(c, payload)
}

// selfPayload 是「当前用户」的唯一投影（`/api/portal/me` 与 `/api/user/self` 共用）。
//
// 为什么必须共用而不是各写一份：两份投影的下场是字段漂移 ——
// 门户加了 group_discount、开放 API 还停在旧字段，第三方客户端算出来的折扣
// 与服务端计费不一致，而两边"看起来都对"。
func selfPayload(ctx context.Context, u *model.User) (gin.H, error) {
	var keyCount, keyEnabled int64
	if err := db.DB.WithContext(ctx).Model(&model.APIKey{}).Where("user_id = ?", u.ID).Count(&keyCount).Error; err != nil {
		return nil, err
	}
	// key_count_enabled 单独统计：key_count 是「名下全部密钥」，含已停用的。
	//
	// 为什么不能直接把 key_count 改成「启用数」：`/api/user/self` 是**对外开放的契约**，
	// 第三方客户端已经按「名下密钥数量」在展示，改语义等于让它静默变形。
	// 所以两个数都下发，由展示层决定用哪个 —— 门户卡片显示「N 个启用 · 共 M 个」。
	if err := db.DB.WithContext(ctx).Model(&model.APIKey{}).
		Where("user_id = ? AND status = ?", u.ID, apikey.KeyStatusEnabled).
		Count(&keyEnabled).Error; err != nil {
		return nil, err
	}

	// 分组折扣单值（G1）：公开 /api/pricing 已裁剪 group_discounts 全表，
	// 门户需要「我的折扣」时从这里拿自己的那一档。
	// 口径与计费完全一致（pricing.Snapshot.GroupDiscount）：未配置 = 不打折（= scale），
	// 并标记 group_configured=false 供前端提示「未享折扣」。
	groupDiscount, groupConfigured := money.Scale, false
	if cat, err := pricing.Get(); err == nil {
		if d, ok := cat.GroupDiscounts[u.Group]; ok && d > 0 {
			groupDiscount, groupConfigured = d, true
		}
	}

	return gin.H{
		"id":       u.ID,
		"username": u.Username,
		"role":     int(u.Role),
		"status":   int(u.Status),
		"group":    u.Group,
		// group_discount 是**微比例**（1e6 = 不打折），前端用 scale 换算成倍数显示。
		"group_discount":    groupDiscount,
		"group_configured":  groupConfigured,
		"balance":           u.Balance,
		"frozen":            u.Frozen,
		"used":              u.Used,
		"request_count":     u.RequestCount,
		"key_count":         keyCount,
		"key_count_enabled": keyEnabled,
		"email":             u.Email,
		"phone":             u.Phone,
		// 头像：`data:image/webp;base64,...`，空串 = 未设置（前端渲染打包进产物的默认头像）。
		// 恒下发空串而不是省略/ null：前端 `<img src={null}>` 会真的发一次 "null" 请求。
		"avatar_url": u.Avatar,
		// 临时口令未改：门户据此把 /settings 收窄成「只改密」（其余端点此刻恒 403），
		// 开放 API 据此知道为什么自己调什么都被拒。判据与中间件同一字段，
		// 前端不得自己推出"什么算初始口令"。
		"must_change_password": u.MustChangePassword,
	}, nil
}

// normalChannel 归一化渠道名：**只补空值**，其余原样返回。
//
// 不能把无法识别的值悄悄折成 email：那会让「渠道拼错」表现为「用邮箱发」，
// 用户看到的是「邮箱格式不正确」而不是「渠道不存在」，排查方向直接跑偏；
// 同理 `phone` 也不该被收编成 email（前端只发 email/sms，真要支持就得改契约枚举）。
// 非法值交由下游 MethodEnabled / Ready 判定，返回 bind_method_disabled。
func normalChannel(ch string) string {
	s := strings.ToLower(strings.TrimSpace(ch))
	if s == "" {
		return verify.ChannelEmail // 缺省按邮箱：兼容不传 channel 的老调用
	}
	return s
}
