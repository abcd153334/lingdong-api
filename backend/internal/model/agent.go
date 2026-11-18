package model

import (
	"time"

	"gorm.io/gorm"
)

// 本文件是 AI 编排（/admin/agent）的 7 张表。
//
// 三条设计铁律（改之前先读）：
//  1. **AI 不直接写生产表**。唯一写入口是 agent_proposals → 人工确认 → apply 事务。
//     提案表本身也只是"凭证"，真正生效靠 apply 时的那一次事务。
//  2. **AI 调用不进 usages**。usages 是用户调用记录（参与对账、账单、usage_daily 聚合），
//     管理台 AI 调用没有 api_key、不该扣额度、不该出现在用户「调用记录」页 → 单独记 agent_runs。
//  3. **不存供应商密钥**。AI 用的是平台默认对话模型的渠道密钥（已 Fernet 加密在 channels.keys），
//     再存一份 base_url + api_key 就是第二个真相源：默认模型换渠道后 AI 还在用旧 key，且无人知晓。

// AgentDoc 是上游 API 文档的可读副本（供 AI 分片读取）。
//
// 为什么不进 prompt 全文：一份厂商文档动辄几十 KB，整份塞进上下文既贵又会让模型忽略后半段。
// 这里只把**目录**（标题/大小/小标题）给模型，细节由 docs_read 按 offset 逐段取。
type AgentDoc struct {
	ID uint64 `gorm:"primaryKey"`
	// Title 是展示名（默认取文档首行或 URL 的最后一段）。
	Title string `gorm:"size:128;not null"`
	// SourceType 只有 paste | url 两值；未来加 file 只需加枚举，不必改结构。
	SourceType string `gorm:"size:16;not null"`
	SourceURL  string `gorm:"size:512"`
	// Content 是已去 HTML 的纯文本（上限 256KB，保存期拒绝更大的）。
	Content string `gorm:"type:text;not null"`
	// SHA256 用于去重提示（同一份文档反复粘贴时提醒"已存在"）。
	SHA256    string         `gorm:"size:64;not null"`
	SizeBytes int            `gorm:"not null"`
	CreatedBy uint64         `gorm:"not null"`
	CreatedAt time.Time      `gorm:"type:timestamptz"`
	DeletedAt gorm.DeletedAt `gorm:"index;type:timestamptz"`
}

func (AgentDoc) TableName() string { return "agent_docs" }

// AgentSession 是一次编排对话（一个运营的一串需求）。
type AgentSession struct {
	ID uint64 `gorm:"primaryKey"`
	// Title 由首条用户消息截断而来（用户没指定时）。
	Title string `gorm:"size:128;not null"`
	// ModelID 为空 = 用系统默认对话模型（pricing.Snapshot().Defaults["chat"]）。
	ModelID string `gorm:"size:64;not null;default:''"`
	// Status：1 活跃 / 2 归档。
	Status int16 `gorm:"not null;default:1"`
	// LastSeq 是消息游标：分配 seq 时用它做单调递增（不依赖 count，避免并发下重号）。
	LastSeq int64 `gorm:"not null;default:0"`
	// RollingSummary 是「已合并的早期轮次摘要」（L2 降级产物）。
	//
	// 为什么必须落库而不是每次现算：它是**有状态的压缩结果** ——
	// 现算意味着每次装配都要重新决定"哪几轮已被合并"，而决定本身依赖上一次的决定，
	// 于是一旦中间插入一条消息，合并边界就会漂移，同一段对话会被合并两遍。
	RollingSummary string `gorm:"column:rolling_summary;type:text"`
	// CompactedUptoSeq 是 RollingSummary 已覆盖到的 seq（含）。
	//
	// 用 seq 而不是"轮数"：轮数的边界会随摘要策略变化而变，
	// 而 seq 是会话内唯一且单调的事实，回放时不会重复也不会漏。
	CompactedUptoSeq int64          `gorm:"column:compacted_upto_seq;not null;default:0"`
	CreatedAt        time.Time      `gorm:"type:timestamptz"`
	UpdatedAt        time.Time      `gorm:"type:timestamptz"`
	DeletedAt        gorm.DeletedAt `gorm:"index;type:timestamptz"`
}

func (AgentSession) TableName() string { return "agent_sessions" }

// AgentMessage 是一条消息（user / assistant / tool）。
//
// seq 在会话内单调递增并建唯一索引：断线重连时前端用 `after_seq` 续传，
// 靠时间戳排序会在同一毫秒内的两条消息上错序。
type AgentMessage struct {
	ID        uint64 `gorm:"primaryKey"`
	SessionID uint64 `gorm:"not null"`
	// Seq 是会话内单调序号（分配走 UPDATE ... RETURNING，原子）。
	//
	// 唯一索引**不在这里声明**：索引的唯一定义处是 db/indexes.go 的
	// uq_agent_messages_seq —— 那里对"历史数据重号"是记日志不阻断启动。
	// 用 gorm tag 声明会让 AutoMigrate 抢先建索引，重号时直接 log.Fatal
	// 把服务打死，且报错只给索引名，不给"哪两条消息重号"。
	Seq  int64  `gorm:"not null"`
	Role string `gorm:"size:16;not null"`
	// Content 是纯文本（assistant 的最终答复、user 的原话）。
	Content string `gorm:"type:text"`
	// Parts 是结构化轨迹：tool 调用/结果、提案引用、上下文摘要标记。
	//
	// 它是**展示投影**（给前端渲染过程用），单段正文被截到 2000 字、工具结果只留 300 字摘要。
	// 想还原"模型当时看到了什么"必须读 Ctx，二者不互相替代。
	Parts JSON `gorm:"type:jsonb"`
	// Ctx 是**回灌投影**：本轮追加给模型的 assistant(tool_calls) 与 tool 消息原文
	// （不含 user 原文与最终正文，那两者已在 Content 里）。
	//
	// 为什么不复用 Parts：Parts 的截断是为了"能看懂发生了什么"（300 字），
	// 回灌要的是"模型能继续用这份结果"（最多 8KB/条）。两者深度不同，
	// 但都由 finalize 从**同一份内存轨迹**派生 —— 只有一条写入路径。
	Ctx JSON `gorm:"column:ctx;type:jsonb"`
	// Summary 是该轮的规则摘要（≤600 字，不调模型，无编造可能）。
	//
	// 用途只有一个：当这一轮被挤出"全保真窗口"时，用它代替原文回灌。
	// 内容取自本轮内存轨迹（需求首段/工具清单/问答/结论/终态），可在界面展示。
	Summary string `gorm:"column:summary;type:text"`
	// Status：ok | aborted | failed。aborted 时 Content 仍保留已生成内容——
	// 「断了就丢」会让运营重新叙述一遍需求，那正是最贵的部分。
	Status    string    `gorm:"size:16;not null;default:'ok'"`
	TokensIn  int       `gorm:"not null;default:0"`
	TokensOut int       `gorm:"not null;default:0"`
	CreatedAt time.Time `gorm:"type:timestamptz"`
}

func (AgentMessage) TableName() string { return "agent_messages" }

// 提案状态（唯一权威枚举，见方案 §2.5.3 的状态机）。
const (
	ProposalDraft    = "draft"
	ProposalApplying = "applying"
	ProposalApplied  = "applied"
	ProposalRejected = "rejected"
	ProposalFailed   = "failed"
	ProposalStale    = "stale"
)

// AgentProposal 是「AI 建议的一次变更」——也是写生产表的唯一凭证。
//
// 为什么必须有这张表（而不是让 AI 直接写）：变更需要人确认、需要 diff、需要回滚、
// 需要事后回答"这是谁在什么时候按什么依据改的"。提案把"意图"与"生效"分开，
// 意图可以随意生成（失败无副作用），生效只有一条路径。
type AgentProposal struct {
	ID        uint64 `gorm:"primaryKey"`
	SessionID uint64 `gorm:"not null"`
	MessageID uint64 `gorm:"not null"`
	// Kind：change | unsupported。unsupported 表示「该上游必须写内置适配器（L3）」，
	// 它不产生任何操作，只把结论沉淀下来，避免同一个需求反复被提。
	Kind   string `gorm:"size:16;not null"`
	Status string `gorm:"size:16;not null"`
	// Plan 是 AI 产出的操作清单（结构见 service/agent/planspec.go），落库前已强校验。
	Plan JSON `gorm:"type:jsonb;not null"`
	// PlanHash = sha256(canonical(plan))。apply 时必须回传同一个 hash：
	// 防的是"UI 显示的是 A 提案、实际提交的是 B"（前端 bug 或中间被改过）。
	PlanHash string `gorm:"size:64;not null"`
	// Diff 是生成时的字段级差异（展示用；apply 时按实际库内状态重新算）。
	Diff JSON `gorm:"type:jsonb"`
	// BaseVersions 记录生成时各实体的 version 与额度刻度（qpc/multiplier）。
	// apply 前逐项比对：不一致 → stale（提示"请重新生成提案"），
	// 否则会用旧价格算出的方案覆盖新价格体系。
	BaseVersions JSON `gorm:"type:jsonb"`
	// Snapshot 在 apply 时写入：每个被触碰实体的完整 before 行 + after_versions。
	// 回滚靠它生成反向提案（不是"魔法撤销"：回滚同样是一次需要确认的变更）。
	Snapshot JSON `gorm:"type:jsonb"`
	// Error 是 apply 失败原因（含字段定位），前端直接展示。
	Error     string     `gorm:"type:text"`
	AppliedBy uint64     `gorm:"not null;default:0"`
	AppliedAt *time.Time `gorm:"type:timestamptz"`
	CreatedAt time.Time  `gorm:"type:timestamptz"`
	UpdatedAt time.Time  `gorm:"type:timestamptz"`
}

func (AgentProposal) TableName() string { return "agent_proposals" }

// AgentRun 是一轮编排的成本记账（**不进 usages**，理由见文件头第 2 条）。
type AgentRun struct {
	ID        uint64 `gorm:"primaryKey"`
	SessionID uint64 `gorm:"not null"`
	// MessageID 是**触发本轮的用户消息**（不是回答）。
	MessageID uint64 `gorm:"not null"`
	// AssistantMessageID 是本轮落库的 assistant 消息（0 = 没有可落库的内容，如纯断线）。
	//
	// 为什么需要它：MessageID 指向用户消息，一条用户消息可以有 0/1 条回答，
	// 账单与上下文占用要归因到"那条回答"，只能靠这一列。
	AssistantMessageID uint64 `gorm:"column:assistant_message_id;not null;default:0"`
	ModelID            string `gorm:"size:64;not null"`
	ChannelID          uint64 `gorm:"not null;default:0"`
	// Status：running | ok | failed | aborted。
	Status string `gorm:"size:16;not null"`
	// Steps 是工具调用轮数（配置上限见 agent_settings.max_steps）。
	Steps     int `gorm:"not null;default:0"`
	TokensIn  int `gorm:"not null;default:0"`
	TokensOut int `gorm:"not null;default:0"`
	// ContextTokens 是**首步**请求的 prompt_tokens（本轮真实上下文占用）。
	//
	// 绝不能用 TokensIn：那是每步整份 prompt 的累加，10 步就会显示成 3–5 倍，
	// 看起来像"上下文爆了"，实际每步只占 1/10。
	ContextTokens int `gorm:"column:context_tokens;not null;default:0"`
	// ContextWindow 是本轮生效的窗口（模型覆盖 > 全局设置 > 内置默认）。
	// 事后复盘"为什么这轮被摘要"只能靠它。
	ContextWindow int `gorm:"column:context_window;not null;default:0"`
	// CompactLevel 是本轮装配/轮内压缩达到的最高级别（0 未压缩 / 1 摘要旧轮 / 2 并入会话摘要）。
	CompactLevel int16 `gorm:"column:compact_level;not null;default:0"`
	// SummarizedTurns 是被降级为摘要回灌的轮数。
	SummarizedTurns int `gorm:"column:summarized_turns;not null;default:0"`
	// TokensEstimated 表示上游有步骤没给 usage，TokensIn/Out 是估算值。
	// 不记这个标记，估算值会被当成账单依据（对账时无法解释差多少）。
	TokensEstimated bool `gorm:"column:tokens_estimated;not null;default:false"`
	// QuotaEst 是估算成本（**微**，不扣费）。取不到价格时为 0。
	QuotaEst  int64     `gorm:"not null;default:0"`
	LatencyMS int       `gorm:"not null;default:0"`
	ErrorCode string    `gorm:"size:64"`
	CreatedAt time.Time `gorm:"type:timestamptz"`
}

func (AgentRun) TableName() string { return "agent_runs" }

// AgentAsk 是"助手向运营提问"的唯一状态真相源。
//
// 为什么必须单独一张表（而不是只写进 agent_messages.parts）：
//  1. parts 只在 finalize 时落库 —— 本轮进行中的提问在库里根本不存在，
//     满足不了"等待期间刷新/断线仍能看到这个问题"；
//  2. parts 是 append-only 的 jsonb，做不了"只允许回答一次"的 CAS。
//
// parts 里的 `k:"ask"` 段只是**渲染快照**，任何判定都读这张表。
type AgentAsk struct {
	ID        uint64 `gorm:"primaryKey"`
	SessionID uint64 `gorm:"not null;index:idx_agent_asks_sess_status,1"`
	MessageID uint64 `gorm:"not null"`
	RunID     uint64 `gorm:"not null"`
	Step      int    `gorm:"not null;default:0"`
	// Question / Why 是给运营看的：问什么、为什么问（缺它就没法判断该不该答）。
	Question string `gorm:"size:512;not null"`
	Why      string `gorm:"size:512"`
	// Options 是 [{value,label,detail}]；Answer 是 {selected,free_text,skipped}。
	Options   JSON `gorm:"type:jsonb"`
	Multi     bool `gorm:"not null;default:false"`
	AllowFree bool `gorm:"not null;default:true"`
	// Status：pending | answered | expired。
	// 只有 pending 能被回答（CAS），expired 表示这一轮已经结束。
	Status     string     `gorm:"size:16;not null;index:idx_agent_asks_sess_status,2"`
	Answer     JSON       `gorm:"type:jsonb"`
	AnsweredBy uint64     `gorm:"not null;default:0"`
	AnsweredAt *time.Time `gorm:"type:timestamptz"`
	ExpiresAt  time.Time  `gorm:"type:timestamptz;not null"`
	CreatedAt  time.Time  `gorm:"type:timestamptz"`
}

func (AgentAsk) TableName() string { return "agent_asks" }

// AgentSetting 是单行设置（ID 恒 1）。
//
// 单行表而不是塞 system_config：这些是 AI 编排自己的运行参数（步数/预算/开关），
// 与平台计费配置无关，混进 system_config 会让「保存后热更新」的语义变得含糊。
type AgentSetting struct {
	ID      uint64 `gorm:"primaryKey"`
	Enabled bool   `gorm:"not null;default:true"`
	// ModelID 为空 = 用默认对话模型。
	ModelID  string `gorm:"size:64;not null;default:''"`
	MaxSteps int    `gorm:"not null;default:12"`
	// MaxTokensPerRun 是一轮的 token 成本闸（累计用量 + 下一步预估 + 输出上限），0 = 不限。
	MaxTokensPerRun int `gorm:"not null;default:2000000"`
	// ContextWindow 是输入上下文预算（模型未单独配置时的全局值），0 = 用内置默认。
	//
	// 它与 MaxTokensPerRun 是两个量纲：
	//   - ContextWindow 决定"这一次请求能装多少历史"（超了要么摘要要么失败）；
	//   - MaxTokensPerRun 决定"这一轮最多烧多少 token"（跨步累计）。
	// 混成一个字段就会出现"为了装下历史而放开成本闸"这种危险联动。
	ContextWindow int `gorm:"column:context_window;not null;default:0"`
	// MaxOutputTokens 是单次请求下发给上游的 max_tokens（防一次输出烧穿预算），0 = 用内置默认。
	MaxOutputTokens int `gorm:"column:max_output_tokens;not null;default:0"`
	// DailyAmountBudget 是每日成本上限（**微**），0 = 不限。
	//
	// 列名与对外 JSON 同名（daily_amount_budget）：历史上列叫 daily_quota_budget、
	// 字段叫 DailyQuotaBudget、JSON 叫 daily_amount_budget，三处三个名字，
	// 直接后果是前端读写都对不上、保存被静默忽略。
	DailyAmountBudget int64 `gorm:"column:daily_amount_budget;not null;default:0"`
	// DryRunRequired 表示新建渠道必须 dry-run 成功才能 apply（守住"没验证就上线"）。
	DryRunRequired bool      `gorm:"not null;default:true"`
	UpdatedAt      time.Time `gorm:"type:timestamptz"`
}

func (AgentSetting) TableName() string { return "agent_settings" }

// AgentDryRun 是一次连通性探测的**持久凭据**。
//
// 为什么必须落库：早前凭据只存在 run 级内存（toolEnv.DryRuns），而它只在
// "同一轮里先探测再 propose_change" 时才会被快照进提案。运营很自然会分成两步
// （这一轮探通、下一轮再提案），于是提案上一条凭据都没有 → apply 恒返 409
// "必须先做一次连通性探测"，而运营明明刚探过、卡片标题里也写着探过。
//
// 键的构成（NormalizeBaseURL + 密钥指纹）决定"探过的到底是不是这一条"：
//   - base_url 归一（去尾斜杠、host 小写、去默认端口）：避免同一地址写成两种形态各探一次；
//   - 密钥指纹：换密钥必须重探 —— 否则"用 A 密钥探通过、拿 B 密钥上线"会直接 401；
//   - session_id：凭据只在会话内有意义（同一条密钥在不同会话得到不同指纹，库泄露也无法关联）。
//
// 不加唯一约束：同目标"先失败后成功"必须留两行。失败记录被成功覆盖，
// 会让"探过但没过"看起来像"从没探过"，两者处置完全不同。
type AgentDryRun struct {
	ID        uint64 `gorm:"primaryKey"`
	SessionID uint64 `gorm:"not null;default:0"`
	RunID     uint64 `gorm:"not null;default:0"`
	MessageID uint64 `gorm:"not null;default:0"`
	AdapterID string `gorm:"size:64;not null;default:''"`
	BaseURL   string `gorm:"size:512;not null;default:''"`
	// KeyFP 是密钥指纹（sha256 前 16 hex），空密钥为 "-"。绝不存明文。
	KeyFP string `gorm:"column:key_fp;size:32;not null;default:'-'"`
	OK    bool   `gorm:"not null;default:false"`
	// Status 是归类后的上游码（401/404/0…），0 表示没有 HTTP 状态（DNS/超时）。
	Status    int       `gorm:"not null;default:0"`
	Message   string    `gorm:"size:512;not null;default:''"`
	LatencyMS int       `gorm:"not null;default:0"`
	Ref       string    `gorm:"size:32;not null;default:''"`
	CreatedAt time.Time `gorm:"type:timestamptz"`
	// ExpiresAt 是有效期（24h）。闸门只认 now < expires_at：
	// 把 TTL 存进列而不是每次现算，是为了让"过期"能直接被 SQL 查出来（对账/清理）。
	ExpiresAt time.Time `gorm:"type:timestamptz"`
}

func (AgentDryRun) TableName() string { return "agent_dry_runs" }
