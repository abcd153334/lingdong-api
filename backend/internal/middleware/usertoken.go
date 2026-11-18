package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/keyscope"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/apikey"
	"lingdong-api/internal/service/session"
)

// 本文件是 `/api/user/*`（对外开放用户 API）的鉴权实现。
//
// 与另外两个鉴权中间件的边界，必须一次说清，否则很容易被"顺手复用"成漏洞：
//
//	UserAuth()       只认 ld_session **Cookie**，整组挂 CSRF（浏览器门户 /api/portal/*）
//	TokenAuth()      只认 Authorization/x-api-key 的 **API KEY**，出 OpenAI 兼容错误体（/v1 网关）
//	UserTokenAuth()  认 Authorization 的 **会话票或 API KEY**，统一错误体（/api/user/*，本文件）
//
// 为什么 /api/user/* 不读 Cookie：这组端点**故意**没有 CSRF 中间件 ——
// 它的调用方是没有 Cookie 罐的脚本/第三方服务，Bearer 不会被浏览器自动附带，
// 天然没有 CSRF。一旦这里也接受 Cookie，同一个 handler 就出现了"浏览器可被
// 第三方页面驱动"的路径，而那条路径上没有任何 CSRF 校验。
// 所以：**`/api/user/*` 永远不读 Cookie**，读 Cookie 的端点一律留在 /api/portal/*。

// UserTokenAuth 校验 Authorization: Bearer <凭证>，接受两种凭证形态：
//
//	lds_...  会话票（POST /api/user/login 下发；浏览器登录时它就是 ld_session 的值）
//	ld-...   API KEY（网关同一把 KEY，用户可在 /api/user/keys 自助管理）
//
// 只给**读**端点用（self / 余额）。写操作与密钥管理必须走 SessionTokenAuth：
// API KEY 是一把长期有效、经常被贴进代码和配置文件的凭证，
// 让它可以改密码或再签发新 KEY，等于一次泄露永久接管账号。
func UserTokenAuth() gin.HandlerFunc { return userTokenAuth(true) }

// SessionTokenAuth 只接受会话票（lds_...）。API KEY 一律 403 session_required。
func SessionTokenAuth() gin.HandlerFunc { return userTokenAuth(false) }

// HostedKeyAuth 只接受 kind='hosted' 的 API KEY（且必须**当前可用**）。
//
// 用于"凭证操作它自己"的写端点（自举轮换）：目标行由命中的那把钥决定，
// 不接受任何入参指定，所以不存在"操作别人数据"的越权面。
//
// 与 SessionTokenAuth 正好互补，两者都不能替换对方：
//   - 会话票无法表达"我要操作哪一把钥"，而自举轮换必须锁死具体 key_id
//     （同一用户在不同应用下各有一把托管密钥）；
//   - 复用 UserTokenAuth（会话票也放行）等于让任意登录用户都能调自举轮换，
//     把 allow_self_issue 这个开关绕过去。
//
// 顺带把"这次鉴权是否走了重叠窗口的第二跳"写进上下文（key_via_prev）。
// 判定必须用**同一个** token 再算一次 Hash，而不是从 Authenticate 反推：
// 后者只知道 k.PrevKeyHash 存在，不知道这次命中的是哪一代。
func HostedKeyAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		release, ok := attachHostedKey(c, false)
		if !ok {
			return
		}
		// 与 UserTokenAuth 同构：并发闸必须等到响应写完才释放。
		defer release()
		c.Next()
	}
}

// HostedKeyInspectAuth 只接受托管密钥，但**不判断它还能不能用**。
//
// 仅给自检端点（GET /api/user/key/health）用：那个端点存在的意义就是回答
// "我为什么用不了"，若也要求"先能用"，它只在没病时可访问 —— 三个诊断字段
// （status / disabled_reason / allow_ips）就永远是"正常"值。
//
// 三条边界，缺一条就变成漏洞：
//   - 仍然必须是 kind='hosted'（自建 KEY 不参与托管生命周期）；
//   - **不建任何数据端点的鉴权**：只有健康自检挂它，别处用 HostedKeyAuth；
//   - 不续期、不刷 last_used_at（Inspect 不做这些），被禁的钥不会因为被看一眼而续命。
//
// 不计入该钥的 RPM/并发：应用打满额度时更需要能自检；这一组由独立的
// 限流桶（按 IP 与用户）兜底，防的是暴力猜钥，不是正常轮询。
func HostedKeyInspectAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, ok := attachHostedKey(c, true); !ok {
			return
		}
		c.Next()
	}
}

// attachHostedKey 是两种托管密钥鉴权的共同实现；inspect=true 时只验明文。
//
// 返回的 release 是 KEY 级并发闸的释放函数（inspect 路径是空函数）。
// 必须由调用方在 c.Next() 之后 defer —— 在函数内部 defer 会在返回时就释放。
func attachHostedKey(c *gin.Context, inspect bool) (func(), bool) {
	token := bearerToken(c)
	if token == "" || !apikey.IsKeyToken(token) {
		// 会话票与未知形态都按"没带对凭证"处理。不回 session_required ——
		// 那会暗示"换个凭证就行"，而换成会话票同样不行：这条端点的语义是
		// "用这把钥证明你是它"。
		response.FailDef(c, errs.Unauthorized)
		return func() {}, false
	}
	if inspect {
		k, err := apikey.Inspect(token)
		if err != nil {
			// 未知形态的明文（不存在的 hash）：与"凭证错"同码。
			// 存储错误已经由 ErrDef 归到 503，不会折成"你填错了"。
			response.FailDef(c, apikey.DefOr(err, errs.InvalidAPIKey))
			return func() {}, false
		}
		if !k.IsHosted() {
			response.FailDef(c, errs.ScopeDenied)
			return func() {}, false
		}
		if !keyscope.Consistent(k) {
			response.FailDef(c, errs.ScopeDenied)
			return func() {}, false
		}
		c.Set("api_key_record", k)
		c.Set("user_id", k.UserID)
		c.Set("key_id", k.ID)
		c.Set("key_via_prev", k.PrevKeyHash != "" && apikey.Hash(token) == k.PrevKeyHash)
		c.Set("auth_channel", "api-key")
		return func() {}, true
	}
	release, ok := attachKeyUser(c, token)
	if !ok {
		return func() {}, false
	}
	v, exists := c.Get("api_key_record")
	k, isKey := v.(*model.APIKey)
	if !exists || !isKey || k == nil || !k.IsHosted() {
		// 自建 KEY：凭证本身有效，只是用错了地方。
		// 必须与"未登录"给出不同的码，客户端才知道该换凭证而不是重新登录。
		response.FailDef(c, errs.ScopeDenied)
		return func() {}, false
	}
	c.Set("key_via_prev", k.PrevKeyHash != "" && apikey.Hash(token) == k.PrevKeyHash)
	return release, true
}

// HostedKeyFromCtx 取出本次请求鉴权时命中的那把托管密钥。
//
// 唯一实现：契约守卫（判断 AuthHostedKey 级别）与自检/轮换 handler 都调它。
// 曾经 controller 与 middleware 各有一个同名函数，判据（是否 IsHosted）写了两遍。
func HostedKeyFromCtx(c *gin.Context) (*model.APIKey, bool) {
	v, ok := c.Get("api_key_record")
	if !ok {
		return nil, false
	}
	k, ok := v.(*model.APIKey)
	if !ok || k == nil {
		return nil, false
	}
	return k, true
}

// userTokenAuth 是两种鉴权级别的共同实现。
func userTokenAuth(allowKey bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := bearerToken(c)
		if token == "" {
			// 不区分"没带"和"带错了"：401 的语义就是"请带上有效凭证"。
			response.FailDef(c, errs.Unauthorized)
			return
		}

		switch {
		case session.IsTicket(token):
			if !attachSessionUser(c, token) {
				return
			}
		case apikey.IsKeyToken(token):
			if !allowKey {
				// 凭证本身可能完全有效，只是**用错了地方**：
				// 必须给出与"未登录"不同的码，客户端才知道该换凭证而不是重新登录。
				response.FailDef(c, errs.SessionRequired)
				return
			}
			release, ok := attachKeyUser(c, token)
			if !ok {
				return
			}
			// 并发闸的释放必须等到响应写完（c.Next() 返回后），
			// 所以由这里 defer —— attachKeyUser 内部 defer 会在返回时就释放，
			// 闸等于没上。
			defer release()
		default:
			// 形态未知：不回 session_required（那会暗示"换个凭证就行"，
			// 而调用方多半是拼错了 Authorization 头）。
			response.FailDef(c, errs.Unauthorized)
			return
		}
		c.Next()
	}
}

// attachSessionUser 校验会话票并写入请求上下文；失败时已写响应。
func attachSessionUser(c *gin.Context, ticket string) bool {
	ctx := c.Request.Context()
	sess, err := session.New(db.DB).Authenticate(ctx, ticket)
	if err != nil {
		if def, ok := errs.AsDef(err); ok {
			response.FailDef(c, def)
		} else {
			response.FailDef(c, errs.Internal)
		}
		return false
	}
	var u model.User
	// 同 authenticate：鉴权热路径不取 avatar（几 KB 的 TOAST 读，用不上）。
	if err := db.DB.WithContext(ctx).Omit("avatar").First(&u, sess.UID).Error; err != nil {
		// 会话还在但用户没了：按会话失效处理（与门户 authenticate 一致）。
		response.FailDef(c, errs.SessionExpired)
		return false
	}
	if u.Status == 2 {
		response.FailDef(c, errs.Forbidden)
		return false
	}
	// 临时口令闸：/api/user/* 与门户共用同一份判据与白名单（auth.go）。
	// 不在这里拦，就等于「门户逼你改密、换条 Bearer 就能继续用」。
	if !mustChangeGate(c, &u) {
		return false
	}
	c.Set("user_id", u.ID)
	c.Set("role", int(u.Role))
	c.Set("username", u.Username)
	c.Set("session_id", sess.ID)
	c.Set("auth_channel", "session-token")
	// 续期与门户共用同一条节流路径：token 客户端同样是"活跃即续期"，
	// 否则 7 天绝对过期会让长期运行的脚本毫无预兆地掉线。
	session.New(db.DB).Touch(ctx, sess)
	return true
}

// attachKeyUser 校验 API KEY 并写入请求上下文；失败时已写响应。
//
// 刻意**不做**欠费闸（billing.ArrearsBlock）：这里读的是账户信息本身，
// 欠费用户更需要看到余额与欠款，把他挡在"查自己余额"的门外毫无意义。
// 欠费闸属于网关（/v1 的 TokenAuth），那里拒绝的是"继续消费"。
// 返回值 release 是 KEY 级并发闸的释放函数（未配并发上限时是空函数），
// 调用方必须在请求处理完成后调用它。
func attachKeyUser(c *gin.Context, token string) (func(), bool) {
	k, err := apikey.Authenticate(token)
	if err != nil {
		// 映射表只有 apikey.ErrDef 一份；认证面的兜底是 invalid_api_key
		// （这条路径上不该出现其它错误，存储错误已被包成 ErrAuthUnavailable）。
		response.FailDef(c, apikey.DefOr(err, errs.InvalidAPIKey))
		return func() {}, false
	}
	if !ipAllowed(k.AllowIPs, c.ClientIP()) {
		response.FailDef(c, errs.KeyIPDenied)
		return func() {}, false
	}
	// 作用域自洽性：托管密钥必须有归属应用。
	// 放行一把 `kind='hosted'` 且 app_id=0 的 KEY，会让下游所有
	// `if app_id != 0` 的过滤静默降级成「整个账号可见」—— fail-open，必须当场拒绝。
	if !keyscope.Consistent(k) {
		response.FailDef(c, errs.ScopeDenied)
		return func() {}, false
	}
	var u model.User
	if err := db.DB.WithContext(c.Request.Context()).Omit("avatar").First(&u, k.UserID).Error; err != nil {
		response.FailDef(c, errs.InvalidAPIKey)
		return func() {}, false
	}
	if u.Status == 2 {
		response.FailDef(c, errs.Forbidden)
		return func() {}, false
	}
	// 临时口令闸（与 attachSessionUser 同一判据）。放在 RPM/并发闸**之前**：
	// 这里还没占用任何计数，直接返回不需要清理。
	if !mustChangeGate(c, &u) {
		return func() {}, false
	}
	// KEY 级 RPM 与并发上限。
	//
	// 原先这里只做认证：同一把 KEY 在 `/v1` 被限到 60 rpm，在 `/api/user/*`
	// 却能打到用户维度的 600 rpm（10 倍），而契约与前端表单都没有"仅 /v1"
	// 的字样 —— 用户设了 60 却实际能打 600，属于承诺与实现不一致。
	// 两个闸的键都是 **KEY 维度**（ld:rl:key:<id>:<分> / ld:cc:key:<id>），
	// 与 /v1 完全同一套，所以在哪儿判定都是同一份计数。
	if !rpmAllowed(k) {
		response.FailDef(c, errs.RateLimited)
		return func() {}, false
	}
	release, ok := acquireConcurrency(k)
	if !ok {
		response.FailDef(c, errs.RateLimited)
		return func() {}, false
	}
	c.Set("user_id", k.UserID)
	c.Set("role", int(u.Role))
	c.Set("username", u.Username)
	// session_id 保持 0：API KEY 请求没有"当前会话"，
	// 依赖它的端点（设备列表里标 current、登出、改密）都必须走 SessionTokenAuth。
	c.Set("key_id", k.ID)
	// 与 TokenAuth 一致地把 KEY 实体放进上下文：/api/user/self 要读 AllowUserQuotaQuery
	// （建 KEY 时勾掉的"允许查询账户额度"必须真的生效，否则那个开关只是装饰）。
	c.Set("api_key_record", k)
	c.Set("auth_channel", "api-key")
	return release, true
}

// BearerToken 返回 Authorization 头里的凭证（未携带为空串）。
//
// 导出的用途仅限一处：`POST /api/user/login` 要在"客户端带着旧票重新登录"时
// 把那张票当作被取代的会话撤掉（会话固定防护）。**不要**在业务 handler 里
// 用它做鉴权判断 —— 鉴权只发生在中间件。
func BearerToken(c *gin.Context) string { return strings.TrimSpace(bearerToken(c)) }
