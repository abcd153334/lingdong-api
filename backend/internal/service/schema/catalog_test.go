package schema

import (
	"sort"
	"strings"
	"testing"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/model"
	"lingdong-api/internal/relay"
	"lingdong-api/internal/service/billing"

	// 空导入触发内置适配器注册（与 router 的做法一致）：
	// 不导入的话注册表是空的，这条守漂移用例会变成永远为真的空断言。
	_ "lingdong-api/internal/relay/adapters"
)

// TestCatalogEndpointsMatchAPIDoc 守「端点清单不漂移」。
//
// 目录里的端点必须是 apidoc 的全集投影（数量与键集都一致）。
// 一旦有人往 apidoc 加了端点却漏了排除规则，这里会失败 —— 而不是等到
// AI 编排出「不存在的端点」才在运行期 500。
func TestCatalogEndpointsMatchAPIDoc(t *testing.T) {
	cat := Build()
	want := apidoc.SpecEndpointTable()
	if len(cat.Endpoints) != len(want) {
		t.Fatalf("端点数不一致：目录 %d，apidoc %d", len(cat.Endpoints), len(want))
	}
	seen := map[string]bool{}
	for _, e := range cat.Endpoints {
		if e.Key == "" {
			t.Fatalf("目录里存在空端点键: %+v", e)
		}
		if seen[e.Key] {
			t.Fatalf("端点键重复: %s", e.Key)
		}
		seen[e.Key] = true
		if _, ok := apidoc.SpecEndpointByKey(e.Key); !ok {
			t.Fatalf("目录内的端点 %q 在 apidoc 里查不到", e.Key)
		}
		// 不支持的原因必须写清楚：只给 false 不给理由，运营/AI 会反复试错。
		if !e.Supported && strings.TrimSpace(e.Reason) == "" {
			t.Fatalf("端点 %q 标记为不支持却没给原因", e.Key)
		}
	}
}

// TestCatalogSpecPriceFieldsMatchBilling 守「计价格字段集 = 反射结构」。
//
// 这是本项目历史上最贵的一个 bug（P0-9）的守门用例：保存期有一个 12 键的
// 校验结构，计费侧是 15 键，少的三个键（n_max / ref_*）写错既不报错也不生效。
// 现在字段集只有一个来源（反射），本用例防止有人再手写一份。
func TestCatalogSpecPriceFieldsMatchBilling(t *testing.T) {
	cat := Build()
	want := billing.SpecPriceFields()
	got := make([]string, 0, len(cat.SpecPrices.Fields))
	for _, f := range cat.SpecPrices.Fields {
		got = append(got, f.Key)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("计价格字段集与反射不一致：\n目录 %v\n反射 %v", got, want)
	}
	// 每个字段都必须有中文名：编辑器与 AI 提示都靠它，缺失就是"看不懂的键"。
	for _, f := range cat.SpecPrices.Fields {
		if strings.TrimSpace(f.Label) == "" {
			t.Fatalf("字段 %q 缺少中文名", f.Key)
		}
		// 取值类型必须能被编辑器/AI 直接理解：number（微金额/秒数）、map（档位 → 单价）、
		// string（枚举）、bool（模型级开关）、object（嵌套约束，见 video_limits：
		// 它是一组"上游会在提交阶段拒绝的组合"，字段多且语义独立，平铺会污染价目表；
		// 这类键由模型表单专门渲染，不走通用标量编辑器）。
		allowed := map[string]bool{"number": true, "map": true, "string": true, "bool": true, "object": true}
		if !allowed[f.Kind] {
			t.Fatalf("字段 %q 的取值类型不合法: %q", f.Key, f.Kind)
		}
	}
}

// TestCatalogFieldLabelNoHallucination 守「目录里不能有幻觉字段」。
//
// priceFieldLabels 是多写的来源（多写一条 = 运营填了系统不认的键）。
// 这里反向检查：labels 的键必须都在真实字段集里。
func TestCatalogFieldLabelNoHallucination(t *testing.T) {
	real := map[string]bool{}
	for _, k := range billing.SpecPriceFields() {
		real[k] = true
	}
	for k := range priceFieldLabels {
		if !real[k] {
			t.Fatalf("priceFieldLabels 里的 %q 不是真实字段（幻觉键）", k)
		}
	}
}

// TestCatalogBillingModeMatrix 守「模态 × 计费方式」矩阵与校验函数同源。
//
// 矩阵是下发给前端/AI 的，AllowsBillingMode 是保存期实际拦截的。两者必须逐格一致，
// 否则会出现「下拉允许选、保存报错」这种自相矛盾的体验。
//
// 遍历的是**全集**（三种常量），而不是矩阵里出现过的值：只遍历矩阵会漏掉
// 「矩阵漏声明某种计费方式」这一类错误 —— 那恰好是最常见的漏改形态。
func TestCatalogBillingModeMatrix(t *testing.T) {
	cat := Build()
	allModes := []string{model.BillingModeTokens, model.BillingModeCall, model.BillingModeSpec}
	seen := map[string]bool{}
	for _, m := range cat.Modalities {
		if seen[m.ID] {
			t.Fatalf("模态重复: %s", m.ID)
		}
		seen[m.ID] = true
		if len(m.BillingModes) == 0 {
			t.Fatalf("模态 %s 没有声明任何计费方式", m.ID)
		}
		for _, mode := range allModes {
			allowedInMatrix := false
			for _, v := range m.BillingModes {
				if v == mode {
					allowedInMatrix = true
				}
			}
			if got := AllowsBillingMode(m.ID, mode); got != allowedInMatrix {
				t.Fatalf("模态 %s 计费方式 %s：矩阵 %v，校验函数 %v", m.ID, mode, allowedInMatrix, got)
			}
		}
		// 矩阵里不得出现未定义的计费方式（拼错会让前端渲染出空选项）。
		for _, v := range m.BillingModes {
			found := false
			for _, mode := range allModes {
				if v == mode {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("模态 %s 声明了未知计费方式 %q", m.ID, v)
			}
		}
		// 按规格计费的模态必须声明 kind，否则前端不知道 spec_prices.kind 填什么。
		if hasBillingMode(m, model.BillingModeSpec) && m.SpecKind == "" {
			t.Fatalf("模态 %s 允许按规格计费却没声明 spec_kind", m.ID)
		}
		if !hasBillingMode(m, model.BillingModeSpec) && m.SpecKind != "" {
			t.Fatalf("模态 %s 不按规格计费却声明了 spec_kind", m.ID)
		}
	}
	// 反向：全集里每个取值的标签必须存在（前端直接渲染文案）。
	if len(cat.BillingModes) != 3 {
		t.Fatalf("计费方式应恰好三种，实际 %d", len(cat.BillingModes))
	}
	for _, bm := range cat.BillingModes {
		if bm.Label == "" {
			t.Fatalf("计费方式 %q 缺少标签", bm.Value)
		}
		known := false
		for _, mode := range allModes {
			if bm.Value == mode {
				known = true
				break
			}
		}
		if !known {
			t.Fatalf("标签表声明了未知计费方式 %q", bm.Value)
		}
	}
}

func hasBillingMode(m Modality, mode string) bool {
	for _, v := range m.BillingModes {
		if v == mode {
			return true
		}
	}
	return false
}

// TestCatalogAudioBillingModes 守语音的计费方式边界：
// 不能按量（永远解析不出 token = 永久 0 元），可以按次，也可以按秒（per_second）。
//
// 允许按秒的前提是"时长有真实来源"（桥接 extract.duration 或音频头探测），
// 这条由 relay 侧保证；目录层只负责让运营/AI 选得到这个选项。
func TestCatalogAudioBillingModes(t *testing.T) {
	if AllowsBillingMode("audio", model.BillingModeTokens) {
		t.Fatal("语音不得按量计费（永远解析不出 token，等于免费）")
	}
	if !AllowsBillingMode("audio", model.BillingModeCall) {
		t.Fatal("语音必须允许按次计费")
	}
	if !AllowsBillingMode("audio", model.BillingModeSpec) {
		t.Fatal("语音必须允许按规格（按秒）计费")
	}
	if got := SpecKindOf("audio"); got != "per_second" {
		t.Fatalf("语音的计价格 kind 应为 per_second，实际 %q", got)
	}
	// 未知模态必须拒绝：拼错模态不应绕过所有组合校验。
	if AllowsBillingMode("vidoe", model.BillingModeSpec) {
		t.Fatal("未知模态不得放行任何计费方式")
	}
	// 未知计费方式也必须拒绝：它会让计费侧走 default 分支，行为未定义。
	if AllowsBillingMode("chat", "pay-as-you-go") {
		t.Fatal("未知计费方式不得放行")
	}
}

// TestCatalogTierKeys 守档位白名单的大小写（image 大写 K / video 小写）。
//
// 大小写写反的后果是静默的：校验通过、运行期查表永不命中 → 回落兜底价。
func TestCatalogTierKeys(t *testing.T) {
	cat := Build()
	for _, k := range cat.SpecPrices.TierKeys["image"] {
		if k != strings.ToUpper(k) {
			t.Fatalf("图像档位必须大写 K，实际 %q", k)
		}
	}
	for _, k := range cat.SpecPrices.TierKeys["video"] {
		if k != strings.ToLower(k) {
			t.Fatalf("视频档位必须小写，实际 %q", k)
		}
	}
	if len(cat.SpecPrices.TierKeys["video"]) == 0 || len(cat.SpecPrices.TierKeys["image"]) == 0 {
		t.Fatal("档位白名单不得为空")
	}
}

// TestCatalogLimitsMatchSpecDSL 守限额同源（目录数字 == 校验用的常量）。
func TestCatalogLimitsMatchSpecDSL(t *testing.T) {
	cat := Build()
	l := cat.Limits.Spec
	if l.MaxEndpoints <= 0 || l.MaxSpecBytes <= 0 || l.MaxPathLen <= 0 {
		t.Fatalf("限额必须为正数: %+v", l)
	}
	if l.PollKeySuffix == "" {
		t.Fatal("轮询端点后缀不能为空（前端要据此生成端点键）")
	}
	if len(l.AuthHeaders) == 0 {
		t.Fatal("允许的鉴权头清单不能为空")
	}
	headers := append([]string(nil), l.AuthHeaders...)
	sort.Strings(headers)
	for i, h := range headers {
		if i > 0 && headers[i-1] == h {
			t.Fatalf("鉴权头清单有重复项: %q", h)
		}
		if h != strings.ToLower(h) {
			t.Fatalf("鉴权头必须小写规范化: %q", h)
		}
	}
	if cat.Limits.Pricing.MaxSeconds != billing.MaxPriceSeconds {
		t.Fatalf("秒数上限与计费实现不一致：%d vs %d",
			cat.Limits.Pricing.MaxSeconds, billing.MaxPriceSeconds)
	}
}

// TestCatalogReservedKeys 守「保留键非空且不含 model」。
//
// model 必须可写：规格与 L1 补丁都经常需要把平台模型名改写成上游型号名，
// 一旦把 model 加进保留键，所有别名渠道的配置都会在保存期被拒。
func TestCatalogReservedKeys(t *testing.T) {
	keys := ReservedPatchKeys()
	if len(keys) == 0 {
		t.Fatal("保留键清单不能为空")
	}
	for _, k := range keys {
		if k == "model" {
			t.Fatal("model 不能进保留键（别名渠道需要写上游型号名）")
		}
	}
	for i := 1; i < len(keys); i++ {
		if keys[i-1] > keys[i] {
			t.Fatalf("保留键必须有序输出，实际 %v", keys)
		}
	}
}

// TestCatalogAdaptersMatchRegistry 守「适配器清单 = 注册表」。
func TestCatalogAdaptersMatchRegistry(t *testing.T) {
	cat := Build()
	if len(cat.Adapters) == 0 {
		t.Fatal("内置适配器清单不能为空")
	}
	reg := relay.Adapters()
	if len(cat.Adapters) != len(reg) {
		t.Fatalf("适配器数量不一致：目录 %d，注册表 %d", len(cat.Adapters), len(reg))
	}
	for _, a := range cat.Adapters {
		if a.Name == "" {
			t.Fatal("适配器缺少名称")
		}
		if _, ok := relay.GetAdapter(a.Name); !ok {
			t.Fatalf("目录里的适配器 %q 在注册表里查不到", a.Name)
		}
		for _, c := range a.Caps {
			if c == "" {
				t.Fatalf("适配器 %s 的能力矩阵含空值", a.Name)
			}
		}
	}
}
