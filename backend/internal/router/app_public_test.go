package router_test

// 应用公开面（免凭证）的 HTTP 级用例。
//
// 这一面在此前**零测试覆盖**。它同时承担三件事，任何一件坏了都是线上事故：
//   1. 返回体是白名单（多一个字段就是把经营数据交出去）；
//   2. 缓存头与 304 正确（错了就是每次冷启动 5 条 SQL 或者永远看不到更新）；
//   3. 停用语义（回 404 会让"能对话、界面显示应用不存在"）。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/apicontract"
	"lingdong-api/internal/db"
	"lingdong-api/internal/middleware"
	"lingdong-api/internal/model"
	"lingdong-api/internal/router"
	appsvc "lingdong-api/internal/service/app"
)

// publicGet 发一个匿名 GET（不带 Authorization），可选带 If-None-Match。
func publicGet(t *testing.T, r *gin.Engine, path, ifNoneMatch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://example.com"+path, nil)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestAppPublicProfile(t *testing.T) {
	r, _ := setupUserAPI(t)
	app := seedApp(t, "pub-profile")
	appsvc.ResetPublicCacheForTest()
	appsvc.ResetMaintenanceForTest()

	if err := db.DB.Model(&model.App{}).Where("id = ?", app.ID).
		Updates(map[string]any{"display_name": "灵动画影", "tagline": "一句话"}).Error; err != nil {
		t.Fatalf("写品牌失败: %v", err)
	}

	rec := publicGet(t, r, "/api/apps/"+app.Slug+"/profile", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=60" {
		t.Fatalf("Cache-Control = %q，期望 public, max-age=60", cc)
	}
	etag := rec.Header().Get("ETag")
	if !strings.HasPrefix(etag, `W/"`) {
		t.Fatalf("必须是弱 ETag（响应会过 gzip），实际 %q", etag)
	}

	d := dataOf(t, rec)
	if d["slug"] != app.Slug {
		t.Fatalf("slug = %v，期望 %q", d["slug"], app.Slug)
	}
	brand, _ := d["brand"].(map[string]any)
	if brand["display_name"] != "灵动画影" || brand["tagline"] != "一句话" {
		t.Fatalf("品牌字段不对: %v", brand)
	}
	maint, _ := d["maintenance"].(map[string]any)
	if maint["enabled"] != false {
		t.Fatalf("未开维护时 enabled 应为 false: %v", maint)
	}

	// 白名单：在线状态与后台辨识名都不得出现在公开面。
	if _, ok := d["status"]; ok {
		t.Fatal("公开面不得下发在线状态（经营情报：offline 等于\"该客户当前没流量\"）")
	}
	if _, ok := d["name"]; ok {
		t.Fatal("公开面不得下发后台辨识名（brand.display_name 已覆盖展示需求）")
	}
}

// TestAppPublicConditionalRevalidation 条件请求命中时必须 304 且空体。
//
// 删掉它会怎样：终端每 60 秒重拉一次完整品牌/公告，公告最坏 40KB，
// 拆分省下来的带宽又原样还回去。
func TestAppPublicConditionalRevalidation(t *testing.T) {
	r, _ := setupUserAPI(t)
	app := seedApp(t, "pub-cond")
	appsvc.ResetPublicCacheForTest()

	first := publicGet(t, r, "/api/apps/"+app.Slug+"/profile", "")
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("首次响应必须带 ETag")
	}

	second := publicGet(t, r, "/api/apps/"+app.Slug+"/profile", etag)
	if second.Code != http.StatusNotModified {
		t.Fatalf("带匹配的 If-None-Match 应 304，实际 %d %s", second.Code, second.Body.String())
	}
	if second.Body.Len() != 0 {
		t.Fatalf("304 不得带响应体，实际 %d 字节", second.Body.Len())
	}
	// 304 也要带缓存头，否则客户端会立刻当"没有缓存信息"重新发完整请求。
	if cc := second.Header().Get("Cache-Control"); cc != "public, max-age=60" {
		t.Fatalf("304 的 Cache-Control = %q", cc)
	}
}

// TestAppPublicNoticesAndReleaseShape 空数据必须是 [] 与 null，不是 0 值。
func TestAppPublicNoticesAndReleaseShape(t *testing.T) {
	r, _ := setupUserAPI(t)
	app := seedApp(t, "pub-empty")
	appsvc.ResetPublicCacheForTest()

	rec := publicGet(t, r, "/api/apps/"+app.Slug+"/notices", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("notices 应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	raw := rec.Body.String()
	// 直接比对字面量：JSON 反序列化会把 null 变成 nil，用 map 断言会漏掉
	// "null 与 [] 的区别"，而终端 .map(null) 就是白屏。
	if !strings.Contains(raw, `"notices":[]`) {
		t.Fatalf("无公告时必须是 []，实际 %s", raw)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=30" {
		t.Fatalf("notices 的 Cache-Control = %q，期望 public, max-age=30", cc)
	}

	rec = publicGet(t, r, "/api/apps/"+app.Slug+"/release", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("release 应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"release":null`) {
		t.Fatalf("无发布版本时必须是 null，实际 %s", rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=300" {
		t.Fatalf("release 的 Cache-Control = %q，期望 public, max-age=300", cc)
	}
}

// TestAppPublicUnknownSlugIsNotFound 不存在的标识 → 404 + no-store。
//
// no-store 必须断言：否则 404 会被浏览器/代理记住，应用建好后终端还在失败。
func TestAppPublicUnknownSlugIsNotFound(t *testing.T) {
	r, _ := setupUserAPI(t)
	appsvc.ResetPublicCacheForTest()

	rec := publicGet(t, r, "/api/apps/no-such-app/profile", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("应 404，实际 %d %s", rec.Code, rec.Body.String())
	}
	if env := decodeEnv(t, rec); env.ErrorCode != "app_not_found" {
		t.Fatalf("错误码 = %q，期望 app_not_found", env.ErrorCode)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("404 必须 no-store，实际 %q", cc)
	}

	// 格式非法与"不存在"同码，避免把参数格式当成枚举面。
	rec = publicGet(t, r, "/api/apps/BAD_SLUG!/profile", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("非法 slug 应 404，实际 %d", rec.Code)
	}
	if env := decodeEnv(t, rec); env.ErrorCode != "app_not_found" {
		t.Fatalf("非法 slug 的错误码 = %q，期望 app_not_found", env.ErrorCode)
	}
}

// TestAppPublicDisabledAppStillServed 停用应用仍要能读品牌。
//
// 这是一条**反向守卫**：`errs.go` 里 `app_not_found` 的注释带限定语
// 「或应用已停用**且调用方尚未持有密钥**」，说的是签发路径。若有人照字面把
// 公开面也改成 404，就会出现"存量密钥能正常调 /v1、终端界面却显示应用不存在"。
// 停用只拦新签发；要断流量用维护模式。
func TestAppPublicDisabledAppStillServed(t *testing.T) {
	r, _ := setupUserAPI(t)
	app := seedApp(t, "pub-disabled")
	appsvc.ResetPublicCacheForTest()
	appsvc.ResetMaintenanceForTest()

	if err := db.DB.Model(&model.App{}).Where("id = ?", app.ID).
		Update("status", model.AppStatusDisabled).Error; err != nil {
		t.Fatalf("停用失败: %v", err)
	}

	rec := publicGet(t, r, "/api/apps/"+app.Slug+"/profile", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("停用应用仍应返回品牌（存量密钥还能调 /v1），实际 %d %s", rec.Code, rec.Body.String())
	}
}

// TestAppPublicMaintenanceFromSharedSnapshot 维护态取共用快照，不读应用行。
//
// 两处各算一份的后果：终端提示"一切正常"，调用却一直 503。
// 这里用"改库但不失效 → 仍是旧值"来证明它走的是 MaintenanceOf 的快照，
// 而不是刚加载的应用行。
func TestAppPublicMaintenanceFromSharedSnapshot(t *testing.T) {
	r, _ := setupUserAPI(t)
	app := seedApp(t, "pub-maint")
	appsvc.ResetPublicCacheForTest()
	appsvc.ResetMaintenanceForTest()

	maintOf := func() bool {
		rec := publicGet(t, r, "/api/apps/"+app.Slug+"/profile", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("应 200，实际 %d %s", rec.Code, rec.Body.String())
		}
		m, _ := dataOf(t, rec)["maintenance"].(map[string]any)
		on, _ := m["enabled"].(bool)
		return on
	}

	if maintOf() {
		t.Fatal("未开维护时应为 false")
	}

	if err := db.DB.Model(&model.App{}).Where("id = ?", app.ID).
		Updates(map[string]any{"maintenance_mode": true, "maintenance_message": "今晚 02:00 升级"}).Error; err != nil {
		t.Fatalf("开启维护失败: %v", err)
	}
	// 不失效：快照仍认为没维护（证明没有直读应用行）。
	if maintOf() {
		t.Fatal("未失效快照前不应立刻看到维护（说明读的是应用行而不是共用快照）")
	}

	appsvc.InvalidateMaintenance()
	rec := publicGet(t, r, "/api/apps/"+app.Slug+"/profile", "")
	m, _ := dataOf(t, rec)["maintenance"].(map[string]any)
	if m["enabled"] != true {
		t.Fatalf("失效后应看到维护: %v", m)
	}
	if m["message"] != "今晚 02:00 升级" {
		t.Fatalf("应下发运营配置的文案，实际 %v", m["message"])
	}
}

// TestAppPublicRoutes 三条新路由存在、旧聚合路由已删。
//
// 防的是"漏删"：旧 `/runtime` 留着就是一个仍然泄漏在线状态的入口。
//
// 刻意**不建库**（router.New 而不是 setupUserAPI）：这条断言的价值在于
// "任何环境都跑"。挂在 DB/Redis 基座上时，缺少 LD_TEST_DSN 的 CI 会把它
// 连同 7 条用例一起静默 skip —— 守卫变成"看起来有"。
func TestAppPublicRoutes(t *testing.T) {
	r := router.New()

	live := map[string]bool{}
	for _, rt := range r.Routes() {
		live[rt.Method+" "+rt.Path] = true
	}
	for _, want := range []string{
		"GET /api/apps/:slug/profile",
		"HEAD /api/apps/:slug/profile",
		"GET /api/apps/:slug/notices",
		"GET /api/apps/:slug/release",
		// 插件数据的对外只读面：HEAD 成对注册的理由同上面三条（curl -I 探活）
		"GET /api/apps/:slug/p/:plugin/:entity",
		"HEAD /api/apps/:slug/p/:plugin/:entity",
	} {
		if !live[want] {
			t.Fatalf("缺少路由 %s", want)
		}
	}
	if live["GET /api/apps/:slug/runtime"] {
		t.Fatal("旧聚合路由 /api/apps/:slug/runtime 必须删除（它会把在线状态交给匿名调用方）")
	}
}

// TestEveryPublicEndpointSurvivesAPISurface 契约里每条免凭证端点都必须在
// 纯 API 节点的允许表里。
//
// 两类事故都不报错：文档说"能调"而线上 404（漏登记），或漏登记后被当成
// "这台机器不提供 API"（终端接完才发现）。单向对账：允许表里还有 /f/、
// /api/icons/ 这类非契约路径，反向比对必然误报。
func TestEveryPublicEndpointSurvivesAPISurface(t *testing.T) {
	n := 0
	for _, e := range apicontract.Rules() {
		if e.Public == nil {
			continue
		}
		n++
		// concretePath 复用 api_scope_gate_test.go 的那份（把 :seg 换成 "1"）
		if !middleware.APISurfaceAllows(concretePath(e.Path)) {
			t.Errorf("%s %s 是免凭证端点，但不在 middleware.APISurfaceAllows 里", e.Method, e.Path)
		}
	}
	// 防"一条都没匹配到"的假绿：公开面今天至少 4 条。
	if n < 4 {
		t.Fatalf("只扫到 %d 条免凭证端点，契约可能没被读到", n)
	}
}
