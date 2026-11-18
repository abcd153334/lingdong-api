package relay

// 冻结估算的全部常量（唯一处）。
//
// 为什么单独一个文件：这些数值直接决定「先冻多少钱」，散落在
// estimateTokens / estimateTextTokens / estimatePromptTokens 里时，
// 调一个忘一个 —— 表现是"某个端点的冻结额莫名其妙偏低"，而代码看上去都合理。
//
// 方向性约定：估算宁可**偏大**。多冻的钱在结算时原路退回（只影响余额边缘用户
// 能否下单），少冻的钱会在结算时穿透冻结额并被 3 倍截顶吃掉，那是确定性少收。
const (
	// tokensPerCJKChar 是中日韩字符的 token 粗略系数。
	// 主流 BPE（cl100k / o200k）对中文约 1–1.5 token/字，取上界 1.6。
	tokensPerCJKChar = 1.6
	// charsPerTokenASCII 是「多少 ASCII 字符一个 token」。
	// 自然英文约 4，代码与 JSON（大量符号）常到 3，取偏小值让估算偏大。
	charsPerTokenASCII = 3.5
	// tokensPerMessage 是每条消息的固定开销（role、分隔符、隐式模板）。
	tokensPerMessage = 4

	// tokensPerImagePart 是单张图片按 token 计的保守值。
	// 1024×1024 高细节约 765 token，低细节约 85；图片模型常按图计费不按 token，
	// 这里覆盖的是「chat 里带图、上游按视觉 token 收费」的情况。
	tokensPerImagePart = 1024
	// tokensPerAudioSecond 是音频输入每秒折合的 token 上限。
	tokensPerAudioSecond = 10
	// audioBytesPerSecond 是估算音频秒数用的字节率：WAV 16bit/16kHz 单声道 = 32KB/s。
	// mp3/aac 实际码率更低 → 按同一字节数会低估秒数？不会：这里用
	// bytes/audioBytesPerSecond 得到的是**下限**秒数，再乘每秒 token 上限，
	// 对高压缩格式偏小；但音频输入主要走按秒/按字符计费，token 口径只影响
	// 「chat 里塞 base64 音频」这种少见形态，取该值即可。
	audioBytesPerSecond = 32000

	// asrBytesPerSecond 是「上传音频字节 → 秒数」的**下界码率**（64kbps = 8KB/s）。
	//
	// 只用于 ASR（transcriptions / translations）的**冻结**估算：这两个端点在上游
	// 返回 usage 之前根本不知道时长，冻结 0 会被结算期的「零冻结上限」截顶，
	// 长音频系统性少收（1 小时 0.01 元/秒 = 36 元，上限只有 2 元）。
	//
	// 取 8KB/s 而不是 WAV 的 32KB/s：码率越高，同一字节数对应的真实秒数越短，
	// 按低码率算出来的秒数是**上限**，冻结偏大；多冻的部分结算时原路退回，
	// 少冻的部分会变成确定性少收 —— 方向必须是这一边。
	asrBytesPerSecond = 8000

	// defaultMaxOutputTokens 是「请求没写任何输出上限、模型也没声明」时的冻结上限。
	//
	// 原值 1024 是长期少收的主因：实际输出 4000 token 的请求，冻结只按 1024 算，
	// 结算额 4 倍于冻结额 → 被 maxChargeMultiple 截顶 → 少收 3/4。
	// 8192 是当前主流模型的常见单次输出上限；模型声明了 max_output_tokens 时优先用它。
	defaultMaxOutputTokens = 8192
	// maxCompletionCap 是冻结时对输出 token 的绝对封顶。
	//
	// 与 model.DefaultAgentMaxOutputTokens（agent_limits.go）保持同值：
	// 两个口径一旦分叉，同一个请求在 Agent 与网关看到的"最大输出"会不一致。
	maxCompletionCap = 32768
)

// 结算侧的 3 倍截顶（maxChargeMultiple）在 billing 包，不在本文件：
// 「冻结估算」与「结算上限」是两件事，混在一处会被误改。
