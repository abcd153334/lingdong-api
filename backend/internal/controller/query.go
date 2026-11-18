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
