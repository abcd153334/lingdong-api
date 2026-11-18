package controller

import (
	"encoding/json"
	"strings"
	"testing"

	"lingdong-api/internal/service/pricing"
)

// TestGatewayModelCarriesIconURL 守住 /v1/models 的图标字段。
//
// 为什么这一条值得单独测：`/v1/models` 是**第三方第一眼看到的接口**，
// 而它在改造前连字段都没有（快照里 `ModelItem.Icon` 被整条丢掉）。
// 图标在这里必须是**绝对地址**：调用方拿到的是 JSON 而不是本站页面，
// 根相对路径 `/api/icons/...` 在跨域与原生客户端里一律 404。
func TestGatewayModelCarriesIconURL(t *testing.T) {
	it := &pricing.ModelItem{ID: "gpt-4o", DisplayName: "GPT-4o", Vendor: "openai",
		Icon: "builtin:openai-gpt", Modality: "chat"}

	m := gatewayModelOf(it, "https://api.example.com")
	if !strings.HasPrefix(m.IconURL, "https://api.example.com/api/icons/b/openai-gpt?v=") {
		t.Fatalf("icon_url 应为绝对地址且带版本参数，实际 %q", m.IconURL)
	}

	// 序列化后键必须真的出现（字段名与契约文档逐字一致）
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	if !strings.Contains(string(raw), `"icon_url":"https://api.example.com/api/icons/b/openai-gpt?v=`) {
		t.Fatalf("响应体缺 icon_url：%s", raw)
	}
}

// TestGatewayModelWithoutIconOmitsField 没配图标时**不出现**该键。
//
// 与契约文档一致（`omitempty`）：空串下发会让客户端画出空白框，
// 而「键不存在」与「值为空」在第三方 SDK 里往往是两种分支（undefined vs ""）。
func TestGatewayModelWithoutIconOmitsField(t *testing.T) {
	m := gatewayModelOf(&pricing.ModelItem{ID: "m", Modality: "chat"}, "https://api.example.com")
	if m.IconURL != "" {
		t.Fatalf("没有图标时 IconURL 应为空，实际 %q", m.IconURL)
	}
	raw, _ := json.Marshal(m)
	if strings.Contains(string(raw), "icon_url") {
		t.Fatalf("没有图标时不应下发该键：%s", raw)
	}
}
