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
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.uber.org/zap"

	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/taskstate"
	"lingdong-api/internal/relay"
)

// Metaso 适配器：秘塔（Metaso）MiniMax-H3 视频生成，两段式异步任务（提交拿 task_id → 轮询）。
//
// 设计要点（改动前先读）：
//  1. **标准/优先通道不是参数级开关**：平台是「先算价冻结 → 再选渠道」，所以渠道级开关
//     影响不到价格。正确做法是把「优先通道」做成另一个模型 ID（metaso-h3-fast，价目 2×），
//     本适配器只负责在请求里下发 fast_mode=true/false 告诉上游走哪条通道。
//  2. **fast_mode 只来自渠道配置**：请求体里的同名字段一律不读。否则用户可以「按标准价
//     请求优先通道」——计费与实付直接倒挂。
//  3. **metadata 是允许清单，不是透传**：上游的 billing_policy / context_ir_enabled /
//     callback_url 都会改变实际计费或把结果回调到用户自己的服务器。这里只放行
//     aigc_watermark，其余键直接 400；context_ir_enabled 上移到渠道配置（运营决定）。
//  4. **参数错在本地判定**（relay.CallerParamError）：任务服务据此短路候选循环、
//     直接给调用方 400，而不是打满所有渠道后返回「上游提交失败」。
//  5. **data: 内联素材直接拒绝**：内联体既读不到时长（参考视频按秒计费），也无法重放。
type Metaso struct{}

const (
	metasoSubmitPath        = "/api/minimax/v2/video_generation"
	metasoRegenPath         = "/api/minimax/v2/video_regeneration"
	metasoDefaultQueryPath  = "/api/minimax/v2/query/video_generation/"
	metasoMaxOptionsBytes   = 4096
	metasoMaxPromptRunes    = 20000
	metasoMinSeconds        = 4
	metasoMaxSeconds        = 30
	metasoMaxSeconds2K      = 15
	metasoMaxTaskIDLen      = 128
	metasoMaxQueryPathLen   = 200
	metasoDefaultModelAlias = "MiniMax-H3"
)

// metasoDefaultResolution 是 平台档位 → 上游字面量的默认映射（"" = 该档未开通）。
//
// 依据：上游价目表逐档列出 480P / 768P / 1080P / 2K。注意 Metaso 官方插件把
// resolution 限制成 768P/2K，若真实上游只认这两个值，运营把对应档位清空即可（不发版）。
var metasoDefaultResolution = map[string]string{
	"480p":  "480P",
	"720p":  "768P",
	"1080p": "1080P",
	"2k":    "2K",
}

// metasoTiers 是分辨率档位的固定遍历顺序（与计费侧 second_price_quota 的键一致）。
var metasoTiers = []string{"480p", "720p", "1080p", "2k"}

// metasoUpstreamResolutions 是上游字面量白名单；"" 表示该档未开通（由 options_schema.go 派生）。
var metasoUpstreamResolutions = stringSet(metasoUpstreamOrder)

// metasoRatios 是上游支持的画幅比例。
var metasoRatios = map[string]bool{
	"adaptive": true, "21:9": true, "16:9": true, "4:3": true, "1:1": true, "3:4": true, "9:16": true,
}

// metasoRefAlias 是素材类型别名（含 Metaso 特有的 reference_* 与 base_video 写法）。
//
// 不复用 refTypeAlias：那个是 AutoDL 的白名单，加键会改变 AutoDL 接受的类型（行为回归）。
var metasoRefAlias = map[string]string{
	"image":           "image",
	"reference_image": "image",
	"audio":           "audio",
	"reference_audio": "audio",
	"video":           "video",
	"reference_video": "video",
	"first_frame":     "first_frame",
	"first-frame":     "first_frame",
	"last_frame":      "last_frame",
	"last-frame":      "last_frame",
	"base_video":      "base_video",
	"base-video":      "base_video",
}

// metasoTaskIDPattern 限定上游任务号字符集：既挡注入，也让 URL 拼接无需担心路径穿越。
var metasoTaskIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

var errMetasoNoEndpoint = errors.New("适配器 metaso：不支持该端点（仅提供异步视频任务）")

func init() {
	// 注册失败只可能是命名冲突（编码错误），与其它适配器保持一致。
	_ = relay.RegisterAdapter(Metaso{})
}

// Name 返回适配器标识（对齐 channels.adapter_id）。
func (Metaso) Name() string { return "metaso" }

// Caps 返回能力子集：本适配器只做视频异步任务。
func (Metaso) Caps() []string { return []string{"video"} }

// SupportsDeclarativeConfig 显式声明不支持声明式配置：路径固定、鉴权只认 Bearer，
// 声明式改写对它没有意义（不实现接口虽也等于 false，但显式声明能防止后人误以为支持）。
func (Metaso) SupportsDeclarativeConfig() bool { return false }

// SupportsEndpoint 声明不承接任何同步端点（不实现该接口等于「全支持」）。
func (Metaso) SupportsEndpoint(relay.EndpointKind) bool { return false }

// AsyncEndpoints：metaso 是当前**唯一**有再生成语义的上游（见 applyRegeneration），
// 视频与再生成两个端点都由它承接；音乐/图像没有上游契约。
func (Metaso) AsyncEndpoints() []string {
	return []string{"video/generations", "video/regenerations"}
}

// ---------------------------------------------------------------- 渠道私有配置

// metasoOptions 是 channels.adapter_options 归一后的结果。
type metasoOptions struct {
	fastMode         bool
	resolutionMap    map[string]string
	contextIREnabled *bool  // nil = 不下发（用上游默认）
	queryPath        string // 轮询路径（再生成与生成可能不同，做成运行时可调）
}

func metasoCfgErr(field, msg string) error {
	return &relay.ConfigError{Scope: "渠道适配器配置错误", Field: field, Msg: msg}
}

// parseMetasoOptions 是**唯一**的配置解析入口（保存期校验与运行期共用）。
// 空（nil/""/null/{}）= 全部使用内置默认。
func parseMetasoOptions(raw []byte) (*metasoOptions, error) {
	opts := &metasoOptions{
		resolutionMap: make(map[string]string, len(metasoDefaultResolution)),
		queryPath:     metasoDefaultQueryPath,
	}
	for k, v := range metasoDefaultResolution {
		opts.resolutionMap[k] = v
	}

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" || string(trimmed) == "{}" {
		return opts, nil
	}
	if len(trimmed) > metasoMaxOptionsBytes {
		return nil, metasoCfgErr("", fmt.Sprintf("配置过大（%d 字节，上限 %d）", len(trimmed), metasoMaxOptionsBytes))
	}

	var doc struct {
		FastMode         *bool             `json:"fast_mode"`
		ResolutionMap    map[string]string `json:"resolution_map"`
		ContextIREnabled *bool             `json:"context_ir_enabled"`
		QueryPath        string            `json:"query_path"`
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, metasoCfgErr("", "不是合法的配置对象："+err.Error())
	}
	// 尾部多余内容（两个对象拼在一起）会让「保存时报错、运行期只读到第一个」，必须拦。
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, metasoCfgErr("", "只能包含一个 JSON 对象")
	}

	if doc.FastMode != nil {
		opts.fastMode = *doc.FastMode
	}
	opts.contextIREnabled = doc.ContextIREnabled

	if qp := strings.TrimSpace(doc.QueryPath); qp != "" {
		if !strings.HasPrefix(qp, "/") {
			return nil, metasoCfgErr("query_path", "必须以 / 开头")
		}
		// 必须以 / 结尾（P2-03）：轮询是 `query_path + 任务号` 的直接拼接。
		// 少了尾斜杠会拼成 `/query/video_generationabc123`，上游回 404，
		// 而 404 在轮询里表现为「连续失败 → 判超时退款」，用户白等一小时还丢产物。
		// 这里**不做静默补全**：静默补全会让运营以为自己写的路径生效了，
		// 换一个上游实现（要求不带尾斜杠）时又变成另一个线上问题。
		if !strings.HasSuffix(qp, "/") {
			return nil, metasoCfgErr("query_path", "必须以 / 结尾（轮询时会直接拼接任务号）")
		}
		if strings.ContainsAny(qp, "?#") {
			return nil, metasoCfgErr("query_path", "不能包含 ? 或 #")
		}
		if len(qp) > metasoMaxQueryPathLen {
			return nil, metasoCfgErr("query_path", fmt.Sprintf("长度不能超过 %d", metasoMaxQueryPathLen))
		}
		opts.queryPath = qp
	}

	for k, v := range doc.ResolutionMap {
		want, ok := metasoDefaultResolution[k]
		if !ok {
			return nil, metasoCfgErr("resolution_map."+k, "只支持 480p/720p/1080p/2k 四个档位")
		}
		v = strings.TrimSpace(v)
		if !metasoUpstreamResolutions[v] {
			return nil, metasoCfgErr("resolution_map."+k, "取值只能是空（未开通）/480P/768P/1080P/2K")
		}
		// 只允许「同档等价映射」（P2-09）：把 1080p 映射到 2K 会让用户按 1080p 的价
		// 拿到 2K 的成片，而平台的成本是按上游 2K 档付的 —— 这是直接的价格倒挂。
		// 因此除「清空（未开通）」之外，取值必须与内置默认完全一致。
		if v != "" && v != want {
			return nil, metasoCfgErr("resolution_map."+k,
				fmt.Sprintf("%s 档只能映射到 %s（跨档映射会造成价格与实际成本倒挂）；如需更换上游字面量请先确认价目一致", k, want))
		}
		opts.resolutionMap[k] = v
	}
	return opts, nil
}

// ValidateAdapterOptions 供管理台保存期校验（relay.OptionsSupporter）。
func (Metaso) ValidateAdapterOptions(raw []byte) error {
	opts, err := parseMetasoOptions(raw)
	if err != nil {
		return err
	}
	// context_ir_enabled 由上游按其自身口径加收（P0-15）。平台侧没有「按请求加收」的
	// 计费规则，因此它的成本必须体现在**这条渠道的价目**上。在保存期提示一次，
	// 比在月底对账时发现「上游成本 > 平台收入」要早得多。
	if opts.contextIREnabled != nil && *opts.contextIREnabled {
		logger.L().Warn("metaso 渠道已开启 context_ir_enabled：上游会额外计费，请确认该渠道的价目已包含这部分成本",
			zap.String("adapter", "metaso"))
	}
	return nil
}

// ---------------------------------------------------------------- 请求解析与组装

// metasoBody 是本适配器关心的请求字段（其余字段忽略）。
type metasoBody struct {
	Prompt     string          `json:"prompt"`
	Seconds    json.RawMessage `json:"seconds"`
	Resolution string          `json:"resolution"`
	Size       string          `json:"size"`
	// Ratio / Aspect 是同一个字段的两种写法（P1-21）：ratio 优先，两者都空则不传（用上游默认）。
	//
	// 为什么要收两种：对外协议（OpenAI 风格）用 aspect_ratio，上游用 ratio；
	// 客户端两种写法都有。只认一种的话，另一种会被静默丢弃 —— 用户看到的是
	// 「我明明指定了 9:16，出来却是 adaptive」。
	Ratio    string         `json:"ratio"`
	Aspect   string         `json:"aspect_ratio"`
	Metadata map[string]any `json:"metadata"`
	// SourceTaskID 只服务再生成：由 task 服务把「平台任务号 → 上游任务号」解析后写入。
	SourceTaskID string `json:"source_task_id"`
	InputRef     []struct {
		Type     string `json:"type"`
		ImageURL string `json:"image_url"`
		AudioURL string `json:"audio_url"`
		VideoURL string `json:"video_url"`
	} `json:"input_reference"`
}

// metasoURL 是上游素材的 {url} 包装形态。
type metasoURL struct {
	URL string `json:"url"`
}

// metasoContent 是 content 数组的一项。
type metasoContent struct {
	Type     string     `json:"type"`
	Role     string     `json:"role,omitempty"`
	Text     string     `json:"text,omitempty"`
	ImageURL *metasoURL `json:"image_url,omitempty"`
	VideoURL *metasoURL `json:"video_url,omitempty"`
	AudioURL *metasoURL `json:"audio_url,omitempty"`
}

// metasoSubmitBody 是生成接口的请求体。
type metasoSubmitBody struct {
	Model            string          `json:"model"`
	Content          []metasoContent `json:"content"`
	Resolution       string          `json:"resolution"`
	Duration         int64           `json:"duration"`
	Ratio            string          `json:"ratio"`
	FastMode         bool            `json:"fast_mode"`
	ContextIREnabled *bool           `json:"context_ir_enabled,omitempty"`
	AIGCWatermark    *bool           `json:"aigc_watermark,omitempty"`
}

// metasoRegenBody 是再生成接口的请求体（不下发 duration/ratio/fast_mode）。
type metasoRegenBody struct {
	Model            string          `json:"model"`
	SourceTaskID     string          `json:"source_task_id,omitempty"`
	Content          []metasoContent `json:"content,omitempty"`
	Resolution       string          `json:"resolution"`
	ContextIREnabled *bool           `json:"context_ir_enabled,omitempty"`
	AIGCWatermark    *bool           `json:"aigc_watermark,omitempty"`
}

// metasoRefs 是素材归类结果（保持提交顺序）。
type metasoRefs struct {
	firstFrame string
	lastFrame  string
	images     []string
	videos     []string
	audios     []string
	baseVideo  string
}

// metasoPlan 是一次提交的最终产物。
type metasoPlan struct {
	path string
	body any
}

// planMetasoRequest 把平台规范请求组装成上游请求体；任何越界/未知都返回 CallerParamError。
func planMetasoRequest(opts *metasoOptions, req *relay.TaskSubmitRequest, body *metasoBody) (*metasoPlan, error) {
	model := strings.TrimSpace(req.UpstreamModel)
	if model == "" {
		// 渠道绑定没填上游模型：用产品名兜底，而不是把对外模型 ID 拼上去。
		model = metasoDefaultModelAlias
	}

	tier, err := metasoTier(body.Resolution, body.Size)
	if err != nil {
		return nil, err
	}
	upstream := opts.resolutionMap[tier]
	if upstream == "" {
		return nil, &relay.CallerParamError{
			Msg: fmt.Sprintf("本模型未开通 %s 档", tier),
		}
	}

	refs, err := classifyMetasoRefs(body)
	if err != nil {
		return nil, err
	}
	// metadata 只放行 aigc_watermark：billing_policy / context_ir_enabled / callback_url
	// 都会改变实际计费或把结果回调到用户自己的服务器（等于用户自助改价）。
	watermark, err := metasoWatermark(body.Metadata)
	if err != nil {
		return nil, err
	}

	// 再生成没有「必填 prompt」的概念（源任务已带内容），prompt 校验必须放在分支之后。
	if req.Kind == relay.TaskKindRegeneration {
		return planMetasoRegen(opts, model, upstream, tier, body, refs, watermark)
	}

	// prompt 非空只是**生成**路径的要求；再生成可以用源任务内容，故校验放在分支之后。
	if err := checkMetasoPrompt(body.Prompt); err != nil {
		return nil, err
	}
	secs, err := metasoSeconds(body.Seconds, upstream, tier)
	if err != nil {
		return nil, err
	}
	// 画幅缺省值按「有没有参考素材」分叉：文生视频上游拒收 adaptive。
	ratioFallback := "adaptive"
	if len(refs.images) == 0 && len(refs.videos) == 0 && refs.baseVideo == "" {
		ratioFallback = metasoDefaultTextRatio
	}
	ratio, err := metasoRatio(body.Ratio, body.Aspect, ratioFallback)
	if err != nil {
		return nil, err
	}
	content, err := metasoContentOf(body.Prompt, refs)
	if err != nil {
		return nil, err
	}
	return &metasoPlan{
		path: metasoSubmitPath,
		body: &metasoSubmitBody{
			Model: model, Content: content, Resolution: upstream, Duration: secs, Ratio: ratio,
			FastMode: opts.fastMode, ContextIREnabled: opts.contextIREnabled, AIGCWatermark: watermark,
		},
	}, nil
}

// planMetasoRegen 组装再生成请求：source_task_id 与 base_video 二选一，同时出现即 400。
func planMetasoRegen(opts *metasoOptions, model, upstream, tier string, body *metasoBody, refs *metasoRefs, watermark *bool) (*metasoPlan, error) {
	if upstream != "2K" {
		// 再生成的目标档只有 2K：其它档位下发出去只会拿到语义不明的上游错误。
		return nil, &relay.CallerParamError{Msg: "再生成的目标分辨率必须是 2k"}
	}
	src := strings.TrimSpace(body.SourceTaskID)
	switch {
	case src != "" && refs.baseVideo != "":
		return nil, &relay.CallerParamError{Msg: "source_task_id 与 base_video 只能二选一"}
	case src != "":
		if !metasoTaskIDPattern.MatchString(src) {
			return nil, &relay.CallerParamError{Msg: "source_task_id 格式不合法"}
		}
		return &metasoPlan{
			path: metasoRegenPath,
			body: &metasoRegenBody{
				Model: model, SourceTaskID: src, Resolution: upstream,
				ContextIREnabled: opts.contextIREnabled, AIGCWatermark: watermark,
			},
		}, nil
	case refs.baseVideo != "":
		p := strings.TrimSpace(body.Prompt)
		if utf8.RuneCountInString(p) > metasoMaxPromptRunes {
			return nil, &relay.CallerParamError{Msg: fmt.Sprintf("prompt 过长（上限 %d 字符）", metasoMaxPromptRunes)}
		}
		// 复用同一个组装器（P1-23/24），而不是在这里手写「text + base_video」两条。
		//
		// 手写的版本会把 first_frame / last_frame / reference_image* 全部丢掉：
		// 用户在再生成请求里带了首尾帧，却被静默忽略——出来的片子与预期完全不同，
		// 而平台照常按「带素材」的价目收钱（收了下发项的钱却没用上素材）。
		content, cerr := metasoContentOf(p, refs)
		if cerr != nil {
			return nil, cerr
		}
		return &metasoPlan{
			path: metasoRegenPath,
			body: &metasoRegenBody{
				Model: model, Content: content, Resolution: upstream,
				ContextIREnabled: opts.contextIREnabled, AIGCWatermark: watermark,
			},
		}, nil
	default:
		return nil, &relay.CallerParamError{Msg: "再生成必须提供 source_task_id 或 base_video 之一"}
	}
}

// metasoTier 把 resolution/size 归一成平台档位键（480p/720p/1080p/2k）。
//
// 与 task.normalizeVideoResolution 语义保持一致，但**不**把 WxH 映射到 2k：
// 2k 只认字面量，否则存量 1440p 请求会从 1080p 跳档到 2k（没有 2k 价就 400）。
func metasoTier(resolution, size string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(resolution))
	if v == "" {
		v = strings.ToLower(strings.TrimSpace(size))
	}
	if v == "" {
		return "", &relay.CallerParamError{Msg: "缺少 resolution（或 size）"}
	}
	switch v {
	case "480p", "720p", "1080p", "2k":
		return v, nil
	}
	// WxH 写法：按短边折算（与平台 normalizeVideoResolution 逐条对齐）
	idx := strings.IndexAny(v, "x×*")
	if idx <= 0 {
		return "", &relay.CallerParamError{Msg: fmt.Sprintf("无法识别的分辨率「%s」（支持 480p/720p/1080p/2k 或 宽x高）", v)}
	}
	// 分隔符按 rune 宽度前移：× 是 3 字节，用 idx+1 会切在字符中间。
	_, w := utf8.DecodeRuneInString(v[idx:])
	if w <= 0 || idx+w >= len(v) {
		return "", &relay.CallerParamError{Msg: fmt.Sprintf("无法识别的分辨率「%s」", v)}
	}
	width, werr := strconv.Atoi(strings.TrimSpace(v[:idx]))
	height, herr := strconv.Atoi(strings.TrimSpace(v[idx+w:]))
	if werr != nil || herr != nil || width <= 0 || height <= 0 {
		return "", &relay.CallerParamError{Msg: fmt.Sprintf("无法识别的分辨率「%s」", v)}
	}
	short := width
	if height < short {
		short = height
	}
	switch {
	case short <= 480:
		return "480p", nil
	case short <= 720:
		return "720p", nil
	default:
		return "1080p", nil
	}
}

// metasoRatio 归一画幅比例（ratio 优先，aspect_ratio 兜底）。
//
// 两者都空时返回 adaptive（上游默认值，显式下发以保证请求体可复现）。
// metasoDefaultTextRatio 是**文生视频**缺省画幅。
//
// 上游对「无参考素材 + ratio 为空或 adaptive」直接 400（`文生视频 ratio 不能为空或 adaptive`），
// 而图生/参考视频场景下 adaptive（跟随素材）是合法且更合理的缺省，所以两者必须分开。
const metasoDefaultTextRatio = "16:9"

// metasoRatio 归一画幅。
//
// fallback 是「调用方没传」时的缺省值：文生视频传具体画幅（上游不接受 adaptive），
// 其余场景传 adaptive。显式传 adaptive 但仍属文生视频时本地就拒——
// 把上游那句 `文生视频 ratio 不能为空或 adaptive` 原样透传，用户根本不知道该改成什么。
func metasoRatio(ratio, aspect, fallback string) (string, error) {
	r := firstNonEmptyStr(strings.TrimSpace(ratio), strings.TrimSpace(aspect))
	if r == "" {
		if fallback == "" {
			return "adaptive", nil
		}
		return fallback, nil
	}
	if !metasoRatios[r] {
		return "", &relay.CallerParamError{Msg: fmt.Sprintf("不支持的画幅比例「%s」", r)}
	}
	if r == "adaptive" && fallback != "" && fallback != "adaptive" {
		return "", &relay.CallerParamError{Msg: fmt.Sprintf(
			"文生视频必须指定具体画幅（如 %s）；adaptive 只在带参考图/参考视频时可用", fallback)}
	}
	return r, nil
}

// checkMetasoPrompt 校验提示词非空且不超过上游长度上限。
func checkMetasoPrompt(p string) error {
	if strings.TrimSpace(p) == "" {
		return &relay.CallerParamError{Msg: "prompt 不能为空"}
	}
	if utf8.RuneCountInString(p) > metasoMaxPromptRunes {
		return &relay.CallerParamError{Msg: fmt.Sprintf("prompt 过长（上限 %d 字符）", metasoMaxPromptRunes)}
	}
	return nil
}

// metasoSeconds 解析并校验时长：4–30 秒；2K 档 4–15 秒。越界不 clamp（clamp 会让用户
// 以为跑了 30 秒，实际只跑 15 秒）。
func metasoSeconds(raw json.RawMessage, upstream, tier string) (int64, error) {
	n, err := parseIntJSON(raw)
	if err != nil {
		return 0, &relay.CallerParamError{Msg: "seconds 必须是整数"}
	}
	max := int64(metasoMaxSeconds)
	if upstream == "2K" {
		max = metasoMaxSeconds2K
	}
	if n < metasoMinSeconds || n > max {
		return 0, &relay.CallerParamError{
			Msg: fmt.Sprintf("%s 档时长必须是 %d–%d 秒的整数（收到 %d）", tier, metasoMinSeconds, max, n),
		}
	}
	return n, nil
}

// parseIntJSON 解析数字或数字字符串。
func parseIntJSON(raw json.RawMessage) (int64, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, errors.New("空值")
	}
	var num float64
	if err := json.Unmarshal([]byte(s), &num); err == nil {
		if num != float64(int64(num)) {
			return 0, errors.New("不是整数")
		}
		return int64(num), nil
	}
	var str string
	if err := json.Unmarshal([]byte(s), &str); err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(str), 10, 64)
}

// classifyMetasoRefs 归类素材；未知 type 与内联 data: 都直接拒绝。
func classifyMetasoRefs(body *metasoBody) (*metasoRefs, error) {
	out := &metasoRefs{}
	for i, r := range body.InputRef {
		typ := strings.ToLower(strings.TrimSpace(r.Type))
		image := strings.TrimSpace(r.ImageURL)
		audio := strings.TrimSpace(r.AudioURL)
		video := strings.TrimSpace(r.VideoURL)
		if typ == "" {
			// type 缺省时按「哪个链接非空」推断（与 task.refKind 的推断方向一致）。
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
		kind, ok := metasoRefAlias[typ]
		if !ok {
			return nil, &relay.CallerParamError{
				Msg: fmt.Sprintf("不支持的素材类型「%s」：本模型只接受 image/audio/video/first_frame/last_frame/base_video", r.Type),
			}
		}
		link := ""
		switch kind {
		case "image":
			link = image
		case "audio":
			link = audio
		case "video", "base_video":
			link = video
		case "first_frame":
			link = firstNonEmptyStr(image, video)
		case "last_frame":
			link = firstNonEmptyStr(image, video)
		}
		if link == "" {
			return nil, &relay.CallerParamError{Msg: fmt.Sprintf("第 %d 个素材缺少链接", i+1)}
		}
		if strings.HasPrefix(strings.ToLower(link), "data:") {
			return nil, &relay.CallerParamError{
				Msg: fmt.Sprintf("第 %d 个素材是内联 data: 形式，本模型不支持（无法计费与重试）：请先用 /v1/files 上传后改用直链", i+1),
			}
		}
		switch kind {
		case "first_frame":
			if out.firstFrame != "" {
				return nil, &relay.CallerParamError{Msg: "first_frame 只能有 1 个"}
			}
			out.firstFrame = link
		case "last_frame":
			if out.lastFrame != "" {
				return nil, &relay.CallerParamError{Msg: "last_frame 只能有 1 个"}
			}
			out.lastFrame = link
		case "base_video":
			if out.baseVideo != "" {
				return nil, &relay.CallerParamError{Msg: "base_video 只能有 1 个"}
			}
			out.baseVideo = link
		case "image":
			out.images = append(out.images, link)
		case "video":
			out.videos = append(out.videos, link)
		case "audio":
			out.audios = append(out.audios, link)
		}
	}
	return out, nil
}

// metasoContentOf 按固定顺序组装 content：text → first_frame → last_frame → base_video →
// reference_image* → reference_video* → reference_audio*。
//
// 固定顺序是上游可复现的前提：content 顺序变化会让同一份输入产出不同结果，
// 也会让"重试 body 与首次 body 逐字节一致"的验收失效。
//
// 空 prompt 不下发 text 项：`{"type":"text","text":""}` 在部分上游实现里会被当成
// 「有一条空指令」而不是「没有指令」，从而拒绝请求。
func metasoContentOf(prompt string, refs *metasoRefs) ([]metasoContent, error) {
	content := make([]metasoContent, 0, 2+len(refs.images)+len(refs.videos)+len(refs.audios))
	if p := strings.TrimSpace(prompt); p != "" {
		content = append(content, metasoContent{Type: "text", Text: prompt})
	}
	if refs.firstFrame != "" {
		content = append(content, metasoContent{Type: "image_url", Role: "first_frame", ImageURL: &metasoURL{URL: refs.firstFrame}})
	}
	if refs.lastFrame != "" {
		content = append(content, metasoContent{Type: "image_url", Role: "last_frame", ImageURL: &metasoURL{URL: refs.lastFrame}})
	}
	if refs.baseVideo != "" {
		content = append(content, metasoContent{Type: "video_url", Role: "base_video", VideoURL: &metasoURL{URL: refs.baseVideo}})
	}
	for _, u := range refs.images {
		content = append(content, metasoContent{Type: "image_url", Role: "reference_image", ImageURL: &metasoURL{URL: u}})
	}
	for _, u := range refs.videos {
		content = append(content, metasoContent{Type: "video_url", Role: "reference_video", VideoURL: &metasoURL{URL: u}})
	}
	for _, u := range refs.audios {
		content = append(content, metasoContent{Type: "audio_url", Role: "reference_audio", AudioURL: &metasoURL{URL: u}})
	}
	return content, nil
}

// metasoWatermark 从 metadata 里取出唯一允许透传的键。
//
// 其它键（model/content/resolution/duration/ratio/fast_mode/source_task_id/
// context_ir_enabled/billing_policy/callback_url）一律拒绝：
// 保留键本身在结构上就不读，但静默忽略会让用户以为配置生效了，所以显式 400。
func metasoWatermark(md map[string]any) (*bool, error) {
	if len(md) == 0 {
		return nil, nil
	}
	var out *bool
	for k, v := range md {
		if k != "aigc_watermark" {
			return nil, &relay.CallerParamError{
				Msg: fmt.Sprintf("metadata 仅支持 aigc_watermark（不支持 %s）", k),
			}
		}
		b, ok := v.(bool)
		if !ok {
			return nil, &relay.CallerParamError{Msg: "metadata.aigc_watermark 必须是布尔值"}
		}
		out = &b
	}
	return out, nil
}

// ---------------------------------------------------------------- 提交与轮询

// SubmitTask 提交生成/再生成任务，返回上游任务号。
func (Metaso) SubmitTask(ctx context.Context, cfg *relay.ChannelConfig, req *relay.TaskSubmitRequest) (string, error) {
	if cfg == nil {
		return "", metasoCfgErr("channel", "渠道配置为空")
	}
	// 配置脏了必须在组装请求之前失败：拿默认值继续跑等于按错误的价格/路径出网。
	if cfg.OptionsErr != nil {
		return "", cfg.OptionsErr
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return "", metasoCfgErr("keys", "渠道未配置密钥")
	}
	if strings.ContainsAny(cfg.APIKey, "\r\n") {
		return "", metasoCfgErr("keys", "渠道密钥包含非法字符")
	}
	opts, err := parseMetasoOptions(cfg.Options)
	if err != nil {
		return "", err
	}
	if req == nil {
		return "", &relay.CallerParamError{Msg: "请求体为空"}
	}
	var body metasoBody
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return "", &relay.CallerParamError{Msg: "请求体不是合法的 JSON 对象"}
	}
	plan, err := planMetasoRequest(opts, req, &body)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(plan.body)
	if err != nil {
		return "", fmt.Errorf("适配器 metaso：序列化请求体失败：%w", err)
	}
	payload, err = metasoForwardUnmapped(payload, req.Body)
	if err != nil {
		return "", err
	}
	target, err := relay.BuildRawURL(cfg, plan.path)
	if err != nil {
		return "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("适配器 metaso：构造请求失败：%w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+strings.TrimSpace(cfg.APIKey))

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
		return "", metasoHTTPError(resp.StatusCode, buf)
	}
	// HTTP 200 也可能是错误信封（上游用这种形态表达业务拒绝）。
	if code, msg, isErr := metasoErrorEnvelope(buf); isErr {
		return "", metasoEnvelopeError(code, msg)
	}

	var p struct {
		TaskID string `json:"task_id"`
		ID     string `json:"id"`
		Task   *struct {
			ID     string `json:"id"`
			TaskID string `json:"task_id"`
		} `json:"task"`
	}
	if err := json.Unmarshal(buf, &p); err != nil {
		return "", fmt.Errorf("适配器 metaso：解析提交响应失败：%w", err)
	}
	id := firstNonEmptyStr(p.TaskID, p.ID)
	if id == "" && p.Task != nil {
		id = firstNonEmptyStr(p.Task.TaskID, p.Task.ID)
	}
	// 200 但没有 task_id 必须判失败：当成成功会让任务空转到 deadline（用户白等，还要退费）。
	if id == "" {
		return "", errors.New("适配器 metaso：提交响应缺少 task_id")
	}
	if !metasoTaskIDPattern.MatchString(id) {
		return "", fmt.Errorf("适配器 metaso：上游返回的 task_id 格式不合法：%s", snippet([]byte(id)))
	}
	return id, nil
}

// PollTask 查询任务状态。
//
// **不校验 OptionsErr**：配置在提交之后被改坏，不该让在途任务全部轮询失败
// （连续 N 次失败会判超时退费，等于把已生成好的产物丢掉）。
func (Metaso) PollTask(ctx context.Context, cfg *relay.ChannelConfig, upstreamTaskID string) (*relay.TaskStatus, error) {
	if cfg == nil {
		return nil, metasoCfgErr("channel", "渠道配置为空")
	}
	id := strings.TrimSpace(upstreamTaskID)
	if id == "" {
		return nil, errors.New("适配器 metaso：上游任务 ID 为空")
	}
	if len(id) > metasoMaxTaskIDLen || !metasoTaskIDPattern.MatchString(id) {
		return nil, errors.New("适配器 metaso：上游任务 ID 格式不合法")
	}
	opts, err := parseMetasoOptions(cfg.Options)
	if err != nil {
		// 配置坏了也用默认查询路径继续查，而不是让在途任务失败。
		opts = &metasoOptions{queryPath: metasoDefaultQueryPath}
	}
	target, err := relay.BuildRawURL(cfg, opts.queryPath+url.PathEscape(id))
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("适配器 metaso：构造请求失败：%w", err)
	}
	req.Header.Set("Accept", "application/json")
	if key := strings.TrimSpace(cfg.APIKey); key != "" && !strings.ContainsAny(key, "\r\n") {
		req.Header.Set("Authorization", "Bearer "+key)
	}

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
		return nil, metasoHTTPError(resp.StatusCode, buf)
	}
	return parseMetasoTask(buf)
}

// metasoTask 是查询响应里的任务视图（兼容 task 包裹与平铺两种形态）。
type metasoTask struct {
	Status string `json:"status"`
	ID     string `json:"id"`
	TaskID string `json:"task_id"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
	Content json.RawMessage `json:"content"`
	URL     string          `json:"url"`
	Output  *struct {
		URL string `json:"url"`
	} `json:"output"`
	// Usage 是上游自报的实际用量（P1-22）：结算按它而不是按请求参数。
	Usage *metasoUsage `json:"usage"`
}

// metasoUsage 是上游 usage 对象：既要解析出平台认识的三项，又要留下原文（B7-3）。
//
// 为什么要原文：三项之外的字段（上游新加的视频时长/计费单位）此前被整段丢掉，
// 少收时连「上游当初报了什么」都无法复核。原文靠 UnmarshalJSON 顺手抓一份，
// 不额外做第二次解析 —— 多一次解析就多一份形状认知，早晚与这里分叉。
//
// 三项全部用 json.RawMessage：上游这三种字段既可能是数字也可能是数字字符串
// （同一个字段在不同区域/版本都出现过），用 float64 会让整条响应解析失败，
// 结果是一条已成功的任务被判「解析失败 → 连续失败 → 超时退款」。
type metasoUsage struct {
	TotalSeconds    json.RawMessage `json:"total_seconds"`
	InputSeconds    json.RawMessage `json:"input_seconds"`
	InputImageCount json.RawMessage `json:"input_image_count"`
	// raw 是这一段 usage 的逐字节原文（未导出：不参与任何 JSON 输出，
	// 也不参与结构比较，避免调用方误把它当业务字段）。
	raw json.RawMessage
}

// UnmarshalJSON 在正常解析三项的同时保存原文。
//
// 用别名类型绕开递归：别名与本体字段完全相同，但不再实现 UnmarshalJSON，
// 所以 json.Unmarshal 不会又绕回这个方法（未导出字段本就不被解码）。
func (u *metasoUsage) UnmarshalJSON(b []byte) error {
	type plain metasoUsage
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*u = metasoUsage(p)
	u.raw = append(json.RawMessage(nil), b...)
	return nil
}

// parseMetasoTask 归一状态并提取产物链接。上游有两种包裹形态（task 包裹 / 平铺），都接受。
func parseMetasoTask(buf []byte) (*relay.TaskStatus, error) {
	var flat metasoTask
	if err := json.Unmarshal(buf, &flat); err != nil {
		return nil, fmt.Errorf("适配器 metaso：解析查询响应失败：%w", err)
	}
	var wrapped struct {
		Task *metasoTask `json:"task"`
	}
	_ = json.Unmarshal(buf, &wrapped) // 形态不匹配不是错误：平铺响应解析成空结构即可
	task := flat
	if wrapped.Task != nil {
		task = *wrapped.Task
	}
	status := strings.ToLower(strings.TrimSpace(task.Status))
	out := &relay.TaskStatus{Status: status}
	// 用量在任何状态下都解析：成功时供结算按实际秒数计费，失败时供对账
	// （上游跑了 8 秒才失败，也可能已经在计费）。
	out.Usage = metasoUsageOf(&task)
	switch status {
	case "succeeded", "success", "completed", "complete", "done", "finished":
		link := metasoArtifactURL(&task)
		if link == "" {
			// 宁可失败退费，也不能「结算成功但用户拿不到文件」。
			out.Status = taskstate.Failed
			out.Error = "上游返回成功但缺少视频产物"
			return out, nil
		}
		out.Status = taskstate.Succeeded
		out.ResultURL = link
	case "failed", "failure", "error":
		out.Status = taskstate.Failed
		msg := ""
		if task.Error != nil {
			msg = task.Error.Message
		}
		out.Error = firstNonEmptyStr(msg, "上游任务失败")
	case "canceled", "cancelled":
		// 赋的是**平台**状态，不是上游原文：上游那两个拼写在上面的 case 里已被吃掉。
		out.Status = taskstate.Canceled
	}
	return out, nil
}

// metasoUsageOf 把上游 usage 归一成 relay.TaskUsage（缺失字段保持零值 = 未上报）。
//
// 只做「宽松读取」，不做任何推断与补齐：上游没报就是没报，
// 由 task 层的 mergeUpstreamUsage 决定回落策略（宁可少收也不能凭空多收）。
func metasoUsageOf(task *metasoTask) relay.TaskUsage {
	var u relay.TaskUsage
	if task == nil || task.Usage == nil {
		return u
	}
	// total_seconds 是「输出 + 输入」的合计，**不是**输出时长：
	// 带参考视频时上游会返回 {"total_seconds":8,"input_seconds":4,
	// "raw":{"output_seconds":4}} —— 直接把 total 当 output，会让同一个参考视频
	// 被收两次（一次算进视频秒数、一次走 ref_video_second_price）。
	// 真机实测：4 秒成片 + 4 秒参考视频，冻结 959,960、结算 1,439,940。
	// 所以这里必须减掉 input；上游没报 input 时 total 就是 output（纯文生场景）。
	u.InputSeconds = parseFloatJSON(task.Usage.InputSeconds)
	out := parseFloatJSON(task.Usage.TotalSeconds) - u.InputSeconds
	if out < 0 {
		out = 0
	}
	u.OutputSeconds = out
	if n := parseFloatJSON(task.Usage.InputImageCount); n > 0 {
		u.ImageCount = int64(n)
	}
	// 原文可能含上面三项之外的字段，必须原样带走（B7-3）。
	u.SetRaw(task.Usage.raw)
	return u
}

// parseFloatJSON 宽容解析数字或数字字符串（失败返回 0，不报错）。
func parseFloatJSON(raw json.RawMessage) float64 {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	var str string
	if err := json.Unmarshal([]byte(s), &str); err != nil {
		return 0
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(str), 64)
	if err != nil {
		return 0
	}
	return f
}

// metasoArtifactURL 按优先级容错提取产物：content.url → content[0].url → url → output.url。
func metasoArtifactURL(task *metasoTask) string {
	if len(task.Content) > 0 {
		var one struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(task.Content, &one); err == nil {
			if u := strings.TrimSpace(one.URL); u != "" {
				return u
			}
		}
		var many []struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(task.Content, &many); err == nil {
			for _, item := range many {
				if u := strings.TrimSpace(item.URL); u != "" {
					return u
				}
			}
		}
	}
	if u := strings.TrimSpace(task.URL); u != "" {
		return u
	}
	if task.Output != nil {
		return strings.TrimSpace(task.Output.URL)
	}
	return ""
}

// ---------------------------------------------------------------- 探活

// Test 探活：查一个必然不存在的任务号，用「路径是否命中 + 鉴权是否通过」判断渠道可用。
//
// 404 仍然判可达（任务不存在是正常语义）；但只有当响应体是 Metaso 错误信封时才这么判——
// 否则运营少写一段路径前缀（比如漏了 /api）也会被当成健康渠道，熔断的渠道会被自动恢复。
func (Metaso) Test(ctx context.Context, cfg *relay.ChannelConfig) error {
	if cfg == nil {
		return metasoCfgErr("channel", "渠道配置为空")
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return metasoCfgErr("keys", "渠道未配置密钥")
	}
	opts, err := parseMetasoOptions(cfg.Options)
	if err != nil {
		return err
	}
	target, err := relay.BuildRawURL(cfg, opts.queryPath+"__probe__")
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("适配器 metaso：构造请求失败：%w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(cfg.APIKey))

	resp, err := client(cfg).Do(req)
	if err != nil {
		return fmt.Errorf("上游请求失败：%w", err)
	}
	defer resp.Body.Close()
	buf, _ := readUpstreamBody(resp.Body, maxUpstreamBodyBytes)

	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("适配器 metaso：鉴权失败（HTTP %d），请检查渠道密钥：%s", resp.StatusCode, snippet(buf))
	case http.StatusNotFound:
		if _, _, isErr := metasoErrorEnvelope(buf); isErr {
			return nil // 任务不存在 = 路径与鉴权都对
		}
		return fmt.Errorf("适配器 metaso：查询接口路径不存在（HTTP 404），请检查 Base URL 与 query_path：%s", snippet(buf))
	case http.StatusMethodNotAllowed:
		return fmt.Errorf("适配器 metaso：查询接口方法不被支持（HTTP 405），请检查 query_path：%s", snippet(buf))
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	// 400/422/429 说明路径与鉴权都通了，只是这个探测请求本身不合法/被限流。
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity || resp.StatusCode == http.StatusTooManyRequests {
		return nil
	}
	return fmt.Errorf("适配器 metaso：探活失败（HTTP %d）：%s", resp.StatusCode, snippet(buf))
}

// ---------------------------------------------------------------- 错误归因

// metasoErrEnvelope 是 Metaso 错误信封的类型化视图。
//
// `error` 与 `http_code` 必须是 RawMessage：上游在不同版本/区域里把 `error` 写成
// 字符串、`{code,message}`、`{message}`，把 `http_code` 写成数字或数字字符串。
// 用 string/int 接的后果不是「解析这段失败」，而是**整个结构体解析失败**——
// 于是错误信封识别静默失效，一条 401 被当成「HTTP 200 但没有 task_id」，
// 归因成上游提交失败并 failover，把所有渠道挨个撞一遍。
type metasoErrEnvelope struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
	// Msg 与 Message 同义（不同区域/版本的差异），只看 Message 会漏掉整句原因。
	Msg      string          `json:"msg"`
	Error    json.RawMessage `json:"error"`
	HTTPCode json.RawMessage `json:"http_code"`
	BaseResp *struct {
		StatusCode int    `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	} `json:"base_resp"`
}

// metasoErrorEnvelope 识别 Metaso 的「200 但带错误体」形态。
//
// 判定条件（任一命中即错误）：`type == "error"` ∨ `error` 非空 ∨ `base_resp.status_code != 0`。
// 返回的 code 用于分档归因：0 表示信封里没给状态码（调用方按 400 处理）。
func metasoErrorEnvelope(buf []byte) (code int, msg string, ok bool) {
	var p metasoErrEnvelope
	if err := json.Unmarshal(buf, &p); err != nil {
		return 0, "", false
	}
	isErr := strings.EqualFold(strings.TrimSpace(p.Type), "error") ||
		metasoNonEmptyRaw(p.Error) ||
		(p.BaseResp != nil && p.BaseResp.StatusCode != 0)
	if !isErr {
		return 0, "", false
	}
	code = metasoHTTPCodeOf(p.HTTPCode)
	if code == 0 && p.BaseResp != nil && p.BaseResp.StatusCode >= 400 && p.BaseResp.StatusCode <= 599 {
		// base_resp.status_code 只有落在 HTTP 错误码区间时才当状态码用。
		// 这里常见的是业务码（1001 = 内容审核不通过），当成状态码会把
		// 「调用方的参数问题」误报成 1001，前端拿到一个不存在的 HTTP 语义。
		code = p.BaseResp.StatusCode
	}
	msg = firstNonEmptyStr(
		strings.TrimSpace(p.Message),
		strings.TrimSpace(p.Msg),
		errTextFromValue(p.Error),
		baseRespMsg(&p),
		strings.TrimSpace(p.Code),
		"上游返回业务错误",
	)
	return code, msg, true
}

func baseRespMsg(p *metasoErrEnvelope) string {
	if p.BaseResp == nil {
		return ""
	}
	return strings.TrimSpace(p.BaseResp.StatusMsg)
}

// metasoNonEmptyRaw 判断一个 RawMessage 是否「有内容」（null / 空串 / {} 视为无）。
func metasoNonEmptyRaw(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	switch s {
	case "", "null", `""`, "{}", "[]":
		return false
	}
	return true
}

// metasoHTTPCodeOf 从 `http_code` 里取状态码（数字 / 数字字符串 / {code} / {http_code}）。
func metasoHTTPCodeOf(raw json.RawMessage) int {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	var str string
	if err := json.Unmarshal([]byte(s), &str); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(str)); err == nil {
			return n
		}
	}
	var obj struct {
		Code     json.RawMessage `json:"code"`
		HTTPCode json.RawMessage `json:"http_code"`
	}
	if err := json.Unmarshal([]byte(s), &obj); err == nil {
		if n := metasoHTTPCodeOf(obj.HTTPCode); n > 0 {
			return n
		}
		if n := metasoHTTPCodeOf(obj.Code); n > 0 {
			return n
		}
	}
	return 0
}

// metasoEnvelopeError 把错误信封按 http_code 映射成与状态码一致的分档。
func metasoEnvelopeError(code int, msg string) error {
	if code <= 0 {
		code = http.StatusBadRequest
	}
	if code < 400 {
		code = http.StatusBadRequest
	}
	return metasoHTTPError(code, []byte(msg))
}

// metasoHTTPError 按状态码分档归因（与 AutoDL 同口径，另加 402 余额不足）。
func metasoHTTPError(status int, buf []byte) error {
	// 优先取信封里的那一句（用户能读懂），原文只在取不到时兜底。
	detail := upstreamDetail(buf, status)
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return &relay.CallerParamError{Msg: "Metaso 拒绝了本次参数（HTTP " + strconv.Itoa(status) + "）：" + detail}
	case http.StatusUnauthorized, http.StatusForbidden:
		return &relay.HTTPStatusError{Code: status, Note: "适配器 metaso：鉴权失败（HTTP " + strconv.Itoa(status) + "），请检查渠道密钥：" + detail}
	case http.StatusPaymentRequired:
		// 402 是「渠道上游账户没钱」：刻意**不**包成 HTTPStatusError。
		// 包了会让 relay 按 4xx 归因成「调用方问题」，渠道健康完全不受影响 →
		// 一条欠费渠道会永远留在候选里，每次请求都先撞它一次再 failover。
		// 保持普通错误（既有语义：无 Class() 一律记上游故障），攒够样本后由熔断自动下线。
		return fmt.Errorf("适配器 metaso：上游余额不足（HTTP 402），请充值后重试：%s", detail)
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return &relay.HTTPStatusError{Code: status, Note: "适配器 metaso：接口路径不存在（HTTP " + strconv.Itoa(status) + "），请检查 Base URL 与 query_path：" + detail}
	case http.StatusTooManyRequests:
		return &relay.HTTPStatusError{Code: status, Note: "适配器 metaso：上游限流（HTTP 429）：" + detail}
	default:
		return relay.StatusError(status, detail)
	}
}

// ---------------------------------------------------------------- 同步端点（不支持）

// Chat 不承接同步对话。
func (Metaso) Chat(context.Context, *relay.ChannelConfig, *relay.RelayRequest) (*relay.RelayResponse, error) {
	return nil, errMetasoNoEndpoint
}

// Embeddings 不承接向量请求。
func (Metaso) Embeddings(context.Context, *relay.ChannelConfig, *relay.RelayRequest) (*relay.RelayResponse, error) {
	return nil, errMetasoNoEndpoint
}

// Completions 不承接旧版补全。
func (Metaso) Completions(context.Context, *relay.ChannelConfig, *relay.RelayRequest) (*relay.RelayResponse, error) {
	return nil, errMetasoNoEndpoint
}

// StreamChat 不承接流式对话。
func (Metaso) StreamChat(context.Context, *relay.ChannelConfig, *relay.RelayRequest, func(relay.SSEFrame) error) (*relay.StreamResult, error) {
	return nil, errMetasoNoEndpoint
}

// Forward 不承接通用透传。
func (Metaso) Forward(context.Context, *relay.ChannelConfig, *relay.RelayRequest, string) (*relay.RawResponse, error) {
	return nil, errMetasoNoEndpoint
}

// metasoLocalOnly 是 metaso 出站时**不下发**的键：平台已消费或已改名。
//
//   - prompt 进 content 数组（metaso 的报文是 content[]，顶层没有 prompt）；
//   - seconds / duration 已由 plan 转成 content 里的时长或上游默认值；
//   - resolution / size / ratio / aspect_ratio 已归一成上游的 resolution；
//   - source_task_id 只用于选路（决定打 /regen 还是 /generate），上游不收；
//   - input_reference 已展开成 first_frame / last_frame / reference_image*；
//   - metadata 是平台容器（watermark 等提示位），上游没有这个键。
//
// 其余顶层键（厂商新增参数、客户端私有扩展）一律原样下发 —— 平台不替上游判断能力。
var metasoLocalOnly = []string{
	"prompt", "seconds", "duration", "resolution", "size", "ratio", "aspect_ratio",
	"source_task_id", "input_reference", "metadata", "model",
}

// metasoForwardUnmapped 把请求体里未被 plan 映射的顶层键补进上游报文。
func metasoForwardUnmapped(payload, raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return payload, nil
	}
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("适配器 metaso：解析组装结果失败：%w", err)
	}
	relay.ForwardUnmapped(out, raw, metasoLocalOnly...)
	return json.Marshal(out)
}
