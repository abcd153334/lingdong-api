package apicontract_test

// 本文件守的是**第二层契约（detail）的完整性**。
//
// 每一条测试都对应一种**真实发生过的漂移形态**，不是"为了覆盖率"。写在注释里
// 的是"如果不测，线上会看到什么"，那是判断这条测试该不该留的唯一标准。

import (
	"testing"

	"lingdong-api/internal/apicontract"
	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/pkg/errs"
)

// detailMissingBudget 是"尚未填写第二层契约的端点数"。
//
// 它是**施工噪音**，故意要随每一批手写而改小：
//
//	P0  41（骨架就绪，detail 空表）
//	P1a 22（认证 6 + 账号 3 + 安全 10）
//	P1b 11（密钥 7 + 用量 4）
//	P1c  0（账单 5 + 任务 4 + 文件 2，必须归零）
//
// 归零之后它就不再是豁免，而是"一条都不许漏"的硬约束。如果将来新增端点
// 却忘了写 detail，这个常量不跟着涨，测试立刻红。
const detailMissingBudget = 0

// docEndpoints 返回已声明 detail 的端点（门禁的实际作用域）。
//
// 未声明的端点由 TestEveryEndpointHasDetail 用预算统一管；其余 13 条只对
// 已声明的做逐项校验，否则 P0 阶段会因为空表而全部误红。
func docEndpoints() []apicontract.Endpoint {
	var out []apicontract.Endpoint
	for _, e := range apicontract.Rules() {
		if e.HasDoc() {
			out = append(out, e)
		}
	}
	return out
}

// TestEveryEndpointHasDetail 保证每条端点都有第二层契约。
//
// 不测的后果：新增端点忘写 detail -> 页面出现一条"没有任何参数"的接口。
// 接入方按空参数表调 POST，被 400 打回，而文档页没有任何红色提示。
func TestEveryEndpointHasDetail(t *testing.T) {
	var missing []string
	for _, e := range apicontract.Rules() {
		if !e.HasDoc() {
			missing = append(missing, e.Key)
		}
	}
	if len(missing) > detailMissingBudget {
		t.Fatalf("有 %d 条端点缺少第二层契约，预算 %d 条：%v",
			len(missing), detailMissingBudget, missing)
	}
	if detailMissingBudget == 0 && len(missing) != 0 {
		t.Fatalf("预算已归零，仍有 %d 条端点缺 detail：%v", len(missing), missing)
	}
	if len(missing) > 0 {
		t.Logf("仍有 %d 条端点待填 detail：%v", len(missing), missing)
	}
}

// TestNoOrphanDetail 保证 detail 里没有拼错字的孤儿条目。
//
// 不测的后果：把 `billing.orders_create` 写成 `billing.order_create`，
// detail 表里多一条查不到的条目，真实端点静默变成"空参数表"。
// 这是本方案最容易犯、也最难靠肉眼发现的错误（两个词只差一个 s）。
func TestNoOrphanDetail(t *testing.T) {
	known := map[string]bool{}
	for _, e := range apicontract.Rules() {
		known[e.Key] = true
	}
	for _, k := range apicontract.DetailKeys() {
		if !known[k] {
			t.Errorf("detail 里的 Key %q 在 catalog 中不存在（拼写错误？）", k)
		}
	}
}

// TestPathParamsMatchPathTemplate 保证路径参数与 Path 占位符一一对应。
//
// 不测的后果分两种，都很难查：
//   - Path 有 `:index` 但 detail 漏声明 -> 试运行拼出 `/contents/` -> 404，
//     用户以为"这个接口坏了"；
//   - detail 声明了 `id` 但 Path 里没有 -> 输入框填了也没用，试运行永远忽略它。
func TestPathParamsMatchPathTemplate(t *testing.T) {
	for _, e := range docEndpoints() {
		want := e.PathParams()
		var got []string
		for _, p := range e.Doc().Params {
			if p.In == apicontract.InPath {
				got = append(got, p.Name)
			}
		}
		if len(got) != len(want) {
			t.Errorf("%s: Path 有 %d 个参数 %v，detail 声明了 %d 个 %v",
				e.Key, len(want), want, len(got), got)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: 第 %d 个路径参数 detail=%q，Path=%q（顺序或名字不一致）",
					e.Key, i, got[i], want[i])
			}
		}
	}
}

// 封闭词表。**刻意在这里独立重写一份**，不引用 schema.go 的私有变量：
// 这样"新增了一个 Type 却没同步前端控件"会红，而不是自动通过。
var (
	knownTypes = map[apicontract.Type]bool{
		apicontract.TypeString: true, apicontract.TypeInteger: true,
		apicontract.TypeNumber: true, apicontract.TypeBoolean: true,
		apicontract.TypeArray: true, apicontract.TypeObject: true,
		apicontract.TypeFile: true, apicontract.TypeBinary: true,
		apicontract.TypeNone: true, apicontract.TypeAny: true,
	}
	knownIns = map[apicontract.In]bool{
		apicontract.InBody: true, apicontract.InQuery: true,
		apicontract.InPath: true, apicontract.InHeader: true,
		apicontract.InForm: true,
	}
	knownRequired = map[apicontract.Required]bool{
		apicontract.ReqRequired: true, apicontract.ReqOptional: true,
		apicontract.ReqConditional: true,
	}
)

// TestParamVocabularyClosed 保证 In/Type/Required 都在封闭词表内。
//
// 不测的后果：前端按 Type 选控件（string -> Input、enum -> Select）时
// switch 落空，这个参数**永远渲染不出输入框**，试运行发出去的是缺参请求。
// 文档页看起来毫无异常，只是少了一个框。
func TestParamVocabularyClosed(t *testing.T) {
	for _, e := range docEndpoints() {
		for _, p := range e.Doc().Params {
			if p.Name == "" {
				t.Errorf("%s: 存在没有名字的参数", e.Key)
			}
			if !knownIns[p.In] {
				t.Errorf("%s.%s: In=%q 不在词表内", e.Key, p.Name, p.In)
			}
			if !knownTypes[p.Type] {
				t.Errorf("%s.%s: Type=%q 不在词表内", e.Key, p.Name, p.Type)
			}
			if !knownRequired[p.Required] {
				t.Errorf("%s.%s: Required=%q 不在词表内", e.Key, p.Name, p.Required)
			}
			if p.Desc == "" {
				t.Errorf("%s.%s: 缺少说明（页面上会是一行空白）", e.Key, p.Name)
			}
		}
	}
}

// TestConditionalNeedsReason 保证"条件必填"都写清了条件。
//
// 不测的后果：`billing.orders_create` 的 package_id / amount 是二选一。
// 只说"条件必填"而不说"什么时候必填"，接入方要么两个都传（被 400），
// 要么两个都不传（被 400），而文档看起来已经说明了。
func TestConditionalNeedsReason(t *testing.T) {
	for _, e := range docEndpoints() {
		for _, p := range e.Doc().Params {
			if p.Required != apicontract.ReqConditional {
				continue
			}
			if p.RequiredNote == "" {
				t.Errorf("%s.%s 标了 conditional 但没写条件（RequiredNote 为空）", e.Key, p.Name)
			}
		}
	}
}

// noBodyWriteEndpoints 是**确实没有请求体**的写方法端点。
//
// 白名单必须是显式的：新增一条 POST 却忘了声明 body 时，如果门禁自动放过，
// 表现就是"文档说这个接口不用传参"，而服务端要 400。
var noBodyWriteEndpoints = map[string]bool{
	"security.logout":              true,
	"security.sessions_revoke_all": true,
	"security.sessions_revoke":     true,
	"keys.reset":                   true,
	"tasks.cancel":                 true,
	"files.delete":                 true,
	"keys.delete":                  true,
	// 自举轮换：POST 但没有任何入参 —— 换哪一把钥由鉴权时命中的凭证决定，
	// 请求体带什么都不会被读取（带了反而会让人以为可以指定目标）。
	"keys.self_rotate": true,
}

// TestMethodWithBodyHasBodyParam 保证写方法要么声明了 body，要么在白名单里。
func TestMethodWithBodyHasBodyParam(t *testing.T) {
	for _, e := range docEndpoints() {
		switch e.Method {
		case "POST", "PUT", "PATCH":
		default:
			continue
		}
		var hasBody bool
		for _, p := range e.Doc().Params {
			if p.In == apicontract.InBody || p.In == apicontract.InForm {
				hasBody = true
				break
			}
		}
		if hasBody {
			if noBodyWriteEndpoints[e.Key] {
				t.Errorf("%s 已声明 body 参数，应从 noBodyWriteEndpoints 白名单里移除", e.Key)
			}
			continue
		}
		if !noBodyWriteEndpoints[e.Key] {
			t.Errorf("%s 是 %s 但没有 body 参数，也不在白名单里；要么补参数，要么登记它确实无 body",
				e.Key, e.Method)
		}
	}
}

// TestMediaContract 保证非 JSON 媒体的响应字段声明自洽。
//
// 不测的后果（每一类都真实存在）：
//   - `files.delete` 实际是 204 空体，若声明了 Returns，文档会说"返回 data"
//     -> 接入方按文档解析 body -> 空字符串 -> 解析失败；
//   - `usage.export` 是 CSV 流，声明 Returns 会让生成的 SDK 去解 JSON；
//   - `tasks.content` 是二进制，同理。
func TestMediaContract(t *testing.T) {
	for _, e := range docEndpoints() {
		d := e.Doc()
		switch e.Media {
		case apicontract.MediaEmpty, apicontract.MediaCSV, apicontract.MediaBinary:
			if len(d.Returns) != 0 {
				t.Errorf("%s: Media=%s 不应声明返回字段（响应体不是 JSON 信封）", e.Key, e.Media)
			}
			for _, p := range d.Params {
				if p.In == apicontract.InBody || p.In == apicontract.InForm {
					t.Errorf("%s: Media=%s 通常不应有请求体参数（%s）", e.Key, e.Media, p.Name)
				}
			}
			if len(d.RespExample) != 0 {
				t.Errorf("%s: Media=%s 不应有响应示例", e.Key, e.Media)
			}
		case apicontract.MediaJSON:
			// 由 TestJSONHasReturns 单独守。
		default:
			t.Errorf("%s: 未知的 Media %q", e.Key, e.Media)
		}
	}
}

// TestJSONHasReturns 保证 JSON 端点都声明了返回字段。
//
// 不测的后果：`data` 退回"业务数据（各端点不同）"这一句描述 ——
// 正是本轮要解决的问题。漏一条就白做一条。
//
// `NoData` 是显式豁免（成功时 data 恒为 null，如 logout），且不允许
// 与 Returns 同时非空 —— 那说明作者自己都没想清这个端点返回什么。
func TestJSONHasReturns(t *testing.T) {
	for _, e := range docEndpoints() {
		if e.Media != apicontract.MediaJSON {
			continue
		}
		d := e.Doc()
		if d.NoData {
			if len(d.Returns) != 0 {
				t.Errorf("%s: NoData 与 Returns 不能同时声明", e.Key)
			}
			if len(d.RespExample) != 0 {
				t.Errorf("%s: NoData 不应有响应示例", e.Key)
			}
			continue
		}
		if len(d.Returns) == 0 {
			t.Errorf("%s: Media=json 但既没有 Returns 也没标 NoData（data 会退化成一句描述）", e.Key)
		}
	}
}

// TestAuthSessionIsNotTrialable 保证不可试运行的端点既不提供试运行、又写清了原因。
//
// 不可试运行有两类：
//   - 仅会话票：文档页能发请求，但服务端必 403 `session_required`
//   - 仅托管密钥：文档页的浏览器只有会话 Cookie，发出去必 401
//
// 不测的后果：文档给了一个按钮，用户点下去必然失败，而页面没有任何说明 ——
// 于是他会去查自己的密钥是不是坏了。
func TestAuthSessionIsNotTrialable(t *testing.T) {
	reasons := map[apicontract.SessionWhy]bool{
		apicontract.WhyCredential: true, apicontract.WhySession: true,
		apicontract.WhyKeyManage: true, apicontract.WhyKeyIssue: true,
		apicontract.WhyHostedKey: true,
	}
	for _, e := range docEndpoints() {
		d := e.Doc()
		if !e.Tries() {
			if d.SessionWhy == apicontract.WhyNone {
				t.Errorf("%s: 不可试运行但 SessionWhy 为空（页面只显示一个灰按钮，没有原因）", e.Key)
			}
			if !reasons[d.SessionWhy] {
				t.Errorf("%s: SessionWhy=%q 不在四类之内", e.Key, d.SessionWhy)
			}
			if e.TryHint() == "" {
				t.Errorf("%s: TryHint() 为空", e.Key)
			}
			continue
		}
		if d.SessionWhy != apicontract.WhyNone {
			t.Errorf("%s: 可试运行却写了 SessionWhy=%q（自相矛盾）", e.Key, d.SessionWhy)
		}
		if e.TryHint() != "" {
			t.Errorf("%s: 可试运行却有 TryHint=%q", e.Key, e.TryHint())
		}
	}
}

// TestScopeMatchesAuth 保证 Scope 与 Auth 级别、ScopeEnforced 三者自洽。
//
// 关键在 ScopeEnforced：ScopeGuard 只对 AuthWriter 校验 scope
// （writerAllowed），AuthReader 与 AuthSession 都只判身份不判 scope。
// 若给 14 条读接口标"需要 usage:read"，是在让接入方去申请一个
// **不存在也永远用不到**的权限点。
func TestScopeMatchesAuth(t *testing.T) {
	hosted := map[string]bool{}
	for _, s := range apicontract.HostedScopes() {
		hosted[s] = true
	}
	for _, e := range docEndpoints() {
		d := e.Doc()
		switch e.Auth {
		case apicontract.AuthOpen:
			if e.Scope != "" {
				t.Errorf("%s: 无凭证端点不应有 scope", e.Key)
			}
			if d.ScopeEnforced {
				t.Errorf("%s: 无凭证端点不应声称 scope 生效", e.Key)
			}
		case apicontract.AuthWriter:
			if !d.ScopeEnforced {
				t.Errorf("%s: 写端点的 scope 由 writerAllowed 真正校验，ScopeEnforced 必须为 true", e.Key)
			}
			if !hosted[e.Scope] {
				t.Errorf("%s: scope %q 不在托管密钥权限集内（托管密钥永远 403）", e.Key, e.Scope)
			}
		case apicontract.AuthReader, apicontract.AuthSession:
			if d.ScopeEnforced {
				t.Errorf("%s: %s 级别不校验 scope，ScopeEnforced 必须为 false", e.Key, e.Auth)
			}
		}
	}
}

// TestErrCodesExist 保证文档里写的每个失败码都真实存在。
//
// 不测的后果：写成 `invald_param`（少个 i），接入方照着写重试分支，
// 而它永远不会命中 —— 错误处理代码永远走 default，生产事故时才发现。
func TestErrCodesExist(t *testing.T) {
	known := map[string]bool{}
	for _, d := range errs.All() {
		known[d.Code] = true
	}
	if len(known) < 50 {
		t.Fatalf("errs.All() 只解析出 %d 个码，明显不对（可能是实现变了）", len(known))
	}
	for _, e := range docEndpoints() {
		for _, ec := range e.Doc().Errors {
			if ec.Code == "" || ec.When == "" {
				t.Errorf("%s: 存在空的错误码或空的原因", e.Key)
				continue
			}
			if !known[ec.Code] {
				t.Errorf("%s: 错误码 %q 在 errs 里不存在（拼写错误？）", e.Key, ec.Code)
			}
		}
	}
}

// topField 取 `items[].key_name` 的顶层名 `items`。
func topField(name string) string {
	for i := 0; i < len(name); i++ {
		if name[i] == '[' || name[i] == '.' {
			return name[:i]
		}
	}
	return name
}

// TestExamplesMatchParams 保证示例字段与声明一致。
//
// 不测的后果：
//   - 示例里出现未声明的字段 -> 试运行照发，服务端忽略它；
//   - 漏了必填项 -> 试运行第一次点击必 400，用户以为"接口要求没写清"。
func TestExamplesMatchParams(t *testing.T) {
	for _, e := range docEndpoints() {
		d := e.Doc()
		bodyNames := map[string]bool{}
		var requiredBody []string
		for _, p := range d.Params {
			if p.In != apicontract.InBody && p.In != apicontract.InForm {
				continue
			}
			bodyNames[p.Name] = true
			if p.Required == apicontract.ReqRequired {
				requiredBody = append(requiredBody, p.Name)
			}
		}
		if len(bodyNames) > 0 && len(d.ReqExample) == 0 {
			t.Errorf("%s: 有 body 参数但没有请求示例（试运行会打开一个空编辑器）", e.Key)
		}
		for k := range d.ReqExample {
			if !bodyNames[k] {
				t.Errorf("%s: 请求示例含未声明的字段 %q", e.Key, k)
			}
		}
		for _, name := range requiredBody {
			if _, ok := d.ReqExample[name]; !ok {
				t.Errorf("%s: 必填字段 %q 不在请求示例里（试运行第一次点击必 400）", e.Key, name)
			}
		}
		if len(d.RespExample) > 0 {
			retNames := map[string]bool{}
			for _, r := range d.Returns {
				retNames[topField(r.Name)] = true
			}
			for k := range d.RespExample {
				if !retNames[k] {
					t.Errorf("%s: 响应示例含未声明的字段 %q", e.Key, k)
				}
			}
		}
	}
}

// TestOutwardWordingIsNeutral 保证契约里的每一句对外文字都不含内部术语。
//
// 复用 apidoc.CheckText 的词表（渠道/上游/适配器/聚合/第三方域名）。
// 只在**测试**里 import apidoc：生产包的依赖方向不变（apicontract 仍只依赖
// 标准库），而词表只有一份，不会出现"文档站查一套、开放接口查另一套"。
func TestOutwardWordingIsNeutral(t *testing.T) {
	for _, e := range apicontract.Rules() {
		texts := []string{e.Title, e.Idempotency, e.Notes}
		for _, p := range e.Doc().Params {
			texts = append(texts, p.Desc, p.Note, p.RequiredNote, p.Default)
		}
		for _, r := range e.Doc().Returns {
			texts = append(texts, r.Desc, r.Note)
		}
		for _, ec := range e.Doc().Errors {
			texts = append(texts, ec.When)
		}
		for _, s := range texts {
			for _, v := range apidoc.CheckText(s) {
				t.Errorf("%s: 对外文字含内部术语 %q（原文：%s）", e.Key, v.Text, s)
			}
		}
	}
	for _, n := range apicontract.NotExposedList() {
		for _, v := range apidoc.CheckText(n.Title + n.Reason) {
			t.Errorf("不开放清单 %q 含内部术语 %q", n.Title, v.Text)
		}
	}
	for _, g := range apicontract.Groups() {
		if vs := apidoc.CheckText(g); len(vs) > 0 {
			t.Errorf("分组名 %q 含内部术语 %q", g, vs[0].Text)
		}
	}
}

// confirmRiskEndpoints 是**必须在发送前二次确认**的端点。
//
// 判据是"点一下会真的改变生产状态"，不是"是不是写方法"：
// `auth.login` 是 POST 但它只会新建一个会话；`auth.reset_password` 也是 POST
// 但它会把账号密码改掉。两者的风险相差一个数量级，靠 Method 分不出来。
var confirmRiskEndpoints = map[string]bool{
	"auth.register":              true, // 真的建一个账号
	"auth.login":                 true, // 真的建一个会话
	"auth.login_mfa":             true, // 真的建一个会话
	"auth.reset_contact_code":    true, // 真的发一条短信（真实成本 + 骚扰）
	"auth.register_contact_code": true, // 同上：真的发一条短信/邮件
	"auth.reset_password":        true, // 真的改掉账号密码
	"billing.orders_create":      true, // 真的建一笔待付订单
	"billing.redeem":             true, // 真的给账户入账（不可逆，码作废）
	"tasks.cancel":               true, // 真的取消任务且不可撤回
	"files.delete":               true, // 真的删对象存储且不可恢复
}

// TestTrialRiskIsExplicit 保证每条可试端点都标了风险，且高风险集合不缩水。
//
// 不测的后果：新增一条"会真写库"的端点并标成 safe -> 页面上一个普通按钮，
// 点一下就改了生产数据。风险级别必须显式声明，不能靠默认值。
func TestTrialRiskIsExplicit(t *testing.T) {
	for _, e := range docEndpoints() {
		d := e.Doc()
		if !e.Tries() {
			continue
		}
		switch d.TrialRisk {
		case apicontract.RiskSafe, apicontract.RiskConfirm:
		default:
			t.Errorf("%s: 可试运行但 TrialRisk=%q 未声明", e.Key, d.TrialRisk)
			continue
		}
		wantConfirm := confirmRiskEndpoints[e.Key]
		if wantConfirm && d.TrialRisk != apicontract.RiskConfirm {
			t.Errorf("%s: 会真实改变生产状态，TrialRisk 必须是 confirm（当前 %q）", e.Key, d.TrialRisk)
		}
		if !wantConfirm && d.TrialRisk == apicontract.RiskConfirm {
			t.Errorf("%s: 标了 confirm 但不在高风险清单里；请确认后二选一（改清单或改标记）", e.Key)
		}
	}
}
