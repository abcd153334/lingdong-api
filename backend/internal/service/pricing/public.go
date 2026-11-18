package pricing

import (
	"lingdong-api/internal/model"
	"lingdong-api/internal/service/iconref"
)

// 本文件是**用户端**定价目录的对外投影（fail-closed 白名单）。
//
// 为什么要独立结构体而不是「复制内部 Snapshot 再删字段」：
// 内部 Snapshot 会随运营需要不断加字段（渠道数、成本、分组折扣、内置标记…），
// 「先复制再清零」是 fail-open 设计 —— 新加的字段默认就泄漏出去。
// 这里改成显式白名单：新增内部字段时白名单不加，用户就看不到。
//
// 用户端不得出现的信息（本文件的存在理由）：
//   - 该模型挂了几条渠道（bindings）—— 直接暴露供应链规模；
//   - 供应商的 priority / sort / builtin / model_count —— 运营排布与市场规模；
//   - group_discounts（计费分组折扣）—— 内部结算参数；
//   - enabled / available —— 目录只收可路由模型，这两个值恒为真，下发即双真相源。

// PublicVendor 是用户端可见的供应商分组（只有 id / name / color / icon_url）。
type PublicVendor struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
	// IconURL 是可直接放进 <img src> 的**绝对地址**（空 = 该分组没有图标）。
	//
	// 下发地址而不是令牌：令牌是本系统的存储表示，第三方拿到 `upload:01j…`
	// 除了本仓库的前端以外没有任何人能解释它。地址由 iconref.URL 单点派生，
	// base 取 siteurl.Base(c)，所以多域名部署下不会把某个 Host 烤进缓存。
	IconURL string `json:"icon_url"`
}

// PublicModel 是用户端可见的模型条目。
//
// 价目单位是**微**（1 货币单位 = 1e6 微），与内部完全一致 —— 不再做任何换算投影。
type PublicModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Vendor      string `json:"vendor"`
	VendorName  string `json:"vendor_name"`
	VendorColor string `json:"vendor_color"`
	// IconURL 已是派生结果（模型自己的 or 供应商的，见 brandicon.Effective），
	// 且已转成可直接渲染的绝对地址；空 = 退回首字占位。
	IconURL     string           `json:"icon_url"`
	Modality    string           `json:"modality"`
	BillingMode string           `json:"billing_mode"`
	Prices      model.Prices     `json:"prices"`
	SpecPrices  model.SpecPrices `json:"spec_prices"`
	IsDefault   bool             `json:"is_default"`
	IsFree      bool             `json:"is_free"`
	Endpoints   model.JSON       `json:"endpoints"`
	// Tags 是营销标签（已过滤到期项）。无标签时是 `[]`，永远是数组。
	Tags      []model.ModelTagView `json:"tags"`
	CreatedAt int64                `json:"created_at"`
}

// PublicSnapshot 是 GET /api/pricing 的响应体（门户模型广场 / 价目页 / 接入文档共用）。
type PublicSnapshot struct {
	Models         []PublicModel     `json:"models"`
	Vendors        []PublicVendor    `json:"vendors"`
	Defaults       map[string]string `json:"defaults"`
	Scale          int64             `json:"scale"`
	Currency       string            `json:"currency"`
	CurrencySymbol string            `json:"currency_symbol"`
	CnyPerUSD      float64           `json:"cny_per_usd"`
	OtherCurrency  string            `json:"other_currency"`
}

// Public 组装用户端定价目录。
//
// 只读 pricing.Get() 的浅拷贝：每次**新建**切片，绝不写回缓存里的 Models/Vendors。
// vendors 只保留「本次快照里真的有模型引用」的分组 —— 空分组自动消失。
//
// base 是站点对外基址（siteurl.Base(c)），只用来拼 icon_url。
// **不能把地址写进缓存**：pricing.Get() 是 10s 全局单例，一旦烤进第一个请求的
// Host，多域名部署下所有客户端都会拿到别人的域名。
func Public(base string) (*PublicSnapshot, error) {
	snap, err := Get()
	if err != nil {
		return nil, err
	}
	return publicFrom(snap, base), nil
}

// publicFrom 是 Public 的纯投影（不碰缓存、不查库），供单测直接驱动。
func publicFrom(snap *Snapshot, base string) *PublicSnapshot {
	if snap == nil {
		return &PublicSnapshot{}
	}
	out := &PublicSnapshot{
		Models:         make([]PublicModel, 0, len(snap.Models)),
		Vendors:        make([]PublicVendor, 0, len(snap.Vendors)),
		Defaults:       snap.Defaults,
		Scale:          snap.Scale,
		Currency:       snap.Currency,
		CurrencySymbol: snap.CurrencySymbol,
		CnyPerUSD:      snap.CnyPerUSD,
		OtherCurrency:  snap.OtherCurrency,
	}

	used := make(map[string]struct{}, len(snap.Models))
	for _, m := range snap.Models {
		used[m.Vendor] = struct{}{}
		out.Models = append(out.Models, PublicModel{
			ID:          m.ID,
			DisplayName: m.DisplayName,
			Vendor:      m.Vendor,
			VendorName:  m.VendorName,
			VendorColor: m.VendorColor,
			IconURL:     iconref.URL(base, m.Icon),
			Modality:    m.Modality,
			BillingMode: m.BillingMode,
			Prices:      m.Prices,
			SpecPrices:  model.EffectiveSpecPricesFor(m.BillingMode, m.SpecPrices),
			IsDefault:   m.IsDefault,
			IsFree:      m.IsFree,
			Endpoints:   m.Endpoints,
			CreatedAt:   m.CreatedAt,
			// 新建切片：直接赋值会让响应与 10s 全局缓存共享底层数组，
			// 任何调用方 append/改元素都会污染下一个人拿到的快照。
			Tags: append([]model.ModelTagView{}, m.Tags...),
		})
	}

	// 顺序沿用快照的 sort ASC, id ASC（Get 已排好），这里只做过滤。
	for _, v := range snap.Vendors {
		if _, ok := used[v.ID]; !ok {
			continue
		}
		out.Vendors = append(out.Vendors, PublicVendor{
			ID: v.ID, Name: v.Name, Color: v.Color, IconURL: iconref.URL(base, v.Icon),
		})
	}
	return out
}

// PublicVendors 是「用户端可见的供应商分组」的唯一实现。
//
// `/api/portal/vendors` 与 `/api/pricing` 的 vendors 段必须**同一个真相源** ——
// 两份各自查询会随时间漂移（一份过滤空分组、另一份不过滤）。
func PublicVendors(base string) ([]PublicVendor, error) {
	pub, err := Public(base)
	if err != nil {
		return nil, err
	}
	if pub == nil {
		return []PublicVendor{}, nil
	}
	return pub.Vendors, nil
}
