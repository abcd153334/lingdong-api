package billing

import (
	"errors"
	"strings"
	"testing"

	"lingdong-api/internal/model"
)

// 本文件是「图片计费全场景」的验收用例集。
//
// 每条用例对应一条**曾经出错或可能出错**的现实路径（用户不按文档写、运营少配字段、
// billing_mode 落在默认值上…）。改造的硬约束是：这些场景**都不许拒绝请求**，
// 只许按一个可解释的口径计费并留痕。
//
// 唯一允许失败的是「价目里一个档都没有」——那不是参数问题，是真没配价。

// imageSpec 构造快照（默认按规格计费）。
func imageSpec(sp model.SpecPrices, mode string) Snapshot {
	if mode == "" {
		mode = model.BillingModeSpec
	}
	return Snapshot{BillingMode: mode, SpecPrice: sp}
}

// TestImageDeclSizesIgnoredBefore 回归：`image_size` / `resolution` 曾**完全不参与计价**。
//
// 用户照 nano 系官方文档写 `image_size: 4K`，平台只读 `size`，于是按默认档算钱 ——
// 「指定了分辨率还是被按默认档收费」正是用户报的那个现象。
func TestImageDeclSizesIgnoredBefore(t *testing.T) {
	sp := proPrice() // 1K/1.5K=300000, 2K=450000
	cases := []struct {
		name string
		decl ImageDecl
		want string
	}{
		{"只写 size", ImageDecl{Size: "2K"}, "2K"},
		{"只写 image_size", ImageDecl{ImageSize: "2K"}, "2K"},
		{"只写 resolution", ImageDecl{Resolution: "2K"}, "2K"},
		{"size 优先于 image_size", ImageDecl{Size: "2K", ImageSize: "1K"}, "2K"},
		{"image_size 优先于 resolution", ImageDecl{ImageSize: "1K", Resolution: "2K"}, "1K"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			decl := c.decl
			size := ParseImageSize(decl)
			size.Anomaly = ResolveImageTier(sp, &size)
			if size.Tier != c.want {
				t.Fatalf("计费档应为 %s，实际 %q（DeclaredTier=%q Source=%q）",
					c.want, size.Tier, size.DeclaredTier, size.Source)
			}
			// 出站原文必须是被选中的那个字段：否则「按 2K 计费、按 1K 出图」。
			if size.Raw != c.want {
				t.Fatalf("出站原文应为 %q，实际 %q", c.want, size.Raw)
			}
		})
	}
}

// TestImagePricingAllScenarios 是计费口径的主矩阵：输入 → 档位 → 金额。
func TestImagePricingAllScenarios(t *testing.T) {
	pro := proPrice()
	cases := []struct {
		name      string
		decl      ImageDecl
		wantTier  string
		wantUnit  int64
		wantFlags string // 必须包含的标记（空 = 不校验）
	}{
		{
			// 最常见的调用方式：什么都不写。默认档 = resolution_fixed = 2K。
			name: "不写尺寸按默认档",
			decl: ImageDecl{},
			// 默认档显式配置成 2K，不算异常。
			wantTier: "2K", wantUnit: 450000,
		},
		{
			// 只给画幅：尺寸由上游挑，平台按默认档计费。
			name: "只给画幅按默认档",
			decl: ImageDecl{AspectRatio: "16:9"},
			// 上游会按 2K 出图（模型默认），与默认档一致。
			wantTier: "2K", wantUnit: 450000,
		},
		{
			name:     "size=auto 按默认档",
			decl:     ImageDecl{Size: "auto"},
			wantTier: "2K", wantUnit: 450000,
		},
		{
			// 明确要 1K：按 1K 收（不能悄悄按默认档收）。
			name:     "显式低档按该档",
			decl:     ImageDecl{Size: "1K"},
			wantTier: "1K", wantUnit: 300000,
		},
		{
			// 写了价目里没有的档位：不拒绝，按**最高单价档**计费并留痕。
			// 用户可能真的拿到了 8K 的图（上游支持），按 2K 收就是系统性少收。
			name:     "未配价档位按最高档",
			decl:     ImageDecl{Size: "8K"},
			wantTier: "2K", wantUnit: 450000, wantFlags: model.AnomalyImageTierUnpriced,
		},
		{
			// 像素写法：按面积吸附到最近的档（8192² 远超上界 → 最高档 + 越窗标记）。
			name:     "巨幅像素按最高档",
			decl:     ImageDecl{Size: "8192x8192"},
			wantTier: "2K", wantUnit: 450000, wantFlags: model.AnomalyImagePixelsOutOfWindow,
		},
		{
			// 档位与像素矛盾：取价高者，绝不按低价糊弄。
			name:     "档位与像素冲突取高价",
			decl:     ImageDecl{Size: "2K", Resolution: "1K"},
			wantTier: "2K", wantUnit: 450000, wantFlags: model.AnomalyImageSizeConflict,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			decl := c.decl
			size := ParseImageSize(decl)
			size.Anomaly = ResolveImageTier(pro, &size)
			if size.Tier != c.wantTier {
				t.Fatalf("档位应为 %s，实际 %q（flags=%q）", c.wantTier, size.Tier, size.Anomaly)
			}
			got, err := Charge(imageSpec(pro, ""), Usage{ImageSpec: size.Tier, ImageN: 1})
			if err != nil {
				t.Fatalf("不应报错：%v", err)
			}
			if got != c.wantUnit {
				t.Fatalf("单价应为 %d，实际 %d", c.wantUnit, got)
			}
			if c.wantFlags != "" && !strings.Contains(size.Anomaly, c.wantFlags) {
				t.Fatalf("必须留 %s，实际 %q", c.wantFlags, size.Anomaly)
			}
		})
	}
}

// TestDefaultTierIsConfigOnly 守「默认档只认配置」这一条。
//
// 曾经的「未配就取最低价档」兜底已经删除：它让「后台没配默认档」完全隐形，
// 用户按一个谁都没选过的档被收费（2026-11-04 实测 flash-lite 缺省落到 2K）。
func TestDefaultTierIsConfigOnly(t *testing.T) {
	// ① 多档但没配默认档 → 不选档，计费报「无法确定档位」。
	noDefault := proPrice()
	noDefault.ResolutionFixed = ""
	size := ParseImageSize(ImageDecl{})
	ResolveImageTier(noDefault, &size)
	if size.Tier != "" {
		t.Fatalf("未配默认档时不得自行选档，实际 %q", size.Tier)
	}
	if _, _, _, err := imageCharge(noDefault, Usage{ImageSpec: size.Tier, ImageN: 1}, true); !errors.Is(err, ErrImageDefaultTierUnset) {
		t.Fatalf("应报 ErrImageDefaultTierUnset，实际 %v", err)
	}

	// ② 默认档指向价目外的键（存量脏数据）→ 同路，不再悄悄退到别的档。
	badDefault := proPrice()
	badDefault.ResolutionFixed = "8K"
	size2 := ParseImageSize(ImageDecl{})
	ResolveImageTier(badDefault, &size2)
	if size2.Tier != "" {
		t.Fatalf("失效默认档不得自行选档，实际 %q", size2.Tier)
	}
	if _, _, _, err := imageCharge(badDefault, Usage{ImageSpec: size2.Tier, ImageN: 1}, true); !errors.Is(err, ErrImageDefaultTierUnset) {
		t.Fatalf("应报 ErrImageDefaultTierUnset，实际 %v", err)
	}

	// ③ 只有一档：没有第二个可选值，直接用它（这不是兜底，是无歧义）。
	single := tierPriceOf(map[string]int64{"1K": 300000}, "")
	size3 := ParseImageSize(ImageDecl{})
	if flags := ResolveImageTier(single, &size3); flags != "" {
		t.Fatalf("单档模型不应产生标记，实际 %q", flags)
	}
	if size3.Tier != "1K" {
		t.Fatalf("单档模型应直接用该档，实际 %q", size3.Tier)
	}

	// ④ 配了默认档：按配置收费，与档位单价一致。
	ok := proPrice()
	ok.ResolutionFixed = "2K"
	size4 := ParseImageSize(ImageDecl{})
	ResolveImageTier(ok, &size4)
	if size4.Tier != "2K" {
		t.Fatalf("应使用配置的默认档 2K，实际 %q", size4.Tier)
	}
}

// TestImageChargeNeverRejectsKnownInputs 守「价目有档位就不许计费失败」。
//
// 唯一的两个失败：价目**全空**（ErrPriceMissing）、以及**多档但没配默认档且没写 size**
// （ErrImageDefaultTierUnset）—— 后者是「不改报错就必须替用户挑一档」的场景，
// 挑任何一档都是在替运营定价。写了 size 就照常计费。
func TestImageChargeNeverRejectsKnownInputs(t *testing.T) {
	snaps := map[string]model.SpecPrices{
		"三档":    proPrice(),
		"单档":    tierPriceOf(map[string]int64{"1K": 300000}, ""),
		"免费":    tierPriceOf(map[string]int64{"1K": 0}, "1K"),
		"自定义档位": tierPriceOf(map[string]int64{"2.5K": 100000, "8K": 900000}, "2.5K"),
	}
	specs := []string{"", "1K", "2K", "8K", "1440p", "ultra", "auto", "1024x1024", "8192x8192"}
	for name, sp := range snaps {
		for _, spec := range specs {
			t.Run(name+"/"+spec, func(t *testing.T) {
				for _, n := range []int64{0, 1, 3, 500} {
					if _, err := Charge(imageSpec(sp, ""), Usage{ImageSpec: spec, ImageN: n}); err != nil {
						t.Fatalf("spec=%q n=%d 不应报错：%v", spec, n, err)
					}
				}
			})
		}
	}

	// 多档但没配默认档：只有「也没写 size」这一种请求会失败。
	noDefault := proPrice()
	noDefault.ResolutionFixed = ""
	if _, err := Charge(imageSpec(noDefault, ""), Usage{ImageSpec: "", ImageN: 1}); !errors.Is(err, ErrImageDefaultTierUnset) {
		t.Fatalf("无默认档且未指定 size 应报 ErrImageDefaultTierUnset，实际 %v", err)
	}
	if _, err := Charge(imageSpec(noDefault, ""), Usage{ImageSpec: "2K", ImageN: 1}); err != nil {
		t.Fatalf("写了 size 就应正常计费：%v", err)
	}

	// 价目全空才是真失败。
	if _, err := Charge(imageSpec(model.SpecPrices{Kind: SpecKindPerImage}, ""), Usage{ImageSpec: "1K"}); err == nil {
		t.Fatal("价目全空必须报 ErrPriceMissing（否则等于白送）")
	}
}

// TestChargeCeilingNotBelowCharge 守「结算上限 ≥ 实收」。
//
// 上限按**最高单价档 × 最大张数**算；若上限与实收用两套取价，就会出现
// 「3× 冻结截顶」把本来就该收的钱削掉（静默少收，账单上看不出来）。
func TestChargeCeilingNotBelowCharge(t *testing.T) {
	snaps := []model.SpecPrices{
		proPrice(),
		litePrice(),
		tierPriceOf(map[string]int64{"2.5K": 100000, "8K": 900000}, ""),
		// max_n 缺失也要有上限（引擎上限兜住），否则多交付的图全被削掉。
		func() model.SpecPrices {
			s := proPrice()
			s.MaxN = nil
			return s
		}(),
	}
	for i, sp := range snaps {
		snap := imageSpec(sp, "")
		ceiling, _ := ChargeCeiling(snap, Usage{ImageSpec: "1K", ImageN: 1})
		if ceiling <= 0 {
			t.Fatalf("snap[%d]：上限必须为正，实际 %d", i, ceiling)
		}
		for _, spec := range []string{"1K", "2K", "8K", ""} {
			for _, n := range []int64{1, 4, 128} {
				got, err := Charge(snap, Usage{ImageSpec: spec, ImageN: n})
				if err != nil {
					continue
				}
				if got > ceiling {
					t.Fatalf("snap[%d] spec=%q n=%d：实收 %d 超过上限 %d（会被截顶少收）",
						i, spec, n, got, ceiling)
				}
			}
		}
	}
}

// TestUpgradeDeliveredTierUsesSamePrice 守补收用的价与计费用的价同源。
//
// 上游交了更高档时要补收差额；若补收基线仍读已删除的「兜底价」，
// 会算出「补收 0」或负数（漏收），而计费侧已经按档位价收了。
func TestUpgradeDeliveredTierUsesSamePrice(t *testing.T) {
	sp := proPrice() // 1K=300000, 2K=450000
	u := Usage{ImageSpec: "1K", ImageN: 1}
	up, flag := UpgradeDeliveredTier(sp, u, "2K")
	if up.ImageSpec != "2K" {
		t.Fatalf("实际交付 2K 应升档，实际 %q", up.ImageSpec)
	}
	if flag == "" {
		t.Fatal("升档必须留标记（否则用户不知道被补收了）")
	}
	// 同档 / 降档一律不动：只升不降。
	if same, _ := UpgradeDeliveredTier(sp, u, "1K"); same.ImageSpec != "1K" {
		t.Fatalf("同档不应变动，实际 %q", same.ImageSpec)
	}
	// 上游交付了**价目外**的档位（8K，价目只到 2K）：按最高价档补收，
	// 而不是「认不出就放过」——放过等于每次交付高档都赔钱。
	if hi, flag := UpgradeDeliveredTier(sp, u, "8K"); hi == u || flag == "" {
		t.Fatalf("价目外的交付档位必须升档补收，实际 %q/%q", hi.ImageSpec, flag)
	}
	// 上游回报像素：按面积吸附到最近的档，同样只升不降。
	if hi, _ := UpgradeDeliveredTier(sp, u, "1792x1792"); hi.ImageSpec != "2K" {
		t.Fatalf("像素写法应吸附到 2K，实际 %q", hi.ImageSpec)
	}
}
