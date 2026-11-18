package billing

// 本文件守的是**计价格必须是整数微**。
//
// 为什么单独立一条：`spec_prices` 的全部数值字段都是 int64 微，
// 而运营/AI 编排最容易写成「元」的小数（`9000.5`、`1000.0`）。
// 一旦允许小数进来，历史失败方式是**整包零值**：解不动的字段全部变 0，
// 档位看起来还在、下单照样成功、按 0 额度结算 —— 静默免费。
//
// 现在 model.SpecPrices 是强类型结构体 + 严格解码（DisallowUnknownFields）：
//   - 小数 → 类型错，且报错点名到字段；
//   - 旧键名（*_quota / free_ref_images）→ 未知字段，直接拒绝，不会被静默忽略。
//
// 旧键名必须拒绝而不是兼容：沿用旧键会被按新单位（微）解释，差 100 万倍。

import (
	"strings"
	"testing"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/money"
)

func TestSpecPricesRejectDecimal(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantKey string // 报错里必须点名的字段/键
	}{
		{
			name:    "图片档位价是小数",
			raw:     `{"kind":"per_image","tier_price":{"1K":9000.5},"max_n":4}`,
			wantKey: "tier_price",
		},
		{
			// 写成 1000.0 的人以为自己填的是整数，必须同样拒绝：
			// 放过去就会得到 0 价，而错误提示里连字段名都不会出现。
			name:    "整数值写成浮点字面量",
			raw:     `{"kind":"per_image","tier_price":{"1K":9000.0},"max_n":4}`,
			wantKey: "tier_price",
		},
		{
			name:    "视频秒价是小数",
			raw:     `{"kind":"per_video","second_price":{"720p":12.5}}`,
			wantKey: "second_price",
		},
		{
			name:    "参考图单价是小数",
			raw:     `{"kind":"per_video","second_price":{"720p":120},"ref_image_price":3.5}`,
			wantKey: "ref_image_price",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := model.DecodeSpecPrices([]byte(c.raw))
			if err == nil {
				t.Fatalf("小数价格被接受了：%s", c.raw)
			}
			if !strings.Contains(err.Error(), "spec_prices") {
				t.Fatalf("错误信息没说是价目字段：%v", err)
			}
			// 报错必须点名到字段，否则运营只能一项一项试。
			if !strings.Contains(err.Error(), c.wantKey) {
				t.Fatalf("错误信息没有指出字段 %q：%v", c.wantKey, err)
			}
			// 必须说清"该怎么改"，而不是只抛 Go 的英文。
			if !strings.Contains(err.Error(), "整数微") {
				t.Fatalf("错误信息没给出改法（整数微）：%v", err)
			}
		})
	}
}

// 旧键名必须被拒绝：静默忽略 = 整块价目变成"未配置"，收费模型变免费。
func TestSpecPricesRejectLegacyKeys(t *testing.T) {
	cases := []struct{ name, raw, wantKey string }{
		{"档位价旧键", `{"kind":"per_image","resolution_prices_quota":{"1K":9000}}`, "resolution_prices_quota"},
		{"默认价旧键", `{"kind":"per_image","default_price_quota":1000}`, "default_price_quota"},
		{"秒价旧键", `{"kind":"per_video","second_price_quota":{"720p":120}}`, "second_price_quota"},
		{"免费张数旧键", `{"kind":"per_video","second_price":{"720p":120},"free_ref_images":0}`, "free_ref_images"},
		{"超时阈值旧键", `{"kind":"per_video","second_price":{"720p":120},"long_video_after_seconds":15}`, "long_video_after_seconds"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := model.DecodeSpecPrices([]byte(c.raw))
			if err == nil {
				t.Fatalf("旧键名被接受了（会被静默忽略 → 变免费）：%s", c.raw)
			}
			if !strings.Contains(err.Error(), c.wantKey) {
				t.Fatalf("错误信息没有指出旧键 %q：%v", c.wantKey, err)
			}
		})
	}
}

func TestSpecPricesAcceptInteger(t *testing.T) {
	cases := []struct{ name, modality, raw string }{
		// 兜底价取 1K 档价：兜底价必须落在已配档位之间（specprices.go 的区间校验），
		// 这个用例本身只验"整数写法能解析"，不要让它踩另一条校验。
		{"图片", "image", `{"kind":"per_image","resolution_fixed":"1K","max_n":4,"tier_price":{"1K":9000,"4K":36000}}`},
		{"视频", "video", `{"kind":"per_video","max_seconds":10,"second_price":{"720p":120,"1080p":200}}`},
		{"任务", "music", `{"kind":"per_task","per_task_price":50000}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sp, err := model.DecodeSpecPrices([]byte(c.raw))
			if err != nil {
				t.Fatalf("整数价格被拒绝：%v（%s）", err, c.raw)
			}
			norm, err := NormalizeSpecPrices(c.modality, sp)
			if err != nil {
				t.Fatalf("归一化失败：%v（%s）", err, c.raw)
			}
			if _, err := ValidateSpecPrices(c.modality, norm); err != nil {
				t.Fatalf("校验失败：%v（%s）", err, c.raw)
			}
		})
	}
}

// TestLongVideoMultiplierUnitTrap 锁定「倍率单位变了」这个静默陷阱。
//
// 旧写法 `"long_video_multiplier": 2` 表示 ×2；新口径是**微比例**（>1e6 才生效），
// 所以 2 会被当成"2 微比例"→ 直接不生效，而且**不报错**。
// 这条用例不是为了证明 2 合法，而是把"静默不生效"钉成一个可被发现的断言：
// 若哪天有人把 2 解释成 ×2，它会红，逼人回来看单位。
func TestLongVideoMultiplierUnitTrap(t *testing.T) {
	sp, err := model.DecodeSpecPrices([]byte(
		`{"kind":"per_video","second_price":{"720p":12000},"long_after_seconds":15,"long_video_multiplier":2}`))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	s := Snapshot{ModelID: "vid", BillingMode: model.BillingModeSpec, SpecPrice: sp}
	// 16s × 12000 = 192000：倍率为 2（微比例）不生效，不得变成 384000。
	got, err := Charge(s, Usage{VideoResolution: "720p", VideoSeconds: 16})
	if err != nil {
		t.Fatalf("Charge err: %v", err)
	}
	if got != 192000 {
		t.Fatalf("want 192000（倍率未生效）, got %d", got)
	}
}

// TestValidateDefaultTier 锁定「默认档」的两条规则。
//
//	非空 → 必须命中自己的 tier_price（**硬错**：配置引用自己的价目表，运行期无法补救）
//	为空 + 多档 → warning（运行期会取最低价档，能出图，但运营看不见按哪一档卖）
//
// 为什么不再要求"默认价等于某档价"：那是把**价格**当**档位选择器**用，
// 改一次档价就会悄悄换掉默认档，而账单上完全看不出来。
func TestValidateDefaultTier(t *testing.T) {
	maxN := int64(4)
	okCases := []struct {
		name string
		tier string
	}{
		{"等于最低档", "1K"},
		{"等于最高档", "2K"},
		{"大小写不同也命中", "2k"},
	}
	for _, c := range okCases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ValidateSpecPrices("image", model.SpecPrices{
				Kind: "per_image", TierPrice: map[string]int64{"1K": 9000, "2K": 13500},
				ResolutionFixed: c.tier, MaxN: &maxN,
			}); err != nil {
				t.Fatalf("默认档 %q 应通过：%v", c.tier, err)
			}
		})
	}

	// 指向价目外的键 → 硬错（存量数据由运行期兜成最低档 + anomaly）。
	if _, err := ValidateSpecPrices("image", model.SpecPrices{
		Kind: "per_image", TierPrice: map[string]int64{"1K": 9000, "2K": 13500},
		ResolutionFixed: "8K", MaxN: &maxN,
	}); err == nil || !strings.Contains(err.Error(), "不在已配置的分辨率档位里") {
		t.Fatalf("失效默认档必须拒绝保存，实际 %v", err)
	}

	// 未配置 + 多档 → 只 warning。
	warns, err := ValidateSpecPrices("image", model.SpecPrices{
		Kind: "per_image", TierPrice: map[string]int64{"1K": 9000, "2K": 13500}, MaxN: &maxN,
	})
	if err != nil {
		t.Fatalf("未配置默认档不应拒绝保存：%v", err)
	}
	found := false
	for _, w := range warns {
		if strings.Contains(w, "未配置默认档") {
			found = true
		}
	}
	if !found {
		t.Fatalf("必须回 warning 说明会按最低档计费，got %v", warns)
	}

	// 只有一档时不提示（无可选）。
	if warns, err := ValidateSpecPrices("image", model.SpecPrices{
		Kind: "per_image", TierPrice: map[string]int64{"1K": 9000}, MaxN: &maxN,
	}); err != nil || len(warns) != 0 {
		t.Fatalf("单档模型不应有告警：warns=%v err=%v", warns, err)
	}
}

// TestValidateImageAcceptsResolutionFixed 图片与视频共用「默认档」字段。
//
// 曾经图片**拒收** resolution_fixed（理由是"默认档只能有一个来源"），
// 而那个来源是 default_price —— 于是一个价格同时当金额和档位选择器。
// 现在两者合并成同一个字段：视频=型号固定档，图片=默认档。
func TestValidateImageAcceptsResolutionFixed(t *testing.T) {
	maxN := int64(1)
	if _, err := ValidateSpecPrices("image", model.SpecPrices{
		Kind: "per_image", TierPrice: map[string]int64{"1K": 9000, "2K": 13500},
		ResolutionFixed: "2K", MaxN: &maxN,
	}); err != nil {
		t.Fatalf("图片模型应接受 resolution_fixed（默认档）：%v", err)
	}
}

// TestRejectForeignSecondPriceAndMinPrice：per_second_price / min_price 配到
// 图片/视频/音乐模型上必须报错。
//
// 这两个键此前**没进 specFieldSet**，于是 rejectForeign 恒判"没配"：
// 运营把 per_second_price 填在图片模型上既不报错也不生效，而他以为按秒计费已开启 ——
// 这正是 rejectForeign 存在的理由。
func TestRejectForeignSecondPriceAndMinPrice(t *testing.T) {
	per := int64(5000)
	maxN := int64(4)
	tier := map[string]int64{"1K": 9000}
	cases := []struct {
		name     string
		modality string
		spec     model.SpecPrices
		wantKey  string
	}{
		{"图片配 per_second_price", "image", model.SpecPrices{
			Kind: "per_image", TierPrice: tier, MaxN: &maxN, PerSecondPrice: &per}, "per_second_price"},
		{"图片配 min_price", "image", model.SpecPrices{
			Kind: "per_image", TierPrice: tier, MaxN: &maxN, MinPrice: &per}, "min_price"},
		{"视频配 per_second_price", "video", model.SpecPrices{
			Kind: "per_video", SecondPrice: map[string]int64{"720p": 1000}, MaxSeconds: &maxN, PerSecondPrice: &per}, "per_second_price"},
		{"视频配 min_price", "video", model.SpecPrices{
			Kind: "per_video", SecondPrice: map[string]int64{"720p": 1000}, MaxSeconds: &maxN, MinPrice: &per}, "min_price"},
		{"音乐配 per_second_price", "music", model.SpecPrices{
			Kind: "per_task", PerTaskPrice: &per, PerSecondPrice: &per}, "per_second_price"},
		{"音乐配 min_price", "music", model.SpecPrices{
			Kind: "per_task", PerTaskPrice: &per, MinPrice: &per}, "min_price"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ValidateSpecPrices(c.modality, c.spec)
			if err == nil {
				t.Fatalf("应拒绝（%s 配 %s）", c.modality, c.wantKey)
			}
			if !strings.Contains(err.Error(), c.wantKey) {
				t.Fatalf("错误信息应指出 %q：%v", c.wantKey, err)
			}
		})
	}
}

// TestSpecPriceHardBounds 锁定「金额上界」与「倍率上界」。
//
// 这两个缺口的表现是**保存成功、结算却按不同金额走**：
//   - 兜底价只判了 <0，9e15 能入库，结算时被 maxCharge 截到 2 万 → 一张图收 2 万；
//   - 倍率只有下界，把 10 倍写成 100 倍也能保存，而它乘的是 max_seconds 的时长费，
//     等于给「按秒计费」的模型留了一个没有封顶的乘数。
func TestSpecPriceHardBounds(t *testing.T) {
	over := int64(9_000_000_000_000_000)
	tier := map[string]int64{"1K": 9000}
	maxN := int64(1)

	// 档位价超出 money.MaxAmount 必须拒绝（兜底价字段已删除，改验档位价）。
	if _, err := ValidateSpecPrices("image", model.SpecPrices{
		Kind: "per_image", TierPrice: map[string]int64{"1K": over}, MaxN: &maxN,
	}); err == nil {
		t.Fatalf("档位价超出 MaxAmount 必须拒绝")
	}
	_ = tier

	mult := int64(money.Scale * 100)
	after := int64(10)
	if _, err := ValidateSpecPrices("video", model.SpecPrices{
		Kind: "per_video", SecondPrice: map[string]int64{"720p": 1000},
		MaxSeconds: &after, LongAfterSeconds: &after, LongVideoMultiplier: &mult,
	}); err == nil {
		t.Fatalf("超时倍率超过 %d 倍必须拒绝", MaxLongVideoMultiplier)
	}

	// 10 倍（上界本身）必须放行：报错文案里的上限值就是它的合法值。
	ok := int64(money.Scale * MaxLongVideoMultiplier)
	if _, err := ValidateSpecPrices("video", model.SpecPrices{
		Kind: "per_video", SecondPrice: map[string]int64{"720p": 1000},
		MaxSeconds: &after, LongAfterSeconds: &after, LongVideoMultiplier: &ok,
	}); err != nil {
		t.Fatalf("%d 倍是上界，不应拒绝：%v", MaxLongVideoMultiplier, err)
	}
}

// TestSpecPriceMagnitudeWarning 锁定「疑似把元当微」的量级提示。
//
// 这条提示是价目链路上唯一能发现「单位写错」的手段：2 与 2000000 都合法，
// 机器无法判断运营的本意，只能把「这个数字不像一个价格」说出来。
func TestSpecPriceMagnitudeWarning(t *testing.T) {
	tiny := int64(2) // 2 微 = 0.000002 元
	maxN := int64(1)
	warnings, err := ValidateSpecPrices("image", model.SpecPrices{
		Kind: "per_image", TierPrice: map[string]int64{"1K": tiny}, MaxN: &maxN,
	})
	if err != nil {
		t.Fatalf("0.000002 元是合法金额，不应拒绝：%v", err)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "疑似把「元」当成「微」") {
		t.Fatalf("必须提示单位疑似写错，got %v", warnings)
	}
}
