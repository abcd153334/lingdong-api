package controller

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/gateway"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/siteurl"
	"lingdong-api/internal/relay"
	"lingdong-api/internal/service/billing"
	"lingdong-api/internal/service/task"
	"lingdong-api/internal/service/taskshell"
)

// maxVideoBodyBytes 是异步任务提交请求体上限（与同步转发一致，§4.3.5）。
const maxVideoBodyBytes = 16 << 20

// multipartFileMaxBytes 是 multipart 提交里单个文件 part 转 data: URL 的上限。
const multipartFileMaxBytes = 10 << 20

// submitIdempotencyKey 读取客户端声明的幂等键（Idempotency-Key 头）。
//
// 只在提交端点上读，且**只在确实带了的时候**透传：不启用幂等的请求路径
// 与改造前逐字节一致（不落库、不加唯一约束压力）。
func submitIdempotencyKey(c *gin.Context) string {
	return strings.TrimSpace(c.GetHeader("Idempotency-Key"))
}

// submitReqSource 标注调用记录里"请求参数"的可信度来源。
//
// multipart 提交会被 readSubmitBody **重构成 JSON**（文件转成 data: URL、
// 数字字段被 ParseFloat 归一），所以那不是客户端原样发出的东西；
// 标成 raw 会让人照着它去核对客户端日志，然后发现处处对不上。
func submitReqSource(c *gin.Context) string {
	ctype := strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Type")))
	if strings.HasPrefix(ctype, "multipart/form-data") {
		return "rebuilt"
	}
	return "raw"
}

// VideoCreate 处理 POST /v1/video/generations 与 POST /v1/videos（Sora 入口）。
//
// 两个入口**不是**别名：请求体相同，回执形状不同（兼容协议的两条路径各自
// 有客户端，回执字段集不同）。这就是为什么同一个 handler 里要按入口分流 ——
// 用别名"省掉"分流，会让 Sora 客户端拿到一份它解析不动的回执。
func VideoCreate(c *gin.Context) {
	k := relay.APIKeyOf(c)
	u := relay.UserOf(c)
	if k == nil || u == nil {
		gateway.FailDef(c, errs.InvalidAPIKey)
		return
	}
	body, err := readSubmitBody(c)
	if err != nil || len(body) == 0 {
		gateway.FailDef(c, errs.InvalidParam)
		return
	}
	// 用视频专用入口：它会把厂商原生的顶层 image_urls 折叠进 input_reference
	// （SSRF 校验与参考图计费都只认那一条链路）。
	req, err := task.ParseVideoSubmit(body)
	if err != nil {
		gateway.FailDef(c, failDefOf(err))
		return
	}
	row, err := task.Submit(c.Request.Context(), task.SubmitInput{
		Key: k, Group: relay.EffectiveGroup(k, u), IP: c.ClientIP(),
		RequestID: c.GetHeader("X-Request-Id"), Req: req, ExpectModality: "video",
		EndpointKey:    "video/generations",
		IdempotencyKey: submitIdempotencyKey(c),
		// 调用记录：原始请求体 + 可信度来源（rebuilt = multipart 被平台重构过）。
		RawBody: body, ReqSource: submitReqSource(c),
	})
	if err != nil {
		gateway.FailDef(c, failDefOf(err))
		return
	}
	writeVideoSubmitAck(c, row)
}

// writeVideoSubmitAck 按**入口**写出受理回执（唯一分流点）。
//
//   - `/v1/video/generations`：5 键扁平回执（id/task_id/status/model/created_at）；
//   - `/v1/videos`：OpenAI Video 对象（object:"video"、progress 整数、四态）。
//
// 判据用 FullPath 而不是「有没有某个头」：入口是路由属性，客户端不可能伪造。
func writeVideoSubmitAck(c *gin.Context, row *model.Task) {
	if c.FullPath() == "/v1/videos" {
		gateway.OK(c, taskshell.VideoOf(row))
		return
	}
	gateway.OK(c, taskshell.AckOf(row))
}

// VideoRegenerate 处理 POST /v1/video/regenerations：把一个已完成任务升档重生成。
//
// 与 VideoCreate 同构（200 立即返回任务行，不是 202），差别只在请求解析：不要求 seconds，
// 要求 source_task_id 或 base_video 之一。真正的源任务解析在 task.Submit 里做（需要 DB 与归属）。
func VideoRegenerate(c *gin.Context) {
	k := relay.APIKeyOf(c)
	u := relay.UserOf(c)
	if k == nil || u == nil {
		gateway.FailDef(c, errs.InvalidAPIKey)
		return
	}
	body, err := readSubmitBody(c)
	if err != nil || len(body) == 0 {
		gateway.FailDef(c, errs.InvalidParam)
		return
	}
	req, err := task.ParseRegenerate(body)
	if err != nil {
		gateway.FailDef(c, failDefOf(err))
		return
	}
	row, err := task.Submit(c.Request.Context(), task.SubmitInput{
		Key: k, Group: relay.EffectiveGroup(k, u), IP: c.ClientIP(),
		RequestID: c.GetHeader("X-Request-Id"), Req: req, ExpectModality: "video",
		EndpointKey:    "video/regenerations",
		IdempotencyKey: submitIdempotencyKey(c),
		// 调用记录：原始请求体 + 可信度来源（rebuilt = multipart 被平台重构过）。
		RawBody: body, ReqSource: submitReqSource(c),
	})
	if err != nil {
		gateway.FailDef(c, failDefOf(err))
		return
	}
	gateway.OK(c, taskshell.AckOf(row))
}

// readSubmitBody 读取提交请求体：JSON 直接透传；multipart/form-data（new-api createvideo 文档形态）
// 把表单字段合成等价 JSON。文件 part 编码为 data: URL（10MB 上限，超出报参数错误）。
func readSubmitBody(c *gin.Context) ([]byte, error) {
	// 判定必须 ToLower + TrimSpace（与 media_task.readJSONOnlyBody 同款）：
	// 客户端发 "Multipart/Form-Data; boundary=..."（部分 SDK 会首字母大写）
	// 时，裸 HasPrefix 会走 JSON 分支，文件被静默丢弃后报一个
	// 与真实原因毫不相干的 400。
	ctype := strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Type")))
	if !strings.HasPrefix(ctype, "multipart/form-data") {
		return io.ReadAll(io.LimitReader(c.Request.Body, maxVideoBodyBytes))
	}
	if err := c.Request.ParseMultipartForm(maxVideoBodyBytes); err != nil {
		return nil, err
	}
	m := map[string]any{}
	for k, vs := range c.Request.MultipartForm.Value {
		if len(vs) == 0 {
			continue
		}
		v := vs[0]
		switch k {
		case "width", "height", "seed", "n", "fps":
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				m[k] = f
			}
		case "metadata":
			var md map[string]any
			if json.Unmarshal([]byte(v), &md) == nil {
				m[k] = md
			}
		default:
			m[k] = v
		}
	}
	for k, fs := range c.Request.MultipartForm.File {
		if len(fs) == 0 {
			continue
		}
		f, err := fs[0].Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(f, multipartFileMaxBytes+1))
		f.Close()
		if err != nil || len(data) > multipartFileMaxBytes {
			return nil, errors.New("file part too large")
		}
		ct := fs[0].Header.Get("Content-Type")
		if ct == "" {
			ct = "application/octet-stream"
		}
		m[k] = "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(data)
	}
	return json.Marshal(m)
}

// uintParam 解析路径参数为正整数（非法/小于 1 返回 false）。
func uintParam(c *gin.Context, name string) (uint64, bool) {
	v, err := strconv.ParseUint(c.Param(name), 10, 64)
	if err != nil || v == 0 {
		return 0, false
	}
	return v, true
}

// publicIDParam 读取并校验路径里的对外任务号。
//
// 只做**形态**校验（task_ + 26 位），不查库：形态错是参数错（400），
// 形态对但查不到是资源不存在（404）。两者混成一个码会让调用方
// 把"我拼错了号"读成"任务没了"，进而去查对账。
func publicIDParam(c *gin.Context, name string) (string, bool) {
	ref := strings.TrimSpace(c.Param(name))
	if !task.IsPublicID(ref) {
		return "", false
	}
	return ref, true
}

// absURL 由当前请求推导对外绝对地址（供产物链接下发）。
func absURL(c *gin.Context, path string) string {
	// 必须走 siteurl.Base：它是「平台对外地址」的唯一推导点（优先运营配置的
	// api_base_url，回退请求 Host）。自己拼 scheme+Host 会让同一个响应里出现
	// 两个 origin —— 任务产物是 site.api_base_url、图标却是内网名。
	base := siteurl.Base(c)
	if base == "" {
		return path
	}
	return base + path
}

// defWithReason 把包装在错误里的具体原因拼进对外文案。
//
// 服务层用 `<sentinel>：<原因>` 的形态包装（例如「本模型不支持 1080p」）。
// 不拼的话调用方只会看到「参数不合法」，永远不知道该改哪个字段；
// 这里剥掉内部包名前缀（"task: 参数不合法："），只透出原因部分。
func defWithReason(def errs.Def, err error) errs.Def {
	msg := err.Error()
	if i := strings.Index(msg, "："); i >= 0 && i+len("：") < len(msg) {
		if reason := strings.TrimSpace(msg[i+len("："):]); reason != "" {
			return def.WithMessage(def.Message + "：" + reason)
		}
	}
	return def
}

// failDefOf 把服务层错误映射为对外错误码定义（异步任务域共用）。
func failDefOf(err error) errs.Def {
	switch {
	case errors.Is(err, task.ErrInvalidParam):
		return defWithReason(errs.InvalidParam, err)
	case errors.Is(err, task.ErrReferenceInvalid):
		return errs.ReferenceInvalid
	case errors.Is(err, task.ErrModelNotFound):
		return errs.ModelNotFound
	case errors.Is(err, task.ErrModelNotAllowed):
		return errs.ModelNotAllowed
	case errors.Is(err, task.ErrModalityNotAllowed):
		return errs.ModalityNotAllowed
	case errors.Is(err, task.ErrAsyncUnsupported):
		// 调用方式与模型能力不匹配：400 并给行动指引（去掉 async），
		// 不能用 502 —— 那会引导用户/运维去查一个并不存在的渠道故障。
		return defWithReason(errs.InvalidParam, err)
	case errors.Is(err, task.ErrNoChannel):
		return errs.ModelUnavailable
	case errors.Is(err, task.ErrUpstreamSubmit):
		// 提交失败的原因（上游原文、渠道名）已在库里，对外只说服务暂不可用。
		return errs.ServiceUnavailable
	case errors.Is(err, task.ErrRateLimited):
		// 渠道限流：不是我们的服务挂了，是「打得太快/配额已满」。
		// 必须 429 —— 客户端会退避重试，而 5xx 会被 SDK 立刻重试，把限流放大。
		return errs.RateLimited
	case errors.Is(err, task.ErrNotCancelable):
		return errs.TaskNotCancelable
	case errors.Is(err, task.ErrNotRetryable):
		return defWithReason(errs.TaskNotRetryable, err)
	case errors.Is(err, task.ErrNotFound):
		return errs.NotFound
	case errors.Is(err, task.ErrContentGone):
		return errs.FileGone
	case errors.Is(err, task.ErrStorageUnavailable):
		// 对象还在桶里，补配置就能读：503（可重试），不是「永久失效」的 410。
		return errs.StorageBackendMissing
	case errors.Is(err, billing.ErrKeyLimitBalanceCap):
		return errs.InsufficientBalance.WithMessage("密钥消费上限已达账户余额上限，充值后自动恢复")
	case errors.Is(err, billing.ErrInsufficientQuota):
		return errs.InsufficientBalance
	case errors.Is(err, billing.ErrPriceMissing):
		return errs.PriceNotConfigured
	case errors.Is(err, billing.ErrInvalidTier):
		return errs.InvalidTier
	case errors.Is(err, billing.ErrImageDefaultTierUnset):
		// 与同步链路同一文案：平台没配默认档（或尺寸写法认不出）时，指定 size 就能继续。
		return errs.InvalidParam.WithMessage("无法确定计费档位：请显式指定 size（如 1K、1024x1024）")
	case errors.Is(err, billing.ErrSpecUnsupported):
		return errs.EndpointUnsupported
	// 时长超过模型级 max_seconds 是**调用方**问题（换个秒数就能成功），必须 400 且带实际值与上限；
	// 落到 errs.Internal 会让用户以为平台故障。触发点在 Estimate，Retry 也会走到这里。
	case errors.Is(err, billing.ErrOverMaxSeconds):
		return defWithReason(errs.InvalidParam, err)
	// 单笔预估超上限：同步链路映射成 400 request_quota_exceeded，
	// 异步这里漏了分支就会落到 Internal（500）—— 同一个原因两个状态码，
	// 用户会以为"同步能过、异步是平台故障"。
	case errors.Is(err, billing.ErrChargeOverLimit):
		return errs.RequestQuotaExceeded
	}
	return errs.Internal
}

// GatewayVideoGet 处理 GET /v1/videos/{id}：OpenAI Video 的 retrieve。
//
// 与 GET /v1/tasks/{id}、GET /v1/video/generations/{id} **同源**（都是同一个
// model.Task 行），差别只在投影：这里是视频对象的四态 + 整数进度。
// 三条读面并存不是三份真相：形状各有协议归属，数据只有一个来源。
func GatewayVideoGet(c *gin.Context) {
	t, ok := gatewayTaskOf(c)
	if !ok {
		return
	}
	gateway.OK(c, taskshell.VideoOf(t))
}

// GatewayVideoContent 处理 GET|HEAD /v1/videos/{id}/content：取视频本身。
//
// 兼容协议在拿不到上游直链时生成的结果地址就是这条路径，把它的内容固定为
// 第 0 个产物：视频任务只有一个主产物，多产物是图像/音乐的能力。
func GatewayVideoContent(c *gin.Context) {
	t, ok := gatewayTaskOf(c)
	if !ok {
		return
	}
	serveTaskArtifact(c, t, 0)
}
