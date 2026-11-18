package relay

// 调用报文采集（C1 · B2）。
//
// 设计要点：
//   - 采集器挂在 gin.Context 上而不是加参数：settleSession / releaseSession 的调用点
//     都在 dispatch 内部，但它们的签名已经被"失败原因/上游单号/来源/异常标记"占满，
//     再挂 5 个报文参数只会让人看不懂哪个才是真相源。
//   - 采集**只发生在内存**，落库在响应写完之后的 persist()：客户端不会因为落库变慢；
//     落库失败只 Warn，绝不影响计费与响应。
//   - 二进制（TTS 音频）**不留内容**：32 MiB 的音频进 text 列既没意义（看不了）
//     又会把表撑爆，只记 mime + 字节数。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/gateway"
	"lingdong-api/internal/middleware"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/netguard"
	"lingdong-api/internal/service/billing"
	"lingdong-api/internal/service/callio"
)

// ioRecKey 是采集器在 gin.Context 上的键。
const ioRecKey = "call_io_recorder"

// 调用分类（与 billing 包同一套枚举，这里做别名避免 relay 各处写全限定名）。
const (
	CallKindSync   = billing.CallKindSync
	CallKindAsync  = billing.CallKindAsync
	CallKindPlugin = billing.CallKindPlugin
)

// callMeta 汇总一次请求里"不属于计费公式、但必须落记录"的元信息。
//
// 为什么单独一个结构体：settleSession / releaseSession 的形参已经 9 个，
// 再加 5 个（尝试次数/首字延迟/上游单号/usage 原文/响应体）没人能看懂哪个是真相源。
type callMeta struct {
	callKind     string
	attempts     int
	firstTokenMs int
	upstreamMs   int
	upstreamID   string
	rawUsage     string
	respBody     []byte
	respCType    string
	respErr      []byte
	// errMsg 是**要写进 usages.error_msg 的脱敏文案**（与 respErr 分开）。
	//
	// respErr 存上游原文，直接落到 call_io 供排障；它可能含上游主机名与密钥片段，
	// 不能进 usages（门户可见 + CSV 导出）。两个用途不同，混用一个字段的结果是
	// 「要么泄漏、要么排障看不到原文」。
	//
	// 目前只有「流式已开始输出后上游中断」用它在成功结算里留错误说明：
	// 那条记录 http_status=200（客户端确实收到了 200 与部分内容），若不写 error_msg，
	// 账单上就是一次完美成功，与渠道健康度记录的失败互相矛盾。
	errMsg string
	// anomaly 是本次调用要写进 usages.anomaly 的标记（逗号分隔）。
	// 目前只有「提交歧义」用它：那是台账对账的唯一线索，不能只留在日志里。
	anomaly string
	// respSize 是二进制响应实际写出的字节数。
	// TTS 走流式时 respBody 为 nil，只有它知道音频到底多大。
	respSize int64
	// upstreamSpec 是上游**实际交付**的图像尺寸原文（拿不到留空）。
	// 只用于落 usages.upstream_spec 并与计费档位比对，不参与金额。
	upstreamSpec string
	// keyID 是本次实际出站使用的渠道密钥标识（HMAC 指纹）。
	//
	// 只落 usages.channel_key_id 供溯源（"这一单走了哪把 key"），**不参与金额**。
	// 取最后一次尝试的那把：成功时就是成功那把，全失败时是最后回话的那把。
	keyID string
}

// noteKey 记录本次实际出站的密钥指纹（归属取最后一次尝试的那把）。
//
// 空 ref 不覆盖：「本次没有可用密钥」不该把上一次的归属抹掉，
// 而 billing 侧对空值也是"不改"语义，两边口径一致。
func (m *callMeta) noteKey(ref KeyRef) {
	if m == nil || ref.ID == "" {
		return
	}
	m.keyID = ref.ID
}

// fillSuccess 记录非流式成功响应的原文与用量原文。
func (m *callMeta) fillSuccess(r *relayResult) {
	if m == nil || r == nil {
		return
	}
	m.respBody = r.body
	m.respCType = r.ctype
	m.rawUsage = r.usage.Raw
}

// setErr 是**唯一**设置上游错误文案的入口：同时写两个字段。
//
// respErr 保留上游原文（call_io 取证 + 排障），errMsg 必须脱敏
// （它会落 usages.error_msg 并可能直接返回客户端）。
// 以前有 4 处各自赋值，其中 2 处忘了脱敏 —— 只要新增赋值点绕开这里，
// 泄漏就会再发生一次，所以一律只允许走本方法。
//
// raw 与 msg 可以相同（大部分场景就是同一段文本），传两次是为了显式表达
// 「留痕用哪份、对外用哪份」；不需要脱敏的调用方仍应走这里，保证口径唯一。
func (m *callMeta) setErr(raw, msg string, ch *model.Channel, key string) {
	if m == nil {
		return
	}
	// respErr 是上游**原始报文**留痕，但同样要过密钥脱敏：
	// 上游 4xx 常把 Authorization 头原样回显，而 call_io 管理台可查全文。
	// 这里用的是全池脱敏（ch 非 nil 时抹掉池子里每一把），不是只抹当前这把。
	m.respErr = []byte(redactUpstream(raw, ch, key))
	m.errMsg = redactUpstream(msg, ch, key)
}

// ioRecorder 收集一次调用的原始报文。
type ioRecorder struct {
	usageID uint64
	userID  uint64
	// kind 决定响应体的媒体提取方式（图片端点才有 media，音频端点只留元数据）。
	kind endpointKind

	// reqRaw 是**原始请求体**（脱敏与截断在 callio.Store 里做，这里保持原始字节）。
	reqRaw []byte
	// reqNote 用于 multipart：原始体是二进制，不能整段存，改成文本字段摘要。
	reqNote []byte

	startAt time.Time
	stream  bool

	// 流式累计：拼接 delta.content 得到"最终文本"，不存逐帧。
	text     strings.Builder
	sseRaw   strings.Builder
	frames   int
	firstTok int

	// 非流式响应体（截断前，实际截断由 callio.Limits 决定）。
	respRaw   []byte
	respErr   []byte
	respBytes int64
	// deliveredRaw 是**真正写回客户端**的响应体（媒体直链改写 / 协议转码之后）。
	// 与 respRaw 相同则不落库 —— 门户按「空 = 同 respRaw」读取。
	deliveredRaw []byte
	// respSize 见 callMeta.respSize（二进制响应的真实字节数）。
	respSize int64

	media []model.MediaItem

	// upstreamRaw 是**实际发给上游**的报文（仅在平台改写了请求体时记录）。
	//
	// 典型场景：/v1/images/edits 的 multipart 文件被换成公网直链后，发送的是 JSON。
	// 不留这一份的话，「上游说参数不对」时详情页只显示原始 multipart 摘要，
	// 完全看不出我们到底发了什么（尤其是那条直链本身是否可访问）。
	upstreamRaw []byte
	// upstreamURL / upstreamStatus / upstreamAttempts 由传输层 tap 回填。
	// URL 是排障「是不是打错端点」的唯一凭据（path override / ?async=true 都在这里）。
	upstreamURL      string
	upstreamStatus   int
	upstreamAttempts int
	// upstreamTruncated 表示出站体超过 tap 上限，只留了前缀。
	upstreamTruncated bool
}

// newIORecorder 构造采集器；未开启报文留存时返回 nil（所有方法都对 nil 安全）。
func newIORecorder(kind endpointKind, userID uint64, raw []byte, stream bool, startAt time.Time) *ioRecorder {
	if !callio.Enabled() {
		return nil
	}
	return &ioRecorder{kind: kind, userID: userID, reqRaw: raw, stream: stream, startAt: startAt}
}

// NoteUpstreamRequest 实现 netguard.BodyTap：记录**真正出站**的报文。
//
// 覆盖写而不是只记第一份：排障关心的是「决定结果的那次请求」。
// 多次覆盖时 attempts 会累加，详情页能看出经历过 failover。
func (r *ioRecorder) NoteUpstreamRequest(_ context.Context, _ string, url, _ string, body []byte, truncated bool) {
	if r == nil {
		return
	}
	r.upstreamAttempts++
	r.upstreamURL = url
	r.upstreamStatus = 0
	r.upstreamRaw = body
	r.upstreamTruncated = truncated
}

// NoteUpstreamResponse 实现 netguard.BodyTap：记录上游状态码（连不上时为 0）。
func (r *ioRecorder) NoteUpstreamResponse(_ context.Context, status int) {
	if r == nil {
		return
	}
	r.upstreamStatus = status
}

// 编译期保证采集器仍是 tap 的实现（接口改名时能立刻发现）。
var _ netguard.BodyTap = (*ioRecorder)(nil)

// attach 把采集器放进上下文。
func attach(c *gin.Context, rec *ioRecorder) {
	if rec == nil {
		return
	}
	// multipart 请求体是二进制，不能整段留存：换成「文本字段 + 文件元信息」摘要。
	if strings.HasPrefix(strings.ToLower(c.GetHeader("Content-Type")), "multipart/form-data") {
		rec.reqNote = multipartPayload(c.GetHeader("Content-Type"), rec.reqRaw)
		rec.reqRaw = nil
	}
	c.Set(ioRecKey, rec)
}

// recorderOf 取上下文里的采集器（可能为 nil）。
func recorderOf(c *gin.Context) *ioRecorder {
	if c == nil {
		return nil
	}
	if v, ok := c.Get(ioRecKey); ok {
		if r, ok := v.(*ioRecorder); ok {
			return r
		}
	}
	return nil
}

// bindUsage 在 Freeze 成功后绑定 usage id（未绑定时 persist 直接跳过）。
func (r *ioRecorder) bindUsage(id uint64) {
	if r == nil {
		return
	}
	r.usageID = id
}

// onFrame 累计一帧流式数据（只记内容与帧数，不存原始 SSE）。
func (r *ioRecorder) onFrame(data []byte) {
	if r == nil || len(data) == 0 {
		return
	}
	r.frames++
	if r.firstTok == 0 && !r.startAt.IsZero() {
		r.firstTok = int(time.Since(r.startAt).Milliseconds())
	}
	if r.text.Len() < maxStreamTextBytes {
		r.text.WriteString(streamDeltaText(data))
	}
	if callio.SSESaveEnabled() && r.sseRaw.Len() < maxStreamTextBytes {
		r.sseRaw.WriteString("data: ")
		r.sseRaw.Write(data)
		r.sseRaw.WriteString("\n\n")
	}
}

// maxStreamTextBytes 是流式拼接文本的上限（最终落库还会按 callio 上限再截一次）。
// 先在这里封顶的原因：一场 200 万 token 的对话能把内存吃光，而客户端早就看完了。
const maxStreamTextBytes = 512 << 10

// noteStreamRaw 让「非文本流式」（图片帧）自己决定 resp_raw。
//
// 为什么必须显式设置，而不是复用 onFrame：
//   - onFrame 会按文本增量拼接（streamDeltaText），图片帧会被当成对话内容；
//   - success() 在 stream 分支**只读 sseRaw**，忽略传入的 body。也就是说
//     调用方把 completed 帧塞进 cm.respBody 也写不进去，最终 resp_raw 会退化成
//     一个空的对话聚合体（`{"choices":[{"message":{"content":""}}]}`）——
//     排查时看不到 usage.generated_images，等于丢了唯一的张数凭据。
//
// 只保留最后一帧（completed）：图片帧里的 b64 可达数 MB，全留会把 call_io 撑爆，
// 而排查只需要「上游自报生成了几张 + 用了多少 token」。
func (r *ioRecorder) noteStreamRaw(raw []byte) {
	if r == nil || len(raw) == 0 {
		return
	}
	r.sseRaw.Reset()
	r.sseRaw.WriteString("data: ")
	r.sseRaw.Write(raw)
	r.sseRaw.WriteString("\n\n")
}

// success 记录成功响应。
func (r *ioRecorder) success(body []byte, ctype string) {
	if r == nil {
		return
	}
	if specOf(r.kind).binary || strings.HasPrefix(ctype, "audio/") {
		// 二进制音频：只留元数据。
		size := int64(len(body))
		if size == 0 && r.respSize > 0 {
			// 流式：body 为 nil，用 writeResponse 实际写出的字节数。
			size = r.respSize
		}
		r.respBytes = size
		r.media = append(r.media, model.MediaItem{
			Kind: "audio", MIME: ctype, Bytes: size, ArtifactIndex: -1,
			Note: "二进制音频已完整发送，内容未留存（要回放需重新调用）",
		})
		return
	}
	if r.stream {
		if r.sseRaw.Len() > 0 {
			r.respRaw = []byte(r.sseRaw.String())
		} else {
			r.respRaw = []byte(streamAggregateJSON(r.text.String()))
		}
		return
	}
	if len(body) == 0 {
		// 上游 200 但 0 字节：必须与"未采集"区分开。
		// 前者是上游违约（值得排查），后者是我们没开留存（改配置即可）。
		r.respRaw = []byte(`{"_note":"上游返回空响应体（0 字节）"}`)
		return
	}
	r.respRaw = body
	r.respBytes = int64(len(body))
	r.media = append(r.media, mediaFromBody(r.kind, body)...)
}

// failure 记录上游错误原文（4xx/5xx）。
func (r *ioRecorder) failure(body []byte) {
	if r == nil || len(body) == 0 {
		return
	}
	r.respErr = body
}

// deliver 记录**真正发给客户端**的响应体（在 writeResponse 之前调用）。
//
// 为什么不能直接用 success() 的 body：那一份是上游原文，采集点在媒体直链改写
// 与协议转码之前。门户要看到自己收到的那一份，管理台要看到上游那一份 —— 两份都要。
func (r *ioRecorder) deliver(body []byte) {
	if r == nil || len(body) == 0 {
		return
	}
	r.deliveredRaw = body
}

// deliveredIfDiffers 只在两份正文不同时返回交付体：普通透传占绝大多数，
// 存两份等于把 call_io 翻倍，而「空 = 同 resp_raw」是读取侧唯一判据。
func (r *ioRecorder) deliveredIfDiffers() []byte {
	if r == nil || len(r.deliveredRaw) == 0 {
		return nil
	}
	if bytes.Equal(r.deliveredRaw, r.respRaw) {
		return nil
	}
	return r.deliveredRaw
}

// persistIO 把上下文里的采集器落库。
//
// 由主 handler 的 defer 调用（响应已写出），不再散落在各条返回路径上：
// 错误响应体由 response.serialize 在「返回前」写出，早落库的写法会让失败报文永远存不下来。
func persistIO(c *gin.Context) {
	r := recorderOf(c)
	if r == nil {
		return
	}
	// 失败调用：客户端收到的是平台错误体，序列化时挂在 context 上。
	if v, ok := c.Get(gateway.ClientErrorBodyKey); ok {
		if b, ok := v.([]byte); ok {
			r.deliver(b)
		}
	}
	r.persist()
}

// persist 落库（响应已写出之后调用；失败只 Warn）。
func (r *ioRecorder) persist() {
	if r == nil || r.usageID == 0 {
		return
	}
	req := r.reqRaw
	if len(req) == 0 {
		req = r.reqNote
	}
	// 响应来源：流式记录的是**拼接结果**，不能标成 raw（否则前端会以为是上游原文）。
	respSource := "none"
	switch {
	case r.stream && (r.text.Len() > 0 || len(r.respRaw) > 0):
		respSource = "aggregate"
	case len(r.respRaw) > 0:
		respSource = "raw"
	}
	callio.Store(contextWithoutCancel(), callio.Rec{
		UsageID:           r.usageID,
		UserID:            r.userID,
		ReqRaw:            req,
		RespRaw:           r.respRaw,
		DeliveredRaw:      r.deliveredIfDiffers(),
		RespError:         r.respErr,
		SSEFrames:         r.frames,
		Media:             r.media,
		RespBytesFor:      r.respBytes,
		RespSource:        respSource,
		UpstreamRaw:       r.upstreamRaw,
		UpstreamURL:       r.upstreamURL,
		UpstreamStatus:    r.upstreamStatus,
		UpstreamAttempts:  r.upstreamAttempts,
		UpstreamTruncated: r.upstreamTruncated,
	})
}

// streamAggregateJSON 把流式拼接文本包装成一个"可读的最终结果"。
//
// 为什么不是纯文本：前端统一按 JSON 渲染（格式化开关对纯文本是禁用状态），
// 包一层 object 让两种形态走同一条展示路径，也顺便标注"这是拼接结果、不是上游原始帧"。
func streamAggregateJSON(text string) string {
	raw, err := json.Marshal(map[string]any{
		"_note":    "流式响应：以下为拼接后的最终文本（原始 SSE 帧未留存）",
		"content":  text,
		"streamed": true,
	})
	if err != nil {
		return text
	}
	return string(raw)
}

// maxMediaItems 是单条记录最多保留的媒体项。
// 上限存在的理由：上游偶发返回几百张缩略图，media 是 jsonb 列，撑大后整行读都变慢。
const maxMediaItems = 20

// mediaPaths 是"结果数组"的候选路径，按常见度排序；命中且解析出项即返回。
//
// 为什么是一张表而不是写死一种形状：OpenAI 的 {"data":[{"url":...}]} 只是其中一种。
// 实际渠道里 {"output":["https://…"]}（多米/AutoDL）、{"images":[...]}、
// {"result":{"data":[...]}} 都很常见 —— 只认一种的表现是
// 「响应原文里明明有链接，但详情页没有任何结果可看」。
var mediaPaths = [][]string{
	{"data"},
	{"output"},
	{"images"},
	{"videos"},
	{"artifacts"},
	{"result", "data"},
	{"result", "images"},
	{"result"},
}

// mediaFromBody 从响应体提取媒体项（只留链接与元数据，内容不下载、不转存）。
//
// 三个形态都要支持：
//   - 元素是字符串          → 直接当 URL；
//   - 对象含 url/image_url/video_url/audio_url/file_url → 取第一个非空；
//   - 对象含 b64_json        → 内联数据，只记解码后大小（内容不留存）。
//
// 不做递归扫描兜底：{"id":123,"url":"docs"} 这类结构会被误判成媒体，
// 一条假结果比没有结果更误导人。
func mediaFromBody(kind endpointKind, body []byte) []model.MediaItem {
	if len(body) == 0 {
		return nil
	}
	mod := specOf(kind).modality
	if mod != "image" && mod != "video" && mod != "audio" {
		return nil
	}
	var root any
	if json.Unmarshal(body, &root) != nil {
		return nil
	}
	for _, p := range mediaPaths {
		arr, ok := digArray(root, p)
		if !ok {
			continue
		}
		if items := mediaFromArray(mod, arr); len(items) > 0 {
			return items
		}
	}
	// 顶层单链接形态：{"url":"https://…"} / {"video_url":"…"}
	return mediaFromArray(mod, []any{root})
}

// digArray 按路径取数组值；末层是单对象时包成一元素数组（部分渠道只返回单个产物）。
func digArray(root any, path []string) ([]any, bool) {
	cur := root
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[k]
		if !ok {
			return nil, false
		}
	}
	switch v := cur.(type) {
	case []any:
		return v, true
	case map[string]any:
		return []any{v}, true
	}
	return nil, false
}

// mediaFromArray 把数组元素逐个映射成媒体项。
//
// ArtifactIndex 恒 -1：这里的媒体来自**同步响应**，不是异步任务的产物。
// 若照原样写数组下标（0/1/2），前端会用 (task_id, 0) 去请求产物端点，
// 对同步记录而言 task_id 为空，逻辑上不成立 —— 这是个语义坑，不是风格问题。
func mediaFromArray(mod string, arr []any) []model.MediaItem {
	out := make([]model.MediaItem, 0, len(arr))
	for _, el := range arr {
		if len(out) >= maxMediaItems {
			break
		}
		item, ok := mediaItemOf(mod, el)
		if !ok {
			continue
		}
		out = append(out, item)
	}
	return out
}

// mediaItemOf 解析单个元素。
func mediaItemOf(mod string, el any) (model.MediaItem, bool) {
	switch v := el.(type) {
	case string:
		if v == "" {
			return model.MediaItem{}, false
		}
		return newMediaItem(mod, v, ""), true
	case map[string]any:
		for _, k := range []string{"url", "image_url", "video_url", "audio_url", "file_url"} {
			if s, ok := v[k].(string); ok && s != "" {
				return newMediaItem(mod, s, mapStr(v, "mime_type")), true
			}
		}
		if b64, ok := v["b64_json"].(string); ok && b64 != "" {
			mime := mapStr(v, "mime_type")
			if mime == "" {
				mime = defaultMIME(mod)
			}
			// 内联 base64：**不存内容**，只记大小。
			// 把 base64 塞进 text 列能让单条记录膨胀到几 MB，而它本来就能从响应原文之外
			// 重新调用获得 —— 存下来只有坏处。
			return model.MediaItem{
				Kind: model.MediaKind(mime, "", mod), MIME: mime,
				Bytes: int64(len(b64) * 3 / 4), ArtifactIndex: -1,
				Note: "上游返回内联数据，内容未留存",
			}, true
		}
	}
	return model.MediaItem{}, false
}

// newMediaItem 按 URL 形态构造媒体项。
//
// Kind 由 **MIME / 扩展名** 判定，端点模态只作兜底：视频端点返回的封面常常是 jpg，
// 按模态写成 video 会让前端用 <video> 渲染一张图（黑屏 + 控制条），
// 用户看到的就是「预览坏了」。
func newMediaItem(mod, rawURL, mime string) model.MediaItem {
	if mime == "" {
		mime = model.MIMETypeOfURL(rawURL)
	}
	it := model.MediaItem{Kind: model.MediaKind(mime, rawURL, mod), MIME: mime, ArtifactIndex: -1}
	if strings.HasPrefix(rawURL, "data:") {
		it.Note = "上游返回内联数据，内容未留存"
		return it
	}
	it.URL = rawURL
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		// 相对路径不拼 base_url：渠道 base 可能是反代，拼出来大概率指向错误主机。
		// 原样留着 + 说明，让排障的人自己判断。
		it.Note = "上游返回相对路径，需以渠道域名补全"
	}
	return it
}

// mapStr 取 map 里的字符串值（缺失或类型不符返回空串）。
func mapStr(m map[string]any, k string) string {
	if s, ok := m[k].(string); ok {
		return s
	}
	return ""
}

// defaultMIME 按模态给一个保守的默认 MIME（上游常不给 mime_type）。
func defaultMIME(mod string) string {
	switch mod {
	case "image":
		return "image/png"
	case "video":
		return "video/mp4"
	case "audio":
		return "audio/mpeg"
	}
	return "application/octet-stream"
}

// streamDeltaText 从一帧 SSE 数据里取出增量文本（delta.content / reasoning_content）。
//
// 复用 anyText 的两种形态处理（string 与 [{text}]）：不同厂商的 content 形状不一样，
// 只认 string 会让 Anthropic/Gemini 的流式记录变成一片空白。
func streamDeltaText(payload []byte) string {
	var p struct {
		Choices []struct {
			Delta struct {
				Content          any `json:"content"`
				ReasoningContent any `json:"reasoning_content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return ""
	}
	var sb strings.Builder
	for _, c := range p.Choices {
		sb.WriteString(anyText(c.Delta.Content))
		sb.WriteString(anyText(c.Delta.ReasoningContent))
	}
	return sb.String()
}

// contextWithoutCancel 返回一个"客户端断开也不取消"的上下文。
//
// 报文落库发生在响应写出之后：客户端拿到结果就关连接是常态（尤其 curl 之后立刻退出），
// 若沿用请求上下文，最好的情况是慢几毫秒，最坏的情况是这条记录根本没写进去。
func contextWithoutCancel() context.Context {
	return context.WithoutCancel(context.Background())
}

// multipartPayload 把 multipart 请求体转为可读摘要（不存文件内容）。
func multipartPayload(contentType string, body []byte) []byte {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" {
		return []byte(`{"_note":"multipart 请求体，无法解析 boundary，内容未留存"}`)
	}
	fields := map[string]string{}
	var files []map[string]any
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		name := p.FormName()
		if name == "" {
			continue
		}
		if fn := p.FileName(); fn != "" {
			// 文件内容绝不读：只跳过并记录大小（Part 读完才知道大小，所以按 LimitReader 计数）。
			n, _ := io.Copy(io.Discard, io.LimitReader(p, middleware.UploadBodyCap()))
			files = append(files, map[string]any{"field": name, "filename": fn, "bytes": n})
			continue
		}
		val, _ := io.ReadAll(io.LimitReader(p, 64<<10))
		fields[name] = strings.TrimSpace(string(val))
	}
	out, err := json.Marshal(map[string]any{
		"_note":        "multipart 请求：仅记录文本字段与文件元信息，文件内容未留存",
		"content_type": contentType,
		"fields":       fields,
		"files":        files,
	})
	if err != nil {
		return []byte(`{"_note":"multipart 请求体，摘要生成失败"}`)
	}
	return out
}
