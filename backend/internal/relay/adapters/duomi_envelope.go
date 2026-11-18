package adapters

import (
	"encoding/json"
	"strings"
)

// duomiEnvelope 是 duomi **任意**响应（提交 / 查询 / 探活）的顶层视图。
//
// 为什么要有这一个结构：改造前提交链路与轮询链路各写一份 JSON 视图，字段集不同 ——
// 提交看 `code/message/msg`，轮询只看 `status/state/data`，于是
//
//	HTTP 200 + {"code":400,"msg":"...","data":null}
//
// 在提交时被正确识别为失败，在**轮询**时却被解析成「状态空、无产物、无错误」，
// 上层据此判定「任务还在跑」，一直轮询到任务超时（1h）才退款 —— 上游早已拒绝并计费，
// 用户等了 1 小时，而渠道健康度全程正常，监控也看不出任何异常。
//
// 一个 JSON 形状只能有一个解析处：字段集、成功码集合、文案优先级都在这里。
type duomiEnvelope struct {
	Code    int  `json:"-"`
	HasCode bool `json:"-"`

	// Status/State/Progress 是任务状态（不同子协议用不同字段名）。
	Status   string          `json:"status"`
	State    string          `json:"state"`
	Progress *int            `json:"progress"`
	ID       string          `json:"id"`
	Data     json.RawMessage `json:"data"`

	// 错误原因的统一提取（见 upstreamErrEnvelope）。
	err upstreamErrEnvelope
}

// duomiEnvelopeOf 解析响应体（解析失败返回零值：调用方按「什么都没有」处理）。
func duomiEnvelopeOf(buf []byte) duomiEnvelope {
	var raw struct {
		Code     json.RawMessage `json:"code"`
		Message  string          `json:"message"`
		Msg      string          `json:"msg"`
		Error    json.RawMessage `json:"error"`
		Data     json.RawMessage `json:"data"`
		Status   string          `json:"status"`
		State    string          `json:"state"`
		Progress *int            `json:"progress"`
		ID       string          `json:"id"`
		BaseResp *struct {
			StatusCode int    `json:"status_code"`
			StatusMsg  string `json:"status_msg"`
		} `json:"base_resp"`
	}
	var out duomiEnvelope
	if json.Unmarshal(buf, &raw) != nil {
		return out
	}
	out.Status, out.State, out.Progress = raw.Status, raw.State, raw.Progress
	out.ID, out.Data = raw.ID, raw.Data
	out.err = upstreamErrEnvelope{
		Message: raw.Message, Msg: raw.Msg, Error: raw.Error, Data: raw.Data, BaseResp: raw.BaseResp,
	}
	if code, ok := duomiIntCodeOK(raw.Code); ok {
		out.Code, out.HasCode = code, true
	}
	return out
}

// text 返回信封里的可读原因（取不到为空串）。
//
// 成功响应里也可能带 `msg:"success"`，因此**不要**用它判断成功与否 ——
// 判定只看 failure()；这里只回答「上游说了什么」。
func (e duomiEnvelope) text() string { return e.err.text() }

// failure 判定「这是失败响应」，并给出业务码与可读原因。
//
// 三条判据（任一命中）：
//  1. `error` 字段有内容；
//  2. 顶层 `code` 不在成功集合 {0, 200}；
//  3. `base_resp.status_code` 非 0（ArK 方言）。这一条不在这里判 —— 它属于
//     metaso 的错误信封，duomi 的响应没有 base_resp；留在这里只会制造假失败。
//
// **成功码时忽略顶层 msg**：大量上游在成功响应里写 `msg:"success"`，
// 若把它当错误原因，正常出图会被判成失败。
func (e duomiEnvelope) failure() (code int, msg string, failed bool) {
	if errTextFromValue(e.err.Error) != "" || duomiNonEmptyRaw(e.err.Error) {
		return 0, e.err.text(), true
	}
	if e.HasCode && e.Code != 0 && e.Code != 200 {
		return e.Code, e.err.text(), true
	}
	return 0, "", false
}

// duomiNonEmptyRaw 判断 RawMessage 是否「有内容」（null / 空串 / {} / [] 视为无）。
func duomiNonEmptyRaw(raw json.RawMessage) bool {
	switch strings.TrimSpace(string(raw)) {
	case "", "null", `""`, "{}", "[]":
		return false
	}
	return true
}

// failureError 把失败信封折成平台错误（提交链路用）。
//
// 文案兜底「上游返回业务错误」保留原文案，不改既有输出。
func (e duomiEnvelope) failureError() (error, bool) {
	code, msg, failed := e.failure()
	if !failed {
		return nil, false
	}
	if msg == "" {
		msg = "上游返回业务错误"
	}
	return duomiBusinessError(code, msg), true
}

// duomiEnvelopeOK 判断响应体是否是多米的 JSON 信封（而不是网关的 HTML 404 页）。
//
// 探活用它区分「路径对、任务不存在」与「路径写错了」：两者都可能是 404，
// 但只有前者说明渠道可用。
func duomiEnvelopeOK(buf []byte) bool {
	env := duomiEnvelopeOf(buf)
	if env.HasCode || env.ID != "" {
		return true
	}
	if len(env.Data) > 0 && strings.TrimSpace(string(env.Data)) != "null" {
		return true
	}
	// 无 code / id / data 但有顶层状态：同样是 JSON 信封（部分轮询接口只回 status）。
	return strings.TrimSpace(env.Status) != "" || strings.TrimSpace(env.State) != ""
}

// duomiEnvelopeError 识别「HTTP 2xx 但业务失败」的信封并映射成错误。
// 返回 ok=false 表示这是成功响应（签名保持不变，5 个提交点零改动）。
func duomiEnvelopeError(buf []byte) (error, bool) {
	return duomiEnvelopeOf(buf).failureError()
}
