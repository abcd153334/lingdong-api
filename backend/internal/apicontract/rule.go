package apicontract

import (
	"sort"
	"strings"
)

// 本文件把 catalog 编译成运行期索引，并对外提供只读查询。
//
// 索引是**进程启动时**构建一次的不可变结构：运行期（每个请求）只做两次 map 查找，
// 不分配、不加锁、不查库。契约是编译期常量，没有"热更新"这种需求 ——
// 需要改契约就必须改代码重新发布，这正是"唯一真相源"的代价与价值。

// ruleIndex 的键是 HTTP 方法，值是 gin 路径到端点的映射。
//
// 用两层 map 而不是单层 "GET /path" 字符串键：后者每个请求要拼一次字符串，
// 是每次请求一次堆分配；两层查表零分配。
var ruleIndex = map[string]map[string]Endpoint{}

// duplicates 记录 init 期间发现的重复键。
//
// 刻意**不 panic**：重复声明是"写了两次同一条端点"，它在开发期就该被测试抓住
// （catalog_test.go 的 TestCatalogKeysUnique），而不是让整个进程在 init 阶段崩掉
// —— 后者的表现是"服务起不来且没有 HTTP 状态码"，排查成本远高于一条红色断言。
// 生产路径上行为是"后声明的那条生效"，这与"后注册的路由覆盖先注册"的直觉一致。
var duplicates []string

func init() {
	for _, e := range catalog {
		method := strings.ToUpper(e.Method)
		byPath, ok := ruleIndex[method]
		if !ok {
			byPath = map[string]Endpoint{}
			ruleIndex[method] = byPath
		}
		if _, exists := byPath[e.Path]; exists {
			duplicates = append(duplicates, method+" "+e.Path)
		}
		byPath[e.Path] = e
	}
}

// RuleFor 返回某条已注册路由的契约。
//
// method 用 `c.Request.Method`，path 用 `c.FullPath()`——这两个正是 gin 在
// 中间件执行时能给出的值。**不要**传 `c.Request.URL.Path`：那是真实请求路径，
// 参数段是实际值（`/api/user/keys/42`），与声明（`/api/user/keys/:id`）不相等。
//
// 查不到就是"未声明"，调用方必须按拒绝处理（fail-closed）。
func RuleFor(method, path string) (Endpoint, bool) {
	byPath, ok := ruleIndex[strings.ToUpper(method)]
	if !ok {
		return Endpoint{}, false
	}
	e, ok := byPath[path]
	return e, ok
}

// Rules 返回全量端点声明的**副本**（顺序即文档顺序）。
//
// 返回副本是为了守住"单一真相源"：调用方拿到的是值拷贝，改它不会影响守卫，
// 因此不会出现"文档页改了顺序、守卫跟着变"这种依赖倒挂。
func Rules() []Endpoint {
	out := make([]Endpoint, len(catalog))
	copy(out, catalog)
	return out
}

// NotExposedList 返回不开放清单的副本。
func NotExposedList() []NotExposed {
	out := make([]NotExposed, len(notExposed))
	copy(out, notExposed)
	return out
}

// Duplicates 返回 init 期间发现的重复声明（正常应为空）。
func Duplicates() []string {
	out := make([]string, len(duplicates))
	copy(out, duplicates)
	return out
}

// Scopes 返回全量已使用的 scope（去重、字典序）。
//
// 用于文档页的权限点清单，以及测试里"scope 必须来自封闭词表"的校验。
func Scopes() []string {
	seen := map[string]struct{}{}
	for _, e := range catalog {
		if e.Scope != "" {
			seen[e.Scope] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// HostedScopes 返回托管密钥应有的 scope 集：所有 AuthWriter 端点 scope 的去重并集。
//
// 为什么不手写一份清单：手写就会漂移 —— 新增一条写端点却忘了把 scope 加进
// "托管密钥权限集"，表现是"该端点对托管密钥永远 403"，而文档上它明明是开放的。
// 从契约派生之后，新增写端点自动进入托管密钥可用集，没有第二处要改。
//
// 返回非 nil 空切片（项目铁律：对外数组不出现 null）。
func HostedScopes() []string {
	seen := map[string]struct{}{}
	for _, e := range catalog {
		if e.Auth == AuthWriter && e.Scope != "" {
			seen[e.Scope] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Groups 返回分组名列表（按 GroupOrder 排序，同序时保持 catalog 内出现顺序）。
func Groups() []string {
	seen := map[string]int{}
	order := make([]string, 0, 9)
	for _, e := range catalog {
		if _, ok := seen[e.Group]; ok {
			continue
		}
		seen[e.Group] = len(order)
		order = append(order, e.Group)
	}
	sort.SliceStable(order, func(i, j int) bool {
		oi, oj := GroupOrder(order[i]), GroupOrder(order[j])
		if oi != oj {
			return oi < oj
		}
		return seen[order[i]] < seen[order[j]]
	})
	return order
}

// CountByAuth 返回各级别的端点数量（测试与文档概览用）。
func CountByAuth() map[Auth]int {
	out := map[Auth]int{
		AuthOpen: 0, AuthReader: 0, AuthWriter: 0, AuthSession: 0,
	}
	for _, e := range catalog {
		out[e.Auth]++
	}
	return out
}

// Requirement 用一句话描述某条端点对凭证的要求（文档与错误提示共用）。
func (e Endpoint) Requirement() string {
	switch e.Auth {
	case AuthOpen:
		return "无需凭证"
	case AuthReader:
		return "需要 Authorization: Bearer <会话票或 API 密钥>"
	case AuthWriter:
		return "需要会话票，或具备 " + e.Scope + " 权限的应用托管密钥"
	case AuthSession:
		return "需要 Authorization: Bearer <会话票>（API 密钥不可用）"
	default:
		return "未定义"
	}
}
