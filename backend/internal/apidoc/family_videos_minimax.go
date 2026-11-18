package apidoc

// MiniMax H3 族的接口文档。
//
// 依据：工作目录「API文档/metaso秘塔」与「API文档/AutoDL」两份官方文档，
// 以及两个适配器的实际实现（`internal/relay/adapters/metaso.go`、`autodl.go`）。
//
// 本页写的是**平台入口的字段**，不是任何一家的原生报文：秘塔收 `content[]`，
// AutoDL 收 `ref_image_N` / `first_frame` + `last_frame` 且要按模式选不同 workflow。
// 平台把这些差异收在适配器里，对外只有一个请求体。
//
// 本族**没有厂商私有参数**：两个适配器都不读任何 H3 专属的顶层键，
// 所以这里不出现 vendor(...) 行 —— 造一个"厂商扩展"既是伪造，也会让读者
// 以为换个模型还能用。

var docsVideosMiniMax = []FamilyEndpoint{
	{
		Doc: DocumentKey{Kind: KindVideoSubmit},
		Spec: EndpointDoc{
			Name:    "创建视频任务",
			Summary: "提交 MiniMax H3 视频生成任务，立即返回任务号。文生视频、图生视频、多图参考、首尾帧、配音，都走同一个请求体。" +
				"模型 ID 取值见 [模型清单](/docs/nodes/models/endpoints/ep-models-list)。",
			// 核心字段由 VideoCoreFields 生成（见 video_common.go），本族只做三处覆盖：
			// 必填性、档位枚举、裁掉本族不读的行。这里没有增量字段可加。
			Fields: VideoCoreFields(VideoCoreOpts{
				Members:      true,
				PromptMaxLen: 20000,
				// 三个都是硬必填：缺 resolution 或 seconds 时适配器直接返回参数错误，
				// 写成"可选"就是让读者照文档写出一条必然 400 的请求。
				Require: map[string]Required{
					"prompt":     ReqRequired,
					"seconds":    ReqRequired,
					"resolution": ReqRequired,
				},
				SecondsNote: "整数秒。取值 4–30；2K 档只支持 4–15 秒。越界在提交时直接 400，" +
					"不截断、不自动降档（截断会让账单与成片长度对不上）。",
				AspectDefault: "16:9（纯文字）/ adaptive（带素材）",
				AspectEnum:    []string{"adaptive", "21:9", "16:9", "4:3", "1:1", "3:4", "9:16"},
				// 用静态枚举而不是模型档位表：H3 的清晰度只有这四个取值，
				// 与"运营给这个模型配了哪几档价目"是两件事。
				ResolutionEnum: []string{"480p", "720p", "1080p", "2k"},
				RefNote: "支持 jpg / png / webp，单张 ≤ 30MB、**短边 ≥ 256px**，最多 9 张。" +
					"公网 http(s) 直链；不接受 `data:` 内联（平台要按张计费、并在重试时原样重放）。" +
					"低于 256px 的图会被直接拒绝（真机验证：1×1 图返回「media dimensions must be between 256 and 5760 pixels」）。",
				// 三行裁掉的理由：
				//   input_reference —— 本族的规范写法就是上面 5 个自解释字段，
				//     把数组写法摆在页面上等于把枚举请回来（平台仍接受，只是不宣传）；
				//   watermark / callback_url —— 本族两个适配器都不读
				//     （水印只认 metadata.aigc_watermark，回调平台根本不推送）。
				//     写在页面上就是"声明了没人读"。
				Omit: []string{"input_reference", "watermark", "callback_url"},
			}),
			Returns: TaskFlatFields(),
			Response: ResponseSpec{
				Variants: []ResponseVariant{{
					Status: 200, Label: "已受理",
					Returns: TaskFlatFields(),
					Example: TaskExampleFlatAccepted(),
					Notes: []string{
						"受理即返回，不代表已开始生成；结果要轮询查询接口。",
						"`status` 的六种取值见「异步任务」一节的节点说明。",
					},
				}},
			},
			Example: Obj{
				{K: "model", V: "minimax-h3"},
				{K: "prompt", V: "清晨的渔港，薄雾贴着水面移动，一艘小船缓缓驶出，镜头从岸边低角度跟随"},
				{K: "seconds", V: 6},
				{K: "resolution", V: "1080p"},
				{K: "aspect_ratio", V: "16:9"},
			},
			Example2: Obj{
				{K: "model", V: "minimax-h3"},
				{K: "prompt", V: "画面从静态照片开始，人物沿着山路走向远处的灯塔，背景音乐随之渐强"},
				{K: "seconds", V: 8},
				{K: "resolution", V: "720p"},
				{K: "first_frame", V: "https://cdn.example.com/harbor-start.png"},
				{K: "last_frame", V: "https://cdn.example.com/harbor-end.png"},
				{K: "audio", V: []any{"https://cdn.example.com/harbor-theme.mp3"}},
			},
			ExampleTitle2: "首尾帧 + 参考音频",
			Notes: []string{
				// 这一句是本页最该被读到的东西：读者不需要声明"我要做哪种视频"。
				"**生成方式由素材自动决定，不需要额外字段**：只给 `prompt` 是文生视频；" +
					"给了 `image` 是图生 / 多图参考；`first_frame` 与 `last_frame` 一起给是首尾帧；" +
					"给了 `audio` 就带上配音。组合就是叠加（例如 `image` + `audio` = 多图参考并配音）。",
				VideoCompatNote(),
				"素材须公网 http(s) 直链；`data:` 内联会被拒绝——先上传换直链再提交。",
				"`audio` 支持 wav / mp3，最多 3 段，按条计费；`video` 按秒计费，提交前平台会探测素材时长。" +
					"参考视频**时长必须 2–15.5 秒**（真机验证：1.6 秒素材被拒绝，返回「media duration must be between 2 and 15.5 seconds」）。",
				"平台不推送回调：本接口没有回调地址字段，请在提交后轮询查询接口，建议 5~10 秒一次，进入终态即停。",
				"以已有任务或一段视频为底重绘 / 升档，走「异步任务」节点下的视频再生成接口；本篇只讲创建。",
			},
			RunMode: RunJSON,
		},
		Presets: []Preset{
			{
				Key: "minimax-t2v", Label: "文生视频", Summary: "只给描述，生成一段视频。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "清晨的渔港，薄雾贴着水面移动，一艘小船缓缓驶出"},
					{K: "seconds", V: presetTokenSeconds},
					{K: "resolution", V: presetTokenResolution},
				},
				Fields: []string{"model", "prompt", "seconds", "resolution"},
			},
			{
				Key: "minimax-i2v", Label: "图生视频", Summary: "给参考图，让它动起来。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "画面里的人物缓缓抬头看向镜头，衣角被海风吹动"},
					{K: "seconds", V: presetTokenSeconds},
					{K: "image", V: []any{"https://cdn.example.com/harbor-portrait.png"}},
				},
				Fields: []string{"model", "prompt", "seconds", "image"},
				Note:   "一张写字符串、多张写数组；最多 9 张，按张计费。要锁定首帧请改用 first_frame。",
			},
			{
				Key: "minimax-first-last", Label: "首尾帧", Summary: "指定开头与结尾画面，中间自动补。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "从清晨过渡到正午，光线逐渐变硬，运镜保持平稳"},
					{K: "seconds", V: presetTokenSeconds},
					{K: "first_frame", V: "https://cdn.example.com/harbor-dawn.png"},
					{K: "last_frame", V: "https://cdn.example.com/harbor-noon.png"},
				},
				Fields: []string{"model", "prompt", "seconds", "first_frame", "last_frame"},
				Note:   "首尾帧不按张收费（已含在档位价里）；两张图给不同链接才会被认成首帧与尾帧。",
			},
			{
				Key: "minimax-multimodal", Label: "多图 + 配音", Summary: "多张参考图加一段参考音频。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "让主角按参考图的造型走过码头，背景音乐缓缓响起"},
					{K: "seconds", V: presetTokenSeconds},
					{K: "image", V: []any{
						"https://cdn.example.com/harbor-hero.png",
						"https://cdn.example.com/harbor-boat.png",
					}},
					{K: "audio", V: []any{"https://cdn.example.com/harbor-bgm.mp3"}},
				},
				Fields: []string{"model", "prompt", "seconds", "image", "audio"},
				Note:   "参考图按张计费、参考音频按条计费；参考视频按秒计费（给 video 字段）。",
			},
		},
	},
}
