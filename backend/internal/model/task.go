package model

import (
	"time"

	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"
)

// Task 对应 tasks 表（v2.0 §4.2.8）。status：queued/running/canceled/succeeded/failed/expired。
type Task struct {
	ID uint64 `gorm:"primaryKey"`
	// PublicID 是**唯一对外任务号**（`task_` + ULID，26 字符）。
	//
	// 为什么内外两套号：主键是 bigint（自增、被 usages.task_id / files / 对账脚本
	// 当外键用），而对外必须是不透明串 —— 自增号会把全站任务量暴露给任何调用方
	// （一个 KEY 的相邻任务号差值就是平台这段时间的接单量）。
	//
	// 生成点只有 BeforeCreate 一处：服务层只有一次 Create，但测试与运维脚本
	// 会直接建行，钩子让它们也拿到号，不需要每处都记得写。
	//
	// 列可空是迁移的过渡形态：AutoMigrate 在**已有行**的库上加非空列会直接失败，
	// 所以先加空列 + 唯一索引（PG 允许多个 NULL），由 M52 回填后再 SET NOT NULL。
	PublicID       string `gorm:"column:public_id;size:32;uniqueIndex:uq_tasks_public_id"`
	UserID         uint64 `gorm:"index;not null"`
	KeyID          uint64 `gorm:"index"`
	UsageID        uint64 `gorm:"index"` // 指向 Freeze 建立的 usages pending 行（S7：Worker 据此重建计费会话）
	ChannelID      uint64 `gorm:"index"`
	Platform       string `gorm:"size:32"`
	Model          string `gorm:"size:64"`
	Modality       string `gorm:"size:16"`
	Status         string `gorm:"size:16;index"`
	UpstreamTaskID string `gorm:"size:128"`
	RequestParams  JSON   `gorm:"type:jsonb"`
	// SubmitBody 是**提交时实际发往上游的请求体原文**（已过 normalizeSubmitBody）。
	//
	// 为什么必须有这一列：request_params 是平台的结构化快照（只有 prompt / 秒数 /
	// 素材摘要这些字段），重试只能靠它「重建」请求体 —— 而重建必然丢字段。
	// 客户端传的 `output_format`、厂商新增参数、音乐任务的 `lyrics`，重建时全都消失，
	// 于是「第一次提交成功、重试后上游 400」或者「重试出的片子与第一次不同」。
	//
	// 有了这一列，重试的语义变成「原样重发，只换 model / 平台决定项」，
	// 与同步链路的透传口径一致。为空 = 该行创建于本列上线之前（兜底走一次重建）。
	SubmitBody      JSON    `gorm:"type:jsonb"`
	PricingSnapshot JSON    `gorm:"type:jsonb"`
	Frozen          int64   `gorm:"column:frozen;not null;default:0"`
	Settled         int64   `gorm:"column:settled;not null;default:0"`
	ResultFileID    *uint64 `gorm:"index"`
	Kind            string  `gorm:"size:16"`
	// ArtifactCount 是本任务成功转存的产物个数（稠密编号 0..n-1，单产物为 1）。
	//
	// 不存上游产物链接：上游多为短效签名 URL，存下来既会过期，也等于把「绕过平台保留策略、
	// 直连上游」的地址交给了调用方。对外只暴露 GET /v1/tasks/{id}/artifacts/{index}/content。
	ArtifactCount int `gorm:"not null;default:0"`
	// ResultText 是**文本类任务**的交付正文（歌词）。空串 = 该任务没有正文。
	//
	// 为什么要单独一列而不是只落产物文件：`/suno/fetch` 一次最多查 100 条，
	// 逐条去对象存储读正文会把一次查询变成 100 次 IO。正文很小（上游限 2000 字符），
	// 存一行比读一百次便宜得多；产物文件仍然照常转存（下载入口不变）。
	ResultText string `gorm:"column:result_text;type:text"`
	// SubTaskKind 是适配器在提交时回写的子协议标记（如 "kling.t2v"），**仅用于轮询选查询路径**。
	// 提交与轮询是两次独立调用，没有共享状态；多路径协议不带回它就会查错端点、产物永远拿不到。
	// 显式指定列名：GORM 默认会把它映射成 sub_task_kind，与手工 SQL/对账脚本约定不一致。
	SubTaskKind string `gorm:"column:subtask_kind;size:32"`
	// ArtifactManifest 是产物转存清单（B3-1 / M14），形状：
	//
	//	[{"i":0,"u":"https://upstream/...","f":123},{"i":1,"t":true,"f":124}]
	//
	// i=产物序号（稠密，与 files.artifact_index 一致）、u=上游源链接（**仅内部**，
	// 绝不下发）、t=文本产物标记（歌词没有链接）、f=本地 files.id（0 = 转存失败待重试）。
	//
	// 为什么必须持久化：Succeed 会因为结算失败被重复调用。只有记住「已经转存到哪、
	// 哪几个失败了」，重试才能只补缺口而不是重新下载全部产物。
	// 单靠 artifact_count 无法区分「部分失败」与「全部完成」——两者都可能是 count>0。
	ArtifactManifest JSON `gorm:"type:jsonb"`
	// UpstreamUsage 是上游自报的 usage 对象**原文**（B3-3 / M14 / B7-3），结算前写入。
	//
	// 一列一义：这里只放上游逐字节给的那段 JSON，**不放平台归一后的结构**
	// （归一值另有 usages.video_seconds / usage_source 等列承载）。两者混在同一列时，
	// 事后核对的人分不清「上游报的」与「平台算的」，而这恰好是少收/多收时唯一要回答的问题。
	//
	// 保留它有三个用途：① 结算按实际上游用量而非请求参数（决策 D）；
	// ② 与请求参数不一致时写 anomaly 供对账；③ 上游谎报用量时能事后追查原始值。
	// 为空 = 该渠道的适配器拿不到 usage 原文（例如规格渠道的 usage 位置不固定，
	// 硬猜一个键只会写进错误内容）。
	UpstreamUsage JSON `gorm:"type:jsonb"`
	// LastPolledAt 是最近一次轮询时间（B4-1），**只用于调度公平性**，不参与任何业务判定。
	//
	// 为什么必须是可空列：NULL 表示「从未被轮询过」，排序用 NULLS FIRST 让它优先。
	// 若用非空列 + 零值时间，PostgreSQL 会把 0001-01-01 当普通值排在最前，
	// 但代码里区分不出「新任务」与「上次轮询于零值」，一是写不出 NULLS FIRST 的语义，
	// 二是历史行升级后全部被认为是"很久以前轮询过"，新任务反而排到队尾。
	//
	// 关键：**更新它与状态推进完全分离**（独立 UPDATE，不看 CAS 结果）。
	// 若只在 CAS 成功时更新，被 CAS 拒绝的任务（例如已被并发推进到终态）
	// 会永远停在原来的 last_polled_at 上，每轮都被选中，把整批名额占满 —— 饥饿。
	LastPolledAt *time.Time `gorm:"type:timestamptz"`
	// IdempotencyKey 是客户端提交幂等键（B4-2），**仅在写面客户端提供时落库**。
	//
	// 语义：同一 KEY + 同一端点 + 同一幂等键只建一个任务。唯一性由部分唯一索引
	// uq_tasks_idem_v2 (key_id, idempotency_endpoint, idempotency_key) 保证，
	// 不用「先查再插」——并发重复提交时先查必然都查不到，只有唯一索引能兜住。
	IdempotencyKey string `gorm:"size:64"`
	// IdempotencyEndpoint 是幂等键的**作用域端点**（如 "video/generations"）。
	//
	// 为什么幂等键必须带端点：键只表达「这次请求是上一次的重试」，而不同端点
	// 用同一个键不是重试。旧实现只按 (key_id, idempotency_key) 去重，于是
	// 「同一把钥、同一个 Idempotency-Key，先提音乐再提视频」会直接把音乐任务
	// 返回给视频请求 —— 不报错、模态不符、扣费与产物全错。
	//
	// 唯一性见 uq_tasks_idem_v2 (key_id, idempotency_endpoint, idempotency_key)；
	// 空串表示历史行或未声明端点（旧行为：只按 KEY + 键去重）。
	IdempotencyEndpoint string     `gorm:"column:idempotency_endpoint;size:64"`
	ErrorMsg            string     `gorm:"size:500"`
	DeadlineAt          *time.Time `gorm:"type:timestamptz"`
	// RecheckUntil 是「终态后复查窗口」的终点（W1），**在进入终态的那一刻快照写入**。
	//
	// 为什么要落列而不是用「created_at + 常量窗口」：
	//  1. 常量窗口表达不了「这个任务已经查过几次」→ 每轮都会重复选中同一批，
	//     6h × 每 5min = 72 次/任务。上游集体超时时几千条同时进入，出站量直接翻倍。
	//  2. 改常量会让**历史行瞬间全部变成候选**：一次配置变更引发一次出站洪峰，
	//     而且上线前无法预演。写死窗口终点与 deadline_at 是同一套哲学。
	//  3. 表达不了「上游 404 提前放弃」——窗口一旦落列，关窗就是置 NULL，语义单一。
	//
	// 只有拿到过上游单号的任务才开窗：没单号就没有可复查的对象。
	RecheckUntil *time.Time `gorm:"column:recheck_until;type:timestamptz"`
	// RecheckAt 是「下一次复查时刻」，兼作**租约**：领取时把它推到 now()+lease，
	// 崩溃/leader 切换后靠它自动重试，不需要额外的 locked_by 列。
	RecheckAt *time.Time `gorm:"column:recheck_at;type:timestamptz"`
	// RecheckCount 是已经复查过的次数：既是退避档位（2m→5m→15m→30m→60m），
	// 也是「迟到成功」这个事实的唯一持久痕迹（不新增 status，见 SucceedLate 的注释）。
	// 必须落库：内存计数重启归零，退避就退化成「每轮都查」。
	RecheckCount int            `gorm:"column:recheck_count;not null;default:0"`
	CreatedAt    time.Time      `gorm:"type:timestamptz"`
	UpdatedAt    time.Time      `gorm:"type:timestamptz"`
	DeletedAt    gorm.DeletedAt `gorm:"index;type:timestamptz"`
}

func (Task) TableName() string { return "tasks" }

// PublicIDPrefix 是对外任务号的固定前缀。
const PublicIDPrefix = "task_"

// BeforeCreate 给新任务分配对外号（已显式赋值时不动，迁移/测试可注入）。
func (t *Task) BeforeCreate(tx *gorm.DB) error {
	if t.PublicID == "" {
		t.PublicID = PublicIDPrefix + ulid.Make().String()
	}
	return nil
}
