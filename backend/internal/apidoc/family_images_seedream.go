package apidoc

// Seedream 族（火山方舟 Seedream 5.0 / 4.5 / 4.0 图像协议）的接口文档。
//
// 依据：工作目录「API文档/火山方舟/图片生成API.md」（官方《图片生成 API》）。
//
// **只有一篇**：这个协议是「客户端写什么就发什么」——适配器只替换 model、对齐 size、
// 显式写 stream，其余字节原样出站（见 `volcengine_ark_images.go` 的 arkImageBody）。
// 因此"平台入口"与"厂商原生"是同一套字段，再拆一篇「即梦格式」只会得到两页同文 ——
// 那不是补充文档，是把同一份内容印两遍。`images/generations@jimeng` 已从方言登记里删除。

var docsImagesSeedream = []FamilyEndpoint{
	{
		Doc: DocumentKey{Kind: KindImagesGenerations},
		Spec: EndpointDoc{
			Name:    "图片生成",
			Summary: "Seedream 原生报文：尺寸用 size（档位或像素），张数用 sequential_image_generation 表达，字段与官方文档同名同义。",
			Fields: []Field{
				plat("model", TypeString, ReqRequired, "—", "模型 ID；出站时替换成厂商侧的真实模型名。").withMembers(),
				// 官方：图片生成场景必选；图层拆分场景可选（不传则自动识别主要元素）。
				plat("prompt", TypeString, ReqConditional, "—", "画面描述或编辑指令。").
					withReq("图片生成场景必填；图层拆分场景可省（不传则自动识别图中主要元素）。").
					withNote("字数建议：中文 ≤ 300 字、英文 ≤ 600 词（官方原文：字数过多模型会只关注重点，造成元素缺失）。" +
						"\nSeedream 5.0 pro / flash 另支持俄语、阿拉伯语、菲律宾语、泰语、土耳其语、韩语、马来语、西班牙语、葡萄牙语、印尼语、法语、德语、越南语、日语。"),
				std("image", TypeAny, ReqOptional, "—", "参考图：公网 URL 或 data:image/<格式>;base64,…；单张传字符串，多张传数组。").
					withNote("张数：5.0 pro / flash 最多 10 张，5.0 lite / 4.5 / 4.0 最多 14 张。" +
						"\n单张要求：jpeg / png / webp / bmp / tiff / gif / heic / heif；宽高比 [1/16, 16]；宽高 > 14px；≤ 30MB；总像素 [196, 3600 万]。" +
						"\n图层拆分场景：image 必填且只能 1 张（png / jpeg，总像素 [262144, 3600 万]），传多张报错。"),
				// 默认档是**按模型**不同的（官方：pro / flash 默认 2K，lite 默认 2048x2048，
				// 图层拆分场景默认 auto），一个页面覆盖三个模型时不能写死其中一个默认值。
				std("size", TypeString, ReqOptional, "模型默认（见下）", "图像尺寸：分辨率档位，或「宽x高」像素值。").
					withNote(seedreamSizeNote()),
				plat("layer_decomposition", TypeBoolean, ReqOptional, "false", "图层拆分：把 1 张图拆成底图 + 最多 16 个可独立编辑的图层（各为带透明通道的 png）。").
					withNote("仅 Seedream 5.0 pro / flash。只能输入 1 张图；任一图层失败整体报错、不支持部分成功；拆分数量超上限可能丢图层。"),
				std("sequential_image_generation", TypeString, ReqOptional, "disabled", "组图模式。").
					withEnum("auto", "disabled").
					withNote("仅 Seedream 5.0 lite / 4.5 / 4.0。auto 由模型判断是否出组图及张数；disabled 只出一张。" +
						"5.0 pro / flash 只出单图，不支持本参数。"),
				std("sequential_image_generation_options", TypeObject, ReqOptional, "—", "组图配置，仅 sequential_image_generation=auto 时生效。").
					withNote("仅 Seedream 5.0 lite / 4.5 / 4.0。"),
				std("sequential_image_generation_options.max_images", TypeInteger, ReqOptional, "15", "本次最多生成几张。").
					withLim("max", 15, "1 ~ 15").
					withNote("官方：输入的参考图数量 + 最终生成的图片数量 ≤ 15 张。"),
				std("optimize_prompt_options", TypeObject, ReqOptional, "—", "提示词优化配置。"),
				std("optimize_prompt_options.mode", TypeString, ReqOptional, "standard", "优化模式。").
					withEnum("standard", "fast").
					withNote("standard 质量更高、耗时较长；fast 更快、效果略低，**Seedream 5.0 flash / lite / 4.5 不支持 fast**。"),
				std("output_format", TypeString, ReqOptional, "jpeg", "输出图像格式。").
					withEnum("png", "jpeg").
					withNote("仅 Seedream 5.0 pro / flash / lite。图层拆分场景下只控制底图，图层恒为 png。"),
				std("background", TypeString, ReqOptional, "opaque", "透明通道。").
					withEnum("transparent", "opaque").
					withNote("仅 Seedream 5.0 pro / flash，且仅在**图生图**场景、只输入 1 张带透明通道的图片时可用。" +
						"透明底输出默认 png：同时指定 output_format=jpeg 会报错；输入 jpeg 等不支持透明通道的格式也会报错。"),
				std("response_format", TypeString, ReqOptional, "url", "返回链接还是内嵌 base64。").
					withEnum("url", "b64_json").
					withNote("url 的链接 24 小时内有效，需要长期保存请先转存。"),
				std("watermark", TypeBoolean, ReqOptional, "true", "是否在图片右下角添加「AI 生成」水印。").
					withNote("官方默认 **true**：不传就会带水印，要去掉必须显式传 false。"),
				std("stream", TypeBoolean, ReqOptional, "false", "流式输出：逐张返回结果。").
					withNote("仅 Seedream 5.0 lite / 4.5 / 4.0。开启后响应是 SSE，不是一段 JSON。" +
						"平台同步出图时强制写 false，以免模型回 SSE 而网关按 JSON 交付。"),
				std("tools", TypeArray, ReqOptional, "—", "工具配置（目前只有联网搜索）。").
					withNote("仅 Seedream 5.0 lite。写法 [{type: \"web_search\"}]；实际搜索次数见响应 usage.tool_usage.web_search，为 0 表示未搜索。"),
				std("tools[].type", TypeString, ReqRequired, "—", "工具类型。").
					withEnum("web_search"),
				// 平台字段：Ark 没有这个键，出站前会被删掉。它只参与计费口径（按画幅核对档位），
				// 不改变出图像素 —— 像素由 size 与提示词决定。写成"决定画幅"就是骗人。
				alias("aspect_ratio", TypeString, ReqOptional, "—", "画幅比例，平台字段：只参与计费口径核对，**不发给模型**，也不改变出图像素。").
					withNote("Seedream 没有画幅参数：要控制形状请直接用 size 写像素（如 2048x1024），或在提示词里描述画幅。"),
			},
			Returns: []Field{
				ret("created", TypeInteger, "创建时间（Unix 秒）。"),
				ret("model", TypeString, "本次实际使用的模型名。"),
				ret("data[].url", TypeString, "图片地址（response_format=url 时返回，24 小时有效）。"),
				ret("data[].b64_json", TypeString, "图片 base64（response_format=b64_json 时返回）。"),
				ret("data[].size", TypeString, "实际出图尺寸，形如 2048x2048。"),
				ret("data[].z_index", TypeInteger, "图层叠放顺序：底图固定 0，图层从 1 递增，数值越大越靠上。"),
				ret("data[].name", TypeString, "图层名称：模型按主体内容自动生成。"),
				ret("data[].description", TypeString, "图层描述：比 name 更细的颜色 / 状态 / 材质等。"),
				ret("data[].bounding_box", TypeObject, "图层在输出底图坐标系中的边界框；底图覆盖整张画布，不返回该字段。").
					withNote("absolute 与 normalized 二选一即可：要还原到底图用 absolute，要还原到任意画布用 normalized。"),
				ret("data[].bounding_box.absolute", TypeArray, "绝对像素坐标 [left, top, right, bottom]，原点为底图左上角。"),
				ret("data[].bounding_box.normalized", TypeArray, "归一化坐标 [left, top, right, bottom]，按底图宽高换算到 [0, 1000]。"),
				ret("data[].error", TypeObject, "组图场景下某一张失败时，这张图的错误信息。").
					withNote("审核不通过会继续生成其余图片；内部服务异常（500）会终止后续生成。"),
				ret("data[].error.code", TypeString, "该图的错误码。"),
				ret("data[].error.message", TypeString, "该图的错误消息。"),
				ret("usage.generated_images", TypeInteger, "成功生成的张数（不含失败）。"),
				ret("usage.input_images", TypeInteger, "输入模型的图片张数（5.0 pro / flash）。"),
				ret("usage.output_tokens", TypeInteger, "出图消耗的 token：sum(长×宽)/256 取整。"),
				ret("usage.total_tokens", TypeInteger, "本次总 token；当前不计算输入，故与 output_tokens 相同。"),
				ret("usage.tool_usage.web_search", TypeInteger, "联网搜索次数（仅 5.0 lite，0 表示未搜）。"),
			},
			Response: ResponseSpec{
				// 只有同步形态：方舟的 images/generations 一次返回结果，没有任务号与查询接口。
				// 曾经按「端点契约里 AcceptsAsync=true」写过 async 字段与已受理变体 ——
				// 那是把 URL 的形状当成了族的协议，读者照着发只会撞上"没有渠道能承接"。
				Variants: []ResponseVariant{
					{Status: 200, Label: "成功（同步出图）", Notes: []string{"data[] 里是本次生成的图片或图层。"}},
				},
			},
			Example: Obj{
				{K: "model", V: "doubao-seedream-5-0-260128"},
				{K: "prompt", V: "赛博朋克风格的城市夜景，霓虹灯与雨后倒影"},
				{K: "size", V: "2K"},
				{K: "watermark", V: false},
			},
			Example2: Obj{
				{K: "model", V: "doubao-seedream-5-0-pro-260628"},
				{K: "image", V: "https://cdn.example.com/poster.png"},
				{K: "layer_decomposition", V: true},
				{K: "size", V: "auto"},
			},
			ExampleTitle2: "图层拆分（1 张图拆成底图 + 图层）",
			Notes: []string{
				"本页字段名、默认值、取值范围与厂商《图片生成 API》逐条对齐；平台只替换 model、对齐 size、显式写 stream，其余字段原样转发。",
				"模型能力：5.0 pro / flash 支持图层拆分，只出单图（不支持组图 / 联网搜索 / 流式）；" +
					"5.0 lite / 4.5 / 4.0 支持组图与流式输出（联网搜索仅 5.0 lite），不支持图层拆分。",
				"没有 `n` / `quality`：张数只认 sequential_image_generation_options.max_images（或组图自动判断），画质由模型决定。",
				"图层拆分的产物是「1 张底图 + 最多 16 个图层」，每条产物各占一个 data[] 条目。",
			},
			RunMode: RunJSON,
		},
		Presets: []Preset{
			{
				Key: "seedream-t2i", Label: "文生图", Summary: "只给描述，按档位出图。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "赛博朋克风格的城市夜景，霓虹灯与雨后倒影"},
					{K: "size", V: "2K"},
				},
				Fields: []string{"model", "prompt", "size"},
			},
			{
				Key: "seedream-i2i", Label: "图生图", Summary: "给参考图，按描述改写（最多 10 张）。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "把背景换成纯白摄影棚布光，保留商品细节"},
					{K: "image", V: []any{"https://cdn.example.com/product.png"}},
					{K: "size", V: "2K"},
				},
				Fields: []string{"model", "prompt", "image", "size"},
				Note:   "公网 URL 或 data: 内联都可以（该协议原生支持 base64）。",
			},
			{
				Key: "seedream-layer", Label: "图层拆分", Summary: "把一张图拆成底图 + 可独立编辑的图层。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "把海报拆成背景、人物、标题文字三层"},
					{K: "image", V: "https://cdn.example.com/poster.png"},
					{K: "layer_decomposition", V: true},
					{K: "size", V: "auto"},
				},
				Fields: []string{"model", "prompt", "image", "layer_decomposition", "size"},
				Note:   "仅 5.0 pro / flash；只能传一张图，返回底图 + 最多 16 个图层。",
			},
			{
				Key: "seedream-group", Label: "组图", Summary: "一次出一组内容关联的图。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "同一角色的四种表情：开心、生气、惊讶、困倦"},
					{K: "sequential_image_generation", V: "auto"},
					{K: "sequential_image_generation_options", V: Obj{{K: "max_images", V: 4}}},
					{K: "size", V: "2K"},
				},
				Fields: []string{"model", "prompt", "sequential_image_generation", "sequential_image_generation_options", "size"},
				Note:   "仅 5.0 lite / 4.5 / 4.0；参考图张数 + 生成张数 ≤ 15。",
			},
		},
	},
}

// seedreamSizeNote 摘自官方「size」一节（各模型的档位、像素区间与宽高比区间）。
func seedreamSizeNote() string {
	return "写法两种，**不可混用**：\n" +
		"① 档位：`1K` / `1.5K` / `2K`（5.0 pro / flash 图片生成默认 2K；图层拆分场景另有 `auto`，可选 1K / 1.5K / 2K）。" +
		"实际像素由模型按提示词里的画幅描述决定，1.5K 与 1K 同价。\n" +
		"② 像素：`宽x高`。5.0 pro / flash 总像素 [921600, 4624220]、宽高比 [1/16, 16]；" +
		"5.0 lite / 4.5 / 4.0 的上限按图生图 / 文生图场景不同，以官方文档为准。\n" +
		// lite 与 pro/flash 的档位集合**不同**：lite 是 2K/3K/4K（没有 1K），
		// 默认 2048x2048。不写这一句，lite 的读者会照 pro/flash 那张表填 1K。
		"5.0 lite 的档位是 `2K` / `3K` / `4K`（**没有 1K**），不传 size 时默认 2048x2048。\n" +
		"5.0 pro / flash 的档位→像素参考（同一行依次是 1K / 1.5K / 2K）：\n" +
		"1:1　1024x1024 · 1536x1536 · 2048x2048\n" +
		"4:3　1152x864 · 1792x1344 · 2368x1776\n" +
		"3:4　864x1152 · 1344x1792 · 1776x2368\n" +
		"16:9　1424x800 · 2048x1152 · 2816x1584\n" +
		"9:16　800x1424 · 1152x2048 · 1584x2816\n" +
		"3:2　1248x832 · 1872x1248 · 2496x1664\n" +
		"2:3　832x1248 · 1248x1872 · 1664x2496\n" +
		"21:9　1568x672 · 2352x1008 · 3136x1344\n" +
		"图层拆分场景的 auto：原图底图与图层尺寸落在 [921600, 4624220] 之间时按原尺寸输出；小于 1K 按 1K；大于 2K 按 2K。"
}
