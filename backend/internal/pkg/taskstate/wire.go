package taskstate

import "strings"

// 本文件是任务状态的**对外编码表**：内部六个态怎么写成某一套协议的词。
//
// 为什么是表而不是 if：三个出口（原生扁平、视频查询封套、将来的其它协议）
// 各自散落一个 switch，第一周就会分叉 —— 加一个 `expired` 时只改了两个
// switch，第三个静默地把它渲染成空字符串，客户端拿到 `status: ""`。
// 表是穷的：`wire_test.go` 逐格断言非空，漏一格就红。

// Dialect 是一套协议词表。
type Dialect int

const (
	// DialectNewAPI 是 new-api 的扁平结构：/v1/tasks/*、/v1/video/generations/*。
	DialectNewAPI Dialect = iota
)

// newAPIWords 是内部状态 → new-api 状态词。
//
// new-api 只有四个词（`model/task.go:36-42` 的常量 + `ToVideoStatus`）：
// SUBMITTED / IN_PROGRESS / SUCCESS / FAILURE。
//
// 平台多出来的两个态折叠规则：
//   - expired（超时全额返还）→ FAILURE，语义靠 fail_reason 保留
//     （「任务超时，已全额返还」）。new-api 客户端不认识 EXPIRED，
//     下发一个它不认识的词比丢一点信息更糟。
//   - canceled → FAILURE，fail_reason =「任务已取消」。
var newAPIWords = map[Status]string{
	Queued:    "SUBMITTED",
	Running:   "IN_PROGRESS",
	Succeeded: "SUCCESS",
	Failed:    "FAILURE",
	Expired:   "FAILURE",
	Canceled:  "FAILURE",
}

// Word 返回某套协议下的状态词。
func Word(d Dialect, s Status) string {
	switch d {
	case DialectNewAPI:
		return newAPIWords[s]
	default:
		return newAPIWords[s]
	}
}

// ParseWord 是 Word 的反向：把某套协议的状态词翻回平台状态。
//
// 只用于**入参**（列表筛选）。`FAILURE` 无法区分 failed / expired / canceled
// （对方协议本身就没有这三者的区别），统一按 failed 处理 —— 需要精确定位
// 超时与取消请查库或看 fail_reason，不要在这里造一个不存在的精度。
func ParseWord(d Dialect, raw string) (Status, bool) {
	w := strings.ToUpper(strings.TrimSpace(raw))
	switch w {
	case "SUBMITTED", "QUEUED":
		return Queued, true
	case "IN_PROGRESS", "RUNNING":
		return Running, true
	case "SUCCESS", "SUCCEEDED":
		return Succeeded, true
	case "FAILURE", "FAILED":
		return Failed, true
	}
	return "", false
}

// Percent 返回 new-api 的进度写法（它把进度存成字符串 "50%"）。
//
// 与 Progress 的分工：Progress 是整数（门户进度条、内部判定），
// Percent 只服务对外文案。两者都从状态派生，不存在第三个来源。
func Percent(s Status) string {
	switch Progress(s) {
	case 0:
		return "0%"
	case 50:
		return "50%"
	default:
		return "100%"
	}
}

// OpenAI Video 的四态（Sora / 兼容协议的视频对象）。
//
// 与我们内部六态是两个不同的词表：视频对象没有 expired/canceled，
// 两者都归 failed。**不要**让这个映射散在 handler 里 —— 那会出现
// "同一个任务在 /v1/tasks 是 expired、在 /v1/videos 是 succeeded"。
const (
	VideoQueued     = "queued"
	VideoInProgress = "in_progress"
	VideoCompleted  = "completed"
	VideoFailed     = "failed"
)

// VideoWord 把内部状态映射成视频对象的四态。
//
// expired / canceled 都归 failed：视频对象的词表里没有这两个概念，
// 而它们对客户端的行为（不再轮询、没有产物）与 failed 完全一致。
func VideoWord(s Status) string {
	switch s {
	case Queued:
		return VideoQueued
	case Running:
		return VideoInProgress
	case Succeeded:
		return VideoCompleted
	default:
		return VideoFailed
	}
}

// VideoProgress 返回视频对象的整数进度（0/50/100）。
//
// 与 Percent 同源（都从 Progress 派生），但类型是 int —— 视频 SDK 按数字解析，
// 给它 "50%" 会得到 0（它们的实现是 strconv.Atoi 的宽松变体）。
func VideoProgress(s Status) int { return Progress(s) }
