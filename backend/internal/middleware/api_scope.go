package middleware

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/apicontract"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
)

// ScopeGuard 是 `/api/user/*` 的**契约守卫**：每个请求都必须能在
// apicontract 里找到自己的声明，并且凭证级别满足声明。
//
// 三条设计决定，都是刻意的：
//
//  1. **未声明即拒绝**（fail-closed）。新加一条路由却忘了写契约时，
//     正确表现是"这条端点 403"，而不是"它悄悄地按某种默认权限开放了"。
//     前者在联调时立刻被发现，后者可能上线很久才被人从外部打到。
//
//  2. **判定只看 context，不看请求头**。凭证识别是 UserTokenAuth /
//     SessionTokenAuth 的职责，本中间件只读它们写下的 `user_id` / `session_id`。
//     一旦这里自己解析 Authorization，就出现两处凭证判定，写反一处就是越权。
//
//  3. **必须挂在鉴权中间件之后**。挂在之前的话读不到身份，所有 reader 端点
//     都会 401，而 401 看起来像"凭证有问题"，排查方向会被带偏。
//
// 它不负责限流、不负责审计、不写响应头。
func ScopeGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		e, ok := apicontract.RuleFor(c.Request.Method, c.FullPath())
		if !ok && c.Request.Method == http.MethodHead {
			// HEAD 是 GET 的**传输变体**（同样的头、无响应体），契约里只声明一次 GET。
			// 不在这做这个回退，就必须给每条端点再写一份"只有方法不同"的声明 ——
			// 那份声明永远会和 GET 漂移，而漂移的后果是 HEAD 一律 403。
			e, ok = apicontract.RuleFor(http.MethodGet, c.FullPath())
		}
		if !ok {
			// 路由已注册但契约里没有它。这不是"猜一个级别放过去"的场景：
			// 契约缺失意味着没人评估过它的权限边界。
			response.FailDef(c, errs.ScopeUndeclared)
			return
		}

		switch e.Auth {
		case apicontract.AuthOpen:
			// 无凭证入口：不校验身份（带了凭证也不拒绝，便于客户端复用同一封装）。
			c.Next()

		case apicontract.AuthReader:
			// 双保险：正常情况下 UserTokenAuth 已经保证了 user_id 非零。
			// 仍然校验一次，是为了防"有人把这组挂到了没有鉴权中间件的父组上"。
			if c.GetUint64("user_id") == 0 {
				response.FailDef(c, errs.Unauthorized)
				return
			}
			c.Next()

		case apicontract.AuthWriter:
			if !writerAllowed(c, e.Scope) {
				// 与 SessionRequired 分开：这里要传达的是"换凭证也不行/或需要更高权限"，
				// 而不是"请改用登录会话"。两者混用会让接入方反复重试登录。
				response.FailDef(c, errs.ScopeDenied)
				return
			}
			c.Next()

		case apicontract.AuthSession:
			if c.GetUint64("user_id") == 0 {
				response.FailDef(c, errs.Unauthorized)
				return
			}
			if c.GetUint64("session_id") == 0 {
				// API KEY 打到"仅会话"端点：凭证本身可能完全有效，
				// 只是用错了地方。必须给出可分辨的码，客户端才知道该换凭证。
				response.FailDef(c, errs.SessionRequired)
				return
			}
			c.Next()

		case apicontract.AuthHostedKey:
			// 只接受托管密钥，且**不看 scope**。
			//
			// 不校验 scope 是刻意的：这两条端点（自举轮换、健康自检）操作的是
			// "调用方自己这张凭证"，不是平台数据。把它做成一个 scope 会引入
			// 版本依赖 —— 存量密钥的 scopes 冻结于签发时，新 scope 会让它们
			// 首次调用即 403，而它们恰恰是最需要能自举轮换的那批。
			if _, ok := HostedKeyFromCtx(c); !ok {
				response.FailDef(c, errs.ScopeDenied)
				return
			}
			c.Next()

		default:
			// 契约里写了未定义的级别（例如后续新增了 Auth 常量但忘了在这里处理）。
			// 与"未声明"同样按拒绝处理，理由相同。
			response.FailDef(c, errs.ScopeUndeclared)
		}
	}
}

// writerAllowed 判定当前请求能否执行**受限写操作**。
//
// 规则（两行，没有第三种情况）：
//
//	会话票                                  -> 允许（全权，与门户一致）
//	kind='hosted' 且 Scopes 含本次 scope     -> 允许
//	其它（含 kind='user' 的自建密钥）        -> 拒绝
//
// 为什么用户自建 KEY 永远不能写：它是一把长期有效、经常被贴进代码与配置的凭证。
// 门户里用户随时能复制、能回显、能贴到任何地方；让它能下单/取消任务/删文件，
// 等于一次泄露就能动钱和动数据。托管密钥不同 —— 它是应用后端**服务端**持有的，
// 由应用在用户授权后换取，权限集在签发时就固定下来（Scopes），可以只给"充值"
// 而不给"删文件"，并且可以单独轮换。
//
// 判定只读**上下文**里已有的实体，不重新查库：UserTokenAuth 已经把 MODEL 放进
// `api_key_record`，这里再查一次会出现"同一请求两个时刻的 KEY 状态"（中间被禁用
// 就产生时序漏洞），也会让每个写请求多一次 DB 往返。
//
// scope 为空时按**拒绝**处理：契约漏写 scope 是声明缺陷，不能因为漏写就把
// 端点变成"任意托管密钥都能调"。
func writerAllowed(c *gin.Context, scope string) bool {
	if c.GetUint64("session_id") != 0 {
		return true
	}
	if scope == "" {
		return false
	}
	v, ok := c.Get("api_key_record")
	if !ok {
		return false
	}
	k, ok := v.(*model.APIKey)
	if !ok || k == nil || !k.IsHosted() {
		return false
	}
	return keyHasScope(k.Scopes, scope)
}

// keyHasScope 判断托管密钥的 Scopes 是否包含指定权限点。
//
// 解析失败一律返回 false（零授权）。这条与 new-api 的
// `Scopes` JSON「解析失败 = 没有权限」一致：一个损坏的权限集
// 如果被当作"空集合所以不限制"，就是权限提升。
func keyHasScope(raw model.JSON, scope string) bool {
	if len(raw) == 0 {
		return false
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return false
	}
	for _, s := range list {
		if s == scope {
			return true
		}
	}
	return false
}
