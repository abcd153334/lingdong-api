package callio

import (
	"context"
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

const storeTestSchema = "ld_test_callio_store"

// withStoreDB 在独立 schema 里准备 call_io 表并指向 db.DB。
// 未设置 LD_TEST_DSN 时跳过（与其它集成用例同一约定）。
//
// 固定 full 口径：本文件的用例断言的是「脱敏后的原文」与「预截断」，
// 而默认口径是 meta（只留结构摘要）。不显式声明的话，用例会因为
// "正文没进库"而失败 —— 那不是缺陷，是切错了口径。
func withStoreDB(t *testing.T) {
	t.Helper()
	t.Setenv("LD_CALL_IO_BODY", "full")
	// Store 的上限来自 conf.EnvConfig()，未 Load 时是零值 → LimitsOf 返回 0 →
	// 每次 Store 都静默 false，用例会误报成"写入失败"而不是"没初始化"。
	conf.Load()
	dsn := os.Getenv("LD_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 LD_TEST_DSN，跳过数据库集成用例")
	}
	silent := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), silent)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + storeTestSchema).Error; err != nil {
		t.Fatalf("创建 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+storeTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.CallIO{}); err != nil {
		t.Fatalf("迁移 call_io 失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		if sqlDB, e := gdb.DB(); e == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + storeTestSchema + " CASCADE").Error
		if sqlDB, e := admin.DB(); e == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
}

func readRow(t *testing.T, usageID uint64) model.CallIO {
	t.Helper()
	var row model.CallIO
	if err := db.DB.WithContext(context.Background()).
		Where("usage_id = ?", usageID).First(&row).Error; err != nil {
		t.Fatalf("读取 call_io 失败: %v", err)
	}
	return row
}

// TestStorePatchKeepsReqRaw 是 P0-4 的回归锁。
//
// 异步任务是"提交时写请求、终态时写响应"两次写。ON CONFLICT 的 DoUpdates
// 默认全列覆盖，第二次会把 req_raw 清成空串 —— 而表面上一切正常：
// 任务成功、详情页打开、只是请求参数没了。这条用例专门盯这个。
func TestStorePatchKeepsReqRaw(t *testing.T) {
	withStoreDB(t)
	ctx := context.Background()

	if ok := Store(ctx, Rec{UsageID: 1, UserID: 7, ReqRaw: []byte(`{"model":"x","prompt":"hi"}`)}); !ok {
		t.Fatal("首次写入应成功")
	}
	if ok := Store(ctx, Rec{
		UsageID: 1, UserID: 7, Patch: true,
		RespRaw: []byte(`{"seconds":5}`),
	}); !ok {
		t.Fatal("Patch 写入应成功")
	}

	row := readRow(t, 1)
	if !strings.Contains(row.ReqRaw, `"prompt":"hi"`) {
		t.Fatalf("Patch 覆盖了 req_raw（P0-4 回归）：%q", row.ReqRaw)
	}
	if !strings.Contains(row.RespRaw, `"seconds":5`) {
		t.Fatalf("resp_raw 应已写入：%q", row.RespRaw)
	}
}

// TestStorePatchKeepsUpstreamRaw 出站报文也必须扛住 Patch 覆盖。
//
// 失败释放路径只带 RespError；若把 upstream_* 无条件加进 DoUpdates，
// 它会把已经写好的「实际发出去的报文」清成零值 —— 排障的第一现场就没了。
func TestStorePatchKeepsUpstreamRaw(t *testing.T) {
	withStoreDB(t)
	ctx := context.Background()

	if ok := Store(ctx, Rec{
		UsageID: 2, UserID: 7,
		ReqRaw:           []byte(`{"model":"x"}`),
		UpstreamRaw:      []byte(`{"model":"upstream-x","prompt":"hi"}`),
		UpstreamURL:      "https://up.example.com/v1/images?async=true",
		UpstreamStatus:   200,
		UpstreamAttempts: 2,
		RespSource:       "raw",
		RespRaw:          []byte(`{"ok":true}`),
	}); !ok {
		t.Fatal("首次写入应成功")
	}
	// 终态补写：只带错误，不带任何 upstream 字段。
	if ok := Store(ctx, Rec{UsageID: 2, UserID: 7, Patch: true, RespError: []byte("upstream 400")}); !ok {
		t.Fatal("Patch 写入应成功")
	}

	row := readRow(t, 2)
	if !strings.Contains(row.UpstreamRaw, `"upstream-x"`) {
		t.Fatalf("Patch 覆盖了 upstream_raw：%q", row.UpstreamRaw)
	}
	if row.UpstreamURL == "" || row.UpstreamStatus != 200 || row.UpstreamAttempts != 2 {
		t.Fatalf("upstream 元信息被清空：url=%q status=%d attempts=%d",
			row.UpstreamURL, row.UpstreamStatus, row.UpstreamAttempts)
	}
	if row.BodyMode != "full" {
		t.Fatalf("body_mode 应为 full，实际 %q", row.BodyMode)
	}
	if row.RespSource != "raw" {
		t.Fatalf("resp_source 应为 raw，实际 %q", row.RespSource)
	}
}

// TestStorePreCapBeforeSanitize 大 body 先预截断再脱敏：既要快，也不能漏密钥。
func TestStorePreCapBeforeSanitize(t *testing.T) {
	withStoreDB(t)
	ctx := context.Background()

	// 3 MiB：远超 2×limits.Req，触发预截断。
	big := strings.Repeat("a", 3<<20)
	body := `{"api_key":"sk-live-abcdef123456","data":"` + big + `"}`
	if ok := Store(ctx, Rec{UsageID: 2, UserID: 7, ReqRaw: []byte(body)}); !ok {
		t.Fatal("写入应成功")
	}

	row := readRow(t, 2)
	if !row.ReqTruncated {
		t.Fatal("超限应标记 req_truncated")
	}
	if row.ReqBytes != int64(len(body)) {
		t.Fatalf("req_bytes 应记原始大小 %d，实际 %d", len(body), row.ReqBytes)
	}
	if len(row.ReqRaw) > 256<<10 {
		t.Fatalf("落库长度应 ≤ 256 KiB，实际 %d", len(row.ReqRaw))
	}
	if strings.Contains(row.ReqRaw, "sk-live") {
		t.Fatal("预截断后仍必须脱敏：库里出现了明文密钥")
	}
}

// TestStoreRecordsReqSource 来源标记必须落库（前端据此提示"这是快照"）。
func TestStoreRecordsReqSource(t *testing.T) {
	withStoreDB(t)
	ctx := context.Background()

	Store(ctx, Rec{UsageID: 3, UserID: 7, ReqRaw: []byte(`{}`), ReqSource: "rebuilt"})
	if got := readRow(t, 3).ReqSource; got != "rebuilt" {
		t.Fatalf("req_source 应为 rebuilt，实际 %q", got)
	}
	// 未知取值落空串，而不是原样信任（错标来源比不标更糟）。
	Store(ctx, Rec{UsageID: 4, UserID: 7, ReqRaw: []byte(`{}`), ReqSource: "bogus"})
	if got := readRow(t, 4).ReqSource; got != "" {
		t.Fatalf("非法 req_source 应落空串，实际 %q", got)
	}
}

// TestSanitizeMediaRejectsNonHTTP 非 http 链接一律不可预览（data:/javascript:）。
func TestSanitizeMediaRejectsNonHTTP(t *testing.T) {
	out := sanitizeMedia([]model.MediaItem{
		{Kind: "image", URL: "data:image/png;base64,AAAA"},
		{Kind: "image", URL: "javascript:alert(1)"},
		{Kind: "image", URL: "https://cdn.test/ok.png"},
	})
	if out[0].URL != "" || !strings.Contains(out[0].Note, "不可预览") {
		t.Fatalf("data: 链接应被清空并说明，实际 %+v", out[0])
	}
	if out[1].URL != "" {
		t.Fatalf("javascript: 链接应被清空，实际 %+v", out[1])
	}
	if out[2].URL != "https://cdn.test/ok.png" {
		t.Fatalf("正常 https 链接应保留，实际 %+v", out[2])
	}
}

// TestSanitizeMediaStripsPlatformKey 查询串里的平台密钥必须抹掉，签名参数保留。
func TestSanitizeMediaStripsPlatformKey(t *testing.T) {
	out := sanitizeMedia([]model.MediaItem{
		{Kind: "image", URL: "https://cdn.test/a.png?api_key=sk-live-1234567890&expires=99"},
		{Kind: "image", URL: "https://cdn.test/b.png?X-Amz-Signature=abcdef&expires=99"},
	})
	if strings.Contains(out[0].URL, "sk-live") {
		t.Fatalf("平台密钥必须抹掉：%q", out[0].URL)
	}
	if !strings.Contains(out[0].Note, "不可预览") {
		t.Fatalf("抹掉密钥后链接必然失效，必须说明：%+v", out[0])
	}
	// 签名参数是预览的必要条件，不能动。
	if !strings.Contains(out[1].URL, "X-Amz-Signature=abcdef") {
		t.Fatalf("签名参数必须保留：%q", out[1].URL)
	}
}

// TestSanitizeMediaDropsHugeURL 超长链接直接丢弃（防异常数据撑大 jsonb）。
func TestSanitizeMediaDropsHugeURL(t *testing.T) {
	huge := "https://cdn.test/a.png?" + strings.Repeat("x", maxMediaURLLen)
	out := sanitizeMedia([]model.MediaItem{{Kind: "image", URL: huge}})
	if out[0].URL != "" {
		t.Fatalf("超长链接应丢弃，实际长度 %d", len(out[0].URL))
	}
}
