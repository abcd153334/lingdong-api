package controller

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/audit"
	"lingdong-api/internal/service/releasecheck"
	"lingdong-api/internal/service/releaseupdate"
)

// releaseChecker 是版本检查的唯一实例（进程内 15s 冷却 + 进程内缓存）。
//
// 只有一个实例是**必须**的：如果每个请求 new 一个，冷却与缓存都失效，
// "点一下检查"就变成"对第三方站点发一次请求"。实例本身由 releasecheck 包持有，
// 因为后台定时任务（worker/release_check.go）也要往同一个缓存里写 ——
// 两边各持一份的话，定时任务抓到的"有新版本"在管理台上看不见。
var releaseChecker = releasecheck.Shared()

// requestIDRe 是前端一次性请求 ID 的形状（UUID 或同等随机串）。
var requestIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// agentOnlineWindow：心跳每 15s 一次，60s 没更新就是"没装/没起/挂了"。
const agentOnlineWindow = 60 * time.Second

// 更新按钮被禁用的原因（前端只按它决定文案，不自己算规则）。
const (
	blockNone             = ""
	blockAlreadyRunning   = "already_running"
	blockNotConfigured    = "not_configured"
	blockManifestError    = "manifest_error"
	blockVersionUnparsable = "version_unparsable"
	blockNoUpdate         = "no_update"
	blockAgentOffline     = "agent_offline"
)

// AdminReleaseGet 处理 GET /api/admin/system/release。
//
// 一次返回"当前版本 + 清单状态 + 更新程序状态 + 进行中的任务 + 在途任务数"：
// 弹窗需要它们同时出现（少一个就会出现"按钮灰着但不知道为什么"）。
// **不发起任何网络请求** —— 只读缓存；要刷新请调 POST .../check。
func AdminReleaseGet(c *gin.Context) {
	payload, err := buildReleasePayload(c.Request.Context(), false)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, payload)
}

// AdminReleaseCheck 处理 POST /api/admin/system/release/check。
//
// 失败也是一种结果（error_kind），**永远 200**：把"抓不到清单"返回成 500 会让
// 前端只能显示"请求失败"，而真正需要告诉管理员的是"DNS 解析不了 / 不是 https"。
func AdminReleaseCheck(c *gin.Context) {
	payload, err := buildReleasePayload(c.Request.Context(), true)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, payload)
}

// AdminReleaseUpdate 处理 POST /api/admin/system/release/update。
func AdminReleaseUpdate(c *gin.Context) {
	var req struct {
		TargetTag string `json:"target_tag"`
		RequestID string `json:"request_id"`
		Force     bool   `json:"force"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	tag := strings.TrimSpace(req.TargetTag)
	if !releasecheck.ValidTag(tag) {
		response.FailDefMsg(c, errs.InvalidParam, "目标版本格式不合法（应为 vX.Y.Z）")
		return
	}
	if !requestIDRe.MatchString(req.RequestID) {
		response.FailDefMsg(c, errs.InvalidParam, "缺少可用的 request_id")
		return
	}

	// 降级必须显式确认：清单写错一个数字就会把线上换回旧版本，
	// 而"回退"这件事有它自己的流程（回滚分支），不该由一次误点完成。
	// 用 Version（后端自己的版本）而不是前端声称的版本。
	if cmp, ok := releasecheck.Compare(tag, Version); ok && cmp < 0 && !req.Force {
		response.FailDefMsg(c, errs.ReleaseUpdateBlocked,
			"目标版本低于当前版本（"+Version+"）；确定要降级请强制提交")
		return
	}

	svc := releaseupdate.New(db.DB)
	job, err := svc.Submit(c.Request.Context(), releaseupdate.SubmitInput{
		RequestID:   req.RequestID,
		TargetTag:   tag,
		FromVersion: Version,
		AdminID:     c.GetUint64("user_id"),
		Force:       req.Force,
	})
	switch {
	case err == nil:
	case err == releaseupdate.ErrInProgress:
		response.FailDef(c, errs.ReleaseUpdateInProgress)
		return
	case err == releaseupdate.ErrFailedRecently:
		response.FailDefMsg(c, errs.ReleaseUpdateBlocked,
			"同一版本在 10 分钟内失败过；排查后如仍要重试请强制提交")
		return
	default:
		response.FailDef(c, errs.Internal)
		return
	}

	// 请求审计在**提交那一刻**就落库：随后容器会被替换，进程内留痕必丢。
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionReleaseUpdateRequest,
		"release_update", job.TargetTag, gin.H{
			"job_id": job.ID, "from_version": job.FromVersion, "force": req.Force,
		})

	response.OK(c, jobView(job))
}

// AdminReleaseCurrent 处理 GET /api/admin/system/release/update/current。
//
// 前端用它 2s 轮询。**带惰性判死**：agent 挂了不会有人来写"失败"，
// 只能由读路径按心跳兜底（没有常驻循环，也没有定时任务）。
func AdminReleaseCurrent(c *gin.Context) {
	svc := releaseupdate.New(db.DB)
	active, last, err := svc.Current(c.Request.Context())
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	out := gin.H{"server_time": time.Now().UTC().Format(time.RFC3339)}
	if active != nil {
		out["active"] = jobView(active)
	} else {
		out["active"] = nil
	}
	if last != nil {
		out["last"] = jobView(last)
	} else {
		out["last"] = nil
	}
	response.OK(c, out)
}

// AdminReleaseCancel 处理 POST /api/admin/system/release/update/cancel。
//
// 只能取消 queued：进入执行之后"取消"意味着杀掉正在构建/切换的进程 —— 那是另一种事故。
// 没有这个端点时，一次误点 + agent 未装 = 全站 10 分钟不能再次提交（唯一索引占位）。
func AdminReleaseCancel(c *gin.Context) {
	var req struct {
		JobID uint64 `json:"job_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.JobID == 0 {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	svc := releaseupdate.New(db.DB)
	err := svc.Cancel(c.Request.Context(), req.JobID)
	switch {
	case err == nil:
	case err == releaseupdate.ErrNotFound:
		response.FailDef(c, errs.ReleaseNotFound)
		return
	case err == releaseupdate.ErrNotCancellable:
		response.FailDefMsg(c, errs.ReleaseUpdateBlocked, "更新已开始执行，不能取消；如需中止请按运维手册处理")
		return
	default:
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, gin.H{"canceled": true})
}

// buildReleasePayload 组装 GET/CHECK 共用的响应。
//
// check=true 时先抓一次清单；false 只读缓存。两者共用一套组装逻辑，
// 避免"检查完的响应"与"刷新页面的响应"出现字段差异。
func buildReleasePayload(ctx context.Context, check bool) (gin.H, error) {
	var st releasecheck.Status
	if check {
		st = releaseChecker.Check(ctx, true)
	} else {
		st = releaseChecker.Status()
	}

	svc := releaseupdate.New(db.DB)
	active, last, err := svc.Current(ctx)
	if err != nil {
		return nil, err
	}
	agent, err := svc.AgentState(ctx)
	if err != nil {
		return nil, err
	}
	running, err := svc.RunningTasks(ctx)
	if err != nil {
		return nil, err
	}

	manifest := gin.H{
		"configured": st.Configured,
		"url":        maskURL(releasecheck.ManifestURL()),
		"error_kind": st.ErrorKind,
	}
	if !st.CheckedAt.IsZero() {
		manifest["checked_at"] = st.CheckedAt.Format(time.RFC3339)
	}
	if st.HTTPStatus != 0 {
		manifest["http_status"] = st.HTTPStatus
	}

	newer, comparable := false, false
	if st.Latest != nil {
		newer, comparable = releasecheck.IsNewer(st.Latest.Version, Version)
		manifest["latest"] = gin.H{
			"version":      st.Latest.Version,
			"tag":          st.Latest.Tag,
			"published_at": st.Latest.PublishedAt,
			"notes":        st.Latest.Notes,
			"commit":       st.Latest.Commit,
			"url":          st.Latest.URL,
		}
	}
	manifest["update_available"] = st.OK() && newer
	// 清单抓到了，但当前版本不是版本号（本地 dev 构建）：这不是"没有更新"，
	// 是"判断不了"。必须能区分，否则会显示成"已是最新"。
	manifest["comparable"] = comparable

	agentOnline := agent != nil && time.Since(agent.SeenAt) < agentOnlineWindow
	agentView := gin.H{"online": agentOnline}
	if agent != nil {
		agentView["host"] = agent.Host
		agentView["current_tag"] = agent.CurrentTag
		agentView["seen_ago_sec"] = int64(time.Since(agent.SeenAt).Seconds())
	}

	reason := blockNone
	switch {
	case active != nil:
		reason = blockAlreadyRunning
	case !st.Configured:
		reason = blockNotConfigured
	case st.ErrorKind != releasecheck.KindNone:
		reason = blockManifestError
	case !comparable && Version != "" && !releasecheck.ValidVersion(Version):
		reason = blockVersionUnparsable
	case !newer:
		reason = blockNoUpdate
	case !agentOnline:
		reason = blockAgentOffline
	}

	update := gin.H{
		"can_update":     reason == blockNone,
		"blocked_reason": reason,
		"active_job":     nil,
		"last_job":       nil,
	}
	if active != nil {
		update["active_job"] = jobView(active)
	}
	if last != nil {
		update["last_job"] = jobView(last)
	}

	return gin.H{
		"current": gin.H{
			"version": Version,
			"node":    nodeName,
			"mode":    conf.EnvConfig().NodeMode,
		},
		"agent":         agentView,
		"manifest":      manifest,
		"update":        update,
		"running_tasks": running,
	}, nil
}

// jobView 是任务对外的形状。时间统一 RFC3339（前端负责本地化与"多久之前"）。
//
// 不返回 request_id：它是幂等键，对展示没有意义，泄露出去只会让人以为可以重放。
func jobView(job *model.ReleaseUpdateJob) gin.H {
	out := gin.H{
		"id":         job.ID,
		"target_tag": job.TargetTag,
		"status":     job.Status,
		"step":       job.Step,
		"progress":   job.Progress,
		"message":    job.Message,
		"attempt":    job.Attempt,
	}
	if job.FailureCode != "" {
		out["failure_code"] = job.FailureCode
	}
	if job.RollbackResult != "" {
		out["rollback_result"] = job.RollbackResult
	}
	if job.AgentHost != "" {
		out["agent_host"] = job.AgentHost
	}
	if job.StartedAt != nil {
		out["started_at"] = job.StartedAt.UTC().Format(time.RFC3339)
	}
	if job.FinishedAt != nil {
		out["finished_at"] = job.FinishedAt.UTC().Format(time.RFC3339)
	}
	out["requested_at"] = job.RequestedAt.UTC().Format(time.RFC3339)
	return out
}

// maskURL 去掉 query（列表地址可能带签名参数，不该出现在页面上与日志里）。
func maskURL(raw string) string {
	if raw == "" {
		return ""
	}
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		return raw[:i]
	}
	return raw
}
