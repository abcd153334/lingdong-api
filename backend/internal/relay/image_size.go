package relay

// 图片尺寸的解析入口（同步链路；异步链路在 service/task 里调同一组 billing 函数）。
//
// 设计前提（产品决定）：**模型支持什么尺寸由模型与官方文档决定，网关不做判定**。
// 所以这里只做两件事：
//   ① 把 size / aspect_ratio 解析成计费用的 ImageSize（解释不出来也不报错）；
//   ② 按模型价目把尺寸解释成一个可计价的档位（`billing.ResolveImageTier`）。
//
// 以前这里还有三道闸（拒绝历史别名、图层拆分只收档位、按方言筛候选）。
// 全部删除：它们拦的是用户参数，而"上游会不会接受"只有上游能回答；
// 拦错的代价是「用户按官方文档写的请求被我们 400」，比让上游回一句原话更糟。

import (
	"encoding/json"
	"strings"

	"lingdong-api/internal/model"
	"lingdong-api/internal/service/billing"
)

// aspectRatioOf 宽松读取 aspect_ratio。
//
// 必须是宽松的：relayHead 是一次性 json.Unmarshal，字段类型不符会让**整条**解析失败，
// 症状是「请求里明明有 model/prompt，却报参数不合法」。客户端把比例写成数字
// （`"aspect_ratio": 16`）并不罕见，这类值直接忽略即可，不该 400。
func aspectRatioOf(h relayHead) string {
	raw := h.AspectRatio
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	return ""
}

// firstNonEmptyText 返回第一个非空串（画幅别名的优先级：原生写法优先）。
func firstNonEmptyText(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// resolveImageSize 解析并归一图片尺寸（**永不返回 error**）。
//
// 必须在 billing.Estimate 之前调用：Estimate 会用档位算金额并冻结。
// 但这里不做任何拒绝 —— 解释不出档位时 ImageSize.Tier 为空，
// 计费回落到默认档（spec_prices.resolution_fixed）；默认档也没配时
// Estimate 会明确报错（不替用户挑档）。
func resolveImageSize(h relayHead, sp model.SpecPrices) billing.ImageSize {
	size := billing.ParseImageSize(billing.ImageDecl{
		Size:       string(h.Size),
		ImageSize:  string(h.ImageSize),
		Resolution: string(h.Resolution),
		// `ratio` 是图片端点上的历史画幅别名，与 `aspect_ratio` 同义。
		//
		// 这里曾经只传 aspect_ratio：字段解析了、注释也写了"必须解析否则计费看不见"，
		// 但解析结果没人用 —— 同步请求写 `ratio:16:9` 既不计画幅、适配器又把键丢掉，
		// 用户拿到一张方图而账单一切正常。异步链路（duomiImageSizeOf）却认它，
		// 同一个字段两条链路口径不同，正是"同一份请求被解释两遍"的典型症状。
		AspectRatio: firstNonEmptyText(strings.TrimSpace(aspectRatioOf(h)), strings.TrimSpace(h.Ratio)),
	})
	size.Anomaly = billing.ResolveImageTier(sp, &size)
	return size
}

// declaredImageCount 返回「用户真正下单的最大张数」；0 表示用户没说。
//
// 与 imageCountHint 的分工：imageCountHint 算「这次最多可能产出几张」（冻结用），
// declaredImageCount 只回答「要不要把结算张数收敛到用户下单数」。
//
// 注意这里**必须**在「用户没声明」时返回 0：edits / variations 不写 n 也会一次回多张，
// 拿 1 去收敛就是把合法交付压成 1 张（系统性少收）。
func declaredImageCount(h relayHead, maxN int64, hasMaxN bool) int64 {
	// max_n 未配置时只认用户**显式写的 n**：组图/图层的"上限"此时是平台兜底的 1 张，
	// 拿它去收敛结算会把上游真实交付的 15 张压成 1 张（系统性少收）。
	if !hasMaxN {
		if h.N.Int64() > 0 {
			return h.N.Int64()
		}
		return 0
	}
	if jsonTrue(h.LayerDecomp) || strings.EqualFold(strings.TrimSpace(h.SeqImageGen), "auto") || h.N.Int64() > 0 {
		// 结算期拿不到模型能力（这里只有请求头）：不做能力裁剪，
		// 由调用方的 max_n 与上游实际交付负责收敛。
		return imageCountHint(h, maxN, imageCaps{})
	}
	return 0
}

// imageMaxN 取该模型的张数上限（未配置时回落到引擎上限）。
//
// 它是冻结张数的**唯一**来源：网关与适配器都不再写死 15 / 17。
func imageMaxN(sp model.SpecPrices) int64 {
	if sp.MaxN != nil && *sp.MaxN > 0 {
		return *sp.MaxN
	}
	// 未配置 max_n 时上限按引擎上限：这个数只用于「夹紧用户要的张数」与
	// 「结算上限」，**不是**默认冻结张数 —— 用户没写 n 时 ImageCountHint 给 1 张。
	//
	// 两者必须分开：把上限当成默认值会让一次普通请求冻 128 张（余额直接打空，402）；
	// 把默认值当成上限会让用户显式要的 3 张被削成 1 张（少交付）。
	return billing.MaxImageN
}

// imageSizeFor 只把尺寸传给图片端点。
//
// 其它端点（chat / tts / video）也走同一个 dispatch，把零值 ImageSize 传下去会让
// 适配器误以为「这是一次 0x0 的图片请求」。nil 的语义是明确的「不是图片请求」。
func imageSizeFor(size *billing.ImageSize, kind endpointKind) *billing.ImageSize {
	if size == nil || !isImageKind(kind) {
		return nil
	}
	return size
}

// carrySpecFields 把请求侧的规格参数**整组**搬到结算用量上。
//
// 4 处构造 billing.Usage 的地方（请求侧 / 同步响应 / 流式 / 流式整段 JSON 兜底）
// 都调它：以前各自手抄 3 个字段，于是 ImageSpecExplicit 恒 false（tier_mismatch
// 条件恒假）、image_spec_source 恒空、ImageRatio 丢失。一个函数一处清单。
func carrySpecFields(dst *billing.Usage, spec billing.Usage) {
	// ImageN 由调用方决定（响应算出来的实际交付张数），这里不覆盖。
	dst.ImageSpec = spec.ImageSpec
	dst.ImageQuality = spec.ImageQuality
	dst.ImageRatio = spec.ImageRatio
	dst.ImageSpecExplicit = spec.ImageSpecExplicit
	dst.ImageSpecSource = spec.ImageSpecSource
	// 参考图张数是**计费维度**（ref_image_price），不是留痕字段。
	//
	// 曾漏搬这一个：冻结期按「参考图 + 生成张数」算过钱，结算期这里不搬、
	// 于是 ref_images 归零、参考图费永远为 0 —— 只要运营配了 ref_image_price，
	// 每条带参考图的请求都少收那部分。漏搬的表现是"账单看起来正常"，
	// 所以只能靠「请求侧维度整组搬」这条规矩守住。
	dst.RefImages = spec.RefImages
	dst.RefVideos = spec.RefVideos
	dst.RefAudios = spec.RefAudios
	dst.RefVideoSeconds = spec.RefVideoSeconds
	dst.VideoResolution = spec.VideoResolution
	dst.VideoSeconds = spec.VideoSeconds
}

// appendFlag 把标记追加到逗号分隔串里（去重、保序）。
func appendFlag(prev, add string) string {
	add = strings.TrimSpace(add)
	if add == "" {
		return prev
	}
	if prev == "" {
		return add
	}
	for _, f := range strings.Split(prev, ",") {
		if f == add {
			return prev
		}
	}
	return prev + "," + add
}
