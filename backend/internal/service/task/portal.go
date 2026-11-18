package task

import "lingdong-api/internal/pkg/taskstate"

// 本文件是**用户端**任务视图的入口说明。
//
// 形状与投影函数在 service/taskview（唯一实现）；PortalView / AdminViewOf 的包装在
// view.go。门户、管理台、应用面下发的是同一个 View，只是外层字段多寡不同 ——
// 后台专属字段（channel_id / platform / upstream_task_id / error_msg）单独一层，
// 永不下发门户。

// PortalTaskError 把任务终态映射成对用户可读的分类文案。
//
// 文案只有 taskstate.PortalError 一处：调用记录详情（logs 包）也要用同一句，
// 两处各写一份必然出现「任务页说已全额返还、调用记录里是上游原文」。
func PortalTaskError(status string) string {
	return taskstate.PortalError(status)
}
