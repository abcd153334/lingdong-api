package apicontract_test

// 本文件守的是**契约自身的完整性**：它能被查到、路径形态正确、权限点是词表内的、
// 数量与阶段预期一致。它不关心 HTTP 行为（那是 router/api_scope_gate_test.go 的事）。
//
// 用外部测试包（`apicontract_test`）而不是同包：这样测试只能通过公开 API 断言，
// 顺带证明了"契约对外暴露的信息足以让守卫与文档工作"——如果某个断言必须读私有字段
// 才能写，说明契约的公开面缺东西。

import (
	"regexp"
	"strings"
	"testing"

	"lingdong-api/internal/apicontract"
)

// TestRuleManifestSize 锁死端点总数。
//
// 这个数**必须随每一条端点一起改**，它是刻意设置的"施工噪音"：
// 加了路由却忘了写契约时，双向门禁（router 包）会红；写了契约却加错一条
// （例如复制粘贴后忘了改 path）时，这个数会红。两个失败都比"静默少一条"好。
//
// 阶段期望值（历史记录，便于核对当前进度）：
//
//	P1 = 24（存量端点全部显式声明）
//	P3 = 36（+12 只读面）
//	P4 = 39（+3 写面）
//	P5 = 41（+2 应用面）
//	开放面补齐 = 45（+captcha、+register_contact_code、+orders/:id/qrcode、+redeem）
//	托管密钥自举 = 51（+keys.self_rotate、+keys.health）
//	应用级后台 = 52（+apps.runtime：唯一的无凭证端点，终端启动自举用）
//	公开面三分 = 54（-apps.runtime，+apps.profile/+apps.notices/+apps.release：
//	三条免凭证端点各自独立缓存策略，合成一条只能取最严的那个）
//	插件数据 = 55（+apps.plugin_records：默认关闭的免凭证只读面，
//	开不开由「安装级开关 + manifest 字段白名单」两个闸共同决定）
const expectedRuleCount = 55

func TestRuleManifestSize(t *testing.T) {
	if got := len(apicontract.Rules()); got != expectedRuleCount {
		t.Fatalf("端点声明数 = %d，期望 %d；新增/删除端点时必须同步改本常量", got, expectedRuleCount)
	}
}

// TestCatalogKeysUnique 保证 (Key) 与 (Method, Path) 都唯一。
//
// 重复的后果不是报错而是"后者覆盖前者"：守卫用后者的级别，
// 文档用两条相同路径渲染两遍 —— 页面看起来只是"重复了一条"，权限却已经变了。
func TestCatalogKeysUnique(t *testing.T) {
	if dup := apicontract.Duplicates(); len(dup) != 0 {
		t.Fatalf("存在重复的 (Method, Path) 声明: %v", dup)
	}
	seenKey := map[string]string{}
	for _, e := range apicontract.Rules() {
		if e.Key == "" {
			t.Errorf("%s %s 缺少 Key", e.Method, e.Path)
			continue
		}
		if prev, ok := seenKey[e.Key]; ok {
			t.Errorf("Key 重复: %q 同时用于 %s 与 %s", e.Key, prev, e.Method+" "+e.Path)
		}
		seenKey[e.Key] = e.Method + " " + e.Path
	}
}

// TestCatalogPathsAreGinForm 校验路径写法。
//
// 写成 `{id}` 的后果是全链路静默失效：`c.FullPath()` 返回 `:id` 形态，
// RuleFor 查不到 -> 守卫 fail-closed 403。现象是"这个端点对谁都 403"，
// 而契约测试（只比对字符串）完全看不出来。
func TestCatalogPathsAreGinForm(t *testing.T) {
	// 三个前缀：前两个是对外用户面，第三个是终端的公开自举面
	// （app.profile/notices/release 三条免凭证端点不在 /api/user 下，
	//  因为终端在用户登录之前就要拿到品牌、公告与版本）。
	allowed := []string{"/api/user/", "/api/auth/", "/api/apps/"}
	for _, e := range apicontract.Rules() {
		ok := false
		for _, pre := range allowed {
			if strings.HasPrefix(e.Path, pre) {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("%s 的路径 %q 不在 %v 下", e.Key, e.Path, allowed)
		}
		if strings.ContainsAny(e.Path, "{}") {
			t.Errorf("%s 的路径 %q 用了大括号写法，必须是 gin 的 :param", e.Key, e.Path)
		}
		if strings.HasSuffix(e.Path, "/") {
			t.Errorf("%s 的路径 %q 以 / 结尾，gin 注册时不会带它", e.Key, e.Path)
		}
		if strings.Contains(e.Path, "//") {
			t.Errorf("%s 的路径 %q 含连续斜杠", e.Key, e.Path)
		}
		// 参数段必须是 :xxx 形态（gin 只认这一种）。
		for _, seg := range strings.Split(e.Path, "/") {
			if strings.HasPrefix(seg, "{") || strings.HasSuffix(seg, "}") {
				t.Errorf("%s 的路径段 %q 不是 gin 参数写法", e.Key, seg)
			}
		}
	}
}

// scopePattern 与 endpoint.go 的命名约定一一对应（resource:action，全小写）。
var scopePattern = regexp.MustCompile(`^[a-z]+:[a-z]+$`)

// TestCatalogScopesInVocabulary 保证 scope 只来自封闭词表，且**只有 AuthWriter 才声明**。
//
// 关键点：scope 只在 middleware.ScopeGuard 的 AuthWriter 分支被读取。其它级别
// （Open / Reader / Session / HostedKey）声明 scope 都是"永不生效的声明"——
// 文档会照抄它，让人以为改了有效果，而实际什么都不影响。所以这里对非 Writer
// 级别要求**必须为空**。
//
// Writer 漏写 scope 的后果是"托管密钥永远拿不到这个权限点"，文档却看起来正常。
func TestCatalogScopesInVocabulary(t *testing.T) {
	vocab := map[string]bool{}
	for _, s := range apicontract.AllScopes() {
		vocab[s] = true
	}
	for _, e := range apicontract.Rules() {
		if e.Auth != apicontract.AuthWriter {
			if e.Scope != "" {
				t.Errorf("%s 是 %s 级别，不该声明 scope（写了 %q）：scope 只在 AuthWriter 上生效",
					e.Key, e.Auth, e.Scope)
			}
			continue
		}
		if e.Scope == "" {
			t.Errorf("%s 是 AuthWriter 端点，必须声明 scope（漏写 = 该权限点永远无法被授予）", e.Key)
			continue
		}
		if !scopePattern.MatchString(e.Scope) {
			t.Errorf("%s 的 scope %q 不符合 resource:action 形式", e.Key, e.Scope)
		}
		if !vocab[e.Scope] {
			t.Errorf("%s 的 scope %q 不在封闭词表内（拼写错误？）", e.Key, e.Scope)
		}
	}
}

// TestAllScopesIsSortedAndUnique 保证词表本身可用于稳定输出（文档 diff 不抖动）。
func TestAllScopesIsSortedAndUnique(t *testing.T) {
	prev := ""
	for _, s := range apicontract.AllScopes() {
		if s <= prev {
			t.Fatalf("AllScopes 未按字典序或含重复: %q 出现在 %q 之后", s, prev)
		}
		prev = s
	}
}

// TestRuleForResolvesEveryEndpoint 保证每条声明都能被运行期查到。
//
// 这是"契约 → 守卫"方向的最近一道保险：ruleIndex 的键是 method+path，
// 任何拼写/大小写差异都会让 RuleFor 落空。
func TestRuleForResolvesEveryEndpoint(t *testing.T) {
	for _, want := range apicontract.Rules() {
		got, ok := apicontract.RuleFor(want.Method, want.Path)
		if !ok {
			t.Errorf("RuleFor(%s, %s) 查不到", want.Method, want.Path)
			continue
		}
		if got.Key != want.Key {
			t.Errorf("RuleFor(%s, %s) 命中 %q，期望 %q", want.Method, want.Path, got.Key, want.Key)
		}
	}
}

// TestRuleForUnknownIsFalse 保证未声明路由**不会**命中任意默认值。
//
// 三类"看起来存在但实际不存在"的组合都要覆盖：
// 路径不存在、路径存在但方法不对、空值。方法大小写不在其中 ——
// RuleFor 会 ToUpper，这是刻意的容错（`c.Request.Method` 恒大写，
// 但测试与脚本里写小写很常见），所以 "get" 能命中是正确行为。
func TestRuleForUnknownIsFalse(t *testing.T) {
	cases := [][2]string{
		{"GET", "/api/user/not-declared"},
		{"PUT", "/api/user/self"},     // 只有 GET
		{"GET", "/api/user/register"}, // 只有 POST
		{"DELETE", "/api/user/keys"},  // 只有 GET / POST
		{"", ""},
	}
	for _, c := range cases {
		if _, ok := apicontract.RuleFor(c[0], c[1]); ok {
			t.Errorf("RuleFor(%q, %q) 不应命中", c[0], c[1])
		}
	}
}

// TestRulesReturnsCopy 保证调用方改不动真相源。
//
// 用 first[0] 自身的 Method 查找，而不是硬编码 POST：目录首条历史上是
// auth.register（POST），补 GET /api/user/captcha 后首条是 GET——硬编码
// 方法会让这个"纯副本安全性"测试依赖端点排序这种无关属性。
func TestRulesReturnsCopy(t *testing.T) {
	first := apicontract.Rules()
	if len(first) == 0 {
		t.Fatal("Rules 为空")
	}
	origPath := first[0].Path
	origMethod := first[0].Method
	first[0].Path = "/api/user/tampered"
	first[0].Auth = apicontract.AuthOpen

	got, ok := apicontract.RuleFor(origMethod, origPath)
	if !ok {
		t.Fatalf("篡改快照后原端点查不到了: %s", origPath)
	}
	if got.Path != origPath {
		t.Fatalf("Rules 返回的不是副本：真相源被改成了 %q", got.Path)
	}
}

// TestHostedScopesDerivedFromWriter 保证托管密钥权限集严格等于写端点的 scope 并集。
//
// 这里独立重算一遍（不调 HostedScopes），否则就是自己断言自己。
func TestHostedScopesDerivedFromWriter(t *testing.T) {
	want := map[string]bool{}
	for _, e := range apicontract.Rules() {
		if e.Auth == apicontract.AuthWriter {
			want[e.Scope] = true
		}
	}
	got := apicontract.HostedScopes()
	if got == nil {
		t.Fatal("HostedScopes 必须返回空切片而不是 nil（对外数组不允许 null）")
	}
	if len(got) != len(want) {
		t.Fatalf("HostedScopes = %v，期望 %d 条（写端点 scope 并集）", got, len(want))
	}
	for _, s := range got {
		if !want[s] {
			t.Errorf("HostedScopes 含非写端点 scope %q", s)
		}
	}
}

// TestGroupsFollowDeclaredOrder 保证分组导航顺序稳定（文档左侧不允许随机跳）。
func TestGroupsFollowDeclaredOrder(t *testing.T) {
	groups := apicontract.Groups()
	if len(groups) == 0 {
		t.Fatal("Groups 为空")
	}
	lastOrder := -1
	for _, g := range groups {
		o := apicontract.GroupOrder(g)
		if o == 99 {
			t.Errorf("分组 %q 未在 GroupOrder 登记，会排到最后", g)
		}
		if o < lastOrder {
			t.Errorf("分组顺序错乱：%q(order=%d) 出现在 order=%d 之后", g, o, lastOrder)
		}
		lastOrder = o
	}
	// 每个分组名都必须至少被一条端点使用，且每组的端点都在 Groups() 里。
	used := map[string]bool{}
	for _, e := range apicontract.Rules() {
		used[e.Group] = true
	}
	for g := range used {
		found := false
		for _, x := range groups {
			if x == g {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("分组 %q 被端点使用但不在 Groups() 里", g)
		}
	}
}

// TestNotExposedNeverLeaksAdminPaths 保证对外文档不暴露管理面拓扑。
//
// 「应用怎么建」只能说"由管理员在后台配置"。写进 /api/admin/... 等于把
// 内部端点清单交给外部读者，而这份文档是给接入方看的。
func TestNotExposedNeverLeaksAdminPaths(t *testing.T) {
	for _, n := range apicontract.NotExposedList() {
		if strings.HasPrefix(n.Path, "/api/admin") {
			t.Errorf("不开放清单含管理面路径 %q", n.Path)
		}
		if n.Title == "" || n.Reason == "" {
			t.Errorf("不开放清单条目缺少标题或原因: %+v", n)
		}
	}
}

// TestPublicPolicyIsOnlyOnOpenAppEndpoints 守公开面策略的声明位置。
//
// 为什么需要：`PublicPolicy` 同时驱动运行期限流、`Cache-Control` 与对外文档。
// 一旦有端点被误标（例如把策略挂到需要凭证的端点上），文档会写"谁都能缓存"，
// 而运行期返回 401；反过来漏标则表现为"文档不说有缓存，客户端每次重取"。
// 两种都不会编译失败。
func TestPublicPolicyIsOnlyOnOpenAppEndpoints(t *testing.T) {
	want := map[string]bool{
		"apps.profile":        false,
		"apps.notices":        false,
		"apps.release":        false,
		"apps.plugin_records": false,
	}
	for _, e := range apicontract.Rules() {
		key := e.Key
		if e.Public == nil {
			if _, tracked := want[key]; tracked {
				t.Errorf("端点 %s 必须声明 PublicPolicy（限流/缓存数值只允许来自契约）", key)
			}
			continue
		}
		if _, tracked := want[key]; !tracked {
			t.Errorf("端点 %s 声明了 PublicPolicy，但它不在公开应用面清单里"+
				"（新增公开面端点必须同时改这里与 docs 生成脚本的预期）", key)
		}
		if e.Auth != apicontract.AuthOpen {
			t.Errorf("端点 %s 不是 AuthOpen 却声明了公开面缓存策略", key)
		}
		if !strings.HasPrefix(e.Path, "/api/apps/") {
			t.Errorf("端点 %s 的路径 %q 不属于 /api/apps/*", key, e.Path)
		}
		if e.Public.RouteLimit <= 0 {
			t.Errorf("端点 %s 的 RouteLimit 必须为正（0 会让中间件直接跳过限流）", key)
		}
		if e.Public.MaxAge <= 0 {
			t.Errorf("端点 %s 的 MaxAge 必须为正（0 等于 no-store）", key)
		}
		want[key] = true
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("缺少端点 %s", k)
		}
	}
}

// TestMustPublicPolicyPanicsOnUnknownKey 锁住"取不到就炸"的契约。
//
// 返回零值的后果是限流 0（=不挂）与 max-age=0（=no-store），也就是端点
// 静默变成了另一个东西；启动即 panic 才是可接受的失败方式。
func TestMustPublicPolicyPanicsOnUnknownKey(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("未知 key 必须 panic，否则会静默退回零值策略")
		}
	}()
	_ = apicontract.MustPublicPolicy("apps.nonexistent")
}
