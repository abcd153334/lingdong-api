package task

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/crypto"
	"lingdong-api/internal/relay"
	"lingdong-api/internal/service/billing"
	"lingdong-api/internal/service/pricing"
)

// fakeTaskAdapter 是最小可用的异步任务适配器（仅供重试用例，不发起真实上游请求）。
//
// unreplayable 只在需要验证「适配器声明 → 平台行为」的用例里填，
// 默认零值 = 不声明任何厂商参数、所有子协议都可重放（与内置单路径适配器一致）。
type fakeTaskAdapter struct {
	err          error
	unreplayable []string
}

func (fakeTaskAdapter) Name() string   { return "fake-async" }
func (fakeTaskAdapter) Caps() []string { return []string{"video"} }

// UnreplayableSubKinds 实现 relay.UnreplayableSupplier。
func (a fakeTaskAdapter) UnreplayableSubKinds() []string { return a.unreplayable }
func (fakeTaskAdapter) Test(context.Context, *relay.ChannelConfig) error {
	return nil
}
func (fakeTaskAdapter) Chat(context.Context, *relay.ChannelConfig, *relay.RelayRequest) (*relay.RelayResponse, error) {
	return nil, errors.New("not implemented")
}
func (fakeTaskAdapter) Embeddings(context.Context, *relay.ChannelConfig, *relay.RelayRequest) (*relay.RelayResponse, error) {
	return nil, errors.New("not implemented")
}
func (fakeTaskAdapter) Completions(context.Context, *relay.ChannelConfig, *relay.RelayRequest) (*relay.RelayResponse, error) {
	return nil, errors.New("not implemented")
}
func (fakeTaskAdapter) StreamChat(context.Context, *relay.ChannelConfig, *relay.RelayRequest, func(relay.SSEFrame) error) (*relay.StreamResult, error) {
	return nil, errors.New("not implemented")
}
func (fakeTaskAdapter) Forward(context.Context, *relay.ChannelConfig, *relay.RelayRequest, string) (*relay.RawResponse, error) {
	return nil, errors.New("not implemented")
}
func (a fakeTaskAdapter) SubmitTask(_ context.Context, _ *relay.ChannelConfig, req *relay.TaskSubmitRequest) (string, error) {
	if a.err != nil {
		return "", a.err
	}
	if len(req.Body) == 0 {
		return "", errors.New("空请求体")
	}
	return "fake-upstream-1", nil
}
func (fakeTaskAdapter) PollTask(context.Context, *relay.ChannelConfig, string) (*relay.TaskStatus, error) {
	return &relay.TaskStatus{Status: "running"}, nil
}

// seedRoutableChannel 造一条可路由渠道（fake-async 适配器 + 可解密密钥 + binding）。
func seedRoutableChannel(t *testing.T, gdb *gorm.DB, modelID string) *model.Channel {
	t.Helper()
	t.Setenv("APP_ENCRYPTION_KEY", "unit-test-master-key")
	conf.Load()
	token, err := crypto.FernetEncrypt(crypto.FernetDerive("unit-test-master-key"), []byte("sk-test"))
	if err != nil {
		t.Fatalf("加密渠道密钥失败: %v", err)
	}
	keys, err := model.NewJSON([]string{token})
	if err != nil {
		t.Fatalf("序列化渠道密钥失败: %v", err)
	}
	ch := &model.Channel{Name: "fake-" + uuid.NewString()[:8], AdapterID: "fake-async", BaseURL: "https://up.invalid", Keys: keys, Status: 1, Priority: 1, Weight: 1}
	if err := gdb.Create(ch).Error; err != nil {
		t.Fatalf("造渠道失败: %v", err)
	}
	if err := gdb.Create(&model.ModelBinding{ModelID: modelID, ChannelID: ch.ID, Enabled: true, Weight: 1}).Error; err != nil {
		t.Fatalf("造绑定失败: %v", err)
	}
	return ch
}

// seedFailedTask 造一条 failed 任务（含历史计费会话行），返回任务。
func seedFailedTask(t *testing.T, gdb *gorm.DB, uid, kid uint64, modelID string, price int64) *model.Task {
	t.Helper()
	ensurePricedModel(t, gdb, modelID, price)
	old := &model.Usage{UserID: uid, KeyID: kid, Model: modelID, Modality: "video", Kind: "relay", Status: "released", Group: "default"}
	if err := gdb.Create(old).Error; err != nil {
		t.Fatalf("造历史会话失败: %v", err)
	}
	row := &model.Task{
		UserID: uid, KeyID: kid, Model: modelID, Modality: "video", Kind: KindVideo,
		Status: StatusFailed, ErrorMsg: "上游失败", UsageID: old.ID,
		PricingSnapshot: model.MustJSON(billing.Snapshot{ModelID: modelID,
			BillingMode: model.BillingModeSpec,
			SpecPrice:   model.SpecPrices{Kind: billing.SpecKindPerTask, PerTaskPrice: &price}}),
	}
	if err := gdb.Create(row).Error; err != nil {
		t.Fatalf("造失败任务失败: %v", err)
	}
	return row
}

// ensurePricedModel 保证定价目录里存在一个「启用的、有价的视频模型」（幂等）。
//
// 视频只能是 spec 模式（按量解析不出 token、按次没有"次"的语义），
// 所以这里配 per_task 价：一个任务一个价。
//
// Retry 现在会回定价目录校验「模型是否仍启用/存在」，并**按当前价目重新快照**。
// 用例里没有模型行时，失败点会停在「模型已不存在」上——那条错误看起来像业务拒绝，
// 实际只是测试数据缺了一半。
func ensurePricedModel(t *testing.T, gdb *gorm.DB, modelID string, price int64) {
	t.Helper()
	var n int64
	if err := gdb.Model(&model.Model{}).Where("id = ?", modelID).Count(&n).Error; err != nil {
		t.Fatalf("查询模型失败: %v", err)
	}
	if n == 0 {
		p := price
		row := &model.Model{
			ID: modelID, DisplayName: modelID, Vendor: "test", Modality: "video",
			BillingMode: model.BillingModeSpec, Enabled: true,
			SpecPrices: model.SpecPrices{Kind: billing.SpecKindPerTask, PerTaskPrice: &p},
		}
		if err := gdb.Create(row).Error; err != nil {
			t.Fatalf("造模型失败: %v", err)
		}
	}
	pricing.InvalidateLocalCache()
}

// ensureDisabledModel 造一个「存在但已下架」的模型（幂等），用于验证 Retry 的下架校验。
//
// 必须 Create 后再补一条 UPDATE：models.enabled 带 `default:true`，GORM 在 Create 时
// 会把 false 当零值丢弃并回填 true（Select("*") 也救不了）。不补这一条，
// 这个用例测的其实是「启用模型」，失败点会漂到「无可用渠道」上，看起来像路由问题。
func ensureDisabledModel(t *testing.T, gdb *gorm.DB, modelID string, price int64) {
	t.Helper()
	p := price
	row := &model.Model{
		ID: modelID, DisplayName: modelID, Vendor: "test", Modality: "video",
		BillingMode: model.BillingModeSpec,
		SpecPrices:  model.SpecPrices{Kind: billing.SpecKindPerTask, PerTaskPrice: &p},
	}
	if err := gdb.Create(row).Error; err != nil {
		t.Fatalf("造下架模型失败: %v", err)
	}
	if err := gdb.Model(&model.Model{}).Where("id = ?", modelID).Update("enabled", false).Error; err != nil {
		t.Fatalf("下架模型失败: %v", err)
	}
	pricing.InvalidateLocalCache()
}

// TestRetryRejectsNonRetryable 断言仅 failed/timeout 可重试：其余状态一律 ErrNotRetryable 且不动账。
func TestRetryRejectsNonRetryable(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	uid, kid := seedUserAndKey(t, gdb, 100000)
	price := int64(1000)

	for _, st := range []string{StatusQueued, StatusRunning, StatusSucceeded, StatusExpired, StatusCanceled} {
		row := seedFailedTask(t, gdb, uid, kid, "vid-"+st, price)
		if err := gdb.Model(row).Update("status", st).Error; err != nil {
			t.Fatalf("改状态失败: %v", err)
		}
		if _, err := Retry(ctx, row.ID, AllScope()); !errors.Is(err, ErrNotRetryable) {
			t.Fatalf("状态 %s 应拒绝重试，got %v", st, err)
		}
		var got model.Task
		if err := gdb.First(&got, row.ID).Error; err != nil {
			t.Fatal(err)
		}
		if got.Status != st {
			t.Fatalf("状态 %s 不应被改动，got %s", st, got.Status)
		}
	}
	assertQuota(t, gdb, uid, kid, 100000, 0, 0)
}

// TestRetryReFreezesAndResubmits 断言重试重新 Freeze 并同步重提交上游（新上游 ID），且重复重试幂等拒绝。
func TestRetryReFreezesAndResubmits(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	uid, kid := seedUserAndKey(t, gdb, 5000)
	price := int64(1000)
	ch := seedRoutableChannel(t, gdb, "vid-ok")
	if err := relay.RegisterAdapter(fakeTaskAdapter{}); err != nil {
		t.Fatalf("注册测试适配器失败: %v", err)
	}
	t.Cleanup(func() { relay.UnregisterAdapter("fake-async") })

	row := seedFailedTask(t, gdb, uid, kid, "vid-ok", price)
	oldUsageID := row.UsageID

	if _, err := Retry(ctx, row.ID, AllScope()); err != nil {
		t.Fatalf("重试失败: %v", err)
	}

	var got model.Task
	if err := gdb.First(&got, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusRunning {
		t.Fatalf("重试后状态应为 running，got %s", got.Status)
	}
	if got.UpstreamTaskID != "fake-upstream-1" {
		t.Fatalf("重试未同步重提交上游，upstream_task_id=%q", got.UpstreamTaskID)
	}
	if got.ChannelID != ch.ID {
		t.Fatalf("重试后渠道未回填，got %d want %d", got.ChannelID, ch.ID)
	}
	if got.Frozen != price || got.UsageID == oldUsageID {
		t.Fatalf("重试未挂载新计费会话: frozen=%d usage=%d(旧 %d)", got.Frozen, got.UsageID, oldUsageID)
	}
	assertQuota(t, gdb, uid, kid, 4000, 1000, price)

	var newUsage model.Usage
	if err := gdb.First(&newUsage, got.UsageID).Error; err != nil {
		t.Fatal(err)
	}
	if newUsage.Status != "pending" || newUsage.Group != "default" {
		t.Fatalf("新会话不符: %+v", newUsage)
	}

	// 重复重试（已 running）→ 幂等拒绝，且不动账。
	if _, err := Retry(ctx, row.ID, AllScope()); !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("running 任务重试应拒绝，got %v", err)
	}
	assertQuota(t, gdb, uid, kid, 4000, 1000, price)
}

// TestRetryInsufficientQuotaRevertsStatus 断言余额不足时回滚为 failed 且不动账、返回 ErrInsufficientQuota。
func TestRetryInsufficientQuotaRevertsStatus(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	uid, kid := seedUserAndKey(t, gdb, 10) // 余额低于预扣额
	price := int64(1000)
	seedRoutableChannel(t, gdb, "vid-poor")
	if err := relay.RegisterAdapter(fakeTaskAdapter{}); err != nil {
		t.Fatalf("注册测试适配器失败: %v", err)
	}
	t.Cleanup(func() { relay.UnregisterAdapter("fake-async") })

	row := seedFailedTask(t, gdb, uid, kid, "vid-poor", price)
	if _, err := Retry(ctx, row.ID, AllScope()); !errors.Is(err, billing.ErrInsufficientQuota) {
		t.Fatalf("余额不足应返回 ErrInsufficientQuota，got %v", err)
	}
	var got model.Task
	if err := gdb.First(&got, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFailed {
		t.Fatalf("余额不足应回滚为 failed，got %s", got.Status)
	}
	assertQuota(t, gdb, uid, kid, 10, 0, 0)
}

// TestRetryRejectsUnreplayableSubKind：适配器声明不可重放的子协议（Ark 原生体 content[]）
// 必须在 CAS 与冻结之前被拒 —— 判据是 tasks.subtask_kind 这一列。
//
// 为什么必须读列：子协议是适配器在 SubmitTask 里回写、由 Submit 落库的；
// request_params 在任务行插入时就已序列化完毕，里面那份副本永远是空串。
// 早期实现读的是 params.SubKind，于是这个拦截是**死代码**：原生体任务照样被重试，
// 重放成"按素材计费、只发一段文本"的错账。
func TestRetryRejectsUnreplayableSubKind(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	uid, kid := seedUserAndKey(t, gdb, 100000)
	price := int64(1000)
	seedRoutableChannel(t, gdb, "vid-native")
	if err := relay.RegisterAdapter(fakeTaskAdapter{unreplayable: []string{"fake.native"}}); err != nil {
		t.Fatalf("注册测试适配器失败: %v", err)
	}
	t.Cleanup(func() { relay.UnregisterAdapter("fake-async") })

	row := seedFailedTask(t, gdb, uid, kid, "vid-native", price)
	if err := gdb.Model(row).Updates(map[string]any{
		"subtask_kind": "fake.native", "platform": "fake-async",
	}).Error; err != nil {
		t.Fatalf("写子协议失败: %v", err)
	}
	if _, err := Retry(ctx, row.ID, AllScope()); !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("原生体任务应拒绝重试，got %v", err)
	}
	var got model.Task
	if err := gdb.First(&got, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFailed {
		t.Fatalf("被拒的重试不应改状态，got %s", got.Status)
	}
	if got.SubTaskKind != "fake.native" {
		t.Fatalf("被拒的重试不应清空子协议，got %q", got.SubTaskKind)
	}
	assertQuota(t, gdb, uid, kid, 100000, 0, 0)
}

// TestRetryRejectsAutoDurationAfterPolicyOff：运营关掉 allow_auto_duration 后，
// 按智能时长提交的失败任务不能再重试（重放会把 duration=-1 发给不接受它的上游）。
//
// 不自动降级成"上限秒数"：那会悄悄改扣款额（用户以为还是智能时长）。
func TestRetryRejectsAutoDurationAfterPolicyOff(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	uid, kid := seedUserAndKey(t, gdb, 100000)
	price := int64(1000)
	seedRoutableChannel(t, gdb, "vid-auto-off")
	if err := relay.RegisterAdapter(fakeTaskAdapter{}); err != nil {
		t.Fatalf("注册测试适配器失败: %v", err)
	}
	t.Cleanup(func() { relay.UnregisterAdapter("fake-async") })

	row := seedFailedTask(t, gdb, uid, kid, "vid-auto-off", price)
	// 任务当初按智能时长提交：冻结基准是上限秒数，上游参数是 -1。
	if err := gdb.Model(row).Update("request_params", model.MustJSON(params{
		Model: "vid-auto-off", Resolution: "720p", Seconds: 12, SecondsAuto: true,
	})).Error; err != nil {
		t.Fatalf("写请求参数失败: %v", err)
	}
	if _, err := Retry(ctx, row.ID, AllScope()); !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("价目关闭智能时长后应拒绝重试，got %v", err)
	}
	var got model.Task
	if err := gdb.First(&got, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFailed || got.Frozen != 0 {
		t.Fatalf("被拒的重试不应改状态/冻结：status=%s frozen=%d", got.Status, got.Frozen)
	}
	assertQuota(t, gdb, uid, kid, 100000, 0, 0)
}
