package model

import "time"

// ReleaseUpdateJob 是一条「一键更新」指令及其执行进度。
//
// 为什么落在 PostgreSQL 而不是 Redis：Redis 在本系统里被定义为**可降级依赖且不备份**
// （Redis 是唯一跨副本状态源），而"管理员已经批准了一次更新"这件事不能因为
// 缓存重启就消失；同时更新的状态必须在 backend 自己被替换重启后仍然可读。
//
// 状态由宿主机上的 ld-updater 写入，backend 只负责创建与读取 —— Web 进程始终拿不到
// Docker 控制权，这是本设计的安全边界。
type ReleaseUpdateJob struct {
	ID uint64 `gorm:"primaryKey"`

	// RequestID 是前端生成的一次性 ID：网络重试/连点都带同一个值，用于幂等。
	// 唯一索引在 db/indexes.go（本仓约定：索引不写在 tag 上）。
	RequestID string `gorm:"size:64;not null"`

	// TargetTag 是目标发布版本（git tag）。只允许版本形状的字符集，
	// 因为它会被拼进 git / docker / 脚本参数。
	TargetTag string `gorm:"size:64;not null"`
	// TargetCommit 是 manifest 声明的提交（可选）。给了就必须与 tag 解析结果一致，
	// 否则"tag 被移动过"，按拒绝处理。
	TargetCommit string `gorm:"size:64"`
	// FromVersion 是提交时**后端自己**的版本，不是前端声称的版本。
	FromVersion string `gorm:"size:64"`

	Status   string `gorm:"size:24;not null"`
	Step     string `gorm:"size:32"`
	Progress int16
	// Message 是给人看的一句话（≤200 字符，写入前去换行）。
	Message string `gorm:"size:512"`
	// FailureCode 是机器可判的原因码（见 releaseupdate 的常量）。
	FailureCode string `gorm:"size:32"`

	// RollbackRef 在**切换前**就写好：镜像 ID 优先（tag 可以被覆盖，ID 不会）。
	RollbackRef    string `gorm:"size:128"`
	RollbackKind   string `gorm:"size:16"` // image | tag
	RollbackResult string `gorm:"size:16"` // ok | failed | skipped
	BackupPath     string `gorm:"size:512"`

	// DataEpochBefore/After 由 agent **直接查库**取，不读 deploy/.release：
	// 那个文件在切换前就被写成新版本，用它比前后会永远得出"没变"。
	DataEpochBefore string `gorm:"size:96"`
	DataEpochAfter  string `gorm:"size:96"`

	AgentHost string `gorm:"size:128"`

	RequestedBy uint64    `gorm:"not null"`
	RequestedAt time.Time `gorm:"type:timestamptz;not null"`
	StartedAt   *time.Time `gorm:"type:timestamptz"`
	// LastHeartbeatAt 由 agent 的**后台子进程**每 10s 刷新。
	// 必须是独立进程：构建要 3–8 分钟且是阻塞调用，主循环刷新不了。
	LastHeartbeatAt *time.Time `gorm:"type:timestamptz"`
	FinishedAt      *time.Time `gorm:"type:timestamptz"`

	Attempt int
	// ResultAuditedAt 让"终态补写审计"在反复读的情况下只写一次。
	ResultAuditedAt *time.Time `gorm:"type:timestamptz"`
}

func (ReleaseUpdateJob) TableName() string { return "release_update_jobs" }

// ReleaseAgentState 是宿主机更新程序的单行心跳（id 恒为 1）。
//
// 为什么值得一张表：没有它就无法回答"这台机器到底装没装更新程序"。
// 这是交付时最常见的故障（忘了装/没启服务），症状是"点了没反应"——
// 比"点了报错"难排查得多。放 Redis 不行：Redis 挂了就答不出来，而
// "要不要允许提交更新"不该依赖一个可降级组件。
type ReleaseAgentState struct {
	ID         int16  `gorm:"primaryKey"`
	Host       string `gorm:"size:128"`
	Version    string `gorm:"size:64"`
	RepoPath   string `gorm:"size:512"`
	CurrentTag string `gorm:"size:64"`
	SeenAt     time.Time `gorm:"type:timestamptz;not null"`
}

func (ReleaseAgentState) TableName() string { return "release_agent_state" }
