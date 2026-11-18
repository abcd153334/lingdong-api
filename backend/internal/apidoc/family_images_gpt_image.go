package apidoc

// GPT Image 族（OpenAI 图像协议）的接口文档。
//
// 依据：工作目录「API文档/多米API/gpt-image-2/生成图片.md」「查询异步结果.md」。
// 官方文档只有 5 个请求字段（model / prompt / size / image / quality）外加
// mask 与 background 两个补充说明；本族的字段表就是这几个，再加上平台自己实现的行为
// （async、response_format、n 的原样转发、档位→像素的换算），每一处都注明了出处或实现文件。
//
// 曾经这一族与 Nano Banana、Seedream 共用一张模板，靠 `appliesTo` 删字段：
// 于是页面上同时出现「image 支持 base64」（多米实际拒收 data:）、
// 「aspect_ratio 决定画幅」（官方没有这个字段）、「quality 可选 xhigh」（官方只到 high）。
// 那三条都不是写错字，是"共用模板"这个结构必然产生的结果。

var docsImagesGptImage = []FamilyEndpoint{
	{
		Doc: DocumentKey{Kind: KindImagesGenerations},
		Spec: EndpointDoc{
			Name:    "生成图片",
			Summary: "文生图与图生图共用这一条路径：给描述出图，给参考图则按描述改写（可带蒙版）。",
			Fields: []Field{
				plat("model", TypeString, ReqRequired, "—", "模型 ID。").withMembers(),
				plat("prompt", TypeString, ReqRequired, "—", "要生成什么。").
					withLim("max_len", GPTImagePromptMaxRunes, "≤ 5000 字符"),
				// 官方原文：支持自定义 widthxheight、档位比例、auto；约束写在说明里。
				plat("size", TypeString, ReqOptional, "auto", "输出尺寸：比例（如 16:9）、像素（如 1024x1024）或 auto。").
					withNote(gptImageSizeNote()),
				// 平台字段：官方协议里没有独立的画幅参数，画幅是 size 的写法之一。
				// 本平台允许把「档位」与「画幅」分开写，由平台换算成像素后只发 size —— 这是
				// 平台自己的便利，不发给模型，所以必须写在文档里（否则用户以为上游认它）。
				alias("aspect_ratio", TypeString, ReqOptional, "—", "画幅比例，平台字段：与 size 的档位词组合换算成像素后下发，本身不发给模型。").
					withNote("只有 size 写档位（1K / 2K / 4K）时才参与换算；size 已写像素或比例时以 size 为准，两者冲突会记一条尺寸冲突告警。"),
				std("image", TypeString, ReqOptional, "—", "参考图：公网 http(s) 直链；单张传字符串，多张传数组。").
					withNote("**不收 data: 内联**（平台需按链接取图并重放，内联体会在重试时丢失）。本地文件请用「编辑图片」上传，" +
						"或先用 /v1/files 上传再取直链。"),
				std("quality", TypeString, ReqOptional, "模型默认", "思考深度：越高越慢、细节越多。").
					withEnum("low", "medium", "high").
					withNote("平台不改写、不降档：写了该模型不认的值，由模型提供方拒绝。"),
				std("background", TypeString, ReqOptional, "auto", "背景处理。").
					withEnum("auto", "transparent", "opaque").
					withNote("官方只把 transparent 列为可选值，且仅 gpt-image-2-vip / 2.5-flare / 2.5-sunburst 支持；其余模型传了由提供方拒绝。"),
				std("mask_url", TypeString, ReqOptional, "—", "蒙版直链：透明区域 = 允许修改的区域。").
					withNote("公网 http(s) 直链，尺寸需与原图一致；出站字段名恒为 mask_url（按官方 SDK 惯性写的 mask 会被平台归一，不重复下发）。"),
				// n 是 OpenAI 契约字段：平台原样转发，按实际交付张数结算，
				// 官方 gpt-image 文档未列该参数 —— 所以只说"平台转发、按实际交付结算"，不承诺一定出 N 张。
				std("n", TypeInteger, ReqOptional, "1", "张数：原样透传给模型提供方，平台不校验上限。").
					withNote("按模型实际交付的张数结算：请求 3 张只回 2 张时只收 2 张的钱，并在调用记录里标一条张数差异。"),
				// 平台本地字段：决定响应里给直链还是给 base64，上游没有这个键。
				std("response_format", TypeString, ReqOptional, "url", "返回链接还是内嵌 base64（平台字段，不发给模型）。").
					withEnum("url", "b64_json").
					withNote("url 指向本平台代理地址，24 小时内有效，长期保存请先转存。"),
				plat("async", TypeBoolean, ReqOptional, "false", "异步模式：立即返回任务号，不等出图。").
					withNote("为 true 时返回 200 + 任务对象，用 GET /v1/tasks/{id} 查状态、GET /v1/tasks/{id}/artifacts 列产物。" +
						"该模式下不接受 response_format=b64_json（产物只能按任务下载）。"),
			},
			Returns: []Field{
				ret("created", TypeInteger, "响应生成时间（Unix 秒）。"),
				ret("data[].url", TypeString, "图片地址，指向本平台代理地址（24 小时内有效）。"),
				ret("data[].b64_json", TypeString, "图片 base64（response_format=b64_json 时返回，此时没有 url）。"),
			},
			Response: ResponseSpec{
				Variants: []ResponseVariant{
					{
						Status: 200, Label: "成功（同步出图）",
						Notes: []string{"默认行为：本次请求内等到出图，直接返回结果。"},
						Example: Obj{
							{K: "created", V: 1735689600},
							{K: "data", V: []any{
								Obj{{K: "url", V: "https://api.example.com/v1/media/tkn_9f2c"}},
							}},
						},
					},
					{
						Status: 200, Label: "已受理（async: true）",
						Returns: TaskFlatFields(),
						Example: TaskExampleFlatAcceptedFor("gpt-image-2"),
						Notes:   []string{"加 async: true 时立刻返回任务号，出图结果用查询接口取。"},
					},
				},
			},
			Example: Obj{
				{K: "model", V: "gpt-image-2"},
				{K: "prompt", V: "海边小岛，海鸥掠过，灯塔在远处，鱼跃出水面"},
				{K: "size", V: "16:9"},
			},
			Example2: Obj{
				{K: "model", V: "gpt-image-2-vip"},
				{K: "prompt", V: "把背景换成纯白摄影棚布光，保留商品细节"},
				{K: "image", V: []any{"https://cdn.example.com/product.png"}},
				{K: "mask_url", V: "https://cdn.example.com/mask.png"},
				{K: "size", V: "2048x2048"},
				{K: "quality", V: "high"},
			},
			ExampleTitle2: "图生图（带参考图与蒙版）",
			Notes: []string{
				"模型提供方的生成过程本身是异步的：平台会在本请求内等结果就绪，默认最长约 13 分钟。" +
					"超时返回 504 且全额释放额度；需要更久请加 async: true。",
				"调用方的 HTTP 超时必须大于服务端等待上限（建议 ≥ 16 分钟）：OpenAI SDK 默认 10 分钟，" +
					"会在服务端返回前先断开，表现为客户端超时、服务端仍在生成。",
				"超时或客户端断开按 499 / 504 记录，调用记录里带任务号，可凭它找回已生成的图。",
			},
			RunMode: RunJSON,
		},
		Presets: []Preset{
			{
				Key: "gpt-image-t2i", Label: "文生图", Summary: "只给描述，直接出图。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "海边小岛，海鸥掠过，灯塔在远处"},
					{K: "size", V: "16:9"},
				},
				Fields: []string{"model", "prompt", "size"},
			},
			{
				Key: "gpt-image-i2i", Label: "图生图", Summary: "给一张公网参考图，按描述改写。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "把背景换成纯白摄影棚布光，保留商品细节"},
					{K: "image", V: []any{"https://cdn.example.com/product.png"}},
					{K: "size", V: "1024x1024"},
				},
				Fields: []string{"model", "prompt", "image", "size"},
				Note:   "参考图必须是公网直链；本地文件请用「编辑图片」上传。",
			},
			{
				Key: "gpt-image-async", Label: "异步文生图", Summary: "提交后立即拿任务号。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "赛博朋克风格的城市夜景，霓虹灯与雨后倒影"},
					{K: "size", V: "2K"},
					{K: "async", V: true},
				},
				Fields: []string{"model", "prompt", "size", "async"},
				Note:   "只多一个 async：出结果后用任务接口取图。",
			},
		},
	},

	{
		Doc: DocumentKey{Kind: KindImagesEdits},
		Spec: EndpointDoc{
			Name:    "编辑图片",
			Summary: "上传原图（可带蒙版）按指令修改，multipart 表单；参考图是链接时请用「生成图片」。",
			Fields: []Field{
				formParam(plat("model", TypeString, ReqRequired, "—", "模型 ID。").withMembers()),
				formParam(std("image", TypeFile, ReqRequired, "—", "原图文件。")).
					withNote("png / jpeg / webp。上传后平台落成公网直链再发给模型提供方。"),
				formParam(plat("prompt", TypeString, ReqRequired, "—", "修改指令。")).
					withLim("max_len", GPTImagePromptMaxRunes, "≤ 5000 字符"),
				formParam(std("mask", TypeFile, ReqOptional, "—", "蒙版文件：透明区域 = 允许修改的区域。")).
					withNote("png，尺寸须与原图一致。"),
				formParam(plat("size", TypeString, ReqOptional, "auto", "输出尺寸，写法同「生成图片」。")),
				formParam(std("n", TypeInteger, ReqOptional, "1", "张数：原样转发，按实际交付张数结算。")),
				formParam(std("quality", TypeString, ReqOptional, "模型默认", "思考深度。")).
					withEnum("low", "medium", "high"),
				formParam(std("background", TypeString, ReqOptional, "auto", "背景处理，取值与支持范围同「生成图片」。")).
					withEnum("auto", "transparent", "opaque"),
				// 返回格式是平台本地开关（上游没有这个键）。返回里写着 b64_json
				// 却不给这个字段，读者会去找一个页面上并不存在的参数。
				formParam(std("response_format", TypeString, ReqOptional, "url", "返回链接还是内嵌 base64（平台字段，不发给模型）。")).
					withEnum("url", "b64_json"),
			},
			Returns: []Field{
				ret("created", TypeInteger, "响应生成时间（Unix 秒）。"),
				ret("data[].url", TypeString, "结果图片地址（本平台代理地址，24 小时内有效）。"),
				ret("data[].b64_json", TypeString, "结果图片 base64（response_format=b64_json 时返回）。"),
			},
			MaxBody: "25MB",
			Notes: []string{
				"表单提交（multipart），不走 JSON。",
				"该端点必须带参考图：没有图时按错误处理，不会悄悄退化成文生图。",
			},
			RunMode: RunJSON,
		},
		Presets: []Preset{
			{
				Key: "gpt-image-edit-upload", Label: "上传原图编辑", Summary: "上传本地图片，按指令修改。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "把背景换成纯白摄影棚布光"},
					{K: "size", V: "1024x1024"},
				},
				Fields:  []string{"model", "prompt", "size", "image"},
				Uploads: []string{"image"},
				Note:    "表单提交（multipart），不走 JSON。",
			},
		},
	},

	{
		Doc: DocumentKey{Kind: KindImagesVariations},
		Spec: EndpointDoc{
			Name:    "图片变体",
			Summary: "上传一张图生成风格相近的变体。本平台多数模型提供方没有该端点，调用会返回 503 —— 新项目请用「编辑图片」。",
			Fields: []Field{
				formParam(plat("model", TypeString, ReqRequired, "—", "模型 ID。").withMembers()),
				formParam(std("image", TypeFile, ReqRequired, "—", "原图文件。")).withNote("png。"),
				formParam(plat("n", TypeInteger, ReqOptional, "1", "变体张数。")),
				formParam(std("response_format", TypeString, ReqOptional, "url", "返回链接还是内嵌 base64（平台字段，不发给模型）。")).
					withEnum("url", "b64_json"),
				formParam(plat("size", TypeString, ReqOptional, "auto", "输出尺寸，写法同「生成图片」。")),
			},
			Returns: []Field{
				ret("created", TypeInteger, "响应生成时间（Unix 秒）。"),
				ret("data[].url", TypeString, "变体图片地址（本平台代理地址，24 小时内有效）。"),
				ret("data[].b64_json", TypeString, "变体图片 base64（response_format=b64_json 时返回）。"),
			},
			MaxBody: "25MB",
			Notes: []string{
				"本页保留是因为路由真实存在：多数提供方没有这个端点，调用会是 503（不是文档没写完）。",
				"需要「同图多种风格」请用「编辑图片」并只给描述不给蒙版。",
			},
			RunMode: RunJSON,
		},
	},
}

// gptImageSizeNote 是 size 的换算说明。
//
// 正文与表格都由 image_facts.go 渲染（唯一来源）：手写一遍的代价是实测过的 ——
// 页面曾挂着 gpt-image-2 的另一套像素（16:9 → 1672x941），而代码从来不发它。
func gptImageSizeNote() string {
	return GPTImageSizeNote() +
		"\n直接写比例也能出图，但这时尺寸由模型提供方自己挑（实测 `16:9` 回来是 1360x768），" +
		"计费按默认档走 —— 想让尺寸与档位严格对应，请写档位或像素。"
}
