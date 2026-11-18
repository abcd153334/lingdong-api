package controller

import (
	"github.com/gin-gonic/gin"

	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/schema"
)

// AdminSchemaCatalog 处理 GET /api/admin/schema/catalog。
//
// 这个端点是「平台的形状」的唯一出口：模态、计费方式矩阵、可声明端点、计价格字段、
// 档位白名单、内置适配器能力、各类限额，全部由服务端投影下发。
//
// 为什么前端与 AI 都必须读它而不是各自内联常量：
//   - 前端内联 → 后端新增端点/档位后下拉里没有，运营以为「不支持」；
//   - AI 内联（写进系统提示词）→ 提示词与代码必然漂移，产出的规格会被保存期校验拒绝，
//     而拒绝理由对模型来说不可见（它只会反复重试同样的错误）。
//
// 不加缓存：这是一次纯内存投影（反射结果本身有缓存），每次重新组装能保证
// 「部署完新代码立刻生效」，不必处理缓存失效。
//
// 目录里三块内容各有权威来源，本控制器只做转发：
// endpoints → apidoc（端点）、spec_prices.fields → 反射 billing.SpecPrices（计价格）、
// adapters → relay 注册表。
func AdminSchemaCatalog(c *gin.Context) {
	response.OK(c, schema.Build())
}
