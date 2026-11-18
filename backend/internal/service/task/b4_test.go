package task

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/service/billing"
)

// TestNormalizeIdempotencyKey 锁死幂等键的取值边界。
//
// 三条都必须拒绝、且都必须是 400（ErrInvalidParam）：超长、含非可见 ASCII、含控制字符。
// 「截断后使用」是最坏的实现——两个不同的键被折叠成同一个，用户会凭空丢一个任务。
func TestNormalizeIdempotencyKey(t *testing.T) {
	long := strings.Repeat("a", maxIdempotencyKeyLen+1)
	cases := []struct {
		name  string
		raw   string
		want  string
		isErr bool
	}{
		{"空串不启用", "", "", false},
		{"只有空白视为不启用", "   \t\n ", "", false},
		{"普通键", "req-2024-0001", "req-2024-0001", false},
		{"去首尾空白", "  abc-123  ", "abc-123", false},
		{"大写与下划线", "REQ_0001", "REQ_0001", false},
		{"恰好到上限", strings.Repeat("a", maxIdempotencyKeyLen), strings.Repeat("a", maxIdempotencyKeyLen), false},
		{"超一位即拒绝", long, "", true},
		{"中文拒绝", "订单-001", "", true},
		{"全角拒绝", "ＡＢＣ", "", true},
		{"控制字符拒绝", "ab\x00cd", "", true},
		{"换行拒绝（防日志注入）", "ab\ncd", "", true},
		{"DEL 拒绝", "ab\x7fcd", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := normalizeIdempotencyKey(c.raw)
			if c.isErr {
				if err == nil {
					t.Fatalf("应被拒绝，实际通过并返回 %q", got)
				}
				if !errors.Is(err, ErrInvalidParam) {
					t.Fatalf("必须是 400 参数错（否则会变成 500），实际 %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if got != c.want {
				t.Fatalf("归一结果 %q，期望 %q", got, c.want)
			}
		})
	}
}

// TestSameSnapshot 是「重试是否按新价重快照」的判定依据：判等判错会漏审计或多审计。
func TestSameSnapshot(t *testing.T) {
	p1, p2 := int64(1000), int64(2000)
	base := billing.Snapshot{ModelID: "m", BillingMode: model.BillingModeCall, Price: model.Prices{Call: &p1}}
	same := billing.Snapshot{ModelID: "m", BillingMode: model.BillingModeCall, Price: model.Prices{Call: &p1}}
	higher := billing.Snapshot{ModelID: "m", BillingMode: model.BillingModeCall, Price: model.Prices{Call: &p2}}
	// 折扣是**微比例**（900000 = 九折）：变化必须判为不等价，否则调折扣对失败任务失效。
	ratio := billing.Snapshot{ModelID: "m", BillingMode: model.BillingModeCall, Price: model.Prices{Call: &p1}, Discount: 900_000}

	if !sameSnapshot(base, same) {
		t.Fatal("同价同组应判为等价（否则每次重试都会多写一条 resnapshot 审计）")
	}
	if sameSnapshot(base, higher) {
		t.Fatal("改价必须判为不等价（否则调价对失败任务失效，两头都是错账）")
	}
	if sameSnapshot(base, ratio) {
		t.Fatal("分组倍率变化必须判为不等价")
	}
}

// TestSubmitIdempotencyHitsExistingTask 验证幂等命中在**任何校验与出站之前**返回。
//
// 用一个不存在的模型来打：如果幂等命中发生在模型解析之后，这里必然报「模型不存在」。
// 这正好证明「重复提交不需要重新走一遍校验/冻结」——那正是客户端超时重试时最需要的。
func TestSubmitIdempotencyHitsExistingTask(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	uid, kid := seedUserAndKey(t, gdb, 100000)

	first := &model.Task{
		UserID: uid, KeyID: kid, Model: "gone-model", Modality: "video", Status: StatusQueued,
		Kind: KindVideo, IdempotencyKey: "k-1",
	}
	if err := gdb.Create(first).Error; err != nil {
		t.Fatalf("造首条任务失败: %v", err)
	}
	key := &model.APIKey{ID: kid, UserID: uid, Status: 1, Group: "default"}

	got, err := Submit(ctx, SubmitInput{
		Key: key, Req: &SubmitRequest{Model: "gone-model", Body: []byte(`{"model":"gone-model"}`)},
		ExpectModality: "video", IdempotencyKey: "k-1",
	})
	if err != nil {
		t.Fatalf("幂等命中应直接返回首次任务，实际报错: %v", err)
	}
	if got == nil || got.ID != first.ID {
		t.Fatalf("应返回 id=%d 的任务，实际 %+v", first.ID, got)
	}

	// 同一个幂等键在不同 KEY 下必须视为**不同**请求（幂等域是 KEY，不是全局）。
	other := &model.APIKey{ID: kid, UserID: uid, Status: 1, Group: "default"}
	_, err = Submit(ctx, SubmitInput{
		Key: other, Req: &SubmitRequest{Model: "gone-model", Body: []byte(`{"model":"gone-model"}`)},
		ExpectModality: "video", IdempotencyKey: "k-2",
	})
	if err == nil {
		t.Fatal("不同幂等键不应命中已有任务（必须真的走一遍提交，这里是模型不存在）")
	}
	if !strings.Contains(err.Error(), "模型") {
		t.Fatalf("非幂等命中时应走到模型校验，实际错误 %v", err)
	}
}

// TestFailOrphanSubmitSkipsTaskWithUpstreamID 是「孤儿回收」最重要的一条边界：
// 扫描与推进之间，提交线程可能刚把任务推到 running 并写上上游任务号。
// 那条任务此刻正在上游真实生成，回收它等于「钱付了、产物丢了」。
func TestFailOrphanSubmitSkipsTaskWithUpstreamID(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()

	live := &model.Task{
		UserID: 1, Model: "m", Modality: "video", Status: StatusQueued, Kind: KindVideo,
		UpstreamTaskID: "up-123",
	}
	if err := gdb.Create(live).Error; err != nil {
		t.Fatalf("造任务失败: %v", err)
	}
	if err := FailOrphanSubmit(ctx, live, "提交未完成"); err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	var got model.Task
	if err := gdb.First(&got, live.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusQueued {
		t.Fatalf("已有上游任务号的任务绝不能被回收，实际状态 %s", got.Status)
	}
	if got.ErrorMsg != "" {
		t.Fatalf("不应写入错误信息，实际 %q", got.ErrorMsg)
	}

	orphan := &model.Task{
		UserID: 1, Model: "m", Modality: "video", Status: StatusQueued, Kind: KindVideo,
	}
	if err := gdb.Create(orphan).Error; err != nil {
		t.Fatalf("造任务失败: %v", err)
	}
	if err := FailOrphanSubmit(ctx, orphan, "提交未完成"); err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	var got2 model.Task
	if err := gdb.First(&got2, orphan.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got2.Status != StatusFailed || got2.Kind != KindTimeout {
		t.Fatalf("无上游任务号的孤儿应判失败，实际 status=%s kind=%s", got2.Status, got2.Kind)
	}

	// 重复执行必须幂等（CAS 已不命中，不再动账）。
	if err := FailOrphanSubmit(ctx, orphan, "提交未完成"); err != nil {
		t.Fatalf("重复执行应静默返回: %v", err)
	}
}

// TestRetryRejectsDisabledOrMissingModel 验证重试前必须回定价目录确认模型仍可用（B4-3）。
//
// 少了这道校验，运营下线一个高成本模型后，在途失败任务仍能被反复重试真实出站。
func TestRetryRejectsDisabledOrMissingModel(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	uid, kid := seedUserAndKey(t, gdb, 100000)
	price := int64(1000)

	// 已下架
	ensureDisabledModel(t, gdb, "off-model", price)
	off := makeFailedTask(t, gdb, uid, kid, "off-model", price)
	if _, err := Retry(ctx, off.ID, AllScope()); !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("下架模型必须拒绝重试，实际 %v", err)
	}
	// 拒绝时不能动账、不能改状态
	var got model.Task
	if err := gdb.First(&got, off.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFailed {
		t.Fatalf("被拒绝的重试不应改状态，实际 %s", got.Status)
	}

	// 完全不存在
	gone := makeFailedTask(t, gdb, uid, kid, "no-such-model", price)
	if _, err := Retry(ctx, gone.ID, AllScope()); !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("不存在的模型必须拒绝重试，实际 %v", err)
	}
}

// TestIdempotencyKillSwitch 验证灰度开关 LD_SUBMIT_IDEMPOTENT=0 能整体关掉幂等。
//
// 开关必须真的挡住「查已有任务」这一步：只忽略返回值不算关（任务仍会被复用，
// 客户端以为新提交了、实际拿到旧任务）。关掉后必须真的走完整条提交链路。
func TestIdempotencyKillSwitch(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	uid, kid := seedUserAndKey(t, gdb, 100000)

	first := &model.Task{
		UserID: uid, KeyID: kid, Model: "gone-model", Modality: "video", Status: StatusQueued,
		Kind: KindVideo, IdempotencyKey: "k-off",
	}
	if err := gdb.Create(first).Error; err != nil {
		t.Fatalf("造首条任务失败: %v", err)
	}
	key := &model.APIKey{ID: kid, UserID: uid, Status: 1, Group: "default"}

	t.Setenv("LD_SUBMIT_IDEMPOTENT", "0")
	got, err := Submit(ctx, SubmitInput{
		Key: key, Req: &SubmitRequest{Model: "gone-model", Body: []byte(`{"model":"gone-model"}`)},
		ExpectModality: "video", IdempotencyKey: "k-off",
	})
	if err == nil {
		t.Fatalf("开关关闭时不应复用已有任务，实际返回 id=%d", got.ID)
	}
	if !strings.Contains(err.Error(), "模型") {
		t.Fatalf("开关关闭后应走到模型校验，实际错误 %v", err)
	}

	// 归一函数在开关关闭时必须直接返回空串（这才是「不落库」的保证）。
	if v, err := normalizeIdempotencyKey("k-off"); err != nil || v != "" {
		t.Fatalf("开关关闭时归一应返回空串，实际 %q / %v", v, err)
	}
	// 开关打开时非法值仍要 400（不能在关掉时顺手放过非法输入）。
	t.Setenv("LD_SUBMIT_IDEMPOTENT", "1")
	if _, err := normalizeIdempotencyKey("订单-001"); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("开关打开时非法键必须 400，实际 %v", err)
	}
}

// makeFailedTask 造一条 failed 任务，**不碰定价目录**。
//
// 与 seedFailedTask 的唯一区别：后者会顺手 ensurePricedModel，于是「模型不存在」这条
// 分支永远测不到（造任务时把模型补上了）。这里必须保持目录原样。
func makeFailedTask(t *testing.T, gdb *gorm.DB, uid, kid uint64, modelID string, price int64) *model.Task {
	t.Helper()
	old := &model.Usage{UserID: uid, KeyID: kid, Model: modelID, Modality: "video", Kind: "relay", Status: "released", Group: "default"}
	if err := gdb.Create(old).Error; err != nil {
		t.Fatalf("造历史会话失败: %v", err)
	}
	row := &model.Task{
		UserID: uid, KeyID: kid, Model: modelID, Modality: "video", Kind: KindVideo,
		Status: StatusFailed, ErrorMsg: "上游失败", UsageID: old.ID,
		PricingSnapshot: model.MustJSON(billing.Snapshot{ModelID: modelID, BillingMode: model.BillingModeCall, Price: model.Prices{Call: &price}}),
	}
	if err := gdb.Create(row).Error; err != nil {
		t.Fatalf("造失败任务失败: %v", err)
	}
	return row
}
