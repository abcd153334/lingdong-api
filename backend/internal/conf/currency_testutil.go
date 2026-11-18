package conf

import (
	"encoding/json"
)

// SetBillingForTest 把全局结算口径固定为指定值，返回恢复函数。
//
// 只给测试用。存在的原因很实际：SiteCurrency / CnyPerUsd 是**读全局配置**的，
// 于是任何断言具体金额的单测都会随部署币种漂移 —— 同一个测试在美元环境绿、
// 人民币环境红。测试必须能显式声明「我在什么口径下断言」。
//
// 调用方必须 defer restore()：不改回去会污染同一测试二进制里的其它用例。
func SetBillingForTest(currency string, cnyPerUsd float64) (restore func()) {
	prev := System().All()
	site, _ := json.Marshal(map[string]any{
		"site_name": "测试站点", "currency": currency,
	})
	next := map[string]json.RawMessage{"site": site}
	if cnyPerUsd > 0 {
		fx, _ := json.Marshal(cnyPerUsd)
		next["cny_per_usd"] = fx
	}
	System().SetMany(next)

	restore = func() { System().Reload(prev) }
	return restore
}
