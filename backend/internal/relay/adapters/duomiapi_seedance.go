package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"lingdong-api/internal/pkg/taskstate"
	"lingdong-api/internal/relay"
)

// Seedance 后端的实现细节（火山方舟官方格式，经多米域名转发）。
//
// 官方文档：https://www.volcengine.com/docs/82379/1520757 （创建）
//           https://www.volcengine.com/docs/82379/1521309 （查询）
// 接入方式（多米说明）：把官方域名换成 https://duomiapi.com、Authorization 换成多米令牌即可。

// duomiSeedanceContent 是 content 数组的一项。
type duomiSeedanceContent struct {
	Type     string              `json:"type"`
	Text     string              `json:"text,omitempty"`
	ImageURL *duomiSeedanceMedia `json:"image_url,omitempty"`
	VideoURL *duomiSeedanceMedia `json:"video_url,omitempty"`
	AudioURL *duomiSeedanceMedia `json:"audio_url,omitempty"`
	Role     string              `json:"role,omitempty"`
}

// duomiSeedanceMedia 是媒体链接的包裹形态（上游要求 {url: "..."} 而不是裸字符串）。
type duomiSeedanceMedia struct {
	URL string `json:"url"`
}

// duomiSeedanceLocalOnly 是 seedance 出站时必须丢弃的键：平台已消费 / 已翻译 / 素材入口。
//
//   - 已翻译：model、content（由 input_reference 组装）、ratio/aspect_ratio、
//     resolution/size、seconds/duration；
//   - 素材入口：input_reference / image / image_tail / image_list（直接写上游字段会绕过
//     content 的顺序约定，而顺序错乱不会报错、只会生成错的内容）；
//   - metadata：走 metadata_allow 逐键放行，原容器不能整个下发。
var duomiSeedanceLocalOnly = []string{
	"model", "content", "prompt", "seconds", "duration",
	"ratio", "aspect_ratio", "resolution", "size",
	"input_reference", "image", "image_tail", "image_list",
	"metadata",
}

// submitDuomiSeedance 组装并提交 Seedance 任务。
//
// content 的顺序是**约定的**：text → 首/尾帧 → reference_image* → reference_video* → reference_audio*。
// 上游按 position 引用素材（"图片1"、"视频1"），顺序错乱会让 prompt 里的指代全部错位，
// 而这种错不会报错、只会生成错的内容。
func submitDuomiSeedance(ctx context.Context, cfg *relay.ChannelConfig, opts *duomiOptions, profile duomiProfile,
	upstream string, body *duomiBody, req *relay.TaskSubmitRequest) (string, string, error) {
	prompt := strings.TrimSpace(body.Prompt)
	if prompt == "" {
		// 纯素材（首尾帧 / 参考图）也能生成，但完全空请求必须拒：上游会返回 400，
		// 拿到这里判能给出人话。
		if len(body.InputRef) == 0 {
			return "", "", &relay.CallerParamError{Msg: "seedance 需要 prompt 或至少一个素材"}
		}
	}
	if len([]rune(prompt)) > duomiMaxPromptRunes {
		return "", "", &relay.CallerParamError{Msg: fmt.Sprintf("prompt 过长（上限 %d 字符）", duomiMaxPromptRunes)}
	}
	refs, err := classifyDuomiRefs(body.InputRef)
	if err != nil {
		return "", "", err
	}
	// 秒数缺失不再拒绝：不下发 duration，由上游用自己的默认值（通常 5 秒）。
	// 平台侧仍按预算冻结、结算以上游实际时长为准（见 task.validateVideoModality）。
	secs := duomiSeconds(body, req)

	content := make([]duomiSeedanceContent, 0, 1+len(refs.allImages())+len(refs.videos)+len(refs.audios))
	content = append(content, duomiSeedanceContent{Type: "text", Text: prompt})
	if refs.firstFrame != "" {
		content = append(content, duomiSeedanceContent{
			Type: "image_url", ImageURL: &duomiSeedanceMedia{URL: refs.firstFrame}, Role: "first_frame",
		})
	}
	if refs.lastFrame != "" {
		content = append(content, duomiSeedanceContent{
			Type: "image_url", ImageURL: &duomiSeedanceMedia{URL: refs.lastFrame}, Role: "last_frame",
		})
	}
	for _, u := range refs.images {
		content = append(content, duomiSeedanceContent{
			Type: "image_url", ImageURL: &duomiSeedanceMedia{URL: u}, Role: "reference_image",
		})
	}
	for _, u := range refs.videos {
		content = append(content, duomiSeedanceContent{
			Type: "video_url", VideoURL: &duomiSeedanceMedia{URL: u}, Role: "reference_video",
		})
	}
	for _, u := range refs.audios {
		content = append(content, duomiSeedanceContent{
			Type: "audio_url", AudioURL: &duomiSeedanceMedia{URL: u}, Role: "reference_audio",
		})
	}

	payload := map[string]any{
		"model":   upstream,
		"content": content,
	}
	if secs > 0 {
		payload["duration"] = secs
	}
	// ratio：平台两种写法（ratio / aspect_ratio）都接受，ratio 优先（P2-10）。
	// 未指定则交给上游默认（不猜，猜错画面比例就废了）。
	if r := strings.TrimSpace(firstNonEmptyStr(body.Ratio, body.AspectRatio)); r != "" {
		payload["ratio"] = r
	}
	// resolution：档位字面量原样下发（480p/720p/1080p/2k）。
	// 注意这里**不**做"未传就不发"之外的推断：档位由 task 层按价目决定并已在 body 里归一。
	if t := duomiResolutionValue(firstNonEmptyStr(body.Resolution, body.Size)); t != "" {
		payload["resolution"] = t
	}
	if seed, ok := duomiSeedValue(body); ok {
		payload["seed"] = seed
	}
	// generate_audio / watermark 会改变上游产出与成本（P1-25）：
	// 顶层显式字段优先；metadata 里的同名键只有被 metadata_allow 放行才生效。
	md, err := duomiMetadataPassthrough(opts, body.Metadata)
	if err != nil {
		return "", "", err
	}
	for k, v := range md {
		payload[k] = v
	}
	if body.GenerateAudio != nil {
		payload["generate_audio"] = *body.GenerateAudio
	}
	if body.Watermark != nil {
		payload["watermark"] = *body.Watermark
	}

	// 平台没映射的顶层键原样下发。素材与尺寸/时长见下面的 localOnly 说明。
	relay.ForwardUnmapped(payload, req.Body, duomiSeedanceLocalOnly...)

	path := opts.paths["seedance.submit"]
	if path == "" {
		path = duomiSeedanceTaskPath
	}
	buf, err := duomiDo(ctx, cfg, opts.authPrefix, "POST", path, payload)
	if err != nil {
		return "", "", err
	}
	if e, isErr := duomiEnvelopeError(buf); isErr {
		return "", "", e
	}
	id, err := duomiExtractTaskID(buf)
	if err != nil {
		return "", "", err
	}
	return id, duomiSubSeedance, nil
}

// parseDuomiVideoTask 解析「视频任务」查询响应（seedance / muse 同形）。
//
// 文档里给了两套形状，必须都认：
//  1. schema：{id, state, data:{videos:[{url}]}, progress, message}
//  2. example：{id, status, content:{video_url}, ...}
//     muse 的查询响应与第 1 种完全同形，所以两个子协议共用这一份解析器。
//     各写一份必然漂移，而漂移的症状是「上游成功但平台判 running」→ 超时退费。
//
// 只认一种会在上游换信封后静默失败：状态解析成 running 一直等到 deadline 退费。
func parseDuomiVideoTask(buf []byte) (*relay.TaskStatus, error) {
	var p struct {
		ID       string          `json:"id"`
		State    string          `json:"state"`
		Status   string          `json:"status"`
		Progress int             `json:"progress"`
		Message  string          `json:"message"`
		Error    json.RawMessage `json:"error"`
		Data     struct {
			Videos []struct {
				URL string `json:"url"`
			} `json:"videos"`
		} `json:"data"`
		Content struct {
			VideoURL string `json:"video_url"`
			Videos   []struct {
				URL string `json:"url"`
			} `json:"videos"`
		} `json:"content"`
	}
	if err := json.Unmarshal(buf, &p); err != nil {
		return nil, fmt.Errorf("适配器 duomiapi：解析视频任务查询响应失败：%w", err)
	}

	out := &relay.TaskStatus{Status: duomiNormalizeStatus(firstNonEmptyStr(p.State, p.Status)), Progress: p.Progress}

	// 业务错误信封（HTTP 200 + error/成功码之外的 code）：直接判失败，别让它烂在 running。
	//
	// 这里取**信封原文**而不是 duomiBusinessError 的组装文案：任务失败要告诉用户
	// 「上游为什么没出片」，而不是「多米API 拒绝了本次参数」——后者是提交期的话术，
	// 在已经建单并轮询到失败之后毫无意义。
	if code, msg, failed := duomiEnvelopeOf(buf).failure(); failed {
		out.Status = taskstate.Failed
		out.Error = firstNonEmptyStr(msg, fmt.Sprintf("上游返回业务错误（code=%d）", code))
		return out, nil
	}

	if out.Status != taskstate.Succeeded {
		if out.Status == taskstate.Failed {
			// 原因取信封（message > msg > error.*），与提交链路口径一致。
			out.Error = firstNonEmptyStr(duomiEnvelopeOf(buf).text(), "上游任务失败")
		}
		return out, nil
	}

	urls := make([]string, 0, len(p.Data.Videos)+len(p.Content.Videos)+1)
	for _, v := range p.Data.Videos {
		urls = append(urls, v.URL)
	}
	for _, v := range p.Content.Videos {
		urls = append(urls, v.URL)
	}
	urls = append(urls, p.Content.VideoURL)
	out.ResultURLs = duomiStrings(urls...)
	if len(out.ResultURLs) == 0 {
		// 宁可失败退费，也不能「结算成功但用户拿不到文件」。
		out.Status = taskstate.Failed
		out.Error = "上游返回成功但缺少视频产物"
		return out, nil
	}
	out.ResultURL = out.ResultURLs[0]
	return out, nil
}

// parseDuomiSeedanceTask 是 parseDuomiVideoTask 的旧名（保留作别名）。
//
// 不直接删：`duomi_envelope_test.go` 与部分调用点按"seedance 的解析器"理解这个名字，
// 改名只为表达"视频信封共用一份实现"，多一层别名比改一圈调用点的 diff 更安全。
func parseDuomiSeedanceTask(buf []byte) (*relay.TaskStatus, error) {
	return parseDuomiVideoTask(buf)
}

// duomiSeedValue 取随机种子（顶层 seed 优先，其次 metadata.seed）。
//
// 种子必须下发：适配器与 task 层都记录了它，重试时不下发就会换一个随机种子，
// 同一提示词重试出完全不同的结果，用户无法复现。
func duomiSeedValue(body *duomiBody) (int64, bool) {
	if n, ok := duomiIntFromRaw(body.Seed); ok {
		return n, true
	}
	if body.Metadata != nil {
		if v, ok := body.Metadata["seed"]; ok {
			switch t := v.(type) {
			case float64:
				return int64(t), true
			case json.Number:
				if n, err := t.Int64(); err == nil {
					return n, true
				}
			case string:
				var n int64
				if _, err := fmt.Sscanf(strings.TrimSpace(t), "%d", &n); err == nil {
					return n, true
				}
			}
		}
	}
	return 0, false
}

// duomiIntFromRaw 把 RawMessage 解析成整数（缺失/非法返回 false）。
func duomiIntFromRaw(raw json.RawMessage) (int64, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, false
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n, true
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		var v int64
		if _, err := fmt.Sscanf(strings.TrimSpace(str), "%d", &v); err == nil {
			return v, true
		}
	}
	return 0, false
}

// duomiMetadataPassthrough 按允许清单过滤 metadata。
//
// seed 是平台的已知字段（已单独下发），不参与白名单判定；其余键不在清单内一律 CallerParamError——
// generate_audio / watermark 会改变上游产出与成本，静默丢弃会让"我要配音"变成"没配音还收了钱"。
func duomiMetadataPassthrough(opts *duomiOptions, md map[string]any) (map[string]any, error) {
	if len(md) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(md))
	for k, v := range md {
		if k == "seed" {
			continue
		}
		if !opts.metadataAllow[k] {
			return nil, &relay.CallerParamError{Msg: fmt.Sprintf(
				"metadata.%s 不允许透传（允许的键：%s）",
				k, strings.Join(opts.metadataAllowKeys(), " / "))}
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// metadataAllowKeys 返回排序后的允许键（错误文案要稳定，便于运营按提示改配置）。
func (o *duomiOptions) metadataAllowKeys() []string {
	keys := make([]string, 0, len(o.metadataAllow))
	for k := range o.metadataAllow {
		keys = append(keys, k)
	}
	// 数量极少（上限 8），插入排序足够且不引入 sort 依赖的顺序争议。
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	if len(keys) == 0 {
		return []string{"（空）"}
	}
	return keys
}
