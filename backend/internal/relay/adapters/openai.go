// Package adapters 注册内置渠道适配器（S4：openai 兼容；其余厂商在 S6/S12 扩展）。
package adapters

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"lingdong-api/internal/pkg/textx"
	"lingdong-api/internal/relay"
)

// maxUpstreamBodyBytes 是读取上游响应体的上限，避免超大响应拖垮内存（与 relay 口径一致）。
const maxUpstreamBodyBytes = relay.MaxUpstreamBodyBytes

// maxTTSBodyBytes 是 TTS 音频响应体的上限（32 MiB），比 JSON 上限宽 4 倍。
//
// 为什么单独给一个上限：TTS 的响应体大小与「文本有多长」成正比，
// 拿 JSON 的 8 MiB 去卡它，长文本朗读会被判成 upstream_body_too_large（合法请求报错）；
// 但也不能不设上限，否则上游回一个无限流就能把网关内存吃满。
const maxTTSBodyBytes = relay.MaxBinaryBodyBytes

func init() {
	// 注册失败说明适配器命名冲突（编码错误），仅在断言层面暴露。
	_ = relay.RegisterAdapter(OpenAI{})
}

// OpenAI 是 OpenAI 兼容适配器：/v1/chat/completions 与 /v1/embeddings 原样透传。
type OpenAI struct{}

// Name 返回适配器标识。
func (OpenAI) Name() string { return "openai" }

// Caps 返回当前已实现的能力子集（S6 增加 image/edit_image/tts/asr/rerank/moderation；S7 增加 video）。
func (OpenAI) Caps() []string {
	return []string{"text", "embedding", "passthrough", "image", "edit_image", "tts", "asr", "rerank", "moderation", "video"}
}

// SupportsDeclarativeConfig 表示本适配器支持渠道级声明式配置
// （auth_header / path_overrides / request_patch / response_path）。
func (OpenAI) SupportsDeclarativeConfig() bool { return true }

// patchBody 应用声明式请求改写。
//
// 只在 JSON 体上调用：multipart 端点的字段改写没有意义（文件 part 更不能动），
// 调用方在 multipart 分支直接跳过，省一次解析。
func patchBody(body []byte, cfg *relay.ChannelConfig, model string) ([]byte, error) {
	if cfg == nil || len(cfg.Decl.RequestPatch) == 0 {
		return body, nil
	}
	return relay.ApplyRequestPatch(body, cfg.Decl.RequestPatch, model)
}

// Test 通过 GET {base_url}/v1/models 探测鉴权与可达性。
func (OpenAI) Test(ctx context.Context, cfg *relay.ChannelConfig) error {
	// nil 配置必须判成配置错误：原实现直接读 cfg.BaseURL 会 panic，
	// 而 panic 会被 HTTP 中间件兜成 500，看起来像「上游故障」（P2-02）。
	if cfg == nil {
		return &relay.ConfigError{Field: "channel", Msg: "适配器 openai：渠道配置为空"}
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return &relay.ConfigError{Field: "base_url", Msg: "适配器 openai：Base URL 未配置"}
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return fmt.Errorf("适配器 openai：密钥池为空")
	}
	// 连通测试与熔断探测都走这条路径：path_overrides 覆盖 /v1/models 后这里必须跟着走，
	// 否则会出现「配置已生效、但测试一直失败」进而把好渠道自动禁用。
	target, urlErr := relay.BuildURL(cfg, relay.PathModels, "")
	if urlErr != nil {
		return urlErr
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("适配器 openai：构造请求失败: %w", err)
	}
	relay.SetAuthHeader(req, cfg)

	resp, err := client(cfg).Do(req)
	if err != nil {
		return fmt.Errorf("连通失败：%w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 带上实际请求路径：覆盖 /v1/models 后失败时，运维要能一眼看出打的是哪个地址。
		return fmt.Errorf("上游返回 %d（GET %s）", resp.StatusCode, target)
	}
	return nil
}

// Chat 透传对话请求到 /v1/chat/completions。
//
// 配了 response_path 时把非标准响应重构成 OpenAI chat 形状。
// 只对非流式 2xx 生效：错误体要原样交给网关判断重试与脱敏；流式帧改了会破坏客户端解析。
func (OpenAI) Chat(ctx context.Context, cfg *relay.ChannelConfig, r *relay.RelayRequest) (*relay.RelayResponse, error) {
	resp, err := forward(ctx, cfg, relay.PathChatCompletions, r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || cfg == nil || cfg.Decl.ResponsePath == nil {
		return resp, nil
	}
	model := ""
	if r != nil {
		model = r.Model
	}
	body, rerr := relay.ReconstructChatCompletion(resp.Body, cfg.Decl.ResponsePath, model)
	if rerr != nil {
		return nil, rerr
	}
	resp.Body = body
	return resp, nil
}

// Embeddings 透传向量请求到 /v1/embeddings。
func (OpenAI) Embeddings(ctx context.Context, cfg *relay.ChannelConfig, r *relay.RelayRequest) (*relay.RelayResponse, error) {
	return forward(ctx, cfg, relay.PathEmbeddings, r)
}

// Completions 透传旧版补全请求到 /v1/completions。
func (OpenAI) Completions(ctx context.Context, cfg *relay.ChannelConfig, r *relay.RelayRequest) (*relay.RelayResponse, error) {
	return forward(ctx, cfg, relay.PathCompletions, r)
}

// Forward 通用透传（S6 多模态端点）：JSON 体走 model 字段替换；multipart 体走 form 内 model 替换；
// 原样回传上游响应（含二进制体与 Content-Type）。
func (OpenAI) Forward(ctx context.Context, cfg *relay.ChannelConfig, r *relay.RelayRequest, path string) (*relay.RawResponse, error) {
	if r == nil || len(r.Body) == 0 {
		return nil, fmt.Errorf("适配器 openai：请求体为空")
	}
	ctype := r.ContentType
	if ctype == "" {
		ctype = "application/json"
	}
	body := []byte(r.Body)
	// 用归一后的媒体类型判定，而不是原始头的字符串前缀（P2-01）。
	switch mt := mediaTypeOf(ctype); {
	case mt == "application/json" || strings.HasSuffix(mt, "+json"):
		// +json 后缀（application/problem+json 等）同样是 JSON，必须走同一套改写。
		patched, perr := patchBody(replaceModel(r), cfg, r.Model)
		if perr != nil {
			return nil, perr
		}
		// 图片端点：把平台档位/画幅翻译成 OpenAI 原生枚举。
		// 不翻译的后果是内置 gpt-image-1 传 "1K" 必然被上游 400（它只认 1024x1024 等固定值）。
		if isImageForwardPath(path) {
			if patched, perr = openAIImageSize(patched, r.Model, r.ImageSize); perr != nil {
				return nil, perr
			}
		}
		body = patched
	case mt == "multipart/form-data":
		// multipart 不做声明式改写：dot-path 对表单字段没有意义，文件 part 更不能改。
		//
		// 但「配了 request_patch 却静默跳过」是最坏的形态：运营以为改生效了，
		// 实际每次请求都按原样发出，且永远不会报错。这里直接判配置错误（fail-closed）。
		if cfg != nil && len(cfg.Decl.RequestPatch) > 0 {
			return nil, &relay.ConfigError{
				Field: "adapter_config.request_patch",
				Msg:   "multipart 端点不支持 request_patch（表单与文件 part 无法按 JSON 路径改写），请改用 path_overrides 或去掉该配置",
			}
		}
		fields := map[string]string{}
		if r.Model != "" {
			fields["model"] = r.Model
		}
		// 图片端点：档位/画幅同样要翻成上游枚举（edits / variations 与 generations 同一套）。
		if isImageForwardPath(path) {
			ff, ferr := openAIImageFormFields(r.Model, r.ImageSize)
			if ferr != nil {
				return nil, ferr
			}
			for k, v := range ff {
				fields[k] = v
			}
		}
		if nb, ok := rewriteMultipartFields(ctype, body, fields); ok {
			body = nb
		}
	}
	target, berr := relay.BuildURL(cfg, path, "")
	if berr != nil {
		return nil, berr
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("适配器 openai：构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", ctype)
	relay.SetAuthHeader(req, cfg)

	resp, err := client(cfg).Do(req)
	if err != nil {
		return nil, fmt.Errorf("上游请求失败：%w", err)
	}
	// 二进制端点（TTS）：2xx 一律流式交还响应体，所有权转移给调用方（这里**不能** defer Close）。
	//
	// 判据用 path 而不是响应头 Content-Type：上游把音频错标成 application/json 很常见，
	// 按响应头分流会退化成整段缓冲（长文本朗读直接撞 8 MiB 上限）。
	// 非 2xx 不走流式：错误体很小，网关要读出来做脱敏与归因。
	if path == relay.PathAudioSpeech && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// 先窥探头部再流式转发：按秒计费的音频必须拿到**产物时长**，而
		// WAV 的 data 块长度、FLAC 的 STREAMINFO 就写在头几十字节里（MP3 需配合
		// Content-Length）。不窥探只有两条路：整段落内存（长文本撞 8 MiB 上限），
		// 或者永远探测不到 → 长音频全部按最低消费收（系统性少收且无人察觉）。
		head, herr := readHead(resp.Body, ttsProbeBytes)
		if herr != nil {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("读取上游音频头失败：%w", herr)
		}
		return &relay.RawResponse{
			StatusCode:   resp.StatusCode,
			ContentType:  resp.Header.Get("Content-Type"),
			DurationSecs: relay.AudioDurationSecsSized(head, resp.ContentLength),
			Stream:       &cappedBody{rc: &peekedBody{head: head, rc: resp.Body}, limit: maxTTSBodyBytes},
		}, nil
	}
	defer resp.Body.Close()
	// 上限按端点族取：出图接口默认回 b64_json，4/3 放大后很容易越过 8 MiB，
	// 按 chat 的上限读会把正常出图判成"上游体过大"（用户看到 502）。
	buf, err := readUpstreamBody(resp.Body, relay.BodyCapFor(path))
	if err != nil {
		return nil, fmt.Errorf("读取上游响应失败：%w", err)
	}
	return &relay.RawResponse{StatusCode: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: buf}, nil
}

// SubmitTask 提交异步视频任务（S7）：POST {base}/v1/video/generations，严格解析上游任务 ID。
// AsyncEndpoints：OpenAI 兼容渠道只承接视频异步任务。
//
// 图像（`images/generations` 的 async 面）不在其中：本适配器的 SubmitTask 固定打
// PathVideoSubmit，把图像 body 发过去只会得到 502，且错误与真实原因无关。要做图像异步
// 必须先把上游端点接进来（新增一条提交/查询路径），不能靠这份声明"允许"它。
func (OpenAI) AsyncEndpoints() []string { return []string{"video/generations"} }

func (OpenAI) SubmitTask(ctx context.Context, cfg *relay.ChannelConfig, t *relay.TaskSubmitRequest) (string, error) {
	if t == nil || len(t.Body) == 0 {
		return "", fmt.Errorf("适配器 openai：请求体为空")
	}
	body, perr := patchBody(replaceModel(&relay.RelayRequest{Model: t.UpstreamModel, Body: t.Body}), cfg, t.UpstreamModel)
	if perr != nil {
		return "", perr
	}
	target, berr := relay.BuildURL(cfg, relay.PathVideoSubmit, "")
	if berr != nil {
		return "", berr
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("适配器 openai：构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	relay.SetAuthHeader(req, cfg)

	resp, err := client(cfg).Do(req)
	if err != nil {
		return "", fmt.Errorf("上游请求失败：%w", err)
	}
	defer resp.Body.Close()
	buf, err := readUpstreamBody(resp.Body, maxUpstreamBodyBytes)
	if err != nil {
		return "", fmt.Errorf("读取上游响应失败：%w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 用带状态码的类型化错误：任务提交路径要靠「有没有拿到响应 / 拿到什么状态」
		// 判断上游是否可能已建单（relay.IsAmbiguousSubmit），纯字符串做不到。
		return "", relay.StatusError(resp.StatusCode, snippet(buf))
	}
	var p struct {
		ID     string `json:"id"`
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(buf, &p); err != nil {
		return "", fmt.Errorf("适配器 openai：解析提交响应失败：%w", err)
	}
	if p.ID != "" {
		return p.ID, nil
	}
	if p.TaskID != "" {
		return p.TaskID, nil
	}
	return "", errors.New("适配器 openai：提交响应缺少任务 ID")
}

// PollTask 查询异步任务（S7）：GET {base}/v1/video/generations/{id}。
// 兼容 {status,video:{url}}、{status,url/output} 与 data 包裹结构；状态归一由调用方负责。
func (OpenAI) PollTask(ctx context.Context, cfg *relay.ChannelConfig, upstreamTaskID string) (*relay.TaskStatus, error) {
	if strings.TrimSpace(upstreamTaskID) == "" {
		return nil, errors.New("适配器 openai：上游任务 ID 为空")
	}
	target, urlErr := relay.BuildURL(cfg, relay.PathVideoPoll, upstreamTaskID)
	if urlErr != nil {
		return nil, urlErr
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("适配器 openai：构造请求失败: %w", err)
	}
	relay.SetAuthHeader(req, cfg)

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
		return nil, fmt.Errorf("上游返回 %d：%s", resp.StatusCode, snippet(buf))
	}
	type videoObj struct {
		URL string `json:"url"`
	}
	var p struct {
		Status   string    `json:"status"`
		Progress *int      `json:"progress"`
		Output   string    `json:"output"`
		URL      string    `json:"url"`
		Video    *videoObj `json:"video"`
		Error    *struct {
			Message string `json:"message"`
		} `json:"error"`
		ErrorMsg string `json:"error_msg"`
		Data     *struct {
			Status   string    `json:"status"`
			Progress *int      `json:"progress"`
			Output   string    `json:"output"`
			Video    *videoObj `json:"video"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf, &p); err != nil {
		return nil, fmt.Errorf("适配器 openai：解析查询响应失败：%w", err)
	}
	out := &relay.TaskStatus{Status: p.Status, ResultURL: p.URL, Error: p.ErrorMsg}
	if p.Video != nil && p.Video.URL != "" {
		out.ResultURL = p.Video.URL
	}
	if out.ResultURL == "" {
		out.ResultURL = p.Output
	}
	if p.Progress != nil {
		out.Progress = *p.Progress
	}
	if p.Error != nil {
		out.Error = p.Error.Message
	}
	if d := p.Data; d != nil {
		if out.Status == "" {
			out.Status = d.Status
		}
		if out.Progress == 0 && d.Progress != nil {
			out.Progress = *d.Progress
		}
		if out.ResultURL == "" {
			out.ResultURL = d.Output
		}
		if out.ResultURL == "" && d.Video != nil {
			out.ResultURL = d.Video.URL
		}
	}
	return out, nil
}

// snippet 截取响应体片段用于错误信息（避免超长日志）。
//
// 两道处理都必要：
//  1. **通用脱敏**（Bearer / sk- / api_key= 形态）。这段文案会出现在 400 响应体与
//     usages.error_msg 里，而适配器最常贴的正是上游 4xx 原文 —— 上游把
//     `Authorization: Bearer sk-...` 回显出来是常态。
//  2. **按字符截断**。按字节切会把 UTF-8 字符劈成非法序列（前端渲染成乱码），
//     中文错误文案里几乎必现。
//
// 渠道主机名的脱敏不在这里做：适配器只有 base_url 的字面值、拿不到渠道对象，
// 由出口统一做（relay.CallerMessageOf），避免两处各写一份规则。
func snippet(b []byte) string {
	const max = 200
	return textx.TruncateRunes(relay.RedactUpstream(string(b), nil, ""), max)
}

// replaceModelMultipart 在 multipart 请求体中替换 model 字段值（保留 boundary、其余 part 与头部原样）。
// 未找到 model 字段时返回 false（调用方沿用原始请求体）。
func replaceModelMultipart(contentType string, body []byte, model string) ([]byte, bool) {
	if model == "" {
		return nil, false
	}
	return rewriteMultipartFields(contentType, body, map[string]string{"model": model})
}

// rewriteMultipartFields 重写 multipart 里的文本字段（**唯一实现**，model / size 共用）。
//
// 语义：
//   - 字段已存在 → 用新值替换（文件 part 不动：带 filename 的 part 不是文本字段）；
//   - 字段不存在且新值非空 → 追加一个 part（用户只写了 aspect_ratio、像素得由网关补）；
//   - 新值为空 → 删除该 part（平台别名不能漏给上游，它们是未知键）。
//
// 为什么必须能"追加"：`/v1/images/edits` 的客户端常常只传 aspect_ratio，
// 而 OpenAI 只认 size —— 只会替换的实现会让这句话变成"什么都没发生"，
// 上游按 1024x1024 出图，账单按 2K 收，且两边都看不出问题。
func rewriteMultipartFields(contentType string, body []byte, fields map[string]string) ([]byte, bool) {
	if len(fields) == 0 {
		return nil, false
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" {
		return nil, false
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.SetBoundary(params["boundary"])
	done := make(map[string]bool, len(fields))
	touched := false
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false
		}
		name := p.FormName()
		if name == "" {
			continue
		}
		if v, ok := fields[name]; ok && p.FileName() == "" {
			done[name] = true
			touched = true
			if v == "" {
				_, _ = io.Copy(io.Discard, p)
				continue
			}
			h := make(textproto.MIMEHeader)
			h.Set("Content-Disposition", "form-data; name="+quoteMIME(name))
			w, err := mw.CreatePart(h)
			if err != nil {
				return nil, false
			}
			if _, err := io.WriteString(w, v); err != nil {
				return nil, false
			}
			continue
		}
		h := make(textproto.MIMEHeader)
		for k, vs := range p.Header {
			if strings.EqualFold(k, "Content-Disposition") {
				continue
			}
			for _, v := range vs {
				h.Add(k, v)
			}
		}
		cd := "form-data; name=" + quoteMIME(name)
		if fn := p.FileName(); fn != "" {
			cd += "; filename=" + quoteMIME(fn)
		}
		h.Set("Content-Disposition", cd)
		w, err := mw.CreatePart(h)
		if err != nil {
			return nil, false
		}
		if _, err := io.Copy(w, p); err != nil {
			return nil, false
		}
	}
	for name, v := range fields {
		if done[name] || v == "" {
			continue
		}
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", "form-data; name="+quoteMIME(name))
		w, err := mw.CreatePart(h)
		if err != nil {
			return nil, false
		}
		if _, err := io.WriteString(w, v); err != nil {
			return nil, false
		}
		touched = true
	}
	if err := mw.Close(); err != nil {
		return nil, false
	}
	if !touched {
		return nil, false
	}
	return buf.Bytes(), true
}

// quoteMIME 对 multipart 参数值加引号（转义双引号与反斜杠）。
func quoteMIME(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// forward 把请求体原样 POST 到上游并回传响应（不做重试/选路，S5 负责）。
func forward(ctx context.Context, cfg *relay.ChannelConfig, path string, r *relay.RelayRequest) (*relay.RelayResponse, error) {
	if r == nil || len(r.Body) == 0 {
		return nil, fmt.Errorf("适配器 openai：请求体为空")
	}
	body, perr := patchBody(replaceModel(r), cfg, r.Model)
	if perr != nil {
		return nil, perr
	}
	target, berr := relay.BuildURL(cfg, path, "")
	if berr != nil {
		return nil, berr
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("适配器 openai：构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	relay.SetAuthHeader(req, cfg)

	resp, err := client(cfg).Do(req)
	if err != nil {
		return nil, fmt.Errorf("上游请求失败：%w", err)
	}
	defer resp.Body.Close()
	buf, err := readUpstreamBody(resp.Body, maxUpstreamBodyBytes)
	if err != nil {
		return nil, fmt.Errorf("读取上游响应失败：%w", err)
	}
	return &relay.RelayResponse{StatusCode: resp.StatusCode, Body: buf}, nil
}

// StreamChat 转发流式对话：POST /v1/chat/completions（Accept: text/event-stream），
// 逐帧回调 onFrame；上游非 2xx 时读取错误体并直接回传（不回调）。
// 请求体会注入 stream_options.include_usage=true，确保终结帧携带 usage 供结算。
func (a OpenAI) StreamChat(ctx context.Context, cfg *relay.ChannelConfig, r *relay.RelayRequest, onFrame func(relay.SSEFrame) error) (*relay.StreamResult, error) {
	return a.streamTo(ctx, cfg, r, relay.PathChatCompletions, true, onFrame)
}

// StreamPath 按指定上游路径转发 SSE（/v1/responses 等）。
//
// 与 StreamChat **共用一个读循环**（streamTo）：事件流里断流、空闲超时、
// 半截流这三类故障的处置只有一份实现 —— 抄第二份的结果是"有一条路径忘了把
// 截断判成失败"，用户为半截答案付全价。
//
// wantUsage=false：stream_options 是 chat 的字段，responses 上游收到会 400。
// 那条链路的 usage 在终止事件的 response.usage 里，由 relay 侧的终态解析取出。
func (a OpenAI) StreamPath(ctx context.Context, cfg *relay.ChannelConfig, r *relay.RelayRequest, path string,
	onFrame func(relay.SSEFrame) error) (*relay.StreamResult, error) {
	return a.streamTo(ctx, cfg, r, path, false, onFrame)
}

// streamTo 是**唯一**的 SSE 读循环（chat 与 responses 共用）。
func (a OpenAI) streamTo(ctx context.Context, cfg *relay.ChannelConfig, r *relay.RelayRequest, path string,
	wantUsage bool, onFrame func(relay.SSEFrame) error) (*relay.StreamResult, error) {
	if r == nil || len(r.Body) == 0 {
		return nil, fmt.Errorf("适配器 openai：请求体为空")
	}
	// 顺序不能换：先 patch 再 include_usage，保证运营无法通过改写关掉 usage 上报
	// （关掉会让流式结算退化成估算口径）。
	patched, perr := patchBody(replaceModel(r), cfg, r.Model)
	if perr != nil {
		return nil, perr
	}
	body := patched
	if wantUsage {
		body = withIncludeUsage(patched)
	}
	target, berr := relay.BuildURL(cfg, path, "")
	if berr != nil {
		return nil, berr
	}

	// 空闲超时用「外部看门狗 + 派生 ctx」实现，而不是 http.ReadDeadline：
	// net/http 不对外暴露连接读截止时间，唯一可靠的中断手段就是取消请求上下文
	// （取消后 transport 直接关连接，读端立刻返回错误）。
	// 计时器在「收到每一帧后」重置，语义正好是「多久没有任何字节」。
	idle := relay.StreamIdleTimeout()
	sctx, guard := relay.NewIdleGuard(ctx, idle)
	defer guard.Stop()

	req, err := http.NewRequestWithContext(sctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("适配器 openai：构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	relay.SetAuthHeader(req, cfg)
	req.Header.Set("Accept", "text/event-stream")

	// 流式必须用 relay.UpstreamStreamClient()：整体超时会把合法的长回答掐断（P1-29）。
	resp, err := relay.UpstreamStreamClient().Do(req)
	if err != nil {
		if guard.Hit() {
			return nil, fmt.Errorf("上游响应头超时（空闲上限 %s）: %w", idle, err)
		}
		return nil, fmt.Errorf("上游请求失败：%w", err)
	}
	defer resp.Body.Close()

	res := &relay.StreamResult{StatusCode: resp.StatusCode, UpstreamRequestID: resp.Header.Get("x-request-id")}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf, _ := readUpstreamBody(resp.Body, maxUpstreamBodyBytes)
		res.ErrorMsg = string(buf)
		return res, nil
	}

	reader := bufio.NewReaderSize(resp.Body, 64*1024)
	for {
		line, readErr := relay.ReadLineLimited(reader, relay.MaxSSELineBytes)
		if trimmed := bytes.TrimRight(line, "\r\n"); bytes.HasPrefix(trimmed, []byte("data:")) {
			payload := bytes.TrimSpace(trimmed[len("data:"):])
			if bytes.Equal(payload, []byte("[DONE]")) {
				if onFrame != nil {
					if ferr := onFrame(relay.SSEFrame{Done: true}); ferr != nil {
						return res, ferr
					}
				}
				break
			}
			res.Frames++
			usage := relay.ParseUsage(payload)
			// 字段级取较大值：标准实现仅末帧带 usage，逐帧上报的实现也不会因中间帧缺字段而丢失用量。
			res.Usage = relay.MergeUsageMax(res.Usage, usage)
			res.CompletionTokensEst += relay.CountStreamOutputTokens(payload)
			if msg := parseStreamError(payload); msg != "" {
				res.ErrorMsg = msg
			}
			if onFrame != nil {
				if ferr := onFrame(relay.SSEFrame{Data: payload, Usage: &usage}); ferr != nil {
					return res, ferr
				}
			}
		}
		if readErr != nil {
			// 看门狗触发：上游连续 idle 时长没有任何字节。
			if guard.Hit() {
				return res, fmt.Errorf("上游流式空闲超时（上限 %s）：上游在 %.0f 秒内没有输出任何数据",
					idle, idle.Seconds())
			}
			// EOF 是正常收尾（上游不发 [DONE] 而是直接关连接，很常见）。
			if errors.Is(readErr, io.EOF) || ctx.Err() != nil {
				break
			}
			// 其余读错误（超时、连接被重置）必须**返回错误**（P1-29）。
			//
			// 改造前这里直接 break，把「传到一半断了」当成正常结束：
			// 调用方看到的是 200 + 不完整内容 + 可能缺失的 usage → 按估算结算，
			// 用户为一段被截断的答案付了全价，而渠道健康度完全没有记录这次失败。
			return res, fmt.Errorf("上游流式读取中断（空闲上限 %s）：%w", idle, readErr)
		}
		// 收到数据（哪怕只是空行/心跳）就重置看门狗：
		// 语义是「多久没有收到任何字节」，不是「整条流总共花了多久」。
		// 少了这一行，任何超过 idle 的合法长回答都会被中途掐断。
		guard.Reset()
	}
	return res, nil
}

// replaceModel 在存在模型别名时替换请求体的 model 字段，其余字段原样保留。
func replaceModel(r *relay.RelayRequest) []byte {
	if r.Model == "" {
		return r.Body
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(r.Body, &m); err != nil {
		return r.Body
	}
	mb, err := json.Marshal(r.Model)
	if err != nil {
		return r.Body
	}
	m["model"] = mb
	nb, err := json.Marshal(m)
	if err != nil {
		return r.Body
	}
	return nb
}

// withIncludeUsage 注入 stream_options.include_usage=true（保留既有 stream_options 字段）。
func withIncludeUsage(body []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	opts := map[string]json.RawMessage{}
	if raw, ok := m["stream_options"]; ok {
		_ = json.Unmarshal(raw, &opts)
	}
	opts["include_usage"] = json.RawMessage("true")
	ob, err := json.Marshal(opts)
	if err != nil {
		return body
	}
	m["stream_options"] = ob
	nb, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return nb
}

// parseStreamError 从错误帧提取 message；非错误帧返回空串。
func parseStreamError(payload []byte) string {
	var p struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &p); err != nil || p.Error == nil {
		return ""
	}
	return p.Error.Message
}

// client 构造带超时的 HTTP 客户端（超时缺省取 relay.DefaultAdapterTimeout）。
// client 返回上游客户端：不跟随重定向（3xx 原样返回，防「上游 302 打内网」），
// 并在建连期校验对端 IP（私网需 LD_UPSTREAM_ALLOW_PRIVATE=1 显式放行）。
func client(cfg *relay.ChannelConfig) *http.Client {
	var timeout time.Duration
	if cfg != nil {
		timeout = cfg.Timeout
	}
	return relay.UpstreamClient(timeout)
}

// errUpstreamBodyTooLarge 表示上游响应体超过上限。
//
// 必须是可识别的具体错误：调用方要据此判「上游回了一个超大垃圾体」，
// 而不是当成网络故障去 failover（每个渠道都会回同样的东西，白撞一遍）。
// errUpstreamBodyTooLarge 指向 relay 包级哨兵：网关侧要用 errors.Is 判归因（不能靠字符串）。
var errUpstreamBodyTooLarge = relay.ErrUpstreamBodyTooLarge

// readUpstreamBody 是 relay.ReadUpstreamBody 的薄包装（保留同包调用点）。
//
// 实现只在 relay 维护：超限要报错而不是静默截断（LoadReader 截断后 JSON 解析失败，
// 错误会被归因成「上游返回了坏数据」）。本包其它适配器一律调这里，不要另写。
func readUpstreamBody(r io.Reader, limit int64) ([]byte, error) {
	return relay.ReadUpstreamBody(r, limit)
}

// cappedBody 是「流式 + 硬上限」的响应体包装：读满 limit 字节后再读即返回超限错误。
//
// 与 readUpstreamBody 的差别只在内存：这里边收边发，不把 32 MiB 攒在内存里，
// 但同样不允许无限流把带宽/磁盘写穿（超限时 io.Copy 会带着错误中断）。
// cappedBody 把底层流的可读字节数卡在上限内。
type cappedBody struct {
	rc    io.ReadCloser
	limit int64
	read  int64
}

// ttsProbeBytes 是 TTS 音频的头部窥探长度。
//
// 8 KiB 覆盖 WAV 头（44 字节）、FLAC STREAMINFO（42 字节）以及带 ID3v2 标签的
// MP3（标签常见 1–4 KiB）。再大就只是多占内存，没有任何判定收益。
const ttsProbeBytes = 8 << 10

// readHead 预读至多 n 字节，**不因提前 EOF 报错**（短音频本来就凑不满 n）。
func readHead(r io.Reader, n int) ([]byte, error) {
	buf := make([]byte, n)
	got := 0
	for got < n {
		m, err := r.Read(buf[got:])
		got += m
		if err != nil {
			if err == io.EOF {
				return buf[:got], nil
			}
			return buf[:got], err
		}
		if m == 0 {
			// 防御 Read 返回 (0, nil)：不判会在此处空转。
			break
		}
	}
	return buf[:got], nil
}

// peekedBody 先把已预读的头部吐出去，再接上底层流；Close 关闭底层流。
//
// 不能简单用 io.MultiReader + NopCloser：那样上游连接的 Close 会丢，
// 长文本朗读下的连接泄漏会直接把渠道的连接池耗尽。
type peekedBody struct {
	head []byte
	off  int
	rc   io.ReadCloser
}

func (p *peekedBody) Read(b []byte) (int, error) {
	if p.off < len(p.head) {
		n := copy(b, p.head[p.off:])
		p.off += n
		return n, nil
	}
	return p.rc.Read(b)
}

func (p *peekedBody) Close() error { return p.rc.Close() }

// Read 最多允许读出 limit 字节。
func (c *cappedBody) Read(p []byte) (int, error) {
	if c.read >= c.limit {
		return 0, fmt.Errorf("%w（上限 %d 字节）", errUpstreamBodyTooLarge, c.limit)
	}
	if remain := c.limit - c.read; int64(len(p)) > remain {
		p = p[:remain]
	}
	n, err := c.rc.Read(p)
	c.read += int64(n)
	return n, err
}

// Close 关闭底层响应体。
func (c *cappedBody) Close() error { return c.rc.Close() }

// mediaTypeOf 归一 Content-Type 的类型段：`Application/JSON; charset=UTF-8` → `application/json`。
//
// 为什么不能直接前缀比较原始头：Content-Type 的类型段大小写不敏感、且允许在分号前带空格，
// 客户端发一次 `Application/JSON ; charset=utf-8` 就会绕过 model 替换与 request_patch
// （表现为「配置了改写却完全没生效」，且永远不报错）。
func mediaTypeOf(ctype string) string {
	mt, _, err := mime.ParseMediaType(ctype)
	if err != nil {
		// 解析失败（如 boundary 非法）时退回「取分号前 + 转小写」，仍比裸比较准确。
		mt = ctype
		if i := strings.IndexByte(mt, ';'); i >= 0 {
			mt = mt[:i]
		}
	}
	return strings.ToLower(strings.TrimSpace(mt))
}
