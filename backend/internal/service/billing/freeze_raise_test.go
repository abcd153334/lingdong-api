package billing

// 托管密钥顶额自动提额：仅 hosted、硬顶=用户余额、自建绝不提额、免费分支同样生效。

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/model"
)

// freezeHostedCase 造一个用户 + 一把指定 kind/限额的 KEY，返回 svc 与二者 id。
func freezeHostedCase(t *testing.T, kind string, balance, amountLimit, used int64) (*Service, uint64, uint64) {
	t.Helper()
	gdb := withTestDB(t)
	suffix := time.Now().UnixNano() % 1_000_000_000_000
	u := &model.User{Username: "raise-" + kind, PasswordHash: "x", Balance: balance, Group: "default", AffCode: "rz" + kind + strconv.FormatInt(suffix, 36)}
	if err := gdb.Create(u).Error; err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	k := &model.APIKey{UserID: u.ID, Kind: kind, KeyCipher: "x", KeyHash: "raise-" + kind + "-" + strconv.FormatInt(suffix, 36),
		Name: "k", Status: 1, Group: "default", AmountLimit: amountLimit, Used: used}
	if err := gdb.Create(k).Error; err != nil {
		t.Fatalf("造密钥失败: %v", err)
	}
	return New(gdb), u.ID, k.ID
}

func hostedKeyAmountLimit(t *testing.T, svc *Service, keyID uint64) int64 {
	t.Helper()
	var k model.APIKey
	if err := svc.db.First(&k, keyID).Error; err != nil {
		t.Fatal(err)
	}
	return k.AmountLimit
}

// TestFreeze_HostedAutoRaise：顶额且余额足 → 自动提额到 max(used+amount, floor) 并放行。
func TestFreeze_HostedAutoRaise(t *testing.T) {
	// floor=100 元=1e8 微；余额远大于 floor → newLimit 恰为 floor。
	svc, uid, kid := freezeHostedCase(t, model.KeyKindHosted, 500_000_000, 1_000, 0)
	_, err := svc.Freeze(context.Background(), FreezeInput{
		UserID: uid, KeyID: kid, Model: "vid", Modality: "video", Group: "default",
		Snapshot: callSnap(), Amount: 2_000,
	})
	if err != nil {
		t.Fatalf("托管顶额应自动提额放行，实际 %v", err)
	}
	if got := hostedKeyAmountLimit(t, svc, kid); got != 100_000_000 {
		t.Fatalf("amount_limit 应提至 max(used+amount, floor)=1e8，实际 %d", got)
	}
}

// TestFreeze_HostedRaiseAtBalanceCap：提额目标超过 max(余额, floor) → BalanceCap、限额不变。
// used 已达 5e8，余额 1.5e8：need=5e8+100 > cap=max(1.5e8,1e8)=1.5e8。
func TestFreeze_HostedRaiseAtBalanceCap(t *testing.T) {
	svc, uid, kid := freezeHostedCase(t, model.KeyKindHosted, 150_000_000, 100_000_000, 500_000_000)
	_, err := svc.Freeze(context.Background(), FreezeInput{
		UserID: uid, KeyID: kid, Model: "vid", Modality: "video", Group: "default",
		Snapshot: callSnap(), Amount: 100,
	})
	if !errors.Is(err, ErrKeyLimitBalanceCap) {
		t.Fatalf("应返回 ErrKeyLimitBalanceCap，实际 %v", err)
	}
	if got := hostedKeyAmountLimit(t, svc, kid); got != 100_000_000 {
		t.Fatalf("拒绝路径限额不得变化，实际 %d", got)
	}
}

// TestFreeze_UserKeyNeverAutoRaise：自建 KEY 顶额一律拒绝，绝不自动提额。
func TestFreeze_UserKeyNeverAutoRaise(t *testing.T) {
	svc, uid, kid := freezeHostedCase(t, model.KeyKindUser, 500_000_000, 1_000, 0)
	_, err := svc.Freeze(context.Background(), FreezeInput{
		UserID: uid, KeyID: kid, Model: "vid", Modality: "video", Group: "default",
		Snapshot: callSnap(), Amount: 2_000,
	})
	if !errors.Is(err, ErrInsufficientQuota) {
		t.Fatalf("自建顶额应 ErrInsufficientQuota，实际 %v", err)
	}
	if got := hostedKeyAmountLimit(t, svc, kid); got != 1_000 {
		t.Fatalf("自建限额不得被改，实际 %d", got)
	}
}

// TestFreeze_FreeModelHostedRaise：0 元请求也要过限额判定，hosted 同样自动提额。
func TestFreeze_FreeModelHostedRaise(t *testing.T) {
	svc, uid, kid := freezeHostedCase(t, model.KeyKindHosted, 500_000_000, 1_000, 2_000)
	sess, err := svc.Freeze(context.Background(), FreezeInput{
		UserID: uid, KeyID: kid, Model: "vid", Modality: "video", Group: "default",
		Snapshot: callSnap(), Amount: 0,
	})
	if err != nil {
		t.Fatalf("免费模型 hosted 顶额应自动提额放行，实际 %v", err)
	}
	if got := hostedKeyAmountLimit(t, svc, kid); got != 100_000_000 {
		t.Fatalf("0 元分支也应提额，实际 %d", got)
	}
	if sess.Frozen != 0 {
		t.Fatalf("0 元冻结额应为 0，实际 %d", sess.Frozen)
	}
}

// TestFreeze_AutoRaiseSwitchOff：allow_auto_raise=false 时热关闭，回退顶额即拒。
func TestFreeze_AutoRaiseSwitchOff(t *testing.T) {
	svc, uid, kid := freezeHostedCase(t, model.KeyKindHosted, 500_000_000, 1_000, 0)
	restore := setAllowAutoRaise(false)
	defer restore()

	_, err := svc.Freeze(context.Background(), FreezeInput{
		UserID: uid, KeyID: kid, Model: "vid", Modality: "video", Group: "default",
		Snapshot: callSnap(), Amount: 2_000,
	})
	if !errors.Is(err, ErrInsufficientQuota) {
		t.Fatalf("灰度关闭后应回退 ErrInsufficientQuota，实际 %v", err)
	}
	if got := hostedKeyAmountLimit(t, svc, kid); got != 1_000 {
		t.Fatalf("灰度关闭后限额不得被改，实际 %d", got)
	}
}

// setAllowAutoRaise 直接改进程内配置（与线上同一真相源 conf.System，热更新语义一致）。
func setAllowAutoRaise(v bool) func() {
	raw, _ := json.Marshal(map[string]any{"allow_auto_raise": v})
	conf.System().Set("hosted_key", raw)
	return func() { conf.System().Set("hosted_key", nil) }
}
