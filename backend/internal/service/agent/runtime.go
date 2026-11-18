package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/money"
	"lingdong-api/internal/service/billing"
	"lingdong-api/internal/service/pricing"

	"go.uber.org/zap"
)

// 本文件是「一轮编排」的执行器：LLM ↔ 工具循环、预算闸、SSE 事件、成本记账。
//
// 三条约束：
//  1. 一轮有**硬上限**（步数 / token / 时长）。没有上限的 agent 循环在
//     上游返回结构化错误、或模型反复调同一个工具时会无限烧钱。
//  2. 成本记 agent_runs，**不进 usages**（管理台调用没有 api_key、不扣额度、
//     不该出现在用户的调用记录里）。
//  3. 事件是**唯一**输出通道：控制器只负责把 Event 编码成 SSE 帧，
//     不解析模型的任何文本 —— 否则"前端显示"与"实际发生"会各有一套逻辑。

const (
	// runMaxDuration 是一轮编排的硬上限。
	//
	// 必须小于最外层代理的读超时（本机链路 nginx 300s），否则客户端会先看到 504，
	// 而服务端还在跑、还在花钱。留 60s 余量给收尾与落库。
	runMaxDuration = 240 * time.Second
	// toolArgsBriefMax / toolResultBriefMax 是 SSE 里工具参数的展示上限。
	toolArgsBriefMax   = 200
	toolResultBriefMax = 300
)

var (
	// ErrAgentDisabled 表示 AI 编排总闸关闭。
	ErrAgentDisabled = errors.New("agent: 功能未启用")
	// ErrBudgetExceeded 表示今日成本预算已用尽。
	ErrBudgetExceeded = errors.New("agent: 今日成本预算已用尽")
)

// Event 是一条 SSE 事件（type + 该类型需要的字段）。
//
// 用一个大结构而不是 map：字段拼错时编译器会拦，而 map 拼错只是"前端少显示一个数"。
type Event struct {
	Type string `json:"type"`

	RunID     uint64 `json:"run_id,omitempty"`
	MessageID uint64 `json:"message_id,omitempty"`
	Seq       int64  `json:"seq,omitempty"`
	// Model 是本轮实际使用的对话模型（不是设置里的期望值）。
	Model string `json:"model,omitempty"`

	Text string `json:"text,omitempty"`

	// Step 是本次工具调用/思考所属的步（1-based）。前端据此把同一步的
	// "说了什么 + 调了什么"归到一起，而不是一串无分组的事件流。
	Step int `json:"step,omitempty"`

	Name   string `json:"name,omitempty"`
	CallID string `json:"call_id,omitempty"`
	// ArgsBrief / Brief 分别是参数与结果的摘要（截断过，只用于展示）。
	ArgsBrief string `json:"args_brief,omitempty"`
	OK        *bool  `json:"ok,omitempty"`
	Brief     string `json:"brief,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	// MS 是工具执行耗时（毫秒）。没有它，卡片就只能显示"完成"，
	// 慢工具与瞬间失败的探测在界面上长得一样。
	MS int64 `json:"ms,omitempty"`

	ProposalID uint64    `json:"proposal_id,omitempty"`
	Kind       string    `json:"kind,omitempty"`
	PlanHash   string    `json:"plan_hash,omitempty"`
	Summary    string    `json:"summary,omitempty"`
	Status     string    `json:"status,omitempty"`
	DiffStats  *DiffStat `json:"diff_stats,omitempty"`

	TokensIn  int   `json:"tokens_in,omitempty"`
	TokensOut int   `json:"tokens_out,omitempty"`
	EstAmount int64 `json:"est_amount,omitempty"`
	Steps     int   `json:"steps,omitempty"`
	// ContextTokens 是**首步**请求的 prompt_tokens（本轮真实上下文占用）。
	// 不能用 TokensIn：那是每步整份 prompt 的累加，显示出来会放大 3–5 倍。
	ContextTokens int `json:"context_tokens,omitempty"`
	// ContextWindow 是本轮生效窗口；与上面一起才能回答"用了多少 / 有多少"。
	ContextWindow int `json:"context_window,omitempty"`
	// CompactLevel / SummarizedTurns 说明本轮压缩到什么程度（界面要留痕）。
	CompactLevel    int  `json:"compact_level,omitempty"`
	SummarizedTurns int  `json:"summarized_turns,omitempty"`
	Estimated       bool `json:"estimated,omitempty"`

	Code      string `json:"code,omitempty"`
	Message   string `json:"message,omitempty"`
	Retryable bool   `json:"retryable,omitempty"`
	Partial   bool   `json:"partial,omitempty"`

	// AskID / Ask 仅 type="ask" 使用（type="ping" 只带 AskID 与 MS）。
	// 提问也走这条统一通道：不加第二个事件出口，前端 switch 里也只多一个 case。
	AskID uint64   `json:"ask_id,omitempty"`
	Ask   *AskView `json:"ask,omitempty"`
}

// AskView 是下发给前端的一张待答卡片。
//
// ExpiresIn 是**秒**（不是绝对时间戳）：客户端时钟不可信，
// 而"还剩多少秒"用同一个起点算出来，两边不会差出时区量级的偏差。
type AskView struct {
	Question      string      `json:"question"`
	Why           string      `json:"why,omitempty"`
	Options       []AskOption `json:"options"`
	Multi         bool        `json:"multi"`
	AllowFreeText bool        `json:"allow_free_text"`
	ExpiresIn     int         `json:"expires_in"`
}

// DiffStat 是提案的增删改计数。
type DiffStat struct {
	Added   int `json:"added"`
	Changed int `json:"changed"`
	Removed int `json:"removed"`
}

// RunInput 是一轮编排的入参。
type RunInput struct {
	SessionID uint64
	MessageID uint64
	AdminID   uint64
	UserText  string
	// ModelID 为空 = 用设置或系统默认对话模型。
	ModelID string
	// DocIDs 是本轮显式引用的文档（为空则由系统给最近 5 份的目录）。
	DocIDs []uint64
	// Scope 是本次入口允许触碰的实体（空 = 不限制，见 toolEnv.Scope）。
	Scope []string
	// UserSeq 是这条用户消息的会话内序号：历史只取 seq < UserSeq。
	//
	// 由调用方从 AppendMessage 的返回值带进来（它刚写完就拿到了），
	// 这样装配不需要再多一次"按 id 查 seq"的往返。
	UserSeq int64
}

// Emitter 是事件出口（控制器把它写成 SSE 帧；测试用它收集事件）。
type Emitter func(Event) error

// Run 执行一轮编排：装配上下文 → 循环（LLM + 工具）→ 记账 → 收尾事件。
//
// 所有终止路径（正常/超时/断线/上游错误/上限/panic）都只走 finalize 这一个出口：
// 落一条 assistant 消息（含已产生的过程与失败原因）→ finishRun → usage → done。
// 返回 error 仅表示"连事件都发不出去"；调用方不要再重复发错误事件。
func Run(ctx context.Context, in RunInput, emit Emitter) error {
	// 落库必须用一个**不被取消**的 ctx：客户端断开时 in.ctx 已 cancel，
	// 用它写库会全部失败 —— 那正是"断线就丢内容"的成因。
	// 它必须在最顶部构建：前置失败（禁用/无模型/预算）也要落失败凭证。
	persistCtx := context.WithoutCancel(ctx)
	runCtx, cancel := context.WithTimeout(ctx, runMaxDuration)
	defer cancel()

	var run model.AgentRun // ID=0 表示 run 行未建（前置失败），finishRun 会跳过
	started := time.Now()
	var env *toolEnv
	// lastStreamed 跟踪已流出的正文：panic 时 out 不可用，靠它保留半截答复。
	lastStreamed := ""
	finalized := false
	// extra 是本轮**追加**给模型的消息原文（assistant tool_calls + tool），
	// 用于落库成 ctx。它只追加、不压缩 —— 压缩是"这次请求装不下"的临时手段，
	// 把压缩结果存进 ctx 会让下一轮也拿不到原文（越滚越少）。
	var extra []Message
	// firstPrompt 是**首步**的 prompt_tokens（本轮真实上下文占用）。
	// TokensIn 是跨步累加，拿它当"上下文占用"会放大 3–5 倍。
	firstPrompt := 0
	// estimated 表示有步骤没拿到上游 usage（用量是估算值）。
	estimated := false
	// inRunLevel 是轮内压缩达到的最高级别（与装配期的 plan.Level 取较大者）。
	inRunLevel := 0

	// finalize 是**唯一**终态出口：
	//  1. 幂等（finalized 守卫）——done 只发一次、消息只落一次；
	//  2. 落库条件：失败一律落（运营要能复盘"为什么没动静"）；成功仅有正文/过程时落；
	//  3. 失败/截断原因作为 error 段进 parts（前端与实时轮同一条渲染管线）；
	//  4. error 事件只在这里发（code+errMsg 非空时），与 done 成对出现；
	//     纯断线（errMsg 为空）不发 error —— 对端已消失，写了也收不到。
	finalize := func(status, code, errMsg, content string, partialOut bool) error {
		if finalized {
			return nil
		}
		finalized = true
		// content 非空才剔除末段 text（它就是 content）；content 为空时保留全部叙述，
		// 否则 max_steps/loop_guard 这类"没有最终答复"的轮会丢掉最后一段话。
		parts := buildParts(env, content != "")
		retryable := retryableOf(code)
		if code != "" && errMsg != "" {
			parts = appendErrorPart(parts, code, errMsg, retryable)
		}
		// 落库条件：有正文或有过程就落。前置失败必带 error 段（parts 非空）→ 一定落；
		// 纯断线且什么都没生成（无内容、无过程）时没有可保留的东西，跳过。
		if strings.TrimSpace(content) != "" || parts != nil {
			// 摘要与 ctx 在同一处、同一份内存轨迹上派生：只有一条写入路径，
			// 不会出现"parts 说调了工具、ctx 里没有"这种分叉。
			summary := ruleSummary(in.UserText, env.traceItems(), content, status, code)
			msg, err := AppendMessage(persistCtx, in.SessionID, "assistant", content, parts, MessageMeta{
				TokensIn: run.TokensIn, TokensOut: run.TokensOut,
				Ctx: encodeCtx(extra), Summary: summary, Status: status,
			})
			if err != nil {
				// 落库失败不能让 run 卡在 running，也不能没有 done：记日志后照常收尾。
				logger.L().Error("agent: assistant 消息落库失败",
					zap.Error(err), zap.Uint64("session", in.SessionID), zap.String("status", status))
			} else {
				// 账单与上下文占用必须能归因到这条回答（MessageID 指的是用户消息）。
				run.AssistantMessageID = msg.ID
			}
		}
		// 等待回答的时长不计入 latency：那是"人在打字"，不是"模型很慢"。
		// 不扣的话，用一次 ask_user 就能把 stats 的均值拉高一分钟量级。
		statStart := started
		if env != nil {
			statStart = started.Add(env.WaitTotal)
		}
		if run.ID != 0 {
			// 先算成本再落 run 行：反过来的话成本列永远是 0（预算闸形同虚设）。
			run.TokensEstimated = estimated
			run.QuotaEst = estimateQuota(run.ModelID, run.TokensIn, run.TokensOut)
			finishRun(persistCtx, &run, status, code, statStart)
		}
		_ = emit(Event{
			Type: "usage", TokensIn: run.TokensIn, TokensOut: run.TokensOut,
			EstAmount: run.QuotaEst, Steps: run.Steps,
			ContextTokens: run.ContextTokens, ContextWindow: run.ContextWindow,
			CompactLevel: int(run.CompactLevel), SummarizedTurns: run.SummarizedTurns,
			Estimated: run.TokensEstimated,
		})
		if code != "" && errMsg != "" {
			_ = emit(Event{Type: "error", Code: code, Message: errMsg, Retryable: retryable})
		}
		// done 必须先发：调用方（控制器）要不要写注释帧由返回错误决定，
		// 但前端拿到 done 才能收尾。失败时返回 RunError，保留"Run 会把原因
		// 交给调用方"的既有契约。
		doneErr := emit(Event{Type: "done", MessageID: in.MessageID, Partial: partialOut || status != "ok"})
		if doneErr != nil {
			return doneErr
		}
		if status == "failed" && code != "" {
			return &RunError{Code: code, Message: errMsg}
		}
		return nil
	}

	// panic 兜底：与主循环同一 goroutine，SSE writer 仍可写；finalize 幂等防双发。
	defer func() {
		if r := recover(); r != nil {
			logger.L().Error("agent: run panic", zap.Any("panic", r), zap.Stack("stack"))
			_ = finalize("failed", "internal_error", "内部错误，已保留本轮过程", lastStreamed, true)
		}
	}()

	// 装配阶段一律用 persistCtx：这些是本地读（设置/会话/预算/历史），
	// 客户端断线不该把"读设置"变成 context canceled 的假失败。
	set, err := Settings(persistCtx)
	if err != nil {
		return finalize("failed", "internal_error", err.Error(), "", true)
	}
	if !set.Enabled {
		return finalize("failed", "agent_disabled", errsText(ErrAgentDisabled), "", true)
	}
	// 模型优先级：本次请求指定 → 会话指定 → 设置里的全站默认 → 系统派生的默认。
	//
	// 会话级 model_id 必须参与：设置里的那个是**全站默认**，而运营在输入框的
	// 模型菜单里选的是"这次对话用哪个"。不读会话字段，选择就恒不生效，
	// 而且没有任何报错 —— 典型的"选了但没变"。
	var sess model.AgentSession
	sessModelID := ""
	if err := db.DB.WithContext(persistCtx).Select("id", "model_id").First(&sess, in.SessionID).Error; err == nil {
		sessModelID = sess.ModelID
	}
	modelID, ok := ResolveChatModel(firstNonEmpty(in.ModelID, sessModelID, set.ModelID))
	if !ok {
		// 会话指定的模型被删/被停时，必须说清"是这个会话选的模型没了"。
		// 报一句通用的"没有可用的对话模型"会让人去改设置与渠道，而真正要做的
		// 只是在输入框的模型菜单里换一个 —— 两件事的排查成本差一个数量级。
		if sessModelID != "" && strings.TrimSpace(in.ModelID) == "" {
			return finalize("failed", "agent_model_unavailable",
				"本会话指定的对话模型「"+sessModelID+"」当前不可用，请在输入框的模型菜单里换一个", "", true)
		}
		return finalize("failed", "agent_model_unavailable",
			"没有可用的对话模型：请先在模型管理配置一个对话模型并绑定到支持对话的渠道", "", true)
	}
	// 预算闸：进循环前先看今日已花，超了直接拒绝（而不是跑完再发现）。
	if set.DailyAmountBudget > 0 {
		spent, err := DailySpend(persistCtx)
		if err == nil && spent >= set.DailyAmountBudget {
			return finalize("failed", "agent_budget_exceeded", errsText(ErrBudgetExceeded), "", true)
		}
	}

	run = model.AgentRun{
		SessionID: in.SessionID, MessageID: in.MessageID, ModelID: modelID,
		Status: "running",
	}
	if err := db.DB.WithContext(persistCtx).Create(&run).Error; err != nil {
		return finalize("failed", "internal_error", err.Error(), "", true)
	}
	// 上一轮遗留的待答问题一律结算为 expired：一个会话任何时刻最多一张可答卡片，
	// 否则刷新后会出现"点得动、点了却没人在等"的死卡片（run 早就结束了）。
	expirePendingAsks(persistCtx, in.SessionID)

	env = &toolEnv{
		Ctx: runCtx, SessionID: in.SessionID, MessageID: in.MessageID, AdminID: in.AdminID,
		Scope: in.Scope,
		RunID: run.ID, MaxSteps: set.MaxSteps, AskReady: true, Emit: emit,
		Calls: map[string]int{}, FailStreak: map[string]int{}, ArgStreak: map[string]int{},
		Repeat: map[string]int{},
	}

	// 生效窗口按「模型声明 > 全局设置 > 内置默认」求一次，整轮复用。
	lim, err := EffectiveLimits(persistCtx, modelID)
	if err != nil {
		return finalize("failed", "internal_error", err.Error(), "", true)
	}
	run.ContextWindow = lim.ContextWindow
	plan, err := BuildContext(persistCtx, BuildInput{
		SessionID: in.SessionID, MessageID: in.MessageID,
		UserSeq: in.UserSeq, UserText: in.UserText, DocIDs: in.DocIDs, ModelID: modelID,
	}, lim)
	if err != nil {
		// 装配期的失败是可解释的：窗口太小 / 上下文满。直接把 code 与中文原文交给前端，
		// 不要再包一层 "context_failed"（那会让"该改什么"这条信息消失）。
		if ce, ok := err.(*ContextError); ok {
			return finalize("failed", ce.Code, ce.Message, "", true)
		}
		return finalize("failed", "context_failed", err.Error(), "", true)
	}
	run.CompactLevel = int16(plan.Level)
	run.SummarizedTurns = plan.SummarizedTurns
	msgs := plan.Messages
	tools := plan.Tools
	if err := emit(Event{Type: "start", RunID: run.ID, MessageID: in.MessageID, Model: modelID}); err != nil {
		// 连 start 都发不出去：对端已断，落 aborted 凭证后静默收尾（不再发事件）。
		return finalize("aborted", "", "", "", true)
	}

	for step := 1; step <= set.MaxSteps; step++ {
		run.Steps = step
		// ask_user 靠这两个值判断"问完还有没有下一步来用答案"。
		env.Step = step
		if err := runCtx.Err(); err != nil {
			// 客户端断开或超时（步与步的间隙）：没有正在流式的半截，
			// 取上下文里最后一条 assistant 文本落库（断了就丢会让运营重述需求）。
			content := partialContent(msgs, "")
			if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
				return finalize("aborted", "agent_timeout",
					"本轮编排超过时长上限，已保留已生成的内容；可以继续追问让它接着做", content, true)
			}
			return finalize("aborted", "", "", content, true)
		}
		// 轮内压缩：本轮已累积的工具结果可能把窗口塞爆（读 5 份 6KB 文档就够了）。
		// 必须在**发请求之前**压，否则上游直接 400，而 400 的文案不会告诉你"是历史太长"。
		if fitted, lv, ok := FitMessages(msgs, tools, plan.Window, plan.Output); lv > 0 || !ok {
			msgs = fitted
			if lv > inRunLevel {
				inRunLevel = lv
				if int16(lv) > run.CompactLevel {
					run.CompactLevel = int16(lv)
				}
			}
			if !ok {
				return finalize("failed", CodeContextOverflow,
					"上下文已满：本轮读入的内容超过了模型窗口，请新建对话继续（或换窗口更大的模型）", "", true)
			}
		}
		// 预算闸：spent 是**已花**（上游实际，跨步累加），next 是**下一步**的整份输入。
		// 两者相加再加输出上限，才是"这一步会不会超预算"的准确判据 ——
		// 只看 spent 会允许最后一步超支，只看 next 会重复计算同一份上下文。
		next := estimateTokens(msgs) + estimateToolsTokens(tools) + plan.Output
		if overBudget(set.MaxTokensPerRun, run.TokensIn+run.TokensOut, next) {
			if step == 1 {
				// 第 1 步就超 = 预算比固定开销还小，属于配置错，而不是"用完了"。
				return finalize("failed", "agent_budget_too_small", fmt.Sprintf(
					"单轮 token 预算 %d 小于单次请求的固定开销（约 %d：系统提示词 + %d 个工具声明 + 本轮输入 + 输出上限）。请在运行参数里调大，或设为 0（不限）",
					set.MaxTokensPerRun, next, len(tools)), "", true)
			}
			return finalize("ok", "max_tokens", fmt.Sprintf(
				"本轮 token 预算用尽（已用 %d / 预算 %d），可以继续追问让它接着做",
				run.TokensIn+run.TokensOut, set.MaxTokensPerRun), "", true)
		}
		// 每步开头先发一条 step：这一步可能要几秒才出第一个字，没有它，
		// 前端在这几秒里只能停在上一段的末尾，看起来像卡死。
		_ = emit(Event{Type: "step", Step: step})
		out, err := StreamChat(runCtx, modelID, msgs, tools, plan.Output,
			func(text string) error {
				lastStreamed += text
				env.appendText(text)
				return emit(Event{Type: "delta", Text: text, Step: step})
			},
			func(reason string) error {
				env.appendReason(reason)
				return emit(Event{Type: "reasoning", Text: reason, Step: step})
			})
		if out != nil {
			run.TokensIn += out.TokensIn
			run.TokensOut += out.TokensOut
			if out.Estimated {
				estimated = true
			}
			if step == 1 {
				// 首步的 prompt_tokens 才是"本轮上下文占用"；
				// 拿不到（上游没给 usage）就用装配期的估算值。
				firstPrompt = out.TokensIn
				if firstPrompt == 0 {
					firstPrompt = plan.Tokens
				}
				run.ContextTokens = firstPrompt
			}
		}
		if err != nil {
			if runCtx.Err() != nil {
				// 半截正文优先取本步已流出的 out.Content（它不在 msgs 里）；
				// 取不到再退化取上下文最后一条 assistant 文本。
				// 不用 lastStreamed：它累积了前几步的全文，拼进 content 会与轨迹重复。
				content := contentOf(out)
				if strings.TrimSpace(content) == "" {
					content = partialContent(msgs, "")
				}
				if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
					return finalize("aborted", "agent_timeout",
						"本轮编排超过时长上限，已保留已生成的内容；可以继续追问让它接着做", content, true)
				}
				return finalize("aborted", "", "", content, true)
			}
			return finalize("failed", "upstream_error", err.Error(), contentOf(out), true)
		}
		if len(out.ToolCalls) == 0 {
			// 没有工具调用 = 本轮答复结束。落助手消息（真正的产出在这里）。
			// 末段正文不入轨迹：它就是这条消息的 content，留在轨迹里会渲染两遍。
			return finalize("ok", "", "", out.Content, false)
		}
		// 有工具调用：先把 assistant 的 tool_calls 回显进上下文（否则上游会认为调用无来源）。
		callMsg := assistantToolCallMessage(out.Content, out.ToolCalls)
		msgs = append(msgs, callMsg)
		extra = append(extra, callMsg)
		for _, call := range out.ToolCalls {
			// 摘要必须**先脱敏再截断**：dry_run_draft 的参数里带 api_key，
			// 而 200 字节的窗口正好足以把它整段露在管理台上。
			argsBrief := SafeArgsBrief(call.Arguments, toolArgsBriefMax)
			_ = emit(Event{
				Type: "tool_call", Name: call.Name, CallID: call.ID,
				ArgsBrief: argsBrief, Step: step,
			})
			callStarted := time.Now()
			fingerprint := call.Name + "|" + call.Arguments
			repeated := env.Calls[fingerprint] >= 2
			var result any
			if repeated {
				// 同一工具同一参数第 3 次：不执行。反复调用不产生新信息，只烧 token。
				// 刻意**不**递增 Calls（继续拒绝是对的），也**不**计失败（它不是失败）。
				result = toolError("repeated_call",
					"同一工具与相同参数已调用两次，本次未执行；请换参数、换工具，或直接回答", "")
			} else {
				env.Calls[fingerprint]++
				result = ExecTool(env, call)
			}
			// 计数与"下一步怎么做"的注入必须早于序列化：
			// toolMsg / traceItem.B / SSE Brief 共用同一份 json.Marshal(result)，
			// 晚改任何一处都会让"模型看到的"与"运营看到的"分叉。
			noteToolOutcome(env, call.Name, result, repeated)
			injectArgAdvice(env, call.Name, result, repeated)
			elapsed := time.Since(callStarted).Milliseconds()
			buf, _ := json.Marshal(result)
			okFlag := true
			if m, isMap := result.(map[string]any); isMap {
				if v, has := m["ok"].(bool); has {
					okFlag = v
				}
			}
			resultBrief := clipText(string(buf), toolResultBriefMax)
			truncated := strings.Contains(string(buf), `"truncated":true`)
			_ = emit(Event{
				Type: "tool_result", Name: call.Name, CallID: call.ID, OK: &okFlag,
				Brief: resultBrief, Truncated: truncated, MS: elapsed, Step: step,
			})
			env.appendTool(traceItem{
				K: traceTool, N: call.Name, A: argsBrief, B: resultBrief,
				OK: &okFlag, S: step, MS: elapsed,
			})
			toolMsg := Message{
				Role: "tool", ToolCallID: call.ID, Name: call.Name,
				Content: string(buf),
			}
			msgs = append(msgs, toolMsg)
			extra = append(extra, toolMsg)
		}
		// 守卫：参数类与业务类分开计数（判据表见 guard.go 的 guardTrip）。
		if code, msg := guardTrip(env); code != "" {
			return finalize("ok", code, msg, "", true)
		}
		if len(env.ToolsUsed) >= 40 {
			return finalize("ok", "agent_loop_guard", "本轮操作数已达上限；可继续追问让它接着做", "", true)
		}

		// 工具跑完就发提案事件：运营能在模型继续说话之前就看到"它要改什么"。
		for _, pid := range env.ProposedIDs {
			if p, ok := emitProposal(emit, pid); ok {
				env.appendProposal(p.ID, planSummary(json.RawMessage(p.Plan)))
			}
			env.AllProposed = append(env.AllProposed, pid)
		}
		env.ProposedIDs = nil
	}
	// 步数用尽：明确告知，而不是假装正常结束（运营会以为 AI 说完了）。
	return finalize("ok", "max_steps", "本轮步数用尽，可继续追问让它接着做", "", true)
}

// overBudget 判断"这一步会不会超预算"。
//
// 判据 = 已花 + 本次请求的整份输入 + 输出上限 > 预算。
// 三个常见错法：
//   - 只看已花 → 允许最后一步大幅超支（一轮 12 步，最后一步可能就占 20%）；
//   - 只看本次输入 → 把"每步都要重付整份 prompt"这件事漏掉，永远不触发；
//   - 忘了输出上限 → 上游按 input+max_tokens 判窗口，预扣却按 input 判，两边对不上。
//
// 预算 0 = 不限。
func overBudget(budget, spent, next int) bool {
	if budget <= 0 {
		return false
	}
	return spent+next > budget
}

// contentOf 取上游半途而废时已流出的正文（out 可能为 nil）。
func contentOf(out *chatOutcome) string {
	if out == nil {
		return ""
	}
	return out.Content
}

// partialContent 取"断线/超时"时刻该落库的正文：优先正在流式输出的半截
// （msgs 里没有它），退化取上下文里最后一条 assistant 文本。
func partialContent(msgs []Message, streamed string) string {
	if s := strings.TrimSpace(streamed); s != "" {
		return s
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" && strings.TrimSpace(msgs[i].Content) != "" {
			return msgs[i].Content
		}
	}
	return ""
}

// retryableOf 是错误码 → 是否可重试的唯一映射（SSE error 事件与落库 error 段共用）。
func retryableOf(code string) bool {
	switch code {
	// agent_tool_args_invalid：改问法/换粒度通常能救回来，值得一次重试；
	// agent_loop_guard 刻意不在其中 —— 重试=重发同一条消息，同一条死路会再烧一遍。
	case "agent_timeout", "upstream_error", "max_steps", "max_tokens", "agent_tool_args_invalid", "internal_error":
		return true
	default:
		// context_*_ / agent_budget_too_small 一律 false：它们是"要人去改设置/换模型"，
		// 给一个重试按钮只会让运营重复点、重复看到同一条失败。
		return false
	}
}

// assistantToolCallMessage 把 assistant 的 tool_calls 回显成上下文消息。
func assistantToolCallMessage(content string, calls []ToolCall) Message {
	wire := make([]wireToolCall, 0, len(calls))
	for _, c := range calls {
		wire = append(wire, wireToolCall{
			ID: c.ID, Type: "function",
			Function: wireFunctionCall{Name: c.Name, Arguments: c.Arguments},
		})
	}
	return Message{Role: "assistant", Content: content, ToolCalls: wire}
}

// emitProposal 发一条 proposal 事件并回传提案行（供调用方记进轨迹）。
//
// 返回 false 表示提案取不到（被并发删除等）：此时**不发事件**，
// 因为前端拿到一个点不开的提案号只会显示一张空白卡。
func emitProposal(emit Emitter, id uint64) (model.AgentProposal, bool) {
	var p model.AgentProposal
	if err := db.DB.First(&p, id).Error; err != nil {
		return p, false
	}
	added, changed, removed := DiffStats(json.RawMessage(p.Diff))
	_ = emit(Event{
		Type: "proposal", ProposalID: p.ID, Kind: p.Kind, Status: p.Status,
		PlanHash:  p.PlanHash,
		Summary:   planSummary(json.RawMessage(p.Plan)),
		DiffStats: &DiffStat{Added: added, Changed: changed, Removed: removed},
	})
	return p, true
}

func planSummary(raw json.RawMessage) string {
	var s struct {
		Summary string `json:"summary"`
	}
	_ = json.Unmarshal(raw, &s)
	return clipText(s.Summary, 120)
}

// finishRun 收尾 agent_runs 行（终态、步数、token、耗时）。
//
// 用 map 而不是 Save：Save 会覆盖 created_at 以外全部列（含并发的其它写入）。
func finishRun(ctx context.Context, run *model.AgentRun, status, code string, started time.Time) {
	updates := map[string]any{
		"status":     status,
		"steps":      run.Steps,
		"tokens_in":  run.TokensIn,
		"tokens_out": run.TokensOut,
		"quota_est":  run.QuotaEst,
		"error_code": code,
		"latency_ms": int(time.Since(started).Milliseconds()),
		// 上下文计量：没有这几列，"为什么这轮被摘要/被拦"永远查不清。
		"assistant_message_id": run.AssistantMessageID,
		"context_tokens":       run.ContextTokens,
		"context_window":       run.ContextWindow,
		"compact_level":        run.CompactLevel,
		"summarized_turns":     run.SummarizedTurns,
		"tokens_estimated":     run.TokensEstimated,
	}
	if err := db.DB.WithContext(ctx).Model(&model.AgentRun{}).Where("id = ?", run.ID).
		Updates(updates).Error; err != nil {
		_ = err
	}
}

// estimateQuota 估算本轮成本（不扣费，只用于预算闸与展示）。
//
// 复用 billing.Estimate（同一套取价函数），不在这里写第二份公式：
// 自己乘一个"倍率"是旧实现的错法 —— 它算出来的数与真实冻结额从来不一致，
// 预算闸因此形同虚设。取不到价格时返回 0 并让调用方标记，不假装知道。
func estimateQuota(modelID string, tokensIn, tokensOut int) int64 {
	snap, err := pricing.Get()
	if err != nil || snap == nil {
		return 0
	}
	for i := range snap.Models {
		m := snap.Models[i]
		if m.ID != modelID {
			continue
		}
		amount, err := billing.Estimate(billing.Snapshot{
			ModelID:     m.ID,
			BillingMode: m.BillingMode,
			Price:       m.Prices,
			SpecPrice:   m.SpecPrices,
			// 估算是运营侧的预算提示，不叠分组折扣：宁可高估被拦，
			// 也不要因为算漏折扣而放行一个会超预算的运行。
			Discount:   money.Scale,
			Currency:   snap.Currency,
			CapturedAt: time.Now(),
		}, int64(tokensIn), int64(tokensOut), billing.Usage{})
		if err != nil {
			return 0
		}
		return amount
	}
	return 0
}

// RunError 是一轮编排失败的结构化原因（code + 可直接展示的文案）。
type RunError struct {
	Code    string
	Message string
}

func (e *RunError) Error() string { return e.Code + "：" + e.Message }

func errsText(err error) string {
	if err == nil {
		return ""
	}
	return strings.TrimPrefix(err.Error(), "agent: ")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// TakeLock 抢会话的执行锁（同一会话同时只允许一轮）。
//
// 用 Redis 的 SET NX 而不是内存锁：多副本部署下内存锁只锁得住本实例，
// 两轮并发跑同一个会话会让 messages.seq 交错、上下文互相污染。
func TakeLock(ctx context.Context, sessionID uint64) (func(), bool) {
	if sessionID == 0 || !db.RedisReady() {
		return func() {}, true
	}
	key := fmt.Sprintf("ld:agent:run:%d", sessionID)
	ok, err := db.RDB.SetNX(ctx, key, "1", runMaxDuration).Result()
	if err != nil || !ok {
		return func() {}, false
	}
	return func() {
		_ = db.RDB.Del(context.WithoutCancel(ctx), key).Err()
	}, true
}
