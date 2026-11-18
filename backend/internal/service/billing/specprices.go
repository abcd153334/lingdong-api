package billing

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/money"
)

// MaxPriceSeconds 是秒数类计费维度的上限（max_seconds 与 long_after_seconds 共用）。
//
// 导出：保存期校验、schema 目录下发的 limits、前端表单提示必须同一份数字。
// 各自写一份的结果是「前端提示 1–3600、后端拒绝 3600」这类自相矛盾。
const MaxPriceSeconds = 3600

// MaxLongVideoMultiplier 是长视频溢价倍率的上界（10×，单位：微比例）。
//
// 为什么要上界：倍率只乘时长费，而时长本身有上限（max_seconds），两者相乘就是
// 单条视频的**最坏金额**——它同时也是结算期的金额上界（`ChargeCeiling`）。
// 没有上界时，运营把 10 填成 2000000（把"倍"写成"微比例"）会让
// 最坏金额直接冲到单次上限，且保存期没有任何提示。
//
// 与前端 `specForm.ts` 的倍数输入上限 10 必须同值：两边各写一份的结果是
// 「前端拦在 10、后端放过 100」。
const MaxLongVideoMultiplier = 10

// specPriceFields 缓存 model.SpecPrices 的 json 键集（首调用时从反射生成）。
//
// 键集的唯一来源就是 model.SpecPrices 的 json tag —— 不在这里另抄一份清单。
var specPriceFields = func() map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeOf(model.SpecPrices{})
	for i := 0; i < t.NumField(); i++ {
		name := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		out[name] = true
	}
	return out
}()

// SpecPriceFields 返回 spec_prices 的合法键（按字典序，供 schema 目录与测试断言）。
func SpecPriceFields() []string {
	out := make([]string, 0, len(specPriceFields))
	for k := range specPriceFields {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ImageTierKeys / VideoTierKeys 是档位**建议词表**：前端预填顺序、价目与文档的展示顺序。
//
// 它们不参与任何校验。档位是运营在 spec_prices 里自由配置的键
// （`1K` / `2.5K` / `8K` / `480p` / `4x`…），某个模型支持哪些档由厂商官方文档决定，
// 平台既不做能力白名单，也不在保存期按「上游能不能出」拒绝配置。
// 保留词表只为两件事：新建模型时前端有一份行业常用档位可预填；展示时按清晰度排序。
var (
	// ImageTierKeys 按清晰度升序（大写 K）。
	//
	// 1.5K 是正式档位而不是 1K 的别名：官方《图片生成 API》里 Seedream 5.0 pro/flash
	// 的可选值就是 1K / 1.5K / 2K，且明确「1.5K 与 1K 同价、出图效果更优」。
	ImageTierKeys = []string{"1K", "1.5K", "2K", "3K", "4K"}
	// VideoTierKeys 按清晰度升序（小写）。
	VideoTierKeys = []string{"480p", "720p", "1080p", "2k", "4k"}
)

// videoTierOrder 给视频档位一个排序权重，只用于展示顺序，不参与校验。
//
// `480p`→480、`1080p`→1080、`2k`→2000、`4k`→4000；认不出的私有写法一律 0（排最前）。
// 用规则而不是查词表：运营可以配 `1440p` / `8k` 这类词表外的档位，查表会让它们全挤在 0。
func videoTierOrder(tier string) int {
	t := strings.ToLower(strings.TrimSpace(tier))
	i := 0
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0
	}
	n, err := strconv.Atoi(t[:i])
	if err != nil {
		return 0
	}
	if strings.HasSuffix(t, "k") {
		return n * 1000
	}
	return n
}

// checkTierKeys 校验价目档位键的**可读性**：非空、≤32 字符、不含逗号、单价非负且在范围内。
//
// 只约束「能不能落库、能不能被展示」，不判定「上游能不能出这个档」——
// 那是厂商官方文档的事实，代码替它判定的结果只会在上游扩容时误杀可用组合。
func checkTierKeys(field string, m map[string]int64) error {
	for k, v := range m {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("%s：档位键不能为空", field)
		}
		if len([]rune(k)) > 32 {
			return fmt.Errorf("%s：档位 %q 过长（上限 32 个字符）", field, k)
		}
		if strings.Contains(k, ",") {
			return fmt.Errorf("%s：档位 %q 不能包含逗号（档位以逗号分隔落库）", field, k)
		}
		if v < 0 {
			return fmt.Errorf("%s（%s 档）不能为负数", field, k)
		}
		if err := money.Check(v); err != nil {
			return fmt.Errorf("%s（%s 档）超出范围", field, k)
		}
	}
	return nil
}

// NormalizeSpecPrices 归一档位键与固定档位的大小写（**保存期落库用**）。
//
// 只改键名/大小写、不动值，也不补齐缺失字段（缺失与 0 的语义不同，
// 补 null 会把「未配置」变成「配置为空」）。规则：
//   - image：TierPrice 的键统一大写 → 1K/2K/4K；
//   - video：SecondPrice / RefVideoSecondPrice 统一小写 → 480p/720p/1080p/2k；
//     ResolutionFixed 统一小写（旧实现不归一，`"1080P"` 过校验但运行期永不命中）；
//   - 归一后撞键直接报错（`2K` 与 `2k` 同时存在时无法判断哪个才是运营的本意）。
func NormalizeSpecPrices(modality string, sp model.SpecPrices) (model.SpecPrices, error) {
	switch modality {
	case "image":
		m, err := normalizeTierMap(sp.TierPrice, strings.ToUpper)
		if err != nil {
			return sp, fmt.Errorf("分辨率档位: %w", err)
		}
		sp.TierPrice = m
	case "video":
		m, err := normalizeTierMap(sp.SecondPrice, strings.ToLower)
		if err != nil {
			return sp, fmt.Errorf("每秒单价档位: %w", err)
		}
		sp.SecondPrice = m
		rm, err := normalizeTierMap(sp.RefVideoSecondPrice, strings.ToLower)
		if err != nil {
			return sp, fmt.Errorf("参考视频每秒单价档位: %w", err)
		}
		sp.RefVideoSecondPrice = rm
		sp.ResolutionFixed = strings.ToLower(strings.TrimSpace(sp.ResolutionFixed))
	default:
		sp.ResolutionFixed = strings.ToLower(strings.TrimSpace(sp.ResolutionFixed))
	}
	return sp, nil
}

// normalizeLowerSet 把字符串集合统一小写、去空白、去重（保序）。
//
// 与 normalizeTierMap 同一口径：档位键与子类型都是「枚举」，大小写混写会让
// 同一份配置在不同判定点给出两种答案。
func normalizeLowerSet(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, v := range in {
		k := strings.ToLower(strings.TrimSpace(v))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

func normalizeTierMap(in map[string]int64, conv func(string) string) (map[string]int64, error) {
	if len(in) == 0 {
		return in, nil
	}
	out := make(map[string]int64, len(in))
	for k, v := range in {
		nk := conv(strings.TrimSpace(k))
		if _, dup := out[nk]; dup {
			return nil, fmt.Errorf("档位 %q 归一化后与其它键重复，请删除多余的那个", k)
		}
		out[nk] = v
	}
	return out, nil
}

// ValidateSpecPrices 校验「模态 × 计价格」的合法组合，并返回**非阻断告警**。
//
// 入参必须是已归一化的结构（NormalizeSpecPrices 的输出）：档位键的大小写在这里
// 是硬判据，拿原始输入校验会得到「2k 不合法」这种对的结论 + 错的理由。
//
// 为什么返回 warnings 而不是全部硬拒：只配了图片档位价的模型是完全合法的，
// 但它的参考图/参考音频会按免费计费 —— 那是运营必须知道、但不该被拦下的事实。
// 硬拒会让"先配基础价、再补素材价"这种正常节奏无法进行。
func ValidateSpecPrices(modality string, sp model.SpecPrices) ([]string, error) {
	warnings, err := validateSpecPrices(modality, sp)
	if err != nil {
		return nil, err
	}
	// 量级告警统一在出口追加：每个分支各写一次必然漏（本次就有三个分支漏了）。
	return append(warnings, priceMagnitudeWarnings(sp)...), nil
}

// priceMagnitudeWarnings 对「不像一个正常价格」的金额给出非阻断提示。
//
// 唯一的真实场景是**单位错**：价目在 API/规格 DSL 里以「微」计价（1 元 = 1e6 微），
// 而人（以及 LLM）习惯按「元」写。填 2 想表达 2 元，实际入库 0.000002 元，
// 结算时不会报错、只会让这个模型的收入静默归零。
//
// 阈值取 0.001 元与 1000 元：规格计价的三个维度（每张图 / 每秒 / 每次任务）
// 落在这个区间之外都极少见，命中时提示一句让人确认，而不是拒绝保存。
func priceMagnitudeWarnings(sp model.SpecPrices) []string {
	var out []string
	const (
		tinyMicro = 1000               // 0.001 元
		hugeMicro = 1000 * money.Scale // 1000 元
	)
	check := func(label string, v int64) {
		if v <= 0 {
			return // 0 = 显式免费，是合法配置
		}
		switch {
		case v < tinyMicro:
			out = append(out, fmt.Sprintf(
				"「%s」为 %s 元，疑似把「元」当成「微」填了（2 元应填 2000000）",
				label, money.Format(v, 6)))
		case v > hugeMicro:
			out = append(out, fmt.Sprintf(
				"「%s」为 %s 元，单次调用金额偏高，请确认单位与档位", label, money.Format(v, 2)))
		}
	}
	for k, v := range sp.TierPrice {
		check(k+" 档单价", v)
	}
	for k, v := range sp.SecondPrice {
		check(k+" 每秒单价", v)
	}
	for k, v := range sp.RefVideoSecondPrice {
		check(k+" 参考视频每秒单价", v)
	}
	for _, f := range []struct {
		label string
		v     *int64
	}{
		{"每次任务价", sp.PerTaskPrice},
		{"每秒单价", sp.PerSecondPrice},
		{"最低消费", sp.MinPrice},
		{"参考图单价", sp.RefImagePrice},
		{"参考音频单价", sp.RefAudioPrice},
		{"参考视频单价", sp.RefVideoPrice},
	} {
		if f.v != nil {
			check(f.label, *f.v)
		}
	}
	return out
}

func validateSpecPrices(modality string, sp model.SpecPrices) (warnings []string, err error) {
	switch modality {
	case "video":
		if sp.Kind != SpecKindPerVideo {
			return nil, errors.New("视频模型的计价格 kind 必须是 per_video")
		}
		if len(sp.SecondPrice) == 0 {
			return nil, errors.New("视频模型必须至少配置一个档位的每秒单价（配了哪些档就代表开放哪些档）")
		}
		// 档位不再是白名单：配了哪些档就代表开放哪些档，具体支持范围以官方文档为准。
		if err := checkTierKeys("每秒单价", sp.SecondPrice); err != nil {
			return nil, err
		}
		if err := checkTierKeys("参考视频每秒单价", sp.RefVideoSecondPrice); err != nil {
			return nil, err
		}
		if sp.RefVideoPrice != nil && len(sp.RefVideoSecondPrice) > 0 {
			return nil, errors.New("参考视频的「按条」与「按秒」只能选一种，请清空其中一个")
		}
		if err := checkNonNeg("参考图单价", sp.RefImagePrice); err != nil {
			return nil, err
		}
		if err := checkNonNeg("参考音频单价", sp.RefAudioPrice); err != nil {
			return nil, err
		}
		if err := checkNonNeg("参考视频单价", sp.RefVideoPrice); err != nil {
			return nil, err
		}
		// 免费张数是**数量**，必须有上界：它只表示"前几张不免"，不是金额。
		// 没有上界时，前端一次单位错（2 被当成 2 元写成 2000000）就能静默把
		// 素材费变成永久 0 —— 不报错、看起来"配过了"，月底对账才会发现整体少收。
		if sp.RefImageFree != nil {
			if *sp.RefImageFree < 0 {
				return nil, errors.New("参考图免费张数不能为负数（0 表示不免费，要全部免费请把单价填 0）")
			}
			if *sp.RefImageFree > MaxImageN {
				return nil, fmt.Errorf("参考图免费张数必须在 0–%d 之间", MaxImageN)
			}
		}
		// "免 N 张"但没有单价：这 N 张本来就不收钱，配置等于无效。
		// 不拦（先配免费额度、后补价是正常节奏），但必须让运营看到。
		if sp.RefImageFree != nil && *sp.RefImageFree > 0 && sp.RefImagePrice == nil {
			warnings = append(warnings, "配了参考图免费张数但没有参考图单价，免费额度不会生效")
		}
		if sp.MaxSeconds != nil && (*sp.MaxSeconds < 1 || *sp.MaxSeconds > MaxPriceSeconds) {
			return nil, fmt.Errorf("最大秒数必须在 1–%d 之间", MaxPriceSeconds)
		}
		// 必填：它是「单条视频最多收多少」的唯一依据，也是结算期的金额上限
		// （`ChargeCeiling`）。缺了它，结算上限只能退回 3×冻结额 ——
		// 冻结估算偏低时合法金额会被削掉（智能时长、长视频尤其明显）。
		if sp.MaxSeconds == nil {
			return nil, errors.New("视频模型必须配置最大秒数（决定单条视频的最坏金额与结算上限）")
		}
		// 超时阈值与倍率必须成对：只填一个会静默不生效，运营会以为在收双倍。
		if (sp.LongAfterSeconds == nil) != (sp.LongVideoMultiplier == nil) {
			return nil, errors.New("超时阈值与超时倍率必须同时填写（只填一个不会生效）")
		}
		if sp.LongAfterSeconds != nil {
			if *sp.LongAfterSeconds < 1 || *sp.LongAfterSeconds > MaxPriceSeconds {
				return nil, fmt.Errorf("超时阈值必须在 1–%d 秒之间", MaxPriceSeconds)
			}
			if *sp.LongVideoMultiplier <= money.Scale {
				return nil, errors.New("超时倍率必须大于 1（等于 1 表示不加价，请留空）")
			}
			if *sp.LongVideoMultiplier > money.Scale*MaxLongVideoMultiplier {
				return nil, fmt.Errorf("超时倍率不能超过 %d 倍（当前 %.1f 倍）",
					MaxLongVideoMultiplier, float64(*sp.LongVideoMultiplier)/float64(money.Scale))
			}
		}
		// 固定档位：上游没有 resolution 参数时（画质由型号决定）用它兜底计价。
		if tier := strings.TrimSpace(sp.ResolutionFixed); tier != "" {
			// 只校验「固定档与自己的秒价表自洽」——否则不传分辨率的请求无法计价。
			// 这个档上游能不能出，由厂商官方文档决定，代码不做能力判定。
			if _, ok := sp.SecondPrice[tier]; !ok {
				return nil, fmt.Errorf("固定档位 %q 没有配置对应的每秒单价，不传分辨率的请求会无法计价", tier)
			}
		}
		// 非阻断告警：素材价未填 = 免费。
		if sp.RefImagePrice == nil {
			warnings = append(warnings, "未填参考图单价，参考图将按免费计费")
		}
		if sp.RefAudioPrice == nil {
			warnings = append(warnings, "未填参考音频单价，参考音频将按免费计费")
		}
		if sp.RefVideoPrice == nil && len(sp.RefVideoSecondPrice) == 0 {
			warnings = append(warnings, "未填参考视频单价，参考视频将按免费计费")
		}
		// 按秒且只配了部分档：未列出的档位**参考视频免费**（运行期口径，见
		// refVideoSecondPriceAt）。这里不阻断，但必须点名缺哪几个档 ——
		// 否则"720p 有价、1080p 悄悄免费"只有等月底对账才发现。
		if len(sp.RefVideoSecondPrice) > 0 {
			missing := make([]string, 0, len(sp.SecondPrice))
			for _, tier := range sortedVideoTiers(sp.SecondPrice) {
				if _, ok := sp.RefVideoSecondPrice[tier]; !ok {
					missing = append(missing, tier)
				}
			}
			if len(missing) > 0 {
				warnings = append(warnings, fmt.Sprintf(
					"参考视频秒价未覆盖档位 %s：这些档位的参考视频将按免费计费",
					strings.Join(missing, " / ")))
			}
		}
		if err := rejectForeign(sp, SpecKindPerVideo, "tier_price", "max_n",
			"per_task_price", "per_clip_price", "per_second_price", "min_price"); err != nil {
			return nil, err
		}
		return warnings, nil

	case "music":
		// 音乐/歌词按**实际交付条数**计价（per_clip）。per_task 只留存量兼容：
		// 已停用的 chirp-v4 与在途订单都还是那个口径，硬禁会让它们无法再保存。
		switch sp.Kind {
		case SpecKindPerClip:
			if sp.PerClipPrice == nil {
				return nil, errors.New("音乐模型按条计费时必须配置每条单价（per_clip_price）")
			}
			if *sp.PerClipPrice < 0 {
				return nil, errors.New("每条单价不能为负数")
			}
			if err := money.Check(*sp.PerClipPrice); err != nil {
				return nil, errors.New("每条单价超出范围")
			}
			// max_n 在 per_clip 下是「单次任务的计费条数上限」：SUNO 出 2 首、
			// 歌词 1 篇。不配就按 2 条冻结，配了必须在 [1, MaxMusicClips]。
			// 上限不能省：配成 999 会让一次冻结把用户余额整段占住。
			if sp.MaxN != nil {
				if *sp.MaxN < 1 || *sp.MaxN > MaxMusicClips {
					return nil, fmt.Errorf("单次计费条数上限必须在 1–%d 之间（当前 %d）", MaxMusicClips, *sp.MaxN)
				}
			}
			if err := rejectForeign(sp, SpecKindPerClip,
				"tier_price", "second_price", "per_second_price", "per_task_price", "min_price",
				"resolution_fixed", "max_seconds", "allow_auto_duration", "long_after_seconds",
				"long_video_multiplier", "ref_image_price", "ref_image_free", "ref_audio_price",
				"ref_video_price", "ref_video_second_price"); err != nil {
				return nil, err
			}
			warnings = append(warnings,
				"按条计费会在结算时用 per_clip_price 乘以实际交付条数；0 条交付免单（全额退回）。")
			return warnings, nil
		case SpecKindPerTask:
			if sp.PerTaskPrice == nil {
				return nil, errors.New("音乐模型必须配置每次任务固定价（per_task_price）或按条单价（per_clip_price）")
			}
			if *sp.PerTaskPrice < 0 {
				return nil, errors.New("每次任务固定价不能为负数")
			}
			if err := money.Check(*sp.PerTaskPrice); err != nil {
				return nil, errors.New("每次任务固定价超出范围")
			}
			if err := rejectForeign(sp, SpecKindPerTask, "tier_price", "max_n", "per_clip_price",
				"second_price", "per_second_price", "min_price", "resolution_fixed", "max_seconds",
				"allow_auto_duration", "long_after_seconds",
				"long_video_multiplier", "ref_image_price", "ref_image_free", "ref_audio_price",
				"ref_video_price", "ref_video_second_price"); err != nil {
				return nil, err
			}
			warnings = append(warnings,
				"per_task 是音乐的存量口径（一次任务一个价，与交付条数无关）；建议改为 per_clip，按实际交付条数计费。")
			return warnings, nil
		default:
			return nil, fmt.Errorf("音乐模型的计价格 kind 必须是 per_clip（按条）或 per_task（存量兼容），当前 %q", sp.Kind)
		}

	case "audio":
		// 音频按秒：单价 × 产物实际秒数，再抬到最低消费。
		//
		// 为什么不再要求"只能按次"：按次的模型遇到长短文本混用时无法定价 ——
		// 5 秒广告语和 10 分钟有声书同价，要么贵得没人用，要么长文本每单亏。
		// 时长来源必须真实（桥接 extract.duration 或音频头探测），见 relay 侧。
		if sp.Kind != SpecKindPerSecond {
			return nil, errors.New("音频模型的计价格 kind 必须是 per_second（按秒计价）")
		}
		if sp.PerSecondPrice == nil {
			return nil, errors.New("音频模型必须配置每秒单价")
		}
		if *sp.PerSecondPrice < 0 {
			return nil, errors.New("每秒单价不能为负数")
		}
		if err := money.Check(*sp.PerSecondPrice); err != nil {
			return nil, errors.New("每秒单价超出范围")
		}
		if sp.MinPrice != nil {
			if *sp.MinPrice < 0 {
				return nil, errors.New("最低消费不能为负数")
			}
			if err := money.Check(*sp.MinPrice); err != nil {
				return nil, errors.New("最低消费超出范围")
			}
		}
		if sp.MaxSeconds != nil && (*sp.MaxSeconds < 1 || *sp.MaxSeconds > MaxPriceSeconds) {
			return nil, fmt.Errorf("最大秒数必须在 1–%d 之间", MaxPriceSeconds)
		}
		// 必填：同视频——它是单次音频的最坏金额，也是结算上限。
		// ASR 的时长在冻结期根本拿不到，没有这条上限就只能被「零冻结兜底」截顶。
		if sp.MaxSeconds == nil {
			return nil, errors.New("音频模型必须配置最大秒数（决定单次音频的最坏金额与结算上限）")
		}
		if err := rejectForeign(sp, SpecKindPerSecond, "tier_price", "max_n", "per_clip_price",
			"second_price", "per_task_price", "resolution_fixed", "allow_auto_duration",
			"long_after_seconds", "long_video_multiplier", "ref_image_price", "ref_image_free",
			"ref_audio_price", "ref_video_price", "ref_video_second_price"); err != nil {
			return nil, err
		}
		return nil, nil

	case "image":
		if sp.Kind != SpecKindPerImage {
			return nil, errors.New("图片模型的计价格 kind 必须是 per_image")
		}
		if len(sp.TierPrice) == 0 {
			return nil, errors.New("图片模型必须至少配置一个分辨率档位")
		}
		if err := checkTierKeys("分辨率档位", sp.TierPrice); err != nil {
			return nil, err
		}
		if sp.MaxN != nil && (*sp.MaxN < 1 || *sp.MaxN > MaxImageN) {
			return nil, fmt.Errorf("单次张数上限必须在 1–%d 之间", MaxImageN)
		}
		// 必填：它是「一次调用最多收多少张的钱」的唯一依据，也是结算上限。
		// 缺了它，上游多出图（组图 / 重试）会直接穿透冻结额，然后被 3× 截顶少收。
		if sp.MaxN == nil {
			return nil, errors.New("图片模型必须配置单次张数上限（决定单次生成的最坏金额与结算上限）")
		}
		// 默认档（resolution_fixed）是「不传 size / auto / 只传比例」时的计费档。
		//
		// 非空时必须命中自己的 tier_price —— 这是**硬错**，理由与"上游能力"无关：
		//   ① 它是配置引用自己的价目表，属于内部自洽，不存在"厂商扩容后误杀"；
		//   ② 留空是允许的（先配价、后定默认档），但运行期不再兜底：
		//      不传尺寸的请求会报「无法确定计费档位」，硬错拦的是配置而不是请求；
		//   ③ 前端是下拉选择，唯一能填错的路径是手写 JSON / 脚本。
		// 反过来若指向价目外的键还放行，线上就会出现「默认档写 2K、价目没有 2K」的模型，
		// 每笔不传尺寸的请求都失败 —— 而运营以为它只是"走默认档"。
		if t := strings.TrimSpace(sp.ResolutionFixed); t != "" {
			if tierKeyInMarket(t, ImageTierMap(sp)) == "" {
				return nil, fmt.Errorf("默认档 %q 不在已配置的分辨率档位里（当前：%s）",
					t, strings.Join(sortedImageTiers(ImageTierMap(sp)), " / "))
			}
		} else if len(sp.TierPrice) > 1 {
			// 不阻断保存（运营可能先配价后补默认档），但必须让他在保存时就看见后果：
			// 运行期不再有「取最低档」的兜底，不传 size 的请求会因为「无档可计」被拒绝。
			warnings = append(warnings, "未配置默认档：不传 size / auto 的请求会被拒绝，建议在价目里指定默认档")
		}
		// resolution_fixed 是图片与视频**共用**的「默认档」字段（图片=不传 size 时按它计费，
		// 视频=型号固定档）。不再另立 default_tier：一个概念两个字段，必然有一天两边不一致。
		if err := rejectForeign(sp, SpecKindPerImage, "second_price", "per_second_price", "per_task_price", "per_clip_price",
			"max_seconds", "allow_auto_duration", "long_after_seconds", "long_video_multiplier",
			"min_price", "ref_image_price", "ref_image_free", "ref_audio_price", "ref_video_price",
			"ref_video_second_price"); err != nil {
			return nil, err
		}
		// 必须回 warnings 而不是 nil：命名返回值被 nil 覆盖 = 上面收集的告警全部丢失，
		// 于是「兜底价越界」这条唯一的提示永远到不了运营眼前（不报错也不提示）。
		return warnings, nil

	default:
		// fail-closed：其它模态（audio / chat / embedding / rerank / moderation）不支持按规格计价。
		// 旧实现把未知模态按图片规则校验（fail-open），配错方向也不报错。
		return nil, errors.New("该模态不支持按规格计价，请改用按 token 或按次计价")
	}
}

// checkNonNeg 校验可选金额字段：nil 放过，负值报错，越界报错。
func checkNonNeg(label string, v *int64) error {
	if v == nil {
		return nil
	}
	if *v < 0 {
		return fmt.Errorf("%s不能为负数", label)
	}
	if err := money.Check(*v); err != nil {
		return fmt.Errorf("%s超出范围", label)
	}
	return nil
}

// rejectForeign 拒绝与该 kind 无关的价目键。
//
// 为什么不是"忽略即可"：`second_price` 配在图片模型上是**意图错误**
// （运营以为按秒计价生效了），静默忽略会让他在计费结果里发现金额对不上却找不到原因。
func rejectForeign(sp model.SpecPrices, kind string, forbidden ...string) error {
	for _, key := range forbidden {
		if !specFieldSet(sp, key) {
			continue
		}
		return fmt.Errorf("kind=%s 不支持字段 %q，请删除它（或者改用匹配的 kind）", kind, key)
	}
	return nil
}

// specFieldSet 判断某个价目键在该结构里是否"有值"。
func specFieldSet(sp model.SpecPrices, key string) bool {
	switch key {
	case "kind":
		return sp.Kind != ""
	case "tier_price":
		return len(sp.TierPrice) > 0
	case "max_n":
		return sp.MaxN != nil
	case "second_price":
		return len(sp.SecondPrice) > 0
	case "max_seconds":
		return sp.MaxSeconds != nil
	case "allow_auto_duration":
		return sp.AllowAutoDuration != nil
	case "resolution_fixed":
		return strings.TrimSpace(sp.ResolutionFixed) != ""
	case "long_after_seconds":
		return sp.LongAfterSeconds != nil
	case "long_video_multiplier":
		return sp.LongVideoMultiplier != nil
	case "ref_image_price":
		return sp.RefImagePrice != nil
	case "ref_image_free":
		return sp.RefImageFree != nil
	case "ref_audio_price":
		return sp.RefAudioPrice != nil
	case "ref_video_price":
		return sp.RefVideoPrice != nil
	case "ref_video_second_price":
		return len(sp.RefVideoSecondPrice) > 0
	case "per_task_price":
		return sp.PerTaskPrice != nil
	case "per_clip_price":
		return sp.PerClipPrice != nil
	case "per_second_price":
		return sp.PerSecondPrice != nil
	case "min_price":
		return sp.MinPrice != nil
	default:
		return false
	}
}

// ImageTierOfSize / tier 归一已整体搬到 imagespec.go（唯一实现）。
//
// 旧实现按**长边**分档（≤1536→1K、≤2560→2K、其余 4K），与上游「档位 = 总像素」
// 的定义冲突：1K 的 21:9（1568×672）判成 2K、2K 的 16:9（2816×1584）判成 4K，
// 而 1.5K/3K 在数学上永远推不出来。现在按「该模型已配档位 + 总面积就近吸附」。
