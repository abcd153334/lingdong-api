package controller

// 本文件守的是**管理台接口手册的可见性与信封形态**。
//
// 两件事必须分开断言：
//
//  1. 缓存指令必须是 private。管理台文档描述的是完整的对外权限模型，
//     一旦被中间代理按 public 缓存，它会被发给下一个不带凭证的请求。
//     `docs.go` 里的 writeDocsETag 原本硬编码 public（公开文档站用），
//     直接复用同一个写出函数就会把管理台内容也标成可公共缓存。
//
//  2. OpenAPI 导出必须是**干净的 OpenAPI**，不能包平台信封。
//     包了信封的话，接入方把它导入 Postman / openapi-generator 会解析失败，
//     而人眼看文件又"确实有 openapi 字段"，排查成本很高。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/apicontract"
)

// runHandler 直接调用 handler（不经过 AdminAuth：鉴权已由路由与门禁测试覆盖）。
func runHandler(t *testing.T, h gin.HandlerFunc, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	eng := gin.New()
	eng.Handle(method, path, h)
	req := httptest.NewRequest(method, "http://example.com"+path, nil)
	rec := httptest.NewRecorder()
	eng.ServeHTTP(rec, req)
	return rec
}

// TestAdminAPIDocsCatalogShape 目录必须自洽：分组覆盖全部条目、计数等于实际条数。
func TestAdminAPIDocsCatalogShape(t *testing.T) {
	rec := runHandler(t, AdminAPIDocsCatalog, http.MethodGet, "/api/admin/api-docs")
	if rec.Code != http.StatusOK {
		t.Fatalf("目录应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Groups []string `json:"groups"`
		Items  []struct {
			Key   string `json:"key"`
			Group string `json:"group"`
			Auth  string `json:"auth"`
			Media string `json:"media"`
		} `json:"items"`
		NotExposed []struct {
			Title  string `json:"title"`
			Reason string `json:"reason"`
		} `json:"not_exposed"`
		Counts struct {
			Total, Open, Reader, Writer, Session int
			// 显式 tag：字段名 HostedKey 与 JSON 的 hosted_key 不是大小写关系，
			// 不加 tag 会静默解析成 0，于是"求和校验"变成一条永远成立的假断言。
			HostedKey int `json:"hosted_key"`
		} `json:"counts"`
		HostedScopes []string `json:"hosted_scopes"`
	}
	// 目录响应有平台信封：先解包 data。
	if err := json.Unmarshal(decodeEnvelopeData(t, rec), &out); err != nil {
		t.Fatalf("data 解析失败: %v", err)
	}

	if len(out.Items) != len(apicontract.Rules()) {
		t.Fatalf("目录条目 %d 条，契约 %d 条", len(out.Items), len(apicontract.Rules()))
	}
	if out.Counts.Total != len(out.Items) {
		t.Fatalf("counts.total=%d，实际条目 %d", out.Counts.Total, len(out.Items))
	}
	if sum := out.Counts.Open + out.Counts.Reader + out.Counts.Writer + out.Counts.Session + out.Counts.HostedKey; sum != out.Counts.Total {
		t.Fatalf("各级别之和 %d ≠ total %d（级别枚举漏了一种）", sum, out.Counts.Total)
	}
	// 每个条目的分组必须在 groups 里，否则前端左侧导航会出现"挂不上"的条目。
	groups := map[string]bool{}
	for _, g := range out.Groups {
		groups[g] = true
	}
	for _, it := range out.Items {
		if !groups[it.Group] {
			t.Errorf("%s 的分组 %q 不在 groups 列表里", it.Key, it.Group)
		}
		if it.Auth == "" || it.Media == "" {
			t.Errorf("%s 缺少 auth 或 media", it.Key)
		}
	}
	if len(out.NotExposed) == 0 {
		t.Error("不开放清单不能为空（接入方最常问的就是为什么没有某个能力）")
	}
	for _, n := range out.NotExposed {
		if n.Title == "" || n.Reason == "" {
			t.Errorf("不开放条目必须同时给出标题与原因: %+v", n)
		}
	}
	if len(out.HostedScopes) == 0 {
		t.Error("hosted_scopes 不能为空：接入方要知道托管密钥默认能做什么")
	}
}

// TestAdminAPIDocsCacheIsPrivate 三个端点的 Cache-Control 都必须是 private。
func TestAdminAPIDocsCacheIsPrivate(t *testing.T) {
	cases := []struct {
		name string
		h    gin.HandlerFunc
		path string
	}{
		{"目录", AdminAPIDocsCatalog, "/api/admin/api-docs"},
		{"OpenAPI", AdminAPIDocsOpenAPI, "/api/admin/api-docs/openapi.json"},
		{"Markdown", AdminAPIDocsMarkdown, "/api/admin/api-docs/markdown"},
	}
	for _, tc := range cases {
		rec := runHandler(t, tc.h, http.MethodGet, tc.path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s 应 200，实际 %d", tc.name, rec.Code)
		}
		cc := rec.Header().Get("Cache-Control")
		if !strings.Contains(cc, "private") {
			t.Errorf("%s 的 Cache-Control 必须含 private，实际 %q", tc.name, cc)
		}
		if strings.Contains(cc, "public") {
			t.Errorf("%s 的 Cache-Control 不得含 public，实际 %q", tc.name, cc)
		}
		if rec.Header().Get("ETag") == "" {
			t.Errorf("%s 必须带 ETag（管理台文档是静态内容，重复拉取应命中 304）", tc.name)
		}
	}
}

// TestAdminAPIDocsOpenAPIIsPureFile 导出必须是干净的 OpenAPI，不是平台信封。
func TestAdminAPIDocsOpenAPIIsPureFile(t *testing.T) {
	rec := runHandler(t, AdminAPIDocsOpenAPI, http.MethodGet, "/api/admin/api-docs/openapi.json")
	if rec.Code != http.StatusOK {
		t.Fatalf("OpenAPI 应 200，实际 %d", rec.Code)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("导出内容不是合法 JSON: %v", err)
	}
	// 顶层必须直接是 OpenAPI 字段 —— 出现 code/message/data 说明包了信封。
	if _, wrapped := doc["data"]; wrapped {
		t.Fatal("OpenAPI 导出不得包裹平台信封（导入工具会解析失败）")
	}
	if doc["openapi"] != "3.1.0" {
		t.Fatalf("openapi 字段应为 3.1.0，实际 %v", doc["openapi"])
	}
	paths, _ := doc["paths"].(map[string]any)
	if len(paths) == 0 {
		t.Fatal("paths 不能为空")
	}
	// 所有路径都必须是 OpenAPI 形式（`{}`），不能残留 gin 的 `:`。
	for p := range paths {
		if strings.Contains(p, "/:") {
			t.Errorf("路径 %q 残留 gin 参数写法（应为 {id}）", p)
		}
	}
}

// TestAdminAPIDocsMarkdownIsText Markdown 导出必须是纯文本，不是信封。
func TestAdminAPIDocsMarkdownIsText(t *testing.T) {
	rec := runHandler(t, AdminAPIDocsMarkdown, http.MethodGet, "/api/admin/api-docs/markdown")
	if rec.Code != http.StatusOK {
		t.Fatalf("Markdown 应 200，实际 %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("Content-Type 应为 text/markdown，实际 %q", ct)
	}
	body := rec.Body.String()
	if strings.HasPrefix(strings.TrimSpace(body), "{") {
		t.Fatal("Markdown 导出不得是 JSON 信封")
	}
	if !strings.Contains(body, "# 灵动开放接口") {
		t.Fatalf("Markdown 缺少标题，实际开头：%.80s", body)
	}
}

// TestAdminAPIDocsCatalogContractShape 每行的**交互契约**必须完整且自洽。
//
// 前一条用例只验了"有这一行"（分组、级别、计数）。这一条验的是"这一行的
// 内容能不能拿来调试"：参数表、返回字段、错误码、试运行三元组、示例。
// 两类问题分开守，是因为它们的失败方式不同：
//   - 缺行 → 接入方看不到这个能力；
//   - 行在但参数为空 → 页面显示"无参数"，接入方照着发请求，然后 400。
func TestAdminAPIDocsCatalogContractShape(t *testing.T) {
	rec := runHandler(t, AdminAPIDocsCatalog, http.MethodGet, "/api/admin/api-docs")
	if rec.Code != http.StatusOK {
		t.Fatalf("目录应 200，实际 %d", rec.Code)
	}
	var out struct {
		Items []struct {
			Key    string `json:"key"`
			Auth   string `json:"auth"`
			Media  string `json:"media"`
			NoData bool   `json:"no_data"`
			Params *[]struct {
				Name     string   `json:"name"`
				In       string   `json:"in"`
				Type     string   `json:"type"`
				Required string   `json:"required"`
				Enum     []string `json:"enum"`
			} `json:"params"`
			Returns *[]struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"returns"`
			Errors *[]struct {
				Code string `json:"code"`
				When string `json:"when"`
			} `json:"errors"`
			Sortable *[]string `json:"sortable"`
			Try      struct {
				Callable bool   `json:"callable"`
				Hint     string `json:"hint"`
				Risk     string `json:"risk"`
			} `json:"try"`
		} `json:"items"`
		Callable int `json:"callable"`
		Links    struct {
			Catalog, OpenAPI, Markdown string
			SwaggerUI                  string `json:"swagger_ui"`
		} `json:"links"`
	}
	if err := json.Unmarshal(decodeEnvelopeData(t, rec), &out); err != nil {
		t.Fatalf("data 解析失败: %v", err)
	}

	callable := 0
	for _, it := range out.Items {
		// 四个数组切片都用指针接：`null` 会解成 nil，`[]` 解成空切片指针。
		// 项目铁律：对外数组不能是 null（前端 `.length` 直接抛错，整页白屏）。
		if it.Params == nil {
			t.Errorf("%s 的 params 是 null（应为 []）", it.Key)
		}
		if it.Returns == nil {
			t.Errorf("%s 的 returns 是 null（应为 []）", it.Key)
		}
		if it.Errors == nil {
			t.Errorf("%s 的 errors 是 null（应为 []）", it.Key)
		}
		if it.Sortable == nil {
			t.Errorf("%s 的 sortable 是 null（应为 []）", it.Key)
		}

		// try.callable 必须与 auth 级别一致：契约里 `Tries()` 的定义是
		// "既不是 session、也不是 hosted_key"。两个反例都会让按钮点了必失败：
		// session 级会 403 session_required，hosted_key 级会 401（浏览器没有托管密钥）。
		wantCallable := it.Auth != "session" && it.Auth != "hosted_key"
		if it.Try.Callable != wantCallable {
			t.Errorf("%s（auth=%s）的 try.callable=%v，应为 %v", it.Key, it.Auth, it.Try.Callable, wantCallable)
		}
		if it.Try.Callable {
			callable++
			if it.Try.Hint != "" {
				t.Errorf("%s 可试运行却带了 hint %q（应留空）", it.Key, it.Try.Hint)
			}
		} else if it.Try.Hint == "" {
			t.Errorf("%s 不可试运行但没给 hint：页面只能显示一个禁用的按钮", it.Key)
		}
		if it.Try.Risk != "safe" && it.Try.Risk != "confirm" {
			t.Errorf("%s 的 try.risk=%q，只能是 safe / confirm", it.Key, it.Try.Risk)
		}

		// 成功即空体（files.delete 的 204）：不能同时声明返回字段。
		if it.NoData && it.Returns != nil && len(*it.Returns) > 0 {
			t.Errorf("%s 标了 no_data 却声明了 %d 个返回字段", it.Key, len(*it.Returns))
		}
		// JSON 端点必须有返回字段（否则接入方不知道 data 里有什么）。
		if it.Media == "json" && !it.NoData && it.Returns != nil && len(*it.Returns) == 0 {
			t.Errorf("%s 是 JSON 端点但没有 returns", it.Key)
		}

		// 参数表的枚举值必须都是非空文本（空串会在前端渲染成一个空选项）。
		if it.Params != nil {
			for _, p := range *it.Params {
				if p.Name == "" || p.In == "" || p.Type == "" {
					t.Errorf("%s 有一条参数缺 name/in/type: %+v", it.Key, p)
				}
				if p.Required != "required" && p.Required != "optional" && p.Required != "conditional" {
					t.Errorf("%s.%s 的 required=%q，三态之外的值前端无法渲染", it.Key, p.Name, p.Required)
				}
				for _, e := range p.Enum {
					if e == "" {
						t.Errorf("%s.%s 的 enum 里有空串", it.Key, p.Name)
					}
				}
			}
		}
		// 错误码必须都有"什么时候会撞上"的说明：只给码等于让接入方去猜。
		if it.Errors != nil {
			for _, e := range *it.Errors {
				if e.Code == "" || e.When == "" {
					t.Errorf("%s 的错误码条目缺 code/when: %+v", it.Key, e)
				}
			}
		}
	}

	if out.Callable != callable {
		t.Fatalf("顶层 callable=%d，实际可试运行 %d 条（两处算法漂移）", out.Callable, callable)
	}

	// links 四个都不能空，且必须落在管理台前缀下（否则前端拼出站外地址）。
	for name, v := range map[string]string{
		"catalog": out.Links.Catalog, "openapi": out.Links.OpenAPI,
		"markdown": out.Links.Markdown, "swagger_ui": out.Links.SwaggerUI,
	} {
		if v == "" {
			t.Errorf("links.%s 为空", name)
		}
		if !strings.HasPrefix(v, "/api/admin/api-docs") {
			t.Errorf("links.%s = %q，不在 /api/admin/api-docs 之下", name, v)
		}
	}
}

// decodeEnvelopeData 取出平台信封里的 data 原始字节。
func decodeEnvelopeData(t *testing.T, rec *httptest.ResponseRecorder) []byte {
	t.Helper()
	var env struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是 JSON: %v，原文 %.120s", err, rec.Body.String())
	}
	if env.Code != 0 {
		t.Fatalf("信封 code = %d，期望 0", env.Code)
	}
	return env.Data
}
