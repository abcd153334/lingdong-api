package channel

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestPruneLegacyDuomiKeys 锁住迁移的边界：只删那一个键，其余字节语义不变。
//
// 这段逻辑一旦多删或少删，后果是「渠道 12 个模型全 503」或「清理后仍不合法」，
// 两种都不会在迁移当时报错，只在第一个真实请求时暴露。
func TestPruneLegacyDuomiKeys(t *testing.T) {
	t.Run("删掉 fixed_tier 且保留其它键", func(t *testing.T) {
		in := `{"model_overrides":{"kling-v3":{"protocol":"kling","mode":"std","fixed_tier":"720p","upstream_model":"kling-v3"},` +
			`"chirp-hawk":{"protocol":"suno.music"}},"metadata_allow":["aigc_watermark"],"auth_prefix":"Token "}`
		out, changed, err := pruneLegacyDuomiKeys([]byte(in))
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if !changed {
			t.Fatal("含 fixed_tier 时必须报告 changed")
		}
		if strings.Contains(string(out), legacyDuomiOptionKey) {
			t.Fatalf("输出仍含 %s: %s", legacyDuomiOptionKey, out)
		}
		var doc struct {
			ModelOverrides map[string]map[string]any `json:"model_overrides"`
			MetadataAllow  []string                  `json:"metadata_allow"`
			AuthPrefix     string                    `json:"auth_prefix"`
		}
		if err := json.Unmarshal(out, &doc); err != nil {
			t.Fatalf("输出不是合法 JSON: %v", err)
		}
		if got := doc.ModelOverrides["kling-v3"]["mode"]; got != "std" {
			t.Fatalf("mode 被改坏: %v", got)
		}
		if got := doc.ModelOverrides["kling-v3"]["upstream_model"]; got != "kling-v3" {
			t.Fatalf("upstream_model 被改坏: %v", got)
		}
		if _, ok := doc.ModelOverrides["chirp-hawk"]; !ok {
			t.Fatal("无关的覆盖项被删掉了")
		}
		if len(doc.MetadataAllow) != 1 || doc.MetadataAllow[0] != "aigc_watermark" {
			t.Fatalf("metadata_allow 被改坏: %v", doc.MetadataAllow)
		}
		if doc.AuthPrefix != "Token " {
			t.Fatalf("auth_prefix 被改坏: %q", doc.AuthPrefix)
		}
	})

	t.Run("没有该键时字节完全不变", func(t *testing.T) {
		in := `{"model_overrides":{"kling-v3":{"protocol":"kling","mode":"std"}}}`
		out, changed, err := pruneLegacyDuomiKeys([]byte(in))
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if changed {
			t.Fatal("没有 fixed_tier 时不应报告 changed（否则每次启动都会重写全部渠道）")
		}
		if string(out) != in {
			t.Fatalf("无变化时不应改写字节:\n in=%s\nout=%s", in, out)
		}
	})

	t.Run("没有 model_overrides 时原样返回", func(t *testing.T) {
		in := `{"metadata_allow":["watermark"]}`
		out, changed, err := pruneLegacyDuomiKeys([]byte(in))
		if err != nil || changed || string(out) != in {
			t.Fatalf("out=%s changed=%v err=%v", out, changed, err)
		}
	})

	t.Run("非法 JSON 必须报错而不是静默跳过", func(t *testing.T) {
		if _, _, err := pruneLegacyDuomiKeys([]byte(`{"model_overrides":`)); err == nil {
			t.Fatal("非法 JSON 应返回错误（调用方据此记为 failed，交人工处理）")
		}
	})
}
