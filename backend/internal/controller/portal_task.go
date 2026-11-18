package controller

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/pkg/siteurl"
	"lingdong-api/internal/pkg/sorting"
	"lingdong-api/internal/service/task"
	"lingdong-api/internal/service/taskview"
)

// taskSortColumns 是任务列表的排序白名单：**对外字段名 → SQL 列**。
//
// 键集合与 apicontract `tasks.list` 的 Sortable 逐字一致（由门禁比对），
// 所以这里多一个键就多一个能扫全表的列，少一个键就是文档在骗人。
var taskSortColumns = map[string]string{
	"created_at": "created_at",
}

// PortalTaskList 处理 GET /api/portal/tasks：当前用户的任务（T21 分页 + 状态/模型筛选）。
//
// key_id 是可选的 KEY 维度筛选：应用侧只有一把托管密钥，但仍统一按 KEY 过滤，
// 这样"同一个用户在应用 A 与平台直连各自的消费"能分开看（不加参数时行为不变）。
func PortalTaskList(c *gin.Context) {
	uid := c.GetUint64("user_id")
	p, err := pagination.Parse(c)
	if err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	keyID, ok := optionalUintQuery(c, "key_id")
	if !ok {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	// 托管密钥强制只看本 KEY 的任务（覆盖查询参数，不是"默认值"）。
	if kid, _, hosted := keyScopeOf(c); hosted {
		keyID = kid
	}
	sortOrd, ok := sorting.Query(c, taskSortColumns)
	if !ok {
		return
	}
	rows, total, err := task.List(c.Request.Context(), task.Scope{UserID: uid, KeyID: keyID}, task.ListFilter{
		Status: strings.TrimSpace(c.Query("status")), Model: strings.TrimSpace(c.Query("model")),
		Page: p.Page, PageSize: p.PageSize,
		OrderBy: sortOrd.ClauseOr("id DESC"),
	})
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	// 产物一次性取回（一条 IN 查询），否则每行都要查一次 files 才能给出 result_url。
	ids := make([]uint64, 0, len(rows))
	for i := range rows {
		ids = append(ids, rows[i].ID)
	}
	arts, err := task.ArtifactRowsByTask(c.Request.Context(), ids)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	items := make([]task.PortalItem, 0, len(rows))
	base := taskContentBase(c)
	for i := range rows {
		items = append(items, task.PortalView(&rows[i], base, arts[rows[i].ID]))
	}
	task.FillModelIcons(c.Request.Context(), items, siteurl.Base(c))
	response.OK(c, pagination.New(items, total, p))
}

// PortalTaskGet 处理 GET /api/portal/tasks/{id}：当前用户的单个任务详情。
func PortalTaskGet(c *gin.Context) {
	t, ok := portalTaskOf(c)
	if !ok {
		return
	}
	v, verr := portalTaskView(c, t)
	if verr != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, v)
}

// portalTaskOf 按对外任务号取当前用户的任务；失败时已写响应。
func portalTaskOf(c *gin.Context) (*model.Task, bool) {
	ref, ok := publicIDParam(c, "id")
	if !ok {
		response.FailDef(c, errs.InvalidParam)
		return nil, false
	}
	t, err := task.GetByRef(c.Request.Context(), ref, taskScopeOf(c))
	if err != nil {
		response.FailDef(c, failDefOf(err))
		return nil, false
	}
	return t, true
}

// portalTaskView 把任务行投影成门户对象（详情与取消共用）。
//
// 单独抽出来是因为"详情返回整对象、取消只返回 {id,status}"曾经是两份形状：
// 客户端为同一件事写两个解析分支。投影只有一处，两者自然一致。
//
// 产物查询失败必须上抛（→ 500）而不是静默给空数组：contents=[] 与"产物已过期"
// 在客户端是同一个渲染结果，静默降级会让一次 DB 抖动看起来像"我的图没了"。
func portalTaskView(c *gin.Context, t *model.Task) (task.PortalItem, error) {
	// 产物清单只在单条响应里给（列表给会变成 N+1）。音乐一次出 2 首，缺了它第二首没有入口。
	// 同一份清单既用于 Contents，也用于第 0 条的地址，不查两次。
	arts, err := task.ArtifactsOf(c.Request.Context(), t.ID)
	if err != nil {
		return task.PortalItem{}, err
	}
	views := []task.PortalItem{task.PortalView(t, taskContentBase(c), arts)}
	task.FillModelIcons(c.Request.Context(), views, siteurl.Base(c))
	return views[0], nil
}

// PortalTaskCancel 处理 POST /api/portal/tasks/{id}/cancel：仅 queued 且未提交上游可取消并全额释放。
func PortalTaskCancel(c *gin.Context) {
	t, ok := portalTaskOf(c)
	if !ok {
		return
	}
	scope := taskScopeOf(c)
	if err := task.Cancel(c.Request.Context(), t.ID, scope); err != nil {
		response.FailDef(c, failDefOf(err))
		return
	}
	// 取消也返回**同一个任务对象**（不是 {id,status} 摘要）：客户端不该为
	// "取消成功"再写一个解析分支。重查是必要的 —— Cancel 只改状态与账本，
	// 内存里那行还是取消前的快照。
	t, err := task.Get(c.Request.Context(), t.ID, scope)
	if err != nil {
		response.FailDef(c, failDefOf(err))
		return
	}
	v, verr := portalTaskView(c, t)
	if verr != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, v)
}

// PortalTaskContentAt 处理 GET /api/portal/tasks/{id}/contents/{index}：按序号取产物。
//
// 为什么必须带序号：音乐一次任务出 2 首、图像任务也可能多产物，只提供 index=0
// 会让第 2 个及以后的产物在门户完全没有下载入口。
// 序号是**稠密编号**（0..n-1），与 task.ArtifactsOf 的重编号口径一致。
func PortalTaskContentAt(c *gin.Context) {
	taskContentAt(c, serveTaskArtifact)
}

// PortalTaskContentAtForAPI 是对外面（`/api/user`）的同能力端点：
// 归属校验与序号解析完全共用，只有失败响应的形状不同（平台信封）。
func PortalTaskContentAtForAPI(c *gin.Context) {
	taskContentAt(c, serveTaskArtifactForAPI)
}

// taskContentAt 是产物按序号取回的**唯一**实现（解析 + 归属校验 + 交给 serve）。
//
// 参数化的是"怎么输出失败"，不是整个流程：解析与归属判据必须只有一份，
// 否则两条路径迟早有一条忘了校验 user_id（那就是越权读别人的产物）。
func taskContentAt(c *gin.Context, serve func(*gin.Context, *model.Task, int)) {
	// 序号必须允许 0（uintParam 会拒 0，这里不能用它）。
	idx, err := strconv.Atoi(strings.TrimSpace(c.Param("index")))
	if err != nil || idx < 0 {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	t, ok := portalTaskOf(c)
	if !ok {
		return
	}
	serve(c, t, idx)
}

// taskScopeOf 返回任务查询的作用域（托管密钥只覆盖本 KEY 的任务）。
func taskScopeOf(c *gin.Context) task.Scope {
	scope := task.Scope{UserID: c.GetUint64("user_id")}
	if kid, _, hosted := keyScopeOf(c); hosted {
		scope.KeyID = kid
	}
	return scope
}

// taskContentBase 返回「本次请求所在入口」的产物前缀。
//
// 同一个 handler 同时挂在 /api/portal/tasks（会话 Cookie）与 /api/user/tasks
// （Bearer/托管密钥）下，两边只认各自的凭证。产物地址必须跟着入口走，
// 否则应用面拿到的 result_url 指回门户路由组，点开必然 401。
func taskContentBase(c *gin.Context) string {
	return taskview.ContentBaseFor(c.FullPath())
}
