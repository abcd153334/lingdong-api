package controller

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/gateway"
	"lingdong-api/internal/middleware"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/relay"
	"lingdong-api/internal/service/task"
	"lingdong-api/internal/service/taskshell"
)

// 本文件是三个新模态提交端点：
//
//	POST /v1/images/generations        图像：默认同步出图，带 async:true 时转任务
//	POST /v1/audio/music/generations   音乐（SUNO）异步任务
//	POST /v1/audio/lyrics              歌词异步任务
//
// 图像为什么和另外两个不同形态：它的上游流程本身就是「提交任务 + 轮询」，同步只是
// 「在请求内等到终态」（多米的同步出图就是 syncwait 等到出图）。把两者拆成两条路径
// 的代价是两篇同字段文档、两个预设 TAB、侧栏两行，以及「我该用哪个地址」这个必然
// 会被问的问题。视频/音乐天生只有任务形态、没有同步面，所以它们仍是独立提交端点。
//
// 统一约束：KEY 鉴权、JSON 体、受理即 202 返回任务对象。

// maxTaskBodyBytes 是任务型提交的 JSON 体上限（1 MiB）。
//
// 与同步端点的 16 MiB 不同是刻意的：任务报文会随任务行落库（tasks.submit_body），
// 重试时原样重发 —— 放开到 16 MiB 等于给每条任务写一行 16 MiB 的正文。
// 三个任务端点都不收文件、不收 base64（内联体会静默丢失），1 MiB 足够承载
// 几百行歌词与全部结构化参数。
const maxTaskBodyBytes = 1 << 20

// maxLyricsPromptRunes 是歌词 prompt 的字符上限（与适配器侧同口径）。
const maxLyricsPromptRunes = 2000

// readJSONOnlyBody 读取 JSON 请求体（拒绝 multipart）。
//
// 为什么在这里拒绝 multipart 而不是交给解析失败：任务端点返回的是任务号，
// 若客户端按同步 /v1/images/edits 的习惯发 multipart，解析会把文件 part 静默丢掉，
// 最后变成「任务成功但 prompt 为空」——报错必须指向真实原因。
func readJSONOnlyBody(c *gin.Context, errCode, errMsg string) ([]byte, bool) {
	body, def := readJSONBodyDef(c, errs.Def{Code: errCode, HTTP: http.StatusBadRequest, Message: errMsg})
	if def.Code != "" {
		gateway.FailDef(c, def)
		return nil, false
	}
	return body, true
}

// readJSONBodyDef 是同一件事，但把失败作为 Def 返回、不写响应。
//
// 存在的理由：不同协议面的**业务码词表不同**。/suno 只认七码，直接把
// `invalid_param` 发给它，客户端会落进 default 分支当成"未知错误"。
// 所以那边要拿到 Def 再过一道自己的投影，而不是让这里直接写响应。
func readJSONBodyDef(c *gin.Context, ctypeDef errs.Def) ([]byte, errs.Def) {
	ctype := strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Type")))
	if strings.HasPrefix(ctype, "multipart/form-data") {
		return nil, ctypeDef
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxTaskBodyBytes+1))
	if err != nil {
		return nil, errs.InvalidParam
	}
	if len(body) > maxTaskBodyBytes {
		return nil, errs.RequestTooLarge
	}
	if len(body) == 0 {
		return nil, errs.InvalidParam
	}
	return body, errs.Def{}
}

// peekBody 预读请求体并把请求流**完整复原**，供同一路由的第二个分支继续读。
//
// 复原必须是 `已读字节 + 原 Reader` 的拼接，不能只把已读部分塞回去：
//   - 少接回后半段，合法的 10 MB 请求会在同步分支被静默截断（能成功的请求变成 400）；
//   - 超限时 relay 的 readRequest 读不满 limit+1，tooLarge 判不出来，
//     413 会退化成一个被截断的 400，排障时先怀疑的是 JSON 结构。
//
// 读失败返回 ok=false：调用方直接交给 relay，让错误文案只有一份。
func peekBody(c *gin.Context, limit int64) ([]byte, bool) {
	src := c.Request.Body
	if src == nil {
		return nil, true
	}
	body, err := io.ReadAll(io.LimitReader(src, limit+1))
	c.Request.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(body), src), src}
	if err != nil {
		return nil, false
	}
	return body, true
}

// imageMode 是图像生成请求里与「同步还是任务」相关的三个判定。
type imageMode struct {
	// async 是 async==true。
	async bool
	// present 表示报文里出现了 async 键（含 false / null）—— 同步分支据此决定是否重写报文。
	present bool
	// stream 是 stream==true（与 async 互斥）。
	stream bool
	// valid=false 表示 async 不是布尔值。
	valid bool
}

// parseImageMode 解析 async / stream 两个顶层字段。
//
// 只认 JSON 布尔：`"true"` 字符串必须 400。宽松转换（把非空字符串当真）会让
// `"false"` 也被判成 true，客户端写错一个引号就静默拿到任务号而不是图片。
//
// 报文本身不是 JSON 对象时返回「全 false + valid=true」：结构错误交给 relay 统一报
// invalid_param，这里再写一套就等于同一件事两种文案。
func parseImageMode(body []byte) imageMode {
	m := imageMode{valid: true}
	var obj map[string]json.RawMessage
	if len(body) == 0 || json.Unmarshal(body, &obj) != nil {
		return m
	}
	if raw, ok := obj["async"]; ok {
		m.present = true
		switch strings.TrimSpace(string(raw)) {
		case "true":
			m.async = true
		case "false", "null":
			// 显式同步：剥离该键后按同步处理
		default:
			m.valid = false
			return m
		}
	}
	if raw, ok := obj["stream"]; ok && strings.TrimSpace(string(raw)) == "true" {
		m.stream = true
	}
	return m
}

// stripAsyncKey 删除顶层的 async 键（其余字段原样保留）。
//
// 为什么必须删：出站走 ForwardUnmapped，未映射的键会**原样发给上游**，
// 而厂商对未知字段普遍直接 400。只在报文真的带了这个键时调用。
func stripAsyncKey(body []byte) []byte {
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	delete(obj, "async")
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// 不转义 < > &：prompt 带尖括号时转义后语义相同，但落库的报文会与用户
	// 发来的不一致，排障时容易被误判成「平台改写了我的请求」。
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obj); err != nil {
		return body
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// ImagesCreate 处理 POST /v1/images/generations。
//
// 默认同步：在本次请求内等到出图，返回 OpenAI 图像响应。
// `async: true`：立即 202 返回任务对象，之后用 /v1/tasks/* 查状态与取图。
func ImagesCreate(c *gin.Context) {
	body, ok := peekBody(c, middleware.MaxRelayBodyBytes)
	if !ok {
		// 读失败：交给 relay 报 413/400，错误文案只有一份。
		relay.ImagesGenerations(c)
		return
	}
	m := parseImageMode(body)
	if !m.valid {
		gateway.FailDefMsg(c, errs.InvalidParam, "async 只接受布尔值 true / false")
		return
	}
	if m.async && m.stream {
		gateway.FailDef(c, errs.AsyncStreamConflict)
		return
	}
	if m.async {
		imageSubmitTask(c, body)
		return
	}
	// 同步：只有真的带了 async 键才重写报文（未使用该字段的流量一个字节都不动）。
	if m.present {
		c.Request.Body = io.NopCloser(bytes.NewReader(stripAsyncKey(body)))
	}
	relay.ImagesGenerations(c)
}

// imageSubmitTask 把一次图像生成请求落成任务（原 ImageAsyncCreate 的主体）。
//
// raw 是客户端原文（含 async），用于调用记录与幂等指纹；发上游与落 submit_body 的
// 是剥掉 async 之后的报文。
func imageSubmitTask(c *gin.Context, raw []byte) {
	k := relay.APIKeyOf(c)
	u := relay.UserOf(c)
	if k == nil || u == nil {
		gateway.FailDef(c, errs.InvalidAPIKey)
		return
	}
	// 能走到这里的报文一定是 JSON 对象（parseImageMode 已解析成功），
	// 因此不再单独判 Content-Type：multipart 请求根本带不出 async 键。
	if len(raw) > maxTaskBodyBytes {
		gateway.FailDef(c, errs.RequestTooLarge)
		return
	}
	body := stripAsyncKey(raw)

	var probe struct {
		ResponseFormat string          `json:"response_format"`
		Image          json.RawMessage `json:"image"`
		MaskURL        string          `json:"mask_url"`
		N              json.RawMessage `json:"n"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		gateway.FailDef(c, errs.InvalidParam)
		return
	}
	// b64_json：任务结果必须落成可下载的产物，base64 既不落库也不下发（会随任务行无限膨胀）。
	if strings.EqualFold(strings.TrimSpace(probe.ResponseFormat), "b64_json") {
		gateway.FailDef(c, errs.B64NotSupportedAsync)
		return
	}
	// data: 内联：与视频链路同口径（内联体不落库，重试时素材静默丢失）。
	if duomiHasInlineImage(probe.Image) || strings.HasPrefix(strings.ToLower(strings.TrimSpace(probe.MaskURL)), "data:") {
		gateway.FailDef(c, errs.InlineImageNotSupported)
		return
	}
	// n 不再本地判定上限：支持几张由上游决定，平台按上游实际交付的张数结算
	// （image_n_over_delivered / image_n_mismatch 会把差异记进 anomaly）。
	// 只校验它是个整数 —— 写成 "two" 这种连上游都读不懂的值没有转发意义。
	if _, ok := looseInt(probe.N); !ok {
		gateway.FailDef(c, errs.InvalidParam)
		return
	}

	req, err := task.ParseSubmit(body)
	if err != nil {
		gateway.FailDef(c, failDefOf(err))
		return
	}
	row, err := task.Submit(c.Request.Context(), task.SubmitInput{
		Key: k, Group: relay.EffectiveGroup(k, u), IP: c.ClientIP(),
		RequestID: c.GetHeader("X-Request-Id"), Req: req, ExpectModality: "image",
		// 与同步分支同一个端点键：能力声明、文档、价目都只有一份。
		EndpointKey:    "images/generations",
		IdempotencyKey: submitIdempotencyKey(c),
		// 调用记录：原始请求体 + 可信度来源（rebuilt = multipart 被平台重构过）。
		RawBody: raw, ReqSource: submitReqSource(c),
	})
	if err != nil {
		gateway.FailDef(c, failDefOf(err))
		return
	}
	gateway.OK(c, taskshell.FlatOf(row))
}

// MusicCreate 处理 POST /v1/audio/music/generations：音乐异步任务。
//
// SUNO 一次出 2 首，产物按实际成功条数计；计费按 per_task（一次任务）。
func MusicCreate(c *gin.Context) {
	k := relay.APIKeyOf(c)
	u := relay.UserOf(c)
	if k == nil || u == nil {
		gateway.FailDef(c, errs.InvalidAPIKey)
		return
	}
	body, ok := readJSONOnlyBody(c, "music_requires_json",
		"音乐端点只接受 application/json（multipart 请改用字段 lyrics 传歌词）")
	if !ok {
		return
	}

	var probe struct {
		Prompt       string `json:"prompt"`
		Lyrics       string `json:"lyrics"`
		Instrumental *bool  `json:"instrumental"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		gateway.FailDef(c, errs.InvalidParam)
		return
	}
	lyrics := strings.TrimSpace(probe.Lyrics)
	// 纯音乐与歌词互斥：同时给等于「歌词永远不会被唱」，必须显式 400 而不是静默丢弃。
	if probe.Instrumental != nil && *probe.Instrumental && lyrics != "" {
		gateway.FailDef(c, errs.InstrumentalLyricsConflict)
		return
	}
	if strings.TrimSpace(probe.Prompt) == "" && lyrics == "" {
		gateway.FailDef(c, errs.InvalidParam)
		return
	}

	req, err := task.ParseSubmit(body)
	if err != nil {
		gateway.FailDef(c, failDefOf(err))
		return
	}
	// 动作落库：/suno/fetch 要按动作决定内层 data 是 clip 数组还是歌词对象，
	// 而两条原生入口的模态都是 music —— 不落这一条就只能靠产物类型反推。
	req.Params.MusicAction = "music"
	row, err := task.Submit(c.Request.Context(), task.SubmitInput{
		Key: k, Group: relay.EffectiveGroup(k, u), IP: c.ClientIP(),
		RequestID: c.GetHeader("X-Request-Id"), Req: req, ExpectModality: "music",
		EndpointKey:    "audio/music/generations",
		IdempotencyKey: submitIdempotencyKey(c),
		// 调用记录：原始请求体 + 可信度来源（rebuilt = multipart 被平台重构过）。
		RawBody: body, ReqSource: submitReqSource(c),
	})
	if err != nil {
		gateway.FailDef(c, failDefOf(err))
		return
	}
	gateway.OK(c, taskshell.FlatOf(row))
}

// LyricsCreate 处理 POST /v1/audio/lyrics：歌词异步任务（产物是纯文本）。
func LyricsCreate(c *gin.Context) {
	k := relay.APIKeyOf(c)
	u := relay.UserOf(c)
	if k == nil || u == nil {
		gateway.FailDef(c, errs.InvalidAPIKey)
		return
	}
	body, ok := readJSONOnlyBody(c, "lyrics_requires_json",
		"歌词端点只接受 application/json")
	if !ok {
		return
	}

	var probe struct {
		Prompt string `json:"prompt"`
		Lyrics string `json:"lyrics"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		gateway.FailDef(c, errs.InvalidParam)
		return
	}
	prompt := strings.TrimSpace(probe.Prompt)
	if prompt == "" {
		prompt = strings.TrimSpace(probe.Lyrics)
	}
	if prompt == "" {
		gateway.FailDef(c, errs.InvalidParam)
		return
	}
	// 字符数（不是字节数）校验：中文歌词 2000 字 ≈ 6KB，按字节判会误拒。
	if n := utf8.RuneCountInString(prompt); n > maxLyricsPromptRunes {
		gateway.FailDef(c, errs.PromptTooLong)
		return
	}

	req, err := task.ParseSubmit(body)
	if err != nil {
		gateway.FailDef(c, failDefOf(err))
		return
	}
	req.Params.MusicAction = "lyrics"
	row, err := task.Submit(c.Request.Context(), task.SubmitInput{
		Key: k, Group: relay.EffectiveGroup(k, u), IP: c.ClientIP(),
		RequestID: c.GetHeader("X-Request-Id"), Req: req, ExpectModality: "music",
		EndpointKey:    "audio/lyrics",
		IdempotencyKey: submitIdempotencyKey(c),
		// 调用记录：原始请求体 + 可信度来源（rebuilt = multipart 被平台重构过）。
		RawBody: body, ReqSource: submitReqSource(c),
	})
	if err != nil {
		gateway.FailDef(c, failDefOf(err))
		return
	}
	gateway.OK(c, taskshell.FlatOf(row))
}

// looseInt 解析可空整数（缺失/null 返回 0；类型非法返回 ok=false）。
func looseInt(raw json.RawMessage) (int, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, true
	}
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, true
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		if v, aerr := strconv.Atoi(strings.TrimSpace(str)); aerr == nil {
			return v, true
		}
	}
	return 0, false
}

// duomiHasInlineImage 判断 image 字段（字符串或数组）是否含 data: 内联。
func duomiHasInlineImage(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return false
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return strings.HasPrefix(strings.ToLower(strings.TrimSpace(one)), "data:")
	}
	var arr []string
	if json.Unmarshal(raw, &arr) == nil {
		for _, it := range arr {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(it)), "data:") {
				return true
			}
		}
	}
	return false
}
