package billing

import (
	"strings"
	"testing"

	"lingdong-api/internal/model"
)

/*
本文件锁死「音乐/歌词按实际交付条数计费」（per_clip）。

回归的真实资损路径：一次任务出 1~2 首，旧口径 per_task 收同一个价 ——
上游只交付 1 首时那一半无法解释；反过来 0 首交付（上游明确回成功但没有导出链接）
也照收，用户拿不到任何东西却被扣了钱。这两条都由 per_clip + BillableClips 收口。

判据三件：
 1. 0 条 = 0 元（绝不回落到 1 条，那是 per_image 的语义）；
 2. n 条 = 单价 × n，上限由 max_n 夹住；
 3. 冻结期按 max_n 占额度，结算期按实际交付——两者都不能超过 max_n。
*/

func perClipPrices() model.SpecPrices {
	price := int64(500_000)
	maxN := int64(2)
	return model.SpecPrices{Kind: SpecKindPerClip, PerClipPrice: &price, MaxN: &maxN}
}

// TestValidateSpecPricesPerClip 保存期校验：合法配置给提示、缺价/越界/混配一律拒绝。
func TestValidateSpecPricesPerClip(t *testing.T) {
	price := int64(500_000)
	zero := int64(0)
	neg := int64(-1)
	one := int64(1)
	legacy := int64(999_994)

	t.Run("合法配置只给提示", func(t *testing.T) {
		warns, err := ValidateSpecPrices("music", perClipPrices())
		if err != nil {
			t.Fatalf("per_clip 合法配置不应报错: %v", err)
		}
		if len(warns) == 0 {
			t.Fatal("按条计费必须给一条「结算按实际条数」的提示：运营需要知道账单会随交付条数变化")
		}
	})

	t.Run("缺单价", func(t *testing.T) {
		if _, err := ValidateSpecPrices("music", model.SpecPrices{Kind: SpecKindPerClip}); err == nil {
			t.Fatal("没有 per_clip_price 必须拒绝：缺价会让结算报未配价，模型看起来在售却调不通")
		}
	})

	t.Run("负价", func(t *testing.T) {
		_, err := ValidateSpecPrices("music", model.SpecPrices{Kind: SpecKindPerClip, PerClipPrice: &neg})
		if err == nil {
			t.Fatal("负单价必须拒绝")
		}
	})

	t.Run("0 是合法免费", func(t *testing.T) {
		if _, err := ValidateSpecPrices("music", model.SpecPrices{Kind: SpecKindPerClip, PerClipPrice: &zero}); err != nil {
			t.Fatalf("0 元/条是合法配置（免费模型），不应拒绝: %v", err)
		}
	})

	t.Run("条数上限越界", func(t *testing.T) {
		for _, n := range []int64{0, 9} {
			v := n
			if _, err := ValidateSpecPrices("music", model.SpecPrices{
				Kind: SpecKindPerClip, PerClipPrice: &price, MaxN: &v,
			}); err == nil {
				t.Fatalf("max_n=%d 必须拒绝（合法区间 1–%d）", n, MaxMusicClips)
			}
		}
		if _, err := ValidateSpecPrices("music", model.SpecPrices{
			Kind: SpecKindPerClip, PerClipPrice: &price, MaxN: &one,
		}); err != nil {
			t.Fatalf("max_n=1 是歌词模型的正常配置: %v", err)
		}
	})

	t.Run("混配其它口径", func(t *testing.T) {
		sp := perClipPrices()
		sp.TierPrice = map[string]int64{"2K": price}
		if _, err := ValidateSpecPrices("music", sp); err == nil {
			t.Fatal("per_clip 与 tier_price 混配必须拒绝：否则运营以为档位生效，实际被忽略")
		}
		sp = perClipPrices()
		sp.PerTaskPrice = &legacy
		if _, err := ValidateSpecPrices("music", sp); err == nil {
			t.Fatal("per_clip 与 per_task_price 混配必须拒绝：两种口径同时非空时没人说得清哪个算数")
		}
	})

	t.Run("per_task 存量口径仍可保存但给警告", func(t *testing.T) {
		warns, err := ValidateSpecPrices("music", model.SpecPrices{Kind: SpecKindPerTask, PerTaskPrice: &legacy})
		if err != nil {
			t.Fatalf("per_task 是存量口径，不能拒绝（否则已停用的旧模型无法再保存）: %v", err)
		}
		if len(warns) == 0 || !strings.Contains(warns[0], "per_clip") {
			t.Fatalf("per_task 应提示改用 per_clip，实际 %v", warns)
		}
	})
}

// TestEffectiveKindForMusic music 同时接受 per_clip（新）与 per_task（存量）。
func TestEffectiveKindForMusic(t *testing.T) {
	if k, conflict := EffectiveKindFor(perClipPrices(), "music"); k != SpecKindPerClip || conflict {
		t.Fatalf("per_clip 是 music 的首选口径，不应有 conflict，实际 %s %v", k, conflict)
	}
	legacy := int64(999_994)
	if k, conflict := EffectiveKindFor(model.SpecPrices{
		Kind: SpecKindPerTask, PerTaskPrice: &legacy,
	}, "music"); k != SpecKindPerTask || conflict {
		t.Fatalf("存量 per_task 必须原样生效（不打 conflict），实际 %s %v", k, conflict)
	}
}

// TestPerClipChargeDelivered 结算口径：0 条免单、n 条按条收、超额夹到上限。
func TestPerClipChargeDelivered(t *testing.T) {
	sp := perClipPrices()
	cases := []struct {
		n        int64
		want     int64
		capped   bool
		strictOK bool
	}{
		{0, 0, false, true},         // 0 条 = 免单（不是按 1 条收）
		{1, 500_000, false, true},   // 只交付 1 首
		{2, 1_000_000, false, true}, // 两首都交付
		{3, 1_000_000, true, false}, // 上游多给：多给不收费，夹到 max_n 并留痕
	}
	for _, c := range cases {
		got, capped, err := perClipCharge(sp, Usage{ImageN: c.n}, false)
		if err != nil {
			t.Fatalf("n=%d 结算报错: %v", c.n, err)
		}
		if got != c.want || capped != c.capped {
			t.Fatalf("n=%d: got %d capped=%v，期望 %d capped=%v", c.n, got, capped, c.want, c.capped)
		}
		// 预扣期（strictLimits）对超限必须硬拒：否则冻结额与实际应收脱节。
		_, _, serr := perClipCharge(sp, Usage{ImageN: c.n}, true)
		if c.strictOK && serr != nil {
			t.Fatalf("n=%d 在预扣期不应报错: %v", c.n, serr)
		}
		if !c.strictOK && serr == nil {
			t.Fatalf("n=%d 在预扣期必须拒绝（超过 max_n）", c.n)
		}
	}
}

// TestPerClipChargeMissingPrice 缺价不是免费：必须报 ErrPriceMissing。
func TestPerClipChargeMissingPrice(t *testing.T) {
	if _, _, err := perClipCharge(model.SpecPrices{Kind: SpecKindPerClip}, Usage{ImageN: 1}, false); err == nil {
		t.Fatal("没有 per_clip_price 必须报未配价，不能静默收 0")
	}
}

// TestMusicClipFreezeCount 冻结条数：未配 max_n 按 2 条，配了尊重它并夹到引擎上限。
func TestMusicClipFreezeCount(t *testing.T) {
	if got := MusicClipFreezeCount(model.SpecPrices{Kind: SpecKindPerClip}); got != 2 {
		t.Fatalf("未配 max_n 时按 2 条冻结（SUNO 默认交付两首），实际 %d", got)
	}
	one := int64(1)
	if got := MusicClipFreezeCount(model.SpecPrices{Kind: SpecKindPerClip, MaxN: &one}); got != 1 {
		t.Fatalf("歌词模型 max_n=1，冻结应 1 条，实际 %d", got)
	}
	nine := int64(9)
	if got := MusicClipFreezeCount(model.SpecPrices{Kind: SpecKindPerClip, MaxN: &nine}); got != MaxMusicClips {
		t.Fatalf("超过引擎上限必须夹到 %d，实际 %d", MaxMusicClips, got)
	}
}

// TestBillableClipsZeroStaysZero 与 BillableImageN 的关键分歧。
//
// 这一条单独存在：两者的签名一样、目的相似，谁"顺手复用"其中一个，
// 就会让另一边错收（图片少收 / 音乐多收）。
func TestBillableClipsZeroStaysZero(t *testing.T) {
	if got := BillableClips(2, 0); got != 0 {
		t.Fatalf("音乐交付 0 条必须计 0 条（免单），实际 %d", got)
	}
	if got := BillableImageN(2, 0); got != 2 {
		t.Fatalf("图片的 0 是「没有交付证据」，必须保持意图 2（由全失败闸门处理），实际 %d", got)
	}
	if got := BillableClips(2, 1); got != 1 {
		t.Fatalf("交付 1 条应计 1，实际 %d", got)
	}
	if got := BillableClips(2, 3); got != 2 {
		t.Fatalf("多给不向用户收费，应夹到意图 2，实际 %d", got)
	}
	if got := BillableClips(0, 0); got != 0 {
		t.Fatalf("没有条数意图的历史行应原样返回 0，实际 %d", got)
	}
}

// TestChargeCeilingPerClip 结算上限 = 单价 × 计费条数上限。
func TestChargeCeilingPerClip(t *testing.T) {
	amount, reason := ChargeCeiling(Snapshot{BillingMode: model.BillingModeSpec, SpecPrice: perClipPrices()}, Usage{})
	if amount != 1_000_000 {
		t.Fatalf("上限应为 500000×2=1000000，实际 %d", amount)
	}
	if reason != "per_clip_max_n" {
		t.Fatalf("上限原因应为 per_clip_max_n，实际 %q", reason)
	}
}

// TestTierInfoPerClip 计费档位来源：账单要能回答「这一单按什么单价算的」。
func TestTierInfoPerClip(t *testing.T) {
	unit, source := TierInfo(Snapshot{BillingMode: model.BillingModeSpec, SpecPrice: perClipPrices()}, Usage{ImageN: 2})
	if unit != 500_000 || source != TierSourcePerClip {
		t.Fatalf("TierInfo 应为 500000/per_clip，实际 %d/%s", unit, source)
	}
}
