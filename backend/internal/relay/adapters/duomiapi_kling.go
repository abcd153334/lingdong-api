package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/metrics"
	"lingdong-api/internal/pkg/taskstate"
	"lingdong-api/internal/relay"
)

// Kling 后端实现（可灵官方格式，经多米域名转发）。
//
// 四条提交路径共用一套响应信封，但**查询路径必须与提交路径同段**：
//
//	POST /api/video/kling/v1/videos/text2video          → GET .../text2video/{id}
//	POST /api/video/kling/v1/videos/image2video         → GET .../image2video/{id}
//	POST /api/video/kling/v1/videos/multi-image2video   → GET .../multi-image2video/{id}
//	POST /api/video/kling/v1/videos/omni-video          → GET .../omni-video/{id}
//
// 这就是 SubKind 必须落库的原因：提交与轮询是两次独立调用，不带上段名就查不到任务，
// 表现为「上游生成好了、平台一直 running 到超时退费」。

// duomiKlingMaxPromptRunes 是可灵的提示词上限（3072 字符，官方文档）。
//
// 与 seedance 的 20000 不同，故不能共用一个常量：用大值放行会被上游 400，
// 而那条 400 会被归因成「上游提交失败」，用户看到的是 502 而不是「你的提示词太长」。
const duomiKlingMaxPromptRunes = 3072

// duomiKlingLocalOnly 是可灵出站时必须丢弃的键：平台已消费 / 已翻译 / 已由档案决定。
//
// 分三类，每一类都对应一个真实故障面：
//   - 已翻译：model（→ model_name）、seconds/duration、aspect_ratio/ratio；
//   - 素材入口：platform 只认 input_reference，客户端直接写 image / image_tail /
//     image_list 会绕过「按素材形态选分段」的判定（请求带着图却走文生视频路径）；
//   - **计费维度**：mode 决定 std/pro 价差，sound 决定是否同声音画，multi_shot 决定分镜。
//     这三者若能被请求体改写，就是「按 std 付钱、上游出 pro 画面」；
//   - **计费维度（尺寸）**：resolution / size 只用于平台选价目档（可灵的清晰度由
//     model_name 本身决定，官方报文里没有这两个键）。透传上去是「按 4K 收钱、
//     上游按模型默认档出片」，账单与交付对不上。
var duomiKlingLocalOnly = []string{
	"model", "prompt", "seconds", "duration",
	"aspect_ratio", "ratio", "resolution", "size",
	"input_reference", "image", "image_tail", "image_list",
	"metadata", "camera_control", "mode", "sound", "multi_shot",
}

// submitDuomiKling 组装并提交可灵任务。
func submitDuomiKling(ctx context.Context, cfg *relay.ChannelConfig, opts *duomiOptions, profile duomiProfile,
	upstream string, body *duomiBody, req *relay.TaskSubmitRequest) (string, string, error) {
	refs, err := classifyDuomiRefs(body.InputRef)
	if err != nil {
		return "", "", err
	}
	prompt := strings.TrimSpace(body.Prompt)
	if len([]rune(prompt)) > duomiKlingMaxPromptRunes {
		return "", "", &relay.CallerParamError{Msg: fmt.Sprintf("提示词过长（可灵上限 %d 字符，当前 %d）",
			duomiKlingMaxPromptRunes, len([]rune(prompt)))}
	}
	// 可灵没有 metadata 透传概念（P1-25）：收到 metadata 直接 400。
	// 静默忽略会让用户以为 generate_audio / watermark 生效了 —— 那是改上游成本的字段，
	// 「以为生效了其实没生效」比「明确拒绝」危险得多。
	if md := duomiNonSeedMetadata(body.Metadata); len(md) > 0 {
		return "", "", &relay.CallerParamError{Msg: fmt.Sprintf(
			"可灵协议不支持 metadata 透传（收到 %s）；配音/水印等参数请用 seedance 协议的模型",
			strings.Join(duomiSortedKeys(md), " / "))}
	}

	// 档位与时长一律原样下发：这个型号能出哪几档、只认哪几个秒数，
	// 是厂商官方文档的事实，上游不认会明确报错。平台在提交期替它判定，
	// 只会在上游扩容时把自己人挡在门外（且型号换上游后旧上限更荒谬）。
	secs := duomiSeconds(body, req)

	seg := duomiKlingSegment(profile, refs)
	payload := map[string]any{
		"model_name": upstream,
	}
	// duration：部分型号（kling-v1.6-multi / kling-v3-omni）只认字符串写法（P0-11）。
	//
	// 用户没写秒数时**不下发**该字段（由上游报自己的必填错误），
	// 而不是补一个 0：补 0 会让上游把它当成一个非法值，报错信息也更难懂。
	if secs > 0 {
		if profile.DurationAsString {
			payload["duration"] = fmt.Sprintf("%d", secs)
		} else {
			payload["duration"] = secs
		}
	}
	// mode 只来自档案/渠道配置：请求体里的同名字段一律不读，
	// 否则用户可以「按 std 价买 pro 画质」，计费与实付直接倒挂。
	//
	// **空值必须做到不下发**：可灵把空串当非法枚举直接 400，
	// 而那条错误会被归因成渠道故障，把一条健康渠道熔断。
	if m := strings.TrimSpace(profile.Mode); m != "" {
		payload["mode"] = m
	}
	// sound：同声音画开关。v3 及更新型号支持，v1/v2 不支持（v1.6-multi 实测不下发）。
	//
	// **不给调用方开关**：它会改变上游产出与成本，开放等于让人自助改计费维度
	// （与 metadata 白名单同一条原则）。运营若要关，改用 v2 型号。
	//
	// 判定依据是 upstream（上游真实模型名）而不是本地 modelID：运营可能建一个叫
	// `kling-omni-hd` 的本地 ID 但上游是 `kling-v1-6`，按本地 ID 判会误发。
	//
	// 旧实现只认「含 v3」，于是 kling-v4-pro 静默不下发 → 成片无声、账单照收、无日志。
	if on, versionUnknown := duomiKlingSound(upstream); on {
		payload["sound"] = "on"
		if versionUnknown {
			// 不直接转 stdout：这条事实会影响「成片有没有声音」，必须能被运维检索。
			logger.L().Warn("可灵型号无版本号，已按支持同声音画处理（sound=on）",
				zap.String("upstream", upstream))
			metrics.Inc(metrics.NameKlingSound, "source", "unknown_version")
		}
	}
	if prompt != "" {
		payload["prompt"] = prompt
	} else if seg == duomiKlingOmniVideo {
		// 源代码 input.prompt.trim() || ' '：omni 恒下发 prompt，空则单空格。
		// 不下发 prompt 时上游会把「纯参考图」当成缺参处理，行为不可预期。
		payload["prompt"] = " "
	}
	// 画幅：**只有 image2video 没有** aspect_ratio（官方文档三段：
	// text2video 与 multi-image2video 都有，image2video 只有 image/image_tail/prompt/mode/duration，
	// 画幅由参考图决定）。
	//
	// 非该段**不能 400**：客户端（以及未映射字段透传）会带上 aspect_ratio，
	// 一次 400 之后每次重试都会撞同一堵墙，用户看到的是"偶发失败"。
	// 这里静默忽略并**不落任何 anomaly**：证据由出站报文承担
	// （call_io.req_raw 带 aspect_ratio、upstream_raw 不带，详情页两栏一对比即可）。
	// 借用 Notes 会落 weak_param_pricing_conflict，那会在账单上显示
	// 「提示词携带计价参数」——与事实不符，排障方向直接走偏。
	if ar := strings.TrimSpace(firstNonEmptyStr(body.Ratio, body.AspectRatio)); ar != "" {
		if seg != duomiKlingImage2Video {
			payload["aspect_ratio"] = ar
		}
	}
	// 运镜：校验后原样下发（六键补零）。只对支持运镜的分段下发 ——
	// 多图参考与 omni 的运镜语义不同（omni 有自己的镜头指令），
	// 给它们发 v1 的六轴对象会被当成未知字段忽略，用户以为生效。
	cc, err := duomiKlingCamera(body.CameraControl)
	if err != nil {
		return "", "", err
	}
	if cc != nil {
		// 运镜只在**文生视频**的 schema 里（官方文档的 camera_control 六轴对象就在该段）。
		// 图生视频由首帧决定镜头，上游没有这个字段；静默下发会被当未知键丢掉，
		// 用户以为运镜生效了 —— 所以这里必须明确拒绝。
		if seg != duomiKlingText2Video {
			return "", "", &relay.CallerParamError{Msg: "camera_control 只在文生视频时可用：" +
				"图生视频与多图参考请用 prompt 描述镜头"}
		}
		payload["camera_control"] = cc
	}

	switch seg {
	case duomiKlingText2Video:
		if prompt == "" {
			return "", "", &relay.CallerParamError{Msg: "文生视频需要 prompt"}
		}
	case duomiKlingImage2Video:
		images := refs.allImages()
		if len(images) == 0 {
			return "", "", &relay.CallerParamError{Msg: "图生视频需要一张参考图"}
		}
		// 图生视频必须带 prompt（P2-12）：上游对「只有图没有描述」的请求行为不稳定，
		// 有时补默认描述、有时 400，两种结果都不可预期。
		if prompt == "" {
			return "", "", &relay.CallerParamError{Msg: "图生视频需要 prompt（描述期望的运动与镜头）"}
		}
		// 只给尾帧（没有首帧）是不合法的（P2-12）：这类请求会被拼成
		// image == image_tail（同一张图），上游要么拒绝要么产出一段静止画面。
		if refs.firstFrame == "" && refs.lastFrame != "" {
			return "", "", &relay.CallerParamError{Msg: "图生视频不支持只传尾帧；请同时提供首帧，或改用 reference_image"}
		}
		payload["image"] = images[0]
		if refs.lastFrame != "" {
			payload["image_tail"] = refs.lastFrame
			if refs.lastFrame == refs.firstFrame {
				return "", "", &relay.CallerParamError{Msg: "首帧与尾帧不能是同一张图"}
			}
		}
		// 运镜与尾帧的互斥不需要在这里判：camera_control 只允许文生视频
		// （见上面的守卫），而文生视频没有尾帧 —— 这条分支到达时 CameraControl 必为 nil。
	case duomiKlingMulti2Video:
		images := refs.allImages()
		if len(images) == 0 {
			return "", "", &relay.CallerParamError{Msg: "多图参考生视频需要至少一张参考图"}
		}
		list := make([]map[string]string, 0, len(images))
		for _, u := range images {
			list = append(list, map[string]string{"image": u})
		}
		payload["image_list"] = list
	case duomiKlingOmniVideo:
		// omni **只有 image_list 一个素材入口**，没有 image / image_tail。
		//
		// 改造前这一支复用了 multi-image2video 的报文：单图发顶层 `image`、
		// 两图发 `image` + `image_tail`、多图发 `image_list:[{image: u}]`。
		// 三个写法在 omni 上全部无效 —— 上游对 image_list 元素的必填校验是
		// `image_url`（实测缺它返回 code=1102「第1张图片缺少image_url字段」），
		// 顶层 image/image_tail 则是未知字段被静默忽略。
		// 也就是「参考图一张都没进去，片子照出、钱照收」。
		if prompt == "" && refs.imageCount() == 0 {
			return "", "", &relay.CallerParamError{Msg: "omni 需要 prompt 或至少一张参考图"}
		}
		// 视频/音频素材在 omni 上不支持（上游另有约束）：明确拒绝而不是静默丢弃。
		// 静默丢弃等于「收了下发项的钱却不用素材」。
		if len(refs.videos) > 0 || len(refs.audios) > 0 {
			return "", "", &relay.CallerParamError{Msg: "omni 视频模型只接受图片素材（首帧/尾帧/参考图），不支持参考视频或音频"}
		}
		items, err := duomiOmniImageList(refs)
		if err != nil {
			return "", "", err
		}
		if len(items) > 0 {
			payload["image_list"] = items
		}
		// multi_shot 恒 false：多镜头需要 multi_prompt（每段 prompt + 时长），
		// 本平台没有分镜结构，置 true 会让上游按空 multi_prompt 处理。
		// 源代码同样恒发 false，这是显式声明的「单镜头」，不是省略。
		payload["multi_shot"] = false
	}

	// 平台没映射的顶层键原样下发（厂商私有键、上游新增参数…）。
	// 映射写过的键一律不被覆盖；素材与计费维度见 duomiKlingLocalOnly 的注释。
	relay.ForwardUnmapped(payload, req.Body, duomiKlingLocalOnly...)

	path := duomiKlingPath(opts.paths["kling.prefix"], seg, "")

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
	return id, "kling." + seg, nil
}

// duomiNonSeedMetadata 返回 metadata 里除 seed 之外的键（seed 是平台已知字段，另行下发）。
func duomiNonSeedMetadata(md map[string]any) map[string]any {
	if len(md) == 0 {
		return nil
	}
	out := make(map[string]any, len(md))
	for k, v := range md {
		if k == "seed" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// duomiSortedKeys 返回排序后的键名（错误文案必须稳定，便于运营按提示改参数）。
func duomiSortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// duomiKlingSegment 按模型档案与素材形态选提交路径段。
//
// 判定顺序是刻意的：**首/尾帧存在时永远走 image2video**，只有当素材全是
// 「普通参考图」且多于一张时才走 multi-image2video。
//
// 改造前只数图片个数（`n > 1 → multi`），于是「首帧 + 尾帧」这种最常见的用法
// 会被送进 multi-image2video：那是另一个端点（不同的输入语义、不同的上游成本），
// 首尾帧的时序关系完全丢失，而平台仍按 image2video 的价目收钱。
func duomiKlingSegment(profile duomiProfile, refs *duomiRefs) string {
	switch profile.Protocol {
	case duomiProtoKlingOmni:
		return duomiKlingOmniVideo
	case duomiProtoKlingMulti:
		return duomiKlingMulti2Video
	}
	if refs.firstFrame != "" || refs.lastFrame != "" {
		return duomiKlingImage2Video
	}
	switch {
	case len(refs.images) == 0:
		return duomiKlingText2Video
	case len(refs.images) == 1:
		return duomiKlingImage2Video
	default:
		return duomiKlingMulti2Video
	}
}

// parseDuomiKlingTask 解析可灵查询响应：
//
//	{code:0, message:"SUCCEED", data:{task_id, task_status:"succeed", task_status_msg, task_result:{videos:[{url}], images:[...]}}}
//
// code != 0 表示业务失败（1201 参数错、1203 任务不存在、1302 限流…）→ 归一为 failed，
// 让 worker 立刻退费，而不是累计网络失败到超时。
func parseDuomiKlingTask(buf []byte) (*relay.TaskStatus, error) {
	var p struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
		Data    struct {
			TaskID        string `json:"task_id"`
			TaskStatus    string `json:"task_status"`
			TaskStatusMsg string `json:"task_status_msg"`
			TaskResult    struct {
				Videos []struct {
					URL string `json:"url"`
				} `json:"videos"`
				Images []struct {
					URL string `json:"url"`
				} `json:"images"`
			} `json:"task_result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf, &p); err != nil {
		return nil, fmt.Errorf("适配器 duomiapi：解析 kling 查询响应失败：%w", err)
	}
	// 失败原因统一从信封取（message > msg > error.*）：可灵各版本分别把原因写在
	// `message` / `msg` / `data.task_status_msg`，只读一个会退化成「上游返回业务错误」。
	env := duomiEnvelopeOf(buf)

	if code, ok := duomiIntCodeOK(p.Code); ok && code != 0 && code != 200 {
		return &relay.TaskStatus{
			Status: taskstate.Failed,
			Error:  firstNonEmptyStr(env.text(), fmt.Sprintf("上游返回业务错误（code=%d）", code)),
		}, nil
	}

	out := &relay.TaskStatus{Status: duomiNormalizeStatus(p.Data.TaskStatus)}
	if out.Status == taskstate.Failed {
		out.Error = firstNonEmptyStr(p.Data.TaskStatusMsg, env.text(), "上游任务失败")
		return out, nil
	}
	if out.Status != taskstate.Succeeded {
		return out, nil
	}

	urls := make([]string, 0, len(p.Data.TaskResult.Videos)+len(p.Data.TaskResult.Images))
	for _, v := range p.Data.TaskResult.Videos {
		urls = append(urls, v.URL)
	}
	for _, v := range p.Data.TaskResult.Images {
		urls = append(urls, v.URL)
	}
	out.ResultURLs = duomiStrings(urls...)
	if len(out.ResultURLs) == 0 {
		out.Status = taskstate.Failed
		out.Error = "上游返回成功但缺少产物"
		return out, nil
	}
	out.ResultURL = out.ResultURLs[0]
	return out, nil
}

// joinInts 把整数切片拼成文案（如 "5 / 10"）。
func joinInts(vals []int64, sep string) string {
	parts := make([]string, 0, len(vals))
	for _, v := range vals {
		parts = append(parts, fmt.Sprintf("%d", v))
	}
	return strings.Join(parts, sep)
}

// ---------------------------------------------------------------- omni 素材

// duomiMaxOmniImages 是 omni-video 的 image_list 上限（官方文档 1~7 张）。
//
// **不能复用 duomiMaxKlingImages**：那是 multi-image2video 的 4 张上限，
// 两者是不同端点、不同上游成本，共用一个常量会让其中一个越界放行或提前拒绝。
const duomiMaxOmniImages = 7

// duomiOmniImageItem 是 omni-video 的 image_list 元素。
//
// 键名是 **image_url**，不是 image：
//   - multi-image2video 用 `{"image": url}`（多米该端点文档的 required 就是 image）；
//   - omni-video 用 `{"image_url": url}`（源代码 kling-omni-body-builder + 线上实测）。
//
// 实测证据：向 omni-video 发 `image_list:[{}]`，上游返回
// `{"code":1102,"message":"第1张图片缺少image_url字段"}` —— 它只认 image_url。
//
// Type 只认 first_frame / end_frame（发 last_frame 会被明确拒：
// 「第1张图片的type字段值无效: last_frame」）。注意是 **end_frame**，
// 不是 input_reference 里用的 last_frame —— 这个字面差异必须在这一层转换。
type duomiOmniImageItem struct {
	ImageURL string `json:"image_url"`
	Type     string `json:"type,omitempty"`
}

// duomiOmniImageList 把素材视图映射成 omni 的 image_list。
//
// omni 没有 image / image_tail 两个字段，首尾帧只能靠 image_list[].type 表达，
// 所以必须按**语义组合**分派，不能按图片张数分派：
// 「首帧 + 尾帧」在张数上和「两张普通参考图」完全一样，但报文形态相反
// （前者带 type 的 2 项、后者是不带 type 的 2 项）。
func duomiOmniImageList(refs *duomiRefs) ([]duomiOmniImageItem, error) {
	hasFirst := refs.firstFrame != ""
	hasLast := refs.lastFrame != ""
	n := len(refs.images)

	// 尾帧不能单独存在：上游会直接拒（实测「有尾帧图时必须有首帧图」），
	// 且本平台如果放过去，就是「收首尾帧的钱、只用了一张图的素材」。
	if !hasFirst && hasLast {
		return nil, &relay.CallerParamError{Msg: "omni 不支持只传尾帧；请同时提供首帧，或改用 reference_image"}
	}
	// 首尾帧 + 普通参考图：两种语义互斥。混传会得到 3 项以上 image_list，
	// 上游无从判断哪两张是首尾帧，时序必然错乱。
	if hasFirst && hasLast && n > 0 {
		return nil, &relay.CallerParamError{Msg: fmt.Sprintf(
			"omni 首尾帧模式不能同时传普通参考图（除首尾帧外还有 %d 张）；请二选一", n)}
	}
	if hasFirst && hasLast {
		return []duomiOmniImageItem{
			{ImageURL: refs.firstFrame, Type: "first_frame"},
			{ImageURL: refs.lastFrame, Type: "end_frame"},
		}, nil
	}

	// 其余情况一律「普通参考图」语义（不带 type）：
	// 只有首帧 = 单图生视频，首帧排最前；纯参考图按请求顺序。
	out := make([]duomiOmniImageItem, 0, refs.imageCount())
	if hasFirst {
		out = append(out, duomiOmniImageItem{ImageURL: refs.firstFrame})
	}
	for _, u := range refs.images {
		out = append(out, duomiOmniImageItem{ImageURL: u})
	}
	return out, nil
}

// duomiKlingCameraTypeSimple 是「按六轴自定义运镜」的官方取值。
const duomiKlingCameraTypeSimple = "simple"

// duomiKlingCamera 校验并规范化运镜参数（返回可直接下发的 {type, config} 对象）。
//
// 三条硬约束（都来自可灵官方契约，违反任一条上游都会 400，而报错信息
// 完全不提"运镜"二字，会被归因成渠道故障）：
//  1. 六个轴只能有**一个**非零 —— 官方不支持组合运镜；
//  2. 每个轴取值 [-10, 10]，超出即非法（不是截断）；
//  3. 与首尾帧模式互斥 —— 已有首/尾帧时运镜会被忽略或拒绝，
//     平台按非法处理并说明原因，不静默丢弃。
//
// 返回 nil 表示「不下发」：调用方没写、或写了全 0（全 0 也是非法，见上）。
func duomiKlingCamera(cc *duomiCameraControl) (*duomiCameraOut, error) {
	if cc == nil {
		return nil, nil
	}
	axes := cc.axes()
	switch {
	case len(axes) == 0:
		return nil, &relay.CallerParamError{Msg: "camera_control 至少要有一个方向非零：" +
			"horizontal / vertical / pan / tilt / roll / zoom 各取 -10 ~ 10"}
	case len(axes) > 1:
		names := make([]string, 0, len(axes))
		for _, a := range axes {
			names = append(names, a.Name)
		}
		return nil, &relay.CallerParamError{Msg: "camera_control 一次只能指定一个方向，当前同时给了 " +
			strings.Join(names, " / ") + "（可灵不支持组合运镜）"}
	}
	if v := axes[0].Val; v < -10 || v > 10 {
		return nil, &relay.CallerParamError{Msg: fmt.Sprintf(
			"camera_control.%s 必须在 -10 ~ 10 之间（当前 %g）", axes[0].Name, v)}
	}
	// 出站补零成六键：上游按「固定六个字段」解析，缺键与全 0 都会被判非法。
	return &duomiCameraOut{
		Type: duomiKlingCameraTypeSimple,
		Config: &duomiCameraControl{
			Horizontal: cc.Horizontal, Vertical: cc.Vertical,
			Pan: cc.Pan, Tilt: cc.Tilt, Roll: cc.Roll, Zoom: cc.Zoom,
		},
	}, nil
}

// duomiKlingVersionRe 取型号里的版本号（kling-v3 / kling-v1-6 / kling-v2.5 都认）。
//
// 用「版本号」而不是「含 v3」：厂商的型号是递增的（v3 → v4 → v5），
// 按子串判会把所有新版本判成不支持，表现为「成片无声但照常收费」，且完全无日志。
var duomiKlingVersionRe = regexp.MustCompile(`(?:^|[-_.])v(\d{1,2})(?:[._-]|$)`)

// duomiKlingSound 决定是否给上游下发 sound:"on"。
//
//	v1 / v2        → 不支持同声音画（v1.6-multi 实测不下发）
//	v3 及更新      → 支持
//	认不出版本号   → 按支持处理并留痕
//
// 第三档的取舍：给不认识的型号多发一个字段，最坏是上游忽略它；
// 而不发导致的「静默无声」用户要到成片出来才发现，钱已经扣了。
// 若上游对未知型号的 sound 是 **400** 而不是忽略，把这一档改成 return false, true
// （见 docs/渠道接入手册-新增模型与协议.md 的验证方法）。
func duomiKlingSound(upstream string) (on, versionUnknown bool) {
	id := strings.ToLower(strings.TrimSpace(upstream))
	if id == "" {
		return false, false
	}
	m := duomiKlingVersionRe.FindStringSubmatch(id)
	if m == nil {
		return true, true
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return true, true
	}
	return n >= 3, false
}
