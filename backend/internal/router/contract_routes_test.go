package router_test

// 本文件是**契约与真实路由的对账**（B6 门禁）。
//
// 为什么必须双向：
//   - 只从契约→路由查：能拦住「文档里写了、实际 404」；
//   - 只从路由→契约查：能拦住「代码加了接口、文档站永远看不到」。
// 单向的后果都是静默的 —— 前者让用户照着文档调到 404，后者让新接口在文档里
// 压根不存在，而两个方向都不会报错。
//
// 唯一的路径真相源是 `apidoc.Transport.Path`（`relay` 不得再写路径字面量），
// 所以这里不扫源码，直接拿 gin 路由表跟它比。

import (
	"sort"
	"strings"
	"testing"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/router"
)

// ginPath 把契约写法 `/v1/video/generations/{id}` 折成 gin 的 `:id` 写法。
//
// 两种写法各有用途（契约面向读者用大括号，gin 用冒号），转换只在这里做：
// 别处各写一份，迟早有一处把 `{index}` 漏掉，然后这条端点被静默跳过。
//
// 还要处理「参数内后缀动作」：`/v1beta/models/{model}:generateContent` 在 gin 里
// 注册成 `POST /v1beta/models/:model` —— 参数的**唯一终止符是 `/`**，所以 `:action`
// 是参数值的一部分，注册时不存在。这条规则对所有路径都成立（段内不可能有第二个
// 通配），不是 Gemini 的特例。
func ginPath(p string) string {
	p = strings.ReplaceAll(p, "{", ":")
	for {
		i := strings.IndexByte(p, '}')
		if i < 0 {
			break
		}
		tail := p[i+1:]
		if j := strings.IndexByte(tail, '/'); j >= 0 {
			// 占位符后面还有路径段：丢掉 `}:action` 尾巴，保留后续 `/...`。
			p = p[:i] + tail[j:]
			continue
		}
		// 占位符在末尾：`}:action` 整段丢弃。
		p = p[:i]
		break
	}
	return p
}

// routeKey 是路由表/契约的比对键。
func routeKey(method, path string) string { return method + " " + path }

// gatewayScope 判断一条路由是否属于**对外 API 面**（要进文档站的那些）。
//
// 管理台 /api/admin/* 与门户 /api/portal/* 不在文档站里（它们进 OpenAPI 是另一件事），
// 纳进来只会逼出一长串白名单。
func gatewayScope(path string) bool {
	return strings.HasPrefix(path, "/v1/") ||
		// /v1beta 是 Gemini 协议面：它与 /v1 一样是「对外 API 面」。漏掉这一行的
		// 后果比多写白名单严重得多 —— 整棵树的「路由有、契约没有」被静默跳过，
		// 新增端点漏登记永远不报错。
		strings.HasPrefix(path, "/v1beta/") ||
		strings.HasPrefix(path, "/f/") ||
		// /suno 是 new-api 的 SUNO 协议面：同样是对外 API 面，漏掉它
		// 新增的三条路由就永远不进「路由↔契约」双向对账。
		strings.HasPrefix(path, "/suno/") ||
		strings.HasPrefix(path, "/dashboard/")
}

// contractRoutes 把 apidoc 的端点表折成路由键集合（含 Aliases）。
//
// 这里**不**做「去掉 /v1 前缀」的展开：`/dashboard/billing/*` 这类历史兼容路径
// 在反向检查里单独处理。展开进来会让 `/files`、`/models` 这种本不存在的路径
// 在正向检查里被当成「已声明」，正向检查就失去了意义。
func contractRoutes() map[string]apidoc.Kind {
	out := map[string]apidoc.Kind{}
	for _, k := range apidoc.Kinds() {
		t, ok := apidoc.TransportOf(k)
		if !ok {
			continue
		}
		paths := append([]string{t.Path}, t.Aliases...)
		for _, p := range paths {
			out[routeKey(t.Method, ginPath(p))] = k
		}
	}
	return out
}

// TestEveryContractEndpointIsRegistered 查「文档里写了、路由里没有」。
func TestEveryContractEndpointIsRegistered(t *testing.T) {
	registered := map[string]bool{}
	byPath := map[string][]string{}
	for _, ri := range router.New().Routes() {
		registered[routeKey(ri.Method, ri.Path)] = true
		byPath[ri.Path] = append(byPath[ri.Path], ri.Method)
	}

	contract := contractRoutes()
	keys := make([]string, 0, len(contract))
	for k := range contract {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		path := strings.SplitN(key, " ", 2)[1]
		if registered[key] {
			continue
		}
		// 媒体代理用 `r.Any` 注册（POST 打进来必须命中它而不是掉进 NoRoute 的
		// /v1 兜底被当成网关端点），契约里只能写一个方法。这里只要求路径存在。
		if contract[key] == apidoc.KindMediaProxy && len(byPath[path]) > 0 {
			continue
		}
		// 文件直链 `/f/{id}/{name}` 是 NoRoute 前缀分发的，不在路由表里。
		//
		// gin 的 catch-all 与静态子路由同层会冲突，项目因此选择「NoRoute 里按前缀
		// 分发」而不是 `r.GET("/f/*path")`。这条端点的可达性由 e2e 覆盖。
		if contract[key] == apidoc.KindFileDirect && strings.HasPrefix(path, "/f/") {
			continue
		}
		if len(byPath[path]) > 0 {
			t.Errorf("契约声明了 %s，但该路径只注册了 %v", key, byPath[path])
			continue
		}
		t.Errorf("契约声明了 %s，但路由表里没有这条路径（文档会指向 404）", key)
	}
}

// notOfferedRoutes 是「路由存在、但**刻意不提供**」的端点：它们恒返回 501。
//
// 这类端点不该在文档站单列一篇（价值低），但放行**必须有据可查**：
// 下面的用例会验证「文档里确实提到了这个路径」，否则这份白名单迟早退化成
// 「新增接口时先把路径加进来」的垃圾桶 —— 那正是这条双向对账要防的事。
var notOfferedRoutes = map[string]apidoc.Kind{
	// count_tokens 显式 501：给错数字比不给更难查（客户端会按它管理上下文）。
	// 说明见 KindMessages 的 Notes。
	"POST /v1/messages/count_tokens": apidoc.KindMessages,
}

// TestNoUndocumentedGatewayRoute 查「路由里有、契约里没有」。
func TestNoUndocumentedGatewayRoute(t *testing.T) {
	contract := contractRoutes()

	// 白名单自检：放行的每条路径都必须在对应端点的文档里被点名。
	for key, k := range notOfferedRoutes {
		doc, ok := apidoc.EndpointDocOf(k)
		if !ok {
			t.Fatalf("白名单里的 %s 指向了不存在的端点契约 %q", key, k)
		}
		path := strings.TrimPrefix(key, "POST ")
		mentioned := false
		for _, n := range doc.Notes {
			if strings.Contains(n, path) {
				mentioned = true
				break
			}
		}
		if !mentioned {
			t.Errorf("白名单里的 %s 在 %q 的文档 Notes 里没有说明：用户会找不到它为什么 501", key, k)
		}
	}

	for _, ri := range router.New().Routes() {
		if !gatewayScope(ri.Path) {
			continue
		}
		if _, ok := contract[routeKey(ri.Method, ri.Path)]; ok {
			continue
		}
		if _, ok := notOfferedRoutes[routeKey(ri.Method, ri.Path)]; ok {
			continue
		}
		// 历史兼容路径：部分客户端按 OpenAI 老习惯直连 /dashboard/billing/*，
		// 契约只登记规范形式 /v1/dashboard/billing/*。
		if strings.HasPrefix(ri.Path, "/dashboard/") {
			if _, ok := contract[routeKey(ri.Method, "/v1"+ri.Path)]; ok {
				continue
			}
		}
		// HEAD 是 GET 的传输变体（只回头部，内容与 GET 相同），文档不单列一篇；
		// 只要对应的 GET 已登记进契约，HEAD 就不算「未登记路由」。
		// 只放行 HEAD：其它方法的新增仍然必须先在契约里登记，否则文档站看不到。
		if ri.Method == "HEAD" {
			if _, ok := contract[routeKey("GET", ri.Path)]; ok {
				continue
			}
		}
		// 媒体代理 Any 展开出来的其它方法（POST/PUT/DELETE/…）。
		//
		// 这不是「多注册了方法」，而是刻意的：见 router.go 里的注释 ——
		// 只注册 GET/HEAD 时，一个 POST /v1/media/... 会掉进 NoRoute 被当网关端点。
		if ri.Path == "/v1/media/:token" {
			continue
		}
		t.Errorf("对外路由 %s %s 没有登记进 apidoc（文档站看不到它）", ri.Method, ri.Path)
	}
}
