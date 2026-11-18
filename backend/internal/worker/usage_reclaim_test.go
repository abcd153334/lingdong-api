package worker

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/service/task"
)

// seedReclaim 造一条 pending 会话（含冻结流水与用户冻结额）。
//
// 冻结额必须是**流水**而不是只看 users.frozen：Release 的退款额从 freeze 流水反推
// （frozenOf），只改 users.frozen 会让用例测到一个现实中不存在的状态。
func seedReclaim(t *testing.T, gdb *gorm.DB, frozen, estPrompt int64) (model.User, model.Usage) {
	t.Helper()
	u := model.User{Username: "rc" + strconv.FormatInt(time.Now().UnixNano()%1e12, 36), Balance: 10000 - frozen, Frozen: frozen}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	snap, _ := json.Marshal(map[string]any{
		"billing_mode": "spec",
		"spec_price":   map[string]any{"kind": "per_image", "tier_price": map[string]int64{"1K": 50000}},
	})
	usage := model.Usage{
		UserID: u.ID, Model: "seedream", Status: model.UsageStatusPending,
		PricingSnapshot: snap, EstPromptTokens: estPrompt,
		CreatedAt: time.Now().Add(-time.Hour),
	}
	if err := gdb.Create(&usage).Error; err != nil {
		t.Fatalf("建 usage 失败: %v", err)
	}
	if frozen > 0 {
		if err := gdb.Create(&model.Transaction{
			UserID: u.ID, Type: model.TxnFreeze, Amount: -frozen, BalanceAfter: 10000 - frozen,
			FrozenAfter: frozen, RefType: model.RefTypeUsage, RefID: strconv.FormatUint(usage.ID, 10),
		}).Error; err != nil {
			t.Fatalf("建冻结流水失败: %v", err)
		}
	}
	// BindTask 的等价写入：锚点在 usages.task_id 上，调用方（用例）决定指向哪个任务。
	return u, usage
}

func bindUsageTask(t *testing.T, gdb *gorm.DB, usageID, taskID uint64) {
	t.Helper()
	if err := gdb.Model(&model.Usage{}).Where("id = ?", usageID).Update("task_id", taskID).Error; err != nil {
		t.Fatalf("绑定任务失败: %v", err)
	}
}

func readUsage(t *testing.T, gdb *gorm.DB, id uint64) model.Usage {
	t.Helper()
	var u model.Usage
	if err := gdb.First(&u, id).Error; err != nil {
		t.Fatalf("读 usage 失败: %v", err)
	}
	return u
}

func readUser(t *testing.T, gdb *gorm.DB, id uint64) model.User {
	t.Helper()
	var u model.User
	if err := gdb.First(&u, id).Error; err != nil {
		t.Fatalf("读用户失败: %v", err)
	}
	return u
}

// 被重试取代的旧会话：任务当前指向新会话 → 旧冻结只能退，绝不能补收。
//
// 这是历史缺陷的正面回归：旧实现按 tasks.usage_id 关联，旧会话 join 不到任务，
// 落进"孤儿"分支按冻结额再收一次，用户为一次失败付两次钱。
func TestReclaimRetrySupersededOnlyRefunds(t *testing.T) {
	gdb := workerTestDB(t)
	u, oldUsage := seedReclaim(t, gdb, 1000, 500)

	tk := model.Task{UserID: u.ID, UsageID: 999999, Model: "seedream", Status: task.StatusQueued, Kind: task.KindVideo}
	if err := gdb.Create(&tk).Error; err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	// 新会话（重试产生）：同属该任务，且任务当前指向它。
	_, newUsage := seedReclaim(t, gdb, 1000, 500)
	if err := gdb.Model(&model.Task{}).Where("id = ?", tk.ID).Update("usage_id", newUsage.ID).Error; err != nil {
		t.Fatalf("改写任务会话失败: %v", err)
	}
	bindUsageTask(t, gdb, oldUsage.ID, tk.ID)
	bindUsageTask(t, gdb, newUsage.ID, tk.ID)

	if err := NewUsageReclaimJob().Run(context.Background()); err != nil {
		t.Fatalf("回收失败: %v", err)
	}

	got := readUsage(t, gdb, oldUsage.ID)
	if got.Status != model.UsageStatusReleased || got.Charge != 0 {
		t.Fatalf("旧会话应全额退款：status=%s charge=%d", got.Status, got.Charge)
	}
	if !strings.Contains(got.Anomaly, model.AnomalyReclaimedRetrySuperseded) {
		t.Fatalf("缺少 retry_superseded 标记：%q", got.Anomaly)
	}
	if u2 := readUser(t, gdb, u.ID); u2.Frozen != 0 || u2.Balance != 10000 {
		t.Fatalf("冻结未退回：balance=%d frozen=%d", u2.Balance, u2.Frozen)
	}

	// 新会话仍在 grace 期内，必须原样不动。
	if n := readUsage(t, gdb, newUsage.ID); n.Status != model.UsageStatusPending {
		t.Fatalf("新会话不该被处理：status=%s", n.Status)
	}
}

// 在途任务不占批次：SQL 里被过滤掉，不会挤掉真正的孤儿。
func TestReclaimSkipsActiveInFlight(t *testing.T) {
	gdb := workerTestDB(t)
	u, usage := seedReclaim(t, gdb, 1000, 100)

	tk := model.Task{UserID: u.ID, UsageID: usage.ID, Model: "seedream", Status: task.StatusRunning, Kind: task.KindVideo}
	if err := gdb.Create(&tk).Error; err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	bindUsageTask(t, gdb, usage.ID, tk.ID)

	if err := NewUsageReclaimJob().Run(context.Background()); err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	if got := readUsage(t, gdb, usage.ID); got.Status != model.UsageStatusPending {
		t.Fatalf("在途会话必须保持 pending：%s", got.Status)
	}
}

// 已交付但未结算：按冻结额补结（不是退款）。
func TestReclaimDeliveredChargesFrozen(t *testing.T) {
	gdb := workerTestDB(t)
	u, usage := seedReclaim(t, gdb, 1000, 100)

	tk := model.Task{
		UserID: u.ID, UsageID: usage.ID, Model: "seedream", Status: task.StatusSucceeded,
		Kind: task.KindVideo, ArtifactCount: 2,
	}
	if err := gdb.Create(&tk).Error; err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	bindUsageTask(t, gdb, usage.ID, tk.ID)

	if err := NewUsageReclaimJob().Run(context.Background()); err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	got := readUsage(t, gdb, usage.ID)
	if got.Status != model.UsageStatusSettled || got.Charge != 1000 {
		t.Fatalf("应按冻结额补结：status=%s charge=%d", got.Status, got.Charge)
	}
	if !strings.Contains(got.Anomaly, model.AnomalyReclaimedAfterSuccess) {
		t.Fatalf("缺少 after_success 标记：%q", got.Anomaly)
	}
	if u2 := readUser(t, gdb, u.ID); u2.Frozen != 0 {
		t.Fatalf("冻结未释放：frozen=%d", u2.Frozen)
	}
}

// 无任务且无上游证据：**默认**就全额退（上游一定没被调用，按冻结额收是纯多收）。
func TestReclaimNoEvidenceRefundsByDefault(t *testing.T) {
	t.Setenv("LD_RECLAIM_NO_EVIDENCE", "")
	gdb := workerTestDB(t)
	u, usage := seedReclaim(t, gdb, 1000, 100)

	if err := NewUsageReclaimJob().Run(context.Background()); err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	got := readUsage(t, gdb, usage.ID)
	if got.Status != model.UsageStatusReleased || got.Charge != 0 {
		t.Fatalf("无证据孤账应全额退款：status=%s charge=%d", got.Status, got.Charge)
	}
	if !strings.Contains(got.Anomaly, model.AnomalyReclaimedNoEvidence) {
		t.Fatalf("缺少 no_evidence 标记：%q", got.Anomaly)
	}
	if u2 := readUser(t, gdb, u.ID); u2.Balance != 10000 || u2.Frozen != 0 {
		t.Fatalf("退款金额不符：balance=%d frozen=%d", u2.Balance, u2.Frozen)
	}
}

// 无任务且无上游证据：显式置 charge 时退回历史口径（观察期 / 对账口径不一致时用）。
func TestReclaimNoEvidenceChargesWhenOptedOut(t *testing.T) {
	t.Setenv("LD_RECLAIM_NO_EVIDENCE", "charge")
	gdb := workerTestDB(t)
	u, usage := seedReclaim(t, gdb, 1000, 100)

	if err := NewUsageReclaimJob().Run(context.Background()); err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	got := readUsage(t, gdb, usage.ID)
	if got.Status != model.UsageStatusSettled || got.Charge != 1000 {
		t.Fatalf("退出口径应按冻结额补结：status=%s charge=%d", got.Status, got.Charge)
	}
	if u2 := readUser(t, gdb, u.ID); u2.Frozen != 0 {
		t.Fatalf("冻结未释放：frozen=%d", u2.Frozen)
	}
}

// 回收不得把估算值写进实测 token 列（stats 的 usage_daily 直接 SUM 它们）。
func TestReclaimDoesNotWriteMeasuredTokens(t *testing.T) {
	gdb := workerTestDB(t)
	u, usage := seedReclaim(t, gdb, 1000, 777)

	tk := model.Task{
		UserID: u.ID, UsageID: usage.ID, Model: "seedream", Status: task.StatusSucceeded,
		Kind: task.KindVideo, ArtifactCount: 1,
	}
	if err := gdb.Create(&tk).Error; err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	bindUsageTask(t, gdb, usage.ID, tk.ID)

	if err := NewUsageReclaimJob().Run(context.Background()); err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	got := readUsage(t, gdb, usage.ID)
	if got.PromptTokens != 0 || got.CompletionTokens != 0 {
		t.Fatalf("实测列被估算值污染：prompt=%d completion=%d", got.PromptTokens, got.CompletionTokens)
	}
}
