// Package apicontract 是**对外开放用户 API**（`/api/user/*`）的端点契约。
//
// 它是这一面的唯一真相源：运行期守卫（middleware.ScopeGuard）与后台接口文档页
// （/api/admin/api-docs）都从本包的同一份声明派生。
//
// 为什么必须只有一份：
//
//	只要"守卫规则表"和"文档契约表"分开存在，就必须再写一个"双向相等"的门禁
//	来兜底，而这个门禁本身又是一份需要维护的清单（豁免项、归一化规则）。
//	合并之后，"文档写错"与"守卫判错"是同一个 bug，不可能只错一半。
//
// 依赖方向（严格单向，无环）：
//
//	apicontract  ->  仅标准库（测试里才 import errs）
//	middleware   ->  apicontract, errs, response
//	controller   ->  apicontract, errs, response
//	router       ->  middleware, controller
//
// 本包**刻意不 import relay / controller / model**：契约是纯数据，
// 一旦反向依赖了业务包，文档页就会把整条执行业务链拉进编译依赖里，
// 并且"文档里能不能出现某个字段"会变成业务代码的函数。
package apicontract

import "sort"

// Auth 是端点的鉴权级别。
//
// 五级的分界线是**凭证能做什么**：
//
//	AuthOpen       无凭证（注册、登录、找回密码）
//	AuthReader     会话票 或 任意 API KEY（只读面：查自己、查用量、查订单）
//	AuthWriter     会话票 或 托管密钥（且必须带对应 scope）—— 能改数据
//	AuthSession    仅会话票（改密、2FA、会话撤销、KEY 增删改、KEY 明文回显）
//	AuthHostedKey  仅托管密钥（换自己的钥、查自己的健康）—— 只操作**自己**这张凭证
//
// 为什么 AuthHostedKey 不能并进 AuthWriter：`writerAllowed` 对会话票无条件放行，
// 并进去就等于"任意登录用户都能调自举轮换"，绕过 allow_self_issue；它也不能
// 依赖 scope —— 存量密钥的 scopes 冻结在签发那一刻，新增一个 scope 会让所有
// 存量钥首次调用即 403。
//
// 为什么 AuthWriter 不能放给用户自建 KEY：用户自建 KEY 是长期凭证、常被贴进代码，
// 让它能下单/删文件，等于把"一次泄露"放大成"资产可被操作"。托管密钥则是
// 应用自己申请、自己保管、可随时轮换的凭证，且 scope 由契约派生，边界是静态的。
type Auth uint8

const (
	// AuthOpen 不需要任何凭证。
	AuthOpen Auth = iota
	// AuthReader 接受会话票或任意有效 API KEY。
	AuthReader
	// AuthWriter 接受会话票，或带对应 scope 的托管密钥。
	AuthWriter
	// AuthSession 只接受会话票（API KEY 一律 403 session_required）。
	AuthSession
	// AuthHostedKey 只接受 kind='hosted' 的 API KEY，且不校验 scope。
	//
	// 语义是"这把凭证操作它自己"：自举轮换、健康自检。它不存在"越权到别人的
	// 数据"这种可能 —— 目标行由鉴权时命中的那把钥决定，不接受任何入参指定。
	AuthHostedKey
)

// String 返回级别的稳定标识（用于文档与测试输出，不要改成中文）。
func (a Auth) String() string {
	switch a {
	case AuthOpen:
		return "open"
	case AuthReader:
		return "reader"
	case AuthWriter:
		return "writer"
	case AuthSession:
		return "session"
	case AuthHostedKey:
		return "hosted_key"
	default:
		return "unknown"
	}
}

// Media 是**成功**响应的媒体形态。
//
// 失败响应永远是平台信封 `{code,message,error_code,data}`，与这里无关 ——
// 端点的媒体形态只描述"成功时 body 长什么样"。
type Media string

const (
	// MediaJSON 是默认形态。
	MediaJSON Media = "json"
	// MediaCSV 是原始 CSV（UTF-8 BOM，带 Content-Disposition）。
	MediaCSV Media = "csv"
	// MediaBinary 是产物字节流（真实 MIME + nosniff，支持 Range/206）。
	MediaBinary Media = "binary"
	// MediaEmpty 是 204 空体。
	MediaEmpty Media = "empty"
)

// scope 词表。
//
// 命名 `resource:action`，全部小写。词表是**封闭**的：新增能力必须在这里加常量，
// 不允许在 catalog 里写字面量 —— 否则拼错的 scope 会静默变成"永不放行"，
// 而它在文档里看起来完全正常。
//
// **只收录写能力**。ScopeGuard 仅在 AuthWriter 级别校验 scope —— 读面与"仅会话"
// 面的权限边界由凭证级别本身决定，不需要权限点。曾经给每个读端点也挂一个
// `xxx:read`，结果是文档写着"需要 usage:read"而运行期从不校验：一处永不生效的
// 声明比没有更糟，它让人以为动了它就会有效果（排查时会往错误方向走很远）。
const (
	ScopeBillingWrite = "billing:write"
	ScopeTaskWrite    = "task:write"
	ScopeFileWrite    = "file:write"
)

// AllScopes 返回封闭词表里的全部 scope（字典序）。
//
// 存在的理由：catalog 里写错一个 scope（`task:write` 写成 `task:wrte`）不会
// 编译失败，也不会让端点失效 —— 它只是一条永远不会被授予的权限，表现为
// 「托管密钥调它总 403」，而文档上看起来完全正常。有了这份词表，测试可以断言
// catalog 里的每个 scope 都在词表内，把拼写错误变成一条红色断言。
//
// 新增 scope 时**必须同时**加常量与这里的一行：Go 没有常量枚举，这份列表是
// 唯一能让"词表"被运行时读取的形式。漏加的表现是测试报"不在封闭词表内"。
func AllScopes() []string {
	out := []string{
		ScopeBillingWrite, ScopeFileWrite, ScopeTaskWrite,
	}
	sort.Strings(out)
	return out
}

// Endpoint 是一条对外端点的完整契约。
//
// Method 必须大写、Path 必须是 **gin 形式**（参数段写 `:id`，不是 `{id}`）——
// gin 运行期 `c.FullPath()` 返回的就是这个形式，两者必须是同一个字符串，
// 否则守卫查不到规则（fail-closed 403），而文档看起来毫无问题。
type Endpoint struct {
	// Key 是端点的稳定标识（`resource.action`），用于文档锚点与前端跳转。
	// 一旦对外发布就不再改：接入方的书签会指向它。
	Key string
	// Group 是文档左侧导航的分组名（中文，运营可读）。
	Group string
	// Title 是一句话说明（中文，动词开头）。
	Title string
	// Method 是 HTTP 方法（大写）。
	Method string
	// Path 是 gin 形式的完整路径。
	Path string
	// Auth 是鉴权级别（运行期守卫据此判定）。
	Auth Auth
	// Scope 是权限点；AuthOpen 时为空串。
	Scope string
	// Media 是成功响应的媒体形态。
	Media Media
	// Idempotency 是幂等语义的一句话说明（文档直出，不做二次加工）。
	Idempotency string
	// Notes 是需要接入方额外注意的一句话（如"明文只返回一次"）。
	Notes string
	// Public 是**免凭证公开面**的限流与缓存策略；只有终端在登录前就要取的端点才非空。
	//
	// 放在契约里而不是各写一处：这两个数字此前分散在路由注册（限流）、
	// controller（Cache-Control）与文档脚本（再抄一遍说明）三处，改一处漏一处
	// 的表现是"文档说 600、线上是 300"，只能靠用户投诉发现。
	Public *PublicPolicy
}

// PublicPolicy 是公开面的对外策略。两个数字都是"客户端可观察"的承诺，
// 因此必须与文档同源（见 .audit/r2/scripts/gen_api_reference.py）。
type PublicPolicy struct {
	// RouteLimit 是按 IP 每分钟的请求上限。
	RouteLimit int `json:"route_limit"`
	// MaxAge 是 `Cache-Control: public, max-age=N` 的秒数。
	MaxAge int `json:"max_age"`
}

// PublicPolicyOf 取某端点声明的公开面策略。第二个返回值为 false 表示
// "这不是公开面端点"，而不是"取不到"——调用方据此决定是否需要报错。
func PublicPolicyOf(key string) (PublicPolicy, bool) {
	for _, e := range catalog {
		if e.Key != key {
			continue
		}
		if e.Public == nil {
			return PublicPolicy{}, false
		}
		return *e.Public, true
	}
	return PublicPolicy{}, false
}

// MustPublicPolicy 是路由与 controller 的唯一取法：取不到直接 panic。
//
// 用 panic 而不是返回零值：调用方传的是**编译期常量 key**，取不到只可能是
// "有人改了 catalog 的 Key 却漏改调用点"。零值会让限流失效（0 = 不挂）与
// `max-age=0`（等于 no-store），也就是静默变成另一个端点，比启动失败难查得多。
func MustPublicPolicy(key string) PublicPolicy {
	p, ok := PublicPolicyOf(key)
	if !ok {
		panic("apicontract: 端点 " + key + " 未声明 PublicPolicy（公开面的限流/缓存必须来自契约）")
	}
	return p
}
