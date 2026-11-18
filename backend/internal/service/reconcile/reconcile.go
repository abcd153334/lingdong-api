// Package reconcile 提供计费对账（只读）：余额链、冻结、用量汇总、额度累计、运行态与索引健康度。
//
// 设计约束：
//   - 只读：不修改任何业务数据，可安全地在生产按日运行；
//   - 不假设初始余额为 0：以 transactions.balance_after 链式重放为准；
//   - 每项最多返回 20 条样本，避免大表把响应撑爆。
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"time"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

// 样本上限与查询超时。
const (
	sampleLimit = 20
	// queryTimeout 是**单个检查段**的上限，不是整份对账的上限。
	//
	// 两者必须分开：过去整份对账共用一个 10s deadline，任何一段慢
	// （大表 count、缺索引、锁等待）都会把已经算完的段一起作废 ——
	// 运维看到「执行失败」，既不知道钱有没有问题，也不知道该修哪一段。
	queryTimeout = 10 * time.Second
	// runTimeout 是整份对账的墙钟上限：七个段各 10s 已是 70s，
	// 给到 60s 意味着「全部段都卡住」时整体超时，而不是无限等下去。
	runTimeout = 60 * time.Second
)

// UserDiff 是用户维度的差异样本。
type UserDiff struct {
	UserID     uint64 `json:"user_id"`
	Balance    int64  `json:"balance"`
	Expected   int64  `json:"expected"`
	Frozen     int64  `json:"frozen"`
	FrozenWant int64  `json:"frozen_want"`
}

// ChainSample 是一条「余额链断裂」的样本。
//
// 只有计数没有样本时，运维看到 chain_mismatch=27 完全无从下手：
// 是今天的并发写坏了，还是搬迁时的舍入残差？后者只需一次性校正，
// 前者要立刻查。样本里带上 delta，前者通常不是 ±1 微。
type ChainSample struct {
	UserID uint64 `json:"user_id"`
	TxnID  uint64 `json:"txn_id"`
	Prev   int64  `json:"prev_balance_after"`
	Amount int64  `json:"amount"`
	After  int64  `json:"balance_after"`
	Delta  int64  `json:"delta"`
}

// ChargeDiff 是用量/流水不一致样本。
type ChargeDiff struct {
	UsageID uint64 `json:"usage_id"`
	Charge  int64  `json:"charge"`
	Ledger  int64  `json:"ledger_charge"`
	UserID  uint64 `json:"user_id"`
	Status  string `json:"status"`
}

// OrderDiff 是订单维度的差异样本。
//
// 注意口径：orders.credited 与 transactions.amount **都是微**，可以直接比；
// 但 orders.amount_cent 是「分」，与微不可比 —— 拿订单金额去对账只会得到噪声。
type OrderDiff struct {
	OrderID    uint64 `json:"order_id"`
	OutTradeNo string `json:"out_trade_no"`
	Status     string `json:"status"`
	Credited   int64  `json:"credited"`
	Ledger     int64  `json:"ledger_amount"`
	Issue      string `json:"issue"`
}

// Report 是对账结果。
type Report struct {
	GeneratedAt time.Time `json:"generated_at"`
	// Errors 是**本次未完成**的检查段名（users / usages / ledger / orders /
	// refunds / affiliate / tasks / indexes）。
	//
	// 为什么必须显式给出：段失败时它的计数全是 0，而 0 在页面上与
	// 「查过了、没有差异」完全同形。少了这一栏，一次没跑成的对账会被
	// 当成「账目完美」——这比不跑更危险。
	//
	// 只放段名不放错误原文：这个字段会原样进管理台，而 SQL 错误里带
	// 表名/列名/条件，对运营没有意义，对外部却是现成的库结构地图。
	Errors []string `json:"errors"`
	Users  struct {
		Checked         int64         `json:"checked"`
		ChainMismatch   int64         `json:"chain_mismatch"`
		BalanceMismatch int64         `json:"balance_mismatch"`
		FrozenMismatch  int64         `json:"frozen_mismatch"`
		NegativeFrozen  int64         `json:"negative_frozen"`
		Samples         []UserDiff    `json:"samples"`
		ChainSamples    []ChainSample `json:"chain_samples"`
	} `json:"users"`
	Usages struct {
		PendingTotal  int64 `json:"pending_total"`
		PendingOld    int64 `json:"pending_old"`
		PendingOrphan int64 `json:"pending_orphan"`
		Estimated24h  int64 `json:"estimated_24h"`
		Reclaimed24h  int64 `json:"reclaimed_24h"`
		ZeroUsage24h  int64 `json:"zero_usage_24h"`
		// CalcFailed24h 是按「冻结额兜底入账」的单数：金额可能既不是应收也不是 0，
		// 必须有人复核。以前这个标记只有产生方没有消费方。
		CalcFailed24h int64 `json:"calc_failed_24h"`
	} `json:"usages"`
	Ledger struct {
		TotalRows          int64        `json:"total_rows"`
		Earliest           time.Time    `json:"earliest"`
		DuplicateUsageRows int64        `json:"duplicate_usage_rows"`
		ChargeRows         int64        `json:"charge_rows"`
		ChargeDiff         int64        `json:"charge_diff"`
		Samples            []ChargeDiff `json:"samples"`
	} `json:"ledger"`
	// Orders 是订单与到账流水的一致性（虎皮椒接入新增）。
	//
	// 五类差异各有明确成因：
	//   - missing_ledger       已支付但无流水（入账事务回滚 / 人工改库）
	//   - duplicate_ledger     一单多笔到账（唯一索引未生效时的重复回调）
	//   - pending_but_credited 钱到了但订单还是 pending（入账后状态更新失败）
	//   - amount_mismatch      应到账金额与流水金额不一致（改过套餐/赠送额）
	//   - paid_orders 与 recharge_rows 的计数差额本身
	Orders struct {
		PaidOrders         int64 `json:"paid_orders"`
		RechargeRows       int64 `json:"recharge_rows"`
		MissingLedger      int64 `json:"missing_ledger"`
		DuplicateLedger    int64 `json:"duplicate_ledger"`
		PendingButCredited int64 `json:"pending_but_credited"`
		AmountMismatch     int64 `json:"amount_mismatch"`
		// NeedManualUncredited 是「钱收到了、额度没到、订单卡在 need_manual」。
		//
		// 这类单**没有 recharge 流水**，所以既不在 pending_but_credited（要求有流水），
		// 也不在 missing_ledger（只查 paid）—— 它对账上是完全隐形的，
		// 而它是用户最会投诉的形态（付了钱没到账）。
		NeedManualUncredited int64       `json:"need_manual_uncredited"`
		Samples              []OrderDiff `json:"samples"`
	} `json:"orders"`
	// Refunds 是退款口径的一致性（原路退回 vs 仅回退余额）。
	//
	// 四类差异：
	//   - refunding        在途退款数量（正常值应长期接近 0）
	//   - stuck_refunding  超过 30 分钟未终结（回调丢失 / 结果未知，需人工确认）
	//   - missing_refund_ledger 已退款但无扣回流水（额度没扣回来 = 直接资金损失）
	//   - gateway_without_no  原路退款成功但没有上游退款单号（无法与网关对账）
	Refunds struct {
		Refunding           int64 `json:"refunding"`
		StuckRefunding      int64 `json:"stuck_refunding"`
		MissingRefundLedger int64 `json:"missing_refund_ledger"`
		GatewayWithoutNo    int64 `json:"gateway_without_no"`
		// UpstreamOnly 是「上游已退款、本地未扣额度」的单量。
		//
		// 这类单以前靠 status=need_manual 暴露，而 need_manual 不在支付成功的
		// 终态短路集合里（网关重放支付成功会二次入账），所以载体改成了
		// refund_status=upstream_only、status 仍是 paid。对账必须继续盯住它，
		// 否则「钱退了、额度还在」这件事再没有任何地方可见。
		UpstreamOnly int64 `json:"upstream_only"`
	} `json:"refunds"`
	// Affiliate 是邀请返利的凭据一致性。
	//
	// 返利额度由 affiliate_rewards 的行派生，所以**不存在欠款**这一概念：
	// 退款时只作废「还没转出」的行，绝不从余额倒扣。
	// 这里盯四件事：
	//   - Leak：钱已经退给用户，返利却仍然有效（真差异，唯一可能是某条退款路径
	//     没走 reclaimAffiliate）。判据只看 transferred_at，不比较时钟 ——
	//     旧实现用「应用写的时间 < 数据库写的时间」，既会假报也会漏报；
	//   - Missing：订单已入账却一行返利都没有（返利发放失败后没补发）；
	//   - LedgerDiff：已标记转出的返利总额与 invite 流水总额之差，应恒为 0；
	//   - Cooling：当前冷却中、未转出的返利总额（观察值，不是差异）。
	Affiliate struct {
		LeakRows       int64           `json:"leak_rows"`
		LeakTotal      int64           `json:"leak_total"`
		MissingRows    int64           `json:"missing_rows"`
		TransferredSum int64           `json:"transferred_sum"`
		LedgerDiff     int64           `json:"ledger_diff"`
		CoolingRows    int64           `json:"cooling_rows"`
		CoolingTotal   int64           `json:"cooling_total"`
		Samples        []AffiliateDiff `json:"samples"`
	} `json:"affiliate"`
	Indexes struct {
		Missing []string `json:"missing"`
	} `json:"indexes"`
	// Tasks 是异步任务与 usages 的口径一致性（P0-8）。
	//
	// 为什么必须单独对：任务链路的钱全部走 usages（frozen/charge 都在那张表），
	// tasks.frozen/settled 只是给运营看的副本。两者算错时账面依然「平」——
	// 因为错的公式是一致的错：余额链平、流水平、pending 规模正常，只有把两个来源
	// 摆在一起才看得见。缺了这块，「结算成功但 settled=0」这类事故没有任何自动发现通道。
	Tasks struct {
		// SettledMismatch：任务已成功，但 settled 与 usage.charge 不一致。
		SettledMismatch int64 `json:"settled_mismatch"`
		// PendingAfterTerminal：任务已终结，usage 仍是 pending。
		//
		// 这是最坏的一种：用户余额里那笔钱永远处于「冻结中」（可用余额少一块），
		// 而回收任务可能在很久之后二次补收。
		PendingAfterTerminal int64 `json:"pending_after_terminal"`
		// FrozenMismatch：tasks.frozen 与 usages.frozen 不一致的单量。
		FrozenMismatch int64 `json:"frozen_mismatch"`
		// FrozenDiff：上述行的差额合计（正 = 任务侧记的冻结额偏大）。
		FrozenDiff int64 `json:"frozen_diff"`
		// SettledShortfall：任务已成功交付，但账已**释放**（没收钱）。
		//
		// 三种真实成因，都不在任何既有判据里：
		//   - 迟到成功补收失败（LateSettle 报错，按免费交付）；
		//   - usage 已归档/用户已物理删除（钱无处可记）；
		//   - 提交拿到单号后状态推进失败导致的弃单（W3 之前单号会丢）。
		// 它不会让余额链或流水出现任何不平 —— 钱压根没动过，所以只有这条能发现。
		SettledShortfall int64 `json:"settled_shortfall"`
		// ChargedButFailed：收了钱但任务是失败/超时/取消（settled > 0 且 charge > 0）。
		//
		// 成因是「Settle 成功 → CAS succeeded 失败」这半成功：钱扣了，状态没落，
		// 超时任务随后又被判 expired。用户付了钱拿到的是失败结果，必须人工退。
		ChargedButFailed int64      `json:"charged_but_failed"`
		Samples          []TaskDiff `json:"samples"`
	} `json:"tasks"`
}

// TaskDiff 是一条任务口径不一致的样本。
//
// 字段名与 SQL 里的列别名**逐字对应**（task_frozen / usage_frozen）：
// GORM 的 Scan 按列名映射字段，别名写成 frozen 会静默落到零值 ——
// 样本表里全是 0 而计数非 0，看的人只会更困惑。
type TaskDiff struct {
	TaskID      uint64 `json:"task_id"`
	Status      string `json:"status"`
	Settled     int64  `json:"settled"`
	Charge      int64  `json:"charge"`
	TaskFrozen  int64  `json:"task_frozen"`
	UsageFrozen int64  `json:"usage_frozen"`
	Issue       string `json:"issue"`
}

// AffiliateDiff 是一条「返利凭据对不上」的样本。
type AffiliateDiff struct {
	OrderID       uint64    `json:"order_id"`
	InviteeID     uint64    `json:"invitee_id"`
	BeneficiaryID uint64    `json:"beneficiary_id"`
	Role          string    `json:"role"`
	Amount        int64     `json:"amount"`
	AvailableAt   time.Time `json:"available_at"`
	// Issue 说明这条为什么被列出来：refund_leak（钱退了返利还在）
	// 或 missing_reward（已入账却没有返利行）。
	Issue string `json:"issue"`
}

// Run 执行一次全量对账（只读）。
//
// 分段执行、单段隔离。七个段各自一个 10s deadline，任何一段失败只记进
// Report.Errors 并继续跑其余段 —— 「订单段查不出来」不该顺手把「余额链
// 断了 27 条」这个更要紧的结论一起吞掉。
//
// 只有**全部**段失败才返回 error：那已经说明库不可用，报告没有意义。
func Run(ctx context.Context) (*Report, error) {
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()

	rep := &Report{GeneratedAt: time.Now(), Errors: []string{}}
	rep.Users.Samples = []UserDiff{}
	rep.Users.ChainSamples = []ChainSample{}
	rep.Ledger.Samples = []ChargeDiff{}
	rep.Orders.Samples = []OrderDiff{}
	rep.Affiliate.Samples = []AffiliateDiff{}
	rep.Tasks.Samples = []TaskDiff{}
	rep.Indexes.Missing = []string{}

	segments := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"users", func(c context.Context) error { return checkUsers(c, rep) }},
		{"usages", func(c context.Context) error { return checkUsages(c, rep) }},
		{"ledger", func(c context.Context) error { return checkLedger(c, rep) }},
		{"orders", func(c context.Context) error { return checkOrders(c, rep) }},
		{"refunds", func(c context.Context) error { return checkRefunds(c, rep) }},
		{"affiliate", func(c context.Context) error { return checkAffiliate(c, rep) }},
		{"tasks", func(c context.Context) error { return checkTasks(c, rep) }},
		{"indexes", func(c context.Context) error { return checkIndexes(c, rep) }},
	}

	var errs []error
	for _, seg := range segments {
		sctx, scancel := context.WithTimeout(ctx, queryTimeout)
		err := seg.fn(sctx)
		scancel()
		if err == nil {
			continue
		}
		rep.Errors = append(rep.Errors, seg.name)
		errs = append(errs, fmt.Errorf("%s: %w", seg.name, err))
	}
	if len(errs) == len(segments) {
		return nil, errors.Join(errs...)
	}
	return rep, nil
}

// checkUsers 是用户段：余额链、末行 vs users.balance、冻结一致性、负冻结，以及两类样本。
//
// 样本与计数用同一个 WHERE（同一份 CTE）是刻意的：口径分家时最典型的故障是
// 「计数说 3 条、样本却列出 20 条」，看的人无法判断哪个是真的。
func checkUsers(ctx context.Context, rep *Report) error {
	gdb := db.DB.WithContext(ctx)
	if err := gdb.Raw(`SELECT count(*) FROM users`).Scan(&rep.Users.Checked).Error; err != nil {
		return err
	}

	// 1) 余额链：上一行 balance_after + 本行 amount 必须等于本行 balance_after。
	if err := gdb.Raw(`
		WITH chain AS (
		  SELECT user_id, id, amount, balance_after,
		         lag(balance_after) OVER (PARTITION BY user_id ORDER BY id) AS prev
		    FROM transactions)
		SELECT count(*) FROM chain WHERE prev IS NOT NULL AND prev + amount <> balance_after`).
		Scan(&rep.Users.ChainMismatch).Error; err != nil {
		return err
	}

	// 2) 末行 vs 用户表 + 冻结一致性。
	//
	// 冻结**不能**用「末行流水的 frozen_after」判断。原因：按次/按规格调用是精确预扣（call/spec），
	// 结算时 charge == frozen，`billing.Settle` 在 diff==0 时**按设计不写 settle 流水**（流水记的是
	// 资金变动，消费明细在 usages）。于是末行永远停在 freeze 那一笔的 frozen_after 上，而用户实际的
	// frozen 早已归零 —— 每个跑过一次按次调用的用户都会天天下报 frozen_mismatch。
	//
	// 正确口径是「未结算（pending）会话的冻结合计」：Freeze 时把冻结额固化进 usages.frozen，
	// Settle / Release 把它改成终态，所以 pending 之和就是此刻**应该**被冻结的额度。
	if err := gdb.Raw(`
		WITH last AS (
		  SELECT DISTINCT ON (user_id) user_id, balance_after
		    FROM transactions ORDER BY user_id, id DESC),
		pend AS (
		  SELECT user_id, sum(frozen) AS frozen_want
		    FROM usages WHERE status = 'pending' GROUP BY user_id)
		SELECT
		  count(*) FILTER (WHERE u.balance <> COALESCE(l.balance_after, 0))            AS balance_mismatch,
		  count(*) FILTER (WHERE u.frozen <> COALESCE(p.frozen_want, 0))       AS frozen_mismatch,
		  count(*) FILTER (WHERE u.frozen < 0)                                 AS negative_frozen
		  FROM users u
		  LEFT JOIN last l ON l.user_id = u.id
		  LEFT JOIN pend p ON p.user_id = u.id
		 WHERE u.deleted_at IS NULL`).
		Row().Scan(&rep.Users.BalanceMismatch, &rep.Users.FrozenMismatch, &rep.Users.NegativeFrozen); err != nil {
		return err
	}

	// 3) 差异样本（最多 20 条）。
	samples := make([]UserDiff, 0, sampleLimit)
	if err := gdb.Raw(`
		WITH last AS (
		  SELECT DISTINCT ON (user_id) user_id, balance_after
		    FROM transactions ORDER BY user_id, id DESC),
		pend AS (
		  SELECT user_id, sum(frozen) AS frozen_want
		    FROM usages WHERE status = 'pending' GROUP BY user_id)
		SELECT u.id AS user_id, u.balance, COALESCE(l.balance_after, 0) AS expected,
		       u.frozen AS frozen, COALESCE(p.frozen_want, 0) AS frozen_want
		  FROM users u
		  LEFT JOIN last l ON l.user_id = u.id
		  LEFT JOIN pend p ON p.user_id = u.id
		 WHERE u.deleted_at IS NULL
		   AND (u.balance <> COALESCE(l.balance_after, 0)
		        OR u.frozen <> COALESCE(p.frozen_want, 0))
		 ORDER BY u.id LIMIT ?`, sampleLimit).Scan(&samples).Error; err != nil {
		return err
	}
	rep.Users.Samples = samples

	// 3b) 余额链断裂的样本（可定位到具体流水）。
	chainSamples := make([]ChainSample, 0, sampleLimit)
	if err := gdb.Raw(`
		WITH chain AS (
		  SELECT user_id, id, amount, balance_after,
		         lag(balance_after) OVER (PARTITION BY user_id ORDER BY id) AS prev
		    FROM transactions)
		SELECT user_id, id AS txn_id, prev AS prev_balance_after, amount,
		       balance_after, balance_after - (prev + amount) AS delta
		  FROM chain WHERE prev IS NOT NULL AND prev + amount <> balance_after
		 ORDER BY user_id, id LIMIT ?`, sampleLimit).Scan(&chainSamples).Error; err != nil {
		return err
	}
	rep.Users.ChainSamples = chainSamples
	return nil
}

// checkUsages 是运行态段：pending 规模/年龄/孤儿 + 24 小时口径。
//
// 这一段天然长期非零（有请求在跑就有 pending），所以它只进「观察档」。
func checkUsages(ctx context.Context, rep *Report) error {
	gdb := db.DB.WithContext(ctx)
	// 4) 运行态：pending 规模/年龄/孤儿（无 task 的同步请求）。
	if err := gdb.Raw(`
		SELECT
		  count(*)                                                                   AS pending_total,
		  count(*) FILTER (WHERE created_at < now() - interval '10 minutes')          AS pending_old,
		  count(*) FILTER (WHERE NOT EXISTS (SELECT 1 FROM tasks t WHERE t.usage_id = u.id)) AS pending_orphan
		  FROM usages u WHERE status = 'pending'`).
		Row().Scan(&rep.Usages.PendingTotal, &rep.Usages.PendingOld, &rep.Usages.PendingOrphan); err != nil {
		return err
	}
	// 三个计数都必须**按项精确匹配**：`LIKE '%reclaimed%'` 会把
	// `reclaimed_after_success` / `reclaimed_empty_success` 一起算进去，
	// 于是"回收入账"的数字永远偏高，而这条曲线正是判断少收的依据。
	// `anomaly` 是逗号分隔串，用 string_to_array 精确比一项（与 model.HasAnomaly 同口径）。
	if err := gdb.Raw(`
		SELECT
		  count(*) FILTER (WHERE usage_source = 'estimated')                          AS estimated_24h,
		  count(*) FILTER (WHERE 'reclaimed' = ANY(string_to_array(COALESCE(anomaly,''), ',')))  AS reclaimed_24h,
		  count(*) FILTER (WHERE 'zero_usage' = ANY(string_to_array(COALESCE(anomaly,''), ','))) AS zero_usage_24h,
		  count(*) FILTER (WHERE 'charge_calc_failed' = ANY(string_to_array(COALESCE(anomaly,''), ','))) AS calc_failed_24h
		  FROM usages WHERE created_at > now() - interval '24 hours'`).
		Row().Scan(&rep.Usages.Estimated24h, &rep.Usages.Reclaimed24h, &rep.Usages.ZeroUsage24h,
		&rep.Usages.CalcFailed24h); err != nil {
		return err
	}
	return nil
}

// checkLedger 是流水段：总量/最早时间/重复 usage 维度，以及「usage 维度净额 vs charge」。
func checkLedger(ctx context.Context, rep *Report) error {
	gdb := db.DB.WithContext(ctx)
	// 5) 流水：总量/最早时间/重复 usage 维度。
	if err := gdb.Raw(`SELECT count(*), COALESCE(min(created_at), now()) FROM transactions`).
		Row().Scan(&rep.Ledger.TotalRows, &rep.Ledger.Earliest); err != nil {
		return err
	}
	if err := gdb.Raw(`
		SELECT COALESCE(sum(c), 0) FROM (
		  SELECT count(*) AS c FROM transactions
		   WHERE ref_type='usage' GROUP BY ref_id, type HAVING count(*) > 1) t`).
		Scan(&rep.Ledger.DuplicateUsageRows).Error; err != nil {
		return err
	}

	// 6) 用量汇总：usage 维度流水 -Σ(amount) 应等于 charge（freeze+settle/release 净额）。
	//
	// ref_id 是 text 列（订单用 id、用量也用 id），直接 ::bigint 转换时
	// 只要出现一行非数字（历史导入、手工改库）就会让**整份对账** 500。
	// 正则守卫把这类脏行排除在外：它们本来就不是合法的 usage 引用。
	if err := gdb.Raw(`
		WITH l AS (
		  SELECT ref_id::bigint AS usage_id, -sum(amount) AS charge
		    FROM transactions WHERE ref_type = 'usage' AND ref_id ~ '^[0-9]+$' GROUP BY 1)
		SELECT count(*) FILTER (WHERE COALESCE(l.charge, 0) <> u.charge) AS charge_rows,
		       COALESCE(sum(COALESCE(l.charge, 0) - u.charge), 0)         AS charge_diff
		  FROM usages u LEFT JOIN l ON l.usage_id = u.id
		 WHERE u.status <> 'pending'`).
		Row().Scan(&rep.Ledger.ChargeRows, &rep.Ledger.ChargeDiff); err != nil {
		return err
	}
	chargeSamples := make([]ChargeDiff, 0, sampleLimit)
	if err := gdb.Raw(`
		WITH l AS (
		  SELECT ref_id::bigint AS usage_id, -sum(amount) AS charge
		    FROM transactions WHERE ref_type = 'usage' AND ref_id ~ '^[0-9]+$' GROUP BY 1)
		SELECT u.id AS usage_id, u.charge AS charge, COALESCE(l.charge, 0) AS ledger,
		       u.user_id, u.status
		  FROM usages u LEFT JOIN l ON l.usage_id = u.id
		 WHERE u.status <> 'pending' AND COALESCE(l.charge, 0) <> u.charge
		 ORDER BY u.id LIMIT ?`, sampleLimit).Scan(&chargeSamples).Error; err != nil {
		return err
	}
	rep.Ledger.Samples = chargeSamples
	return nil
}

// checkIndexes 是索引健康度段（缺失或 INVALID 都会导致去重/扫描保护失效）。
func checkIndexes(ctx context.Context, rep *Report) error {
	gdb := db.DB.WithContext(ctx)
	for _, name := range []string{
		"idx_usages_pending", "idx_usages_user_created", "idx_usages_key_created", "uq_transactions_usage_ref",
		// 支付相关：uq_transactions_order_recharge 缺失时并发回调可能重复入账；
		// idx_orders_query 缺失时主动查单会退化为全表扫。
		"uq_transactions_order_recharge", "idx_orders_status_expire", "idx_orders_query",
		// 退款滞留巡检依赖它（status='refunding' 的部分索引）。
		"idx_orders_refunding",
	} {
		var valid *bool
		if err := gdb.Raw(
			`SELECT i.indisvalid FROM pg_index i WHERE i.indexrelid = to_regclass(?)`, name).Scan(&valid).Error; err != nil {
			return err
		}
		if valid == nil || !*valid {
			rep.Indexes.Missing = append(rep.Indexes.Missing, name)
		}
	}
	return nil
}

// checkTasks 对账异步任务与 usages（只读）。
//
// 五条判据对应五种真实事故形态：
//  1. `status='succeeded'` 但 `settled <> usage.charge` —— 结算公式用错（最典型：
//     按请求秒数而不是上游实际秒数、张数按 1 而不是实际张数）。账面全平，只是少收/多收；
//  2. 任务已终结但 usage 还是 pending —— 钱永远挂在「冻结中」，且回收任务会二次补收；
//  3. `tasks.frozen <> usages.frozen` —— 冻结快照与账务记录脱节，退款额会按错的数退；
//  4. succeeded + usage released —— 交付了却没收钱（迟到补收失败 / 账目归档 / 单号丢失弃单）；
//  5. usage settled 且 charge>0 + 任务 failed/expired/canceled —— 收了钱但结果不可用。
//
// 4/5 两条是**只读的结构性判据**，不试图自动修复：能不能补收、要不要退款
// 取决于「产物还在不在、用户看没看过」，那是人工判断，不是对账能替的。
//
// 全部用 JOIN 而非子查询：tasks.usage_id 是唯一的关联键（提交时 Freeze 建立），
// 没有它说明任务根本没走资金链路，属于另一类问题（由 orphan 检查覆盖）。
func checkTasks(ctx context.Context, rep *Report) error {
	gdb := db.DB.WithContext(ctx)
	const terminal = `('succeeded','failed','canceled','expired')`

	if err := gdb.Raw(`
		SELECT count(*) FROM tasks t JOIN usages u ON u.id = t.usage_id
		 WHERE t.deleted_at IS NULL AND t.status = 'succeeded' AND t.settled <> u.charge`).
		Scan(&rep.Tasks.SettledMismatch).Error; err != nil {
		return err
	}
	if err := gdb.Raw(`
		SELECT count(*) FROM tasks t JOIN usages u ON u.id = t.usage_id
		 WHERE t.deleted_at IS NULL AND t.status IN ` + terminal + ` AND u.status = 'pending'`).
		Scan(&rep.Tasks.PendingAfterTerminal).Error; err != nil {
		return err
	}
	// FrozenMismatch 只在**钱还压着**的时候才有意义：`usages.frozen` 是当前占用额
	// （结算/释放后归零），`tasks.frozen` 是提交时的冻结快照（保留原值用于展示）。
	//
	// 不加 `u.status = 'pending'` 就会把这个"快照 vs 现值"的差异当成异常：
	// 2026-11-16 实测线上恒为 25 条（15 条 settled + 10 条 released，全是终态），
	// 每天报一次红色差异 —— 报警一旦长期为真且无害，就再也没人看后面的真实异常。
	if err := gdb.Raw(`
		SELECT count(*), COALESCE(sum(t.frozen - u.frozen), 0)
		  FROM tasks t JOIN usages u ON u.id = t.usage_id
		 WHERE t.deleted_at IS NULL AND u.status = 'pending' AND t.frozen <> u.frozen`).
		Row().Scan(&rep.Tasks.FrozenMismatch, &rep.Tasks.FrozenDiff); err != nil {
		return err
	}
	// 4) 交付了却没收到钱。
	//
	// 误报评估：正常成功必走 Settle（usage → settled），只有异常路径会停在 released；
	// Retry 产生的**旧** usage 是 released，但它对应的任务已被重置为 queued，
	// 不会命中 succeeded，所以不会误报。
	if err := gdb.Raw(`
		SELECT count(*) FROM tasks t JOIN usages u ON u.id = t.usage_id
		 WHERE t.deleted_at IS NULL AND t.status = 'succeeded' AND u.status = 'released'`).
		Scan(&rep.Tasks.SettledShortfall).Error; err != nil {
		return err
	}
	// 5) 收了钱但任务不可用。
	//
	// 判据带 u.charge > 0：免费模型/额度为 0 的调用本来就不该收钱，
	// 把它们算进来会让这条判据在免费模型上长期非零，从而被当成噪声忽略。
	if err := gdb.Raw(`
		SELECT count(*) FROM tasks t JOIN usages u ON u.id = t.usage_id
		 WHERE t.deleted_at IS NULL AND t.status IN ('failed','expired','canceled')
		   AND u.status = 'settled' AND u.charge > 0`).
		Scan(&rep.Tasks.ChargedButFailed).Error; err != nil {
		return err
	}

	samples := make([]TaskDiff, 0, sampleLimit)
	if err := gdb.Raw(`
		SELECT t.id AS task_id, t.status, t.settled, u.charge,
		       t.frozen AS task_frozen, u.frozen AS usage_frozen,
		       CASE WHEN t.status = 'succeeded' AND u.status = 'released' THEN 'settled_shortfall'
		            WHEN t.status IN ('failed','expired','canceled') AND u.status = 'settled' AND u.charge > 0 THEN 'charged_but_failed'
		            WHEN t.status = 'succeeded' AND t.settled <> u.charge THEN 'settled_mismatch'
		            WHEN u.status = 'pending' THEN 'pending_after_terminal'
		            ELSE 'frozen_mismatch' END AS issue
		  FROM tasks t JOIN usages u ON u.id = t.usage_id
		 WHERE t.deleted_at IS NULL
		   AND ((t.status = 'succeeded' AND u.status = 'released')
		        OR (t.status IN ('failed','expired','canceled') AND u.status = 'settled' AND u.charge > 0)
		        OR (t.status = 'succeeded' AND t.settled <> u.charge)
		        OR (t.status IN `+terminal+` AND u.status = 'pending')
		        OR t.frozen <> u.frozen)
		 ORDER BY t.id DESC LIMIT ?`, sampleLimit).Scan(&samples).Error; err != nil {
		return err
	}
	rep.Tasks.Samples = samples
	return nil
}

// checkOrders 对账订单与到账流水（只读）。
//
// 为什么必须单独对：虎皮椒的回调与主动查单虽然是同一条入账路径（settlePaid），
// 但「入账成功、订单状态更新失败」这种半成功状态只有对账能发现 ——
// 用户余额已经加了，订单却还显示待支付。
func checkOrders(ctx context.Context, rep *Report) error {
	gdb := db.DB.WithContext(ctx)

	if err := gdb.Raw(`SELECT count(*) FROM orders WHERE status = 'paid' AND deleted_at IS NULL`).
		Scan(&rep.Orders.PaidOrders).Error; err != nil {
		return err
	}
	if err := gdb.Raw(`SELECT count(*) FROM transactions WHERE ref_type = 'order' AND type = 'recharge'`).
		Scan(&rep.Orders.RechargeRows).Error; err != nil {
		return err
	}
	if err := gdb.Raw(`
		SELECT count(*) FROM orders o
		WHERE o.status = 'paid' AND o.deleted_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM transactions t
		                  WHERE t.ref_type = 'order' AND t.ref_id = o.id::text AND t.type = 'recharge')`).
		Scan(&rep.Orders.MissingLedger).Error; err != nil {
		return err
	}
	if err := gdb.Raw(`
		SELECT count(*) FROM (
		  SELECT ref_id FROM transactions
		  WHERE ref_type = 'order' AND type = 'recharge'
		  GROUP BY ref_id HAVING count(*) > 1) x`).
		Scan(&rep.Orders.DuplicateLedger).Error; err != nil {
		return err
	}
	// 覆盖所有「钱已入账但状态没推进到 paid」的形态：
	//   - pending：并发/崩溃留下的半成功；
	//   - need_manual：金额不符、超余额上限等被转人工，但入账可能已经发生。
	// 只查 pending 会漏掉后者 —— 而后者正是「已扣款却看不到已支付」的客诉来源。
	if err := gdb.Raw(`
		SELECT count(*) FROM orders o
		WHERE o.status IN ('pending', 'need_manual') AND o.deleted_at IS NULL
		  AND EXISTS (SELECT 1 FROM transactions t
		              WHERE t.ref_type = 'order' AND t.ref_id = o.id::text AND t.type = 'recharge')`).
		Scan(&rep.Orders.PendingButCredited).Error; err != nil {
		return err
	}
	if err := gdb.Raw(`
		SELECT count(*) FROM orders o
		JOIN transactions t ON t.ref_type = 'order' AND t.ref_id = o.id::text AND t.type = 'recharge'
		WHERE o.status = 'paid' AND o.deleted_at IS NULL AND o.credited <> t.amount`).
		Scan(&rep.Orders.AmountMismatch).Error; err != nil {
		return err
	}
	// 钱收到了（有回调、写了 pay_error）但额度没到，且订单卡在 need_manual。
	// 这是"用户付了钱、平台没给额度"的唯一可见入口。
	if err := gdb.Raw(`
		SELECT count(*) FROM orders o
		WHERE o.status = 'need_manual' AND o.deleted_at IS NULL
		  AND COALESCE(o.pay_error, '') <> ''
		  AND NOT EXISTS (SELECT 1 FROM transactions t
		                  WHERE t.ref_type = 'order' AND t.ref_id = o.id::text AND t.type = 'recharge')`).
		Scan(&rep.Orders.NeedManualUncredited).Error; err != nil {
		return err
	}

	samples := make([]OrderDiff, 0, sampleLimit)
	// 样本的 WHERE 必须与上面五个计数**逐条对应**，包括 need_manual 与一单多流水：
	//   - pending_but_credited 的计数含 need_manual，样本只写 pending 时，
	//     就会出现「计数 3、样本 0」——看的人只会认为报表坏了；
	//   - duplicate_ledger 原本没有样本分支，运维拿到一个数字却定位不到任何一单。
	// LEFT JOIN + CASE 能把五类差异压进同一个结果集，顺序按「越像资金损失越靠前」。
	if err := gdb.Raw(`
		SELECT o.id AS order_id, o.out_trade_no, o.status, o.credited,
		       COALESCE(t.amount, 0) AS ledger,
		       CASE WHEN t.id IS NULL THEN 'missing_ledger'
		            WHEN t.dup > 1 THEN 'duplicate_ledger'
		            WHEN o.credited <> t.amount THEN 'amount_mismatch'
		            ELSE 'pending_but_credited' END AS issue
		FROM orders o
		LEFT JOIN LATERAL (
		  SELECT min(id) AS id, sum(amount) AS amount, count(*) AS dup
		    FROM transactions
		   WHERE ref_type = 'order' AND ref_id = o.id::text AND type = 'recharge'
		) t ON true
		WHERE o.deleted_at IS NULL
		  AND ((o.status = 'paid' AND t.id IS NULL)
		       OR (o.status = 'paid' AND t.dup > 1)
		       OR (o.status = 'paid' AND o.credited <> t.amount)
		       OR (o.status IN ('pending', 'need_manual') AND t.id IS NOT NULL))
		ORDER BY o.id DESC LIMIT ?`, sampleLimit).Scan(&samples).Error; err != nil {
		return err
	}
	rep.Orders.Samples = samples

	return nil
}

// checkRefunds 对账退款（只读）。
//
// 为什么要单独对：原路退款是「先扣额度、再出网」，中间任何一步半成功都会留下
// 「额度扣了但退款没成」或「退款成了但没扣额度」——两者都是真金白银的差异，
// 而且都不会自动暴露（订单状态看起来完全正常）。
//
// 独立成段（不再由 checkOrders 顺带调用）：退款段失败时必须只标记 refunds 未完成，
// 而不是把整段订单结论一起作废 —— 订单段里还有「已支付无流水」这类更急的信号。
func checkRefunds(ctx context.Context, rep *Report) error {
	gdb := db.DB.WithContext(ctx)
	if err := gdb.Raw(`SELECT count(*) FROM orders WHERE status = 'refunding' AND deleted_at IS NULL`).
		Scan(&rep.Refunds.Refunding).Error; err != nil {
		return err
	}
	// 30 分钟：虎皮椒退款基本秒级完成，超过这个窗口只可能是回调丢了或结果未知。
	if err := gdb.Raw(`
		SELECT count(*) FROM orders
		WHERE status = 'refunding' AND deleted_at IS NULL
		  AND refund_at IS NOT NULL AND refund_at < now() - interval '30 minutes'`).
		Scan(&rep.Refunds.StuckRefunding).Error; err != nil {
		return err
	}
	// 已退款但没有任何 type='refund' 流水：额度没被扣回（历史手工改状态也会落在这里）。
	// 只统计「确实入过账」的订单：从未入账的单（回调金额不符/超余额上限转人工后
	// 只回退余额）本来就没有额度可扣，也没有 refund 流水，把它们算进来会永久误报。
	// 判据是 recharge 流水，不是 status —— 状态会被人工改，流水不会。
	if err := gdb.Raw(`
		SELECT count(*) FROM orders o
		WHERE o.status = 'refunded' AND o.deleted_at IS NULL
		  AND EXISTS (SELECT 1 FROM transactions r
		              WHERE r.ref_type = 'order' AND r.ref_id = o.id::text AND r.type = 'recharge')
		  AND NOT EXISTS (SELECT 1 FROM transactions t
		                  WHERE t.ref_type = 'order' AND t.ref_id = o.id::text AND t.type = 'refund')`).
		Scan(&rep.Refunds.MissingRefundLedger).Error; err != nil {
		return err
	}
	if err := gdb.Raw(`
		SELECT count(*) FROM orders
		WHERE refund_status = 'upstream_only' AND deleted_at IS NULL`).
		Scan(&rep.Refunds.UpstreamOnly).Error; err != nil {
		return err
	}
	// 原路退款成功却没有上游退款单号：无法与网关逐笔对账。
	if err := gdb.Raw(`
		SELECT count(*) FROM orders
		WHERE status = 'refunded' AND refund_mode = 'gateway' AND refund_no = '' AND deleted_at IS NULL`).
		Scan(&rep.Refunds.GatewayWithoutNo).Error; err != nil {
		return err
	}
	return nil
}

// checkAffiliate 对账邀请返利的凭据一致性（只读）。
//
// 四项口径：
//   - Leak（真差异）：钱已经退回用户（含 upstream_only），返利却仍然有效。
//     判据只看「未作废 + 未转出」：钱在不在用户手里由 transferred_at 决定，
//     与「有没有到期」无关。旧实现比 `refund_done_at < available_at` 既会假报
//     （应用时钟快于库时钟）也会漏报（upstream_only 不在 refund_status='success' 里）。
//   - Missing（真差异）：订单已入账（有 recharge 流水）、未退款，却一行返利都没有 ——
//     返利发放失败后的补发缺口，靠 BackfillMissing 修。
//   - LedgerDiff（真差异）：已转出的返利总额与 invite 流水总额之差，应恒为 0。
//   - Cooling（观察值）：当前冷却中、未转出的返利总额，用来核对
//     「近 T 小时的已付订单 × 预期返利率」，不是错误。
func checkAffiliate(ctx context.Context, rep *Report) error {
	gdb := db.DB.WithContext(ctx)
	const leakWhere = `NOT r.voided AND r.transferred_at IS NULL
		   AND o.refund_status IN ('success', 'upstream_only')`

	if err := gdb.Raw(`
		SELECT count(*), COALESCE(SUM(r.amount), 0)
		  FROM affiliate_rewards r
		  JOIN orders o ON o.id = r.order_id
		 WHERE `+leakWhere).Row().
		Scan(&rep.Affiliate.LeakRows, &rep.Affiliate.LeakTotal); err != nil {
		return err
	}
	samples := make([]AffiliateDiff, 0, sampleLimit)
	if err := gdb.Raw(`
		SELECT r.order_id, r.invitee_id, r.beneficiary_id, r.role,
		       r.amount, r.available_at, 'refund_leak' AS issue
		  FROM affiliate_rewards r
		  JOIN orders o ON o.id = r.order_id
		 WHERE `+leakWhere+`
		 ORDER BY r.id DESC LIMIT ?`, sampleLimit).Scan(&samples).Error; err != nil {
		return err
	}

	// 「已入账但无返利行」：流水才是入账事实（订单状态可能被人改过）。
	// 已退款的一律排除：补发出去就是「钱退了还能提返利」。
	const missingWhere = `o.status = 'paid'
		   AND (o.refund_status IS NULL OR o.refund_status = '')
		   AND EXISTS (SELECT 1 FROM transactions t
		                WHERE t.ref_type = 'order' AND t.ref_id = o.id::text AND t.type = 'recharge')
		   AND NOT EXISTS (SELECT 1 FROM affiliate_rewards r WHERE r.order_id = o.id)`
	if err := gdb.Raw(`SELECT count(*) FROM orders o WHERE ` + missingWhere).Row().
		Scan(&rep.Affiliate.MissingRows); err != nil {
		return err
	}
	if rep.Affiliate.MissingRows > 0 {
		var miss []AffiliateDiff
		if err := gdb.Raw(`
			SELECT o.id AS order_id, o.user_id AS invitee_id,
			       0 AS beneficiary_id, '' AS role, 0 AS amount,
			       now() AS available_at, 'missing_reward' AS issue
			  FROM orders o
			 WHERE `+missingWhere+`
			 ORDER BY o.id DESC LIMIT ?`, sampleLimit).Scan(&miss).Error; err != nil {
			return err
		}
		samples = append(samples, miss...)
	}
	rep.Affiliate.Samples = samples

	if err := gdb.Raw(`
		SELECT COALESCE((SELECT SUM(amount) FROM affiliate_rewards WHERE transferred_at IS NOT NULL), 0)`).
		Row().Scan(&rep.Affiliate.TransferredSum); err != nil {
		return err
	}
	var ledger int64
	if err := gdb.Raw(`SELECT COALESCE(SUM(amount), 0) FROM transactions WHERE type = ?`,
		model.TxnInvite).Row().Scan(&ledger); err != nil {
		return err
	}
	rep.Affiliate.LedgerDiff = rep.Affiliate.TransferredSum - ledger

	if err := gdb.Raw(`
		SELECT count(*), COALESCE(SUM(amount), 0)
		  FROM affiliate_rewards
		 WHERE NOT voided AND transferred_at IS NULL AND available_at > now()`).
		Row().Scan(&rep.Affiliate.CoolingRows, &rep.Affiliate.CoolingTotal); err != nil {
		return err
	}
	return nil
}
