package spec

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/pkg/taskstate"
	"lingdong-api/internal/pkg/textx"
	"lingdong-api/internal/relay"
)

// Adapter 是一份规格的运行时形态（一个 slug 一个实例）。
//
// 实现的能力接口：relay.Adapter（必需）、relay.EndpointSupporter、relay.TaskAdapter、
// relay.DetailedTaskPoller。**不实现** OptionsSupporter / DeclarativeSupporter——
// 保存期会因此拒绝给规格渠道填 adapter_config/adapter_options，
// 这正是「鉴权与路径只有一个真相源」的机制保障。
type Adapter struct {
	spec  *Spec
	byKey map[string]Endpoint // 端点键 → 规则（含 poll 端点）
}

// New 由已校验的规格构造适配器；调用方必须先跑 Validate。
func New(s *Spec) *Adapter {
	byKey := make(map[string]Endpoint, len(s.Endpoints))
	for k, v := range s.Endpoints {
		// Binary / AcceptsAsync 都是派生事实（唯一来源是 apidoc 端点表），
		// 不能让规格自己声明：声明错了会把 JSON 端点按 32 MiB 读、
		// 或把"同步也要等结果"的端点当成一次性转发（回给用户一段任务号 JSON）。
		if fact, ok := apidoc.SpecEndpointByKey(k); ok {
			v.Binary = fact.Binary
			v.AcceptsAsync = fact.AcceptsAsync
		}
		byKey[k] = v
	}
	return &Adapter{spec: s, byKey: byKey}
}

// Name 是注册名（channels.adapter_id 的原值）。
func (a *Adapter) Name() string { return "spec:" + a.spec.Slug }

// Spec 返回规格只读视图（管理台展示用）。
func (a *Adapter) Spec() *Spec { return a.spec }

// Caps 由端点键派生（单一真相源：规格里不单独声明能力）。
func (a *Adapter) Caps() []string { return a.spec.Caps() }

// SupportsEndpoint 判断规格是否声明了该端点。
//
// 必须实现：不实现 EndpointSupporter 等于「全部端点都支持」，
// 同步网关会把 chat 请求派给只做视频的规格，结果是每次调用都 400 + 渠道健康度下降。
func (a *Adapter) SupportsEndpoint(kind relay.EndpointKind) bool {
	key := endpointKeyOf(kind)
	if key == "" {
		return false
	}
	_, ok := a.byKey[key]
	return ok
}

// AsyncEndpoints 返回规格声明的异步提交端点（models.endpoints 写法）。
//
// 空切片 = 这份规格只做同步（含「同步端点 + 桥接」形态）：那种情况下它不该承接任何
// 异步任务，返回空让判据自然为 false，不要在这里额外判断桥接。
func (a *Adapter) AsyncEndpoints() []string { return a.spec.AsyncEndpointKeys() }

// endpointKeyOf 把 relay 端点枚举转成 models.endpoints 写法（"chat/completions"）。
//
// relay 没有导出这个映射（docKindOf 是包内表），因此这里按端点枚举顺序建一张
// 反向表：值与 apidoc 的 EndpointKey 完全一致，由 schema 守护测试比对
// 「规格端点清单 == relay 端点枚举」来防止漂移。
func endpointKeyOf(kind relay.EndpointKind) string { return relay.EndpointKeyOf(kind) }

// endpoint 取端点规则；未声明时返回 ConfigError（不记渠道健康、不冷却密钥）。
func (a *Adapter) endpoint(key string) (Endpoint, error) {
	ep, ok := a.byKey[key]
	if !ok {
		return Endpoint{}, &relay.ConfigError{
			Scope: "渠道规格配置错误",
			Field: "endpoints." + key,
			Msg:   fmt.Sprintf("规格 %s 未声明端点 %s", a.spec.Slug, key),
		}
	}
	return ep, nil
}

/* ------------------------------- Adapter 接口 ------------------------------- */

// Test 做一次最小连通性探测：GET 渠道 BaseURL，把「鉴权失败」与「连不上」区分开。
//
// 不用 chat 探活的原因：会真实消耗上游额度。也不需要构造请求体——
// 这里要回答的只是「域名可达 + 鉴权头能被上游接受吗」，401/403 已经说明域名与路径是通的。
func (a *Adapter) Test(ctx context.Context, cfg *relay.ChannelConfig) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cfg.BaseURL, "/"), nil)
	if err != nil {
		return err
	}
	a.applyAuth(req, nil, cfg)
	resp, err := a.client(cfg).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("上游拒绝鉴权（HTTP %d），请检查渠道密钥", resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound:
		// 根路径 404 很常见（上游只开放业务路径），说明域名与 TLS 是通的。
		return nil
	default:
		return fmt.Errorf("上游返回 HTTP %d", resp.StatusCode)
	}
}

// Chat 转发一次（非流式）对话请求。
func (a *Adapter) Chat(ctx context.Context, cfg *relay.ChannelConfig, req *relay.RelayRequest) (*relay.RelayResponse, error) {
	return a.syncCall(ctx, cfg, req, "chat/completions")
}

// Completions 转发一次旧版补全请求。
func (a *Adapter) Completions(ctx context.Context, cfg *relay.ChannelConfig, req *relay.RelayRequest) (*relay.RelayResponse, error) {
	return a.syncCall(ctx, cfg, req, "completions")
}

// Embeddings 转发一次向量请求。
func (a *Adapter) Embeddings(ctx context.Context, cfg *relay.ChannelConfig, req *relay.RelayRequest) (*relay.RelayResponse, error) {
	return a.syncCall(ctx, cfg, req, "embeddings")
}

// Forward 通用透传：按 apidoc 给出的上游路径找到对应端点规则后转发。
func (a *Adapter) Forward(ctx context.Context, cfg *relay.ChannelConfig, req *relay.RelayRequest, path string) (*relay.RawResponse, error) {
	key := a.keyForPath(path)
	if key == "" {
		return nil, &relay.ConfigError{
			Scope: "渠道规格配置错误",
			Field: "endpoints",
			Msg:   fmt.Sprintf("规格 %s 没有声明路径 %s 对应的端点", a.spec.Slug, path),
		}
	}
	return a.forward(ctx, cfg, req, key, path)
}

// StreamChat 转发流式对话。
//
// 只支持 passthrough 模式：对话请求体里有 tools/response_format/stop 等十几项，
// 逐字段重建既写不完也必然漏；非 OpenAI 兼容的对话协议属于 L3（内置适配器）。
func (a *Adapter) StreamChat(ctx context.Context, cfg *relay.ChannelConfig, req *relay.RelayRequest,
	onFrame func(relay.SSEFrame) error) (*relay.StreamResult, error) {

	ep, err := a.endpoint("chat/completions")
	if err != nil {
		return nil, err
	}
	if !ep.Passthrough {
		return nil, &relay.ConfigError{
			Scope: "渠道规格配置错误", Field: "endpoints.chat/completions.passthrough",
			Msg: "流式对话要求该端点标记 passthrough（规格不支持逐字段重建对话请求体）",
		}
	}
	src, err := a.prepareRequest(ep, req)
	if err != nil {
		return nil, err
	}
	body, err := a.buildBody(cfg, ep, req, src)
	if err != nil {
		return nil, err
	}
	body = withStreamFlag(body, true)

	// 流式必须用**无整体超时**的 client + 空闲看门狗：
	// client(cfg) 的 60s 整体超时会切断合法的长回答（一个长思考可以跑 10 分钟）。
	// 这两件事必须成对：去掉整体超时却不上看门狗，上游「接上就不说话」会永久挂起。
	idle := relay.StreamIdleTimeout()
	sctx, guard := relay.NewIdleGuard(ctx, idle)
	defer guard.Stop()

	httpReq, err := a.newRequest(sctx, cfg, ep, body,
		requestPathVals(ep, src, req.Model))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "text/event-stream")
	resp, err := relay.UpstreamStreamClient().Do(httpReq)
	if err != nil {
		if guard.Hit() {
			return nil, fmt.Errorf("上游响应头超时（空闲上限 %s）：%w", idle, err)
		}
		return nil, err
	}
	defer resp.Body.Close()

	result := &relay.StreamResult{StatusCode: resp.StatusCode}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		result.ErrorMsg = strings.TrimSpace(string(raw))
		if result.ErrorMsg == "" {
			result.ErrorMsg = fmt.Sprintf("上游返回 HTTP %d", resp.StatusCode)
		}
		return result, nil
	}

	// SSE 逐行解析：只把 `data:` 载荷交给上层，其余（event/id/注释）丢弃。
	// 必须自己数帧：Frames=0 会被判成 upstream_empty_stream 并全额释放，
	// 但「上游 200 且真给了内容」的请求不该被当成空流。
	reader := bufio.NewReaderSize(resp.Body, 64*1024)
	for {
		line, readErr := relay.ReadLineLimited(reader, relay.MaxSSELineBytes)
		if trimmed := bytes.TrimRight(line, "\r\n"); len(trimmed) > 0 {
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				payload := bytes.TrimSpace(trimmed[len("data:"):])
				if len(payload) > 0 {
					if bytes.Equal(payload, []byte("[DONE]")) {
						return result, nil
					}
					frame := relay.SSEFrame{Data: payload}
					if u := relay.ParseUsage(payload); u.HasTokenUsage() {
						frame.Usage = &u
						result.Usage = u
					}
					result.Frames++
					if onFrame != nil {
						if cbErr := onFrame(frame); cbErr != nil {
							result.ErrorMsg = cbErr.Error()
							return result, cbErr
						}
					}
					// 收到有效数据才重置：空行/心跳也不算「有内容」，
					// 但它们同样证明连接还活着 —— 所以放在判空之前更准确。
				}
			}
		}
		if readErr != nil {
			if guard.Hit() {
				result.ErrorMsg = fmt.Sprintf("上游流式空闲超时（上限 %s）：上游在 %.0f 秒内没有输出任何数据",
					idle, idle.Seconds())
				return result, nil
			}
			if errors.Is(readErr, io.EOF) || ctx.Err() != nil {
				return result, nil
			}
			// 已吐字后中断：如实回传错误（上层按"部分成功"结算，不重复换渠道）。
			result.ErrorMsg = readErr.Error()
			return result, nil
		}
		guard.Reset()
	}
}

/* ------------------------------- 任务接口 ------------------------------- */

// SubmitTask 提交异步任务：按**平台给的入站端点**选提交端点，按 extract.task_id 取上游任务号。
func (a *Adapter) SubmitTask(ctx context.Context, cfg *relay.ChannelConfig, req *relay.TaskSubmitRequest) (string, error) {
	key, err := a.pickAsyncEndpoint(req.ModelID, req.EndpointKey)
	if err != nil {
		return "", err
	}
	ep := a.byKey[key]
	relayReq := &relay.RelayRequest{Model: req.UpstreamModel, ModelID: req.ModelID, Body: req.Body}
	src, err := a.prepareRequest(ep, relayReq)
	if err != nil {
		return "", err
	}
	body, err := a.buildBody(cfg, ep, relayReq, src)
	if err != nil {
		return "", err
	}
	raw, err := a.doJSON(ctx, cfg, ep, key, body,
		requestPathVals(ep, src, req.UpstreamModel))
	if err != nil {
		return "", err
	}
	taskID := strings.TrimSpace(firstString(raw, ep.Extract.TaskID))
	if taskID == "" {
		return "", fmt.Errorf("上游未返回任务号（取值路径 %s），无法轮询", ep.Extract.TaskID)
	}
	// 回写子协议标记：提交与轮询是两次独立调用，轮询必须知道查哪个端点。
	req.SubKind = key
	return taskID, nil
}

// PollTask 是旧签名的实现（无子协议上下文）：按唯一/首个异步端点轮询。
func (a *Adapter) PollTask(ctx context.Context, cfg *relay.ChannelConfig, upstreamTaskID string) (*relay.TaskStatus, error) {
	keys := a.spec.AsyncEndpointKeys()
	if len(keys) != 1 {
		return nil, fmt.Errorf("规格 %s 有 %d 个异步端点，轮询必须带子协议标记", a.spec.Slug, len(keys))
	}
	return a.poll(ctx, cfg, keys[0], upstreamTaskID)
}

// PollTaskDetailed 按 tasks.subtask_kind 选轮询端点。
func (a *Adapter) PollTaskDetailed(ctx context.Context, cfg *relay.ChannelConfig, req *relay.TaskPollRequest) (*relay.TaskStatus, error) {
	key := req.SubKind
	if key == "" {
		return a.PollTask(ctx, cfg, req.UpstreamTaskID)
	}
	if _, ok := a.byKey[key]; !ok {
		return nil, fmt.Errorf("规格 %s 没有异步端点 %s", a.spec.Slug, key)
	}
	return a.poll(ctx, cfg, key, req.UpstreamTaskID)
}

func (a *Adapter) poll(ctx context.Context, cfg *relay.ChannelConfig, submitKey, upstreamTaskID string) (*relay.TaskStatus, error) {
	submit := a.byKey[submitKey]
	if submit.Async == nil {
		return nil, fmt.Errorf("端点 %s 未声明 async", submitKey)
	}
	pollEP, ok := a.byKey[submit.Async.Poll]
	if !ok {
		return nil, fmt.Errorf("轮询端点 %s 不存在", submit.Async.Poll)
	}
	raw, err := a.doJSON(ctx, cfg, pollEP, submit.Async.Poll, nil, taskPathVals(upstreamTaskID))
	if err != nil {
		return nil, err
	}
	rawStatus := strings.TrimSpace(firstString(raw, pollEP.Extract.Status))
	status := strings.ToLower(rawStatus)
	mapped := ""
	// 1) 规格声明的映射优先：厂商私有值是运营写的，只有他知道 "3" 是什么意思。
	for platform, values := range pollEP.StatusMap {
		for _, v := range values {
			if equalCode(v, status) {
				mapped = platform
				break
			}
		}
		if mapped != "" {
			break
		}
	}
	// 2) 平台级别名兜底：status_map 漏写常见拼写时（SUCCESS/Completed/done）不必
	//    等到超时才报警。作用域不同，不构成第二真相源。
	matched := mapped != ""
	if !matched {
		if norm, ok := taskstate.Normalize(status); ok {
			mapped, matched = norm, true
		}
	}
	if !matched {
		// 未知状态一律按 running：宁可多轮询一次，也不要把「上游新增了排队态」
		// 误判成失败（失败会立即全额退款 + 丢弃已经产生的上游成本）。
		// 但 StatusMatched=false 会向上传递，桥接据此在连续多次后报配置错。
		mapped = taskstate.Running
	}
	// RawStatus 保留**上游原样**（不转小写）：报错文案要给运营一个能直接
	// 粘进 status_map 的值，转成小写会让"照抄仍不匹配"的困惑再发生一次。
	out := &relay.TaskStatus{Status: mapped, StatusMatched: matched, RawStatus: rawStatus}
	if pollEP.Extract.Progress != "" {
		if v := firstValue(raw, pollEP.Extract.Progress); v != nil {
			switch n := v.(type) {
			case float64:
				out.Progress = int(n)
			case string:
				if p, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
					out.Progress = p
				}
			}
		}
	}
	if pollEP.Extract.Error != "" {
		out.Error = firstString(raw, pollEP.Extract.Error)
	}
	// 产物时长（秒）：按秒计费的唯一来源。上游多给小数（1.234s），
	// 这里四舍五入后至少记 1 秒 —— 记 0 会让一次真实生成的音频变成免费。
	if pollEP.Extract.Duration != "" {
		if secs := secondsOf(firstValue(raw, pollEP.Extract.Duration), pollEP.Extract.DurationUnit); secs > 0 {
			// 两个字段同填：视频按 OutputSeconds 计、音频/音乐按 AudioSeconds 计，
			// 规格不知道自己是哪一类（同一份 poll 规则可能被两个模态复用），
			// 填同一个"产物时长"对两边都是正确语义。
			out.Usage.OutputSeconds = secs
			out.Usage.AudioSeconds = secs
		}
	}
	if mapped == taskstate.Succeeded && !pollEP.Extract.Results.IsZero() {
		out.ResultURLs = selectURLs(raw, *pollEP.Extract.Results)
	}
	if mapped == taskstate.Succeeded && pollEP.Extract.Result != "" && len(out.ResultURLs) == 0 {
		if u := firstString(raw, pollEP.Extract.Result); u != "" {
			out.ResultURLs = []string{u}
		}
	}
	return out, nil
}

// secondsOf 把上游的时长值（数字或数字字符串）折成整秒。
//
// 四舍五入而不是截断：1.6 秒的音频截断成 1 秒是系统性少收 37%。
//
// unit 决定是否需要先换算：有的上游按毫秒给（duration=196000）。
// 单位只有两个取值且保存期已校验，所以这里不需要再兜底。
func secondsOf(v any, unit string) float64 {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0
		}
		f = parsed
	default:
		return 0
	}
	if unit == "ms" {
		f = f / 1000
	}
	if f <= 0 {
		return 0
	}
	n := math.Floor(f + 0.5)
	if n < 1 {
		n = 1
	}
	return n
}

// selectURLs 是「从响应里取产物链接」的**唯一**实现（poll 与 bridge 共用）。
//
// 没有 where 时就是原来的 allStrings；有 where 时先按元素字段过滤再取链接 ——
// 上游的 results 数组经常混排（封面图、字幕、多段音频），不筛就永远取第 0 个，
// 表现为「音频请求回了一张 PNG」。
func selectURLs(root any, sel URLSelector) []string {
	if len(sel.Where) == 0 {
		return allStrings(root, sel.Path)
	}
	i := strings.Index(sel.Path, "[*]")
	if i < 0 {
		return allStrings(root, sel.Path)
	}
	head, tail := sel.Path[:i], sel.Path[i+len("[*]"):]
	elems := allValues(root, head+"[*]")
	out := make([]string, 0, len(elems))
	for _, el := range elems {
		if !matchWhere(el, sel.Where) {
			continue
		}
		if tail == "" {
			if s := toString(el); s != "" {
				out = append(out, s)
			}
			continue
		}
		for _, v := range allValues(el, "$"+tail) {
			if s := toString(v); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// matchWhere 判断一个数组元素是否满足全部过滤条件（元素内浅路径，精确相等）。
func matchWhere(el any, where map[string]string) bool {
	m, ok := el.(map[string]any)
	if !ok {
		return false
	}
	for k, want := range where {
		var cur any = m
		for _, part := range strings.Split(k, ".") {
			mm, ok := cur.(map[string]any)
			if !ok {
				return false
			}
			cur, ok = mm[part]
			if !ok {
				return false
			}
		}
		if !equalCode(toString(cur), want) {
			return false
		}
	}
	return true
}

// equalCode 是「上游给的字符串 vs 配置里写的字符串」的唯一比较器。
//
// ok_codes 与 status_map 曾经一个逐字比、一个忽略大小写：同一份配置里
// "Success" 与 "success" 一个判成功一个判失败，而两边都看不出问题。
func equalCode(got, want string) bool {
	return strings.EqualFold(strings.TrimSpace(got), strings.TrimSpace(want))
}

// pickAsyncEndpoint 按模型模态选提交端点。
//
// 规格可以有多个异步端点（例如同一厂商既有视频又有音乐），而 TaskSubmitRequest
// 只带模型 ID：用平台模型目录的 modality 分派，比"猜请求体里的字段"可靠得多。
func (a *Adapter) pickAsyncEndpoint(modelID, want string) (string, error) {
	keys := a.spec.AsyncEndpointKeys()
	if len(keys) == 0 {
		return "", &relay.ConfigError{
			Scope: "渠道规格配置错误", Field: "endpoints",
			Msg: fmt.Sprintf("规格 %s 没有声明异步端点，不能承接任务类模型", a.spec.Slug),
		}
	}
	// 平台给的端点优先：同一规格可以同时声明多条 submit 端点（如 video 与 music），
	// 按模态猜会在「一个模型能出图也能出片」时选错一条 —— 而选错的表现是上游 400，
	// 与真实原因（端点不匹配）完全无关。
	if want = apidoc.NormalizeEndpointKey(want); want != "" {
		for _, k := range keys {
			if apidoc.NormalizeEndpointKey(k) == want {
				return k, nil
			}
		}
		return "", &relay.ConfigError{
			Scope: "渠道规格配置错误", Field: "endpoints." + want,
			Msg: fmt.Sprintf("规格 %s 未声明端点 %s（已声明：%s）", a.spec.Slug, want, strings.Join(keys, " / ")),
		}
	}
	if len(keys) == 1 {
		return keys[0], nil
	}
	modality := modalityOf(modelID)
	for _, k := range keys {
		if fact, ok := apidoc.SpecEndpointByKey(k); ok && fact.Modality == modality {
			return k, nil
		}
	}
	return "", fmt.Errorf("规格 %s 有多个异步端点，但无法按模型 %s 的模态（%s）选中其一", a.spec.Slug, modelID, modality)
}

// keyForPath 把 Forward 拿到的**平台路径**反查成端点键。
//
// 两步，顺序不可颠倒：
//  1. 平台路径 → 端点键（`/v1/audio/speech` → `audio/speech`）。这是**主路径**：
//     规格里声明的是上游路径（`/api/v1/tts/submit`），平台路径永远对不上它。
//     旧实现只做了第 2 步，于是所有"上游路径 ≠ 平台路径"的规格（也就是绝大多数
//     非标厂商）都会报"规格没有声明路径 /v1/audio/speech 对应的端点"——
//     配置完全正确却被判成配置错误，这是非标接口接入失败的结构性原因之一。
//  2. 回退：按规格声明的上游路径匹配（兼容把上游路径写成与平台一致的规格）。
func (a *Adapter) keyForPath(path string) string {
	p := strings.TrimSpace(path)
	if p == "" {
		return ""
	}
	if key, ok := apidoc.EndpointKeyByPath(p); ok {
		if _, declared := a.byKey[key]; declared {
			return key
		}
	}
	keys := make([]string, 0, len(a.byKey))
	for k := range a.byKey {
		if IsPollKey(k) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fallback := ""
	for _, k := range keys {
		if strings.TrimRight(a.byKey[k].Path, "/") != strings.TrimRight(p, "/") {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(a.byKey[k].Method), http.MethodPost) {
			return k
		}
		if fallback == "" {
			fallback = k
		}
	}
	return fallback
}

/* ------------------------------- HTTP 细节 ------------------------------- */

func (a *Adapter) client(cfg *relay.ChannelConfig) *http.Client {
	var timeout time.Duration
	if cfg != nil {
		timeout = cfg.Timeout
	}
	return relay.UpstreamClient(timeout)
}

// newRequest 组装上游请求：URL 只能是 base_url + path（不允许规格自带域名）。
//
// 路径占位符的替换统一走 buildPath（唯一实现）：取值来源在保存期就定死了，
// 这里只负责把值交给它。
func (a *Adapter) newRequest(ctx context.Context, cfg *relay.ChannelConfig, ep Endpoint,
	body []byte, pv pathVals) (*http.Request, error) {

	path, err := buildPath(ep.Path, pv.values, pv.scopes)
	if err != nil {
		return nil, err
	}
	full := strings.TrimRight(cfg.BaseURL, "/") + path
	if len(ep.Query) > 0 {
		q := make([]string, 0, len(ep.Query))
		keys := make([]string, 0, len(ep.Query))
		for k := range ep.Query {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			q = append(q, k+"="+ep.Query[k])
		}
		sep := "?"
		if strings.Contains(full, "?") {
			sep = "&"
		}
		full += sep + strings.Join(q, "&")
	}

	method := strings.ToUpper(strings.TrimSpace(ep.Method))
	if method == "" {
		method = http.MethodPost
	}
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, full, reader)
	if err != nil {
		return nil, err
	}
	ct := ep.ContentType
	if ct == "" {
		ct = "application/json"
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", ct)
	}
	req.Header.Set("Accept", "application/json")
	a.applyAuth(req, &ep, cfg)
	return req, nil
}

// applyAuth 注入渠道密钥（唯一形态：前缀 + 密钥；不接受任意头名）。
func (a *Adapter) applyAuth(req *http.Request, ep *Endpoint, cfg *relay.ChannelConfig) {
	auth := Auth{In: "header", Name: "Authorization", Prefix: "Bearer "}
	if ep != nil && ep.Auth != nil {
		auth = *ep.Auth
		if strings.TrimSpace(auth.In) == "" {
			auth.In = "header"
		}
		if strings.TrimSpace(auth.Name) == "" {
			auth.Name = "Authorization"
		}
	}
	key := cfg.APIKey
	if key == "" {
		return
	}
	switch strings.ToLower(auth.In) {
	case "query":
		q := req.URL.Query()
		q.Set(auth.Name, key)
		req.URL.RawQuery = q.Encode()
	case "body":
		// body 注入在 buildBody 阶段完成（这里只处理 header/query，避免二次读 body）
	default:
		req.Header.Set(auth.Name, auth.Prefix+key)
	}
}

// doJSON 发请求并把响应解析成 JSON（或返回 ConfigError/上游错误）。
func (a *Adapter) doJSON(ctx context.Context, cfg *relay.ChannelConfig, ep Endpoint, key string,
	body []byte, pv pathVals) (any, error) {

	req, err := a.newRequest(ctx, cfg, ep, body, pv)
	if err != nil {
		return nil, err
	}
	resp, err := a.client(cfg).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := relay.ReadUpstreamBody(resp.Body, relay.MaxUpstreamBodyBytes)
	if err != nil {
		return nil, err
	}
	if err := a.checkSuccess(cfg, ep, resp.StatusCode, raw); err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &relay.ConfigError{
			Scope: "渠道规格配置错误", Field: "endpoints." + key + ".extract",
			Msg: "上游返回的不是 JSON，无法按取值路径解析",
		}
	}
	return out, nil
}

// syncCall 是 Dedicated 端点（chat/completions/embeddings）的统一实现。
func (a *Adapter) syncCall(ctx context.Context, cfg *relay.ChannelConfig, req *relay.RelayRequest, key string) (*relay.RelayResponse, error) {
	ep, err := a.endpoint(key)
	if err != nil {
		return nil, err
	}
	src, err := a.prepareRequest(ep, req)
	if err != nil {
		return nil, err
	}
	body, err := a.buildBody(cfg, ep, req, src)
	if err != nil {
		return nil, err
	}
	res, err := a.doRaw(ctx, cfg, ep, key, body,
		requestPathVals(ep, src, req.Model))
	if err != nil {
		return nil, err
	}
	return &relay.RelayResponse{StatusCode: res.Status, Body: res.Body}, nil
}

// forward 是 Forward 的实现：找到端点后转发并回传原始响应（含二进制）。
func (a *Adapter) forward(ctx context.Context, cfg *relay.ChannelConfig, req *relay.RelayRequest, key, path string) (*relay.RawResponse, error) {
	ep := a.byKey[key]
	// 扩展字段的默认值与必填/类型校验必须在**分流之前**：桥接与普通转发
	// 都要经过这里。校验放在 buildBody 里会漏掉「没有 body 规则」的端点
	// （那时 buildBody 根本不会被调用）。
	src, err := a.prepareRequest(ep, req)
	if err != nil {
		return nil, err
	}
	pv := requestPathVals(ep, src, req.Model)
	// 同步桥接：上游只有「提交 + 查询」两个端点，而平台这个端点是同步的。
	// 必须在组装普通转发之前分流，否则会把提交响应（任务号 JSON）当成产物回给调用方。
	//
	// mode=task 的两步响应端点（images/generations）同样要走这条路：
	// async:true 的请求由任务队列轮询（SubmitTask），而**不传 async 的请求**
	// 是同步调用，只能在本次请求内等完 —— 一份声明支撑两条路。
	if ep.Async.IsBridge() || (ep.Async.IsTask() && ep.AcceptsAsync) {
		return a.bridge(ctx, cfg, ep, key, req, src, pv)
	}
	// multipart 端点（图片编辑/转写）由内置适配器承担；这里只透传原始 body。
	body := []byte(req.Body)
	if ep.Body != nil || ep.Passthrough {
		built, berr := a.buildBody(cfg, ep, req, src)
		if berr != nil {
			return nil, berr
		}
		body = built
	} else if ep.Method == "" || strings.EqualFold(ep.Method, http.MethodGet) {
		body = nil
	}
	res, err := a.doRaw(ctx, cfg, ep, key, body, pv)
	if err != nil {
		return nil, err
	}
	// 响应类型必须以上游为准：写死 application/json 会把音频/图片标错，
	// 浏览器与播放器据此拒绝解析（表现为"接口 200 但没有声音/图裂"）。
	// 兜底顺序：上游头 → 规格声明的 response_content_type → JSON。
	ctype := strings.TrimSpace(res.ContentType)
	if ctype == "" {
		ctype = strings.TrimSpace(ep.ResponseType)
	}
	if ctype == "" {
		ctype = "application/json"
	}
	return &relay.RawResponse{StatusCode: res.Status, Body: res.Body, ContentType: ctype}, nil
}

// rawResp 是一次上游调用的原始结果。
//
// 带上 Content-Type 而不是让调用方假设 JSON：同一个规格里可能既有 JSON 端点
// 也有二进制端点（同步 TTS 直接回 WAV），写死类型就是"200 但播不出来"。
type rawResp struct {
	Body        []byte
	Status      int
	ContentType string
}

// doRaw 发请求并按成功判据校验，返回原始响应体、状态码与响应类型。
func (a *Adapter) doRaw(ctx context.Context, cfg *relay.ChannelConfig, ep Endpoint, key string,
	body []byte, pv pathVals) (*rawResp, error) {

	req, err := a.newRequest(ctx, cfg, ep, body, pv)
	if err != nil {
		return nil, err
	}
	if ep.Passthrough {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client(cfg).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// 上限按端点族取：
	//   - 二进制产物（音频/文件）：32 MiB —— 按 8 MiB 读会把长文本朗读判成
	//     "上游体过大"（用户看到 502，实际只是音频长了）；
	//   - 图片 JSON（b64_json，4/3 放大）：64 MiB，见 relay.BodyCapFor；
	//   - 其余 JSON：8 MiB（bridge_test 锁住这条）。
	limit := relay.BodyCapFor(key)
	if ep.Binary {
		limit = BridgeMaxArtifactBytes
	}
	raw, err := relay.ReadUpstreamBody(resp.Body, limit)
	if err != nil {
		return nil, err
	}
	if err := a.checkSuccess(cfg, ep, resp.StatusCode, raw); err != nil {
		return nil, err
	}
	return &rawResp{Body: raw, Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type")}, nil
}

// checkSuccess 按端点声明的成功判据判断；失败时尽量带出上游错误信息。
//
// 归因必须分两类（否则用户参数写错永远显示成"服务暂时不可用"）：
//   - 400/404/422 → CallerParamError：用户参数或路径错，400 返回，**不记渠道健康**；
//   - 401/403 → 上游故障：鉴权错是渠道配置问题，记为渠道失败才会触发运营处理；
//   - 5xx → 上游故障。
func (a *Adapter) checkSuccess(cfg *relay.ChannelConfig, ep Endpoint, status int, body []byte) error {
	success := ""
	codePath, okPath := "", ""
	var okCodes []string
	if ep.Errors != nil {
		success = ep.Errors.Success
		codePath = ep.Errors.CodePath
		okCodes = ep.Errors.OKCodes
		if ep.Errors.MessagePath != "" {
			okPath = ep.Errors.MessagePath
		}
	}
	if success == "" || success == "http_2xx" {
		if status >= 200 && status < 300 {
			return nil
		}
		return a.failOf(cfg, status, extractMessage(body, okPath))
	}
	// body_code：HTTP 200 但业务码表示失败是国产厂商的常见形态。
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return &relay.ConfigError{
			Scope: "渠道规格配置错误", Field: "endpoints.errors.code_path",
			Msg: "success=body_code 但上游返回的不是 JSON",
		}
	}
	code := strings.TrimSpace(firstString(parsed, codePath))
	for _, ok := range okCodes {
		if equalCode(code, ok) {
			return nil
		}
	}
	msg := extractMessage(body, okPath)
	if msg == "" {
		msg = "上游业务码 " + code
	}
	// HTTP 通了、业务码说失败：这是**上游拒绝了这次请求的内容**（参数越界、素材
	// MIME 不在白名单、内容审核…），不是「服务挂了」。
	//
	// 必须按调用方参数错返回（400 + 上游原话），否则用户看到的是
	// 「服务暂时不可用，本次未扣费，请稍后重试」——IndexTTS 的参考音频返回
	// `audio/wave` 而白名单只收 `audio/wav` 时就是这个症状（线上踩过）：
	// 模型配置完全正确、上游也活着，用户却被引导去"稍后重试"，永远查不出原因。
	//
	// 上游真故障会伴随 HTTP 5xx / 401，不会伪装成 200，所以这里不存在误判。
	if status >= 200 && status < 300 {
		return a.rejectedByUpstream(cfg, msg)
	}
	return a.failOf(cfg, status, msg)
}

// rejectedByUpstream 把「HTTP 2xx + 业务码失败」归到调用方侧（400，不记渠道健康）。
//
// 与 failOf 的 4xx 分支同源：都走 relay 的同一套脱敏，不在这里另写一份。
func (a *Adapter) rejectedByUpstream(cfg *relay.ChannelConfig, msg string) error {
	base, key := "", ""
	if cfg != nil {
		base, key = cfg.BaseURL, cfg.APIKey
	}
	safe := relay.RedactUpstreamWith(msg, base, key)
	return &relay.CallerParamError{Msg: "上游未接受本次请求：" + safe}
}

// failOf 把一次失败响应分诊成「调用方参数错」或「上游故障」。
//
// 400/404/422 一律算参数错：这是内置 autodl 适配器早就在做的事（把上游 400 的
// 原文回给调用方），规格链路之前统一折成 503，导致同一个平台上两种行为。
// 脱敏必须走 relay 的同一套规则（先脱敏后截断），不能在这里另写一份。
func (a *Adapter) failOf(cfg *relay.ChannelConfig, status int, msg string) error {
	if status == 400 || status == 404 || status == 422 {
		base, key := "", ""
		if cfg != nil {
			base, key = cfg.BaseURL, cfg.APIKey
		}
		safe := relay.RedactUpstreamWith(msg, base, key)
		return &relay.CallerParamError{Msg: fmt.Sprintf("上游拒绝了本次参数（HTTP %d）：%s", status, safe)}
	}
	return &upstreamError{status: status, msg: msg}
}

// upstreamError 是「上游明确返回失败」：计入渠道健康度（与配置错误区分开）。
type upstreamError struct {
	status int
	msg    string
}

func (e *upstreamError) Error() string {
	return fmt.Sprintf("上游返回失败（HTTP %d）：%s", e.status, e.msg)
}

// maxMessageRunes 是上游错误摘要的最大**字符**数（不是字节）。
//
// 落库列 usages.error_msg 是 varchar(500)（PG 按字符计数），300 个字符留足余量。
const maxMessageRunes = 300

// extractMessage 从上游响应体里取一段可展示的原因。
//
// 截断必须走 textx（按 rune）：这里曾经是 `s[:n]+"…"` 按**字节**切，
// 一旦切在多字节字符中间就产出非法 UTF-8，PG 写入报 22021，
// 整个结算/释放事务回滚 —— 结果是冻结永久滞留（textx 的文件头记录过同类事故）。
func extractMessage(body []byte, path string) string {
	if path == "" {
		return textx.TruncateRunesEllipsis(strings.TrimSpace(string(body)), maxMessageRunes)
	}
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return textx.TruncateRunesEllipsis(strings.TrimSpace(string(body)), maxMessageRunes)
	}
	if v := firstString(parsed, path); v != "" {
		return textx.TruncateRunesEllipsis(v, maxMessageRunes)
	}
	return textx.TruncateRunesEllipsis(strings.TrimSpace(string(body)), maxMessageRunes)
}

/* ------------------------------- 请求体重建 ------------------------------- */

// buildBody 按规则重建请求体（**唯一**的请求体合成实现）。
//
// 两条路径共用同一套收尾逻辑，区别只在起点：
//   - passthrough：原样转发调用方请求体，只做「按 drop 删字段」；
//   - 白名单（Body != nil）：**从零重建**，只保留 const/copy/arrays 声明的字段。
//
// 为什么白名单而不是"改几个字段"：调用方请求体里有我们不想透传的东西
// （如 n、watermark、回调地址），逐字段删除必然漏；重建的默认值就是"不发"。
//
// 收尾顺序（改动前先看，任何一步提前 return 都会让后面的规则静默失效）：
// body 鉴权 → 上游模型名（除非 omit_model）。历史实现里 passthrough 分支提前 return，
// 于是"透传 + drop"永远拿不到 body 鉴权与模型覆盖，报错却看起来像上游的问题。
// prepareRequest 把调用方请求体解析成 `$req` 视图，并补齐扩展字段默认值、做必填/类型校验。
//
// 为什么单独一步、且在每个入口都调用：`request_fields` 要服务的端点可能**没有**
// body 规则（buildBody 因此不会被调用）。校验若写在 buildBody 里，那类端点就会
// 把缺字段的请求直接发给上游。
func (a *Adapter) prepareRequest(ep Endpoint, req *relay.RelayRequest) (map[string]any, error) {
	src := map[string]any{}
	if req != nil && len(req.Body) > 0 {
		if err := json.Unmarshal(req.Body, &src); err != nil {
			return nil, &relay.CallerParamError{Msg: "请求体不是合法 JSON"}
		}
	}
	if len(ep.RequestFields) == 0 {
		return src, nil
	}
	applyRequestDefaults(ep, src)
	if err := checkRequestFields(ep, src); err != nil {
		return nil, err
	}
	return src, nil
}

func (a *Adapter) buildBody(cfg *relay.ChannelConfig, ep Endpoint, req *relay.RelayRequest,
	src map[string]any) ([]byte, error) {

	out := make(map[string]any, 8)
	if ep.Passthrough {
		for k, v := range src {
			out[k] = v
		}
		if ep.Body != nil {
			for _, k := range ep.Body.Drop {
				delete(out, k)
			}
		}
	} else if ep.Body != nil {
		for k, v := range ep.Body.Const {
			out[k] = v
		}
		for target, sources := range ep.Body.Copy {
			// 候选链：取第一个**有值**的来源（空串视为没给，继续试下一个）。
			if v, ok := pickReqField(src, sources); ok {
				out[target] = v
			}
		}
		for target, arr := range ep.Body.Arrays {
			values, ok := lookupReqField(src, strings.TrimPrefix(arr.From, "$req."))
			if !ok {
				continue
			}
			list, ok := values.([]any)
			if !ok {
				continue
			}
			items := make([]any, 0, len(list))
			for _, el := range list {
				if arr.Max > 0 && len(items) >= arr.Max {
					break
				}
				item := make(map[string]any, len(arr.Item))
				for k, v := range arr.Item {
					if v == "$value" {
						item[k] = el
					}
				}
				items = append(items, item)
			}
			if len(items) > 0 {
				out[target] = items
			}
		}
	}

	// body 鉴权：auth.in=body 一直是**声明了但不生效**的死开关（applyAuth 只处理 header/query）。
	// 一批自建服务把密钥放在请求体的 `token`/`api_key` 字段里，缺这一步它们只能 401。
	if ep.Auth != nil && strings.EqualFold(strings.TrimSpace(ep.Auth.In), "body") &&
		cfg != nil && cfg.APIKey != "" {
		out[ep.Auth.Name] = ep.Auth.Prefix + cfg.APIKey
	}

	if !ep.OmitModel {
		setUpstreamModel(out, ep, req)
	}
	return json.Marshal(out)
}

// pickReqField 按候选链取第一个有值的请求字段。
func pickReqField(src map[string]any, sources CopySource) (any, bool) {
	for _, s := range sources {
		v, ok := lookupReqField(src, strings.TrimPrefix(s, "$req."))
		if !ok || isEmptyJSONValue(v) {
			continue
		}
		return v, true
	}
	return nil, false
}

// isEmptyJSONValue 判断"调用方没给这个字段"：null 与空白字符串都算没给。
func isEmptyJSONValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(x) == ""
	default:
		return false
	}
}

// setUpstreamModel 写入上游模型名：const 显式写死时以 const 为准，否则一律覆盖。
//
// 为什么必须**覆盖**而不是"有就跳过"：调用方发来的是平台对外模型 ID，
// 上游只认自己的型号名（运营经常配置别名，例如本地 `gpt-4o-mini` → 上游 `MiniMax-H3`）。
// 透传原值的效果是「渠道看起来完全正常，但每次调用都 404 model_not_found」。
// 内置 openai 适配器的 replaceModel 也是无条件覆盖，两边语义必须一致。
func setUpstreamModel(body map[string]any, ep Endpoint, req *relay.RelayRequest) {
	if ep.Body != nil {
		if v, ok := ep.Body.Const["model"]; ok {
			body["model"] = v
			return
		}
	}
	if req.Model != "" {
		body["model"] = req.Model
	}
}

// lookupReqField 支持 `a.b` 形式的浅层取字段（深度 ≤ 3），够表达
// `$req.image_urls`、`$req.input.images` 这类常见形态；更深的取值属 L3。
func lookupReqField(src map[string]any, path string) (any, bool) {
	parts := strings.Split(path, ".")
	var cur any = src
	for _, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// withStreamFlag 强制 stream=true（非流式调用方不该走到这里）。
func withStreamFlag(body []byte, stream bool) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	m["stream"] = stream
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

/* ------------------------------- JSON 取值 ------------------------------- */

// segment 是取值路径的一段：字段名 / 下标 / 通配。
type segment struct {
	field string
	index int
	wild  bool
}

func parsePath(path string) ([]segment, bool) {
	if path == "" || !strings.HasPrefix(path, "$") {
		return nil, false
	}
	rest := path[1:]
	var segs []segment
	for len(rest) > 0 {
		switch rest[0] {
		case '.':
			rest = rest[1:]
			end := strings.IndexAny(rest, ".[")
			if end < 0 {
				end = len(rest)
			}
			segs = append(segs, segment{field: rest[:end]})
			rest = rest[end:]
		case '[':
			end := strings.IndexByte(rest, ']')
			if end < 0 {
				return nil, false
			}
			inner := rest[1:end]
			if inner == "*" {
				segs = append(segs, segment{wild: true})
			} else {
				idx, err := strconv.Atoi(inner)
				if err != nil {
					return nil, false
				}
				segs = append(segs, segment{index: idx})
			}
			rest = rest[end+1:]
		default:
			return nil, false
		}
	}
	return segs, true
}

// firstValue 按路径取第一个命中的值。
func firstValue(root any, path string) any {
	vals := allValues(root, path)
	if len(vals) == 0 {
		return nil
	}
	return vals[0]
}

// firstString 按路径取第一个命中的值并转成字符串（数字/布尔也能转）。
func firstString(root any, path string) string {
	v := firstValue(root, path)
	return toString(v)
}

// allStrings 按路径取出全部字符串值（`[*]` 场景），保持原顺序。
func allStrings(root any, path string) []string {
	vals := allValues(root, path)
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if s := toString(v); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func allValues(root any, path string) []any {
	segs, ok := parsePath(path)
	if !ok {
		return nil
	}
	cur := []any{root}
	for _, seg := range segs {
		next := make([]any, 0, len(cur))
		for _, node := range cur {
			switch {
			case seg.wild:
				if arr, ok := node.([]any); ok {
					next = append(next, arr...)
				}
			case seg.field != "":
				if m, ok := node.(map[string]any); ok {
					if v, exists := m[seg.field]; exists {
						next = append(next, v)
					}
				}
			default:
				if arr, ok := node.([]any); ok && seg.index >= 0 && seg.index < len(arr) {
					next = append(next, arr[seg.index])
				}
			}
		}
		cur = next
		if len(cur) == 0 {
			return nil
		}
	}
	return cur
}

func toString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		return ""
	}
}

/* ------------------------------- SSE 读取 ------------------------------- */

// SSE 逐行读取与单帧限长统一在 relay.ReadLineLimited（唯一实现）：
// 以前本包有一个 bufReader、adapters 包用 ReadBytes('\n')，两份都没上限 ——
// 上游发一个不换行的超大帧就能把进程打爆。限长必须只有一处，否则修一份漏一份。

// ModalityOf 暴露给外部（契约测试用）。
func (a *Adapter) ModalityOf(modelID string) string { return modalityOf(modelID) }

// ensure interface compliance（编译期断言：少实现一个方法会在这里报错）。
var (
	_ relay.Adapter            = (*Adapter)(nil)
	_ relay.EndpointSupporter  = (*Adapter)(nil)
	_ relay.TaskAdapter        = (*Adapter)(nil)
	_ relay.DetailedTaskPoller = (*Adapter)(nil)
)
