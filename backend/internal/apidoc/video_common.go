package apidoc

import (
	"fmt"
	"strings"
)

// 本文件是**视频提交字段的公共层**：一份词表 + 一个字段生成器 + 一张别名映射。
//
// 存在的理由（一个真实的成本问题）：改造前三篇视频文档共写了 82 条字段定义，
// 去重只剩 47 个名字，重复率 42.7%；三族真正共有的只有 5 个。而其中有 30 个名字
// 在反复表达 11 组语义（时长、清晰度、画幅、素材、首帧…）。
//
// 两份手写的字段表必然漂移 —— 这不是"文案没写好"，是人不可能同步维护三份同样的东西。
// 所以核心字段由本文件**生成**，各族只写自己的增量。
//
// 但**不做"共享表 + 按族删字段"**：字段集合仍是每族完整的（生成器 + 增量），
// 因为「族页漏写 seconds」必须是编译期/门禁级的报错，而不是靠读者跨页拼。
// 这一点与 family_endpoints.go 头部的三纪律一致。

// VideoCoreOpts 是核心字段的**按族覆盖点**。
//
// 只开放真正会因模型而变的四处：模型枚举、时长范围、清晰度来源、画幅默认。
// 不做"任意字段可覆盖"：那等于把生成器退化成模板，改一处要读三处。
type VideoCoreOpts struct {
	// Members 为 true 时 model 走成员枚举（族页必须为 true：读者要知道这一页讲哪个模型）。
	Members bool
	// Omit 按字段名裁掉生成器给出的某几行。
	//
	// 为什么需要它：同一个生成器服务多个族，而"某个字段这一族根本不读"是常态
	// （`watermark` / `callback_url` 只有部分通道实现）。把它留在页面上等于
	// 「文档声明了、代码没人读」——读者传了不生效，比不写更糟。
	Omit []string
	// Require 按字段名覆盖必填性。
	//
	// 为什么需要它：`prompt` / `seconds` / `resolution` 在视频适配器里是**硬必填**
	// （缺一个就是 CallerParamError），而图像/音乐语义下它们可以是可选。
	// 生成器只能取一个保守默认，具体到某族必须能改——否则文档会承诺"可以不传"。
	Require map[string]Required
	// SecondsDefault 是默认值文案，空则写 "—"。
	SecondsDefault string
	// SecondsNote 覆盖时长取值范围说明（各模型上限不同）。
	SecondsNote string
	// AspectDefault 覆盖画幅默认值文案。
	AspectDefault string
	// AspectEnum 覆盖画幅枚举；为空用平台统一 7 值。
	AspectEnum []string
	// AspectNote 覆盖画幅说明。
	//
	// 默认那句讲的是 `adaptive`（跟随首个素材画幅），只有认这个值的族才成立。
	// muse 只收 16:9 / 9:16，把 `adaptive` 留在说明里等于给了一个枚举里没有的值。
	AspectNote string
	// ResolutionSource 默认 EnumVideoTier（取值与秒价来自模型的档位表）。
	ResolutionSource EnumSource
	// ResolutionEnum 非空时改为静态枚举（少数模型的清晰度与价目档无关）。
	ResolutionEnum []string
	// PromptMaxLen > 0 时追加长度上限（各模型上限不同，0 表示不写上限）。
	PromptMaxLen int
	// RefNote 追加到 `image` 描述末尾（素材格式、尺寸、张数等模型差异）。
	//
	// 为什么单开一个口子而不是让族页重写整个字段：素材的**格式规格**是真实信息，
	// 收敛时丢掉它等于降低文档质量；而整字段重写又会重新引入"同一字段两份文本"。
	RefNote string
	// ImageNote 覆盖 `image` 的说明正文。
	//
	// 默认那句讲两件事：「按张计费」与「要锁定首帧改用 first_frame」。
	// muse 这两句都不成立 —— 它按秒计价、且没有首帧概念，照抄会让读者
	// 去找一个根本不存在的计费项。
	ImageNote string
}

// VideoCoreFields 生成视频提交的核心字段（不含任何厂商私有参数）。
//
// 顺序是**读者的书写顺序**：先选模型，再写画面，再定时长/清晰度/画幅，
// 最后才是素材。素材放最后是有意的 —— 它是最容易写错的部分，
// 读者先用最小请求体跑通，再加素材。
func VideoCoreFields(o VideoCoreOpts) []Field {
	model := plat("model", TypeString, ReqRequired, "—", "模型 ID。取值见「模型清单」，须与视频模态一致。")
	if o.Members {
		model = model.withMembers()
	}
	secondsDefault := o.SecondsDefault
	if strings.TrimSpace(secondsDefault) == "" {
		secondsDefault = "—"
	}
	secondsNote := o.SecondsNote
	if strings.TrimSpace(secondsNote) == "" {
		secondsNote = "取值为整数秒，上限随模型；越界在提交时直接报错，不截断、不自动降档（截断会让账单与成片长度对不上）。"
	}
	aspectDefault := o.AspectDefault
	if strings.TrimSpace(aspectDefault) == "" {
		aspectDefault = "随模型"
	}
	resSrc := o.ResolutionSource
	if resSrc == "" {
		resSrc = EnumVideoTier
	}

	prompt := plat("prompt", TypeString, ReqConditional, "—", "画面描述：主体、动作、场景、镜头运动、光线与声音。").
		withReq("`prompt` 与 `input_reference` 至少给一项；只给素材不给描述也能生成，但成片会更依赖参考素材本身。").
		withNote("中文一个字算一个字符。分句写主体、动作、镜头、光线，比堆形容词更稳。")
	if o.PromptMaxLen > 0 {
		prompt = prompt.withLim("max_len", float64(o.PromptMaxLen), fmt.Sprintf("≤ %d 字符", o.PromptMaxLen))
	}

	resolution := plat("resolution", TypeString, ReqOptional, "随模型", "清晰度档位。").
		withNote("这是计费维度：同一段视频在不同档位价格不同。也接受 `宽x高`（如 `1280x720`），平台按短边折算到最接近的档位。")
	if len(o.ResolutionEnum) > 0 {
		resolution = resolution.withEnum(o.ResolutionEnum...)
	} else {
		resolution = resolution.withEnumFrom(resSrc)
	}

	aspectNote := o.AspectNote
	if strings.TrimSpace(aspectNote) == "" {
		aspectNote = "`adaptive` 表示跟随首个参考素材的原始画幅，只在带素材时有意义；纯文字生成时请给具体比例。"
	}
	aspect := plat("aspect_ratio", TypeString, ReqOptional, aspectDefault, "画幅比例。").withNote(aspectNote)
	if len(o.AspectEnum) > 0 {
		aspect = aspect.withEnum(o.AspectEnum...)
	} else {
		aspect = aspect.withEnum("adaptive", "21:9", "16:9", "4:3", "1:1", "3:4", "9:16")
	}

	return applyVideoCoreOverrides([]Field{
		model,
		prompt,
		plat("seconds", TypeAny, ReqOptional, secondsDefault, "视频时长（秒）。").withNote(secondsNote),
		resolution,
		aspect,
		videoImageField(o.RefNote, o.ImageNote),
		plat("first_frame", TypeString, ReqOptional, "—", "首帧：成片从这里开始。").
			withNote("只写一张图的地址。首帧已含在「档位×秒数」的价里，**不按张计费**——"+
				"同一张图写成 `image` 就会按参考图收费，这是两者唯一的实质差别。"),
		plat("last_frame", TypeString, ReqOptional, "—", "尾帧：成片在这里结束，中间由模型补全。").
			withNote("与 `first_frame` 成对使用效果最稳；只给尾帧时部分模型会直接拒绝。"),
		plat("audio", TypeAny, ReqOptional, "—", "参考音频。一条写字符串，多条写数组。").
			withNote("用于配音、音色或节奏参考，按条计费。与 `image` 同时给就是「图 + 音」的生成方式，不需要额外声明模式。"),
		plat("video", TypeAny, ReqOptional, "—", "参考视频。一条写字符串，多条写数组。").
			withNote("用于运镜、动作或风格参考，按秒计费；平台会在提交前探测素材时长，读不到就报错。"),
		inputReference(),
		plat("watermark", TypeBoolean, ReqOptional, "false", "成片是否叠加水印。").
			withNote("只影响成片标记，不影响内容与计费。"),
		// callback_url 保留在文档里，尽管平台**不推送**回调。
		//
		// 理由：从别家迁过来的客户端几乎都会带这个键，而我们确实接受它（原样转发）。
		// 文档里没有它，用户看到的是"按文档写的请求里没这个字段"，然后自己猜
		// "是不是不支持"；写出来并说明"传了也不会收到，请轮询"，才是可执行的指引。
		// 删掉它反而会让白名单里的透传键变成文档盲区（Ark 的一致性门禁就发现了这点）。
		plat("callback_url", TypeString, ReqOptional, "—", "回调地址。").
			withNote("**平台不推送回调**：传了这个字段也不会收到任何请求，请在提交后轮询查询接口。保留它是为了兼容从别家迁移过来的客户端，避免它们因未知字段被拒。"),
	}, o)
}

// applyVideoCoreOverrides 按族的覆盖点裁字段、改必填性。
//
// 覆盖按字段名进行而不是按下标：生成器的顺序会调整，写下标等于让"改了顺序就改错字段"。
func applyVideoCoreOverrides(fields []Field, o VideoCoreOpts) []Field {
	omit := make(map[string]bool, len(o.Omit))
	for _, name := range o.Omit {
		omit[name] = true
	}
	out := make([]Field, 0, len(fields))
	for _, f := range fields {
		if omit[f.Name] {
			continue
		}
		if want, ok := o.Require[f.Name]; ok {
			f.Required = want
			// 覆盖成必填后，原来那句"条件是什么"就没有意义了；留着会让读者
			// 在「必填」旁边看到一段"至少要给一项"的条件说明，自相矛盾。
			if want != ReqConditional {
				f.RequiredNote = ""
			}
		}
		out = append(out, f)
	}
	return out
}

// videoImageField 构造 `image` 行（参考图）。
//
// 一张写字符串、多张写数组：同一个字段名承担两种形态，是为了不让"单图"和"多图"
// 分裂成两个字段（那正是改造前 image / image_urls / input_reference 三套写法的来源）。
//
// override 非空时替换说明正文（见 VideoCoreOpts.ImageNote）；extra 永远追加在后面。
func videoImageField(extra, override string) Field {
	note := override
	if strings.TrimSpace(note) == "" {
		note = "按张计费，用来提供主体、风格或构图参考。要锁定首帧请改用 `first_frame`（不按张收费）。" +
			"素材须公网 http(s) 直链，不接受 `data:` 内联（无法计费与重试）。"
	}
	f := plat("image", TypeAny, ReqOptional, "—", "参考图。一张写字符串，多张写数组。").withNote(note)
	if strings.TrimSpace(extra) != "" {
		f = f.withNote(extra)
	}
	return f
}

// VideoCompatAlias 是一条「旧写法 → 当前写法」的映射。
type VideoCompatAlias struct {
	// Legacy 是旧写法（字段名或字段组合）。
	Legacy string
	// Canonical 是它等价于什么。
	Canonical string
	// Meaning 是这条映射在讲什么（时长/清晰度/…），用于拼人话。
	Meaning string
	// AppliesTo 是「这条别名落在哪个字段上」（可选）。
	//
	// 只给 Canonical 是**取值**而非字段名的那些别名用：`768p → 720p` 的目标是
	// resolution 的一个取值，光看 Canonical 无法判断某页是否适用它。
	// 有了这一格，门禁才能问出「本页有没有被吸附的那个字段」。
	AppliesTo string
}

// videoCompatAliases 是**唯一**的别名清单：代码的归一逻辑与文档的说明都从这里取。
//
// 为什么要有这张表：改造前这些别名分散在 6 处开关里，文档各写一遍、
// 网关各写一遍，于是出现过「文档承诺自动映射、某个适配器却直接 400」的裂缝。
// 现在文档的「兼容写法」段落由本表生成，门禁再反过来检查
// 「代码认的每个别名都在文档里出现过」。
var videoCompatAliases = []VideoCompatAlias{
	{Legacy: "duration", Canonical: "seconds", Meaning: "时长"},
	{Legacy: "size", Canonical: "resolution", Meaning: "清晰度"},
	{Legacy: "width + height", Canonical: "resolution", Meaning: "画面像素宽高"},
	{Legacy: "ratio", Canonical: "aspect_ratio", Meaning: "画幅"},
	{Legacy: "image_urls", Canonical: "image", Meaning: "多张参考图"},
	{Legacy: "768p", Canonical: "720p", Meaning: "清晰度（部分模型的档位写法）", AppliesTo: "resolution"},
	{Legacy: "input_reference", Canonical: "image / first_frame / last_frame / audio / video", Meaning: "素材数组的旧写法"},
}

// VideoCompatAliases 返回别名清单的副本（供门禁与文档生成使用）。
func VideoCompatAliases() []VideoCompatAlias {
	out := make([]VideoCompatAlias, len(videoCompatAliases))
	copy(out, videoCompatAliases)
	return out
}

// VideoCompatNote 生成「兼容写法」那一段人话（全部别名）。
//
// 用生成而不是手写：多个族页 + 通用页都要出现同一段话，手写四遍必然漂移，
// 而漂移的表现是「按 A 页写的请求在 B 页的模型上不生效」。
func VideoCompatNote() string {
	return VideoCompatNoteOf()
}

// VideoCompatNoteOf 只讲 keep 指定的别名（按 Legacy 名；不传 = 全部）。
//
// 为什么需要按族裁剪：别名的**目标字段在各族不一定存在**。Muse 族没有
// `resolution`（上游没有这个参数，适配器出网前丢掉），也没有首尾帧/音视频素材
// （适配器直接 400）。在那一页写「`size` → `resolution`，旧写法仍然可用」，
// 读者照着传会得到「传了没反应」——正是本族刻意不写 `resolution` 想避免的那类反馈。
// 裁剪后留下的每一条都指向本页真实存在的字段，门禁
// `TestVideoCompatNoteMatchesFamilyFields` 双向核对「该说的都说了、不该说的没说」。
func VideoCompatNoteOf(keep ...string) string {
	aliases := videoCompatAliases
	if len(keep) > 0 {
		want := make(map[string]bool, len(keep))
		for _, k := range keep {
			want[strings.TrimSpace(k)] = true
		}
		aliases = make([]VideoCompatAlias, 0, len(keep))
		for _, a := range videoCompatAliases {
			if want[a.Legacy] {
				aliases = append(aliases, a)
			}
		}
	}
	var b strings.Builder
	b.WriteString("旧写法仍然可用，平台会自动改写，不需要改客户端：")
	for i, a := range aliases {
		if i > 0 {
			b.WriteString("；")
		}
		b.WriteString("`" + a.Legacy + "` → `" + a.Canonical + "`（" + a.Meaning + "）")
	}
	b.WriteString("。同一个语义给了两种写法时，以当前写法为准。")
	return b.String()
}

// inputReference 构造 `input_reference` 行。
//
// 它现在是**兼容承载**：新接入一律用上面 5 个自解释字段，这一行只为两件事存在——
//  1. 已发布的客户端仍在用数组写法，平台继续接受（网关负责折叠）；
//  2. 可灵 / Seedance 的示例与预设里用了它，字段表必须声明（否则示例引用未声明字段）。
func inputReference() Field {
	return plat("input_reference", TypeArray, ReqOptional, "[]", "素材数组（兼容写法）。").
		withNote("每一项形如 `{type, image_url|audio_url|video_url}`，等价于上面 5 个字段。" +
			"新接入请直接用 `image` / `first_frame` / `last_frame` / `audio` / `video`；" +
			"数组里的顺序决定素材的编号顺序，不要与顶层字段混用。")
}
