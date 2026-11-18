package relay

// 同步图片响应的**语义判定**（唯一处）。
//
// 为什么必须有这一层：
//
//	上游对「整单没出图」的表示是 **HTTP 200 + 顶层 error**（官方《图片生成 API》：
//	顶层 error = 整个请求未产出任何图片）。而平台侧的计费兜底是「0 张也按 1 张收」
//	（billing.ResolveImageCount → clampImageCount，理由见该函数注释：上游真跑过，
//	不能让整单白送）。两条规则各自都对，叠在一起就变成：
//	  上游明确失败（审核拦截 / 服务异常）→ 用户一张图没拿到、却被收 1 张的钱。
//
//	所以必须先把「明确的失败」和「我们看不懂」分开：
//	  - 有 error 依据 → 上游失败，0 张结算 + 全额释放；
//	  - 没有任何 error 依据 → 疑「我们的解析漏了」，维持 1 张 + 告警（不静默免单）。
//
// 判据顺序铁律：**Count > 0 一律优先**。上游偶尔会在部分失败时也带顶层 error，
// 若先看 error 就会把已经交付的图判成失败、白送一次。

import (
	"encoding/json"
	"strings"
)

// imageVerdict 是一次同步图片响应的语义结论。
type imageVerdict int

const (
	// imageOK：至少交付了 1 张（可能同时带顶层 error，见 PartialError）。
	imageOK imageVerdict = iota
	// imageNoImage：0 张，且响应里没有任何 error 依据 → 疑「我们没解析出来」。
	imageNoImage
	// imageUpstreamRejected：0 张 + 上游明确拒绝（内容审核/违规）。
	imageUpstreamRejected
	// imageUpstreamFailed：0 张 + 上游明确失败（服务异常等非调用方原因）。
	imageUpstreamFailed
)

// imageOutcome 是判定结果。
type imageOutcome struct {
	Verdict imageVerdict
	// Count 是**真正交付**的图片数（url 或 b64_json 非空）。
	Count int64
	// Code / Message 是顶层 error 的原文（没有则为空）。
	Code    string
	Message string
	// PartialError 表示「已交付 ≥1 张，但顶层同时带 error」——上游契约违约，
	// 交付照常、金额照常，只需留痕。
	PartialError bool
	// PerImageFailed 表示 0 张，但 data[] 里每项都带 error（组图全失败，
	// 该字段只有部分模型会返回）。
	PerImageFailed bool
}

// UserMessage 是给调用方看的那句话（**不透传上游原文**）。
//
// 上游原文里可能带内部 code / 参考图 URL / 审核细则，直接回显既无助于用户改参数，
// 又会把平台内部链路暴露出去。原文照旧落 resp_err 与 usages.error_msg 供运营查。
func (o imageOutcome) UserMessage() string {
	switch o.Verdict {
	case imageUpstreamRejected:
		return "内容未通过上游审核，本次未扣费"
	default:
		return "上游未能生成任何图片（已全额释放），请稍后重试"
	}
}

// judgeImageResponse 解析一次响应体得出语义结论。
//
// 只读，不改字节：2xx 的响应体仍按原样透传给客户端（那是「1:1 转发」的定义）。
func judgeImageResponse(body []byte) imageOutcome {
	var p struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Data []struct {
			URL     string `json:"url"`
			B64JSON string `json:"b64_json"`
			Error   *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"data"`
	}
	out := imageOutcome{Verdict: imageNoImage}
	if json.Unmarshal(body, &p) != nil {
		// 解析不了 = 我们的问题，不是上游的失败：按「没图」处理（维持 1 张 + 告警），
		// 绝不能在解析失败时判上游失败 —— 那会让一段畸形响应白送一次生成。
		return out
	}
	var perImageFailed int64
	for _, d := range p.Data {
		if d.URL != "" || d.B64JSON != "" {
			out.Count++
			continue
		}
		if d.Error != nil && (d.Error.Code != "" || d.Error.Message != "") {
			perImageFailed++
		}
	}
	if p.Error != nil {
		out.Code = strings.TrimSpace(p.Error.Code)
		out.Message = strings.TrimSpace(p.Error.Message)
	}
	if out.Count > 0 {
		out.Verdict = imageOK
		out.PartialError = out.Code != "" || out.Message != ""
		return out
	}
	if out.Code != "" || out.Message != "" {
		// 注意：这里用 code+message 一起判审核，与任务链路共用同一份词表。
		if IsModerationRejection(out.Code + " " + out.Message) {
			out.Verdict = imageUpstreamRejected
		} else {
			out.Verdict = imageUpstreamFailed
		}
		return out
	}
	out.Verdict = imageNoImage
	out.PerImageFailed = perImageFailed > 0
	return out
}
