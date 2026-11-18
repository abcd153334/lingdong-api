package billing

import (
	"context"
	"encoding/json"
	"testing"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
)

/*
本文件锁死一条铁律：**结算期算不出金额，也必须落账**。

回归的是三个真实卡死路径（旧实现都是 `return err`）：
  - Settle：快照损坏 / 价目缺失 → usages 停在 pending → 同步请求冻结永不归还；
  - ReclaimSettle：同一个必败运算被回收 worker 每 5 分钟重试一次，冻结永久滞留；
  - LateSettle：迟到补收永远不落账（钱少收了，而且没人知道）。

统一口径：公式类错误 → 按**预扣冻结额**入账 + anomaly `charge_calc_failed`；
只有 DB/事务错误才允许上抛（本文件最后一条用例守这个边界）。
*/

// TestSettleFallsBackToFrozenOnBrokenSnapshot 快照 JSON 损坏时按冻结额入账。
func TestSettleFallsBackToFrozenOnBrokenSnapshot(t *testing.T) {
	ResetLimitsForTest()
	gdb := withTestDB(t)
	ctx := context.Background()

	svc := New(gdb)
	sess := seedFrozenSession(t, gdb, "broken-snap", 300)

	// 把快照写成「合法 JSON、形状不对」（jsonb 列不允许语法错误，所以真实可达的
	// 损坏形态是这种：历史结构变更、手工改库、把快照塞成了数组）。
	if err := gdb.Model(&model.Usage{}).Where("id = ?", sess.UsageID).
		Update("pricing_snapshot", []byte(`[1,2,3]`)).Error; err != nil {
		t.Fatalf("写坏快照失败: %v", err)
	}

	res, err := svc.Settle(ctx, sess, SettleInput{StatusCode: 200})
	if err != nil {
		t.Fatalf("快照损坏不应让结算报错（否则 usage 永远 pending、冻结不归还）: %v", err)
	}
	if res.Charge != 300 {
		t.Fatalf("应回落到冻结额 300，实际 %d", res.Charge)
	}
	assertSettledWithAnomaly(t, gdb, sess, 300, model.AnomalyChargeCalcFailed)
}

// TestSettleFallsBackToFrozenOnMissingPrice 价目缺失（nil 单价）时同样回落。
//
// 与上一条的区别：快照本身是合法 JSON，失败发生在 `ChargeDetailed` 里。
// 两条路径都必须被兜住，只修一条等于没修。
func TestSettleFallsBackToFrozenOnMissingPrice(t *testing.T) {
	ResetLimitsForTest()
	gdb := withTestDB(t)
	ctx := context.Background()

	svc := New(gdb)
	sess := seedFrozenSession(t, gdb, "no-price", 700)

	// billing_mode=tokens 但 input 单价为 nil → ChargeDetailed 返回 ErrPriceMissing。
	noPrice := Snapshot{ModelID: "vid", BillingMode: model.BillingModeTokens, Price: model.Prices{}}
	raw, _ := json.Marshal(noPrice)
	if err := gdb.Model(&model.Usage{}).Where("id = ?", sess.UsageID).
		Update("pricing_snapshot", raw).Error; err != nil {
		t.Fatalf("写快照失败: %v", err)
	}

	if _, err := svc.Settle(ctx, sess, SettleInput{StatusCode: 200}); err != nil {
		t.Fatalf("价目缺失不应让结算报错: %v", err)
	}
	assertSettledWithAnomaly(t, gdb, sess, 700, model.AnomalyChargeCalcFailed)
}

// TestReclaimSettleFallsBackOnBrokenSnapshot 回收路径必须自己落账，不能靠 worker 重试。
func TestReclaimSettleFallsBackOnBrokenSnapshot(t *testing.T) {
	ResetLimitsForTest()
	gdb := withTestDB(t)
	ctx := context.Background()

	svc := New(gdb)
	sess := seedFrozenSession(t, gdb, "reclaim-broken", 400)
	if err := gdb.Model(&model.Usage{}).Where("id = ?", sess.UsageID).
		Update("pricing_snapshot", []byte(`"oops"`)).Error; err != nil {
		t.Fatalf("写坏快照失败: %v", err)
	}

	charge, err := svc.ReclaimSettle(ctx, sess.UsageID, "", 0)
	if err != nil {
		t.Fatalf("回收结算不应报错（否则 worker 每 5 分钟重试同一个必败运算）: %v", err)
	}
	if charge != 400 {
		t.Fatalf("回收应按冻结额 400 入账，实际 %d", charge)
	}
	assertSettledWithAnomaly(t, gdb, sess, 400, model.AnomalyChargeCalcFailed)
}

// TestLateSettleFallsBackOnBrokenSnapshot 迟到补收同样要落账（此时冻结已退给用户）。
func TestLateSettleFallsBackOnBrokenSnapshot(t *testing.T) {
	ResetLimitsForTest()
	gdb := withTestDB(t)
	ctx := context.Background()

	svc := New(gdb)
	sess := seedFrozenSession(t, gdb, "late-broken", 500)

	// 先正常释放（模拟任务超时）：冻结退回、状态 released。
	if err := svc.Release(ctx, sess, SettleInput{StatusCode: 504, ErrorMsg: "timeout"}); err != nil {
		t.Fatalf("释放失败: %v", err)
	}
	if err := gdb.Model(&model.Usage{}).Where("id = ?", sess.UsageID).
		Update("pricing_snapshot", []byte(`"oops"`)).Error; err != nil {
		t.Fatalf("写坏快照失败: %v", err)
	}

	charge, err := svc.LateSettle(ctx, sess.UsageID, Usage{PromptTokens: 10, CompletionTokens: 10})
	if err != nil {
		t.Fatalf("迟到结算不应报错: %v", err)
	}
	// 冻结已退回，补收 = 冻结额（把预扣额重新扣回来），且不超过冻结额。
	if charge != 500 {
		t.Fatalf("迟到补收应按冻结额 500，实际 %d", charge)
	}
	assertSettledWithAnomaly(t, gdb, sess, 500, model.AnomalyChargeCalcFailed)
}

// TestSettleStillFailsOnDBError 兜底只吞「公式错误」，DB 错误必须照常上抛。
//
// 反例（本测试防的）：为了不卡单把 `return err` 全换成兜底，于是「用户行不存在」
// 这种真错误也被当成「按冻结额入账」成功，钱从账上消失而没人知道。
func TestSettleStillFailsOnDBError(t *testing.T) {
	ResetLimitsForTest()
	gdb := withTestDB(t)
	ctx := context.Background()

	svc := New(gdb)
	sess := seedFrozenSession(t, gdb, "db-error", 200)

	// 删掉用户行：结算事务里 `SELECT users ... FOR UPDATE` 会 record not found。
	if err := gdb.Exec("DELETE FROM users WHERE id = ?", sess.UserID).Error; err != nil {
		t.Fatalf("删除用户失败: %v", err)
	}

	if _, err := svc.Settle(ctx, sess, SettleInput{StatusCode: 200}); err == nil {
		t.Fatal("用户行缺失属于 DB 错误，结算必须返回 err（不能被兜底吞掉）")
	}
	var usage model.Usage
	if err := gdb.First(&usage, sess.UsageID).Error; err != nil {
		t.Fatal(err)
	}
	if usage.Status != model.UsageStatusPending {
		t.Fatalf("事务应已回滚、usage 仍为 pending，实际 %s", usage.Status)
	}
}

// seedFrozenSession 造「用户 + KEY + 已冻结会话」。
//
// 余额给足（10 倍冻结额），避免用例被「余额下限」二次降额干扰 —— 本文件测的是
// 「金额从哪来」，不是「扣不扣得动」。
func seedFrozenSession(t *testing.T, gdb *gorm.DB, name string, amount int64) *Session {
	t.Helper()
	u := &model.User{
		Username: name, PasswordHash: "x", Group: "default",
		AffCode: "af" + name, Balance: amount * 10,
	}
	if err := gdb.Create(u).Error; err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	k := &model.APIKey{
		UserID: u.ID, KeyCipher: "x", KeyHash: "hk-" + name, Name: "k", Status: 1, Group: "default",
	}
	if err := gdb.Create(k).Error; err != nil {
		t.Fatalf("造密钥失败: %v", err)
	}
	sess, err := New(gdb).Freeze(context.Background(), FreezeInput{
		UserID: u.ID, KeyID: k.ID, Model: "vid", Modality: "video", Group: "default",
		Snapshot: callSnap(), Amount: amount,
	})
	if err != nil {
		t.Fatalf("Freeze 失败: %v", err)
	}
	return sess
}

// assertSettledWithAnomaly 断言 usage 已终态、charge/冻结符合预期、带指定 anomaly。
func assertSettledWithAnomaly(t *testing.T, gdb *gorm.DB, sess *Session, wantCharge int64, anomaly string) {
	t.Helper()
	var usage model.Usage
	if err := gdb.First(&usage, sess.UsageID).Error; err != nil {
		t.Fatal(err)
	}
	if usage.Status != model.UsageStatusSettled {
		t.Fatalf("状态应为 settled，实际 %s", usage.Status)
	}
	if usage.Charge != wantCharge {
		t.Fatalf("charge 应为 %d，实际 %d", wantCharge, usage.Charge)
	}
	if !model.HasAnomaly(usage.Anomaly, anomaly) {
		t.Fatalf("anomaly 应含 %s，实际 %q", anomaly, usage.Anomaly)
	}
	var u model.User
	if err := gdb.First(&u, sess.UserID).Error; err != nil {
		t.Fatal(err)
	}
	if u.Frozen != 0 {
		t.Fatalf("冻结额应归零，实际 %d", u.Frozen)
	}
}
