package model

// PricingIsFree 判定模型是否「价格全部为 0」（免费模型）。
//
// 这是 is_free 的**唯一派生判据**（单真相源）：运行期计费只看价格数值，
// is_free 只是这个判据落库后的展示投影，供门户徽标与管理台回显使用，绝不参与扣费。
//
// 三种计费方式各有各的"0 价"形态：
//   - tokens：Input 必填；其余 lane 允许 nil（nil = 按输入价）。全部已配置值为 0 才算免费。
//   - call：Call 必填，== 0 即免费。
//   - spec：价目里所有已配置值全为 0 才算免费。
//
// 未配置（Input/Call 为 nil、spec 为空）视为**收费**：
// 无法证明它免费时，宁可让保存期报"未配置价格"，也不要静默标成免费放行。
func (m *Model) PricingIsFree() bool {
	if m == nil {
		return true
	}
	switch m.BillingMode {
	case BillingModeTokens:
		if m.Input == nil {
			return false
		}
		return m.Prices.AllConfiguredFree()
	case BillingModeCall:
		if m.Call == nil {
			return false
		}
		return *m.Call == 0
	case BillingModeSpec:
		return m.SpecPrices.AllConfiguredZero()
	default:
		return false
	}
}

// EffectiveSpecPrices 返回「当前计费方式下真正生效的规格价目」。
//
// 为什么需要它：`spec_prices` 与 `prices` 是两套并列的价目，`billing_mode` 只选其中
// 一套生效。库里另一套可能是历史残留（切过计费方式、手工改库、老版本行），而
// 所有对外投影（门户定价、内部价格快照、管理台 DTO）都在整行透传 —— 结果就是
// 「按次」的语音模型在门户上显示 ¥/秒 的旧价目，实际扣费走的却是 prices.call。
// 用户按看到的价算账、平台按另一套收，这是最难解释的一类账务投诉。
//
// 所以投影层一律取这个函数，**不要**直接读 m.SpecPrices。
func (m *Model) EffectiveSpecPrices() SpecPrices {
	if m == nil {
		return SpecPrices{}
	}
	return EffectiveSpecPricesFor(m.BillingMode, m.SpecPrices)
}

// EffectiveSpecPricesFor 是 EffectiveSpecPrices 的按值版本，供**投影快照**复用。
//
// 快照（pricing.ModelItem / PublicModel）已经把行拆成了字段，拿不到 *Model，
// 若在那边另写一份判断，两处就会各自演化 —— 一处补了、一处忘了，
// 症状仍然是"门户显示旧价目"，且只在一部分页面上出现。
//
// 只把 `tokens` / `call` 判为「不该有规格价目」：
// 空串不是"非 spec"，而是"这条查询没取 billing_mode"（例如能力补齐的
// `Select("id","modality","spec_prices",…)`）。此时若也判空，就会让
// 「按价目推导能力」凭空丢字段（ref_video 消失），而那条路径根本不做计费判断。
func EffectiveSpecPricesFor(billingMode string, sp SpecPrices) SpecPrices {
	switch billingMode {
	case BillingModeTokens, BillingModeCall:
		return SpecPrices{}
	default:
		return sp
	}
}

// AllConfiguredZero 报告 spec 价目里是否存在价格，且全部为 0。
//
// 返回 false 有两种情况，两者都不是"免费"：完全没有配置；存在正价。
//
// **只看金额字段**。max_n / max_seconds / ref_image_free / long_after_seconds 是
// 数量，long_video_multiplier 是比例 —— 把它们算进"是否全零"，会让一个
// 0 元视频模型（秒价 0 + max_seconds 30）被判成"不免费"，门户的免费徽标就没了。
// 这个 bug 之前真实存在：数量字段混进金额判断，正是「单位混用」的另一种形态。
func (s SpecPrices) AllConfiguredZero() bool {
	any := false
	check := func(v *int64) bool {
		if v == nil {
			return true
		}
		any = true
		return *v == 0
	}
	checkMap := func(m map[string]int64) bool {
		for k := range m {
			v := m[k]
			if !check(&v) {
				return false
			}
		}
		return true
	}
	if !check(s.PerTaskPrice) || !check(s.PerSecondPrice) ||
		!check(s.MinPrice) || !check(s.RefImagePrice) || !check(s.RefAudioPrice) ||
		!check(s.RefVideoPrice) ||
		!checkMap(s.TierPrice) || !checkMap(s.SecondPrice) || !checkMap(s.RefVideoSecondPrice) {
		return false
	}
	return any
}
