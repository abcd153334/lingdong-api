// Package taskshell 是 `/v1` 任务面的**唯一对外结构**。
//
// 为什么单独成包：任务对象曾经有三套对外形状（富对象 / OpenAI Video / new-api
// 扁平），每个出口各拼一份字段，于是「同一个任务在两个接口里字段不同」成为常态。
// 现在只有一个结构 `TaskFlat`，所有 `/v1` 路径共用，差别只在
// 「有没有 {code,message,data} 信封」「附不附 result_url」这种协议外壳上。
//
// 不依赖 apidoc（那是文档层）：形状守卫测试放在本包（见 flat_shape_test.go），
// 由它反射比对 `apidoc.TaskFlatFields()` —— 反向依赖会成环。
package taskshell

import (
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/taskstate"
)

// PlatformValue 是对外声明的平台标识。
//
// new-api 在任务对象里输出**渠道平台名**（kling / suno / …），那是供应链信息；
// 平台的既有约定是「渠道号、上游代号、上游单号永不下发」（见 taskview.AdminView）。
// 这里固定成一个中性的自有标识：客户端拿它做分组仍然可用，但推不出供货方。
const PlatformValue = "lingdong"

// TaskFlat 是 `/v1` 的任务对象（new-api 扁平结构 + 两个兼容别名键）。
//
// 字段来源全部是 `model.Task` 与 `taskstate`，不查库、不拼 URL。
type TaskFlat struct {
	// ID 与 TaskID 同值。new-api 的提交壳两个键都发（controller/relay.go 的
	// map 里同时有 id 与 task_id），只给一个会让一半客户端取到 undefined。
	ID     string `json:"id"`
	TaskID string `json:"task_id"`
	// Platform 恒为 lingdong（见 PlatformValue）。
	Platform string `json:"platform"`
	// Model 是平台模型 ID（不是渠道绑定里的 upstream_model）。
	Model string `json:"model"`
	// Status 是 new-api 词表：SUBMITTED / IN_PROGRESS / SUCCESS / FAILURE。
	Status string `json:"status"`
	// Progress 是 new-api 的字符串进度："0%" / "50%" / "100%"。
	Progress string `json:"progress"`
	// FailReason 是分类文案（「任务处理失败，已全额返还」），不是上游原文；
	// 成功时为空串。
	FailReason string `json:"fail_reason"`
	CreatedAt  int64  `json:"created_at"`
	// FinishedAt / FinishTime 是同一个值的两个键：new-api 的 /v1/tasks/:id 用
	// finished_at，视频查询壳（TaskDto）用 finish_time。同值下发，避免任一类
	// 客户端取到 undefined —— 一份数据两个键，不是第二份真相。
	FinishedAt int64 `json:"finished_at"`
	FinishTime int64 `json:"finish_time"`
	// ResultURL 只在视频查询壳里填（new-api 的 TaskDto 有这个键，GetTask 没有）。
	ResultURL string `json:"result_url,omitempty"`
}

// FlatOf 把任务行投影成对外结构。
//
// 只读 model.Task：返回字段全是行上的事实或 taskstate 的派生，
// 没有一份需要再查库（产物走单独的 artifacts 端点）。
func FlatOf(t *model.Task) TaskFlat {
	f := TaskFlat{
		ID:         t.PublicID,
		TaskID:     t.PublicID,
		Platform:   PlatformValue,
		Model:      t.Model,
		Status:     taskstate.Word(taskstate.DialectNewAPI, t.Status),
		Progress:   taskstate.Percent(t.Status),
		FailReason: taskstate.PortalError(t.Status),
		CreatedAt:  t.CreatedAt.Unix(),
	}
	if taskstate.IsTerminal(t.Status) {
		// 与 View.CompletedAt 同一口径：终态才给完成时间，运行中给「现在」
		// 会让每次查询的完成时间都在变。
		ft := t.UpdatedAt.Unix()
		f.FinishedAt, f.FinishTime = ft, ft
	}
	return f
}

// Artifact 是产物清单里的一项（new-api 的 `taskArtifactResponse`）。
//
// Key 是对外序号（"0" 起、稠密），同时是 content 端点的路径段：
// new-api 只要求 `^[A-Za-z0-9][A-Za-z0-9._~-]{0,127}$`，序号天然满足。
type Artifact struct {
	Key  string `json:"key"`
	Type string `json:"type"`
	// MimeType 不加 omitempty：上游不给 MIME 时是**空字符串**，键仍然在。
	//
	// 加了 omitempty 的表现是"有的产物有这个键、有的没有"，客户端要为此写一个
	// `?.` 分支；而字段表（文档）没法表达"可能缺"，只能写死"恒在"。
	// 对外键恒在 > 省几个字节。
	MimeType   string `json:"mime_type"`
	ContentURL string `json:"content_url"`
}

// ArtifactList 是 `GET /v1/tasks/{id}/artifacts` 的响应。
type ArtifactList struct {
	TaskID    string     `json:"task_id"`
	Artifacts []Artifact `json:"artifacts"`
}

// SubmitAck 是兼容协议 `/v1/video/generations` 的受理回执。
//
// 只有五个键，一个都不能多：兼容协议的客户端按固定字段解析这份回执，
// 多出来的键（progress/fail_reason/…）会被某些强校验的客户端当成版本不符。
// 要完整任务对象的客户端走 `GET /v1/tasks/{id}`（那是同一个行的另一份投影）。
type SubmitAck struct {
	ID        string `json:"id"`
	TaskID    string `json:"task_id"`
	Status    string `json:"status"`
	Model     string `json:"model"`
	CreatedAt int64  `json:"created_at"`
}

// AckOf 是 SubmitAck 的唯一投影。
//
// Status 恒为 "queued"：受理回执只表达「我们接下了」，此时任务还没有任何
// 上游进度可报。给 SUBMITTED（/v1/tasks 的词表）会让同一份受理在不同端点
// 出现两种状态词，客户端要写两套判断。
func AckOf(t *model.Task) SubmitAck {
	return SubmitAck{
		ID:        t.PublicID,
		TaskID:    t.PublicID,
		Status:    taskstate.VideoQueued,
		Model:     t.Model,
		CreatedAt: t.CreatedAt.Unix(),
	}
}

// VideoError 是视频对象的失败原因（OpenAI Video 的形状：对象，不是字符串）。
type VideoError struct {
	Message string `json:"message"`
	Code    string `json:"code"`
}

// Video 是 OpenAI Video 对象（`/v1/videos` 与 `/v1/videos/{id}`）。
//
// 与 TaskFlat 的区别不是"更详细"，而是**另一份协议**：状态词四态、进度是整数、
// 完成时间键名不同。两者都从 model.Task + taskstate 派生，不存在第三份真相。
type Video struct {
	ID        string `json:"id"`
	TaskID    string `json:"task_id,omitempty"`
	Object    string `json:"object"`
	Model     string `json:"model"`
	Status    string `json:"status"`
	Progress  int    `json:"progress"`
	CreatedAt int64  `json:"created_at"`
	// CompletedAt 只在终态给（运行中给「现在」会让每次查询的时间都在变）。
	CompletedAt int64       `json:"completed_at,omitempty"`
	Error       *VideoError `json:"error,omitempty"`
}

// VideoOf 是 Video 的唯一投影。
func VideoOf(t *model.Task) Video {
	v := Video{
		ID:        t.PublicID,
		TaskID:    t.PublicID,
		Object:    "video",
		Model:     t.Model,
		Status:    taskstate.VideoWord(t.Status),
		Progress:  taskstate.VideoProgress(t.Status),
		CreatedAt: t.CreatedAt.Unix(),
	}
	if taskstate.IsTerminal(t.Status) {
		v.CompletedAt = t.UpdatedAt.Unix()
	}
	if msg := taskstate.PortalError(t.Status); msg != "" {
		v.Error = &VideoError{Message: msg, Code: string(t.Status)}
	}
	return v
}
