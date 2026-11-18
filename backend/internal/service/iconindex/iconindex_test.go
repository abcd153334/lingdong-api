package iconindex

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

// 本文件覆盖图标批量解析的**回落**与**降级**语义（必须连库）：
// 模型 → 供应商 → 首字的三级回落、软删模型仍可解析、未知 id 不炸。
// 这几条错了都不会报错，只是"历史记录里的图标悄悄变成首字"。
const iconIndexSchema = "ld_test_iconindex"

func withIconIndexDB(t *testing.T) *gorm.DB {
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
	if err := admin.Exec("DROP SCHEMA IF EXISTS " + iconIndexSchema + " CASCADE").Error; err != nil {
		t.Fatalf("清理 schema 失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA " + iconIndexSchema).Error; err != nil {
		t.Fatalf("建 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+iconIndexSchema), silent)
	if err != nil {
		t.Fatalf("连接 schema 失败: %v", err)
	}
	// 三张表都要建：模型图标要 LEFT JOIN vendors，渠道是另一条 SQL。
	if err := gdb.AutoMigrate(&model.Model{}, &model.Channel{}, &model.Vendor{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	// 全局连接是 Resolve 的唯一数据源（与生产一致，不为测试开第二参数）。
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() { db.DB = prev })
	return gdb
}

func seed(t *testing.T, gdb *gorm.DB) {
	t.Helper()
	rows := []model.Vendor{
		{ID: "openai", Name: "OpenAI", Icon: "builtin:openai"},
		{ID: "naked", Name: "无图标供应商"},
	}
	if err := gdb.Create(&rows).Error; err != nil {
		t.Fatalf("插入供应商失败: %v", err)
	}
	models := []model.Model{
		{ID: "gpt-4o", Vendor: "openai"},                          // 无自有图标 → 回落供应商
		{ID: "gpt-4o-mini", Vendor: "openai", Icon: "upload:aaa"}, // 自有图标优先
		{ID: "bare", Vendor: "naked"},                             // 供应商也没有 → ""
		{ID: "orphan", Vendor: "已删除的供应商"},                         // 供应商不存在 → ""
	}
	if err := gdb.Create(&models).Error; err != nil {
		t.Fatalf("插入模型失败: %v", err)
	}
	if err := gdb.Create(&model.Channel{ID: 7, Name: "主渠道", Icon: "builtin:openai"}).Error; err != nil {
		t.Fatalf("插入渠道失败: %v", err)
	}
}

func TestResolveModelFallback(t *testing.T) {
	gdb := withIconIndexDB(t)
	seed(t, gdb)

	ix := Resolve(context.Background(),
		[]string{"gpt-4o", "gpt-4o-mini", "bare", "orphan", "不存在的模型"},
		[]uint64{7, 99},
	)

	if got := ix.Model("gpt-4o"); got != "builtin:openai" {
		t.Fatalf("模型无自有图标应回落到供应商图标，得到 %q", got)
	}
	if got := ix.Model("gpt-4o-mini"); got != "upload:aaa" {
		t.Fatalf("自有图标必须优先于供应商，得到 %q", got)
	}
	if got := ix.Model("bare"); got != "" {
		t.Fatalf("两级都没有图标时必须是空串（由前端首字占位），得到 %q", got)
	}
	// 供应商行不存在（LEFT JOIN 的 NULL 侧）不能把整行丢掉
	if got := ix.Model("orphan"); got != "" {
		t.Fatalf("供应商缺失时应返回空串而不是丢弃该行，得到 %q", got)
	}
	if got := ix.Model("不存在的模型"); got != "" {
		t.Fatalf("未知 id 返回空串，得到 %q", got)
	}
	if got := ix.Channel(7); got != "builtin:openai" {
		t.Fatalf("渠道图标解析错误，得到 %q", got)
	}
	if got := ix.Channel(99); got != "" {
		t.Fatalf("未知渠道 id 返回空串，得到 %q", got)
	}
}

func TestResolveKeepsSoftDeletedModel(t *testing.T) {
	gdb := withIconIndexDB(t)
	seed(t, gdb)

	// 软删一个模型：历史日志里仍然出现它，图标不能因此消失
	if err := gdb.Where("id = ?", "gpt-4o").Delete(&model.Model{}).Error; err != nil {
		t.Fatalf("软删失败: %v", err)
	}
	ix := Resolve(context.Background(), []string{"gpt-4o"}, nil)
	if got := ix.Model("gpt-4o"); got != "builtin:openai" {
		t.Fatalf("软删模型仍应能解析图标（历史日志要显示当时用的模型），得到 %q", got)
	}
}

func TestResolveEmptyInput(t *testing.T) {
	withIconIndexDB(t)

	// 空入参不发查询：零值可用是对外契约的一部分（调用方不写 nil 判断）
	ix := Resolve(context.Background(), nil, nil)
	if got := ix.Model("gpt-4o"); got != "" {
		t.Fatalf("空入参应返回零值，得到 %q", got)
	}
	if got := ix.Channel(7); got != "" {
		t.Fatalf("空入参应返回零值，得到 %q", got)
	}
}

func TestResolveWithoutDB(t *testing.T) {
	prev := db.DB
	db.DB = nil
	t.Cleanup(func() { db.DB = prev })

	// DB 未初始化（单测/子命令）时不能 panic：图标是装饰，降级即可
	ix := Resolve(context.Background(), []string{"x"}, []uint64{1})
	if got := ix.Model("x"); got != "" {
		t.Fatalf("无 DB 时应返回零值，得到 %q", got)
	}
}

func TestResolveDedupesAndBatches(t *testing.T) {
	gdb := withIconIndexDB(t)
	seed(t, gdb)

	// 同一 id 重复出现（日志页两列、任务页各一行）不该重复查询；
	// 用远超 batchSize 的入参验证分批循环本身不丢数据。
	ids := make([]string, 0, 1201)
	ids = append(ids, "gpt-4o", "gpt-4o", "gpt-4o")
	for i := 0; i < 1198; i++ {
		ids = append(ids, fmt.Sprintf("missing-%d", i))
	}
	ix := Resolve(context.Background(), ids, nil)

	if got := ix.Model("gpt-4o"); got != "builtin:openai" {
		t.Fatalf("重复 id 不应影响解析结果，得到 %q", got)
	}
	if len(ix.models) != 1 {
		t.Fatalf("去重后只应有 1 条命中，得到 %d", len(ix.models))
	}
}
