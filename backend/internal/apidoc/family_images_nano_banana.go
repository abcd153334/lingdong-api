package apidoc

// Nano Banana 族（Gemini 图像协议）的接口文档。
//
// 依据：工作目录「API文档/多米API/NANO-BANANA/文生图.md」「图生图(图片编辑).md」
// 「查询任务状态.md」。官方只有 4 个请求字段（model / prompt / aspect_ratio / image_size），
// 加上编辑端点独有的 image_urls（数组、最多 10 张、**只收链接**）。
//
// 与 GPT Image 族的三处关键差异（这三条以前因为共用模板而互相污染）：
//   - 参考图的官方字段名是 `image_urls`（数组），不是 `image`；且**明确不支持 base64**；
//   - 尺寸的官方字段名是 `image_size`，只收 1K / 2K / 4K（K 必须大写），没有像素写法；
//   - 画幅是独立字段 `aspect_ratio`（11 个取值），不是 size 的写法。

var docsImagesNanoBanana = []FamilyEndpoint{
	{
		Doc: DocumentKey{Kind: KindImagesGenerations},
		Spec: EndpointDoc{
			Name:    "文生图",
			Summary: "文生图与图生图共用这一条路径：不带参考图走文生图，带参考图自动切到编辑端点。",
			Fields: []Field{
				plat("model", TypeString, ReqRequired, "—", "模型 ID。").withMembers(),
				plat("prompt", TypeString, ReqRequired, "—", "要生成或修改成什么。").
					withLim("max_len", NanoPromptMaxRunes, "≤ 4000 字符").
					withNote("超长会被平台直接拒绝（与官方上限一致），不会先发出去再换回一句错误。"),
				// 官方字段名是 image_size（只收 1K/2K/4K，K 大写）；平台两种写法都认，统一归一到 image_size。
				plat("size", TypeString, ReqOptional, "模型默认", "输出分辨率：1K / 2K / 4K（K 必须大写）。").
					withNote("官方字段名是 `image_size`，平台两种写法都认，出站统一发 `image_size`。" +
						"像素写法会原样转发，但官方只承诺档位词——写了像素由模型提供方判定。" +
						"只有 gemini-3-pro-image-preview（nano-banana-pro）与 gemini-3.1-flash-image-preview（nano-banana-2）接受该参数。"),
				std("aspect_ratio", TypeString, ReqOptional, "auto", "画幅比例。").
					withEnum("auto", "1:1", "2:3", "3:2", "3:4", "4:3", "4:5", "5:4", "9:16", "16:9", "21:9").
					withNote("官方原文：不传则自适应，**单参考图尽量不要传这个参数**（会强行改变构图）。"),
				std("image", TypeAny, ReqOptional, "—", "参考图：公网 http(s) 直链组成的数组（最多 10 张）。").
					withNote("官方字段名是 `image_urls`，平台两种写法都认；**不支持 base64**（官方原文：只支持链接形式）。" +
						"本地文件请用「图生图（图片编辑）」上传。带参考图时平台自动切到编辑端点，字段集不变。"),
				std("n", TypeInteger, ReqOptional, "1", "张数：原样透传给模型提供方，平台不校验上限。").
					withNote("按实际交付张数结算，并在调用记录里标一条张数差异。"),
				std("response_format", TypeString, ReqOptional, "url", "返回链接还是内嵌 base64（平台字段，不发给模型）。").
					withEnum("url", "b64_json").
					withNote("url 指向本平台代理地址，24 小时内有效。"),
				plat("async", TypeBoolean, ReqOptional, "false", "异步模式：立即返回任务号，不等出图。").
					withNote("为 true 时返回 200 + 任务对象，用 GET /v1/tasks/{id} 查状态、GET /v1/tasks/{id}/artifacts 列产物。"),
			},
			Returns: []Field{
				ret("created", TypeInteger, "响应生成时间（Unix 秒）。"),
				ret("data[].url", TypeString, "图片地址，指向本平台代理地址（24 小时内有效）。"),
				ret("data[].b64_json", TypeString, "图片 base64（response_format=b64_json 时返回）。"),
			},
			Response: ResponseSpec{
				Variants: []ResponseVariant{
					{
						Status: 200, Label: "成功（同步出图）",
						Notes: []string{"默认行为：本次请求内等到出图，直接返回结果。"},
						Example: Obj{
							{K: "created", V: 1735689600},
							{K: "data", V: []any{
								Obj{{K: "url", V: "https://api.example.com/v1/media/tkn_7a1b"}},
							}},
						},
					},
					{
						Status: 200, Label: "已受理（async: true）",
						Returns: TaskFlatFields(),
						Example: TaskExampleFlatAcceptedFor("gemini-nano-banana-2.1"),
						Notes:   []string{"加 async: true 时立刻返回任务号，出图结果用查询接口取。"},
					},
				},
			},
			Example: Obj{
				{K: "model", V: "nano-banana"},
				{K: "prompt", V: "中文海报：AI 视频生成器，智能角色，暗色背景"},
				{K: "aspect_ratio", V: "16:9"},
			},
			Example2: Obj{
				{K: "model", V: "nano-banana-pro"},
				{K: "prompt", V: "把这张图的人物换成穿红色外套，其余保持不变"},
				{K: "image", V: []any{"https://cdn.example.com/person.png"}},
				{K: "size", V: "2K"},
			},
			ExampleTitle2: "图生图（带参考图）",
			Notes: []string{
				"带不带参考图决定走文生图还是编辑端点：平台按是否给了图自动切换，字段集与计费口径都不变。",
				"参考图只收公网链接：平台需要按链接取图并重放，内联体会在重试时丢失。",
			},
			RunMode: RunJSON,
		},
		Presets: []Preset{
			{
				Key: "nano-t2i", Label: "文生图", Summary: "只给描述，直接出图。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "中文海报：AI 视频生成器，智能角色"},
					{K: "aspect_ratio", V: "16:9"},
				},
				Fields: []string{"model", "prompt", "aspect_ratio"},
			},
			{
				Key: "nano-i2i", Label: "图生图", Summary: "给公网参考图，按描述改写。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "把背景换成海边日落，保留人物"},
					{K: "image", V: []any{"https://cdn.example.com/person.png"}},
				},
				Fields: []string{"model", "prompt", "image"},
				Note:   "不要同时传 aspect_ratio：单参考图时它会强行改变构图。",
			},
			{
				Key: "nano-async", Label: "异步文生图", Summary: "提交后立即拿任务号。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "中文海报：AI 视频生成器，智能角色"},
					{K: "async", V: true},
				},
				Fields: []string{"model", "prompt", "async"},
			},
		},
	},

	{
		Doc: DocumentKey{Kind: KindImagesEdits},
		Spec: EndpointDoc{
			Name:    "图生图（图片编辑）",
			Summary: "上传本地图片按指令修改，multipart 表单；参考图是链接时请用「文生图」。",
			Fields: []Field{
				formParam(plat("model", TypeString, ReqRequired, "—", "模型 ID。").withMembers()),
				formParam(std("image", TypeFile, ReqRequired, "—", "原图文件（可多张）。")).
					withNote("上传后平台落成公网直链，再按官方字段 image_urls 下发。"),
				formParam(plat("prompt", TypeString, ReqRequired, "—", "修改指令。")),
				formParam(std("size", TypeString, ReqOptional, "模型默认", "输出分辨率：1K / 2K / 4K。")),
				formParam(std("aspect_ratio", TypeString, ReqOptional, "auto", "画幅比例，取值同「文生图」。")).
					withEnum("auto", "1:1", "2:3", "3:2", "3:4", "4:3", "4:5", "5:4", "9:16", "16:9", "21:9"),
				formParam(std("n", TypeInteger, ReqOptional, "1", "张数：原样转发，按实际交付张数结算。")),
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
				Key: "nano-edit-upload", Label: "上传原图编辑", Summary: "上传本地图片，按指令修改。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "把背景换成纯白摄影棚布光"},
				},
				Fields:  []string{"model", "prompt", "image"},
				Uploads: []string{"image"},
				Note:    "表单提交（multipart），不走 JSON。",
			},
		},
	},
}
