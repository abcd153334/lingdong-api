package modelcfg

import (
	"reflect"
	"testing"

	"lingdong-api/internal/model"
)

// 本文件守住 2026-10-29 审计 A-01：
//
//	PUT /api/admin/models/{id} 带 `{}` → 200，display_name 从 "Qwen3 Reranker" 变空、
//	version +1，无任何日志。根因是 Input 用字符串零值表达"未提交"，而 Updates 是全列写。
//
// 修法：DisplayName / Description 改指针，nil = 未提交（FillMissing 用库内值补），
// 非 nil = 覆盖（"" 表示显式清空）。下面两条用例分别守"不丢"与"能清"。

func TestUpdateKeepsNameAndDescriptionWhenOmitted(t *testing.T) {
	gdb := withCfgDB(t)
	seedVendorChannel(t, gdb)

	name, desc := "原始名称", "原始描述"
	cw, mo := 128000, 8192
	in := Input{
		ID: "keep", DisplayName: &name, Description: &desc,
		Vendor: "v1", Modality: "chat", BillingMode: model.BillingModeTokens, BillingModeSet: true,
		Prices:    model.Prices{Input: i64p(1_000_000), Output: i64p(2_000_000)},
		Endpoints: []string{"chat/completions"}, EndpointsSet: true,
		Capabilities: []string{"tool_call"}, CapabilitiesSet: true,
		ContextWindow: &cw, MaxOutputTokens: &mo,
	}
	created, err := CreateTx(gdb, in, nil)
	if err != nil {
		t.Fatalf("建模型失败: %v", err)
	}
	// 只提交一个价格 lane —— 这正是"运营改个价"的实际形态。
	only := Input{ID: "keep", Prices: model.Prices{Input: i64p(1_500_000)}}
	updated, err := UpdateTx(gdb, only, 0)
	if err != nil {
		t.Fatalf("改价失败: %v", err)
	}

	if updated.DisplayName != "原始名称" {
		t.Fatalf("展示名被改价清空了：%q（改前 %q）", updated.DisplayName, created.DisplayName)
	}
	if updated.Description != "原始描述" {
		t.Fatalf("描述被改价清空了：%q", updated.Description)
	}
	// 端点/能力以"建库时的值"为准：某些能力会被按模态归一化，不该由用例假设具体内容。
	if string(updated.Endpoints) != string(created.Endpoints) {
		t.Fatalf("端点被改价改动/清空：改前 %s，改后 %s", string(created.Endpoints), string(updated.Endpoints))
	}
	if string(updated.Capabilities) != string(created.Capabilities) {
		t.Fatalf("能力被改价改动/清空：改前 %s，改后 %s", string(created.Capabilities), string(updated.Capabilities))
	}
	if updated.ContextWindow != 128000 || updated.MaxOutputTokens != 8192 {
		t.Fatalf("窗口被改价清空了：%d/%d", updated.ContextWindow, updated.MaxOutputTokens)
	}
	if updated.Prices.Input == nil || *updated.Prices.Input != 1_500_000 {
		t.Fatalf("提交的价格没生效：%v", updated.Prices.Input)
	}
	if updated.Prices.Output == nil || *updated.Prices.Output != 2_000_000 {
		t.Fatalf("未提交的 output 价被清空了：%v", updated.Prices.Output)
	}
	if updated.Version != created.Version+1 {
		t.Fatalf("版本应 +1：%d → %d", created.Version, updated.Version)
	}
}

func TestEmptyDescriptionClearsButEmptyNameFallsBackToID(t *testing.T) {
	gdb := withCfgDB(t)
	seedVendorChannel(t, gdb)

	name, desc := "原名", "原描述"
	in := Input{ID: "clr", DisplayName: &name, Description: &desc,
		Vendor: "v1", Modality: "chat", BillingMode: model.BillingModeTokens, BillingModeSet: true,
		Prices: model.Prices{Input: i64p(1)}}
	if _, err := CreateTx(gdb, in, nil); err != nil {
		t.Fatalf("建模型失败: %v", err)
	}

	// 显式传空串：描述应当被清空（这是合法操作），展示名回落到 ID。
	empty := ""
	got, err := UpdateTx(gdb, Input{ID: "clr", DisplayName: &empty, Description: &empty}, 0)
	if err != nil {
		t.Fatalf("清空失败: %v", err)
	}
	if got.Description != "" {
		t.Fatalf("显式空串应清空描述，实际 %q", got.Description)
	}
	if got.DisplayName != "clr" {
		t.Fatalf("空展示名应回落到 ID，实际 %q", got.DisplayName)
	}
}

// TestIconSurvivesPriceOnlyUpdate 图标与展示名同类：它是全列写里的一员，
// 不补 FillMissing 就会被「只改价格」这条最常见的提交形态清空。
func TestIconSurvivesPriceOnlyUpdate(t *testing.T) {
	gdb := withCfgDB(t)
	seedVendorChannel(t, gdb)

	name := "带图标的模型"
	icon := "builtin:openai-gpt"
	in := Input{
		ID: "icon-keep", DisplayName: &name, Icon: &icon,
		Vendor: "v1", Modality: "chat", BillingMode: model.BillingModeTokens, BillingModeSet: true,
		Prices:    model.Prices{Input: i64p(1_000_000), Output: i64p(2_000_000)},
		Endpoints: []string{"chat/completions"}, EndpointsSet: true,
	}
	created, err := CreateTx(gdb, in, nil)
	if err != nil {
		t.Fatalf("建模型失败: %v", err)
	}
	if created.Icon != "builtin:openai-gpt" {
		t.Fatalf("图标没落库: %q", created.Icon)
	}

	got, err := UpdateTx(gdb, Input{ID: "icon-keep", Prices: model.Prices{Input: i64p(1_500_000)}}, 0)
	if err != nil {
		t.Fatalf("改价失败: %v", err)
	}
	if got.Icon != "builtin:openai-gpt" {
		t.Fatalf("改价把图标清空了：%q", got.Icon)
	}

	// 显式空串 = 清空图标（回落到「模型→供应商→首字」的派生链）。
	empty := ""
	cleared, err := UpdateTx(gdb, Input{ID: "icon-keep", Icon: &empty}, 0)
	if err != nil {
		t.Fatalf("清空图标失败: %v", err)
	}
	if cleared.Icon != "" {
		t.Fatalf("显式空串应清空图标，实际 %q", cleared.Icon)
	}
}

// TestBuildRejectsBadIcon 非法 token 必须在 Build 里被挡成 icon 字段错误。
// 三处写入口（管理台 / AI 编排 / 种子）都过 Build，所以校验只放这一处。
func TestBuildRejectsBadIcon(t *testing.T) {
	for _, bad := range []string{"data:image/png;base64,AAA", "https://cdn.example/a.png", "builtin:../x"} {
		b := bad
		_, err := Build(Input{ID: "m", Vendor: "v1", Modality: "chat", BillingMode: model.BillingModeTokens, BillingModeSet: true, Icon: &b})
		fe, ok := err.(*FieldError)
		if !ok || fe.Field != "icon" {
			t.Fatalf("非法图标 %q 应返回 icon 字段错误，实际 %v", bad, err)
		}
	}
}

// TestFillMissingCoversEveryPointerField 是**结构性守卫**：
// Updates 是全列写，所以 Input 里每个指针字段都必须有"未提交时用库内值补"的分支。
// 以后再加一个指针字段却忘了补 FillMissing，这条会立刻红 —— 而不是等线上某天
// 发现"改个价把 XX 清空了"。
func TestFillMissingCoversEveryPointerField(t *testing.T) {
	cur := &model.Model{
		ID: "m", DisplayName: "n", Description: "d", Vendor: "v", Modality: "chat",
		BillingMode: model.BillingModeTokens, Enabled: true,
		Prices:        model.Prices{Input: i64p(1), Output: i64p(2), Call: i64p(3)},
		Endpoints:     model.MustJSON([]string{"chat/completions"}),
		Capabilities:  model.MustJSON([]string{"tool_call"}),
		ContextWindow: 1000, MaxOutputTokens: 100,
	}
	var in Input
	in.ID = "m"
	FillMissing(&in, cur)

	v := reflect.ValueOf(in)
	typ := v.Type()
	for i := 0; i < v.NumField(); i++ {
		f := typ.Field(i)
		if v.Field(i).Kind() != reflect.Ptr {
			continue
		}
		if v.Field(i).IsNil() {
			t.Fatalf("FillMissing 没有覆盖指针字段 %s：全列写会把它清空", f.Name)
		}
	}
}
