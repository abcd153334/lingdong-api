package controller

// 应用后台的**写路径**回归：这几处此前没有任何 HTTP 级用例，
// 而它们的失败形态都是"静默的错误状态"而不是报错。
//
//   - 关闭自助签发：GORM 的零值 + default:true 会让开关静默失效（安全面）；
//   - map 插入：不显式写 created_at 会落 NULL，列表按它排序会反转；
//   - 公告 PUT：写 enabled 会覆盖别人的上下架处置；
//   - 版本状态：`int16(65538)` 截断成 2，一个越界值就能"发布"。

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	appsvc "lingdong-api/internal/service/app"
)

const appAdminTestSchema = "ld_test_app_admin"

func withAppDB(t *testing.T) *gorm.DB {
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
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + appAdminTestSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+appAdminTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.App{}, &model.AppNotice{}, &model.AppRelease{}, &model.AdminAudit{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + appAdminTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

// callAdmin 用最小 gin 上下文调用 handler（带 user_id，供审计使用）。
func callAdmin(t *testing.T, handler gin.HandlerFunc, method, path string, body any) *httptest.ResponseRecorder {
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
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set("user_id", uint64(1))
	handler(c)
	return rec
}

// TestAdminAppCreateKeepsSelfIssueOff 关闭自助签发必须真的落库为 false。
//
// 失败形态：接口返回 500、库里留着一个 allow_self_issue=true 的应用 ——
// 任意登录用户仍可为敏感应用签发可花钱的托管密钥，而运营以为已经关掉了。
func TestAdminAppCreateKeepsSelfIssueOff(t *testing.T) {
	gdb := withAppDB(t)

	rec := callAdmin(t, AdminAppCreate, http.MethodPost, "/api/admin/apps", map[string]any{
		"slug": "closed-app", "name": "敏感应用", "description": "仅运营发放",
		"allow_self_issue": false,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("创建应成功，实际 %d: %s", rec.Code, rec.Body.String())
	}

	var row model.App
	if err := gdb.Where("slug = ?", "closed-app").First(&row).Error; err != nil {
		t.Fatalf("读回应用失败: %v", err)
	}
	if row.AllowSelfIssue {
		t.Fatal("allow_self_issue 必须落库为 false（map 插入不得被 default:true 覆盖）")
	}
	if row.Status != model.AppStatusEnabled {
		t.Fatalf("新建应用状态应为启用，实际 %d", row.Status)
	}
	// map 插入不会自动填时间：漏写会让 sort=created_at 排序反转、后台显示 0001-01-01。
	if row.CreatedAt.IsZero() || row.UpdatedAt.IsZero() {
		t.Fatalf("created_at/updated_at 必须显式写入，实际 %+v/%+v", row.CreatedAt, row.UpdatedAt)
	}
}

// TestAdminAppCreateRejectsLongDescription description 列宽 255，超长必须 400 而不是 500。
func TestAdminAppCreateRejectsLongDescription(t *testing.T) {
	withAppDB(t)
	rec := callAdmin(t, AdminAppCreate, http.MethodPost, "/api/admin/apps", map[string]any{
		"slug": "long-desc", "name": "超长说明", "description": strings.Repeat("字", 256),
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("超长说明应回 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// TestUpdateNoticeDoesNotTouchEnabled 编辑公告不得改写上下架。
//
// 失败形态：运营 A 刚把公告下架，运营 B 的旧编辑弹窗一保存，
// 公告被静默重新上架并下发给全量终端。
func TestUpdateNoticeDoesNotTouchEnabled(t *testing.T) {
	gdb := withAppDB(t)
	ctx := context.Background()
	app := model.App{Slug: "n-app", Name: "公告应用", Status: model.AppStatusEnabled,
		AllowSelfIssue: true, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := gdb.Create(&app).Error; err != nil {
		t.Fatalf("建应用失败: %v", err)
	}
	row, err := appsvc.CreateNotice(ctx, gdb, app.ID, appsvc.NoticeInput{
		Title: "原公告", Content: "正文", Level: "info", Enabled: true,
	})
	if err != nil {
		t.Fatalf("建公告失败: %v", err)
	}

	// 传 Enabled=false —— 服务层必须忽略它（上下架只由 PATCH 改）。
	if err := appsvc.UpdateNotice(ctx, gdb, app.ID, row.ID, appsvc.NoticeInput{
		Title: "改过的标题", Content: "正文", Level: "info", Enabled: false,
	}); err != nil {
		t.Fatalf("更新公告失败: %v", err)
	}

	var got model.AppNotice
	if err := gdb.Where("id = ?", row.ID).First(&got).Error; err != nil {
		t.Fatalf("读回公告失败: %v", err)
	}
	if got.Title != "改过的标题" {
		t.Fatalf("标题应被更新，实际 %q", got.Title)
	}
	if !got.Enabled {
		t.Fatal("PUT 不得改写 enabled（上下架只走 PATCH /status）")
	}
}

// TestAdminAppReleaseStatusRejectsTruncatedInt 越界状态码不得被 int16 截断成 2。
func TestAdminAppReleaseStatusRejectsTruncatedInt(t *testing.T) {
	gdb := withAppDB(t)
	app := model.App{Slug: "r-app", Name: "版本应用", Status: model.AppStatusEnabled,
		AllowSelfIssue: true, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := gdb.Create(&app).Error; err != nil {
		t.Fatalf("建应用失败: %v", err)
	}
	rel := model.AppRelease{AppID: app.ID, Version: "1.0.0", ReleaseNotes: "说明",
		DownloadURL: "https://dl.example.com/1.0.0.exe", Status: model.AppReleaseDraft,
		CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := gdb.Create(&rel).Error; err != nil {
		t.Fatalf("建版本失败: %v", err)
	}

	rec := callAdmin(t, AdminAppReleaseStatus, http.MethodPatch,
		"/api/admin/apps/"+strconv.FormatUint(app.ID, 10)+"/releases/"+strconv.FormatUint(rel.ID, 10)+"/status",
		map[string]any{"status": 65538}) // 0x10002 → int16 截断后是 2
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("越界状态码应回 400，实际 %d: %s", rec.Code, rec.Body.String())
	}

	var got model.AppRelease
	if err := gdb.Where("id = ?", rel.ID).First(&got).Error; err != nil {
		t.Fatalf("读回版本失败: %v", err)
	}
	if got.Status != model.AppReleaseDraft {
		t.Fatalf("越界状态码不得改变版本状态，实际 %d", got.Status)
	}
	if got.PublishedAt != nil {
		t.Fatal("越界状态码不得写 published_at")
	}
}
