package conf

// 余额上限（balance_limit）的口径单源。
//
// 为什么需要这个函数：上限有**两层**来源 ——
//   - 全局配置 `balance_limit`（运营在管理台配的默认值，单位：微）；
//   - 用户级列 `users.balance_limit`（对个别用户的覆盖）。
//
// 这两层曾经在四处各写一份回落逻辑（下单、兑换、人工调账、对上游回显），
// 任一处改口径都会与其它三处静默分叉。
//
// 只保留一条规则、一个实现：**用户级 > 0 优先，否则回落全局**。
// 用户列默认 0（0 = 未单独设置），全局配置是唯一默认真相源。
// 返回 0 表示两层都没配置 —— 调用方据此视为「不限制」。

// BalanceLimitFor 返回某用户生效的余额上限（微）。
func BalanceLimitFor(userCap int64) int64 {
	if userCap > 0 {
		return userCap
	}
	return System().Int("balance_limit", 0)
}
