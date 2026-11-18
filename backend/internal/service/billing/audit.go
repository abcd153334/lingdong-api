package billing

import (
	"fmt"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
)

// UnparsableSpecModels 返回「档位价有问题」的在售模型 id 及原因。
//
// 数据库句柄由调用方传入而不是取包级单例：billing 是计费核心，
// 反向依赖 internal/db 会形成 db → apidoc → billing → db 的 import 环
// （db 的内置模型播种要用 apidoc 的能力词表）。传参也便于测试用独立库。
//
// 两类问题：
//  1. spec_prices 的 JSON **解不出来**（旧键名、填了元或小数、类型写错）。
//     Scan 阶段是宽松的（坏行退化成空价目，否则一行坏数据会让整张 models
//     表查询失败、全站价目不可用），所以必须在启动时单独点名报出来。
//  2. 按规格计价但价目为空 / 缺 kind。运行期每次调用都会 ErrPriceMissing
//     （400 price_not_configured），而管理台看着"已启用在售" —— 只会在
//     第一个真实请求上暴露，启动时报出来成本最低。
//
// 只在 enabled=true 上检查：停用的模型可能是运营刻意留着的半成品，
// 每次启动报它的红字会训练出"忽略 ERROR"的习惯。
func UnparsableSpecModels(g *gorm.DB) ([]string, error) {
	if g == nil {
		return nil, fmt.Errorf("billing: 数据库句柄为空")
	}
	// 第 2 类：走模型（拿到类型化价目）。
	// 范围是**全部启用模型**，不只 billing_mode='spec'：
	// 「配好了价目但 billing_mode 落在列默认值 tokens」是最常见的配置错，
	// 只在 spec 里查会让这类模型完全不在体检视野内（而运行期已按 PriceProblem 纠偏）。
	var rows []model.Model
	if err := g.Select("id", "billing_mode", "spec_prices", "price_input", "price_call").
		Where("enabled = ?", true).Find(&rows).Error; err != nil {
		return nil, err
	}
	bad := make([]string, 0, len(rows))
	for i := range rows {
		if why := PriceProblem(rows[i].BillingMode, rows[i].Prices, rows[i].SpecPrices); why != "" {
			bad = append(bad, rows[i].ID+"（"+why+"）")
		}
	}

	// 第 1 类：必须走**原始文本**，因为类型化读取已经把坏值吞成空结构了。
	// spec_prices::text 把 JSONB 还原成可再解码的字符串（不依赖驱动返回 []byte）。
	//
	// 必须带 `deleted_at IS NULL`：原生 SQL **绕过** GORM 的软删作用域，
	// 否则已删模型（正是 M28 之前的老行）会一直被报成"价目非法" ——
	// 启动日志里十几条 ERROR，运营以为全站坏了，实际一条都不影响计费。
	raw, err := g.Raw(
		`SELECT id, spec_prices::text FROM models
		 WHERE spec_prices IS NOT NULL AND deleted_at IS NULL`).Rows()
	if err != nil {
		return nil, err
	}
	defer raw.Close()
	for raw.Next() {
		var (
			id  string
			txt string
		)
		if err := raw.Scan(&id, &txt); err != nil {
			return nil, err
		}
		if _, decErr := model.DecodeSpecPrices([]byte(txt)); decErr != nil {
			bad = append(bad, fmt.Sprintf("%s（价目 JSON 非法：%v）", id, decErr))
		}
	}
	if err := raw.Err(); err != nil {
		return nil, err
	}
	return bad, nil
}
