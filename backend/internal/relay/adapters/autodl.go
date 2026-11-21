package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"lingdong-api/internal/pkg/taskstate"
	"lingdong-api/internal/relay"
)

// AutoDL 适配器：把一个对外模型 ID 映射到 4 个 ComfyUI 工作流（多模态 / 图生 / 文生 / 首尾帧）。
//
// 设计要点（改动前先读）：
//  1. 上游协议里没有 "model" 概念，**上游模型 = workflow_id**，只能来自渠道配置
//     （channels.adapter_options）。因此 TaskSubmitRequest.UpstreamModel 一律忽略——
//     选路会把模型绑定里的 upstream_model 回填进来（空则回落到对外模型名），
//     拿它当 workflow 用就是把「autodl-video」拼进 URL。
//  2. 上游画质只有 480p/768p（分竖/横/方），平台计费档是 480p/720p/1080p。
//     口径：768p 计入平台 720p 档；未映射的档位一律拒绝，**绝不静默降档**
//     （降档会让冻结的额度与真实产能不符）。
//  3. 参数越界在本地就判定并返回 relay.CallerParamError：任务服务据此短路候选循环、
//     直接给调用方 400，而不是打满所有渠道后返回「上游提交失败」。
type AutoDL struct{}

const (
	autodlSubmitPath = "/api/v1/comfyui/comfyui_workflow/"
	autodlResultPath = "/api/v1/comfyui/comfyui_workflow/result/"

	autodlMaxOptionsBytes = 4096
	autodlMaxPromptRunes  = 20000
	autodlMinSeconds      = 1
	autodlMaxSeconds      = 15
	autodlMaxImages       = 9
	autodlMaxAudios       = 3
	autodlMaxTaskIDLen    = 128
)

// autodlMode 是四种生成模式。
type autodlMode string

const (
	modeMultimodal autodlMode = "multimodal"
	modeImage      autodlMode = "image"
	modeText       autodlMode = "text"
	modeFirstLast  autodlMode = "first_last"
)

// autodlModes 固定遍历顺序（同时也是「workflows 非空即全量」的判据）。
var autodlModes = []autodlMode{modeMultimodal, modeImage, modeText, modeFirstLast}

// autodlModeLabel 用于错误文案，避免把内部枚举名直接甩给用户。
var autodlModeLabel = map[autodlMode]string{
	modeMultimodal: "多模态（图+音）",
	modeImage:      "图片参考",
	modeText:       "纯文本",
	modeFirstLast:  "首尾帧",
}

// autodlDefaultWorkflows 是未配置时的上游工作流（与官方文档给出的 4 个 ID 一致）。
var autodlDefaultWorkflows = map[autodlMode]string{
	modeMultimodal: "minimax_h3_zm_u24",
	modeImage:      "minimax_h3_lightx2v_v5_15s",
	modeText:       "minimax_h3_lightx2v_no_pic",
	modeFirstLast:  "minimax_h3_lightx2v",
}

// autodlDefaultResolution 是 平台档位 → 上游画质基名（"" = 不支持该档）。
var autodlDefaultResolution = map[string]string{
	"480p":  "480p",
	"720p":  "768p", // 口径：768p 计入平台 720p 档
	"1080p": "",     // 上游最高 768p：未映射即不支持（禁止静默降档）
}

// autodlWorkflowPattern 限定 workflow_id 的字符集：既挡住拼错的配置，也让 URL 拼接无需转义。
var autodlWorkflowPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

var errAutoDLNoEndpoint = errors.New("适配器 autodl：不支持该端点（仅提供异步视频任务）")

func init() {
	// 注册失败只可能是命名冲突（编码错误），与 openai 适配器保持同样处理。
	_ = relay.RegisterAdapter(AutoDL{})
}

// Name 返回适配器标识（对齐 channels.adapter_id）。
func (AutoDL) Name() string { return "autodl" }

// Caps 返回能力子集：本适配器只做视频异步任务。
func (AutoDL) Caps() []string { return []string{"video"} }

// SupportsDeclarativeConfig 显式声明不支持渠道级声明式配置。
//
// 鉴权头是裸 token、路径由 workflow 决定，声明式改写对它没有意义；
// 显式返回 false 而不是靠「没实现接口」隐式成立，避免以后有人以为它支持。
func (AutoDL) SupportsDeclarativeConfig() bool { return false }

// SupportsEndpoint 声明不承接任何同步端点。
//
// 必须实现且全 false：不实现该接口等于「全支持」，同步网关会把 chat 请求派给视频适配器。
func (AutoDL) SupportsEndpoint(relay.EndpointKind) bool { return false }

// AsyncEndpoints：AutoDL 只承接视频异步任务（图像/音乐没有上游契约）。
func (AutoDL) AsyncEndpoints() []string { return []string{"video/generations"} }

// ---------------------------------------------------------------- 渠道私有配置

// autodlOptions 是 channels.adapter_options 归一后的结果。
type autodlOptions struct {
	workflows  map[autodlMode]string
	resolution map[string]string // 平台档位 → 上游基名（"" = 不支持）
}

func autodlCfgErr(field, msg string) error {
	return &relay.ConfigError{Scope: "渠道适配器配置错误", Field: field, Msg: msg}
}

// parseAutoDLOptions 是**唯一**的配置解析入口：保存期校验与运行期取配置共用它，
// 避免「保存时能过、跑起来是别的值」。
//
// 空（nil/""/null/{}）= 全部使用内置默认。
func parseAutoDLOptions(raw []byte) (*autodlOptions, error) {
	opts := &autodlOptions{
		workflows:  make(map[autodlMode]string, len(autodlDefaultWorkflows)),
		resolution: make(map[string]string, len(autodlDefaultResolution)),
	}
	for k, v := range autodlDefaultWorkflows {
		opts.workflows[k] = v
	}
	for k, v := range autodlDefaultResolution {
		opts.resolution[k] = v
	}

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" || string(trimmed) == "{}" {
		return opts, nil
	}
	if len(trimmed) > autodlMaxOptionsBytes {
		return nil, autodlCfgErr("", fmt.Sprintf("配置过大（%d 字节，上限 %d）", len(trimmed), autodlMaxOptionsBytes))
	}

	var doc struct {
		Workflows     map[string]string `json:"workflows"`
		ResolutionMap map[string]string `json:"resolution_map"`
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, autodlCfgErr("", "不是合法的配置对象："+err.Error())
	}
	// 尾部多余内容（两个对象拼在一起）会让「保存时报错、运行期只读到第一个」，必须拦。
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, autodlCfgErr("", "只能包含一个 JSON 对象")
	}

	// workflows：**非空即全量**。只填两个、另外两个静默用默认，是运营最难发现的配置错误。
	if len(doc.Workflows) > 0 {
		seen := map[string]bool{}
		for _, m := range autodlModes {
			v, ok := doc.Workflows[string(m)]
			if !ok {
				return nil, autodlCfgErr("workflows."+string(m),
					fmt.Sprintf("四项工作流必须全部填写（缺少 %s）", autodlModeLabel[m]))
			}
			v = strings.TrimSpace(v)
			if !autodlWorkflowPattern.MatchString(v) {
				return nil, autodlCfgErr("workflows."+string(m), "工作流 ID 只能是 1-64 位字母/数字/下划线/连字符")
			}
			if seen[v] {
				return nil, autodlCfgErr("workflows."+string(m),
					"四个模式不能共用同一个工作流（同一 ID 服务两种模式必然有一种跑错）")
			}
			seen[v] = true
			opts.workflows[m] = v
		}
		for k := range doc.Workflows {
			found := false
			for _, m := range autodlModes {
				if k == string(m) {
					found = true
					break
				}
			}
			if !found {
				return nil, autodlCfgErr("workflows."+k, "未知的模式键（只支持 multimodal/image/text/first_last）")
			}
		}
	}

	for k, v := range doc.ResolutionMap {
		if _, ok := autodlDefaultResolution[k]; !ok {
			return nil, autodlCfgErr("resolution_map."+k, "只支持 480p/720p/1080p 三个档位")
		}
		v = strings.TrimSpace(v)
		if !autodlUpstreamSet[v] {
			return nil, autodlCfgErr("resolution_map."+k, "取值只能是空（不支持）/480p/768p/1080p")
		}
		opts.resolution[k] = v
	}
	return opts, nil
}

// ValidateAdapterOptions 供管理台保存期校验（relay.OptionsSupporter）。
func (AutoDL) ValidateAdapterOptions(raw []byte) error {
	_, err := parseAutoDLOptions(raw)
	return err
}

// ---------------------------------------------------------------- 请求解析与组装

// autodlBody 是本适配器关心的请求字段（其余字段忽略，不参与组装）。
type autodlBody struct {
	Prompt     string          `json:"prompt"`
	Seconds    json.RawMessage `json:"seconds"`
	Resolution string          `json:"resolution"`
	Size       string          `json:"size"`
	Aspect     string          `json:"aspect_ratio"`
	Seed       json.RawMessage `json:"seed"`
	Metadata   map[string]any  `json:"metadata"`
	InputRef   []struct {
		Type     string `json:"type"`
		ImageURL string `json:"image_url"`
		AudioURL string `json:"audio_url"`
		VideoURL string `json:"video_url"`
	} `json:"input_reference"`
}

// autodlRefs 是素材归类结果。
type autodlRefs struct {
	images     []string
	audios     []string
	firstFrame string
	lastFrame  string
	hasVideo   bool
}

// autodlPlan 是一次提交的最终产物。
type autodlPlan struct {
	mode     autodlMode
	workflow string
	body     map[string]any
}

// refTypeAlias 只接受两种书写风格，不做模糊匹配：
// 拼错一个字母就应当报错，而不是被当成普通参考图收钱后跑错模式。
var refTypeAlias = map[string]string{
	"image":       "image",
	"audio":       "audio",
	"video":       "video",
	"first_frame": "first_frame",
	"last_frame":  "last_frame",
	"first-frame": "first_frame",
	"last-frame":  "last_frame",
}

// classifyRefs 把 input_reference 归类；任何不认识/不合法的形态都直接报参数错。
func classifyRefs(body *autodlBody) (*autodlRefs, error) {
	out := &autodlRefs{}
	for i, r := range body.InputRef {
		typ := strings.ToLower(strings.TrimSpace(r.Type))
		image := strings.TrimSpace(r.ImageURL)
		audio := strings.TrimSpace(r.AudioURL)
		video := strings.TrimSpace(r.VideoURL)

		// type 缺省时按「哪个链接非空」推断（与 task.refKind 的推断方向一致）。
		if typ == "" {
			switch {
			case image != "":
				typ = "image"
			case audio != "":
				typ = "audio"
			case video != "":
				typ = "video"
			default:
				return nil, &relay.CallerParamError{Msg: fmt.Sprintf("第 %d 个素材既没有 type 也没有链接", i+1)}
			}
		}
		kind, ok := refTypeAlias[typ]
		if !ok {
			return nil, &relay.CallerParamError{
				Msg: fmt.Sprintf("不支持的素材类型「%s」：本模型只接受 image/audio/first_frame/last_frame", r.Type),
			}
		}
		switch kind {
		case "image":
			if image == "" {
				return nil, &relay.CallerParamError{Msg: fmt.Sprintf("第 %d 个图片素材缺少 image_url", i+1)}
			}
			out.images = append(out.images, image)
		case "audio":
			if audio == "" {
				return nil, &relay.CallerParamError{Msg: fmt.Sprintf("第 %d 个音频素材缺少 audio_url", i+1)}
			}
			out.audios = append(out.audios, audio)
		case "video":
			if video == "" {
				return nil, &relay.CallerParamError{Msg: fmt.Sprintf("第 %d 个视频素材缺少 video_url", i+1)}
			}
			out.hasVideo = true
		case "first_frame":
			if image == "" {
				return nil, &relay.CallerParamError{Msg: "首帧素材缺少 image_url"}
			}
			if out.firstFrame != "" {
				return nil, &relay.CallerParamError{Msg: "首帧只能有 1 个"}
			}
			out.firstFrame = image
		case "last_frame":
			if image == "" {
				return nil, &relay.CallerParamError{Msg: "尾帧素材缺少 image_url"}
			}
			if out.lastFrame != "" {
				return nil, &relay.CallerParamError{Msg: "尾帧只能有 1 个"}
			}
			out.lastFrame = image
		}
	}
	// data: 内联素材：上游只接受公网 http(s) 直链，逐个放行等于到上游才失败。
	// 一次把所有位置都报出来，省得用户改一个再撞下一个。
	if pos := firstInlineRef(body); pos != "" {
		return nil, &relay.CallerParamError{
			Msg: "本模型只接受公网 http(s) 素材直链，不支持内联素材（" + pos +
				"）：请先调用 POST /v1/files 上传，再把返回的直链填入",
		}
	}
	return out, nil
}

// firstInlineRef 返回第一个内联素材的位置描述（空 = 没有内联素材）。
func firstInlineRef(body *autodlBody) string {
	for i, r := range body.InputRef {
		for _, link := range []string{r.ImageURL, r.AudioURL, r.VideoURL} {
			if strings.HasPrefix(strings.TrimSpace(link), "data:") {
				return "第 " + strconv.Itoa(i+1) + " 个素材"
			}
		}
	}
	return ""
}

// planAutoDLRequest 做模式识别 + 参数翻译 + 分辨率映射，产出可直接提交的请求体。
// 所有失败都是 relay.CallerParamError（调用方问题），配置问题在调用方之前就已拦掉。
func planAutoDLRequest(opts *autodlOptions, body *autodlBody) (*autodlPlan, error) {
	refs, err := classifyRefs(body)
	if err != nil {
		return nil, err
	}
	if refs.hasVideo {
		return nil, &relay.CallerParamError{Msg: "本模型不支持视频参考素材（只支持图片/音频/首尾帧）"}
	}

	prompt := body.Prompt
	if strings.TrimSpace(prompt) == "" {
		return nil, &relay.CallerParamError{Msg: "prompt 不能为空"}
	}
	if n := utf8.RuneCountInString(prompt); n > autodlMaxPromptRunes {
		return nil, &relay.CallerParamError{Msg: fmt.Sprintf("prompt 过长（%d 字，上限 %d 字）", n, autodlMaxPromptRunes)}
	}

	secs, err := autodlSeconds(body.Seconds)
	if err != nil {
		return nil, err
	}

	tier := tierOf(body.Resolution, body.Size)
	base, ok := opts.resolution[tier]
	if !ok {
		return nil, &relay.CallerParamError{
			Msg: fmt.Sprintf("不支持的分辨率「%s」：请传 480p 或 720p", firstNonEmptyStr(body.Resolution, body.Size)),
		}
	}
	if base == "" {
		return nil, &relay.CallerParamError{
			Msg: fmt.Sprintf("本模型不支持 %s 档（最高支持 720p）", tier),
		}
	}
	orient, err := orientationOf(body)
	if err != nil {
		return nil, err
	}
	resolution := base + autodlOrientationSuffix(orient)

	mode := modeText
	switch {
	case refs.firstFrame != "" || refs.lastFrame != "":
		mode = modeFirstLast
	case len(refs.audios) > 0:
		mode = modeMultimodal
	case len(refs.images) > 0:
		mode = modeImage
	}

	switch mode {
	case modeFirstLast:
		if refs.firstFrame == "" || refs.lastFrame == "" {
			return nil, &relay.CallerParamError{
				Msg: "首尾帧模式必须同时提供 first_frame 与 last_frame；若只想用一张首帧参考，请改用 type=image",
			}
		}
		if len(refs.images) > 0 {
			return nil, &relay.CallerParamError{Msg: "首尾帧模式不能再附加参考图（该模式只有首帧与尾帧两个位置）"}
		}
		if len(refs.audios) > 0 {
			return nil, &relay.CallerParamError{Msg: "首尾帧模式不支持音频（需要配音请改用图片参考模式）"}
		}
	case modeMultimodal:
		if len(refs.images) == 0 {
			return nil, &relay.CallerParamError{Msg: "多模态模式至少需要 1 张参考图（input_reference 不能为空）"}
		}
		if len(refs.images) > autodlMaxImages {
			return nil, &relay.CallerParamError{Msg: fmt.Sprintf("参考图过多（%d 张，上限 %d 张）", len(refs.images), autodlMaxImages)}
		}
		if len(refs.audios) > autodlMaxAudios {
			return nil, &relay.CallerParamError{Msg: fmt.Sprintf("音频过多（%d 个，上限 %d 个）", len(refs.audios), autodlMaxAudios)}
		}
	case modeImage:
		if len(refs.images) > autodlMaxImages {
			return nil, &relay.CallerParamError{Msg: fmt.Sprintf("参考图过多（%d 张，上限 %d 张）", len(refs.images), autodlMaxImages)}
		}
	}

	// 组装：先写「权威字段」，再让 metadata 补空缺——顺序不能反，否则 metadata 能改写上游请求。
	out := map[string]any{
		"prompt":     prompt,
		"duration":   secs,
		"resolution": resolution,
	}
	switch mode {
	case modeFirstLast:
		out["first_frame"] = refs.firstFrame
		out["last_frame"] = refs.lastFrame
	case modeMultimodal:
		for i, u := range refs.images {
			out["ref_image_"+strconv.Itoa(i)] = u
		}
		for i, u := range refs.audios {
			out["ref_audio_"+strconv.Itoa(i)] = u
		}
	case modeImage:
		for i, u := range refs.images {
			out["ref_image_"+strconv.Itoa(i)] = u
		}
	}

	if seed, ok, err := autodlSeed(body.Seed, body.Metadata); err != nil {
		return nil, err
	} else if ok {
		out["seed"] = seed
	}
	// metadata 是白名单，不是透传（P2-04）。
	//
	// 改造前这里做「未知键原样塞进上游请求体」。AutoDL 是 workflow 网关：
	// 请求体里的任何一个键都可能是该 workflow 的参数名（模型名、步数、guidance…），
	// 也就是说调用方可以靠 metadata 直接改上游的产出与成本，而平台对此毫无感知。
	// 文档给定的输入字段只有 seed/prompt/duration/resolution/ref_image_*/ref_audio_*/
	// first_frame/last_frame，其中除 seed 外全部由适配器自己写（见 autodlReservedKeys）。
	if err := autodlCheckMetadata(body.Metadata); err != nil {
		return nil, err
	}

	return &autodlPlan{mode: mode, workflow: opts.workflows[mode], body: out}, nil
}

// autodlMetadataAllow 是 metadata 允许出现的全部键。
//
//   - seed：文档字段，但已由 autodlSeed 单独处理（顶层优先），这里放行是为了不误报；
//   - orientation：平台侧提示（portrait/landscape/square），用于在只给 aspect_ratio 时
//     推导 resolution，**不下发上游**。
//
// 除此之外一律 400：静默忽略会让用户以为参数生效了，透传则会改上游行为。
var autodlMetadataAllow = map[string]bool{
	"seed":        true,
	"orientation": true,
}

// autodlCheckMetadata 校验 metadata 只包含允许的键。
func autodlCheckMetadata(md map[string]any) error {
	if len(md) == 0 {
		return nil
	}
	bad := make([]string, 0, len(md))
	for k := range md {
		if autodlMetadataAllow[k] {
			continue
		}
		bad = append(bad, k)
	}
	if len(bad) == 0 {
		return nil
	}
	// 排序保证文案稳定（同一份请求每次报同一句，便于运营按提示改）。
	for i := 1; i < len(bad); i++ {
		for j := i; j > 0 && bad[j] < bad[j-1]; j-- {
			bad[j], bad[j-1] = bad[j-1], bad[j]
		}
	}
	return &relay.CallerParamError{Msg: fmt.Sprintf(
		"metadata 只支持 seed / orientation（不支持 %s）；其余参数请用标准字段（prompt / seconds / resolution / input_reference）",
		strings.Join(bad, " / "))}
}

// autodlReservedKeys 是「由适配器自己写、绝不允许 metadata 注入」的键。
//
// 不加这道闸，metadata.first_frame 就能塞一个没做过 SSRF 校验、也没计入素材费的链接，
// 等于绕开平台的素材管控。
var autodlReservedKeys = map[string]bool{
	"model": true, "prompt": true, "duration": true, "seconds": true,
	"resolution": true, "size": true, "seed": true, "metadata": true,
	"input_reference": true, "first_frame": true, "last_frame": true,
	"aspect_ratio": true, "orientation": true,
}

func autodlReservedKey(k string) bool {
	if autodlReservedKeys[k] {
		return true
	}
	return strings.HasPrefix(k, "ref_image_") || strings.HasPrefix(k, "ref_audio_")
}

// autodlSeconds 解析并校验时长（1–15 整数；越界即拒，绝不 clamp）。
//
// 不能 clamp 的原因：冻结额度是按请求秒数算的，clamp 之后「冻结 15 秒、上游跑 15 秒」
// 看似一致，实际是「用户要 30 秒，我们只生成 15 秒却按 15 秒收费」——用户拿到的东西
// 与描述不符，且平台无法自证。
func autodlSeconds(raw json.RawMessage) (int64, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, &relay.CallerParamError{Msg: "缺少 seconds（视频时长，1–15 秒）"}
	}
	v, err := strconv.ParseFloat(strings.Trim(strings.TrimSpace(s), `"`), 64)
	if err != nil {
		return 0, &relay.CallerParamError{Msg: "seconds 必须是数字"}
	}
	if v != float64(int64(v)) {
		return 0, &relay.CallerParamError{Msg: "seconds 必须是整数秒"}
	}
	n := int64(v)
	if n < autodlMinSeconds || n > autodlMaxSeconds {
		return 0, &relay.CallerParamError{
			Msg: fmt.Sprintf("时长需为 %d–%d 秒（当前 %d）", autodlMinSeconds, autodlMaxSeconds, n),
		}
	}
	return n, nil
}

// autodlSeed 取随机种子：顶层 seed 优先，其次 metadata.seed；必须是整数。
func autodlSeed(raw json.RawMessage, md map[string]any) (int64, bool, error) {
	s := strings.TrimSpace(string(raw))
	if s != "" && s != "null" {
		v, err := strconv.ParseFloat(strings.Trim(strings.TrimSpace(s), `"`), 64)
		if err != nil || v != float64(int64(v)) {
			return 0, false, &relay.CallerParamError{Msg: "seed 必须是整数"}
		}
		return int64(v), true, nil
	}
	v, ok := md["seed"]
	if !ok {
		return 0, false, nil
	}
	switch n := v.(type) {
	case float64:
		if n != float64(int64(n)) {
			return 0, false, &relay.CallerParamError{Msg: "seed 必须是整数"}
		}
		return int64(n), true, nil
	case int64:
		return n, true, nil
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false, &relay.CallerParamError{Msg: "seed 必须是整数"}
		}
		return i, true, nil
	default:
		return 0, false, &relay.CallerParamError{Msg: "seed 必须是整数"}
	}
}

// ---------------------------------------------------------------- 分辨率与朝向

// tierOf 把 resolution/size 归一为平台档位键。
//
// 规则必须与 service/task.normalizeVideoResolution 完全一致：两处不一致会出现
// 「按 480p 冻结、按 720p 生成」的错账。跨包无法复用（task 依赖 relay，反向导入成环），
// 因此复制一份并由 TestTierOfMatchesTask 交叉锁定。
func tierOf(resolution, size string) string {
	s := strings.ToLower(strings.TrimSpace(resolution))
	if s == "" {
		s = strings.ToLower(strings.TrimSpace(size))
	}
	if s == "" {
		return ""
	}
	switch s {
	case "480p", "720p", "1080p":
		return s
	case "768p":
		// 768p 是**上游档位名**（平台 720p 下发的就是它）。用户在官方文档里看到的
		// 就是这个写法，必须吸附到 720p，否则「平台 720p → 上游 768p」这条映射
		// 只对平台内部可见，对外是个单向门：写 720p 能成、写 768p 就 400。
		return "720p"
	}
	if w, h, ok := whOf(s); ok {
		short := w
		if h < short {
			short = h
		}
		switch {
		case short <= 480:
			return "480p"
		case short <= 720:
			return "720p"
		default:
			return "1080p"
		}
	}
	return s
}

// whOf 解析 "WxH" / "W*H" / "W×H"。
//
// 分隔符按 rune 宽度前移，不能写 i+1：× 是 3 字节，i+1 会切在字符中间
// （中文文档里 "720×1280" 是常见写法，必须能识别）。
func whOf(s string) (int, int, bool) {
	v := strings.ToLower(strings.TrimSpace(s))
	i := strings.IndexAny(v, "x*×")
	if i <= 0 {
		return 0, 0, false
	}
	_, size := utf8.DecodeRuneInString(v[i:])
	if size <= 0 || i+size >= len(v) {
		return 0, 0, false
	}
	w, e1 := strconv.Atoi(strings.TrimSpace(v[:i]))
	h, e2 := strconv.Atoi(strings.TrimSpace(v[i+size:]))
	if e1 != nil || e2 != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// orientationOf 判定画幅朝向。
//
// 优先级：显式宽高 > metadata.orientation > aspect_ratio > 默认竖。
// 显式数字排在最前：`720x1280` 比一个手填的 orientation 更具体，让它生效才不会自相矛盾。
func orientationOf(body *autodlBody) (string, error) {
	if w, h, ok := whOf(body.Resolution); ok {
		return orientationByWH(w, h), nil
	}
	if w, h, ok := whOf(body.Size); ok {
		return orientationByWH(w, h), nil
	}
	if v, ok := body.Metadata["orientation"]; ok {
		s := strings.ToLower(strings.TrimSpace(fmt.Sprint(v)))
		switch s {
		case "":
		case "portrait", "vertical", "竖":
			return "portrait", nil
		case "landscape", "horizontal", "横":
			return "landscape", nil
		case "square", "1:1", "方":
			return "square", nil
		default:
			return "", &relay.CallerParamError{Msg: "metadata.orientation 只能是 portrait/landscape/square"}
		}
	}
	aspect := body.Aspect
	if strings.TrimSpace(aspect) == "" {
		if v, ok := body.Metadata["aspect_ratio"]; ok {
			aspect = fmt.Sprint(v)
		}
	}
	// 缺省横向：文档对外承诺「不传 aspect_ratio 就是 16:9」，而这里原来默认 portrait，
	// 于是同一个请求体在秘塔上出横屏、在 AutoDL 上出竖屏（真机 D1 实测 480x864）。
	// 两通道的缺省必须一致，否则「文档说 16:9」在 flash 上是假的。
	if strings.TrimSpace(aspect) == "" {
		return "landscape", nil
	}
	i := strings.Index(aspect, ":")
	if i <= 0 || i >= len(aspect)-1 {
		return "", &relay.CallerParamError{Msg: fmt.Sprintf("aspect_ratio 需为 W:H 形式（当前 %q）", aspect)}
	}
	w, e1 := strconv.ParseFloat(strings.TrimSpace(aspect[:i]), 64)
	h, e2 := strconv.ParseFloat(strings.TrimSpace(aspect[i+1:]), 64)
	if e1 != nil || e2 != nil || w <= 0 || h <= 0 {
		return "", &relay.CallerParamError{Msg: fmt.Sprintf("aspect_ratio 需为 W:H 形式（当前 %q）", aspect)}
	}
	if w == h {
		return "square", nil
	}
	if w > h {
		return "landscape", nil
	}
	return "portrait", nil
}

func orientationByWH(w, h int) string {
	switch {
	case w > h:
		return "landscape"
	case w < h:
		return "portrait"
	default:
		return "square"
	}
}

// autodlOrientationSuffix 把朝向转成上游画质后缀（上游取值是中文后缀 + 方形括号）。
func autodlOrientationSuffix(orient string) string {
	switch orient {
	case "landscape":
		return "横"
	case "square":
		return "(1:1)"
	default:
		return "竖"
	}
}

// ---------------------------------------------------------------- 出站请求

// Test 用「查询一个必然不存在的任务」探测鉴权与可达性。
//
// 上游没有探活接口，只能这样探：2xx/400/422/429 说明「路由通 + 鉴权过」，
// 401/403 是密钥问题，404/405 说明 Base URL 少了路径前缀（这类配置错必须报出来，
// 否则熔断中的渠道会被 probe 当成健康渠道自动恢复）。
func (AutoDL) Test(ctx context.Context, cfg *relay.ChannelConfig) error {
	if cfg == nil {
		return errors.New("适配器 autodl：渠道配置为空")
	}
	if _, err := parseAutoDLOptions(cfg.Options); err != nil {
		return err
	}
	token := strings.TrimSpace(cfg.APIKey)
	if token == "" {
		return errors.New("适配器 autodl：密钥池为空")
	}
	target, err := relay.BuildRawURL(cfg, autodlResultPath+"__probe__")
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("适配器 autodl：构造请求失败：%w", err)
	}
	autodlSetAuth(req, token)

	resp, err := client(cfg).Do(req)
	if err != nil {
		return fmt.Errorf("连通失败：%w", err)
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusBadRequest, resp.StatusCode == http.StatusUnprocessableEntity,
		resp.StatusCode == http.StatusTooManyRequests:
		// 鉴权与路由都已通过，只是「任务不存在/参数不对」，不影响渠道可用性判定。
		return nil
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("鉴权失败（HTTP %d），请检查渠道密钥：%s", resp.StatusCode, snippet(buf))
	case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusMethodNotAllowed:
		return fmt.Errorf("接口路径不存在（HTTP %d），请检查 Base URL 是否带 /api 前缀：%s", resp.StatusCode, snippet(buf))
	default:
		return fmt.Errorf("上游返回 %d：%s", resp.StatusCode, snippet(buf))
	}
}

// SubmitTask 提交一次生成任务，返回上游 task_id。
func (AutoDL) SubmitTask(ctx context.Context, cfg *relay.ChannelConfig, req *relay.TaskSubmitRequest) (string, error) {
	if cfg == nil {
		return "", &relay.ConfigError{Field: "channel", Msg: "渠道配置为空"}
	}
	// 配置错必须在发请求前返回：静默用默认 workflow 会让「改了配置却没生效」无从察觉。
	if cfg.OptionsErr != nil {
		return "", cfg.OptionsErr
	}
	opts, err := parseAutoDLOptions(cfg.Options)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(cfg.APIKey)
	if token == "" {
		return "", autodlCfgErr("api_key", "渠道未配置密钥")
	}
	if strings.ContainsAny(token, "\r\n") {
		return "", autodlCfgErr("api_key", "密钥包含换行（会造成请求头注入）")
	}
	if req == nil || len(req.Body) == 0 {
		return "", &relay.CallerParamError{Msg: "请求体为空"}
	}
	var body autodlBody
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return "", &relay.CallerParamError{Msg: "请求体不是合法 JSON"}
	}
	// 未知顶层键**显式拒绝**，不静默丢弃。
	//
	// 本适配器是「未声明字段原样透传」的**唯一例外**，而且是有意的：
	// AutoDL 是 workflow 网关，请求体里的任何一个键都可能是该 workflow 的参数名
	// （步数、guidance、采样器…），透传等于让调用方直接改上游计算量与成本，
	// 而平台按任务一次计费，收不到对应费用。静默丢弃更糟 —— 用户以为生效了。
	// 所以这里选第三条路：说清楚哪个键不认识、该用什么字段。
	if err := autodlRejectUnknownKeys(req.Body); err != nil {
		return "", err
	}
	// req.UpstreamModel 故意不用：它是模型绑定里的别名，本适配器的"上游模型"是 workflow_id。
	plan, err := planAutoDLRequest(opts, &body)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(plan.body)
	if err != nil {
		return "", fmt.Errorf("适配器 autodl：序列化请求体失败：%w", err)
	}
	target, err := relay.BuildRawURL(cfg, autodlSubmitPath+url.PathEscape(plan.workflow))
	if err != nil {
		return "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("适配器 autodl：构造请求失败：%w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	autodlSetAuth(httpReq, token)

	resp, err := client(cfg).Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("上游请求失败：%w", err)
	}
	defer resp.Body.Close()
	buf, err := readUpstreamBody(resp.Body, maxUpstreamBodyBytes)
	if err != nil {
		return "", fmt.Errorf("读取上游响应失败：%w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", autodlHTTPError(resp.StatusCode, buf)
	}

	var p struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		// Message 与 Msg 同义（不同版本/网关的差异）：只读一个会让业务拒绝
		// 退化成「无错误信息」，用户看不出要改什么。
		Message string `json:"message"`
		Data    *struct {
			TaskID  string `json:"task_id"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf, &p); err != nil {
		return "", fmt.Errorf("适配器 autodl：解析提交响应失败：%w", err)
	}
	dataMsg := ""
	if p.Data != nil {
		dataMsg = p.Data.Message
	}
	// HTTP 200 + code != Success 是上游的业务拒绝形态：当成成功会让任务卡在 queued
	// 直到 1 小时超时（用户白等，渠道也看不出异常）。
	if !strings.EqualFold(strings.TrimSpace(p.Code), "Success") {
		return "", fmt.Errorf("适配器 autodl：上游拒绝提交（code=%s）：%s",
			strings.TrimSpace(p.Code), firstNonEmptyStr(dataMsg, p.Msg, strings.TrimSpace(p.Message), "无错误信息"))
	}
	if p.Data == nil || strings.TrimSpace(p.Data.TaskID) == "" {
		return "", errors.New("适配器 autodl：提交响应缺少 task_id")
	}
	return strings.TrimSpace(p.Data.TaskID), nil
}

// PollTask 查询任务状态；状态文案原样交给 worker 归一，产物只取视频项。
func (AutoDL) PollTask(ctx context.Context, cfg *relay.ChannelConfig, upstreamTaskID string) (*relay.TaskStatus, error) {
	id := strings.TrimSpace(upstreamTaskID)
	if id == "" {
		return nil, errors.New("适配器 autodl：上游任务 ID 为空")
	}
	if len(id) > autodlMaxTaskIDLen {
		return nil, errors.New("适配器 autodl：上游任务 ID 过长")
	}
	// 这里不校验 OptionsErr：配置在任务提交之后被改坏，不该让在途任务全部轮询失败
	// （连续 20 次失败会判定超时并退费，等于把已生成好的产物丢掉）。
	target, err := relay.BuildRawURL(cfg, autodlResultPath+url.PathEscape(id))
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("适配器 autodl：构造请求失败：%w", err)
	}
	autodlSetAuth(req, strings.TrimSpace(cfg.APIKey))

	resp, err := client(cfg).Do(req)
	if err != nil {
		return nil, fmt.Errorf("上游请求失败：%w", err)
	}
	defer resp.Body.Close()
	buf, err := readUpstreamBody(resp.Body, maxUpstreamBodyBytes)
	if err != nil {
		return nil, fmt.Errorf("读取上游响应失败：%w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("适配器 autodl：查询任务失败（HTTP %d）：%s", resp.StatusCode, snippet(buf))
	}

	var p struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		// Message 与 Msg 同义（不同版本/网关的差异）：只读一个会让业务拒绝
		// 退化成「无错误信息」，用户看不出要改什么。
		Message string `json:"message"`
		Data    *struct {
			TaskID  string `json:"task_id"`
			Status  string `json:"status"`
			Message string `json:"message"`
			// 这里曾经解析 `duration`：它是**任务耗时**，不是成片时长。
			// 一旦有人把它当视频秒数用，4 秒的片子会按 196 秒计费。字段已删，
			// 杜绝「顺手拿来用」这条路（计费秒数只来自请求参数，见 manifest.go）。
			Results []autodlResult `json:"results"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf, &p); err != nil {
		return nil, fmt.Errorf("适配器 autodl：解析查询响应失败：%w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(p.Code), "Success") {
		return nil, fmt.Errorf("适配器 autodl：查询被上游拒绝（code=%s）：%s",
			strings.TrimSpace(p.Code), firstNonEmptyStr(p.Msg, "无错误信息"))
	}
	if p.Data == nil {
		return nil, errors.New("适配器 autodl：查询响应缺少 data")
	}

	status := strings.ToLower(strings.TrimSpace(p.Data.Status))
	out := &relay.TaskStatus{Status: status}
	switch status {
	case "success", "succeeded", "completed", "complete", "done", "finished":
		// 产物必须**全量保留**（P1-28）：改造前只挑一条视频，
		// 一次返回「视频 + 封面图 + 音轨」的任务会把后两者直接丢掉 ——
		// 上游按多产物计费，平台却只交付一个文件。
		urls := pickAutoDLArtifacts(p.Data.Results)
		if len(urls) == 0 {
			// 宁可失败退费，也不能「结算成功但用户拿不到文件」。
			out.Status = taskstate.Failed
			out.Error = "上游返回成功但缺少视频产物"
			return out, nil
		}
		out.Status = taskstate.Succeeded
		out.ResultURLs = urls
		out.ResultURL = urls[0]
	case "failed", "failure", "error":
		out.Status = taskstate.Failed
		out.Error = firstNonEmptyStr(p.Data.Message, p.Msg, "上游任务失败")
	}
	return out, nil
}

// autodlResult 是查询响应里的一条产物。
type autodlResult struct {
	URL      string `json:"url"`
	Type     string `json:"type"`
	FileType string `json:"file_type"`
}

// pickAutoDLArtifacts 从 results 里挑出**全部**可用产物（视频优先，其次图片/音频）。
//
// 三条规则：
//  1. 视频排在最前 —— 首个产物会被用作 `result_url` / 详情页主图，必须是成片；
//  2. 其余类型（图片封面、音频音轨）全部保留，交给多产物链路按 artifact_index 落盘；
//  3. 保留上游给的相对顺序（同一类型内部不重排），这样产物下标在重试时是稳定的。
//
// 不能盲取 `results[0]`：上游可能先返回封面图，盲取会把封面当成视频交付。
func pickAutoDLArtifacts(results []autodlResult) []string {
	if len(results) == 0 {
		return nil
	}
	videos := make([]string, 0, len(results))
	others := make([]string, 0, len(results))
	for _, r := range results {
		u := strings.TrimSpace(r.URL)
		if u == "" {
			continue
		}
		if autodlIsVideo(r) {
			videos = append(videos, u)
			continue
		}
		others = append(others, u)
	}
	out := make([]string, 0, len(videos)+len(others))
	out = append(out, videos...)
	out = append(out, others...)
	return out
}

// autodlIsVideo 判断一条产物是不是视频（type 优先，其次 file_type 后缀）。
func autodlIsVideo(r autodlResult) bool {
	t := strings.ToLower(strings.TrimSpace(r.Type))
	switch t {
	case "video", "videos", "mp4", "mov", "webm":
		return true
	case "image", "img", "picture", "photo", "audio", "music", "cover":
		return false
	}
	switch strings.ToLower(strings.TrimSpace(r.FileType)) {
	case "mp4", "mov", "webm", "mkv", "avi":
		return true
	}
	return false
}

// autodlHTTPError 按状态码分档归因。
//
// 只有 400/422 归「调用方参数错」：404/405 是运营把 workflow id 或 Base URL 填错，
// 归给调用方会让用户收到「参数不合法」，运营永远查不到根因。
func autodlHTTPError(status int, buf []byte) error {
	// 优先取信封里的那一句（用户能读懂），原文只在取不到时兜底。
	detail := upstreamDetail(buf, status)
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return &relay.CallerParamError{Msg: "AutoDL 拒绝了本次参数（HTTP " + strconv.Itoa(status) + "）：" + detail}
	case http.StatusUnauthorized, http.StatusForbidden:
		return &relay.HTTPStatusError{Code: status, Note: "适配器 autodl：鉴权失败（HTTP " + strconv.Itoa(status) + "），请检查渠道密钥：" + detail}
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		// 404 在本适配器里 90% 是「Base URL 多写了 /api」（P2-05）：
		// AutoDL 的 workflow 端点在根路径下，不是 /api 前缀。文案直接给出可执行的检查项，
		// 比让运维对着「路径不存在」猜半小时有用。
		return &relay.HTTPStatusError{Code: status, Note: "适配器 autodl：workflow 或接口路径不存在（HTTP " + strconv.Itoa(status) +
			"）。请检查：① Base URL 是否多写了 /api（正确形态如 https://xxx.autodl.com）；② workflow ID 是否属于该实例：" + detail}
	case http.StatusTooManyRequests:
		return &relay.HTTPStatusError{Code: status, Note: "适配器 autodl：上游限流（HTTP 429）：" + detail}
	default:
		// 5xx 走标准文案并带上响应体：Body 为空时 relay.IsAmbiguousSubmit 会判定为
		// 「上游可能已建单」，禁止换渠道重投（否则一次网关抖动就是两份上游成本）。
		return relay.StatusError(status, detail)
	}
}

// autodlSetAuth 注入裸 token（上游不接受 "Bearer " 前缀）。
//
// 不复用 relay.SetAuthHeader：那个默认加 Bearer，正好是这里不能要的。
func autodlSetAuth(req *http.Request, token string) {
	if req == nil || token == "" {
		return
	}
	req.Header.Set("Authorization", token)
}

// ---------------------------------------------------------------- 同步端点（不支持）

// Chat 不承接同步对话。
func (AutoDL) Chat(context.Context, *relay.ChannelConfig, *relay.RelayRequest) (*relay.RelayResponse, error) {
	return nil, errAutoDLNoEndpoint
}

// Embeddings 不承接向量请求。
func (AutoDL) Embeddings(context.Context, *relay.ChannelConfig, *relay.RelayRequest) (*relay.RelayResponse, error) {
	return nil, errAutoDLNoEndpoint
}

// Completions 不承接旧版补全。
func (AutoDL) Completions(context.Context, *relay.ChannelConfig, *relay.RelayRequest) (*relay.RelayResponse, error) {
	return nil, errAutoDLNoEndpoint
}

// StreamChat 不承接流式对话。
func (AutoDL) StreamChat(context.Context, *relay.ChannelConfig, *relay.RelayRequest, func(relay.SSEFrame) error) (*relay.StreamResult, error) {
	return nil, errAutoDLNoEndpoint
}

// Forward 不承接通用透传。
func (AutoDL) Forward(context.Context, *relay.ChannelConfig, *relay.RelayRequest, string) (*relay.RawResponse, error) {
	return nil, errAutoDLNoEndpoint
}

// firstNonEmptyStr 返回第一个非空（去空白后）的字符串。
func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// autodlKnownTopKeys 是 autodlBody 声明的顶层键（由 json tag 反射得出，不手写）。
//
// 手写清单会与结构体漂移：加了字段忘了登记 = 新字段被拒；删了字段没删登记 = 幽灵键。
var autodlKnownTopKeys = func() map[string]bool {
	out := make(map[string]bool, 8)
	t := reflect.TypeOf(autodlBody{})
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if name, _, ok := strings.Cut(tag, ","); ok {
			tag = name
		}
		if tag != "" && tag != "-" {
			out[tag] = true
		}
	}
	// model 由平台注入（选路用），客户端写它不算「未知键」。
	out["model"] = true
	return out
}()

// autodlRejectUnknownKeys 拒绝平台不认识的顶层键（原因见 Forward 里的注释）。
func autodlRejectUnknownKeys(raw []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(raw), &top); err != nil || top == nil {
		return nil // 非对象由调用方的结构体解析报错
	}
	bad := make([]string, 0, 4)
	for k := range top {
		if !autodlKnownTopKeys[k] {
			bad = append(bad, k)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad) // 文案稳定：同一份请求每次报同一句
	return &relay.CallerParamError{Msg: fmt.Sprintf(
		"不支持的参数 %s；本模型只接受 prompt / seconds / resolution / aspect_ratio / seed / input_reference",
		strings.Join(bad, " / "))}
}
