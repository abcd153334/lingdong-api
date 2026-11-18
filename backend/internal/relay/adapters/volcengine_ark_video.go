package adapters

// 火山方舟视频链路（Seedance 系列）：提交 + 轮询。
//
// **整条链路不使用官方 SDK**，这是刻意的取舍（详见 docs/火山方舟接入方案.md 2.5.1）：
//  1. SDK 的 CreateContentGenerationTaskRequest 用 `file_format` 表达输出格式，
//     而官方文档字段是 `output_format`（创建视频生成任务.md:566）—— 用 typed 结构
//     传 `output_format:"mov"` 会被**静默丢弃**，用户拿到 mp4 却查不出原因。
//  2. SDK 的 TaskStatus 枚举只有 5 个值，**没有 `expired`**，且解析是严格的；
//     上游返回「已过期」时 SDK 直接报解析错 → 轮询连败 20 次判失败（白挂一小时）
//     并误记渠道健康（把一条健康渠道熔断）。
//  3. 「不遗漏字段」的硬要求：typed 往返必然重排键、丢未知键。原始 JSON 直发是唯一
//     能保证「用户传什么、上游就收到什么」的形态。
//
// 代价：提交/轮询要自己写 HTTP（arkDo）与宽松解析。这与「用官方 SDK」并不矛盾——
// 图片链路仍然用 SDK 的 typed 结构构请求，视频链路用文档事实构请求。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/taskstate"
	"lingdong-api/internal/relay"
)

// arkSubKindNative / arkSubKindSpec 是落 tasks.subtask_kind 的子协议标记。
//
// 只用于排障留痕：两条通道的**轮询路径相同**（同一个 GET），
// 但重试策略不同（原生体无法重放，见 task.Retry）—— 标记是那条策略的判据。
const (
	arkSubKindNative = "ark.native"
	arkSubKindSpec   = "ark.spec"
)

// arkMaxPromptRunes 是提示词长度上限。
//
// 取 20000 字符而不是官方建议的「中文 500 字」：官方建议是效果建议，不是硬限制；
// 硬限制来自请求体大小。设一个有明确依据的上界（远小于 64 MB 请求体上限），
// 目的是让明显异常的请求在本地 400，而不是带着几十 MB 文本出站。
const arkMaxPromptRunes = 20000

// arkVendorTopKeys 是 Ark 认可、且允许原样出现在请求体**顶层**的厂商参数（唯一清单）。
//
// 一份清单两个用途，缺了任何一半都会出静默错误：
//   - **出站**：顶层与 metadata 里命中的键提升到 Ark 顶层（未命中的一律不出站 ——
//     metadata 是平台通用通道，全量平铺会把别家私有键塞进 Ark，上游是 400 还是忽略不可控）；
//   - **metadata 闸门**：metadata 里只有命中的键才允许提升，其余报一条告警
//     （metadata 是平台通用通道，全量平铺会把别家私有键塞进 Ark）。
//
// 表内键必须**在上游顶层合法**：`aspect_ratio` 这类平台别名不能进（Ark 只认 ratio），
// 混进来就是必然 400；平台规范化由 arkBuildSpecBody 的 typed 字段负责。
//
// 与 arkSpecOwnedKeys 的分工：本表是「原样透传的值」，arkSpecOwnedKeys 是「被适配器
// 消费/翻译的键」。一个键可以同时在两处（watermark —— 既要透传也要能被识别为已知键，
// 否则每天报一条"未识别参数"告警）。
var arkVendorTopKeys = []string{
	"watermark",
	"generate_audio",
	"return_last_frame",
	"camera_fixed",
	"service_tier",
	"priority",
	"tools",
	"draft",
	"output_format",
	"omni_reference_task_type",
	"execution_expires_after",
	"safety_identifier",
	// seed 必须在表内（P3-6）：它是视频/图片的确定性种子，Ark 在顶层接受它。
	// 漏掉的后果最隐蔽——首次请求被平台当未知字段丢弃、重试时又从快照回填生效，
	// 于是「同一份请求两次结果不同」，而两次请求的代码单看都对。
	"seed",
	"frames",
	// subdivision_level / optimize_prompt 已删除（P2-9）：官方数据面文档里查不到
	// 这两个顶层键，留着等于「表面能配、实际靠上游忽略」。删掉后它们会落进
	// ignored 告警，用户至少能看到「没生效」，而不是以为生效了。
	// callback_url 只在显式传入时透传：
	// 平台自己**不接收**回调（没有回调端点），默认丢弃才是正确语义。
	"callback_url",
}

// arkMetaWhitelist 是本表的查表形态（由 arkVendorTopKeys 派生，禁止另写一份）。
var arkMetaWhitelist = func() map[string]bool {
	m := make(map[string]bool, len(arkVendorTopKeys))
	for _, k := range arkVendorTopKeys {
		m[k] = true
	}
	return m
}()

// arkMetadataMaxBytes 是 metadata 的体积上限（64 KB）。
//
// 取 64 KB：白名单里的键全是标量（布尔/小整数/枚举字符串），
// 正常请求的 metadata 只有几十字节。给到 64 KB 是留足「以后上游加个结构体参数」的余量，
// 同时把「把 base64 塞进 metadata」这类用法挡在入口。
const arkMetadataMaxBytes = 64 << 10

// UnreplayableSubKinds 实现 relay.UnreplayableSupplier。
//
// 原生体（content[]）的请求体完全由用户拼装，平台只替换 model：它不由 request_params
// 里的规范字段拼装得出，重放只会造出「按素材计费、只发一段文本」的错账。
// 因此 fail-closed 拒绝重试，而不是再存一份原始体（那会成为与 InputRef 并存的第二真相源）。
func (VolcengineARK) UnreplayableSubKinds() []string { return []string{arkSubKindNative} }

// arkIsNative 判定走原生通道：body 里有**非空** `content[]`。
//
// 只看这一个事实，不看模型名、不看 metadata：
// 看模型名会造出第二套判定，运营给一个模型改绑别名后判定就漂了。
func arkIsNative(body []byte) bool {
	var probe struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return false
	}
	var arr []json.RawMessage
	return json.Unmarshal(probe.Content, &arr) == nil && len(arr) > 0
}

// SubmitTask 提交视频任务，返回上游任务号。
func (VolcengineARK) SubmitTask(ctx context.Context, cfg *relay.ChannelConfig,
	req *relay.TaskSubmitRequest) (string, error) {

	if req == nil {
		return "", &relay.CallerParamError{Msg: "提交参数为空"}
	}
	// 提示词弱校验参数（`--rs/--dur/--frames`）**只留痕、不改字节**：
	// 上游会把它们当真实参数执行，而平台只按 body 计费。详见 volcengine_ark_weakparams.go。
	// 检测放在最前面：原生通道与规范体都要走（原生通道更容易藏参数）。
	if notes := arkWeakNotesFromBody(req.Body); len(notes) > 0 {
		req.Notes = append(req.Notes, notes...)
	}
	native := arkIsNative(req.Body)
	var (
		body []byte
		err  error
	)
	if native {
		// 原生通道只做一件事：替换 model。其余字节原样（1:1 的最硬保障）。
		body, err = arkReplaceModel(req.Body, req.UpstreamModel)
	} else {
		var ignored []string
		body, ignored, err = arkBuildSpecBody(req.Body, req.UpstreamModel, req.AutoDuration, req.OmitDuration)
		// 认不出的顶层参数**原样转发**，只留一条告警。
		//
		// 不再丢弃：丢弃等于平台替上游判定「这个参数没用」，而厂商随时会加字段。
		// 告警的作用变成「这次请求带了平台没映射的键」——如果上游回 400，这条日志
		// 就是定位线索（用户拼错了 `frame` vs `frames`，或照别家协议传参）。
		//
		// 只记键名不记值：值里可能是 prompt 的一部分。
		if len(ignored) > 0 {
			logger.L().Warn("视频请求含平台未映射的参数，已原样转发",
				zap.Strings("keys", ignored),
				zap.String("model", req.UpstreamModel))
		}
	}
	if err != nil {
		return "", err
	}
	// 校验只针对**规范体**。
	//
	// 原生 content[] 通道的契约是「平台只替换 model，不改变出站报文」
	// （对外文档 endpoints.go 的 content 字段），所以这里不能拿平台对规范体的
	// 约束（20000 字 prompt 上限、首尾帧与参考图互斥）去卡用户手写的原生体 ——
	// 那些是**平台自己**为了让规范体可计费而设的限制，上游是否接受由上游判定。
	// 拦在这里的表现就是「照文档写原生体，平台回 400 说首尾帧不能混用」。
	//
	// 保留的只有结构性检查：必须是合法 JSON（否则根本发不出去）。
	if native {
		if !json.Valid(body) {
			return "", &relay.CallerParamError{Msg: "原生请求体不是合法 JSON"}
		}
	}
	resp, err := arkDo(ctx, cfg, http.MethodPost, arkPathTaskSubmit, body)
	if err != nil {
		return "", err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return "", arkSubmitError(resp.Status, resp.Body, cfg)
	}
	id, perr := arkDecodeTaskID(resp.Body)
	if perr != nil {
		// 2xx 却解析不出任务号 = 契约违约。必须让上层**停止 failover**：
		// 换渠道重投会再建一个上游任务（双倍成本，且第一个任务永远无人查询）。
		// AmbiguousSubmitError 正是这个语义（网关会 502 + 全额退款 + 打标对账）。
		return "", &relay.AmbiguousSubmitError{Err: fmt.Errorf("上游未返回任务号：%s", snippet(resp.Body))}
	}
	// SubKind 由 task.Submit 落库：轮询与重试策略都读它。
	if native {
		req.SubKind = arkSubKindNative
	} else {
		req.SubKind = arkSubKindSpec
	}
	return id, nil
}

// arkSubmitError 按状态码分档归因（与 duomiapi / metaso 同一口径）。
//
// 为什么不能一律返回普通 error：5xx 带体 vs 不带体决定「上游是否可能已建单」，
// 而 IsAmbiguousSubmit 只认 HTTPStatusError / 网络错误。全压成 fmt.Errorf 会让
// 「网关 502 在上游建单之后返回」被当成可换渠道 → 建出两个上游任务。
func arkSubmitError(status int, body []byte, cfg *relay.ChannelConfig) error {
	detail := arkRedact(arkErrText(body), cfg)
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		// 参数错：换渠道不会变好，必须立刻 400（task.Submit 会终止 failover）。
		return &relay.CallerParamError{Msg: detail}
	case http.StatusUnauthorized, http.StatusForbidden:
		return &relay.HTTPStatusError{Code: status,
			Note: fmt.Sprintf("火山方舟鉴权失败（HTTP %d），请检查渠道密钥：%s", status, detail)}
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return &relay.HTTPStatusError{Code: status,
			Note: fmt.Sprintf("火山方舟接口路径不存在（HTTP %d），请确认 Base URL 填到 /api/v3 为止：%s", status, detail)}
	case http.StatusTooManyRequests:
		return &relay.HTTPStatusError{Code: status, Note: "火山方舟上游限流（HTTP 429）：" + detail}
	default:
		return relay.StatusError(status, detail)
	}
}

// arkDecodeTaskID 从提交响应里取任务号。
//
// Ark 的响应是 `{"id":"cgt-..."}`；为兼容网关改版与代理包装，同时接受
// data.id / task_id / data.task_id 三种历史写法（都见过）。
func arkDecodeTaskID(body []byte) (string, error) {
	var probe struct {
		ID     string          `json:"id"`
		TaskID string          `json:"task_id"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", err
	}
	for _, cand := range []string{probe.ID, probe.TaskID} {
		if v := strings.TrimSpace(cand); v != "" {
			return v, nil
		}
	}
	if len(probe.Data) > 0 {
		var nested struct {
			ID     string `json:"id"`
			TaskID string `json:"task_id"`
		}
		if json.Unmarshal(probe.Data, &nested) == nil {
			for _, cand := range []string{nested.ID, nested.TaskID} {
				if v := strings.TrimSpace(cand); v != "" {
					return v, nil
				}
			}
		}
	}
	return "", fmt.Errorf("响应里没有任务号")
}

// arkBuildSpecBody 把平台的规范字段组装成 Ark 请求体（内容映射见 2.5.3 全表）。
//
// autoDuration 来自 task.Submit（params.SecondsAuto），**不从 body 猜**：
// body 里可能是 `seconds`、`duration`，也可能两者都有；判定只能有一处。
// arkSpecOwnedKeys 是 `arkBuildSpecBody` **自己消费**的顶层键。
//
// 分三类，缺一类都会让下面的「未识别参数」告警变成噪音：
//  1. 平台规范字段（prompt / seconds / resolution / …）—— 由 typed 结构读取；
//  2. 平台别名（image / width / height / first_frame / last_frame）—— task 层已
//     折叠进 input_reference，但**原键不会删**（`normalizeSubmitBody` 只删首尾帧）。
//     不列进来的后果是：用户正常传 `image` 提交视频，日志里每天报一条"未识别参数"；
//  3. `model` / `content` —— 前者被替换、后者是原生通道的载体。
var arkSpecOwnedKeys = map[string]bool{
	"model": true, "content": true,
	"prompt": true, "seconds": true, "duration": true,
	"resolution": true, "size": true, "aspect_ratio": true, "ratio": true,
	"seed": true, "watermark": true, "generate_audio": true,
	"input_reference": true, "metadata": true,
	"image": true, "width": true, "height": true,
}

// arkBuildSpecBody 把平台规范的视频请求体构造成 Ark 的 content[] 形态。
//
// 第三个返回值是「认不出的顶层键」（已排序），由调用方记告警 —— 函数本身保持纯粹，
// 这样「丢弃了哪些键」可以直接被测试断言，不必去抓日志。
func arkBuildSpecBody(body []byte, upstreamModel string, autoDuration, omitDuration bool) ([]byte, []string, error) {
	var in struct {
		Prompt      string                     `json:"prompt"`
		Seconds     json.RawMessage            `json:"seconds"`
		Duration    json.RawMessage            `json:"duration"`
		Resolution  string                     `json:"resolution"`
		Size        string                     `json:"size"`
		AspectRatio string                     `json:"aspect_ratio"`
		Ratio       string                     `json:"ratio"`
		Seed        json.RawMessage            `json:"seed"`
		InputRef    []arkInputRefItem          `json:"input_reference"`
		Metadata    map[string]json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(body), &in); err != nil {
		return nil, nil, &relay.CallerParamError{Msg: "视频参数不合法：" + err.Error()}
	}
	// 顶层原文留一份：白名单透传要按字段名取值（typed 结构只认识平台规范字段）。
	var top map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(body), &top); err != nil {
		return nil, nil, &relay.CallerParamError{Msg: "视频参数不合法：" + err.Error()}
	}
	// metadata 体积闸（B7-5）：它是「厂商扩展参数」通道，正常只有几个标量。
	// 没有这道闸时，网关的 16 MiB 请求体上限就是它的实际上限 —— 有人往里塞
	// base64 图片或整段提示词，请求体能过、上游会 400（或忽略），而这条几 MB 的
	// JSON 会一路跟着 RequestParams 落进 tasks 的 jsonb、每次重试/replay 再写一遍。
	// 拒绝在入口，比事后清库划算得多。
	if raw, ok := top["metadata"]; ok && len(raw) > arkMetadataMaxBytes {
		return nil, nil, &relay.CallerParamError{
			Msg: fmt.Sprintf("metadata 过大（%d 字节，上限 %d 字节）：这里只放厂商扩展参数，不要放图片或长文本",
				len(raw), arkMetadataMaxBytes),
		}
	}

	content := make([]any, 0, len(in.InputRef)+1)
	// 文本必须排第一：Ark 按 content 顺序解析指代（`--参数` 的弱校验也依赖文本在首位）。
	if p := strings.TrimSpace(in.Prompt); p != "" {
		content = append(content, map[string]any{"type": "text", "text": p})
	}
	for _, r := range in.InputRef {
		item, err := r.toArkContent()
		if err != nil {
			return nil, nil, err
		}
		content = append(content, item)
	}
	if len(content) == 0 {
		return nil, nil, &relay.CallerParamError{Msg: "请提供 prompt 或至少一个素材（图片/视频/音频）"}
	}

	out := map[string]any{"model": upstreamModel, "content": content}

	// 时长：autoDuration 时发 -1（Ark 的「智能时长」），否则发请求秒数。
	// 计费基准与上游参数是**两个语义**，平台侧已在 task 层拆开（params.Seconds/SecondsAuto）。
	switch {
	case autoDuration:
		out["duration"] = -1
	case omitDuration:
		// 用户没写时长、模型也不认 -1：**不下发**该字段，由 Ark 用自己的默认值
		// （不猜一个秒数：猜大了用户多付，猜小了拿到的不是他要的片子）。
	case true:
		if v, ok := arkIntValue(in.Seconds); ok {
			out["duration"] = v
		} else if v, ok := arkIntValue(in.Duration); ok {
			out["duration"] = v
		}
	}

	if v := firstNonEmptyStr(in.Resolution, in.Size); v != "" {
		// 档位键必须小写（Ark 只认 480p/720p/1080p/4k）：task 层已归一，
		// 这里再 ToLower 一次是防「运营在价目里配了 1080P」这种历史脏数据出站被拒。
		out["resolution"] = strings.ToLower(v)
	}
	if v := firstNonEmptyStr(in.AspectRatio, in.Ratio); v != "" {
		out["ratio"] = v
	}
	if v, ok := arkIntValue(in.Seed); ok {
		out["seed"] = v
	}

	// metadata 白名单提升到顶层（输出格式 / 尾帧 / 样片 / 优先级 / 水印等）。
	//
	// watermark 与 generate_audio 也走这一条路（不再单独 typed 读取）：
	// 两个入口（顶层 / metadata）必须走同一段代码，否则「顶层传 false → 下发、
	// metadata 传 false → 丢失」这类差异永远查不出来。
	metaIgnored := make([]string, 0, 2)
	for k, v := range in.Metadata {
		if !arkMetaWhitelist[k] {
			// metadata 里的未知键同样要留痕（P2-6）：与顶层共用同一套告警机制，
			// 不另造一条「直接 400」的规则 —— 那会让同一个键在两个入口有两种命运。
			// 前缀标出它来自哪个入口，否则用户拿着 "foo" 会去顶层找。
			metaIgnored = append(metaIgnored, "metadata."+k)
			continue
		}
		raw := bytes.TrimSpace(v)
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		out[k] = json.RawMessage(raw)
	}
	// 顶层厂商参数透传（**与 metadata 同一份白名单**）。
	//
	// 为什么两个入口都要认：契约里这些字段是**顶层**公开的（照文档粘贴就该生效），
	// metadata 只是历史入口。只认 metadata 的后果是「文档写了 frames，实际发出去没有」——
	// 静默丢参，用户看到的是"模型不按我说的时长生成"，查不出原因。
	//
	// 只放行白名单键：未知键既不能猜也不能直发（上游对未知字段是 400 还是忽略不可控）。
	for k := range arkMetaWhitelist {
		raw, ok := top[k]
		if !ok {
			continue
		}
		v := bytes.TrimSpace(raw)
		if len(v) == 0 || string(v) == "null" {
			continue
		}
		out[k] = json.RawMessage(v) // metadata 里的同名键已先写入，语义相同，覆盖无害
	}

	// 平台不认识的顶层键：**原样透传**，只留一条告警。
	//
	// 以前这里是"丢弃 + 告警"。丢弃等于平台替上游判定「这个参数没用」，
	// 而事实是厂商随时会加字段（`omni_reference_task_type` 当初就是这样被吃掉的）。
	// 透传的代价是上游可能对未知键报错 —— 那是上游的原话，用户按文档改即可。
	ignored := make([]string, 0, 4)
	for k, raw := range top {
		if arkSpecOwnedKeys[k] || arkMetaWhitelist[k] {
			continue
		}
		if v := bytes.TrimSpace(raw); len(v) > 0 && string(v) != "null" {
			out[k] = json.RawMessage(v)
		}
		ignored = append(ignored, k)
	}
	ignored = append(ignored, metaIgnored...)
	sort.Strings(ignored) // 顺序稳定：同一请求每次告警的键序一致，便于聚合

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, nil, err
	}
	return raw, ignored, nil
}

// arkInputRefItem 是平台规范素材项（type + 三个互斥链接字段）。
type arkInputRefItem struct {
	Type     string `json:"type"`
	ImageURL string `json:"image_url"`
	VideoURL string `json:"video_url"`
	AudioURL string `json:"audio_url"`
}

// toArkContent 把平台的素材项映射成 Ark 的 content 元素。
//
// role 必须显式写出：Ark 在「首尾帧」场景下 role 必填，省略时的推断规则随模型版本变化。
// 显式写出来对上游是幂等的，对排查是可读的。
func (r arkInputRefItem) toArkContent() (map[string]any, error) {
	typ := strings.ToLower(strings.TrimSpace(r.Type))
	// 未标注类型时按链接推断：平台文档承诺「按链接推断」，这里必须兑现，
	// 否则用户不传 type 的合法请求会在适配器里 400。
	if typ == "" {
		switch {
		case strings.TrimSpace(r.VideoURL) != "":
			typ = "video"
		case strings.TrimSpace(r.AudioURL) != "":
			typ = "audio"
		default:
			typ = "image"
		}
	}
	switch typ {
	case "image", "first_frame", "last_frame", "reference_image":
		u := strings.TrimSpace(r.ImageURL)
		if u == "" {
			return nil, &relay.CallerParamError{Msg: "图片素材缺少 image_url"}
		}
		role := "reference_image"
		switch typ {
		case "first_frame", "last_frame":
			role = typ
		case "reference_image":
			role = "reference_image"
		case "image":
			// 平台语义：input_reference 里的 image 就是参考图。
			role = "reference_image"
		}
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}, "role": role}, nil
	case "video", "base_video":
		u := strings.TrimSpace(r.VideoURL)
		if u == "" {
			return nil, &relay.CallerParamError{Msg: "视频素材缺少 video_url"}
		}
		return map[string]any{"type": "video_url", "video_url": map[string]any{"url": u}, "role": "reference_video"}, nil
	case "audio":
		u := strings.TrimSpace(r.AudioURL)
		if u == "" {
			return nil, &relay.CallerParamError{Msg: "音频素材缺少 audio_url"}
		}
		return map[string]any{"type": "audio_url", "audio_url": map[string]any{"url": u}, "role": "reference_audio"}, nil
	}
	return nil, &relay.CallerParamError{Msg: "不支持的素材类型：" + r.Type}
}

// arkValidateTaskBody 做「本地就能定性」的视频校验。
//
// 三条都对应「等上游异步报错就太晚」的场景：Ark 在提交阶段只做浅校验，
// PollTask 是 TaskAdapter 的旧签名入口（保留：外部实现方可能按它断言）。
func (VolcengineARK) PollTask(ctx context.Context, cfg *relay.ChannelConfig, upstreamTaskID string) (*relay.TaskStatus, error) {
	return VolcengineARK{}.PollTaskDetailed(ctx, cfg, &relay.TaskPollRequest{UpstreamTaskID: upstreamTaskID})
}

// PollTaskDetailed 查询任务状态。
//
// 宽松解析是这里的核心设计：status 用 string，不校验枚举。
// 严格解析（SDK 的做法）在上游新增状态词时会解析失败 → 轮询连败 20 次 → 判失败退款，
// 并把一条健康渠道记成故障。这里「不认识」只意味着「继续等」，且 StatusMatched=false
// 让告警能统计到「有一个状态词我们还没认识」。
func (VolcengineARK) PollTaskDetailed(ctx context.Context, cfg *relay.ChannelConfig,
	req *relay.TaskPollRequest) (*relay.TaskStatus, error) {

	if req == nil || strings.TrimSpace(req.UpstreamTaskID) == "" {
		return nil, &relay.CallerParamError{Msg: "缺少任务号"}
	}
	path := fmt.Sprintf(arkPathTaskGetFmt, url.PathEscape(strings.TrimSpace(req.UpstreamTaskID)))
	resp, err := arkDo(ctx, cfg, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		switch {
		case resp.Status == http.StatusUnauthorized || resp.Status == http.StatusForbidden:
			return nil, &relay.HTTPStatusError{Code: resp.Status,
				Note: fmt.Sprintf("火山方舟鉴权失败（HTTP %d），请检查渠道密钥：%s", resp.Status, arkRedact(arkErrText(resp.Body), cfg))}
		case resp.Status == http.StatusNotFound:
			// 任务不存在：可能是过期后被上游清理。按「上游已无此任务」处理，
			// 让上层继续轮询到超时退款，而不是当成渠道故障去熔断。
			return &relay.TaskStatus{Status: taskstate.Running, RawStatus: "not_found", StatusMatched: true}, nil
		case resp.Status == http.StatusTooManyRequests || resp.Status >= 500:
			// 限流与上游故障：可重试，维持错误语义（worker 会记连败并下轮再试）。
			return nil, arkSubmitError(resp.Status, resp.Body, cfg)
		default:
			// 其余 4xx（P3-4）：请求本身不被接受 —— 任务号非法、参数不兼容、
			// 任务已不可查询。重试 20 次也不会变好，此前会一直重试到平台超时
			// （默认 1 小时）才判失败，用户白等一小时且期间渠道连败计数被刷满。
			// 直接判失败 → 立即全额退款，用户能马上重试。
			return &relay.TaskStatus{
				Status:        taskstate.Failed,
				RawStatus:     "http_" + fmt.Sprint(resp.Status),
				StatusMatched: true,
				Error: fmt.Sprintf("上游拒绝查询该任务（HTTP %d）：%s",
					resp.Status, arkRedact(arkErrText(resp.Body), cfg)),
			}, nil
		}
	}

	var r struct {
		Status  string       `json:"status"`
		Error   *arkAPIError `json:"error"`
		Content struct {
			VideoURL     string `json:"video_url"`
			LastFrameURL string `json:"last_frame_url"`
			FileURL      string `json:"file_url"`
		} `json:"content"`
		// Usage 取原文而不是解成 token 结构（B7-3）。
		//
		// Ark 视频任务的 usage 目前是 token 口径（completion_tokens / total_tokens），
		// 与「秒数」无关 —— 秒数只能从 duration/frames 推。但把它当原文留下来，
		// 是发现「上游其实报了我们没解析的字段」的唯一途径；
		// 解成两个 token 字段反而丢掉了其余部分。
		Usage           json.RawMessage `json:"usage"`
		Duration        int64           `json:"duration"`
		Frames          int64           `json:"frames"`
		FramesPerSecond int64           `json:"framespersecond"`
	}
	if err := json.Unmarshal(resp.Body, &r); err != nil {
		return nil, fmt.Errorf("上游任务查询响应不是合法 JSON：%w", err)
	}

	raw := strings.TrimSpace(r.Status)
	st := &relay.TaskStatus{StatusMatched: true, RawStatus: raw}
	// 用量原文在任何状态下都留（失败的任务也可能已经计费）。
	st.Usage.SetRaw(r.Usage)
	switch strings.ToLower(raw) {
	case "queued":
		st.Status = taskstate.Queued
	case "running":
		st.Status = taskstate.Running
	case "succeeded":
		st.Status = taskstate.Succeeded
		// 顺序即产物顺序：「主视频」恒为 index 0（manifestStats 靠它选主产物）。
		st.ResultURLs = arkCompactURLs(r.Content.VideoURL, r.Content.LastFrameURL, r.Content.FileURL)
		// 秒数优先用帧数换算。Ark 的 duration 是「实际帧数 / 24 向下取整」
		// （官方「duration 返回值说明」：133 帧 → 实际 5.54 秒 → 返回 5），
		// 只读 duration 会让每条视频系统性少收最多 1 秒。
		//
		// frames 与 framespersecond 同时存在才换算；缺 fps 时**不猜 24** ——
		// 猜错会变成静默多收/少收，宁可让上层打 usage_upstream_missing 让人看见。
		switch {
		case r.Frames > 0 && r.FramesPerSecond > 0:
			st.Usage.OutputSeconds = float64(r.Frames) / float64(r.FramesPerSecond)
		case r.Duration > 0:
			st.Usage.OutputSeconds = float64(r.Duration)
		}
	case "failed":
		st.Status = taskstate.Failed
		st.Error = arkTaskErrorText(r.Error)
	case "cancelled", "canceled":
		// 上游明确说「被取消」：映射成 canceled 而不是 failed（P3-5）。
		// 两者在结算上都走全额释放，但对外语义不同 —— 用户看到「已取消」
		// 知道是自己（或上游）主动取消的，看到「失败」会以为是平台故障去报障。
		st.Status = taskstate.Canceled
		st.Error = "上游任务已取消"
	case "expired":
		// 必须显式映射：relay.statusAliases 不认 expired，落到 default 会被当成
		// running 一直轮询到平台超时（默认 1 小时）才退款，用户白等。
		st.Status = taskstate.Failed
		st.Error = "上游任务已过期（超过执行时限），已全额释放"
	default:
		// 不认识的状态：保持运行中并标记 StatusMatched=false（供告警统计）。
		// 绝不判失败：上游新增排队态时误判失败会立即退款并丢弃已生成的产物。
		st.Status = taskstate.Running
		st.StatusMatched = false
	}
	return st, nil
}

// arkAPIError 是 Ark 的错误对象（提交与查询两个端点的形状一致）。
type arkAPIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// arkTaskErrorText 拼出「message（code）」形式的失败原因（两者都缺时给兜底文案）。
func arkTaskErrorText(e *arkAPIError) string {
	if e == nil {
		return "上游任务失败"
	}
	code := strings.TrimSpace(e.Code)
	msg := strings.TrimSpace(e.Message)
	switch {
	case code != "" && msg != "":
		return msg + "（" + code + "）"
	case msg != "":
		return msg
	case code != "":
		return "上游任务失败（" + code + "）"
	}
	return "上游任务失败"
}

// arkCompactURLs 跳过空链接（空串会占号，导致产物编号不稠密）。
func arkCompactURLs(urls ...string) []string {
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		if s := strings.TrimSpace(u); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// arkIntValue 把 JSON 原始值解析成整数（支持数字与数字字符串）；缺失/null 返回 false。
func arkIntValue(raw json.RawMessage) (int64, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, false
	}
	var i int64
	if err := json.Unmarshal(raw, &i); err == nil {
		return i, true
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		if v, perr := strconv.ParseInt(strings.TrimSpace(str), 10, 64); perr == nil {
			return v, true
		}
	}
	return 0, false
}
