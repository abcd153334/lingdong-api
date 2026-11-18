package apidoc

// Seedance 族（火山方舟 Seedance 视频协议）的接口文档，两篇：
//
//   - `video/submit`：平台统一写法（input_reference 素材数组 + seconds + ratio），
//     由适配器翻译成厂商报文，绝大多数模型走这条；
//   - `video/submit@jimeng`：**厂商原生报文**（content[] 素材数组），
//     字段名与官方文档逐条一致，只替换 model，其余字节原样出站。
//
// 为什么这里保留两篇而不是合并：两者是**两套报文**，平台无法在不丢信息的前提下互转
// （原生报文里 content[] 的顺序就是指代顺序，翻译成 input_reference 会丢掉分镜语义）。
// 反过来，图片族那篇「即梦格式」被删掉了 —— 那里两种写法本来就是同一份字节，拆两页是同文重复。

var docsVideosSeedance = []FamilyEndpoint{
	{
		Doc: DocumentKey{Kind: KindVideoSubmit},
		Spec: EndpointDoc{
			Name:    "创建视频生成任务",
			Summary: "提交 Seedance 视频生成任务，立即返回任务号。文字 / 首帧 / 首尾帧 / 参考素材统一入口。",
			// 核心字段由 VideoCoreFields 生成（见 video_common.go）。本族只写增量，
			// 素材格式规格作为补充说明传进去，不重写整个字段。
			Fields: append(VideoCoreFields(VideoCoreOpts{
				Members:        true,
				SecondsDefault: "5",
				SecondsNote: "可取值随模型（2.5 为 4–30，2.0 系列为 4–15）；超上限直接报错，不截断、不自动降档。" +
					"传 -1 为智能时长（仅声明了该能力的模型支持）。",
				RefNote: "图：jpeg / png / webp / bmp / tiff / gif（2.0 及以上另有 heic / heif），" +
					"宽高比 [0.4, 2.5]、宽高 ∈ [300, 6000]px、单张 ≤ 30MB；" +
					"张数：首帧 1 张、首尾帧 2 张、全模态参考 1–30 张（2.0 系列 1–9 张）。" +
					"音频 wav / mp3、单段 ≤ 15MB（2.5 允许单独传音频，2.0 系列至少要配 1 个图或视频）。",
			}),
				vendor("ratio", TypeString, ReqOptional, "—", "画幅比例（厂商原生写法），等价于平台写法 aspect_ratio。", "Seedance 系列").
					withEnum("16:9", "9:16", "1:1", "4:3", "3:4", "21:9", "adaptive").
					withNote("adaptive = 跟随参考图 / 由任务类型决定。2.5 的首帧 / 首尾帧、视频编辑、视频延长任务只接受 adaptive。"),
				vendor("generate_audio", TypeBoolean, ReqOptional, "true", "是否生成与画面同步的声音。", "Seedance 2.x").
					withNote("默认有声；要静音请显式传 false。"),
				vendor("seed", TypeInteger, ReqOptional, "随机", "随机种子：相同种子生成结果相近（不保证完全一致）。", "部分模型"),
				vendor("camera_fixed", TypeBoolean, ReqOptional, "false", "固定镜头（不做运镜）。", "Seedance 系列"),
				vendor("frames", TypeInteger, ReqOptional, "—", "按帧数指定时长（与 seconds 二选一）。", "Seedance 2.x").
					withNote("与 seconds 同时给时由厂商决定谁生效；需要精确帧数对齐（如二次剪辑）时用它。"),
				vendor("safety_identifier", TypeString, ReqOptional, "—", "终端用户标识，随请求透传给服务方做内容安全溯源。", "Seedance 2.x").
					withNote("填你自己系统里的用户 ID，不要填手机号 / 邮箱等直接身份信息。"),
				vendor("execution_expires_after", TypeInteger, ReqOptional, "—", "任务执行超时（秒），到点由服务方终止。", "部分模型"),
				vendor("omni_reference_task_type", TypeString, ReqOptional, "—", "Omni 系列的参考任务类型。", "Seedance 2.5 Omni").
					withNote("取值由厂商定义，平台原样透传；写错会被服务方拒绝。"),
				vendor("return_last_frame", TypeBoolean, ReqOptional, "false", "是否额外返回成片的尾帧图（jpeg）。", "部分模型").
					withNote("常用来把上一段尾帧接成下一段首帧，连续生成多段。"),
				vendor("output_format", TypeString, ReqOptional, "mp4", "成片容器格式。", "Seedance 2.5").
					withEnum("mp4", "mov").
					withNote("mov 是高色深格式（H.264 + yuv444p + PCM），适合调色 / 抠像，部分播放器不支持。"),
				vendor("draft", TypeBoolean, ReqOptional, "false", "样片模式：先出低清预览。", "Seedance 系列").
					withNote("样片任务 id 与正式任务不同，确认后再提正式任务。"),
				vendor("service_tier", TypeString, ReqOptional, "default", "服务等级：default 在线推理，flex 离线推理。", "部分模型").
					withEnum("default", "flex").
					withNote("flex 配额更高、价格约为在线的一半，但时延不保证；2.5 / 2.0 系列不支持。提交后不可修改。"),
				vendor("priority", TypeInteger, ReqOptional, "0", "排队优先级（越大越优先，只影响排队）。", "Seedance 2.x").
					withLim("min", 0, "0 ~ 9").
					withNote("flex 离线模式不支持优先级。"),
				vendor("tools", TypeArray, ReqOptional, "—", "工具列表（目前只有联网搜索）。", "Seedance 2.x").
					withNote(`写法 [{type:"web_search"}]；实际搜索次数见查询结果的 usage.tool_usage.web_search。`),
				vendor("tools[].type", TypeString, ReqRequired, "—", "工具类型。", "Seedance 2.x").
					withEnum("web_search"),
				vendor("metadata", TypeObject, ReqOptional, "—", "厂商私有参数容器，原样透传并在任务查询中回显。", "Seedance 系列").
					withNote("只下发平台为该模型开放的白名单键；不在白名单里的键不会生效，并在调用记录里给出「未识别参数」告警。"),
			),
			Returns: TaskFlatFields(),
			Response: ResponseSpec{
				Variants: []ResponseVariant{{
					Status: 200, Label: "已受理",
					Returns: TaskFlatFields(),
					Example: TaskExampleFlatAccepted(),
					Notes:   []string{"受理即返回，不代表已开始生成；结果要轮询查询接口。"},
				}},
			},
			Example: Obj{
				{K: "model", V: "doubao-seedance-2-0-260128"},
				{K: "prompt", V: "小猫对着镜头打哈欠，镜头保持不动"},
				{K: "seconds", V: 5},
				{K: "resolution", V: "720p"},
				{K: "ratio", V: "16:9"},
			},
			Example2: Obj{
				{K: "model", V: "doubao-seedance-2-0-260128"},
				{K: "prompt", V: "镜头缓缓推近，海面泛起晨雾"},
				{K: "seconds", V: 5},
				{K: "input_reference", V: []any{
					Obj{{K: "type", V: "first_frame"}, {K: "image_url", V: "https://cdn.example.com/sea.jpg"}},
				}},
				{K: "generate_audio", V: true},
			},
			ExampleTitle2: "图生视频（首帧）",
			Notes: []string{
				VideoCompatNote(),
				"Seedance 的秒数下限是 4 秒（2.5 为 4–30，2.0 为 4–15）：低于下限平台不拦，但生成会失败。",
				"平台不做回调推送：callback_url 只透传，请轮询查询接口。",
				"素材直链按 IP 限流 120 次/分钟：单次提交多张图会并发取图，高并发场景请自行收敛。",
				"要按厂商原生报文提交（content[] 顺序即指代顺序）请用「创建视频任务（原生报文）」那篇。",
			},
			RunMode: RunJSON,
		},
		Presets: []Preset{
			{
				Key: "seedance-t2v", Label: "文生视频", Summary: "只给描述，生成一段视频。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "小猫对着镜头打哈欠，镜头保持不动"},
					{K: "seconds", V: presetTokenSeconds},
					{K: "resolution", V: presetTokenResolution},
				},
				Fields: []string{"model", "prompt", "seconds", "resolution"},
			},
			{
				Key: "seedance-first-last", Label: "首尾帧", Summary: "指定开头与结尾画面。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "从白天过渡到黄昏，运镜保持平稳"},
					{K: "seconds", V: presetTokenSeconds},
					{K: "ratio", V: "adaptive"},
					{K: "input_reference", V: []any{
						Obj{{K: "type", V: "first_frame"}, {K: "image_url", V: "https://cdn.example.com/first.png"}},
						Obj{{K: "type", V: "last_frame"}, {K: "image_url", V: "https://cdn.example.com/last.png"}},
					}},
				},
				Fields: []string{"model", "prompt", "seconds", "ratio", "input_reference"},
				Note:   "首尾帧任务只接受 ratio=adaptive。",
			},
			{
				Key: "seedance-ref-audio", Label: "图片 + 配音", Summary: "参考图 + 音频，让画面跟着声音演。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "人物随音乐轻轻点头，口型贴合"},
					{K: "seconds", V: presetTokenSeconds},
					{K: "input_reference", V: []any{
						Obj{{K: "type", V: "image"}, {K: "image_url", V: "https://cdn.example.com/person.png"}},
						Obj{{K: "type", V: "audio"}, {K: "audio_url", V: "https://cdn.example.com/voice.mp3"}},
					}},
				},
				Fields: []string{"model", "prompt", "seconds", "input_reference"},
				Note:   "2.0 系列至少要配 1 个图或视频，不能只给音频。",
			},
			{
				Key: "seedance-silent", Label: "静音视频", Summary: "关掉音轨，只保留画面。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "街头艺人在雨夜弹唱"},
					{K: "seconds", V: presetTokenSeconds},
					{K: "generate_audio", V: false},
				},
				Fields: []string{"model", "prompt", "seconds", "generate_audio"},
				Note:   "2.x 默认有声，要静音必须显式传 false。",
			},
		},
	},

	// ── 厂商原生报文 ──
	{
		Doc: DocumentKey{Kind: KindVideoSubmit, Variant: VariantJimeng},
		Spec: EndpointDoc{
			Name: "创建视频生成任务（原生报文）",
			Summary: "厂商原生视频报文：素材放在 content[] 里，其余参数与官方文档同名同义。" +
				"只有声明了「原生参数」能力的模型可用；平台只替换 model，其余字节原样转发。",
			Fields: []Field{
				plat("model", TypeString, ReqRequired, "—", "平台模型 ID；出站时替换成厂商侧的真实模型名。").withMembers(),
				std("content", TypeArray, ReqRequired, "—", "输入内容数组（文本 / 图 / 视频 / 音频 / 样片任务混排）。").
					withNote("允许的组合：纯文本；文本 + 图；文本 + 视频；文本 + 图 + 音频；文本 + 图 + 视频；文本 + 视频 + 音频；四者全有；" +
						"或只给样片任务（draft_task）。文本元素必须排第一，厂商按顺序解析指代。"),
				std("content[].type", TypeString, ReqRequired, "—", "这一项是什么。").
					withEnum("text", "image_url", "video_url", "audio_url", "draft_task"),
				std("content[].text", TypeString, ReqConditional, "—", "文本提示词（type=text 时必填）。").
					withReq("type=text 时必填。").
					withNote("字数建议：中文 ≤ 500 字、英文 ≤ 1000 词。"),
				std("content[].image_url.url", TypeString, ReqConditional, "—", "图片地址（type=image_url 时必填）。").
					withReq("type=image_url 时必填。").
					withNote("格式 jpeg / png / webp / bmp / tiff / gif（2.0 及以上另有 heic / heif）；宽高比 [0.4, 2.5]，宽高均 ∈ [300, 6000]px，单张 ≤ 30MB。" +
						"张数：首帧 1 张、首尾帧 2 张、全模态参考 1~30 张（2.0 系列 1~9 张）。"),
				std("content[].video_url.url", TypeString, ReqConditional, "—", "待编辑 / 待延长 / 参考视频地址（type=video_url 时必填）。").
					withReq("type=video_url 时必填。"),
				std("content[].audio_url.url", TypeString, ReqConditional, "—", "参考音频地址（type=audio_url 时必填）。").
					withReq("type=audio_url 时必填。").
					withNote("格式 wav / mp3，单段 ≤ 15MB。"),
				std("content[].draft_task.id", TypeString, ReqConditional, "—", "样片任务 ID（type=draft_task 时必填），有效期 7 天。").
					withReq("type=draft_task 时必填。"),
				std("content[].role", TypeString, ReqOptional, "—", "这一项的用途，决定它被当成首帧、尾帧还是参考素材。").
					withEnum("first_frame", "last_frame", "reference_image", "reference_video", "reference_audio").
					withNote("图生视频-首帧填 first_frame（或不填）；首尾帧再给一个 last_frame；全模态参考生视频填 reference_* 系列。"),
				std("duration", TypeInteger, ReqOptional, "随模型", "视频时长（秒）。").
					withNote("可取值范围随模型；平台不截断、不改写。传 -1 = 让模型自行选时长。与 frames 二选一。"),
				std("frames", TypeInteger, ReqOptional, "—", "按帧数精确指定时长（1.0 pro / pro fast 支持）。").
					withLim("min", 29, "29 ~ 289，且须为 25+4n").
					withNote("帧率 24：133 帧 ≈ 5.54 秒。与 duration 二选一，frames 优先。"),
				std("resolution", TypeString, ReqOptional, "—", "清晰度档位。").
					withEnumFrom(EnumVideoTier).
					withNote("写法原样透传；该模型支持哪些档位以厂商文档为准。"),
				std("ratio", TypeString, ReqOptional, "—", "画幅比例。").
					withEnum("16:9", "9:16", "1:1", "4:3", "3:4", "21:9", "adaptive").
					withNote("adaptive = 按任务类型与素材自动适配。"),
				std("seed", TypeInteger, ReqOptional, "随机", "随机种子：相同种子生成结果相近（不保证完全一致）。").
					withLim("min", -1, "-1 ~ 2147483647（-1 = 随机）"),
				std("camera_fixed", TypeBoolean, ReqOptional, "false", "固定镜头：true 时不做运镜。"),
				std("watermark", TypeBoolean, ReqOptional, "false", "成片右下角是否加「AI 生成」水印。"),
				std("generate_audio", TypeBoolean, ReqOptional, "true", "是否生成与画面同步的声音。").
					withNote("默认有声；要静音请显式传 false。"),
				std("return_last_frame", TypeBoolean, ReqOptional, "false", "是否额外返回成片的尾帧图（jpeg）。").
					withNote("常用来把上一段尾帧接成下一段首帧，连续生成多段。"),
				std("draft", TypeBoolean, ReqOptional, "false", "样片模式：先出低清预览。").
					withNote("样片的任务 ID 与正式任务不同，确认后再用 draft_task 提交正式视频。"),
				std("service_tier", TypeString, ReqOptional, "default", "服务等级。").
					withEnum("default", "flex").
					withNote("flex 是离线推理：配额更高、价格约为在线的一半，但时延不保证；2.5 / 2.0 系列不支持。"),
				std("priority", TypeInteger, ReqOptional, "0", "排队优先级（数值越大越靠前）。").
					withLim("min", 0, "0 ~ 9"),
				std("tools", TypeArray, ReqOptional, "—", "工具列表（目前只有联网搜索）。").
					withNote(`写法 [{type:"web_search"}]；实际搜索次数见查询结果的 usage.tool_usage.web_search。`),
				std("tools[].type", TypeString, ReqRequired, "—", "工具类型。").
					withEnum("web_search"),
				std("output_format", TypeString, ReqOptional, "mp4", "成片容器格式。").
					withEnum("mp4", "mov").
					withNote("mov 是高色深格式（H.264 + yuv444p + PCM），适合调色 / 抠像 / 合成，部分播放器不支持。2.5 支持。"),
				std("callback_url", TypeString, ReqOptional, "—", "回调地址。").
					withNote("本平台不接收回调：这个地址会原样发给厂商，但你不会在本平台收到推送，请轮询任务查询接口。"),
				std("execution_expires_after", TypeInteger, ReqOptional, "172800", "任务过期阈值（秒），从提交时间起算。").
					withLim("min", 3600, "3600 ~ 259200").
					withNote("超过即被厂商终止并标记 expired。平台自己的超时判据更短、会先生效，一般不必填。"),
				std("safety_identifier", TypeString, ReqOptional, "—", "终端用户标识，用于厂商侧合规检测。").
					withNote("传哈希后的用户标识，不要传原始手机号 / 邮箱；≤ 64 字符。"),
			},
			Returns: []Field{
				ret("id", TypeString, "任务号。"),
			},
			Example: Obj{
				{K: "model", V: "doubao-seedance-1-0-pro-250528"},
				{K: "content", V: []any{
					Obj{{K: "type", V: "text"}, {K: "text", V: "小猫对着镜头打哈欠"}},
				}},
				{K: "resolution", V: "720p"},
				{K: "ratio", V: "16:9"},
				{K: "duration", V: 5},
				{K: "watermark", V: false},
			},
			Example2: Obj{
				{K: "model", V: "doubao-seedance-2-0-260128"},
				{K: "content", V: []any{
					Obj{{K: "type", V: "text"}, {K: "text", V: "镜头缓缓推近，海面泛起晨雾"}},
					Obj{{K: "type", V: "image_url"}, {K: "image_url", V: Obj{{K: "url", V: "https://cdn.example.com/sea.jpg"}}}, {K: "role", V: "first_frame"}},
				}},
				{K: "ratio", V: "adaptive"},
				{K: "duration", V: -1},
				{K: "generate_audio", V: true},
			},
			ExampleTitle2: "图生视频（首帧 + 智能时长）",
			Notes: []string{
				"这篇是厂商原生报文：平台只替换 `model`，其余字节原样转发 —— 因此这类任务**不支持重试**（重新拼装会丢掉素材），失败请重新提交。",
				"素材必须是公网 http(s) 直链、data: 内联或厂商素材 ID；平台只读 mp4 头尾索引取参考视频时长，读不到会直接 400。",
				"厂商对未知字段的处置不可控（400 或忽略），所以平台白名单外的顶层键不会出站，并在调用记录里给出「未识别参数」告警。",
			},
			RunMode: RunJSON,
		},
		Presets: []Preset{
			{
				Key: "seedance-native", Label: "原生参数", Summary: "按厂商原始写法提交，字段一个不改。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "content", V: []any{
						Obj{{K: "type", V: "text"}, {K: "text", V: "小猫对着镜头打哈欠，镜头保持不动"}},
					}},
					{K: "duration", V: -1},
					{K: "ratio", V: "adaptive"},
					{K: "resolution", V: presetTokenResolution},
				},
				Fields: []string{"model", "content", "duration", "ratio", "resolution"},
				Note:   "content[] 原样转发，字段与厂商文档一致；此类任务不支持重试。",
			},
		},
	},
}
