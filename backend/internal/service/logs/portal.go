package logs

import (
	"encoding/json"
	"strings"
	"time"

	"lingdong-api/internal/service/taskview"
)

// 本文件是**用户端**调用记录的投影（fail-closed 白名单）。
//
// 为什么不用「复制 Detail 再清零字段」：那是 fail-open 设计 —— Detail 每加一个字段，
// 门户就自动多泄漏一个（旧实现正是这样把 call_io 原文、cap_reason、usage_source
// 一起漏给了用户）。这里改成显式白名单：内部新增字段，白名单不加 = 用户看不到。

// PortalItem 是门户调用记录列表/详情共用的字段集（/api/portal/usage、/v1/usage 共用）。
//
// 相对内部 Item 的差异（每一项都对应一个泄漏面）：
//   - 无 error_msg：上游原文含域名与上游真实模型名 → 换 error_summary（分类文案）；
//   - 无 upstream_request_id：上游开单号是内部对账标识；
//   - 无 cap_reason：值域 global/frozen3x/balance 是风控策略代号；
//   - 无 usage_source：值 upstream 直接说明「这条走的是上游实测」；
//   - 无 channel_* / adapter_id / ip / group / username / cost / pricing_snapshot；
//   - upstream_ms 改名 service_ms：键名本身带 upstream 也是泄漏；
//   - anomaly 只保留 reclaimed（前端「回收补账」角标用），其余是风控内部状态。
type PortalItem struct {
	ID    uint64 `json:"id"`
	KeyID uint64 `json:"key_id"`
	// KeyName 是用户自己那把 KEY 的名字（不是密钥内容）。
	// 用户有多把 KEY 时，第一眼要能确认「这笔是哪把花的」，所以它属于用户端信息。
	KeyName   string    `json:"key_name,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Model     string    `json:"model"`
	// ModelIconURL 是模型图标的可渲染地址（可能为空 → 前端首字占位）。
	// 它只是"长什么样"，不携带渠道/上游信息，所以可以下发给用户。
	ModelIconURL string `json:"model_icon_url"`
	Modality     string `json:"modality"`
	Status       string `json:"status"`
	// HTTPStatus 是本次调用的状态码（用户排查自己的请求时会看）。
	HTTPStatus        int   `json:"http_status"`
	PromptTokens      int64 `json:"prompt_tokens"`
	CompletionTokens  int64 `json:"completion_tokens"`
	CacheTokens       int64 `json:"cache_tokens"`
	ImageTokens       int64 `json:"image_tokens"`
	AudioInputTokens  int64 `json:"audio_input_tokens"`
	AudioOutputTokens int64 `json:"audio_output_tokens"`
	Charge            int64 `json:"charge"`
	// LatencyMs 是**总耗时**（含重试与排队）；ElapsedMs 是进行中的实时值。
	LatencyMs int   `json:"latency_ms"`
	ElapsedMs int64 `json:"elapsed_ms"`
	// ServiceMs 是最后一次服务调用的耗时（与总耗时一起看才能判断慢在哪一段）。
	ServiceMs    int `json:"service_ms"`
	FirstTokenMs int `json:"first_token_ms"`
	Attempts     int `json:"attempts"`

	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	IsStream  bool   `json:"is_stream"`
	RequestID string `json:"request_id,omitempty"`
	CallKind  string `json:"call_kind"`
	// TaskID 是对外任务号（`task_` 前缀，空 = 同步调用）：
	// 控制面任务端点只认公开号，下发内部自增 id 等于给一个"填进去必然 400"的值。
	TaskID string `json:"task_id,omitempty"`
	// IsRejected 表示请求在预检阶段被拒（未消耗额度）。
	IsRejected bool `json:"is_rejected,omitempty"`
	// ErrorSummary 是分类文案（PortalErrorSummary），空串表示成功。
	ErrorSummary string `json:"error_summary,omitempty"`
	// Anomaly 只含 reclaimed（其余风控标记不下发）。
	Anomaly string `json:"anomaly,omitempty"`
	// 视频/档位维度：门户要用它解释「为什么按 N 秒收」。
	VideoResolution string `json:"video_resolution,omitempty"`
	VideoSeconds    int64  `json:"video_seconds,omitempty"`
	TierPrice       int64  `json:"tier_price,omitempty"`
	// ChargeBeforeCap 供前端展示「已按单次上限截顶：应收 X → 实收 Y」。
	ChargeBeforeCap int64 `json:"charge_before_cap,omitempty"`
	// Currency 是**行内快照**的结算币种：金额列必须按它展示（老行取当前币种）。
	Currency string `json:"currency,omitempty"`
}

// portalAnomalyKeep 是允许下发给用户的异常标记。
//
// 只留 reclaimed：前端 Elapsed 组件的「回收补账」角标依赖它。
// charge_capped / negative_balance_blocked 等属于平台风控状态，不下发。
const portalAnomalyKeep = "reclaimed"

// PortalItemOf 把内部 Item 投影成门户视图（唯一转换点）。
//
// 视频/档位维度的四个值由调用方传入：它们在 Detail 上而不在 Item 上
// （列表与详情都要展示，但来源结构不同）。多一个参数胜过各自裁剪一遍。
func PortalItemOf(it Item, videoResolution string, videoSeconds, tierUnitPriceQuota, chargeBeforeCap int64) PortalItem {
	out := PortalItem{
		ID: it.ID, KeyID: it.KeyID, KeyName: it.KeyName, CreatedAt: it.CreatedAt,
		Model: it.Model, ModelIconURL: it.ModelIconURL,
		Modality: it.Modality, Status: it.Status, HTTPStatus: it.HTTPStatus,
		PromptTokens: it.PromptTokens, CompletionTokens: it.CompletionTokens,
		CacheTokens: it.CacheTokens, ImageTokens: it.ImageTokens,
		AudioInputTokens: it.AudioInputTokens, AudioOutputTokens: it.AudioOutputTokens,
		Charge: it.Charge, LatencyMs: it.LatencyMs, ElapsedMs: it.ElapsedMs,
		ServiceMs: it.UpstreamMs, FirstTokenMs: it.FirstTokenMs, Attempts: it.Attempts,
		StartedAt: it.StartedAt, FinishedAt: it.FinishedAt,
		IsStream: it.IsStream, RequestID: it.RequestID, CallKind: it.CallKind, TaskID: it.TaskPublicID,
		IsRejected:      it.Status == "rejected",
		ErrorSummary:    PortalErrorSummary(it.Status, it.HTTPStatus, it.Status == "rejected"),
		Anomaly:         filterPortalAnomaly(it.Anomaly),
		VideoResolution: videoResolution,
		VideoSeconds:    videoSeconds,
		TierPrice:       tierUnitPriceQuota,
		ChargeBeforeCap: chargeBeforeCap,
		Currency:        it.Currency,
	}
	return out
}

// filterPortalAnomaly 只保留 reclaimed。
func filterPortalAnomaly(raw string) string {
	if raw == "" {
		return ""
	}
	for _, part := range strings.Split(raw, ",") {
		if strings.TrimSpace(part) == portalAnomalyKeep {
			return portalAnomalyKeep
		}
	}
	return ""
}

// PortalMedia 是产物的**元数据**（不含 URL）。
//
// 上游直链（model.MediaItem.URL）是上游 CDN 域名，必须由平台代理端点
// （GET /api/portal/usage/:id/media/:index）转出，不能直接下发。
type PortalMedia struct {
	Kind string `json:"kind"`
	MIME string `json:"mime,omitempty"`
	// Bytes 是内联数据的字节数（0 = 上游只给了链接，大小未知）。
	Bytes int64 `json:"bytes,omitempty"`
	// ArtifactIndex 是异步任务的产物序号（-1 = 非任务产物 → 走 media 代理）。
	ArtifactIndex int    `json:"artifact_index"`
	Note          string `json:"note,omitempty"`
}

// PortalIO 是一次调用的报文：**用户自己发的**与**平台交给用户的**。
//
// 两类内容各归其位，混用会直接泄漏供应链：
//   - ReqRaw 是客户端原始请求体（平台只脱敏、不重排）；用户本来就知道自己发了什么。
//   - RespBody 是平台**实际返回给客户端**的响应体（媒体直链换本站地址、协议转码之后）。
//     它不是上游原文：上游原文里是服务商 CDN 域名与上游协议形状，永不下发。
//
// 失败调用没有成功响应体，此时 RespBody 是平台写出的错误 JSON ——
// 那正是用户当时收到的那一段。
type PortalIO struct {
	// ---- 请求（我发的）----
	ReqRaw string `json:"req_raw,omitempty"`
	// ReqBytes 是**截断前**的原始字节数（用户据此判断"我是不是发了张 5MB 的图"）。
	ReqBytes     int64  `json:"req_bytes"`
	ReqTruncated bool   `json:"req_truncated"`
	ReqSource    string `json:"req_source,omitempty"`
	BodyMode     string `json:"body_mode,omitempty"`

	// ---- 响应（我收到的）----
	RespBody  string `json:"resp_body,omitempty"`
	RespBytes int64  `json:"resp_bytes"`
	SSEFrames int    `json:"sse_frames"`
	// OutputText 是 RespBody 的**文本视图**（≤8 KiB）：对话类记录直接给出答案，
	// 免去用户在 JSON 里找 choices[0].message.content。
	OutputText string        `json:"output_text,omitempty"`
	Media      []PortalMedia `json:"media,omitempty"`
}

// PortalDetailView 是门户详情的响应体：列表投影 + 计费解释 + 报文摘要。
type PortalDetailView struct {
	PortalItem
	BillingMode       string    `json:"billing_mode,omitempty"`
	ImageSpec         string    `json:"image_spec,omitempty"`
	ImageRatio        string    `json:"image_ratio,omitempty"`
	ImageSpecSource   string    `json:"image_spec_source,omitempty"`
	ImageQuality      string    `json:"image_quality,omitempty"`
	ImageN            int64     `json:"image_n"`
	AudioDurationSecs int64     `json:"audio_duration_secs"`
	AudioChars        int64     `json:"audio_chars"`
	RerankDocs        int64     `json:"rerank_docs"`
	VideoResolution   string    `json:"video_resolution,omitempty"`
	VideoSeconds      int64     `json:"video_seconds"`
	RefImages         int64     `json:"ref_images"`
	RefVideos         int64     `json:"ref_videos"`
	RefAudios         int64     `json:"ref_audios"`
	RefVideoSeconds   int64     `json:"ref_video_seconds"`
	Frozen            int64     `json:"frozen"`
	IO                *PortalIO `json:"call_io,omitempty"`
	// Task 与内部详情**同一个对象**：taskview.View 里没有任何内部字段
	// （error 已经是分类文案），门户不需要再裁剪一遍 —— 以前那个
	// portalTaskRef 只为了把原文换成文案，现在投影时就只有文案了。
	Task *taskview.View `json:"task,omitempty"`
}

// PortalDetail 把内部详情投影成门户视图。
//
// 纯函数、不查库：所有裁剪都在这里一次做完（旧实现散落在 handler 里，漏一处就泄漏）。
// 报文侧下发「客户端发的（req_raw）」与「平台交给客户端的（resp_body）」；
// 不下发：upstream_raw / upstream_url / upstream_status / resp_raw（上游原文）/ resp_error /
// req_sha256 / pricing_snapshot / raw_usage / request_digest / channel_* / adapter_id /
// ip / group / cost / username / cap_reason。
//
// 为什么 resp_raw 也不给：它是上游原文，里面有服务商 CDN 域名与上游协议形状；
// 用户该看到的是自己真正收到的那一份（见 IOView.DeliveredRaw）。
func PortalDetail(d *Detail) *PortalDetailView {
	if d == nil {
		return nil
	}
	out := &PortalDetailView{
		PortalItem: PortalItemOf(d.Item, d.VideoResolution, d.VideoSeconds,
			d.TierPrice, d.ChargeBeforeCap),
		BillingMode:       d.BillingMode,
		ImageSpec:         d.ImageSpec,
		ImageRatio:        d.ImageRatio,
		ImageSpecSource:   d.ImageSpecSource,
		ImageQuality:      d.ImageQuality,
		ImageN:            d.ImageN,
		AudioDurationSecs: d.AudioDurationSecs,
		AudioChars:        d.AudioChars,
		RerankDocs:        d.RerankDocs,
		VideoResolution:   d.VideoResolution,
		VideoSeconds:      d.VideoSeconds,
		RefImages:         d.RefImages,
		RefVideos:         d.RefVideos,
		RefAudios:         d.RefAudios,
		RefVideoSeconds:   d.RefVideoSeconds,
		Frozen:            d.Frozen,
		Task:              d.Task,
	}
	if d.IO != nil {
		out.IO = portalIOOf(d.IO)
	}
	return out
}

// portalIOOf 把 IOView 投影成用户端报文视图。
//
// 选体规则只有一处：DeliveredRaw 有值就用它（改写 / 转码 / 失败错误体），
// 否则用 RespRaw（普通透传，两份逐字节相同，落库时只存了一份）。
func portalIOOf(io *IOView) *PortalIO {
	if io == nil {
		return nil
	}
	out := &PortalIO{
		ReqRaw:       io.ReqRaw,
		ReqBytes:     io.ReqBytes,
		ReqTruncated: io.ReqTruncated,
		ReqSource:    io.ReqSource,
		BodyMode:     io.BodyMode,
		RespBody:     io.RespRaw,
		RespBytes:    io.RespBytes,
		SSEFrames:    io.SSEFrames,
	}
	if io.DeliveredRaw != "" {
		out.RespBody = io.DeliveredRaw
	}
	// 失败体不经过 resp_bytes（那条路径没有「写出字节数」），不补就会显示「响应 0 B」
	// 却带着一段正文，用户会以为页面串了。
	if out.RespBytes == 0 && out.RespBody != "" {
		out.RespBytes = int64(len(out.RespBody))
	}
	out.OutputText = extractOutputText(out.RespBody)
	if len(io.Media) > 0 {
		out.Media = make([]PortalMedia, 0, len(io.Media))
		for _, m := range io.Media {
			out.Media = append(out.Media, PortalMedia{
				Kind: m.Kind, MIME: m.MIME, Bytes: m.Bytes,
				ArtifactIndex: m.ArtifactIndex, Note: m.Note,
			})
		}
	}
	return out
}

// portalOutputTextLimit 是抽取文本的上限（8 KiB）。
//
// 上限存在的理由：对话可以很长，而「文本视图」只是帮用户少点一次格式化；
// 正文已经完整下发在 RespBody 里，这里再复制一份没有意义。
const portalOutputTextLimit = 8 << 10

// extractOutputText 从响应体里抽取纯文本输出。
//
// 覆盖三种真实形态（都来自本系统自己的采集器，不是猜的）：
//   - 非流式 JSON：choices[0].message.content / choices[0].text / output_text / Anthropic content[]
//   - 流式聚合体：{"_note": ..., "content": ..., "streamed": true}（relay.streamAggregateJSON）
//   - 原始 SSE：开了 LD_CALL_IO_SSE 时逐帧 data: 拼接
//
// 取不到就返回空串（前端按「无可预览文本」处理），绝不回退成原文 ——
// 回退会让「没有文本输出」的图片/音频记录显示成一段 base64 或 JSON。
func extractOutputText(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var text string
	switch {
	case strings.HasPrefix(raw, "{"):
		text = textFromJSON([]byte(raw))
	case strings.Contains(raw, "data:"):
		text = textFromSSE(raw)
	}
	text = strings.TrimSpace(text)
	if len(text) > portalOutputTextLimit {
		// 按字节截断可能切坏 UTF-8 多字节字符，回退到最近一个完整字符边界。
		text = strings.ToValidUTF8(text[:portalOutputTextLimit], "")
	}
	return text
}

// textFromJSON 从完整 JSON 响应里取文本（含流式聚合体）。
func textFromJSON(raw []byte) string {
	var body struct {
		Content json.RawMessage `json:"content"`
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
			Text string `json:"text"`
		} `json:"choices"`
		OutputText json.RawMessage `json:"output_text"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return ""
	}
	if len(body.Choices) > 0 {
		if s, ok := jsonString(body.Choices[0].Message.Content); ok && s != "" {
			return s
		}
		if body.Choices[0].Text != "" {
			return body.Choices[0].Text
		}
	}
	if s, ok := jsonString(body.OutputText); ok && s != "" {
		return s
	}
	// 流式聚合体与 Anthropic 的顶层 content 都落在这里。
	if s, ok := jsonString(body.Content); ok {
		return s
	}
	return ""
}

// textFromSSE 从原始 SSE 文本里拼接增量（OpenAI delta / Anthropic content_block_delta）。
func textFromSSE(raw string) string {
	var b strings.Builder
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var oa struct {
			Choices []struct {
				Delta struct {
					Content          json.RawMessage `json:"content"`
					ReasoningContent json.RawMessage `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &oa) == nil {
			for _, c := range oa.Choices {
				if s, ok := jsonString(c.Delta.Content); ok {
					b.WriteString(s)
				}
				if s, ok := jsonString(c.Delta.ReasoningContent); ok {
					b.WriteString(s)
				}
			}
		}
		var cl struct {
			Type  string `json:"type"`
			Delta struct {
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(payload), &cl) == nil && cl.Type == "content_block_delta" {
			b.WriteString(cl.Delta.Text)
		}
		if b.Len() >= portalOutputTextLimit {
			break
		}
	}
	return b.String()
}

// jsonString 解析「字符串或字符串数组」两种形态的文本字段。
func jsonString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				b.WriteString(p.Text)
			}
		}
		return b.String(), true
	}
	return "", false
}
