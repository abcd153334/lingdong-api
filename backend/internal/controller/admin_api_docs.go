package controller

// 本文件是**管理后台的接口手册**：只读展示 `/api/user/*` 的对外契约，并支持导出。
//
// 为什么放在管理后台而不做成公开文档站：这一面是给"接入方"看的，而接入方是
// 由平台逐个拉进来的应用（应用由管理员创建、slug 由管理员分配）。公开文档站
// 会把这套接口的存在与语义暴露给任何人，而它对普通用户没有意义 ——
// 普通用户用门户，不需要知道 `client_order_no` 是什么。
//
// 四个端点，共享同一份数据源（apicontract）：
//
//	GET /api/admin/api-docs              目录（分组 + 每条端点的完整契约 + 不开放清单）
//	GET /api/admin/api-docs/openapi.json OpenAPI 3.1（交给接入方导入 Postman/SDK）
//	GET /api/admin/api-docs/markdown     Markdown 手册（贴进接入群/README 文件）
//	GET /api/admin/api-docs/ui/*         内嵌的 Swagger UI（标准工具视图）
//
// **仍然没有"单条详情"端点**，理由在加了参数之后依然成立：
//
//  1. 目录响应已经包含每条端点的全部字段（参数挂在同一条 Endpoint 上），
//     开 detail 只是让前端多一次往返；
//  2. 更糟的是会引入新漂移面：两个 handler 两套投影，将来只在一处补 `errors`，
//     两处都"看起来正常"；
//  3. 体积代价可接受：41 × 8 参数 ≈ 60KB（gzip ~8KB），private + ETag 60s，
//     不值得为它换一次 RTT。

import (
	"encoding/json"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/apicontract"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
)

// adminAPIDocsCatalog 是目录响应。
type adminAPIDocsCatalog struct {
	// Groups 是左侧导航（顺序即契约声明顺序）。
	Groups []string `json:"groups"`
	// Items 是扁平端点表，每行的 Group 决定它挂在哪个分组下。
	Items []adminAPIDocsItem `json:"items"`
	// NotExposed 是「明确不开放」的能力与原因（接入方最常问的问题）。
	NotExposed []adminAPIDocsNotExposed `json:"not_exposed"`
	// Counts 是各级别端点数（前端用来显示"共 N 条 / M 条写操作"，N/M 由契约算出）。
	Counts adminAPIDocsCounts `json:"counts"`
	// HostedScopes 是托管密钥默认权限集（接入方签发后能做什么）。
	HostedScopes []string `json:"hosted_scopes"`
	// Callable 是"可以在本页直接调试"的端点数。
	//
	// 由服务端算而不是让前端 filter().length：前端算就要复刻
	// 「Auth != session」这条判据，而它是契约的一部分 —— 复刻即第二真相源。
	Callable int `json:"callable"`
	// Links 是四个视图的地址，前端直接拿来渲染跳转。
	Links adminAPIDocsLinks `json:"links"`
}

// adminAPIDocsLinks 是各视图的路径。
//
// 这里的字符串与 router.go 注册的路径**是两处**，无法靠编译器保证一致，
// 由 router 包的 TestAPIDocsLinksMatchRoutes 兜住（它遍历 engine.Routes()
// 并逐个请求 links 里的地址）。防的是"文档页给出一个 404 链接"。
type adminAPIDocsLinks struct {
	Catalog   string `json:"catalog"`
	OpenAPI   string `json:"openapi"`
	Markdown  string `json:"markdown"`
	SwaggerUI string `json:"swagger_ui"`
}

// adminAPIDocsItem 是目录里的一行（= 一条端点的完整契约）。
type adminAPIDocsItem struct {
	Key    string `json:"key"`
	Group  string `json:"group"`
	Title  string `json:"title"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Auth   string `json:"auth"`
	Scope  string `json:"scope"`
	// ScopeEnforced 表示上面的 scope 是否真的在运行期被校验。
	//
	// 必须如实下发：ScopeGuard 只对 AuthWriter 校验 scope。scopes 已收敛为
	// "只有写能力才有"，所以非 Writer 端点的 Scope 恒为空、ScopeEnforced 恒为 false。
	// 保留这两个字段是为了让页面能显式表达"这条没有权限点要求"，而不是留白让人猜。
	ScopeEnforced   bool                 `json:"scope_enforced"`
	Media           string               `json:"media"`
	Idempotency     string               `json:"idempotency"`
	Notes           string               `json:"notes"`
	Params          []adminAPIDocsParam  `json:"params"`
	Returns         []adminAPIDocsReturn `json:"returns"`
	Errors          []adminAPIDocsError  `json:"errors"`
	NoData          bool                 `json:"no_data"`
	Sortable        []string             `json:"sortable"`
	RequestExample  map[string]any       `json:"request_example"`
	ResponseExample map[string]any       `json:"response_example"`
	Try             adminAPIDocsTry      `json:"try"`
}

// adminAPIDocsParam 是一个入参。
type adminAPIDocsParam struct {
	Name         string                   `json:"name"`
	In           string                   `json:"in"`
	Type         string                   `json:"type"`
	Required     string                   `json:"required"`
	RequiredNote string                   `json:"required_note"`
	Desc         string                   `json:"desc"`
	Note         string                   `json:"note"`
	Default      string                   `json:"default"`
	Enum         []string                 `json:"enum"`
	Example      any                      `json:"example"`
	Constraints  []adminAPIDocsConstraint `json:"constraints"`
}

// adminAPIDocsConstraint 是一条取值约束。
type adminAPIDocsConstraint struct {
	Kind  string  `json:"kind"`
	Value float64 `json:"value"`
	Text  string  `json:"text"`
}

// adminAPIDocsReturn 是一个响应字段。
type adminAPIDocsReturn struct {
	Name string   `json:"name"`
	Type string   `json:"type"`
	Desc string   `json:"desc"`
	Note string   `json:"note"`
	Enum []string `json:"enum"`
}

// adminAPIDocsError 是一个可能的失败码。
type adminAPIDocsError struct {
	Code string `json:"code"`
	When string `json:"when"`
}

// adminAPIDocsTry 是试运行的三元组。
type adminAPIDocsTry struct {
	// Callable 表示能否在本页发出真实请求。
	Callable bool `json:"callable"`
	// Hint 是不能发的原因（可发时为空串）。
	Hint string `json:"hint"`
	// Risk 是 safe / confirm。confirm 必须二次确认。
	Risk string `json:"risk"`
}

// adminAPIDocsNotExposed 是不开放条目。
type adminAPIDocsNotExposed struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Title  string `json:"title"`
	Reason string `json:"reason"`
}

// adminAPIDocsCounts 是级别统计。
//
// 新增 Auth 级别时**必须**在这里加一项：否则"各级别之和 ≠ total"，
// 页面会显示一个对不上的数字，而且看不出少了什么。
type adminAPIDocsCounts struct {
	Total     int `json:"total"`
	Open      int `json:"open"`
	Reader    int `json:"reader"`
	Writer    int `json:"writer"`
	Session   int `json:"session"`
	HostedKey int `json:"hosted_key"`
}

// 各视图的路径常量。
//
// 抽成常量而不是在每个 handler 里写字面量：`links` 与路由注册必须一致，
// 常量至少让"改一处忘另一处"变成"改一处，另一处编译期就指向同一个值"。
// 路由侧引用同一组常量（见 router.go），因此这一组是**唯一**的路径来源。
const (
	APIDocsPathCatalog   = "/api/admin/api-docs"
	APIDocsPathOpenAPI   = "/api/admin/api-docs/openapi.json"
	APIDocsPathMarkdown  = "/api/admin/api-docs/markdown"
	APIDocsPathSwaggerUI = "/api/admin/api-docs/ui/"
	// APIDocsPathSwaggerAsset 是 catch-all（gin 的 `*asset` 段）。
	// gin 不允许同一层同时有"叶子"（/ui）与 catch-all（/ui/*asset），
	// 所以只注册这一条；访问 `/ui` 由 gin 自己 301 到 `/ui/`。
	APIDocsPathSwaggerAsset = "/api/admin/api-docs/ui/*asset"
)

// AdminAPIDocsCatalog 处理 GET /api/admin/api-docs。
func AdminAPIDocsCatalog(c *gin.Context) {
	rules := apicontract.Rules()
	items := make([]adminAPIDocsItem, 0, len(rules))
	callable := 0
	for _, e := range rules {
		if e.Tries() {
			callable++
		}
		items = append(items, toAdminAPIDocsItem(e))
	}
	nots := apicontract.NotExposedList()
	hidden := make([]adminAPIDocsNotExposed, 0, len(nots))
	for _, n := range nots {
		hidden = append(hidden, adminAPIDocsNotExposed{
			Method: n.Method, Path: n.Path, Title: n.Title, Reason: n.Reason,
		})
	}
	byAuth := apicontract.CountByAuth()
	writeAdminDocsJSON(c, adminAPIDocsCatalog{
		Groups:     apicontract.Groups(),
		Items:      items,
		NotExposed: hidden,
		Counts: adminAPIDocsCounts{
			Total:     len(items),
			Open:      byAuth[apicontract.AuthOpen],
			Reader:    byAuth[apicontract.AuthReader],
			Writer:    byAuth[apicontract.AuthWriter],
			Session:   byAuth[apicontract.AuthSession],
			HostedKey: byAuth[apicontract.AuthHostedKey],
		},
		HostedScopes: apicontract.HostedScopes(),
		Callable:     callable,
		Links: adminAPIDocsLinks{
			Catalog:   APIDocsPathCatalog,
			OpenAPI:   APIDocsPathOpenAPI,
			Markdown:  APIDocsPathMarkdown,
			SwaggerUI: APIDocsPathSwaggerUI,
		},
	}, 60)
}

// toAdminAPIDocsItem 把一条契约投影成目录行。
//
// 所有切片都保证非 nil（项目铁律：对外数组不能是 null —— 前端 `.length`
// 在 null 上会抛错，整页白屏而 HTTP 仍是 200）。
func toAdminAPIDocsItem(e apicontract.Endpoint) adminAPIDocsItem {
	d := e.Doc()
	params := make([]adminAPIDocsParam, 0, len(d.Params))
	for _, p := range d.Params {
		cs := make([]adminAPIDocsConstraint, 0, len(p.Constraints))
		for _, x := range p.Constraints {
			cs = append(cs, adminAPIDocsConstraint{Kind: x.Kind, Value: x.Value, Text: x.Text})
		}
		params = append(params, adminAPIDocsParam{
			Name: p.Name, In: string(p.In), Type: string(p.Type),
			Required: string(p.Required), RequiredNote: p.RequiredNote,
			Desc: p.Desc, Note: p.Note, Default: p.Default,
			Enum: p.Enum, Example: p.Example, Constraints: cs,
		})
	}
	returns := make([]adminAPIDocsReturn, 0, len(d.Returns))
	for _, r := range d.Returns {
		returns = append(returns, adminAPIDocsReturn{
			Name: r.Name, Type: string(r.Type), Desc: r.Desc, Note: r.Note, Enum: r.Enum,
		})
	}
	errsList := make([]adminAPIDocsError, 0, len(d.Errors))
	for _, ec := range d.Errors {
		errsList = append(errsList, adminAPIDocsError{Code: ec.Code, When: ec.When})
	}
	return adminAPIDocsItem{
		Key: e.Key, Group: e.Group, Title: e.Title,
		Method: e.Method, Path: e.Path,
		Auth: e.Auth.String(), Scope: e.Scope, ScopeEnforced: d.ScopeEnforced,
		Media: string(e.Media), Idempotency: e.Idempotency, Notes: e.Notes,
		Params: params, Returns: returns, Errors: errsList,
		NoData:          d.NoData,
		Sortable:        d.Sortable,
		RequestExample:  orEmptyMap(d.ReqExample),
		ResponseExample: orEmptyMap(d.RespExample),
		Try: adminAPIDocsTry{
			Callable: e.Tries(),
			Hint:     e.TryHint(),
			Risk:     string(e.TrialRiskOf()),
		},
	}
}

// orEmptyMap 把 nil map 变成空对象。
//
// nil map 序列化成 `null`，前端 `Object.keys(null)` 抛 TypeError，
// 表现为"参数表整块消失"而接口 200。空对象是安全的中性值。
func orEmptyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// AdminAPIDocsOpenAPI 处理 GET /api/admin/api-docs/openapi.json。
//
// server 地址取站点对外基址（与文件直链同一口径）：接入方导入后能直接发请求，
// 不需要自己替换 host。取不到时回落 "/"，生成文档仍然合法。
func AdminAPIDocsOpenAPI(c *gin.Context) {
	doc := apicontract.OpenAPI(siteBase(c))
	// 直接序列化到字节（而不是走 writeAdminDocsJSON）：OpenAPI 文档不是平台信封，
	// 它必须是一份**干净的** OpenAPI 文件，否则导入工具会解析失败。
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	writeAdminDocsRaw(c, raw, "application/json; charset=utf-8", 300)
}

// AdminAPIDocsMarkdown 处理 GET /api/admin/api-docs/markdown。
func AdminAPIDocsMarkdown(c *gin.Context) {
	writeAdminDocsRaw(c, []byte(apicontract.Markdown()), "text/markdown; charset=utf-8", 300)
}
