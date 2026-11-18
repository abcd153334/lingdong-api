package controller

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/gateway"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/mediaurl"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/pkg/siteurl"
	"lingdong-api/internal/pkg/taskstate"
	"lingdong-api/internal/relay"
	"lingdong-api/internal/service/task"
	"lingdong-api/internal/service/taskshell"
	"lingdong-api/internal/service/taskview"
)

// 任务列表的 limit 边界（与 OpenAI 风格的 1..100 对齐）。
const (
	taskListLimitDef = 20
	taskListLimitMax = 100
)

// GatewayTaskList 处理 GET /v1/tasks：本 KEY 的任务列表（游标分页）。
//
// 每个元素与单条查询**同一个结构**（taskshell.TaskFlat）：列表里再嵌一份
// 带产物的富对象，就会出现「列表有 contents、详情要另查」的双口径。
// 产物统一走 /v1/tasks/{id}/artifacts。
func GatewayTaskList(c *gin.Context) {
	k := relay.APIKeyOf(c)
	if k == nil {
		gateway.FailDef(c, errs.InvalidAPIKey)
		return
	}
	limit, ok := parseTaskLimit(c.Query("limit"))
	if !ok {
		gateway.FailDef(c, errs.InvalidParam)
		return
	}
	// after 是对外任务号（不是数字 id）：游标的键必须与 first_id/last_id 同形，
	// 否则客户端照 last_id 回填就必然 400。
	var after uint64
	if raw := strings.TrimSpace(c.Query("after")); raw != "" {
		v, rerr := task.ResolveRef(c.Request.Context(), raw)
		if rerr != nil {
			gateway.FailDef(c, errs.InvalidParam)
			return
		}
		after = v
	}

	// 多取一条用于判断 has_more：靠 total 判断在有游标时会算「剩余总数」，
	// 语义与「本页之后还有没有」不一致，客户端会提前停止翻页。
	// 筛选词与响应词同一套（SUBMITTED/IN_PROGRESS/SUCCESS/FAILURE）：
	// 让客户端"看到什么就能筛什么"，不要另教一套内部小写词。
	var statusFilter string
	if raw := strings.TrimSpace(c.Query("status")); raw != "" {
		st, ok := taskstate.ParseWord(taskstate.DialectNewAPI, raw)
		if !ok {
			gateway.FailDef(c, errs.InvalidParam)
			return
		}
		statusFilter = st
	}
	rows, total, err := task.List(c.Request.Context(), task.Scope{KeyID: k.ID}, task.ListFilter{
		Status:   statusFilter,
		Model:    strings.TrimSpace(c.Query("model")),
		Modality: strings.TrimSpace(c.Query("modality")),
		PageSize: limit + 1,
		After:    after,
	})
	if err != nil {
		gateway.FailDef(c, errs.Internal)
		return
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}

	// 不再批量查产物：扁平结构里没有产物字段，多查一次 IN 只是给分页加延迟。
	items := make([]taskshell.TaskFlat, 0, len(rows))
	for i := range rows {
		items = append(items, taskshell.FlatOf(&rows[i]))
	}
	out := gin.H{
		"object":   "list",
		"data":     items,
		"has_more": hasMore,
		"total":    total,
	}
	if len(items) > 0 {
		out["first_id"] = items[0].ID
		out["last_id"] = items[len(items)-1].ID
	}
	gateway.OK(c, out)
}

// GatewayTaskGet 处理 GET /v1/tasks/{id}：单任务（new-api 扁平结构）。
func GatewayTaskGet(c *gin.Context) {
	t, ok := gatewayTaskOf(c)
	if !ok {
		return
	}
	gateway.OK(c, taskshell.FlatOf(t))
}

// GatewayTaskArtifacts 处理 GET /v1/tasks/{id}/artifacts：产物清单。
//
// 为什么产物从任务对象里拆出来：new-api 就是这么分的，而且拆开之后
// 「任务状态」与「产物列表」各有各的刷新节奏（轮询状态不必每次拖回几十条产物）。
func GatewayTaskArtifacts(c *gin.Context) {
	t, ok := gatewayTaskOf(c)
	if !ok {
		return
	}
	arts, err := task.ArtifactsOf(c.Request.Context(), t.ID)
	if err != nil {
		gateway.FailDef(c, errs.Internal)
		return
	}
	out := taskshell.ArtifactList{TaskID: t.PublicID, Artifacts: make([]taskshell.Artifact, 0, len(arts))}
	for _, a := range arts {
		out.Artifacts = append(out.Artifacts, taskshell.Artifact{
			Key:        strconv.Itoa(a.Index),
			Type:       a.Kind,
			MimeType:   a.ContentType,
			ContentURL: artifactContentURL(c, t, a.Index),
		})
	}
	gateway.OK(c, out)
}

// GatewayTaskArtifactContent 处理 GET /v1/tasks/{id}/artifacts/{key}/content：按序号取字节。
//
// key 就是稠密序号（"0" 起）。稀疏编号会让「第 0 个下载失败」与「第 1 个变成
// 第 0 个」在客户端看来完全一样，无法区分「少了」还是「换了」。
func GatewayTaskArtifactContent(c *gin.Context) {
	t, ok := gatewayTaskOf(c)
	if !ok {
		return
	}
	idx, ok := artifactIndex(c.Param("key"))
	if !ok {
		gateway.FailDef(c, errs.InvalidParam)
		return
	}
	serveTaskArtifact(c, t, idx)
}

// artifactIndex 把产物 key 归一成稠密序号。
//
// 序号是**唯一**生成器（清单里也只下发 "0"/"1"…）：稀疏编号会让客户端分不清
// 「少了」还是「换了」。允许 "video" 只是因为兼容协议里单产物视频用这个别名，
// 它恒等于第 0 个 —— 是别名而不是第二套编号。
func artifactIndex(key string) (int, bool) {
	k := strings.TrimSpace(key)
	if k == "video" {
		return 0, true
	}
	idx, err := strconv.Atoi(k)
	if err != nil || idx < 0 {
		return 0, false
	}
	return idx, true
}

// GatewayVideoTaskGet 处理 GET /v1/video/generations/{id}：new-api 的查询封套。
//
// 结构与 /v1/tasks/{id} 完全一致，只是多一层 {code,message,data} 并附上
// result_url —— 这两个是 new-api 视频查询壳自己的协议形状，不是第二套任务结构。
func GatewayVideoTaskGet(c *gin.Context) {
	t, ok := gatewayTaskOf(c)
	if !ok {
		return
	}
	flat := taskshell.FlatOf(t)
	if arts, err := task.ArtifactsOf(c.Request.Context(), t.ID); err == nil && len(arts) > 0 {
		// new-api 的 result_url 是「主产物」：第 0 件。完整清单在 /artifacts。
		flat.ResultURL = artifactContentURL(c, t, arts[0].Index)
	}
	gateway.OKNewAPI(c, flat)
}

// GatewayTaskCancel 处理 DELETE /v1/tasks/{id}：取消尚未提交上游的任务。
//
// 终态任务刻意**不物理删除**：对账需要它（删除由管理台保留策略负责）。
func GatewayTaskCancel(c *gin.Context) {
	k := relay.APIKeyOf(c)
	if k == nil {
		gateway.FailDef(c, errs.InvalidAPIKey)
		return
	}
	t, ok := gatewayTaskOf(c)
	if !ok {
		return
	}
	if err := task.Cancel(c.Request.Context(), t.ID, task.Scope{KeyID: k.ID}); err != nil {
		gateway.FailDef(c, failDefOf(err))
		return
	}
	// 取消也返回**同一个任务对象**：客户端不需要为"取消成功"写第二个解析分支。
	// 重查一次是必要的 —— Cancel 只返回错误，状态以库里的最终值为准。
	got, gerr := task.Get(c.Request.Context(), t.ID, task.Scope{KeyID: k.ID})
	if gerr != nil {
		gateway.FailDef(c, failDefOf(gerr))
		return
	}
	gateway.OK(c, taskshell.FlatOf(got))
}

// artifactContentURL 是产物地址的**唯一构造点**。
//
// 能签就签成能力 URL（免鉴权）：<img>/<video>/下载器直接可用，接入方不必为
// 「预览」和「下载」写两条分支。签不出来（没配 APP_ENCRYPTION_KEY 或推不出
// 对外基址）就回落到带鉴权的正式端点 —— 不报错、也不给一个必然 403 的地址。
func artifactContentURL(c *gin.Context, t *model.Task, idx int) string {
	if link, ok := mediaurl.ArtifactLink(siteurl.Base(c), t.ID, idx, mediaurl.TTL()); ok {
		return link
	}
	return absURL(c, taskview.PathGateway+"/"+t.PublicID+"/artifacts/"+strconv.Itoa(idx)+"/content")
}

// findByKeyParam 按路径 id + KEY 归属取任务（不存在/越权一律 ErrNotFound，不泄露存在性）。
func findByKeyParam(c *gin.Context, k *model.APIKey) (*model.Task, error) {
	ref := strings.TrimSpace(c.Param("id"))
	if !task.IsPublicID(ref) {
		return nil, task.ErrNotFound
	}
	return task.GetByRef(c.Request.Context(), ref, task.Scope{KeyID: k.ID})
}

// gatewayTaskOf 按路径 id + KEY 归属取任务；失败时已写响应，调用方直接 return。
func gatewayTaskOf(c *gin.Context) (*model.Task, bool) {
	k := relay.APIKeyOf(c)
	if k == nil {
		gateway.FailDef(c, errs.InvalidAPIKey)
		return nil, false
	}
	t, err := findByKeyParam(c, k)
	if err != nil {
		gateway.FailDef(c, failDefOf(err))
		return nil, false
	}
	return t, true
}

// parseTaskLimit 解析 limit：缺省 20，范围 1..100，非法直接报参数错（不静默夹取）。
func parseTaskLimit(raw string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return taskListLimitDef, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > taskListLimitMax {
		return 0, false
	}
	return n, true
}

// serveTaskArtifact 返回任务产物内容（`/v1` 与门户共用，错误体为 OpenAI 形状）。
//
// 顺序：已转存产物 → （仅 idx=0）实时回源上游 → 410。
// 多产物**不**做回源：上游只给一个「主链接」，其余产物没有稳定入口，
// 强行按序号猜链接会把 410 变成 502，用户更难判断。
func serveTaskArtifact(c *gin.Context, t *model.Task, idx int) {
	serveArtifact(c, t, idx, gateway.FailDef)
}

// serveTaskArtifactForAPI 是同一个实现的**平台信封**版本（`/api/user` 用）。
//
// 为什么必须区分：同一个客户端 SDK 如果控制面收 `{code,message,error_code,data}`、
// 二进制端点收 `{error:{...}}`，就得写两套错误解析；而二进制端点恰恰是最容易
// 出错的地方（客户端通常直接把 body 当文件写盘，错误信封会被存成一个坏文件）。
//
// 刻意做成"一个实现 + 两个失败写法"，而不是复制一份：产物读取涉及 MIME 白名单、
// nosniff、Range/206 三段安全逻辑，复制一份就一定会有一份先腐坏。
func serveTaskArtifactForAPI(c *gin.Context, t *model.Task, idx int) {
	serveArtifact(c, t, idx, response.FailDef)
}

// serveArtifact 是产物输出的唯一实现。
//
// 唯一可变量是**失败响应怎么写**（fail），成功路径（字节流 / 206 / 白名单 MIME）
// 完全一致 —— 所以 `/v1` 的响应在改造前后逐字节相同。
func serveArtifact(c *gin.Context, t *model.Task, idx int, fail func(*gin.Context, errs.Def)) {
	ctx := c.Request.Context()
	start, length, ranged := parseRangeHeader(c.GetHeader("Range"))

	// HEAD 只报元信息：不走 OpenArtifactAt（那会把整个对象读一遍，见 task.HeadArtifact 注释）。
	if c.Request.Method == http.MethodHead {
		size, ct, herr := task.HeadArtifact(ctx, t, idx)
		if herr != nil {
			fail(c, headFailDefOf(herr))
			return
		}
		writeArtifactMeta(c, size, ct)
		return
	}

	stream, err := task.OpenArtifactAt(ctx, t, idx, start, length)
	if err != nil {
		if errors.Is(err, task.ErrRangeNotSatisfiable) {
			if stream != nil && stream.Total > 0 {
				c.Header("Content-Range", fmt.Sprintf("bytes */%d", stream.Total))
			}
			fail(c, errs.RangeNotSatisfiable)
			return
		}
		// 回源的唯一条件：该任务**从未转存过任何产物**（artifact_count == 0）。
		//
		// 旧实现的条件是「本地打开失败」—— 过期、对象缺失、存储抖动都会命中，
		// 于是产物过了保留期仍能从上游拿到文件。过期必须是终态。
		if errors.Is(err, task.ErrContentNotStored) {
			// 按 idx 回源：多产物任务的清单里本来就有全部链接（零出站），
			// 硬编码 0 会让第 2 件起永远拿不到 —— 而用户明明拿到了 task_id 与序号。
			body, size, ct, oerr := task.OpenUpstreamArtifact(ctx, t, idx)
			if oerr == nil {
				defer body.Close()
				writeArtifact(c, body, size, ct, 0, -1, false)
				return
			}
			// 回源也拿不到：任务还没跑完（或上游链接已失效）。409 让客户端继续轮询，
			// 410 会让它当场放弃 —— 而 30 秒后产物本来就会出现。
			fail(c, errs.ArtifactNotReady)
			return
		}
		// 四类失败各自一个码：写错序号(404) / 已过期(410) / 后端配置缺失(503) / 其它(500)。
		// 折叠成同一个码时，运维分不清「用户查错了」与「我们把桶配错了」。
		fail(c, artifactFailDefOf(err))
		return
	}
	defer stream.Body.Close()
	writeArtifact(c, stream.Body, stream.Size, stream.ContentType, start, stream.Total, ranged)
}

// artifactFailDefOf 把产物读取的失败归一成对外码（GET 与 HEAD 共用这一张表）。
//
// 规则只有四条，对应四种处置动作：重新取清单(404) / 继续轮询(409) /
// 别再试(410) / 报障(503) / 联系我们(500)。
func artifactFailDefOf(err error) errs.Def {
	switch {
	case errors.Is(err, task.ErrArtifactNotFound):
		return errs.ArtifactNotFound
	case errors.Is(err, task.ErrStorageUnavailable):
		return errs.StorageBackendMissing
	case errors.Is(err, task.ErrContentNotStored):
		return errs.ArtifactNotReady
	case errors.Is(err, task.ErrContentGone):
		return errs.FileGone
	default:
		// 数据库故障等：不是「产物没了」，而是我们这边出错。
		return errs.Internal
	}
}

// headFailDefOf 把 HEAD 路径的失败归一成对外码。
//
// 与 GET 的差别只有一处：HEAD 不产生 416（它不处理 Range，也没有 body），
// 其余（404 无此序号 / 409 未就绪 / 410 已过期 / 503 后端缺失）与 GET 同源。
func headFailDefOf(err error) errs.Def { return artifactFailDefOf(err) }

// writeArtifactMeta 输出 HEAD 的元信息（不读字节）。
//
// Content-Type 走与 GET 完全相同的白名单归一：HEAD 与 GET 给出不同的
// Content-Type 时，抓图器会认为「这个地址不是图片」，直接放弃下载。
func writeArtifactMeta(c *gin.Context, size int64, contentType string) {
	safeType, inline := safeArtifactContentType(contentType)
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "private, max-age=300")
	if inline {
		c.Header("Content-Disposition", "inline")
	} else {
		c.Header("Content-Disposition", "attachment")
	}
	c.Header("Accept-Ranges", "bytes")
	if size > 0 {
		c.Header("Content-Length", strconv.FormatInt(size, 10))
	}
	c.Header("Content-Type", safeType)
	c.Status(http.StatusOK)
}

// writeArtifact 输出产物字节流（支持 206 区间响应）。
//
// 安全约束（P1-13）：上游/插件返回的 Content-Type 一律过白名单，
// 不在白名单内的统一降级为 application/octet-stream + attachment，
// 并始终带 nosniff —— 否则 `text/html` 产物会在同源下执行脚本（存储型 XSS）。
func writeArtifact(c *gin.Context, body io.Reader, size int64, contentType string, start, total int64, ranged bool) {
	safeType, inline := safeArtifactContentType(contentType)
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "private, max-age=300")
	if inline {
		c.Header("Content-Disposition", "inline")
	} else {
		c.Header("Content-Disposition", "attachment")
	}
	if total > 0 {
		c.Header("Accept-Ranges", "bytes")
	}
	if ranged && total > 0 && size >= 0 && start < total {
		end := start + size - 1
		if end >= total {
			end = total - 1
		}
		c.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		c.DataFromReader(http.StatusPartialContent, end-start+1, safeType, body, nil)
		return
	}
	if size <= 0 {
		size = -1
	}
	c.DataFromReader(http.StatusOK, size, safeType, body, nil)
}

// safeArtifactContentType 归一产物 Content-Type，返回「可安全内联的类型」与是否内联。
//
// image/svg+xml 刻意**不**内联：SVG 可内嵌 <script>，内联渲染等于把产物变成可执行页面。
func safeArtifactContentType(ct string) (string, bool) {
	base := strings.ToLower(strings.TrimSpace(ct))
	if i := strings.IndexByte(base, ';'); i >= 0 {
		base = strings.TrimSpace(base[:i])
	}
	switch {
	case base == "image/svg+xml":
		return "application/octet-stream", false
	case strings.HasPrefix(base, "image/"), strings.HasPrefix(base, "video/"), strings.HasPrefix(base, "audio/"):
		return base, true
	case base == "text/plain":
		return "text/plain; charset=utf-8", true
	default:
		// application/octet-stream、未知类型、以及 html/js/pdf 等一律按附件下载。
		return "application/octet-stream", false
	}
}

// parseRangeHeader 解析单段 Range：`bytes=start-end`、`bytes=start-`。
//
// 多段（`bytes=0-1,5-6`）与后缀式（`bytes=-500`）一律忽略并回落 200 全量：
// 前者需要 multipart/byteranges 响应，后者需要对象总长度（只有存储层知道），
// 两者都不值得为「播放器拖动」这一场景引入额外复杂度。
func parseRangeHeader(raw string) (start, length int64, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) < 7 || !strings.EqualFold(raw[:6], "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimSpace(raw[6:])
	if spec == "" || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	i := strings.IndexByte(spec, '-')
	if i < 0 {
		return 0, 0, false
	}
	startRaw := strings.TrimSpace(spec[:i])
	endRaw := strings.TrimSpace(spec[i+1:])
	if startRaw == "" {
		return 0, 0, false
	}
	s, err := strconv.ParseInt(startRaw, 10, 64)
	if err != nil || s < 0 {
		return 0, 0, false
	}
	if endRaw == "" {
		return s, 0, true
	}
	e, err := strconv.ParseInt(endRaw, 10, 64)
	if err != nil || e < s {
		return 0, 0, false
	}
	return s, e - s + 1, true
}
