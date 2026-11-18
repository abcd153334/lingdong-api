package billing

import (
	"strings"

	"lingdong-api/internal/model"
)

// 运行时容错：**配置写错不能让模型不可用**。
//
// 背景（线上真实症状）：图片模型配好了 tier_price，但 models.billing_mode 落在列默认值
// `tokens` 上 → 网关在冻结前直接 400「模型未配置价格」；或者 spec_prices.kind 没写 →
// 计费层判「未配价」→ 同样 400。用户看到的是"这个模型调不了"，而价目明明是对的。
//
// 这里的原则只有一条：**按价目形状纠正计费方式，只升级、永不降级**。
//   - 升级 `""/tokens → spec` 是安全的：价目里有单价，说明运营确实想按规格卖；
//   - 反向降级（spec → tokens）会让图片/视频请求的 token 数为 0 → 收 0 元 = 白送，
//     所以 EffectiveMode 里没有任何降级分支。
//
// 纠偏结果写进 Snapshot.ConfigAnomaly，由 baseCharge 并进本次计费的 flags →
// 结算时落到 usages.anomaly，可被看板/对账直接筛出来。

// HasSpecPricing 报告价目里有没有**任何**可用于计费的价（不看 kind 与 billing_mode）。
//
// 它是"这个模型到底配没配价"的唯一判据：stats 的未配价计数、启动体检、
// 容错分支都调它 —— 各处自己写一遍 switch，结果就是「看板说 0 个未配价，
// 用户一调就 400」。
func HasSpecPricing(sp model.SpecPrices) bool {
	// ① 显式声明了 kind，且该 kind 需要的表非空 → 算"配了价目"。
	//    这条覆盖**全 0 的免费模型**：0 是合法配置，不是"没配"。
	if k := strings.TrimSpace(sp.Kind); k != "" && kindSupported(sp, k) {
		return true
	}
	// ② 没声明 kind（旧库迁移残留的常见形态）：必须至少有一个**正价**才算。
	//    只看"map 非空"会让 `tier_price:{"1K":0}` 这种残留把 chat 模型纠偏成
	//    per_image → 整单 0 元（白送）；那是 hash 级事故，不是配置容错。
	return hasPositive(sp.TierPrice) || hasPositive(sp.SecondPrice) ||
		positivePtr(sp.PerTaskPrice) || positivePtr(sp.PerSecondPrice)
}

// hasPositive 报告价目表里是否存在 >0 的单价。
func hasPositive(m map[string]int64) bool {
	for _, v := range m {
		if v > 0 {
			return true
		}
	}
	return false
}

// positivePtr 报告可选单价是否 >0。
func positivePtr(v *int64) bool { return v != nil && *v > 0 }

// kindSupported 报告价目里是否有该 kind 需要的那张表（与 EffectiveKind 的判据一致）。
func kindSupported(sp model.SpecPrices, kind string) bool {
	switch kind {
	case SpecKindPerImage:
		return len(sp.TierPrice) > 0
	case SpecKindPerVideo:
		return len(sp.SecondPrice) > 0
	case SpecKindPerTask:
		return sp.PerTaskPrice != nil
	case SpecKindPerClip:
		return sp.PerClipPrice != nil
	case SpecKindPerSecond:
		return sp.PerSecondPrice != nil
	}
	return false
}

// ModalityKinds 返回该模态**允许**的 spec kind 集合（非规格计价的模态返回 nil）。
//
// 必须是一个集合而不是单个 kind：视频可以用 per_video（按秒），也可以用
// per_task（一次任务固定价）—— 后者在"按条卖"的渠道上很常见。
// 只认一个值会把合法的 per_task 视频模型判成"价目与端点不符"，
// 进而被改成 per_video 而 second_price 为空 → 直接不可用（这是最糟的破坏）。
func ModalityKinds(modality string) []string {
	switch strings.ToLower(strings.TrimSpace(modality)) {
	case "image":
		return []string{SpecKindPerImage}
	case "video":
		return []string{SpecKindPerVideo, SpecKindPerTask}
	case "music":
		// per_clip 是新口径（按实际交付条数）；per_task 留着只为存量模型
		// （已停用的 chirp-v4 之类）能通过保存期的自洽检查 —— 把它们从集合里
		// 删掉，运营只要动一下标题就会撞"价目与端点不符"，模型直接不可编辑。
		return []string{SpecKindPerClip, SpecKindPerTask}
	case "audio":
		return []string{SpecKindPerSecond, SpecKindPerTask}
	}
	return nil
}

// EffectiveKindFor 在 EffectiveKind 之上加一道**端点模态**约束。
//
// 三级判定（顺序即优先级）：
//  1. 价目形状落在该模态允许的集合里 → 用它（绝大多数情况，无标记）；
//  2. 不在集合里，但该模态有另一张可用的表 → 按那张表算（正确口径）+ conflict；
//  3. 都不支持 → **保持形状**（能跑）+ conflict。
//
// 第 3 条是刻意的保守：配置错的模型按错口径收钱固然不对，但把它改成
// "本端点无可用价目"会让一个原本能出结果的模型彻底不可用 —— 那比多收/少收
// 更难接受（用户会直接报"模型坏了"）。错口径由标记暴露，运营去修配置。
func EffectiveKindFor(sp model.SpecPrices, modality string) (kind string, conflict bool) {
	kinds := ModalityKinds(modality)
	shape := EffectiveKind(sp)
	if len(kinds) == 0 {
		return shape, false
	}
	if shape == "" {
		// 形状推不出（价目全空）：取该模态首选 kind，让后续报"未配价"而不是沉默。
		return kinds[0], false
	}
	for _, k := range kinds {
		if k == shape {
			return shape, false
		}
	}
	for _, k := range kinds {
		if kindSupported(sp, k) {
			return k, true
		}
	}
	return shape, true
}

// EffectiveKind 返回「按价目形状，这个模型实际应该按哪种规格计费」。
//
// 顺序刻意是「先尊重声明、再按形状推断」：
//  1. 声明的 kind 自身自洽（它需要的那张价目表存在）→ 直接用它；
//  2. 否则按价目形状推断（tier → second → per_second → per_task）。
//
// 为什么不无条件按形状：一个视频模型若残留了 tier_price，无条件按形状会把它
// 判成图片，于是视频请求按张计价 —— 比"不纠正"更糟。
func EffectiveKind(sp model.SpecPrices) string {
	switch sp.Kind {
	case SpecKindPerImage:
		if len(sp.TierPrice) > 0 {
			return SpecKindPerImage
		}
	case SpecKindPerVideo:
		if len(sp.SecondPrice) > 0 {
			return SpecKindPerVideo
		}
	case SpecKindPerTask:
		if sp.PerTaskPrice != nil {
			return SpecKindPerTask
		}
	case SpecKindPerClip:
		if sp.PerClipPrice != nil {
			return SpecKindPerClip
		}
	case SpecKindPerSecond:
		if sp.PerSecondPrice != nil {
			return SpecKindPerSecond
		}
	}
	switch {
	case len(sp.TierPrice) > 0:
		return SpecKindPerImage
	case len(sp.SecondPrice) > 0:
		return SpecKindPerVideo
	case sp.PerSecondPrice != nil:
		return SpecKindPerSecond
	case sp.PerClipPrice != nil:
		return SpecKindPerClip
	case sp.PerTaskPrice != nil:
		return SpecKindPerTask
	}
	return ""
}

// EffectiveMode 返回实际生效的计费方式。
//
// **只允许 "" / tokens → spec 这一个方向**（tokens 是 models.billing_mode 的列默认值，
// 绝大多数"配了价却不生效"都是它）。call 是运营显式选的，不纠偏。
func EffectiveMode(mode string, sp model.SpecPrices) string {
	m := strings.TrimSpace(mode)
	if m != "" && m != model.BillingModeTokens {
		return m
	}
	if HasSpecPricing(sp) {
		return model.BillingModeSpec
	}
	return model.BillingModeTokens
}

// Effective 返回纠偏后的快照与本次纠偏产生的 anomaly（逗号分隔，可能为空）。
//
// 不改入参：快照是要落库的账单凭证，调用方应当显式替换。
func Effective(s Snapshot, modality string) (Snapshot, string) {
	flags := ""
	if mode := EffectiveMode(s.BillingMode, s.SpecPrice); mode != s.BillingMode {
		s.BillingMode = mode
		flags = joinSpecFlags(flags, model.AnomalyBillingModeCoerced)
	}
	if s.BillingMode == model.BillingModeSpec {
		k, conflict := EffectiveKindFor(s.SpecPrice, modality)
		if k != "" && k != s.SpecPrice.Kind {
			s.SpecPrice.Kind = k
			flags = joinSpecFlags(flags, model.AnomalySpecKindInferred)
		}
		if conflict {
			flags = joinSpecFlags(flags, model.AnomalySpecKindConflict)
		}
	}
	return s, flags
}

// PriceProblem 返回「这个模型无法出价」的原因（空串 = 可出价）。
//
// 唯一判据：stats 的未配价计数与启动体检都调它。两处各写一套 switch 的结果是
// 「看板报 0 个未配价，用户一调就 400」（现状正是如此：图片模型 kind=per_image
// 但价目全空时 IsZero() 为 false，看板就漏了）。
func PriceProblem(mode string, p model.Prices, sp model.SpecPrices) string {
	switch EffectiveMode(mode, sp) {
	case model.BillingModeSpec:
		if !HasSpecPricing(sp) {
			return "按规格计价但没有配置任何规格价（tier_price / second_price / …）"
		}
		if EffectiveKind(sp) == "" {
			return "价目形状无法推断出计费类型"
		}
		// 多档但没配默认档：运行期「不传 size」的请求会 ErrImageDefaultTierUnset（400）。
		// 它不会被 ErrPriceMissing 覆盖 —— 价目本身是好的，缺的是"按哪一档算"。
		if EffectiveKind(sp) == SpecKindPerImage && len(ImageTierMap(sp)) > 1 &&
			strings.TrimSpace(sp.ResolutionFixed) == "" {
			return "图片模型配置了多个档位但没有默认档（不传 size 的请求会报错）"
		}
		return ""
	case model.BillingModeCall:
		if p.Call == nil {
			return "按次计价但未配置单次价"
		}
		return ""
	case model.BillingModeTokens:
		if p.Input == nil {
			return "按量计价但未配置输入价"
		}
		return ""
	}
	return "未知计费方式"
}
