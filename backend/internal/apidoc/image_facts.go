package apidoc

import (
	"fmt"
	"sort"
	"strings"
)

// 图像 prompt 的字符上限（官方文档原话，平台不放大也不缩小）。
//
// 必须与适配器共用同一个常量：曾经适配器按 OpenAI 契约放行 32000、
// 页面写 5000 —— 代码比文档更宽松同样是「文档不准」，而且用户拿到的是
// 一句看不出处方的上游原话。nano 与 gpt-image 是两个协议，上限也不同。
const (
	GPTImagePromptMaxRunes = 5000
	NanoPromptMaxRunes     = 4000
)

// GPTImageTierPixels 是「平台档位词 + 画幅 → 上游像素」的**唯一**事实来源。
//
// 为什么放在 apidoc 而不是适配器里：
//
//	这张表同时被两个出口消费 —— 出站翻译（适配器把 `2K` 换成像素才能让上游按档出图）
//	和对外文档（页面上那张「比例换算」表）。写在适配器里，文档只能手抄一遍，
//	手抄就会漂移：2026-11 之前文档里挂着 gpt-image-2 的另一套像素
//	（16:9 → 1672x941），而代码从来不发它 —— 读者按文档算像素、按代码出图，对不上。
//
// 取值口径（逐格照抄官方《生成图片》的「比例参考」，不推导）：
//   - 行 = 画幅，列 = 档位；**缺列代表上游没有这一档**（1:3 / 3:1 只有 2K 与 4K）。
//   - 像素是"要发出去的宽度x高度"，与出图的实际尺寸一致（2026-11-16 真机实测：
//     发 2048x688，回来的成片就是 2048x688；发 2048x2048，成片 2048x2048）。
var GPTImageTierPixels = map[string]map[string]string{
	"1:1":  {"1K": "1024x1024", "2K": "2048x2048", "4K": "2880x2880"},
	"16:9": {"1K": "1280x720", "2K": "2048x1152", "4K": "3840x2160"},
	"9:16": {"1K": "720x1280", "2K": "1152x2048", "4K": "2160x3840"},
	"4:3":  {"1K": "1152x864", "2K": "2304x1728", "4K": "3264x2448"},
	"3:4":  {"1K": "864x1152", "2K": "1728x2304", "4K": "2448x3264"},
	"3:2":  {"1K": "1536x1024", "2K": "2048x1360", "4K": "3504x2336"},
	"2:3":  {"1K": "1024x1536", "2K": "1360x2048", "4K": "2336x3504"},
	"5:4":  {"1K": "1120x896", "2K": "2240x1792", "4K": "3200x2560"},
	"4:5":  {"1K": "896x1120", "2K": "1792x2240", "4K": "2560x3200"},
	"21:9": {"1K": "1456x624", "2K": "2912x1248", "4K": "3840x1648"},
	"9:21": {"1K": "624x1456", "2K": "1248x2912", "4K": "1648x3840"},
	// 1:3 / 3:1 官方只给两个值，缺 1K 一列：按长边归到 2K / 4K。
	// 缺列就是缺列 —— 拼一个 1K 像素出来等于「按 1K 收钱、按猜的尺寸出图」。
	"1:3": {"2K": "688x2048", "4K": "1280x3840"},
	"3:1": {"2K": "2048x688", "4K": "3840x1280"},
	"2:1": {"1K": "1536x768", "2K": "3072x1536", "4K": "3840x1920"},
	"1:2": {"1K": "768x1536", "2K": "1536x3072", "4K": "1920x3840"},
}

// GPTImagePixelFor 取「档位 + 画幅」对应的像素。
//
// 三种失败必须能被调用方区分（它要给出不同的 400 文案）：
//   - ratio 为空：调用方该先补默认画幅（1:1），不是错误；
//   - 画幅不认识：告诉用户支持哪些画幅；
//   - 画幅认识但这一档没有：告诉用户这个画幅支持哪些档位。
func GPTImagePixelFor(tier, ratio string) (pixel string, supportedTiers []string, ok bool) {
	cols, hit := GPTImageTierPixels[strings.TrimSpace(ratio)]
	if !hit {
		return "", nil, false
	}
	if px, has := cols[strings.ToUpper(strings.TrimSpace(tier))]; has {
		return px, nil, true
	}
	tiers := make([]string, 0, len(cols))
	for t := range cols {
		tiers = append(tiers, t)
	}
	sort.Slice(tiers, func(i, j int) bool { return tierRank(tiers[i]) < tierRank(tiers[j]) })
	return "", tiers, false
}

// GPTImageSizeProblem 回答「这个尺寸组合能不能换算成像素」，返回人话原因（空串 = 可以）。
//
// 这是**建单前**的判据：适配器在出站构造时也会用同一张表拒绝一次，但那时任务行
// 与冻结已经产生 —— 客户端拿到 400 的同时，自己的任务列表里会多出一条 failed 记录，
// 钱虽然退了，用户却会问"我明明被拒了，为什么还有单"。
//
// 只对 gpt-image 族生效：这些族的 size 必须由平台换成像素（模型提供方只收宽高）。
// 其余族（Seedream / Nano）的 size 是厂商原生字段，原样透传，平台不替它判合法性。
//
// tier 为空（用户直接写了像素 / auto / 比例）时一律放行：那不是"档位 + 画幅"的组合。
func GPTImageSizeProblem(modelID, tier, ratio string) string {
	f, ok := FamilyDocOfModelID(modelID)
	if !ok || f.Key != FamilyImagesGptImage {
		return ""
	}
	tier = strings.ToUpper(strings.TrimSpace(tier))
	if tier == "" {
		return ""
	}
	ratio = strings.TrimSpace(ratio)
	if ratio == "" {
		ratio = "1:1"
	}
	if _, tiers, ok := GPTImagePixelFor(tier, ratio); !ok {
		if len(tiers) > 0 {
			return fmt.Sprintf("画幅 %s 没有 %s 档：该画幅只提供 %s，请改档位或换画幅",
				ratio, tier, strings.Join(tiers, " / "))
		}
		return fmt.Sprintf("画幅 %s 不在支持列表里：可用画幅为 %s（也可以直接写像素，如 2048x1152）",
			ratio, strings.Join(GPTImageRatios(), "、"))
	}
	return ""
}

// GPTImageRatios 返回支持的画幅（按面积档位与常见度排序前的固定顺序，供文档与报错共用）。
func GPTImageRatios() []string {
	out := make([]string, 0, len(GPTImageTierPixels))
	for r := range GPTImageTierPixels {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		// 先按"竖/横/方"粗排，再按数值：文档表格读起来才连贯。
		return ratioSortKey(out[i]) < ratioSortKey(out[j])
	})
	return out
}

func ratioSortKey(r string) string {
	parts := strings.SplitN(r, ":", 2)
	if len(parts) != 2 {
		return "9" + r
	}
	return fmt.Sprintf("%03s:%03s", parts[0], parts[1])
}

// tierRank 只用于把 1K / 2K / 4K 这类档位按数值排好（未知档排最后）。
func tierRank(t string) float64 {
	n, ok := parseTierNumber(t)
	if !ok {
		return 1e9
	}
	return n
}

func parseTierNumber(t string) (float64, bool) {
	s := strings.ToUpper(strings.TrimSpace(t))
	if !strings.HasSuffix(s, "K") {
		return 0, false
	}
	var v float64
	var dot bool
	var scale float64 = 1
	for i := 0; i < len(s)-1; i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			if dot {
				scale /= 10
				v += float64(c-'0') * scale
			} else {
				v = v*10 + float64(c-'0')
			}
		case c == '.' && !dot:
			dot = true
		default:
			return 0, false
		}
	}
	return v, true
}

// GPTImageSizeNote 渲染「尺寸写法与换算」那段说明 —— 与 GPTImageTierPixels 同源。
//
// 文档里不再手写这张表：手写就会漂移（曾经挂着 gpt-image-2 的另一套像素，
// 而代码从不使用它）。表格由数据生成，改表就是改文档。
func GPTImageSizeNote() string {
	var b strings.Builder
	b.WriteString("写法三选一：比例（如 `16:9`）、像素（如 `1024x1024`）、`auto`。\n")
	b.WriteString("平台按下面这张表把「档位 + 画幅」换成像素，所以出图尺寸与计价档一一对应；")
	b.WriteString("直接写像素时不查表，按你写的发（总像素面积决定按哪一档计费）。\n")
	b.WriteString("所有 gpt-image 系列模型共用这一张表。\n")
	ratios := GPTImageRatios()
	for _, r := range ratios {
		cols := GPTImageTierPixels[r]
		cells := make([]string, 0, 3)
		for _, t := range []string{"1K", "2K", "4K"} {
			if px, ok := cols[t]; ok {
				cells = append(cells, px)
			} else {
				cells = append(cells, "—")
			}
		}
		b.WriteString(r + "　" + strings.Join(cells, " · ") + "\n")
	}
	b.WriteString("同一行依次是 1K / 2K / 4K；`—` 表示该档位不可用（1:3、3:1 只有 2K 与 4K）。\n")
	b.WriteString("自定义像素值的四条约束：最长边 ≤ `3840px`、两条边均为 `16` 的倍数、" +
		"长短边比 ≤ `3:1`、总像素 `655,360` – `8,294,400`。")
	return b.String()
}
