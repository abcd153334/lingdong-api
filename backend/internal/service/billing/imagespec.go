package billing

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"lingdong-api/internal/model"
)

// 图片尺寸契约与档位归一的**唯一实现**。
//
// 为什么放在 billing 而不是 relay：结算期要用同一套阈值判断「上游实际交付的尺寸」
// 是否与计费档一致（tier_mismatch），而 billing 不能反向 import relay（会成环）。
// 历史上这里有两份实现（同步一套 1024/2048、异步一套 1536/2560），
// 同一个 size 在两个端点落进不同价档。
//
// 契约（对外文档与此处必须一致）：
//   - 档位键：1K / 1.5K / 2K / 3K / 4K（大小写不敏感）；
//   - 像素：1024x1024（分隔符 x / X / * / ×，允许任意空白）；
//   - 画幅：16:9（**只能**通过独立字段 aspect_ratio 传）；
//   - auto：交给上游决定（仅部分模型）。
//
// `1.5K` 是**正式档位**，不是 1K 的别名：官方《图片生成 API》明确它「与 1K 同价、
// 出图效果更优」，Seedream 5.0 pro / flash 的可选值就是 1K / 1.5K / 2K。
// 它曾被降级成 1K（计费按 1K、交付也压到 1024²）：用户按官方推荐传 1.5K，
// 拿到的却是 1K 的图 —— 价格没变、交付缩水，属于最隐蔽的一种假话。
//
// 入参只有 size 与 aspect_ratio 两个字段。历史上还接受 resolution / image_size /
// ratio 三个同义别名，并在运行期判「多个字段是否写了不同值」，那是同一语义的五个入口：
// 文档要列五行、每个上游字段要删四次、冲突规则还得单独解释一遍。

// ErrImageDefaultTierUnset 是「图片模型没配默认档」的哨兵错误。
//
// 它和 ErrPriceMissing 分开：价目是有的（每个档都能计价），只是「不传 size 时按哪一档」
// 没配置。调用方看到「价目未配置」只会去查价格，实际上该补的是默认档字段；
// 而用户侧的动作也不同 —— 指定 size 就能继续。
var ErrImageDefaultTierUnset = errors.New("图片模型未配置默认档")

// ErrImageSpec 是「尺寸参数不可用」的哨兵错误。
//
// 调用方（relay / task）必须把它映射成 400 invalid_param，且**在冻结之前**：
// 冻结之后再报错就是一次「冻结 → 释放」的无效流水，还会污染渠道健康统计。
var ErrImageSpec = errors.New("图片尺寸参数不可用")

// 尺寸来源（落 usages.image_spec_source，用于回答「这个档位是用户选的还是平台推的」）。
const (
	ImageSizeSourceTier   = "tier"
	ImageSizeSourcePixels = "pixels"
	ImageSizeSourceRatio  = "ratio"
	ImageSizeSourceAuto   = "auto"
	ImageSizeSourceEmpty  = "empty"
	ImageSizeSourceOther  = "other"
)

// imageRawMaxLen 限制落库/标记里保存的尺寸原文长度（不影响出站）。
//
// 这里曾有 imageSideMin/Max、imageAreaMin/Max 四个"绝对边界"与 validPixels，
// 但它们**从未被调用**：面积越窗的处理是「切到最高档 + 留痕」（见 ResolveImageTier），
// 不是拒绝。留着只会让下一个人以为 8192 是硬上限 —— 已删除。
const imageRawMaxLen = 64

// TierSidePixels 是「档位 → 方形边长」的**唯一推导规则**：`side = round(N * 1024)`。
//
// 为什么用规则而不是常量表：档位由运营在价目里自由配置（8K / 2.5K / 6K…），
// 表一旦写死，新档位就没有面积，像素写法与面积吸附全部失效。
// 这条规则能完整复现历史的 1K/1.5K/2K/3K/4K（1024/1536/2048/3072/4096）。
func TierSidePixels(tier string) (int, bool) {
	t := strings.ToUpper(strings.TrimSpace(tier))
	if !strings.HasSuffix(t, "K") {
		return 0, false
	}
	num := strings.TrimSuffix(t, "K")
	if num == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || f <= 0 || f > 64 {
		return 0, false
	}
	return int(math.Round(f * 1024)), true
}

func ImageTierMap(sp model.SpecPrices) map[string]int64 {
	if len(sp.TierPrice) == 0 {
		return nil
	}
	out := make(map[string]int64, len(sp.TierPrice))
	for k, v := range sp.TierPrice {
		key := strings.ToUpper(strings.TrimSpace(k))
		if key == "" {
			continue
		}
		// 不再按白名单过滤：档位由运营自由配置（8K / 2.5K / 私有名），
		// 过滤掉就等于「价目里有价、运行时永远不认」。
		if _, dup := out[key]; dup {
			continue
		}
		out[key] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// imageStandardRatios 是归一比例时允许吸附的标准画幅。
//
// 不吸附的表现是 usages.image_ratio 里出现 85:48 这种只有机器看得懂的值，
// 运营与用户都无法用它做统计或对账。
//
// 必须是**全部方言画幅超集**：像 1:3（688x2048）、1:8（nano 系）这类真实存在的
// 画幅若不在表里，像素写法归一后会落成 43:128 这样的分数，统计维度当场失效。
//
// 7:4 / 4:7 尤其不能漏：它是 DALL·E 3 仅有的横竖两个非方尺寸（1792x1024 / 1024x1792）。
// 漏掉之后 NormalizeRatio(1792,1024) 会吸附成 16:9（偏差 1.6%，刚好在容差内），
// 于是用户按上游唯一合法的横图传 1792x1024，却被换算成一个该方言根本不存在的画幅
// —— 文档看起来没问题，请求必然被拒。
var imageStandardRatios = []string{
	"1:1", "4:3", "3:4", "3:2", "2:3", "16:9", "9:16", "21:9", "9:21", "5:4", "4:5",
	"2:1", "1:2", "1:3", "3:1", "1:4", "4:1", "1:8", "8:1", "7:4", "4:7",
}

// ratioSnapTolerance 是像素比例吸附到标准画幅的最大相对偏差。
const ratioSnapTolerance = 0.02

// ImageSize 是一次图片请求解析后的尺寸规格。
//
// Tier / Explicit 由 ResolveImageTier 填充（依赖该模型的价目表），
// 其余字段由 ParseImageSize 填充（纯语法，与模型无关）。
type ImageSize struct {
	// DeclaredTier 是用户显式写下的档位（已大写）；多个字段冲突时为最终唯一值。
	DeclaredTier string
	// Width / Height 只在像素写法或由比例推算后有值。
	Width  int
	Height int
	// Ratio 是归一后的画幅（"16:9"），可能为空。
	Ratio string
	// Auto 表示用户显式写了 auto（交给上游决定尺寸）。
	Auto bool
	// Unknown 是既不是档位/像素/比例/auto 的原值（如上游私有写法 portrait_4_3）。
	// 保留它是为了**原样透传**：非标渠道可能认这些值，网关不该替它判死。
	Unknown string
	// Source 是判定来源（tier/pixels/ratio/auto/empty/other），落库用。
	Source string
	// Tier 是归一后的计费档（空 = 价目表里连一个档都没有，计费侧报未配价）。
	Tier string
	// Explicit 表示尺寸是用户明确指定的。只有它为 true 才做 tier_mismatch 比对：
	// auto / 没传 size 的请求由上游自己选档，报不一致只会变成噪声。
	Explicit bool
	// Raw 是原始 size 文本（截断后，便于留痕）。
	Raw string
	// InvalidRatio 表示 aspect_ratio 写了但**认不出来**（如 "foo" / "16:0"）。
	//
	// 必须单独标记而不是当"没写"：调用方要据此 400。此前这个错误被
	// `declaredRatio, _, _ :=` 丢掉，于是 `aspect_ratio:"abc"` 既不报错、
	// 也不计画幅、更不改出站 —— 用户以为设了 16:9，拿到一张方图而账单正常。
	// "auto" / 空串是合法写法（由模型决定），不进这里。
	InvalidRatio bool
	// Anomaly 是本次尺寸解释产生的标记（逗号分隔），随结算落 usages.anomaly。
	//
	// 参数不再被拦截，所以「解释不出来」必须留下可查证据 ——
	// 否则「按默认价收、上游按高档出图」这类差异永远没人发现。
	Anomaly string
}

// IsZero 判断这次请求是否完全没有声明尺寸。
func (s ImageSize) IsZero() bool {
	return s.Source == ImageSizeSourceEmpty
}

// ImageDecl 是三个同义尺寸字段的原文（优先级 size > image_size > resolution）。
//
// 用结构体而不是四个位置参数：size 与 image_size 写反了编译期完全看不出来，
// 后果是「按 A 档计价、按 B 档出图」。
//
// 为什么要认三个：`image_size` 是多米 nano 系的上游字段名，`resolution` 是视频
// 协议的习惯写法 —— 用户照厂商文档写，平台却只读 `size`，等于"写了等于没写"，
// 而账单照样按默认档扣钱。本轮之前它们被解析后直接丢掉。
type ImageDecl struct {
	Size        string
	ImageSize   string
	Resolution  string
	AspectRatio string
}

// ParseImageSize 解析尺寸三字段 + aspect_ratio —— **永不报错**。
//
// 设计前提：模型支持什么由用户与官方文档决定，网关不做参数合法性判定。
// 任何写法都原样保留（`Unknown` / `Raw`），由上游决定接受与否；
// 平台只在计费侧尽量解释出一个档位，解释不出来也不拒绝请求。
func ParseImageSize(d ImageDecl) ImageSize {
	raws := [...]string{d.Size, d.ImageSize, d.Resolution}

	out := ImageSize{Source: ImageSizeSourceEmpty}
	conflict := false
	// 第一轮：只认「具体意图」（档位 / 像素 / 比例）。最高优先级的命中者胜出，
	// 后面若还有具体意图且与它不一致，说明用户同时写了两个不同的尺寸 → 留痕。
	for _, raw := range raws {
		p := parseSizeToken(raw)
		switch p.Source {
		case ImageSizeSourceTier, ImageSizeSourcePixels, ImageSizeSourceRatio:
			if out.Source == ImageSizeSourceEmpty {
				out = p
			} else if !sameSizeIntent(out, p) {
				conflict = true
			}
		}
	}
	// 第二轮：没有具体意图时，才认 auto / 认不出的写法（如 portrait_4_3）。
	// 顺序反过来会让 `size=auto` 压过 `image_size=2K` —— 用户明明给了档位，
	// 平台却按"上游自己挑"计费。
	if out.Source == ImageSizeSourceEmpty {
		for _, raw := range raws {
			if p := parseSizeToken(raw); p.Source != ImageSizeSourceEmpty {
				out = p
				break
			}
		}
	}
	// 胜出的不是 size 时，若 size 也写了别的具体意图，同样算冲突。
	if out.Raw != strings.TrimSpace(truncRunes(d.Size, imageRawMaxLen)) && strings.TrimSpace(d.Size) != "" {
		if p := parseSizeToken(d.Size); isConcreteSize(p.Source) {
			conflict = true
		}
	}
	if conflict {
		out.Anomaly = joinSpecFlags(out.Anomaly, model.AnomalyImageSizeConflict)
	}

	declaredRatio, _, ratioErr := parseAspectField(d.AspectRatio)
	// auto / 空串是**合法**写法（由模型决定尺寸），parseAspectField 认不出它们，
	// 所以只有非 auto 的解析失败才算"写错了"。
	if ratioErr != nil && !strings.EqualFold(normalizeSizeText(d.AspectRatio), "auto") {
		out.InvalidRatio = true
	}
	switch {
	case declaredRatio != "" && out.Width > 0 && !ratioMatchesPixels(declaredRatio, out.Width, out.Height):
		// 画幅与像素矛盾：保留像素（更精确的意图），画幅取像素推导值，留痕不报错。
		out.Ratio = NormalizeRatio(out.Width, out.Height)
		out.Anomaly = joinSpecFlags(out.Anomaly, model.AnomalyImageRatioConflict)
	case declaredRatio != "":
		// size 里写了比例、aspect_ratio 又写了另一个比例：两者是同一个语义，
		// 保留 aspect_ratio（专用字段），但必须留痕 —— 否则「按 size 的比例计费、
		// 按 aspect_ratio 的比例出图」这种偏差在账单上完全看不出来。
		if out.Ratio != "" && !strings.EqualFold(out.Ratio, declaredRatio) {
			out.Anomaly = joinSpecFlags(out.Anomaly, model.AnomalyImageRatioConflict)
		}
		out.Ratio = declaredRatio
	case out.Width > 0:
		out.Ratio = NormalizeRatio(out.Width, out.Height)
	}

	if out.Width > 0 {
		out.Source = ImageSizeSourcePixels
	} else if out.DeclaredTier != "" {
		out.Source = ImageSizeSourceTier
	} else if out.Ratio != "" && !out.Auto {
		// 写下的是画幅（含 size 里写 16:9）：这是**认得出的意图**，不是"认不出的写法"。
		// 比例分支同时填了 Unknown（供适配器判「不要塞进 image_size」），
		// 若这里再以 Unknown 为条件，同一个包里就会出现「一处当画幅、一处当乱写」。
		out.Source = ImageSizeSourceRatio
	} else if out.Auto {
		out.Source = ImageSizeSourceAuto
	} else if out.Unknown != "" {
		out.Source = ImageSizeSourceOther
	}
	// Explicit 只看用户「写没写」，与归一结果无关：
	// 归一失败时它若跟着变 false，适配器会走「没声明尺寸」分支把 size 删掉 ——
	// 用户写了 4K 却按默认档出图，账单上看不出任何异常。
	// auto 不算「显式」：尺寸由上游挑，拿它去比对上游实际档位只会变成一堆假告警。
	out.Explicit = (out.Raw != "" && !out.Auto) || strings.TrimSpace(d.AspectRatio) != ""
	return out
}

// parseSizeToken 把**单个**字段原文解析成尺寸意图（不处理 aspect_ratio）。
func parseSizeToken(raw string) ImageSize {
	out := ImageSize{Source: ImageSizeSourceEmpty}
	trimmed := strings.TrimSpace(raw)
	out.Raw = truncRunes(trimmed, imageRawMaxLen)
	if v := normalizeSizeText(out.Raw); v != "" {
		switch {
		case isTierToken(v):
			out.DeclaredTier = strings.ToUpper(v)
			out.Source = ImageSizeSourceTier
		case func() bool {
			w, h, ok := asPixels(v)
			if ok {
				out.Width, out.Height = w, h
				out.Source = ImageSizeSourcePixels
			}
			return ok
		}():
		case func() bool { _, _, ok := asRatio(v); return ok }():
			// 写比例：按画幅理解，并保留原文供上游判断（不替它换算成像素）。
			out.Ratio = v
			out.Unknown = v
			out.Source = ImageSizeSourceRatio
		case strings.EqualFold(v, "auto"):
			out.Auto = true
			out.Source = ImageSizeSourceAuto
		default:
			out.Unknown = v
			out.Source = ImageSizeSourceOther
		}
	}
	return out
}

// isConcreteSize 判断解析结果里是否含"具体意图"（与 auto / 认不出的写法相对）。
func isConcreteSize(src string) bool {
	switch src {
	case ImageSizeSourceTier, ImageSizeSourcePixels, ImageSizeSourceRatio:
		return true
	}
	return false
}

// sameSizeIntent 判断两个解析结果是否指向同一个尺寸意图。
func sameSizeIntent(a, b ImageSize) bool {
	switch {
	case a.DeclaredTier != "" && b.DeclaredTier != "":
		return strings.EqualFold(a.DeclaredTier, b.DeclaredTier)
	case a.Width > 0 && b.Width > 0:
		return a.Width == b.Width && a.Height == b.Height
	case a.Ratio != "" && b.Ratio != "":
		return strings.EqualFold(a.Ratio, b.Ratio)
	}
	return false
}

// truncRunes 按字符截断（超长只影响留痕与档位识别，不影响出站）。
func truncRunes(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	return string([]rune(s)[:max])
}

// isTierToken 判断一个 token 是不是档位写法（`1K` / `1.5K` / `8K`）。
//
// 用规则而不是白名单：运营可以配置任意档位（8K / 2.5K / 6K），
// 白名单会让这些键永远识别不出、退化成「未知写法」而丢掉用户意图。
func isTierToken(s string) bool {
	t := strings.ToUpper(strings.TrimSpace(s))
	if !strings.HasSuffix(t, "K") || len(t) < 2 {
		return false
	}
	num := strings.TrimSuffix(t, "K")
	dot := false
	for i := 0; i < len(num); i++ {
		c := num[i]
		if c >= '0' && c <= '9' {
			continue
		}
		if c == '.' && !dot {
			dot = true
			continue
		}
		return false
	}
	return num != "" && num != "."
}

// ResolveImageTier 按该模型价目把尺寸解释成一个**可计价的档位**，永不报错。
//
// 返回值是逗号分隔的 anomaly 标记（可能为空）。原则：
//   - 能解释出档位 → 用用户意图对应的档位；
//   - 解释不出 → 退到默认档（未配置则最低价档）；价目全空才返回空串。
//
// 任何情况下都不拒绝请求：参数合法性由上游与官方文档决定，平台只负责算钱。
func ResolveImageTier(sp model.SpecPrices, s *ImageSize) string {
	if s == nil {
		return ""
	}
	tiers := ImageTierMap(sp)
	flags := s.Anomaly
	if len(tiers) == 0 {
		// 价目里一个档都没有：这里不编标记（"未配价"由 imageCharge 报，
		// 在这里再打一条只会让同一次失败出现两个原因）。
		return flags
	}

	// 1. 用户写的不是档位写法，但价目里有同名键（如自定义档 "ultra"）→ 认它。
	//    画幅（Source=Ratio）不走这条：它的 Unknown 只是原文副本，语义是"认得出的画幅"。
	if s.Unknown != "" && s.Source != ImageSizeSourceRatio {
		if k := tierKeyInMarket(s.Unknown, tiers); k != "" {
			s.DeclaredTier, s.Unknown = k, ""
			s.Source = ImageSizeSourceTier
		} else {
			flags = joinSpecFlags(flags, model.AnomalyImageSpecUnrecognized)
		}
	}
	// 声明档归一成价目里的真实键（大小写不敏感），后续全部用规范键。
	if s.DeclaredTier != "" {
		if k := tierKeyInMarket(s.DeclaredTier, tiers); k != "" {
			s.DeclaredTier = k
		}
	}

	var pixelTier string
	var area int64
	if s.Width > 0 && s.Height > 0 {
		area = int64(s.Width) * int64(s.Height)
		pixelTier = nearestTier(sp, area)
	}

	switch {
	case s.DeclaredTier != "" && pixelTier != "" && !strings.EqualFold(s.DeclaredTier, pixelTier):
		// 档位与像素矛盾：按**单价更高**的那个计费，绝不按低的收。
		s.Tier = higherUnitTier(sp, s.DeclaredTier, pixelTier)
		flags = joinSpecFlags(flags, model.AnomalyImageTierConflict)
	case s.DeclaredTier != "":
		s.Tier = s.DeclaredTier
	case pixelTier != "":
		s.Tier = pixelTier
	default:
		// 没有任何尺寸意图（含 size=auto / 只给画幅 / 认不出的写法）→ **默认档**。
		//
		// 默认档只认配置（spec_prices.resolution_fixed）。这里不再有「取最低价档」的兜底：
		// 那个兜底会让「后台没配默认档」这件事彻底隐形，用户按一个谁都没选的档被收费
		// （2026-11-04 实测：flash-lite 缺省落到 2K）。未配置时不猜档，
		// 由 imageCharge 报 ErrImageDefaultTierUnset，让调用方显式指定 size。
		s.Tier = defaultTierOf(sp)
	}

	// 2. 档位不在价目（用户写了 8K）→ 按最高单价档计费（不按低价糊弄）。
	if s.Tier != "" {
		if _, ok := tiers[s.Tier]; !ok {
			flags = joinSpecFlags(flags, model.AnomalyImageTierUnpriced)
			if t, _, hit := highestPricedTier(sp); hit {
				s.Tier = t
			}
		}
	}
	// 3. 面积窗口：唯一拦住「8192×8192（67MP）按 4K 收」的闸，保留。
	if area > 0 {
		if lo, hi, ok := imageAreaWindow(sp); ok {
			switch {
			case area > hi:
				flags = joinSpecFlags(flags, model.AnomalyImagePixelsOutOfWindow)
				if t, _, hit := highestPricedTier(sp); hit {
					s.Tier = t
				}
			case area < lo:
				// 比最低档还小：仍按最近的档收，不抬价（用户很可能只是写错）。
				flags = joinSpecFlags(flags, model.AnomalyImagePixelsOutOfWindow)
			}
		}
	}
	if s.Tier == "" {
		flags = joinSpecFlags(flags, model.AnomalyImageSpecUnrecognized)
	}
	return flags
}

// tierKeyInMarket 在价目键里找与用户写法等价的键（大小写不敏感）。
func tierKeyInMarket(raw string, tiers map[string]int64) string {
	if len(tiers) == 0 {
		return ""
	}
	want := strings.ToUpper(strings.TrimSpace(raw))
	for k := range tiers {
		if strings.EqualFold(k, want) {
			return k
		}
	}
	return ""
}

// higherUnitTier 返回两个档位里**单价更高**的那个（价目缺失时取另一个）。
func higherUnitTier(sp model.SpecPrices, a, b string) string {
	tiers := ImageTierMap(sp)
	ua, oka := tiers[a]
	ub, okb := tiers[b]
	switch {
	case oka && okb:
		if ub > ua {
			return b
		}
		return a
	case oka:
		return a
	default:
		return b
	}
}

// joinSpecFlags 追加一个标记（去重、保序、逗号分隔）。
func joinSpecFlags(prev, add string) string {
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

// defaultTierOf 返回「请求没写尺寸时按哪一档计费」，**只认配置**。
//
// 唯一的来源是 `spec_prices.resolution_fixed`（与视频同一个字段）：
//   - 命中价目 → 用它；
//   - 价目只有一档 → 用那一档（没有第二个可选值，谈不上"猜"）；
//   - 其余 → 空，由 `imageCharge` 报 `ErrImageDefaultTierUnset`。
//
// **这里不再有「取最低价档」的兜底**：它让「后台没配默认档」彻底隐形，
// 用户会被按一个谁都没选过的档收费（2026-11-04 实测：flash-lite 缺省落到 2K）。
// 也**不按最高档兜底**——那同样是替运营做定价决定。
//
// 为什么不「由 default_price 反推」：那要求兜底价恰好等于某个档价，改一次档价
// 就会悄悄换掉默认档 —— 一个价格同时当"金额"和"档位选择器"用，是本目录最大的坑。
func defaultTierOf(sp model.SpecPrices) string {
	tiers := ImageTierMap(sp)
	if len(tiers) == 0 {
		return ""
	}
	if k := tierKeyInMarket(sp.ResolutionFixed, tiers); k != "" {
		return k
	}
	if len(tiers) == 1 {
		for k := range tiers {
			return k
		}
	}
	return ""
}

// highestPricedTier 返回价目里**单价最高**的档（并列取清晰度低者，再取字典序）。
//
// 两处使用：① 请求档位不在价目时的保底计费；② ChargeCeiling 求上界。
// 两处必须同一个实现 —— 上限若按另一套取价算，会低于实收（静默少收）。
func highestPricedTier(sp model.SpecPrices) (tier string, unit int64, ok bool) {
	for k, v := range ImageTierMap(sp) {
		switch {
		case !ok, v > unit:
			tier, unit, ok = k, v, true
		case v == unit && (ImageTierRank(k) < ImageTierRank(tier) ||
			(ImageTierRank(k) == ImageTierRank(tier) && k < tier)):
			tier = k
		}
	}
	return tier, unit, ok
}

// priceOfImage 是图片档位取价的**唯一实现**。
//
// imageCharge / ChargeCeiling / TierInfo 三处都必须调它：任何一处自己读
// `sp.TierPrice`，就会出现"上限与实收用两套价格"——那比截顶本身更危险。
//
//	tier 命中        → (单价, TierSourceResolution)
//	tier 非空但未命中 → (最高单价档, TierSourceHighest)
//	tier 为空        → (默认档, TierSourceDefault)
//	价目为空          → ok=false（调用方 ErrPriceMissing）
func priceOfImage(sp model.SpecPrices, tier string) (unit int64, source string, ok bool) {
	tiers := ImageTierMap(sp)
	if len(tiers) == 0 {
		return 0, "", false
	}
	if k := tierKeyInMarket(tier, tiers); k != "" {
		return tiers[k], TierSourceResolution, true
	}
	if strings.TrimSpace(tier) != "" {
		if _, v, hit := highestPricedTier(sp); hit {
			return v, TierSourceHighest, true
		}
	}
	if d := defaultTierOf(sp); d != "" {
		return tiers[d], TierSourceDefault, true
	}
	return 0, "", false
}

// ImageTierRank 返回图像档位的清晰度序号（越大越清晰）；未知档返回 -1。
func ImageTierRank(tier string) int {
	if p, ok := PixelsOfTier(tier); ok {
		return int(p / (1024 * 1024))
	}
	return 0
}

// PixelsOfTier 返回档位的方形像素数。
func PixelsOfTier(tier string) (int64, bool) {
	side, ok := TierSidePixels(tier)
	if !ok {
		return 0, false
	}
	return int64(side) * int64(side), true
}

// NormalizeRatio 把宽高约分并吸附到标准画幅（偏差 ≤2%）。
func NormalizeRatio(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	g := gcd(w, h)
	rw, rh := w/g, h/g
	exact := fmt.Sprintf("%d:%d", rw, rh)
	value := float64(rw) / float64(rh)
	best, bestDiff := exact, math.Inf(1)
	for _, label := range imageStandardRatios {
		a, b, ok := asRatio(label)
		if !ok {
			continue
		}
		target := float64(a) / float64(b)
		diff := math.Abs(math.Log(value) - math.Log(target))
		if diff < bestDiff {
			best, bestDiff = label, diff
		}
	}
	if math.Abs(math.Exp(bestDiff)-1) <= ratioSnapTolerance {
		return best
	}
	return exact
}

// nearestTier 在该模型已配档位里按面积就近吸附（并列取更高档 = 更保守的收费）。
func nearestTier(sp model.SpecPrices, area int64) string {
	if area <= 0 {
		return ""
	}
	best, bestDiff := "", math.Inf(1)
	for tier := range ImageTierMap(sp) {
		p, ok := PixelsOfTier(tier)
		if !ok {
			continue
		}
		diff := math.Abs(math.Log(float64(area)) - math.Log(float64(p)))
		switch {
		case diff < bestDiff-1e-9:
			best, bestDiff = tier, diff
		case math.Abs(diff-bestDiff) <= 1e-9 && ImageTierRank(tier) > ImageTierRank(best):
			best = tier
		}
	}
	return best
}

// imageAreaWindow 是「该模型可售尺寸」的面积区间：最低档的一半 ~ 最高档的两倍。
//
// 没有这道闸，像素写法就绕过了档位区间：8192×8192（67MP）会吸附到 4K 档并按 4K 收，
// 而按面积算它值 4 张 4K；256×256 则会吸附到 1K 档并收 1K 的钱。
func imageAreaWindow(sp model.SpecPrices) (lo, hi int64, ok bool) {
	var minP, maxP int64
	for tier := range ImageTierMap(sp) {
		p, ok := PixelsOfTier(tier)
		if !ok {
			continue
		}
		if minP == 0 || p < minP {
			minP = p
		}
		if p > maxP {
			maxP = p
		}
	}
	if minP == 0 || maxP == 0 {
		return 0, 0, false
	}
	return minP / 2, maxP * 2, true
}

// normalizeSizeText 统一书写：去两端与内部空白，× / * / 全角 x 一律转成 x。
//
// 内部空白必须一起去掉：`" 1024 x 1024 "` 在多米的 strings.FieldsFunc 解析里
// 会留下 " 1024 "，Atoi 失败后变成「无法识别 size」，而计费侧却认得。
func normalizeSizeText(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if len(s) > imageRawMaxLen {
		s = s[:imageRawMaxLen]
	}
	s = strings.Join(strings.Fields(s), "")
	return strings.NewReplacer("×", "x", "*", "x", "X", "x", "ｘ", "x", "＊", "x").Replace(s)
}

// asTierKey 把写法归一成档位键（大写）。判据是**规则**不是词表：
// 运营可以配 `8K` / `2.5K` / `6K`，查词表会让这些档位在上游回报时被判成"无法解析"。
func asTierKey(s string) (string, bool) {
	t := strings.ToUpper(strings.TrimSpace(s))
	if !isTierToken(t) {
		return "", false
	}
	return t, true
}

// asPixels 解析 `WxH`（已经过 normalizeSizeText，分隔符只剩 x）。
func asPixels(s string) (int, int, bool) {
	i := strings.IndexByte(s, 'x')
	if i <= 0 || i == len(s)-1 {
		return 0, 0, false
	}
	w, errW := strconv.Atoi(s[:i])
	h, errH := strconv.Atoi(s[i+1:])
	if errW != nil || errH != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// asRatio 解析 `W:H`。
func asRatio(s string) (int, int, bool) {
	i := strings.IndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return 0, 0, false
	}
	w, errW := strconv.Atoi(s[:i])
	h, errH := strconv.Atoi(s[i+1:])
	if errW != nil || errH != nil || w <= 0 || h <= 0 || w > 64 || h > 64 {
		return 0, 0, false
	}
	return w, h, true
}

// parseAspectField 解析 aspect_ratio 字段（宽松：允许 "16:9" 与 "16x9"）。
func parseAspectField(raw string) (string, bool, error) {
	s := normalizeSizeText(raw)
	if s == "" {
		return "", false, nil
	}
	if w, h, ok := asRatio(s); ok {
		if !ratioWithinLimits(w, h) {
			return "", false, fmt.Errorf("%w：aspect_ratio=%s 过于极端（宽高比需在 1:16 至 16:1 之间）", ErrImageSpec, raw)
		}
		return fmt.Sprintf("%d:%d", w, h), true, nil
	}
	if w, h, ok := asPixels(s); ok {
		if !ratioWithinLimits(w, h) {
			return "", false, fmt.Errorf("%w：aspect_ratio=%s 过于极端（宽高比需在 1:16 至 16:1 之间）", ErrImageSpec, raw)
		}
		return NormalizeRatio(w, h), true, nil
	}
	return "", false, fmt.Errorf("%w：aspect_ratio=%q 无法识别，请写成 16:9 这样的比例", ErrImageSpec, raw)
}

func ratioWithinLimits(w, h int) bool {
	r := float64(w) / float64(h)
	return r >= 1.0/16.0 && r <= 16.0
}

func ratioMatchesPixels(ratio string, w, h int) bool {
	rw, rh, ok := asRatio(ratio)
	if !ok {
		return false
	}
	want := float64(rw) / float64(rh)
	got := float64(w) / float64(h)
	if want <= 0 || got <= 0 {
		return false
	}
	return math.Abs(got-want)/want <= ratioSnapTolerance
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	if a < 0 {
		return -a
	}
	return a
}

// upstreamSpecMaxLen 是 usages.upstream_spec 的列宽。
//
// 落库前必须自己判长度：Postgres 在 varchar(16) 上遇到更长串会直接报
// "value too long for type character varying(16)"，而这条 UPDATE 在结算事务里，
// 一次失败就让整单停在 pending（钱冻着、图已交付）。上限必须与 `usages.image_spec`
// 的列宽（32）一致：档位键上限就是 32 个字符，这里若更小，运营配的长档位名
// 会被判成「无法解析」，上游交付档就再也补收不上来。
const upstreamSpecMaxLen = 32

// normalizeUpstreamSpec 校验上游回报的尺寸串：超长或无法解析的一律丢弃并留痕。
//
// 丢弃而不是截断：截断后的 "2048x11" 会被当成合法尺寸参与比对，比没有更糟。
func normalizeUpstreamSpec(raw string) (string, []string) {
	s := normalizeSizeText(raw)
	if s == "" {
		return "", nil
	}
	if len(s) > upstreamSpecMaxLen {
		return "", []string{model.AnomalyUpstreamSpecUnparsable}
	}
	if _, ok := asTierKey(s); ok {
		return s, nil
	}
	if _, _, ok := asPixels(s); ok {
		return s, nil
	}
	if _, _, ok := asRatio(s); ok {
		return s, nil
	}
	if strings.EqualFold(s, "auto") {
		return "auto", nil
	}
	return "", []string{model.AnomalyUpstreamSpecUnparsable}
}

// upstreamTier 把上游回报的尺寸换算成**该模型**的档位（与计费侧同一套吸附规则）。
func upstreamTier(sp model.SpecPrices, raw string) (string, bool) {
	s := normalizeSizeText(raw)
	if s == "" {
		return "", false
	}
	if t, ok := asTierKey(s); ok {
		return t, true
	}
	w, h, ok := asPixels(s)
	if !ok {
		return "", false
	}
	t := nearestTier(sp, int64(w)*int64(h))
	return t, t != ""
}

// upstreamRatio 取上游尺寸对应的画幅（无法判定时返回空）。
func upstreamRatio(raw string) string {
	s := normalizeSizeText(raw)
	if w, h, ok := asPixels(s); ok {
		return NormalizeRatio(w, h)
	}
	if w, h, ok := asRatio(s); ok && w > 0 && h > 0 {
		g := gcd(w, h)
		return fmt.Sprintf("%d:%d", w/g, h/g)
	}
	return ""
}

// ratioClose 判断两个比例是否在容差内一致（避免 16:9 与 1.78:1 这类同义写法误报）。
func ratioClose(a, b string) bool {
	aw, ah, ok1 := asRatio(a)
	bw, bh, ok2 := asRatio(b)
	if !ok1 || !ok2 {
		return true
	}
	x := float64(aw) / float64(ah)
	y := float64(bw) / float64(bh)
	if x <= 0 || y <= 0 {
		return true
	}
	return math.Abs(x-y)/y <= ratioSnapTolerance
}
