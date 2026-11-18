package router_test

// 本文件是**应用级隔离的门禁**：
//
// `/api/user/*` 的读写面本来是按 user_id 过滤的（门户要的是"我名下全部"），
// 于是"新增一个读端点"这个动作天然会继承整账号作用域 —— 它不会报错，只会
// 让应用 A 的托管密钥看到应用 B 的数据。`/api/user/apps` 就是这样漏掉的：
// 两套作用域判据（controller 的 keyScopeOf、relay 的 apiKeyOf+IsHosted）它都没用。
//
// 光修一次不够，必须让"漏声明"变成编译期之后的第一道红灯：任何新增的
// /api/user 路由只要没在下表里表态，这条用例就失败。

import (
	"sort"
	"strings"
	"testing"

	"lingdong-api/internal/router"
)

// scopeKind 是一条 /api/user 路由的数据作用域。
type scopeKind string

const (
	// scopeApp 表示必须收敛到「当前应用 / 当前 KEY」，不能用 user_id 一把梭。
	scopeApp scopeKind = "app"
	// scopeAccount 表示整账号可见是**设计意图**（账户资料、密钥管理、会话、MFA）。
	scopeAccount scopeKind = "account"
	// scopeNone 表示不涉及任何用户数据（验证码、注册、登录、找回密码）。
	scopeNone scopeKind = "none"
)

// appScopeDecl 是 /api/user/* 的作用域声明表。新增路由必须在这里加一行。
var appScopeDecl = map[string]scopeKind{
	// ---- 无凭证入口（不涉及用户数据）----
	"GET /api/user/captcha":                      scopeNone,
	"POST /api/user/register":                    scopeNone,
	"POST /api/user/register/contact-code":       scopeNone,
	"POST /api/user/password/reset/contact-code": scopeNone,
	"POST /api/user/login":                       scopeNone,
	"POST /api/user/login/mfa":                   scopeNone,
	"POST /api/user/password/reset/verify":       scopeNone,
	"POST /api/user/password/reset":              scopeNone,

	// ---- 只读账户面（会话票或 API KEY）----
	"GET /api/user/self":                      scopeAccount, // 账户额度；hosted 已脱敏 PII
	"GET /api/user/usage":                     scopeApp,     // hosted 覆盖 key_id
	"GET /api/user/usage/summary":             scopeApp,
	"GET /api/user/usage/export":              scopeApp,
	"GET /api/user/usage/:id":                 scopeApp,
	"GET /api/user/transactions":              scopeApp, // 按 app_id 收口
	"GET /api/user/packages":                  scopeAccount,
	"GET /api/user/orders":                    scopeApp,
	"GET /api/user/orders/:id":                scopeApp,
	"GET /api/user/orders/:id/qrcode":         scopeApp,
	"GET /api/user/tasks":                     scopeApp,
	"GET /api/user/tasks/:id":                 scopeApp,
	"GET /api/user/tasks/:id/contents/:index": scopeApp,
	"GET /api/user/files":                     scopeApp,
	"GET /api/user/apps":                      scopeApp, // hosted 时只返回本应用

	// ---- 写面（AuthWriter + scope）----
	"POST /api/user/orders":           scopeApp,
	"POST /api/user/redeem":           scopeAccount, // 兑换码充进共享余额，无 app 归属
	"POST /api/user/tasks/:id/cancel": scopeApp,
	"DELETE /api/user/files/:id":      scopeApp,

	// ---- 仅会话票（hosted 密钥不可达）----
	"POST /api/user/logout":                      scopeAccount,
	"POST /api/user/password/change":             scopeAccount,
	"POST /api/user/contact/code":                scopeAccount,
	"POST /api/user/contact/bind":                scopeAccount,
	"GET /api/user/redeems":                      scopeAccount,
	"GET /api/user/sessions":                     scopeAccount,
	"POST /api/user/sessions/revoke-all":         scopeAccount,
	"POST /api/user/sessions/:id/revoke":         scopeAccount,
	"GET /api/user/keys":                         scopeAccount,
	"POST /api/user/keys":                        scopeAccount,
	"PUT /api/user/keys/:id":                     scopeAccount,
	"PATCH /api/user/keys/:id/status":            scopeAccount,
	"POST /api/user/keys/:id/reset":              scopeAccount,
	"GET /api/user/keys/:id/plaintext":           scopeAccount,
	"DELETE /api/user/keys/:id":                  scopeAccount,
	"POST /api/user/apps/:slug/hosted-key":       scopeAccount, // 按目标 slug 签发，且仅会话票
	"GET /api/user/mfa/status":                   scopeAccount,
	"POST /api/user/mfa/setup":                   scopeAccount,
	"POST /api/user/mfa/enable":                  scopeAccount,
	"POST /api/user/mfa/disable":                 scopeAccount,
	"POST /api/user/mfa/backup-codes/regenerate": scopeAccount,

	// ---- 自举面（只认托管密钥，作用域=本 KEY）----
	"POST /api/user/key/rotate": scopeApp,
	"GET /api/user/key/health":  scopeApp,
}

// TestEveryUserRouteDeclaresAppScope 守住「新增读端点必须表态」。
//
// 为什么不用反射或标签：声明表本身就是文档 —— 读的人能一眼看到哪些端点是
// 账户级、哪些必须收口，而不是从 handler 里逐个推断。
func TestEveryUserRouteDeclaresAppScope(t *testing.T) {
	r := router.New()
	var missing []string
	for _, rt := range r.Routes() {
		if !strings.HasPrefix(rt.Path, "/api/user") {
			continue
		}
		key := rt.Method + " " + rt.Path
		if _, ok := appScopeDecl[key]; !ok {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("以下 /api/user 路由未声明数据作用域，请在 appScopeDecl 里补上（app=收敛到当前应用/KEY，account=整账号，none=无用户数据）：\n%s",
			strings.Join(missing, "\n"))
	}
}

// TestAppScopeDeclHasNoStaleEntries 反向校验：声明表里不该有已经被删掉的路由。
//
// 没有这条的话，端点删了而声明留着，表会慢慢变成一份"看起来很有安全感"的
// 死文档，而真正新增的端点反而混在里面看不出来。
func TestAppScopeDeclHasNoStaleEntries(t *testing.T) {
	live := map[string]struct{}{}
	for _, rt := range router.New().Routes() {
		live[rt.Method+" "+rt.Path] = struct{}{}
	}
	var stale []string
	for key := range appScopeDecl {
		if _, ok := live[key]; !ok {
			stale = append(stale, key)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Fatalf("appScopeDecl 里有已不存在的路由，请删除：\n%s", strings.Join(stale, "\n"))
	}
}
