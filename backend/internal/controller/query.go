package controller

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// optionalUintQuery 解析可选的 uint64 query 参数。
//
// 返回 (0, true) 表示没传这个参数；(v, true) 表示传了且合法；
// (0, false) 表示传了但不是合法数字 —— 调用方必须回 400，而不是当成"没传"。
//
// 为什么单独抽出来：审计（admin_id）与认证事件（uid）两个筛选项此前各写了一份
// 「TrimSpace → 空则跳过 → ParseUint → 失败即 400」，两份完全一样。
// 这类解析最怕有人图省事把失败当 0：那就成了"传错参数反而拿到全量数据"——
// 运营以为自己筛了某个管理员的操作，实际看到的是所有人的。
func optionalUintQuery(c *gin.Context, name string) (uint64, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return 0, true
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// appScopeQuery 解析 `?app_id=`（**三态**，不能复用 optionalUintQuery）。
//
// 返回：(0,false,true) 未传；(0,true,true) 只看平台直连；(n,true,true) 只看该应用；
// (_,_,false) 传了非法值 —— 调用方必须 400。
//
// 为什么不复用 optionalUintQuery：它对 "" 与 "0" 都返回 (0,true)，而 `app_id=0`
// 是有业务含义的（平台直连：用户在门户/开放 API 自充的订单、用自建密钥产生的
// 调用与任务）。若把 0 当成"没传"，运营点开「平台直连」会拿到全量数据 ——
// 数据看起来正常、结论全错，且没有任何报错，这是最难排查的一类失败。
func appScopeQuery(c *gin.Context) (id uint64, scoped, ok bool) {
	raw := strings.TrimSpace(c.Query("app_id"))
	if raw == "" {
		return 0, false, true
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, false, false
	}
	return v, true, true
}
