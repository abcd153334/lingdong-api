package conf

import "strings"

// 结算币种与参考汇率。
//
// 为什么单独一个文件而不是塞进 conf.go：conf.go 的 Env 结构体是「进程级配置」，
// 而这里读的是 system_config（热更新）。两者混在一起时，后来者很容易把
// SiteCurrency() 也当成「启动时读一次」的 Env 字段做成进程内缓存 ——
// 于是多副本部署下「A 实例改了币种，B 实例还在按旧币种算钱」。
//
// 本文件的函数每次都读 conf.System()，不自建缓存：热更新靠既有的
// Set/Reload + configsync 广播同步。
//
// 币种是**锁定**的：初始化时可设定，一旦库里出现任何账务记录（orders /
// transactions / usages）就永久只读，由 service/systemconfig 在保存时校验。
// 因此全库只有一个币种，`usages.currency` 这类快照列只是账单凭证，不参与分支。

// 币种枚举。只支持两位小数的币种：订单金额以「最小单位整数（分）」存储，
// 零小数（JPY）与三位小数（KWD）需要额外换算，当前不接。
const (
	CurrencyCNY = "CNY"
	CurrencyUSD = "USD"
)

// DefaultCnyPerUsd 是汇率的内置默认值（仅参考展示与 OpenAI 协议换算用）。
const DefaultCnyPerUsd = 7.2

// NormalizeCurrency 把任意输入归一为受支持的币种代码；无法识别时 ok=false。
func NormalizeCurrency(raw string) (code string, ok bool) {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case CurrencyCNY:
		return CurrencyCNY, true
	case CurrencyUSD:
		return CurrencyUSD, true
	}
	return "", false
}

// siteCurrencyNode 只解析 site 子树里的币种字段。
// 单独定义而不是复用 service/systemconfig 的 siteCfg：conf 不能反向依赖它（会成环）。
type siteCurrencyNode struct {
	Currency string `json:"currency"`
}

// SiteCurrency 返回当前结算币种（CNY / USD）。
//
// 缺键、解析失败、取值非法一律回落 CNY：
// 运营在中国、充值以元计、四个支付网关全收人民币 —— 默认值必须与现实一致，
// 否则首次打开管理台就与现实不符。
func SiteCurrency() string {
	var node siteCurrencyNode
	_ = System().Unmarshal("site", &node)
	if code, ok := NormalizeCurrency(node.Currency); ok {
		return code
	}
	return CurrencyCNY
}

// SiteCurrencySymbol 返回展示用货币符号（¥ / $）。
//
// 符号由币种派生，不再是独立的配置键（site.currency_symbol 已删除）：
// 一个可自由编辑的符号能与币种不一致（币种 CNY 却显示 $），
// 那是纯粹的展示事故来源，而它不携带任何计费信息。
func SiteCurrencySymbol() string { return DefaultSymbolFor(SiteCurrency()) }

// DefaultSymbolFor 返回币种的内置符号。
func DefaultSymbolFor(code string) string {
	if code == CurrencyUSD {
		return "$"
	}
	return "¥"
}

// OtherCurrency 返回「另一个」受支持币种，用于展示次币种参考金额。
func OtherCurrency(code string) string {
	if code == CurrencyUSD {
		return CurrencyCNY
	}
	return CurrencyUSD
}

// CnyPerUsd 返回参考汇率（1 美元 = 多少人民币）。
//
// 合法用途只有三处，且都不影响用户扣费：
//  1. OpenAI 兼容接口的美元报价（controller/gateway_catalog.go，协议契约）；
//  2. 次币种参考展示（只展示，不参与计费）；
//  3. 上游美元成本折算为结算币种（一次性，落库后不再随汇率变）。
//
// 计费公式里出现它即为缺陷（由 TestNoFloat64InMoneyPath / 代码评审守）。
func CnyPerUsd() float64 {
	fx := System().Float("cny_per_usd", DefaultCnyPerUsd)
	if fx <= 0 {
		return DefaultCnyPerUsd
	}
	return fx
}
