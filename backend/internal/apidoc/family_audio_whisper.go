package apidoc

// 语音识别族（OpenAI 语音协议：转写 / 翻译）的接口文档。
//
// 依据：OpenAI 官方 audio API（`/v1/audio/transcriptions`、`/v1/audio/translations`）。
// 两个端点都是 multipart，字段集几乎相同，差别只有 `language`：翻译端点固定译成英文，
// 因此不接受 `language`（官方明确把它列为 transcribe 专有）。
//
// 这一族当前没有在售模型，但两个路由真实存在、真实可用（配好渠道即可），
// 所以文档必须存在 —— 「路由存在、文档查不到」对读者是一条死路。

var docsAudioWhisper = []FamilyEndpoint{
	{
		Doc: DocumentKey{Kind: KindAudioTranscriptions},
		Spec: EndpointDoc{
			Name:    "语音转写",
			Summary: "上传录音返回识别文字（multipart 表单）。",
			Fields: []Field{
				formParam(plat("model", TypeString, ReqRequired, "—", "识别模型。").withMembers()),
				formParam(std("file", TypeFile, ReqRequired, "—", "录音文件。")).
					withNote("mp3 / mp4 / mpeg / mpga / m4a / wav / webm。").
					withLim("max", 25, "≤ 25MB"),
				formParam(std("language", TypeString, ReqOptional, "自动检测", "音频语言，ISO-639-1（如 zh / en）。")).
					withNote("指定语言能明显提升准确率与速度；不传由模型自动检测。"),
				formParam(std("prompt", TypeString, ReqOptional, "—", "提示词：先给专有名词、术语表或上下文，能提高识别准确率。")),
				formParam(std("response_format", TypeString, ReqOptional, "json", "结果格式。")).
					withEnum("json", "text", "srt", "vtt", "verbose_json").
					withNote("srt / vtt 直接出字幕；verbose_json 带分句与时间戳；text 只返回纯文本。"),
				formParam(std("temperature", TypeNumber, ReqOptional, "0", "采样温度，0 ~ 1。")).
					withLim("min", 0, "0 ~ 1"),
				formParam(std("timestamp_granularities[]", TypeArray, ReqOptional, "segment", "时间戳粒度：word / segment。")).
					withEnum("word", "segment").
					withNote("只在 response_format=verbose_json 时生效。"),
			},
			Returns: []Field{
				ret("text", TypeString, "识别出的文字。"),
				ret("language", TypeString, "识别到的语言（verbose_json）。"),
				ret("duration", TypeNumber, "音频时长，秒（verbose_json）。"),
				ret("segments", TypeArray, "分句与时间戳（verbose_json）。"),
				ret("segments[].start", TypeNumber, "该句开始时间（秒）。"),
				ret("segments[].end", TypeNumber, "该句结束时间（秒）。"),
				ret("segments[].text", TypeString, "该句文字。"),
				ret("words", TypeArray, "逐词时间戳（timestamp_granularities 含 word 时）。"),
			},
			Response: ResponseSpec{
				Notes: []string{
					"response_format=text / srt / vtt 时响应体**不是 JSON**，直接是文本或字幕文件。",
				},
			},
			MaxBody: "25MB",
			RunMode: RunJSON,
		},
		Presets: []Preset{
			{
				Key: "whisper-transcribe", Label: "转写文字", Summary: "上传录音，拿回文字。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "response_format", V: "json"},
				},
				Fields:  []string{"model", "file", "response_format"},
				Uploads: []string{"file"},
				Note:    "表单提交（multipart），不走 JSON。",
			},
			{
				Key: "whisper-subtitle", Label: "出字幕", Summary: "直接生成 SRT 字幕文件。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "response_format", V: "srt"},
				},
				Fields:  []string{"model", "file", "response_format"},
				Uploads: []string{"file"},
				Note:    "响应体是字幕文本，不是 JSON。",
			},
		},
	},

	{
		Doc: DocumentKey{Kind: KindAudioTranslations},
		Spec: EndpointDoc{
			Name:    "语音翻译",
			Summary: "录音直接翻译成英文，参数与语音转写基本相同（不接受 language）。",
			Fields: []Field{
				formParam(plat("model", TypeString, ReqRequired, "—", "模型。").withMembers()),
				formParam(std("file", TypeFile, ReqRequired, "—", "录音文件。")).
					withNote("mp3 / mp4 / mpeg / mpga / m4a / wav / webm；≤ 25MB。"),
				formParam(std("prompt", TypeString, ReqOptional, "—", "提示词，同语音转文字。")),
				formParam(std("response_format", TypeString, ReqOptional, "json", "结果格式，同语音转文字。")).
					withEnum("json", "text", "srt", "vtt", "verbose_json"),
				formParam(std("temperature", TypeNumber, ReqOptional, "0", "采样温度，0 ~ 1。")).
					withLim("min", 0, "0 ~ 1"),
			},
			Returns: []Field{
				ret("text", TypeString, "英文译文。"),
			},
			MaxBody: "25MB",
			Notes: []string{
				"官方把这个端点定义为「任意语言 → 英文」，所以没有 language 参数：传了不会生效。",
			},
			RunMode: RunJSON,
		},
		Presets: []Preset{
			{
				Key: "whisper-translate", Label: "翻译成英文", Summary: "上传录音，拿回英文译文。",
				Body: Obj{
					{K: "model", V: PresetModelToken},
					{K: "response_format", V: "json"},
				},
				Fields:  []string{"model", "file", "response_format"},
				Uploads: []string{"file"},
				Note:    "表单提交（multipart），不走 JSON。",
			},
		},
	},
}
