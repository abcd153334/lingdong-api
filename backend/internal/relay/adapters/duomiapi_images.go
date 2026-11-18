package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/pkg/taskstate"
	"lingdong-api/internal/relay"
	"lingdong-api/internal/service/billing"
)

// 本文件实现多米API 的**图像**子协议（B6-1）：
//
//	nano       POST /api/gemini/nano-banana       （无参考图）
//	           POST /api/gemini/nano-banana-edit  （有参考图）
//	           GET  /api/gemini/nano-banana/{id}  （两个端点共用这一个查询）
//	gptimage   POST /v1/images/generations?async=true   GET /v1/tasks/{id}
//
// 三条设计约束：
//  1. **出站体白名单重建**（与视频同口径）：平台的 normalizeSubmitBody 会把 image 改写成
//     input_reference、把 width/height 折成 resolution，原样透传会让图像模型收到视频字段直接 400。
//  2. **任务号必须取到**：200 但拿不到 task_id 一律判失败。当成成功会让任务空转到 deadline，
//     用户白等 + 平台退费，而且渠道健康度看起来完全正常。
//  3. **产物以「上游实际给出的链接」为准**：nano 的返回形状在上游不同版本里出现过
//     data:["url"] / data:{images:[{url}]} / data:{output:"url"} 三种，
//     因此用带键白名单的宽容遍历收集链接，宁可多收一个也不要漏（漏 = 已生成的图被丢弃）。

const (
	// duomiNanoT2IPath 是文生图端点，也是**任务查询**的唯一前缀。
	duomiNanoT2IPath = "/api/gemini/nano-banana"
	// duomiNanoEditPath 是图生图 / 图片编辑端点（必须带 image_urls）。
	duomiNanoEditPath   = "/api/gemini/nano-banana-edit"
	duomiGPTImageSubmit = "/v1/images/generations"
	duomiGPTImagePoll   = "/v1/tasks"

	// duomiMaxNanoRefImages 是编辑端点的参考图上限（官方：数组最多 10 张、只收链接）。
	duomiMaxNanoRefImages = 10
	// duomiMaxGPTImageRefs 是 gpt-image 的 image 数组上限。
	//
	// 文档未给出明确数字，这里取一个**保守上界**而不是不设限：
	// 不设限等于允许调用方把 500 个 URL 塞进一个请求（出站体膨胀 + 上游按张计费）。
	duomiMaxGPTImageRefs = 16
	// 上限的唯一来源是 apidoc（页面上的「≤ N 字符」由同一个常量渲染）。
	duomiMaxNanoPromptRunes = apidoc.NanoPromptMaxRunes
	// duomiMaxGPTImagePromptRunes 是 gpt-image 的 prompt 上限。
	//
	// 取值依据是**多米的官方文档原文**（`API文档/多米API/gpt-image-2/生成图片.md`：
	// 「所需图像的文本描述。最大长度为 5000 个字符」），与对外文档写的 5000 一致。
	// 曾经按 OpenAI 契约写成 32000：平台放行、上游 400，用户拿到的是一句看不出来源
	// 的上游原话，而文档上写的 5000 反而是对的 —— 代码比文档更宽松同样是"文档不准"。
	// 两个协议分开：nano 是 4000，拿它卡 gpt-image 会误拒合法的长描述。
	duomiMaxGPTImagePromptRunes = apidoc.GPTImagePromptMaxRunes
)

// duomiImageSubmitPath 返回图像子协议的提交路径（channels.adapter_options.paths 可覆盖）。
//
// edit 由「有没有参考图」决定，不由模型决定：官方两个端点用的是同一套 model 枚举，
// 把端点写进模型档案会逼出「同一个模型只能文生图」的假约束。
// 覆盖能力是刻意的：多米的图像代理换过路径前缀，改配置比发版快。
func duomiImageSubmitPath(opts *duomiOptions, edit bool) string {
	key, fallback := "nano.submit", duomiNanoT2IPath
	if edit {
		key, fallback = "nano.edit", duomiNanoEditPath
	}
	if p := strings.TrimSpace(opts.paths[key]); p != "" {
		return p
	}
	return fallback
}

// duomiImagePollPath 返回图像子协议的任务查询路径（不含任务号）。
//
// nano 系把任务号拼在路径后面（`/api/gemini/nano-banana/{id}`），**两个提交端点共用它**；
// gpt-image 是 OpenAI 形状的独立任务端点（`/v1/tasks/{id}`）。
func duomiImagePollPath(opts *duomiOptions, proto string) string {
	switch proto {
	case duomiProtoGPTImage:
		if p := strings.TrimSpace(opts.paths["gptimage.poll"]); p != "" {
			return strings.TrimRight(p, "/")
		}
		return duomiGPTImagePoll
	}
	if p := strings.TrimSpace(opts.paths["nano.poll"]); p != "" {
		return strings.TrimRight(p, "/")
	}
	return duomiNanoT2IPath
}

// duomiWithQuery 在路径后追加查询串（路径本身已带 ? 时用 & 连接）。
func duomiWithQuery(path, query string) string {
	if strings.TrimSpace(query) == "" {
		return path
	}
	if strings.Contains(path, "?") {
		return path + "&" + query
	}
	return path + "?" + query
}

// ---------------------------------------------------------------- 提交

// duomiImageSizeOf 从请求体解析尺寸规格（**异步链路专用**）。
//
// 同步链路的尺寸由网关统一解析并注入 `RelayRequest.ImageSize`，适配器直接用；
// 异步链路没有网关那一层，所以在这里解析**唯一的一次**（无模型价目，故不含档位归一：
// 适配器只需要用户声明了什么，不需要它值多少钱）。
func duomiImageSizeOf(body *duomiBody) billing.ImageSize {
	return billing.ParseImageSize(billing.ImageDecl{
		Size:        body.Size,
		ImageSize:   body.ImageSize,
		Resolution:  body.Resolution,
		AspectRatio: firstNonEmptyStr(body.AspectRatio, body.Ratio),
	})
}

// submitDuomiImage 分发图像子协议的提交。
//
// im 是**网关注入的尺寸归一结果**（`RelayRequest.ImageSize` 同源）：同步链路直接把网关
// 解析好的对象传进来，异步链路在 SubmitTask 里解析一次。适配器**不再自己解析请求体**——
// 同一语义在两处各解释一次，必然出现「计费认 2K、出站什么都没发」这种静默分叉
// （2026-11-04 实测：同步链路把 image_size 丢在 duomiBuildSyncImageBody 里）。
func submitDuomiImage(ctx context.Context, cfg *relay.ChannelConfig, opts *duomiOptions,
	profile duomiProfile, upstream string, body *duomiBody, im billing.ImageSize) (string, string, error) {
	switch profile.Protocol {
	case duomiProtoNano:
		return submitDuomiNano(ctx, cfg, opts, profile, upstream, body, im)
	case duomiProtoGPTImage:
		return submitDuomiGPTImage(ctx, cfg, opts, profile, upstream, body, im)
	default:
		return "", "", duomiCfgErr("model_overrides", fmt.Sprintf("协议 %q 暂未实现", profile.Protocol))
	}
}

// duomiNanoLocalOnly 是 nano 出站时必须丢弃的键：平台已消费，或已翻译成别的表示。
//
// 只装这两类，**不装**「上游不认的业务键」—— 那些必须原样下发（由上游决定收不收）。
//   - image：改名为 image_urls；
//   - size / resolution：已由 ImageSize 翻译成 image_size / aspect_ratio，原键再发是重复表达；
//   - response_format：平台本地决定返回 b64 还是直链，上游没有这个键；
//   - seconds / duration / input_reference：平台归一产物，图像协议没有这些语义。
//
// mask_url **不在**这里：平台字段名就是 mask_url，出站不改名、不丢弃。
// `mask` 是 gpt-image 那边归一的别名，nano 端点没有蒙版语义 —— 丢弃它，
// 免得出站报文里出现平台没有的字段名。
var duomiNanoLocalOnly = []string{
	"mask", "image", "image_urls", "size", "resolution", "ratio", "response_format",
	"seconds", "duration", "input_reference",
}

// duomiGPTImageLocalOnly 是 gpt-image 出站时必须丢弃的键（同 nano 的分类）。
//
// mask_url 同样不在其中：适配器把它原样写进 payload["mask_url"]，dst 已有键 ⇒
// ForwardUnmapped 天然跳过，不需要在 skip 表里再列一次。
// mask 不在其中：它已在 duomiMaskURL 里归一成 mask_url，若再透传就会同时出现两个名字。
var duomiGPTImageLocalOnly = []string{
	"mask", "image_urls", "response_format",
	"resolution", "image_size", "aspect_ratio", "ratio",
	"seconds", "duration", "input_reference",
}

// duomiNanoSubmitRefs 取 nano 编辑端点的参考图。
//
// 兼容两种入参形态：`image_urls`（平台内部归一后的数组）与 `image`（OpenAI 官方字段）。
// 只认前者会让「按官方文档传 image」的请求整张图丢失 —— 编辑端点随后退化成文生图。
func duomiNanoSubmitRefs(body *duomiBody) ([]string, error) {
	if len(body.ImageURLs) > 0 {
		return duomiRefImageURLs(body.ImageURLs, duomiMaxNanoRefImages, "image_urls")
	}
	if hasSyncImage(body.Image) {
		return duomiSyncImageURLs(body.Image)
	}
	return nil, nil
}

// submitDuomiNano 提交 nano（Gemini 图像）任务。
//
// 端点由**有没有参考图**决定（与官方一致）：
//
//	无图 → /api/gemini/nano-banana        体 {model, prompt, aspect_ratio?, image_size?}
//	有图 → /api/gemini/nano-banana-edit   体 {model, prompt, image_urls[], aspect_ratio?, image_size?}
//
// 两个端点共用同一套 model 枚举与同一个查询端点，所以「编辑」不是模型属性。
func submitDuomiNano(ctx context.Context, cfg *relay.ChannelConfig, opts *duomiOptions,
	profile duomiProfile, upstream string, body *duomiBody, im billing.ImageSize) (string, string, error) {
	prompt, err := duomiImagePrompt(body, duomiMaxNanoPromptRunes)
	if err != nil {
		return "", "", err
	}
	// quality 是 gpt-image 的字段，nano 没有它 —— 但「nano 支不支持」由上游回答，
	// 平台不再本地拒绝（拒绝等于替上游判能力，上游哪天支持了用户也拿不到）。
	refs, rerr := duomiNanoSubmitRefs(body)
	if rerr != nil {
		return "", "", rerr
	}
	edit := len(refs) > 0

	payload := map[string]any{"model": upstream, "prompt": prompt}
	// 尺寸只下发**用户声明过**的值：nano 的字段叫 image_size，取值原样透传
	// （档位词 / 像素写法都照用户写的发，上游认不认由它决定）。
	// 用户没写就不发 —— 平台不替上游决定默认分辨率，计费默认档由后台配置负责。
	if v := duomiNanoImageSize(im); v != "" {
		payload["image_size"] = v
	}
	if v := strings.TrimSpace(im.Ratio); v != "" {
		payload["aspect_ratio"] = v
	}
	if edit {
		payload["image_urls"] = refs
	}
	duomiPutN(payload, body.N)
	// 未映射的顶层键原样下发（quality / background / mask / seed / 厂商私有键…）。
	relay.ForwardUnmapped(payload, body.raw, duomiNanoLocalOnly...)

	path := duomiImageSubmitPath(opts, edit)
	buf, derr := duomiDo(ctx, cfg, opts.authPrefix, http.MethodPost, path, payload)
	if derr != nil {
		return "", "", derr
	}
	if berr, bad := duomiEnvelopeError(buf); bad {
		return "", "", berr
	}
	id, ierr := duomiExtractTaskID(buf)
	if ierr != nil {
		return "", "", ierr
	}
	return id, duomiProtoNano, nil
}

// duomiNanoImageSize 返回要下发的 image_size（空 = 不下发）。
//
// nano 的 image_size 只收档位词（1K/2K/4K，K 必须大写）。按**语义**分派，
// 不给"顺手塞进去"留空间：
//   - 像素写法 → 原文透传；
//   - 档位写法 → 归一后的大写档位；
//   - 画幅写法（`16:9`）→ **不下发**：它属于 aspect_ratio，塞进 image_size 是非法值；
//   - auto / 没写 → 不下发（image_size 没有 auto 取值）；
//   - 其余上游私有写法 → 原文透传，由上游判定。
func duomiNanoImageSize(im billing.ImageSize) string {
	switch im.Source {
	case billing.ImageSizeSourcePixels:
		return strings.TrimSpace(im.Raw)
	case billing.ImageSizeSourceTier:
		return strings.ToUpper(strings.TrimSpace(im.DeclaredTier))
	case billing.ImageSizeSourceRatio, billing.ImageSizeSourceAuto:
		// 画幅只进 aspect_ratio；auto 在 image_size 里没有取值。
		// 这里按 Source（唯一语义）分派，不再用 `Unknown == Ratio` 这种隐式不变量：
		// 一旦用户同时写了 size=16:9 与 aspect_ratio=4:3，Ratio 被后者覆盖，
		// 旧判据就会失效并把 "16:9" 塞进 image_size（上游直接 400）。
		return ""
	}
	// 其余（上游私有写法）：原文透传，由上游判定。
	return strings.TrimSpace(im.Raw)
}

// duomiGPTImageSize 把**平台档位词 + 画幅**换成上游像素；像素 / 比例 / auto 一律原样透传。
//
// 换算表不在本文件：唯一来源是 apidoc.GPTImageTierPixels（对外文档那张表由它渲染）。
// 写两份的代价是实测过的 —— 文档挂着 gpt-image-2 的另一套像素（16:9 → 1672x941），
// 而代码从来不发它，读者按文档算像素、按代码出图，对不上也没人发现。
//
// 为什么必须翻译而不是把 "2K" 原样发出去：上游 size 只接受「宽x高」/ 比例 / auto，
// 发档位词会换回一句 `size must be widthxheight ... or one of: 1:1, 3:2, 2:3, auto`。
// 为什么不能"翻译不出来就发原文"：那等于把一个必然 400 的请求推给上游，
// 用户看到的是一句上游原话，而真正的原因（这个画幅没有这一档）只有平台知道。
func duomiGPTImageSize(im billing.ImageSize) (string, error) {
	tier := strings.ToUpper(strings.TrimSpace(im.DeclaredTier))
	if tier == "" {
		// 像素 / 比例 / auto / 上游私有写法：原样透传，由上游判定。
		return strings.TrimSpace(im.Raw), nil
	}
	ratio := strings.TrimSpace(im.Ratio)
	if ratio == "" {
		// 画幅缺省 1:1（与上游不传 size 时的默认出图一致）。
		ratio = "1:1"
	}
	px, tiers, ok := apidoc.GPTImagePixelFor(tier, ratio)
	if ok {
		return px, nil
	}
	if len(tiers) > 0 {
		return "", &relay.CallerParamError{Msg: fmt.Sprintf(
			"画幅 %s 没有 %s 档：该画幅只提供 %s，请改档位或换画幅",
			ratio, im.DeclaredTier, strings.Join(tiers, " / "))}
	}
	return "", &relay.CallerParamError{Msg: fmt.Sprintf(
		"画幅 %s 不在支持列表里：可用画幅为 %s（也可以直接写像素，如 2048x1152）",
		ratio, strings.Join(apidoc.GPTImageRatios(), "、"))}
}

// duomiPutN 把客户端的 n 原值写进上游报文（缺失 / null / 0 不写）。
//
// 用 RawMessage 而不是整数：multipart 与部分客户端会把 n 写成字符串，
// 上游能不能接受由它回答；平台只保证「写进去的就是用户写的那个值」。
func duomiPutN(payload map[string]any, n json.RawMessage) {
	v := strings.TrimSpace(string(n))
	if v == "" || v == "null" || v == "0" {
		return
	}
	payload["n"] = n
}

// duomiMaskURL 返回本次出站要用的蒙版直链（平台字段名恒为 mask_url）。
//
// 入站存在两个写法：正式的 `mask_url`，以及按上游/官方 SDK 惯性写下的 `mask`。
// 两者在这里归一成同一个值，出站**只发 mask_url** —— 调用方在出站报文里看到的
// 永远是自己写的那个平台字段，不会凭空多出一个 mask（2026-11-05 实测故障）。
func duomiMaskURL(body *duomiBody) string {
	if v := strings.TrimSpace(body.MaskURL); v != "" {
		return v
	}
	var m map[string]json.RawMessage
	if len(body.raw) == 0 || json.Unmarshal(body.raw, &m) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(m["mask"], &s) != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// submitDuomiGPTImage 提交 gpt-image 系（OpenAI 兼容、异步）任务。
func submitDuomiGPTImage(ctx context.Context, cfg *relay.ChannelConfig, opts *duomiOptions,
	profile duomiProfile, upstream string, body *duomiBody, im billing.ImageSize) (string, string, error) {
	prompt, err := duomiImagePrompt(body, duomiMaxGPTImagePromptRunes)
	if err != nil {
		return "", "", err
	}

	payload := map[string]any{"model": upstream, "prompt": prompt}
	size, serr := duomiGPTImageSize(im)
	if serr != nil {
		return "", "", serr
	}
	if size != "" {
		payload["size"] = size
	}
	// quality 的语义是「思考深度」，官方 enum 是 low / medium / high；
	// 用户写 hd / standard 时不再本地判死 —— 上游认不认由它回答（原值下发）。
	if v := strings.TrimSpace(body.Quality); v != "" {
		payload["quality"] = v
	}
	if v := strings.TrimSpace(body.Background); v != "" {
		payload["background"] = v
	}
	// 蒙版：平台字段名恒为 mask_url，**出站不改名**（写成上游文档里的 mask 会让
	// 调用方在出站报文里看到一个自己没传过的字段，对不上账也复现不了）。
	if v := duomiMaskURL(body); v != "" {
		u, uerr := duomiPublicURL(v, "mask_url")
		if uerr != nil {
			return "", "", uerr
		}
		payload["mask_url"] = u
	}
	// image 既可能是字符串也可能是数组：按原形态校验后原样下发。
	if len(body.Image) > 0 && strings.TrimSpace(string(body.Image)) != "null" {
		img, ierr := duomiImageField(body.Image)
		if ierr != nil {
			return "", "", ierr
		}
		payload["image"] = img
	}
	duomiPutN(payload, body.N)
	// 未映射的顶层键原样下发（output_format / user / output_compression / 厂商私有键…）。
	relay.ForwardUnmapped(payload, body.raw, duomiGPTImageLocalOnly...)

	path := strings.TrimSpace(opts.paths["gptimage.submit"])
	if path == "" {
		path = duomiGPTImageSubmit
	}
	buf, derr := duomiDo(ctx, cfg, opts.authPrefix, http.MethodPost, duomiWithQuery(path, "async=true"), payload)
	if derr != nil {
		return "", "", derr
	}
	if berr, bad := duomiEnvelopeError(buf); bad {
		return "", "", berr
	}
	id, ierr := duomiExtractTaskID(buf)
	if ierr != nil {
		return "", "", ierr
	}
	return id, string(duomiProtoGPTImage), nil
}

// ---------------------------------------------------------------- 轮询

// parseDuomiImageTask 解析图像任务的查询响应。
func parseDuomiImageTask(buf []byte) (*relay.TaskStatus, error) {
	env := duomiParseTaskEnvelope(buf, duomiImageArtifactKeys, duomiModeImage)
	return duomiEnvelopeToTaskStatus(env)
}

// duomiImageArtifactKeys 是图像产物可能出现的键（所有出现过的形状都列上）。
var duomiImageArtifactKeys = []string{
	"url", "image_url", "images", "outputs", "output", "results", "result",
	"data", "urls", "file_url", "image_urls", "b64_json",
}

// ---------------------------------------------------------------- 公共视图

// duomiTaskEnvelope 是「任务查询响应」的宽容视图。
type duomiTaskEnvelope struct {
	status   string
	urls     []string
	texts    []string
	errMsg   string
	progress int
}

// duomiArtifactMode 决定「遍历时什么算产物」。
//
// 三种模式必须分开：图像响应的链接散落在任意层级（数组/对象/裸串），
// 而音频/歌词响应里同时含封面图（image_url）与无关链接——按图像模式收集会把封面当成
// 第二首音乐、把无关链接当产物，用户拿到的是错的东西。宁可显式画出白名单。
type duomiArtifactMode int

const (
	// duomiModeImage：任意层级的 http(s) 链接都算产物。
	duomiModeImage duomiArtifactMode = iota
	// duomiModeAudio：只有脚本白名单里的音频键算产物。
	duomiModeAudio
	// duomiModeText：只有脚本白名单里的文本键算产物。
	duomiModeText
)

// duomiParseTaskEnvelope 解析查询响应：状态、产物链接、文本产物、错误、进度。
//
// 为什么按「键白名单递归收集」而不是按固定结构解析：
// 多米把多家上游的响应原样透出，同一家的不同版本也换过形状（数组/对象/裸字符串）。
// 按固定结构解析时，形状一变就是「200 但产物为空」→ 任务判成功却没有产物 → 用户白付钱。
func duomiParseTaskEnvelope(buf []byte, keys []string, mode duomiArtifactMode) duomiTaskEnvelope {
	var out duomiTaskEnvelope
	env := duomiEnvelopeOf(buf)
	out.status = firstNonEmptyStr(env.Status, env.State)
	if env.Progress != nil {
		out.progress = *env.Progress
	}
	out.errMsg = errTextFromValue(env.err.Error)

	// data 可能是对象、数组或裸字符串：三种都按白名单遍历。
	// 只遍历整包（data 是其中一个键）而不是先遍历 data 再遍历整包：
	// 两遍会产生重复项，还得靠去重兜底，白白多一次全树解析。
	duomiWalkArtifacts(buf, keys, mode, 0, &out.urls, &out.texts)
	if d := duomiNestedView(env.Data); d != nil {
		if out.status == "" {
			out.status = firstNonEmptyStr(d.Status, d.State)
		}
		if out.progress == 0 && d.Progress != nil {
			out.progress = *d.Progress
		}
		if out.errMsg == "" {
			out.errMsg = firstNonEmptyStr(errTextFromValue(d.Error), d.Message, d.FailReason)
		}
		// nano 的失败原因在 `data.msg`（成功时为空或 "success"）：只在状态确实是失败时读，
		// 否则成功响应里的 msg 会变成一条假的错误说明。
		if out.errMsg == "" && duomiStatusOf(out.status) == taskstate.Failed {
			out.errMsg = strings.TrimSpace(d.Msg)
		}
	}
	// 根级业务失败：`{"code":400,"msg":"...","data":null}` 既没有状态也没有产物。
	// 不在这里取原因的话，上层会把它当「还在跑」，一直轮询到任务超时才退款 ——
	// 上游早已拒绝，用户白等，而渠道健康度全程正常。
	//
	// 有产物时不改判：产物优先的规则在 duomiEnvelopeToTaskStatus 里（那里能同时看到两者）。
	if out.errMsg == "" {
		if _, msg, failed := env.failure(); failed {
			out.errMsg = msg
			if out.errMsg == "" {
				out.errMsg = "上游任务失败（未返回原因）"
			}
		}
	}
	out.urls = duomiDedup(out.urls)
	out.texts = duomiDedup(out.texts)
	return out
}

// duomiNestedViewOf 是 data 对象的常用字段视图。
type duomiNestedViewOf struct {
	Status     string `json:"status"`
	State      string `json:"state"`
	TaskStatus string `json:"task_status"`
	Progress   *int   `json:"progress"`
	Message    string `json:"message"`
	// Msg 是 nano 的失败原因载体（官方失败样本：`data.msg` 里是上游原话，
	// 而 `data.data` 为 null）。只读 `message` 会让任务失败永远显示「未返回原因」。
	Msg        string          `json:"msg"`
	FailReason string          `json:"fail_reason"`
	Error      json.RawMessage `json:"error"`
}

// duomiNestedView 抽出 data 对象的常用字段（非对象返回 nil）。
func duomiNestedView(raw json.RawMessage) *duomiNestedViewOf {
	if len(raw) == 0 {
		return nil
	}
	var v duomiNestedViewOf
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	if v.TaskStatus != "" && v.Status == "" {
		v.Status = v.TaskStatus
	}
	return &v
}

// duomiWalkArtifacts 按键白名单递归收集链接与文本（深度上限 6，防异常嵌套）。
//
// 「什么算产物」完全由 keys 白名单决定，不靠层级位置判断：
// 沿着白名单里的键往下走，走到字符串就按 mode 收下。这样
// `data:[{audio_url}]`、`data:{ret_a:{text}}`、`data:"http://..."` 三种形状都能命中，
// 而白名单之外的键（如 SUNO 的封面 image_url）永远不会被当成产物。
//
// 只收集 http(s) 链接：上游偶尔把 base64 塞进 b64_json 或写相对路径，
// 这些都无法转存（平台只按 URL 拉取），静默收集会产生「产物存在但下载必失败」的坏数据。
func duomiWalkArtifacts(raw json.RawMessage, keys []string, mode duomiArtifactMode,
	depth int, urls *[]string, texts *[]string) {
	if depth > 6 || len(raw) == 0 {
		return
	}
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return
	}
	switch s[0] {
	case '"':
		var str string
		if json.Unmarshal(raw, &str) != nil {
			return
		}
		str = strings.TrimSpace(str)
		if str == "" {
			return
		}
		switch mode {
		case duomiModeImage, duomiModeAudio:
			if duomiHTTPURL(str) {
				*urls = append(*urls, str)
			}
		case duomiModeText:
			*texts = append(*texts, str)
		}
	case '[':
		var arr []json.RawMessage
		if json.Unmarshal(raw, &arr) != nil {
			return
		}
		for _, it := range arr {
			duomiWalkArtifacts(it, keys, mode, depth+1, urls, texts)
		}
	case '{':
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil {
			return
		}
		for _, k := range keys {
			v, ok := m[k]
			if !ok {
				continue
			}
			duomiWalkArtifacts(v, keys, mode, depth+1, urls, texts)
		}
	}
}

// duomiStatusOf 只在状态非空时归一化。
//
// 为什么不直接调 duomiNormalizeStatus：它对未知/空值一律返回 "running"，
// 于是「上游没报状态但已经给了产物」会被判成 running —— 任务一直等到 deadline，
// 已经生成好的产物按超时退费。空状态必须保留空串，交给调用方按产物/错误推断。
func duomiStatusOf(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	return duomiNormalizeStatus(raw)
}

// duomiEnvelopeToTaskStatus 把宽容视图折成平台 TaskStatus（状态归一 + 产物判定）。
func duomiEnvelopeToTaskStatus(env duomiTaskEnvelope) (*relay.TaskStatus, error) {
	out := &relay.TaskStatus{
		Status:   duomiStatusOf(env.status),
		Progress: env.progress,
		Error:    env.errMsg,
	}
	out.ResultURLs = env.urls
	if len(env.urls) > 0 {
		out.ResultURL = env.urls[0]
	}
	if len(env.texts) > 0 {
		out.ResultText = env.texts[0]
	}
	// 有产物但上游没报状态：判成功。这是「上游只回 data:[url]」的常见收尾形态。
	// 不这么判，任务会一直 running 到 deadline（产物已经在手却按超时退费）。
	if out.Status == "" {
		if len(env.urls) > 0 || out.ResultText != "" {
			out.Status = taskstate.Succeeded
		} else if env.errMsg != "" {
			out.Status = taskstate.Failed
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- 小工具

// duomiImagePrompt 取并校验图像 prompt（上限按协议区分）。
func duomiImagePrompt(body *duomiBody, limit int) (string, error) {
	p := strings.TrimSpace(body.Prompt)
	if p == "" {
		return "", &relay.CallerParamError{Msg: "图像任务必须提供 prompt"}
	}
	if n := len([]rune(p)); n > limit {
		return "", &relay.CallerParamError{Msg: fmt.Sprintf(
			"prompt 过长（%d 字符，上限 %d）", n, limit)}
	}
	return p, nil
}

// duomiRefImageURLs 校验参考图列表（公网直链、数量上限）。
//
// 空数组返回 (nil, nil)：「至少一张」是**端点**的结构前提（/nano-banana-edit 与
// /v1/images/edits 必须有图），由调用方按端点判定 —— 放在这里会让「文生图不带图」
// 也报一句「编辑模型必须提供参考图」。
func duomiRefImageURLs(raw []string, limit int, field string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	// 上限是上游的**结构前提**（nano 编辑端点：数组最多 10 张），本地拦住省一次出站：
	// 放过去只会让上游回一句与人数无关的 400，还白占一次渠道健康统计。
	if limit > 0 && len(raw) > limit {
		return nil, &relay.CallerParamError{Msg: fmt.Sprintf(
			"%s 最多 %d 张（当前 %d 张）", field, limit, len(raw))}
	}
	out := make([]string, 0, len(raw))
	for i, it := range raw {
		u, err := duomiPublicURL(it, fmt.Sprintf("%s[%d]", field, i))
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

// duomiPublicURL 校验并返回公网直链（拒绝 data: / 相对路径 / 非 http(s)）。
func duomiPublicURL(raw, field string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", &relay.CallerParamError{Msg: field + " 不能为空"}
	}
	if strings.HasPrefix(strings.ToLower(s), "data:") {
		return "", &relay.CallerParamError{Msg: field + " 不支持 data: 内联，请先用 /v1/files 上传后改用公网直链"}
	}
	if !duomiHTTPURL(s) {
		return "", &relay.CallerParamError{Msg: field + " 必须是 http(s) 公网直链"}
	}
	return s, nil
}

// duomiImageField 校验 gpt-image 的 image 字段（字符串或字符串数组），原形态返回。
func duomiImageField(raw json.RawMessage) (any, error) {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		u, err := duomiPublicURL(one, "image")
		if err != nil {
			return nil, err
		}
		return u, nil
	}
	var arr []string
	if json.Unmarshal(raw, &arr) == nil {
		urls, err := duomiRefImageURLs(arr, duomiMaxGPTImageRefs, "image")
		if err != nil {
			return nil, err
		}
		return urls, nil
	}
	return nil, &relay.CallerParamError{Msg: "image 必须是字符串或字符串数组（公网直链）"}
}

// duomiHTTPURL 判断是否 http(s) 绝对链接。
func duomiHTTPURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// duomiDedup 去重并保持首次出现顺序。
func duomiDedup(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
