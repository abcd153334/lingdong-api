// Package keyscope 是「本次请求的数据作用域」的**唯一判据**。
//
// 为什么必须收口：同一个判断原先写了两套 —— controller 的 keyScopeOf 用
// `*model.APIKey`，relay 里三处直接 `apiKeyOf(c).IsHosted()`。两套判据的代价
// 已经在 `/api/user/apps` 上兑现过：那条查询两个都没用，于是应用 A 的托管密钥
// 能枚举用户在应用 B/C 下的密钥元数据。
//
// 语义边界：
//   - 会话票（浏览器）与自建 API KEY（kind='user'）⇒ 账户作用域，Hosted=false；
//   - 托管密钥（kind='hosted'）⇒ 只应看到本应用/本 KEY 产生的东西，Hosted=true。
//
// **不要用 `AppID > 0` 判别托管密钥**：自建 KEY 的 AppID 恒为 0，但 0 不是
// 「非托管」的充分条件（model/api_key.go 的注释写了同一件事）。
package keyscope

import (
	"github.com/gin-gonic/gin"

	"lingdong-api/internal/model"
)

// Scope 是一次请求的数据作用域。
type Scope struct {
	// KeyID 是命中的 API KEY；会话票请求为 0。
	KeyID uint64
	// AppID 只在 Hosted=true 时有效，且必然 > 0（见 Consistent）。
	AppID uint64
	// Hosted 表示凭证是应用托管密钥。
	Hosted bool
}

// Consistent 判断一把 KEY 的作用域是否自洽。
//
// 唯一的非法形态是 `kind='hosted'` 但没有 app_id：这种行只可能来自手工改库
// 或数据修复。放它过去，下游 `if appID != 0` 的过滤会全部静默降级成
// 「整个账号可见」—— 那是 fail-open，比直接报错危险得多。
func Consistent(k *model.APIKey) bool {
	if k == nil || !k.IsHosted() {
		return true
	}
	return k.AppID > 0
}

// Of 读取上下文里的作用域。ok=false 表示作用域自相矛盾，调用方**必须拒绝**，
// 不能降级成账户作用域。
//
// 上下文由鉴权中间件写入（api_key_record）；未写（Cookie 面）即账户作用域。
func Of(c *gin.Context) (Scope, bool) {
	v, ok := c.Get("api_key_record")
	if !ok {
		return Scope{}, true
	}
	k, _ := v.(*model.APIKey)
	if k == nil || !k.IsHosted() {
		return Scope{}, true
	}
	if !Consistent(k) {
		return Scope{KeyID: k.ID, Hosted: true}, false
	}
	return Scope{KeyID: k.ID, AppID: k.AppID, Hosted: true}, true
}

// AppFilter 返回按应用过滤的参数，专门给「订单/流水」这类按 app_id 收口的查询。
//
//	scoped=false        ⇒ 账户作用域，调用方**不要**加过滤条件
//	scoped=true, ok=true ⇒ 必须加 `app_id = appID`
//	ok=false            ⇒ 凭证自相矛盾，调用方必须返回 403
//
// 单独提供它而不是让调用方写 `if hosted { x = appID }`：后者在 appID=0 时
// 会静默变成「不过滤」，正是 V8 的结构性来源。
func AppFilter(c *gin.Context) (appID uint64, scoped, ok bool) {
	s, valid := Of(c)
	if !valid {
		return 0, false, false
	}
	if !s.Hosted {
		return 0, false, true
	}
	return s.AppID, true, true
}
