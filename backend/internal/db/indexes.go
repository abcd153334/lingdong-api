package db

import (
	"errors"
	"fmt"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/pgerr"
)

// indexes 是 AutoMigrate 无法表达的索引（部分索引/条件唯一索引），启动时幂等创建。
//
// 约束：
//   - 全部 IF NOT EXISTS，重复启动无副作用；
//   - 普通 CREATE INDEX（非 CONCURRENTLY）：GORM 写操作默认包事务，CONCURRENTLY 在事务内会报错；
//     大表上线请走运维 runbook（CONCURRENTLY + 校验 indisvalid），本函数保留兜底。
//   - 唯一索引因历史重复数据创建失败（23505）时**不阻断启动**，只记 Error 并在对账报告中暴露。
//
// required=true 的索引是**安全或资金正确性的硬依赖**（如邮箱唯一、会话票据唯一）：
// 这类索引建不成时启动必须失败。历史索引保持 required=false 的宽松行为——
// 它们缺失只影响性能，让服务起不来的代价更大。
var indexes = []struct {
	name string
	sql  string
	// required 为 true 时：创建遇重复数据、或自检发现缺失/INVALID 一律返回 error。
	required bool
}{
	{
		name: "idx_usages_pending",
		sql:  `CREATE INDEX IF NOT EXISTS idx_usages_pending ON usages (created_at) WHERE status = 'pending'`,
	},
	{
		name: "idx_usages_user_created",
		sql:  `CREATE INDEX IF NOT EXISTS idx_usages_user_created ON usages (user_id, created_at DESC)`,
	},
	{
		name: "idx_usages_key_created",
		sql:  `CREATE INDEX IF NOT EXISTS idx_usages_key_created ON usages (key_id, created_at DESC)`,
	},
	{
		// 仅约束 usage 维度的账务流水唯一性；admin_adjust / affiliate 等 ref_type 允许重复。
		//
		// required：这是"一单只结算一次"的**最后一道**闸（业务层还有 status 行锁 +
		// 内存 closed）。缺失时并发/重放会重复入账，而并发在测试里几乎撞不出来。
		name:     "uq_transactions_usage_ref",
		required: true,
		sql: `CREATE UNIQUE INDEX IF NOT EXISTS uq_transactions_usage_ref
		      ON transactions (ref_type, ref_id, type) WHERE ref_type = 'usage'`,
	},
	// B3 容量：以下四个索引服务于管理台列表与删除保护（原先全是顺序扫描）。
	{
		// 渠道删除保护 / 绑定抽屉按渠道反查（B3 分页后只查页内渠道，仍需按 channel_id 命中）。
		name: "idx_model_bindings_channel",
		sql:  `CREATE INDEX IF NOT EXISTS idx_model_bindings_channel ON model_bindings (channel_id)`,
	},
	{
		// 适配器页按 adapter_id 统计渠道数、渠道列表关键词前缀匹配；
		// 文档投影按 `spec:<slug>` 反查规格（docdocs.attachSpecRequests）也走它。
		name: "idx_channels_adapter",
		sql:  `CREATE INDEX IF NOT EXISTS idx_channels_adapter ON channels (adapter_id)`,
	},
	{
		// 适配器管理页「我的」分组按创建人过滤。
		name: "idx_adapter_specs_owner",
		sql:  `CREATE INDEX IF NOT EXISTS idx_adapter_specs_owner ON adapter_specs (owner_id)`,
	},
	{
		// 会话内的 seq 唯一：断线续传按 after_seq 取，重号会让消息错序或漏拉。
		name: "uq_agent_messages_seq",
		sql:  `CREATE UNIQUE INDEX IF NOT EXISTS uq_agent_messages_seq ON agent_messages (session_id, seq)`,
	},
	{
		// 探测凭据的判定键：apply 的闸门与提案列表每页都要按这四列（含指纹）匹配。
		//
		// 刻意**不**用 GORM tag：索引的唯一声明处是本文件（tag 建的索引进不了
		// verifyIndexes，缺了也不会在启动时报出来）。这里是同名同列迁移 ——
		// tag 建过的旧库不会留下孤儿索引，新库由 EnsureIndexes 建。
		name: "idx_agent_dry_runs_lookup",
		sql:  `CREATE INDEX IF NOT EXISTS idx_agent_dry_runs_lookup ON agent_dry_runs (session_id, adapter_id, base_url)`,
	},
	{
		// 过期清理任务按 expires_at 扫；闸门判定也需要它筛"未过期"。
		name: "idx_agent_dry_runs_expires",
		sql:  `CREATE INDEX IF NOT EXISTS idx_agent_dry_runs_expires ON agent_dry_runs (expires_at)`,
	},
	{
		// 提案按会话反查（详情页与「本会话已有提案」提示）。
		name: "idx_agent_proposals_session",
		sql:  `CREATE INDEX IF NOT EXISTS idx_agent_proposals_session ON agent_proposals (session_id, id DESC)`,
	},
	{
		// 预算闸：按天汇总 agent_runs.quota_est。
		name: "idx_agent_runs_created",
		sql:  `CREATE INDEX IF NOT EXISTS idx_agent_runs_created ON agent_runs (created_at)`,
	},
	{
		// 文档列表按创建时间倒序。
		name: "idx_agent_docs_created",
		sql:  `CREATE INDEX IF NOT EXISTS idx_agent_docs_created ON agent_docs (created_at DESC) WHERE deleted_at IS NULL`,
	},
	{
		// 调用记录按模型维度筛选（门户/管理台的 model + 时间范围组合）。
		name: "idx_usages_model_created",
		sql:  `CREATE INDEX IF NOT EXISTS idx_usages_model_created ON usages (model, created_at DESC)`,
	},
	// 虎皮椒接入：以下四个索引服务于订单生命周期（原先 orders.status 连普通索引都没有，
	// ExpirePending 是全表扫；加主动查单 worker 后扫描频率还会翻倍）。
	{
		// 过期关单：WHERE status='pending' AND (expire_at < now() ...)。
		name: "idx_orders_status_expire",
		sql:  `CREATE INDEX IF NOT EXISTS idx_orders_status_expire ON orders (status, expire_at) WHERE deleted_at IS NULL`,
	},
	{
		// 门户订单列表：按用户倒序分页。
		name: "idx_orders_user_id",
		sql:  `CREATE INDEX IF NOT EXISTS idx_orders_user_id ON orders (user_id, id DESC) WHERE deleted_at IS NULL`,
	},
	{
		// 下单上限闸要算「在途 pending 占额」（见 order.placeOrder）：没有它，
		// 这次聚合会退化成扫该用户的全部历史订单（orders 是增长最快的大表）。
		name: "idx_orders_user_pending",
		sql:  `CREATE INDEX IF NOT EXISTS idx_orders_user_pending ON orders (user_id, created_at) WHERE status = 'pending' AND deleted_at IS NULL`,
	},
	{
		// 主动查单领取：status + protocol + last_query_at（30s 节流的命中索引）。
		name: "idx_orders_query",
		sql:  `CREATE INDEX IF NOT EXISTS idx_orders_query ON orders (status, pay_protocol, last_query_at) WHERE deleted_at IS NULL`,
	},
	{
		// 退款滞留巡检：只扫 refunding（量极小），按 last_refund_query_at 节流。
		// 没有它时 WHERE status='refunding' 会退化到全表扫（orders 是增长最快的大表之一）。
		name: "idx_orders_refunding",
		sql:  `CREATE INDEX IF NOT EXISTS idx_orders_refunding ON orders (last_refund_query_at) WHERE status = 'refunding'`,
	},
	{
		// 一单最多一笔到账流水。
		// 退款写 type='refund'，不在条件内；admin_adjust 的 ref_type 也不是 'order'，均不受影响。
		//
		// required：充值回调的重放是**常态**（网关重试 + 用户重复支付），
		// 这道索引缺失时唯一兜底只剩订单行锁，多实例下仍可能重复加钱。
		// 建不成会拒绝启动 —— 上线前必须先跑重复数据体检（见 docs 迁移runbook）。
		name:     "uq_transactions_order_recharge",
		required: true,
		sql: `CREATE UNIQUE INDEX IF NOT EXISTS uq_transactions_order_recharge
		      ON transactions (ref_type, ref_id) WHERE ref_type = 'order' AND type = 'recharge'`,
	},
	{
		// 一单最多一次"退款扣回" + 一次"退款失败回滚"。
		//
		// 之前退款族流水完全没有唯一约束：同一订单被重复退款时，第二次
		// CreditTx(-credited) 照样成功，用户余额被扣两遍，而 reconcile 只判
		// "有没有 refund 流水"（见 checkRefunds），发现不了。
		// type 必须进键：refund 与 refund_revert 是同一订单的两条不同流水。
		name: "uq_transactions_order_refund",
		sql: `CREATE UNIQUE INDEX IF NOT EXISTS uq_transactions_order_refund
		      ON transactions (ref_type, ref_id, type)
		      WHERE ref_type = 'order' AND type IN ('refund', 'refund_revert')`,
	},
	{
		// 一单最多两行：inviter 一行、invitee 一行。
		//
		// required：返利额度是 affiliate_rewards 的派生量，多插一行就等于多发一笔钱，
		// 而且没有任何累加器能暴露它（users 上已经没有 aff_balance）。
		// 业务层靠「同单预查 + 被邀请人行锁」拦重复，但重放（回调重试、人工补单）
		// 是常态，这道唯一索引是最后一道闸；建不成必须拒绝启动。
		//
		// role 必须进键：改成一行一受益人后，同一订单本来就有两行。
		name:     "uq_affiliate_rewards_order_role",
		required: true,
		sql: `CREATE UNIQUE INDEX IF NOT EXISTS uq_affiliate_rewards_order_role
		      ON affiliate_rewards (order_id, role)`,
	},
	{
		// 受益人侧：Info 的三项派生额、Transfer 的候选扫描、Records 的列表。
		// available_at 进键是为了让「到期可转出」的范围扫描不回表判序。
		name: "idx_aff_reward_beneficiary",
		sql:  `CREATE INDEX IF NOT EXISTS idx_aff_reward_beneficiary ON affiliate_rewards (beneficiary_id, available_at) WHERE NOT voided`,
	},
	{
		// 付款方侧：次数闸 COUNT(DISTINCT order_id) WHERE invitee_id=? AND NOT voided。
		name: "idx_aff_reward_invitee_order",
		sql:  `CREATE INDEX IF NOT EXISTS idx_aff_reward_invitee_order ON affiliate_rewards (invitee_id, order_id) WHERE NOT voided`,
	},
	// 充值套餐后台配置：以下两个索引服务于门户列表与订单侧的套餐追溯。
	{
		// 门户套餐列表：WHERE enabled AND deleted_at IS NULL ORDER BY sort_order, price, id。
		// 三列全在索引里 → 覆盖 index scan，count/排序都不回表。
		name: "idx_packages_portal",
		sql: `CREATE INDEX IF NOT EXISTS idx_packages_portal
		      ON packages (sort_order, price_cent, id) WHERE deleted_at IS NULL AND enabled = true`,
	},
	{
		// 删除保护计数 / 列表批量补 order_count / 订单补套餐名。
		// 条件**刻意不含 deleted_at**：删除保护与 order_count 都是 Unscoped 的全量口径
		// （含软删订单），带上 deleted_at 会让唯一需要它的查询用不上这个索引。
		name: "idx_orders_package",
		sql:  `CREATE INDEX IF NOT EXISTS idx_orders_package ON orders (package_id) WHERE package_id > 0`,
	},
	{
		// 多产物任务：同一任务内每个 artifact_index 只允许一行。
		//
		// 为什么带 WHERE task_id > 0：files 目前只存任务产物（用户上传在 uploads 表），
		// 这条条件是**防御**——历史上若出现 task_id=0 的脏行（导入、手工修数据），
		// 无条件唯一索引会让启动期建索引 23505 失败。反正 (0, 0) 本身没有业务含义。
		// 唯一索引因重复数据创建失败时本函数只记 Error 不阻断启动，上线前请按
		// 「上线核对」跑一次重复检查 SQL。
		name: "uniq_files_task_artifact",
		sql: `CREATE UNIQUE INDEX IF NOT EXISTS uniq_files_task_artifact
		      ON files (task_id, artifact_index) WHERE task_id > 0`,
	},
	// B4-1 轮询吞吐：以下两个索引服务于 TaskPollingJob 的两条扫描。
	{
		// 轮询领取：WHERE status IN ('queued','running') AND upstream_task_id <> ''
		//            AND deadline_at > now() ORDER BY last_polled_at NULLS FIRST, id
		//
		// NULLS FIRST 必须与 ORDER BY 逐字一致，否则索引给不出所需顺序，规划器退化成
		// 「用 status 过滤 + 全量排序」——在途 3000 条时每 5s 一次排序，正是 P0-10 的病灶。
		// 列顺序也不能改：status 在前是因为它是唯一有选择性的等值条件（2 个取值）。
		name: "idx_tasks_poll",
		sql: `CREATE INDEX IF NOT EXISTS idx_tasks_poll
		      ON tasks (status, last_polled_at ASC NULLS FIRST, id) WHERE deleted_at IS NULL`,
	},
	{
		// 到期扫描：WHERE status IN ('queued','running') AND deadline_at <= now()。
		// 部分索引只含在途行（数量级远小于全表），扫描代价与在途数成正比而不是与历史总量成正比。
		name: "idx_tasks_expire",
		sql: `CREATE INDEX IF NOT EXISTS idx_tasks_expire
		      ON tasks (deadline_at) WHERE status IN ('queued','running') AND deleted_at IS NULL`,
	},
	{
		// 孤儿回收：WHERE status = 'queued' AND COALESCE(upstream_task_id,'') = ''
		//            AND created_at < cutoff ORDER BY id
		//
		// 谓词与 orphanPass **逐字一致**（含 COALESCE）：where 子句里写了函数而索引写
		// upstream_task_id = ''，规划器不会用这条索引 —— 表现是"加了索引还是全表扫"，
		// 而且 EXPLAIN 里看不出是写错了。
		name: "idx_tasks_orphan",
		sql: `CREATE INDEX IF NOT EXISTS idx_tasks_orphan
		      ON tasks (id) WHERE status = 'queued'
		        AND COALESCE(upstream_task_id, '') = '' AND deleted_at IS NULL`,
	},
	{
		// 预扣回收：WHERE status IN ('queued','running') AND frozen > 0 AND created_at < cutoff
		//            ORDER BY id
		//
		// 与 idx_tasks_expire 的区别：那条按 deadline_at 找"该判超时"的，这条按 created_at
		// 找"钱还冻着但任务早就没了动静"的。两者都只装少量行，互不替代。
		name: "idx_tasks_reclaim",
		sql: `CREATE INDEX IF NOT EXISTS idx_tasks_reclaim
		      ON tasks (id) WHERE status IN ('queued','running')
		        AND frozen > 0 AND deleted_at IS NULL`,
	},
	{
		// 终态复查领取（W1）：WHERE status IN ('expired','failed') AND recheck_until IS NOT NULL
		//                    AND recheck_at IS NOT NULL AND recheck_at <= now()
		//                    ORDER BY recheck_at, id
		//
		// status 与 recheck_until 都放进谓词、**不进键列**：它们在这个查询里分别是
		// 2 值等值和纯过滤条件，放进键列会把 `ORDER BY recheck_at` 的前缀打断，
		// 索引给不出所需顺序 → 规划器退化成「过滤 + 全量排序」，与 idx_tasks_poll 同一个病灶。
		//
		// 三个 IS NOT NULL 保证索引只装「窗口内待复查」的行：量级与「近 6h 失败任务数」
		// 成正比，而不是与历史终态总量成正比。关窗（置 NULL）后行会自然从索引上消失。
		name: "idx_tasks_recheck",
		sql: `CREATE INDEX IF NOT EXISTS idx_tasks_recheck
		      ON tasks (recheck_at, id) WHERE status IN ('expired','failed')
		        AND recheck_at IS NOT NULL AND recheck_until IS NOT NULL AND deleted_at IS NULL`,
	},
	{
		// 提交幂等（B4-2）：同一 KEY + 同一幂等键只允许一个任务。
		//
		// WHERE idempotency_key <> ''：未声明幂等键的提交（绝大多数）不受任何约束，
		// 也不会因为空串互相冲突。NULL 与 '' 都要排除，故用 <> '' 而不是 IS NOT NULL。
		//
		// WHERE deleted_at IS NULL 必须与业务查询口径（GORM 默认作用域）一致：
		// 若把软删行也算进唯一性，客户端在任务被删后复用同一个幂等键会插入失败，
		// 而按幂等键回查又查不到那行（默认作用域排除软删）→ 只能报错，
		// 幂等键从此变成"-一次性、永久失效"。
		// 幂等作用域必须含**端点**：同一个键打到不同端点不是重试。
		// 旧索引 (key_id, idempotency_key) 会让「先提音乐再提视频」的第二次请求
		// 直接拿回音乐任务（不报错），客户端拿到模态不符的任务对象去轮询产物。
		// 新索引严格**更强**的约束面：同键同端点仍然只允许一条。
		name: "uq_tasks_idem_v2",
		sql: `CREATE UNIQUE INDEX IF NOT EXISTS uq_tasks_idem_v2
		      ON tasks (key_id, idempotency_endpoint, idempotency_key)
		      WHERE idempotency_key <> '' AND deleted_at IS NULL`,
	},
	// 任务列表分页（B7-5）：门户与开放 API 的列表都是「按归属过滤 + id 倒序」，
	// 没有复合索引时 PostgreSQL 只能走单列 idx_tasks_user_id / idx_tasks_key_id
	// 取出该归属的**全部历史任务**再排序 —— 而 tasks 与 usages 是同一量级的大表，
	// 一个跑了几万次视频任务的 KEY 打开列表就是几万行的 Sort。
	//
	// 为什么要两条而不是一条：归属有两种（门户按 user_id，`/v1/videos` 按 key_id），
	// 复合索引的列顺序决定它只能服务前缀相同的那一类，一条索引覆盖不了两种。
	// id 用 DESC 与 `ORDER BY id DESC` 逐字一致，才能免排序直接取前 N 行。
	{
		name: "idx_tasks_user_list",
		sql: `CREATE INDEX IF NOT EXISTS idx_tasks_user_list
		      ON tasks (user_id, id DESC) WHERE deleted_at IS NULL`,
	},
	{
		name: "idx_tasks_key_list",
		sql: `CREATE INDEX IF NOT EXISTS idx_tasks_key_list
		      ON tasks (key_id, id DESC) WHERE deleted_at IS NULL`,
	},

	// ---- 文档内容 v7（板块 / 节点 / 接口 / 字段 / 变体 / 预设 / 错误码）----
	//
	// 全部用**部分唯一索引**表达唯一性（GORM 的 uniqueIndex tag 在空串上会把
	// 「没填」当成一个真实值互相冲突；见 doc_groups.section 的历史坑）。
	{
		// 板块 key 是节点 `section` 的指向目标，全局唯一。
		name:     "uq_doc_sections_key",
		required: true,
		sql:      `CREATE UNIQUE INDEX IF NOT EXISTS uq_doc_sections_key ON doc_sections (key) WHERE key <> ''`,
	},
	{
		// 节点 key 是 URL 身份，全局唯一。
		name:     "uq_doc_nodes_key",
		required: true,
		sql:      `CREATE UNIQUE INDEX IF NOT EXISTS uq_doc_nodes_key ON doc_nodes (key) WHERE key <> ''`,
	},
	{
		// 一套协议只能有一个节点：否则 OpenAI 兼容会出现两篇，模型清单对半分。
		name:     "uq_doc_nodes_protocol",
		required: true,
		sql:      `CREATE UNIQUE INDEX IF NOT EXISTS uq_doc_nodes_protocol ON doc_nodes (protocol) WHERE protocol <> ''`,
	},
	{
		// 侧栏：WHERE hidden = false AND status = 'published' ORDER BY section, sort, key。
		name: "idx_doc_nodes_nav",
		sql:  `CREATE INDEX IF NOT EXISTS idx_doc_nodes_nav ON doc_nodes (section, sort, key) WHERE hidden = false AND status = 'published'`,
	},
	{
		// 接口 key（URL 段）**全站唯一**：三族的「生成图片」是三篇不同文档，
		// 标识必须能区分它们（族已写进 key，见 apidoc.PageSlugOf）。
		//
		// 旧定义是 (node_id, key) —— 那让三族共用同一个 URL 段。改定义只能换名字：
		// `CREATE UNIQUE INDEX IF NOT EXISTS` 遇到同名索引会跳过、不换定义。
		//
		// required=false 是**顺序要求**，不是「不重要」：EnsureIndexes 跑在 M50 之前，
		// 旧库里三族同 key 还在，这条建不出来；M50 清空后由它自己建好。
		// 若在这里要求必须成功，旧库会直接起不来，M50 永远等不到执行。
		name: "uq_doc_endpoints_key_v2",
		sql:  `CREATE UNIQUE INDEX IF NOT EXISTS uq_doc_endpoints_key_v2 ON doc_endpoints (key) WHERE key <> ''`,
	},
	{
		// 同一节点内，一个已登记端点只能有一篇文档；不同节点各有一篇是**允许**的
		// （参考图里 KLING / MUSE / GROK 各有一页「创建视频生成任务」，字段与示例不同）。
		// 早期方案写成全局唯一，那会让「多厂商各一页」直接建不出来。
		name:     "uq_doc_endpoints_transport",
		required: true,
		sql: `CREATE UNIQUE INDEX IF NOT EXISTS uq_doc_endpoints_transport
		      ON doc_endpoints (node_id, transport_doc_id) WHERE transport_doc_id IS NOT NULL`,
	},
	{
		name: "idx_doc_endpoints_nav",
		sql:  `CREATE INDEX IF NOT EXISTS idx_doc_endpoints_nav ON doc_endpoints (node_id, sort, key) WHERE hidden = false`,
	},
	{
		name: "idx_doc_fields_ep",
		sql:  `CREATE INDEX IF NOT EXISTS idx_doc_fields_ep ON doc_endpoint_fields (endpoint_id, sort)`,
	},
	{
		// 请求字段：同 (endpoint, in, name) 不许重复（体检查重与这里同源）。
		name:     "uq_doc_fields_req",
		required: true,
		sql: `CREATE UNIQUE INDEX IF NOT EXISTS uq_doc_fields_req
		      ON doc_endpoint_fields (endpoint_id, location, name) WHERE variant_id IS NULL`,
	},
	{
		// 返回字段：挂在变体上，只按变体内的 name 唯一。
		name:     "uq_doc_fields_resp",
		required: true,
		sql: `CREATE UNIQUE INDEX IF NOT EXISTS uq_doc_fields_resp
		      ON doc_endpoint_fields (variant_id, name) WHERE variant_id IS NOT NULL`,
	},
	{
		name:     "uq_doc_response_variants",
		required: true,
		sql: `CREATE UNIQUE INDEX IF NOT EXISTS uq_doc_response_variants
		      ON doc_response_variants (endpoint_id, status_code, label)`,
	},
	{
		name:     "uq_doc_presets_key",
		required: true,
		sql:      `CREATE UNIQUE INDEX IF NOT EXISTS uq_doc_presets_key ON doc_presets (endpoint_id, key)`,
	},
	{
		name:     "uq_doc_endpoint_errors",
		required: true,
		sql: `CREATE UNIQUE INDEX IF NOT EXISTS uq_doc_endpoint_errors
		      ON doc_endpoint_errors (endpoint_id, status_code, code)`,
	},

	// ---- 认证域（全部 required）----
	//
	// 这 9 条是「注册并发去重 / 会话吊销 / 绑定码一次性」的正确性依赖，
	// 任何一条静默缺失都会让对应修复变成摆设，所以必须让启动失败而不是只记日志。
	{
		// 邮箱唯一：**必须**是 lower(email) 表达式 + 部分条件。
		//   - 不分大小写：A@x.com 与 a@x.com 是同一个邮箱，分别建号等于一人两号，
		//     而「找回密码」按 email 查会随机命中其中之一；
		//   - WHERE email <> ''：空串表示未绑定，不加条件时所有未绑定用户互相冲突。
		name:     "uq_users_email_lower",
		sql:      `CREATE UNIQUE INDEX IF NOT EXISTS uq_users_email_lower ON users (lower(email)) WHERE email <> ''`,
		required: true,
	},
	{
		// 手机号唯一，与邮箱同样走**归一后**的表达式索引。
		//
		// 不能直接建在原文列上：verify.Normalize 允许 `+86…` 与 `86…` 两种写法入库，
		// 原文索引会把同一个号码当成两个（可重复注册、可绕开发码冷却）。表达式
		// 与 Normalize 的归一规则必须逐字一致（只去前导 +），否则约束与实际写入
		// 的值不是同一个函数算出来的。
		name:     "uq_users_phone_norm",
		sql:      `CREATE UNIQUE INDEX IF NOT EXISTS uq_users_phone_norm ON users (ltrim(phone, '+')) WHERE phone <> ''`,
		required: true,
	},
	{
		// 邀请码唯一：**必须**带 `WHERE aff_code <> ''`。
		//
		// 这一列的「未生成」状态就是空串，而邀请码是**懒生成**的
		// （注册时先插入用户，随后由 affiliate.OnRegister 补齐）。
		// 全集唯一索引下，第二个空串用户插入直接 23505 ——
		// 症状是「并发注册时随机失败，错误码还是『邮箱已占用』」。
		name:     "uq_users_aff_code",
		sql:      `CREATE UNIQUE INDEX IF NOT EXISTS uq_users_aff_code ON users (aff_code) WHERE aff_code <> ''`,
		required: true,
	},
	{
		// 会话票据等值查询的唯一入口：每个已登录请求都会打这一次索引。
		name:     "uq_sessions_ticket",
		sql:      `CREATE UNIQUE INDEX IF NOT EXISTS uq_sessions_ticket ON user_sessions (ticket_hash)`,
		required: true,
	},
	{
		// 设备列表（含「超额会话清理」按 last_seen_at 排序踢最旧）。
		// 只含有效会话，吊销行不进索引。
		name:     "idx_sessions_uid",
		sql:      `CREATE INDEX IF NOT EXISTS idx_sessions_uid ON user_sessions (uid, last_seen_at DESC) WHERE revoked_at IS NULL`,
		required: true,
	},
	{
		// 过期会话清理任务（每小时扫一次）。
		name:     "idx_sessions_expire",
		sql:      `CREATE INDEX IF NOT EXISTS idx_sessions_expire ON user_sessions (expires_at) WHERE revoked_at IS NULL`,
		required: true,
	},
	{
		name:     "idx_auth_events_time",
		sql:      `CREATE INDEX IF NOT EXISTS idx_auth_events_time ON auth_events (created_at DESC)`,
		required: true,
	},
	{
		// 单个用户的登录历史（「我的登录记录」与「某账号是否被撞库」）。
		name:     "idx_auth_events_uid",
		sql:      `CREATE INDEX IF NOT EXISTS idx_auth_events_uid ON auth_events (uid, created_at DESC)`,
		required: true,
	},
	{
		// 按事件类型统计（管理台筛选 event=login_failed）。
		name:     "idx_auth_events_evt",
		sql:      `CREATE INDEX IF NOT EXISTS idx_auth_events_evt ON auth_events (event, created_at DESC)`,
		required: true,
	},
	{
		// 备份码唯一：并发用同一个码时，两条 UPDATE ... WHERE used_at IS NULL
		// 会命中**不同行**（若同一码对应多行），唯一约束让重复码在生成阶段就被挡住。
		name:     "uq_mfa_backup_code",
		sql:      `CREATE UNIQUE INDEX IF NOT EXISTS uq_mfa_backup_code ON user_mfa_backup_codes (uid, code_hash)`,
		required: true,
	},

	// ---- 应用域（对外 API 的归属锚点与幂等）----
	{
		// 应用标识唯一：接入方按 slug 签发密钥（POST /api/user/apps/{slug}/hosted-key），
		// 重名会让"到底签给了哪个应用"取决于查询顺序。
		name:     "uq_apps_slug",
		sql:      `CREATE UNIQUE INDEX IF NOT EXISTS uq_apps_slug ON apps (slug)`,
		required: true,
	},
	{
		// 应用公告列表：管理台按 app_id 分页、按创建时间倒序。
		// 每条应用的公告通常只有个位数，不需要为"生效窗口"再建部分索引。
		name: "idx_app_notices_app_created",
		sql:  `CREATE INDEX IF NOT EXISTS idx_app_notices_app_created ON app_notices (app_id, created_at DESC)`,
	},
	{
		// 版本号在**应用内**唯一，不是全局唯一：不同应用各自维护自己的版本序列。
		// 含撤回状态（status=3）是刻意的 —— 重发同一个版本号应当复用那一行，
		// 而不是先物理删掉再重建（后者会丢发布历史）。
		name:     "uq_app_releases_version",
		sql:      `CREATE UNIQUE INDEX IF NOT EXISTS uq_app_releases_version ON app_releases (app_id, version)`,
		required: true,
	},
	{
		// 管理台版本列表：按 app_id 分页、id 倒序（最新建的在前）。
		name: "idx_app_releases_app_list",
		sql:  `CREATE INDEX IF NOT EXISTS idx_app_releases_app_list ON app_releases (app_id, id DESC)`,
	},
	{
		// 对外读「最近发布」：WHERE app_id=? AND status=2 ORDER BY published_at DESC LIMIT 1。
		// 部分索引只收已发布行，草稿/撤回不进索引。
		name: "idx_app_releases_latest",
		sql:  `CREATE INDEX IF NOT EXISTS idx_app_releases_latest ON app_releases (app_id, published_at DESC) WHERE status = 2`,
	},
	{
		// uq_api_keys_hosted 是**归因正确性**的硬依赖：同一 (app,user) 若出现两把托管密钥，
		// 应用侧调 /v1 时用的是哪一把取决于它从自己的存储里读到哪一行 —— 用量会随机落到
		// 两把之一，两把的 used 各自累计，KEY 级金额限额也各自计算。
		// 表现是"账单里同一个用户的应用消费被拆成两份"，而任何单点日志都看不出问题。
		//
		// 条件里的 deleted_at IS NULL 是必须的：api_keys 是软删表，
		// 不加这条的话，用户删掉托管密钥后**永远发不出第二把**（墓碑占位）。
		name:     "uq_api_keys_hosted",
		sql:      `CREATE UNIQUE INDEX IF NOT EXISTS uq_api_keys_hosted ON api_keys (app_id, user_id) WHERE kind = 'hosted' AND deleted_at IS NULL`,
		required: true,
	},
	{
		// 管理台按应用筛密钥（应用详情页的「该应用有哪些用户在用」）。
		// 条件 app_id > 0 让平台直连密钥（数量占绝大多数）不进索引。
		name: "idx_api_keys_app",
		sql:  `CREATE INDEX IF NOT EXISTS idx_api_keys_app ON api_keys (app_id, user_id) WHERE app_id > 0`,
	},
	{
		// 轮换重叠窗口的查找索引：鉴权在 key_hash 命中失败后，还要按
		// prev_key_hash 查一次（旧明文在窗口内仍可用）。
		//
		// 必须是部分索引：绝大多数行没有上一代（prev_key_hash 为空串），
		// 全表索引会让每次轮换后的写入都维护大量无用键。
		// 查询也必须带 prev_key_expires_at > now，否则退役钥会被长期命中。
		name: "idx_api_keys_prev_hash",
		sql:  `CREATE INDEX IF NOT EXISTS idx_api_keys_prev_hash ON api_keys (prev_key_hash) WHERE prev_key_hash <> ''`,
	},
	{
		// uq_orders_client_no_v2 是**资金正确性 + 应用隔离**的硬依赖。
		//
		// 为什么在 v1 上加了 app_id：幂等号是「某个调用方对某笔充值的去重键」，
		// 而调用方是**应用**，不是用户。旧索引只按 (user_id, client_order_no) 唯一，
		// 于是应用 A 提交一个应用 B 用过的号时会撞索引 → 回查按 user_id 命中 B 的订单
		// → 把 B 的金额、状态、支付链接返回给 A（跨应用订单信息泄露）；
		// 反过来 A 先占号，B 的充值就建不出来。
		//
		// 换名而不是原地改：`CREATE UNIQUE INDEX IF NOT EXISTS` 遇到同名索引会
		// 直接跳过、不换定义。旧名进 obsoleteIndexes，先 DROP 再建（顺序见 EnsureIndexes）。
		// 新索引严格**弱于**旧索引，历史数据不可能让它建不成。
		name:     "uq_orders_client_no_v2",
		sql:      `CREATE UNIQUE INDEX IF NOT EXISTS uq_orders_client_no_v2 ON orders (user_id, app_id, client_order_no) WHERE client_order_no <> '' AND deleted_at IS NULL`,
		required: true,
	},
	{
		// 管理台按应用筛订单（应用详情页的充值流水）。
		name: "idx_orders_app_created",
		sql:  `CREATE INDEX IF NOT EXISTS idx_orders_app_created ON orders (app_id, created_at DESC) WHERE app_id > 0`,
	},
	{
		// 托管密钥的订单列表：`GET /api/user/orders` 在托管密钥下调用的
		// 过滤条件是 (user_id, app_id)，排序 id DESC。缺它时退化成
		// `(user_id)` 索引再回表过滤 app，应用侧订单一多就慢。
		name: "idx_orders_user_app",
		sql:  `CREATE INDEX IF NOT EXISTS idx_orders_user_app ON orders (user_id, app_id, id DESC) WHERE deleted_at IS NULL`,
	},
	{
		// 「我的兑换记录」：`GET /api/user/redeems` 只查 used_by = 本人，
		// 按 used_at 倒序。used_by 是稀疏列（绝大多数码未被使用），
		// 部分索引把未使用的行排除在外，索引体积约等于已兑换数量。
		name: "idx_redeem_codes_used_by",
		sql:  `CREATE INDEX IF NOT EXISTS idx_redeem_codes_used_by ON redeem_codes (used_by, used_at DESC) WHERE used_by IS NOT NULL`,
	},
	{
		// 图标去重：同一张图被两个管理员先后上传时只留一行。
		//
		// 这条索引不只是"省空间"：service/icon.Save 的**幂等判据**是它 ——
		// 按 sha256 回查命中就直接返回已有行，插入撞 23505 则回查（并发上传同一张图）。
		// 少了它，两次并发上传会产生两行、两个 id、两份对象，
		// 而两行都"能用"，运营看到的是选择器里出现两张一模一样的图。
		//
		// 不用 required：图标重复是浪费而不是错误（不像邮箱唯一会影响登录），
		// 为它拒绝启动代价过大。缺失时 verifyIndexes 会记 ERROR 暴露出来。
		name: "uq_icons_sha256",
		sql:  `CREATE UNIQUE INDEX IF NOT EXISTS uq_icons_sha256 ON icons (sha256)`,
	},
	{
		// 门户「本月用量」：usage_daily 的主键是 (date, model, modality, user_id)，
		// **没有任何以 user_id 为前导的索引** —— 单用户查 `user_id=? AND date>=?`
		// 只能先按日期范围扫全平台的行，再逐行过滤 user_id。
		// 行数量级 = 用户数 × 31 天 × 该用户用过的模型数：1000 个用户时约 15 万行/次。
		//
		// 表本身很小（单用户一个月上限约 6200 行），普通 CREATE 即可，不必 CONCURRENTLY。
		name: "idx_usage_daily_user_date",
		sql:  `CREATE INDEX IF NOT EXISTS idx_usage_daily_user_date ON usage_daily (user_id, date)`,
	},
	{
		// 充值聚合：transactions 上只有单列的 user_id / created_at 索引。
		// 门户主页、管理台看板、两张趋势图都要「某用户某区间的充值净额」，
		// 没有复合索引时每次都会把该用户的全部流水扫一遍（freeze/settle 占绝大多数）。
		//
		// 谓词与 stats.rechargeTypes 逐字一致：三者都是「充值类」流水，
		// 用部分索引把 freeze/release/settle/redeem/adjust 全部排除在索引外。
		// 改动白名单时必须同步改这里，否则新类型不进索引（查询仍正确，只是退化成全表扫）。
		name: "idx_transactions_user_recharge",
		sql: `CREATE INDEX IF NOT EXISTS idx_transactions_user_recharge ON transactions (user_id, created_at)
		      WHERE type IN ('recharge','refund','refund_revert')`,
	},
	{
		// 门户「在途任务数」：已有 idx_tasks_user_list (user_id, id DESC) 能过滤用户，
		// 但历史任务多时仍要扫完全部行才数出那几个 queued/running。
		// 部分索引让扫描量与「当前在途」成正比，而不是与「历史总量」成正比。
		name: "idx_tasks_user_active",
		sql: `CREATE INDEX IF NOT EXISTS idx_tasks_user_active ON tasks (user_id)
		      WHERE status IN ('queued','running') AND deleted_at IS NULL`,
	},
	// ---- 应用插件（声明式）----
	{
		// 插件标识唯一：它进 URL（/admin/apps/:id/p/:key）与安装记录，
		// 重名会让"装的是哪一份 manifest"取决于查询顺序。
		name:     "uq_app_plugins_key",
		sql:      `CREATE UNIQUE INDEX IF NOT EXISTS uq_app_plugins_key ON app_plugins (key)`,
		required: true,
	},
	{
		// 一个应用对同一插件只能有一条安装记录。这是 install 的幂等闸：
		// 并发两次安装靠 23505 收敛，不靠"先查再插"（TOCTOU）。
		name:     "uq_plugin_installs_app_key",
		sql:      `CREATE UNIQUE INDEX IF NOT EXISTS uq_plugin_installs_app_key ON app_plugin_installs (app_id, plugin_key)`,
		required: true,
	},
	{
		// 对外身份唯一。record_key 是服务端 ULID，理论不会撞；
		// 建唯一索引是为了让"算法出错"在写入时立刻暴露，而不是留下两条同号记录。
		name:     "uq_plugin_records_key",
		sql:      `CREATE UNIQUE INDEX IF NOT EXISTS uq_plugin_records_key ON app_plugin_records (app_id, plugin_key, entity, record_key)`,
		required: true,
	},
	{
		// 业务唯一。unique_norm 只在实体声明了 unique 字段时非空，
		// Postgres 默认 NULLS DISTINCT ⇒ 没有唯一字段的实体彼此不冲突。
		name:     "uq_plugin_records_unique",
		sql:      `CREATE UNIQUE INDEX IF NOT EXISTS uq_plugin_records_unique ON app_plugin_records (app_id, plugin_key, entity, unique_norm)`,
		required: true,
	},
	{
		// 列表与导出：WHERE 三元组 + ORDER BY id DESC。
		// 分页只走这一条，不按 data->>'x' 排序（那是 Sort 全扫 + TOAST 读放大）。
		name: "idx_plugin_records_list",
		sql:  `CREATE INDEX IF NOT EXISTS idx_plugin_records_list ON app_plugin_records (app_id, plugin_key, entity, id DESC)`,
	},
	// ---- 一键更新 ----
	{
		// 「全局同时只能有一个进行中的更新」必须是**数据库约束**，不是"先查再插"。
		// 两个管理员同时点，第二条 INSERT 直接 23505 → 409；
		// 而先查再插在并发下会两条都通过，然后两个 agent 抢同一台机器。
		//
		// 表达式索引 `((true))` 的作用：唯一键是常量，所以最多只允许一行满足谓词。
		name:     "uq_release_update_active",
		required: true,
		sql: `CREATE UNIQUE INDEX IF NOT EXISTS uq_release_update_active
		      ON release_update_jobs ((true))
		      WHERE status IN ('queued','fetching','building','backing_up','switching','verifying','rolling_back')`,
	},
	{
		// 幂等：网络重试 / 连点带同一个 request_id，靠唯一索引收敛到同一行。
		name:     "uq_release_update_request",
		required: true,
		sql:      `CREATE UNIQUE INDEX IF NOT EXISTS uq_release_update_request ON release_update_jobs (request_id)`,
	},
	{
		// 最近任务列表（每次打开弹窗都要读"上一次更新是什么结果"）。
		name: "idx_release_update_recent",
		sql:  `CREATE INDEX IF NOT EXISTS idx_release_update_recent ON release_update_jobs (requested_at DESC)`,
	},
}

// nameUniqueTargets 是「同名不允许并存」的配置表（B3 索引项）。
//
// 为什么用**部分**唯一索引（WHERE deleted_at IS NULL）：软删除后名字必须可以复用
// （`channel.ensureNameFree` 与供应商查重都排除软删行），普通唯一索引会让「删掉再建同名」
// 直接失败。条件与业务查重口径严格一致。
//
// 历史库中若已存在重名，创建会失败——此时**跳过并 WARN**，不阻断启动：
// 已有的重名是既成事实，阻断启动只会让服务起不来，而应用层查重仍能阻止新增重复。
var nameUniqueTargets = []struct {
	table  string
	column string
	index  string
}{
	{table: "channels", column: "name", index: "uq_channels_name"},
	{table: "vendors", column: "name", index: "uq_vendors_name"},
	// 套餐名会进支付标题（SanitizeTitle("充值-"+name)），同名会让运营在订单里无法区分档位。
	{table: "packages", column: "name", index: "uq_packages_name"},
	// 规格 slug 直接进 channels.adapter_id（`spec:<slug>`），重名会让两条规格争同一个标识，
	// 结果是"改了 A 却影响 B"这种无法排查的现象。
	// 条件带 deleted_at IS NULL（本函数统一口径）→ 删掉后可以重建同名 slug。
	{table: "adapter_specs", column: "slug", index: "uq_adapter_specs_slug"},
}

// EnsureIndexes 幂等创建计费相关索引，并在结束后自检索引是否真实可用（含 INVALID 检测）。
// obsoleteIndexes 是「历史上存在过、现已废弃」的索引名。
//
// 为什么要显式删：GORM 的 AutoMigrate 只建不删，去掉 `;index` tag 之后那条索引
// 仍会留在库里，与新索引前缀相同、顺序更差，优化器可能选中它。
// 删除是幂等的（IF EXISTS），且**不能**放进 indexes 切片 —— verifyIndexes 会按名字
// 逐个校验存在性，放进切片就等于要求一个「必须不存在」的索引存在。
//
// 表已经整张删掉时（如文档站 v6 的 doc_group*），索引随表消失，这里不必再列。
var obsoleteIndexes = []string{
	// 手机号旧索引（建在原文列上）。`CREATE UNIQUE INDEX IF NOT EXISTS` 遇到
	// 同名索引会直接跳过，不换定义 —— 所以必须先用新名字建，再把旧名删掉。
	"uq_users_phone",
	// 邀请返利的旧索引（GORM tag 建的 + 上一版的三个）。改成「一行一受益人」后：
	// order_id 要的是 (order_id, role) 唯一索引、invitee 侧要的是
	// (invitee_id, order_id)、受益人侧要的是 (beneficiary_id, available_at) ——
	// 全部换了定义，同名建不出来（IF NOT EXISTS 会跳过），只能换新名字再删旧名。
	"idx_aff_reward_order",
	"idx_aff_reward_invitee",
	"uq_affiliate_rewards_order",
	"idx_aff_reward_invitee_avail",
	"idx_aff_reward_inviter_avail",
	// 订单幂等号的旧索引：只按 (user_id, client_order_no) 唯一，会把
	// 应用 A 的幂等号与应用 B 的撞在一起（跨应用订单回查）。
	// 已换成 uq_orders_client_no_v2（多一列 app_id），旧名必须删掉。
	"uq_orders_client_no",
	// 接口 key 的旧定义（node_id, key）：三族共用同一个 URL 段，
	// 已换成全站唯一的 uq_doc_endpoints_key_v2。
	"uq_doc_endpoints_key",
	// 任务幂等的旧定义 (key_id, idempotency_key)：缺端点维度，
	// 会把「同一把钥用同一个键打另一个端点」折叠成同一次提交。
	// 已换成 uq_tasks_idem_v2（多一列 idempotency_endpoint）。
	"uq_tasks_idem",
}

func EnsureIndexes(gdb *gorm.DB) error {
	// SkipDefaultTransaction：DDL 不放进隐式事务（避免后续切换 CONCURRENTLY 时被事务包裹）。
	raw := gdb.Session(&gorm.Session{SkipDefaultTransaction: true})
	for _, name := range obsoleteIndexes {
		if err := raw.Exec("DROP INDEX IF EXISTS " + name).Error; err != nil {
			return fmt.Errorf("db: 删除废弃索引 %s 失败: %w", name, err)
		}
	}
	for _, idx := range indexes {
		if err := raw.Exec(idx.sql).Error; err != nil {
			if isDuplicateData(err) {
				if idx.required {
					// 关键索引建不成 → 业务正确性依赖缺失。继续启动的后果是
					// 「注册可以并发刷出同邮箱账号」这类缺陷在生产静默存在，
					// 且没有任何信号，所以这里必须失败。
					return fmt.Errorf("db: 关键索引 %s 创建失败（存在重复数据），拒绝启动: %w", idx.name, err)
				}
				logger.L().Error("索引创建失败：存在重复数据，去重保护未生效",
					zap.String("index", idx.name), zap.Error(err))
				continue
			}
			if isAlreadyExists(err) {
				continue
			}
			return fmt.Errorf("db: 创建索引 %s 失败: %w", idx.name, err)
		}
	}
	if err := ensureNameUniques(raw); err != nil {
		return err
	}
	return verifyIndexes(gdb)
}

// ensureNameUniques 为配置表建「同名列唯一」部分索引（先查重，有冲突则跳过 + WARN）。
func ensureNameUniques(gdb *gorm.DB) error {
	for _, t := range nameUniqueTargets {
		dups, err := duplicateNames(gdb, t.table, t.column)
		if err != nil {
			return fmt.Errorf("db: 检查 %s.%s 重名失败: %w", t.table, t.column, err)
		}
		if len(dups) > 0 {
			logger.L().Warn("存在重名数据，已跳过唯一索引（应用层查重仍生效）",
				zap.String("table", t.table), zap.String("column", t.column),
				zap.Strings("duplicated_names", dups))
			continue
		}
		sql := fmt.Sprintf(`CREATE UNIQUE INDEX IF NOT EXISTS %s ON %s (%s) WHERE deleted_at IS NULL`,
			t.index, t.table, t.column)
		if err := gdb.Exec(sql).Error; err != nil {
			// 与普通索引不同：这里是**并发启动**下可能撞车（另一实例刚建好），按已存在处理。
			if isDuplicateData(err) || isAlreadyExists(err) {
				logger.L().Warn("唯一索引创建被跳过", zap.String("index", t.index), zap.Error(err))
				continue
			}
			return fmt.Errorf("db: 创建唯一索引 %s 失败: %w", t.index, err)
		}
	}
	return nil
}

// duplicateNames 返回最多 10 个重复的名字（用于日志定位，不用于业务判定）。
func duplicateNames(gdb *gorm.DB, table, column string) ([]string, error) {
	// 表名/列名来自代码内常量白名单，不拼接外部输入。
	sql := fmt.Sprintf(`SELECT %s FROM %s WHERE deleted_at IS NULL GROUP BY %s HAVING count(*) > 1 ORDER BY %s LIMIT 10`,
		column, table, column, column)
	var out []string
	if err := gdb.Raw(sql).Scan(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// verifyIndexes 自检索引存在且未被标记为 INVALID（CONCURRENTLY 中断会留下 INVALID 索引）。
//
// required 索引缺失或 INVALID 时返回 error：CREATE INDEX IF NOT EXISTS 在
// 「同名但定义不同」时也会静默跳过（见 idx_doc_groups_nav 的注释），
// 只按名字建完就当成功，正是让安全约束悄悄消失的那条路。
func verifyIndexes(gdb *gorm.DB) error {
	type target struct {
		name     string
		required bool
	}
	targets := make([]target, 0, len(indexes)+len(nameUniqueTargets))
	for _, idx := range indexes {
		targets = append(targets, target{name: idx.name, required: idx.required})
	}
	for _, t := range nameUniqueTargets {
		targets = append(targets, target{name: t.index})
	}
	for _, t := range targets {
		var valid *bool
		// to_regclass 遵循 search_path，避免多 schema 下误判到同名索引。
		err := gdb.Raw(`SELECT i.indisvalid FROM pg_index i WHERE i.indexrelid = to_regclass(?)`,
			t.name).Scan(&valid).Error
		if err != nil {
			return fmt.Errorf("db: 校验索引 %s 失败: %w", t.name, err)
		}
		if valid == nil {
			if t.required {
				return fmt.Errorf("db: 关键索引 %s 缺失（创建被跳过或回滚），拒绝启动", t.name)
			}
			logger.L().Error("索引缺失（创建被跳过或回滚）", zap.String("index", t.name))
			continue
		}
		if !*valid {
			if t.required {
				return fmt.Errorf("db: 关键索引 %s 处于 INVALID 状态，需重建后启动", t.name)
			}
			logger.L().Error("索引处于 INVALID 状态，需重建", zap.String("index", t.name))
		}
	}
	return nil
}

// isDuplicateData 判定是否为唯一索引因重复数据创建失败。
func isDuplicateData(err error) bool { return pgerr.IsUnique(err) }

// isAlreadyExists 判定索引已存在（并发启动时可能由另一实例先建）。
func isAlreadyExists(err error) bool { return pgerr.IsAlreadyExists(err) }

// ErrIndexMissing 供对账/健康检查引用（索引未建成时业务仍可运行，但去重保护缺失）。
var ErrIndexMissing = errors.New("db: 计费索引缺失")
