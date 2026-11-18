package controller

import (
	"encoding/json"
	"strings"
	"testing"

	"lingdong-api/internal/model"
	"lingdong-api/internal/service/pricing"
)

// 本文件守 /v1/models 的 tags 对外契约：有则原样、无则空数组（绝不是 null）。

func TestGatewayModelCarriesTags(t *testing.T) {
	it := &pricing.ModelItem{ID: "m", Modality: "chat",
		Tags: []model.ModelTagView{{Label: "限时免费", ColorHex: "#E11D48"}}}

	m := gatewayModelOf(it, "https://api.example.com")
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	if !strings.Contains(string(raw), `"tags":[{"label":"限时免费","color_hex":"#E11D48"}]`) {
		t.Fatalf("响应体缺 tags 或形状不对：%s", raw)
	}
}

func TestGatewayModelTagsNeverNull(t *testing.T) {
	// 客户端会把 tags 当数组用（x.tags.length）：null 会直接抛错，键消失更糟。
	m := gatewayModelOf(&pricing.ModelItem{ID: "m", Modality: "chat"}, "")
	raw, _ := json.Marshal(m)
	if !strings.Contains(string(raw), `"tags":[]`) {
		t.Fatalf("无标签时应下发 []，实际 %s", raw)
	}
}
