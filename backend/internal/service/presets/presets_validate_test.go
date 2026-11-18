package presets

import (
	"testing"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/model"
	"lingdong-api/internal/service/modelcfg"
)

// 内置预设的价目/能力必须能过管理台的**同一条**校验路径（`modelcfg.Build`）。
//
// 为什么这条必须存在：预设层直接 `Create` 落库，**绕开了 Build 的校验**。
// 「占位价目结构不合法」于是在写入期完全无症状 —— 等运营在管理台点开这个模型
// 保存或点启用时才 400/500，那时预设代码已经上线很久了，而现场看起来
// 像是"管理台坏了"。
//
// 这条同时锁住四件事：档位键在白名单内（`4k` 而不是 `4K`）、`max_n` 与
// `per_image` 匹配、`allow_auto_duration` 只出现在视频模态（图片/音乐带它会被
// `rejectForeign` 拒）、端点与模态自洽。
//
// 遍历所有预设组：新加一组自动纳入，不需要再复制一份用例。
func TestPresetSpecPassesModelBuild(t *testing.T) {
	for _, g := range presetGroups() {
		for _, s := range g.Models {
			in := modelcfg.Input{
				ID: s.ID, DisplayName: spPreset(s.DisplayName), Vendor: g.Vendor.ID, Modality: s.Modality,
				BillingMode: model.BillingModeSpec, BillingModeSet: true,
				Endpoints: s.Endpoints, EndpointsSet: true,
				Capabilities: apidoc.CapKeys(s.Caps), CapabilitiesSet: true,
				SpecPrices: s.Spec, SpecPricesSet: true,
			}
			if _, err := modelcfg.Build(in); err != nil {
				t.Fatalf("%s/%s 的预设过不了管理台校验：%v", g.MarkerKey, s.ID, err)
			}
		}
	}
}

// 能力声明必须过管理台保存时的同一条校验（词表封闭 + 该模态允许）。
//
// 漏了的症状是"文档里少一个 TAB"或"某个参数传了 400"，全程无报错：
// `CapKeys` 会把认不出的词原样写进 `models.capabilities`。
func TestPresetCapabilitiesAreInVocabulary(t *testing.T) {
	for _, g := range presetGroups() {
		for _, s := range g.Models {
			if err := apidoc.ValidateCapabilityKeys(s.Modality, apidoc.CapKeys(s.Caps)); err != nil {
				t.Fatalf("%s/%s 的能力声明不合法：%v", g.MarkerKey, s.ID, err)
			}
			// 端点 × 能力也要自洽：声明了 i2v 却没有 video/generations = 文档里永远不出现。
			if err := apidoc.ValidateCapabilitiesAgainstEndpoints(s.Caps, s.Endpoints); err != nil {
				t.Fatalf("%s/%s 的能力与端点不自洽：%v", g.MarkerKey, s.ID, err)
			}
		}
	}
}

func spPreset(s string) *string { return &s }
