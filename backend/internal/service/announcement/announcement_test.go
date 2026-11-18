package announcement

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

func setAnnouncements(t *testing.T, raw string) {
	t.Helper()
	prev := conf.System().All()
	conf.System().Reload(map[string]json.RawMessage{"announcements": json.RawMessage(raw)})
	t.Cleanup(func() { conf.System().Reload(prev) })
}

// TestSortByDate 时间线按发布时间倒序；时间戳非法的条目排在末尾并保持原始相对顺序。
func TestSortByDate(t *testing.T) {
	setAnnouncements(t, `{"enabled":true,"items":[
		{"id":1,"content":"a","publishDate":"2026-10-01T08:00:00+08:00","type":"default","extra":""},
		{"id":3,"content":"c","publishDate":"2026-10-15T10:00:00+08:00","type":"warning","extra":""},
		{"id":9,"content":"bad","publishDate":"not-a-date","type":"default","extra":""},
		{"id":2,"content":"b","publishDate":"2026-10-06T14:30:00+08:00","type":"ongoing","extra":""}
	]}`)
	items := Load().Items
	if len(items) != 4 || items[0].ID != 3 || items[1].ID != 2 || items[2].ID != 1 || items[3].ID != 9 {
		t.Fatalf("排序不符: %+v", items)
	}
}

// TestKey 已读锚：id 优先且形如 id:<id>，无有效 id 时不可锚定。
func TestKey(t *testing.T) {
	if Key(Item{ID: 7}) != "id:7" {
		t.Fatalf("锚不符: %s", Key(Item{ID: 7}))
	}
	if Key(Item{ID: 0}) != "" {
		t.Fatalf("无 id 应返回空锚")
	}
}

// TestAnnotateNoUserMarked 无可用锚的条目恒为已读。
//
// 这是「点了也清不掉徽标」的根因守卫：id<=0 没有锚，MarkRead 会跳过它，
// 若把它算成未读，未读数永远减不到 0。
func TestAnnotateNoUserMarked(t *testing.T) {
	views, err := Annotate(context.Background(), nil, 0, []Item{
		{ID: 0, Content: "legacy"},
		{ID: 5, Content: "new"},
	})
	if err != nil {
		t.Fatalf("无用户时不应查库/报错: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("条目数不符: %+v", views)
	}
	if !views[0].Read {
		t.Fatalf("无 id 条目必须恒为已读")
	}
	if views[1].Read {
		t.Fatalf("有 id 条目在无用户时不应算已读")
	}
}

// ---------- 数据库集成用例（需 LD_TEST_DSN；未设置则跳过） ----------

const testSchema = "ld_test_s10_ann"

func withTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("LD_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 LD_TEST_DSN，跳过数据库集成用例")
	}
	cfg := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), cfg)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + testSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+testSchema), cfg)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.AnnouncementRead{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + testSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

// TestListForReadFlags 读模型的已读标记：整组已读幂等、同 id 编辑不重现、新 id 重现、他人隔离。
func TestListForReadFlags(t *testing.T) {
	gdb := withTestDB(t)
	setAnnouncements(t, `{"enabled":true,"items":[
		{"id":1,"content":"a","publishDate":"2026-10-01T08:00:00+08:00","type":"default","extra":""},
		{"id":2,"content":"b","publishDate":"2026-10-02T08:00:00+08:00","type":"default","extra":""}
	]}`)
	ctx := context.Background()

	countUnread := func(uid uint64) int {
		t.Helper()
		_, views, err := ListFor(ctx, gdb, uid)
		if err != nil {
			t.Fatalf("读取公告失败: %v", err)
		}
		n := 0
		for _, v := range views {
			if !v.Read {
				n++
			}
		}
		return n
	}

	if n := countUnread(42); n != 2 {
		t.Fatalf("初始未读应为 2，实际 %d", n)
	}
	if err := MarkReadAll(ctx, gdb, 42, Load().Items); err != nil {
		t.Fatalf("整组标记已读失败: %v", err)
	}
	if n := countUnread(42); n != 0 {
		t.Fatalf("整组已读后未读应为 0，实际 %d", n)
	}
	// 重复标记不报错（唯一键冲突静默忽略）。
	if err := MarkReadAll(ctx, gdb, 42, Load().Items); err != nil {
		t.Fatalf("重复标记应幂等: %v", err)
	}

	// 同 id 编辑内容不重现；新增 id 才重现为未读。
	conf.System().Set("announcements", json.RawMessage(`{"enabled":true,"items":[
		{"id":1,"content":"a-edited","publishDate":"2026-10-01T08:00:00+08:00","type":"default","extra":""},
		{"id":2,"content":"b","publishDate":"2026-10-02T08:00:00+08:00","type":"default","extra":""},
		{"id":3,"content":"c","publishDate":"2026-10-03T08:00:00+08:00","type":"default","extra":""}
	]}`))
	if n := countUnread(42); n != 1 {
		t.Fatalf("同 id 编辑不重现、新 id 重现 → 未读应为 1，实际 %d", n)
	}
	// 其他用户互不影响。
	if n := countUnread(77); n != 3 {
		t.Fatalf("他人未读应为 3，实际 %d", n)
	}
}

// TestListForDisabledSkipsWrites 关闭公告时不查库、不写已读、返回空数组。
func TestListForDisabledSkipsWrites(t *testing.T) {
	gdb := withTestDB(t)
	setAnnouncements(t, `{"enabled":false,"items":[
		{"id":1,"content":"a","publishDate":"2026-10-01T08:00:00+08:00","type":"default","extra":""}
	]}`)
	ctx := context.Background()

	cfg, views, err := ListFor(ctx, gdb, 42)
	if err != nil {
		t.Fatalf("关闭时不应报错: %v", err)
	}
	if cfg.Enabled || len(views) != 0 {
		t.Fatalf("关闭时应返回空列表: enabled=%v views=%+v", cfg.Enabled, views)
	}
	// 关闭期间不能产生已读行，否则重新开启时老公告会被静默吞掉。
	var n int64
	if err := gdb.Model(&model.AnnouncementRead{}).Where("user_id = ?", 42).Count(&n).Error; err != nil {
		t.Fatalf("统计已读行失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("关闭时不应写入已读行，实际 %d", n)
	}
}

// TestMarkReadAllRejectsAnonymous user_id=0 会让所有匿名请求共享一行已读记录，必须拒绝。
func TestMarkReadAllRejectsAnonymous(t *testing.T) {
	if err := MarkReadAll(context.Background(), nil, 0, []Item{{ID: 1}}); err == nil {
		t.Fatalf("未登录用户写已读必须报错")
	}
}
