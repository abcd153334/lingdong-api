package apidoc

// SUNO 族（音乐 / 歌词）的接口文档。
//
// 两套入口，一套能力：
//   - `/suno/*` 是 new-api 的 SUNO 协议面（完全对标：`{code,message,data}` 信封、
//     提交时 data 是任务号字符串）—— 已经按 new-api 写好的客户端只改 base_url 就能用；
//   - `/v1/audio/*` 是平台原生入口（裸任务对象，与 `/v1/tasks` 同一套形状）。
//
// 为什么两套都要写、且字段必须同源：只写一套时另一套就成了"没人知道它存在"的
// 隐藏接口；而两套各写一份字段表，迟早出现"按 A 页传的字段在 B 页无效"。
// 字段一致性由 `music_doc_gate_test.go` 的 `TestSunoAndNativeFieldsAreSameSource` 守。
//
// 上游事实（`API文档/多米API/SUNO AI音乐`）：提交是 multipart 且 `custom_mode` /
// `make_instrumental` 必填；查询 `GET /api/suno/feed?task_id=`；一首歌两次回调
// （status=1 已可展示、status=3 出 mp3）。平台把它们收敛成 JSON + 人话字段，
// 由适配器补齐开关 —— 因此调用方看不到 multipart，也看不到 `custom_mode` 的必填性。

// sunoTaskFields 是 `/suno/fetch` 任务对象的字段（两页共用，单一来源）。
//
// 与 `/v1/tasks` 的 TaskFlatFields 不是同一份：那一份是"没有内层 data"的扁平对象，
// 而 SUNO 客户端要读的是 `data[].audio_url`。共用一份就会有一边缺字段。
func sunoTaskFields() []Field {
	return []Field{
		ret("created_at", TypeInteger, "任务创建时间（Unix 秒）。"),
		ret("updated_at", TypeInteger, "最近一次状态变化时间（Unix 秒）。"),
		ret("task_id", TypeString, "任务号（`task_` 开头），查询与下载都用它。"),
		ret("platform", TypeString, "平台标识，固定为 `lingdong`。"),
		ret("status", TypeString, "任务状态：SUBMITTED / IN_PROGRESS / SUCCESS / FAILURE。").
			withEnum("SUBMITTED", "IN_PROGRESS", "SUCCESS", "FAILURE").
			withNote("不会出现 NOT_START / QUEUED：平台把排队与提交都归到 SUBMITTED。"),
		ret("fail_reason", TypeString, "失败原因（成功时为空串）。"),
		ret("submit_time", TypeInteger, "提交时间（Unix 秒），与 created_at 同值。"),
		ret("finish_time", TypeInteger, "完成时间（Unix 秒）；非终态为 0。"),
		ret("progress", TypeString, "进度文案：0% / 50% / 100%。"),
		ret("action", TypeString, "动作：music / lyrics（决定内层 data 的形状）。").
			withEnum("music", "lyrics"),
		ret("data", TypeArray, "成功时的交付内容：music 是音频数组，lyrics 是歌词对象；"+
			"非终态或失败为 null。"),
	}
}

// sunoClipFields 是 music 任务内层 `data[]` 的字段。
func sunoClipFields() []Field {
	return []Field{
		ret("id", TypeString, "交付项标识（`<任务号>:<序号>`）。"),
		ret("audio_url", TypeString, "音频地址：平台能力地址，可直接下载。"),
		ret("image_url", TypeString, "封面地址。平台不转存封面，恒为空串。"),
		ret("image_large_url", TypeString, "大图封面地址。同上，恒为空串。"),
		ret("video_url", TypeString, "视频版地址。平台不转存视频版，恒为空串。"),
		ret("status", TypeString, "交付项状态：complete / streaming / error。"),
		ret("title", TypeString, "歌曲标题：自定义模式下回显请求里的 `title`；"+
			"平台不采集模型自动起的名字，未传时为空串。"),
		ret("text", TypeString, "该曲对应的歌词（请求里给了歌词时）。"),
		ret("model_name", TypeString, "本次使用的模型版本：请求里的 `mv`；"+
			"未传时为平台模型 ID（通常就是版本名）。"),
		ret("major_model_version", TypeString, "与 `model_name` 同值（兼容键）。"),
		ret("metadata", TypeObject, "元信息：tags / prompt / gpt_description_prompt；"+
			"duration 等平台不采集的字段恒为 null。"),
	}
}

// sunoEnvelopeHead 是 `{code,message,data}` 信封的头三行（三页共用）。
//
// dataType 必须由调用方给：提交回执的 `data` 是**字符串**任务号，单查是**对象**，
// 批量是**数组**。三页共用一个写死的 TypeArray，会让提交页把任务号画成一个列表。
func sunoEnvelopeHead(dataType Type, dataDesc string) []Field {
	return []Field{
		ret("code", TypeString, "成功固定为 `success`；失败见错误码一节。"),
		ret("message", TypeString, "附加信息，通常为空串。"),
		ret("data", dataType, dataDesc),
	}
}

// sunoNest 给一组返回字段加前缀（`data.` 或 `data[].`）。
//
// 页面上必须能看出「这层字段在信封的第几层」：把 `task_id` 与 `code` 平铺成同级，
// 读者会按 `{"code":...,"task_id":...}` 去解析，而真实报文里它俩差两层。
func sunoNest(fields []Field, prefix string) []Field {
	out := make([]Field, 0, len(fields))
	for _, f := range fields {
		f.Name = prefix + f.Name
		out = append(out, f)
	}
	return out
}

// sunoSubmitExample 是提交回执示例（data 是**字符串**任务号）。
func sunoSubmitExample() Obj {
	return Obj{
		{K: "code", V: "success"},
		{K: "message", V: ""},
		{K: "data", V: "task_01M7H9Z3QK8R2V5X7C4B6N1P0D"},
	}
}

// sunoFetchExample 是单查示例（内层 data 是音频数组）。
func sunoFetchExample() Obj {
	return Obj{
		{K: "code", V: "success"},
		{K: "message", V: ""},
		{K: "data", V: Obj{
			{K: "created_at", V: 1794818458},
			{K: "updated_at", V: 1794818572},
			{K: "task_id", V: "task_01M7H9Z3QK8R2V5X7C4B6N1P0D"},
			{K: "platform", V: "lingdong"},
			{K: "status", V: "SUCCESS"},
			{K: "fail_reason", V: ""},
			{K: "submit_time", V: 1794818458},
			{K: "finish_time", V: 1794818572},
			{K: "progress", V: "100%"},
			{K: "action", V: "music"},
			{K: "data", V: []Obj{
				{
					{K: "id", V: "task_01M7H9Z3QK8R2V5X7C4B6N1P0D:0"},
					{K: "audio_url", V: "https://api.example.com/v1/tasks/task_01M7H9Z3QK8R2V5X7C4B6N1P0D/artifacts/0/content"},
					{K: "image_url", V: ""},
					{K: "video_url", V: ""},
					{K: "status", V: "complete"},
					{K: "title", V: "还在路上"},
					{K: "text", V: "[Verse]\n路灯把影子拉长"},
					{K: "metadata", V: Obj{
						{K: "tags", V: "民谣,温暖"},
						{K: "prompt", V: "轻快的民谣，木吉他为主"},
						{K: "duration", V: nil},
					}},
				},
			}},
		}},
	}
}

// sunoLyricsExample 是歌词任务单查示例（内层 data 是**单个对象**）。
func sunoLyricsExample() Obj {
	return Obj{
		{K: "code", V: "success"},
		{K: "message", V: ""},
		{K: "data", V: Obj{
			{K: "task_id", V: "task_01M7HAB4QW3E8R6T2Y9U5I7O1P"},
			{K: "status", V: "SUCCESS"},
			{K: "action", V: "lyrics"},
			{K: "data", V: Obj{
				{K: "id", V: "task_01M7HAB4QW3E8R6T2Y9U5I7O1P:0"},
				{K: "status", V: "complete"},
				{K: "title", V: "深夜加班"},
				{K: "text", V: "[Verse]\n打卡机响过十二点"},
			}},
		}},
	}
}

var docsMusicSuno = []FamilyEndpoint{
	// ── 1. /suno/submit/{action}（SUNO 兼容，主入口）──
	{
		Doc: DocumentKey{Kind: KindSunoSubmit},
		Spec: EndpointDoc{
			Name: "提交音乐 / 歌词（SUNO 兼容）",
			Summary: "一次调用提交作曲或写词任务，立即返回任务号字符串；" +
				"用 `/suno/fetch/{task_id}` 查结果。",
			Fields: []Field{
				pathParam(plat("action", TypeString, ReqRequired, "—",
					"动作：`music` 作曲、`lyrics` 写词（大小写不敏感）。").
					withEnum("music", "lyrics")),
				plat("model", TypeString, ReqOptional, "自动选当前在售模型",
					"平台模型 ID（与 `/v1/models` 一致）。").
					withMembers().
					withNote("不传时按动作取当前在售的默认模型；也可以传 `suno_music` / `suno_lyrics` 让平台按动作选。"),
				plat("prompt", TypeString, ReqConditional, "—",
					"音乐：风格 / 主题描述；写词：写词要求。").
					withReq("music 的非自定义模式必填；lyrics 与 `gpt_description_prompt` 二选一。").
					withLim("max_len", 4000, "≤ 4000 字符"),
				alias("gpt_description_prompt", TypeString, ReqConditional, "—",
					"`prompt` 的另一个写法（两种写法都收）。").
					withReq("与 `prompt` 二选一。").
					withNote("与 `prompt` 同时给时以 `prompt` 为准。"),
				plat("lyrics", TypeString, ReqConditional, "—", "自定义歌词。").
					withReq("`custom_mode=true` 时必填（或由 `prompt` 充当歌词）。").
					withLim("max_len", 4000, "≤ 4000 字符").
					withNote("填了本字段即进入自定义模式。分段用 [Verse] / [Chorus] 这类标签。"),
				plat("custom_mode", TypeBoolean, ReqOptional, "false",
					"是否自定义填词模式。").
					withNote("为 true 但没给 `lyrics` 时，平台把 `prompt` 当歌词；" +
						"false 时 tags / title 不生效。"),
				plat("make_instrumental", TypeBoolean, ReqOptional, "false", "纯音乐（不唱）。").
					withNote("与 `lyrics` 互斥，同时给直接报错；为 true 时 `prompt` 只作风格描述。"),
				vendor("tags", TypeString, ReqOptional, "—", "风格标签，仅自定义模式生效。", "SUNO").
					withLim("max_len", 500, "≤ 500 字符"),
				vendor("title", TypeString, ReqOptional, "—", "歌曲标题，仅自定义模式生效。", "SUNO").
					withLim("max_len", 200, "≤ 200 字符"),
				std("mv", TypeString, ReqOptional, "模型默认版本",
					"模型版本串（原样随请求下发）。").
					withNote("不传时用平台为该模型配置的版本；换版本只影响音色与结构，不改变计价。"),
			},
			Returns: sunoEnvelopeHead(TypeString,
				"成功时是**任务号字符串**（`task_` 开头），不是对象。"),
			Response: ResponseSpec{
				Variants: []ResponseVariant{{
					Status: 200, Label: "已受理",
					Returns: sunoEnvelopeHead(TypeString, "任务号字符串。"),
					Example: sunoSubmitExample(),
					Notes:   []string{"`data` 是字符串而不是对象：直接把它当任务号用。"},
				}},
			},
			Example: Obj{
				{K: "model", V: "chirp-hawk"},
				{K: "prompt", V: "轻快的民谣，木吉他为主，男声"},
				{K: "make_instrumental", V: false},
			},
			Example2: Obj{
				{K: "model", V: "chirp-hawk"},
				{K: "custom_mode", V: true},
				{K: "prompt", V: "[Verse]\n路灯把影子拉长\n[Chorus]\n我们还在路上"},
				{K: "tags", V: "民谣,温暖"},
				{K: "title", V: "还在路上"},
			},
			ExampleTitle2: "自定义歌词",
			MaxBody:       "1MB",
			Notes: []string{
				"只接受 `application/json`：平台负责把它转成提交表单，不需要 multipart。",
				"计费按**实际交付条数**：一次作曲通常出 2 首，收 2 条的钱；只出 1 首就只收 1 条；" +
					"一首都没交付则整单不计费。写词按 1 篇计。",
				"不提供回调：`notify_hook` / `callback_url` 会直接报错，请轮询 `/suno/fetch/{task_id}`。",
				"暂不支持续写：`continue_clip_id` / `continue_at` 会直接报错，不会静默忽略。",
				"`Idempotency-Key` 请求头可用：同一个键重复提交只会建一单（与原生入口共用同一个幂等域）。",
			},
			RunMode: RunJSON,
		},
		Presets: []Preset{
			{
				Key: "suno-compat-style", Label: "风格作曲", Summary: "给风格，生成完整歌曲。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "轻快的民谣，木吉他为主，男声"},
					{K: "make_instrumental", V: false},
				},
				Fields: []string{"model", "prompt", "make_instrumental"},
			},
			{
				Key: "suno-compat-lyrics", Label: "自定义歌词", Summary: "带歌词，控制段落与主题。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "custom_mode", V: true},
					{K: "prompt", V: "[Verse]\n路灯把影子拉长\n[Chorus]\n我们还在路上"},
					{K: "tags", V: "民谣,温暖"},
					{K: "title", V: "还在路上"},
				},
				Fields: []string{"model", "custom_mode", "prompt", "tags", "title"},
			},
		},
	},

	// ── 2. /suno/fetch/{task_id}（单查）──
	{
		Doc: DocumentKey{Kind: KindSunoFetch},
		Spec: EndpointDoc{
			Name:    "查询任务（SUNO 兼容）",
			Summary: "按任务号查询状态与交付内容（音频数组或歌词对象）。",
			Fields: []Field{
				pathParam(plat("task_id", TypeString, ReqRequired, "—",
					"提交接口返回的任务号（`task_` 开头）。")),
			},
			Returns: append(sunoEnvelopeHead(TypeObject,
				"任务对象，自身再带一层 `data`（music 是音频数组、lyrics 是歌词对象）。"),
				sunoNest(sunoTaskFields(), "data.")...),
			Response: ResponseSpec{
				Variants: []ResponseVariant{{
					Status: 200, Label: "任务对象",
					Returns: append(sunoEnvelopeHead(TypeObject, "任务对象。"),
						sunoNest(sunoTaskFields(), "data.")...),
					Example: sunoFetchExample(),
					Notes: []string{
						"未完成时 `data` 为 null，`status` 为 SUBMITTED / IN_PROGRESS。",
						"成功时 `data` 是音频数组，每项含 `audio_url`（平台能力地址）。",
					},
				}},
			},
			ExampleTitle2: "任务号",
			Notes: []string{
				"任务号只属于创建它的 API Key：换一把 Key 查询会得到 `task_not_found`。",
				"建议 5~10 秒轮询一次；`status` 进入 SUCCESS / FAILURE 后停止。",
				"`action=lyrics` 时内层 `data` 是**单个对象**（含 `text`），不是数组。",
			},
			RunMode: RunJSON,
		},
	},

	// ── 3. /suno/fetch（批量）──
	{
		Doc: DocumentKey{Kind: KindSunoFetchBatch},
		Spec: EndpointDoc{
			Name:    "批量查询任务（SUNO 兼容）",
			Summary: "一次查询最多 100 个任务，返回顺序与入参一致。",
			Fields: []Field{
				plat("ids", TypeArray, ReqRequired, "—", "任务号数组，1–100 个。").
					withReq("必填，且不能为空。").
					withNote("超过 100 个直接报错，请分批查询。"),
			},
			Returns: append(sunoEnvelopeHead(TypeArray, "任务对象数组，顺序与 `ids` 一致。"),
				sunoNest(sunoTaskFields(), "data[].")...),
			Response: ResponseSpec{
				Variants: []ResponseVariant{{
					Status: 200, Label: "任务对象数组",
					Returns: append(sunoEnvelopeHead(TypeArray, "任务对象数组。"),
						sunoNest(sunoTaskFields(), "data[].")...),
					Example: Obj{
						{K: "code", V: "success"},
						{K: "message", V: ""},
						{K: "data", V: []Obj{
							{{K: "task_id", V: "task_01M7H9Z3QK8R2V5X7C4B6N1P0D"},
								{K: "status", V: "SUCCESS"},
								{K: "action", V: "music"}},
						}},
					},
				}},
			},
			Example: Obj{
				{K: "ids", V: []string{"task_01M7H9Z3QK8R2V5X7C4B6N1P0D"}},
			},
			MaxBody: "256KB",
			Notes: []string{
				"`ids` 里任何一个任务不存在或不属于当前 Key，整批返回 `task_not_found`（不返回部分结果）——" +
					"部分成功会让调用方分不清「还在排队」与「不是我的」。",
				"每个元素与单查接口返回的任务对象完全同构。",
			},
			RunMode: RunJSON,
		},
	},

	// ── 4. 原生入口：提交音乐 ──
	{
		Doc: DocumentKey{Kind: KindAudioMusic},
		Spec: EndpointDoc{
			Name: "原生入口：提交音乐",
			Summary: "平台原生入口，返回裸任务对象（与 `/v1/tasks/{id}` 同一形状）。" +
				"按 SUNO 兼容写法接入的客户端请用 `/suno/submit/music`。",
			Fields: []Field{
				plat("model", TypeString, ReqRequired, "—", "音乐模型（按模态校验）。").withMembers(),
				plat("prompt", TypeString, ReqConditional, "—", "风格 / 主题描述。").
					withReq("非自定义模式下必填。").
					withLim("max_len", 4000, "≤ 4000 字符").
					withNote("自定义模式下 prompt 会被当作歌词，请改用 lyrics。"),
				plat("lyrics", TypeString, ReqConditional, "—", "自定义歌词。").
					withReq("自定义模式下必填（或由 prompt 充当歌词）。").
					withLim("max_len", 4000, "≤ 4000 字符").
					withNote("填了本字段即自动进入自定义模式。分段用 [Verse] / [Chorus] 这类标签。"),
				plat("instrumental", TypeBoolean, ReqOptional, "false", "纯音乐（不唱）。").
					withNote("与 lyrics 互斥，同时给直接报错。"),
				plat("custom_mode", TypeBoolean, ReqOptional, "平台推断", "显式覆盖自定义模式。").
					withNote("不填时由平台推断：给了 lyrics 就是自定义模式。"),
				vendor("tags", TypeString, ReqOptional, "—", "风格标签，仅自定义模式生效。", "SUNO").
					withLim("max_len", 500, "≤ 500 字符"),
				vendor("title", TypeString, ReqOptional, "—", "歌曲标题，仅自定义模式生效。", "SUNO").
					withLim("max_len", 200, "≤ 200 字符"),
				std("mv", TypeString, ReqOptional, "模型默认版本",
					"模型版本串（原样随请求下发）。"),
			},
			Returns: TaskFlatFields(),
			Response: ResponseSpec{
				Variants: []ResponseVariant{{
					Status: 200, Label: "已受理",
					Returns: TaskFlatFields(),
					Example: TaskExampleFlatAcceptedFor("chirp-hawk"),
					Notes:   []string{"一次任务通常出 2 首，产物清单里按 `key` 排列。"},
				}},
			},
			Example: Obj{
				{K: "model", V: "chirp-hawk"},
				{K: "prompt", V: "轻快的民谣，木吉他为主，男声"},
			},
			Example2: Obj{
				{K: "model", V: "chirp-hawk"},
				{K: "lyrics", V: "[Verse]\n路灯把影子拉长\n[Chorus]\n我们还在路上"},
				{K: "tags", V: "民谣,温暖"},
				{K: "title", V: "还在路上"},
			},
			ExampleTitle2: "自定义歌词",
			MaxBody:       "1MB",
			Notes: []string{
				"本页是原生入口；字段与 `/suno/submit/music` 同源，只有响应形状不同。",
				"产物按实际成功条数转存并计费：只出 1 首就只存 1 首、只收 1 条；整轮一首都没成功则全额退回。",
				"instrumental=true 与 lyrics 互斥：同时给会报错，不会静默丢掉歌词。",
				"封面图不是产物，不会出现在产物列表里。",
				"只接受 application/json：平台负责把它转成提交表单。",
			},
			RunMode: RunJSON,
		},
		Presets: []Preset{
			{
				Key: "suno-style", Label: "风格作曲", Summary: "给风格，生成完整歌曲。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "轻快的民谣，木吉他为主，男声"},
					{K: "instrumental", V: false},
				},
				Fields: []string{"model", "prompt", "instrumental"},
			},
			{
				Key: "suno-lyrics", Label: "自定义歌词", Summary: "带歌词，控制段落与主题。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "lyrics", V: "[Verse]\n路灯把影子拉长\n[Chorus]\n我们还在路上"},
					{K: "tags", V: "民谣,温暖"},
					{K: "title", V: "还在路上"},
				},
				Fields: []string{"model", "lyrics", "tags", "title"},
				Note:   "填了 lyrics 即进入自定义模式。",
			},
			{
				Key: "suno-instrumental", Label: "纯音乐", Summary: "只要配乐，不要人声。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "舒缓的钢琴独奏，雨夜氛围"},
					{K: "instrumental", V: true},
				},
				Fields: []string{"model", "prompt", "instrumental"},
				Note:   "与 lyrics 互斥：给了歌词就不能是纯音乐。",
			},
		},
	},

	// ── 5. 原生入口：提交歌词 ──
	{
		Doc: DocumentKey{Kind: KindAudioLyrics},
		Spec: EndpointDoc{
			Name: "原生入口：提交歌词",
			Summary: "平台原生入口，返回裸任务对象；产物是一篇纯文本。" +
				"按 SUNO 兼容写法接入的客户端请用 `/suno/submit/lyrics`。",
			Fields: []Field{
				plat("model", TypeString, ReqRequired, "—", "歌词模型。").withMembers(),
				plat("prompt", TypeString, ReqConditional, "—", "写词要求（主题 / 情绪 / 段落结构）。").
					withReq("与 lyrics 二选一。").
					withLim("max_len", 2000, "≤ 2000 字符"),
				alias("lyrics", TypeString, ReqConditional, "—", "prompt 的别名。").
					withReq("与 prompt 二选一。").
					withNote("两者都填时以 prompt 为准。"),
			},
			Returns: TaskFlatFields(),
			Response: ResponseSpec{
				Variants: []ResponseVariant{{
					Status: 200, Label: "已受理",
					Returns: TaskFlatFields(),
					Example: TaskExampleFlatAcceptedFor("suno-lyrics"),
				}},
			},
			Example: Obj{
				{K: "model", V: "suno-lyrics"},
				{K: "prompt", V: "写一首关于深夜加班的城市民谣，两段主歌一段副歌"},
			},
			MaxBody: "1MB",
			Notes: []string{
				"长度按字符数校验（中文 2000 字也放行），不是按字节。",
				"产物是文本而不是链接：用产物下载接口读正文，不要因为「没有 url 字段」就判定失败。",
				"返回完成但正文为空时判定失败并自动退款。",
				"计费按 1 篇计（与作曲的按条计费同一口径）。",
			},
			RunMode: RunJSON,
		},
		Presets: []Preset{
			{
				Key: "suno-lyrics-write", Label: "写歌词", Summary: "给主题，出一首完整歌词。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "prompt", V: "写一首关于深夜加班的城市民谣，两段主歌一段副歌"},
				},
				Fields: []string{"model", "prompt"},
			},
		},
	},
}
