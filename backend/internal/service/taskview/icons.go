package taskview

import (
	"context"

	"lingdong-api/internal/service/iconindex"
	"lingdong-api/internal/service/iconref"
)

// 本文件只做一件事：给任务视图批量补上模型图标。
//
// 为什么不写进 ViewOf / PortalOf：那两个是**纯内存投影**（不查库），
// 一旦在里面查图标，每个列表页都会变成 N+1（200 条任务 = 200 次查询），
// 而且 View 还被对账、告警、内部日志复用过 —— 那些路径根本不需要图标。
//
// 为什么用泛型而不是两份循环：门户与管理台是两个视图（刻意分层的字段集），
// 各写一遍"抽 id → Resolve → 回填"就是两处会在新增视图时漏掉的入口。

// IconTarget 是「带模型图标」的两个对外视图的公共能力。
//
// 方法名**非导出**：这是包内约定，不允许外部类型混进来 ——
// 混进来的类型不会有对应的 JSON 字段，回填就等于静默丢弃。
// 接口名导出只是为了 task 包能转发 FillModelIcons（controller 只 import task）。
type IconTarget interface {
	modelID() string
	setModelIconURL(string)
}

// PortalItem 通过嵌入 View 获得下面两个方法（方法集会提升），AdminView 再嵌 PortalItem。
func (v *PortalItem) modelID() string          { return v.Model }
func (v *PortalItem) setModelIconURL(s string) { v.ModelIconURL = s }

// FillModelIcons 给一批视图批量补模型图标（一次 IN 查询，见 iconindex）。
//
// 入参用值切片而不是指针切片：调用方本来就是 `[]task.PortalItem`，
// 为了回填再转一遍指针切片只是多一次分配。
func FillModelIcons[T any, PT interface {
	*T
	IconTarget
}](ctx context.Context, items []T, base string) {
	if len(items) == 0 {
		return
	}
	ids := make([]string, 0, len(items))
	for i := range items {
		ids = append(ids, PT(&items[i]).modelID())
	}
	ix := iconindex.Resolve(ctx, ids, nil)
	for i := range items {
		p := PT(&items[i])
		p.setModelIconURL(iconref.URL(base, ix.Model(p.modelID())))
	}
}
