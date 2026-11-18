package billing

import (
	"sort"
	"strings"

	"lingdong-api/internal/model"
)

// 按规格计价的 kind（对齐 spec_prices.kind）。
//
// 导出：文档层（internal/apidoc）要按 kind 决定「这个模型是按张、按秒还是按次计价」，
// 判据必须与计费同源。以前是三个未导出常量，文档层只能自己写字符串字面量，
// 一旦改字面量就静默错档。
const (
	SpecKindPerImage = "per_image"
	SpecKindPerVideo = "per_video"
	// SpecKindPerTask 是「一次任务固定价」（音乐的存量口径）。
	//
	// 为什么音乐已经改用 per_clip 还留着它：存量模型（例如已停用的 chirp-v4）
	// 的价目与在途订单都按这个口径落库，删掉常量会让它们变成「未知 kind」
	// 并在保存任何字段时报错。新配置一律走 per_clip。
	SpecKindPerTask = "per_task"
	// SpecKindPerClip 是「按实际交付条数计价」（音乐/歌词）。
	//
	// 为什么不是 per_task：一次任务出 1 首还是 2 首，per_task 收同一个价 ——
	// 少交付的那一半无法解释，也无法与"按实际交付结算"的行业口径对齐。
	// 为什么不是 per_image：那条链路绑死"档位 × 张数"，计价要命中 tier_price，
	// 且 ResolveImageCount 会把 0 张夹成 1 张，与"0 条免单"直接冲突。
	SpecKindPerClip = "per_clip"
	// SpecKindPerSecond 是「按产物秒数计价」（音频 TTS/ASR）。
	//
	// 与 per_video 的区别：视频按秒还要乘分辨率档位，音频没有档位，
	// 只有一个单价 + 一个最低消费。用 per_video 表达音频需要编造档位键
	// （"720p" 对音频毫无意义），计费口径会变成一句假话。
	SpecKindPerSecond = "per_second"
)

// MaxImageN 是单次图片生成张数的引擎上限。
//
// 导出：文档要写「最多几张」，写死的数字与这里不一致时用户会按文档传参然后被拒。
const MaxImageN = 128

// ImageLayerDecompMax 是「图层拆分」单次产物上限：1 张底图 + 最多 16 个图层。
//
// 一个数字必须在两处同时生效，缺一处就是"功能不可用"或"确定少收"：
//   - **冻结张数**（relay.imageCountHint）：只改 max_n 不改它 → 冻结 1 张、
//     结算 17 张，被 3 倍截顶后少收 14 张（平台承担差额）；
//   - **收费上限**（models.spec_prices.max_n，预设写入这里）：只改它不改 hint →
//     冻结 17 > max_n，冻结阶段直接 ErrInvalidTier，图层拆分一次也跑不起来。
const ImageLayerDecompMax int64 = 17

// MaxMusicClips 是单次音乐任务的计费条数上限（引擎护栏，不是业务事实）。
//
// SUNO 一次出 2 首、歌词 1 篇；留 4 是给"未来可能出 4 首"的模型留位置，
// 同时防止运营把 max_n 配成 999 之后一次冻结把用户余额整段占住。
// 真正的条数事实由模型价目里的 max_n 表达，这里只做最后的夹取。
const MaxMusicClips int64 = 4

// MusicClipFreezeCount 返回一次音乐任务在**冻结期**应该按几条折算。
//
// 冻结期还不知道上游会交付几首，只能按上限占额度（与图片按 n 冻结同一道理）。
// max_n 未配置时按 2 条 —— SUNO 的默认交付条数，也是"两首同价"时代的旧口径；
// 配了 max_n 就尊重它（歌词模型配 1）。
func MusicClipFreezeCount(sp model.SpecPrices) int64 {
	n := int64(2)
	if sp.MaxN != nil && *sp.MaxN > 0 {
		n = *sp.MaxN
	}
	if n > MaxMusicClips {
		n = MaxMusicClips
	}
	if n < 1 {
		n = 1
	}
	return n
}

// IsPerClip 报告该价目是否按条计价。
//
// 调用方（任务侧结算、回收、文档）必须用它，不要自己比字符串：
// "per_clip" 这个字面量散在四处时，改一处漏一处的症状是「某条路径仍按任务计价」。
func IsPerClip(sp model.SpecPrices) bool {
	return strings.TrimSpace(sp.Kind) == SpecKindPerClip
}

// SpecFacts 是 spec_prices 的只读规范化视图，也是**唯一**解析入口。
//
// 为什么必须收敛：FixedTier / SupportsVideoTier / ImageCountHint / TierInfo 之前各自
// 反序列化一遍 spec_prices。解析本身不贵，但「哪一档算已配置」的判断分散在四处，
// 新增档位或改语义时必须同时改对四处；漏一处的表现是「文档说支持 4K、
// 下单却报档位未知」——这类不一致最难查，因为两侧都"看起来没错"。
//
// 用法约束：
//   - 只能通过 ParseSpecFacts 构造（零值表示「未配置」，Valid=false）；
//   - 排序敏感的场景用 ImageTiers / VideoTiers（已排序），**不要**自己遍历 map；
//   - 文档层只读导出的「有哪些档/上限多少」，不读单价（文档不写金额）。
type SpecFacts struct {
	// Kind 是 spec_prices.kind（per_image / per_video / per_task）；空串 = 未配置。
	Kind string
	// Valid 表示可解析（kind 未知也算 true，由调用方按 Kind 分派）。
	Valid bool

	// ImageTiers 是已配置的图像档位键，按 1K → 2K → 3K → 4K 排序（只含已配置的）。
	ImageTiers []string
	// NMax 是 max_n 原值；<=0 表示未配置，调用方回落到 MaxImageN。
	NMax int64

	// VideoTiers 是已配置的视频档位键，按 480p → 720p → 1080p → 2k 排序。
	VideoTiers []string
	// MaxSeconds 是模型级秒数上限；<=0 表示未配置（只受 MaxVideoSeconds 约束）。
	MaxSeconds int64
	// ResolutionFixed 是默认计费档（图片与视频共用，已转小写）；空串 = 没配默认档。
	ResolutionFixed string
	// AutoDuration 表示该模型接受「智能时长」（seconds:-1 / duration:-1）。
	//
	// 文档层要据此写一句说明：能传 -1 的模型与不能传的模型，同一句
	// 「seconds 必填，传 -1 会报错」对其中一半是假话。
	AutoDuration bool

	// FreeRefImages / FreeRefImagesSet：ref_image_free 是否配置与值。
	// **语义**：前 N 张免费，0 = 无免费（旧实现的"0 = 全部免费"是反直觉陷阱，已取消）。
	FreeRefImages    int64
	FreeRefImagesSet bool

	// HasRefImagePrice / HasRefAudioPrice 报告是否配了参考图 / 参考音频单价。
	HasRefImagePrice bool
	HasRefAudioPrice bool
	// RefVideoSecondTiers 是按秒计价的参考视频档位（非空即按秒计费）。
	RefVideoSecondTiers []string
	// RefVideoPerClip 表示按「条」计价生效。
	RefVideoPerClip bool

	// LongVideoAfter / LongVideoMultiplier 是超时阶梯；After<=0 表示未启用。
	// Multiplier 是**微比例**（1500000 = 1.5 倍），与金额同一单位体系。
	LongVideoAfter      int64
	LongVideoMultiplier int64

	// HasPerTaskPrice 表示配了 per_task_price。
	HasPerTaskPrice bool

	// HasPerClipPrice 表示配了 per_clip_price（音乐/歌词按条计费）。
	HasPerClipPrice bool

	// raw 是原始价目，仅供本包（计费/TierInfo）读取，不导出：
	// 文档不写金额，一旦导出就会有人拿它去前端算钱，形成第二份计价逻辑。
	raw model.SpecPrices
}

// ParseSpecFacts 解析 spec_prices。空值/非法返回零值（Valid=false），不报错。
//
// 刻意不返回 error：调用方（FixedTier / SupportsVideoTier / ImageCountHint / TierInfo）
// 的既有契约就是「非法即静默回落」，改成 error 会改变计费错误的对外表现。
// 但**保存期**用的是严格解码（model.DecodeSpecPrices），非法配置根本进不了库。
func ParseSpecFacts(sp model.SpecPrices) SpecFacts {
	if sp.IsZero() {
		return SpecFacts{}
	}
	f := SpecFacts{
		Kind:       sp.Kind,
		Valid:      true,
		raw:        sp,
		ImageTiers: sortedImageTiers(ImageTierMap(sp)),
		VideoTiers: sortedVideoTiers(sp.SecondPrice),

		RefVideoSecondTiers: sortedVideoTiers(sp.RefVideoSecondPrice),
		RefVideoPerClip:     sp.RefVideoPrice != nil && len(sp.RefVideoSecondPrice) == 0,

		HasRefImagePrice: sp.RefImagePrice != nil,
		HasRefAudioPrice: sp.RefAudioPrice != nil,
		HasPerTaskPrice:  sp.PerTaskPrice != nil,
		HasPerClipPrice:  sp.PerClipPrice != nil,
	}
	if sp.MaxN != nil {
		f.NMax = *sp.MaxN
	}
	if sp.MaxSeconds != nil {
		f.MaxSeconds = *sp.MaxSeconds
	}
	f.ResolutionFixed = strings.ToLower(strings.TrimSpace(sp.ResolutionFixed))
	f.AutoDuration = sp.AllowAutoDuration != nil && *sp.AllowAutoDuration
	if sp.RefImageFree != nil {
		f.FreeRefImages, f.FreeRefImagesSet = *sp.RefImageFree, true
	}
	if sp.LongAfterSeconds != nil {
		f.LongVideoAfter = *sp.LongAfterSeconds
	}
	if sp.LongVideoMultiplier != nil {
		f.LongVideoMultiplier = *sp.LongVideoMultiplier
	}
	return f
}

// sortedImageTiers 按清晰度升序列出**全部**已配置的图像档位。
//
// 必须保留任意键：档位由运营自由配置，早先「词表外的键一律丢弃」会让
// 运营配的 `8K` 既不计费、也不出现在文档里，而请求照常按它冻结 —— 用户看到的是
// 「文档没写这个档、账单却按这个档收了」。排序按面积，认不出面积的键排在最后。
func sortedImageTiers(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		ri, rj := ImageTierRank(out[i]), ImageTierRank(out[j])
		if ri != rj {
			return ri < rj
		}
		return out[i] < out[j]
	})
	return out
}

// sortedVideoTiers 按清晰度升序列出**全部**已配置的视频档位（同上的「不丢键」口径）。
func sortedVideoTiers(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		ri, rj := videoTierOrder(out[i]), videoTierOrder(out[j])
		if ri != rj {
			return ri < rj
		}
		return out[i] < out[j]
	})
	return out
}

// VideoTierPrice 返回视频档位秒价。未配置返回 false。
func (f SpecFacts) VideoTierPrice(tier string) (int64, bool) {
	v, ok := f.raw.SecondPrice[tier]
	return v, ok
}

// PerTaskPrice 返回一次任务固定价。未配置返回 false。
func (f SpecFacts) PerTaskPrice() (int64, bool) {
	if f.raw.PerTaskPrice == nil {
		return 0, false
	}
	return *f.raw.PerTaskPrice, true
}

// PerClipPrice 返回按条计价的单价（微 / 条）。未配置返回 false。
func (f SpecFacts) PerClipPrice() (int64, bool) {
	if f.raw.PerClipPrice == nil {
		return 0, false
	}
	return *f.raw.PerClipPrice, true
}

// RefVideoSecondPrice 返回按秒计价的参考视频档位单价。未配置返回 false。
func (f SpecFacts) RefVideoSecondPrice(tier string) (int64, bool) {
	v, ok := f.raw.RefVideoSecondPrice[tier]
	return v, ok
}

// FixedTier 返回 spec_prices.resolution_fixed（未配置返回空串）。
//
// 调用方语义：空串 = 该模型没有固定档位，请求**必须**自己给 resolution，
// 否则无法确定计费档 → 拒绝提交。绝不在这里猜一个默认档：猜错就是计费倒挂。
func FixedTier(sp model.SpecPrices) string {
	return ParseSpecFacts(sp).ResolutionFixed
}

// HighestVideoTier 返回价目里**单价最高**的视频档位键（0 档时返回空串）。
//
// 只被「请求档位不在价目里」的兜底计费用：参数不再拦截后，请求照样出站，
// 平台必须选一个不亏的价格——最高档是唯一可解释的选择（视频已经生成了）。
func HighestVideoTier(sp model.SpecPrices) string {
	best, bestUnit := "", int64(-1)
	for _, k := range sortedVideoTiers(sp.SecondPrice) {
		if v := sp.SecondPrice[k]; v > bestUnit {
			best, bestUnit = k, v
		}
	}
	return best
}

// AllowsAutoDuration 报告该模型是否支持「智能时长」（上游自行决定时长）。
//
// 只有 video 模态且 spec_prices.allow_auto_duration=true 才为真。
// 未配置 = 不支持（fail-closed）：放开一个厂商不认的 -1 会被记成渠道故障，
// 把一条健康渠道熔断掉。
func AllowsAutoDuration(sp model.SpecPrices) bool {
	// 走 ParseSpecFacts 而不是直接读指针：AutoDuration 这个事实有**两个**消费者
	// （计费闸门与文档文案），两处各读一次指针，改结构时必然漏一处。
	return ParseSpecFacts(sp).AutoDuration
}

// AutoDurationBudget 返回「智能时长」请求的**计费基准秒数**（冻结用）。
//
// 为什么必须有一个正数基准：冻结额 = 秒价 × 秒数，而 -1 不能参与乘法
// （会被 clampSeconds 钳成 0 → 0 元冻结 → 免费出片）。
// 取 max_seconds 是唯一正确选择：它是该模型的时长上限，冻结即「按最坏情况占用」，
// 结算再按上游返回的实际时长回调（多冻结的部分退回）。
//
// 未配 max_seconds 时回落 defaultAutoDurationBudget（30 秒）：这是 Seedance 2.5 的
// 时长上限，也是这类模型最常见的上限；比它更保守（比如取 3600）会让一次请求
// 冻结出远超用户余额的额度，直接把正常用户挡在门外。
func AutoDurationBudget(sp model.SpecPrices) int64 {
	if sp.MaxSeconds != nil && *sp.MaxSeconds > 0 {
		return *sp.MaxSeconds
	}
	return defaultAutoDurationBudget
}

// defaultAutoDurationBudget 是未配 max_seconds 时的智能时长冻结基准（秒）。
const defaultAutoDurationBudget = 30

// SupportsVideoTier 报告该模型的秒价表里是否配置了指定档位。
//
// 只用于**选计费档**，不是参数校验：请求写了一个价目里没有的档位时，
// 任务层据此换成「固定档 / 最高价档」计费并留痕，请求照常出站。
// 早先它被用来在提交期拒绝请求（"本模型不支持该档"），而支持与否只有上游说了算。
func SupportsVideoTier(sp model.SpecPrices, tier string) bool {
	tier = strings.ToLower(strings.TrimSpace(tier))
	if tier == "" {
		return false
	}
	_, ok := ParseSpecFacts(sp).VideoTierPrice(tier)
	return ok
}

// RefVideoBillable 报告该价目是否**真的用得上**参考视频秒数（即按秒计）。
//
// 提交链路用它决定是否出站探测素材时长：只有 ref_video_second_price 非空才用秒数；
// 只有按条单价时 videoCharge 走 refVideos，秒数完全不参与计算 ——
// 此时探测纯属浪费（会给每次带视频的请求平白加数秒延迟）。
func RefVideoBillable(sp model.SpecPrices) bool {
	if len(sp.RefVideoSecondPrice) == 0 {
		return false
	}
	for _, v := range sp.RefVideoSecondPrice {
		if v > 0 {
			return true
		}
	}
	return false
}
