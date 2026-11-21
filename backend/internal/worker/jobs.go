package worker

// Jobs 返回 Worker 的全部定时任务（S7：异步任务轮询；S8：文件/产物到期清理；S9：订单过期关单；
// S11：usage_daily 聚合、数据保留维护、渠道熔断探测；虎皮椒接入：主动查单与退款结果兜底；
// B7：任务保留策略与容量体检）。
//
// 注意 order_expire 与 order_query 的执行顺序**不可依赖**：两个 Job 各自按自己的节拍跑，
// 它们的互斥靠 SQL 条件（order_expire 对虎皮椒订单只关「超出补查窗口」的），
// 而不是靠注册顺序。同理 task_retention 与 artifact_purge 也在删同一批 files 行，
// 但两者都是「按条件删、失败跳过」，谁先跑都不会删错（都会跳过已被对方删掉的行）。
func Jobs() []Job {
	return []Job{
		// 任务链路三个 Job 各自独立（B7-5）：轮询出站、到期推进、孤儿回收互不挤占。
		NewTaskPollingJob(), NewTaskExpireJob(), NewTaskOrphanJob(),
		// 终态复查（W1）：捞出「已判失败、上游其实成功」的任务。
		// 必须独立于任务链路那三个：它要按 recheck_at 排序（另一条索引），
		// 且要能被 LD_TASK_RECHECK=0 单独停掉（事故现场只停「对已退款用户补扣」）。
		NewTaskRecheckJob(),
		NewTaskReclaimJob(), NewUsageReclaimJob(), NewFilePurgeJob(), NewArtifactPurgeJob(),
		// 产物补转存：SUCCESS 路径转存失败的条目（f=0）在终态后没有任何路径会重试，
		// 真机实测 SUNO 两次任务各丢 1 首。只补交付、不补账（结算金额已定）。
		NewArtifactRepairJob(),
		NewOrderExpireJob(), NewOrderQueryJob(), NewOrderRefundQueryJob(), NewUsageRollupJob(), NewUsageArchiveJob(),
		NewChannelProbeJob(), NewReconcileJob(),
		NewTaskRetentionJob(), NewCapacityJob(), NewCallIOPurgeJob(),
		// 孤儿对账：默认不跑（LD_STORAGE_GC 未设置即返回），设置了也默认 dry-run。
		NewStorageGCJob(),
		// 计价配置体检：坏价目行只在启动日志出现过一次，运行期必须能被看板发现。
		NewSpecPriceGuardJob(),
		NewAgentRunReclaimJob(),
		// 探测凭据回收：过期 7 天后删（过期行是"上周为什么没探通"的唯一证据）。
		NewAgentDryRunCleanupJob(),
		// 认证数据回收：过期会话 / 超保留期的认证事件 / 用过的备份码。
		NewAuthCleanupJob(),
		// 自动检查新版本（6h 一次，只写出站抓清单，不做更新动作）。
		// 没有它，"有没有新版本"就只取决于管理员有没有点过「检查更新」——
		// 而进程内缓存在重启后是空的，侧栏红点永远不会自己出现。
		NewReleaseCheckJob(),
	}
}
