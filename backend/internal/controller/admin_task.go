package controller

import (
	"errors"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/pkg/siteurl"
	"lingdong-api/internal/service/audit"
	"lingdong-api/internal/service/task"
	"lingdong-api/internal/service/taskview"
)

// AdminTaskList 处理 GET /api/admin/tasks：全量任务（T21 分页 + 状态/模型/用户筛选）。
func AdminTaskList(c *gin.Context) {
	p, err := pagination.Parse(c)
	if err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	f := task.ListFilter{
		Status: strings.TrimSpace(c.Query("status")), Model: strings.TrimSpace(c.Query("model")),
		Page: p.Page, PageSize: p.PageSize,
	}
	if v := strings.TrimSpace(c.Query("user_id")); v != "" {
		n, perr := strconv.ParseUint(v, 10, 64)
		if perr != nil {
			response.FailDef(c, errs.InvalidParam)
			return
		}
		f.UserID = n
	}
	rows, total, err := task.List(c.Request.Context(), task.AllScope(), f)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	items := make([]task.AdminView, 0, len(rows))
	for i := range rows {
		// 列表不下发产物（每条都要查一次 files = N+1），只给空数组。
		items = append(items, task.AdminViewOf(&rows[i], taskview.ContentBaseFor(c.FullPath()), nil))
	}
	// 图标在 handler 层补（View 不查库）：一次 IN 覆盖整页。
	task.FillModelIcons(c.Request.Context(), items, siteurl.Base(c))
	response.OK(c, pagination.New(items, total, p))
}

// adminTaskOf 按对外任务号取任务；失败时已写响应。
//
// 形态错（不是 task_ 号）与查不到分开：前者是参数错，后者才是 404。
func adminTaskOf(c *gin.Context) (*model.Task, bool) {
	ref, ok := publicIDParam(c, "id")
	if !ok {
		response.FailDef(c, errs.InvalidParam)
		return nil, false
	}
	t, err := task.GetByRef(c.Request.Context(), ref, task.AllScope())
	if err != nil {
		response.FailDef(c, failDefOf(err))
		return nil, false
	}
	return t, true
}

// AdminTaskGet 处理 GET /api/admin/tasks/{id}：任务详情（含产物清单）。
//
// 列表接口不下发 contents（每条都要查一次 file 表 = N+1），运营点开详情时才补，
// 这样音乐任务的两个产物都能在管理台逐个下载。
func AdminTaskGet(c *gin.Context) {
	t, ok := adminTaskOf(c)
	if !ok {
		return
	}
	v, verr := adminTaskView(c, t)
	if verr != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, v)
}

// adminTaskView 把任务行投影成管理台对象（详情与取消共用）。
//
// 产物查询失败必须上抛（→ 500）：空 contents 在界面上就是"这个任务没有产物"，
// 静默降级会让一次 DB 抖动看起来像"上游没给图"。
func adminTaskView(c *gin.Context, t *model.Task) (task.AdminView, error) {
	// 单条也走同一个填充函数（传 1 元素切片）：详情页最需要认出模型，
	// 而"列表有图标、点进去没有"正是两套填充逻辑的典型症状。
	arts, err := task.ArtifactsOf(c.Request.Context(), t.ID)
	if err != nil {
		return task.AdminView{}, err
	}
	views := []task.AdminView{task.AdminViewOf(t, taskview.ContentBaseFor(c.FullPath()), arts)}
	task.FillModelIcons(c.Request.Context(), views, siteurl.Base(c))
	return views[0], nil
}

// AdminTaskContentAt 处理 GET /api/admin/tasks/{id}/contents/{index}：按序号下载产物（运营排障用）。
func AdminTaskContentAt(c *gin.Context) {
	idx, ierr := strconv.Atoi(strings.TrimSpace(c.Param("index")))
	if ierr != nil || idx < 0 {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	t, ok := adminTaskOf(c)
	if !ok {
		return
	}
	serveTaskArtifact(c, t, idx)
}

// AdminTaskCancel 处理 POST /api/admin/tasks/{id}/cancel：管理员取消 queued 任务并全额释放预扣。
func AdminTaskCancel(c *gin.Context) {
	t, ok := adminTaskOf(c)
	if !ok {
		return
	}
	if err := task.Cancel(c.Request.Context(), t.ID, task.AllScope()); err != nil {
		response.FailDef(c, failDefOf(err))
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionTaskCancel, "task", t.PublicID,
		gin.H{"reason": "管理员取消异步任务"})
	// 取消返回同一个任务对象（与 /v1/tasks/{id} 的 DELETE 同形状）：
	// 运营点完"取消"看到的就是取消后的行，不用再拉一次详情。
	t, err := task.Get(c.Request.Context(), t.ID, task.AllScope())
	if err != nil {
		response.FailDef(c, failDefOf(err))
		return
	}
	v, verr := adminTaskView(c, t)
	if verr != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, v)
}

// AdminTaskRetry 处理 POST /api/admin/tasks/{id}/retry：管理员重试 failed/timeout 任务
// （重新冻结 + 同步重提交上游）。非 failed/timeout → 409 task_not_retryable（幂等）。
func AdminTaskRetry(c *gin.Context) {
	t, ok := adminTaskOf(c)
	if !ok {
		return
	}
	adminID := c.GetUint64("user_id")
	report, err := task.Retry(c.Request.Context(), t.ID, task.AllScope())
	if err != nil {
		if errors.Is(err, task.ErrNotRetryable) || errors.Is(err, task.ErrInvalidParam) {
			// 带上具体原因：「不可重试」与「模型已下架」「含内联素材无法重放」对运营是三件事。
			def := errs.TaskNotRetryable
			if errors.Is(err, task.ErrInvalidParam) {
				def = errs.InvalidParam
			}
			response.FailDef(c, defWithReason(def, err))
			return
		}
		response.FailDef(c, failDefOf(err))
		return
	}
	audit.RecordOrWarn(db.DB, adminID, audit.ActionTaskRetry, "task", t.PublicID,
		gin.H{"reason": "管理员重试异步任务"})
	// 按当前价目重新快照是一次**改价生效**：必须单独留痕，否则「为什么这次重试扣得更多」
	// 只能靠翻日志。只在价目真的变过时写，正常重试不会多出一条审计。
	if report != nil && report.Resnapshot {
		audit.RecordOrWarn(db.DB, adminID, audit.ActionTaskPricingResnapshot, "task",
			t.PublicID, gin.H{
				"reason":        "重试按当前价目重新快照",
				"before_frozen": report.BeforeFrozen,
				"after_frozen":  report.AfterFrozen,
			})
	}
	// 重试同样返回整对象（与取消一致）：`{id,status}` 摘要等于给客户端
	// 再开一个"这个接口返回什么"的分支，而它要的其实就是重试后的那一行。
	t, gerr := task.Get(c.Request.Context(), t.ID, task.AllScope())
	if gerr != nil {
		response.FailDef(c, failDefOf(gerr))
		return
	}
	v, verr := adminTaskView(c, t)
	if verr != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, v)
}

// AdminTaskPurge 处理 DELETE /api/admin/tasks/{id}?purge=true：物理删除终态任务与产物（B7 §2.10）。
//
// 三个刻意的设计：
//  1. **必须显式带 `purge=true`**：同一个 URL 上的 DELETE 语义太容易被误触发
//     （curl 脚本、误点的客户端），而这里是真删数据，不存在撤销；
//  2. **仅终态**：queued/running 的任务还占着预扣额度，删掉任务行会让冻结的钱
//     变成查不到来源的挂账；
//  3. **先删对象再删行**（在 service 层实现）：对象删不掉就保留任务行，等下一轮重试 ——
//     否则会留下「行没了、对象还在」的孤儿，除了手工扫桶再也发现不了。
//
// 与保留策略（LD_TASK_RETENTION_DAYS）的区别：本接口是**单条人工操作**，必须写审计；
// 保留策略是批量后台动作，只落日志与指标。
func AdminTaskPurge(c *gin.Context) {
	// 只认 `true` / `1`，其余一律拒绝：`?purge=` 或 `?purge=false` 都不该删数据。
	if !purgeFlagOf(c) {
		response.FailDef(c, defWithReason(errs.InvalidParam,
			errors.New("物理删除必须显式传 purge=true")))
		return
	}
	t, ok := adminTaskOf(c)
	if !ok {
		return
	}
	files, bytes, perr := task.PurgeTask(c.Request.Context(), t)
	if perr != nil {
		response.FailDef(c, failDefOf(perr))
		return
	}
	// 审计要能回答「删的是谁的任务、什么状态、多大体积」——只记 id 无法事后追责。
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionTaskPurge, "task",
		t.PublicID, gin.H{
			"reason":     "管理员物理删除任务与产物",
			"user_id":    t.UserID,
			"key_id":     t.KeyID,
			"model":      t.Model,
			"status":     t.Status,
			"files":      files,
			"bytes":      bytes,
			"usage_id":   t.UsageID,
			"channel_id": t.ChannelID,
		})
	response.OKMsg(c, "任务已删除", gin.H{"id": t.PublicID, "files": files, "bytes": bytes})
}

// purgeFlagOf 判定请求是否**显式**要求物理删除（唯一接受的写法：`true` / `1`）。
//
// 单独成函数是为了能直接测：把它内联在 handler 里，任何「合法值」用例都会
// 继续走到 task.Get（测试环境没有数据库），从而只能靠 panic 恢复来区分，
// 反而测不准「参数是否被正确接受」。
func purgeFlagOf(c *gin.Context) bool {
	switch strings.ToLower(strings.TrimSpace(c.Query("purge"))) {
	case "true", "1":
		return true
	default:
		return false
	}
}
