package task

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/service/billing"
	"lingdong-api/internal/service/pricing"
)

// ---------- 纯函数用例（无 DB 依赖） ----------

// TestParseSubmitValid 校验合法提交解析出全部参数与素材摘要。
func TestParseSubmitValid(t *testing.T) {
	body := []byte(`{"model":"kling-v2-master","prompt":"一只猫在弹钢琴","seconds":5,"resolution":"720p",
		"input_reference":[{"type":"image","image_url":"https://cdn.example.com/a.png"}]}`)
	req, err := ParseSubmit(body)
	if err != nil {
		t.Fatalf("合法请求被拒: %v", err)
	}
	if req.Model != "kling-v2-master" || req.Seconds != 5 || req.Resolution != "720p" {
		t.Fatalf("参数解析不符: %+v", req)
	}
	if len(req.Refs) != 1 || req.Refs[0].Kind != "image" {
		t.Fatalf("素材解析不符: %+v", req.Refs)
	}
	if len(req.Body) == 0 || len(req.Params.References) != 1 {
		t.Fatalf("原始请求体/落库快照缺失: %+v", req.Params)
	}
}

// TestParseSubmitSeconds 校验 seconds 兼容数字与数字字符串，并饱和到 [0,3600]。
//
// 三条约定：
//   - 缺字段 / null 不是错误（图像、音乐本就没有秒数概念），但**类型非法仍必须 400**：
//     把 "abc" 当"未传"会静默按 0 秒冻结，等于白送一次生成；
//   - 负数除 -1 以外一律 400：-1 是「智能时长」的约定值（是否支持由模型价目决定），
//     其它负数没有任何语义，饱和成 0 只会把错误推迟到模态校验、给出更模糊的文案；
//   - 上限饱和到 3600（调用方不必先知道模型上限）。
func TestParseSubmitSeconds(t *testing.T) {
	cases := []struct {
		raw  string
		want int64
	}{
		{`5`, 5}, {`"10"`, 10}, {`0`, 0}, {`-1`, -1}, {`3601`, 3600}, {`"7200"`, 3600},
	}
	for _, c := range cases {
		req, err := ParseSubmit([]byte(`{"model":"m","seconds":` + c.raw + `,"resolution":"720p"}`))
		if err != nil {
			t.Fatalf("seconds=%s 被拒: %v", c.raw, err)
		}
		if req.Seconds != c.want {
			t.Fatalf("seconds=%s want %d, got %d", c.raw, c.want, req.Seconds)
		}
	}
	// -1 之外的负数没有语义：必须在解析期 400，而不是饱和成 0 再报"必须指定秒数"。
	for _, raw := range []string{`-5`, `"-2"`} {
		if _, err := ParseSubmit([]byte(`{"model":"m","seconds":` + raw + `,"resolution":"720p"}`)); err == nil {
			t.Fatalf("seconds=%s 应被拒", raw)
		}
	}
	// 缺失 / null：解析期放行，秒数归零
	for _, absent := range []string{``, `null`} {
		body := `{"model":"m","resolution":"720p"}`
		if absent != `` {
			body = `{"model":"m","seconds":` + absent + `,"resolution":"720p"}`
		}
		req, err := ParseSubmit([]byte(body))
		if err != nil {
			t.Fatalf("seconds=%s 不应在解析期被拒（模态校验延后）: %v", absent, err)
		}
		if req.Seconds != 0 {
			t.Fatalf("seconds=%s 应归零，got %d", absent, req.Seconds)
		}
	}
	// 类型非法：仍然 400
	for _, bad := range []string{`"abc"`, `1.5`} {
		body := `{"model":"m","seconds":` + bad + `,"resolution":"720p"}`
		if _, err := ParseSubmit([]byte(body)); !errors.Is(err, ErrInvalidParam) {
			t.Fatalf("seconds=%s 应被拒，got %v", bad, err)
		}
	}
}

// TestParseSubmitResolution 校验档位归一化（显式档位 / WxH 短边映射 / size 兜底 / 未知原样）。
func TestParseSubmitResolution(t *testing.T) {
	// 显式档位
	if r := normalizeVideoResolution("720P"); r != "720p" {
		t.Fatalf("显式档位应归一为小写: %s", r)
	}
	// WxH 按短边映射
	// 768p 是上游（AutoDL）的档位名，必须吸附到平台 720p 档：真机 C1 里它原来直接 400。
	cases := map[string]string{"854x480": "480p", "1280x720": "720p", "1920x1080": "1080p",
		"3840x2160": "1080p", "640*360": "480p", "768p": "720p", "768P": "720p"}
	for in, want := range cases {
		if got := normalizeVideoResolution(in); got != want {
			t.Fatalf("%s want %s, got %s", in, want, got)
		}
	}
	// resolution 缺失时回落 size 字段
	req, err := ParseSubmit([]byte(`{"model":"m","seconds":1,"size":"1080p"}`))
	if err != nil || req.Resolution != "1080p" {
		t.Fatalf("size 兜底失败: %+v/%v", req, err)
	}
	// 无法识别的档位原样返回（由计价拒绝）
	req, err = ParseSubmit([]byte(`{"model":"m","seconds":1,"resolution":"4k"}`))
	if err != nil || req.Resolution != "4k" {
		t.Fatalf("未知档位应原样透传: %+v/%v", req, err)
	}
}

// TestParseSubmitMissingFields 校验缺 model / 非法 JSON 均拒绝。
//
// 缺 resolution **不在解析期拒绝**：图像/音乐没有档位概念，视频的档位（含 resolution_fixed
// 兜底）由 validateModality 决定，那里才知道模态与价目。
func TestParseSubmitMissingFields(t *testing.T) {
	for _, body := range []string{
		`not-json`,
		`{"seconds":1,"resolution":"720p"}`,
		`{"model":"  ","seconds":1,"resolution":"720p"}`,
	} {
		if _, err := ParseSubmit([]byte(body)); !errors.Is(err, ErrInvalidParam) {
			t.Fatalf("body=%s 应被拒为 ErrInvalidParam，got %v", body, err)
		}
	}
	// 缺 resolution 且缺 seconds：解析期放行（模态校验负责）
	if req, err := ParseSubmit([]byte(`{"model":"m"}`)); err != nil {
		t.Fatalf("缺 seconds/resolution 不应在解析期被拒: %v", err)
	} else if req.Resolution != "" {
		t.Fatalf("缺失档位应留空，got %q", req.Resolution)
	}
}

// TestParseSubmitSSRF 校验素材链接协议白名单与内网地址拦截（§4.7.6 R8）。
func TestParseSubmitSSRF(t *testing.T) {
	bad := []string{
		"http://127.0.0.1/a.png", "http://localhost/a.png", "http://10.0.0.5/a.png",
		"http://192.168.1.1/a.png", "http://169.254.1.1/a.png", "ftp://cdn.example.com/a.png",
		"http://nas.local/a.png", "http://intranet/a.png", "http://svc.internal/a.png",
	}
	for _, link := range bad {
		body := `{"model":"m","seconds":1,"resolution":"720p","input_reference":[{"type":"image","image_url":"` + link + `"}]}`
		if _, err := ParseSubmit([]byte(body)); !errors.Is(err, ErrReferenceInvalid) {
			t.Fatalf("素材 %s 应被拒为 ErrReferenceInvalid，got %v", link, err)
		}
	}
	// 合法公网链接放行
	ok := `{"model":"m","seconds":1,"resolution":"720p","input_reference":[{"type":"image","image_url":"https://cdn.example.com/a.png"}]}`
	if _, err := ParseSubmit([]byte(ok)); err != nil {
		t.Fatalf("公网链接应放行: %v", err)
	}
	// data: URL 不做 SSRF 校验
	dataURL := `{"model":"m","seconds":1,"resolution":"720p","input_reference":[{"type":"image","image_url":"data:image/png;base64,aGVsbG8="}]}`
	if _, err := ParseSubmit([]byte(dataURL)); err != nil {
		t.Fatalf("data: URL 应放行: %v", err)
	}
}

// TestParseSubmitRefKindAndDigest 校验素材类别推断与摘要口径（data: 取解码内容）。
func TestParseSubmitRefKindAndDigest(t *testing.T) {
	// 未显式声明 type 时按已填链接字段推断
	body := []byte(`{"model":"m","seconds":1,"resolution":"720p","input_reference":[
		{"image_url":"https://cdn.example.com/a.png"},
		{"audio_url":"https://cdn.example.com/b.mp3"},
		{"video_url":"https://cdn.example.com/c.mp4"}]}`)
	req, err := ParseSubmit(body)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	kinds := []string{req.Refs[0].Kind, req.Refs[1].Kind, req.Refs[2].Kind}
	if kinds[0] != "image" || kinds[1] != "audio" || kinds[2] != "video" {
		t.Fatalf("素材类别推断不符: %v", kinds)
	}

	// 空链接 / 无链接字段 → 参数不合法
	if _, err := ParseSubmit([]byte(`{"model":"m","seconds":1,"resolution":"720p","input_reference":[{"type":"image"}]}`)); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("空素材链接应被拒，got %v", err)
	}

	// data: URL 摘要按解码内容计算，size 为解码字节数
	req, err = ParseSubmit([]byte(`{"model":"m","seconds":1,"resolution":"720p","input_reference":[{"type":"image","image_url":"data:image/png;base64,aGVsbG8="}]}`))
	if err != nil {
		t.Fatalf("data: URL 解析失败: %v", err)
	}
	sum := sha256.Sum256([]byte("hello"))
	if req.Refs[0].SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("data: URL 摘要应按解码内容计算: %s", req.Refs[0].SHA256)
	}
	if req.Refs[0].SizeBytes != 5 {
		t.Fatalf("data: URL size 应为解码长度 5: %d", req.Refs[0].SizeBytes)
	}
}

// TestProgressOf 校验进度由状态派生（queued=0、running=50、终态=100）。
func TestProgressOf(t *testing.T) {
	cases := map[string]int{
		StatusQueued: 0, StatusRunning: 50,
		StatusSucceeded: 100, StatusFailed: 100, StatusExpired: 100, StatusCanceled: 100,
	}
	for st, want := range cases {
		if got := ProgressOf(st); got != want {
			t.Fatalf("status=%s want %d, got %d", st, want, got)
		}
	}
}

// TestViewOf 校验对外任务对象：进度派生、终态才给 completed_at、产物地址与 kind 同时给。
func TestViewOf(t *testing.T) {
	artifacts := []ArtifactEntry{{Index: 0, Kind: "video", ContentType: "video/mp4", Bytes: 10}}
	run := &model.Task{ID: 7, PublicID: "task_01HZZZZZZZZZZZZZZZZZZZZZZX", Status: StatusRunning, Kind: KindVideo, Model: "m", Frozen: 100}
	v := ViewOf(run, ViewOptions{ContentBase: "/v1/tasks", Artifacts: artifacts})
	if v.Progress != 50 || v.Object != "task" || v.ID != run.PublicID {
		t.Fatalf("running 视图不符: %+v", v)
	}
	// 运行中不给 completed_at：给一个「现在」会让每次查询的完成时间都在变。
	if v.CompletedAt != nil {
		t.Fatalf("running 不该有 completed_at: %+v", v.CompletedAt)
	}
	// 产物地址与 kind 必须在**同一项**里同时给出（改造前一边有 url 没 kind、一边反过来）。
	if len(v.Contents) != 1 || v.Contents[0].URL != "/v1/tasks/"+run.PublicID+"/contents/0" || v.Contents[0].Kind != "video" {
		t.Fatalf("产物投影不符: %+v", v.Contents)
	}
	if v.Error != nil {
		t.Fatalf("running 的 error 必须为 nil: %+v", v.Error)
	}

	done := &model.Task{ID: 7, PublicID: run.PublicID, Status: StatusSucceeded, Kind: KindVideo, Model: "m", Settled: 120}
	dv := ViewOf(done, ViewOptions{ContentBase: "/v1/tasks"})
	if dv.Progress != 100 || dv.CompletedAt == nil {
		t.Fatalf("终态视图不符: %+v", dv)
	}
	// 没有产物时是空数组而不是 null：前端直接 map，不再判空。
	if dv.Contents == nil || len(dv.Contents) != 0 {
		t.Fatalf("无产物应是空数组: %#v", dv.Contents)
	}

	failed := &model.Task{ID: 8, Status: StatusFailed}
	fv := ViewOf(failed, ViewOptions{})
	if fv.Error == nil || fv.Error.Message == "" {
		t.Fatalf("失败必须带分类文案: %+v", fv.Error)
	}
}

func TestUsageOfRequest(t *testing.T) {
	req := &SubmitRequest{
		Seconds: 3, Resolution: "1440p",
		Params: params{Resolution: "1080p"},
		Refs:   []RefSummary{{Kind: "image"}, {Kind: "image"}, {Kind: "video"}, {Kind: "audio"}},
	}
	u := usageOfRequest(req)
	if u.VideoSeconds != 3 || u.VideoResolution != "1080p" || u.RefImages != 2 || u.RefVideos != 1 || u.RefAudios != 1 {
		t.Fatalf("用量口径不符: %+v", u)
	}
}

// TestTruncate 校验按字符（非字节）截断。
func TestTruncate(t *testing.T) {
	if got := truncate("中文测试", 2); got != "中文" {
		t.Fatalf("want 中文, got %s", got)
	}
	if got := truncate("abc", 10); got != "abc" {
		t.Fatalf("未超长不应截断: %s", got)
	}
}

// ---------- 数据库集成用例（需 LD_TEST_DSN；未设置则跳过） ----------
// LD_TEST_DSN 必须是 URL 形式（如 postgres://user:pass@host:5432/db?sslmode=disable），
// 因为用例会在其后追加 search_path 以隔离到独立 schema；关键字/值形式不支持追加参数。

const testSchema = "ld_test_s7"

// withTestDB 在独立 schema 中准备测试表，返回连接；测试结束清理 schema 并恢复全局连接。
func withTestDB(t *testing.T) *gorm.DB {
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
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + testSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+testSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	// Vendor / Model 必须建表：Retry 会走 pricing.Get() 校验「模型是否仍启用」，
	// 缺表时失败点会停在 "relation vendors does not exist" 上，看起来像基础设施故障，
	// 实际只是测试 schema 的建表清单没跟上被测代码新增的依赖。
	if err := gdb.AutoMigrate(&model.User{}, &model.APIKey{}, &model.Usage{}, &model.Transaction{}, &model.Task{},
		&model.Channel{}, &model.ModelBinding{}, &model.Vendor{}, &model.Model{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	// 定价目录是**进程级**缓存（TTL 10s），不清的话：
	// 上一个用例把空目录缓存下来 → 本用例造的模型查不到 → Retry 报「模型已不存在」，
	// 而错误信息完全指不到缓存上，只会在 CI 里表现为"换个顺序就过"。
	pricing.InvalidateLocalCache()
	// 目录默认要求「存在启用绑定」，这些用例只关心计费/重试逻辑、不关心目录口径，
	// 关掉它避免每个用例都要多造一条绑定（多造的绑定还会污染路由候选）。
	t.Setenv("LD_CATALOG_REQUIRE_BINDING", "0")
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + testSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
		pricing.InvalidateLocalCache()
	})
	return gdb
}

// seedUserAndKey 造一条测试用户与 KEY（唯一后缀避免唯一索引冲突）。
func seedUserAndKey(t *testing.T, gdb *gorm.DB, quota int64) (uint64, uint64) {
	t.Helper()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	u := &model.User{Username: "s7" + suffix, PasswordHash: "x", Balance: quota, Group: "default", AffCode: "aff" + suffix}
	if err := gdb.Create(u).Error; err != nil {
		t.Fatalf("造测试用户失败: %v", err)
	}
	k := &model.APIKey{UserID: u.ID, KeyCipher: "x", KeyHash: "h" + suffix, Name: "测试KEY", Status: 1, Group: "default"}
	if err := gdb.Create(k).Error; err != nil {
		t.Fatalf("造测试 KEY 失败: %v", err)
	}
	return u.ID, k.ID
}

// TestCASConcurrentAdvance 校验并发推进仅有唯一一方命中（CAS 语义）。
func TestCASConcurrentAdvance(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	row := &model.Task{UserID: 1, Model: "m", Status: StatusQueued, Kind: KindVideo, PricingSnapshot: model.MustJSON(billing.Snapshot{})}
	if err := gdb.Create(row).Error; err != nil {
		t.Fatalf("造任务行失败: %v", err)
	}

	var wg sync.WaitGroup
	var hits int64
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := cas(ctx, row.ID, advanceFrom, StatusSucceeded, nil)
			if err != nil {
				t.Errorf("并发 CAS 报错: %v", err)
				return
			}
			if ok {
				atomic.AddInt64(&hits, 1)
			}
		}()
	}
	wg.Wait()
	if hits != 1 {
		t.Fatalf("并发 CAS 应仅 1 方命中，实际 %d", hits)
	}
}

// TestCASTerminalNotOverwritten 校验终态不可被迟到结果覆盖。
func TestCASTerminalNotOverwritten(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	row := &model.Task{UserID: 1, Model: "m", Status: StatusQueued, Kind: KindVideo, PricingSnapshot: model.MustJSON(billing.Snapshot{})}
	if err := gdb.Create(row).Error; err != nil {
		t.Fatalf("造任务行失败: %v", err)
	}
	if ok, err := cas(ctx, row.ID, advanceFrom, StatusSucceeded, nil); err != nil || !ok {
		t.Fatalf("首次推进应命中: %v/%v", ok, err)
	}
	// 终态 succeeded 之后，迟到 failed 结果被拒
	ok, err := cas(ctx, row.ID, advanceFrom, StatusFailed, map[string]any{"error_msg": "迟到失败"})
	if err != nil || ok {
		t.Fatalf("终态不应被覆盖: ok=%v err=%v", ok, err)
	}
	var got model.Task
	if err := gdb.First(&got, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusSucceeded {
		t.Fatalf("终态被改写为 %s", got.Status)
	}
}

// TestFreezeReopenReleaseRefund 校验异步退款闭环：Freeze → Reopen 读回冻结额 → Release 全额回退。
func TestFreezeReopenReleaseRefund(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	uid, kid := seedUserAndKey(t, gdb, 100000)
	svc := billing.New(gdb)
	snap := billing.Snapshot{ModelID: "vid", BillingMode: model.BillingModeSpec}

	sess, err := svc.Freeze(ctx, billing.FreezeInput{
		UserID: uid, KeyID: kid, Model: "vid", Modality: "video", Group: "default", Snapshot: snap, Amount: 30000,
	})
	if err != nil {
		t.Fatalf("Freeze 失败: %v", err)
	}
	assertQuota(t, gdb, uid, kid, 70000, 30000, 30000)

	reopened, err := svc.Reopen(ctx, sess.UsageID)
	if err != nil {
		t.Fatalf("Reopen 失败: %v", err)
	}
	if reopened.Frozen != 30000 || reopened.UserID != uid || reopened.KeyID != kid {
		t.Fatalf("Reopen 未读回冻结额: %+v", reopened)
	}

	if err := svc.Release(ctx, reopened, billing.SettleInput{StatusCode: statusClientClosed, ErrorMsg: "任务已取消"}); err != nil {
		t.Fatalf("Release 失败: %v", err)
	}
	assertQuota(t, gdb, uid, kid, 100000, 0, 0)

	var usage model.Usage
	if err := gdb.First(&usage, sess.UsageID).Error; err != nil {
		t.Fatal(err)
	}
	if usage.Status != "released" || usage.Charge != 0 {
		t.Fatalf("usages 终态不符: %+v", usage)
	}
	var rel model.Transaction
	if err := gdb.Where("ref_type = ? AND ref_id = ? AND type = ?", "usage", strconv.FormatUint(sess.UsageID, 10), "release").First(&rel).Error; err != nil {
		t.Fatalf("未写 release 流水: %v", err)
	}
	if rel.Amount != 30000 || rel.BalanceAfter != 100000 {
		t.Fatalf("release 流水不符: %+v", rel)
	}

	// 会话已终态：重复 Reopen 被拒（防重复动账）
	if _, err := svc.Reopen(ctx, sess.UsageID); !errors.Is(err, billing.ErrSessionClosed) {
		t.Fatalf("终态会话应拒 Reopen，got %v", err)
	}
}

// TestExpireReleasesFrozen 校验超时 expired 走 CAS + 全额释放（E3 异步退款）。
func TestExpireReleasesFrozen(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	uid, kid := seedUserAndKey(t, gdb, 50000)
	svc := billing.New(gdb)
	sess, err := svc.Freeze(ctx, billing.FreezeInput{
		UserID: uid, KeyID: kid, Model: "vid", Modality: "video", Group: "default",
		Snapshot: billing.Snapshot{ModelID: "vid", BillingMode: model.BillingModeSpec}, Amount: 20000,
	})
	if err != nil {
		t.Fatalf("Freeze 失败: %v", err)
	}
	row := &model.Task{
		UserID: uid, KeyID: kid, UsageID: sess.UsageID, Model: "vid", Modality: "video",
		Status: StatusRunning, Kind: KindVideo, Frozen: 20000,
		PricingSnapshot: model.MustJSON(billing.Snapshot{ModelID: "vid", BillingMode: model.BillingModeSpec}),
	}
	if err := gdb.Create(row).Error; err != nil {
		t.Fatalf("造任务行失败: %v", err)
	}

	if err := Expire(ctx, row); err != nil {
		t.Fatalf("Expire 失败: %v", err)
	}
	var got model.Task
	if err := gdb.First(&got, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusExpired || got.ErrorMsg == "" {
		t.Fatalf("expired 状态/原因不符: %+v", got)
	}
	assertQuota(t, gdb, uid, kid, 50000, 0, 0)

	// 终态后再 Expire 不应重复退款
	if err := Expire(ctx, &got); err != nil {
		t.Fatalf("终态重复 Expire 不应报错: %v", err)
	}
	assertQuota(t, gdb, uid, kid, 50000, 0, 0)
}

// TestCancelReleasesQueuedTask 校验取消：仅 queued 且未提交上游可取消，CAS canceled + 全额释放。
func TestCancelReleasesQueuedTask(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	uid, kid := seedUserAndKey(t, gdb, 40000)
	svc := billing.New(gdb)
	sess, err := svc.Freeze(ctx, billing.FreezeInput{
		UserID: uid, KeyID: kid, Model: "vid", Modality: "video", Group: "default",
		Snapshot: billing.Snapshot{ModelID: "vid", BillingMode: model.BillingModeSpec}, Amount: 15000,
	})
	if err != nil {
		t.Fatalf("Freeze 失败: %v", err)
	}
	row := &model.Task{
		UserID: uid, KeyID: kid, UsageID: sess.UsageID, Model: "vid", Modality: "video",
		Status: StatusQueued, Kind: KindVideo, Frozen: 15000,
		PricingSnapshot: model.MustJSON(billing.Snapshot{ModelID: "vid", BillingMode: model.BillingModeSpec}),
	}
	if err := gdb.Create(row).Error; err != nil {
		t.Fatalf("造任务行失败: %v", err)
	}

	// 归属校验：他人取消应视为不存在（不泄露存在性）
	if err := Cancel(ctx, row.ID, Scope{UserID: uid + 1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("越权取消应为 ErrNotFound，got %v", err)
	}

	if err := Cancel(ctx, row.ID, Scope{UserID: uid}); err != nil {
		t.Fatalf("取消失败: %v", err)
	}
	var got model.Task
	if err := gdb.First(&got, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusCanceled {
		t.Fatalf("状态应为 canceled，实际 %s", got.Status)
	}
	assertQuota(t, gdb, uid, kid, 40000, 0, 0)

	// 已取消任务不可再次取消（终止态拒绝）
	if err := Cancel(ctx, row.ID, Scope{UserID: uid}); !errors.Is(err, ErrNotCancelable) {
		t.Fatalf("终态重复取消应为 ErrNotCancelable，got %v", err)
	}
}

// TestCancelRejectsSubmittedTask 校验已提交上游（有 upstream_task_id）的 running 任务不可取消。
func TestCancelRejectsSubmittedTask(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	row := &model.Task{
		UserID: 1, Model: "vid", Status: StatusRunning, Kind: KindVideo, UpstreamTaskID: "up-1",
		PricingSnapshot: model.MustJSON(billing.Snapshot{ModelID: "vid", BillingMode: model.BillingModeSpec}),
	}
	if err := gdb.Create(row).Error; err != nil {
		t.Fatalf("造任务行失败: %v", err)
	}
	if err := Cancel(ctx, row.ID, AllScope()); !errors.Is(err, ErrNotCancelable) {
		t.Fatalf("running 任务应为 ErrNotCancelable，got %v", err)
	}
}

// assertQuota 断言用户余额/冻结额与 KEY 已用额度。
func assertQuota(t *testing.T, gdb *gorm.DB, uid, kid uint64, wantQuota, wantFrozen, wantKeyUsed int64) {
	t.Helper()
	var u model.User
	if err := gdb.First(&u, uid).Error; err != nil {
		t.Fatal(err)
	}
	if u.Balance != wantQuota || u.Frozen != wantFrozen {
		t.Fatalf("用户资金不符: quota=%d frozen=%d (want %d/%d)", u.Balance, u.Frozen, wantQuota, wantFrozen)
	}
	var k model.APIKey
	if err := gdb.First(&k, kid).Error; err != nil {
		t.Fatal(err)
	}
	if k.Used != wantKeyUsed {
		t.Fatalf("KEY 已用额度不符: %d (want %d)", k.Used, wantKeyUsed)
	}
}
