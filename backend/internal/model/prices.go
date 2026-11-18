package model

// Prices 是模型价目的**唯一结构定义**（单一真相源）。
//
// 所有需要"列出模型价格字段"的地方都必须内嵌/复用本结构，禁止各自声明一份：
// 库列（Model 内嵌）、写库列清单（modelcfg.Updates 反射）、计费快照（billing.Snapshot）、
// 目录投影（pricing.ModelItem）、管理台 DTO、AI 提案（agent.ModelData）。
//
// 历史教训：改造前价格字段有 6 份副本（库列 / 入参 / DTO / 写库列清单 / 快照 / 投影），
// 任何一处漏改都表现为"保存成功但价格不生效"，且测试拦不住。
//
// 单位与语义：
//   - Input..AudioOutput 是「微 / 百万 tokens」。微 = 1e-6 结算货币单位（money.Scale=1e6）。
//   - Call 是「微 / 次」（按次计费的固定价）。
//   - 全部用 *int64 而不是 int64：nil = 未配置，指向 0 = 显式免费。
//     这两者在绝对价下是完全不同的语义，用非指针会让"0 元"和"没填"无法区分，
//     而 omitempty 会把显式 0 元省略成"未配置"→ 结算时 ErrPriceMissing → 500。
type Prices struct {
	Input         *int64 `gorm:"column:price_input" json:"input,omitempty"`
	Output        *int64 `gorm:"column:price_output" json:"output,omitempty"`
	Cache         *int64 `gorm:"column:price_cache" json:"cache,omitempty"`
	CacheCreate   *int64 `gorm:"column:price_cache_create" json:"cache_create,omitempty"`
	CacheCreate1h *int64 `gorm:"column:price_cache_create_1h" json:"cache_create_1h,omitempty"`
	Image         *int64 `gorm:"column:price_image" json:"image,omitempty"`
	Audio         *int64 `gorm:"column:price_audio" json:"audio,omitempty"`
	AudioOutput   *int64 `gorm:"column:price_audio_output" json:"audio_output,omitempty"`
	Call          *int64 `gorm:"column:price_call" json:"call,omitempty"`
}

// tokenLanes 是 token 类价格字段的枚举（顺序固定，供反射/测试/回显复用）。
//
// 返回指针的指针是为了让调用方能同时读值与写值，而不必为 8 个 lane 各写一遍 switch。
func (p *Prices) tokenLanes() []struct {
	Name string
	Ptr  **int64
} {
	return []struct {
		Name string
		Ptr  **int64
	}{
		{"input", &p.Input},
		{"output", &p.Output},
		{"cache", &p.Cache},
		{"cache_create", &p.CacheCreate},
		{"cache_create_1h", &p.CacheCreate1h},
		{"image", &p.Image},
		{"audio", &p.Audio},
		{"audio_output", &p.AudioOutput},
	}
}

// HasAnyPrice 报告是否至少配置了一个价格（任意 lane 非 nil）。
func (p *Prices) HasAnyPrice() bool {
	if p.Call != nil {
		return true
	}
	for _, l := range p.tokenLanes() {
		if *l.Ptr != nil {
			return true
		}
	}
	return false
}

// AllConfiguredFree 报告"已配置的价格是否全部为 0"（is_free 的派生判据之一）。
//
// 未配置（nil）不计入：只有全 nil 时由调用方按"没有价格"处理，而不是"免费"。
func (p *Prices) AllConfiguredFree() bool {
	any := false
	check := func(v *int64) bool {
		if v == nil {
			return true
		}
		any = true
		return *v == 0
	}
	if !check(p.Call) {
		return false
	}
	for _, l := range p.tokenLanes() {
		if !check(*l.Ptr) {
			return false
		}
	}
	return any
}
