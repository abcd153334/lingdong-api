// Package relay 是网关转发与选路的核心（S5 最小闭环）。
package relay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/gateway"
	"lingdong-api/internal/middleware"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/keyscope"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/metrics"
	"lingdong-api/internal/pkg/money"
	"lingdong-api/internal/pkg/netguard"
	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/pkg/siteurl"
	"lingdong-api/internal/pkg/sorting"
	"lingdong-api/internal/pkg/textx"
	"lingdong-api/internal/relay/conv"
	"lingdong-api/internal/service/apikey"
	"lingdong-api/internal/service/authcfg"
	"lingdong-api/internal/service/billing"
	"lingdong-api/internal/service/channellimit"
	"lingdong-api/internal/service/logs"
	"lingdong-api/internal/service/pricing"
	"lingdong-api/internal/service/taskview"
)

// 请求体上限与分页/重试的通用常量。
//
// 体上限常量的**唯一真相源在 middleware/bodylimit.go**（MaxRelayBodyBytes /
// MaxMultipartBodyBytes，本包引用）——门与内层分离改值时只改一处。
// 估算相关常量（token 系数、图片/音频折算、输出上限缺省与封顶）全部在
// estimate_consts.go —— 那里是唯一处，不要在这里再加。
const (
	defaultRetryTimes  = 3
	statusClientClosed = 499 // 客户端断开（门户调用记录语义）
	// imageStreamHeartbeat 是图片流式的心跳间隔。
	//
	// 15 秒：远小于任何一层代理/SDK 的默认空闲超时（常见 60–120s），
	// 又足够稀疏，不会把流量放大成「心跳占满带宽」。
	imageStreamHeartbeat = 15 * time.Second
)

// UsageInfo 的解析与计费口径在同一包内保持唯一真相源。

var (
	billOnce sync.Once
	billSvc  *billing.Service
)

// billingService 返回进程级计费服务（余额唯一写入口）。
func billingService() *billing.Service {
	billOnce.Do(func() { billSvc = billing.New(db.DB) })
	return billSvc
}

// endpointKind 标识网关端点语义（决定适配器方法与默认模态）。
type endpointKind int

const (
	kindChat endpointKind = iota
	kindCompletions
	kindEmbeddings
	kindImagesGenerations   // S6
	kindImagesEdits         // S6
	kindImagesVariations    // S6
	kindAudioSpeech         // S6
	kindAudioTranscriptions // S6
	kindAudioTranslations   // S6
	kindModerations         // S6
	kindRerank              // S6
	kindResponses           // S9
	// kindMessages 是 Anthropic Messages 协议入站（POST /v1/messages）。
	//
	// 必须追加在末尾：这个枚举是 iota，插在中间会让所有后续值整体移位，
	// 而非测试代码有 40+ 处引用（日志、指标、测试快照都会跟着错位）。
	kindMessages
	// kindResponsesCompact 是 /v1/responses/compact（Codex 上下文压缩）。
	// 追加在末尾的理由同 kindMessages：iota 插队会让 40+ 处引用整体错位。
	kindResponsesCompact
	// kindGeminiGenerate 是 /v1beta/models/{model}:generateContent（Gemini 入站 → OpenAI 上游）。
	kindGeminiGenerate
	// kindGeminiEmbedContent 是 :embedContent 与遗留的 /v1/engines/{model}/embeddings。
	kindGeminiEmbedContent
)

// endpointSpec 描述端点契约：上游相对路径、是否 multipart、默认模态、响应是否二进制。
//
// 字段值全部来自 internal/apidoc 的端点表（唯一真相源），这里只做 endpointKind
// 与 apidoc.Kind 的桥接 —— 两份路径字面量曾经并存，改一处漏一处就是
// 「文档说 multipart、实际拒收文件」。
type endpointSpec struct {
	path      string
	multipart bool
	modality  string
	binary    bool
}

// docKindOf 把内部端点枚举映射为文档层的稳定 id。
//
// 为什么不直接把 endpointKind 换成 apidoc.Kind：它是 iota 整数，非测试代码有 44 处引用，
// 全量替换的回归面远大于收益；而 apidoc.Kind 是字符串（要进文档、URL、models.endpoints）。
// 两者职责不同，用一张 12 项的身份对照表桥接，双向覆盖由
// TestEveryRelayKindMapsToContract 守住（少一项、多一项都红）。
var docKindOf = map[endpointKind]apidoc.Kind{
	kindChat:                apidoc.KindChatCompletions,
	kindCompletions:         apidoc.KindCompletions,
	kindEmbeddings:          apidoc.KindEmbeddings,
	kindImagesGenerations:   apidoc.KindImagesGenerations,
	kindImagesEdits:         apidoc.KindImagesEdits,
	kindImagesVariations:    apidoc.KindImagesVariations,
	kindAudioSpeech:         apidoc.KindAudioSpeech,
	kindAudioTranscriptions: apidoc.KindAudioTranscriptions,
	kindAudioTranslations:   apidoc.KindAudioTranslations,
	kindModerations:         apidoc.KindModerations,
	kindRerank:              apidoc.KindRerank,
	kindResponses:           apidoc.KindResponses,
	kindMessages:            apidoc.KindMessages,
	kindResponsesCompact:    apidoc.KindResponsesCompact,
	kindGeminiGenerate:      apidoc.KindGeminiGenerate,
	kindGeminiEmbedContent:  apidoc.KindGeminiEmbedContent,
}

// specOf 返回端点契约；Dedicated 端点（chat/completions/embeddings）path 归一为空串，
// 因为 invokeEndpoint 用「path 是否为空」决定走专用适配器方法还是通用 Forward。
func specOf(kind endpointKind) endpointSpec {
	t, ok := apidoc.TransportOf(docKindOf[kind])
	if !ok {
		// 未登记即契约缺失：返回零值会让日志变成「请求成功但 upstream 空」，
		// 必须留下明确线索（B0 阶段用 warn 而不是 panic，避免上线瞬间打挂进程）。
		logger.L().Warn("端点缺少文档契约（internal/apidoc 未登记）", zap.Int("endpoint_kind", int(kind)))
		return endpointSpec{}
	}
	p := t.Path
	if t.Dedicated {
		p = ""
	}
	return endpointSpec{path: p, multipart: t.Multipart, modality: t.Modality, binary: t.Binary}
}

// ChatCompletions 处理 POST /v1/chat/completions。
func ChatCompletions(c *gin.Context) { relayRequest(c, kindChat) }

// Completions 处理 POST /v1/completions。
func Completions(c *gin.Context) { relayRequest(c, kindCompletions) }

// Embeddings 处理 POST /v1/embeddings。
func Embeddings(c *gin.Context) { relayRequest(c, kindEmbeddings) }

// ImagesGenerations 处理 POST /v1/images/generations（JSON）。
func ImagesGenerations(c *gin.Context) { relayRequest(c, kindImagesGenerations) }

// ImagesEdits 处理 POST /v1/images/edits（multipart）。
func ImagesEdits(c *gin.Context) { relayRequest(c, kindImagesEdits) }

// ImagesVariations 处理 POST /v1/images/variations（multipart）。
func ImagesVariations(c *gin.Context) { relayRequest(c, kindImagesVariations) }

// AudioSpeech 处理 POST /v1/audio/speech（JSON 入参、二进制出参）。
func AudioSpeech(c *gin.Context) { relayRequest(c, kindAudioSpeech) }

// AudioTranscriptions 处理 POST /v1/audio/transcriptions（multipart）。
func AudioTranscriptions(c *gin.Context) { relayRequest(c, kindAudioTranscriptions) }

// AudioTranslations 处理 POST /v1/audio/translations（multipart）。
func AudioTranslations(c *gin.Context) { relayRequest(c, kindAudioTranslations) }

// Moderations 处理 POST /v1/moderations（JSON）。
func Moderations(c *gin.Context) { relayRequest(c, kindModerations) }

// Rerank 处理 POST /v1/rerank（JSON）。
func Rerank(c *gin.Context) { relayRequest(c, kindRerank) }

// Responses 处理 POST /v1/responses（JSON，设计 §4.3.1 L383）：仅 OpenAI 兼容渠道，否则 400 endpoint_unsupported。
func Responses(c *gin.Context) { relayRequest(c, kindResponses) }

// ResponsesCompact 处理 POST /v1/responses/compact（Codex 上下文压缩）。
//
// 不写独立实现：上游路径不同、语义相同，走同一条 relayRequest 管线
// （定价、冻结、选路、结算、失败释放都复用 responses 的那一套）。
func ResponsesCompact(c *gin.Context) { relayRequest(c, kindResponsesCompact) }

// readRequest 读取请求体并判定是否超限（多读 1 字节即可判定，不把整段超限体留在内存里）。
func readRequest(r io.Reader, limit int64) (body []byte, tooLarge bool, err error) {
	body, err = io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(body)) > limit {
		return nil, true, nil
	}
	return body, false, nil
}

// relayHead 是转发前的请求体关键字段（其余字段原样透传）。
// flexString 是「字符串或数字一律当字符串」的宽松类型。
//
// 为什么必须宽松：参数不再被拦截，但 `{"size":2048}` / `{"seconds":5}` 这类
// 写法会在 json.Unmarshal 阶段就让**整条请求**失败（报「参数不合法」），
// 等于在校验层之外又加了一道隐式拦截。数字转十进制文本、布尔/对象/null 视为空。
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	switch {
	case trimmed == "" || trimmed == "null":
		*f = ""
	case trimmed[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return nil // 非法字符串按空处理，绝不因此打挂整条请求
		}
		*f = flexString(s)
	case trimmed == "true" || trimmed == "false":
		*f = ""
	default:
		*f = flexString(trimmed) // 数字原样（含负号/小数点）
	}
	return nil
}

func (f flexString) String() string { return string(f) }

// flexInt 是「整数 / 浮点 / 数字字符串一律当整数」的宽松类型，认不出按 0（= 未设置）。
//
// 与 flexString 同一个理由：`{"n":"2"}`、`{"max_tokens":"1000"}` 这类写法在
// json.Unmarshal 阶段会让**整条请求**失败，用户看到的是「参数不合法」，
// 而他永远不会想到问题出在一个可选字段的类型上。0 的语义是「没给」，
// 下游的默认值逻辑（冻结估算、张数上限）都按这个语义处理。
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	*f = 0
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(b, &s) != nil {
			return nil // 非法字符串按未设置处理，绝不因此打挂整条请求
		}
		trimmed = strings.TrimSpace(s)
		if trimmed == "" {
			return nil
		}
	}
	// 只接受非负值：负的 max_tokens / n 没有合法语义，按未设置处理。
	// 上界用 float 比较，避免超大数转换溢出成负数。
	if v, err := strconv.ParseFloat(trimmed, 64); err == nil && v >= 0 && v < 9.2e18 {
		*f = flexInt(int64(v))
	}
	return nil
}

func (f flexInt) Int64() int64 { return int64(f) }

// flexBool 是「布尔 / "true" / 1」的宽松类型，认不出按 false。
type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	switch {
	case trimmed == "true" || trimmed == "1":
		*f = true
	case trimmed == "false" || trimmed == "0" || trimmed == "" || trimmed == "null":
		*f = false
	case trimmed[0] == '"':
		var s string
		if json.Unmarshal(b, &s) != nil {
			*f = false
			return nil
		}
		v := strings.TrimSpace(s)
		*f = flexBool(strings.EqualFold(v, "true") || v == "1")
	default:
		*f = false
	}
	return nil
}

func (f flexBool) Bool() bool { return bool(f) }

type relayHead struct {
	Model  string   `json:"model"`
	Stream flexBool `json:"stream"`
	// max_tokens 系三个字段只影响冻结估算，但类型必须宽松：
	// 它们是**可选**的，一个写错不该让整条请求变成 400。
	MaxTokens       flexInt `json:"max_tokens"`
	MaxOutputTokens flexInt `json:"max_output_tokens"` // Responses 端点输出上限（S9）
	// MaxCompletionTokens 是 OpenAI 新式输出上限字段（chat/completions 已推荐用它替代 max_tokens）。
	// 不识别它会让长输出请求按默认 1024 冻结 → 结算远超冻结额 → 余额穿透。
	MaxCompletionTokens flexInt         `json:"max_completion_tokens"`
	Messages            json.RawMessage `json:"messages"`
	Input               json.RawMessage `json:"input"`
	Prompt              json.RawMessage `json:"prompt"`
	// System 与 Tools 只影响**冻结估算**（不进任何计费公式）。
	//
	// 为什么必须读它们：工具 schema（JSON Schema 往往上万字符）与 system 提示
	// 都是真实的输入 token，此前完全没进估算 → 带工具/长 system 的请求冻结额
	// 低一个数量级，结算被 3 倍截顶后确定性少收。
	// Claude 入站更极端：它把 system 放在顶层、tools 是顶层数组，转换后是首条
	// system 消息 + messages 里的 tool 定义，两处都必须计入。
	System     json.RawMessage `json:"system"`
	Tools      json.RawMessage `json:"tools"`
	Size       flexString      `json:"size"`       // 图片尺寸（档位/WxH/auto/私有写法）
	Resolution flexString      `json:"resolution"` // 档位别名（图片侧仅用于折叠，视频侧是正式字段）
	// Ratio 是图片端点上的历史画幅别名，只作为 `aspect_ratio` 的兜底候选参与解析。
	//
	// 必须解析出来：不解析它，`billing.ParseImageSize` 就看不见这个键，
	// 而适配器又会在出站时删掉它 —— 用户写了 16:9、拿到一张方图，账单上一切正常。
	// 视频端点的 ratio 是正式入参，所以兜底只对图片分支生效（见 isImageKind 判定）。
	Ratio string `json:"ratio"`
	// ImageSize 是上游真正下发的档位字段（多米 nano 系只认它）。
	//
	// 必须单独解析：`billing.ParseImageSize` 按 **size → image_size → resolution**
	// 取第一个「具体意图」，少解析一个字段就会出现「按 size 计价、按 image_size 出图」的错档。
	ImageSize flexString `json:"image_size"`
	// AspectRatio 是画幅比例（"16:9"）。用 RawMessage 而不是 string：
	// 类型写错（`"aspect_ratio": 16` 或对象）会让整条 json.Unmarshal 失败，
	// 症状是「请求里有 model 和 prompt，却报参数不合法」——整条请求被一个可选字段打死。
	AspectRatio json.RawMessage `json:"aspect_ratio"`
	// FileBytes 是 multipart 请求里**上传文件的总字节数**（无文件为 0）。
	//
	// 只服务于音频按秒计费的冻结估算：ASR 的时长在冻结期根本不存在，
	// 拿不到正数冻结额就只能冻结 0，而冻结 0 会在结算时被「零冻结上限」截顶，
	// 系统性少收（长音频尤其明显）。
	FileBytes int64           `json:"-"`
	Quality   flexString      `json:"quality"`
	N         flexInt         `json:"n"`
	Seconds   flexString      `json:"seconds"`   // 视频秒数（S7）
	Query     json.RawMessage `json:"query"`     // rerank 查询
	Documents json.RawMessage `json:"documents"` // rerank 文档集
	// Image 是参考图原文（单个字符串或字符串数组），只用于数参考图张数。
	//
	// 用 RawMessage 而不是 []string：上游允许 "url" 与 ["url1","url2"] 两种形态，
	// 直接 Unmarshal 到 []string 对字符串形态会失败并静默返回 0 —— 于是
	// 「单图生组图」会被当成「文生组图」，冻结多算 1 张。
	Image json.RawMessage `json:"image"`
	// SeqImageGen 是组图开关（Ark 的 sequential_image_generation：auto / disabled）。
	SeqImageGen string `json:"sequential_image_generation"`
	// LayerDecomp 是图层拆分开关（Ark 的 layer_decomposition）。
	//
	// 用 RawMessage 而不是 bool：两种写法都出现过（`true` 与 `"true"`），
	// 声明成 bool 时字符串写法会让**整条** json.Unmarshal 失败 —— 头部解析一失败，
	// 后面所有判定都拿到零值，症状是"这个请求的参数全丢了"，极难定位。
	LayerDecomp json.RawMessage `json:"layer_decomposition"`
	// SeqOptions 必须是**嵌套**结构。
	//
	// 直接写扁平字段名（`json:"sequential_image_generation_options.max_images"`）
	// encoding/json 不会识别：编译通过、运行期恒 0，是典型的「看起来对、跑起来错」。
	SeqOptions struct {
		MaxImages int64 `json:"max_images"`
	} `json:"sequential_image_generation_options"`
}

// relayRequest 是网关端点统一流程：鉴权已完成 → 白名单 → 定价预检 → 选路预检 → 冻结 → 派发 → 结算。
func relayRequest(c *gin.Context, kind endpointKind) {
	relayRequestIn(c, kind, nil)
}

// relayRequestIn 在统一流程前面加了「入站线格式解码」。
//
// codec 为 nil 时与改造前**逐字节一致**（OpenAI 入站直接透传）；
// 非 nil 时先把请求体解成 OpenAI Chat 形状，之后所有判定都作用在解码后的请求体上
// —— 这一点很关键：定价、冻结、模态白名单、渠道能力判定今天都只认 OpenAI 字段，
// 让它们各自去理解第二种协议就等于写出第二、第三份真相源。
func relayRequestIn(c *gin.Context, kind endpointKind, codec ingressCodec) {
	// startedAt 是**本次请求的真实入口时刻**：总耗时的唯一正确起点。
	//
	// 原实现把 time.Now() 写在派发重试循环内部（每次尝试一个 start），
	// 于是"换渠道重试 3 次后成功"只记录了最后一次的 0.8s，而用户实际等了 6s。
	// 这里取一次并向下传，Settle 侧算 finished_at − started_at。
	startedAt := time.Now()
	k := apiKeyOf(c)
	u := userOf(c)
	if k == nil || u == nil {
		gateway.FailDef(c, errs.InvalidAPIKey)
		return
	}

	spec := specOf(kind)
	limit := int64(middleware.MaxRelayBodyBytes)
	if spec.multipart {
		// multipart 端点的整包上限同样来自上传配置（不再有第二个常量）。
		limit = middleware.UploadBodyCap()
	}
	body, tooLarge, err := readRequest(c.Request.Body, limit)
	if err != nil {
		// 中间件（middleware.BodyLimit）先套了一层 MaxBytesReader：它超限时
		// io.ReadAll 返回 *http.MaxBytesError，**必须翻译成 413**。
		// 落成 400「参数不合法」的后果是把「你传了 20MB」说成「你字段写错了」，
		// 排障会先怀疑 JSON 结构，而真正的原因一眼也看不出。
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			gateway.FailDef(c, errs.RequestTooLarge)
			return
		}
		gateway.FailDef(c, errs.InvalidParam)
		return
	}
	// P2-5：超限必须 413，不能「读一半就当正常请求转发」（截断后的 JSON 要么解析失败报 400，
	// 要么更糟——解析成功但少了一半消息，用户为缺失的内容付费）。
	if tooLarge {
		gateway.FailDef(c, errs.RequestTooLarge)
		return
	}
	if len(body) == 0 {
		gateway.FailDef(c, errs.InvalidParam)
		return
	}
	// 入站解码：只有「客户端协议 ≠ 平台内部协议」时才发生。
	//
	// 必须在解析 relayHead **之前**：后面所有判定（定价、模态、选路、冻结）
	// 读的都是 decoded 之后的字段。若把转换放在后面，Claude 请求会因为没有
	// model/messages 而在第一步就被判 400。
	//
	// body 保持客户端原文（落 call_io、算 digest），wireBody 才是发给上游的报文。
	wireBody := body
	if codec != nil {
		decoded, derr := codec.ToUpstream(body)
		if derr != nil {
			var ce *conv.Error
			if errors.As(derr, &ce) {
				// 转换失败发生在冻结之前：一分钱不动，也不需要释放。
				gateway.Fail(c, ce.Status, ce.Code, ce.Message)
				return
			}
			gateway.FailDef(c, errs.InvalidParam)
			return
		}
		wireBody = decoded
	}
	// 图片端点：官方字段名 `image_urls` 折叠成平台唯一的参考图字段 `image`。
	//
	// 必须在解析 relayHead **之前**，而且与异步链路共用同一个函数
	// （task.normalizeSubmitBody → FoldImageURLsMap）。此前折叠只存在于异步那一条：
	// 同步链路上 relayHead 只认 `image`，`image_urls` 又被适配器的 localOnly
	// 白名单当"平台内部键"删掉 —— 用户按官方文档传参考图，得到一张纯文生图，
	// 账单正常、没有任何报错。这是"文档准确"最坏的一种：文档对，代码不听。
	if isImageKind(kind) && json.Valid(wireBody) {
		folded, ferr := FoldImageURLsBody(wireBody)
		if ferr != nil {
			gateway.FailDef(c, errs.InvalidParam.WithMessage(ferr.Error()))
			return
		}
		wireBody = folded
	}
	contentType := c.Request.Header.Get("Content-Type")
	var head relayHead
	switch {
	case !spec.multipart:
		err = json.Unmarshal(wireBody, &head)
	case jsonBodyAllowed(kind) && !isMultipartBody(contentType):
		// 同一能力的第二种入参形态（只有 images/edits，见 jsonBodyAllowed）。
		// 报文原样转发给上游：这条端点上游本就两种都收，网关不替它做形态转换。
		err = json.Unmarshal(wireBody, &head)
	default:
		head, err = parseMultipartHead(contentType, body)
	}
	if errors.Is(err, errNotMultipart) {
		// multipart 端点收到了 JSON/无 boundary 请求体：必须告诉他换形态，
		// 落成通用 invalid_param 时接入方只会反复检查 JSON 字段（P7）。
		gateway.FailDef(c, errs.MultipartRequired)
		return
	}
	if err != nil || strings.TrimSpace(head.Model) == "" {
		gateway.FailDef(c, errs.InvalidParam)
		return
	}
	// 与访问日志联动：模型名是网关侧排障的第一索引（按模型查错、查慢、查扣费）。
	c.Set("gateway_model", strings.TrimSpace(head.Model))

	// 定价预检：模型必须存在且启用，价格必须可见（防白嫖，§4.5.5）。
	cat, err := pricing.Get()
	if err != nil {
		gateway.FailDef(c, errs.Internal)
		return
	}
	item := findModel(cat, head.Model)
	if item == nil || !item.Enabled {
		gateway.FailDef(c, errs.ModelNotFound)
		return
	}
	// 模态闸门（P0-01）：模型 modality 必须与端点匹配。
	//
	// 不校验时的实际后果：一个 quota_type=0 的 chat 模型被调用 /v1/images/generations 或
	// /v1/audio/speech，estimateTokens 对这些端点返回 (0,0)，billing.Estimate 直接返回 0，
	// 上游真实出图/出音频，账单 0 元 —— 只要上游接受该模型名，就是持续白送。
	//
	// 这里**没有灰度开关**：图像同步与异步共用同一条路径（`async:true` 分流），
	// 一个能关的开关会让同一 URL 按分支给出不同结论 —— 同步放行、异步硬拦。
	// 而 service/task 的提交预检本来就是硬拦，两边必须一致。
	if !modalityAllows(item, kind) {
		// Gemini 生成只对 chat 模型开放，拒绝时要指路而不是只说"不行"：
		// 用户多半是把图片/视频模型名填进了 Gemini 路径。
		msg := fmt.Sprintf(
			"模型 %s 的模态是 %s，不能调用 %s；如需启用请在模型配置的 endpoints 中声明该端点",
			head.Model, displayModality(item.Modality), endpointDisplayOf(kind))
		if kind == kindGeminiGenerate {
			msg = fmt.Sprintf(
				"模型 %s 不支持 Gemini 协议生成（它的模态是 %s）：图片请改用 /v1/images/generations，视频请改用 /v1/video/generations",
				head.Model, displayModality(item.Modality))
		}
		gateway.FailDef(c, errs.EndpointUnsupported.WithMessage(msg))
		return
	}
	// 二道闸（P0-01 的异步/误配兜底）：非 token 端点不允许按量计费。
	// 保存期已由 schema.AllowsBillingMode 拦截，这里挡住「存量脏数据 / 手工改库」。
	if billingModeOf(item) == model.BillingModeTokens && !tokenBillable(kind) {
		gateway.FailDef(c, errs.PriceNotConfigured)
		return
	}
	modality := item.Modality
	if modality == "" {
		modality = endpointModality(kind)
	}
	c.Set("gateway_modality", modality)
	// 双白名单预检（§4.3.1）。
	if !apikey.AllowsModel(k, head.Model, modality) {
		if models := apikey.DecodeStrings(k.ModelsAllowed); len(models) > 0 && !contains(models, head.Model) {
			gateway.FailDef(c, errs.ModelNotAllowed)
		} else {
			gateway.FailDef(c, errs.ModalityNotAllowed)
		}
		return
	}

	group := effectiveGroup(k, u)
	snap := buildSnapshot(item, groupDiscount(cat, group))
	promptEst, completionEst := estimateTokens(head, kind, item.MaxOutputTokens)
	// 图片尺寸解析 + 档位归一（唯一入口，见 image_size.go）。
	//
	// 两种写法：`size` 写档位（2K）或像素（2048x1152），`aspect_ratio` 写画幅（16:9）。
	// 历史别名 resolution / image_size 在图片端点上直接拒绝（见 rejectRetiredSizeFields）：
	// 静默忽略等于「用户写了 4K、拿到 1K 图，账单按默认档收」。
	// 必须在 Estimate 之前判：之后判就是冻结→释放。
	var imageSize billing.ImageSize
	if isImageKind(kind) {
		// 尺寸只做「计费解释」，不拒绝任何写法：
		// 支持什么由模型与官方文档决定，网关不替上游判定合法性。
		// 解释不出来的情况会以 anomaly 落账（image_tier_unpriced 等），不影响出站字节。
		imageSize = resolveImageSize(head, snap.SpecPrice)
		// 画幅写错必须 400：以前 parseAspectField 的错误被丢掉，`aspect_ratio:"abc"`
		// 既不报错也不计画幅，用户以为设了 16:9，拿到一张方图而账单一切正常。
		// 放在 Estimate 之前 —— 参数错不该产生冻结与流水。
		if imageSize.InvalidRatio {
			gateway.FailDef(c, errs.InvalidParam.WithMessage(
				"画幅比例写法不合法：请写成 16:9 这样的比例；留空或 auto 表示由模型决定"))
			return
		}
		// 「档位 + 画幅」换算不出像素：在冻结之前拒绝（与异步提交同一判据）。
		// 这一条必须在 Estimate 之前 —— 参数错不该产生冻结、流水与一条 failed 记录。
		if msg := apidoc.GPTImageSizeProblem(head.Model, imageSize.DeclaredTier, imageSize.Ratio); msg != "" {
			gateway.FailDef(c, errs.InvalidParam.WithMessage(msg))
			return
		}
	}
	specUsage := specUsageOf(kind, head, imageSize, imageMaxN(snap.SpecPrice), imageCapsOf(item))
	// 注：这里以前有两道「能力闸门」（stream / layer_decomposition）。
	// 产品决定「模型支持什么由用户与官方文档决定」后已删除：
	// 不支持的组合由上游拒绝，平台只按实际交付结算（stream 回落整段 JSON 时
	// 已有 AnomalyStreamJSONFallback 留痕）。
	//
	// 为什么必须在预检期：这里才「知道是哪个模型」，400 是终局 —— 不冻结、不选路、
	// 不记渠道健康。放到适配器里按模型名写白名单就是第二真相源：运营在管理台改了
	// models.capabilities，适配器那份不会跟着变，两边会打架。
	//
	// 不拦的后果：客户端传 stream:true 拿到一个整段 JSON（非 SSE），
	// SDK 按 SSE 解析直接抛错，而平台记录里是一次 200 成功 —— 用户投诉"流式不可用"
	// 却查不出任何失败。宁可明确 400 并告诉他能用哪个模型。
	amount, perr := billing.Estimate(snap, promptEst, completionEst, specUsage)
	if perr != nil {
		switch {
		case errors.Is(perr, billing.ErrPriceMissing):
			gateway.FailDef(c, errs.PriceNotConfigured)
		case errors.Is(perr, billing.ErrInvalidTier):
			gateway.FailDef(c, errs.InvalidTier)
		case errors.Is(perr, billing.ErrImageDefaultTierUnset):
			// 平台侧配置缺口，但调用方有办法继续（写死 size）：报 400 并说清动作。
			// 归到 PriceNotConfigured 会让用户去查价格，而真正要补的是「默认档」。
			gateway.FailDef(c, errs.InvalidParam.WithMessage(
				"无法确定计费档位：请显式指定 size（如 1K、1024x1024）"))
		case errors.Is(perr, billing.ErrOverMaxSeconds):
			// 时长超过模型级 max_seconds 是调用方问题（换个秒数就能成功），必须 400。
			// 落到 default 会变成 500，用户以为平台故障。
			gateway.FailDef(c, errs.InvalidParam.WithMessage("时长超过模型上限，请缩短后重试"))
		case errors.Is(perr, billing.ErrChargeOverLimit):
			gateway.FailDef(c, errs.RequestQuotaExceeded)
		case errors.Is(perr, billing.ErrSpecUnsupported):
			gateway.FailDef(c, errs.EndpointUnsupported)
		default:
			gateway.FailDef(c, errs.Internal)
		}
		return
	}

	// 路由预检：候选为空必须在冻结前拦截（铁律：不建不可恢复的冻结）。
	candidates, err := SelectCandidates(head.Model, group)
	if err != nil {
		gateway.FailDef(c, errs.Internal)
		return
	}
	if len(candidates) == 0 {
		// 候选为空是运营侧事实（模型没绑渠道 / 全被冷却），对外只表达「这个模型现在用不了」。
		gateway.FailDef(c, errs.ModelUnavailable)
		return
	}
	// /v1/responses 仅 OpenAI 兼容渠道（§4.3.1 L383）：非 openai 渠道不静默回落。
	if kind == kindResponses {
		candidates = filterCandidatesByAdapter(candidates, "openai")
		if len(candidates) == 0 {
			gateway.FailDef(c, errs.EndpointUnsupported)
			return
		}
	}

	// 渠道交付能力过滤：把「上游根本做不出这个尺寸」的候选先摘掉。
	//
	// 在这里而不是适配器里，是因为冻结发生在下一行：适配器要到 dispatch 才被调用，
	// 那时钱已经冻了、图已经承诺了，再 400 就是一次无效流水。
	// 两级原子预扣。
	sess, err := billingService().Freeze(c.Request.Context(), billing.FreezeInput{
		UserID: u.ID, KeyID: k.ID, Model: head.Model, Modality: modality, Group: group,
		IP: c.ClientIP(), RequestID: c.GetHeader("X-Request-Id"), IsStream: head.Stream.Bool(),
		Snapshot: snap, Amount: amount,
		PromptEst: promptEst, CompletionEst: completionEst,
		// 计费维度随冻结一起落库：回收期只有这一行可读（见 FreezeInput.Estimate）。
		Estimate: specUsage,
		// 入口时刻由 handler 传入：Freeze 发生在鉴权/读体/定价/选路之后，
		// 在这里取 now() 会把这几个阶段的实际等待时间漏掉。
		StartedAt: startedAt,
		CallKind:  CallKindSync,
		// 同步链路的幂等键：同一个 Key 在同一个端点上只建一次计费记录。
		//
		// 作用域取路由模板（而不是完整路径）：`/v1/images/generations` 与
		// `/v1/images/edits` 是两个不同的端点，同一个键在两边互不影响；
		// 用模板还能避免把路径参数（任务号之类）算进作用域。
		IdemKey:   strings.TrimSpace(c.GetHeader("Idempotency-Key")),
		IdemScope: strings.TrimSpace(c.FullPath()),
	})
	if err != nil {
		// 重复提交：不扣第二次钱，把首次的记录号告诉调用方。
		// 用 409 而不是 200 重放：同步端点可能返回 base64 图片（几 MB），
		// 平台不存响应体，回放就必须再造一次，那是另一套真值源。
		var idem *billing.ErrIdemReplay
		if errors.As(err, &idem) {
			// 措辞不写"相同请求"：平台**没有存请求体**，无法判断这次是不是同一个请求。
			// 同一个键换了参数再来，结论仍然是"这个键已经用过"（否则幂等就靠猜）。
			gateway.FailDef(c, errs.IdempotentReplay.WithMessage(fmt.Sprintf(
				"该 Idempotency-Key 已经用于本接口的一次请求（计费记录 #%d），本次未重复扣费；如需发起新请求请换一个 Key",
				idem.UsageID)))
			return
		}
		if errors.Is(err, billing.ErrKeyLimitBalanceCap) {
			// 托管密钥顶额且提额已达余额硬顶：语义是"充值即可恢复"，
			// 复用 402 码但给明确行动指引；必须放在 ErrInsufficientQuota 之前。
			gateway.FailDef(c, errs.InsufficientBalance.WithMessage("密钥消费上限已达账户余额上限，充值后自动恢复"))
			return
		}
		if errors.Is(err, billing.ErrInsufficientQuota) {
			gateway.FailDef(c, errs.InsufficientBalance)
			return
		}
		gateway.FailDef(c, errs.Internal)
		return
	}
	// 冻结已成立：从这里到本函数返回，任何 panic 都必须把冻结退掉。
	//
	// middleware.Recover 只回 500、不动账，于是 panic 的请求会停在 pending，
	// 20 分钟后被回收任务按"估算"补账 —— 而那条路径在历史上是最坏值口径。
	// 用户看到的是 500，账单却是一笔扣费，且没有任何地方能把两者对上。
	//
	// 不在这里判断"是否已结算"：Settle 成功与否由 ledger 的 status 决定，
	// ReleaseByUsageID 自己对已终态的 usage 返回 released=false（幂等）。
	defer guardFreezeOnPanic(c, sess)
	// 报文采集器绑定到本次 usage；正文在响应写出之后落库（不影响客户端等待时间）。
	//
	// 采集器拿的是**客户端原文**（req_raw）；实际出站报文由下面的传输层 tap
	// 采集到 upstream_raw。两者分开存，排障时才能看出平台改了什么。
	rec := newIORecorder(kind, u.ID, body, head.Stream.Bool(), startedAt)
	if rec != nil {
		rec.bindUsage(sess.UsageID)
		attach(c, rec)
		// 落库**只在这里**注册一个 defer，不再在每条返回路径上手写 persistIO。
		//
		// 为什么必须等到函数返回：错误响应体是 response.serialize 在返回前写出的，
		// 早期那些「先 persist 再写响应」的调用点会让失败报文永远存不下来。
		// 顺带把 36 处重复调用收成一处（原来漏一处就少落一条报文）。
		defer persistIO(c)
		// 出站报文由**传输层 tap**采集：覆盖 codec 转码、multipart 直链化、
		// failover 换候选与各适配器自己的重建体 —— 手写埋点永远会漏某一条路径。
		// tap 只读 req.GetBody()，不消费原 body（读掉它上游会收到空体）。
		c.Request = c.Request.WithContext(netguard.WithBodyTap(c.Request.Context(), rec))
	}

	dispatch(c, kind, sess, candidates, wireBody, contentType, specUsage, head.Stream.Bool(),
		promptCompletionEst{Prompt: promptEst, Completion: completionEst}, billingModeOf(item), head.Model, codec,
		declaredImageCount(head, imageMaxN(snap.SpecPrice), snap.SpecPrice.MaxN != nil && *snap.SpecPrice.MaxN > 0), &imageSize)
}

// upstreamKind 把「协议变体端点」归一为上游实际使用的端点语义。
//
// 应用的判定只有四件事：**适配器方法选择、token 估算、能否按 token 计费、流式走哪条分支**。
// 不归一的话，规格渠道会拿一个没有 EndpointKey 的端点去查能力表 → 全部判"不支持"；
// estimateTokens 落进 default 返回 (0,0) → 冻结 0 元、结算被 3 倍截顶。
//
// **边界（这条划错会静默把请求打到错的上游）**：上游路径（specOf）、响应 id/model 改写
// （shapeRules）、规格用量（specUsageOf）、图片判定必须用**对外 kind**。
// 例如 responses/compact 的 ukind 是 responses（同一个适配器方法、同一套 usage），
// 但它的上游路径是 /v1/responses/compact —— 拿 ukind 去取路径会打到 /v1/responses。
func upstreamKind(k endpointKind) endpointKind {
	switch k {
	case kindMessages:
		return kindChat
	case kindResponsesCompact:
		// 适配器方法、token 估算、计费闸、流式分支全部按 responses 走：
		// compact 只是"在 responses 上做上下文压缩"，上游语义完全相同。
		return kindResponses
	case kindGeminiGenerate:
		// Gemini 入站但上游说 OpenAI：解码后的请求体就是 chat 请求，
		// 能力判定/方法选择/估算都必须按 chat 走（否则规格渠道会判"不支持"）。
		return kindChat
	case kindGeminiEmbedContent:
		return kindEmbeddings
	default:
		return k
	}
}

// dispatch 按候选顺序派发并处理 failover / 结算 (v2.0 §4.6.2 / §4.6.5)。
//
// B1：每次尝试按 outcome 归因——key 级问题只冷却那枚密钥，调用方问题不记渠道，
// 只有上游自身问题（5xx/连接/超时）才计入渠道健康（见 outcome.go）。
//
// W0-1：**尝试次数与候选下标是两件事**。旧实现用 `i`（切片下标）当次数闸门，导致
//  1. retry_times 实际语义变成「只看前 N 个候选」；
//  2. 一旦某类候选被跳过（适配器未注册 / 不支持该端点 / 取 key 失败 / 被限流），
//     跳过的那几个也会吃掉额度，后面的可用候选永远轮不到。
//
// 现在只统计「真正打到上游」的次数；跳过分类计数，用于给出可归因的终局错误。
func dispatch(c *gin.Context, kind endpointKind, sess *billing.Session, candidates []Candidate, body []byte, contentType string,
	specUsage billing.Usage, stream bool, est promptCompletionEst, mode string, modelID string,
	codec ingressCodec, declaredN int64, imageSize *billing.ImageSize) {
	ctx := c.Request.Context()
	spec := specOf(kind)
	// 上游能力判定与流式判定一律按**归一后的端点**做：
	// 协议变体（messages）在上游看来就是对话，若拿它自己的 kind 去问
	// adapterSupports，规格适配器会按 EndpointKey 查表（messages 没有 key）
	// 而把全部候选判成"不支持"，表现为"所有渠道都不可用"。
	ukind := upstreamKind(kind)
	maxAttempts := retryTimes() + 1
	var lastErr string

	// multipart 直链化结果缓存：多个候选可能都需要同一份直链，
	// 每换一个候选就重传一次文件等于把用户的图重复落到存储、重复占上传配额。
	// 用懒计算 + 单次缓存：只有真的碰到「要求直链」的适配器才会触发上传。
	mp := multipartMaterializer{body: body, contentType: contentType}

	// 渠道限流（P2-4）：每轮尝试先占额度，成功返回前释放。
	// held 在函数返回时兜底释放——限流槽位一旦泄漏就是永久性的（并发额度不会自愈），
	// 所以宁可多一层 defer，也不能依赖每条 return 路径都记得手动释放。
	var held []*slotReleaser
	defer func() {
		for _, r := range held {
			r.Release()
		}
	}()
	limitedOnly := 0   // 被限流拦下的候选数
	retryAfter := 0    // 建议重试秒数（取最大值）
	upstreamCalls := 0 // 真正打到上游的次数（= attempts）
	skippedNoAdapter := 0
	skippedNoCap := 0
	configFailures := 0 // 因渠道声明式配置失败的真实尝试数
	// cm 汇总本次请求里「不属于计费公式、但必须落记录」的元信息（尝试次数/首字延迟/
	// 原始报文/上游 usage 原文）。用结构体而不是继续加参数：settleSession 与
	// releaseSession 的形参已经 9 个，再加 5 个谁也看不懂哪个才是真相源。
	cm := &callMeta{callKind: CallKindSync}
	// 尺寸解释产生的标记（档位未配价 / 默认档未配置 / 别名冲突…）走**既有**的
	// cm.anomaly 通道落账：同步、流式、整段 JSON 兜底三条出口都会带上它。
	// 不在 usage 上另开字段 —— 每条链路本来就只有一个留痕通道。
	cm.anomaly = appendFlag(cm.anomaly, imageSize.Anomaly)
	// 最后一个真实尝试的开始时刻：终局失败时的 upstream_ms 用它算，
	// 原先直接传 time.Now() 会让"全员失败"的记录显示 0ms。
	var lastAttemptStart time.Time
	// lastStatus 是最后一个**拿到过响应**的上游状态码（没拿到响应则为 0）。
	// 终局分档要用它：全员失败里只要最后一个是 429，对外就必须还是 429。
	lastStatus := 0

	// 尝试队列：正常情况下与 candidates 一一对应。开启「换 key 重试」时，
	// 一次失败会在**当前位置之后**插入"同一个渠道 + 已试过 key 的排除集合"，
	// 因此仍然是「先把这个渠道的可用 key 试完，再降级到下一层」，
	// 不会被低优先级渠道插队（那会让优先级配置形同虚设）。
	queue := make([]attempt, 0, len(candidates))
	for _, c := range candidates {
		queue = append(queue, attempt{cand: c})
	}
	for qi := 0; qi < len(queue); qi++ {
		cand, exclude := queue[qi].cand, queue[qi].exclude
		if upstreamCalls >= maxAttempts {
			break
		}
		// 能力判定必须早于限流与取 key：否则会为一个注定失败的候选解密密钥、白占限流槽位。
		adapter, ok := GetAdapter(cand.Channel.AdapterID)
		if !ok {
			// 必须说清是「不存在 / 已停用 / 内容非法」：三者要改的东西完全不同，
			// 而这句话是运营在调用记录里唯一能看到的原因。
			lastErr = adapterMissReason(cand.Channel.AdapterID)
			skippedNoAdapter++
			continue
		}
		if !adapterSupports(adapter, ukind) {
			lastErr = fmt.Sprintf("渠道 %d（%s）不支持端点 %s", cand.Channel.ID, cand.Channel.AdapterID, kindLabel(kind))
			skippedNoCap++
			continue
		}

		// 取 key 必须早于限流（P1-7）。
		//
		// 顺序反过来会白吃配额：rpm 是窗口内的**累加**计数，释放函数不回退它
		// （见 channellimit 包注释）。空密钥池/解密失败的候选一次上游都打不出去，
		// 却已经从这个渠道的每分钟额度里扣掉一笔；配 60 rpm 的渠道被几条坏配置
		// 反复命中时，正常请求会先撞上自己的限流，报的还是「渠道限流」——
		// 一个纯配置问题被伪装成容量问题。
		ref, err := PickKey(cand.Channel, exclude)
		if err != nil {
			// 本渠道可用密钥已试完：不是配置错也不是上游故障，直接换下一个候选，
			// 不能记一次内部错误（那会把"重试预算用尽"算成"渠道配置全坏"）。
			if errors.Is(err, ErrNoCandidate) {
				continue
			}
			// 整池因**限流**冷却：与渠道限流同一档 —— 对外 429 + Retry-After，
			// 不动渠道健康。继续挑冷却中的 key 去打只会把账号级限流拖得更久（P1-8）。
			var cooling *AllKeysCoolingError
			if errors.As(err, &cooling) && !cooling.Auth {
				lastErr = cooling.Error()
				limitedOnly++
				if secs := int(cooling.RetryAfter/time.Second) + 1; cooling.RetryAfter > 0 && secs > retryAfter {
					retryAfter = secs
				}
				continue
			}
			// 密钥池为空/全禁用/全部解密失败/整池鉴权失效：**我方配置问题**，不是上游故障。
			//
			// 记成 outcomeUpstream 的后果很严重：一条空 key 的渠道每次请求都在这里失败，
			// 10 次之内自锁熔断（status=3），模型从目录里消失；而熔断恢复探测
			// （ProbeOne）同样要先拿到 key，于是它永远探不通 —— 渠道不会自愈，
			// 只能人工改库。归因成 internal 后，失败率进的是"配置错"计数，不触发熔断。
			lastErr = err.Error()
			// 指标按原因分档：这三个原因要改的东西完全不同（补 key / 启用 key / 换主密钥）。
			var noKey *NoUsableKeyError
			if errors.As(err, &noKey) {
				metrics.Inc(metrics.NameChannelKeyUnusable, "reason", noKey.Reason())
			} else {
				metrics.Inc(metrics.NameChannelKeyUnusable, "reason", "empty")
			}
			recordAttempt(cand, KeyRef{}, outcomeInternal, 0)
			continue
		}
		// 拿到 key 之后才占额度（P1-7 顺序）。
		//
		// 归属（usages.channel_key_id）在这里记，而不是在各 dispatch 内：
		// 同步 / 流式 / 图片流式三条分支各走独立函数，漏一条就是那条链路
		// 归属恒空 —— 而现象只是"查不到"，没有任何报错。
		cm.noteKey(ref)
		rel, lerr := channellimit.Acquire(ctx, cand.Channel)
		if lerr != nil {
			// 限流是**配置**的结果，不是上游故障：不记渠道健康，直接换下一个候选。
			lastErr = lerr.Error()
			limitedOnly++
			var le *channellimit.LimitError
			if errors.As(lerr, &le) && le.RetryAfter > retryAfter {
				retryAfter = le.RetryAfter
			}
			continue
		}
		r := &slotReleaser{fn: rel}
		held = append(held, r)
		cfg := channelConfig(cand.Channel, ref.Key)
		// 按端点族抬升超时：channelConfig 拿不到 kind（它被 4 处任务链路共用，
		// 改签名是纯噪声），所以在 dispatch 作用域里补这一句。
		cfg.raiseTimeoutFor(kind)
		// multipart 直链化只在适配器显式要求时发生（见 MultipartURLSupporter）：
		// OpenAI 兼容渠道要的是原始 multipart，替它转成 JSON 反而会 400。
		attemptBody, attemptCT, mErr := mp.forAdapter(ctx, c, adapter, kind)
		if mErr != nil {
			var ce *CallerParamError
			if errors.As(mErr, &ce) {
				// 素材不可用是**调用方侧**问题（文件打不开/超限/存储关停）：
				// 换渠道也不会变好，直接终局 400，不做无谓 failover。
				releaseSession(c, sess, http.StatusBadRequest, Candidate{}, "", time.Now(), ce.Msg, cm)
				gateway.FailDef(c, errs.InvalidParam.WithMessage(ce.Msg))
				return
			}
			lastErr = mErr.Error()
			r.Release()
			continue
		}
		req := &RelayRequest{Model: cand.UpstreamModel, ModelID: modelID, Body: attemptBody, ContentType: attemptCT,
			ImageSize: imageSizeFor(imageSize, kind)}
		// 图片端点的响应形态由平台决定：只有实现 ImageStreamer 的候选才走 SSE 分支
		// （下方 stream && isImageKind），其余候选必须**不带** stream。
		//
		// 不剥的后果不是 400，而是静默算错：上游回 SSE 时响应侧 json.Unmarshal 失败，
		// image_verdict 把它判成「0 张」，usage_estimate 再兜底成 1 张 —— 客户端拿到一坨
		// 事件流、账单按 1 张收、全程没有任何报错。
		//
		// 必须删而不是置 false：多米等协议没有 stream 这个键，发 false 也是未知字段。
		if isImageKind(kind) {
			_, isStreamer := adapter.(ImageStreamer)
			req.Body = imageOutboundBody(req.Body, isStreamer)
		}
		start := time.Now()
		// 插件渠道的记录要能一眼认出来：插件失败与上游失败的责任方完全不同。
		if strings.HasPrefix(cand.Channel.AdapterID, "plugin:") {
			cm.callKind = CallKindPlugin
		}

		if stream && ukind == kindChat {
			upstreamCalls++
			cm.attempts = upstreamCalls
			lastAttemptStart = start
			if done := streamDispatch(c, ctx, sess, cand, adapter, cfg, req, body, start, est, mode, ref, cm, codec); done {
				return
			}
			lastErr = "流式渠道失败"
			r.Release()
			continue
		}
		// /v1/responses（含 compact）真流式：按上游路径逐帧转发 SSE。
		//
		// 只认 PathStreamer：不实现的适配器**不回落** Forward —— 客户端要 SSE 却拿到
		// 整段 JSON 是静默的协议破坏。这里记一条可归因的原因后换下一个候选，
		// 全部候选都失败时由既有出口统一释放冻结并报错（复用 failover，不新写错误分支）。
		if stream && ukind == kindResponses {
			if _, ok := adapter.(PathStreamer); ok {
				upstreamCalls++
				cm.attempts = upstreamCalls
				lastAttemptStart = start
				if done := streamForwardDispatch(c, ctx, sess, cand, adapter, cfg, req, body,
					start, est, mode, ref, cm, kind, specOf(kind).path); done {
					return
				}
				lastErr = "流式渠道失败"
				r.Release()
				continue
			}
			lastErr = "该渠道不支持流式响应"
			r.Release()
			continue
		}
		// 图片流式：只有实现了 ImageStreamer 的适配器才走这条分支。
		//
		// 其它适配器（gpt-image 等）保持改造前的行为 —— Forward 整段返回 JSON。
		// 刻意**不**在这里改写它们的语义：给既有渠道新增一条失败路径（400 或降级）
		// 都不是本次任务的目标，而「模型声明了 image_stream 但渠道不是流式适配器」
		// 属于运营配置问题，降级到整段返回比 500 更可用。
		if stream && isImageKind(kind) {
			if _, ok := adapter.(ImageStreamer); ok {
				upstreamCalls++
				cm.attempts = upstreamCalls
				lastAttemptStart = start
				if done := streamImageDispatch(c, ctx, sess, cand, adapter, cfg, req, body,
					start, specUsage, ref, cm, declaredN); done {
					return
				}
				lastErr = "流式渠道失败"
				r.Release()
				continue
			}
		}

		upstreamCalls++
		cm.attempts = upstreamCalls
		lastAttemptStart = start
		resp, err := invokeEndpoint(ctx, adapter, ukind, specOf(kind).path, cfg, req)
		if err != nil {
			aborted := ctx.Err() != nil
			// 配置错单独计数：终局归因是「我方配置问题」而不是上游故障，日志必须能分开。
			var cfgErr *ConfigError
			if errors.As(err, &cfgErr) {
				configFailures++
				// 逐次打 Error：这类错误按设计**不记渠道健康、不熔断**，
				// 不打日志的结果就是「渠道面板常绿、该渠道所有模型 503」
				// （2026-11-04 的真实表现：12 个模型全挂，面板看不出异常）。
				logger.L().Error("渠道配置错误，请求未出网",
					zap.String("channel", cand.Channel.Name),
					zap.String("adapter", cand.Channel.AdapterID),
					zap.Error(cfgErr))
			}
			// 同步出图的超时不能按「上游故障」处理：换渠道 = 重新生成一次 =
			// 付两次上游成本，用户还可能先后拿到两张不同的图。包装规则见
			// wrapImageTimeout（只对图片族、且客户端未断开时生效）。
			err = wrapImageTimeout(kind, err, aborted, cfg.Timeout)
			attemptOutcome := classify(StatusOf(err), err, aborted)
			recordAttempt(cand, ref, attemptOutcome, StatusOf(err))
			// 连接失败/超时（没拿到任何响应）才换 key：上游 5xx 换 key 大概率还是 5xx，
			// 只会多付一次上游调用。
			if shouldSwitchKey(attemptOutcome, StatusOf(err), cand.Channel.SwitchKeyRetry,
				maxAttempts-upstreamCalls) {
				queue = insertKeyRetry(queue, qi, cand, exclude, ref.ID)
			}
			if st := StatusOf(err); st > 0 {
				lastStatus = st
			}
			if aborted {
				// 客户端断开：无法继续 failover，按失败释放（499 = 客户端关闭）。
				//
				// 文案优先用带上游任务号的那条：同步桥接已经把生成提交给上游了，
				// 客户端走后上游常常还会成功。任务号是事后补图（或向渠道追偿）的唯一线索，
				// 丢掉它就只能看到一句「客户端断开」，白付一次生成成本且无从追查。
				msg := "客户端断开"
				upstreamID := ""
				var ab *SyncAbandonedError
				if errors.As(err, &ab) {
					msg = ab.Error()
					// 任务号同时落到 upstream_request_id：管理台与 CSV 导出都认这一列，
					// 塞在 500 字符的错误文案里会被截断、也没法按列筛选。
					upstreamID = ab.TaskID
				}
				releaseSession(c, sess, statusClientClosed, cand, upstreamID, start, msg, cm)
				// 这条路径带 SyncAbandonedError 的上游任务号（已建单），
				// call_io 是后续追偿的唯一证据 —— 其它 13 处 release 都写了，这里不能漏。
				c.Abort()
				return
			}
			// 适配器在**本地**就判定「调用方参数错」（模型与端点不匹配、n 超限、
			// 参数该模型不支持…）：换渠道也不会变好，必须立刻 400 并把原因透出来。
			//
			// 不短路时的表现：把全部候选都试一遍，最后回 503「全部渠道调用失败」——
			// 用户明明只是用错模型，却看到「服务端全挂了」，而且白白多打几次上游。
			var callerErr *CallerParamError
			if errors.As(err, &callerErr) {
				// 用户可见文案统一「脱敏 + 截断」：适配器把上游响应体片段拼进了 Msg，
				// 原文常带上游主机名与 `Authorization: Bearer sk-...`。原文留 call_io，
				// 对外只留脱敏版；两个字段都由 setErr 一处写，不会漏。
				cm.setErr(callerErr.Error(), CallerMessageOf(err, cand.Channel, ref.Key), cand.Channel, ref.Key)
				releaseSession(c, sess, http.StatusBadRequest, cand, "", start, cm.errMsg, cm)
				gateway.FailDef(c, errs.InvalidParam.WithMessage(cm.errMsg))
				return
			}
			// 同步桥接等待超时：上游没错、渠道也没坏，是「我们不愿再等」。
			// 必须 504 且**不换渠道重试**——换一个渠道就要重新提交一次上游任务，
			// 等于为一个注定超时的请求再付一次上游成本。
			var syncTO *SyncWaitTimeoutError
			if errors.As(err, &syncTO) {
				cm.setErr(syncTO.Error(), syncTO.Error(), cand.Channel, ref.Key)
				// 409 而不是 504：任务**已经提交给上游**了，只是我们不愿再等。
				// 504 在主流 SDK 的重试白名单里，自动重试会再提交一次任务、
				// 再计一次费 —— 用户拿到两份产物，平台付两次成本。
				releaseSession(c, sess, http.StatusConflict, cand, syncTO.TaskID, start, cm.errMsg, cm)
				gateway.FailDefMsg(c, errs.UpstreamResultUnknown, cm.errMsg)
				return
			}
			// 提交歧义：上游可能已经受理（网络中断/5xx），换渠道重试会再付一次上游钱。
			// 502 但不换渠道 + 打 submit_ambiguous 供对账筛出这批记录。
			var amb *AmbiguousSubmitError
			if errors.As(err, &amb) {
				cm.setErr(amb.Error(), amb.Error(), cand.Channel, ref.Key)
				cm.anomaly = appendFlag(cm.anomaly, model.AnomalySubmitAmbiguous)
				recordAttempt(cand, ref, outcomeUpstream, 0)
				r.Release()
				releaseSession(c, sess, http.StatusBadGateway, cand, "", start, cm.errMsg, cm)
				gateway.FailDefMsg(c, errs.UpstreamSubmitAmbiguous,
					"上游提交结果未知，任务可能已创建；请勿直接重试，先到调用记录确认")
				return
			}
			// 同步出图超时（P1-1）：与「提交歧义」同一性质 —— 上游可能已经出图并计费。
			// 必须停止 failover（换渠道 = 再生成一次 = 付两次钱 + 用户拿到两张图），
			// 并打 submit_ambiguous 供对账筛出「付了上游但没交付」的这一批。
			//
			// 归因仍走 internal（Class()=="slow"）：慢是上游特性，不是渠道故障，
			// 记成 upstream 会让「模型越慢越容易熔断自己的渠道」。
			var imgTO *ImageGenTimeoutError
			if errors.As(err, &imgTO) {
				cm.setErr(imgTO.Error(), imgTO.Error(), cand.Channel, ref.Key)
				cm.anomaly = model.AnomalySubmitAmbiguous
				recordAttempt(cand, ref, outcomeInternal, 0)
				r.Release()
				// 409 而不是 504：上游可能已经出图并计费，504 会诱发自动重试，
				// 于是「再生成一次 + 再付一次」。文案与码同源，不再自相矛盾。
				releaseSession(c, sess, http.StatusConflict, cand, "", start, cm.errMsg, cm)
				gateway.FailDefMsg(c, errs.UpstreamResultUnknown,
					"请求超时未返回结果，可能已生成；请勿直接重试（会产生第二次费用），"+
						"稍后在调用记录/产物列表确认")
				return
			}
			// 上游响应体超限：**不换渠道**。
			//
			// 每个渠道回的都是同一个超大垃圾体，重试只是把 N 家都撞一遍，
			// 而图片/长文本端点每次真实出站都要付上游成本。二进制流式路径
			// （writeResponse 处）早已这样处置，这里补上同步 JSON 路径 ——
			// 在此之前它经 classify 落 outcomeUpstream，会先换 key 再换渠道。
			if errors.Is(err, ErrUpstreamBodyTooLarge) {
				cm.setErr(err.Error(), "上游响应体超过上限，已停止重试", cand.Channel, ref.Key)
				recordAttempt(cand, ref, outcomeUpstream, 0)
				r.Release()
				releaseSession(c, sess, http.StatusBadGateway, cand, "", start, cm.errMsg, cm)
				gateway.FailDefMsg(c, errs.UpstreamBodyError,
					"上游返回的响应过大，本次未产生结果，未扣费；请勿直接重试")
				return
			}
			// 插件错误串同样脱敏：它会写进 usages.error_msg（门户可见 + CSV 导出）。
			lastErr = redactUpstream(err.Error(), cand.Channel, ref.Key)
			r.Release()
			continue
		}
		if resp.status >= 200 && resp.status < 300 {
			// 图片同步的**响应语义闸**（必须早于 recordAttempt/结算）。
			//
			// 上游对「整单没出图」的表示是 200 + 顶层 error；而计费侧的兜底是
			// 「0 张也按 1 张收」。两者叠加就是「用户一张没拿到、却被收 1 张」。
			// 判据与计数同源（image_verdict.go），这里只负责「明确失败就别收钱」。
			//
			// 不 failover：与 ImageGenTimeoutError 同一处置。上游已经明确答复
			// 「这次没产出」，换渠道会再真实出一次站；而这个分支必然先 releaseSession，
			// 继续重投就会造出「已释放又重投」的双账。
			if isImageKind(kind) && !spec.binary {
				if out := judgeImageResponse(resp.body); out.Verdict == imageUpstreamRejected ||
					out.Verdict == imageUpstreamFailed {
					status, def, oc := http.StatusBadGateway, errs.ServiceUnavailable.WithMessage(out.UserMessage()), outcomeUpstream
					if out.Verdict == imageUpstreamRejected {
						// 审核拦截是**用户内容**问题：400 且不记渠道健康。
						// 记成上游故障的后果是「一条健康渠道因为几个违规 prompt 被熔断」。
						status, def, oc = http.StatusBadRequest, errs.InvalidParam.WithMessage(out.UserMessage()), outcomeInternal
					}
					recordAttempt(cand, ref, oc, status)
					cm.anomaly = joinFlags([]string{cm.anomaly, model.AnomalyImageResponseFailed})
					// usages.error_msg 里要带上上游原话（已脱敏）：它是对账时唯一能
					// 区分「审核拦的」与「上游挂了」的依据，而对外文案两者都不透传。
					detail := out.UserMessage()
					if up := strings.TrimSpace(out.Code + " " + out.Message); up != "" {
						detail += "（上游：" + up + "）"
					}
					cm.setErr(string(resp.body), detail, cand.Channel, ref.Key)
					releaseSession(c, sess, status, cand, "", start, cm.errMsg, cm)
					gateway.FailDef(c, def)
					return
				}
			}
			// 通用「2xx 但响应体是错误」判闸（chat / embeddings / rerank / moderations）。
			//
			// 上面的图片判闸只覆盖「0 张 + error」这一种语义；其余端点以前完全不判，
			// 于是上游用 200 回 {"error":{...}} 时会被当成成功：照常结算扣费，
			// 还把上游错误体原样写给客户端（用户看到「200 里套 error」）。
			// 判据刻意做窄：顶层有 error 且没有任何成功形状（choices/data/output…），
			// 所以「部分成功 + 顶层 error」不会被误判。
			if !spec.binary {
				if msg, isErr := judgeErrorEnvelope(resp.body); isErr {
					recordAttempt(cand, ref, outcomeUpstream, http.StatusBadGateway)
					cm.setErr(string(resp.body), msg, cand.Channel, ref.Key)
					cm.anomaly = joinFlags([]string{cm.anomaly, model.AnomalyUpstreamBodyError})
					releaseSession(c, sess, http.StatusBadGateway, cand, "", start, cm.errMsg, cm)
					persistIO(c)
					gateway.FailDefMsg(c, errs.UpstreamBodyError,
						"上游返回了错误（本次未产生结果，未扣费），请稍后重试")
					return
				}
			}
			recordAttempt(cand, ref, outcomeSuccess, resp.status)
			// 截断判据必须按**该端点自己的**上限，且必须与实际读取点一致
			// （适配器侧读体走 BodyCapFor / MaxBinaryBodyBytes）：
			// 用 chat 的 8 MiB 去比，会把长音频（32 MiB）和 4K 出图（64 MiB）
			// 标成 body_truncated，每次正常请求都多一条假告警。
			upLimit := BodyCapFor(spec.path)
			if spec.binary {
				upLimit = MaxBinaryBodyBytes
			}
			billUsage, source, anomaly := buildBillingUsage(kind, resp.usage, est, mode, specUsage,
				resp.body, 0, int64(len(resp.body)) >= upLimit, false, declaredN)
			cm.fillSuccess(resp)
			// 上游实际出图尺寸：只在响应里明确带了 size 时才记，绝不推断。
			if isImageKind(kind) {
				cm.upstreamSpec = upstreamImageSpec(resp.body)
			}
			if spec.binary {
				// TTS（§4.3.4 规则5）：完整发送后结算；部分送达按实测结算并打标；
				// 一个字节都没送出去才全额释放。
				n, werr := writeResponse(c, resp)
				// 二进制体不进 resp_raw，size 就是这里拿到的写入量 ——
				// 流式分支的 resp.body 是 nil，不接这个值就只能记 0。
				cm.respSize = n
				// 归因必须分开：客户端主动断开是 499（用户的锅），
				// 上游体超限是上游违约 → 502 + 渠道健康度，否则「上游塞了 100 MiB 音频」
				// 会被永久记成「用户取消」，这条渠道永远不会被熔断。
				if errors.Is(werr, ErrUpstreamBodyTooLarge) {
					recordAttempt(cand, ref, outcomeUpstream, 0)
					cm.setErr(werr.Error(), "上游音频体超过上限（"+werr.Error()+"）", cand.Channel, ref.Key)
					releaseSession(c, sess, http.StatusBadGateway, cand, "", start, cm.errMsg, cm)
					c.Abort()
					return
				}
				if werr != nil && n == 0 {
					// 一个字节都没送出去：用户什么都没拿到 → 全额释放。
					releaseSession(c, sess, statusClientClosed, cand, "", start, "客户端断开，音频未送出", cm)
					return
				}
				if werr != nil {
					// 部分送达：已写出的音频前段用户可播，不能既不给费也不给货。
					anomaly = appendFlag(anomaly, model.AnomalyResponseShortWrite)
				}
				settleSession(c, sess, cand, billUsage, resp.status, start, "", digest(body), source, anomaly, cm)
				return
			}
			// 交付前的形状处理（顺序固定，二者互斥）：
			//   ① 入站协议 ≠ 平台协议 → 编码器整份转码（id/model 由它自己产出）
			//   ② 否则 → 一次解析完成「图像直链 → 能力 URL」与 id/model 归一
			//
			// 必须都在 cm.fillSuccess 之后：调用记录与 call_io 要留上游真实地址
			// （排查「图是谁给的、给的什么链接」只有这一份线索），而用户拿到的
			// 必须是平台形状。复制 relayResult 而不是原地改：resp 已被采集器引用。
			//
			// 转码与归一不能叠加：Claude 编码器已经把 model 写成平台模型 ID、
			// id 是它自己生成的 msg_，再归一就是两套 id 互相覆盖。
			if codec != nil {
				converted, cerr := codec.ToClient(resp.body, modelID)
				if cerr != nil {
					// 上游给了无法翻译的形状（没有 choices / 工具参数不是 JSON 对象）。
					// 必须**释放冻结**：用户没拿到可用结果，按坏体结算等于白收费。
					// 归类为上游问题（记渠道健康），因为错的确实是返回内容。
					recordAttempt(cand, ref, outcomeUpstream, 0)
					cm.setErr(cerr.Message, cerr.Message, cand.Channel, ref.Key)
					releaseSession(c, sess, cerr.Status, cand, "", start, cm.errMsg, cm)
					gateway.Fail(c, cerr.Status, cerr.Code, cerr.Message)
					return
				}
				clone := *resp
				clone.body = converted
				resp = &clone
			} else if shaped := shapeResponse(kind, modelID, shapeRespID(), siteurl.Base(c), resp.body); shaped != nil {
				clone := *resp
				clone.body = shaped
				resp = &clone
			}
			// 顺序必须是「先交付、后结算」。
			//
			// 反过来（先 settle 再写）会在客户端已经断开时把一次**没送达**的调用记成
			// settled：钱扣了、图没到，而调用记录里显示 200 成功。用户看到的正是
			// 「上游成功了、系统也成功了，只有我这边 504」——排查时会去怀疑上游，
			// 实际是我们自己把没交付的响应当成了已交付。
			//
			// 这里用写入量判定送达，且必须**分两档**：
			//   - 一个字节都没写出去（n==0）：用户什么都没拿到 → 全额释放；
			//   - 部分送达（0<n<len）：前 n 字节真的到了用户手里（图片前几帧可用），
			//     按实测结算并打 response_short_write，供客服与对账区分两者。
			// 以前把两者合并成「全额释放」：拿到 95% 后断开的用户可以白拿产物。
			//
			// 交付体在写出前记一次：上面可能已经改写了产物直链、也可能做了协议转码，
			// 此刻的 resp.body 才是客户端真正收到的那一份（采集器内部与上游原文比较，
			// 相同则不落库）。
			recorderOf(c).deliver(resp.body)
			n, werr := writeResponse(c, resp)
			cm.respSize = n
			if werr == nil && n < int64(len(resp.body)) {
				werr = io.ErrShortWrite
			}
			if werr != nil {
				cm.setErr(werr.Error(), werr.Error(), cand.Channel, ref.Key)
				if n == 0 {
					releaseSession(c, sess, statusClientClosed, cand, "", start,
						"客户端断开，响应未送出："+werr.Error(), cm)
					return
				}
				anomaly = appendFlag(anomaly, model.AnomalyResponseShortWrite)
			}
			settleSession(c, sess, cand, billUsage, resp.status, start, "", digest(body), source, anomaly, cm)
			return
		}
		// 非 2xx：按归属归因后决定换渠道还是原样透传并释放冻结。
		o := classify(resp.status, nil, false)
		recordAttempt(cand, ref, o, resp.status)
		lastStatus = resp.status
		// 同渠道换 key 重试（默认关）：401 说明这把 key 废了、429 说明它被限流，
		// 换同渠道另一把往往立刻成功；比"降级到下一层渠道"更保优先级，
		// 也避免把一个还有好几把好 key 的渠道整体跳过。
		if shouldSwitchKey(o, resp.status, cand.Channel.SwitchKeyRetry, maxAttempts-upstreamCalls) {
			lastErr = upstreamSnippet(resp.body, cand.Channel, ref.Key)
			r.Release()
			queue = insertKeyRetry(queue, qi, cand, exclude, ref.ID)
			continue
		}
		// 已发生的尝试次数 = upstreamCalls（decideAfterResponse 的 attempt 是 0-based）。
		if decideAfterResponse(resp.status, upstreamCalls-1, maxAttempts, false) == decideRetry {
			lastErr = upstreamSnippet(resp.body, cand.Channel, ref.Key)
			r.Release()
			continue
		}
		// 上游错误原文留存在 call_io 与 usages.error_msg（管理台按 request_id 可查全文），
		// 但**不再回传给调用方**：上游报文里既有上游模型名（`doubao-seedance-2-0-260128`），
		// 也有上游自己的主机与产品名，原样透传等于把供应链交给终端用户。
		cm.setErr(string(resp.body), upstreamSnippet(resp.body, cand.Channel, ref.Key), cand.Channel, ref.Key)
		releaseSession(c, sess, resp.status, cand, "", start, cm.errMsg, cm)
		if resp.status == http.StatusTooManyRequests {
			// 上游限流必须原样回 429。
			//
			// 遮成 503 的后果很直接：OpenAI SDK 只在 429 上做指数退避，503 会被当成
			// 「服务故障」**立刻重试** —— 上游正在限流时，客户端集体加速重试 = 雪崩。
			if retryAfter > 0 {
				c.Header("Retry-After", strconv.Itoa(retryAfter))
			}
			gateway.FailDef(c, errs.RateLimited)
			return
		}
		if !failUpstreamCaller(c, resp.status) {
			gateway.FailDef(c, errs.ServiceUnavailable.WithMessage(fmt.Sprintf(
				"服务暂时不可用（HTTP %d），本次未扣费，请稍后重试", resp.status)))
		}
		return
	}

	// 全员失败：释放冻结。
	if !lastAttemptStart.IsZero() {
		cm.upstreamMs = int(time.Since(lastAttemptStart).Milliseconds())
	}
	// 终局分档必须在落库**之前**算出来：`releaseSession` 会把状态码写进
	// `usages.status_code`，先写 503 再回 429 会让调用记录与客户端看到的不一致，
	// 事后按记录排障会得出"平台回 503"的错误结论。
	//
	// 两档 429 的语义不同，但对外的处置相同（退避后重试，不要报障）：
	//   - 一次上游都没打出去、纯粹被**我们自己的**渠道限流拦下（limitedOnly）；
	//   - 打到了上游，最后一个是 429（上游在限流这个账号）。
	rateLimited := (upstreamCalls == 0 && limitedOnly > 0) ||
		lastStatus == http.StatusTooManyRequests
	termStatus := http.StatusServiceUnavailable
	if rateLimited {
		termStatus = http.StatusTooManyRequests
	}
	releaseSession(c, sess, termStatus, Candidate{}, "", time.Now(), lastErr, cm)

	if rateLimited {
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		gateway.FailDef(c, errs.RateLimited)
		return
	}
	// 全部真实尝试都因「渠道声明式配置」失败：既不是上游故障也不是调用方的错。
	// 内部归因进日志（运维要能定位到是哪个渠道的哪条配置），对外只说平台暂时不可用。
	if upstreamCalls > 0 && configFailures == upstreamCalls {
		gateway.FailDef(c, errs.ServiceUnavailable)
		return
	}
	// 有候选但全都不支持该端点（典型：只声明 text 的插件渠道被绑到 image 模型）。
	// 候选数/未注册数是运营指标，只进日志，不进错误体。
	if upstreamCalls == 0 && skippedNoCap > 0 {
		logger.L().Warn("路由候选能力不匹配",
			zap.String("model", modelID),
			zap.String("endpoint", kindLabel(kind)),
			zap.Int("skipped_no_cap", skippedNoCap),
			zap.Int("skipped_no_adapter", skippedNoAdapter))
		gateway.FailDef(c, errs.EndpointUnsupported)
		return
	}
	gateway.FailDef(c, errs.ServiceUnavailable)
}

// kindLabel 返回端点的可读名称（仅用于错误文案与日志，不参与判定）。
func kindLabel(kind endpointKind) string {
	switch kind {
	case kindChat:
		return "chat/completions"
	case kindCompletions:
		return "completions"
	case kindEmbeddings:
		return "embeddings"
	case kindImagesGenerations:
		return "images/generations"
	case kindImagesEdits:
		return "images/edits"
	case kindImagesVariations:
		return "images/variations"
	case kindAudioSpeech:
		return "audio/speech"
	case kindAudioTranscriptions:
		return "audio/transcriptions"
	case kindAudioTranslations:
		return "audio/translations"
	case kindModerations:
		return "moderations"
	case kindRerank:
		return "rerank"
	case kindResponses:
		return "responses"
	case kindMessages:
		return "messages"
	case kindResponsesCompact:
		return "responses/compact"
	case kindGeminiGenerate:
		return "gemini/generate-content"
	case kindGeminiEmbedContent:
		return "gemini/embed-content"
	default:
		return "unknown"
	}
}

// slotReleaser 把「只释放一次」包成幂等操作：限流槽位重复释放会把额度放大。
type slotReleaser struct {
	once sync.Once
	fn   func()
}

func (r *slotReleaser) Release() {
	if r == nil || r.fn == nil {
		return
	}
	r.once.Do(r.fn)
}

// relayResult 是派发结果的统一视图（JSON 或二进制体 + 解析出的上游用量）。
type relayResult struct {
	status int
	ctype  string
	body   []byte
	// stream 非 nil 时 body 为空：大二进制体（TTS 音频）边收边发，不整段驻留内存。
	// 结算口径不变（先完整发送再结算，§4.3.4 规则5），只是不再先收完再发。
	stream io.ReadCloser
	usage  UsageInfo
}

// invokeEndpoint 调用适配器：多模态端点走通用透传，其余走专用方法。
//
// path 由调用方按**对外 kind** 给出（而不是在这里用 kind 现查）：kind 是归一后的
// 上游语义（compact → responses），拿它去 specOf 会取到 /v1/responses；
// 而上游真正要打的是 /v1/responses/compact。路径归路径、语义归语义。
func invokeEndpoint(ctx context.Context, a Adapter, kind endpointKind, path string, cfg *ChannelConfig, req *RelayRequest) (*relayResult, error) {
	if path != "" {
		raw, err := a.Forward(ctx, cfg, req, path)
		if err != nil {
			return nil, err
		}
		ct := raw.ContentType
		if ct == "" {
			ct = "application/json"
		}
		// 桥接链路回传的产物时长：有值时作为上游用量交给结算，
		// 让"按秒计费"落在**实际生成秒数**上而不是请求参数上。
		var usage UsageInfo
		if raw.DurationSecs > 0 {
			usage.AudioDurationSecs = raw.DurationSecs
		}
		// 没有 extract.duration 的规格（上游查询接口不给时长）：从音频头读。
		// 这是按秒计费在没有上游时长时的最后一道真实来源；读不出来就是 0，
		// 由结算按最低消费兜底并记 anomaly —— 绝不拿"请求要了 5 秒"冒充产物 5 秒。
		if usage.AudioDurationSecs == 0 && kind == kindAudioSpeech && len(raw.Body) > 0 {
			usage.AudioDurationSecs = AudioDurationSecs(raw.Body)
		}
		if raw.Stream != nil {
			// 二进制流式体：usage 恒为上游给不出（音频没有 token 用量），
			// 计费口径来自请求侧估算（specUsage），这里不解析。
			return &relayResult{status: raw.StatusCode, ctype: ct, stream: raw.Stream, usage: usage}, nil
		}
		if parsed := ParseUsage(raw.Body); parsed.HasTokenUsage() || parsed.AudioDurationSecs > 0 {
			// 整块搬用量而不是逐字段赋值：逐字段写法每新增一条通道就要记得加一行，
			// 漏了就是"上游报了、账单没算"（CacheWrite5m/1h 与 ImageTokens 曾经如此）。
			// 只有 AudioDurationSecs 例外：raw.DurationSecs（规格 extract.duration）
			// 是上游**显式**给的产物时长，优先于响应体里的 duration 字段。
			dur := usage.AudioDurationSecs
			usage.Usage = parsed.Usage
			if dur > 0 {
				usage.AudioDurationSecs = dur
			}
			usage.Source, usage.Raw = parsed.Source, parsed.Raw
		}
		return &relayResult{status: raw.StatusCode, ctype: ct, body: raw.Body, usage: usage}, nil
	}
	resp, err := callAdapter(ctx, a, kind, cfg, req)
	if err != nil {
		return nil, err
	}
	return &relayResult{status: resp.StatusCode, ctype: "application/json", body: resp.Body, usage: ParseUsage(resp.Body)}, nil
}

// writeResponse 写出二进制/非 JSON 响应；返回 error 表示未完整发送。
//
// 区分「客户端断开」与「上游体超限」：两者的处置完全不同——
// 前者是用户取消（499、全额释放），后者是上游违约（502、全额释放 + 渠道健康度）。
// 用布尔值表达会把超限误记成客户端断开，运维永远查不到真实原因。
func writeResponse(c *gin.Context, r *relayResult) (int64, error) {
	if r.ctype != "" {
		c.Header("Content-Type", r.ctype)
	}
	c.Status(r.status)
	if r.stream != nil {
		defer r.stream.Close()
		// 用 32KiB 定长缓冲直传：既不攒整段音频，也让客户端逐块收到。
		buf := make([]byte, 32*1024)
		n, err := io.CopyBuffer(c.Writer, r.stream, buf)
		if err != nil {
			// 返回已写出的字节数：调用方要拿它记"音频实际多大"。
			// 记 0 会让调用记录里出现"mime=audio/mpeg · 0 B"这种自相矛盾的行。
			return n, err
		}
		if f, ok := c.Writer.(http.Flusher); ok {
			f.Flush()
		}
		return n, nil
	}
	n, err := c.Writer.Write(r.body)
	return int64(n), err
}

// streamDispatch 处理流式对话；返回 true 表示本次请求已终结（无需再 failover）。
//
// ref 用于把本次结果归因到具体密钥（401/429 只冷却这一枚；空 ID 只记渠道级）。
// codec 非 nil 时上游每帧经编码器翻译后再写给客户端（入站协议 ≠ 上游协议）。
func streamDispatch(c *gin.Context, ctx context.Context, sess *billing.Session, cand Candidate,
	adapter Adapter, cfg *ChannelConfig, req *RelayRequest, body []byte, start time.Time,
	est promptCompletionEst, mode string, ref KeyRef, cm *callMeta, codec ingressCodec) bool {
	started := false
	flusher, _ := c.Writer.(http.Flusher)
	rec := recorderOf(c)
	// 本次响应唯一的对外 id：所有帧共用，客户端看到的是同一个。
	respID := shapeRespID()

	// writeFrame 是唯一的写出点（原始帧与编码后的事件共用）。
	//
	// event 为空时输出 "data: <payload>\n\n"，与改造前**逐字节一致**
	// ——OpenAI 端点的流式输出正在被大量客户端解析，顺手改格式会打掉一片。
	writeFrame := func(event string, data []byte) error {
		if !started {
			// P2-2：必须**先设头再写状态码**。原实现先 Status(200) 后设头，
			// 一旦 Writer 已写出响应头，Content-Type / X-Accel-Buffering 就会丢失
			// （表现为客户端按 text/plain 处理、Nginx 侧缓冲导致「假卡住」）。
			for k, v := range gateway.StreamHeaders() {
				c.Header(k, v)
			}
			c.Status(http.StatusOK)
			started = true
		}
		if event == "" && len(data) == 0 {
			return nil
		}
		_, err := c.Writer.Write(gateway.Frame(event, data))
		if flusher != nil {
			flusher.Flush()
		}
		return err
	}

	// 编码器按「每次真实尝试」新建：它持有块序号与已关闭状态，
	// 复用会让第二次尝试从半截接着写。
	var enc conv.StreamEncoder
	if codec != nil {
		enc = codec.NewStream(req.ModelID, writeFrame)
	}

	onFrame := func(f SSEFrame) error {
		if f.Done {
			// 编码器自己出 message_stop，不能再发 [DONE]（Claude 客户端不认识它）。
			if codec != nil {
				return nil
			}
			return writeFrame("", []byte("[DONE]"))
		}
		if len(f.Data) == 0 {
			return nil
		}
		// 采集器只吃上游原始载荷：拼接最终文本 / 记首帧延迟 / 计帧数。
		// 必须在转换之前——它记录的是"上游说了什么"，排障与用量复核都靠它。
		rec.onFrame(f.Data)
		if codec != nil {
			return enc.OnChunk(f.Data)
		}
		// 帧内归一：只改已存在的 id/model。采集器拿到的仍是上游原文，
		// 客户端看到的是平台 id 与平台模型名（否则流式与非流式会是两套 id）。
		return writeFrame(f.Event, shapeFrame(kindChat, req.ModelID, respID, f.Data))
	}

	res, err := adapter.StreamChat(ctx, cfg, req, onFrame)
	if err != nil {
		// P1-12：首帧之前客户端就断开了 —— 上游一个 token 都没产出，
		// 必须全额释放（原实现走 settle(200) 兜底估算，等于零输出也收费）。
		if !started && ctx.Err() != nil {
			recordAttempt(cand, ref, outcomeClientAbort, 0)
			releaseSession(c, sess, statusClientClosed, cand, "", start, "客户端断开（未收到首帧）", cm)
			c.Abort()
			return true
		}
		if started {
			// 已开始输出：按已收结算（无 usage 时用兜底估算，不再视为免费），并记一次上游失败。
			recordAttempt(cand, ref, classify(0, err, ctx.Err() != nil), 0)
			var usage UsageInfo
			observed := int64(0)
			if res != nil {
				usage = res.Usage
				observed = res.CompletionTokensEst
			}
			billUsage, source, anomaly := buildBillingUsage(kindChat, usage, est, mode, billing.Usage{},
				nil, observed, false, true, 0)
			msg := redactUpstream(err.Error(), cand.Channel, ref.Key)
			// clientAborted 已由 buildBillingUsage 打了 client_abort；
			// 只有「客户端仍在等、上游断了」才是 stream_interrupted。
			// 两种情况都写 error_msg：账单要能解释「为什么这次金额只算了半截」。
			if ctx.Err() == nil {
				anomaly = joinFlags([]string{anomaly, model.AnomalyStreamInterrupted})
			}
			cm.setErr(msg, msg, cand.Channel, ref.Key)
			// 先告诉客户端"这条流断了"，再结算：
			// 不发 error 事件的话，客户端会把半截回答当成正常结束。
			if enc != nil {
				_ = enc.Fail(msg)
			}
			settleSession(c, sess, cand, billUsage, http.StatusOK, start, "", digest(body), source, anomaly, cm)
			c.Abort()
			return true
		}
		recordAttempt(cand, ref, classify(0, err, false), 0)
		// 首帧前就判定为调用方参数错（如模型不支持某个参数）：换渠道也不会变好，
		// 立刻 400 并透出原因，与同步链路同一口径。
		var callerErr *CallerParamError
		if errors.As(err, &callerErr) {
			cm.setErr(callerErr.Error(), CallerMessageOf(err, cand.Channel, ref.Key), cand.Channel, ref.Key)
			msg := cm.errMsg
			releaseSession(c, sess, http.StatusBadRequest, cand, "", start, msg, cm)
			gateway.FailDef(c, errs.InvalidParam.WithMessage(msg))
			return true
		}
		return false
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		recordAttempt(cand, ref, classify(res.StatusCode, nil, false), res.StatusCode)
		// settleSession 的 ErrorMsg 取自 cm.errMsg：只写 respErr 的话，
		// "交付了 N 张后上游断流"在账单上会是完美成功、error_msg 空。
		cm.setErr(res.ErrorMsg, res.ErrorMsg, cand.Channel, ref.Key)
		if started {
			releaseSession(c, sess, res.StatusCode, cand, "", start, cm.errMsg, cm)
			c.Abort()
			return true
		}
		if retryableStatus(res.StatusCode) {
			return false
		}
		releaseSession(c, sess, res.StatusCode, cand, "", start, cm.errMsg, cm)
		if !failUpstreamCaller(c, res.StatusCode) {
			gateway.FailDef(c, errs.ServiceUnavailable.WithMessage(fmt.Sprintf(
				"服务暂时不可用（HTTP %d），本次未扣费，请稍后重试", res.StatusCode)))
		}
		return true
	}
	if res.ErrorMsg != "" {
		// 流中错误帧。两种结局必须分开，否则同一事件会有两个价格：
		//   - 已交付 ≥1 帧：用户拿到了内容，按**已收**结算 + stream_interrupted
		//     （与上面「已开始输出 + 上游断」同一口径）；
		//   - 零帧：用户一无所获，全额释放。
		// 以前这里一律全额释放，于是「上游推了 30 帧再报错」变成免费交付，
		// 而上游成本已经发生 —— 与同文件另一条分支的结论正好相反。
		recordAttempt(cand, ref, outcomeUpstream, res.StatusCode)
		cm.setErr(res.ErrorMsg, res.ErrorMsg, cand.Channel, ref.Key)
		if enc != nil {
			_ = enc.Fail(cm.errMsg)
		}
		if res.Frames > 0 {
			billUsage, source, anomaly := buildBillingUsage(kindChat, res.Usage, est, mode, billing.Usage{},
				nil, res.CompletionTokensEst, false, true, 0)
			anomaly = joinFlags([]string{anomaly, model.AnomalyStreamInterrupted})
			settleSession(c, sess, cand, billUsage, http.StatusOK, start, "", digest(body), source, anomaly, cm)
			c.Abort()
			return true
		}
		releaseSession(c, sess, http.StatusOK, cand, res.UpstreamRequestID, start, cm.errMsg, cm)
		return true
	}
	// P1-31：上游 200 却一帧数据都没给（空 SSE / 只回 [DONE] / 写完响应头就断开）。
	//
	// 改造前这类响应会走「成功但无 usage」分支 → 按估算结算 → 用户为一无所获付钱，
	// 且渠道健康度完全正常（谁都不会发现这条渠道在空转）。
	// 现在一律：全额释放 + 502 empty_response + 记一次上游失败。
	if res.Frames == 0 {
		recordAttempt(cand, ref, outcomeUpstream, res.StatusCode)
		releaseSession(c, sess, http.StatusBadGateway, cand, res.UpstreamRequestID, start,
			"upstream_empty_stream：上游返回零数据帧", cm)
		if !started {
			// 客户端还没收到任何响应头：回明确的错误体，别让它以为是空答案。
			// 走 response 包而不是直写 JSON —— 直写就绕过了线格式判定，
			// Claude 入站会拿到 OpenAI 形状的错误体。
			gateway.FailDef(c, errs.EmptyResponse)
			return true
		}
		// 已经按 SSE 写了头（只可能是 [DONE]）：不能再写 JSON 体，直接断开。
		c.Abort()
		return true
	}
	recordAttempt(cand, ref, outcomeSuccess, res.StatusCode)
	// 正常收尾：编码器补 message_delta（含完整用量）与 message_stop。
	// 写失败只记日志、不改变结算：正文已逐帧交付，缺的只是一个终止事件，
	// 把它当成"未送达"会把这些用户实际收到的回答变成免费。
	if enc != nil {
		if cerr := enc.Close(); cerr != nil {
			logger.L().Warn("流式收尾写入失败（客户端可能已断开）",
				zap.Uint64("usage_id", sess.UsageID), zap.Error(cerr))
		}
	}
	billUsage, source, anomaly := buildBillingUsage(kindChat, res.Usage, est, mode, billing.Usage{},
		nil, res.CompletionTokensEst, false, false, 0)
	cm.upstreamID = res.UpstreamRequestID
	cm.rawUsage = res.Usage.Raw
	cm.upstreamMs = int(time.Since(start).Milliseconds())
	settleSession(c, sess, cand, billUsage, res.StatusCode, start, res.UpstreamRequestID, digest(body), source, anomaly, cm)
	return true
}

// streamForwardDispatch 处理「按上游路径逐帧转发 SSE」的端点（/v1/responses 及其 compact）。
// 返回 true 表示本次请求已终结（无需再 failover）。
//
// 与 streamDispatch 的关系：结构对齐（同样的先交付后结算、同样的释放/结算出口），
// 但**不复用函数**，因为终止语义完全不同：
//   - chat 流靠 `[DONE]` 结束、usage 在任一帧的顶层 usage 里；
//   - responses 流**不发 [DONE]**，终态是一条 `response.completed|incomplete|failed` 事件，
//     usage 嵌在 `response.usage`（顶层没有）—— 按 chat 的解析器读它会得到 0，
//     于是"按估算计费 + output_unmeasured"，每一笔都偏。
//
// 三条不变量（漏一条就会静默出错）：
//  1. 先设头再写状态码（P2-2 教训）；
//  2. 心跳与帧写共用一把锁（gin 的 ResponseWriter 非并发安全）；
//  3. 只有非文本事件也要判成功：工具调用型回答没有文本 delta，按文本判会误退款。
func streamForwardDispatch(c *gin.Context, ctx context.Context, sess *billing.Session, cand Candidate,
	adapter Adapter, cfg *ChannelConfig, req *RelayRequest, body []byte, start time.Time,
	est promptCompletionEst, mode string, ref KeyRef, cm *callMeta, kind endpointKind, path string) bool {

	streamer, ok := adapter.(PathStreamer)
	if !ok {
		// 不能回落 Forward：客户端要的是 SSE，拿到整段 JSON 是**静默的协议破坏**
		// （SDK 按事件流解析会报 unexpected token，而平台记录里是一次 200 成功）。
		return false
	}
	flusher, _ := c.Writer.(http.Flusher)
	rec := recorderOf(c)
	respID := shapeRespID()

	var wmu sync.Mutex
	started := false
	var frames int

	// write 是唯一的写出点（帧与心跳共用，必须串行）。
	write := func(event string, data []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		if !started {
			for k, v := range gateway.StreamHeaders() {
				c.Header(k, v)
			}
			c.Status(http.StatusOK)
			started = true
		}
		if event == "" && len(data) == 0 {
			return nil
		}
		_, err := c.Writer.Write(gateway.Frame(event, data))
		if flusher != nil {
			flusher.Flush()
		}
		return err
	}

	// 心跳：上游长时间不吐字时（推理模型的首帧可能要几十秒），中间的代理与客户端 SDK
	// 都按「多久没收到字节」判卡死。注释帧对解析器无副作用。
	hbStop := make(chan struct{})
	var hbOnce sync.Once
	var hbDone sync.WaitGroup
	hbDone.Add(1)
	stopHeartbeat := func() {
		hbOnce.Do(func() { close(hbStop) })
		hbDone.Wait()
	}
	defer stopHeartbeat()
	go func() {
		defer hbDone.Done()
		t := time.NewTicker(imageStreamHeartbeat)
		defer t.Stop()
		for {
			select {
			case <-hbStop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				wmu.Lock()
				if started {
					_, _ = c.Writer.Write([]byte(": ping\n\n"))
					if flusher != nil {
						flusher.Flush()
					}
				}
				wmu.Unlock()
			}
		}
	}()

	// 终态观测：文本累计（无 usage 时用于估算）+ 终态事件类型 + 终态 usage。
	var outText strings.Builder
	var terminal string
	var failMsg string
	var finalUsage UsageInfo

	onFrame := func(f SSEFrame) error {
		if f.Done {
			// responses 协议不发 [DONE]；上游真发了也不转给客户端（解析器会当成脏数据）。
			return nil
		}
		if len(f.Data) == 0 {
			return nil
		}
		rec.onFrame(f.Data)
		frames++
		var ev respStreamEvent
		if json.Unmarshal(f.Data, &ev) == nil {
			switch ev.Type {
			case "response.output_text.delta":
				outText.WriteString(ev.Delta)
			case "response.output_text.done":
				// 与 delta 累计交叉校验：取较大值，避免上游只发 done 不发 delta 时
				// 把输出量记成 0（那会让这次调用按"零输出"结算）。
				if len(ev.Text) > outText.Len() {
					outText.Reset()
					outText.WriteString(ev.Text)
				}
			case "response.completed", "response.incomplete":
				terminal = ev.Type
				if ev.Response != nil && len(ev.Response.Usage) > 0 {
					if u, ok := usageFromResponses(ev.Response.Usage); ok {
						finalUsage = u
					}
				}
			case "response.failed", "error":
				terminal = ev.Type
				if ev.Response != nil && ev.Response.Error != nil && ev.Response.Error.Message != "" {
					failMsg = ev.Response.Error.Message
				}
				if ev.Error != nil && ev.Error.Message != "" {
					failMsg = ev.Error.Message
				}
			}
		}
		return write(f.Event, shapeFrame(kind, req.ModelID, respID, f.Data))
	}

	res, err := streamer.StreamPath(ctx, cfg, req, path, onFrame)
	if err != nil {
		if !started && ctx.Err() != nil {
			recordAttempt(cand, ref, outcomeClientAbort, 0)
			releaseSession(c, sess, statusClientClosed, cand, "", start, "客户端断开（未收到首帧）", cm)
			c.Abort()
			return true
		}
		if started {
			recordAttempt(cand, ref, classify(0, err, ctx.Err() != nil), 0)
			var usage UsageInfo
			if res != nil {
				usage = res.Usage
			}
			billUsage, source, anomaly := buildBillingUsage(kind, usage, est, mode, billing.Usage{},
				nil, estimateTextTokens(outText.String()), false, true, 0)
			msg := redactUpstream(err.Error(), cand.Channel, ref.Key)
			if ctx.Err() == nil {
				anomaly = joinFlags([]string{anomaly, model.AnomalyStreamInterrupted})
			}
			cm.setErr(msg, msg, cand.Channel, ref.Key)
			settleSession(c, sess, cand, billUsage, http.StatusOK, start, "", digest(body), source, anomaly, cm)
			c.Abort()
			return true
		}
		recordAttempt(cand, ref, classify(0, err, false), 0)
		var callerErr *CallerParamError
		if errors.As(err, &callerErr) {
			cm.setErr(callerErr.Error(), CallerMessageOf(err, cand.Channel, ref.Key), cand.Channel, ref.Key)
			msg := cm.errMsg
			releaseSession(c, sess, http.StatusBadRequest, cand, "", start, msg, cm)
			gateway.FailDef(c, errs.InvalidParam.WithMessage(msg))
			return true
		}
		return false
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		recordAttempt(cand, ref, classify(res.StatusCode, nil, false), res.StatusCode)
		cm.setErr(res.ErrorMsg, res.ErrorMsg, cand.Channel, ref.Key)
		if started {
			releaseSession(c, sess, res.StatusCode, cand, "", start, cm.errMsg, cm)
			c.Abort()
			return true
		}
		if retryableStatus(res.StatusCode) {
			return false
		}
		releaseSession(c, sess, res.StatusCode, cand, "", start, cm.errMsg, cm)
		if !failUpstreamCaller(c, res.StatusCode) {
			gateway.FailDef(c, errs.ServiceUnavailable.WithMessage(fmt.Sprintf(
				"服务暂时不可用（HTTP %d），本次未扣费，请稍后重试", res.StatusCode)))
		}
		return true
	}

	observed := estimateTextTokens(outText.String())

	// 终态是失败事件：上游明确告诉你这条流不成立。
	if terminal == "response.failed" || terminal == "error" {
		recordAttempt(cand, ref, outcomeUpstream, res.StatusCode)
		msg := failMsg
		if msg == "" {
			msg = "上游流式返回失败（未给出原因）"
		}
		cm.setErr(msg, redactUpstream(msg, cand.Channel, ref.Key), cand.Channel, ref.Key)
		if frames > 0 {
			// 已经交付了内容：按已收结算 + stream_interrupted（与 chat 流同一口径：
			// 用户拿到了东西就不能全退，但账单要能解释为什么只算了半截）。
			billUsage, source, anomaly := buildBillingUsage(kind, finalUsage, est, mode, billing.Usage{},
				nil, observed, false, true, 0)
			anomaly = joinFlags([]string{anomaly, model.AnomalyStreamInterrupted})
			settleSession(c, sess, cand, billUsage, http.StatusOK, start, "", digest(body), source, anomaly, cm)
			c.Abort()
			return true
		}
		releaseSession(c, sess, http.StatusBadGateway, cand, res.UpstreamRequestID, start, cm.errMsg, cm)
		if !started {
			gateway.FailDef(c, errs.UpstreamBodyError.WithMessage(cm.errMsg))
			return true
		}
		c.Abort()
		return true
	}

	// 零帧且没有终态：上游写了响应头就断开（或在空转）。
	if frames == 0 {
		recordAttempt(cand, ref, outcomeUpstream, res.StatusCode)
		releaseSession(c, sess, http.StatusBadGateway, cand, res.UpstreamRequestID, start,
			"upstream_empty_stream：上游返回零数据帧", cm)
		if !started {
			gateway.FailDef(c, errs.EmptyResponse)
			return true
		}
		c.Abort()
		return true
	}

	recordAttempt(cand, ref, outcomeSuccess, res.StatusCode)
	// 没有终态事件但有内容帧：上游直接关连接是常见形态（与 chat 流一致），按成功结算。
	// usage 缺失时由 ensureUsage 退回估算并打 output_unmeasured 标记。
	billUsage, source, anomaly := buildBillingUsage(kind, finalUsage, est, mode, billing.Usage{},
		nil, observed, false, false, 0)
	cm.upstreamID = res.UpstreamRequestID
	cm.rawUsage = finalUsage.Raw
	cm.upstreamMs = int(time.Since(start).Milliseconds())
	settleSession(c, sess, cand, billUsage, res.StatusCode, start, res.UpstreamRequestID, digest(body), source, anomaly, cm)
	return true
}

// respStreamEvent 是 Responses 流式事件的判别视图（只取网关关心的字段）。
type respStreamEvent struct {
	Type     string `json:"type"`
	Delta    string `json:"delta"`
	Text     string `json:"text"`
	Response *struct {
		Usage json.RawMessage `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"response"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// streamImageDispatch 处理图片端点流式转发；返回 true 表示本次请求已终结（无需再 failover）。
//
// 与 streamDispatch（对话流式）**同结构、不共用**计费与采集：
//   - 对话流式按 token 结算、采集靠文本拼接；
//   - 图片流式按「实际交付张数」结算，采集只留 completed 帧原文。
//
// 强行合并会让两条链路的判定条件互相污染（例如把图片帧塞进 rec.onFrame 的文本拼接器）。
//
// 关键设计：**先交付、后结算**。与同步链路同一哲学（relay.go 的 settle 顺序注释）——
// 反过来会在客户端已断开时把一次没送达的调用记成 settled：钱扣了、图没到，
// 而调用记录显示 200 成功。
func streamImageDispatch(c *gin.Context, ctx context.Context, sess *billing.Session, cand Candidate,
	adapter Adapter, cfg *ChannelConfig, req *RelayRequest, body []byte, start time.Time,
	specUsage billing.Usage, ref KeyRef, cm *callMeta, declaredN int64) bool {

	streamer, ok := adapter.(ImageStreamer)
	if !ok {
		return false
	}
	started := false
	var wrote int64 // 已写字节数（cm.respSize）
	delivered := 0  // 成功下发的图帧数（结算口径之一）
	flusher, _ := c.Writer.(http.Flusher)
	rec := recorderOf(c)
	// 对外基址：与 rewriteMediaURLs 同一来源（site.api_base_url 优先，回落 Host）。
	base := siteurl.Base(c)

	// 写锁：心跳是**另一个 goroutine**，gin 的 ResponseWriter 不是并发安全的。
	// 两处写必须串行，否则一帧图的字节会被心跳插进中间，客户端解析直接炸。
	var wmu sync.Mutex

	// writeHead 写 SSE 响应头（幂等）。触发点有两个：
	//   1. 适配器回调「上游已接受」（P1-9）—— 首帧可能还要 90 秒，先让客户端看到 200；
	//   2. 第一帧真的到来（未实现回调的适配器走这里，行为与改造前一致）。
	writeHead := func() {
		wmu.Lock()
		defer wmu.Unlock()
		if started {
			return
		}
		// 顺序不能反：先设头再写状态码（P2-2 教训，见 streamDispatch）。
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("X-Accel-Buffering", "no")
		c.Status(http.StatusOK)
		started = true
	}

	// 心跳：SSE 注释帧（客户端与解析器都忽略，零风险）。
	//
	// 为什么必须有：Ark 接受了请求但 4K 组图可能 90 秒不出帧。中间的反向代理
	// 与客户端 SDK 都按「多久没收到字节」判空闲，静默 90 秒会被当成卡死并主动断开，
	// 于是一次真实的生成变成「客户端断开 + 全额退款 + 上游成本照付」。
	hbStop := make(chan struct{})
	var hbOnce sync.Once
	var hbDone sync.WaitGroup
	hbDone.Add(1)
	// stopHeartbeat 关闭心跳并**等它退出**。必须等：wrote 是心跳与 onFrame 共享的
	// 累加量，收尾时读它做 resp_size，若心跳还在写就是数据竞争（且会漏/多记字节）。
	stopHeartbeat := func() {
		hbOnce.Do(func() { close(hbStop) })
		hbDone.Wait()
	}
	defer stopHeartbeat()
	headerSent := func() bool {
		wmu.Lock()
		defer wmu.Unlock()
		return started
	}
	go func() {
		defer hbDone.Done()
		t := time.NewTicker(imageStreamHeartbeat)
		defer t.Stop()
		for {
			select {
			case <-hbStop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if !headerSent() {
					continue
				}
				wmu.Lock()
				n, werr := c.Writer.Write([]byte(": ping\n\n"))
				wrote += int64(n)
				if flusher != nil {
					flusher.Flush()
				}
				wmu.Unlock()
				if werr != nil {
					return
				}
			}
		}
	}()

	onFrame := func(f ImageFrame) error {
		writeHead()
		// 计数必须用**改写前**的事实（f.HasImage / f.URL）：base 为空时（未配
		// site.api_base_url 且 Host 不可用）不改写，但这一帧仍然有图 —— 用改写结果判
		// 「有没有图」会把它漏计成免费单。b64 帧没有 url，靠 HasImage 兜住。
		hasImage := f.HasImage || strings.TrimSpace(f.URL) != ""
		data := f.Data
		if hasImage && base != "" && strings.TrimSpace(f.URL) != "" {
			if out, ok := rewriteImageFrameURL(f.Data, base); ok {
				data = out
			}
		}
		buf := make([]byte, 0, len(data)+64)
		if f.Event != "" {
			buf = append(buf, "event: "...)
			buf = append(buf, f.Event...)
			buf = append(buf, '\n')
		}
		buf = append(buf, "data: "...)
		buf = append(buf, data...)
		buf = append(buf, '\n', '\n')
		wmu.Lock()
		n, err := c.Writer.Write(buf)
		wrote += int64(n)
		wmu.Unlock()
		if err == nil && hasImage {
			delivered++
		}
		if flusher != nil {
			flusher.Flush()
		}
		return err
	}

	// 适配器在拿到 2xx 时回调：先把头写出去，让客户端立刻看到「已开始」。
	req.OnAccepted = writeHead
	defer func() { req.OnAccepted = nil }()

	res, err := streamer.StreamImages(ctx, cfg, req, onFrame)
	// 流已结束：先停心跳再记账（wrote 是两者共享的累加量）。
	stopHeartbeat()
	if res == nil {
		// 适配器违约返回 nil：当成「零帧」处理，绝不能带着 nil 走到结算
		// （结算会按「≥1 张」兜底，等于用户为一无所获付钱）。
		res = &ImageStreamResult{}
	}
	res.Delivered = delivered

	// —— 200 但上游回的是整段 JSON（P3-2）：按普通响应交付，绝不能当空流丢弃 ——
	//
	// 上游已经出图并计费，此时判「零帧」= 用户付钱拿到 502 + 图被丢掉。
	if len(res.RawJSON) > 0 {
		return imageStreamJSONFallback(c, sess, cand, specUsage, res, body, start, ref, cm, rec, declaredN)
	}

	// —— 未开流（上游还没接受就失败）：不写 SSE 头，按普通错误处理（可 failover）——
	// 注意判据是 headerSent 而不是「有没有帧」：上游接受后我们会立刻写头（P1-9），
	// 此时即使零帧也已经无法改状态码，必须走下面的「已开流」收尾。
	if !started {
		if ctx.Err() != nil {
			recordAttempt(cand, ref, outcomeClientAbort, 0)
			releaseSession(c, sess, statusClientClosed, cand, "", start, "客户端断开（未收到首帧）", cm)
			c.Abort()
			return true
		}
		// 调用方参数错（例如该模型不支持 stream、size 非法）：换渠道不会变好，立刻 400。
		var callerErr *CallerParamError
		if errors.As(err, &callerErr) {
			cm.setErr(callerErr.Error(), CallerMessageOf(err, cand.Channel, ref.Key), cand.Channel, ref.Key)
			msg := cm.errMsg
			releaseSession(c, sess, http.StatusBadRequest, cand, "", start, msg, cm)
			gateway.FailDef(c, errs.InvalidParam.WithMessage(msg))
			return true
		}
		if res != nil && (res.StatusCode < 200 || res.StatusCode >= 300) {
			recordAttempt(cand, ref, classify(res.StatusCode, nil, false), res.StatusCode)
			cm.setErr(res.ErrorMsg, res.ErrorMsg, cand.Channel, ref.Key)
			if retryableStatus(res.StatusCode) {
				return false
			}
			releaseSession(c, sess, res.StatusCode, cand, "", start, cm.errMsg, cm)
			if !failUpstreamCaller(c, res.StatusCode) {
				gateway.FailDef(c, errs.ServiceUnavailable.WithMessage(fmt.Sprintf(
					"服务暂时不可用（HTTP %d），本次未扣费，请稍后重试", res.StatusCode)))
			}
			return true
		}
		// 网络层错误（连接/超时/上游体超限）：记一次上游失败，交给 failover。
		recordAttempt(cand, ref, classify(0, err, false), 0)
		cm.setErr(errText(err), errText(err), cand.Channel, ref.Key)
		return false
	}

	// —— 已开流：客户端的响应头已经发出，只能按「已交付多少」收尾 ——
	if delivered == 0 {
		// 客户端先断开（P3-3）：这不是上游违约。
		//
		// 判在「上游零数据帧」之前：客户端取消后适配器必然立刻返回 ctx 错误、
		// 一帧都没有，若按上游失败记，一次用户关页面就会给渠道记一次失败样本，
		// 反复几次就把一条健康渠道熔断；同时会把 499 说成 502「上游返回零数据帧」。
		if ctx.Err() != nil {
			recordAttempt(cand, ref, outcomeClientAbort, 0)
			releaseSession(c, sess, statusClientClosed, cand, "", start, "客户端断开（未收到图片帧）", cm)
			c.Abort()
			return true
		}
		// 一张图都没下发（上游写完头就断开 / 只回 [DONE] / 只回用量帧）。
		// 判据用 delivered 而不是 res.Frames：帧数包含心跳与用量帧，
		// 有帧不等于用户拿到了图，那种情况照样是「付了钱拿到空气」。
		// 必须 502 empty_response + 全额释放。
		recordAttempt(cand, ref, outcomeUpstream, res.StatusCode)
		releaseSession(c, sess, http.StatusBadGateway, cand, "", start,
			"upstream_empty_stream：上游返回零数据帧", cm)
		c.Abort()
		return true
	}

	billUsage, source, anomaly := imageStreamBillUsage(specUsage, res, declaredN)
	if err != nil {
		recordAttempt(cand, ref, classify(0, err, ctx.Err() != nil), 0)
		cm.setErr(errText(err), errText(err), cand.Channel, ref.Key)
	} else {
		recordAttempt(cand, ref, outcomeSuccess, res.StatusCode)
	}
	// 采集：图片帧不能进 rec.onFrame（那是文本拼接口），必须显式写 resp_raw。
	// 不写的话 success() 的 stream 分支会回落成「空的对话聚合体」，排查时看不到张数。
	rec.noteStreamRaw(res.UsageRaw)
	cm.rawUsage = string(res.UsageRaw)
	rec.respSize = wrote
	cm.respSize = wrote
	cm.upstreamMs = int(time.Since(start).Milliseconds())
	// 状态码固定 200（SSE 头已经发出，改不了）：结算张数才是「交付了多少」的真相。
	settleSession(c, sess, cand, billUsage, http.StatusOK, start, "", digest(body), source, anomaly, cm)
	c.Abort()
	return true
}

// imageStreamJSONFallback 交付「上游 200、但报文是整段 JSON 而不是 SSE」的结果（P3-2）。
//
// 为什么不能按「零帧空流」处理：上游已经出图并计费，判零帧 = 用户付钱拿到 502 + 图被丢掉。
//
// 交付形态由「响应头是否已经发出」决定，而不是由我们的偏好决定：
//   - 已发出（适配器在拿到 2xx 时回调过 accepted）→ 包成**一帧 SSE**。此时改
//     Content-Type 已经来不及，客户端本来就是按流解析的；一帧承载完整 JSON 的 data
//     是它能拿到的最好结果，且与「上游其实支持流式、只是这一单回得短」同形。
//   - 未发出 → 按普通 JSON 200 交付，语义最准确（客户端拿到的是它请求的那份结构）。
//
// 无论哪种形态都要按实际张数计费并打 stream_json_fallback 标记：
// 这条标记是「客户端按流写、实际拿到的不是流」的唯一可筛痕迹。
func imageStreamJSONFallback(c *gin.Context, sess *billing.Session, cand Candidate,
	specUsage billing.Usage, res *ImageStreamResult, body []byte, start time.Time,
	ref KeyRef, cm *callMeta, rec *ioRecorder, declaredN int64) bool {

	raw := res.RawJSON

	// 先做语义判定：上游对「整单没出图」的表示是 **HTTP 200 + 顶层 error**
	// （审核拦截 / 服务异常）。以前这条路径只看"数得出几张"，数不出就按 1 张结算 ——
	// 于是同一份响应在同步链路是「400/502 + 全额释放」，在流式兜底却是
	// 「原样转发 + 收 1 张钱」。两条链路必须同一口径。
	out := judgeImageResponse(raw)
	if out.Verdict == imageUpstreamRejected || out.Verdict == imageUpstreamFailed {
		code, def := http.StatusBadGateway, errs.ServiceUnavailable
		if out.Verdict == imageUpstreamRejected {
			code, def = http.StatusBadRequest, errs.InvalidParam
		}
		recordAttempt(cand, ref, outcomeUpstream, code)
		// 上游原文进 call_io：对客户只回一句中性话，原文留给运营。
		// 进 usages.error_msg 的那一份必须脱敏（上游 code/message 里常带上游主机名）。
		cm.setErr(string(raw), out.Code+": "+out.Message, cand.Channel, ref.Key)
		if !c.Writer.Written() {
			c.Header("Content-Type", "application/json")
			gateway.FailDef(c, def.WithMessage(out.UserMessage()))
		} else {
			// 头已发出（客户端已收到 200）：钱一分不能收，但失败必须让客户端看得见。
			// 必须包成 SSE 帧 —— 客户端此刻按 event-stream 解析，裸 JSON 不是合法帧，
			// 合规解析器会整段丢弃，客户端只看到「一条正常结束的空流」，
			// 与「白送一次生成」同症状，且没人能从客户端侧发现。
			payload, _ := json.Marshal(map[string]any{
				"error": map[string]string{"code": "upstream_error", "message": out.UserMessage()},
			})
			buf := make([]byte, 0, len(payload)+16)
			buf = append(buf, "data: "...)
			buf = append(buf, payload...)
			buf = append(buf, '\n', '\n')
			_, _ = c.Writer.Write(buf)
			if f, ok := c.Writer.(http.Flusher); ok {
				f.Flush()
			}
		}
		releaseSession(c, sess, code, cand, "", start, cm.errMsg, cm)
		c.Abort()
		return true
	}

	n := out.Count
	source := "upstream"
	if n <= 0 {
		// 数不出 data 数组（上游换了结构）：按 1 张结算并标 estimated，
		// 让对账能认出这批记录 —— 按 0 张算等于白送一次生成。
		// 注意这只在"没有任何 error 依据"时才会走到（上面已把明确失败分流出去）。
		n = 1
		source = "estimated"
	}
	anomaly := model.AnomalyStreamJSONFallback
	// 上游多给的图不向用户收费：用户只下单了 declaredN 张。
	// 与 SSE 路径（imageStreamBillUsage）同一判据，否则同一份响应在两条出口上
	// 会算出两个价格（整段 JSON 兜底按全张数收 = 多收）。
	if capped, over := capImageCountToDeclared(n, declaredN); over {
		n = capped
		anomaly = joinFlags([]string{anomaly, model.AnomalyImageNOverDelivered})
	}

	if c.Writer.Written() {
		// 已发过 SSE 头：压成单行再包一帧（多行 JSON 直接拼进 data: 会破坏 SSE 分帧）。
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			// 走到这里说明 res.RawJSON 不是合法 JSON —— 适配器契约被破坏，
			// 但不能改成 502：头已发出，且上游确实计费了。原样发出，让客户端看到原文。
			compact.Reset()
			compact.Write(raw)
		}
		frame := make([]byte, 0, compact.Len()+16)
		frame = append(frame, "data: "...)
		frame = append(frame, compact.Bytes()...)
		frame = append(frame, '\n', '\n')
		if _, werr := c.Writer.Write(frame); werr != nil {
			// 客户端已经走了：上游成本无处追偿，按客户端断开全额释放。
			recordAttempt(cand, ref, outcomeClientAbort, 0)
			releaseSession(c, sess, statusClientClosed, cand, "", start, "客户端断开（JSON 兜底交付失败）", cm)
			c.Abort()
			return true
		}
		if f, ok := c.Writer.(http.Flusher); ok {
			f.Flush()
		}
	} else {
		c.Header("Content-Type", "application/json")
		c.Status(http.StatusOK)
		if _, werr := c.Writer.Write(raw); werr != nil {
			recordAttempt(cand, ref, outcomeClientAbort, 0)
			releaseSession(c, sess, statusClientClosed, cand, "", start, "客户端断开（JSON 兜底交付失败）", cm)
			c.Abort()
			return true
		}
	}

	recordAttempt(cand, ref, outcomeSuccess, http.StatusOK)
	if rec != nil {
		rec.noteStreamRaw(raw)
	}
	cm.rawUsage = string(raw)
	cm.respSize = int64(len(raw))
	cm.upstreamMs = int(time.Since(start).Milliseconds())
	billUsage := billing.Usage{ImageN: n}
	carrySpecFields(&billUsage, specUsage)
	settleSession(c, sess, cand, billUsage, http.StatusOK, start, "", digest(body), source, anomaly, cm)
	c.Abort()
	return true
}

// rawJSONImageCount 数「整段 JSON」里到底有几张图（`data[]` 中带 url 或 b64_json 的项）。
//
// 不用 len(data)：Ark 的整段响应里 data 项可能带 error（单张失败），那是没产出的。
func rawJSONImageCount(raw []byte) int64 {
	var probe struct {
		Data []struct {
			URL     string `json:"url"`
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return 0
	}
	var n int64
	for _, d := range probe.Data {
		if strings.TrimSpace(d.URL) != "" || strings.TrimSpace(d.B64JSON) != "" {
			n++
		}
	}
	return n
}

// imageStreamBillUsage 计算图片流式的结算张数（**唯一实现**）。
//
//	n = max(1, min(completed.usage.generated_images, 实际下发成功帧数))
//
// 为什么取 min：completed 是**上游口径**（它按生成张数收我们的钱），
// 下发帧数才是**用户实际拿到的**。两者不同时按用户拿到的收费，差额靠 anomaly 对账，
// 不转嫁给用户 —— 否则「上游生成了但流断了」会变成用户付钱买空气。
//
// 为什么下限是 1：已经开了流就说明至少交付了一张（onFrame 写过一次），
// 否则会出现「收到图却记 0 张」的免费单。
//
// 为什么这里**不夹 max_n**：结算走 imageCharge（strictLimits=false），它已按 spec.MaxN
// 钳过并标记 capped。在这里再夹一次就是第二处上限判定，运营调 max_n 后两处会不同步。
//
// 第三个返回值是 anomaly（两条口径不一致时的对账标记）。
func imageStreamBillUsage(spec billing.Usage, res *ImageStreamResult, declaredN int64) (billing.Usage, string, string) {
	var delivered, generated int64
	if res != nil {
		delivered = int64(res.Delivered)
		generated = res.GeneratedImages
	}
	// 张数规则单源（billing.ResolveImageCount）：流式把「上游报的生成数」当产出、
	// 「已下发帧数」当交付。规则细节不在这里重复，避免与同步路径分叉。
	n, partial := billing.ResolveImageCount(generated, delivered, spec.ImageN)
	// 上游多给的帧不收费：用户只下单了 declaredN 张（未写 n 时不收敛，见 capImageCountToDeclared）。
	overDelivered := false
	if capped, over := capImageCountToDeclared(n, declaredN); over {
		n, overDelivered = capped, true
	}
	u := billing.Usage{ImageN: n}
	carrySpecFields(&u, spec)
	source := "upstream"
	if generated <= 0 {
		// 上游没报张数：退化成「按已交付算」，来源标记为估算，对账要能筛出来。
		source = "estimated"
	}
	anomaly := ""
	if partial {
		anomaly = model.AnomalyStreamPartialDelivered
	}
	if overDelivered {
		anomaly = joinFlags([]string{anomaly, model.AnomalyImageNOverDelivered})
	}
	return u, source, anomaly
}

// errText 取错误文案（nil 安全），供脱敏与落库使用。
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// callAdapter 按端点语义调用适配器（**显式列表，无 default**）。
//
// 曾经是 `default: a.Chat(...)`：新增端点漏登记时会**静默**把请求当对话发到
// /v1/chat/completions —— 渠道返回 200，账单按 chat 算，只有一条 warn 日志。
// 现在落成 ConfigError（不计渠道健康、不重试、逐次打 Error），把"看起来通了、
// 钱算错了"变成一条响亮的配置错误。
func callAdapter(ctx context.Context, a Adapter, kind endpointKind, cfg *ChannelConfig, req *RelayRequest) (*RelayResponse, error) {
	switch kind {
	case kindChat, kindMessages, kindGeminiGenerate:
		return a.Chat(ctx, cfg, req)
	case kindCompletions:
		return a.Completions(ctx, cfg, req)
	case kindEmbeddings, kindGeminiEmbedContent:
		return a.Embeddings(ctx, cfg, req)
	default:
		return nil, &ConfigError{
			Scope: "网关端点实现缺失",
			Field: "endpoint_kind",
			Msg:   "端点 " + kindLabel(kind) + " 没有对应的适配器方法（callAdapter 缺登记）",
		}
	}
}

// settleSession 结算成功调用：按上游 usage 复核计费并落终态（先落账后回响应）。
// 失败重试 2 次（幂等由 usages.status + 流水唯一索引双保险）；最终失败必须留 Error 日志，交由回收任务兜底。
func settleSession(c *gin.Context, sess *billing.Session, cand Candidate, usage billing.Usage, status int,
	start time.Time, upstreamID, reqDigest, source, anomaly string, cm *callMeta) {
	recordChannel(cand, true)
	// 真实记录已产生：预检层的兜底记录必须让位（否则同一次调用会写两条）。
	markUsageRecorded(c)
	if cm == nil {
		cm = &callMeta{}
	}
	if upstreamID == "" {
		upstreamID = cm.upstreamID
	}
	// 成功响应原文进 call_io（流式由采集器自己拼最终文本）。
	if rec := recorderOf(c); rec != nil {
		rec.respSize = cm.respSize
		rec.success(cm.respBody, cm.respCType)
	}
	in := billing.SettleInput{
		StatusCode: status, ChannelID: cand.Channel.ID, AdapterID: cand.Channel.AdapterID,
		ChannelKeyID:      cm.keyID,
		UpstreamRequestID: upstreamID,
		UpstreamMs:        upstreamMsOf(start, cm),
		Attempts:          cm.attempts,
		FirstTokenMs:      cm.firstTokenMs,
		RequestDigest:     reqDigest, Usage: usage, UsageSource: source,
		// 尺寸解释产生的标记只记在 cm.anomaly 上（档位未配价 / 张数夹紧 / 面积越窗…）。
		// 不并进来，它们就只会在"失败释放"路径落库，成功结算全丢 —— 而成功单
		// 恰恰是最需要解释"为什么按这一档收"的那批。
		Anomaly:  joinFlags([]string{anomaly, cm.anomaly}),
		RawUsage: cm.rawUsage, CallKind: cm.callKind, UpstreamSpec: cm.upstreamSpec,
		// 失败但已结算的场景（流式中断）必须留下原因，
		// 否则这条记录在账单上是完美成功，无法解释「用户只拿到半条流」。
		ErrorMsg: cm.errMsg,
	}
	// 账务上下文必须与请求生命周期解耦：客户端断流时 c.Request.Context() 已取消，
	// 而"这一单到底收了多少"不能取决于客户端还在不在。billing 内部也做了
	// WithoutCancel，但那是跨包约定 —— 这里显式写一次，改动 billing 时不会静默丢结算。
	settleCtx, cancelSettle := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 10*time.Second)
	defer cancelSettle()
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		_, err = billingService().Settle(settleCtx, sess, in)
		if err == nil {
			return
		}
		// 会话已关闭 = 这一单已经被别人结过（回收任务抢先 / 迟到补结 / 重复调用）。
		// 绝不能静默：它正是「用户拿到结果、账单却停在估算额」的唯一出口，
		// 不落痕就永远查不出是谁先结的、结成了多少。
		if errors.Is(err, billing.ErrSessionClosed) {
			metrics.Inc(metrics.NameUsageSettleClosed, "phase", "settle")
			logger.L().Warn("结算会话已关闭（已被回收/迟到补结抢先）",
				zap.Bool("billing_session_closed", true),
				zap.Uint64("usage_id", sess.UsageID),
				zap.Uint64("user_id", sess.UserID),
				zap.Int("http_status", status),
				zap.String("usage_source", source))
			return
		}
		time.Sleep(time.Duration(50*(attempt+1)) * time.Millisecond)
	}
	logger.L().Error("计费结算失败（等待回收任务兜底）",
		zap.Bool("billing_settle_failed", true),
		zap.Uint64("usage_id", sess.UsageID),
		zap.Uint64("user_id", sess.UserID),
		zap.Uint64("key_id", sess.KeyID),
		zap.Uint64("channel_id", in.ChannelID),
		zap.String("adapter", in.AdapterID),
		zap.String("usage_source", source),
		zap.Int64("prompt_tokens", usage.PromptTokens),
		zap.Int64("completion_tokens", usage.CompletionTokens),
		zap.Error(err))
}

// upstreamMsOf 返回本次请求「最后一次上游调用」的耗时。
//
// 优先用采集到的较大值：终局失败路径的调用方传的是 time.Now()（拿不到最后一次 start），
// 直接算会恒为 0，而 cm.upstreamMs 在循环里已经被填成真实的最后一次耗时。
func upstreamMsOf(start time.Time, cm *callMeta) int {
	v := 0
	if !start.IsZero() {
		v = int(time.Since(start).Milliseconds())
	}
	if cm != nil && cm.upstreamMs > v {
		v = cm.upstreamMs
	}
	if v < 0 {
		return 0
	}
	return v
}

// releaseSession 释放冻结（失败/取消/超时/耗尽），charge=0。
// adapterMissReason 返回适配器不可用的可读原因（负缓存里带着，取不到时给兜底文案）。
func adapterMissReason(id string) string {
	if err := AdapterResolveReason(id); err != nil {
		return err.Error()
	}
	return "适配器未注册：" + id
}

func releaseSession(c *gin.Context, sess *billing.Session, status int, cand Candidate, upstreamID string,
	start time.Time, errMsg string, cm *callMeta) {
	var channelID uint64
	var adapterID string
	if cand.Channel != nil {
		channelID, adapterID = cand.Channel.ID, cand.Channel.AdapterID
	}
	if cm == nil {
		cm = &callMeta{}
	}
	markUsageRecorded(c)
	// 上游错误原文进 call_io：列表里的 error_msg 只有 500 字符，排障时经常不够。
	if rec := recorderOf(c); rec != nil && len(cm.respErr) > 0 {
		rec.failure(cm.respErr)
	}
	// 统一走 ReleaseByUsageID：冻结额以账本（freeze 流水）为准，不依赖手里这个
	// Session 是否还新鲜。调用方 27 处签名不变，差异只在实现。
	released, err := billingService().ReleaseByUsageID(context.WithoutCancel(c.Request.Context()), sess.UsageID, billing.SettleInput{
		StatusCode: status, ChannelID: channelID, AdapterID: adapterID,
		ChannelKeyID:      cm.keyID,
		UpstreamRequestID: upstreamID, UpstreamMs: upstreamMsOf(start, cm),
		Attempts: cm.attempts, FirstTokenMs: cm.firstTokenMs,
		ErrorMsg: errMsg, UsageSource: "none", CallKind: cm.callKind, Anomaly: cm.anomaly,
	})
	if !released && err == nil {
		// 已被结算/回收抢先（幂等成功）：这里不再动钱，但必须留痕。
		metrics.Inc(metrics.NameUsageSettleClosed, "phase", "release")
		logger.L().Warn("释放会话已关闭（已被结算/回收抢先）",
			zap.Bool("billing_session_closed", true),
			zap.Uint64("usage_id", sess.UsageID),
			zap.Uint64("user_id", sess.UserID),
			zap.Int("http_status", status))
	} else if err != nil {
		logger.L().Error("预扣释放失败（等待回收任务兜底）",
			zap.Bool("billing_release_failed", true),
			zap.Uint64("usage_id", sess.UsageID),
			zap.Uint64("user_id", sess.UserID),
			zap.Uint64("key_id", sess.KeyID),
			zap.Int("http_status", status),
			zap.Error(err))
	}
}

// guardFreezeOnPanic 是 Freeze 之后的 panic 兜底，必须直接 defer 调用。
//
// 它只做一件事：把这次冻结退回去，然后把 panic 原样抛回 middleware.Recover
// （保留 500 与完整堆栈）。守卫自身再套一层 recover —— 释放过程中再 panic
// 绝不能把真正的业务 panic 吞掉，那会变成"静默 200 + 账没退"。
func guardFreezeOnPanic(c *gin.Context, sess *billing.Session) {
	r := recover()
	if r == nil {
		return
	}
	func() {
		defer func() { _ = recover() }()
		// 让 PreflightRecorder 不再补一条"预检失败"记录：这次请求已经真实冻结过。
		markUsageRecorded(c)
		logger.L().Error("网关请求 panic，正在释放预扣",
			zap.Bool("billing_panic_release", true),
			zap.Uint64("usage_id", sess.UsageID),
			zap.Uint64("user_id", sess.UserID),
			zap.Any("panic", r))
		ctx := context.WithoutCancel(c.Request.Context())
		if _, err := billingService().ReleaseByUsageID(ctx, sess.UsageID, billing.SettleInput{
			StatusCode: 500, ErrorMsg: "内部错误（panic），已全额释放", UsageSource: "none",
			CallKind: CallKindSync,
		}); err != nil {
			metrics.Inc(metrics.NameTaskSettleFailures, "stage", "panic_release")
			logger.L().Error("panic 后释放预扣失败（等待回收任务兜底）",
				zap.Bool("billing_release_failed", true),
				zap.Uint64("usage_id", sess.UsageID), zap.Error(err))
			return
		}
		metrics.Inc(metrics.NameUsageSettleClosed, "phase", "panic")
	}()
	panic(r)
}

func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// upstreamSnippet 截取上游错误体片段用于落库（usages.error_msg）。
//
// 与 CallerMessage 同一实现（脱敏 → 按 rune 截断 → 同一上限），差别只是入口收 []byte。
// 两条路径各写一份规则的后果，是「响应里已脱敏、账单里没脱敏」这种半修。
func upstreamSnippet(body []byte, ch *model.Channel, key string) string {
	if len(body) == 0 {
		return ""
	}
	return CallerMessage(string(body), ch, key)
}

// displayModality 把模态枚举翻成中文（仅用于错误文案）。
func displayModality(m string) string {
	switch strings.TrimSpace(m) {
	case "chat":
		return "对话"
	case "image":
		return "图像"
	case "video":
		return "视频"
	case "audio":
		return "音频"
	case "music":
		return "音乐"
	case "embedding":
		return "向量"
	case "rerank":
		return "重排"
	case "moderation":
		return "审核"
	}
	if m == "" {
		return "未知"
	}
	return m
}

// filterCandidatesByAdapter 只保留指定适配器的候选（/v1/responses 仅 OpenAI 兼容渠道）。
// 渠道为空的候选直接丢弃：它在后续流程里必然 panic 或被跳过，留在这里只会让错误更难读。
func filterCandidatesByAdapter(cands []Candidate, adapterID string) []Candidate {
	out := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if c.Channel != nil && c.Channel.AdapterID == adapterID {
			out = append(out, c)
		}
	}
	return out
}

// ParseUsage 解析 OpenAI 兼容响应体中的 usage（缺失返回空用量）。
func ParseUsage(payload []byte) UsageInfo {
	if len(payload) == 0 {
		return UsageInfo{}
	}
	var p struct {
		Usage    json.RawMessage `json:"usage"`
		Duration *float64        `json:"duration"` // ASR 响应时长（秒）
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return UsageInfo{}
	}
	out := UsageInfo{}
	if p.Duration != nil && *p.Duration > 0 {
		out.AudioDurationSecs = int64(*p.Duration + 0.5)
	}
	// 只认顶层 usage：嵌在 data 下的同名结构必须由 response_path 显式取出来。
	// 若在这里递归找，运营配了 response_path 反而会被"双重解析"覆盖。
	if len(p.Usage) == 0 || string(p.Usage) == "null" {
		return out
	}
	// 上游 usage 原文（复核用）：截断到 2000 字符，避免上游把整段报文塞进 usage 字段。
	out.Raw = textx.TruncateRunes(string(p.Usage), 2000)

	// 语义判定：有 prompt_tokens → OpenAI；只有 input_tokens → Anthropic。
	// 这个标记决定 base 怎么算（OpenAI 的 prompt 已含缓存/图片通道，Anthropic 不含），
	// 判错会让缓存请求的计费差近一倍，所以必须显式识别，不能一律按 OpenAI 兜底。
	var probe struct {
		PromptTokens *int64 `json:"prompt_tokens"`
		InputTokens  *int64 `json:"input_tokens"`
	}
	_ = json.Unmarshal(p.Usage, &probe)
	if probe.PromptTokens == nil && probe.InputTokens != nil {
		// 先判 Responses：它的 input_tokens 与 OpenAI 的 prompt_tokens 同义（已含缓存），
		// 而 Anthropic 的不含。判别必须窄（见 usage_responses.go），否则会把 Anthropic
		// 的真实用量按 OpenAI 语义算 —— 反方向的错账。
		if u, ok := usageFromResponses(p.Usage); ok {
			u.Raw = out.Raw
			return u
		}
		out.Semantic = billing.SemanticAnthropic
		var an struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheCreation            *struct {
				Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
				Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		}
		if err := json.Unmarshal(p.Usage, &an); err != nil {
			return out
		}
		out.PromptTokens, out.CompletionTokens = an.InputTokens, an.OutputTokens
		out.CacheTokens = an.CacheReadInputTokens
		if an.CacheCreation != nil {
			// 新版分档上报：5m / 1h 各自的写入量，直接采用。
			out.CacheWrite5mTokens = an.CacheCreation.Ephemeral5m
			out.CacheWrite1hTokens = an.CacheCreation.Ephemeral1h
		} else {
			// 旧版只有一个 cache_creation_input_tokens：按 5 分钟档算（默认 TTL）。
			out.CacheWrite5mTokens = an.CacheCreationInputTokens
		}
		return out
	}

	out.Semantic = billing.SemanticOpenAI
	var oa struct {
		PromptTokens        int64 `json:"prompt_tokens"`
		CompletionTokens    int64 `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int64 `json:"cached_tokens"`
			ImageTokens  int64 `json:"image_tokens"`
			AudioTokens  int64 `json:"audio_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails *struct {
			AudioTokens int64 `json:"audio_tokens"`
		} `json:"completion_tokens_details"`
	}
	if err := json.Unmarshal(p.Usage, &oa); err != nil {
		return out
	}
	out.PromptTokens, out.CompletionTokens = oa.PromptTokens, oa.CompletionTokens
	if d := oa.PromptTokensDetails; d != nil {
		out.CacheTokens = d.CachedTokens
		out.ImageTokens = d.ImageTokens
		out.AudioInputTokens = d.AudioTokens
	}
	if d := oa.CompletionTokensDetails; d != nil {
		out.AudioOutputTokens = d.AudioTokens
	}
	return out
}

// MergeUsageMax 逐字段取较大值合并两次上游用量（流式多帧累计）。
//
// 为什么是"取大"而不是"取最后一帧"：标准 OpenAI 实现只在末帧带 usage，
// 但自建/兼容网关常见"每帧都报、且中间帧缺字段"的形态。取最后一帧会把
// prompt_tokens 之类的首帧字段丢掉 → 计费按 0 走；逐字段取大则两种实现都对。
//
// Raw 取更长的那个：usage 原文只用于事后复核，保留信息量更大的一份即可。
func MergeUsageMax(a, b UsageInfo) UsageInfo {
	out := a
	out.PromptTokens = maxInt64(a.PromptTokens, b.PromptTokens)
	out.CompletionTokens = maxInt64(a.CompletionTokens, b.CompletionTokens)
	out.CacheTokens = maxInt64(a.CacheTokens, b.CacheTokens)
	out.CacheWrite5mTokens = maxInt64(a.CacheWrite5mTokens, b.CacheWrite5mTokens)
	out.CacheWrite1hTokens = maxInt64(a.CacheWrite1hTokens, b.CacheWrite1hTokens)
	out.ImageTokens = maxInt64(a.ImageTokens, b.ImageTokens)
	out.AudioInputTokens = maxInt64(a.AudioInputTokens, b.AudioInputTokens)
	out.AudioOutputTokens = maxInt64(a.AudioOutputTokens, b.AudioOutputTokens)
	out.AudioDurationSecs = maxInt64(a.AudioDurationSecs, b.AudioDurationSecs)
	if b.Semantic != "" {
		out.Semantic = b.Semantic
	}
	if b.Source != "" {
		out.Source = b.Source
	}
	if len(b.Raw) > len(out.Raw) {
		out.Raw = b.Raw
	}
	return out
}

// maxInt64 返回两个 int64 的较大值（本地小工具，避免为两处比较引 math）。
func maxInt64(a, b int64) int64 {
	if b > a {
		return b
	}
	return a
}

// buildSnapshot 由定价目录项构造 pricing_snapshot（Open 时冻结，Settle 只读）。
//
// 快照只装「这笔请求自己的」事实：模型、计费方式、**当时的绝对价目**、分组折扣、币种。
// 不再有比率/刻度 —— 那些中间量会让「改一次配置，在途请求全变价」。
func buildSnapshot(item *pricing.ModelItem, discount int64) billing.Snapshot {
	snap, anomaly := billing.Effective(billing.Snapshot{
		ModelID:     item.ID,
		BillingMode: billingModeOf(item),
		Price:       item.Prices,
		SpecPrice:   item.SpecPrices,
		Discount:    discount,
		Currency:    conf.SiteCurrency(),
		CapturedAt:  time.Now(),
	}, item.Modality)
	snap.ConfigAnomaly = anomaly
	return snap
}

// billingModeOf 返回目录条目的**实际生效**计费方式；空值按按量（与模态默认一致）。
//
// 必须过 billing.EffectiveMode：models.billing_mode 的列默认值是 tokens，
// 「价目配好了但 billing_mode 没改」是最常见的配置错 —— 不纠偏的话
// 下面的二道闸会直接 400（模型完全不可用），而价目其实完全正确。
func billingModeOf(item *pricing.ModelItem) string {
	if item == nil {
		return ""
	}
	return billing.EffectiveMode(item.BillingMode, item.SpecPrices)
}

// findModel 在定价目录中定位模型（未命中返回 nil）。
func findModel(cat *pricing.Snapshot, id string) *pricing.ModelItem {
	for i := range cat.Models {
		if cat.Models[i].ID == id {
			return &cat.Models[i]
		}
	}
	return nil
}

// groupDiscount 返回调用分组的折扣（微比例，1e6 = 不打折）。
//
// 回落从静默改为**限频 Warn**（G2）：孤儿分组在计费时按不打折处理，
// 若没有任何提示，运营会以为用户享了折扣、实际按原价收，通常要等客诉才发现。
// 但这条路径在网关请求里，逐次 Warn 会把日志冲爆，故按 group 每 5 分钟最多一次。
func groupDiscount(cat *pricing.Snapshot, group string) int64 {
	if r, ok := cat.GroupDiscounts[group]; ok && r > 0 {
		return r
	}
	warnOrphanGroup(group)
	return money.Scale
}

// orphanGroupWarnMu 保护 orphanGroupLastWarn；用独立小锁避免与请求主路径争用。
var (
	orphanGroupWarnMu   sync.Mutex
	orphanGroupLastWarn = map[string]time.Time{}
)

// warnOrphanGroup 对「有调用但没配倍率」的分组限频告警一次。
//
// 空串等价于 default（在 effectiveGroup 里已归一），default 由 validate 强制保留，
// 所以这里只对非 default 的缺配分组告警，避免「default 未命中」这种本不可能发生的噪声。
func warnOrphanGroup(group string) {
	if group == "" || group == "default" {
		return
	}
	now := time.Now()
	orphanGroupWarnMu.Lock()
	if last, ok := orphanGroupLastWarn[group]; ok && now.Sub(last) < 5*time.Minute {
		orphanGroupWarnMu.Unlock()
		return
	}
	orphanGroupLastWarn[group] = now
	orphanGroupWarnMu.Unlock()
	logger.L().Warn("调用分组未配置倍率，按 1.0 全额计费", zap.String("group", group))
}

// estimateTokens 估算预扣 token（只用于冻结，不参与结算）。
//
// 参数 modelMaxOutput 是模型声明的单次输出上限（models.max_output_tokens，0=未声明）：
// 请求没写任何输出上限时按它冻结，再退到 defaultMaxOutputTokens。
// 这一层回落必须有 —— 写死 1024 会让长输出请求的结算额数倍于冻结额，
// 被结算侧 3 倍截顶后确定性少收。
func estimateTokens(h relayHead, kind endpointKind, modelMaxOutput int) (int64, int64) {
	// 按**上游语义**估算：协议变体（compact → responses、gemini → chat）在这里
	// 必须归一，否则落进 default 返回 (0,0) → 冻结 0 元 → 结算被 3 倍截顶、
	// 或者直接判"模型未配置价格"把请求打回 400。
	// 归一放在函数内而不是调用点：估算只有这一处实现，调用点会越加越多。
	kind = upstreamKind(kind)
	var raw json.RawMessage
	switch kind {
	case kindEmbeddings:
		raw = h.Input
	case kindCompletions:
		raw = h.Prompt
	case kindRerank:
		raw = append(append(json.RawMessage{}, h.Query...), h.Documents...)
	case kindModerations:
		raw = h.Input
	case kindChat, kindMessages:
		raw = h.Messages
	case kindResponses:
		raw = h.Input
	default:
		return 0, 0
	}
	// CJK 感知估算（estimatePromptTokens）：按 len/4 会把中文系统性低估约 25%，
	// 冻结额随之偏低 → 长中文请求结算时大幅穿透冻结额，触发截顶少收。
	prompt := estimatePromptTokens(raw)
	// tools 与 system 是真实输入 token，此前完全没进估算：
	// 工具 schema 动辄上万字符，带工具或长 system 的请求冻结额会低一个数量级。
	// 只有会带这两项的端点才加（rerank/embeddings/moderations 没有这些字段）。
	// messages 与 chat 一样带 system / tools：漏掉它们会让 Claude 入站的冻结额
	// 低一个数量级（工具 schema 常上万字符），结算被 3 倍截顶后确定性少收。
	if kind == kindChat || kind == kindMessages || kind == kindCompletions || kind == kindResponses {
		prompt += estimatePromptTokens(h.System) + estimatePromptTokens(h.Tools)
	}
	var completion int64
	if kind == kindChat || kind == kindMessages || kind == kindCompletions || kind == kindResponses {
		// 三个字段都可能是"真实的上限"，取最大：用户只传其中一个时其余为 0。
		// max_completion_tokens 之前被解析却完全没用 —— 按文档传参的用户
		// 得到的是 1024 的冻结，与文档承诺相反。
		completion = maxInt64(maxInt64(h.MaxTokens.Int64(), h.MaxCompletionTokens.Int64()), h.MaxOutputTokens.Int64())
		if completion <= 0 && modelMaxOutput > 0 {
			completion = int64(modelMaxOutput)
		}
		if completion <= 0 {
			completion = defaultMaxOutputTokens
		}
		if completion > maxCompletionCap {
			completion = maxCompletionCap
		}
	}
	return prompt, completion
}

// specUsageOf 提取请求侧的按规格/快照参数（图片档位与张数、TTS 字符数、rerank 文档数）。
func specUsageOf(kind endpointKind, h relayHead, size billing.ImageSize, maxN int64, caps imageCaps) billing.Usage {
	switch kind {
	case kindImagesGenerations, kindImagesEdits, kindImagesVariations:
		// 档位来自唯一的归一实现（billing.ResolveImageTier，需要模型价目表），
		// 这里不再自己算任何阈值。
		//
		// 张数用 imageCountHint 而不是 h.N：组图请求（sequential_image_generation=auto）
		// 的 n 通常是 1，真实产出由 max_images 决定（缺省 15）。按 n 冻结会在结算时
		// 大幅穿透冻结额（15 张 vs 1 张），而穿透由平台承担。
		return billing.Usage{
			ImageSpec: size.Tier, ImageRatio: size.Ratio,
			ImageSpecExplicit: size.Explicit, ImageSpecSource: size.Source,
			ImageQuality: string(h.Quality), ImageN: imageCountHint(h, maxN, caps),
			// 参考图数必须与异步链路同源（异步从 input_reference 数）。
			// 冻结期不落它，回收/补结就无法解释"这个 15 张额度为什么只剩 13 张"。
			RefImages: int64(imageRefCount(h.Image))}
	case kindAudioSpeech:
		// 必须数**字符**而不是字节：中文用 UTF-8 占 3 字节，按 len() 会把 40 字的
		// 一句中文算成 120 字 —— 预扣冻结 3 倍、结算兜底也按 3 倍收费（若时长不可测）。
		// 这是按秒计费里最容易"看起来对"的一处单位错。
		return billing.Usage{AudioChars: int64(utf8.RuneCountInString(rawString(h.Input)))}
	case kindRerank:
		return billing.Usage{RerankDocs: countDocs(h.Documents)}
	case kindAudioTranscriptions, kindAudioTranslations:
		// 转写/翻译按秒计价，而**产物在冻结期不存在**：时长只能等上游返回 usage。
		// 什么都不填就是冻结 0，结算期被「零冻结上限」截顶，长音频系统性少收；
		// 余额低的用户还会再被「余额下限」削一次（等于白送）。
		//
		// 这里用上传文件字节数折一个**偏大**的秒数：多冻的钱结算时原路退回，
		// 少冻的钱收不回来。取不到文件（非 multipart 形态）时留 0，
		// 由结算期的配置上限兜底。
		if h.FileBytes <= 0 {
			return billing.Usage{}
		}
		secs := (h.FileBytes + asrBytesPerSecond - 1) / asrBytesPerSecond
		if secs < 1 {
			secs = 1
		}
		return billing.Usage{AudioDurationSecs: secs}
	}
	return billing.Usage{}
}

// imageRefCount 数参考图张数：`image` 既可能是字符串，也可能是字符串数组。
func imageRefCount(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		if strings.TrimSpace(one) == "" {
			return 0
		}
		return 1
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		n := 0
		for _, s := range many {
			if strings.TrimSpace(s) != "" {
				n++
			}
		}
		return n
	}
	return 0
}

// imageCountHint 是图片计费的**预估张数**（冻结期的唯一口径）。
//
// 为什么不能放在适配器里：`Estimate`（本文件 relayRequest 内）发生在
// `SelectCandidates` **之前** —— 那时还不知道会落到哪个适配器，
// 按适配器预估永远拿不到实例。所以它必须是与适配器无关的纯函数。
//
// 规则（照抄 Ark 的默认值，缺一不可）：
//  1. layer_decomposition=true → 17（1 张底图 + 最多 16 个图层），见 billing.ImageLayerDecompMax；
//  2. sequential_image_generation != "auto" → n（缺省 1）；
//  3. == "auto" → max_images（**缺省 15**：不写这个默认值就是最大的穿透点，
//     按 n=1 冻结、按 15 张结算，差额由平台承担）；
//  4. 受「参考图数 + 生成数 ≤ 15」约束（Ark 的硬限制）→ min(hint, 15-refs)；
//  5. 下限 1（0 会让 imageCharge 按 0 张免费出图）。
//
// 图层拆分排在最前：它是一次调用的**产物形态**（底图+图层），与组图的"连续生成 N 张"
// 是互斥场景；按 n 冻结会让 16 个图层全部落在冻结额之外。
//
// 非 Ark 渠道传这些字段吗？不会。它们只影响「传了组图参数」的请求，
// 其它厂商不传就与改造前逐字一致（n 或 1）。
func imageCountHint(h relayHead, maxN int64, caps imageCaps) int64 {
	// 规则单源在 billing.ImageCountHint（异步任务链路也调它）：这里只做 relayHead 适配。
	//
	// 组图 / 图层这两个开关只有在**模型真的具备该能力**时才参与冻结张数：
	// 它们的缺省值来自 Ark 语义（组图 15 张、图层 17 张），而 nano / gpt-image
	// 根本没有这两个参数。不按能力裁剪，向 nano 传一个 sequential_image_generation
	// 就会按 15 张冻结 —— 用户额度被占、上游回一句 400，钱再退回，
	// 期间这个 15 张的窗口对并发请求就是"余额不足"。字段本身仍原样出站（由上游判）。
	seqAuto := strings.EqualFold(strings.TrimSpace(h.SeqImageGen), "auto")
	layer := jsonTrue(h.LayerDecomp)
	if !caps.allowGroup() {
		seqAuto = false
	}
	if !caps.allowLayer() {
		layer = false
	}
	return billing.ImageCountHint(billing.ImageCountInput{
		N:           h.N.Int64(),
		SeqAuto:     seqAuto,
		MaxImages:   h.SeqOptions.MaxImages,
		LayerDecomp: layer,
		RefImages:   imageRefCount(h.Image),
		MaxN:        maxN,
	})
}

// imageCaps 是冻结张数需要的两个能力位（组图 / 图层）。
//
// Known=false 表示"这一层拿不到模型能力"（例如结算期的 declaredImageCount）。
// 此时**不做裁剪**：宁可沿用改造前的行为，也不能凭"不知道"把冻结张数改成 1。
type imageCaps struct {
	Known bool
	Group bool
	Layer bool
}

func (c imageCaps) allowGroup() bool { return !c.Known || c.Group }
func (c imageCaps) allowLayer() bool { return !c.Known || c.Layer }

// imageCapsOf 从目录条目取能力位（唯一映射处：apidoc 的能力常量 → 冻结口径）。
func imageCapsOf(item *pricing.ModelItem) imageCaps {
	if item == nil {
		return imageCaps{}
	}
	caps := apidoc.NormalizeCapabilities(item.Modality, json.RawMessage(item.Capabilities))
	return imageCaps{
		Known: true,
		Group: apidoc.HasCap(caps, apidoc.CapImageGroup),
		Layer: apidoc.HasCap(caps, apidoc.CapImageLayer),
	}
}

// jsonTrue 判断一个 JSON 原始值是否表示"真"（bool true 或字符串 "true"/"1"）。
//
// 存在的理由：同一个开关在不同客户端的写法不同（`true` / `"true"`），
// 只认其中一种的表现是"我明明开了却没生效"，且没有任何报错。
func jsonTrue(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "1", "yes", "on", "auto":
			return true
		}
	}
	return false
}

// rawString 把 JSON 原始值解码为字符串（非字符串 → 原样文本）。
func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// countDocs 统计 rerank documents 数组元素个数（非数组 → 0）。
func countDocs(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var docs []json.RawMessage
	if json.Unmarshal(raw, &docs) == nil {
		return int64(len(docs))
	}
	return 0
}

// jsonBodyAllowed 表示该端点**同时接受 JSON 请求体**（同一能力、两种入参形态）。
//
// 只有 images/edits：OpenAI 官方与 new-api 都两种都收，`/v1/edits` 是它的旧别名
// （历史上只发 JSON）。variations 上游只认表单，放 JSON 进来只会把「形态用错」
// 变成上游一句含糊的 400 —— 那边继续用 multipart_required 指路。
func jsonBodyAllowed(kind endpointKind) bool { return kind == kindImagesEdits }

// isMultipartBody 判断 Content-Type 是否为 multipart/form-data。
//
// 用 ParseMediaType 而不是 hasPrefix：`multipart/form-datax` 这种拼错的类型
// 不该被当成表单（它的 boundary 一定解析不出来，最后报的是「缺 boundary」，
// 而真实原因是类型名写错，文案会把排障方向带偏）。
func isMultipartBody(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && mt == "multipart/form-data"
}

// errNotMultipart 表示 multipart 端点收到了非 multipart（或缺 boundary）的请求体。
// dispatch 据此把错误码从通用 invalid_param 升级为 multipart_required（P7）：
// 接入方按 SDK 习惯打成 JSON 时，必须告诉他「改请求形态」而不是「改字段」。
var errNotMultipart = errors.New("relay: multipart 缺少 boundary")

// parseMultipartHead 从 multipart 请求体提取 model 及计费相关表单字段（文件 part 跳过，表单值截断 64KB）。
func parseMultipartHead(contentType string, body []byte) (relayHead, error) {
	var head relayHead
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" {
		return head, errNotMultipart
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return head, nil
		}
		if err != nil {
			return head, err
		}
		name := p.FormName()
		if name == "" || p.FileName() != "" {
			// 文件 part 只数字节、不读内容：音频按秒计费要靠它估冻结时长，
			// 把整个文件读进内存只为计数没有意义（大文件直接打爆内存）。
			if p.FileName() != "" {
				n, err := io.Copy(io.Discard, p)
				if err != nil {
					return head, err
				}
				head.FileBytes += n
			}
			continue
		}
		val, err := io.ReadAll(io.LimitReader(p, 64<<10))
		if err != nil {
			return head, err
		}
		s := strings.TrimSpace(string(val))
		switch name {
		case "model":
			head.Model = s
		case "size":
			head.Size = flexString(s)
		case "aspect_ratio":
			// multipart 里画幅同样是字符串；转成 JSON 字符串好让 aspectRatioOf 走同一条路。
			if raw, err := json.Marshal(s); err == nil {
				head.AspectRatio = raw
			}
		case "quality":
			head.Quality = flexString(s)
		case "resolution":
			head.Resolution = flexString(s)
		case "image_size":
			head.ImageSize = flexString(s)
		case "seconds":
			head.Seconds = flexString(s)
		case "n":
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				head.N = flexInt(n)
			}
		}
	}
}

// retryTimes 读取 system_config.retry_times（缺省 3）。
func retryTimes() int {
	if raw, ok := conf.System().Get("retry_times"); ok {
		var n int
		if json.Unmarshal(raw, &n) == nil && n > 0 {
			return n
		}
	}
	return defaultRetryTimes
}

// effectiveGroup 返回调用分组：KEY 分组优先，未设置时用用户分组。
func effectiveGroup(k *model.APIKey, u *model.User) string {
	if k.Group != "" {
		return k.Group
	}
	if u.Group != "" {
		return u.Group
	}
	return "default"
}

// defaultModality 返回端点默认模态（对齐定价目录枚举：chat / embedding / image / audio / rerank / moderation）。
func defaultModality(kind endpointKind) string {
	if kind == kindEmbeddings {
		return "embedding"
	}
	if m := specOf(kind).modality; m != "" {
		return m
	}
	return "chat"
}

// digest 计算请求体摘要（sha256 前 16 位，用于去重与溯源）。
func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])[:16]
}

// contains 判断字符串是否在列表内。
func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// apiKeyOf / userOf 从上下文取鉴权对象（TokenAuth 写入）。
func apiKeyOf(c *gin.Context) *model.APIKey {
	if v, ok := c.Get("api_key_record"); ok {
		if k, ok := v.(*model.APIKey); ok {
			return k
		}
	}
	return nil
}

func userOf(c *gin.Context) *model.User {
	if v, ok := c.Get("user_record"); ok {
		if u, ok := v.(*model.User); ok {
			return u
		}
	}
	return nil
}

// Account 处理 GET /v1/account：回显用户余额与当前 KEY 元信息（§4.3.9）。
func Account(c *gin.Context) {
	k := apiKeyOf(c)
	u := userOf(c)
	if k == nil || u == nil {
		gateway.FailDef(c, errs.InvalidAPIKey)
		return
	}
	// 顶层四个额度键也受 allow_user_quota_query 约束。
	//
	// 原先只有 user 子对象受控，顶层 balance/frozen/used 恒下发 —— 关掉开关的
	// 那把 KEY 照样能在这里读到余额，开关等于不存在（同一把 KEY 还能从
	// /dashboard/billing/subscription 拿到换算成美元的同一个数）。
	// key 子对象保留：它是"这把钥匙自己的用量"，不是账户余额。
	data := gin.H{
		"group": effectiveGroup(k, u),
		"key": gin.H{
			"id": k.ID, "name": k.Name, "key_prefix": k.KeyPrefix,
			"amount_limit": k.AmountLimit, "used": k.Used,
			"status": k.Status, "expired_at": k.ExpiredAt, "group": k.Group,
			"rpm_limit": k.RPMLimit, "concurrency_limit": k.ConcurrencyLimit,
		},
	}
	if k.AllowUserQuotaQuery {
		data["balance"] = u.Balance
		data["frozen"] = u.Frozen
		data["used"] = u.Used
		data["request_count"] = u.RequestCount
		data["user"] = gin.H{
			"username": u.Username, "balance": u.Balance, "used": u.Used,
			"request_count": u.RequestCount, "group": u.Group,
			// 回显**生效值**（用户级 > 0 优先，否则全局）。不能直接吐存储列：
			// 列的 0 表示「未单独设置」，原样下发会让调用方以为上限是 0。
			"balance_limit": conf.BalanceLimitFor(u.BalanceLimit),
		}
	}
	response.OK(c, data)
}

// usageFilter 是调用记录筛选条件的唯一实现（门户列表 / 门户导出 / /v1/usage 共用）。
type usageFilter struct {
	Start, End       time.Time
	KeyID            uint64
	Modality, Status string
	Model            string
}

// usageDefaultWindow 给「两端都没传时间」的查询补一个默认窗口。
//
// 为什么必须有：`/api/user/usage` 每次请求都要跑一次 `GROUP BY modality`
// 的全历史聚合（门户首屏默认也不传时间），老账号的一次刷新就是对全表扫一遍。
// 30 天足够覆盖"看最近消费"，要更早的数据显式传 start/end 即可。
//
// 只作用于 strict=true 的门户/用户面；`/v1/usage` 兼容端点不动
// （OpenAI 生态客户端不认这个隐式限制）。
func usageDefaultWindow(f usageFilter) usageFilter {
	if !f.Start.IsZero() || !f.End.IsZero() {
		return f
	}
	days := authcfg.UsageDefaultWindowDays()
	if days <= 0 {
		return f
	}
	f.Start = time.Now().AddDate(0, 0, -days)
	return f
}

// parseUsageFilter 解析 start/end(RFC3339)/modality/status/model/key_id。
// strict=true（门户）：日期不可解析或区间颠倒一律失败，禁止静默丢弃筛选条件；
// strict=false（/v1/usage 兼容端点）：非法日期按「未传」处理，保持既有宽松行为。
func parseUsageFilter(c *gin.Context, strict bool) (usageFilter, bool) {
	var f usageFilter
	parseAt := func(key string, dst *time.Time) bool {
		raw := strings.TrimSpace(c.Query(key))
		if raw == "" {
			return true
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return !strict
		}
		*dst = t
		return true
	}
	if !parseAt("start", &f.Start) || !parseAt("end", &f.End) {
		return f, false
	}
	if !f.Start.IsZero() && !f.End.IsZero() && f.End.Before(f.Start) {
		return f, false
	}
	f.Modality, f.Status, f.Model = c.Query("modality"), c.Query("status"), c.Query("model")
	if raw := strings.TrimSpace(c.Query("key_id")); raw != "" {
		kid, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			// 与日期分支同一条规则：strict（门户）下非法筛选值一律失败。
			// 静默丢弃的表现是「筛了单个 KEY，返回全部 KEY 的本人记录」，
			// 用户会以为自己的账单少了/多了，而不是发现参数写错了。
			if strict {
				return f, false
			}
		} else {
			f.KeyID = kid
		}
	}
	return f, true
}

// apply 叠加筛选条件（列名对齐 usages 表；end 为闭区间上界）。
func (f usageFilter) apply(q *gorm.DB) *gorm.DB {
	if !f.Start.IsZero() {
		q = q.Where("created_at >= ?", f.Start)
	}
	if !f.End.IsZero() {
		q = q.Where("created_at <= ?", f.End)
	}
	if f.KeyID > 0 {
		q = q.Where("key_id = ?", f.KeyID)
	}
	if f.Modality != "" {
		q = q.Where("modality = ?", f.Modality)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.Model != "" {
		q = q.Where("model = ?", f.Model)
	}
	return q
}

// UsageQuery 处理 GET /v1/usage：按当前 KEY 查询调用记录（§4.3.9）。
func UsageQuery(c *gin.Context) {
	k := apiKeyOf(c)
	if k == nil {
		gateway.FailDef(c, errs.InvalidAPIKey)
		return
	}
	f, _ := parseUsageFilter(c, false)
	f.KeyID = 0 // 恒定按当前 KEY 作用域，忽略客户端传入的 key_id
	usageList(c, f.apply(db.DB.Model(&model.Usage{}).Where("key_id = ?", k.ID)))
}

// ModalityStat 是门户「类型分布」项：请求数 + 消耗额度。
type ModalityStat struct {
	Modality string `json:"modality"`
	Requests int64  `json:"requests"`
	Charge   int64  `json:"charge"`
}

// PortalUsage 处理 GET /api/portal/usage：门户（JWT）按本人维度查询调用记录。
// 支持 key_id 二次过滤（Usage 页按 KEY 筛选）并返回与明细同源同口径的类型分布。
func PortalUsage(c *gin.Context) {
	userID := c.GetUint64("user_id")
	if userID == 0 {
		response.FailDef(c, errs.Unauthorized)
		return
	}
	f, ok := parseUsageFilter(c, true)
	if !ok {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	f = usageDefaultWindow(f)
	// 托管密钥只能看自己这把 KEY 产生的调用记录。
	// 覆盖而不是"默认值"：查询参数里的 key_id 是给门户筛选用途，对托管密钥无效。
	if s, _ := keyscope.Of(c); s.Hosted {
		f.KeyID = s.KeyID
	}
	// sort 只在这条端点解析（/v1/usage 兼容端点不认它，见 usagePage 注释）。
	order, ok := sorting.Query(c, usageSortColumns)
	if !ok {
		return
	}
	pg, perr := pagination.Parse(c)
	if perr != nil {
		response.FailDefMsg(c, errs.InvalidParam, perr.Error())
		return
	}
	base := f.apply(db.DB.Model(&model.Usage{}).Where("user_id = ?", userID))
	items, total, page, size, err := usagePage(c, base, order, pg)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	dist, err := modalityDist(c.Request.Context(), f, userID)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, gin.H{
		"total": total, "items": items, "page": page, "page_size": size,
		"modality_dist": dist,
	})
}

// PortalUsageDetail 处理 GET /api/portal/usage/:id：本人单条调用记录详情。
//
// 门户版是 logs.PortalDetailView（白名单投影）：不含渠道/适配器/IP/上游单号/上游 usage
// 原文/计费快照/报文原文/上游媒体直链；保留扣费解释、耗时、产物入口与「输出文本」摘要。
// 报文原文只在管理台可见（/api/admin/logs/:id）。
// usageBelongsToKey 判断一条调用记录是否由指定 KEY 产生。
//
// 只用于托管密钥的详情/产物读取：存在性本身不返回给调用方（一律 404），
// 所以这里不需要区分"没有这条"与"不是你这条"。
func usageBelongsToKey(ctx context.Context, usageID, keyID uint64) bool {
	var n int64
	if err := db.DB.WithContext(ctx).Model(&model.Usage{}).
		Where("id = ? AND key_id = ?", usageID, keyID).Count(&n).Error; err != nil {
		return false
	}
	return n > 0
}

func PortalUsageDetail(c *gin.Context) {
	userID := c.GetUint64("user_id")
	if userID == 0 {
		response.FailDef(c, errs.Unauthorized)
		return
	}
	id, err := strconv.ParseUint(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id == 0 {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	if s, _ := keyscope.Of(c); s.Hosted {
		if !usageBelongsToKey(c.Request.Context(), id, s.KeyID) {
			// 与「不存在」同码：托管密钥不该知道别的应用/平台直连下有没有这条记录。
			response.FailDef(c, errs.NotFound)
			return
		}
	}
	// 产物链接必须给**绝对地址**：调用方不在我们的 origin 上。
	// 前缀走 taskview.ContentBaseFor（与门户/应用/管理台同一判据），
	// origin 走 siteurl.Base。这里曾硬拼 taskview.PathGateway，而协议面的产物
	// 路径已改成 /artifacts 且只认 API KEY —— 门户页上那个链接必然 404。
	d, err := logs.DetailByIDForUser(c.Request.Context(), id, userID,
		siteurl.Base(c), siteurl.Base(c)+taskview.ContentBaseFor(c.FullPath()))
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	if d == nil {
		// 不存在与"不是你的"统一 404：回 403 等于告诉对方这条 id 存在。
		response.FailDef(c, errs.NotFound)
		return
	}
	response.OK(c, logs.PortalDetail(d))
}

// modalityDist 按当前筛选条件在 usages 上实时聚合类型分布（与下方明细完全同源，
// 不做 usage_daily 二次口径，避免同一页面出现两套数字）。
func modalityDist(ctx context.Context, f usageFilter, userID uint64) ([]ModalityStat, error) {
	out := make([]ModalityStat, 0, 4)
	err := f.apply(db.DB.WithContext(ctx).Model(&model.Usage{}).Where("user_id = ?", userID)).
		Select("modality, COUNT(*) AS requests, COALESCE(SUM(charge), 0) AS charge").
		Group("modality").Order("charge DESC").Scan(&out).Error
	if err != nil {
		return nil, err
	}
	return out, nil
}

// portalUsageExportLimit 是门户导出单次最大行数（与后台日志导出一致量级）。
const portalUsageExportLimit = 10000

// PortalUsageExport 处理 GET /api/portal/usage/export：按当前筛选导出本人调用记录 CSV。
// 作用域恒为当前 JWT 用户，输出不含上游渠道等内部信息。
func PortalUsageExport(c *gin.Context) {
	userID := c.GetUint64("user_id")
	if userID == 0 {
		response.FailDef(c, errs.Unauthorized)
		return
	}
	f, ok := parseUsageFilter(c, true)
	if !ok {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	f = usageDefaultWindow(f)
	// 托管密钥只能看自己这把 KEY 产生的调用记录。
	// 覆盖而不是"默认值"：查询参数里的 key_id 是给门户筛选用途，对托管密钥无效。
	if s, _ := keyscope.Of(c); s.Hosted {
		f.KeyID = s.KeyID
	}
	var rows []model.Usage
	err := f.apply(db.DB.Model(&model.Usage{}).Where("user_id = ?", userID)).
		Order("id DESC").Limit(portalUsageExportLimit).Find(&rows).Error
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	body := usageCSV(rows, logs.KeyNames(c.Request.Context(), usageKeyIDs(rows)))
	name := "usage-" + time.Now().Format("20060102-150405") + ".csv"
	c.Header("Content-Disposition", "attachment; filename="+name)
	c.Data(http.StatusOK, "text/csv; charset=utf-8", append([]byte{0xEF, 0xBB, 0xBF}, []byte(body)...))
}

// usageKeyIDs 收集去重后的 KEY id（用于批量解析展示名）。
func usageKeyIDs(rows []model.Usage) []uint64 {
	seen := make(map[uint64]struct{}, len(rows))
	ids := make([]uint64, 0, len(rows))
	for _, r := range rows {
		if _, ok := seen[r.KeyID]; ok {
			continue
		}
		seen[r.KeyID] = struct{}{}
		ids = append(ids, r.KeyID)
	}
	return ids
}

// usageCSV 生成门户维度的调用记录 CSV。
//
// 金额列**逐行**带币种：同一份导出里可能同时含人民币与美元时期（切币种前后）的记录，
// 表头写死「美元」会把人民币金额标错 —— 用户拿 CSV 对账时会直接认为系统记错账。
func usageCSV(rows []model.Usage, keyNames map[uint64]string) string {
	var sb strings.Builder
	// 门户导出**不沿用管理台的 E5 表头**：E5 带「渠道」「成本USD」「IP」「上游请求ID」，
	// 那几列属于供应链与内部拓扑，不能给终端用户。这里给的是用户自查需要的全集：
	// 各 token 通道（解释金额）、三种耗时（自查"是我这边慢还是服务慢"）、请求号（报障用）。
	// 金额直接输出十进制（微 → 字符串在导出边界做，CSV 里不出现内部单位）。
	// 缓存写入两列必须单列出来：Anthropic 语义下 cache_write 的单价是普通输入的
	// 1.25–2 倍，合并进「缓存tok」会让用户拿 CSV 永远算不平金额。
	sb.WriteString("时间,模型,类型,密钥,状态,HTTP,输入tok,输出tok,缓存tok,缓存写5mtok,缓存写1htok," +
		"图片tok,音频入tok,音频出tok," +
		"金额,币种,总耗时ms,服务耗时ms,首字ms,请求ID,错误\n")
	for _, r := range rows {
		key := keyNames[r.KeyID]
		if key == "" {
			key = strconv.FormatUint(r.KeyID, 10)
		}
		// 币种回落规则与列表/详情一致：记录自带币种优先，否则取当前结算币种。
		cur := r.Currency
		if code, ok := conf.NormalizeCurrency(cur); ok {
			cur = code
		} else {
			cur = conf.SiteCurrency()
		}
		fmt.Fprintf(&sb, "%s,%s,%s,%s,%s,%d,%d,%d,%d,%d,%d,%d,%d,%d,%s,%s,%d,%d,%d,%s,%s\n",
			r.CreatedAt.Format("2006-01-02 15:04:05"), logs.Esc(r.Model), logs.Esc(r.Modality), logs.Esc(key),
			usageStatusText(r.Status, r.HTTPStatus), r.HTTPStatus,
			r.PromptTokens, r.CompletionTokens, r.CacheTokens,
			r.CacheWrite5mTokens, r.CacheWrite1hTokens,
			r.ImageTokens,
			r.AudioInputTokens, r.AudioOutputTokens,
			money.Format(r.Charge, 6), cur,
			r.LatencyMs, r.UpstreamMs, r.FirstTokenMs,
			logs.Esc(r.RequestID), logs.Esc(logs.PortalErrorSummary(r.Status, r.HTTPStatus, false)))
	}
	return sb.String()
}

// usageStatusText 把计费会话态转为门户状态文案（与 Usage 页状态标签一致）。
func usageStatusText(status string, httpStatus int) string {
	switch status {
	case "settled":
		return "已成功"
	case "pending":
		return "进行中"
	}
	if httpStatus > 0 {
		return fmt.Sprintf("失败(%d)", httpStatus)
	}
	return "失败"
}

// usageList 输出调用记录的统一分页响应（/v1/usage 兼容端点）。
func usageList(c *gin.Context, q *gorm.DB) {
	// 零值 Order = 不排序（`id DESC`）：/v1/usage 的契约在 internal/apidoc，
	// 没有 sort 参数，这里不能凭"函数顺手"多给一个能力。
	pg, perr := pagination.Parse(c)
	if perr != nil {
		response.FailDefMsg(c, errs.InvalidParam, perr.Error())
		return
	}
	items, total, page, size, err := usagePage(c, q, sorting.Order{}, pg)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, gin.H{"total": total, "items": items, "page": page, "page_size": size})
}

// Handle 处理 /v1 下**没有注册**的路径：404 `Invalid URL (METHOD /path)`。
//
// 为什么不是 501：兼容协议把它答成 404，而 501 会让 SDK 认定「服务端不支持这个
// 能力」并放弃重试/降级；真实原因通常只是路径拼错（把别家的端点贴过来了）。
// 「地址存在但能力没开」走 gateway.FailDef(errs.NotImplemented)（例如灰度关闭的 /v1/messages），
// 状态 501 而不是 404 —— 地址没错、只是能力没开。
//
// NoRoute 不在 /v1 路由组里，ingress 中间件不会跑，所以这里按**请求路径**自行判定线格式：
// 否则 Claude 客户端会收到 OpenAI 形状，只显示"未知错误"。
func Handle(c *gin.Context) {
	if c != nil && c.Request != nil {
		gateway.SetFormat(c, apidoc.ProtocolOfRequestPath(c.Request.URL.Path))
	}
	gateway.NotFoundRoute(c)
}

// —— 以下为 S7（异步任务）复用的导出包装，内部实现与同步转发保持同一真相源 ——

// BuildSnapshot 由定价目录项构造 pricing_snapshot（Open 时冻结）。
func BuildSnapshot(item *pricing.ModelItem, discount int64) billing.Snapshot {
	return buildSnapshot(item, discount)
}

// FindModel 在定价目录中定位模型（未命中返回 nil）。
func FindModel(cat *pricing.Snapshot, id string) *pricing.ModelItem { return findModel(cat, id) }

// GroupRatioOf 返回调用分组倍率（未配置分组回落 1.0）。
func GroupDiscountOf(cat *pricing.Snapshot, group string) int64 { return groupDiscount(cat, group) }

// EffectiveGroup 返回调用分组：KEY 分组优先，未设置时用用户分组。
func EffectiveGroup(k *model.APIKey, u *model.User) string { return effectiveGroup(k, u) }

// APIKeyOf 取 TokenAuth 注入的 API KEY 记录（未鉴权返回 nil；异步任务控制器复用）。
func APIKeyOf(c *gin.Context) *model.APIKey { return apiKeyOf(c) }

// UserOf 取 TokenAuth 注入的用户记录（未鉴权返回 nil；异步任务控制器复用）。
func UserOf(c *gin.Context) *model.User { return userOf(c) }
