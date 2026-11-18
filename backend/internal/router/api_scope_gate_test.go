package router_test

// 本文件是**契约与真实路由的对账**，以及鉴权守卫的行为门禁。
//
// 与 contract_routes_test.go 的分工：那个文件管 `/v1` 网关面（真相源是 apidoc），
// 这个文件管 `/api/user` 对外面（真相源是 apicontract）。两者都是双向对账，
// 但对象不同、失败信息不同，混在一个文件里会让"到底是哪一面漂了"难以判断。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/apicontract"
	"lingdong-api/internal/middleware"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/router"
)

// 对外面的路径前缀（也是本文件所有断言的筛选条件）。
//
// 有三个：`/api/user/` 是主面，`/api/auth/` 是只读的接入能力探测
// （无凭证、只回准入策略），`/api/apps/` 是终端的公开面
// （无凭证，三条端点分别读品牌+维护状态 / 公告 / 版本）。把它们都算进来，双向对账才能发现
// "文档写了但路由没注册"——少一个前缀，那个前缀下的端点就永远不被对账。
var exposedPrefixes = []string{"/api/user/", "/api/auth/", "/api/apps/"}

func isExposedPath(p string) bool {
	for _, pre := range exposedPrefixes {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// concretePath 把声明里的参数段换成具体值，用于发真实请求。
//
// 不换也能发（gin 会把 `:id` 当成字面值匹配），但那样一旦 handler 内做了
// 数字解析就会先失败，测试失败原因会指向"参数不是数字"而不是真正要验的东西。
func concretePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if strings.HasPrefix(s, ":") {
			segs[i] = "1"
		}
	}
	return strings.Join(segs, "/")
}

// scopeEnvelope 是只取判定所需字段的响应体。
type scopeEnvelope struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	ErrorCode string `json:"error_code"`
}

func decodeScopeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) scopeEnvelope {
	t.Helper()
	var env scopeEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是 JSON 信封: %s", rec.Body.String())
	}
	return env
}

// TestContractMatchesUserAPIRoutes 双向对账：契约 ↔ 真实路由。
//
// 两个方向各自拦截一类事故：
//   - 契约有、路由无 -> 文档写了 404 的地址（接入方照着调，永远失败）；
//   - 路由有、契约无 -> 守卫 fail-closed，该端点永远 403（联调时才发现）。
//
// 单向检查对这两类都无效：前者只能靠"实际请求一次"，后者只能靠"看日志"。
func TestContractMatchesUserAPIRoutes(t *testing.T) {
	declared := map[string]apicontract.Endpoint{}
	for _, e := range apicontract.Rules() {
		declared[e.Method+" "+e.Path] = e
	}

	registered := map[string]bool{}
	for _, ri := range router.New().Routes() {
		if !isExposedPath(ri.Path) {
			continue
		}
		// HEAD 是 GET 的传输变体（同头无体），契约不为它单独声明 ——
		// ScopeGuard 里有对应的 HEAD→GET 回退，这里按 GET 归并，
		// 否则每加一条 HEAD 都要在契约里复制一份只差方法的声明。
		method := ri.Method
		if method == http.MethodHead {
			method = http.MethodGet
		}
		registered[method+" "+ri.Path] = true
	}
	if len(registered) == 0 {
		t.Fatal("路由表里没有任何 /api/user/ 端点，测试前提不成立")
	}

	for k := range declared {
		if !registered[k] {
			t.Errorf("契约声明了但路由未注册: %s", k)
		}
	}
	for k := range registered {
		if _, ok := declared[k]; !ok {
			t.Errorf("路由已注册但契约未声明（该端点会 fail-closed 403）: %s", k)
		}
	}
}

// TestDeclaredLevelDistribution 锁死各级别的端点数量。
//
// 这不是"统计信息"，而是安全边界：AuthSession 少了 3 条意味着有 3 条本该
// 只认会话票的端点降级成了别的级别。数字变了就必须有人解释为什么。
//
// 阶段期望值：P1 = 6/1/0/17（24）；P3 = 6/13/0/17（36）；P4 加入 3 条写端点 = 6/13/3/17（39）；P5 加入 apps 读写 = 6/14/3/18（41）。
// 开放面补齐（lab 接入测试）：+captcha/+register_contact_code（open）、+orders/:id/qrcode（reader）、+redeem（writer）= 8/15/4/18（45）。
// 托管密钥自举与运行时面：+keys.self_rotate/+keys.health（hosted_key，不计入四级）、
// +apps.runtime（open）= 9/15/4/21（49）→ 应用级后台 = 10/15/4/21（50）。
// 公开面三分：-apps.runtime，+apps.profile/+apps.notices/+apps.release = 12/15/4/21（52）。
// 插件数据对外只读：+apps.plugin_records（open，默认关闭但不改变鉴权级别）= 13/15/4/21（53）。
func TestDeclaredLevelDistribution(t *testing.T) {
	got := apicontract.CountByAuth()
	want := map[apicontract.Auth]int{
		apicontract.AuthOpen:    13,
		apicontract.AuthReader:  15,
		apicontract.AuthWriter:  4,
		apicontract.AuthSession: 21,
	}
	for level, n := range want {
		if got[level] != n {
			t.Errorf("级别 %s 的端点数 = %d，期望 %d", level, got[level], n)
		}
	}
	if total := got[apicontract.AuthOpen] + got[apicontract.AuthReader] +
		got[apicontract.AuthWriter] + got[apicontract.AuthSession]; total != expectedRuleCountForRouter {
		t.Errorf("各级别合计 = %d，期望 %d", total, expectedRuleCountForRouter)
	}
}

// expectedRuleCountForRouter 与 apicontract 包的常量保持一致。
//
// 刻意重复一次而不是导出：两个包的期望值同时被改，才说明是真的有意变更；
// 只改一处会红，这正是我们想要的摩擦。
const expectedRuleCountForRouter = 53

// TestAnonymousRejectedOnCredentialRequiredRoutes 无凭证访问 reader / session 端点必须 401。
//
// 为什么连 AuthReader 也要断言：这组端点的只读面本身合法，但"未鉴权就返回数据"
// 是典型的越权。历史上这类问题的形态往往是"某个分支忘了 return"，
// 而不是整条链没挂中间件。
func TestAnonymousRejectedOnCredentialRequiredRoutes(t *testing.T) {
	eng := router.New()
	checked := 0
	for _, e := range apicontract.Rules() {
		if e.Auth != apicontract.AuthReader && e.Auth != apicontract.AuthSession &&
			e.Auth != apicontract.AuthWriter {
			continue
		}
		req := httptest.NewRequest(e.Method, "http://example.com"+concretePath(e.Path), nil)
		rec := httptest.NewRecorder()
		eng.ServeHTTP(rec, req)
		checked++
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s（%s）无凭证应为 401，实际 %d: %s",
				e.Method, e.Path, e.Auth, rec.Code, rec.Body.String())
			continue
		}
		// 必须是平台信封，且 code 与 error_code 同时给出：
		// 只给 code 的话，接入方无法区分"没带凭证"与"凭证失效"。
		env := decodeScopeEnvelope(t, rec)
		if env.ErrorCode == "" {
			t.Errorf("%s %s 的 401 缺少 error_code", e.Method, e.Path)
		}
	}
	if checked == 0 {
		t.Fatal("没有可校验的 reader/session 端点")
	}
}

// TestCookieOnlyRejected 只带会话 Cookie、不带 Authorization 必须 401。
//
// 这是"凭证按路径隔离"铁律的行为验证：`/api/user/*` 永远不读 Cookie。
// 一旦有人把门户的 authenticate() 复制过来，浏览器就能在这条没有 CSRF 校验的
// 组里获得一个有效会话——而功能测试会全绿（因为浏览器确实带着 Cookie）。
func TestCookieOnlyRejected(t *testing.T) {
	eng := router.New()
	// 三条覆盖两种级别（reader 一条、session 两条），并刻意包含一个"看起来
	// 最该被 Cookie 放行"的 /sessions（它读的就是会话本身）。
	paths := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/user/self"},
		{http.MethodGet, "/api/user/sessions"},
		{http.MethodGet, "/api/user/keys"},
	}
	for _, p := range paths {
		req := httptest.NewRequest(p.method, "http://example.com"+p.path, nil)
		req.AddCookie(&http.Cookie{Name: "ld_session", Value: "lds_forged_cookie_value"})
		rec := httptest.NewRecorder()
		eng.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s 只带 Cookie 应为 401，实际 %d", p.method, p.path, rec.Code)
		}
	}
}

// TestUndeclaredRouteFailClosed 未声明路由必须 403，而不是"按默认权限放行"。
//
// 用独立引擎而不是主引擎：主引擎里所有路由都已声明（上一条测试保证），
// 造不出"已注册但未声明"的状态。独立引擎能让这个分支真正被执行到。
func TestUndeclaredRouteFailClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	eng := gin.New()
	eng.GET("/api/user/undeclared-probe", middleware.ScopeGuard(), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/api/user/undeclared-probe", nil)
	rec := httptest.NewRecorder()
	eng.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("未声明路由应为 403，实际 %d: %s", rec.Code, rec.Body.String())
	}
	env := decodeScopeEnvelope(t, rec)
	if env.ErrorCode != errs.CodeScopeUndeclared {
		t.Errorf("error_code = %q，期望 %q", env.ErrorCode, errs.CodeScopeUndeclared)
	}
}
