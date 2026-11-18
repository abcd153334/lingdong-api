package brandicon

// catalog 是内置图标的**唯一清单**（72 条，与 icons/ 目录一一对应）。
//
// 维护规则：
//   - Key 一旦发布**不许改名**：存量配置里存的是 `builtin:<key>`，改名等于让它
//     静默退回首字占位，且没有任何报错；
//   - 新增图标要同时提交 icons/<key>.png，否则 embed_test 会红（go:embed 缺文件
//     编译照样过，测试是唯一防线）；
//   - Aliases 是搜索别名（中文厂商名/产品名），不参与展示。
var catalog = []Item{
	// ---------- 大模型厂商 ----------
	{Key: "01ai-yi", Name: "01.AI Yi", Group: GroupLLM, Aliases: []string{"零一万物", "Yi"}},
	{Key: "360-ai360", Name: "360 AI360", Group: GroupLLM, Aliases: []string{"360", "智脑"}},
	{Key: "ai21-labs", Name: "AI21 Labs", Group: GroupLLM, Aliases: []string{"Jurassic"}},
	{Key: "alibaba", Name: "Alibaba", Group: GroupLLM, Aliases: []string{"阿里巴巴", "阿里"}},
	{Key: "alibaba-qwen", Name: "Qwen", Group: GroupLLM, Aliases: []string{"通义千问", "通义", "千问"}},
	{Key: "allenai-olmo", Name: "OLMo", Group: GroupLLM, Aliases: []string{"AllenAI"}},
	{Key: "amazon-aws-nova", Name: "AWS Nova", Group: GroupLLM, Aliases: []string{"亚马逊", "AWS"}},
	{Key: "anthropic-claude", Name: "Claude", Group: GroupLLM, Aliases: []string{"Anthropic", "克劳德"}},
	{Key: "apple-intelligence", Name: "Apple Intelligence", Group: GroupLLM, Aliases: []string{"苹果"}},
	{Key: "baichuan", Name: "Baichuan", Group: GroupLLM, Aliases: []string{"百川"}},
	{Key: "baidu", Name: "Baidu", Group: GroupLLM, Aliases: []string{"百度"}},
	{Key: "baidu-ernie-wenxin", Name: "ERNIE", Group: GroupLLM, Aliases: []string{"文心一言", "文心", "百度"}},
	{Key: "bytedance", Name: "ByteDance", Group: GroupLLM, Aliases: []string{"字节跳动", "字节"}},
	{Key: "bytedance-doubao", Name: "Doubao", Group: GroupLLM, Aliases: []string{"豆包", "字节"}},
	{Key: "cohere", Name: "Cohere", Group: GroupLLM, Aliases: []string{}},
	{Key: "deepseek", Name: "DeepSeek", Group: GroupLLM, Aliases: []string{"深度求索"}},
	{Key: "github-copilot", Name: "GitHub Copilot", Group: GroupLLM, Aliases: []string{"GitHub"}},
	{Key: "google-deepmind", Name: "DeepMind", Group: GroupLLM, Aliases: []string{"谷歌", "Google"}},
	{Key: "google-gemini", Name: "Gemini", Group: GroupLLM, Aliases: []string{"谷歌", "双子座"}},
	{Key: "huawei-pangu", Name: "Pangu", Group: GroupLLM, Aliases: []string{"华为", "盘古"}},
	{Key: "ibm-granite", Name: "Granite", Group: GroupLLM, Aliases: []string{"IBM"}},
	{Key: "iflytek-spark", Name: "Spark", Group: GroupLLM, Aliases: []string{"讯飞", "星火"}},
	{Key: "inflection-pi", Name: "Pi", Group: GroupLLM, Aliases: []string{"Inflection"}},
	{Key: "kunlun-tiangong", Name: "Tiangong", Group: GroupLLM, Aliases: []string{"昆仑万维", "天工"}},
	{Key: "meituan-longcat", Name: "LongCat", Group: GroupLLM, Aliases: []string{"美团", "龙猫"}},
	{Key: "meta-llama", Name: "Llama", Group: GroupLLM, Aliases: []string{"Meta"}},
	{Key: "microsoft-copilot", Name: "Copilot", Group: GroupLLM, Aliases: []string{"微软"}},
	{Key: "minimax", Name: "MiniMax", Group: GroupLLM, Aliases: []string{}},
	{Key: "mistral-ai", Name: "Mistral", Group: GroupLLM, Aliases: []string{}},
	{Key: "moonshot-kimi", Name: "Kimi", Group: GroupLLM, Aliases: []string{"月之暗面", "Moonshot"}},
	{Key: "nousresearch-hermes", Name: "Hermes", Group: GroupLLM, Aliases: []string{"NousResearch"}},
	{Key: "openai-gpt", Name: "OpenAI", Group: GroupLLM, Aliases: []string{"GPT", "开放人工智能"}},
	{Key: "perplexity", Name: "Perplexity", Group: GroupLLM, Aliases: []string{}},
	{Key: "salesforce-einstein", Name: "Einstein", Group: GroupLLM, Aliases: []string{"Salesforce"}},
	{Key: "sensetime-sensenova", Name: "SenseNova", Group: GroupLLM, Aliases: []string{"商汤", "日日新"}},
	{Key: "shanghaiai-lab-internlm", Name: "InternLM", Group: GroupLLM, Aliases: []string{"书生", "上海AI实验室"}},
	{Key: "stepfun-step", Name: "Step", Group: GroupLLM, Aliases: []string{"阶跃星辰", "跃问"}},
	{Key: "tencent", Name: "Tencent", Group: GroupLLM, Aliases: []string{"腾讯"}},
	{Key: "tencent-hunyuan", Name: "Hunyuan", Group: GroupLLM, Aliases: []string{"混元", "腾讯"}},
	{Key: "vivo-bluelm", Name: "BlueLM", Group: GroupLLM, Aliases: []string{"蓝心", "vivo"}},
	{Key: "volcengine", Name: "Volcengine", Group: GroupLLM, Aliases: []string{"火山引擎", "火山", "方舟"}},
	{Key: "xai-grok", Name: "Grok", Group: GroupLLM, Aliases: []string{"xAI"}},
	{Key: "xiaomi-mimo", Name: "MiMo", Group: GroupLLM, Aliases: []string{"小米"}},
	{Key: "zhipu-glm", Name: "GLM", Group: GroupLLM, Aliases: []string{"智谱", "清言"}},

	// ---------- 图像 / 视频 / 音频 ----------
	{Key: "adobe-firefly", Name: "Adobe Firefly", Group: GroupMedia, Aliases: []string{"Adobe"}},
	{Key: "blackforestlabs-flux", Name: "FLUX", Group: GroupMedia, Aliases: []string{"黑森林", "BlackForestLabs"}},
	{Key: "elevenlabs", Name: "ElevenLabs", Group: GroupMedia, Aliases: []string{"语音合成"}},
	{Key: "ideogram", Name: "Ideogram", Group: GroupMedia, Aliases: []string{}},
	{Key: "kolors", Name: "Kolors", Group: GroupMedia, Aliases: []string{"可图", "快手"}},
	{Key: "kuaishou-kling", Name: "Kling", Group: GroupMedia, Aliases: []string{"可灵", "快手"}},
	{Key: "luma-ai", Name: "Luma", Group: GroupMedia, Aliases: []string{"Dream Machine"}},
	{Key: "midjourney", Name: "Midjourney", Group: GroupMedia, Aliases: []string{"MJ"}},
	{Key: "openai-sora", Name: "Sora", Group: GroupMedia, Aliases: []string{"OpenAI"}},
	{Key: "pika", Name: "Pika", Group: GroupMedia, Aliases: []string{}},
	{Key: "pixverse", Name: "PixVerse", Group: GroupMedia, Aliases: []string{"爱诗"}},
	{Key: "runway", Name: "Runway", Group: GroupMedia, Aliases: []string{}},
	{Key: "shengshu-vidu", Name: "Vidu", Group: GroupMedia, Aliases: []string{"生数"}},
	{Key: "stability-ai", Name: "Stable Diffusion", Group: GroupMedia, Aliases: []string{"SD", "Stability"}},
	{Key: "suno", Name: "Suno", Group: GroupMedia, Aliases: []string{"音乐"}},

	// ---------- 云平台 / 推理服务 / 工具 ----------
	{Key: "autodl", Name: "AutoDL", Group: GroupPlatform, Aliases: []string{"算力云"}},
	{Key: "cerebras", Name: "Cerebras", Group: GroupPlatform, Aliases: []string{}},
	{Key: "databricks-dbrx", Name: "DBRX", Group: GroupPlatform, Aliases: []string{"Databricks"}},
	{Key: "duomi", Name: "Duomi", Group: GroupPlatform, Aliases: []string{"多米"}},
	{Key: "fireworks-ai", Name: "Fireworks AI", Group: GroupPlatform, Aliases: []string{}},
	{Key: "groq", Name: "Groq", Group: GroupPlatform, Aliases: []string{}},
	{Key: "huggingface", Name: "Hugging Face", Group: GroupPlatform, Aliases: []string{"hf"}},
	{Key: "lm-studio", Name: "LM Studio", Group: GroupPlatform, Aliases: []string{"本地部署"}},
	{Key: "nvidia", Name: "NVIDIA", Group: GroupPlatform, Aliases: []string{"英伟达"}},
	{Key: "ollama", Name: "Ollama", Group: GroupPlatform, Aliases: []string{"本地部署"}},
	{Key: "replicate", Name: "Replicate", Group: GroupPlatform, Aliases: []string{}},
	{Key: "sambanova", Name: "SambaNova", Group: GroupPlatform, Aliases: []string{}},
	{Key: "together-ai", Name: "Together AI", Group: GroupPlatform, Aliases: []string{}},
}
