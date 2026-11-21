package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"lingdong-api/internal/pkg/taskstate"
	"lingdong-api/internal/relay"
)

// duomiBody 是提交请求体的**只读视图**（白名单字段，绝不整体透传给上游）。
//
// 为什么必须白名单重建：平台的 normalizeSubmitBody 对所有请求体做无条件改写
// （duration→seconds、width/height→resolution、image→input_reference），
// 原样转发会把平台内部字段塞给不认它的上游（图像模型收到 seconds、seedance 收到 size），
// 直接 400 且报错信息与真实原因无关。
type duomiBody struct {
	Model   string          `json:"model"`
	Prompt  string          `json:"prompt"`
	Seconds json.RawMessage `json:"seconds"`
	// Duration 是部分客户端用的时长写法（与 seconds 同义）。
	// 平台归一后通过 TaskSubmitRequest.Seconds 下发；这里保留作同步/重放兜底。
	Duration   json.RawMessage `json:"duration"`
	Resolution string          `json:"resolution"`
	Size       string          `json:"size"`
	ImageSize  string          `json:"image_size"`
	// AspectRatio / Ratio 是同一字段的两种写法（P2-10）：Ratio 优先，两者同值时不重复下发。
	AspectRatio string `json:"aspect_ratio"`
	Ratio       string `json:"ratio"`
	Quality     string `json:"quality"`
	// MaskURL 是平台蒙版的**唯一**字段名（公网直链），入站与出站同名：适配器不做
	// 改名，出站报文里出现的也是 mask_url（调用方传什么就看到什么）。
	//
	// 入站只认这一个名字：客户端写 `mask` 时它作为「未声明键」被原样透传，
	// 平台不解释、不改写、不承诺语义。
	MaskURL string `json:"mask_url"`
	// ResponseFormat 是 OpenAI 的返回形态（url / b64_json）。
	//
	// **平台本地消费**：同步桥据此决定把产物转成 b64 还是直链，必须放进 localOnly、
	// 绝不下发 —— 多米的上游 schema 没有这个键，发给它只会换来一句未知字段错误。
	ResponseFormat string `json:"response_format"`
	Background     string `json:"background"`
	// MV 是音乐生成要的模型版本串（如 chirp-hawk）。
	//
	// 必须在这里显式解析：它同时在 `duomiSunoMusicLocalOnly` 里（未映射键不能
	// 原样透传），所以不解析就等于**客户端传的 mv 被静默丢弃**，出站只剩渠道默认
	// 或绑定里的 upstream_model —— 用户按文档换了模型版本，声音一点没变。
	MV string `json:"mv"`
	// GenerateAudio / Watermark 是 seedance 的顶层成本参数（P1-25）。
	//
	// 它们会改变上游产出与成本，所以：① 只对 seedance 协议下发；② 顶层优先于 metadata；
	// ③ 顶层出现不受 metadata_allow 白名单限制（顶层是本平台的显式契约字段，
	// 白名单管的是"任意键透传"这件事）。
	GenerateAudio *bool           `json:"generate_audio"`
	Watermark     *bool           `json:"watermark"`
	Metadata      map[string]any  `json:"metadata"`
	Seed          json.RawMessage `json:"seed"`
	InputRef      []duomiRefItem  `json:"input_reference"`
	SourceTaskID  json.RawMessage `json:"source_task_id"`

	// ---------------------------------------------------------------- 图像 / 音乐（B6-1）
	//
	// ImageURLs 是 nano 编辑端点（/api/gemini/nano-banana-edit）的参考图；
	// 上游只收公网链接，禁 data:。有图走编辑端点、无图走文生图端点。
	ImageURLs []string `json:"image_urls"`
	// Image 是 gpt-image 系的上游入参形态：**既可能是字符串也可能是数组**，
	// 因此保留原始 JSON，由适配器按形态校验后原样下发（擅自转成数组会让只认字符串的实现 400）。
	Image json.RawMessage `json:"image"`
	// N 是生成张数。图像任务固定 1（多图靠多次任务，见 §2.9 B6-2 的计费口径）。
	N json.RawMessage `json:"n"`
	// ---------------------------------------------------------------- 音乐（SUNO）
	//
	// Lyrics 是自定义歌词；与 Instrumental=true 互斥（上游会直接拒）。
	Lyrics string `json:"lyrics"`
	// Instrumental 对应上游 make_instrumental：true 表示纯音乐、不需要歌词。
	Instrumental *bool `json:"instrumental"`
	// Tags / Title 只在 custom_mode 下有意义（SUNO 的自定义歌词模式）。
	Tags  string `json:"tags"`
	Title string `json:"title"`
	// CustomMode 允许调用方显式覆盖渠道默认的自定义模式。
	CustomMode *bool `json:"custom_mode"`

	// Content 是火山方舟的**原生报文结构**（多模态素材数组）。
	//
	// 它出现在这里只为了一件事：明确拒收。经多米转发的模型没有一条认这个字段，
	// 上游会把它当未知键丢掉 —— 用户以为传了参考图，成片里却没有，
	// 而账单照收。按「不伪造能力」原则直接 400 并告诉他该用哪个字段。
	Content json.RawMessage `json:"content"`

	// ---------------------------------------------------------------- 可灵运镜
	//
	// CameraControl 是**单方向**运镜（6 个轴里只能一个非零，官方契约），
	// 用指针而不是值类型：`camera_control: null` 与「没传」语义相同，
	// 值类型会把两者都变成「六轴全 0」——那是一个非法请求，上游直接 400。
	CameraControl *duomiCameraControl `json:"camera_control"`

	// raw 是**解析前的原始报文**，由 Forward 在 Unmarshal 后一处赋值。
	//
	// 它的唯一用途是 ForwardUnmapped：把本结构体没声明的顶层键原样补进上游报文。
	// 挂在 body 上而不是层层传参，是因为出站组装点有六处（nano / gpt-image /
	// kling / seedance / suno / 同步桥），每处多一个参数就多一个漏传的机会 ——
	// 漏了不会编译错，只会静默丢字段。
	raw []byte
}

// duomiCameraControl 是可灵的运镜参数（image2video / omni 支持）。
//
// 六个轴各 [-10,10]，**一次只能选一个方向**：官方把「同时推近又平移」定义为
// 非法输入，不是"组合运镜"。平台照实校验，不替用户挑一个（静默丢弃会让
// 用户以为运镜生效，成片却一动不动）。
type duomiCameraControl struct {
	Horizontal float64 `json:"horizontal"`
	Vertical   float64 `json:"vertical"`
	Pan        float64 `json:"pan"`
	Tilt       float64 `json:"tilt"`
	Roll       float64 `json:"roll"`
	Zoom       float64 `json:"zoom"`
}

// duomiCameraOut 是**出站**形状：可灵契约要 `{type, config:{六轴}}`，两层都是必填。
//
// 为什么不能直接把六轴平铺下发（这是真机联调前最容易被漏掉的一处）：
// 可灵/多米的请求结构里 `type` 与 `config` 都是 required，六轴平铺等于两个必填键都缺，
// 上游回的是"参数不合法"——报错里没有「运镜」二字，会被当渠道故障熔断一条健康渠道。
// 对外契约仍是平铺六轴（读者不用理解这层包装），由这里翻译一次。
type duomiCameraOut struct {
	// Type 固定 simple = 按 config 里的六轴自定义运镜（官方枚举里的自定义档）。
	Type   string              `json:"type"`
	Config *duomiCameraControl `json:"config"`
}

// axes 按固定顺序返回非零轴（顺序只影响错误文案，不影响校验结果）。
func (c *duomiCameraControl) axes() []struct {
	Name string
	Val  float64
} {
	if c == nil {
		return nil
	}
	all := []struct {
		Name string
		Val  float64
	}{
		{"horizontal", c.Horizontal},
		{"vertical", c.Vertical},
		{"pan", c.Pan},
		{"tilt", c.Tilt},
		{"roll", c.Roll},
		{"zoom", c.Zoom},
	}
	out := make([]struct {
		Name string
		Val  float64
	}, 0, 6)
	for _, a := range all {
		if a.Val != 0 {
			out = append(out, a)
		}
	}
	return out
}

// duomiRefItem 是 input_reference 的一项。
type duomiRefItem struct {
	Type     string `json:"type"`
	ImageURL string `json:"image_url"`
	AudioURL string `json:"audio_url"`
	VideoURL string `json:"video_url"`
}

// duomiRefs 是素材按语义分类后的视图（顺序保持请求顺序）。
type duomiRefs struct {
	firstFrame string
	lastFrame  string
	images     []string
	audios     []string
	videos     []string
}

// imageCount 返回图像素材总数（首帧/尾帧也算图，可灵按"有几张图"选协议路径）。
func (r *duomiRefs) imageCount() int {
	n := len(r.images)
	if r.firstFrame != "" {
		n++
	}
	if r.lastFrame != "" {
		n++
	}
	return n
}

// allImages 按「首帧 → 普通图 → 尾帧」的稳定顺序返回全部图像。
func (r *duomiRefs) allImages() []string {
	out := make([]string, 0, r.imageCount())
	if r.firstFrame != "" {
		out = append(out, r.firstFrame)
	}
	out = append(out, r.images...)
	if r.lastFrame != "" {
		out = append(out, r.lastFrame)
	}
	return out
}

// classifyDuomiRefs 解析素材并做本地拒绝（data: 内联 / 缺链接 / 超量）。
//
// data: 一律拒绝：多米的各家上游（Seedance / 可灵 / Gemini）都要公网 URL，
// base64 内联必然 400；而且内联体不落库，重试时会静默丢素材。
func classifyDuomiRefs(items []duomiRefItem) (*duomiRefs, error) {
	out := &duomiRefs{}
	for i, it := range items {
		typ := strings.ToLower(strings.TrimSpace(it.Type))
		link := ""
		switch typ {
		case "first_frame", "first-frame":
			typ = "first_frame"
			link = it.ImageURL
		case "last_frame", "last-frame":
			typ = "last_frame"
			link = it.ImageURL
		case "image", "reference_image", "":
			typ = "image"
			link = it.ImageURL
		case "audio", "reference_audio":
			typ = "audio"
			link = it.AudioURL
		case "video", "reference_video":
			typ = "video"
			link = it.VideoURL
		default:
			return nil, &relay.CallerParamError{Msg: fmt.Sprintf("第 %d 个素材的 type %q 不支持（可用：image / first_frame / last_frame / audio / video）", i+1, it.Type)}
		}
		link = strings.TrimSpace(link)
		if link == "" {
			return nil, &relay.CallerParamError{Msg: fmt.Sprintf("第 %d 个素材（%s）缺少链接", i+1, typ)}
		}
		if strings.HasPrefix(strings.ToLower(link), "data:") {
			return nil, &relay.CallerParamError{Msg: "多米API 不支持 data: 内联素材，请先用 /v1/files 上传后改用公网直链"}
		}
		switch typ {
		case "first_frame":
			if out.firstFrame != "" {
				return nil, &relay.CallerParamError{Msg: "首帧只能有一个"}
			}
			out.firstFrame = link
		case "last_frame":
			if out.lastFrame != "" {
				return nil, &relay.CallerParamError{Msg: "尾帧只能有一个"}
			}
			out.lastFrame = link
		case "image":
			out.images = append(out.images, link)
		case "audio":
			out.audios = append(out.audios, link)
		case "video":
			out.videos = append(out.videos, link)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- HTTP

// duomiDo 发起一次请求并返回响应体；HTTP 非 2xx 按状态码分档归因。
func duomiDo(ctx context.Context, cfg *relay.ChannelConfig, authPrefix, method, path string, payload any) ([]byte, error) {
	_, buf, err := duomiDoRaw(ctx, cfg, authPrefix, method, path, payload)
	return buf, err
}

// duomiDoRaw 与 duomiDo 相同，但同时返回响应（探活需要看状态码）。
func duomiDoRaw(ctx context.Context, cfg *relay.ChannelConfig, authPrefix, method, path string, payload any) (*http.Response, []byte, error) {
	target, err := relay.BuildRawURL(cfg, path)
	if err != nil {
		return nil, nil, err
	}
	var rdr io.Reader
	if payload != nil {
		b, merr := json.Marshal(payload)
		if merr != nil {
			return nil, nil, fmt.Errorf("适配器 duomiapi：序列化请求体失败：%w", merr)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rdr)
	if err != nil {
		return nil, nil, fmt.Errorf("适配器 duomiapi：构造请求失败：%w", err)
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key := strings.TrimSpace(cfg.APIKey); key != "" && !strings.ContainsAny(key, "\r\n") {
		req.Header.Set("Authorization", authPrefix+key)
	}

	resp, err := client(cfg).Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("上游请求失败：%w", err)
	}
	defer resp.Body.Close()
	buf, err := readUpstreamBody(resp.Body, maxUpstreamBodyBytes)
	if err != nil {
		return resp, nil, fmt.Errorf("读取上游响应失败：%w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, buf, duomiHTTPError(resp.StatusCode, buf)
	}
	return resp, buf, nil
}

// duomiDoMultipart 以 multipart/form-data 提交（SUNO 的生成接口只收表单，不收 JSON）。
//
// fields 用有序切片而不是 map：SUNO 侧对字段顺序敏感（continue_* 必须跟在 mv 之后），
// map 的随机迭代顺序会让同一份请求时好时坏，是最难复现的一类故障。
func duomiDoMultipart(ctx context.Context, cfg *relay.ChannelConfig, authPrefix, path string, fields [][2]string) ([]byte, error) {
	target, err := relay.BuildRawURL(cfg, path)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, kv := range fields {
		if werr := mw.WriteField(kv[0], kv[1]); werr != nil {
			return nil, fmt.Errorf("适配器 duomiapi：构造表单失败：%w", werr)
		}
	}
	if cerr := mw.Close(); cerr != nil {
		return nil, fmt.Errorf("适配器 duomiapi：构造表单失败：%w", cerr)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, &buf)
	if err != nil {
		return nil, fmt.Errorf("适配器 duomiapi：构造请求失败：%w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if key := strings.TrimSpace(cfg.APIKey); key != "" && !strings.ContainsAny(key, "\r\n") {
		req.Header.Set("Authorization", authPrefix+key)
	}
	resp, err := client(cfg).Do(req)
	if err != nil {
		return nil, fmt.Errorf("上游请求失败：%w", err)
	}
	defer resp.Body.Close()
	body, rerr := readUpstreamBody(resp.Body, maxUpstreamBodyBytes)
	if rerr != nil {
		return nil, fmt.Errorf("读取上游响应失败：%w", rerr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return body, duomiHTTPError(resp.StatusCode, body)
	}
	return body, nil
}

// ---------------------------------------------------------------- 信封

// duomiBusinessError 把业务码映射成与 HTTP 分档一致的错误。
//
// code 语义按上游习惯：401/403/402/404/429 与 HTTP 同义；其余（如可灵的 1201/1203）
// 视为参数或业务拒绝——可灵的 1201 是「参数错误」、1203 是「任务不存在」，
// 都不是渠道故障，报成渠道失败会污染健康度。
//
// 401/403/429 **必须**包成 HTTPStatusError：多米把这类失败放在 HTTP 200 的信封里，
// 只回裸 error 的话 relay 拿不到状态码 → 一律记成「上游故障」→ 一枚坏 key 能把
// 整条渠道打成禁用，而且该 key 永远不冷却（每次请求继续先选它）。
// 402 刻意保持裸 error：欠费是渠道级问题，需要累计失败把渠道熔断下线，
// 而不是只冷却某一枚 key（与 duomiHTTPError 的 402 分支同一理由）。
func duomiBusinessError(code int, msg string) error {
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden:
		// Note 保留改造前的文案（管理台渠道测试与用户可见 502 文案会逐字展示它）。
		return &relay.HTTPStatusError{Code: code,
			Note: fmt.Sprintf("适配器 duomiapi：鉴权失败（业务码 %d），请检查渠道密钥：%s", code, msg)}
	case http.StatusPaymentRequired:
		return fmt.Errorf("适配器 duomiapi：上游余额不足（业务码 %d），请充值后重试：%s", code, msg)
	case http.StatusTooManyRequests:
		return &relay.HTTPStatusError{Code: http.StatusTooManyRequests,
			Note: fmt.Sprintf("适配器 duomiapi：上游限流（业务码 %d）：%s", code, msg)}
	case 0:
		return &relay.CallerParamError{Msg: "多米API 拒绝了本次参数：" + msg}
	}
	return &relay.CallerParamError{Msg: fmt.Sprintf("多米API 拒绝了本次参数（业务码 %d）：%s", code, msg)}
}

// duomiHTTPError 按状态码分档归因（与 metaso 同口径）。
func duomiHTTPError(status int, buf []byte) error {
	// 优先取信封里的那一句（用户能读懂），原文只在取不到时兜底。
	detail := upstreamDetail(buf, status)
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return &relay.CallerParamError{Msg: "多米API 拒绝了本次参数（HTTP " + strconv.Itoa(status) + "）：" + detail}
	case http.StatusUnauthorized, http.StatusForbidden:
		return &relay.HTTPStatusError{Code: status, Note: "适配器 duomiapi：鉴权失败（HTTP " + strconv.Itoa(status) + "），请检查渠道密钥与 auth_prefix：" + detail}
	case http.StatusPaymentRequired:
		// 与 metaso 同口径：402 保持普通错误 → 记为上游故障，欠费渠道会被熔断下线，
		// 而不是永远留在候选里让每个请求都白撞一次。
		return fmt.Errorf("适配器 duomiapi：上游余额不足（HTTP 402），请充值后重试：%s", detail)
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return &relay.HTTPStatusError{Code: status, Note: "适配器 duomiapi：接口路径不存在（HTTP " + strconv.Itoa(status) + "），请检查 Base URL 与渠道 paths 配置：" + detail}
	case http.StatusTooManyRequests:
		return &relay.HTTPStatusError{Code: status, Note: "适配器 duomiapi：上游限流（HTTP 429）：" + detail}
	default:
		return relay.StatusError(status, detail)
	}
}

// duomiIntCode 解析业务码（数字或数字字符串），解析失败返回 0。
func duomiIntCode(raw json.RawMessage) int {
	v, _ := duomiIntCodeOK(raw)
	return v
}

// duomiIntCodeOK 解析业务码并报告是否存在。
func duomiIntCodeOK(raw json.RawMessage) (int, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, false
	}
	var n int
	if json.Unmarshal(raw, &n) == nil {
		return n, true
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		if v, err := strconv.Atoi(strings.TrimSpace(str)); err == nil {
			return v, true
		}
	}
	return 0, false
}

// ---------------------------------------------------------------- 任务号与状态

// duomiExtractTaskID 从提交响应里提取任务号，覆盖四种形态：
// 字符串（{id:"x"}）、对象（{data:{task_id:"x"}}）、数组（{data:[{task_id:"x"}]}）、
// 裸字符串（{data:"x"} —— SUNO 歌词接口就是这样回的）。
//
// 四种都见过，只认一种会在换模型后静默失败（200 但取不到 ID → 任务空转到 deadline）。
func duomiExtractTaskID(buf []byte) (string, error) {
	var p struct {
		ID     string          `json:"id"`
		TaskID string          `json:"task_id"`
		Data   json.RawMessage `json:"data"`
		Task   json.RawMessage `json:"task"`
	}
	if err := json.Unmarshal(buf, &p); err != nil {
		return "", fmt.Errorf("适配器 duomiapi：解析提交响应失败：%w", err)
	}
	candidates := []string{p.TaskID, p.ID}
	for _, raw := range []json.RawMessage{p.Data, p.Task} {
		candidates = append(candidates, duomiTaskIDFromRaw(raw)...)
	}
	for _, c := range candidates {
		id := strings.TrimSpace(c)
		if id == "" {
			continue
		}
		if len(id) > duomiMaxTaskIDLen || !duomiTaskIDPattern.MatchString(id) {
			return "", fmt.Errorf("适配器 duomiapi：上游返回的 task_id 格式不合法：%s", snippet([]byte(id)))
		}
		return id, nil
	}
	// 200 但没有任务号必须判失败：当成成功会让任务空转到 deadline（用户白等，还要退费）。
	return "", fmt.Errorf("适配器 duomiapi：提交响应缺少 task_id：%s", snippet(buf))
}

// duomiTaskIDFromRaw 从对象/数组/裸字符串形态里取 task_id（形态不匹配返回空切片，不是错误）。
//
// 裸字符串分支是 B6-1 补的：SUNO 歌词接口返回 `{"code":200,"data":"<taskid>"}`，
// 少了这条会被判成「提交响应缺少 task_id」→ 歌词任务永远提交不成功。
func duomiTaskIDFromRaw(raw json.RawMessage) []string {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return []string{str}
	}
	var obj struct {
		TaskID string `json:"task_id"`
		ID     string `json:"id"`
	}
	if json.Unmarshal(raw, &obj) == nil && (obj.TaskID != "" || obj.ID != "") {
		return []string{obj.TaskID, obj.ID}
	}
	var arr []struct {
		TaskID string `json:"task_id"`
		ID     string `json:"id"`
	}
	if json.Unmarshal(raw, &arr) == nil {
		out := make([]string, 0, len(arr)*2)
		for _, it := range arr {
			out = append(out, it.TaskID, it.ID)
		}
		return out
	}
	return nil
}

// duomiNormalizeStatus 把上游状态归一为平台状态词（succeeded/failed/canceled/queued/running）。
//
// 必须归一到底：worker 的 normalizeUpstreamStatus 只认特定写法，把上游原文（可灵的 "succeed"、
// nano 的数字 "3"）直接透出会落到 running 一直等到 deadline 退费，已生成好的产物被丢掉。
func duomiNormalizeStatus(raw string) string {
	// 数字码是多米自己的约定（"1" 排队 / "2" 失败 / "3" 成功），只在这里映射；
	// 平台级别名表故意不收数字 —— 各家数字含义不同，收进去必然误判别家。
	switch strings.TrimSpace(raw) {
	case "1":
		return taskstate.Queued
	case "2":
		return taskstate.Failed
	case "3":
		return taskstate.Succeeded
	}
	if st, ok := taskstate.Normalize(raw); ok {
		return st
	}
	return taskstate.Running
}

// duomiStrings 收集非空字符串（保序去空）。
func duomiStrings(vals ...string) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// duomiSeconds 取本次请求要下发的秒数：平台归一值优先，其次 body 里的 seconds / duration。
//
// 为什么不能只读 body：
//   - 客户端可能只写 duration、不写 seconds（平台字段两套写法）；
//   - 未映射键透传后，body 里可能同时存在两个时长键。
//
// 只看一个字段必然丢时长；丢了时长上游就按自己的默认值出片，而平台仍按请求秒数
// 计费（少收或多收），且账单表面完全正常。
//
// 负数归零、超上限饱和的钳位必须保留：`-1` 是「自动」的标记，下发给不认它的上游
// 就是 400。
func duomiSeconds(body *duomiBody, req *relay.TaskSubmitRequest) int64 {
	if req != nil {
		if req.AutoDuration || req.OmitDuration {
			return 0 // 明确要求自动：不下发任何时长字段
		}
		if req.Seconds > 0 {
			return clampDuomiSeconds(req.Seconds)
		}
	}
	for _, raw := range []json.RawMessage{body.Seconds, body.Duration} {
		if n := duomiSecondValue(raw); n > 0 {
			return clampDuomiSeconds(n)
		}
	}
	return 0
}

// duomiSecondValue 解析单个时长值（JSON 数字或数字字符串）；缺失/非法返回 -1。
func duomiSecondValue(raw json.RawMessage) int64 {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return -1
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		if v, err := strconv.ParseInt(strings.TrimSpace(str), 10, 64); err == nil {
			return v
		}
	}
	return -1
}

// clampDuomiSeconds 把秒数夹到上游能接受的区间（负数/0 视为「不指定」）。
func clampDuomiSeconds(n int64) int64 {
	if n < 0 {
		return 0
	}
	if n > 3600 {
		return 3600
	}
	return n
}

// duomiResolutionValue 取请求里的分辨率写法并**原样**下发。
//
// 曾经这里把 `WxH` 归一成平台档位键（1920x1080 → 1080p）。那等于替用户改参数：
// 用户按文档要的是精确像素，平台却按自己的档位表猜一个枚举值发过去，
// 猜错的后果是「按 A 档收钱、按 B 档出图」，而账单上看不出来。
// 现在只 trim：写法对不对由上游判断（不认会明确报错），计费仍按用户写的档位走。
func duomiResolutionValue(v string) string {
	return strings.TrimSpace(v)
}

// duomiAdapterID 是本适配器的渠道类型标识（与 DuoMi.Name() 同值）。
//
// 单独一个常量是为了让「方言解析」不必构造适配器实例：尺寸方言的键是
// (适配器 ID, 上游模型名)，而适配器只在 Forward 里才存在。
const duomiAdapterID = "duomiapi"

// isJSONAbsent 判断一个原始 JSON 字段是否"没传"（长度 0 或字面量 null）。
//
// 只判这两种：`{}` / `[]` / `""` 都是**用户明确传了**的，拒收逻辑必须照常触发
// —— 把空数组当成没传，会让 `"content": []` 静默通过，而它同样是一份
// 当前模型不认识的报文。
func isJSONAbsent(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null"
}
