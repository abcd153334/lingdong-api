package relay

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"

	"lingdong-api/internal/pkg/mediaurl"
)

// 本文件是**同步响应交付前的唯一形状改写点**。
//
// 三件事曾经散在三个地方（media_rewrite.go 的直链改写、relay.go 的 codec 转码、
// 无处的 id/model 原样透出）：
//  1. 图像响应里的 `data[].url` 是上游 CDN 直链 —— 会泄漏供货方域名、受防盗链影响；
//  2. `model` 不回填时，客户端看到的是渠道绑定的上游模型名，与它请求的模型对不上；
//  3. `id` 原样透出上游单号，等于把对方系统里的请求号交给终端用户。
//
// 三件事都要在**采集之后、写出之前**做：call_io 与调用记录要留上游原文
// （排障时唯一能去对方后台对单的线索），而用户拿到的必须是平台形状。

// shapeRule 描述某端点要归一哪些键。
//
// 只列「响应里真的会出现这些键」的端点：`/v1/responses` 用的是 `created_at`，
// 给它补 `created` 就是伪造一个规范里没有的字段。
type shapeRule struct {
	// idPrefix 是响应 id 的前缀（空 = 不改 id）。
	idPrefix string
	// appendModel 表示 model 缺失时补上（OpenAI 规范里 model 是必填的那几类）。
	appendModel bool
}

var shapeRules = map[endpointKind]shapeRule{
	kindChat:        {idPrefix: "chatcmpl-", appendModel: true},
	kindCompletions: {idPrefix: "cmpl-", appendModel: true},
	kindEmbeddings:  {idPrefix: "embd-", appendModel: true},
	kindResponses:   {idPrefix: "resp_", appendModel: true},
	// compact 的响应同样是 responses 形状（id 前缀 resp_、model 必填）：
	// 不登记的话上游单号与上游模型名会原样透给终端用户。
	kindResponsesCompact: {idPrefix: "resp_", appendModel: true},
	// messages 不在表里：Claude 编解码器自己产出 id/model（claudeconv 的 msg_ 前缀），
	// 这里再改一次就是两套 id 互相覆盖。
}

// shapeResponse 返回改写后的报文；nil 表示「不需要改」（调用方沿用原体）。
//
// 解析失败一律原样交付：把一次成功的响应因为"我们改不动"变成 500，
// 是比上游模型名泄漏严重得多的故障。
func shapeResponse(kind endpointKind, modelID, respID, base string, body []byte) []byte {
	if len(body) == 0 {
		return nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		return nil
	}
	changed := false

	if rule, ok := shapeRules[kind]; ok {
		if rule.idPrefix != "" && respID != "" {
			if _, exists := top["id"]; exists {
				if enc, err := marshalNoEsc(rule.idPrefix + respID); err == nil {
					top["id"] = enc
					changed = true
				}
			}
		}
		if modelID != "" {
			_, exists := top["model"]
			if exists || rule.appendModel {
				if enc, err := marshalNoEsc(modelID); err == nil {
					top["model"] = enc
					changed = true
				}
			}
		}
	}

	// 图像三兄弟的 data[].url：与 id/model 共用同一次解析。
	if returnsImageURLs(kind) && base != "" {
		if n := mediaurl.RewriteDataURLs(top, base, mediaurl.TTL()); n > 0 {
			changed = true
		}
	}

	if !changed {
		return nil
	}
	out, err := marshalNoEsc(top)
	if err != nil {
		return nil
	}
	return out
}

// shapeFrame 对**单帧** SSE data 做同样的归一（只动已存在的 id/model）。
//
// 只服务 chat / completions / responses：图像帧的 url 改写另有出口
// （rewriteImageFrameURL），Claude 帧由编码器生成，都不该走到这里。
func shapeFrame(kind endpointKind, modelID, respID string, data []byte) []byte {
	rule, ok := shapeRules[kind]
	if !ok || len(data) == 0 {
		return data
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil || top == nil {
		return data
	}
	changed := false
	if rule.idPrefix != "" && respID != "" {
		if _, exists := top["id"]; exists {
			if enc, err := marshalNoEsc(rule.idPrefix + respID); err == nil {
				top["id"] = enc
				changed = true
			}
		}
	}
	if modelID != "" {
		if _, exists := top["model"]; exists {
			if enc, err := marshalNoEsc(modelID); err == nil {
				top["model"] = enc
				changed = true
			}
		}
	}
	if !changed {
		return data
	}
	out, err := marshalNoEsc(top)
	if err != nil {
		return data
	}
	return out
}

// shapeRespID 生成一次响应的对外 id 主体（12 字节随机 → 24 位十六进制）。
//
// 为什么不用 usages.id 派生：那会把全站调用量编码进每一个响应 id
// （相邻号差值 = 平台这段时间的接单量），属于业务数据泄漏。
// 随机串没有这个问题，而且同一个请求的所有帧共用它，客户端看到的是同一个 id。
func shapeRespID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 熵源不可用（极罕见）：退化成时间戳，宁可 id 不随机，也不要空 id。
		return hex.EncodeToString([]byte{byte(len(b))})
	}
	return hex.EncodeToString(b[:])
}

// marshalNoEsc 序列化并关闭 HTML 转义（URL 里的 & 不该变成 \u0026）。
func marshalNoEsc(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}
