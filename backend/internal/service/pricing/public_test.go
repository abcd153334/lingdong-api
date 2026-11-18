package pricing

import (
	"encoding/json"
	"strings"
	"testing"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/money"
)

// testBase 是投影用的站点基址。绝对地址必须由它派生（相对地址在第三方场景不可用）。
const testBase = "https://api.example.com"

// jsonKeys 把任意值序列化后取出顶层键集合。
func jsonKeys(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	return out
}

func sampleSnapshot() *Snapshot {
	price := money.MustParse("0.15")
	return &Snapshot{
		Models: []ModelItem{
			{
				ID: "gpt-image-2", DisplayName: "GPT Image 2", Vendor: "openai",
				VendorName: "OpenAI", VendorColor: "#10A37F", Modality: "image",
				Icon:        "builtin:openai-gpt",
				BillingMode: model.BillingModeSpec,
				Prices:      model.Prices{Call: &price},
				SpecPrices: model.SpecPrices{
					Kind:            "per_image",
					ResolutionFixed: "1K",
					TierPrice:       map[string]int64{"1K": price},
				},
				Endpoints: model.MustJSON([]string{"images/generations"}),
				Enabled:   true, IsDefault: true, Available: true, CreatedAt: 1700000000,
			},
		},
		Vendors: []VendorItem{
			{ID: "openai", Name: "OpenAI", Color: "#10A37F", Icon: "builtin:openai-gpt", Priority: 9, Sort: 1, Builtin: true, ModelCount: 1},
			// 空分组（没有任何在售模型）：不得下发，否则门户出现一个点不动的筛选项。
			{ID: "duomi", Name: "多米API", Color: "#0EA5E9", Priority: 5, Sort: 15, Builtin: true, ModelCount: 0},
		},
		GroupDiscounts: map[string]int64{"default": money.Scale, "vip": 800_000},
		Defaults:       map[string]string{"image": "gpt-image-2"},
		Scale:          money.Scale,
		Currency:       "CNY",
		CurrencySymbol: "¥",
		CnyPerUSD:      7.2,
		OtherCurrency:  "USD",
	}
}

// TestPublicSnapshotHasNoOperationalKeys 用户端目录不得出现任何运营/供应链字段。
func TestPublicSnapshotHasNoOperationalKeys(t *testing.T) {
	pub := publicFrom(sampleSnapshot(), testBase)
	keys := jsonKeys(t, pub)

	forbidden := []string{
		"bindings",        // 该模型挂了几条渠道
		"model_count",     // 供应商下有几个在售模型
		"priority",        // 供应商运营优先级
		"sort",            // 供应商运营排序
		"builtin",         // 内置标记
		"group_discounts", // 计费分组折扣表
		"enabled",         // 目录只收可路由模型，恒真
		"available",       // 同上
		"channel",         // 兜底
		"upstream",        // 兜底
	}
	for _, k := range forbidden {
		if _, ok := keys[k]; ok {
			t.Fatalf("用户端定价目录不得包含键 %q", k)
		}
	}
	// 必备键仍在（避免「裁得太狠」把前端依赖的字段也删了）。
	for _, k := range []string{"models", "vendors", "defaults", "scale", "currency", "currency_symbol", "cny_per_usd"} {
		if _, ok := keys[k]; !ok {
			t.Fatalf("用户端定价目录缺少必备键 %q", k)
		}
	}
}

// TestPublicModelHasNoBindings 单条模型也不得出现 bindings。
func TestPublicModelHasNoBindings(t *testing.T) {
	pub := publicFrom(sampleSnapshot(), testBase)
	if len(pub.Models) != 1 {
		t.Fatalf("模型数不符：%d", len(pub.Models))
	}
	keys := jsonKeys(t, pub.Models[0])
	for _, k := range []string{"bindings", "enabled", "available"} {
		if _, ok := keys[k]; ok {
			t.Fatalf("用户端模型条目不得包含键 %q", k)
		}
	}
	if pub.Models[0].ID != "gpt-image-2" || pub.Models[0].DisplayName != "GPT Image 2" {
		t.Fatalf("模型投影丢字段：%+v", pub.Models[0])
	}
	if pub.Models[0].BillingMode != model.BillingModeSpec {
		t.Fatalf("计费方式应原样透出，实际 %q", pub.Models[0].BillingMode)
	}
	// icon_url 是派生结果（模型自己的 or 回落供应商的）转成的绝对地址，
	// 投影只做"令牌 → 地址"这一步，不再重算回落。
	if got := pub.Models[0].IconURL; !strings.HasPrefix(got, testBase+"/api/icons/b/openai-gpt?v=") {
		t.Fatalf("模型 icon_url 未透传，实际 %q", got)
	}
}

// TestPublicModelTagsAlwaysArray 守「无标签 = []」：前端按数组渲染。
func TestPublicModelTagsAlwaysArray(t *testing.T) {
	pub := publicFrom(sampleSnapshot(), testBase)
	raw, err := json.Marshal(pub.Models[0])
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	if !strings.Contains(string(raw), `"tags":[]`) {
		t.Fatalf("无标签时应下发 []，实际 %s", raw)
	}
}

// TestPublicTagsDoNotShareBackingArray 守缓存不被污染：
// publicFrom 的返回值与 pricing.Get() 的全局快照共享底层数组时，
// 任何调用方 append/改元素都会改掉下一个人拿到的数据。
func TestPublicTagsDoNotShareBackingArray(t *testing.T) {
	snap := sampleSnapshot()
	snap.Models[0].Tags = []model.ModelTagView{{Label: "限时免费", ColorHex: "#E11D48"}}

	pub := publicFrom(snap, testBase)
	pub.Models[0].Tags[0].Label = "被调用方改了"
	if snap.Models[0].Tags[0].Label != "限时免费" {
		t.Fatal("公开投影与源快照共享了底层数组：缓存会被调用方改写")
	}
}

// TestPublicPricesAreAbsoluteMicro 用户端价目必须是**微**，不得二次换算。
//
// 这是「所见即所得」的守门测试：库里 0.15 元 → 150000 微，
// 门户拿到的必须也是 150000，而不是被 qpc 或倍率再过一遍。
func TestPublicPricesAreAbsoluteMicro(t *testing.T) {
	pub := publicFrom(sampleSnapshot(), testBase)
	if got := *pub.Models[0].Prices.Call; got != 150_000 {
		t.Fatalf("按次价应为 150000 微，实际 %d", got)
	}
	// 默认档是档位键（不再是金额）：金额只在 tier_price 里，端口价必须是微。
	if got := pub.Models[0].SpecPrices.TierPrice["1K"]; got != 150_000 {
		t.Fatalf("1K 档单价应为 150000 微，实际 %d", got)
	}
}

// TestPublicVendorsFilteredByUsage 供应商只保留「本次快照真的有模型」的分组，且恰好四键。
func TestPublicVendorsFilteredByUsage(t *testing.T) {
	pub := publicFrom(sampleSnapshot(), testBase)
	if len(pub.Vendors) != 1 {
		t.Fatalf("空分组应被过滤，实际 %d 个：%+v", len(pub.Vendors), pub.Vendors)
	}
	v := pub.Vendors[0]
	if v.ID != "openai" {
		t.Fatalf("保留的分组应为 openai，实际 %s", v.ID)
	}
	keys := jsonKeys(t, v)
	if len(keys) != 4 {
		t.Fatalf("供应商投影恰好 4 个键，实际 %d 个：%v", len(keys), keys)
	}
	for _, k := range []string{"id", "name", "color", "icon_url"} {
		if _, ok := keys[k]; !ok {
			t.Fatalf("供应商投影缺少键 %q", k)
		}
	}
	// icon_url 必须真的透传：白名单加了字段但装配处忘填，就是「永远返回空串」。
	if v.IconURL == "" {
		t.Fatal("供应商 icon_url 未透传")
	}
}

// TestPublicDoesNotMutateSource 投影不得写回内部快照（缓存是全局共享的）。
func TestPublicDoesNotMutateSource(t *testing.T) {
	src := sampleSnapshot()
	before := len(src.Vendors)
	_ = publicFrom(src, testBase)
	if len(src.Vendors) != before {
		t.Fatalf("投影修改了内部快照的 vendors 切片：%d → %d", before, len(src.Vendors))
	}
	if src.GroupDiscounts["vip"] != 800_000 {
		t.Fatal("投影修改了内部快照的 group_discounts")
	}
}

// TestPublicNilSnapshot 空快照不 panic（首次启动、库未就绪）。
func TestPublicNilSnapshot(t *testing.T) {
	pub := publicFrom(nil, testBase)
	if pub == nil || len(pub.Models) != 0 || len(pub.Vendors) != 0 {
		t.Fatalf("空快照应返回空投影，实际 %+v", pub)
	}
}

// TestCloneCopiesMaps 快照浅拷贝必须复制 map（否则调用方能改到缓存里的折扣表）。
func TestCloneCopiesMaps(t *testing.T) {
	src := sampleSnapshot()
	c := src.clone()
	c.GroupDiscounts["vip"] = 1
	c.Defaults["image"] = "other"
	if src.GroupDiscounts["vip"] != 800_000 {
		t.Fatal("clone 未复制 group_discounts")
	}
	if src.Defaults["image"] != "gpt-image-2" {
		t.Fatal("clone 未复制 defaults")
	}
}

var _ = jsonKeys
