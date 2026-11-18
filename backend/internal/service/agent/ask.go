package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

// 本文件是"助手向运营提问"的唯一实现。
//
// 设计要点（改之前先读）：
//  1. **同一轮内阻塞**：提问发生在工具调用里，run 还活着、SSE 连接还在，
//     运营答完模型立刻接着做 —— 不是"结束本轮、下一轮再说"。
//  2. **状态真相源是 agent_asks**：parts 里的 `k:"ask"` 只是渲染快照。
//     等待中的问题库里必须有行，否则刷新/断线就再也看不到它。
//  3. **等待有上限**：60s 且给收尾留 25s。超时一律 ok:true（见 respondToModel），
//     绝不能返回 ok:false —— 那会让 FailStreak 累加，三次就把整轮判成失败。
//  4. **answer 端点不取会话锁**（见 controller）：作答是"正在跑的那一轮的输入"。

const (
	// askWaitMax 是单次等待上限。定 60s 而不是 300s：模型在等人，
	// 整轮 240s 的预算里最多容纳两次提问 + 一次收尾。
	askWaitMax = 60 * time.Second
	// askReserveAfter 是"答完之后至少要留给模型的时间"。
	askReserveAfter = 25 * time.Second
	// askPollInterval 是多副本下的兜底轮询（进程内信号可能落到别的实例）。
	askPollInterval = time.Second
	// askPingInterval 是等待期心跳间隔（写注释帧，防代理掐断长连接）。
	askPingInterval = 15 * time.Second
	// askMaxPerRun 是单轮提问上限：问第三次就开始变成"审问"了。
	askMaxPerRun = 2
)

// 提问的终态（也是落库 status 与 trace 段 st 的取值）。
const (
	AskPending     = "pending"
	AskAnswered    = "answered"
	AskTimeout     = "timeout"
	AskExpired     = "expired"
	AskDenied      = "denied"
	AskNoTime      = "no_time"
	AskAborted     = "aborted"
	AskUnavailable = "unavailable"
)

// AskOption 是一个选项。value 是回传值，label 是人看的，detail 说明代价。
type AskOption struct {
	Value  string `json:"value"`
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
}

// AskRequest 是一次提问（工具层校验后的形态）。
type AskRequest struct {
	SessionID uint64
	MessageID uint64
	RunID     uint64
	Step      int
	Question  string
	Why       string
	Options   []AskOption
	Multi     bool
	AllowFree bool
}

// AskAnswer 是等待的结果。
type AskAnswer struct {
	AskID uint64
	// Status 是终态（answered / timeout / expired / aborted ...）。
	Status   string
	Selected []string
	// SelectedLabels 与 Selected 一一对应的人话文案（选项的 label）。
	// 落库的"答了什么"用 label 而不是 value：运营回看时看到 "per_spec"
	// 得自己回忆它对应哪个按钮，而轨迹是给人在复盘时读的。
	SelectedLabels []string
	FreeText       string
	Skipped        bool
	Waited         time.Duration
}

// askHub 是进程内唤醒（回答端点 CAS 成功后立刻叫醒等待者）。
//
// 多副本部署时信号可能留在另一个实例上，因此等待循环同时按 1s 轮询 DB：
// 少一次唤醒最多慢 1 秒，丢一次唤醒则是"等满 60 秒"，这两者差距太大。
var askHub = struct {
	mu      sync.Mutex
	waiters map[uint64]chan struct{}
}{waiters: map[uint64]chan struct{}{}}

func askWaitChan(id uint64) chan struct{} {
	askHub.mu.Lock()
	defer askHub.mu.Unlock()
	ch, ok := askHub.waiters[id]
	if !ok {
		ch = make(chan struct{})
		askHub.waiters[id] = ch
	}
	return ch
}

func dropAskWaiter(id uint64) {
	askHub.mu.Lock()
	defer askHub.mu.Unlock()
	delete(askHub.waiters, id)
}

// signalAsk 唤醒等待者（没有等待者时是空操作）。
func signalAsk(id uint64) {
	askHub.mu.Lock()
	ch, ok := askHub.waiters[id]
	delete(askHub.waiters, id)
	askHub.mu.Unlock()
	if ok {
		close(ch)
	}
}

/* ------------------------------ 存储 ------------------------------ */

func createAskRow(ctx context.Context, a *model.AgentAsk) error {
	return db.DB.WithContext(ctx).Create(a).Error
}

func loadAskRow(ctx context.Context, id uint64) (*model.AgentAsk, error) {
	var row model.AgentAsk
	if err := db.DB.WithContext(ctx).First(&row, id).Error; err != nil {
		return nil, err
	}
	return &row, nil
}

// answerAskRow 用 CAS 把 pending 改成 answered。
//
// 返回 false 表示"已经不是我该答的那一条"（别人先答了，或已过期）。
// 双击提交、两个管理员同时选同一个问题，都靠这一步兜住。
func answerAskRow(ctx context.Context, id, by uint64, ans AskAnswer) (bool, error) {
	buf, err := json.Marshal(map[string]any{
		"selected": ans.Selected, "free_text": ans.FreeText, "skipped": ans.Skipped,
	})
	if err != nil {
		return false, err
	}
	res := db.DB.WithContext(ctx).Model(&model.AgentAsk{}).
		Where("id = ? AND status = ?", id, AskPending).
		Updates(map[string]any{
			"status":      AskAnswered,
			"answer":      model.JSON(buf),
			"answered_by": by,
			"answered_at": time.Now(),
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// expireAskRow 把一条 pending 置为 expired（幂等）。
func expireAskRow(ctx context.Context, id uint64) {
	_ = db.DB.WithContext(ctx).Model(&model.AgentAsk{}).
		Where("id = ? AND status = ?", id, AskPending).
		Update("status", AskExpired).Error
}

// expirePendingAsks 在新一轮开场时清掉上一个会话遗留的待答行。
//
// 保证"任何时刻一个会话最多一张可答卡片"：否则上一轮断开后留下的 pending
// 会一直显示"待回答"，而它所属的 run 早就结束了。
func expirePendingAsks(ctx context.Context, sessionID uint64) {
	_ = db.DB.WithContext(ctx).Model(&model.AgentAsk{}).
		Where("session_id = ? AND status = ?", sessionID, AskPending).
		Update("status", AskExpired).Error
}

/* ------------------------------ 等待实现 ------------------------------ */

// askAndWait 落库 → 发事件 → 阻塞等待 → 结算 trace 段。
//
// 全部 DB 写用 persistCtx（不受断线影响）：断线时要把这一条置为 expired，
// 用请求 ctx 会写失败，留下一个永远 pending 的行。
func (e *toolEnv) askAndWait(ctx context.Context, req AskRequest) AskAnswer {
	persistCtx := context.WithoutCancel(ctx)
	started := time.Now()

	opts, _ := json.Marshal(req.Options)
	row := &model.AgentAsk{
		SessionID: req.SessionID, MessageID: req.MessageID, RunID: req.RunID,
		Step: req.Step, Question: clipRunes(req.Question, 300), Why: clipRunes(req.Why, 300),
		Options: model.JSON(opts), Multi: req.Multi, AllowFree: req.AllowFree,
		Status: AskPending, ExpiresAt: started.Add(askWaitMax),
	}
	if err := createAskRow(persistCtx, row); err != nil {
		return AskAnswer{Status: AskUnavailable}
	}

	idx := e.appendAsk(traceItem{
		K: traceAsk, Q: row.Question, W: row.Why, O: req.Options,
		MU: req.Multi, FT: req.AllowFree, ST: AskPending,
	})

	expiresIn := int(askWaitMax / time.Second)
	evErr := e.emitEvent(Event{
		Type: "ask", AskID: row.ID, Step: req.Step,
		Ask: &AskView{
			Question: row.Question, Why: row.Why, Options: req.Options,
			Multi: req.Multi, AllowFreeText: req.AllowFree, ExpiresIn: expiresIn,
		},
	})
	if evErr != nil {
		expireAskRow(persistCtx, row.ID)
		e.resolveAsk(idx, AskAborted, "")
		return AskAnswer{AskID: row.ID, Status: AskAborted, Waited: time.Since(started)}
	}

	ans := AskAnswer{AskID: row.ID}
	waitCh := askWaitChan(row.ID)
	defer dropAskWaiter(row.ID)

	deadline := time.NewTimer(askWaitMax)
	defer deadline.Stop()
	poll := time.NewTicker(askPollInterval)
	defer poll.Stop()
	ping := time.NewTicker(askPingInterval)
	defer ping.Stop()

	for {
		select {
		case <-waitCh:
			if a := e.collectAnswer(persistCtx, row.ID, &ans); a {
				e.finishAsk(persistCtx, idx, row.ID, &ans, started)
				return ans
			}
		case <-poll.C:
			if a := e.collectAnswer(persistCtx, row.ID, &ans); a {
				e.finishAsk(persistCtx, idx, row.ID, &ans, started)
				return ans
			}
		case <-ping.C:
			// 心跳用注释帧：发不出去说明对端已断，别再等下去把整轮拖死。
			if err := e.emitEvent(Event{Type: "ping", AskID: row.ID, MS: time.Since(started).Milliseconds()}); err != nil {
				expireAskRow(persistCtx, row.ID)
				ans.Status = AskAborted
				e.finishAsk(persistCtx, idx, row.ID, &ans, started)
				return ans
			}
		case <-ctx.Done():
			expireAskRow(persistCtx, row.ID)
			ans.Status = AskAborted
			e.finishAsk(persistCtx, idx, row.ID, &ans, started)
			return ans
		case <-deadline.C:
			expireAskRow(persistCtx, row.ID)
			ans.Status = AskTimeout
			e.finishAsk(persistCtx, idx, row.ID, &ans, started)
			return ans
		}
	}
}

// collectAnswer 读一次 DB；已答则填充 ans 并返回 true。
func (e *toolEnv) collectAnswer(ctx context.Context, id uint64, ans *AskAnswer) bool {
	row, err := loadAskRow(ctx, id)
	if err != nil || row == nil {
		return false
	}
	if row.Status != AskAnswered {
		return false
	}
	ans.Status = AskAnswered
	var body struct {
		Selected []string `json:"selected"`
		FreeText string   `json:"free_text"`
		Skipped  bool     `json:"skipped"`
	}
	if len(row.Answer) > 0 {
		_ = json.Unmarshal(row.Answer, &body)
	}
	ans.Selected = body.Selected
	ans.FreeText = body.FreeText
	ans.Skipped = body.Skipped
	// value → label 只在这里做一次：前端展示与落库轨迹读的是同一个摘要。
	labels := map[string]string{}
	var opts []AskOption
	if len(row.Options) > 0 {
		_ = json.Unmarshal(row.Options, &opts)
	}
	for _, o := range opts {
		labels[o.Value] = o.Label
	}
	ans.SelectedLabels = make([]string, 0, len(ans.Selected))
	for _, v := range ans.Selected {
		if l := labels[v]; l != "" {
			ans.SelectedLabels = append(ans.SelectedLabels, l)
		} else {
			ans.SelectedLabels = append(ans.SelectedLabels, v)
		}
	}
	return true
}

// finishAsk 统一结算：写 trace 段 + 累加等待时长。
func (e *toolEnv) finishAsk(ctx context.Context, idx int, id uint64, ans *AskAnswer, started time.Time) {
	ans.Waited = time.Since(started)
	if ans.AskID == 0 {
		ans.AskID = id
	}
	e.WaitTotal += ans.Waited
	e.resolveAsk(idx, ans.Status, answerBrief(ans))
}

// answerBrief 是落进 trace 的"答了什么"摘要（刷新后运营要能回看）。
func answerBrief(ans *AskAnswer) string {
	if ans.Skipped {
		return "已跳过"
	}
	parts := make([]string, 0, 2)
	if len(ans.SelectedLabels) > 0 {
		parts = append(parts, strings.Join(ans.SelectedLabels, "、"))
	} else if len(ans.Selected) > 0 {
		parts = append(parts, strings.Join(ans.Selected, "、"))
	}
	if strings.TrimSpace(ans.FreeText) != "" {
		parts = append(parts, clipRunes(ans.FreeText, 160))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "；")
}

/* ------------------------------ 作答 ------------------------------ */

var (
	// ErrAskNotFound 表示提问不存在（或已被清理）。
	ErrAskNotFound = errors.New("agent: 提问不存在")
	// ErrAskAnswered 表示已经答过（CAS 失败且状态是 answered）。
	ErrAskAnswered = errors.New("agent: 该提问已回答")
	// ErrAskExpired 表示这一轮已经结束，卡片不能再点。
	ErrAskExpired = errors.New("agent: 该提问已随本轮结束")
	// ErrAskInvalid 表示提交的值不在选项里（客户端在乱传）。
	ErrAskInvalid = errors.New("agent: 回答内容不合法")
)

// AnswerAsk 处理一次作答：校验 → CAS → 唤醒。
//
// **不取会话锁**（与发消息端点相反）。原因：
// TakeLock 的语义是"同一会话只跑一轮"，而作答是**正在跑的那一轮的输入**，
// 取锁必然 409 —— 功能会等于不存在。替代防线有三道：
//  1. 行状态必须是 pending（已答/已过期一律拒绝）；
//  2. CAS（WHERE status='pending'）兜住双击与两个管理员同时提交；
//  3. 所属 run 必须还是 running（run 结束 = 没人等了，答案无处可去）。
func AnswerAsk(ctx context.Context, id, adminID uint64, in AskAnswer) (*model.AgentAsk, error) {
	if id == 0 {
		return nil, ErrAskNotFound
	}
	row, err := loadAskRow(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrAskNotFound
		}
		return nil, err
	}
	switch row.Status {
	case AskAnswered:
		return nil, ErrAskAnswered
	case AskPending:
	default:
		return nil, ErrAskExpired
	}
	if time.Now().After(row.ExpiresAt) {
		expireAskRow(ctx, id)
		return nil, ErrAskExpired
	}
	// run 活着才有人接这个答案。run 表不在了（历史清理）也算结束。
	var run model.AgentRun
	if err := db.DB.WithContext(ctx).First(&run, row.RunID).Error; err != nil || run.Status != "running" {
		expireAskRow(ctx, id)
		return nil, ErrAskExpired
	}

	// 值域校验：selected 必须是 options 里的 value；不许编。
	var opts []AskOption
	if len(row.Options) > 0 {
		_ = json.Unmarshal(row.Options, &opts)
	}
	if err := normalizeAnswer(&in, opts, row.Multi, row.AllowFree); err != nil {
		return nil, err
	}

	ok, err := answerAskRow(ctx, id, adminID, in)
	if err != nil {
		return nil, err
	}
	if !ok {
		// CAS 失败：不是被我抢到的。重读一次决定报"已答"还是"已结束"。
		if again, e := loadAskRow(ctx, id); e == nil && again.Status == AskAnswered {
			return nil, ErrAskAnswered
		}
		return nil, ErrAskExpired
	}
	// 先落库再唤醒：反过来会出现"模型已经拿到答案、库里还是 pending"的窗口，
	// 那一瞬刷新页面就看不到已经给出的答案。
	signalAsk(id)
	return loadAskRow(ctx, id)
}

// normalizeAnswer 校验并清洗作答。
func normalizeAnswer(in *AskAnswer, opts []AskOption, multi, allowFree bool) error {
	in.FreeText = strings.TrimSpace(in.FreeText)
	if len([]rune(in.FreeText)) > 2000 {
		return ErrAskInvalid
	}
	if in.Skipped {
		// 跳过就是跳过：带着选项一起提交是客户端 bug，不要静默丢掉用户的意图。
		if len(in.Selected) > 0 {
			return ErrAskInvalid
		}
		return nil
	}
	if len(in.Selected) == 0 && in.FreeText == "" {
		return ErrAskInvalid
	}
	if len(in.Selected) > 0 && len(opts) > 0 {
		allowed := make(map[string]bool, len(opts))
		for _, o := range opts {
			allowed[o.Value] = true
		}
		seen := make(map[string]bool, len(in.Selected))
		for _, v := range in.Selected {
			if !allowed[v] || seen[v] {
				return ErrAskInvalid
			}
			seen[v] = true
		}
		if !multi && len(in.Selected) > 1 {
			return ErrAskInvalid
		}
	}
	if in.FreeText != "" && !allowFree && len(in.Selected) == 0 {
		return ErrAskInvalid
	}
	return nil
}

// PendingAsk 读一个会话当前待答的问题（没有则返回 nil）。
//
// 发消息端点用它做 409 分流：正在等回答时发新消息不是"忙"，
// 而是"你得先回答上面那个问题"，两者的用户动作完全不同。
func PendingAsk(ctx context.Context, sessionID uint64) (*model.AgentAsk, error) {
	var rows []model.AgentAsk
	err := db.DB.WithContext(ctx).
		Where("session_id = ? AND status = ? AND expires_at > ?", sessionID, AskPending, time.Now()).
		Order("id DESC").Limit(1).Find(&rows).Error
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

// AskViewOf 把库里的行转成下发给前端的卡片形态。
//
// ExpiresIn 用**剩余秒数**而不是绝对时间：客户端时钟不可信，两边各自算
// "还剩多少秒"不会差出时区量级的偏差；负数夹到 0，前端据此直接显示已结束。
func AskViewOf(row *model.AgentAsk) AskView {
	if row == nil {
		return AskView{}
	}
	var opts []AskOption
	if len(row.Options) > 0 {
		_ = json.Unmarshal(row.Options, &opts)
	}
	left := int(time.Until(row.ExpiresAt).Seconds())
	if left < 0 {
		left = 0
	}
	return AskView{
		Question: row.Question, Why: row.Why, Options: opts,
		Multi: row.Multi, AllowFreeText: row.AllowFree, ExpiresIn: left,
	}
}

/* ------------------------------ 工具实现 ------------------------------ */

// askParams 是 ask_user 的参数 schema。
func askParams() json.RawMessage {
	m := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"question": map[string]any{"type": "string", "maxLength": 300,
				"description": "一句话问清楚（一次只问一件事）"},
			"why": map[string]any{"type": "string", "maxLength": 300,
				"description": "为什么必须问：缺了它会怎样。运营据此判断该不该答"},
			"options": map[string]any{
				"type": "array", "minItems": 2, "maxItems": 5,
				"items": map[string]any{
					"type": "object", "additionalProperties": false,
					"properties": map[string]any{
						"value":  map[string]any{"type": "string", "maxLength": 32, "description": "回传值（英文/短标识）"},
						"label":  map[string]any{"type": "string", "maxLength": 40, "description": "按钮文案"},
						"detail": map[string]any{"type": "string", "maxLength": 120, "description": "代价说明，如「按次计费，贵 3 倍」"},
					},
					"required": []string{"value", "label"},
				},
			},
			"multi":           map[string]any{"type": "boolean", "description": "是否允许多选"},
			"allow_free_text": map[string]any{"type": "boolean", "description": "是否允许补充文字（默认允许）"},
		},
		"required": []string{"question", "why", "options"},
	}
	b, _ := json.Marshal(m)
	return b
}

func toolAskUser(env *toolEnv, args json.RawMessage) (any, error) {
	var in struct {
		Question  string      `json:"question"`
		Why       string      `json:"why"`
		Options   []AskOption `json:"options"`
		Multi     bool        `json:"multi"`
		AllowFree *bool       `json:"allow_free_text"`
	}
	if err := strictUnmarshal(args, &in); err != nil {
		return toolFieldError("question", err.Error()), nil
	}
	in.Question = strings.TrimSpace(in.Question)
	in.Why = strings.TrimSpace(in.Why)
	if in.Question == "" || in.Why == "" {
		return toolFieldError("question", "question 与 why 都不能为空"), nil
	}
	if len(in.Options) < 2 || len(in.Options) > 5 {
		return toolFieldError("options", fmt.Sprintf("选项必须 2–5 个（当前 %d 个）", len(in.Options))), nil
	}
	seen := map[string]bool{}
	for i := range in.Options {
		in.Options[i].Value = strings.TrimSpace(in.Options[i].Value)
		in.Options[i].Label = strings.TrimSpace(in.Options[i].Label)
		if in.Options[i].Value == "" || in.Options[i].Label == "" {
			return toolFieldError(fmt.Sprintf("options[%d]", i), "value 与 label 都不能为空"), nil
		}
		if seen[in.Options[i].Value] {
			return toolFieldError(fmt.Sprintf("options[%d].value", i), "value 不能重复"), nil
		}
		seen[in.Options[i].Value] = true
	}
	allowFree := true
	if in.AllowFree != nil {
		allowFree = *in.AllowFree
	}

	// 闸门顺序：能不能问 → 还有没有额度 → 还来不来得及。
	if env == nil || !env.AskReady {
		return askResult(AskAnswer{Status: AskUnavailable}), nil
	}
	if env.AskCount >= askMaxPerRun {
		return askResult(AskAnswer{Status: AskDenied}), nil
	}
	if env.Step >= env.MaxSteps {
		// 问完也没有下一步来用这个答案了。
		return askResult(AskAnswer{Status: AskNoTime}), nil
	}
	if deadline, ok := env.Ctx.Deadline(); ok && time.Until(deadline) < askReserveAfter {
		return askResult(AskAnswer{Status: AskNoTime}), nil
	}

	env.AskCount++
	ans := env.askAndWait(env.Ctx, AskRequest{
		SessionID: env.SessionID, MessageID: env.MessageID, RunID: env.RunID,
		Step: env.Step, Question: in.Question, Why: in.Why,
		Options: in.Options, Multi: in.Multi, AllowFree: allowFree,
	})
	return askResult(ans), nil
}

// askResult 把等待结果转成**给模型看**的结果。
//
// 一律 ok:true 是硬要求：ok:false 会让 FailStreak 累加（见 runtime 的工具循环），
// 连续三次就触发 agent_loop_guard 整轮中止 —— 把"人不在电脑前"变成"这一轮报错"。
func askResult(ans AskAnswer) map[string]any {
	out := map[string]any{"ok": true, "status": ans.Status}
	if ans.AskID != 0 {
		out["ask_id"] = ans.AskID
	}
	switch ans.Status {
	case AskAnswered:
		out["selected"] = ans.Selected
		out["free_text"] = ans.FreeText
		if ans.Skipped {
			out["skipped"] = true
			out["note"] = "运营选择跳过。请自行按最保守的默认继续，并把该假设写进「未验证项」"
		} else {
			out["note"] = "运营已回答，按此继续；不要重复询问同一件事"
		}
	case AskTimeout:
		out["note"] = "运营 60 秒内未回答。请按最保守的默认继续，并把该假设写进「未验证项」"
	case AskDenied:
		out["note"] = "本轮提问次数已用完（最多 2 次）。请自行决策，并把不确定的地方写进「未验证项」"
	case AskNoTime:
		out["note"] = "本轮剩余时间不足以等待回答。请自行决策，并把假设写进「未验证项」"
	case AskAborted:
		out["note"] = "这一轮在等待回答时被中断（连接断开或超时）。已生成的过程会保留"
	default:
		out["note"] = "当前无法提问，请自行决策并标注未验证项"
	}
	return out
}
