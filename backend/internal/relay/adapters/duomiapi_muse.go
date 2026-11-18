package adapters

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"lingdong-api/internal/relay"
)

// muse 是多米**自有**视频协议（不是转发某家官方 API）：
//
//	提交 POST /v1/videos/generations   体 {model, prompt, aspect_ratio, duration?, image_urls[]}
//	查询 GET  /v1/videos/tasks/{id}    → {id, state, data:{videos:[{url}]}, progress, message}
//
// 三条硬约束（改动前先读）：
//  1. **查询路径独立**：/v1/videos/tasks/{id} 不是 /v1/videos/generations/{id}。走错 → 每轮 404
//     → 连续失败判超时退费，而渠道健康度看起来完全正常。
//  2. **上游没有 resolution**：平台的 normalizeSubmitBody 会注入 resolution / size，
//     原样透传等于替上游发一个它不认的参数（400 且报错与真实原因无关）。
//  3. **参考图入口唯一**：官方字段 image_urls 由平台入口（task.ParseVideoSubmit）折叠成
//     input_reference 后再进来 —— 这里只读 input_reference，出站再还原成官方数组。
//     直接在适配器里读顶层 image_urls 会跳过 SSRF 校验与参考图计费（两者都只认落库的
//     input_reference），所以那个键仍在 localOnly 里，永不透传。
const duomiMuseMaxRefImages = 8 // 只用于错误文案，**不做**本地上限（能力交还上游）

// duomiMuseLocalOnly 是 muse 出站时必须丢弃的键（与 seedance 同口径 + image_urls）：
//   - 已翻译：model / prompt / aspect_ratio / ratio / duration / seconds / image_urls；
//   - 平台内部：resolution / size / image_size（normalizeSubmitBody 的产物，上游不认）；
//   - 素材入口：input_reference / image / image_tail / image_list（直接写会绕过本地校验与计费）；
//   - metadata / content：平台容器，上游没有对应语义。
//
// **image_urls 必须显式列出**：即使 ① 已经拒绝，也不能依赖"调用顺序"——
// ForwardUnmapped 是最后一步，任何一条早退分支被后人改掉都会让顶层数组悄悄透传。
var duomiMuseLocalOnly = []string{
	"model", "prompt", "seconds", "duration",
	"ratio", "aspect_ratio", "resolution", "size", "image_size",
	"input_reference", "image", "image_tail", "image_list", "image_urls",
	"metadata", "content",
}

// submitDuomiMuse 组装并提交 muse 视频任务。
//
// 取用顺序固定（每一步都可能在出网前返回 CallerParamError，顺序决定报错先后）：
//  1. prompt（上游 required）；
//  2. input_reference → image_urls（上游 required，1–8 张；视频/音频素材直接拒绝，不静默丢）；
//  3. aspect_ratio（上游 required）；
//  4. duration（可选，**原话下发**，不 clamp 到 10/20/30）。
func submitDuomiMuse(ctx context.Context, cfg *relay.ChannelConfig, opts *duomiOptions,
	profile duomiProfile, upstream string, body *duomiBody, req *relay.TaskSubmitRequest) (string, string, error) {
	// ① prompt：上游 required。
	prompt := strings.TrimSpace(body.Prompt)
	if prompt == "" {
		return "", "", &relay.CallerParamError{Msg: "muse 视频需要 prompt（画面描述）"}
	}
	if len([]rune(prompt)) > duomiMaxPromptRunes {
		return "", "", &relay.CallerParamError{Msg: fmt.Sprintf("prompt 过长（上限 %d 字符）", duomiMaxPromptRunes)}
	}

	// ③ 参考图：只认 input_reference，按「首帧 → 普通图 → 尾帧」的稳定顺序展开。
	// data: 由 classifyDuomiRefs 统一拒绝（内联体上游不收，且重放会静默丢素材）。
	refs, err := classifyDuomiRefs(body.InputRef)
	if err != nil {
		return "", "", err
	}
	// 视频/音频素材在 muse 里没有对应字段：静默丢弃会让用户以为用上了（而账单照收）。
	if len(refs.videos) > 0 || len(refs.audios) > 0 {
		return "", "", &relay.CallerParamError{
			Msg: "muse 只接受图片参考：audio / video 素材不支持，请改用 image 传图",
		}
	}
	images := refs.allImages()
	if len(images) == 0 {
		// 文案里的字段名必须是**文档上的那个**（image），不能是本层内部的
		// input_reference：读者手边只有文档，报错指到数组写法等于让他去用
		// 一个页面上不存在的字段。
		return "", "", &relay.CallerParamError{Msg: fmt.Sprintf(
			"muse 需要 1–%d 张参考图：请用 image 传（公网直链，一张写字符串、多张写数组）",
			duomiMuseMaxRefImages)}
	}

	// ④ aspect_ratio：上游 required。不做枚举白名单（16:9 / 9:16 之外的写法原话下发，
	// 由上游判定）—— 在代码里硬编码枚举等于替上游判能力，上游放开档位就得再发一次版。
	// aspect_ratio 优先于 ratio：上游字段名就是 aspect_ratio，两者冲突时按原生写法走。
	aspect := strings.TrimSpace(firstNonEmptyStr(body.AspectRatio, body.Ratio))
	if aspect == "" {
		return "", "", &relay.CallerParamError{Msg: "muse 必须指定 aspect_ratio（如 16:9 / 9:16）"}
	}

	// ⑤ duration：10/20/30 之外也照原话下发（不 clamp、不替换成最近档）。
	// 上限由 spec_prices.max_seconds 在 Estimate 阶段拦（400 invalid_param，带实际值与上限）。
	// AutoDuration / OmitDuration 时 duomiSeconds 返回 0 → 不下发该键（上游用默认时长）。
	secs := duomiSeconds(body, req)

	payload := map[string]any{
		"model":        upstream,
		"prompt":       prompt,
		"aspect_ratio": aspect,
		// 裸字符串数组：上游要的就是这个形状，不是 {url: ...} 包裹。
		"image_urls": images,
	}
	if secs > 0 {
		payload["duration"] = secs // 整数秒
	}
	// 平台没映射的顶层键原样下发；已翻译/素材入口/平台内部字段见 localOnly。
	relay.ForwardUnmapped(payload, body.raw, duomiMuseLocalOnly...)

	path := strings.TrimSpace(opts.paths["muse.submit"])
	if path == "" {
		path = duomiMuseSubmitPath
	}
	buf, derr := duomiDo(ctx, cfg, opts.authPrefix, http.MethodPost, path, payload)
	if derr != nil {
		return "", "", derr
	}
	// HTTP 200 + 业务码失败：duomiBusinessError 会把 400 类归成 CallerParamError，
	// 401/403/429 包成 HTTPStatusError（钥匙级冷却），402 保持裸 error（渠道级熔断）。
	if berr, bad := duomiEnvelopeError(buf); bad {
		return "", "", berr
	}
	// 响应形状 {"id":"8cb2..."}：duomiExtractTaskID 已覆盖顶层 id。
	id, ierr := duomiExtractTaskID(buf)
	if ierr != nil {
		return "", "", ierr
	}
	return id, duomiSubMuse, nil
}
