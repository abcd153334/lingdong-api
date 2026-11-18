// Package taskview 是**任务对象的唯一定义**。
//
// 为什么单独成包而不是放在 service/task：`logs` 包也要下发同一个任务对象
// （调用记录详情里的 `task` 字段），但 `relay → logs → task → relay` 是已存在的
// 导入环，logs 永远 import 不到 task。以前为了绕开它，logs 自己定义了一份
// TaskRef（id 是数字、时间是 RFC3339、error 是字符串），于是"同一个任务"在
// 两个接口里长得不一样 —— 客户端要写两套解析。
//
// 这里只依赖 model 与 storage 两个叶子包，task 与 logs 都能 import：
// 任务对象的形状只有这一份，投影函数只有 ViewOf 一个。
package taskview

import (
	"strconv"
	"strings"
	"time"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/taskstate"
	"lingdong-api/internal/service/storage"
)

// ViewError 是任务失败原因。
//
// 恒为对象（成功时整个 error 为 null）而不是字符串：字符串无法在不破坏契约的前提下
// 加 code / retryable 这类字段，而 error 一旦下发为字符串，客户端就会写 `.error !== ""`。
type ViewError struct {
	// Message 是**分类文案**（如「任务处理失败，已全额返还」），不是上游原文：
	// 上游原文可能含渠道名、内部单号甚至密钥片段。原文只走管理台（AdminView.ErrorMsg）。
	Message string `json:"message"`
}

// ViewArtifact 是一项任务产物。
//
// Index 是**对外序号**：0 起、稠密编号（失败与过期条目不占号），与
// 控制面 `/contents/{index}` 与协议面 `/artifacts/{index}/content` 的取数顺序严格一致。
type ViewArtifact struct {
	Index       int    `json:"index"`
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
	Bytes       int64  `json:"bytes"`
	// Kind 是渲染档：image / video / audio / text / file。
	// 由 model.MediaKind 按 MIME 派生，前端不再各写一份前缀判断。
	Kind string `json:"kind"`
}

// ViewUsage 是本次任务的用量（结算口径的原始量，不是金额）。
type ViewUsage struct {
	Images    int64 `json:"images"`
	Seconds   int64 `json:"seconds"`
	RefImages int64 `json:"ref_images"`
}

// View 是全站唯一的任务对象。
type View struct {
	ID     string `json:"id"`
	Object string `json:"object"` // 恒为 "task"
	Status string `json:"status"` // queued/running/succeeded/failed/expired/canceled
	Model  string `json:"model"`
	// Modality 是模型大类（chat/image/video/audio/...）。
	Modality string `json:"modality"`
	// Kind 是任务子类型（video/... ；timeout 表示轮询网络失败超限），来自 tasks.kind。
	Kind string `json:"kind,omitempty"`
	// ModelIconURL 由 handler 批量填充（ViewOf 是纯内存投影，不查库）。
	ModelIconURL string `json:"model_icon_url,omitempty"`
	// Progress 由状态派生：queued=0、running=50、终态=100。表里没有进度列。
	Progress int `json:"progress"`
	// CreatedAt / CompletedAt / DeadlineAt 一律 Unix 秒。
	//
	// 用 Unix 秒而不是 RFC3339：/v1 的其它对象（created 等）都是整数时间戳，
	// 同一份响应里混两种时间格式会让客户端写两套解析。前端 formatDateTime
	// 已按「小于 1e11 视为秒」处理。
	CreatedAt   int64  `json:"created_at"`
	CompletedAt *int64 `json:"completed_at,omitempty"`
	DeadlineAt  *int64 `json:"deadline_at,omitempty"`
	// Error 恒有字段（成功为 null）：缺字段与 null 在客户端是两种分支，
	// 少下发一次就要多写一个 `?.`。
	Error *ViewError `json:"error"`
	// Contents 恒为数组（没有产物时是 []，不是 null）。
	Contents []ViewArtifact `json:"contents"`
	Usage    ViewUsage      `json:"usage"`
}

// PortalItem 是门户/应用面的任务对象：公共 View + 资金字段。
//
// 用**嵌入**而不是重写一遍字段：改造前 PortalItem 与 ItemView 各写一份同名同义字段，
// 结果 id 一个是 number 一个是 string、时间一个 RFC3339 一个 Unix —— 同一份数据两套口径。
type PortalItem struct {
	View
	// Frozen 是预扣金额（微），Settled 是结算金额（微）。
	// 不加 omitempty：0 是有效值（免费任务 / 尚未结算），省略会被前端当成"没有这个字段"。
	Frozen  int64 `json:"frozen"`
	Settled int64 `json:"settled"`
}

// AdminView 是管理台的任务对象：门户 View + 归属与供应链字段。
//
// 这些字段**永远不下发门户**：渠道号、上游代号、上游单号能反推我们的供应链。
type AdminView struct {
	PortalItem
	UserID    uint64 `json:"user_id"`
	KeyID     uint64 `json:"key_id"`
	ChannelID uint64 `json:"channel_id"`
	// Platform 是适配器标识（duomi / metaso / spec:xxx），仅管理台可见。
	Platform string `json:"platform"`
	// UpstreamTaskID 是上游给的任务号，排障时用它去对方后台捞单。
	UpstreamTaskID string `json:"upstream_task_id,omitempty"`
	// ErrorMsg 是上游/平台记录的原始错误串，排障用；对外只给分类文案（View.Error）。
	ErrorMsg string `json:"error_msg,omitempty"`
}

// PathGateway 是 /v1 任务路由组的前缀。
//
// 协议面的产物**不在**任务对象里：`TaskFlat` 没有 `contents`，产物走独立资源
// `/v1/tasks/{id}/artifacts`（清单）与 `/v1/tasks/{id}/artifacts/{index}/content`（字节）。
// 前缀只在 controller 拼绝对地址时用一次（origin 由 siteurl.Base 推导）。
const PathGateway = "/v1/tasks"

// ContentBaseFor 返回**控制面**某入口下的产物地址前缀（不含 origin）。
//
// 控制面三个入口各挂一份同一批 handler（门户 Cookie / 开放接口 Bearer / 管理台），
// 产物地址必须跟着入口走：应用面拿到指向门户路由组的地址，点开必然 401。
// 判据只有这一处 —— 以前 controller 里写一份、relay 里再拼一次 `/v1/tasks`，
// 结果「调用记录详情」里的产物地址指向协议面，而协议面只认 API KEY，且路径已改成
// `/artifacts`，于是门户页面上那个链接永久 404。
func ContentBaseFor(fullPath string) string {
	switch {
	case strings.HasPrefix(fullPath, "/api/user/"):
		return "/api/user/tasks"
	case strings.HasPrefix(fullPath, "/api/admin/"):
		return "/api/admin/tasks"
	default:
		return "/api/portal/tasks"
	}
}

// ViewOptions 是投影所需的上下文（视图层不查库、不读 gin.Context）。
type ViewOptions struct {
	// ContentBase 是产物地址前缀（不含任务 id），拼成 `<base>/<id>/contents/<index>`。
	// /v1 传站点绝对地址 + "/v1/tasks"，门户传 "/api/portal/tasks"。
	ContentBase string
	// IconURL 是该模型已解析好的图标地址（由 FillModelIcons 批量填）。
	IconURL string
	// Artifacts 是该任务已转存的产物（列表页由调用方一次 IN 查好，不是逐条查）。
	Artifacts []model.Artifact
	// Usage 是本次任务的原始用量。
	//
	// 由**调用方**填入而不是在这里从 tasks.request_params 反解：用量口径属于结算，
	// 反解要用计费包的参数解析器；而 logs 包（调用记录详情）拿到的是
	// **usage 行上的实测用量**，比按参数反解更准。两者都填同一个 ViewUsage，
	// 谁也不必为了投影去 import 计费包的解析器（那又会造出新的导入环）。
	Usage ViewUsage
}

// ViewOf 是本文件唯一的投影函数。
func ViewOf(t *model.Task, opt ViewOptions) View {
	v := View{
		// 对外只用公开号：门户、管理台、日志详情拿到的是同一个字符串，
		// 也正好是 /v1/tasks/{id} 能接受的那个（见 service/task.GetByRef）。
		ID:         t.PublicID,
		Object:     "task",
		Status:     t.Status,
		Model:      t.Model,
		Modality:   t.Modality,
		Kind:       t.Kind,
		CreatedAt:  t.CreatedAt.Unix(),
		DeadlineAt: unixPtr(t.DeadlineAt),
		Contents:   make([]ViewArtifact, 0, len(opt.Artifacts)),
		Usage:      opt.Usage,
	}
	v.ModelIconURL = opt.IconURL
	v.Progress = taskstate.Progress(t.Status)
	// 终态才给 completed_at：运行中给一个「现在」会让每次查询的完成时间都在变。
	if IsTerminal(t.Status) {
		v.CompletedAt = unixPtr(&t.UpdatedAt)
	}
	if msg := taskstate.PortalError(t.Status); msg != "" {
		v.Error = &ViewError{Message: msg}
	}
	for _, a := range opt.Artifacts {
		v.Contents = append(v.Contents, ViewArtifact{
			Index:       a.Index,
			URL:         artifactURL(t.PublicID, a, opt.ContentBase),
			ContentType: a.ContentType,
			Bytes:       a.Bytes,
			Kind:        a.Kind,
		})
	}
	return v
}

// PortalOf 投影门户对象（公共字段 + 资金字段）。
func PortalOf(t *model.Task, opt ViewOptions) PortalItem {
	return PortalItem{View: ViewOf(t, opt), Frozen: t.Frozen, Settled: t.Settled}
}

// AdminOf 投影管理台对象（门户字段 + 归属与供应链）。
func AdminOf(t *model.Task, opt ViewOptions) AdminView {
	return AdminView{
		PortalItem:     PortalOf(t, opt),
		UserID:         t.UserID,
		KeyID:          t.KeyID,
		ChannelID:      t.ChannelID,
		Platform:       t.Platform,
		UpstreamTaskID: t.UpstreamTaskID,
		ErrorMsg:       t.ErrorMsg,
	}
}

// IsTerminal 判断任务是否已到终态（不再推进）。
//
// 只是 taskstate 的**薄转发**：判据只在 pkg/taskstate 一处（六态的定义点）。
// 这里曾经复制了一份 switch，后果是加第七态时两边必须同时改，
// 而漏改的表现只是"某个终态被当成仍在运行"（页面一直转圈、任务永不超时）。
func IsTerminal(status string) bool { return taskstate.IsTerminal(taskstate.Status(status)) }

// artifactURL 是产物地址的唯一派生点：配了云存储就给对象存储地址，否则给平台取数端点。
//
// 与同步出图的 /v1/media/{token} 不同源，也**不该**同源：同步路径逐字节透传上游体、
// 平台没有本地产物，只能给签名代理；任务路径的产物已转存，给的是取数端点。
func artifactURL(publicID string, a model.Artifact, contentBase string) string {
	fallback := ""
	if contentBase != "" {
		fallback = contentBase + "/" + publicID + "/contents/" + strconv.Itoa(a.Index)
	}
	return storage.URLOr(storage.Ref{Backend: a.Backend, Bucket: a.Bucket, Key: a.Key}, fallback)
}

// unixPtr 把可空时间转成 Unix 秒指针（nil 保持 nil，不落成 0）。
func unixPtr(t *time.Time) *int64 {
	if t == nil {
		return nil
	}
	sec := t.Unix()
	return &sec
}
