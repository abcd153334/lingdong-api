// Package spec 是 L2 数据驱动适配器：把一份受限 JSON（adapter_specs.spec）编译成
// 实现 relay.Adapter / EndpointSupporter / TaskAdapter 的运行时对象。
//
// 设计边界（不可放宽，放宽即等于允许在数据库里写代码）：
//   - 没有循环、条件、函数、算术；只有「复制字段 / 塞常量 / 数组映射 / 删字段」四种动作；
//   - URL 只能是 `base_url + path`，path 不允许 `://`、`..`、控制字符；
//   - 请求头只能是固定白名单里的鉴权头，值只能是「前缀 + 密钥」；
//   - 响应只能按 `$.a.b` / `$.a[0].b` / `$.a[*].b` 三种路径取值，深度 ≤ 6。
package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/pkg/taskstate"
	"lingdong-api/internal/relay"
)

// 限额（与 schema 目录下发的 limits 同源，改动只改这里）。
const (
	MaxEndpoints    = 32
	MaxConstKeys    = 32
	MaxCopyKeys     = 32
	MaxArrays       = 8
	MaxArrayItems   = 16
	MaxStaticQuery  = 8
	MaxPathLen      = 512
	MaxSpecBytes    = 64 * 1024
	MaxSlugLen      = 32
	MaxLabelRunes   = 32
	PollKeySuffix   = "@poll"
	MaxJSONPathDeep = 6
	// MaxRequestFields 是一个端点最多声明几个「调用方可额外传」的字段。
	MaxRequestFields = 16
	// MaxRequestFieldEnum 是枚举类型字段的取值上限。
	MaxRequestFieldEnum = 32
	// MaxPathVars 是上游路径里最多几个占位符。
	MaxPathVars = 8
	// MaxFieldDescLen 是扩展字段说明的字数上限（它会进公网文档）。
	MaxFieldDescLen = 200
	// MaxSelectorWhere 是产物过滤条件的条数上限。
	MaxSelectorWhere = 4
)

// 异步规格的两种形态（AsyncSpec.Mode）。
const (
	// AsyncModeTask 是默认形态：提交只拿上游任务号，平台落 tasks 表，
	// 由 worker 按统一节奏轮询 —— 适合视频/音乐这类分钟级任务。
	AsyncModeTask = "task"
	// AsyncModeBridge 是**同步桥接**：提交后由本适配器在本次请求内自旋等待，
	// 拿到产物后直接作为同步响应返回。
	//
	// 为什么必须有它：大量厂商的 TTS/图像接口只有「提交 + 查询」两个端点，
	// 而 OpenAI 契约里 /v1/audio/speech 是同步二进制。没有桥接时这类厂商
	// 只能被写成内置适配器（改代码），规格 DSL 完全用不上 —— 这正是
	// 「AI 无法完成非标 API 接入」的结构性原因。
	AsyncModeBridge = "bridge"
)

// 桥接等待的默认值与边界（保存期校验与运行期共用同一份常量）。
const (
	// DefaultBridgeTimeoutSecs 是未声明 timeout_secs 时的等待上限。
	// 120s 覆盖 5 万字以内的 TTS（实测约 1 分钟），同时远小于 nginx 的 proxy_read_timeout。
	DefaultBridgeTimeoutSecs = 120
	// MaxBridgeTimeoutSecs 是等待上限的硬顶（必须小于反向代理超时，否则用户拿到代理的 504）。
	//
	// **改这个值必须同时改 worker.usageReclaimTTL**（要求 TTL > 本值 + 结算余量）：
	// TTL 短于等待上限时，在途请求会被回收任务提前结算，真结算静默失败。
	MaxBridgeTimeoutSecs    = 900
	DefaultBridgeIntervalMs = 1000
	MinBridgeIntervalMs     = 200
	MaxBridgeIntervalMs     = 5000
	// BridgeMaxArtifactBytes 是桥接下载产物的上限（音频/图片，32 MiB）。
	// 与内置 openai 适配器的 maxTTSBodyBytes 同值：两条链路对"多大算超限"必须一致。
	// BridgeMaxArtifactBytes 与二进制端点的读取上限同源（relay.MaxBinaryBodyBytes），
	// 两边各写一个 32MiB 的话，调大时必然漏掉其中一处。
	BridgeMaxArtifactBytes = relay.MaxBinaryBodyBytes
)

// StatusStates 是轮询状态全集（status_map 的键）。导出给字段字典用：
// 枚举只能有一处定义，否则文档写 four states 与实际校验分叉。
//
// canceled 是第五态：上游「已取消」既不是成功也不是失败，混进 failed 会让
// 台账分不清「用户取消」与「上游出错」，而取消是不该记渠道故障的。
var StatusStates = append([]string(nil), taskstate.All...)

// SlugPattern 是规格 slug 的格式（进 channels.adapter_id 的 spec:<slug>）。
// 导出后由 slugRe 编译、由字段字典下发 —— 两处用同一个字符串。
const SlugPattern = `^[a-z][a-z0-9-]{1,31}$`

// 鉴权头白名单：只允许这些头名承载渠道密钥。
//
// 为什么必须白名单：规格是运营/AI 写的数据，若允许任意头名，就能写
// `Host`、`X-Forwarded-For`、`Content-Length` 这类会改变请求语义的头，
// 等于把 HTTP 客户端的一部分控制权交给数据。
var allowedAuthHeaders = map[string]bool{
	"authorization":     true,
	"x-api-key":         true,
	"api-key":           true,
	"x-goog-api-key":    true,
	"x-dashscope-async": true,
	"x-auth-token":      true,
	"token":             true,
	"apikey":            true,
	"x-token":           true,
	"x-access-token":    true,
	"x-key":             true,
	"access-token":      true,
}

var (
	slugRe      = regexp.MustCompile(SlugPattern)
	identRe     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	jsonPath    = regexp.MustCompile(`^\$(\.[A-Za-z_][A-Za-z0-9_]*|\[[0-9]+\]|\[\*\])+$`)
	reqRefRe    = regexp.MustCompile(`^\$req\.[A-Za-z_][A-Za-z0-9_.]*$`)
	safePath    = regexp.MustCompile(`^/[A-Za-z0-9._~!$&'()*+,;=:@%/{}<>-]*$`)
	clampName   = regexp.MustCompile(`[^A-Za-z0-9_.-]`)
	fieldNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)
	pathVarRe   = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	// pathVarValueRe 是**占位符取值**的白名单：路径段里不许出现斜杠、问号、井号、点段。
	pathVarValueRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~:@-]{0,127}$`)
)

// Spec 是一份规格的完整形状（对应 adapter_specs.spec 列）。
type Spec struct {
	// Slug 是规格的稳定标识，进 channels.adapter_id（spec:<slug>）；创建后不可改。
	Slug  string `json:"slug"`
	Label string `json:"label"`
	// Vendor 是 vendors.id，只用于展示与归组，不参与选路。
	Vendor    string              `json:"vendor,omitempty"`
	Endpoints map[string]Endpoint `json:"endpoints"`
}

// Endpoint 是一个端点的请求/响应映射规则。
type Endpoint struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	// Auth 缺省沿用规格级（本实现只支持端点级，缺省 = 用渠道密钥放在 Authorization: Bearer）。
	Auth *Auth `json:"auth,omitempty"`
	// Query 是常量查询参数（不超过 8 个）。
	Query map[string]string `json:"query,omitempty"`
	// RequestFields 声明「调用方在本接入上可以额外传哪些参数」。
	//
	// 它不是搬运规则：真正发给上游仍由 body.copy / body.arrays 与 path 占位符决定。
	// 它只解决三件事 —— 文档字段表要显示、调试台要出控件、缺必填要在平台侧拦下
	// （而不是等上游 400，再被折成一句「服务暂时不可用」）。
	RequestFields []RequestField `json:"request_fields,omitempty"`
	// Body 是请求体重建规则（同步端点用）。
	Body *BodySpec `json:"body,omitempty"`
	// Passthrough 表示「原样转发请求体」：仅 chat/completions 允许。
	//
	// 为什么只有 chat 允许：对话请求体里混着 tools/stream/stop 等十几项，
	// 逐字段映射既写不完也容易漏；而 OpenAI 兼容对话的差异通常只在域名与鉴权。
	// 非兼容的对话协议属于 L3，必须写内置适配器。
	Passthrough bool `json:"passthrough,omitempty"`
	// Extract 是响应取值规则。
	Extract Extract `json:"extract,omitempty"`
	// Errors 是成功判据（缺省 = HTTP 2xx）。
	Errors *ErrSpec `json:"errors,omitempty"`
	// Async 只出现在 submit 端点上：声明轮询端点与节奏。
	Async *AsyncSpec `json:"async,omitempty"`
	// StatusMap 只出现在 poll 端点上：上游状态值 → 平台四态。
	StatusMap map[string][]string `json:"status_map,omitempty"`
	// ContentType 覆盖请求体类型（缺省 application/json）。
	ContentType string `json:"content_type,omitempty"`
	// OmitModel 表示**不写**上游模型名字段。
	//
	// 缺省行为是「一律覆盖 model」（见 setUpstreamModel）：上游只认自己的型号名，
	// 透传平台模型 ID 会 404。但一部分厂商（如自建 TTS 服务）的请求体里根本没有
	// model 字段，多塞一个未知键会被严格校验的上游 400 —— 那类端点必须能声明"别写"。
	OmitModel bool `json:"omit_model,omitempty"`
	// ResponseType 是桥接下载体缺少 Content-Type 时使用的响应类型
	// （如 audio/wav）。有上游头时以上游为准，这里只是兜底。
	ResponseType string `json:"response_content_type,omitempty"`
	// Binary 由端点事实派生（不来自 JSON）：该端点的响应体是二进制（音频/图片）。
	//
	// 为什么需要它：同步二进制端点必须按产物上限读（32 MiB）、并把上游 Content-Type
	// 原样回给调用方。按 JSON 的 8 MiB 上限与 application/json 处理的话，
	// 长音频会被判"上游体过大"，短音频则被标成 JSON —— 播放器一个都打不开。
	Binary bool `json:"-"`
	// AcceptsAsync 由端点事实派生（不来自 JSON）：该端点默认同步、可加 async:true 转任务。
	//
	// 为什么需要它：这类端点的**两条形态共用一份声明**。声明 mode=task 表示
	// "async:true 交给平台任务队列"，但同一个路径在不传 async 时仍必须给出同步结果，
	// 所以同步面要桥接（见 forward）。判据只在 apidoc 的端点表里，规格不能自己声明。
	AcceptsAsync bool `json:"-"`
}

// Auth 描述渠道密钥放在哪里。
type Auth struct {
	// In: header | query | body；缺省 header。
	In     string `json:"in,omitempty"`
	Name   string `json:"name,omitempty"`
	Prefix string `json:"prefix,omitempty"`
}

// RequestField 是一个「本接入扩展字段」的声明。
//
// 为什么需要它：平台契约（apidoc.Field）是按端点固定的，而每一种非标上游都会
// 多要几个参数（参考音频地址、情绪权重、工作流参数）。没有它时，规格只能把
// 这些值猜着映射到已有字段上（真实事故：把「音色名」当「参考音频 URL」发出去），
// 而文档完全不会提到这些参数，用户只能靠试错。
//
// 它只被三处消费：文档字段投影、调试台表单、平台侧必填/类型校验。
type RequestField struct {
	// Name 是调用方传的字段名（小写字母开头的标识符）。
	Name string `json:"name"`
	// Type: string | number | integer | boolean | url。
	Type string `json:"type"`
	// Required 表示缺了就无法调用（与 Default 互斥）。
	Required bool `json:"required,omitempty"`
	// Default 是缺省值（类型必须与 Type 一致）。
	Default any `json:"default,omitempty"`
	// Enum 是枚举取值（仅 string）。
	Enum []string `json:"enum,omitempty"`
	// Desc 是给用户看的说明（进公网文档，不得写上上游地址或密钥前缀）。
	Desc string `json:"desc,omitempty"`
	// Example 是文档示例值。
	Example any `json:"example,omitempty"`
	// MaxLen 是字符串/URL 的最大长度（0 = 不限制）。
	MaxLen int `json:"max_len,omitempty"`
}

// 扩展字段的类型枚举（校验与字段字典共用同一份）。
var RequestFieldTypes = []string{"string", "number", "integer", "boolean", "url"}

// URLSelector 是「从响应里取产物链接」的规则。
//
// 两种 JSON 形态（自定义 Unmarshal 兼容，落库统一为对象形态）：
//
//	"result_urls": "$.data.results[*].url"
//	"result_urls": {"path":"$.data.results[*].url","where":{"type":"audio"}}
//
// where 只做「元素内字段精确相等」的过滤，不支持表达式：一旦有条件表达式，
// 这套 DSL 就变成「数据库里存代码」，评审与安全都失控。
type URLSelector struct {
	Path  string            `json:"path"`
	Where map[string]string `json:"where,omitempty"`
}

// IsZero 判断选择器是否为空（沿用 string 形态的空值语义）。
func (s *URLSelector) IsZero() bool { return s == nil || strings.TrimSpace(s.Path) == "" }

// UnmarshalJSON 同时接受字符串与对象。
func (s *URLSelector) UnmarshalJSON(raw []byte) error {
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		*s = URLSelector{Path: one}
		return nil
	}
	type alias URLSelector
	var obj alias
	if err := json.Unmarshal(raw, &obj); err == nil {
		*s = URLSelector(obj)
		return nil
	}
	return errors.New(`result_urls 必须是 "$.a[*].url" 或 {"path":"...","where":{...}}`)
}

// BodySpec 是请求体重建规则。
type BodySpec struct {
	Const  map[string]any        `json:"const,omitempty"`
	Copy   map[string]CopySource `json:"copy,omitempty"`
	Arrays map[string]ArraySpec  `json:"arrays,omitempty"`
	Drop   []string              `json:"drop,omitempty"`
}

// MaxCopyCandidates 是一个目标字段最多几个候选来源。
const MaxCopyCandidates = 4

// CopySource 是一个目标字段的候选来源列表（按顺序取第一个**存在**的）。
//
// 为什么需要候选链：同一语义在不同客户端里有不同字段名 ——
// 音色参考既可能是 `reference_audio`（自定义字段）也可能是 `voice`（OpenAI 标准），
// 而规格只能声明一份映射。只允许单来源时，另一种客户端会 400「缺少字段」，
// 而运营看到的却是"配置明明写了"。
//
// JSON 形状两种都接受：`"copy":{"a":"$req.x"}` 与 `"copy":{"a":["$req.x","$req.y"]}`。
type CopySource []string

// UnmarshalJSON 同时接受字符串与字符串数组。
func (c *CopySource) UnmarshalJSON(raw []byte) error {
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		*c = CopySource{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		*c = CopySource(many)
		return nil
	}
	return errors.New(`copy 的来源必须是 "$req.<字段>" 或它的数组`)
}

// ArraySpec 把请求里的一个数组映射成上游的数组结构。
type ArraySpec struct {
	// From 只能是 `$req.<字段>`。
	From string `json:"from"`
	// Max 是该数组最多取几项（上游对张数通常有硬上限）。
	Max int `json:"max,omitempty"`
	// Item 是每个元素的目标形状，值只能用 `$value`。
	Item map[string]string `json:"item"`
}

// Extract 是响应取值规则（`$.a.b` / `$.a[0].b` / `$.a[*].b`）。
type Extract struct {
	// TaskID 用于 submit 端点：从响应里取上游任务号。
	TaskID string `json:"task_id,omitempty"`
	// Status / Progress / Error 用于 poll 端点。
	Status   string `json:"status,omitempty"`
	Progress string `json:"progress,omitempty"`
	Error    string `json:"error,omitempty"`
	// Results 是多产物链接（`$.data[*].url`），顺序即产物顺序。
	// 带 where 时按元素内字段过滤（例如只取 type=audio 的那一条）。
	Results *URLSelector `json:"result_urls,omitempty"`
	// Result 是同步端点的单产物链接（`$.data[0].url`）。
	Result string `json:"result,omitempty"`
	// Text 是同步端点的文本结果（歌词等）。
	Text string `json:"text,omitempty"`
	// Duration 是**产物时长**（秒）的取值路径，供按秒计费。
	// 上游多数返回小数（1.234），平台按四舍五入取整秒。
	//
	// 铁律：这里必须是「产物时长」，**不是任务耗时**。有的上游（如 AutoDL ComfyUI）
	// 在查询响应里给的是 duration=任务执行秒数，接进来会把 4 秒音频按 196 秒计费。
	// 分不清时宁可不配——桥接回的是完整音频，平台会自己探测时长。
	Duration string `json:"duration,omitempty"`
	// DurationUnit 是 duration 的单位：secs（缺省）| ms。
	//
	// 只有声明了 duration 才有意义（否则是永远不生效的开关，校验会拒绝）。
	DurationUnit string `json:"duration_unit,omitempty"`
}

// ErrSpec 是成功判据与错误信息提取。
type ErrSpec struct {
	// Success: http_2xx | body_code。
	Success     string   `json:"success,omitempty"`
	CodePath    string   `json:"code_path,omitempty"`
	OKCodes     []string `json:"ok_codes,omitempty"`
	MessagePath string   `json:"message_path,omitempty"`
}

// AsyncSpec 声明提交端点提交后如何取得结果。
//
// 两种形态（Mode）：
//   - task（默认）：只声明「轮询哪个端点」，节奏由平台统一调度
//     （worker/task_polling.go 的批次与 TTL）。给单渠道配独立节奏会绕开那套限流与公平性，
//     结果是一条渠道就能把轮询名额占满、其它渠道的任务集体饥饿。
//   - bridge：本适配器在**当前请求内**提交并轮询到终态，把产物作为同步响应返回。
//     只有这种形态需要 timeout_secs / interval_ms —— 它是这次 HTTP 请求的等待预算，
//     不参与平台的任务调度，因此不会与 worker 抢名额。
type AsyncSpec struct {
	// Mode: task | bridge；空串等价于 task（老规格不受影响）。
	Mode string `json:"mode,omitempty"`
	// Poll 指向同一规格里的 poll 端点键（`<endpoint>@poll`）。
	Poll string `json:"poll"`
	// TimeoutSecs 是 bridge 的等待上限（秒）；0 = 用默认值。
	TimeoutSecs int `json:"timeout_secs,omitempty"`
	// IntervalMs 是 bridge 的轮询间隔（毫秒）；0 = 用默认值。
	IntervalMs int `json:"interval_ms,omitempty"`
}

// IsBridge 表示这是同步桥接形态。
func (a *AsyncSpec) IsBridge() bool { return a != nil && a.Mode == AsyncModeBridge }

// IsTask 表示这是平台调度的任务形态（空 Mode 也是）。
func (a *AsyncSpec) IsTask() bool { return a != nil && a.Mode != AsyncModeBridge }

// Decode 严格解析规格 JSON：未知键、类型不符都返回错误。
func Decode(raw []byte) (*Spec, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("规格内容为空")
	}
	if len(raw) > MaxSpecBytes {
		return nil, fmt.Errorf("规格超过 %d KB 上限", MaxSpecBytes/1024)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var s Spec
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("规格不是合法的 JSON 结构：%v", err)
	}
	return &s, nil
}

// PollKey 返回某端点对应的轮询端点键。
func PollKey(endpointKey string) string { return endpointKey + PollKeySuffix }

// BaseKey 把 poll 键还原成 submit 键；非 poll 键原样返回。
func BaseKey(key string) string { return strings.TrimSuffix(key, PollKeySuffix) }

// IsPollKey 判断键是否轮询端点。
func IsPollKey(key string) bool { return strings.HasSuffix(key, PollKeySuffix) }

// Validate 校验规格（保存期强校验；所有失败都带字段名，便于管理台定位）。
func (s *Spec) Validate() error {
	if !slugRe.MatchString(s.Slug) {
		return fieldErr("slug", "规格标识需为小写字母开头、2–32 位（小写字母/数字/连字符）")
	}
	if strings.TrimSpace(s.Label) == "" {
		return fieldErr("label", "请填写规格名称")
	}
	if len([]rune(s.Label)) > MaxLabelRunes {
		return fieldErr("label", fmt.Sprintf("规格名称不超过 %d 个字", MaxLabelRunes))
	}
	if len(s.Endpoints) == 0 {
		return fieldErr("endpoints", "至少声明一个端点")
	}
	if len(s.Endpoints) > MaxEndpoints {
		return fieldErr("endpoints", fmt.Sprintf("端点最多 %d 个", MaxEndpoints))
	}

	keys := make([]string, 0, len(s.Endpoints))
	for k := range s.Endpoints {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// 两轮而不是一轮：轮询端点的合法性依赖「提交端点声明了什么 async」，
	// 一轮遍历里先遇到 @poll 就看不到提交端点的规则（字典序碰巧成立不算设计）。
	submits := make([]string, 0, len(keys))
	polls := make([]string, 0, 2)
	for _, key := range keys {
		if IsPollKey(key) {
			polls = append(polls, key)
		} else {
			submits = append(submits, key)
		}
	}
	for _, key := range submits {
		if err := s.validateEndpoint(key, s.Endpoints[key]); err != nil {
			return err
		}
		if err := s.validateAsync(key, s.Endpoints[key]); err != nil {
			return err
		}
	}
	for _, key := range polls {
		if err := s.validateEndpoint(key, s.Endpoints[key]); err != nil {
			return err
		}
		if !s.pollReferenced(key, submits) {
			return fieldErr("endpoints."+key, "轮询端点没有被任何提交端点引用")
		}
	}
	return nil
}

// pollReferenced 判断轮询端点是否被某个提交端点的 async.poll 引用。
//
// 孤儿 poll 永远不会被调用：配置看起来"完整"，运行期却永远拿不到结果，
// 只能靠这条校验在保存期挡住。
func (s *Spec) pollReferenced(pollKey string, submits []string) bool {
	for _, base := range submits {
		if ep := s.Endpoints[base]; ep.Async != nil && ep.Async.Poll == pollKey {
			return true
		}
	}
	return false
}

// validateAsync 校验提交端点的 async 声明（形态与端点相位必须相容）。
//
// 三条规则，每条都对应一个真实故障：
//  1. 异步提交端点必须声明 async.poll —— 否则提交成功但永远取不回结果；
//  2. 异步提交端点**不能**用 bridge —— 那是分钟级任务，一个 HTTP 请求等不到，
//     必须交给 worker 的任务队列；
//  3. 同步端点声明 async 时必须 bridge —— 同步端点没有 tasks 行可挂，
//     声明成 task 会写出一条永远不会被轮询的任务。
func (s *Spec) validateAsync(key string, ep Endpoint) error {
	fact, ok := apidoc.SpecEndpointByKey(key)
	if !ok {
		return nil // validateEndpoint 已给出"不在允许清单"的精确错误
	}
	prefix := "endpoints." + key
	if ep.Async == nil {
		if fact.Async {
			return fieldErr(prefix+".async",
				fmt.Sprintf("%s 是异步端点，必须声明 async.poll（否则提交后无法取回结果）", key))
		}
		return nil
	}
	if ep.Async.Mode != "" && ep.Async.Mode != AsyncModeTask && ep.Async.Mode != AsyncModeBridge {
		return fieldErr(prefix+".async.mode", "mode 只能是 task（平台排队轮询）或 bridge（本次请求内等待）")
	}
	if ep.Async.Poll != PollKey(key) {
		return fieldErr(prefix+".async.poll", fmt.Sprintf("轮询端点必须是 %q", PollKey(key)))
	}
	if _, ok := s.Endpoints[ep.Async.Poll]; !ok {
		return fieldErr(prefix+".async.poll", fmt.Sprintf("轮询端点 %q 未在规格中声明", ep.Async.Poll))
	}
	if fact.Async && ep.Async.IsBridge() {
		return fieldErr(prefix+".async.mode",
			fmt.Sprintf("%s 是异步提交端点，只能 mode=task（由平台任务队列轮询）；bridge 只用于同步端点", key))
	}
	if !fact.Async && !fact.AcceptsAsync && !ep.Async.IsBridge() {
		return fieldErr(prefix+".async.mode",
			fmt.Sprintf("%s 是同步端点，声明 async 时必须 mode=bridge（提交后在本次请求内等到结果）", key))
	}
	if ep.Async.IsBridge() {
		return s.validateBridge(key, ep)
	}
	// 走到这里只剩 task 形态。两步响应形状的端点（AcceptsAsync，目前只有
	// images/generations）在**不传 async 时**仍然是同步调用，那份等待由同一条
	// poll 声明支撑 —— 所以它的必备条件与 bridge 完全相同，必须一起校验。
	// 只校验 task 形态的话，保存能过、同步调用时才发现 poll 没声明 result。
	if fact.AcceptsAsync {
		return s.validateBridge(key, ep)
	}
	// task 形态的节奏由 worker 统一调度：允许 timeout_secs / interval_ms
	// 等于给运营一个永远不生效的开关（他以为调快了，实际毫无变化）。
	if ep.Async.TimeoutSecs != 0 || ep.Async.IntervalMs != 0 {
		return fieldErr(prefix+".async",
			"timeout_secs / interval_ms 只属于 mode=bridge；task 形态的轮询节奏由平台统一调度")
	}
	return nil
}

// validateBridge 校验同步桥接的必备条件。
func (s *Spec) validateBridge(key string, ep Endpoint) error {
	prefix := "endpoints." + key
	if ep.Extract.TaskID == "" {
		return fieldErr(prefix+".extract.task_id", "bridge 形态必须声明 extract.task_id（从提交响应里取上游任务号）")
	}
	// 产物交付形状只认端点事实：binary 端点回字节，图像生成端点回 JSON 直链。
	// 其余 JSON 契约（rerank / moderation / embeddings）无法推导产物结构，
	// 允许桥接等于让适配器猜一个形状出来。
	fact, ok := apidoc.SpecEndpointByKey(key)
	if ok && fact.Artifact() == apidoc.ArtifactNone {
		return fieldErr(prefix+".async.mode",
			fmt.Sprintf("%s 的响应契约无法承载桥接产物（仅二进制端点与图像生成端点支持 bridge）", key))
	}
	pollPrefix := "endpoints." + ep.Async.Poll
	pollEP := s.Endpoints[ep.Async.Poll]
	if pollEP.Extract.Result == "" && pollEP.Extract.Results.IsZero() {
		return fieldErr(pollPrefix+".extract", "桥接必须声明 result 或 result_urls（产物链接的来源）")
	}
	if d := ep.Async.TimeoutSecs; d != 0 && (d < 1 || d > MaxBridgeTimeoutSecs) {
		return fieldErr(prefix+".async.timeout_secs", fmt.Sprintf("超时上限需在 1–%d 秒之间", MaxBridgeTimeoutSecs))
	}
	if i := ep.Async.IntervalMs; i != 0 && (i < MinBridgeIntervalMs || i > MaxBridgeIntervalMs) {
		return fieldErr(prefix+".async.interval_ms",
			fmt.Sprintf("轮询间隔需在 %d–%d 毫秒之间", MinBridgeIntervalMs, MaxBridgeIntervalMs))
	}
	return nil
}

// validateRequestFields 校验「本接入扩展字段」的声明。
//
// 这里有一条容易漏但很关键的规则：**声明了就必须被消费**。否则会出现
// 「文档里写了 reference_audio、用户也填了，但规格根本没把它发给上游」——
// 请求成功、结果全错，而且没有任何报错指向配置。
func validateRequestFields(prefix, key string, ep Endpoint) error {
	base := prefix + ".request_fields"
	if len(ep.RequestFields) > MaxRequestFields {
		return fieldErr(base, fmt.Sprintf("扩展字段最多 %d 个", MaxRequestFields))
	}
	contract := contractFieldNames(key)
	seen := map[string]bool{}
	for i, f := range ep.RequestFields {
		at := fmt.Sprintf("%s[%d]", base, i)
		name := strings.TrimSpace(f.Name)
		if !strings.HasPrefix(name, f.Name) || name == "" {
			return fieldErr(at+".name", "字段名不能为空")
		}
		if !fieldNameRe.MatchString(name) {
			return fieldErr(at+".name", "字段名需小写字母开头，只含小写字母/数字/下划线（2–32 位）")
		}
		if seen[name] {
			return fieldErr(at+".name", fmt.Sprintf("字段 %q 重复声明", name))
		}
		seen[name] = true
		if name == "upstream_model" {
			return fieldErr(at+".name", "upstream_model 是保留占位符，不能作为扩展字段")
		}
		if contract[name] || reservedRequestKeys[name] {
			return fieldErr(at+".name", fmt.Sprintf("%s 与平台契约字段同名", name))
		}
		if !containsStr(RequestFieldTypes, f.Type) {
			return fieldErr(at+".type", "type 只能是 string / number / integer / boolean / url")
		}
		if len([]rune(f.Desc)) > MaxFieldDescLen {
			return fieldErr(at+".desc", fmt.Sprintf("说明不超过 %d 字", MaxFieldDescLen))
		}
		if leaksUpstreamHint(f.Desc) {
			return fieldErr(at+".desc", "说明里不要写上上游地址或密钥前缀（它会进公网文档）")
		}
		if f.Type == "url" && strings.TrimSpace(f.Desc) == "" {
			return fieldErr(at+".desc", "url 类型必须写清用途（用户要知道该填什么地址）")
		}
		if len(f.Enum) > 0 {
			if f.Type != "string" {
				return fieldErr(at+".enum", "只有 string 类型可以声明 enum")
			}
			if len(f.Enum) > MaxRequestFieldEnum {
				return fieldErr(at+".enum", fmt.Sprintf("枚举取值最多 %d 个", MaxRequestFieldEnum))
			}
			es := map[string]bool{}
			for _, v := range f.Enum {
				if strings.TrimSpace(v) == "" {
					return fieldErr(at+".enum", "枚举取值不能为空")
				}
				if es[v] {
					return fieldErr(at+".enum", fmt.Sprintf("枚举取值 %q 重复", v))
				}
				es[v] = true
			}
		}
		if f.Default != nil {
			if f.Required {
				return fieldErr(at+".default", "必填字段不能有默认值（否则必填永不触发）")
			}
			if err := checkFieldValueType(f, f.Default); err != nil {
				return fieldErr(at+".default", err.Error())
			}
		}
		if f.MaxLen < 0 {
			return fieldErr(at+".max_len", "max_len 不能为负数")
		}
		if !requestFieldConsumed(ep, name) {
			return fieldErr(at, fmt.Sprintf("字段 %q 声明了但不会发给上游（需出现在 body.copy/arrays 或 path 占位符里）", name))
		}
		if ep.Body != nil {
			if _, ok := ep.Body.Const[name]; ok {
				return fieldErr(at+".name", fmt.Sprintf("%s 与 body.const 同名，常量会覆盖它", name))
			}
		}
	}
	return nil
}

// contractFieldNames 返回某端点的平台契约字段名（唯一来源：apidoc 端点文档）。
func contractFieldNames(key string) map[string]bool {
	out := map[string]bool{}
	fact, ok := apidoc.SpecEndpointByKey(key)
	if !ok {
		return out
	}
	doc, ok := apidoc.EndpointDocOf(fact.Kind)
	if !ok {
		return out
	}
	for _, f := range doc.Fields {
		out[f.Name] = true
	}
	return out
}

// reservedRequestKeys 是无契约字段的端点上也要挡住的平台保留字段。
//
// 只在契约表没有该名字时兜底：有契约时以契约为准（单一来源），
// 这里只是为了不让规格凭空造出一个和平台语义冲突的字段。
var reservedRequestKeys = map[string]bool{
	"model": true, "stream": true, "user": true, "n": true,
	"size": true, "input": true, "prompt": true, "response_format": true,
}

// requestFieldConsumed 判断扩展字段是否真的会出现在上游请求里。
func requestFieldConsumed(ep Endpoint, name string) bool {
	if ep.Body != nil {
		for _, sources := range ep.Body.Copy {
			for _, s := range sources {
				if strings.TrimSuffix(s, "."+name) != s || strings.TrimPrefix(s, "$req.") == name {
					return true
				}
			}
		}
		for _, arr := range ep.Body.Arrays {
			if strings.TrimPrefix(arr.From, "$req.") == name {
				return true
			}
		}
	}
	for _, v := range pathPlaceholders(ep.Path) {
		if v == name {
			return true
		}
	}
	return false
}

// pathPlaceholders 取出 path 里的占位符名（按出现顺序）。
func pathPlaceholders(p string) []string {
	out := make([]string, 0, 2)
	for _, m := range pathVarRe.FindAllStringSubmatch(p, -1) {
		out = append(out, m[1])
	}
	return out
}

func containsStr(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// leaksUpstreamHint 判断说明文字是否泄漏了上游地址/密钥前缀。
func leaksUpstreamHint(s string) bool {
	l := strings.ToLower(s)
	for _, bad := range []string{"http://", "https://", "/api/", "sk-", "bearer "} {
		if strings.Contains(l, bad) {
			return true
		}
	}
	return false
}

func (s *Spec) validateEndpoint(key string, ep Endpoint) error {
	prefix := "endpoints." + key
	poll := IsPollKey(key)

	var fact apidoc.SpecEndpoint
	if poll {
		baseKey := BaseKey(key)
		baseFact, ok := apidoc.SpecEndpointByKey(baseKey)
		if !ok {
			return fieldErr(prefix, fmt.Sprintf("轮询端点 %q 没有对应的已知端点", key))
		}
		// 允许轮询的三种提交端点：原生异步（PhaseSubmit）、声明了同步桥接的同步端点、
		// 以及两步响应端点（AcceptsAsync，声明了 mode=task 时同步面仍要靠这条 poll 等结果）。
		baseEP, declared := s.Endpoints[baseKey]
		bridge := declared && baseEP.Async.IsBridge()
		twoFaced := declared && baseEP.Async.IsTask() && baseFact.AcceptsAsync
		if !baseFact.Async && !bridge && !twoFaced {
			return fieldErr(prefix, fmt.Sprintf("%s 不是异步端点，也没有声明 async.mode=bridge，不能有轮询端点", baseKey))
		}
		fact = baseFact
		fact.Async = false
	} else {
		f, ok := apidoc.SpecEndpointByKey(key)
		if !ok {
			return fieldErr(prefix, fmt.Sprintf("端点 %q 不在允许清单内（可用端点见 schema 目录）", key))
		}
		if !f.SpecSupported {
			return fieldErr(prefix, fmt.Sprintf("端点 %q 不能由规格承载：%s", key, f.UnsupportedReason))
		}
		fact = f
	}

	// method / path
	method := strings.ToUpper(strings.TrimSpace(ep.Method))
	if method == "" {
		method = fact.Method
	}
	if method != "POST" && method != "GET" {
		return fieldErr(prefix+".method", "method 只能是 POST 或 GET")
	}
	// 扩展字段必须**先**校验：path 里的占位符来源就是它们的名字。
	// 轮询端点不接收调用方参数，所以在下面单独拒绝（不在这里校验"是否被消费"）。
	pathVars := map[string]bool{"upstream_model": true}
	if len(ep.RequestFields) > 0 && !poll {
		if err := validateRequestFields(prefix, key, ep); err != nil {
			return err
		}
		for _, f := range ep.RequestFields {
			pathVars[f.Name] = true
		}
	}
	if err := validateUpstreamPath(ep.Path, poll, pathVars); err != nil {
		return fieldErr(prefix+".path", err.Error())
	}
	// 规格只能重建 JSON 请求体；multipart 端点本身 SpecSupported=false，
	// 允许 content_type=multipart 等于给运营一个永远走不到的分支。
	if ep.ContentType != "" && ep.ContentType != "application/json" {
		return fieldErr(prefix+".content_type", "content_type 只支持 application/json")
	}

	// auth
	if ep.Auth != nil {
		if err := validateAuth(*ep.Auth); err != nil {
			return fieldErr(prefix+".auth", err.Error())
		}
		// body 鉴权写在 buildBody 里，而没有 body 规则时 buildBody 根本不会被调用
		// （见 adapter.forward）。缺了这条校验，配置看起来完全正常，请求里却没有密钥，
		// 上游 401 会被记成「渠道故障」。
		if strings.EqualFold(strings.TrimSpace(ep.Auth.In), "body") && ep.Body == nil {
			return fieldErr(prefix+".auth.in",
				"body 鉴权需要同时声明 body 规则（否则密钥不会被写进请求体）")
		}
	}
	if len(ep.Query) > MaxStaticQuery {
		return fieldErr(prefix+".query", fmt.Sprintf("常量查询参数最多 %d 个", MaxStaticQuery))
	}
	for k := range ep.Query {
		if !identRe.MatchString(k) {
			return fieldErr(prefix+".query", fmt.Sprintf("查询参数名 %q 不合法", k))
		}
	}

	// body
	//
	// passthrough 对**所有非轮询端点**开放：它的语义是「映射之后，未声明的字段原样
	// 转发」，与平台自身的透传口径一致。此前只允许 chat/completions，导致运营在
	// 图片/视频渠道上无路可走 —— 只能逐字段声明，上游一加参数就得改配置。
	if ep.Passthrough && poll {
		return fieldErr(prefix+".passthrough", "轮询端点不接受 passthrough（结果由 extract 解析）")
	}
	if ep.Body != nil {
		if err := validateBody(prefix+".body", *ep.Body); err != nil {
			return err
		}
		if !ep.Passthrough && len(ep.Body.Drop) > 0 {
			// Body 非透传时是「白名单重建」，请求里没有的字段本来就不会被复制，
			// drop 无从生效 —— 允许它等于给运营一个永远不报错的死开关。
			return fieldErr(prefix+".body.drop", "drop 只能与 passthrough 一起使用（非透传模式按白名单重建请求体）")
		}
	}

	// extract / errors / status_map
	if poll {
		if ep.Passthrough || ep.Body != nil {
			return fieldErr(prefix, "轮询端点不接受 body/passthrough 规则")
		}
		if len(ep.RequestFields) > 0 {
			return fieldErr(prefix+".request_fields", "轮询端点不接受 request_fields（它不接收调用方参数）")
		}
		if ep.Async != nil {
			return fieldErr(prefix+".async", "轮询端点不能再声明 async")
		}
		if err := validateStatusMap(prefix+".status_map", ep.StatusMap); err != nil {
			return err
		}
		if ep.Extract.Status == "" {
			return fieldErr(prefix+".extract.status", "轮询端点必须声明 extract.status")
		}
	} else if fact.Async {
		if ep.Extract.TaskID == "" {
			return fieldErr(prefix+".extract.task_id", "异步提交端点必须声明 extract.task_id")
		}
		if len(ep.StatusMap) > 0 {
			return fieldErr(prefix+".status_map", "status_map 只属于轮询端点")
		}
	} else if ep.Async.IsBridge() {
		// 桥接：产物取值在轮询端点上（validateBridge 已校验 download/result），
		// 提交端点只需要任务号，不该再要求 result/text。
		if len(ep.StatusMap) > 0 {
			return fieldErr(prefix+".status_map", "status_map 只属于轮询端点")
		}
	} else if !ep.Passthrough && ep.Extract.Result == "" && ep.Extract.Results.IsZero() && ep.Extract.Text == "" {
		return fieldErr(prefix+".extract", "同步端点至少声明一个结果取值（result / result_urls / text），或标记 passthrough")
	}
	// 产物选择器：where 只对数组路径有意义，且只做元素内精确相等。
	if !ep.Extract.Results.IsZero() {
		if err := validateSelector(prefix+".extract.result_urls", ep.Extract.Results); err != nil {
			return err
		}
	}
	// duration_unit 只有配合 duration 才有意义（否则是个永远不生效的开关）。
	if ep.Extract.DurationUnit != "" && ep.Extract.Duration == "" {
		return fieldErr(prefix+".extract.duration_unit",
			"只有声明了 extract.duration 才能指定 duration_unit")
	}
	if u := ep.Extract.DurationUnit; u != "" && u != "secs" && u != "ms" {
		return fieldErr(prefix+".extract.duration_unit", "duration_unit 只能是 secs 或 ms")
	}

	paths := [][2]string{
		{"extract.task_id", ep.Extract.TaskID},
		{"extract.status", ep.Extract.Status},
		{"extract.progress", ep.Extract.Progress},
		{"extract.error", ep.Extract.Error},
		{"extract.result", ep.Extract.Result},
		{"extract.text", ep.Extract.Text},
		{"extract.duration", ep.Extract.Duration},
		{"errors.code_path", errCodePath(ep.Errors)},
		{"errors.message_path", errMsgPath(ep.Errors)},
	}
	for _, pair := range paths {
		if pair[1] == "" {
			continue
		}
		if err := validateJSONPath(pair[1], prefix+"."+pair[0]); err != nil {
			return err
		}
	}
	if ep.Errors != nil {
		switch ep.Errors.Success {
		case "", "http_2xx":
		case "body_code":
			if ep.Errors.CodePath == "" || len(ep.Errors.OKCodes) == 0 {
				return fieldErr(prefix+".errors", "success=body_code 时必须同时配置 code_path 与 ok_codes")
			}
		default:
			return fieldErr(prefix+".errors.success", "success 只能是 http_2xx 或 body_code")
		}
	}
	return nil
}

func errCodePath(e *ErrSpec) string {
	if e == nil {
		return ""
	}
	return e.CodePath
}

func errMsgPath(e *ErrSpec) string {
	if e == nil {
		return ""
	}
	return e.MessagePath
}

// validateUpstreamPath 校验上游相对路径。
//
// 复用 declarative.go 的同款规则（这里不 import relay 以免成环，规则逐条对齐）：
// 必须以 / 开头、不含协议与 ..、不含空白与控制字符、长度受限。
//
// vars 是**允许出现的占位符名集合**（保存期定源）：`upstream_model` 来自绑定，
// 其余来自本端点的 request_fields。运行期不做"猜来源"，避免同一份配置在两次调用
// 里取到不同的值。
func validateUpstreamPath(p string, poll bool, vars map[string]bool) error {
	p = strings.TrimSpace(p)
	if p == "" {
		return errors.New("path 不能为空")
	}
	if len(p) > MaxPathLen {
		return fmt.Errorf("path 超过 %d 字节", MaxPathLen)
	}
	if strings.Contains(p, "://") || strings.Contains(p, "..") {
		return errors.New("path 不能包含协议或 ..")
	}
	// % 会打开 %2e%2e / %2f 这类编码绕过，而占位符的值由我们 PathEscape 注入，
	// 静态路径段没有任何理由需要百分号。
	if strings.Contains(p, "%") {
		return errors.New("path 不能包含 %（请直接写字符，占位符值由平台编码）")
	}
	if !strings.HasPrefix(p, "/") {
		return errors.New("path 必须以 / 开头")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == ' ' {
			return errors.New("path 不能包含空白或控制字符")
		}
	}
	if !safePath.MatchString(p) {
		return errors.New("path 含有不允许的字符")
	}

	names := pathPlaceholders(p)
	if poll {
		// poll：唯一允许的占位符就是 {task_id}，且必须出现恰好一次
		if len(names) != 1 || names[0] != "task_id" {
			return errors.New("轮询端点的 path 必须包含且只包含一个 {task_id} 占位符")
		}
		return nil
	}
	if len(names) > MaxPathVars {
		return fmt.Errorf("path 占位符最多 %d 个", MaxPathVars)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if n == "task_id" {
			return errors.New("只有轮询端点的 path 能使用 {task_id}")
		}
		if !vars[n] {
			return fmt.Errorf("占位符 {%s} 未声明（可用：upstream_model 或 request_fields 里的字段名）", n)
		}
		if seen[n] {
			return fmt.Errorf("占位符 {%s} 重复出现（同一个值只替换一次）", n)
		}
		seen[n] = true
	}
	return nil
}

// validateSelector 校验产物选择器。
func validateSelector(field string, sel *URLSelector) error {
	if sel == nil {
		return nil
	}
	if strings.TrimSpace(sel.Path) == "" {
		return fieldErr(field, "path 不能为空")
	}
	if err := validateJSONPath(sel.Path, field); err != nil {
		return err
	}
	if len(sel.Where) == 0 {
		return nil
	}
	// where 的语义是「数组元素里某个字段等于某值」，所以路径必须落到数组元素上。
	if !strings.Contains(sel.Path, "[*]") {
		return fieldErr(field+".where", "只有数组路径（$.a[*].b）才能用 where 过滤")
	}
	if len(sel.Where) > MaxSelectorWhere {
		return fieldErr(field+".where", fmt.Sprintf("过滤条件最多 %d 条", MaxSelectorWhere))
	}
	for k, v := range sel.Where {
		if strings.TrimSpace(k) == "" || strings.Count(k, ".") > 2 {
			return fieldErr(field+".where", fmt.Sprintf("过滤键 %q 不合法（元素内浅路径，最多两层）", k))
		}
		if len(v) > 64 {
			return fieldErr(field+".where."+k, "过滤值过长")
		}
	}
	return nil
}

// checkFieldValueType 校验扩展字段的取值与声明类型一致。
//
// 只在**平台侧**做类型判定（不做隐式转换）：把 "true" 当布尔、把 "3" 当数字
// 会让上游收到字符串，而严格校验的上游会 400——那时用户看到的是上游原话，
// 定位成本远高于在这里直接告诉他类型不对。
func checkFieldValueType(f RequestField, v any) error {
	switch f.Type {
	case "string", "url":
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("需要 %s，实际收到 %s", f.Type, jsonTypeName(v))
		}
		if f.Type == "url" {
			if len(s) > 2048 {
				return errors.New("url 过长（上限 2048 字符）")
			}
			u, err := url.Parse(s)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return errors.New("需要 http/https 地址")
			}
		}
		if f.MaxLen > 0 && len([]rune(s)) > f.MaxLen {
			return fmt.Errorf("长度超过 %d 字", f.MaxLen)
		}
		if len(f.Enum) > 0 && !containsStr(f.Enum, s) {
			return fmt.Errorf("只能是「%s」，收到「%s」", strings.Join(f.Enum, " / "), s)
		}
	case "number":
		if _, ok := toFloat(v); !ok {
			return fmt.Errorf("需要 number，实际收到 %s", jsonTypeName(v))
		}
	case "integer":
		fl, ok := toFloat(v)
		if !ok || fl != float64(int64(fl)) {
			return fmt.Errorf("需要整数，实际收到 %s", jsonTypeName(v))
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("需要 boolean，实际收到 %s", jsonTypeName(v))
		}
	}
	return nil
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

func jsonTypeName(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case float64, json.Number:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	default:
		return "object"
	}
}

func validateAuth(a Auth) error {
	in := strings.ToLower(strings.TrimSpace(a.In))
	switch in {
	case "", "header":
		name := strings.ToLower(strings.TrimSpace(a.Name))
		if name == "" {
			name = "authorization"
		}
		if !allowedAuthHeaders[name] {
			return fmt.Errorf("鉴权头 %q 不在允许清单内", a.Name)
		}
	case "query", "body":
		if !identRe.MatchString(strings.TrimSpace(a.Name)) {
			return errors.New("放在 query/body 里的鉴权字段名必须是合法标识符")
		}
	default:
		return errors.New("auth.in 只能是 header / query / body")
	}
	return nil
}

func validateBody(prefix string, b BodySpec) error {
	if len(b.Const) > MaxConstKeys {
		return fieldErr(prefix+".const", fmt.Sprintf("常量字段最多 %d 个", MaxConstKeys))
	}
	for k, v := range b.Const {
		if !identRe.MatchString(k) {
			return fieldErr(prefix+".const", fmt.Sprintf("字段名 %q 不合法", k))
		}
		if err := validateConstValue(k, v); err != nil {
			return fieldErr(prefix+".const."+k, err.Error())
		}
	}
	if len(b.Copy) > MaxCopyKeys {
		return fieldErr(prefix+".copy", fmt.Sprintf("复制字段最多 %d 个", MaxCopyKeys))
	}
	for k, v := range b.Copy {
		if !identRe.MatchString(k) {
			return fieldErr(prefix+".copy", fmt.Sprintf("目标字段名 %q 不合法", k))
		}
		if len(v) == 0 {
			return fieldErr(prefix+".copy."+k, "来源不能为空")
		}
		if len(v) > MaxCopyCandidates {
			return fieldErr(prefix+".copy."+k, fmt.Sprintf("候选来源最多 %d 个", MaxCopyCandidates))
		}
		for _, src := range v {
			if !reqRefRe.MatchString(src) {
				return fieldErr(prefix+".copy."+k, "来源必须是 $req.<字段>")
			}
		}
	}
	if len(b.Arrays) > MaxArrays {
		return fieldErr(prefix+".arrays", fmt.Sprintf("数组映射最多 %d 个", MaxArrays))
	}
	for k, arr := range b.Arrays {
		if !identRe.MatchString(k) {
			return fieldErr(prefix+".arrays", fmt.Sprintf("数组字段名 %q 不合法", k))
		}
		if !reqRefRe.MatchString(arr.From) {
			return fieldErr(prefix+".arrays."+k+".from", "来源必须是 $req.<字段>")
		}
		if arr.Max < 0 || arr.Max > MaxArrayItems {
			return fieldErr(prefix+".arrays."+k+".max", fmt.Sprintf("max 需在 0–%d 之间", MaxArrayItems))
		}
		if len(arr.Item) == 0 {
			return fieldErr(prefix+".arrays."+k+".item", "item 不能为空")
		}
		for ik, iv := range arr.Item {
			if !identRe.MatchString(ik) {
				return fieldErr(prefix+".arrays."+k+".item", fmt.Sprintf("元素字段名 %q 不合法", ik))
			}
			if iv != "$value" {
				return fieldErr(prefix+".arrays."+k+".item."+ik, "数组元素内只能使用 $value")
			}
		}
	}
	for _, f := range b.Drop {
		if !identRe.MatchString(f) {
			return fieldErr(prefix+".drop", fmt.Sprintf("字段名 %q 不合法", f))
		}
	}
	return nil
}

// reservedConstKeys 是**不允许**由 const 写入的计费维度字段。
//
// 与 relay 的 request_patch 保留键同名单（declarative.go）：这些字段直接决定收多少钱，
// 若能由规格常量写死，同一个模型的不同渠道就会按不同秒数/张数计费，而账单看不出来。
// ReservedConstKeys 返回保留键清单（供 schema 目录下发，避免前端/AI 反复试错）。
func ReservedConstKeys() []string {
	out := make([]string, 0, len(reservedConstKeys))
	for k := range reservedConstKeys {
		out = append(out, k)
	}
	return out
}

// AuthHeaders 返回允许承载密钥的头名清单（小写，供 schema 目录下发）。
func AuthHeaders() []string {
	out := make([]string, 0, len(allowedAuthHeaders))
	for k := range allowedAuthHeaders {
		out = append(out, k)
	}
	return out
}

var reservedConstKeys = map[string]bool{
	"n": true, "seconds": true, "duration": true, "size": true,
	"resolution": true, "quality": true, "num_frames": true, "fps": true,
	"prompt_tokens": true, "max_tokens": true,
}

func validateConstValue(key string, v any) error {
	if reservedConstKeys[strings.ToLower(key)] {
		return fmt.Errorf("字段 %q 属于计费维度，不能由规格常量写死（请用 $req 复制调用方传值）", key)
	}
	switch v.(type) {
	case string, float64, bool, nil:
		return nil
	default:
		return errors.New("常量值只能是字符串、数字、布尔或 null")
	}
}

// statusMapRequired 是必须声明的两个终态。
//
// 只强制这两态：未知状态一律按 running 处理（见 adapter.poll），
// 所以排队/进行中的取值可以有遗漏（多轮询一次不会错账）；
// 而 succeeded / failed 是**收钱与否**的判定，缺一个就会把失败当成功、
// 或者把完成的任务永远挂着，必须由运营显式给出。
var statusMapRequired = []string{string(taskstate.Succeeded), string(taskstate.Failed)}

func validateStatusMap(prefix string, m map[string][]string) error {
	if len(m) == 0 {
		return fieldErr(prefix, "轮询端点必须声明 status_map（上游状态值 → 平台状态）")
	}
	allowed := map[string]bool{}
	for _, s := range StatusStates {
		allowed[s] = true
	}
	// 键排序后校验：map 遍历顺序随机，不排序的话同一条错配置每次给出不同的
	// 错误文案，用户会以为改过之后又冒出新问题。
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !allowed[k] {
			return fieldErr(prefix, fmt.Sprintf("status_map 的键 %q 不支持（只能是 %s）",
				k, strings.Join(StatusStates, "/")))
		}
	}
	for _, k := range statusMapRequired {
		if len(m[k]) == 0 {
			return fieldErr(prefix, fmt.Sprintf("status_map 缺少 %q 的取值（succeeded 与 failed 是终态判定，必须齐全）", k))
		}
	}
	seen := map[string]string{}
	for _, k := range keys {
		for _, v := range m[k] {
			v = strings.TrimSpace(v)
			if v == "" {
				return fieldErr(prefix, "status_map 里的状态值不能为空")
			}
			if prev, ok := seen[v]; ok && prev != k {
				return fieldErr(prefix, fmt.Sprintf("状态值 %q 同时映射到 %s 与 %s", v, prev, k))
			}
			seen[v] = k
		}
	}
	return nil
}

func validateJSONPath(p, field string) error {
	if !jsonPath.MatchString(p) {
		return fieldErr(field, "取值路径只支持 $.a.b / $.a[0].b / $.a[*].b 三种写法")
	}
	if strings.Count(p, ".")+strings.Count(p, "[") > MaxJSONPathDeep {
		return fieldErr(field, fmt.Sprintf("取值路径层级不得超过 %d 层", MaxJSONPathDeep))
	}
	return nil
}

// FieldError 是带字段定位的校验错误（管理台可直接高亮对应输入）。
type FieldError struct {
	Field   string
	Message string
}

func (e *FieldError) Error() string { return e.Field + "：" + e.Message }

func fieldErr(field, msg string) error { return &FieldError{Field: field, Message: msg} }

// AsFieldError 把任意错误转成字段错误（非字段错误时 field 为空）。
func AsFieldError(err error) (field, msg string) {
	var fe *FieldError
	if errors.As(err, &fe) {
		return fe.Field, fe.Message
	}
	return "", err.Error()
}

// Caps 由端点键派生能力矩阵（单一真相源：不单独声明 caps）。
func (s *Spec) Caps() []string {
	seen := map[string]bool{}
	out := make([]string, 0, 4)
	add := func(c string) {
		if c != "" && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	for key := range s.Endpoints {
		if IsPollKey(key) {
			continue
		}
		fact, ok := apidoc.SpecEndpointByKey(key)
		if !ok {
			continue
		}
		switch fact.Key {
		case "chat/completions", "completions", "responses":
			add("text")
		case "embeddings", "rerank":
			add("embedding")
		case "images/generations":
			add("image")
		case "images/edits", "images/variations":
			add("image")
			add("edit_image")
		case "audio/speech":
			add("tts")
		case "audio/transcriptions", "audio/translations":
			add("asr")
		case "audio/music/generations":
			add("music")
		case "video/generations":
			add("video")
		case "moderations":
			add("passthrough")
		}
	}
	sort.Strings(out)
	return out
}

// SupportsEndpointKey 判断规格是否声明了该端点（同步/异步提交键）。
func (s *Spec) SupportsEndpointKey(key string) bool {
	_, ok := s.Endpoints[key]
	return ok
}

// AsyncEndpointKeys 返回全部**任务态**异步提交端点键（按字典序，保证调度稳定）。
//
// 明确排除 bridge 端点：桥接是"在本次同步请求内等完"，没有平台任务行可挂，
// 若把它算成异步端点，AsyncEndpoints() 会为一份纯桥接规格返回非空 ——
// 于是任务链路会把视频/音乐任务派给一个根本不会产生 task_id 的端点。
func (s *Spec) AsyncEndpointKeys() []string {
	out := make([]string, 0, 2)
	for key, ep := range s.Endpoints {
		if IsPollKey(key) || ep.Async.IsBridge() || ep.Async == nil {
			continue
		}
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// ClampName 把任意字符串裁剪成可安全用于 Header/文件名标识的形式。
func ClampName(s string) string { return clampName.ReplaceAllString(s, "-") }
