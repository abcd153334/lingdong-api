package docdocs

import (
	"lingdong-api/internal/apidoc"
)

// baselinePages 是出厂说明页（顺序即侧栏顺序）。
//
// 内容刻意短：说明页的读者是「刚拿到密钥、想跑通第一个请求」的人，
// 长篇接入指南只会让他在第三段放弃。深度内容归各模型组页，不归这里。
func baselinePages() []struct {
	Slug   string
	Title  string
	Sort   int
	Blocks []apidoc.GuideBlock
} {
	return []struct {
		Slug   string
		Title  string
		Sort   int
		Blocks []apidoc.GuideBlock
	}{
		{
			Slug: "quickstart", Title: "快速开始", Sort: 10,
			Blocks: []apidoc.GuideBlock{
				{Type: apidoc.BlockPara, Text: "所有接口都是标准 HTTP，地址为 `{{base_url}}/v1`。官方 OpenAI SDK 只要改 `base_url` 就能直接用。"},
				{Type: apidoc.BlockSteps, Title: "四步跑通", Items: []string{
					"在控制台创建 API Key",
					"把 Key 放进请求头：`Authorization: Bearer <KEY>`",
					"选一个模型：`GET /v1/models` 列出当前可用模型",
					"按该模型的文档发送请求（左侧按模型类型分组）",
				}},
				{Type: apidoc.BlockCode, Title: "第一个请求", Lang: "bash", Code: `curl {{base_url}}/v1/models \
  -H "Authorization: Bearer $API_KEY"`},
				// 口径与 kind.go 的 Transport 逐条对齐：
				// images/generations 是 PhaseSync + AcceptsAsync（同一路径两种模式）；
				// video / music / lyrics 是 PhaseSubmit（端点本身就是任务）。
				// 从前写「图像…是异步的」，与同站另一篇「图片同步返回」直接打架；
				// 后来又把图像拆成两条路径，让用户不知道该用哪个地址。
				{Type: apidoc.BlockNote, Text: "视频与音乐只有异步：提交后拿到任务号，再轮询查询结果；图像默认同步（请求内返回），长任务在同一个接口加 `async: true` 即返回任务号；对话、向量、重排、审核都在请求内返回。"},
				// 同步端点的重试安全：出图要 20–60 秒，客户端（SDK、网关、负载均衡）
				// 普遍在 30/60 秒断开并自动重发。没有幂等键时，那次重发就是一笔新订单 ——
				// 功能上线了却在文档站上找不到任何说明，等于让每个用户自己踩一遍。
				{Type: apidoc.BlockNote, Text: "同步接口可以带 `Idempotency-Key: <随机串>` 请求头：同一个键在同一个接口上重复提交时，平台只执行并计费一次，第二次返回 `409` 与首次的计费记录号。超时重试请沿用同一个键，换一次新请求请换新键（用 UUID 即可）。失败且未计费的请求不会占用这个键，改正参数后可以直接重试。"},
			},
		},
		{
			Slug: "auth", Title: "鉴权与密钥", Sort: 20,
			Blocks: []apidoc.GuideBlock{
				{Type: apidoc.BlockPara, Text: "所有 `/v1/*` 请求都需要在请求头带上 API Key。不同 Key 可以分别设置额度与模型权限。"},
				{Type: apidoc.BlockCode, Lang: "bash", Code: `Authorization: Bearer sk-xxxxxxxxxxxxxxxx`},
				{Type: apidoc.BlockBullets, Title: "三条注意", Items: []string{
					"Key 只在创建时完整显示一次，之后无法再查看",
					"怀疑泄露时立即在控制台删除并重新创建",
					"不要写进前端代码或公开仓库：浏览器里的请求头谁都能看到",
				}},
				{Type: apidoc.BlockNote, Text: "余额不足时接口返回 402，请先在控制台充值。"},
			},
		},
		{
			Slug: "errors", Title: "错误码", Sort: 30,
			Blocks: []apidoc.GuideBlock{
				{Type: apidoc.BlockPara, Text: "出错时返回 OpenAI 兼容的错误体，`message` 是给人看的，`type` 适合做条件判断。"},
				{Type: apidoc.BlockCode, Title: "错误体", Lang: "json", Code: `{
  "error": {
    "message": "参数 size 不支持该值",
    "type": "invalid_request_error",
    "code": "invalid_parameter"
  }
}`},
				{Type: apidoc.BlockBullets, Title: "常见状态码", Items: []string{
					"400 参数不合法：字段名、类型或取值超出该模型支持范围",
					"401 密钥无效或已删除",
					"402 额度不足",
					"404 模型、任务、文件或说明页不存在",
					"409 状态冲突：任务已开始生成不可取消、任务尚未结束不可清理，或 `Idempotency-Key` 重复提交（`idempotent_replay`，本次未扣费）",
					"410 已过期：产物超过保留期、临时文件超过 24 小时",
					"416 下载时 Range 起点超过文件长度",
					"429 请求过于频繁，稍后重试",
					"500 服务端异常",
					"501 路径未实现：该地址不是本平台提供的接口（通常是拼错了路径）",
					"503 该模型暂时不可用，请稍后或换模型",
				}},
				{Type: apidoc.BlockNote, Text: "4xx 除 429 外不要重试（同样的请求会同样失败）；5xx 与 429 建议退避重试，最多 3 次。"},
				{Type: apidoc.BlockNote, Text: "501 不是「模型不支持」：模型不支持某个端点时返回 400 `endpoint_unsupported`，两者别混。"},
			},
		},
	}
}
