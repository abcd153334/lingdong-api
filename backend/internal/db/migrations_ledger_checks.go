package db

import (
	"gorm.io/gorm"

	"lingdong-api/internal/pkg/logger"

	"go.uber.org/zap"
)

// ledgerCheck 是一条资金列的非负约束。
//
// 为什么要加：账务正确性此前**完全**靠应用层的行锁与三条部分唯一索引，
// 数据库对 `frozen < 0` / `charge < 0` 这种"物理上不可能"的值没有任何约束。
// 一次人工改库、一次迁移脚本写错、一次代码把 -1 当成"未知"写进列，都只能靠
// 24 小时一轮的只读对账事后发现 —— 而那时钱已经动了。
//
// 为什么只加这几列：`users.balance` 与 `transactions.amount / balance_after`
// **刻意允许为负**（`CreditInput.AllowNegative` 用于上游已退款、本地补扣的场景），
// 给它们加非负约束会把一条合法路径直接堵死。
type ledgerCheck struct {
	table  string
	name   string
	expr   string
	reason string
}

var ledgerChecks = []ledgerCheck{
	{"users", "ck_users_frozen_nonneg", "frozen >= 0", "用户冻结额是「还欠着多少」的账面值，负数意味着凭空多冻了钱"},
	{"usages", "ck_usages_frozen_nonneg", "frozen >= 0", "会话冻结额是资金流的来源，负数只能来自写坏"},
	{"usages", "ck_usages_charge_nonneg", "charge >= 0", "应收额恒非负；负数会被当成退款冲正"},
	{"usages", "ck_usages_charge_before_cap_nonneg", "charge_before_cap >= 0", "截顶前的金额同样恒非负"},
	{"transactions", "ck_txn_frozen_after_nonneg", "frozen_after >= 0", "流水的冻结快照恒非负"},
	{"orders", "ck_orders_amount_cent_nonneg", "amount_cent >= 0", "订单金额恒非负（退款走独立流水）"},
	{"orders", "ck_orders_credited_nonneg", "credited >= 0", "已入账额度恒非负"},
	{"orders", "ck_orders_refund_credited_nonneg", "refund_credited >= 0", "已退额度恒非负"},
}

// EnsureLedgerCheckConstraints 给资金列补上非负约束（幂等，可回滚）。
//
// 两步走，顺序不可变：
//  1. `ADD CONSTRAINT ... CHECK (...) NOT VALID` —— 立刻开始拦**新写入**，但不扫存量；
//  2. `VALIDATE CONSTRAINT` —— 校验存量，期间只持 SHARE UPDATE EXCLUSIVE（不阻写）。
//
// 为什么要拆两步：直接 ADD + 校验会在存量有脏数据时失败，而"启动失败"与"少一道守卫"
// 之间必须选后者 —— 这条约束是防线，不是业务前提。VALIDATE 失败只告警并保持 NOT VALID，
// 下一次启动会再试（脏数据通常由对账/人工修正后再自然通过）。
//
// 回滚（任何一步出问题都可以单独执行，不影响数据）：
//
//	ALTER TABLE users        DROP CONSTRAINT IF EXISTS ck_users_frozen_nonneg;
//	ALTER TABLE usages       DROP CONSTRAINT IF EXISTS ck_usages_frozen_nonneg;
//	ALTER TABLE usages       DROP CONSTRAINT IF EXISTS ck_usages_charge_nonneg;
//	ALTER TABLE usages       DROP CONSTRAINT IF EXISTS ck_usages_charge_before_cap_nonneg;
//	ALTER TABLE transactions DROP CONSTRAINT IF EXISTS ck_txn_frozen_after_nonneg;
//	ALTER TABLE orders       DROP CONSTRAINT IF EXISTS ck_orders_amount_cent_nonneg;
//	ALTER TABLE orders       DROP CONSTRAINT IF EXISTS ck_orders_credited_nonneg;
//	ALTER TABLE orders       DROP CONSTRAINT IF EXISTS ck_orders_refund_credited_nonneg;
func EnsureLedgerCheckConstraints(gdb *gorm.DB) {
	for _, c := range ledgerChecks {
		var row struct {
			Convalidated bool
		}
		err := gdb.Raw(`SELECT convalidated FROM pg_constraint WHERE conname = ?`, c.name).Scan(&row).Error
		if err != nil {
			logger.L().Warn("账务约束检查失败（本次跳过）",
				zap.String("constraint", c.name), zap.Error(err))
			continue
		}
		if row.Convalidated {
			continue // 已存在且已校验：无事可做
		}
		// 不存在时 Scan 出零值（false），与"存在但未校验"同分支：交给下面的 ADD 判定。
		var exists bool
		if err := gdb.Raw(`SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conname = ?)`, c.name).
			Scan(&exists).Error; err != nil {
			continue
		}
		if !exists {
			stmt := "ALTER TABLE " + c.table + " ADD CONSTRAINT " + c.name + " CHECK (" + c.expr + ") NOT VALID"
			if err := gdb.Exec(stmt).Error; err != nil {
				// 表不存在（灰度库/裁剪部署）或权限不足：告警，不阻断启动。
				logger.L().Warn("账务约束添加失败（本次跳过）",
					zap.String("constraint", c.name), zap.String("reason", c.reason), zap.Error(err))
				continue
			}
		}
		if err := gdb.Exec("ALTER TABLE " + c.table + " VALIDATE CONSTRAINT " + c.name).Error; err != nil {
			// 存量有脏数据：约束保持 NOT VALID（新写入照样被拦），并留下可查的告警。
			logger.L().Warn("账务约束校验未通过（存量有脏数据，新写入已被拦截）",
				zap.Bool("billing_check_unvalidated", true),
				zap.String("constraint", c.name), zap.String("reason", c.reason), zap.Error(err))
		}
	}
}
