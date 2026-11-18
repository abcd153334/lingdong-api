package adapters

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// 上游错误文案的**唯一**提取实现。
//
// 为什么必须收敛：改造前有四份互不相同的实现（duomi 两份、metaso 一份、autodl/ark 各一份），
// 每份支持的字段都不一样 —— 多米把原因写在 `msg`、metaso 写在 `message`/`code`、
// 火山写在 `error.message`。结果同一种故障在 A 渠道能显示上游原话、在 B 渠道
// 只剩「上游返回业务错误」，用户和运维都无从下手；而新增一个上游时，
// 复制哪一份都是随机的，漏读字段也不会报错，只会静默退化。
//
// 识别顺序（固定）：
//
//	message > msg > error.* > data.message/msg > base_resp.status_msg
//
// `message` 先于 `msg` 是刻意的：多米可灵用 `message` 表达成功/失败的原因，
// 而 `msg` 在部分子协议里是调试字段；把 `msg` 提前会让成功响应里的 `msg:"success"`
// 变成错误说明（见 duomiEnvelope.failure 的同类约束）。
type upstreamErrEnvelope struct {
	Message  string          `json:"message"`
	Msg      string          `json:"msg"`
	Error    json.RawMessage `json:"error"`
	Data     json.RawMessage `json:"data"`
	BaseResp *struct {
		StatusCode int    `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	} `json:"base_resp"`
}

// readUpstreamErrEnvelope 解析任意上游错误体（解析失败返回零值，text() 为空）。
func readUpstreamErrEnvelope(buf []byte) upstreamErrEnvelope {
	var e upstreamErrEnvelope
	if len(buf) == 0 {
		return e
	}
	_ = json.Unmarshal(buf, &e)
	return e
}

// text 返回「给人看的那一句」，取不到返回空串（由调用方决定兜底文案）。
func (e upstreamErrEnvelope) text() string {
	if v := firstNonEmptyStr(strings.TrimSpace(e.Message), strings.TrimSpace(e.Msg)); v != "" {
		return v
	}
	if v := errTextFromValue(e.Error); v != "" {
		return v
	}
	var d struct {
		Message string `json:"message"`
		Msg     string `json:"msg"`
	}
	if len(e.Data) > 0 {
		_ = json.Unmarshal(e.Data, &d)
	}
	if v := firstNonEmptyStr(strings.TrimSpace(d.Message), strings.TrimSpace(d.Msg)); v != "" {
		return v
	}
	if e.BaseResp != nil {
		return strings.TrimSpace(e.BaseResp.StatusMsg)
	}
	return ""
}

// envelopeTextOf 是「整段响应体 → 可读原因」的入口。
//
// 解析不出任何可读字段时**返回空串**而不是原文：把整段 JSON 拼进 400 响应体，
// 用户看到的是 `{"code":400,...}`，既读不懂也容易被当成平台故障。
// 需要原文排障的场景由 call_io 的 upstream_raw 承担（那是完整报文）。
func envelopeTextOf(buf []byte) string {
	return readUpstreamErrEnvelope(buf).text()
}

// upstreamDetail 是各适配器 `*HTTPError` 的统一取因入口。
//
// 优先级：错误信封里的那一句 → 原文片段（HTML 错误页等非 JSON 响应只能这样）→
// 明确告知「没给原因」。最后一档很重要：把空串拼进文案会得到
// 「上游返回 502：」这种没有任何信息的句子，运维无法区分「上游没说」与「我们没取」。
//
// JSON 但取不到任何可读字段时**不贴原文**：`{"code":400,"data":null}` 对用户
// 毫无意义，原文排障看 call_io 的 upstream_raw。
func upstreamDetail(buf []byte, status int) string {
	if t := envelopeTextOf(buf); t != "" {
		return t
	}
	trimmed := bytes.TrimSpace(buf)
	if len(trimmed) > 0 && trimmed[0] != '{' && trimmed[0] != '[' {
		if s := snippet(trimmed); s != "" {
			return s
		}
	}
	return "上游未给出原因（HTTP " + strconv.Itoa(status) + "）"
}

// errTextFromValue 从「可能是字符串 / 对象 / 数组」的字段值里取出文案。
//
// 三态都必须支持：同一家上游在不同版本里把 `error` 写成 `"msg"`、
// `{code,message}` 或 `[{...},{...}]`。只支持一种的后果是形状一变
// 整段原因就消失（而不是解析报错），排障时表现为「昨天还能看到原因」。
func errTextFromValue(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" || s == `""` || s == "{}" || s == "[]" {
		return ""
	}
	// 字符串
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return strings.TrimSpace(str)
	}
	// 对象：message > msg > code（code 仅作最后兜底，见文件头）
	var obj struct {
		Message string          `json:"message"`
		Msg     string          `json:"msg"`
		Code    json.RawMessage `json:"code"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		if v := firstNonEmptyStr(strings.TrimSpace(obj.Message), strings.TrimSpace(obj.Msg)); v != "" {
			return v
		}
		// code 可能是数字（HTTP 同义码）也可能是可读字符串：只有可读字符串能当文案，
		// 数字回 "400" 对用户毫无意义。
		if c := strings.TrimSpace(string(obj.Code)); c != "" && c != "null" {
			var codeStr string
			if json.Unmarshal(obj.Code, &codeStr) == nil && strings.TrimSpace(codeStr) != "" {
				return strings.TrimSpace(codeStr)
			}
		}
		return ""
	}
	// 数组：逐项取文案后拼接（校验类错误常见 [{field,message},...]）
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		parts := make([]string, 0, len(arr))
		for _, it := range arr {
			if m := errTextFromValue(it); m != "" {
				parts = append(parts, m)
			}
		}
		return strings.Join(parts, "; ")
	}
	return ""
}
