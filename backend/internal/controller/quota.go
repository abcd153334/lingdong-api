package controller

import (
	"github.com/gin-gonic/gin"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/keyscope"
)

// quotaVisible 判断本次请求是否允许看到**账户级额度**（余额 / 冻结 / 累计消费）。
//
// 为什么必须收口成一个函数：这个开关（`api_keys.allow_user_quota_query`）建 KEY
// 时默认开启，关掉它的语义是"这把密钥交给第三方，但不许它看我的余额"。
// 原先只有 `GET /api/user/self` 判了一次，于是同一把 KEY 可以从
// `/api/user/transactions`（balance_after 快照）、`/v1/account`（顶层 balance）、
// `/dashboard/billing/subscription`（换算成美元）三处拿到同样的数字
// —— 开关看起来生效了，实际一点没挡住。
//
// 判定只看请求上下文里的 KEY 实体，不重查库：`UserTokenAuth` 已经把
// `api_key_record` 放进来，再查一次会出现"同一请求两个时刻的 KEY 状态"。
//
// 会话票（浏览器）永远 true：那是用户本人，开关约束的是"把 KEY 交出去"这件事。
func quotaVisible(c *gin.Context) bool {
	v, ok := c.Get("api_key_record")
	if !ok {
		return true
	}
	k, _ := v.(*model.APIKey)
	if k == nil {
		return true
	}
	return k.AllowUserQuotaQuery
}

// keyScopeOf 返回本次请求的**数据作用域**（按 KEY 收口的端点用）。
//
// 应用托管密钥（hosted）是"应用后端代表某个用户"的凭证：它只应看到
// 自己在这个应用下产生的东西。但 /api/user/* 的读写面是按 user_id 过滤的
// （门户要的就是"我名下全部"），于是 A 应用的密钥能列出、甚至删除用户在
// B 应用下上传的文件，取消 B 的任务，读 B 的订单。
//
// 判据只有一处：pkg/keyscope。本函数只是转成 controller 里惯用的三元组。
// 会话票与非托管 KEY 返回 hosted=false（作用域 = 整个账号，与门户一致）。
func keyScopeOf(c *gin.Context) (keyID, appID uint64, hosted bool) {
	s, _ := keyscope.Of(c)
	return s.KeyID, s.AppID, s.Hosted
}

// appFilterOf 返回「按应用收口」的过滤参数（订单/流水这类查询用）。
//
// 不要退回 `if hosted { filter = appID }` 的写法：hosted 但 app_id=0 的脏行
// 会让过滤条件变成 0，而下游 service 的判据是 `if app_id != 0` —— 结果是
// 静默变成整账号可见（fail-open）。ok=false 必须当场拒绝。
func appFilterOf(c *gin.Context) (appID uint64, scoped, ok bool) {
	return keyscope.AppFilter(c)
}
