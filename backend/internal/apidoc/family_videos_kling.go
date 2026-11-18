package apidoc

// 可灵族（Kling 视频协议）的接口文档。
//
// 依据：工作目录「API文档/多米API/可灵API/Kling 3.0 & 3.0 Omni（官方）.md」
// 「Kling 3.0 Turbo（官方）.md」。
//
// 本页写的是**平台入口的字段**（input_reference 数组 + seconds + resolution），
// 不是可灵原生的 `contents[]`：平台把统一写法翻译成厂商报文（见 `duomiapi_kling.go`
// 的 submitDuomiKling）。因此这里只列出"翻译后确实会生效"的字段，
// 官方有、但平台不消费的参数（`multi_shot` 的多镜头语法等）写在说明里指路，不伪造字段。

var docsVideosKling = []FamilyEndpoint{
	{
		Doc: DocumentKey{Kind: KindVideoSubmit},
		Spec: EndpointDoc{
			Name:    "创建视频任务",
			Summary: "提交可灵视频生成任务，立即返回任务号。文生视频 / 图生视频 / 首尾帧 / 配音动画统一入口。",
			// 核心字段由 VideoCoreFields 生成（见 video_common.go）：它们在本族与其它
			// 视频族完全一致，各写一遍只会漂移（改造前三篇共 82 条定义、去重只剩 47 个）。
			// 下面只列**本族增量**：这些参数只有本族模型认识。
			Fields: append(VideoCoreFields(VideoCoreOpts{
				Members:        true,
				PromptMaxLen:   3072,
				SecondsDefault: "5",
				SecondsNote:    "官方取值 3–15 秒；超出模型配置的上限在提交时直接 400。传 -1 为智能时长（仅声明了该能力的模型支持）。",
				AspectDefault:  "16:9",
				AspectEnum:     []string{"16:9", "9:16", "1:1"},
				ResolutionEnum: []string{"720p", "1080p", "4k"},
			}),
				vendor("camera_control", TypeObject, ReqOptional, "—", "运镜控制（推拉摇移）。", "可灵").
					withNote("六个方向只能填一个，各取 -10 ~ 10（正负代表方向）；与尾帧互斥，仅图生视频可用。平台按厂商要求的结构下发。"),
				vendor("camera_control.horizontal", TypeNumber, ReqOptional, "0", "水平移动力度。", "可灵").
					withLim("min", -10, "-10 ~ 10"),
				vendor("camera_control.vertical", TypeNumber, ReqOptional, "0", "垂直移动力度。", "可灵").
					withLim("min", -10, "-10 ~ 10"),
				vendor("camera_control.pan", TypeNumber, ReqOptional, "0", "左右摇镜力度。", "可灵").
					withLim("min", -10, "-10 ~ 10"),
				vendor("camera_control.tilt", TypeNumber, ReqOptional, "0", "上下摇镜力度。", "可灵").
					withLim("min", -10, "-10 ~ 10"),
				vendor("camera_control.roll", TypeNumber, ReqOptional, "0", "旋转力度。", "可灵").
					withLim("min", -10, "-10 ~ 10"),
				vendor("camera_control.zoom", TypeNumber, ReqOptional, "0", "推近 / 拉远力度。", "可灵").
					withLim("min", -10, "-10 ~ 10"),
				vendor("metadata", TypeObject, ReqOptional, "—", "厂商私有参数容器，原样透传并在任务查询中回显。", "部分模型").
					withNote("只下发平台为该模型开放的白名单键；不在白名单里的键不会生效，并在调用记录里给出「未识别参数」告警。"),
			),
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
				{K: "model", V: "kling-v3"},
				{K: "prompt", V: "一只橘猫在钢琴前专注弹奏，镜头从中景缓缓推近"},
				{K: "seconds", V: 5},
				{K: "resolution", V: "720p"},
				{K: "aspect_ratio", V: "16:9"},
			},
			Example2: Obj{
				{K: "model", V: "kling-v3"},
				{K: "prompt", V: "镜头缓缓推近，光线由暖转冷"},
				{K: "seconds", V: 5},
				{K: "input_reference", V: []any{
					Obj{{K: "type", V: "first_frame"}, {K: "image_url", V: "https://cdn.example.com/first.png"}},
					Obj{{K: "type", V: "last_frame"}, {K: "image_url", V: "https://cdn.example.com/last.png"}},
				}},
			},
			ExampleTitle2: "首尾帧",
			Notes: []string{
				VideoCompatNote(),
				"模型可配置最大秒数：超出即在提交时 400；已受理的任务不会因该上限失败。",
				"素材须公网 http(s) 直链；含内联素材的任务不可重试。",
				"平台不做回调推送：callback_url 只透传，请轮询查询接口。",
				"官方还有 `settings.multi_shot` / `settings.audio` 两个开关：平台在翻译报文时按模型档案决定，" +
					"不接受调用方覆盖（平台对不同模型的默认值不同，让调用方改会让成片与价目脱节）。",
			},
			RunMode: RunJSON,
		},
		Presets: []Preset{
			{
				Key: "kling-t2v", Label: "文生视频", Summary: "只给描述，生成一段视频。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "一只橘猫在钢琴前专注弹奏，镜头缓缓推近"},
					{K: "seconds", V: presetTokenSeconds},
					{K: "resolution", V: presetTokenResolution},
				},
				Fields: []string{"model", "prompt", "seconds", "resolution"},
			},
			{
				Key: "kling-i2v", Label: "图生视频", Summary: "给首帧图，让它动起来。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "镜头缓缓推近，光线由暖转冷"},
					{K: "seconds", V: presetTokenSeconds},
					{K: "input_reference", V: []any{
						Obj{{K: "type", V: "image"}, {K: "image_url", V: "https://cdn.example.com/first.png"}},
					}},
				},
				Fields: []string{"model", "prompt", "seconds", "input_reference"},
				Note:   "素材须公网直链，推荐先用文件接口上传。",
			},
			{
				Key: "kling-first-last", Label: "首尾帧", Summary: "指定开头与结尾画面，中间自动补。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "从白天过渡到黄昏，运镜保持平稳"},
					{K: "seconds", V: presetTokenSeconds},
					{K: "input_reference", V: []any{
						Obj{{K: "type", V: "first_frame"}, {K: "image_url", V: "https://cdn.example.com/first.png"}},
						Obj{{K: "type", V: "last_frame"}, {K: "image_url", V: "https://cdn.example.com/last.png"}},
					}},
				},
				Fields: []string{"model", "prompt", "seconds", "input_reference"},
				Note:   "官方不支持只给尾帧；与 camera_control 互斥。",
			},
			{
				Key: "kling-camera", Label: "运镜控制", Summary: "用六个轴描述推拉摇移。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "镜头缓慢推进，人物保持不动"},
					{K: "seconds", V: presetTokenSeconds},
					{K: "camera_control", V: Obj{{K: "zoom", V: 5}}},
				},
				Fields: []string{"model", "prompt", "seconds", "camera_control"},
				Note:   "六个方向只能填一个，各取 -10 ~ 10；仅图生视频可用。",
			},
		},
	},
}
