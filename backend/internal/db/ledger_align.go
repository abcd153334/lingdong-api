package db

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

// 本文件负责「账务链校正」：把 transactions.balance_after 重算成一条自洽的链。
//
// 为什么需要它：M30 把存量账务从「额度」搬到「微」时，amount 与 balance_after
// 是**逐列独立四舍五入**的（round(x*fx*1e6/69444)），两列各自的舍入误差一正一负，
// 于是 `上一行 balance_after + 本行 amount ≠ 本行 balance_after`。
// 金额上每行只差 1 微，但对账的链式判据会把这些行永远算成差异 ——
// 每日一条 ERROR 日志，真正的差异反而被噪声埋掉。
//
// 校正口径（关键，改之前先读）：
//   - 以 **users.balance 为终点**反向推导（末行 = 当前余额，第 i 行 = 第 i+1 行 - 第 i+1 行 amount）；
//   - 只改 `balance_after` 这一列快照，**不动 amount / 不动任何人的余额** ——
//     余额是真金白银，amount 是业务上真实发生的扣费，只有快照是「可以算出来的」。
//   - 不以首行为锚：首行之前没有记录，锚在首行等于把误差累积到用户当前余额上，那才是真改钱。

// chainRowsSQL 是「自洽链」的唯一算法（供统计与改写共用，避免两处漂移）。
//
// after_sum 是「本行及更晚所有行的 amount 之和」；减去本行 amount 即「比本行更晚的金额之和」。
// 于是 expected = users.balance - 更晚行的金额之和。
const chainRowsSQL = `
	WITH c AS (
	  SELECT t.id AS txn_id, t.user_id, t.amount, t.balance_after,
	         sum(t.amount) OVER (PARTITION BY t.user_id ORDER BY t.id DESC) - t.amount AS after_sum
	    FROM transactions t)
	SELECT c.txn_id, c.user_id, c.balance_after AS stored, c.amount,
	       u.balance - c.after_sum AS expected,
	       (u.balance - c.after_sum) - c.balance_after AS drift
	  FROM c JOIN users u ON u.id = c.user_id`

// ChainDiff 是一行需要校正的余额快照。
type ChainDiff struct {
	UserID   uint64 `json:"user_id"`
	TxnID    uint64 `json:"txn_id"`
	Amount   int64  `json:"amount"`
	Stored   int64  `json:"stored"`
	Expected int64  `json:"expected"`
	Drift    int64  `json:"drift"`
}

// ChainAlignReport 是一次链校正的结果。
type ChainAlignReport struct {
	DryRun    bool        `json:"dry_run"`
	BadRows   int64       `json:"bad_rows"`
	FixedRows int64       `json:"fixed_rows"`
	MaxDrift  int64       `json:"max_drift"`
	Users     int64       `json:"users"`
	Skipped   []string    `json:"skipped,omitempty"`
	Samples   []ChainDiff `json:"samples"`
}

type userDrift struct {
	UserID    uint64
	BadRows   int64
	TotalRows int64
	MaxDrift  int64
}

// driftTolerance 是对齐时允许的偏移上限（微）。
//
// 依据：每行两列各舍入一次，误差 ≤1 微，累积到某一行最多也就是「这个用户有多少行」。
// 超过这个量级说明不是舍入，而是真的有账漏了（唯一索引失效导致的重复入账、
// 人工改库、丢失流水…）—— 那种情况必须人工查，绝不能被一次「自动对齐」抹平。
func driftTolerance(totalRows int64) int64 {
	if totalRows < 0 {
		totalRows = 0
	}
	return totalRows + 2
}

// AlignLedgerChain 校正所有用户的余额链。dryRun 时只统计不改写。
func AlignLedgerChain(gdb *gorm.DB, dryRun bool) (ChainAlignReport, error) {
	rep := ChainAlignReport{DryRun: dryRun, Samples: []ChainDiff{}}

	var per []userDrift
	if err := gdb.Raw(`
		WITH d AS (` + chainRowsSQL + `)
		SELECT user_id,
		       count(*) FILTER (WHERE drift <> 0) AS bad_rows,
		       count(*)                            AS total_rows,
		       COALESCE(max(abs(drift)) FILTER (WHERE drift <> 0), 0) AS max_drift
		  FROM d GROUP BY user_id
		 HAVING count(*) FILTER (WHERE drift <> 0) > 0
		 ORDER BY user_id`).Scan(&per).Error; err != nil {
		return rep, fmt.Errorf("db: 统计余额链差异失败: %w", err)
	}
	if len(per) == 0 {
		return rep, nil
	}

	// 超出舍入量级的用户一律跳过：宁可让它继续报警，也不能用「对齐」掩盖真实资金差异。
	fixable := make([]uint64, 0, len(per))
	for _, p := range per {
		if p.MaxDrift > driftTolerance(p.TotalRows) {
			rep.Skipped = append(rep.Skipped, fmt.Sprintf(
				"用户 %d：最大偏移 %d 微 > 容差 %d 微（共 %d 行，%d 行不自洽）——疑似真实账务差异，需人工核查",
				p.UserID, p.MaxDrift, driftTolerance(p.TotalRows), p.TotalRows, p.BadRows))
			continue
		}
		fixable = append(fixable, p.UserID)
		rep.BadRows += p.BadRows
		rep.Users++
		if p.MaxDrift > rep.MaxDrift {
			rep.MaxDrift = p.MaxDrift
		}
	}

	if len(fixable) > 0 {
		samples := make([]ChainDiff, 0, sampleRows)
		if err := gdb.Raw(`
			WITH d AS (`+chainRowsSQL+`)
			SELECT user_id, txn_id, amount, stored, expected, drift
			  FROM d
			 WHERE drift <> 0 AND user_id IN ?
			 ORDER BY user_id, txn_id LIMIT ?`, fixable, sampleRows).Scan(&samples).Error; err != nil {
			return rep, fmt.Errorf("db: 读取余额链差异样本失败: %w", err)
		}
		rep.Samples = samples
	}

	if dryRun || rep.BadRows == 0 {
		return rep, nil
	}

	err := gdb.Transaction(func(tx *gorm.DB) error {
		res := tx.Exec(`
			WITH d AS (`+chainRowsSQL+`)
			UPDATE transactions t
			   SET balance_after = d.expected
			  FROM d
			 WHERE t.id = d.txn_id AND d.drift <> 0 AND d.user_id IN ?`, fixable)
		if res.Error != nil {
			return fmt.Errorf("db: 校正余额链失败: %w", res.Error)
		}
		rep.FixedRows = res.RowsAffected
		return nil
	})
	return rep, err
}

// sampleRows 是报告里保留的样本条数（够定位问题，又不会把响应撑爆）。
const sampleRows = 50

// chargeDiffSQL 选出「已终态用量的 charge 与流水净额不一致」的记录。
//
// JOIN（不是 LEFT JOIN）是刻意的：**没有流水的用量一律不在此列**。
// 那种行是「算出了应收金额但一分钱没扣到」（唯一索引失效 / 结算回滚…），
// 属于真实资金损失，必须靠对账报警，绝不能被「对齐到流水」抹成 0。
const chargeDiffSQL = `
	WITH l AS (
	  SELECT ref_id::bigint AS usage_id, -sum(amount) AS ledger
	    FROM transactions WHERE ref_type = 'usage' GROUP BY 1)
	SELECT u.id AS usage_id, u.user_id, u.charge AS stored, l.ledger AS ledger
	  FROM usages u JOIN l ON l.usage_id = u.id
	 WHERE u.status <> 'pending' AND l.ledger <> u.charge AND abs(l.ledger - u.charge) <= ?`

// ChargeDiff 是一行需要校正的用量金额。
type ChargeDiff struct {
	UsageID uint64 `json:"usage_id"`
	UserID  uint64 `json:"user_id"`
	Stored  int64  `json:"stored"`
	Ledger  int64  `json:"ledger"`
}

// ChargeAlignReport 是一次用量金额校正的结果。
type ChargeAlignReport struct {
	DryRun    bool         `json:"dry_run"`
	BadRows   int64        `json:"bad_rows"`
	FixedRows int64        `json:"fixed_rows"`
	Tolerance int64        `json:"tolerance"`
	Samples   []ChargeDiff `json:"samples"`
}

// AlignUsageCharge 把「因单位换算各自舍入」而与流水差 1 微的 usages.charge 对齐到流水净额。
//
// 为什么以流水为准：流水是**实际从余额里扣掉的钱**（用户真实付出的金额），
// usages.charge 是「应收多少」。两者相差 1 微时，把应收改成实收既不动任何人的钱，
// 又让账单与流水自洽；反过来（按 charge 改流水并补扣）等于替用户决定多花 1 微。
//
// 容差默认 2 微：只够吸收「一次舍入」，再大就不是舍入了。
func AlignUsageCharge(gdb *gorm.DB, dryRun bool, tolerance int64) (ChargeAlignReport, error) {
	if tolerance <= 0 {
		tolerance = 2
	}
	rep := ChargeAlignReport{DryRun: dryRun, Tolerance: tolerance, Samples: []ChargeDiff{}}

	samples := make([]ChargeDiff, 0, sampleRows)
	if err := gdb.Raw(chargeDiffSQL, tolerance).Scan(&samples).Error; err != nil {
		return rep, fmt.Errorf("db: 统计用量金额差异失败: %w", err)
	}
	rep.Samples = samples
	rep.BadRows = int64(len(samples))
	if dryRun || rep.BadRows == 0 {
		return rep, nil
	}

	err := gdb.Transaction(func(tx *gorm.DB) error {
		// 子查询里再判一次同样条件：统计与改写之间可能有人落了新账。
		res := tx.Exec(`
			WITH l AS (
			  SELECT ref_id::bigint AS usage_id, -sum(amount) AS ledger
			    FROM transactions WHERE ref_type = 'usage' GROUP BY 1)
			UPDATE usages u
			   SET charge = l.ledger
			  FROM l
			 WHERE l.usage_id = u.id
			   AND u.status <> 'pending'
			   AND l.ledger <> u.charge
			   AND abs(l.ledger - u.charge) <= ?`, tolerance)
		if res.Error != nil {
			return fmt.Errorf("db: 校正用量金额失败: %w", res.Error)
		}
		rep.FixedRows = res.RowsAffected
		return nil
	})
	return rep, err
}

// FormatSkipped 把跳过原因拼成一行（供命令行输出）。
func FormatSkipped(list []string) string {
	if len(list) == 0 {
		return ""
	}
	return strings.Join(list, "；")
}
