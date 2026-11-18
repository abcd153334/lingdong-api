package adapters

// 本文件是火山方舟适配器的**出站与错误翻译唯一层**。
//
// 为什么要单独一层，而不是各文件各写一遍：
//   - 出站 URL 只有一种构造方式（cfg.BaseURL 含 /api/v3 + 适配器路径常量）。
//     任何一处写成 platform canonical path 都会 404，而 404 会被归因成渠道故障；
//   - 上游错误体的形状只有一种（`{"error":{"code","message","param","type"}}`），
//     翻译规则（哪些是调用方错、哪些是上游故障）必须只有一份 —— 两处各写一份时，
//     图片把它当调用方错、视频把它当上游故障，同一条 400 会让渠道被误熔断。
//
// 视频提交/轮询与**图片同步**都**有意不用 SDK 发送**（原因见 volcengine_ark_video.go
// 与 volcengine_ark_images.go 的文件头），所以这里的 arkDo 是它们唯一的出站口；
// 图片流式自己发（要读 SSE 原文），错误翻译同样回到 arkError / arkStatusError。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/volcengine/ark-runtime-go/arkruntime/model"

	"lingdong-api/internal/relay"
)

// arkCheckConfig 是所有出站前必须通过的配置校验（唯一入口）。
//
// 为什么单独抽出来：图片走 SDK、视频走自建 HTTP，两条路的出站方式不同，
// 但「密钥为空 / 密钥含换行 / 声明式配置解析失败」这三种错的处置完全一致。
// 分成两处写会漏掉其中一条，表现为「图片报配置错、视频 401」。
//
// DeclErr 必须上抛而不是忽略：它是 channels.adapter_config 解析失败的信号，
// 静默忽略会让「配置写错了」表现成「请求能发但结果不对」，排查成本极高。
func arkCheckConfig(cfg *relay.ChannelConfig) error {
	if cfg == nil {
		return &relay.ConfigError{Field: "channel", Msg: "渠道配置为空"}
	}
	if cfg.DeclErr != nil {
		return cfg.DeclErr
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return &relay.ConfigError{Field: "keys", Msg: "渠道未配置 API Key"}
	}
	// 换行注入：HTTP 头里带 \r\n 会构造出第二个头（请求走私），必须在这里挡掉。
	if strings.ContainsAny(cfg.APIKey, "\r\n") {
		return &relay.ConfigError{Field: "keys", Msg: "渠道密钥包含非法字符"}
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return &relay.ConfigError{Field: "base_url", Msg: "渠道未配置 Base URL（应形如 " + arkDefaultBase + "）"}
	}
	return nil
}

// arkHTTPResp 是一次原始出站的结果（状态码 + 原始体 + 响应头）。
type arkHTTPResp struct {
	Status int
	Body   []byte
	Header http.Header
}

// arkDo 是**所有非流式 Ark 出站**（视频提交/轮询/探活、图片同步）的唯一出站函数。
//
// 它只负责搬运：拼 URL、带鉴权、读原始字节、返回状态码；**不翻译错误**
// （翻译统一在 arkError / arkStatusError，见 TestArkDoReturnsStatusWithoutTranslating）。
//
// 两个刻意的选择：
//  1. 读体上限用 arkMaxUpstreamBody（64 MiB）而不是 JSON 通用的 8 MiB：
//     查询视频任务成功时会带产物直链与 usage，正常只有几 KB；
//     但上游偶尔回一整页列表（page_size 被运营调大）会超 8 MiB，
//     按 8 MiB 截断的表现是「JSON 解析失败」，归因会跑到「上游故障」上去。
//  2. 超限返回 relay.ErrUpstreamBodyTooLarge（包级哨兵），而不是静默截断：
//     网关要据此判「上游回了超大垃圾体」，不能当成客户端断开（499）。
func arkDo(ctx context.Context, cfg *relay.ChannelConfig, method, path string, body []byte) (*arkHTTPResp, error) {
	if err := arkCheckConfig(cfg); err != nil {
		return nil, err
	}
	u, err := relay.BuildRawURL(cfg, path)
	if err != nil {
		return nil, err
	}
	var rd io.Reader
	if len(body) > 0 {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, fmt.Errorf("构造上游请求失败：%w", err)
	}
	req.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	relay.SetAuthHeader(req, cfg)

	resp, err := client(cfg).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, rerr := readUpstreamBody(resp.Body, arkMaxUpstreamBody)
	if rerr != nil {
		return nil, rerr
	}
	return &arkHTTPResp{Status: resp.StatusCode, Body: b, Header: resp.Header}, nil
}

// arkErrText 从 Ark 错误体里取**给人看的那一句**。
//
// 取 `error.message` 而不是整段 JSON：这段文案会原样进 400 响应体与调用记录，
// 塞进 `{"code":"InvalidParameter","message":"..."}` 这种原文，用户看不出该怎么办。
// 取不到就退化成截断后的原文（保留 code 便于运营定位，绝不返回空串——
// 空串会让上层拼出「参数不合法：」这种没有信息量的句子）。
func arkErrText(b []byte) string {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 {
		return ""
	}
	var env struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(trimmed, &env) == nil && env.Error != nil {
		msg := strings.TrimSpace(env.Error.Message)
		if msg != "" {
			if code := strings.TrimSpace(env.Error.Code); code != "" {
				return msg + "（" + code + "）"
			}
			return msg
		}
	}
	return snippet(trimmed)
}

// arkStatusError 把**原始出站**（arkDo）拿到的非 2xx 翻译成平台错误（唯一处）。
//
// 为什么构造 model.APIError 再交给 arkError，而不是自己写一份 switch：
// 归类规则（哪些算调用方错、哪些算上游故障）必须只有一份。分成两份的后果是
// 同一条 400 在图片链路算调用方错、在视频链路算上游故障，渠道因此被误熔断。
//
// 两个细节都是踩过的：
//   - 4xx 取不到 `error.message` 时必须补一句人话：空文案会让上层拼出「参数不合法：」，
//     用户既不知道错在哪，也看不出这是上游拒的；
//   - 5xx 空体必须保持空串：IsAmbiguousSubmit 用「5xx + 空响应体」判「上游可能已建单」，
//     填满它等于允许网关换渠道重投，建出第二个上游任务。
func arkStatusError(status int, body []byte, cfg *relay.ChannelConfig) error {
	msg := strings.TrimSpace(arkErrText(body))
	if msg == "" && status >= 400 && status < 500 {
		msg = "上游拒绝了该请求但未说明原因"
	}
	return arkError(&model.APIError{HTTPStatusCode: status, Message: msg}, cfg)
}

// arkError 把上游/SDK 错误翻译成平台能正确归因的错误。
//
// 归类规则（与 outcome.go 的 classify 一致，改这里等于改计费归因，必须慎重）：
//   - 400 / 422 = 上游明确拒绝了**参数** → CallerParamError（400、不记渠道健康、不换渠道）。
//     典型：该模型不支持 stream、size 不合法、参考图超限。
//     落成普通 error 的后果是「每个候选都试一遍 → 502 全部失败」，用户看不到真正原因，
//     而每次尝试都是真实出站（甚至真实计费）。
//   - 401 / 403 / 429 / 5xx = **必须带状态码**返回 HTTPStatusError：
//     网关只按状态码决定「冷却这枚密钥 / 计渠道健康 / 换渠道」。
//     包成普通 error 的后果（改造前的实际症状）：一枚坏 key 让整条渠道被判上游故障、
//     10 次内自锁熔断；429 不冷却 key，于是下一跳又选中同一个被打限流的 key。
//   - 404 = 路径/配置问题（base_url 少写 /api/v3）→ 保留可读文案。
func arkError(err error, cfg *relay.ChannelConfig) error {
	if err == nil {
		return nil
	}
	var apiErr *model.APIError
	if errors.As(err, &apiErr) {
		msg := strings.TrimSpace(apiErr.Message)
		if msg == "" {
			msg = strings.TrimSpace(apiErr.Code)
		}
		code := apiErr.HTTPStatusCode
		switch code {
		case http.StatusBadRequest, http.StatusUnprocessableEntity:
			return &relay.CallerParamError{Msg: arkRedact(msg, cfg)}
		case http.StatusNotFound:
			return fmt.Errorf("上游接口不存在（HTTP 404）：请确认 Base URL 填到 /api/v3 为止")
		default:
			body := arkRedact(msg, cfg)
			if code < 500 && body == "" {
				body = "上游返回 HTTP " + fmt.Sprint(code)
			}
			// 5xx 且 body 为空时**保持为空**：IsAmbiguousSubmit 用「5xx + 空响应体」
			// 判定「上游可能已建单」，此时换渠道重投会建出第二个上游任务。
			// 恰好能覆盖「网关 502/504 返回空体」这个真实场景。
			return relay.StatusError(code, body)
		}
	}
	// RequestError 是「请求没发出去 / 响应读不出来」（含 ctx 取消、连接重置）。
	// 必须原样返回：它带着底层 ctx.Err()，网关靠 errors.Is 判客户端断开（499）。
	var reqErr *model.RequestError
	if errors.As(err, &reqErr) {
		if reqErr.HTTPStatusCode == http.StatusBadRequest && reqErr.Err != nil {
			// 构造请求体阶段的本地错误（如 extra_body 非对象）：属调用方参数问题。
			return &relay.CallerParamError{Msg: arkRedact(reqErr.Err.Error(), cfg)}
		}
		return err
	}
	return err
}

// arkRedact 是脱敏的唯一入口（密钥与上游域名绝不进对外文案）。
func arkRedact(text string, cfg *relay.ChannelConfig) string {
	if text == "" {
		return ""
	}
	base, key := "", ""
	if cfg != nil {
		base, key = cfg.BaseURL, cfg.APIKey
	}
	return relay.RedactUpstreamWith(text, base, key)
}

// arkReplaceModel 只替换顶层 model 一个键，其余键的**值字节保持原样**。
//
// 为什么用 map[string]json.RawMessage 而不是 map[string]any：
//   - any 会把整数变 float64（9007199254740993 丢精度）、把 1e10 变 10000000000、
//     把 1.0 变 1；计费参数（duration/seed/frames）全在这类值上，改了就错账。
//   - RawMessage 保留每个值的原始字节，只有**键序**会被 Go 的 map 排序重排。
//     JSON 语义上完全等价（对象键无序），这是「1:1 保真」在 Go 里的可达形态；
//     金标准测试按语义比较（键序无关），不按字节比较。
//
// 返回 error 而不是静默回退原始体：拿不到 model 键就无法保证请求打到对的模型，
// 静默发送等于把用户的请求送给错误的模型，不如 400。
func arkReplaceModel(body []byte, upstreamModel string) ([]byte, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, &relay.CallerParamError{Msg: "请求体为空"}
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &m); err != nil || m == nil {
		return nil, &relay.CallerParamError{Msg: "请求体必须是 JSON 对象"}
	}
	raw, err := json.Marshal(strings.TrimSpace(upstreamModel))
	if err != nil {
		return nil, err
	}
	m["model"] = raw
	out, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// arkMergeExtra 把未知字段合并进 typed 请求体（与 SDK 的 mergeExtraBody 同语义）。
//
// 为什么需要自己实现：SDK 只导出 WithExtraBody（作用于 SDK 自己的发送管道），
// 而流式请求我们**自己发**（SDK 的流式解析器会把未知事件当致命错误）。
// 合并顺序必须一致：extra 覆盖 typed 同名键（与 SDK 文档一致），
// 否则「typed 里有、但值是默认零值」的键会盖掉用户真实传的值。
func arkMergeExtra(typed any, extra map[string]any) ([]byte, error) {
	base, err := json.Marshal(typed)
	if err != nil {
		return nil, err
	}
	if len(extra) == 0 {
		return base, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(base, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("请求体不是 JSON 对象，无法合并未知字段")
	}
	for k, v := range extra {
		raw, merr := json.Marshal(v)
		if merr != nil {
			return nil, fmt.Errorf("字段 %s 无法序列化：%w", k, merr)
		}
		fields[k] = raw
	}
	return json.Marshal(fields)
}
