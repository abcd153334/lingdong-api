package adapters

import (
	"bytes"
	"encoding/json"
	"strings"

	"lingdong-api/internal/relay"
	"lingdong-api/internal/service/billing"
)

// isImageForwardPath 判断这条转发是否走图片端点（generations / edits / variations）。
func isImageForwardPath(path string) bool {
	switch path {
	case relay.PathImagesGenerations, relay.PathImagesEdits, relay.PathImagesVariations:
		return true
	}
	return false
}

// OpenAI 兼容渠道的图片尺寸处理。
//
// 设计前提（与网关一致）：**模型支持什么尺寸由模型与官方文档决定**，
// 适配器不做取值判定、不做「档位 + 画幅 → 像素」的换算 —— 那会改写用户原话
// （要 2048x1152 却被改写成 1536x1024），且每接一个新模型就要补一张枚举表。
//
// 这里只做**协议翻译**：平台对外暴露的 `aspect_ratio` / `ratio` 两个别名，
// OpenAI 的图片接口没有这两个字段，留着就是未知键 —— 删掉即可。
// `size` 一律原样下发：用户写 `1024x1024` 就发 `1024x1024`，写 `1K` 就发 `1K`，
// 上游认不认由它自己回错（那是唯一准确的原话）。

// openAIImageSize 只清理平台别名；`size` 的取值来自**计费侧的同一份解析结果**。
func openAIImageSize(body []byte, _ string, im *billing.ImageSize) ([]byte, error) {
	return stripPlatformSizeAliases(body, im)
}

// hasJSONValue 判断一个原始 JSON 值是否「有内容」（缺失 / null / "" 算没有）。
func hasJSONValue(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && string(t) != "null" && string(t) != `""`
}

// stripPlatformSizeAliases 删除平台自有、OpenAI 不认识的四个字段，并把胜出的尺寸写回 size。
//
// 为什么必须用 `im.Raw` 而不是在这里再挑一遍 resolution/image_size：
// 计费侧 `billing.ParseImageSize` 按 size → image_size → resolution 取**第一个具体意图**，
// 这里若按 resolution 优先，就会在「resolution=1K + image_size=2K」时变成
// 「按 2K 收钱、按 1K 出图」。`ImageSize.Raw` 就是那次解析的胜出原文，一个真相源。
//
// size 已经等于胜出值时**逐字节保留**（用户原话优先，也避免无意义的重新序列化）。
func stripPlatformSizeAliases(body []byte, im *billing.ImageSize) ([]byte, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return body, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(body), &m); err != nil || m == nil {
		return body, nil
	}
	changed := false
	want := ""
	if im != nil {
		want = strings.TrimSpace(im.Raw)
	}
	if want != "" {
		cur := ""
		if raw, ok := m["size"]; ok && hasJSONValue(raw) {
			_ = json.Unmarshal(raw, &cur)
			cur = strings.TrimSpace(cur)
		}
		if cur != want {
			m["size"], _ = json.Marshal(want)
			changed = true
		}
	}
	for _, k := range []string{"resolution", "image_size", "aspect_ratio", "ratio"} {
		if _, ok := m[k]; ok {
			delete(m, k)
			changed = true
		}
	}
	if !changed {
		return body, nil
	}
	return json.Marshal(m)
}

// openAIImageFormFields 计算 multipart 端点要写回的表单字段（空值 = 删除该 part）。
//
// 与 JSON 侧（stripPlatformSizeAliases）同口径，**包括「别名折成 size」这一步**：
// 只删别名不折叠，结果是「按 image_size=2K 计费、按上游默认尺寸出图」——
// 同一份参数在 JSON 端点生效、在 multipart 端点静默失效，账单上看不出来。
//
// 表单里已有 size 时，这里返回的值会**替换**它；值来自同一个解析结果（同一份原文），
// 因此不会改写用户意图。
func openAIImageFormFields(_ string, im *billing.ImageSize) (map[string]string, error) {
	fields := map[string]string{
		"resolution":   "",
		"image_size":   "",
		"aspect_ratio": "",
		"ratio":        "",
	}
	if im != nil {
		if v := strings.TrimSpace(im.Raw); v != "" {
			fields["size"] = v
		}
	}
	return fields, nil
}
