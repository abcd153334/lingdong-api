package app

// 应用级后台的服务层集成用例。覆盖三类"看起来能跑、实际会静默出错"的点：
// 零值陷阱（enabled=false）、版本状态机、以及"最近发布"按时间而不是版本号。

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/model"
)

const appTestSchema = "ld_test_appsvc"

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
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + appTestSchema).Error; err != nil {
		t.Fatalf("创建 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+appTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.App{}, &model.AppNotice{}, &model.AppRelease{},
		&model.APIKey{}, &model.Order{}, &model.Usage{}); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + appTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return gdb
}

func seedAppRow(t *testing.T, gdb *gorm.DB, slug string) *model.App {
	t.Helper()
	a := &model.App{Slug: slug, Name: "测试应用", Status: model.AppStatusEnabled, AllowSelfIssue: true}
	if err := gdb.Create(a).Error; err != nil {
		t.Fatalf("造应用失败: %v", err)
	}
	return a
}

func TestNormalizeBrand(t *testing.T) {
	cases := []struct {
		name string
		in   BrandInput
		want string
	}{
		{"缺展示名", BrandInput{}, "展示名称必填"},
		{"非法链接", BrandInput{DisplayName: "A", SupportURL: "javascript:alert(1)"}, "支持链接必须是 http(s) 地址"},
		{"相对路径", BrandInput{DisplayName: "A", PrivacyURL: "/privacy"}, "隐私政策必须是 http(s) 地址"},
		{"邮箱格式", BrandInput{DisplayName: "A", ContactEmail: "not-an-email"}, "联系邮箱格式不正确"},
		{"通过", BrandInput{DisplayName: "A", SupportURL: "https://x.com/h"}, ""},
	}
	for _, c := range cases {
		if _, msg := NormalizeBrand(c.in); msg != c.want {
			t.Fatalf("%s：期望 %q，实际 %q", c.name, c.want, msg)
		}
	}
}

func TestSaveBrandAndFallbackName(t *testing.T) {
	gdb := withAppDB(t)
	ctx := context.Background()
	a := seedAppRow(t, gdb, "brand-app")

	// 未配置展示名时回落后台名称：终端至少要有个能显示的名字。
	if got := BrandOf(a, "").DisplayName; got != a.Name {
		t.Fatalf("空展示名应回落 Name，实际 %q", got)
	}

	in, msg := NormalizeBrand(BrandInput{DisplayName: "  灵动画影  ", ContactEmail: " hi@example.com "})
	if msg != "" {
		t.Fatalf("校验不应失败: %s", msg)
	}
	if err := SaveBrand(ctx, gdb, a.ID, in); err != nil {
		t.Fatalf("保存品牌失败: %v", err)
	}
	got, err := Get(ctx, gdb, a.ID)
	if err != nil {
		t.Fatalf("重读失败: %v", err)
	}
	if got.DisplayName != "灵动画影" || got.ContactEmail != "hi@example.com" {
		t.Fatalf("品牌未按归一化后的值落库: %+v", got)
	}
	// 身份字段不能被品牌保存顺带改掉。
	if !got.AllowSelfIssue || got.Status != model.AppStatusEnabled {
		t.Fatalf("品牌保存不应触碰身份字段: allow_self_issue=%v status=%d", got.AllowSelfIssue, got.Status)
	}
}

func TestCreateNoticeDisabledStaysDisabled(t *testing.T) {
	gdb := withAppDB(t)
	ctx := context.Background()
	a := seedAppRow(t, gdb, "notice-app")

	in, msg := NormalizeNotice(NoticeInput{Title: "维护通知", Content: "今晚升级", Enabled: false})
	if msg != "" {
		t.Fatalf("校验不应失败: %s", msg)
	}
	row, err := CreateNotice(ctx, gdb, a.ID, in)
	if err != nil {
		t.Fatalf("建公告失败: %v", err)
	}
	// enabled 带 default:true 标签：不显式回写就会被列默认值吃掉。
	var reloaded model.AppNotice
	if err := gdb.Where("id = ?", row.ID).First(&reloaded).Error; err != nil {
		t.Fatalf("重读失败: %v", err)
	}
	if reloaded.Enabled {
		t.Fatal("新建时 enabled=false 必须落成 false（零值陷阱）")
	}

	// 生效判据：enabled && 在时间窗内。
	now := time.Now()
	future := now.Add(48 * time.Hour)
	if _, err := CreateNotice(ctx, gdb, a.ID, NoticeInput{
		Title: "定时公告", Content: "x", Enabled: true, StartsAt: &future,
	}); err != nil {
		t.Fatalf("建定时公告失败: %v", err)
	}
	rows, err := EffectiveNotices(ctx, gdb, a.ID, now, 20)
	if err != nil {
		t.Fatalf("取生效公告失败: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("未到开始时间的公告不应下发，实际 %d 条", len(rows))
	}
	// 数组恒非 nil（对外 JSON 必须是 [] 而不是 null）。
	if views := ViewsOf(nil); views == nil || len(views) != 0 {
		t.Fatalf("空公告应投影成空数组，实际 %v", views)
	}
}

func TestNoticeTimeWindowValidation(t *testing.T) {
	start := time.Now()
	end := start.Add(-time.Minute)
	if _, msg := NormalizeNotice(NoticeInput{
		Title: "t", Content: "c", Enabled: true, StartsAt: &start, EndsAt: &end,
	}); msg != "结束时间必须晚于开始时间" {
		t.Fatalf("结束早于开始应被拒，实际 %q", msg)
	}
}

func TestReleaseStateMachine(t *testing.T) {
	gdb := withAppDB(t)
	ctx := context.Background()
	a := seedAppRow(t, gdb, "release-app")

	in, msg := NormalizeRelease(ReleaseInput{
		Version: "1.0.0", ReleaseNotes: "首个版本", DownloadURL: "https://dl.example.com/a.exe",
	})
	if msg != "" {
		t.Fatalf("校验不应失败: %s", msg)
	}
	draft, err := CreateRelease(ctx, gdb, a.ID, in)
	if err != nil {
		t.Fatalf("建草稿失败: %v", err)
	}
	if draft.Status != model.AppReleaseDraft {
		t.Fatalf("新版本必须是草稿，实际 %d", draft.Status)
	}
	// 重号：应用内唯一。
	if _, err := CreateRelease(ctx, gdb, a.ID, in); err != ErrVersionExists {
		t.Fatalf("重复版本号应报 ErrVersionExists，实际 %v", err)
	}

	// 草稿可删（返回被删的行，供调用方写审计）。
	if _, err := DeleteRelease(ctx, gdb, a.ID, draft.ID); err != nil {
		t.Fatalf("草稿应可删除: %v", err)
	}
	draft, err = CreateRelease(ctx, gdb, a.ID, in)
	if err != nil {
		t.Fatalf("重建草稿失败: %v", err)
	}

	// 发布：写 published_at。
	if err := SetReleaseStatus(ctx, gdb, a.ID, draft.ID, model.AppReleasePublished); err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	var pub model.AppRelease
	if err := gdb.Where("id = ?", draft.ID).First(&pub).Error; err != nil {
		t.Fatalf("重读失败: %v", err)
	}
	if pub.PublishedAt == nil {
		t.Fatal("发布必须写 published_at")
	}
	// 已发布不可删、不可改版本号、不可退回草稿。
	if _, err := DeleteRelease(ctx, gdb, a.ID, draft.ID); err != ErrReleaseState {
		t.Fatalf("已发布删除应被拒，实际 %v", err)
	}
	bad := in
	bad.Version = "1.0.1"
	if _, err := UpdateRelease(ctx, gdb, a.ID, draft.ID, bad); err != ErrReleaseState {
		t.Fatalf("已发布改版本号应被拒，实际 %v", err)
	}
	if err := SetReleaseStatus(ctx, gdb, a.ID, draft.ID, model.AppReleaseDraft); err != ErrReleaseState {
		t.Fatalf("已发布退回草稿应被拒，实际 %v", err)
	}
	// 撤回后保留 published_at，并可重发。
	if err := SetReleaseStatus(ctx, gdb, a.ID, draft.ID, model.AppReleaseRevoked); err != nil {
		t.Fatalf("撤回失败: %v", err)
	}
	var revoked model.AppRelease
	if err := gdb.Where("id = ?", draft.ID).First(&revoked).Error; err != nil {
		t.Fatalf("重读失败: %v", err)
	}
	if revoked.PublishedAt == nil {
		t.Fatal("撤回应保留 published_at（审计要知道何时上过线）")
	}
	if err := SetReleaseStatus(ctx, gdb, a.ID, draft.ID, model.AppReleasePublished); err != nil {
		t.Fatalf("撤回后应可重发: %v", err)
	}
}

// TestLatestPublishedByTimeNotSemver 守「最近发布」的口径。
//
// 先发 2.0.0，再补发 1.9.9（hotfix）。按版本号取最大会返回 1.9.9 之外的 2.0.0，
// 但真实最新上线的其实是 1.9.9 —— 用版本号比较会让 hotfix 静默失效。
func TestLatestPublishedByTimeNotSemver(t *testing.T) {
	gdb := withAppDB(t)
	ctx := context.Background()
	a := seedAppRow(t, gdb, "semver-app")

	mk := func(version string) *model.AppRelease {
		r, err := CreateRelease(ctx, gdb, a.ID, ReleaseInput{
			Version: version, ReleaseNotes: "x", DownloadURL: "https://dl.example.com/x",
		})
		if err != nil {
			t.Fatalf("建 %s 失败: %v", version, err)
		}
		if err := SetReleaseStatus(ctx, gdb, a.ID, r.ID, model.AppReleasePublished); err != nil {
			t.Fatalf("发布 %s 失败: %v", version, err)
		}
		return r
	}
	mk("2.0.0")
	time.Sleep(10 * time.Millisecond)
	latest := mk("1.9.9")

	got, err := LatestPublishedOf(ctx, gdb, a.ID)
	if err != nil {
		t.Fatalf("取最近发布失败: %v", err)
	}
	if got == nil || got.ID != latest.ID {
		t.Fatalf("最近发布应为最后发布的 1.9.9，实际 %+v", got)
	}
}
