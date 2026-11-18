package geminiconv

import (
	"encoding/json"
	"strings"

	"lingdong-api/internal/relay/conv"
)

// 本文件是向量化端点的映射：Gemini embedContent ↔ OpenAI embeddings。
//
// 为什么单独一个文件而不是混进 chat 的映射：两者的请求/响应形状完全不同
// （`content:{parts:[{text}]}` ↔ `input`、`{embedding:{values}}` ↔ `{data:[{embedding}]}`），
// 混在一起的后果是"生成走通了、向量化 200 但拿不到数据"。

// embedRequest 是 Gemini 的 embedContent 请求体。
type embedRequest struct {
	Model                string         `json:"model"`
	Content              *geminiContent `json:"content"`
	OutputDimensionality *int64         `json:"outputDimensionality"`
	TaskType             string         `json:"taskType"`
	Title                string         `json:"title"`
}

// embedToUpstream 把 Gemini embedContent 请求转成 OpenAI embeddings 请求。
//
// 兼容一条遗留形态：`/v1/engines/{model}/embeddings` 在 OpenAI 老客户端里发的是
// **OpenAI 形状**（`{input:"hi"}`）。它被登记成同一端点的别名，所以这里按
// "有没有 input 键"判定：有就原样透传（只补 model），没有才按 Gemini 映射。
// 不做这个兼容，那个别名就是个 400 生成器。
func (c Codec) embedToUpstream(body []byte) ([]byte, error) {
	var probe struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, conv.BadRequest("gemini_invalid_json", "请求体不是合法 JSON：%s", err.Error())
	}
	if len(probe.Input) > 0 && string(probe.Input) != "null" {
		return passthroughWithModel(body, c.Model)
	}

	var req embedRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, conv.BadRequest("gemini_invalid_json", "请求体不是合法 JSON：%s", err.Error())
	}
	model := strings.TrimSpace(c.Model)
	if model == "" {
		model = strings.TrimPrefix(strings.TrimSpace(req.Model), "models/")
	}
	if model == "" {
		return nil, conv.BadRequest("gemini_model_required",
			"缺少 model：请在路径里写 /v1beta/models/<model>:embedContent")
	}
	if req.Content == nil {
		return nil, conv.BadRequest("gemini_content_required", "缺少 content：向量化必须给要编码的内容")
	}
	var texts []string
	for _, p := range req.Content.Parts {
		if p.Text == "" {
			// 图片/文件向量化在 OpenAI 兼容上游没有对应入参，硬塞会让上游报一个
			// 与真实原因无关的错（"input 类型不支持"）。
			return nil, conv.BadRequest("gemini_embed_non_text",
				"向量化只支持文本 parts：图片/文件向量请改用支持该能力的模型")
		}
		texts = append(texts, p.Text)
	}
	if len(texts) == 0 {
		return nil, conv.BadRequest("gemini_content_required", "content.parts 不能为空")
	}
	out := map[string]any{"model": model}
	// 单条时给字符串（多数上游对单条字符串的支持最好），多条给数组。
	if len(texts) == 1 {
		out["input"] = texts[0]
	} else {
		out["input"] = texts
	}
	if req.OutputDimensionality != nil && *req.OutputDimensionality > 0 {
		out["dimensions"] = *req.OutputDimensionality
	}
	// taskType / title 是 Gemini 特有的检索任务提示，OpenAI 兼容上游没有对应字段。
	// 属 B 类（只影响检索偏好、不改变输出结构）：接受并忽略，文档写明。
	return json.Marshal(out)
}

// passthroughWithModel 给 OpenAI 形状的请求补上 model（路径是权威来源）。
func passthroughWithModel(body []byte, model string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, conv.BadRequest("gemini_invalid_json", "请求体不是合法 JSON：%s", err.Error())
	}
	if strings.TrimSpace(model) != "" {
		mb, _ := json.Marshal(strings.TrimSpace(model))
		m["model"] = mb
	}
	if _, ok := m["model"]; !ok {
		return nil, conv.BadRequest("gemini_model_required",
			"缺少 model：请在路径里写 /v1beta/models/<model>:embedContent")
	}
	return json.Marshal(m)
}

// embedToClient 把 OpenAI embeddings 响应转成 Gemini 的 embedContent 响应。
//
// 单条 → `{embedding:{values:[…]}}`；多条 → `{embeddings:[{values:[…]}]}`。
// 这不是"多给一个字段"，而是协议规定：Google SDK 读的是 embedding.values，
// 给它 OpenAI 的 data[].embedding 会解析成空向量（200 但拿不到数据）。
func (Codec) embedToClient(body []byte, model string) ([]byte, *conv.Error) {
	var in struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Usage *struct {
			PromptTokens int64 `json:"prompt_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, conv.BadUpstream("gemini_embed_response_invalid", "服务响应不是合法 JSON：%s", err.Error())
	}
	if len(in.Data) == 0 {
		return nil, conv.BadUpstream("gemini_embed_empty", "没有返回任何向量，本次不计费")
	}
	// Gemini 的 usageMetadata 用 tokenCount 单字段（没有 prompt/completion 之分）。
	var usageMeta any
	if in.Usage != nil {
		usageMeta = map[string]any{"tokenCount": in.Usage.PromptTokens}
	}
	var out map[string]any
	if len(in.Data) == 1 {
		out = map[string]any{"embedding": map[string]any{"values": in.Data[0].Embedding}}
	} else {
		items := make([]map[string]any, 0, len(in.Data))
		for _, d := range in.Data {
			items = append(items, map[string]any{"values": d.Embedding})
		}
		out = map[string]any{"embeddings": items}
	}
	if usageMeta != nil {
		out["usageMetadata"] = usageMeta
	}
	return mustMarshal(out)
}
