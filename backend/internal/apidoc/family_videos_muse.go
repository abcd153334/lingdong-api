package apidoc

// Muse 族的接口文档。
//
// 依据：多米API 的 muse 协议（官方只收 5 个入参）与适配器实现
// （internal/relay/adapters/duomiapi_muse.go）。
//
// 本族与其它视频族最大的两处差别，决定了它只有 5 行字段：
//  1. **上游没有分辨率参数** —— `duomiMuseLocalOnly` 会把它丢掉，价目也固定 720p；
//  2. **只收图片参考** —— 音频/视频素材在适配器里直接 400。
//
// 裁掉的每一个字段都有代码依据（见 Omit 的注释），不是"暂时没写"。
// 把不生效的字段留在页面上，读者会照着传，然后得到"传了没反应"这种最难查的反馈。

var docsVideosMuse = []FamilyEndpoint{
	{
		Doc: DocumentKey{Kind: KindVideoSubmit},
		Spec: EndpointDoc{
			Name:    "创建视频任务",
			Summary: "提交 Muse 视频生成任务，立即返回任务号。图生视频：5 个字段就是全部入参。" +
				"模型 ID 取值见 [模型清单](/docs/nodes/models/endpoints/ep-models-list)。",
			Fields: VideoCoreFields(VideoCoreOpts{
				Members:      true,
				PromptMaxLen: 20000,
				// 四个必填都由适配器在出网前逐个拦下：prompt 为空、没有图、缺画幅
				// 各有一条明确的 CallerParamError。写成可选就是让读者照文档写出一条
				// 必然 400 的请求。
				Require: map[string]Required{
					"prompt":       ReqRequired,
					"seconds":      ReqRequired,
					"aspect_ratio": ReqRequired,
					"image":        ReqRequired,
				},
				SecondsNote: "整数秒，官方只支持 10 / 20 / 30 三档；其它值平台不拦、由模型判定（被拒会全额退款）。" +
					"**务必显式传**：不传时平台按模型上限 30 秒预扣并结算，而这个模型不回传实际时长，" +
					"10 秒的成片会按 30 秒计费。",
				// 上游不填默认值：缺了 aspect_ratio 直接 400，所以这里不能写「默认 16:9」。
				AspectDefault: "—",
				AspectEnum:    []string{"16:9", "9:16"},
				AspectNote:    "只有 `16:9` 与 `9:16` 两种，**必填**：平台不会替你补默认值，缺了在提交时直接报错。",
				// 素材说明：本族按秒计价、没有首帧概念，所以不复用默认那句
				// 「按张计费 / 要首帧请用 first_frame」（两句在这里都不成立）。
				ImageNote: "参考图，**必填** 1–8 张：一张写字符串，多张写数组。按张数不额外计费（价格只跟时长走）。" +
					"素材须公网 http(s) 直链，不接受 `data:` 内联（无法在重试时原样重放）。",
				RefNote: "支持 jpg / png / webp；超过 8 张不会被平台拦截，但会被服务方拒绝并全额退款。",
				// 8 行裁掉的理由：
				//   resolution —— 上游没有这个参数（duomiMuseLocalOnly 丢弃），价目固定 720p；
				//   first_frame / last_frame —— 上游只有 image_urls，这两个会被当普通参考图
				//     展开（allImages()），写成"首帧"是伪造语义；
				//   audio / video —— 适配器直接 400「muse 只接受图片参考」；
				//   input_reference —— 规范写法就是 image，数组写法摆在页面上等于把枚举请回来；
				//   watermark / callback_url —— 多米通道不读，平台也不推回调。
				Omit: []string{
					"resolution", "first_frame", "last_frame", "audio", "video",
					"input_reference", "watermark", "callback_url",
				},
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
				{K: "model", V: "muse-video"},
				{K: "prompt", V: "海边灯塔的黄昏，海浪缓慢拍打礁石，镜头从远处缓缓推近"},
				{K: "seconds", V: 10},
				{K: "aspect_ratio", V: "16:9"},
				{K: "image", V: "https://cdn.example.com/lighthouse-dusk.png"},
			},
			Example2: Obj{
				{K: "model", V: "muse-video"},
				{K: "prompt", V: "同一位角色依次走过码头、集市与山顶，镜头始终跟随，色调统一"},
				{K: "seconds", V: 20},
				{K: "aspect_ratio", V: "9:16"},
				{K: "image", V: []any{
					"https://cdn.example.com/role-dock.png",
					"https://cdn.example.com/role-market.png",
					"https://cdn.example.com/role-summit.png",
				}},
			},
			ExampleTitle2: "竖版 + 多张参考图",
			Notes: []string{
				// 本页最容易被误解的两点，放在最前面。
				"**只支持图生视频**：`image` 必填，纯文字提交不了；`audio` / `video` 素材不支持，" +
					"传了会被直接拒绝（不是忽略）。",
				"**时长就是价格**：10 / 20 / 30 秒三档，清晰度固定 720p，不按分辨率分档 —— " +
					"所以本页没有 `resolution` 字段，写了也不会生效。",
				// 兼容段按本族裁剪：`size` / `768p` 指向 resolution（本页没有这个字段，
				// 上游也不收），`input_reference` 的目标里含首尾帧与音视频素材（本族直接 400）。
				// 留下三条与本页字段一一对应的：duration→seconds、ratio→aspect_ratio、
				// image_urls→image（多米官方报文里就叫 image_urls，老客户端最可能带这个）。
				VideoCompatNoteOf("duration", "ratio", "image_urls"),
				"平台不推送回调：提交后轮询查询接口，建议 5~10 秒一次，进入终态即停。",
				"以已有任务或一段视频为底重绘 / 升档，走「异步任务」节点下的视频再生成接口；本篇只讲创建。",
			},
			RunMode: RunJSON,
		},
		Presets: []Preset{
			{
				Key: "muse-10s", Label: "10 秒 · 横版", Summary: "官方最短档，一张参考图，适合先跑通接入。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "清晨的旧书店，阳光穿过窗棂照在书架上，镜头缓慢横移"},
					{K: "seconds", V: presetTokenSeconds},
					{K: "aspect_ratio", V: "16:9"},
					{K: "image", V: "https://cdn.example.com/bookstore-morning.png"},
				},
				Fields: []string{"model", "prompt", "seconds", "aspect_ratio", "image"},
			},
			{
				Key: "muse-vertical", Label: "10 秒 · 竖版", Summary: "9:16 竖屏，适合短视频投放。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "雨天的街角咖啡店，玻璃上的水珠缓缓滑落，镜头贴近橱窗"},
					{K: "seconds", V: presetTokenSeconds},
					{K: "aspect_ratio", V: "9:16"},
					{K: "image", V: "https://cdn.example.com/cafe-rain.png"},
				},
				Fields: []string{"model", "prompt", "seconds", "aspect_ratio", "image"},
				Note:   "竖版只需要把 aspect_ratio 换成 9:16，其余字段不变。",
			},
			{
				Key: "muse-multi", Label: "20 秒 · 多图", Summary: "三张参考图，20 秒长片。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "三张参考图里的场景依次出现，角色造型保持一致，运镜平稳过渡"},
					{K: "seconds", V: presetTokenSeconds},
					{K: "aspect_ratio", V: "16:9"},
					{K: "image", V: []any{
						"https://cdn.example.com/scene-harbor.png",
						"https://cdn.example.com/scene-street.png",
						"https://cdn.example.com/scene-hill.png",
					}},
				},
				Fields: []string{"model", "prompt", "seconds", "aspect_ratio", "image"},
				Note:   "最多 8 张；张数不影响价格，价格只跟时长走。",
			},
		},
	},
}
