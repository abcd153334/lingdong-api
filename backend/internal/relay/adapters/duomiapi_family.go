package adapters

import (
	"fmt"
	"regexp"
	"strings"

	"lingdong-api/internal/relay"
)

// 本文件是「平台模型 ID → 子协议」的**兜底推断**，只服务一个目的：
// 厂商在原协议上升级型号（gpt-image-2.5-sunburst → 2.6-sunburst、kling-v3 → v4）时，
// 运营不必先发版或先配 model_overrides 才能用。
//
// 三条不可动摇的边界：
//  1. **只作用于平台模型 ID**（req.ModelID）。relay/adapter.go 明确要求路由决策不能
//     从绑定表的 upstream_model 反推 —— 运营把 upstream_model 改成别名时，协议不能跟着漂。
//  2. **只在精确表未命中时才用**（duomiModels 优先）。精确表承载「内置型号的 mode 与上游名」，
//     族表只是骨架，不能反过来覆盖它。
//  3. **推断不出来就报错，绝不猜**。中文 ID、无族前缀的自定义命名一律落「未知」，
//     让运营显式配 override —— 猜错协议等于用错的信封打上游，比报错难查得多。
type duomiFamily struct {
	// Name 是族名，只用于日志与文案（"kling.pro" / "gpt-image"）。
	Name string
	// Re 作用于 strings.ToLower(平台模型 ID)，必须 ^ 锚定。
	Re *regexp.Regexp
	// Protocol 是命中的子协议；Unsupported 非空时留空。
	Protocol string
	// Mode 只对可灵有效："" 表示不下发（上游把空串当非法枚举，直接 400）。
	Mode string
	// DurationAsString 为 true 时 duration 以字符串下发（上游按 string 校验的实现）。
	DurationAsString bool
	// Unsupported 非空 = 该族存在但本适配器尚未实现（C 类），只用于给出可执行的报错。
	Unsupported string
}

func mustRe(pattern string) *regexp.Regexp { return regexp.MustCompile(pattern) }

// duomiFamilies 的**顺序即优先级，首命中即返回**。
//
// 顺序不是随意排的：
//   - kling 的四条特化必须在 ^kling 兜底之前，否则 multi/omni 会被兜底吃掉（走错端点）；
//   - multi 在 pro/std 之前：多图参考端点本身决定画质，且保存期禁止 kling.multi 带 mode，
//     所以 "kling-v4-multi-pro" 这种冗余命名应归 multi、不下发 mode；
//   - suno.lyrics 必须在 suno.music 之前，否则 "suno-lyrics" 被音乐族抢走（会多发 model 字段被上游拒）。
var duomiFamilies = []duomiFamily{
	{Name: "kling.omni", Re: mustRe(`^kling.*[-_.]omni(?:[._-]|$)`), Protocol: duomiProtoKlingOmni, Mode: "std", DurationAsString: true},
	{Name: "kling.multi", Re: mustRe(`^kling.*[-_.]multi(?:[._-]|$)`), Protocol: duomiProtoKlingMulti, DurationAsString: true},
	{Name: "kling.std", Re: mustRe(`^kling.*[-_.]std(?:[._-]|$)`), Protocol: duomiProtoKling, Mode: "std"},
	{Name: "kling.pro", Re: mustRe(`^kling.*[-_.]pro(?:[._-]|$)`), Protocol: duomiProtoKling, Mode: "pro"},
	{Name: "kling", Re: mustRe(`^kling`), Protocol: duomiProtoKling},
	{Name: "seedance", Re: mustRe(`^(?:doubao-)?seedance`), Protocol: duomiProtoSeedance},
	{Name: "gpt-image", Re: mustRe(`^gpt-image`), Protocol: duomiProtoGPTImage},
	{Name: "nano", Re: mustRe(`^nano`), Protocol: duomiProtoNano},
	// Gemini 图像在文档里有两种叫法：gemini-3.1-flash-image-preview 与 gemini-nano-banana-2.1。
	// 后者既不含 image 也不以 nano 开头，只写 `.*image` 会把它判成未知模型。
	{Name: "gemini-image", Re: mustRe(`^gemini[-_.].*(?:image|nano|banana)`), Protocol: duomiProtoNano},
	{Name: "suno.lyrics", Re: mustRe(`^suno[-_.]?lyrics?(?:[._-]|$)`), Protocol: duomiProtoSunoLyrics},
	{Name: "suno.music", Re: mustRe(`^(?:chirp|suno)(?:[._-]|$)`), Protocol: duomiProtoSunoMusic},
	// muse 是多米自有视频协议，命名由厂商自己定（muse-video / 将来的 muse-video-2）。
	// 前缀足够稳定，放进来之后运营接同族新型号零配置。
	{Name: "muse", Re: mustRe(`^muse`), Protocol: duomiProtoMuse},

	// C 类：协议族存在、但本适配器没有对应实现。
	// 单独列出来是为了把「模型没登记」与「平台还没支持这个协议」区分开 ——
	// 后者配 model_overrides 也救不了，文案必须直接说清楚，否则运维会反复试配置。
	{Name: "veo", Re: mustRe(`^veo`), Unsupported: "veo"},
	{Name: "sora", Re: mustRe(`^sora`), Unsupported: "sora"},
	{Name: "hailuo", Re: mustRe(`^minimax[-_.]?hailuo|^hailuo`), Unsupported: "hailuo"},
}

// duomiMatchFamily 按顺序找第一个命中的族。
//
// 命名里的分隔符同时接受 - . _：厂商不同型号用过 kling-v1.6-multi / kling-v3-omni 两种写法。
func duomiMatchFamily(modelID string) (duomiFamily, bool) {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id == "" {
		return duomiFamily{}, false
	}
	for _, f := range duomiFamilies {
		if f.Re.MatchString(id) {
			return f, true
		}
	}
	return duomiFamily{}, false
}

// duomiUnknownModelMsg 是「完全无法识别」的报错（B 类）。
//
// 首句「未知的多米模型」是既有用例与运维习惯的锚点，改写时不要动。
// **不带上游模型名 / 渠道名 / key**：这条错误会经 tasks.error_msg 落到门户可见的任务详情里，
// 带上游真名等于把渠道的上游型号泄露给终端用户。
func duomiUnknownModelMsg(modelID string) string {
	return fmt.Sprintf(
		"未知的多米模型 %q：不属于任何内置协议族。\n"+
			"解法一：把平台模型 ID 改成族内命名（kling-* / seedance-* / gpt-image-* / nano-* / gemini-*-image / muse-* / suno-* / chirp-*），协议可零配置推断；\n"+
			"解法二：在渠道 adapter_options 里按**平台模型 ID** 登记（key 不是上游模型名）：\n"+
			`{"model_overrides":{%s:{"protocol":"<协议>","upstream_model":"<上游真名>"}}}`+"\n"+
			"可用协议：%s",
		modelID, jsonKeyLiteral(modelID), duomiProtocolList)
}

// duomiUnsupportedFamilyMsg 是「协议族存在但未实现」的报错（C 类）。
func duomiUnsupportedFamilyMsg(modelID, family string) string {
	return fmt.Sprintf(
		"模型 %q 属于 %s 系列，但本适配器尚未实现该协议，配置 model_overrides 也无法解决。\n"+
			"请改用支持 %s 的渠道，或为该厂商新增适配器分支（步骤见 docs/渠道接入手册-新增模型与协议.md）",
		modelID, family, family)
}

// jsonKeyLiteral 把模型 ID 渲染成可直接粘贴进 JSON 的键。
//
// 不用 %q：Go 的 %q 对非 ASCII 会输出 \u 转义，而 model_overrides 的键必须与
// 平台模型 ID 逐字相同 —— 运维粘进去的若是 "图像旗舰" 的转义形式，永远命不中。
func jsonKeyLiteral(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// PreflightModel 实现 relay.ModelPreflighter：用渠道当前配置跑一次**真实的**解析。
//
// 复用 duomiResolveProfile 而不是另写一套判断：预检与运行期一旦是两份逻辑，
// 早晚出现「保存时说能承接、调用时报未知模型」这种最难查的不一致。
func (DuoMi) PreflightModel(modelID string, rawOptions []byte) relay.ModelPreflight {
	id := strings.TrimSpace(modelID)
	if id == "" {
		return relay.ModelPreflight{Reason: "模型 ID 为空"}
	}
	opts, err := parseDuomiOptions(rawOptions)
	if err != nil {
		return relay.ModelPreflight{
			Reason: "渠道 adapter_options 本身无法解析：" + err.Error(),
		}
	}
	p, _, err := duomiResolveProfile(opts, id, "")
	if err != nil {
		return relay.ModelPreflight{Reason: err.Error()}
	}
	out := relay.ModelPreflight{OK: true, Family: p.Family}
	// 歌词协议出站不带 model 字段（编号由任务号决定），绑定却要求填上游名。
	// 这里给一句说明，省得运营以为自己填错了。
	if p.Protocol == duomiProtoSunoLyrics {
		out.Note = "歌词协议不使用上游模型名（编号由任务号决定），绑定里填平台模型 ID 即可"
	}
	return out
}
