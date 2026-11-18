package relay

import "encoding/json"

// judgeErrorEnvelope 判定「HTTP 2xx，但响应体其实是错误」。
//
// 为什么需要它：上游（尤其是聚合/兼容层）经常用 **200 + {"error":{...}}** 表达失败 ——
// 视觉模型取图失败、内容审核拦截、上游内部异常都是这个形状。改造前这里只对图片端点
// 做了语义判定（image_verdict.go），chat / embeddings / rerank 一律把这种响应当成功：
//   - 照常结算扣费（用户什么都没拿到却付了钱）；
//   - 把上游错误体原样写给客户端（所以能看到 `param:null` 这种我们从不产出的字段）。
//
// 判据必须**窄**，否则会把正常响应误判成失败（用户白跑一次）：
//
//	顶层有 error，**并且**响应里不存在任何一种成功形状。
//
// “成功形状”按端点族列全（choices / data / output / results / object=response）。
// 只要有一个非空就算成功 —— 「部分成功 + 顶层 error」（例如多图里坏了一张）
// 属于交付事实，由 anomaly 表达，不能整单当失败。
func judgeErrorEnvelope(body []byte) (message string, failed bool) {
	if len(body) == 0 {
		return "", false
	}
	var probe struct {
		Error   json.RawMessage `json:"error"`
		Choices json.RawMessage `json:"choices"`
		Data    json.RawMessage `json:"data"`
		Output  json.RawMessage `json:"output"`
		Results json.RawMessage `json:"results"`
		Object  string          `json:"object"`
	}
	if json.Unmarshal(body, &probe) != nil {
		// 解析不了说明不是我们认识的 JSON 信封：交给后续流程按原逻辑处理，
		// 绝不在这里判失败（畸形响应判失败 = 白送一次上游调用）。
		return "", false
	}
	if isEmptyJSON(probe.Error) {
		return "", false
	}
	// 成功形状优先：任一非空即视为有交付。
	if !isEmptyJSON(probe.Choices) || !isEmptyJSON(probe.Data) ||
		!isEmptyJSON(probe.Output) || !isEmptyJSON(probe.Results) ||
		probe.Object == "response" {
		return "", false
	}
	return errorText(probe.Error), true
}

// isEmptyJSON 判断一个 RawMessage 是否等价于“没有内容”（缺失 / null / 空数组 / 空对象）。
func isEmptyJSON(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	switch string(raw) {
	case "null", "{}", "[]", `""`:
		return true
	}
	return false
}

// errorText 从 error 字段取可读文案：兼容对象 {"message","code"} 与字符串两种形态。
func errorText(raw json.RawMessage) string {
	var obj struct {
		Message string `json:"message"`
		Code    string `json:"code"`
		Type    string `json:"type"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Message != "" {
		prefix := obj.Code
		if prefix == "" {
			prefix = obj.Type
		}
		if prefix == "" {
			return obj.Message
		}
		return prefix + ": " + obj.Message
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return s
	}
	return "upstream error"
}
