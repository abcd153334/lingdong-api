// Package taskstate 是「任务状态」的**唯一词表**：取值、终态判定、进度派生、
// 对外分类文案、上游词归一，全部只在这里。
//
// 为什么必须收口：同一件事曾经有三份实现 ——
//   - `internal/model` 的六个常量 + `TaskProgress` + `TaskPortalError`；
//   - `internal/relay` 的五个常量（**少了 expired**）+ `NormalizeTaskStatus`；
//   - `internal/pkg/syncwait` 的两态常量（同步桥接等待循环用）。
//
// 三份对同一个上游值判定不一致的代价是可量化的：可灵的 "succeed"、nano 的数字 "3"、
// AutoDL 的 "completed" 在不同链路上分别被当成「成功」与「永远运行中」——
// 已经生成好的产物被丢掉，或者任务挂到 deadline 全额退款。
//
// 这里刻意用 `type Status = string`（别名）而不是 defined type：
// 任务状态要流过适配器的 `TaskStatus` 结构体、`tasks.request_params` 的 jsonb 快照、
// 以及 40+ 处字符串赋值。defined type 会逼出一次全仓改名（改一处乱一片），
// 而「单一真相」由**常量表 + 归一表**保证，不需要类型系统再证明一次。
package taskstate

import "strings"

// Status 是平台任务状态。
type Status = string

const (
	// Queued 已受理，尚未开始。
	Queued Status = "queued"
	// Running 上游已开始生成。
	Running Status = "running"
	// Succeeded 已完成，产物已转存。
	Succeeded Status = "succeeded"
	// Failed 上游明确失败（含平台侧参数/素材错误）。
	Failed Status = "failed"
	// Expired 超时未完成，且已全额返还。
	//
	// 它是平台自有态，上游没有：new-api 与 OpenAI 都把它折进 failure。
	// 保留独立取值的理由只有一个 —— 它决定「全额返还」，压成 failed + 标记列
	// 会让对账与客服都要两列联合判定，而这两列还可能不同步。
	Expired Status = "expired"
	// Canceled 用户主动取消（仅未提交上游的任务可取消）。
	Canceled Status = "canceled"
)

// All 是全部平台状态（顺序即展示顺序：流水线顺序，终态在后）。
var All = []Status{Queued, Running, Succeeded, Failed, Expired, Canceled}

// IsTerminal 判断是否终态（终态不再推进、不再轮询）。
func IsTerminal(s Status) bool {
	switch s {
	case Succeeded, Failed, Expired, Canceled:
		return true
	default:
		return false
	}
}

// Progress 由状态派生整数进度：排队 0 / 运行 50 / 终态 100。
//
// 没有进度列是刻意的：上游只暴露「排队/运行/完成」，编造 37% 这种数字
// 只会让用户误以为平台知道得比实际更多。终态的 100 仅表示「不再推进」，
// 界面在失败/取消时不得把它画成满格进度条。
func Progress(s Status) int {
	switch s {
	case Queued:
		return 0
	case Running:
		return 50
	default:
		return 100
	}
}

// PortalError 把终态映射成对用户可读的**分类文案**。
//
// 不能直接下发 tasks.error_msg：那是上游原文（含外部域名与真实模型名）。
// 原文仍留在库里供管理台排障，门户与 /v1 只拿到这一句。
func PortalError(s Status) string {
	switch s {
	case Failed:
		return "任务处理失败，已全额返还"
	case Expired:
		return "任务超时，已全额返还"
	case Canceled:
		return "任务已取消"
	default:
		return ""
	}
}

// statusAliases 是上游常见写法 → 平台状态。
//
// 只收录**语义无歧义**的英文写法，不做模糊匹配、**不收录纯数字**：
// `started` 到底是排队还是运行中取决于厂商，而 "1"/"2"/"3" 这类数字码
// 更是各家自己定的（同一份表里把 "1" 定成 queued 会让另一家的 "1"=running
// 被误判成排队，任务永远轮不到终态）。数字码由**知道该厂商语义的适配器**
// 自己映射，那是厂商知识，不是平台知识。
var statusAliases = map[string]Status{
	"succeeded": Succeeded,
	"succeed":   Succeeded,
	"success":   Succeeded,
	"completed": Succeeded,
	"complete":  Succeeded,
	"done":      Succeeded,
	"finished":  Succeeded,
	"ok":        Succeeded,

	"failed":  Failed,
	"failure": Failed,
	"error":   Failed,
	"expired": Expired,

	"canceled":  Canceled,
	"cancelled": Canceled,

	"queued":    Queued,
	"pending":   Queued,
	"waiting":   Queued,
	"submitted": Queued,

	"running":     Running,
	"processing":  Running,
	"generating":  Running,
	"in_progress": Running,
	"inprogress":  Running,
}

// Normalize 把上游状态词归一成平台状态。
//
// 返回值 ok=false 表示**不认识**这个值：调用方必须据此区分「上游确实在跑」与
// 「我们不认识」—— 前者可以继续等，后者等下去只会等到超时（上游成本已付）。
func Normalize(raw string) (Status, bool) {
	s, ok := statusAliases[strings.ToLower(strings.TrimSpace(raw))]
	return s, ok
}

// Known 判断一个值是否属于平台词表（用于拒绝非法筛选参数）。
func Known(s Status) bool {
	switch s {
	case Queued, Running, Succeeded, Failed, Expired, Canceled:
		return true
	default:
		return false
	}
}
