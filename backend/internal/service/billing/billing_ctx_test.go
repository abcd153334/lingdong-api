package billing

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"lingdong-api/internal/model"
)

// seedSession 造一条冻结态的计费会话语料：用户余额 9000、冻结 1000、KEY 不限额。
//
// 快照用「$1 / 1M tokens」：这样「tokens 数 = 微数」，断言里能一眼看出扣了多少。
func seedSession(t *testing.T, ctx context.Context) (*Service, *Session) {
	t.Helper()
	gdb := withTestDB(t)
	u := model.User{Username: "ctx-user", Balance: 9000, Frozen: 1000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	k := model.APIKey{UserID: u.ID, KeyHash: "hash-ctx", Status: 1, Used: 1000}
	if err := gdb.Create(&k).Error; err != nil {
		t.Fatalf("建 KEY 失败: %v", err)
	}
	snap := Snapshot{
		ModelID: "gpt-4o", BillingMode: model.BillingModeTokens,
		Price: model.Prices{Input: iptr(1_000_000), Output: iptr(1_000_000)},
	}
	raw, _ := json.Marshal(snap)
	usage := model.Usage{UserID: u.ID, KeyID: k.ID, Model: "gpt-4o", Status: "pending", PricingSnapshot: raw}
	if err := gdb.Create(&usage).Error; err != nil {
		t.Fatalf("建 usage 失败: %v", err)
	}
	if err := gdb.Create(&model.Transaction{
		UserID: u.ID, Type: "freeze", Amount: -1000, BalanceAfter: 9000, FrozenAfter: 1000,
		RefType: "usage", RefID: "1",
	}).Error; err != nil {
		t.Fatalf("建冻结流水失败: %v", err)
	}
	return New(gdb), &Session{UsageID: usage.ID, UserID: u.ID, KeyID: k.ID, Frozen: 1000}
}

// 客户端断开后请求 ctx 已取消，结算仍必须落库。
func TestSettleIgnoresCanceledContext(t *testing.T) {
	base := context.Background()
	svc, sess := seedSession(t, base)

	ctx, cancel := context.WithCancel(base)
	cancel() // 模拟客户端断开：ctx 已取消

	if _, err := svc.Settle(ctx, sess, SettleInput{
		StatusCode: 200, Usage: Usage{PromptTokens: 100}, UsageSource: "upstream",
	}); err != nil {
		t.Fatalf("取消上下文不应阻断结算: %v", err)
	}

	var u model.User
	if err := svc.db.First(&u, sess.UserID).Error; err != nil {
		t.Fatalf("读用户失败: %v", err)
	}
	// 冻结 1000、实扣 100 → 余额 9000+900=9900，冻结归零
	if u.Balance != 9900 || u.Frozen != 0 {
		t.Fatalf("余额/冻结不符：balance=%d frozen=%d", u.Balance, u.Frozen)
	}
	var usage model.Usage
	if err := svc.db.First(&usage, sess.UsageID).Error; err != nil {
		t.Fatalf("读 usage 失败: %v", err)
	}
	if usage.Status != "settled" || usage.UsageSource != "upstream" || usage.Charge != 100 {
		t.Fatalf("usage 终态不符：%+v", usage)
	}
}

// 上游中文长错误体释放时不得因截断产生非法 UTF-8 而回滚。
func TestReleaseChineseErrorMsg(t *testing.T) {
	base := context.Background()
	svc, sess := seedSession(t, base)

	longMsg := strings.Repeat("上游返回错误", 200) // 1200 字符
	if err := svc.Release(base, sess, SettleInput{StatusCode: 500, ErrorMsg: longMsg}); err != nil {
		t.Fatalf("释放失败: %v", err)
	}
	var usage model.Usage
	if err := svc.db.First(&usage, sess.UsageID).Error; err != nil {
		t.Fatalf("读 usage 失败: %v", err)
	}
	if usage.Status != "released" || usage.Charge != 0 {
		t.Fatalf("usage 终态不符：%+v", usage)
	}
	if !utf8.ValidString(usage.ErrorMsg) {
		t.Fatal("error_msg 非法 UTF-8")
	}
	if n := utf8.RuneCountInString(usage.ErrorMsg); n != 500 {
		t.Fatalf("error_msg 字符数=%d，期望 500", n)
	}
}

// 重复结算第二次返回 ErrSessionClosed（幂等契约，调用方视为已完成）。
func TestSettleTwiceReturnsClosed(t *testing.T) {
	base := context.Background()
	svc, sess := seedSession(t, base)
	in := SettleInput{StatusCode: 200, Usage: Usage{PromptTokens: 100}}
	if _, err := svc.Settle(base, sess, in); err != nil {
		t.Fatalf("首次结算失败: %v", err)
	}
	if _, err := svc.Settle(base, sess, in); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("重复结算应返回 ErrSessionClosed，实际 %v", err)
	}
}

// seedPending 造一条可回收的 pending 会话：指定预扣额、估算 prompt 与快照。
func seedPending(t *testing.T, ctx context.Context, frozen, estPrompt int64, snap Snapshot) (*Service, uint64) {
	t.Helper()
	gdb := withTestDB(t)
	u := model.User{Username: "reclaim-user", Balance: 10000 - frozen, Frozen: frozen}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	k := model.APIKey{UserID: u.ID, KeyHash: "hash-reclaim", Status: 1, Used: frozen}
	if err := gdb.Create(&k).Error; err != nil {
		t.Fatalf("建 KEY 失败: %v", err)
	}
	raw, _ := json.Marshal(snap)
	usage := model.Usage{
		UserID: u.ID, KeyID: k.ID, Model: "gpt-4o", Status: "pending",
		PricingSnapshot: raw, EstPromptTokens: estPrompt,
	}
	if err := gdb.Create(&usage).Error; err != nil {
		t.Fatalf("建 usage 失败: %v", err)
	}
	if frozen > 0 {
		if err := gdb.Create(&model.Transaction{
			UserID: u.ID, Type: "freeze", Amount: -frozen, BalanceAfter: 10000 - frozen,
			FrozenAfter: frozen, RefType: "usage", RefID: strconv.FormatUint(usage.ID, 10),
		}).Error; err != nil {
			t.Fatalf("建冻结流水失败: %v", err)
		}
	}
	return New(gdb), usage.ID
}

// tokenSnap 是「$1 / 1M tokens」快照（tokens = 微）。
func tokenSnap() Snapshot {
	return Snapshot{
		ModelID: "gpt-4o", BillingMode: model.BillingModeTokens,
		Price: model.Prices{Input: iptr(1_000_000), Output: iptr(1_000_000)},
	}
}

// 按量模型回收：只用 prompt 估算计费，差额退还，终态 settled。
func TestReclaimSettleTokenModel(t *testing.T) {
	base := context.Background()
	svc, usageID := seedPending(t, base, 1000, 400, tokenSnap())

	charge, err := svc.ReclaimSettle(base, usageID, "", 0)
	if err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	if charge != 400 {
		t.Fatalf("charge=%d，期望 400", charge)
	}
	var u model.User
	if err := svc.db.First(&u, 1).Error; err != nil {
		t.Fatalf("读用户失败: %v", err)
	}
	if u.Balance != 9600 || u.Frozen != 0 {
		t.Fatalf("余额/冻结不符：balance=%d frozen=%d", u.Balance, u.Frozen)
	}
	var usage model.Usage
	if err := svc.db.First(&usage, usageID).Error; err != nil {
		t.Fatalf("读 usage 失败: %v", err)
	}
	if usage.Status != "settled" || usage.Charge != 400 || usage.UsageSource != "estimated" {
		t.Fatalf("usage 终态不符：%+v", usage)
	}
	if !strings.Contains(usage.Anomaly, "reclaimed") {
		t.Fatalf("缺少 reclaimed 标记：%q", usage.Anomaly)
	}
	var release model.Transaction
	if err := svc.db.Where("ref_type='usage' AND ref_id=? AND type='release'", strconv.FormatUint(usageID, 10)).
		First(&release).Error; err != nil {
		t.Fatalf("缺少差额释放流水: %v", err)
	}
	if release.Amount != 600 || release.BalanceAfter != 9600 {
		t.Fatalf("释放流水不符：%+v", release)
	}
	// 重复执行必须幂等
	if _, err := svc.ReclaimSettle(base, usageID, "", 0); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("重复回收应返回 ErrSessionClosed，实际 %v", err)
	}
}

// 按规格模型回收：预扣即精确金额，无差额流水。
func TestReclaimSettleSpecModel(t *testing.T) {
	base := context.Background()
	snap := Snapshot{
		ModelID: "seedream", BillingMode: model.BillingModeSpec,
		SpecPrice: model.SpecPrices{
			Kind:      SpecKindPerImage,
			TierPrice: map[string]int64{"1K": 50_000},
		},
	}
	svc, usageID := seedPending(t, base, 50_000, 0, snap)

	charge, err := svc.ReclaimSettle(base, usageID, "", 0)
	if err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	if charge != 50_000 {
		t.Fatalf("charge=%d，期望 50000（预扣即精确）", charge)
	}
	var n int64
	if err := svc.db.Model(&model.Transaction{}).
		Where("ref_type='usage' AND ref_id=? AND type='release'", strconv.FormatUint(usageID, 10)).
		Count(&n).Error; err != nil {
		t.Fatalf("查流水失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("按规格回收不应产生差额流水，实际 %d 条", n)
	}
}

// 估算金额超过冻结额时按冻结额封顶（无上游证据不制造新欠费）。
func TestReclaimNeverExceedsFrozen(t *testing.T) {
	base := context.Background()
	svc, usageID := seedPending(t, base, 100, 5000, tokenSnap())

	charge, err := svc.ReclaimSettle(base, usageID, "", 0)
	if err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	if charge != 100 {
		t.Fatalf("charge=%d，期望封顶 100", charge)
	}
}
