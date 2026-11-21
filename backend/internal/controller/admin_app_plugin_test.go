package controller

// 应用插件的 HTTP 级回归：这条链路的价值在**状态语义**而不是"接口能通" ——
// 卸载后读要 404、描述符仍要 200、清数据要先卸载，这三处任一写错，
// 表现都是"页面看起来正常但数据要么删不掉、要么已经没了"。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/service/appplugins"
)

const pluginAdminTestSchema = "ld_test_app_plugin_admin"

func withPluginAdminDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("LD_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 LD_TEST_DSN，跳过数据库集成用例")
	}
	silent := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), silent)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + pluginAdminTestSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+pluginAdminTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.App{}, &model.AppPlugin{}, &model.AppPluginInstall{},
		&model.AppPluginRecord{}, &model.AdminAudit{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	// 测试库不跑启动期 EnsureIndexes：唯一索引缺了，"冲突 409"会假绿。
	for _, ddl := range []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_app_plugins_key ON app_plugins (key)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_plugin_installs_app_key ON app_plugin_installs (app_id, plugin_key)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_plugin_records_key ON app_plugin_records (app_id, plugin_key, entity, record_key)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_plugin_records_unique ON app_plugin_records (app_id, plugin_key, entity, unique_norm)`,
	} {
		if err := gdb.Exec(ddl).Error; err != nil {
			t.Fatalf("建测试索引失败: %v", err)
		}
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + pluginAdminTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

func callPlugin(t *testing.T, h gin.HandlerFunc, method, path string,
	params gin.Params, body any) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	var buf bytes.Buffer
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		buf.Write(b)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	// c.Stream 会调 CloseNotify，而 httptest.ResponseRecorder 不实现它（直接 panic）。
	// 包一层只为导出用例，其余 hander 走不到这条分支。
	c, _ := gin.CreateTestContext(streamRecorder{rec})
	c.Request = req
	c.Params = params
	c.Set("user_id", uint64(1))
	h(c)
	return rec
}

// streamRecorder 只补齐 http.CloseNotifier：导出用 c.Stream 分段写，
// gin 在 Stream 开头就取 clientGone，缺了它会 panic 而不是失败。
type streamRecorder struct{ *httptest.ResponseRecorder }

func (streamRecorder) CloseNotify() <-chan bool { return make(chan bool) }

func pluginParams(appID string, extra ...gin.Param) gin.Params {
	ps := gin.Params{{Key: "id", Value: appID}, {Key: "key", Value: "ticket"}}
	return append(ps, extra...)
}

// TestPluginFlow 导入 → 安装 → 建记录 → 冲突 → 卸载 → 描述符仍 200 →
// 读写 404 → 清数据。
func TestPluginFlow(t *testing.T) {
	gdb := withPluginAdminDB(t)
	app := model.App{Slug: "p-app", Name: "插件应用", Status: model.AppStatusEnabled, AllowSelfIssue: true}
	if err := gdb.Create(&app).Error; err != nil {
		t.Fatalf("建应用失败: %v", err)
	}
	appID := itoaU(app.ID)

	// 1. 导入示例。
	rec := callPlugin(t, AdminAppPluginImport, http.MethodPut, "/api/admin/app-plugins/ticket",
		gin.Params{{Key: "key", Value: "ticket"}},
		map[string]any{"expected_version": 0, "manifest": json.RawMessage(appplugins.SampleManifest)})
	if rec.Code != http.StatusOK {
		t.Fatalf("导入应成功，实际 %d: %s", rec.Code, rec.Body.String())
	}

	// 2. 用旧版本号再导入 → 409（防 AI 旧稿覆盖运营微调）。
	rec = callPlugin(t, AdminAppPluginImport, http.MethodPut, "/api/admin/app-plugins/ticket",
		gin.Params{{Key: "key", Value: "ticket"}},
		map[string]any{"expected_version": 0, "manifest": json.RawMessage(appplugins.SampleManifest)})
	if rec.Code != http.StatusConflict {
		t.Fatalf("版本不符应 409，实际 %d: %s", rec.Code, rec.Body.String())
	}

	// 3. 预检非法定义 → 200 + problems（预检不落库，所以不是 4xx）。
	bad := strings.Replace(appplugins.SampleManifest, `"apiVersion": 1`, `"apiVersion": 2`, 1)
	rec = callPlugin(t, AdminAppPluginImport, http.MethodPut, "/api/admin/app-plugins/ticket?dry_run=1",
		gin.Params{{Key: "key", Value: "ticket"}},
		map[string]any{"expected_version": 1, "manifest": json.RawMessage(bad)})
	if rec.Code != http.StatusOK {
		t.Fatalf("预检应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "apiVersion") {
		t.Fatalf("预检应指出 apiVersion 问题，实际 %s", rec.Body.String())
	}

	// 4. 安装。
	rec = callPlugin(t, AdminAppPluginInstall, http.MethodPost, "/api/admin/apps/"+appID+"/plugins",
		pluginParams(appID), map[string]any{"plugin_key": "ticket", "config": map[string]any{"slaHours": 12}})
	if rec.Code != http.StatusOK {
		t.Fatalf("安装应成功，实际 %d: %s", rec.Code, rec.Body.String())
	}

	// 5. 建记录。
	rec = callPlugin(t, AdminAppPluginRecordCreate, http.MethodPost,
		"/api/admin/apps/"+appID+"/plugins/ticket/records", pluginParams(appID),
		map[string]any{"entity": "ticket", "data": map[string]any{
			"ticketNo": "T-1", "title": "打不开", "status": "open",
		}})
	if rec.Code != http.StatusOK {
		t.Fatalf("建记录应成功，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"record_key":"`) {
		t.Fatalf("记录应回填 record_key，实际 %s", rec.Body.String())
	}

	// 6. 唯一值冲突 → 409（不能吞成 500）。
	rec = callPlugin(t, AdminAppPluginRecordCreate, http.MethodPost,
		"/api/admin/apps/"+appID+"/plugins/ticket/records", pluginParams(appID),
		map[string]any{"entity": "ticket", "data": map[string]any{"ticketNo": "t-1", "title": "重复", "status": "open"}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("唯一值冲突应 409，实际 %d: %s", rec.Code, rec.Body.String())
	}

	// 7. 卸载 → 读 404、描述符仍 200 且带记录数。
	rec = callPlugin(t, AdminAppPluginUninstall, http.MethodDelete,
		"/api/admin/apps/"+appID+"/plugins/ticket", pluginParams(appID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("卸载应成功，实际 %d: %s", rec.Code, rec.Body.String())
	}
	rec = callPlugin(t, AdminAppPluginRecordList, http.MethodGet,
		"/api/admin/apps/"+appID+"/plugins/ticket/records?entity=ticket", pluginParams(appID), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("卸载后读应 404，实际 %d: %s", rec.Code, rec.Body.String())
	}
	rec = callPlugin(t, AdminAppPluginDetail, http.MethodGet,
		"/api/admin/apps/"+appID+"/plugins/ticket", pluginParams(appID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("卸载后描述符必须 200（否则没有重新启用的入口），实际 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"record_count":1`) {
		t.Fatalf("描述符应显示保留的记录数，实际 %s", rec.Body.String())
	}

	// 8. 清数据。
	rec = callPlugin(t, AdminAppPluginPurge, http.MethodDelete,
		"/api/admin/apps/"+appID+"/plugins/ticket/data", pluginParams(appID), nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"deleted":1`) {
		t.Fatalf("清数据应删 1 条，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPluginExportEscapesCells 导出的每一格都要过 `logs.Esc`：
// 值以 `=`/`+`/`-`/`@` 开头会被 Excel 当公式执行（CSV 注入），
// 含逗号/引号/换行会直接破坏列结构。表头来自 manifest，同样要转义。
func TestPluginExportEscapesCells(t *testing.T) {
	gdb := withPluginAdminDB(t)
	app := model.App{Slug: "p-exp", Name: "导出应用", Status: model.AppStatusEnabled, AllowSelfIssue: true}
	if err := gdb.Create(&app).Error; err != nil {
		t.Fatalf("建应用失败: %v", err)
	}
	appID := itoaU(app.ID)

	// 列名来自 manifest，也可能带逗号：只转义数据格会让表头把列数撑乱。
	manif := strings.Replace(appplugins.SampleManifest, `"label": "标题"`, `"label": "标题,副标题"`, 1)
	if rec := callPlugin(t, AdminAppPluginImport, http.MethodPut, "/api/admin/app-plugins/ticket",
		gin.Params{{Key: "key", Value: "ticket"}},
		map[string]any{"expected_version": 0, "manifest": json.RawMessage(manif)}); rec.Code != http.StatusOK {
		t.Fatalf("导入失败 %d: %s", rec.Code, rec.Body.String())
	}
	if rec := callPlugin(t, AdminAppPluginInstall, http.MethodPost, "/api/admin/apps/"+appID+"/plugins",
		pluginParams(appID), map[string]any{"plugin_key": "ticket", "config": map[string]any{"slaHours": 12}}); rec.Code != http.StatusOK {
		t.Fatalf("安装失败 %d: %s", rec.Code, rec.Body.String())
	}
	for _, row := range []map[string]any{
		{"ticketNo": "T-1", "title": "=cmd|'/c calc'!A0", "status": "open"},
		{"ticketNo": "T-2", "title": `他说,"你好"`, "status": "done"},
	} {
		rec := callPlugin(t, AdminAppPluginRecordCreate, http.MethodPost,
			"/api/admin/apps/"+appID+"/plugins/ticket/records", pluginParams(appID),
			map[string]any{"entity": "ticket", "data": row})
		if rec.Code != http.StatusOK {
			t.Fatalf("建记录失败 %d: %s", rec.Code, rec.Body.String())
		}
	}

	rec := callPlugin(t, AdminAppPluginRecordExport, http.MethodGet,
		"/api/admin/apps/"+appID+"/plugins/ticket/records/export?entity=ticket", pluginParams(appID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("导出应成功，实际 %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// BOM + 表头（列名取自 manifest 的 label；带逗号的那个必须被引用）
	if !strings.HasPrefix(body, "\uFEFF单号,\"标题,副标题\",状态,期望完成\r\n") {
		t.Fatalf("表头不对: %q", body)
	}
	// 公式注入：前缀单引号，Excel 才会当文本
	if !strings.Contains(body, `'=cmd|'/c calc'!A0`) {
		t.Fatalf("公式注入未被转义: %q", body)
	}
	// 逗号与引号：整格加引号、内部引号翻倍
	if !strings.Contains(body, `"他说,""你好"""`) {
		t.Fatalf("含分隔符的格子未被正确引用: %q", body)
	}
	if n := strings.Count(body, "\r\n"); n != 3 {
		t.Fatalf("应为表头 + 2 行，实际 %d 行: %q", n, body)
	}
}

// TestPluginPublicSwitch 对外只读是安装级开关：一次 PATCH 生效，
// 并且必须出现在清单与描述符里（否则管理台的开关只能靠"猜上次点的是什么"）。
func TestPluginPublicSwitch(t *testing.T) {
	gdb := withPluginAdminDB(t)
	app := model.App{Slug: "p-pub", Name: "公开面应用", Status: model.AppStatusEnabled, AllowSelfIssue: true}
	if err := gdb.Create(&app).Error; err != nil {
		t.Fatalf("建应用失败: %v", err)
	}
	appID := itoaU(app.ID)

	if rec := callPlugin(t, AdminAppPluginImport, http.MethodPut, "/api/admin/app-plugins/ticket",
		gin.Params{{Key: "key", Value: "ticket"}},
		map[string]any{"expected_version": 0, "manifest": json.RawMessage(appplugins.SampleManifest)}); rec.Code != http.StatusOK {
		t.Fatalf("导入失败 %d: %s", rec.Code, rec.Body.String())
	}
	if rec := callPlugin(t, AdminAppPluginInstall, http.MethodPost, "/api/admin/apps/"+appID+"/plugins",
		pluginParams(appID), map[string]any{"plugin_key": "ticket", "config": map[string]any{"slaHours": 12}}); rec.Code != http.StatusOK {
		t.Fatalf("安装失败 %d: %s", rec.Code, rec.Body.String())
	}

	// 空 body（只带版本号）必须 400：否则"什么都没改"也会推进版本。
	rec := callPlugin(t, AdminAppPluginPatch, http.MethodPatch,
		"/api/admin/apps/"+appID+"/plugins/ticket", pluginParams(appID),
		map[string]any{"expected_version": 1})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("没有要改的内容应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}

	rec = callPlugin(t, AdminAppPluginPatch, http.MethodPatch,
		"/api/admin/apps/"+appID+"/plugins/ticket", pluginParams(appID),
		map[string]any{"public_enabled": true, "expected_version": 1})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"public_enabled":true`) {
		t.Fatalf("打开对外只读应成功并回填状态，实际 %d: %s", rec.Code, rec.Body.String())
	}

	// 描述符要带出来（页面据此渲染开关当前值）。
	rec = callPlugin(t, AdminAppPluginDetail, http.MethodGet,
		"/api/admin/apps/"+appID+"/plugins/ticket", pluginParams(appID), nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"public_enabled":true`) {
		t.Fatalf("描述符应带出对外开关，实际 %d: %s", rec.Code, rec.Body.String())
	}
	// 清单也要带（弹窗里的行不再单独拉描述符）。
	rec = callPlugin(t, AdminAppPluginInstalls, http.MethodGet,
		"/api/admin/apps/"+appID+"/plugins", pluginParams(appID), nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"public_enabled":true`) {
		t.Fatalf("清单应带出对外开关，实际 %d: %s", rec.Code, rec.Body.String())
	}

	// 关掉 → 立刻回到默认。
	rec = callPlugin(t, AdminAppPluginPatch, http.MethodPatch,
		"/api/admin/apps/"+appID+"/plugins/ticket", pluginParams(appID),
		map[string]any{"public_enabled": false, "expected_version": 2})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"public_enabled":false`) {
		t.Fatalf("关闭对外只读应成功，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPluginPublicHandler 直接打 handler：守的是"路由参数名与 handler 取的键一致"。
//
// 这类错（路由写 :plugin、handler 取 c.Param("key")）不会编译失败，
// 表现是"对外面永远 404"，而 service 层的闸门用例全绿 —— 只能靠这条。
func TestPluginPublicHandler(t *testing.T) {
	gdb := withPluginAdminDB(t)
	app := model.App{Slug: "p-pub3", Name: "对外应用", Status: model.AppStatusEnabled, AllowSelfIssue: true}
	if err := gdb.Create(&app).Error; err != nil {
		t.Fatalf("建应用失败: %v", err)
	}
	appID := itoaU(app.ID)

	if rec := callPlugin(t, AdminAppPluginImport, http.MethodPut, "/api/admin/app-plugins/ticket",
		gin.Params{{Key: "key", Value: "ticket"}},
		map[string]any{"expected_version": 0, "manifest": json.RawMessage(appplugins.SampleManifest)}); rec.Code != http.StatusOK {
		t.Fatalf("导入失败 %d: %s", rec.Code, rec.Body.String())
	}
	if rec := callPlugin(t, AdminAppPluginInstall, http.MethodPost, "/api/admin/apps/"+appID+"/plugins",
		pluginParams(appID), map[string]any{"plugin_key": "ticket", "config": map[string]any{"slaHours": 12}}); rec.Code != http.StatusOK {
		t.Fatalf("安装失败 %d: %s", rec.Code, rec.Body.String())
	}
	if rec := callPlugin(t, AdminAppPluginRecordCreate, http.MethodPost,
		"/api/admin/apps/"+appID+"/plugins/ticket/records", pluginParams(appID),
		map[string]any{"entity": "ticket", "data": map[string]any{
			"ticketNo": "T-9", "title": "对外可见", "status": "open", "detail": "内部说明",
		}}); rec.Code != http.StatusOK {
		t.Fatalf("建记录失败 %d: %s", rec.Code, rec.Body.String())
	}

	pubParams := gin.Params{
		{Key: "slug", Value: "p-pub3"}, {Key: "plugin", Value: "ticket"}, {Key: "entity", Value: "ticket"},
	}
	// ① 没打开对外只读 → 404（不是 403：403 等于确认"这里有个入口"）。
	rec := callPlugin(t, AppPluginPublic, http.MethodGet, "/api/apps/p-pub3/p/ticket/ticket", pubParams, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未开启时应 404，实际 %d: %s", rec.Code, rec.Body.String())
	}

	if rec := callPlugin(t, AdminAppPluginPatch, http.MethodPatch,
		"/api/admin/apps/"+appID+"/plugins/ticket", pluginParams(appID),
		map[string]any{"public_enabled": true, "expected_version": 1}); rec.Code != http.StatusOK {
		t.Fatalf("打开对外只读失败 %d: %s", rec.Code, rec.Body.String())
	}

	rec = callPlugin(t, AppPluginPublic, http.MethodGet, "/api/apps/p-pub3/p/ticket/ticket", pubParams, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("开启后应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "对外可见") {
		t.Fatalf("白名单字段应在响应里: %s", body)
	}
	// ② detail 不在 sample 的 public 白名单里（["title","status"]），不得出现。
	if strings.Contains(body, "内部说明") {
		t.Fatalf("未公开的字段被下发: %s", body)
	}
	// ③ 缓存头来自契约（max-age=30），不是 handler 里另写的常量。
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=30" {
		t.Fatalf("Cache-Control 应取自 PublicPolicy，实际 %q", cc)
	}
	if rec.Header().Get("ETag") == "" {
		t.Fatal("免凭证可缓存端点必须带 ETag（否则每次冷启动全量重查）")
	}
}

// TestPluginRecordRejectsForeignApp 用别的应用 id 访问：appOf 应 404，
// 而不是"查不到记录"这种能被用来探测 id 是否存在的回答。
func TestPluginRecordRejectsForeignApp(t *testing.T) {
	withPluginAdminDB(t)
	rec := callPlugin(t, AdminAppPluginRecordList, http.MethodGet,
		"/api/admin/apps/999/plugins/ticket/records?entity=ticket",
		pluginParams("999"), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的应用应 404，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// TestDevDocHandlerServesGeneratedManual 开发手册的 HTTP 契约。
//
// 这条不需要数据库：手册完全由 devdoc 从常量渲染，所以"能不能下载"与插件数据无关。
// 断言四件事：状态码、内容类型、附件头（含 RFC 5987 的中文名）、ETag 命中 304。
//
// 必须走 engine.ServeHTTP 而不是直接调 handler：gin 的 c.Status(304) 只写
// Writermem，直到 WriteHeaderNow 才落到 ResponseRecorder —— 直接调会一直读到 200。
func TestDevDocHandlerServesGeneratedManual(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/admin/app-plugins/dev-doc.md", AdminAppPluginDevDoc)

	get := func(ifNoneMatch string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/app-plugins/dev-doc.md", nil)
		if ifNoneMatch != "" {
			req.Header.Set("If-None-Match", ifNoneMatch)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	rec := get("")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Fatalf("内容类型应为 markdown，实际 %q", ct)
	}
	// 同时给 ASCII 兜底名与 RFC 5987 的中文名：老客户端拿前者，新客户端拿后者。
	cd := rec.Header().Get("Content-Disposition")
	if !strings.Contains(cd, `filename="plugin-dev-manual.md"`) || !strings.Contains(cd, "filename*=UTF-8''") {
		t.Fatalf("附件头应同时含 ASCII 名与 filename*，实际 %q", cd)
	}
	if !strings.HasPrefix(rec.Body.String(), "# 应用插件开发手册") {
		t.Fatalf("正文应是手册正文，实际开头 %q", rec.Body.String()[:40])
	}

	// 同一份内容再请求一次（带 ETag）应 304：反复下载不该重传二十多 KB。
	tag := rec.Header().Get("ETag")
	if tag == "" {
		t.Fatal("应下发 ETag")
	}
	again := get(tag)
	if again.Code != http.StatusNotModified {
		t.Fatalf("ETag 命中应 304，实际 %d", again.Code)
	}
	if again.Body.Len() != 0 {
		t.Fatalf("304 不应带 body，实际 %d 字节", again.Body.Len())
	}
}
