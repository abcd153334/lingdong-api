// Package geminiconv 是 Google Generative Language 协议与 OpenAI Chat 协议之间的唯一转换处。
//
// 方向只有一条：Gemini 入站 → OpenAI Chat 中枢 → 上游（与 claudeconv 完全对称）。
// 上游线格式仍只有 OpenAI 一种，所以不需要 N×M 矩阵；将来接入 Gemini 原生上游时，
// 只需再加一个"出站编码器"，入站侧不用动。
//
// 为什么单独一个包、且不 import relay：
//   - relay 要调用它（入站解码、出站编码），反向 import 会成环；
//   - 字段映射必须能**脱离 HTTP 与数据库单测**：错一个字段在真机上只表现为
//     "参数没生效 / 金额不对"，几乎无法定位。
//
// 与 claudeconv 的差异：Gemini 的 model 与 stream 来自 **URL**（`models/{model}:generateContent`），
// 所以 Codec 是**构造态**（每次请求新建一个值），而 claudeconv 的 Codec 是无状态的。
package geminiconv

// Codec 是一次请求的编解码器。
//
// Model 与 Stream 由路由层从 URL 切出来注入：Gemini 的请求体里 model 是可选的，
// 权威来源是路径；stream 由 `:streamGenerateContent` 与 `?alt=sse` 共同决定。
// 不注入的话 relay 会在"缺 model"和"判不出流式"两步上直接 400。
type Codec struct {
	Model  string
	Stream bool
	// Embed 表示这次走向量化端点（:embedContent 与 /v1/engines/{model}/embeddings）。
	//
	// 单独一个标志而不是靠"有没有 contents"猜：向量化与生成是**两套**请求/响应形状，
	// 猜错的表现是"200 但拿不到向量"（客户端解析 embedding.values 得到空）。
	Embed bool
}
