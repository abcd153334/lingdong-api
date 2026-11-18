package model

import "time"

// Usage 对应 usages 表（v2.0 §4.2.7）。
// status：pending 冻结期 / settled / released / rejected（预检期拒绝，0 费、不计入消耗）。
type Usage struct {
	ID        uint64    `gorm:"primaryKey"`
	CreatedAt time.Time `gorm:"type:timestamptz;index"`
	UserID    uint64    `gorm:"index"`
	KeyID     uint64    `gorm:"index"`
	ChannelID uint64    `gorm:"index"`
	// ChannelKeyID 是本次实际使用的**上游渠道密钥**的稳定标识（HMAC 指纹，非明文）。
	//
	// 只用于溯源与对账（"这一单走的是渠道里的哪把 key"），**绝不参与金额计算**。
	// 空串 = 未知（历史数据 / 未取到 key）。
	// 注意不能叫 key_id：上面那个 KeyID 是**用户 API KEY**，覆盖它会毁掉账单归属。
	ChannelKeyID       string `gorm:"size:32;not null;default:''"`
	AdapterID          string `gorm:"size:64"`
	Model              string `gorm:"size:64"`
	Modality           string `gorm:"size:16"`
	HTTPStatus         int
	Kind               string `gorm:"size:16"`
	Status             string `gorm:"size:16"`
	PromptTokens       int64
	CompletionTokens   int64
	CacheTokens        int64
	CacheWrite5mTokens int64
	CacheWrite1hTokens int64
	ImageTokens        int64
	AudioInputTokens   int64
	AudioOutputTokens  int64
	// 多模态快照（S6）：图片档位/质量/张数；音频时长/字符数；重排文档数（仅落库溯源）。
	ImageSpec string `gorm:"size:32"`
	// UpstreamSpec 是**上游实际交付**的图像尺寸原文（如 "2048x2048" / "2K"）。
	//
	// 计费永远按请求侧档位（ImageSpec），这一列只用于事后核对：
	// 没有它就无法回答「按 4K 收的那一单，上游到底给了几 K」——
	// 这正是「按 4K 收费、按 2K 交付」能在账单上长期隐身的原因。
	// 上游没报（多数非 Ark 渠道）时留空，不猜。
	//
	// 列宽必须等于 `billing.upstreamSpecMaxLen`（32）：它们曾经不一致（列 16 / 常量 32），
	// 上游回报 17–32 字符档位时 Settle 的 UPDATE 会因 varchar(16) 报错 → 整个结算事务
	// 回滚 → usage 停在 pending → 20 分钟后被回收。写入侧**不允许截断**：
	// 截出来的 `2048x11` 比丢弃更难查（见 M61）。
	UpstreamSpec string `gorm:"size:32;not null;default:''"`
	// IdemKey / IdemScope 是**同步链路**的幂等键（客户端 Idempotency-Key 头 + 端点作用域）。
	//
	// 为什么落在 usages 而不是另建一张表：同步调用没有 tasks 行，而"这次请求已经扣过钱了"
	// 的唯一凭据就是它；写在同一个事务里，唯一索引才能与余额变更同生共死。
	// 空串表示未启用（绝大多数请求），部分唯一索引只对非空生效。
	IdemKey   string `gorm:"size:128;not null;default:''"`
	IdemScope string `gorm:"size:64;not null;default:''"`
	// ImageRatio 是请求侧声明的画幅比例（"16:9"），像素写法由宽高约分+吸附得到。
	//
	// 比例不参与计费（单价只随档位走），但它决定了适配器下发的具体像素：
	// 没有这一列就无法回答「同一个 2K 档，16:9 与 1:1 的实际成本差多少」。
	ImageRatio string `gorm:"size:16;not null;default:''"`
	// ImageSpecSource 说明 ImageSpec 这个档位是怎么来的：
	// user（用户显式写档位/像素）/ default（没写或 auto，按默认档）/ other（认不出的写法）。
	//
	// 它只服务一件事：tier_mismatch 只在 user 时比对。没传尺寸的请求由上游自己挑档，
	// 拿它去比对「上游实际档 vs 平台默认档」会让每一条 auto 请求都变成假告警。
	ImageSpecSource   string `gorm:"size:16;not null;default:''"`
	ImageQuality      string `gorm:"size:16"`
	ImageN            int64
	AudioDurationSecs int64
	AudioChars        int64
	RerankDocs        int64
	Cost              int64 `gorm:"column:cost;not null;default:0"`
	Charge            int64 `gorm:"column:charge;not null;default:0"`
	// Currency 是本次调用的结算币种（CNY/USD），由冻结时的 BillingBasis 写入。
	//
	// **空串是合法值**：本次改造之前的所有行都没有它，它们产生于「1 美元 = 50 万额度」
	// 的美元时代，读取侧必须回落 USD（见 conf.SnapshotCurrency）。
	// 所以这里不能用 default:'CNY' —— 那会把历史美元记录全标成人民币，
	// 金额瞬间放大 7.2 倍，且是不可逆的静默改数。
	Currency        string `gorm:"size:3;not null;default:''"`
	PricingSnapshot JSON
	LatencyMs       int
	ErrorMsg        string `gorm:"size:500"`
	// ErrorCode 是这次失败的业务码（errs 的 Code，不是 HTTP 状态码）。
	//
	// 为什么要落列而不是从 error_msg 反推：事件类型（余额不足/鉴权失败/限流…）
	// 此前按 http_status 分类，而「额度不足」从 402 对齐成 403 之后，就再也
	// 分不出它和「权限不足」—— 后台筛选「余额不足」会恒为空，且没有任何报错。
	// 历史行是空串，分类退回按状态码判（旧语义正确）。
	ErrorCode         string `gorm:"column:error_code;size:64"`
	RequestDigest     string `gorm:"size:16"`
	IsStream          bool
	Group             string `gorm:"size:32"`
	IP                string `gorm:"size:45;index"`
	RequestID         string `gorm:"size:64;index"`
	UpstreamRequestID string `gorm:"size:128;index"`
	// 计费口径溯源（S8 整改）：来源 / 上游 usage 原文 / 预扣估算 / 异常标记。
	// 全部带默认值，避免老库 AutoMigrate 加 NOT NULL 列失败；不加 index（UsageArchive 内嵌本结构会复制索引）。
	UsageSource string `gorm:"size:16;not null;default:''"`
	// RawUsage 是上游 usage 原文。2000 字符不足以容纳视频/多模态的完整 usage 对象
	// （一个 metaso 任务的 usage 就带秒数/张数/音频四组字段），截断后无法复核，故改为 text。
	RawUsage            string `gorm:"type:text;not null;default:''"`
	EstPromptTokens     int64  `gorm:"not null;default:0"`
	EstCompletionTokens int64  `gorm:"not null;default:0"`

	// ---- 调用记录详情（C1）：分类 / 视频维度 / 计费过程 / 耗时 ----
	//
	// 全部 not null + default，理由同上（AutoMigrate 对存量表加 NOT NULL 无默认列会失败）。
	// 不加 index，唯一例外 TaskID 走 M18 的 CONCURRENTLY 部分索引（大表不能在启动期建索引）。

	// CallKind 区分记录来源：sync（同步转发）/async（异步任务）/precheck（预检拒绝）。
	// kind 列历史恒为 "relay"，无法回答「这条是同步还是任务」，故单独加列而不复用 kind。
	CallKind string `gorm:"size:16;not null;default:'sync'"`
	// UsageSemantic 是上游 usage 的 token 语义（openai/anthropic/unknown）。
	// 不存则同一行有两种合法解释：base = prompt 还是 prompt − 缓存通道，事后无法复算金额。
	UsageSemantic string `gorm:"size:16;not null;default:'unknown'"`
	// 视频计费维度（三档秒价差 2 倍，缺任何一项都无法解释金额）。
	VideoResolution    string `gorm:"size:16;not null;default:''"`
	VideoSeconds       int64  `gorm:"not null;default:0"`
	VideoSecondsSource string `gorm:"size:16;not null;default:''"` // request/upstream/reclaimed
	RefImages          int64  `gorm:"not null;default:0"`
	RefVideos          int64  `gorm:"not null;default:0"`
	RefAudios          int64  `gorm:"not null;default:0"`
	RefVideoSeconds    int64  `gorm:"not null;default:0"`
	TierPrice          int64  `gorm:"column:tier_price;not null;default:0"` // 命中档位单价（微，折扣前）
	TierSource         string `gorm:"size:16;not null;default:''"`
	ChargeBeforeCap    int64  `gorm:"column:charge_before_cap;not null;default:0"` // 两级降额之前应收
	CapReason          string `gorm:"size:32;not null;default:''"`
	Frozen             int64  `gorm:"column:frozen;not null;default:0"`
	// 耗时三时间点（C1 核心）：started_at 是**请求入口**时刻，finished_at 是终态时刻。
	// 可空是刻意的：存量行没有真实入口时间，硬填 now() 会让历史耗时全部失真；
	// 读取侧统一 COALESCE(started_at, created_at)，created_at 就是 Freeze 时刻，语义等价。
	StartedAt  *time.Time `gorm:"type:timestamptz"`
	FinishedAt *time.Time `gorm:"type:timestamptz"`
	// LatencyMs 语义变更（C1）：**总耗时** = finished_at − started_at（含换渠道重试与排队）。
	// 旧值 = 最后一次上游调用耗时，历史行仍按旧语义解读，用 UpstreamMs 区分。
	UpstreamMs   int `gorm:"not null;default:0"`
	FirstTokenMs int `gorm:"not null;default:0"`
	Attempts     int `gorm:"not null;default:0"`
	// TaskID 指向异步任务（0 = 同步调用）。不加外键：tasks 会被保留策略物理删除，
	// 外键会让历史记录无法保留，而这一列的价值恰恰是「任务没了也能说出它对应哪个任务」。
	TaskID uint64 `gorm:"not null;default:0"`
	// Anomaly 是逗号分隔的计费口径异常标记（charge_capped / negative_balance_blocked /
	// usage_upstream_higher / upstream_image_count_mismatch / ...）。
	//
	// 128 → 255（M14）：标记种类已经超过 10 种，128 字符在多标记叠加时会被截断，
	// 而截断掉的往往正是排在后面的关键项（charge_capped 之类）。
	// 写入侧另有「关键标记前置」的排序（prependFlags），扩容与排序两者缺一不可。
	Anomaly string `gorm:"size:255;not null;default:''"`

	// BillPromptTokens / BillCompletionTokens 是**本次计费用的** token 数。
	//
	// 为什么必须与 prompt_tokens/completion_tokens 分开：
	//   - 后者是「上游实测值」，stats.RollupDailyWindow 直接 SUM 它们写进 usage_daily；
	//   - 回收场景拿不到实测（会话卡在 pending），只能按估算入账。
	// 历史上回收把 max_tokens 最坏值写进实测列，一次回收就能让当日 token 统计
	// 放大几十倍且不可逆（rollup 已固化）。分开之后，统计只认实测，账单只认计费值。
	//
	// 结算路径两者相同；回收路径只有 bill_* 有值（实测列留 0 且 anomaly 带
	// completion_unmeasured）。
	BillPromptTokens     int64 `gorm:"column:bill_prompt_tokens;not null;default:0"`
	BillCompletionTokens int64 `gorm:"column:bill_completion_tokens;not null;default:0"`
}

func (Usage) TableName() string { return "usages" }
