package apidoc

import "strings"

// 本文件是**端点字段契约的唯一真相源**（L1 的字段维度）。
//
// 改动本文件的四条铁律：
//  1. 不写价格、不写秒单价、不写档位单价 —— 那些是 models.spec_prices 的事实，
//     由 project.go 从 billing.SpecFacts 注入。写死必在改价后失真。
//  2. 不写任何上游/渠道/厂商内部键。厂商专属的**对外**字段标 TierVendor 即可，
//     Desc 里写清「只对哪些模型有效」。
//  3. 枚举若随模型变化，必须把 EnumSource 标成 image_tier / video_tier 而不是 static：
//     judge.go 用 EnumSource.IsModelDriven() 判定「这个模型是否需要独立文档」，
//     标错会让全部模型被判成需要（判定失效）或都不需要（用户看不到档位）。
//  4. Name 是**投影后的字段路径**（`input_reference[].type`、`metadata.aigc_watermark`），
//     它同时是 model_docs.field_notes 的 key 校验依据、运行面板锚点、Markdown 表格列。
//     写法和别处不一致 = 运营写的字段补充说明永远命不中（V4 直接拒绝保存）。

// ── 构造助手 ──
//
// 为什么用助手而不是把结构体字面量写满：一个字段有 12 个可填项，42 个端点几百个字段，
// 字面量写法会让「必填/默认值/说明」淹没在样板里，review 时看不出哪条真的改了。
// 助手把 90% 的字段压成一行，把注意力留给真正有差异的约束与枚举。

func plat(name string, t Type, req Required, def, desc string) Field {
	return Field{Name: name, Type: t, In: InBody, Required: req, Default: def, Desc: desc, Tier: TierPlatform}
}

func std(name string, t Type, req Required, def, desc string) Field {
	return Field{Name: name, Type: t, In: InBody, Required: req, Default: def, Desc: desc, Tier: TierStandard}
}

func vendor(name string, t Type, req Required, def, desc, onlyModels string) Field {
	return Field{Name: name, Type: t, In: InBody, Required: req, Default: def, Desc: desc,
		Tier: TierVendor, OnlyModels: onlyModels}
}

// stdOnly 是「OpenAI 官方字段，但只有部分模型认」。
//
// 为什么不能只写成 std 或只写成 vendor：
//   - 写成 std（不带适用范围），读者会以为每个图片模型都吃 `output_compression`
//     —— Seedream 只认 png/jpeg，传了不生效；
//   - 写成 vendor（第三方扩展），会把它归到「厂商私有字段」里，
//     读者以为要换一家厂商才能用，而它其实是 OpenAI 原生能力。
//
// 分层（Tier）表示「这是谁定的字段」，适用范围（OnlyModels）表示「谁认它」，
// 两件事分开表达才不会对任一侧说谎。
func stdOnly(name string, t Type, req Required, def, desc, onlyModels string) Field {
	return Field{Name: name, Type: t, In: InBody, Required: req, Default: def, Desc: desc,
		Tier: TierStandard, OnlyModels: onlyModels}
}

func alias(name string, t Type, req Required, def, desc string) Field {
	return Field{Name: name, Type: t, In: InBody, Required: req, Default: def, Desc: desc, Tier: TierAlias}
}

// pathParam / queryParam 把字段位置切到 path / query（body 是默认）。
func pathParam(f Field) Field  { f.In = InPath; return f }
func queryParam(f Field) Field { f.In = InQuery; return f }
func formParam(f Field) Field  { f.In = InForm; return f }

// withNote 往字段说明里**追加一段**（空行分段）。
//
// 名字保留是因为播种代码里到处都在用它，但语义已经变了：字段说明只有一列文字，
// 不再有「说明 / 补充说明 / 分组说明」三层。多段就是多行，前台按段落显示，
// 后台是一个多行文本框 —— 与读者看到的完全一致。
func (f Field) withNote(s string) Field {
	s = strings.TrimSpace(s)
	if s == "" {
		return f
	}
	if strings.TrimSpace(f.Desc) == "" {
		f.Desc = s
		return f
	}
	f.Desc = strings.TrimSpace(f.Desc) + "\n" + s
	return f
}

// withEnum 标静态枚举。
func (f Field) withEnum(v ...string) Field { f.Enum = v; f.EnumSource = EnumStatic; return f }

// withEnumFrom 标「枚举来自模型事实」。
func (f Field) withEnumFrom(src EnumSource) Field { f.EnumSource = src; return f }

// passthrough 标「平台只把该字段原样转发给模型提供方，不做校验」。
func (f Field) passthrough() Field { f.Passthrough = true; return f }

// withMembers 标「枚举 = 本篇文档的全部成员模型 ID」。
//
// 只用于 `model` 字段：同一篇文档覆盖多个模型时，用户必须知道能填哪些值
// （这正是「相同参数只因模型 ID 不同就拆文档」要解决的问题的另一半）。
// 取值由投影期注入（ProjectGroup），契约里不写死任何模型 ID。
func (f Field) withMembers() Field { f.EnumSource = EnumGroupMembers; return f }

// withReq 补条件必填的说明（ReqConditional 时必须给，测试守）。
func (f Field) withReq(s string) Field { f.RequiredNote = s; return f }

// withLim 补一条约束。
func (f Field) withLim(kind string, value float64, text string) Field {
	f.Constraints = append(f.Constraints, Constraint{Kind: kind, Value: value, Text: text})
	return f
}

// ret 构造返回字段（返回字段没有必填概念，统一 ReqOptional）。
func ret(name string, t Type, desc string) Field {
	return Field{Name: name, Type: t, Required: ReqOptional, Desc: desc, Tier: TierStandard}
}

// noSketch 把字段从响应骨架里排除（只进字段表）。
func (f Field) noSketch() Field { f.SketchSkip = true; return f }

// withExample 给返回字段挂一个真值（示例里优先于类型占位）。
func (f Field) withExample(v any) Field { f.Example = v; return f }

func retNote(name string, t Type, desc, note string) Field {
	return ret(name, t, desc).withNote(note)
}

// endpointDocs 是端点字段契约表。
var endpointDocs = map[Kind]EndpointDoc{

	// ══ 语言模型 ══

	KindChatCompletions: {
		Kind: KindChatCompletions, Name: "对话补全",
		Summary: "多轮对话主接口，可带图片。兼容 OpenAI Chat 格式，所有厂商模型统一调用。",
		Fields: []Field{
			plat("model", TypeString, ReqRequired, "—", "用哪个模型，网关自动路由。").withMembers(),
			plat("messages", TypeArray, ReqRequired, "—", "对话记录数组。").
				withNote("每项 {role, content}；role 取 system 立规矩 / user 用户说 / assistant 模型答。"),
			std("messages[].role", TypeString, ReqRequired, "—", "这条是谁说的。").
				withEnum("system", "user", "assistant"),
			std("messages[].content", TypeAny, ReqRequired, "—", "该条消息的内容。").
				withNote(`文字用字符串；发图用数组 [{type:"text",text:"…"},{type:"image_url",image_url:{url:"…"}}]。`),
			std("stream", TypeBoolean, ReqOptional, "false", "打字机流式开关。").
				withNote("开启后返回 text/event-stream，以 data: [DONE] 结束。"),
			std("temperature", TypeNumber, ReqOptional, "1", "发散度：0 严谨，高值有创意。").
				withLim("min", 0, "0 ~ 2"),
			std("max_tokens", TypeInteger, ReqOptional, "模型上限", "回答长度上限。"),
			std("max_completion_tokens", TypeInteger, ReqOptional, "—", "回答长度上限（新版字段，与 max_tokens 二选一）。").
				withNote("两个都传时以 max_completion_tokens 为准。"),
			std("stream_options", TypeObject, ReqOptional, "—", "流式附加选项。").
				withNote(`常用 {include_usage:true}；OpenAI 系模型由平台自动开启，无需自己传。其它模型以实际返回为准。`),
			std("top_p", TypeNumber, ReqOptional, "1", "核采样，与 temperature 二选一。").
				withLim("min", 0, "0 ~ 1"),
			std("stop", TypeAny, ReqOptional, "—", "命中即停止输出，字符串或数组。").
				withNote("最多 4 个。"),
			std("tools", TypeArray, ReqOptional, "—", "函数调用：把函数定义交给模型。").
				withNote(`写法 [{type:"function", function:{name, description, parameters}}]。`),
			std("tool_choice", TypeAny, ReqOptional, "auto", "工具调用策略。").
				withNote(`auto 由模型决定 / none 禁止 / {type:"function",function:{name}} 强制指定。`),
			std("response_format", TypeObject, ReqOptional, "—", "强制输出 JSON。").
				withNote(`常用 {type:"json_object"}。`),
			vendor("seed", TypeInteger, ReqOptional, "随机", "固定随机种子便于复现。", "部分模型"),
		},
		Returns: []Field{
			ret("id", TypeString, "本次回答编号。"),
			ret("choices[].message.content", TypeString, "回答正文（非流式）。"),
			ret("choices[].delta.content", TypeString, "增量正文（流式，每个事件一段）。").noSketch(),
			ret("choices[].finish_reason", TypeString, "stop 正常 / length 截断 / tool_calls 调工具。"),
			ret("usage.prompt_tokens", TypeInteger, "输入词块。"),
			ret("usage.completion_tokens", TypeInteger, "输出词块。"),
			ret("usage.total_tokens", TypeInteger, "总词块。"),
		},
		Response: ResponseSpec{
			Variants: []ResponseVariant{
				{Status: 200, Label: "成功（JSON）", Notes: []string{
					"`stream: false`（默认）返回这个结构。",
				}},
				{
					Status: 200, Label: "流式（SSE）", ContentType: "text/event-stream",
					Raw: `data: {"id":"chatcmpl-abc","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"API"}}]}

data: {"id":"chatcmpl-abc","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"content":" 网关"}}]}

data: {"id":"chatcmpl-abc","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"chatcmpl-abc","object":"chat.completion.chunk","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":8,"total_tokens":20}}

data: [DONE]`,
					Notes: []string{
						"`stream: true` 返回事件流，最后一行 `data: [DONE]` 结束。",
						"示例里的回答编号与词块数只是示意，实际值每次不同。",
					},
				},
			},
		},
		Example: Obj{
			{K: "model", V: "gpt-4o"},
			{K: "messages", V: []any{
				Obj{{K: "role", V: "system"}, {K: "content", V: "你是一个耐心的编程助手"}},
				Obj{{K: "role", V: "user"}, {K: "content", V: "用一句话解释什么是 API 网关"}},
			}},
			{K: "temperature", V: 0.7},
			{K: "stream", V: false},
		},
		Notes: []string{
			"流式每帧的 delta.content 顺序拼接即完整回答；网关已自动向厂商申请回传用量，末帧带 usage。",
		},
		Example2: Obj{
			{K: "model", V: "gpt-4o"},
			{K: "messages", V: []any{
				Obj{{K: "role", V: "user"}, {K: "content", V: "写一句关于春天的诗"}},
			}},
			{K: "stream", V: true},
		},
		ExampleTitle2: "流式输出（打字机效果）",
		RunMode:       RunSSE,
	},

	KindCompletions: {
		Kind: KindCompletions, Name: "文本补全",
		Summary: "单轮续写老接口，新项目建议用对话补全。",
		Fields: []Field{
			plat("model", TypeString, ReqRequired, "—", "用哪个模型。").withMembers(),
			plat("prompt", TypeString, ReqRequired, "—", "开头文字，模型接着写。"),
			std("max_tokens", TypeInteger, ReqOptional, "16", "续写上限。"),
			std("stream", TypeBoolean, ReqOptional, "false", "流式开关。").
				withNote("本平台**不生效**：该接口整段读完再返回。要流式请用对话补全。"),
			std("temperature", TypeNumber, ReqOptional, "1", "发散度。").withLim("min", 0, "0 ~ 2"),
		},
		Returns: []Field{
			ret("choices[].text", TypeString, "续写内容。"),
			ret("usage", TypeObject, "词块统计，同对话补全。"),
		},
		Example: Obj{
			{K: "model", V: "gpt-3.5-turbo-instruct"},
			{K: "prompt", V: "写一句关于春天的诗："},
			{K: "max_tokens", V: 50},
			{K: "temperature", V: 0.8},
		},
		RunMode: RunJSON,
	},

	KindResponses: {
		Kind: KindResponses, Name: "新版智能体接口",
		Summary: "OpenAI 新一代接口（模型 + 内置工具）。仅 OpenAI 协议模型可用。",
		Fields: []Field{
			plat("model", TypeString, ReqRequired, "—", "用哪个模型。").withMembers(),
			plat("input", TypeAny, ReqRequired, "—", "要发的内容。").
				withNote("字符串，或消息数组。"),
			std("tools", TypeArray, ReqOptional, "—", "内置工具列表。").
				withNote(`写法与对话补全不同，如 [{type:"web_search"}]。`),
			std("stream", TypeBoolean, ReqOptional, "false", "流式开关。").
				withNote("传 true 时按事件流返回（response.output_text.delta 等）；流结束不发 [DONE]，以 response.completed 收尾。"),
		},
		Returns: []Field{
			ret("output", TypeArray, "产出列表（文本、工具结果）。"),
			ret("usage", TypeObject, "词块统计。").
				withNote("流式下在 response.completed 事件的 response.usage 里。"),
		},
		Example: Obj{
			{K: "model", V: "gpt-4o"},
			{K: "input", V: "帮我查一下今天上海适合穿什么"},
		},
		RunMode: RunSSE,
	},

	KindResponsesCompact: {
		Kind: KindResponsesCompact, Name: "上下文压缩",
		Summary: "把一段长对话压成可继续使用的紧凑上下文（Codex 等智能体客户端用它控制上下文长度）。请求与响应形状与新版智能体接口一致。",
		Fields: []Field{
			plat("model", TypeString, ReqRequired, "—", "用哪个模型。").withMembers(),
			plat("input", TypeAny, ReqRequired, "—", "要压缩的内容。").
				withNote("字符串，或消息数组（与新版智能体接口相同）。"),
			std("instructions", TypeString, ReqOptional, "—", "压缩时遵守的额外说明。").
				withNote("原样下发；平台不认识该字段时按参数错误返回。"),
		},
		Returns: []Field{
			ret("id", TypeString, "本次压缩编号。"),
			ret("object", TypeString, "固定为 response.compaction。"),
			ret("output", TypeArray, "压缩后的内容块列表。"),
			ret("usage", TypeObject, "词块统计。").
				withNote("未返回用量时按估算计费并标记为未实测，金额可能略有偏差。"),
		},
		Example: Obj{
			{K: "model", V: "gpt-4o"},
			{K: "input", V: "（把上面的长对话压成一段摘要）"},
		},
		RunMode: RunJSON,
	},

	KindGeminiModels: {
		Kind: KindGeminiModels, Name: "模型列表（Gemini 格式）",
		Summary: "本密钥可用模型的清单，按 Gemini 的 `{models:[{name}]}` 形状返回。与 OpenAI 面的模型列表同一份数据、同一套过滤。",
		Fields: []Field{
			std("pageSize", TypeInteger, ReqOptional, "50", "每页条数。").
				withNote("1 ~ 1000；超出范围返回 400。"),
			std("pageToken", TypeString, ReqOptional, "—", "翻页游标。").
				withNote("取上一页响应里的 nextPageToken（十进制偏移）；非法值返回 400。"),
		},
		Returns: []Field{
			ret("models[].name", TypeString, "模型名，形如 `models/<id>`。").
				withNote("原样填回 `:generateContent` 前的路径段；去掉 `models/` 前缀也可。").
				withNote("不要自己拼 URL：前缀由服务端决定，不是对外契约。"),
			ret("models[].displayName", TypeString, "展示名。"),
			ret("models[].supportedGenerationMethods", TypeArray, "该模型支持的动作。").
				withNote("对话模型是 generateContent / streamGenerateContent；向量化模型是 embedContent。词块计数（countTokens）本平台不提供，因此不会出现在这里。"),
			ret("nextPageToken", TypeString, "下一页游标。**没有更多时整个键不出现**。"),
		},
		RunMode: RunJSON,
	},

	KindGeminiGenerate: {
		Kind: KindGeminiGenerate, Name: "内容生成（Gemini 格式）",
		Summary: "按 Gemini 协议做一次对话生成：请求 `{contents:[{parts:[{text}]}]}`，响应 `{candidates:[{content:{parts}}]}`。与对话补全同一能力、同一套计费。",
		Fields: []Field{
			pathParam(plat("model", TypeString, ReqRequired, "—", "模型名（路径里 `:generateContent` 之前的部分）。")),
			plat("contents", TypeArray, ReqRequired, "—", "对话内容，按时间顺序。").
				withNote("每项含 role（user / model）与 parts（文本、图片、工具调用）。"),
			plat("contents[].parts[].text", TypeString, ReqOptional, "—", "文本段。"),
			plat("contents[].parts[].inlineData", TypeObject, ReqOptional, "—", "内联图片（mimeType + base64 data）。").
				withNote("只支持图片（mimeType 以 image/ 开头）；音频/文档返回 400。"),
			plat("contents[].parts[].fileData", TypeObject, ReqOptional, "—", "图片直链（mimeType + fileUri）。"),
			std("systemInstruction", TypeObject, ReqOptional, "—", "系统提示（parts 里的文本）。"),
			std("generationConfig.temperature", TypeNumber, ReqOptional, "—", "发散度。"),
			std("generationConfig.topP", TypeNumber, ReqOptional, "—", "核采样阈值。"),
			std("generationConfig.topK", TypeInteger, ReqOptional, "—", "采样候选数。").
				withNote("本平台不生效：传了既不改变结果也不报错。"),
			std("generationConfig.maxOutputTokens", TypeInteger, ReqOptional, "—", "最大输出词块。").
				withNote("不传时按模型配置的输出上限冻结；传 0 视为不传。"),
			std("generationConfig.stopSequences", TypeArray, ReqOptional, "—", "停止序列，最多 4 条。"),
			std("generationConfig.responseMimeType", TypeString, ReqOptional, "—", "响应类型。").
				withNote("只支持 application/json（会转成 JSON 输出模式）；其它值返回 400。"),
			std("generationConfig.responseSchema", TypeObject, ReqOptional, "—", "结构化输出的 JSON Schema。").withNote("原样下发；平台不支持该结构时按参数错误返回。"),
			std("generationConfig.thinkingConfig.thinkingBudget", TypeInteger, ReqOptional, "—", "思考预算（词块）。").
				withNote("会被离散成 minimal / low / medium / high 四档下发，精度会丢失。"),
			std("generationConfig.responseModalities", TypeArray, ReqOptional, "—", "输出模态。").
				withNote("只接受纯文本输出，写别的值返回 400；图片生成请用 /v1/images/generations。"),
			std("tools", TypeArray, ReqOptional, "—", "自定义工具（functionDeclarations）。").
				withNote("服务端工具（googleSearch / codeExecution / urlContext）返回 400。"),
			std("toolConfig", TypeObject, ReqOptional, "—", "工具调用策略（AUTO / ANY / NONE）。"),
			std("cachedContent", TypeString, ReqOptional, "—", "上下文缓存引用。").
				withNote("返回 400：平台不支持上下文缓存引用，静默忽略会按全价收费。"),
			std("safetySettings", TypeArray, ReqOptional, "—", "安全设置。").
				withNote("本平台不生效：按平台策略执行，传了不报错。"),
		},
		Returns: []Field{
			ret("candidates[].content.parts[].text", TypeString, "正文或思考片段。"),
			ret("candidates[].content.parts[].thought", TypeBoolean, "该段是否思考内容。").withNote("只有模型给出推理内容时才会出现。"),
			ret("candidates[].content.parts[].functionCall", TypeObject, "工具调用（name + args 对象）。"),
			ret("candidates[].finishReason", TypeString, "STOP 正常结束 / MAX_TOKENS 截断 / SAFETY 被安全策略拦下 / OTHER 其它。"),
			ret("usageMetadata.promptTokenCount", TypeInteger, "输入词块。"),
			ret("usageMetadata.candidatesTokenCount", TypeInteger, "可见输出词块（不含思考）。"),
			ret("usageMetadata.thoughtsTokenCount", TypeInteger, "思考词块。"),
			ret("usageMetadata.cachedContentTokenCount", TypeInteger, "命中缓存的输入词块。"),
			ret("usageMetadata.totalTokenCount", TypeInteger, "总词块。"),
			ret("promptFeedback.blockReason", TypeString, "整条请求被拦下时为 SAFETY，此时没有 candidates。"),
			ret("modelVersion", TypeString, "实际使用的模型，形如 `models/<id>`。"),
		},
		Example: Obj{
			{K: "model", V: "gemini-2.0-flash"},
			{K: "contents", V: []any{map[string]any{
				"role":  "user",
				"parts": []any{map[string]any{"text": "用一句话介绍杭州"}},
			}}},
		},
		RunMode: RunSSE,
	},

	KindGeminiEmbedContent: {
		Kind: KindGeminiEmbedContent, Name: "向量化（Gemini 格式）",
		Summary: "按 Gemini 协议把文本转成向量：请求 `{content:{parts:[{text}]}}`，响应 `{embedding:{values:[…]}}`。与 /v1/embeddings 同一能力、同一套计费。",
		Fields: []Field{
			pathParam(plat("model", TypeString, ReqRequired, "—", "模型名。")),
			plat("content", TypeObject, ReqRequired, "—", "要编码的内容。").
				withNote("只支持文本 parts；图片/文件向量化返回 400。"),
			std("content.parts[].text", TypeString, ReqRequired, "—", "文本段。"),
			std("outputDimensionality", TypeInteger, ReqOptional, "—", "期望向量维度。").
				withNote("转成向量维度参数；不支持该参数时按参数错误返回。"),
			std("taskType", TypeString, ReqOptional, "—", "检索任务提示。").
				withNote("本平台不生效：没有对应参数，传了不报错。"),
		},
		Returns: []Field{
			ret("embedding.values", TypeArray, "向量（单条输入）。"),
			ret("embeddings[].values", TypeArray, "向量（多条输入时按输入顺序返回）。"),
			ret("usageMetadata.tokenCount", TypeInteger, "计入的输入词块。"),
		},
		Example: Obj{
			{K: "model", V: "text-embedding-004"},
			{K: "content", V: map[string]any{"parts": []any{map[string]any{"text": "杭州"}}}},
		},
		RunMode: RunJSON,
	},

	KindMessages: {
		Kind: KindMessages, Name: "对话补全（Anthropic 格式）",
		Summary: "与对话补全同一个能力，请求与响应按 Anthropic Messages 协议收发：Claude 系列客户端与 SDK 可以直接接入。",
		Fields: []Field{
			plat("model", TypeString, ReqRequired, "—", "用哪个模型。").withMembers(),
			plat("max_tokens", TypeInteger, ReqRequired, "—", "本次回答的最大输出词块。").
				withNote("该协议要求必填：不传或传 0 会返回 400。"),
			plat("messages", TypeArray, ReqRequired, "—", "对话内容，按时间顺序。").
				withNote("每项含 role（user / assistant）与 content（字符串，或文本 / 图片 / 工具块数组）。"),
			std("system", TypeAny, ReqOptional, "—", "系统提示。").
				withNote("字符串或文本块数组；多个文本块会拼成一段系统消息。"),
			std("tools", TypeArray, ReqOptional, "—", "可调用的工具定义。").
				withNote("每项形如 {name, description, input_schema}；只支持自定义工具（type 省略或 function）。"),
			std("tool_choice", TypeAny, ReqOptional, "auto", "工具选择策略。").
				withEnum("auto", "any", "tool", "none"),
			std("temperature", TypeNumber, ReqOptional, "1", "发散度，越大越随机。").withLim("min", 0, "0 ~ 1"),
			std("top_p", TypeNumber, ReqOptional, "1", "核采样阈值。").withLim("min", 0, "0 ~ 1"),
			std("top_k", TypeInteger, ReqOptional, "—", "采样候选数。").
				withNote("本平台不生效：传了既不改变结果也不报错。"),
			std("stop_sequences", TypeArray, ReqOptional, "—", "遇到这些文本就停止生成。").
				withNote("最多 4 条，超出部分会被忽略。"),
			std("stream", TypeBoolean, ReqOptional, "false", "流式开关。").
				withNote("传 true 时按事件流返回，每帧带事件名（message_start / content_block_delta / message_stop）。"),
			std("metadata", TypeObject, ReqOptional, "—", "调用方自带的标记。").
				withNote("user_id 会记录为调用者标记，便于按用户对账。"),
		},
		Returns: []Field{
			ret("id", TypeString, "本次回答编号，报障时提供它可定位到具体一次调用。"),
			ret("type", TypeString, "固定为 message。"),
			ret("role", TypeString, "固定为 assistant。"),
			ret("model", TypeString, "本次实际使用的模型。"),
			ret("content[].type", TypeString, "内容块类型：text 正文 / tool_use 工具调用。"),
			ret("content[].text", TypeString, "正文（type=text）。").noSketch(),
			ret("content[].id", TypeString, "工具调用编号（type=tool_use）。").noSketch(),
			ret("content[].name", TypeString, "工具名（type=tool_use）。").noSketch(),
			ret("content[].input", TypeObject, "工具入参（type=tool_use）。").noSketch(),
			ret("stop_reason", TypeString, "end_turn 正常结束 / max_tokens 截断 / tool_use 需要调工具。").
				withNote("流式下这个值在 message_delta 帧给出。"),
			ret("usage.input_tokens", TypeInteger, "输入词块，不含命中缓存的部分。"),
			ret("usage.output_tokens", TypeInteger, "输出词块。"),
			ret("usage.cache_read_input_tokens", TypeInteger, "命中缓存的输入词块。"),
			ret("usage.cache_creation_input_tokens", TypeInteger, "写入缓存的输入词块。"),
		},
		Response: ResponseSpec{
			Variants: []ResponseVariant{
				{Status: 200, Label: "成功（JSON）"},
				{
					Status: 200, Label: "流式（SSE）", ContentType: "text/event-stream",
					Raw: `event: message_start
data: {"type":"message_start","message":{"id":"msg_abc","type":"message","role":"assistant","model":"claude-3-5-sonnet","content":[],"usage":{"input_tokens":0,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"API 网关"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":12,"output_tokens":8}}

event: message_stop
data: {"type":"message_stop"}`,
					Notes: []string{
						"流式每帧都带事件名；正文在 content_block_delta 里逐段给出，顺序拼接即完整回答。",
						"用量以 message_delta 那一帧为准（首帧的 input_tokens 为 0 是正常的，那时还没有统计）。",
					},
				},
			},
		},
		Example: Obj{
			{K: "model", V: "claude-3-5-sonnet"},
			{K: "max_tokens", V: 1024},
			{K: "system", V: "你是一个耐心的编程助手"},
			{K: "messages", V: []any{
				Obj{{K: "role", V: "user"}, {K: "content", V: "用一句话解释什么是 API 网关"}},
			}},
		},
		Example2: Obj{
			{K: "model", V: "claude-3-5-sonnet"},
			{K: "max_tokens", V: 1024},
			{K: "stream", V: true},
			{K: "messages", V: []any{
				Obj{{K: "role", V: "user"}, {K: "content", V: "写一句关于春天的诗"}},
			}},
		},
		ExampleTitle2: "流式输出（打字机效果）",
		Notes: []string{
			"这是同一个能力的另一种请求格式：可用模型与对话补全完全一致。",
			"`max_tokens` 必填；`top_k` 不生效；`thinking`、`document`、`cache_control` 暂不支持，带这些内容会返回 400 而不是被悄悄忽略。",
			"词块计数接口（`/v1/messages/count_tokens`）暂不提供，会返回 501：请按本地估算管理上下文长度。",
			"鉴权用 `x-api-key` 或 `Authorization: Bearer`，`anthropic-version` 头会被忽略（任意值都按本页契约处理）。",
		},
		RunMode: RunSSE,
	},

	// ══ 向量与检索 ══

	KindEmbeddings: {
		Kind: KindEmbeddings, Name: "文本向量化",
		Summary: "文字变向量，语义越近向量越近。搜索 / 推荐 / RAG 的地基。",
		Fields: []Field{
			plat("model", TypeString, ReqRequired, "—", "向量化模型。").withMembers(),
			plat("input", TypeAny, ReqRequired, "—", "要向量化的文字。").
				withNote("字符串，或字符串数组（批量处理）。"),
			std("dimensions", TypeInteger, ReqOptional, "模型默认", "压缩向量长度，省存储。").
				withNote("仅部分模型支持，如 256 / 1024。"),
			std("encoding_format", TypeString, ReqOptional, "float", "返回编码。").
				withEnum("float", "base64"),
		},
		Returns: []Field{
			ret("data[].embedding", TypeArray, "向量本体，存进向量库即可检索。"),
			ret("usage.prompt_tokens", TypeInteger, "消耗词块。"),
		},
		Example: Obj{
			{K: "model", V: "text-embedding-3-small"},
			{K: "input", V: "灵动 API 提供统一的模型调用接口"},
		},
		RunMode: RunJSON,
	},

	KindRerank: {
		Kind: KindRerank, Name: "相关性重排",
		Summary: "按问题与候选文档的相关度重新排序，RAG 精排用。",
		Fields: []Field{
			plat("model", TypeString, ReqRequired, "—", "重排模型。").withMembers(),
			plat("query", TypeString, ReqRequired, "—", "问题或检索词。"),
			plat("documents", TypeArray, ReqRequired, "—", "候选文档列表（字符串数组）。"),
			std("top_n", TypeInteger, ReqOptional, "全部", "只要前 N 条。"),
		},
		Returns: []Field{
			ret("results[].index", TypeInteger, "对应文档下标。"),
			ret("results[].relevance_score", TypeNumber, "相关度 0~1，已按降序返回。"),
		},
		Example: Obj{
			{K: "model", V: "rerank-v2"},
			{K: "query", V: "如何退款"},
			{K: "documents", V: []any{"退款政策说明…", "发票开具流程…"}},
			{K: "top_n", V: 2},
		},
		RunMode: RunJSON,
	},

	// ══ 视频（仅平台自有端点）══
	//
	// 图片 / 视频提交 / 语音 / 音乐这几类**厂商协议端点不在这张表里**：
	// 同一个路由（如 `/v1/images/generations`）在各族是不同的协议，
	// 一篇共享文档必然对其中一族说谎。它们按族写在 family_*.go 里，
	// 由 family_endpoints.go 的登记表统一索引。

	KindVideoRegenerate: {
		Kind: KindVideoRegenerate, Name: "视频再生成（升档）",
		Summary: "基于一个已成功的任务重新生成（典型用法：低清成片升到 2K），或对一段已有视频做重绘。独立价目，返回新任务号。",
		Fields: []Field{
			plat("model", TypeString, ReqRequired, "—", "再生成模型；只有声明了再生成端点的模型可用。").withMembers(),
			plat("source_task_id", TypeString, ReqConditional, "—", "源任务号（`task_` 前缀，原样回传）。").
				withReq("与 base_video 二选一，同时给或都不给都直接 400。").
				withNote("填本平台的任务号；平台校验归属与状态后自动完成后续换算。"),
			plat("base_video", TypeString, ReqConditional, "—", "直接给一段视频做重绘，免去先建任务。").
				withReq("与 source_task_id 二选一。").
				withNote("公网 http(s) 直链。"),
			plat("prompt", TypeString, ReqOptional, "—", "仅 base_video 形态需要（源任务形态可沿用原提示词）。"),
			plat("resolution", TypeString, ReqRequired, "—", "再生成的目标档位，由模型价目决定。").
				withEnumFrom(EnumVideoTier),
			plat("input_reference", TypeArray, ReqOptional, "[]", "base_video 形态的承载方式。").
				withNote(`等价于顶层 base_video，写法 [{type:"base_video", video_url:"…"}]。`),
			vendor("metadata", TypeObject, ReqOptional, "—", "同生成接口：只允许白名单键。", "部分模型"),
		},
		Returns: TaskFlatFields(),
		Response: ResponseSpec{
			Variants: []ResponseVariant{{
				Status: 200, Label: "已受理",
				Returns: TaskFlatFields(),
				Example: TaskExampleFlatAccepted(),
				Notes: []string{
					"返回的是**新任务号**，与源任务号不同；后续查询与下载都用新的。",
				},
			}},
		},
		Example: Obj{
			{K: "model", V: "<model>"},
			{K: "source_task_id", V: flatExampleID},
			{K: "resolution", V: "2k"},
		},
		Example2: Obj{
			{K: "model", V: "<model>"},
			{K: "prompt", V: "同一镜头，改成黄昏光线"},
			{K: "base_video", V: "https://cdn.example.com/clip.mp4"},
			{K: "resolution", V: "2k"},
		},
		ExampleTitle2: "直接给视频重绘",
		Notes: []string{
			"source_task_id 与 base_video 必须二选一：两个都给（或都不给）直接 400，不做优先级兜底。",
			"只接受本人的、状态为 succeeded 的源任务；不是本人一律按「源任务不存在」处理，不泄露他人任务是否存在。",
			"源任务必须与再生成模型属于同一服务方，不一致时会被拒绝。",
			"base_video 形态的时长靠读取 mp4 头尾索引得到（读不到即 400）；源任务形态沿用源任务记录的秒数。",
			"查询与下载复用视频查询与下载接口，传的是新任务号。",
			"要关闭该能力：把再生成模型停用即可（独立模型，不影响生成主链路）。",
		},
		RunMode: RunJSON,
	},

	// ══ 内容安全 ══

	KindModerations: {
		Kind: KindModerations, Name: "内容审核",
		Summary: "检测文本风险类别，消耗极小，适合发帖 / 提问前预检。",
		Fields: []Field{
			plat("model", TypeString, ReqRequired, "—", "审核模型名（必填，网关不注入默认值）。").withMembers(),
			plat("input", TypeAny, ReqRequired, "—", "待检测内容。").withNote("字符串或字符串数组。"),
		},
		Returns: []Field{
			ret("results[].flagged", TypeBoolean, "true = 有风险，建议拦截。"),
			ret("results[].categories", TypeObject, "命中类别（布尔值）。"),
			ret("results[].category_scores", TypeObject, "各类别的风险分数。"),
		},
		Example: Obj{
			{K: "model", V: "omni-moderation-latest"},
			{K: "input", V: "这里放待检测的用户评论"},
		},
		RunMode: RunJSON,
	},

	// ══ 文件 ══

	KindFilesUpload: {
		Kind: KindFilesUpload, Name: "上传文件",
		Summary: "上传临时文件拿 24 小时直链，填进视频任务的素材数组即完成「上传素材」。",
		Fields: []Field{
			formParam(plat("file", TypeFile, ReqRequired, "—", "要上传的文件。")).
				withNote("表单字段名固定为 file；扩展名与文件头双重校验。"),
			formParam(std("purpose", TypeString, ReqOptional, "—", "OpenAI 兼容字段，本平台忽略。")),
		},
		Returns: []Field{
			ret("id", TypeString, "文件号。"),
			ret("url", TypeString, "文件地址，无需鉴权，填进素材数组即可。云端存储是对象存储的永久直链；本地存储是平台 `/f/{id}/{name}`。"),
			ret("size_bytes", TypeInteger, "文件大小（字节）。"),
			ret("content_type", TypeString, "内容类型。"),
			ret("filename", TypeString, "原始文件名。"),
			ret("safe_name", TypeString, "直链里用的安全文件名（非 ASCII 字符已替换为下划线）。"),
			ret("key_id", TypeInteger, "上传时使用的 API KEY 编号。"),
			// 时间字段是 RFC3339 字符串而不是 Unix 秒：`upload.Item` 用 time.Time，
			// Go 序列化成 `2026-01-01T00:00:00+08:00`。早先文档写成 integer，
			// 用户按数字解析会直接拿到一个字符串、比较永远为 false。
			ret("created_at", TypeString, "创建时间（RFC3339，带时区）。"),
			ret("expires_at", TypeString, "过期时间（RFC3339，带时区）。"),
			ret("remaining_seconds", TypeInteger, "距过期的剩余秒数；过期后按不存在处理。"),
		},
		MaxBody: "以系统配置为准（默认 50MB）",
		Notes: []string{
			"限额由系统配置统一下发：单文件大小 / 每密钥每日次数 / 账号总存储 / 并发数，控制台改完即时生效。",
			"直链无需鉴权，厂商可直接拉取；按 IP 限流，视频任务一次提交多张图会并发取图，注意收敛。",
		},
		RunMode: RunJSON,
	},

	KindFilesList: {
		Kind: KindFilesList, Name: "文件列表",
		Summary: "分页列出本密钥未过期的文件。",
		Fields: []Field{
			queryParam(plat("page", TypeInteger, ReqOptional, "1", "页码。")),
			queryParam(plat("page_size", TypeInteger, ReqOptional, "20", "每页条数。")),
		},
		Returns: []Field{
			ret("data", TypeArray, "文件数组，字段同上传返回。"),
			ret("total", TypeInteger, "总条数。"),
		},
		RunMode: RunJSON,
	},

	// ── 单个文件的元信息与内容 ──
	//
	// 这两个端点解决的是「文件 id 才是引用凭据」：直链 24 小时失效、且不校验归属，
	// 把直链存进业务库第二天就全挂。有了 id 之后，元信息随查随有，内容随时取。
	KindFilesGet: {
		Kind: KindFilesGet, Name: "查询文件",
		Summary: "按文件号取元信息（大小 / 类型 / 过期时间），只认本密钥上传的文件。",
		Fields: []Field{
			pathParam(plat("id", TypeString, ReqRequired, "—", "文件号（上传返回的 id）。")),
		},
		Returns: []Field{
			ret("id", TypeString, "文件号。"),
			ret("filename", TypeString, "原始文件名。"),
			ret("safe_name", TypeString, "直链里使用的安全文件名。"),
			ret("size_bytes", TypeInteger, "文件大小（字节）。"),
			ret("content_type", TypeString, "内容类型。"),
			ret("url", TypeString, "当前可用的文件地址（与上传返回同源，按文件所在存储后端派生）。"),
			ret("key_id", TypeInteger, "上传时使用的 API KEY 编号。"),
			ret("created_at", TypeString, "创建时间（RFC3339，带时区）。"),
			ret("expires_at", TypeString, "过期时间（RFC3339，带时区）。"),
			ret("remaining_seconds", TypeInteger, "距过期的剩余秒数。"),
		},
		Notes: []string{
			"别的密钥上传的文件一律按 `404 file_not_found` 处理，不区分「不存在」与「不属于你」。",
			"过期文件同样返回 404（不是 410）：过期即不存在是本平台统一语义。",
		},
		// 可在线试跑：GET + 路径参数 + KEY，浏览器能构造完整请求。
		RunMode: RunJSON,
	},

	KindFilesContent: {
		Kind: KindFilesContent, Name: "下载文件内容",
		Summary: "按文件号下载内容（等价于 OpenAI 的 files.content），带密钥校验与断点续传。",
		Fields: []Field{
			pathParam(plat("id", TypeString, ReqRequired, "—", "文件号。")),
		},
		Returns: []Field{
			ret("（文件流）", TypeBinary, "文件本体；带 Range 时返回 206。"),
		},
		Response: ResponseSpec{ContentType: "按上传时的类型"},
		Notes: []string{
			"与直链地址的区别：这里要带 `Authorization` 且校验归属，地址本身永不变；直链不需要鉴权，但文件 24 小时后过期（云端地址指向对象存储，本地地址是 `/f/{id}/{name}`）。",
			"支持 `Range`（206 + `Content-Range`），越界返回 416；`HEAD` 返回长度但不返回内容。",
			"文件过期或不属于当前密钥时返回 `404 file_not_found`。",
		},
		// 响应是文件字节：只能按二进制渲染（契约里 Binary=true 的端点不许标 json）。
		RunMode: RunBinary,
	},

	KindFilesDelete: {
		Kind: KindFilesDelete, Name: "删除文件",
		Summary: "提前删除未过期的文件。",
		Fields: []Field{
			pathParam(plat("id", TypeString, ReqRequired, "—", "文件号。")),
		},
		Returns: []Field{
			ret("（无响应体）", TypeNone, "删除成功，此后一律 404。"),
		},
		// DELETE + 路径参数 + KEY：能试跑。真删是不可逆的，付费层由调试面板的二次确认兜。
		RunMode: RunJSON,
	},

	KindFileDirect: {
		Kind: KindFileDirect, Name: "文件直链下载",
		Summary: "本地存储记录的文件直链，无需鉴权（按 IP 限流）；云端记录在上传时已直接给出对象存储地址。",
		Fields: []Field{
			pathParam(plat("id", TypeString, ReqRequired, "—", "文件号。")),
			pathParam(plat("name", TypeString, ReqRequired, "—", "文件名，上传返回的 url 已拼好。")),
		},
		Returns: []Field{
			ret("（文件流）", TypeBinary, "文件本体；过期返回 410，按不存在处理。"),
		},
		Response: ResponseSpec{ContentType: "按上传时的类型"},
		Notes: []string{
			"云端记录会 302 到对象存储的公开地址（无 Range 时）；带 Range 或本地记录由平台代理输出。",
			"支持 HEAD 与 Range（206 + Content-Range），越界 Range 返回 416。",
			"按 IP 限流，超限返回 429。",
			"失败时返回的是 JSON `{code,message,error_code}`（不是文件流），调用方要先看状态码再决定是否读 body。",
		},
		// 唯一不可试跑的读面：直链挂在文件域（不是 API 域），浏览器从这里发不出去。
		// 理由是「域不同」，不是「没有输入框」—— 后者早就不成立了。
		RunMode: RunNone,
		RunHint: "文件直链由上传接口返回，请直接打开响应里的 url。",
	},

	// ══ 异步任务（全模态统一读面） ══

	KindTasksList: {
		Kind: KindTasksList, Name: "任务列表",
		Summary: "列出本密钥的异步任务，图像 / 音乐 / 视频共用一套读面，按任务号游标分页。",
		Fields: []Field{
			queryParam(plat("limit", TypeInteger, ReqOptional, "20", "本页条数。")).withLim("max", 100, "1 ~ 100"),
			queryParam(plat("after", TypeString, ReqOptional, "—", "游标：只取比该任务**更旧**的任务。")).
				withNote("传上一页返回的 last_id。列表按任务号倒序，所以「更旧」= 更小的号。"),
			queryParam(plat("status", TypeString, ReqOptional, "—", "按状态筛选（与响应里的 status 同一套词）。")).
				withEnum("SUBMITTED", "IN_PROGRESS", "SUCCESS", "FAILURE"),
			queryParam(plat("modality", TypeString, ReqOptional, "—", "按模态筛选。")).
				withEnum("image", "music", "video"),
			queryParam(plat("model", TypeString, ReqOptional, "—", "按模型筛选。")),
		},
		Returns: append([]Field{
			ret("object", TypeString, "固定为 list。"),
			ret("data", TypeArray, "任务数组，元素与任务详情**同一个结构**。").withExample([]any{}),
			ret("has_more", TypeBoolean, "本页之后是否还有数据。翻页以它为准，不要用 total 判断。"),
			ret("total", TypeInteger, "符合条件的总条数。"),
			ret("first_id", TypeString, "本页第一条的任务号。").withNote("**空页时整个键不出现**（没有可指向的条目）。"),
			ret("last_id", TypeString, "本页最后一条的任务号；下一页把它传给 after。").withNote("空页时不出现，与 `first_id` 同进同退。"),
		}, TaskFlatFields()...),
		RunMode: RunJSON,
	},

	KindTasksGet: {
		Kind: KindTasksGet, Name: "任务详情",
		Summary: "查单个任务的状态与失败原因，建议 5~10 秒轮询一次；进入终态即停。",
		Fields: []Field{
			pathParam(plat("id", TypeString, ReqRequired, "—", "任务号（`task_` 前缀）。")),
		},
		Returns: TaskFlatFields(),
		Response: ResponseSpec{
			Variants: []ResponseVariant{
				{
					Status: 200, Label: "排队 / 运行中",
					Returns: TaskFlatFields(),
					Example: TaskExampleFlatAccepted(),
					Notes:   []string{"`status` 是 SUBMITTED / IN_PROGRESS 时继续轮询；`progress` 给 `0%` 或 `50%`。"},
				},
				{
					Status: 200, Label: "已完成",
					Returns: TaskFlatFields(),
					Example: TaskExampleFlat("SUCCESS"),
					Notes:   []string{"产物用 `GET /v1/tasks/{id}/artifacts` 取清单，再按 `content_url` 下载。"},
				},
				{
					Status: 200, Label: "已失败",
					Returns: TaskFlatFields(),
					Example: TaskExampleFlat("FAILURE"),
					Notes:   []string{"`fail_reason` 是分类文案；费用已自动退回，产物清单为空。"},
				},
			},
		},
		Notes: []string{
			"**所有异步提交端点的任务都用这一个接口查询**（视频 / 再生成 / 图像异步 / 音乐 / 歌词）：提交拿到任务号后带它来查。",
			"任务归属按密钥校验：查别人的任务一律按「不存在」处理，不泄露是否存在。",
			"产物不在任务对象里：清单用 `GET /v1/tasks/{id}/artifacts`，字节用清单里的 `content_url`（免鉴权短时链接）。",
			"`FAILURE` 同时覆盖生成失败、超时全额返还与已取消，具体原因看 `fail_reason`。",
		},
		// GET + 任务号 + KEY：能试跑。异步闭环的第二步全靠它，禁掉等于让读者提交完无处可查。
		RunMode: RunJSON,
	},

	KindTasksArtifacts: {
		Kind: KindTasksArtifacts, Name: "任务产物清单",
		Summary: "列出任务已转存的产物；每项给出可直接打开的 `content_url`。",
		Fields: []Field{
			pathParam(plat("id", TypeString, ReqRequired, "—", "任务号（`task_` 前缀）。")),
		},
		Returns: TaskArtifactFields(),
		Response: ResponseSpec{
			Variants: []ResponseVariant{
				{
					Status: 200, Label: "有产物",
					Returns: TaskArtifactFields(),
					Example: TaskExampleArtifactList(),
				},
				{
					Status: 200, Label: "还没有产物",
					Returns: TaskArtifactFields(),
					Example: Obj{
						{K: "task_id", V: "task_01J9HA7JPQ9A0Z6JZ3V8M9W6PZ"},
						{K: "artifacts", V: []any{}},
					},
					Notes: []string{"排队 / 运行中、失败、已过期都返回空数组（不是 null）。"},
				},
			},
		},
		Notes: []string{
			"序号是稠密编号：音乐出 2 首就是 `0` 和 `1`，中间不会跳号；失败与过期条目不占号。",
			"`content_url` 是**免鉴权**的短时链接，可直接给 `<img>` / `<video>` / 下载器；过期后重新调本接口。",
			"需要长期可用的地址时用带鉴权的 `GET /v1/tasks/{id}/artifacts/{key}/content`（支持 Range）。",
		},
		RunMode: RunJSON,
	},

	KindTasksArtifactContent: {
		Kind: KindTasksArtifactContent, Name: "下载任务产物",
		Summary: "按产物键下载字节（图片 / 音频 / 视频 / 歌词文本）。键就是清单里的 `key`。",
		Fields: []Field{
			pathParam(plat("id", TypeString, ReqRequired, "—", "任务号（`task_` 前缀）。")),
			pathParam(plat("key", TypeString, ReqRequired, "0", "产物键（清单里的 `key`）；越界返回 404。")),
		},
		Returns: []Field{
			ret("（文件流）", TypeBinary, "按产物类型返回 image/*、audio/*、video/* 或 text/plain；过期返回 410。"),
		},
		Response: ResponseSpec{ContentType: "image/* / audio/* / video/* / text/plain"},
		Notes: []string{
			"支持 `Range`（206 + `Content-Range`），视频拖动与断点续传直接可用。",
			"产物过期或尚未转存时返回 410（不是 404）：410 明确表示「别再重试这条路」。",
		},
		// 产物是文件字节：按二进制渲染，能直接预览 / 下载。
		RunMode: RunBinary,
	},

	KindTasksCancel: {
		Kind: KindTasksCancel, Name: "取消任务",
		Summary: "取消**仍在排队、尚未开始生成**的任务（其余一律 409），费用自动退回。终态任务不会被物理删除（对账需要）。",
		Fields: []Field{
			pathParam(plat("id", TypeString, ReqRequired, "—", "任务号（`task_` 前缀）。")),
		},
		Returns: TaskFlatFields(),
		Response: ResponseSpec{
			Variants: []ResponseVariant{{
				Status: 200, Label: "已取消",
				Returns: TaskFlatFields(),
				Example: TaskExampleFlat("FAILURE").With("fail_reason", "任务已取消"),
				Notes:   []string{"取消成功后 `status` 是 `FAILURE`（兼容词表没有单独的「已取消」），原因写在 `fail_reason`。"},
			}},
		},
		Notes: []string{
			"running / succeeded / failed / expired 都不可取消，返回 409 `task_not_cancelable`。",
		},
		// DELETE + 任务号 + KEY：能试跑。取消会退费，调试面板对 DELETE 做二次确认。
		RunMode: RunJSON,
	},

	KindVideoTaskGet: {
		Kind: KindVideoTaskGet, Name: "查询视频任务（兼容封套）",
		Summary: "与 `GET /v1/tasks/{id}` 同一份任务对象，外面包一层 `{code,message,data}` 并附主产物地址；给按第三方任务协议接入的应用使用。",
		Fields: []Field{
			pathParam(plat("id", TypeString, ReqRequired, "—", "任务号（`task_` 前缀）。")),
		},
		Returns: []Field{
			ret("code", TypeString, "成功恒为 `success`；失败时是业务错误码。").withExample("success"),
			ret("message", TypeString, "失败文案；成功为空串。").withExample(""),
			ret("data", TypeObject, "任务对象，字段同 `GET /v1/tasks/{id}`，额外带 `result_url`。"),
		},
		Response: ResponseSpec{
			Variants: []ResponseVariant{{
				Status: 200, Label: "成功",
				Returns: append([]Field{
					ret("code", TypeString, "固定 success。").withExample("success"),
					ret("message", TypeString, "固定空串。").withExample(""),
					ret("data", TypeObject, "任务对象（含 result_url）。"),
				}, TaskFlatFields()...),
				Example: Obj{
					{K: "code", V: "success"},
					{K: "message", V: ""},
					{K: "data", V: TaskExampleFlat("SUCCESS").
						With("result_url", "https://api.example.com/v1/media/eyJ2Ijoy...")},
				},
			}},
		},
		Notes: []string{
			"失败时返回 `{code:,message:,data:null}` 形状（code 是业务码字符串、data 恒为 null），**不是** OpenAI 的 `{error:{…}}`。",
			"`result_url` 只是主产物（第 0 件）；完整清单用 `GET /v1/tasks/{id}/artifacts`。",
		},
		RunMode: RunJSON,
	},

	KindVideoGet: {
		Kind: KindVideoGet, Name: "查询视频对象",
		Summary: "OpenAI Video 形状：状态四态（queued / in_progress / completed / failed）、进度是整数。与 `GET /v1/tasks/{id}` 同一个任务，只是协议面不同。",
		Fields: []Field{
			pathParam(plat("id", TypeString, ReqRequired, "—", "任务号（`task_` 前缀）。")),
		},
		Returns: []Field{
			ret("id", TypeString, "任务号（`task_` 前缀）。").withExample(flatExampleID),
			ret("task_id", TypeString, "同 `id`（兼容键，可能缺省）。"),
			ret("object", TypeString, "固定为 `video`。").withExample("video"),
			ret("model", TypeString, "模型 ID。").withExample("kling-v2"),
			ret("status", TypeString, "视频状态。").
				withEnum("queued", "in_progress", "completed", "failed").
				withNote("`failed` 同时覆盖生成失败、超时与取消 —— 视频对象没有这两个词。").
				withExample("in_progress"),
			ret("progress", TypeInteger, "进度 0 / 50 / 100（整数，不是带百分号的字符串）。").withExample(50),
			ret("created_at", TypeInteger, "创建时间（Unix 秒）。").withExample(int64(1762318830)),
			ret("completed_at", TypeInteger, "完成时间（Unix 秒，仅终态出现）。"),
			ret("error", TypeObject, "失败原因；成功时不出现。"),
			ret("error.message", TypeString, "分类文案。").withExample("任务处理失败，已全额返还"),
			ret("error.code", TypeString, "内部状态词（failed / expired / canceled）。").withExample("failed"),
		},
		Notes: []string{
			"与 `GET /v1/tasks/{id}` 返回**同一行数据**：状态词与进度类型不同是因为协议不同，不是两份数据。",
			"`expired` 与 `canceled` 在这里都显示成 `failed`，`error.code` 才区分原因。",
		},
		RunMode: RunJSON,
	},

	KindVideoContent: {
		Kind: KindVideoContent, Name: "下载视频",
		Summary: "取该视频任务的第 0 个产物（视频只有一个主产物）。支持 Range 续传；`HEAD` 只返回元信息。",
		Fields: []Field{
			pathParam(plat("id", TypeString, ReqRequired, "—", "任务号（`task_` 前缀）。")),
		},
		Returns: []Field{
			ret("（文件流）", TypeBinary, "视频字节（通常 video/mp4）；任务未产出返回 409，产物过期返回 410。"),
		},
		Response: ResponseSpec{ContentType: "video/*"},
		Notes: []string{
			"成功时是字节流（真实 MIME + nosniff，支持 `Range` → 206 与 `HEAD`）；失败时是 JSON 错误体，**必须按 Content-Type 分流**。",
			"任务还没产出时返回 409 `artifact_not_ready`（继续轮询）；产物已过期返回 410 `file_gone`（别再试）；存储后端缺失返回 503。",
		},
		RunMode: RunBinary,
	},

	// ══ 元信息与账户 ══

	KindModelsList: {
		Kind: KindModelsList, Name: "可用模型列表",
		Summary: "本密钥可用模型的清单（按密钥白名单与分组过滤，与调用权限一致），OpenAI 格式并附价格扩展。",
		Returns: []Field{
			ret("success", TypeBoolean, "固定为 true（兼容协议的列表信封）。"),
			ret("object", TypeString, "固定为 list。").withExample("list"),
			ret("data[].id", TypeString, "模型名，填进请求的 model。"),
			ret("data[].object", TypeString, "固定为 model。"),
			ret("data[].created", TypeInteger, "创建时间戳。"),
			ret("data[].owned_by", TypeString, "归属厂商。"),
			ret("data[].modality", TypeString, "模态：chat / image / video / audio / music / embedding / rerank / moderation。"),
			ret("data[].endpoints", TypeArray, "该模型可用的端点路径清单。"),
			ret("data[].available", TypeBoolean, "当前是否可调用（已上架且当前可用）。"),
			ret("data[].icon_url", TypeString, "模型图标的绝对地址（模型自己的，没有则回落所属供应商）。").
				withNote("可能为空：这家没有配图标。空值时按首字占位，**不要**自己拼 `/api/icons/**` 路径 —— 路由形态由服务端决定，不是对外契约。"),
			ret("data[].tags", TypeArray, "营销标签（按展示顺序）。无标签时是空数组，不会是 null。").
				withNote("每项含 label（文案）与 color_hex（渲染色，可直接用于文字/边框）。已过期的标签不会出现在这里。"),
			ret("data[].tags[].label", TypeString, "标签文案，最多 12 字。"),
			ret("data[].tags[].color_hex", TypeString, "渲染色（#RRGGBB）。"),
			ret("data[].pricing", TypeObject, "计费口径。").
				withNote("含 billing_mode（tokens/call/spec）、currency、prices（美元微：input/output/cache/call…）、spec_prices（按档位/秒/任务的美元微）、default。金额单位微：1 美元 = 1000000 微。"),
		},
		RunMode: RunJSON,
	},

	KindModelsGet: {
		Kind: KindModelsGet, Name: "单模型查询",
		Summary: "OpenAI 标准 models.retrieve()，查某个模型是否存在及归属。仅**已启用**的模型可查。",
		Fields: []Field{
			pathParam(plat("model", TypeString, ReqRequired, "—", "模型名。")),
		},
		Returns: []Field{
			ret("id", TypeString, "模型名。"),
			ret("object", TypeString, "固定为 model。"),
			ret("owned_by", TypeString, "归属厂商。"),
			ret("icon_url", TypeString, "模型图标的绝对地址。").withNote("**为空时整个键不出现**（不是空串），按可缺省处理。"),
			ret("tags", TypeArray, "营销标签（同 models/list 的 tags 形状）。"),
		},
		// GET + 模型名 + KEY：能试跑（读者验证某模型是否可用的最短路径）。
		RunMode: RunJSON,
	},

	KindModelsDefaults: {
		Kind: KindModelsDefaults, Name: "各类默认模型",
		Summary: "按模态返回推荐的默认模型，`allowed` 表示当前密钥是否被允许调用它。",
		Response: ResponseSpec{
			Envelope: true,
			Notes:    []string{"`data` 下每个模态一个键；示例只展开 `chat`，其余 image / video / audio / music / embedding / rerank / moderation 结构相同。"},
		},
		Returns: []Field{
			ret("chat.model", TypeString, "对话模态的默认模型。").
				withNote("`chat` 只是一个模态键；image / video / audio / music / embedding / rerank / moderation 同构。"),
			ret("chat.allowed", TypeBoolean, "当前密钥是否可用该默认模型。"),
		},
		RunMode: RunJSON,
	},

	KindAccount: {
		Kind: KindAccount, Name: "账户信息",
		Summary: "当前密钥的额度与限额（额度是平台内部单位）。",
		Response: ResponseSpec{
			Envelope: true,
			Notes:    []string{"`data.user` 只在密钥开放「查询用户额度」时出现。"},
		},
		Returns: []Field{
			ret("balance_quota", TypeNumber, "可用额度余额。"),
			ret("frozen", TypeNumber, "进行中请求的冻结额度，结算后释放。"),
			ret("used", TypeNumber, "本密钥累计已用额度。"),
			ret("request_count", TypeInteger, "本密钥累计请求次数。"),
			ret("group", TypeString, "本密钥生效的分组倍率名。"),
			ret("key", TypeObject, "密钥自身的额度上限、过期时间、限速等。").
				withNote("含 id / name / key_prefix / quota_limit / used / status / expired_at / group / rpm_limit / concurrency_limit。"),
			ret("user", TypeObject, "仅当密钥开放「查询用户额度」时返回。").
				withNote("含 username / quota / used / request_count / group / balance_limit。"),
		},
		RunMode: RunJSON,
	},

	KindUsageList: {
		Kind: KindUsageList, Name: "调用明细",
		Summary:  "按时间范围查每笔调用记录（仅当前密钥）。",
		Response: ResponseSpec{Envelope: true},
		Fields: []Field{
			queryParam(plat("start", TypeString, ReqOptional, "近 7 天", "起始时间。")).withNote("RFC3339，如 2026-10-01T00:00:00Z；格式非法会被忽略。"),
			queryParam(plat("end", TypeString, ReqOptional, "今天", "结束时间。")).withNote("RFC3339，同上。"),
			queryParam(plat("modality", TypeString, ReqOptional, "—", "按模态过滤。")),
			queryParam(plat("status", TypeString, ReqOptional, "—", "按结算状态过滤。")),
			queryParam(plat("model", TypeString, ReqOptional, "—", "按模型过滤。")),
			queryParam(plat("page", TypeInteger, ReqOptional, "1", "页码。")),
			queryParam(plat("page_size", TypeInteger, ReqOptional, "20", "每页条数，上限 100。")),
		},
		Returns: []Field{
			ret("items", TypeArray, "明细：模型 / 词块 / 扣费 / 时间 / 状态。"),
			ret("total", TypeInteger, "总条数。"),
			ret("page", TypeInteger, "当前页。"),
			ret("page_size", TypeInteger, "每页条数。"),
		},
		RunMode: RunJSON,
	},

	KindUsageSummary: {
		Kind: KindUsageSummary, Name: "用量汇总",
		Summary: "按模型汇总当前密钥的用量（与调用明细同一份数据实时统计）。",
		Response: ResponseSpec{
			Envelope: true,
			Notes:    []string{"`items` 按扣费从高到低排序；没有用量时是空数组。"},
		},
		Returns: []Field{
			ret("items[].model", TypeString, "模型名。"),
			ret("items[].requests", TypeInteger, "调用次数。"),
			ret("items[].prompt_tokens", TypeInteger, "输入词块合计。"),
			ret("items[].completion_tokens", TypeInteger, "输出词块合计。"),
			ret("items[].charge", TypeInteger, "扣费额度合计。"),
		},
		RunMode: RunJSON,
	},

	KindBillingSubscription: {
		Kind: KindBillingSubscription, Name: "订阅配额（兼容）",
		Summary: "兼容生态客户端（NextChat / LobeChat 等）的余额显示接口。",
		Returns: []Field{
			ret("hard_limit_usd", TypeNumber, "总额度折算美元。"),
			ret("system_hard_limit_usd", TypeNumber, "系统级总额度折算美元。"),
			ret("access_until", TypeInteger, "到期时间戳。"),
		},
		RunMode: RunJSON,
	},

	KindBillingUsage: {
		Kind: KindBillingUsage, Name: "累计用量（兼容）",
		Summary: "兼容生态客户端的已用金额接口，与订阅配额配对使用。",
		Fields: []Field{
			queryParam(plat("start_date", TypeString, ReqOptional, "近 30 天", "统计起始日期。")).withNote("YYYY-MM-DD"),
			queryParam(plat("end_date", TypeString, ReqOptional, "今天", "统计结束日期。")).withNote("YYYY-MM-DD"),
		},
		Returns: []Field{
			ret("total_usage", TypeNumber, "已用金额，单位 0.01 美元。"),
			ret("object", TypeString, "固定为 list。"),
		},
		RunMode: RunJSON,
	},

	// ══ 平台自有 ══

	KindMediaProxy: {
		Kind: KindMediaProxy, Name: "产物代理",
		Summary: "同步图像等产物通过这个地址回传，链接本身即凭证，可直接放进 img / video 标签。",
		Fields: []Field{
			pathParam(plat("token", TypeString, ReqRequired, "—", "平台签发的能力串，由地址与过期时间签名得到。")),
		},
		Returns: []Field{
			ret("（文件流）", TypeBinary, "媒体二进制，支持断点续传（Range）。"),
		},
		Response: ResponseSpec{ContentType: "按原媒体类型"},
		Notes: []string{
			"无需携带 Authorization；链接有过期时间，长期保存请自行转存。",
			"只有图片 / 音频 / 视频会被内联展示，其他类型按附件下载（不会被当成网页执行）。",
			"失败时**不是 JSON**：非 GET/HEAD 返回 405、签名错误 404、已过期 410、限流 429，body 均为纯文本。",
		},
		RunMode: RunBinary,
	},
}

// EndpointDocOf 取端点字段契约；未登记返回 false。
//
// 未登记是**编程错误**（新增 Kind 忘了写契约），由 TestEveryKindHasFieldDoc 拦。
// 这里返回 false 而不是零值，是为了让调用方能显式决定降级方式。
func EndpointDocOf(k Kind) (EndpointDoc, bool) {
	d, ok := endpointDocs[k]
	return d, ok
}

// MustEndpointDoc 取端点字段契约，缺则 panic（只用于渲染与测试）。
func MustEndpointDoc(k Kind) EndpointDoc {
	d, ok := endpointDocs[k]
	if !ok {
		panic("apidoc: 端点 " + string(k) + " 缺少字段契约")
	}
	return d
}
