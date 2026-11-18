package task

import (
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/taskstate"
	"lingdong-api/internal/service/taskview"
)

// 本文件是 task 包对**任务对象**的唯一出口。
//
// 形状定义在 service/taskview（logs 包也要用同一个对象，而 logs 不能 import task：
// relay → logs → task → relay 是已存在的导入环）。这里只做两件事：
//
//  1. 把类型别名出去，本包与各 controller 的调用点不必改名；
//  2. 补上**只有本包知道**的用量口径（tasks.request_params → 计费用量），
//     再交给唯一的投影函数 taskview.ViewOf。
//
// 为什么用量必须由本包填：反解参数要用 billing 的参数解析器，而 logs 包拿到的是
// usage 行上的实测用量。投影函数因此收 ViewUsage，谁都知道自己那份从哪来。
type (
	ViewError    = taskview.ViewError
	ViewArtifact = taskview.ViewArtifact
	ViewUsage    = taskview.ViewUsage
	View         = taskview.View
	PortalItem   = taskview.PortalItem
	AdminView    = taskview.AdminView
	ViewOptions  = taskview.ViewOptions
)

// ViewOf 投影任务对象（提交 202 / 查询详情 / 列表元素 / 取消返回共用）。
func ViewOf(t *model.Task, opt ViewOptions) View {
	opt.Usage = viewUsage(t)
	return taskview.ViewOf(t, opt)
}

// PortalView 把内部任务投影成门户视图（公共字段 + 资金字段）。
//
// contentBase 是**本次请求所在入口**的产物前缀：门户是 `/api/portal/tasks`，
// 应用面（/api/user，Bearer/托管密钥）是 `/api/user/tasks`。写死其中一个会让另一个
// 入口的调用方拿到必然 401 的地址 —— 那不是泄漏，是功能缺陷（两个路由组只认自己的凭证）。
func PortalView(t *model.Task, contentBase string, artifacts []ArtifactEntry) PortalItem {
	return taskview.PortalOf(t, ViewOptions{
		ContentBase: contentBase, Artifacts: artifacts, Usage: viewUsage(t),
	})
}

// AdminViewOf 投影管理台对象（门户字段 + 归属与供应链）。
func AdminViewOf(t *model.Task, contentBase string, artifacts []ArtifactEntry) AdminView {
	return taskview.AdminOf(t, ViewOptions{
		ContentBase: contentBase, Artifacts: artifacts, Usage: viewUsage(t),
	})
}

// ProgressOf 由状态派生进度（排队 0 / 运行 50 / 终态 100）。
func ProgressOf(status string) int { return taskstate.Progress(status) }

// viewUsage 把任务参数快照转成对外用量（唯一转换点）。
func viewUsage(t *model.Task) taskview.ViewUsage {
	u := usageOf(t)
	return taskview.ViewUsage{Images: u.ImageN, Seconds: u.VideoSeconds, RefImages: u.RefImages}
}
