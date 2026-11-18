package logs

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

const callDetailTestSchema = "ld_test_call_detail"

// withCallDetailDB 在独立 schema 里准备调用记录相关表并指向 db.DB。
// 未设置 LD_TEST_DSN 时跳过（与其它集成用例同一约定）。
func withCallDetailDB(t *testing.T) *gorm.DB {
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
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + callDetailTestSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+callDetailTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.Usage{}, &model.UsageArchive{}, &model.CallIO{},
		&model.Task{}, &model.User{}, &model.APIKey{}, &model.Channel{}, &model.File{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + callDetailTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

// TestDetailByIDMatchesColumns 是详情查询的**列对齐**回归锁。
//
// 为什么必须有：详情用一条手写 SELECT 扫进「匿名结构体 + 内嵌 rawRow」，
// 列名与字段名一旦对不上，GORM 不会报错 —— 它会静默留零值。
// 表现就是"详情页某些字段永远是 0"，而且列表看起来完全正常，很难定位。
//
// 所以这里逐字段断言（尤其是 `"group"` 保留字、`elapsed_ms` 表达式、`video_*` 新列）。
func TestDetailByIDMatchesColumns(t *testing.T) {
	gdb := withCallDetailDB(t)
	ctx := context.Background()

	started := time.Now().Add(-3 * time.Second)
	finished := started.Add(2500 * time.Millisecond)
	u := model.Usage{
		CreatedAt: started, UserID: 11, KeyID: 22, ChannelID: 33, AdapterID: "openai",
		// 上游密钥归属：加列后这里没跟上就会「详情页这格永远是空」，
		// 而列表看起来完全正常 —— 正是本用例要拦的那类静默错位。
		ChannelKeyID: "0123456789abcdef0123456789abcdef",
		Model:        "gpt-4o", Modality: "chat", HTTPStatus: 200, Kind: "relay",
		Status: "settled", CallKind: "async", UsageSemantic: "openai",
		PromptTokens: 100, CompletionTokens: 50, CacheTokens: 10,
		Charge: 1234, ChargeBeforeCap: 2000, CapReason: "frozen3x", Frozen: 1500,
		LatencyMs: 2500, UpstreamMs: 1800, FirstTokenMs: 300, Attempts: 2,
		VideoResolution: "720p", VideoSeconds: 12, VideoSecondsSource: "upstream",
		RefImages: 2, RefVideos: 1, RefAudios: 3, RefVideoSeconds: 8,
		TierPrice: 900, TierSource: "resolution",
		StartedAt: &started, FinishedAt: &finished,
		Group: "vip", IP: "1.2.3.4", RequestID: "req_x", UpstreamRequestID: "up_x",
		ErrorMsg: "", UsageSource: "upstream", Anomaly: "image_n_mismatch",
		TaskID: 77, RequestDigest: "abc123",
		RawUsage: `{"prompt_tokens":100}`,
	}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("插入 usage 失败: %v", err)
	}
	if err := gdb.Model(&model.Usage{}).Where("id = ?", u.ID).
		Update("pricing_snapshot", model.JSON(`{"quota_per_unit":500000,"model_id":"gpt-4o"}`)).Error; err != nil {
		t.Fatalf("写入快照失败: %v", err)
	}
	io := model.CallIO{
		UsageID: u.ID, UserID: 11,
		ReqRaw: `{"model":"gpt-4o"}`, ReqBytes: 16, ReqSHA256: "deadbeef",
		RespRaw: `{"ok":true}`, RespError: `{"error":"boom"}`, RespBytes: 11, SSEFrames: 3,
		Media:     model.JSON(`[{"kind":"image","url":"https://up.example.com/a.png","artifact_index":0}]`),
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}
	if err := gdb.Create(&io).Error; err != nil {
		t.Fatalf("插入 call_io 失败: %v", err)
	}
	if err := gdb.Create(&model.Task{ID: 77, PublicID: "task_01HZZZZZZZZZZZZZZZZZZZZZZY", UserID: 11, Status: "succeeded", Kind: "video", Model: "metaso-h3"}).Error; err != nil {
		t.Fatalf("插入 task 失败: %v", err)
	}

	d, err := DetailByID(ctx, ScopeHot, u.ID, "", "/api/admin/tasks")
	if err != nil {
		t.Fatalf("查询详情失败: %v", err)
	}
	if d == nil {
		t.Fatal("详情为空（列名/表名映射错误会让 Scan 静默成功但 ID=0）")
	}

	// 逐字段核对：这些正是"详情页会显示 0/空"的部分。
	// 对外任务号必须随行扫出：它来自相关子查询（tasks.public_id），
	// 列名与字段名对不上时 GORM 静默留空，门户就会给一个"有任务但点不开"的行。
	if d.Item.TaskPublicID != "task_01HZZZZZZZZZZZZZZZZZZZZZZY" {
		t.Errorf("task_public_id 未落: %q", d.Item.TaskPublicID)
	}
	if d.CallKind != "async" || d.UsageSemantic != "openai" {
		t.Errorf("call_kind/usage_semantic 未落: %+v", d.Item)
	}
	if d.VideoResolution != "720p" || d.VideoSeconds != 12 || d.VideoSecondsSource != "upstream" {
		t.Errorf("视频维度未落: %s/%d/%s", d.VideoResolution, d.VideoSeconds, d.VideoSecondsSource)
	}
	if d.RefImages != 2 || d.RefVideos != 1 || d.RefAudios != 3 || d.RefVideoSeconds != 8 {
		t.Errorf("素材计数未落: %d/%d/%d/%d", d.RefImages, d.RefVideos, d.RefAudios, d.RefVideoSeconds)
	}
	if d.TierPrice != 900 || d.TierSource != "resolution" {
		t.Errorf("档位未落: %d/%s", d.TierPrice, d.TierSource)
	}
	if d.ChargeBeforeCap != 2000 || d.CapReason != "frozen3x" || d.Frozen != 1500 {
		t.Errorf("截顶/冻结未落: %d/%s/%d", d.ChargeBeforeCap, d.CapReason, d.Frozen)
	}
	if d.UpstreamMs != 1800 || d.FirstTokenMs != 300 || d.Attempts != 2 {
		t.Errorf("耗时拆分未落: %d/%d/%d", d.UpstreamMs, d.FirstTokenMs, d.Attempts)
	}
	if d.Group != "vip" { // "group" 是 SQL 保留字，漏引号这条会失败
		t.Errorf("group 列未取到（引号问题）: %q", d.Group)
	}
	if d.RequestDigest != "abc123" || d.RawUsage == "" {
		t.Errorf("digest/raw_usage 未落: %q/%q", d.RequestDigest, d.RawUsage)
	}
	if d.ChannelKeyID != "0123456789abcdef0123456789abcdef" {
		t.Errorf("channel_key_id 未取到（列名/字段没对齐）: %q", d.ChannelKeyID)
	}
	if d.Task == nil || d.Task.ID != "task_01HZZZZZZZZZZZZZZZZZZZZZZY" || d.Task.Status != "succeeded" {
		t.Errorf("任务引用未解析: %+v", d.Task)
	}
	// 任务对象与任务端点同形：id 是十进制字符串、进度派生、contents 恒为数组。
	if d.Task != nil && (d.Task.Progress != 100 || d.Task.Contents == nil || d.Task.Object != "task") {
		t.Errorf("任务对象形状不对（应与 /v1/tasks/{id} 一致）: %+v", d.Task)
	}
	if d.IO == nil || d.IO.ReqSHA256 != "deadbeef" || d.IO.RespError == "" || len(d.IO.Media) != 1 {
		t.Errorf("报文未解析: %+v", d.IO)
	}
	if d.IO != nil && d.IO.Media[0].URL != "https://up.example.com/a.png" {
		t.Errorf("媒体链接未解析: %+v", d.IO.Media)
	}
	if len(d.PricingSnapshot) == 0 {
		t.Error("pricing_snapshot 未返回")
	}
	// 名字解析（用户/KEY/渠道）
	if d.Username != "" || d.ChannelName != "" {
		t.Errorf("展示名解析不应 panic 或串行: %q/%q", d.Username, d.ChannelName)
	}
}

// TestDetailByIDForUserIsolates 校验门户越权：别人的 id 必须查不到。
func TestDetailByIDForUserIsolates(t *testing.T) {
	gdb := withCallDetailDB(t)
	ctx := context.Background()

	u := model.Usage{UserID: 100, Status: "settled", Model: "gpt-4o"}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("插入 usage 失败: %v", err)
	}
	if d, err := DetailByIDForUser(ctx, u.ID, 100, "", "/api/portal/tasks"); err != nil || d == nil {
		t.Fatalf("本人应能查到: d=%v err=%v", d, err)
	}
	if d, err := DetailByIDForUser(ctx, u.ID, 999, "", "/api/portal/tasks"); err != nil || d != nil {
		t.Fatalf("他人不应查到（越权过滤必须在 SQL 里）: d=%v err=%v", d, err)
	}
	if d, err := DetailByIDForUser(ctx, u.ID, 0, "", "/api/portal/tasks"); err != nil || d != nil {
		t.Fatalf("user_id=0 必须直接拒绝: d=%v err=%v", d, err)
	}
}

// TestListElapsedForPending 校验进行中记录的 elapsed_ms（总耗时列的数据来源）。
func TestListElapsedForPending(t *testing.T) {
	gdb := withCallDetailDB(t)
	ctx := context.Background()

	started := time.Now().Add(-90 * time.Second)
	rows := []model.Usage{
		{CreatedAt: started, UserID: 1, Status: "pending", Model: "m1", StartedAt: &started},
		{CreatedAt: started, UserID: 1, Status: "settled", Model: "m2", StartedAt: &started,
			FinishedAt: ptrTime(started.Add(2 * time.Second)), LatencyMs: 2000},
	}
	if err := gdb.Create(&rows).Error; err != nil {
		t.Fatalf("插入 usage 失败: %v", err)
	}

	items, total, err := List(ctx, Filter{Scope: ScopeHot, UserID: 1}, 1, 10, "")
	if err != nil {
		t.Fatalf("列表查询失败: %v", err)
	}
	if total != 2 || len(items) != 2 {
		t.Fatalf("条数不符: total=%d len=%d", total, len(items))
	}
	byModel := map[string]Item{}
	for _, it := range items {
		byModel[it.Model] = it
	}
	// pending：elapsed 应约等于 90s（允许几秒误差），且 latency_ms 仍为 0
	if e := byModel["m1"].ElapsedMs; e < 85_000 || e > 120_000 {
		t.Errorf("pending 的 elapsed_ms 应约为 90s，实际 %d", e)
	}
	if byModel["m1"].LatencyMs != 0 {
		t.Errorf("pending 的 latency_ms 应为 0，实际 %d", byModel["m1"].LatencyMs)
	}
	// 终态：elapsed 与 latency 同源
	if e := byModel["m2"].ElapsedMs; e != 2000 {
		t.Errorf("终态 elapsed_ms 应等于 2000，实际 %d", e)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// TestDetailBillingFactsAndImageDims 计费方式与行内快照必须下发。
//
// 门户拿不到 pricing_snapshot（含平台定价参数），但"这笔按什么算"
// 与"当时的结算币种"必须能解释 —— 否则金额是个无字天书，
// 而且用全局币种会让历史记录在切换后自行变味。
func TestDetailBillingFactsAndImageDims(t *testing.T) {
	gdb := withCallDetailDB(t)
	ctx := context.Background()

	u := model.Usage{
		UserID: 11, KeyID: 22, Model: "gpt-image-1", Modality: "image",
		Kind: "relay", Status: "settled", CallKind: "sync",
		ImageSpec: "1024x1024", ImageQuality: "high", ImageN: 2,
		Charge: 4000, TierPrice: 2000, TierSource: "resolution",
	}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("插入 usage 失败: %v", err)
	}
	if err := gdb.Model(&model.Usage{}).Where("id = ?", u.ID).Update("pricing_snapshot",
		model.JSON(`{"billing_mode":"spec","currency":"USD","model_id":"gpt-image-1"}`)).Error; err != nil {
		t.Fatalf("写入快照失败: %v", err)
	}

	d, err := DetailByID(ctx, ScopeHot, u.ID, "", "/api/admin/tasks")
	if err != nil || d == nil {
		t.Fatalf("查询详情失败: %v", err)
	}
	if d.BillingMode != "spec" {
		t.Fatalf("billing_mode 应为 spec，实际 %q", d.BillingMode)
	}
	// 币种必须取**行内快照**而不是当前站点币种：记账凭证不能因为后来切币种而改变含义。
	if d.Currency != "USD" {
		t.Fatalf("currency 应取行内快照 USD，实际 %q", d.Currency)
	}
	if d.ImageN != 2 || d.ImageSpec != "1024x1024" {
		t.Fatalf("图片维度未落: n=%d spec=%s", d.ImageN, d.ImageSpec)
	}

	// 门户裁剪：快照必须不下发（连字段都不存在），但计费方式与基准要留下。
	p := PortalDetail(d)
	if p == nil {
		t.Fatal("门户详情不应为空")
	}
	if p.BillingMode != "spec" {
		t.Fatalf("门户丢失计费方式: mode=%q", p.BillingMode)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("序列化门户详情失败: %v", err)
	}
	for _, key := range []string{"pricing_snapshot", "raw_usage", "channel", "upstream", "cost"} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("门户详情不应包含 %q: %s", key, raw)
		}
	}
}

// TestDetailFallsBackToTaskParams 历史异步记录（无 call_io）用任务快照兜底。
//
// 没有这条兜底，C1-R3 之前创建的任务在详情页永远显示"未采集"，
// 而用户会认为是新功能坏了。
func TestDetailFallsBackToTaskParams(t *testing.T) {
	gdb := withCallDetailDB(t)
	ctx := context.Background()

	u := model.Usage{
		UserID: 11, KeyID: 22, Model: "metaso-h3", Modality: "video",
		Kind: "relay", Status: "settled", CallKind: "async", TaskID: 88,
	}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("插入 usage 失败: %v", err)
	}
	task := model.Task{
		ID: 88, UserID: 11, Status: "succeeded", Kind: "video", Model: "metaso-h3",
		RequestParams: model.JSON(`{"prompt":"海边日落","seconds":5}`),
		UpstreamUsage: model.JSON(`{"output_seconds":5}`),
		ArtifactCount: 1,
	}
	if err := gdb.Create(&task).Error; err != nil {
		t.Fatalf("插入 task 失败: %v", err)
	}

	d, err := DetailByID(ctx, ScopeHot, u.ID, "", "/api/admin/tasks")
	if err != nil || d == nil {
		t.Fatalf("查询详情失败: %v", err)
	}
	if d.IO == nil {
		t.Fatal("没有 call_io 时应由任务快照兜底，而不是留空")
	}
	if d.IO.ReqSource != "snapshot" {
		t.Fatalf("兜底来源应标 snapshot（不能冒充客户端原文），实际 %q", d.IO.ReqSource)
	}
	if !strings.Contains(d.IO.ReqRaw, "海边日落") {
		t.Fatalf("请求参数应来自 tasks.request_params，实际 %q", d.IO.ReqRaw)
	}
	if d.IO.RespSource != "usage" || !strings.Contains(d.IO.RespRaw, "output_seconds") {
		t.Fatalf("响应应来自 tasks.upstream_usage，实际 %q/%q", d.IO.RespSource, d.IO.RespRaw)
	}
}
