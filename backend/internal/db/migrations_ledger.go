package db

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/money"
)

// MigrationNameLedgerUnits 是「存量账务数据搬迁到微」的数据动作标识。
//
// 为什么必须有这一条：M28 只迁了 **models 的价目**，没迁 **钱本身**。
// `users.balance` / `transactions.amount` / `usages.charge` / `orders.credited`
// 这些都是整改时新加的列（AutoMigrate 建列，默认 0），旧列
// （`quota` / `amount_quota` / `charge_quota` / `quota_credited`）里的数据还在原地。
//
// 症状比"余额显示成 0"严重得多：
//   - 存量用户的余额、累计消费、推广佣金全部归零 —— 用户的钱凭空消失；
//   - 历史账单与营收统计归零 —— 对账时"平台赚了多少"查不出来；
//   - 冻结额归零 —— 在途任务的冻结记录与账实不符。
//
// 换算锚（与 M28 完全相同，只在这一处出现）：`微 = 额度 ÷ legacyQuotaPerUSD × 汇率 × 1e6`。
// 旧刻度 1 CNY = 69444 额度 ≈ 500000 / 7.2，两种表达同源，所以汇率用当前配置即可。
const MigrationNameLedgerUnits = "m30_ledger_units"

// ledgerPair 是一对「旧额度列 → 新微列」。
//
// 判据统一是「新列为 0 且旧列非 0」：旧列非 0 说明这行是搬迁前落库的，
// 新列非 0 说明已经是新口径（或人工补过），两者都不该被覆盖。
type ledgerPair struct {
	Table string
	Old   string
	New   string
	// AlsoConvert 是同表里「没改名的旧金额列」，随同一行一起换算。
	//
	// 为什么必须挤在同一条 UPDATE 里：这些列（`balance_after` / `frozen_after`）
	// 一直有旧值，无法用「新列 = 0」判断"是否迁过"。唯一可靠的锚是同表那次
	// 改名搬迁（`amount = 0` = 还没迁）。拆成两条语句就会出现
	// 「金额搬完了、balance_after 还停在旧额度」——而且重跑永远修不回来。
	AlsoConvert []string
}

// ledgerPairs 是全部需要搬迁的金额列。
//
// 只列**真实存在于库里**的旧列（用 HasColumn 过滤），漏一个的症状是
// 「某张表的某个数字永远是 0」，而且不会报错。
var ledgerPairs = []ledgerPair{
	{"users", "quota", "balance", nil},
	{"users", "used_quota", "used", nil},
	{"users", "frozen_quota", "frozen", nil},
	// 邀请返利的额度已改为「由 affiliate_rewards 行派生」，users 上不再有
	// aff_balance / aff_history —— 因此也不再有 aff_quota / aff_history_quota
	// 的搬迁目标。两条声明已删除，留着会指向一个不存在的列。
	// 注意：`users.quota_upper_limit → balance_limit` **不在**这张表里。
	// 旧代码给每个注册用户都写了一份全局默认值（500000000），照搬会把
	// 「跟随全局」变成「永久锁定旧上限」——见 userBalanceLimitSQL。
	{"api_keys", "quota_limit", "amount_limit", nil},
	{"api_keys", "used_quota", "used", nil},
	{"transactions", "amount_quota", "amount", []string{"balance_after", "frozen_after"}},
	{"usages", "charge_quota", "charge", nil},
	{"usages", "cost_quota", "cost", nil},
	{"usages", "frozen_quota", "frozen", nil},
	{"usages", "tier_unit_price_quota", "tier_price", nil},
	{"usages_archive", "charge_quota", "charge", nil},
	{"usages_archive", "cost_quota", "cost", nil},
	{"usages_archive", "frozen_quota", "frozen", nil},
	{"usages_archive", "tier_unit_price_quota", "tier_price", nil},
	{"usage_daily", "charge_quota", "charge", nil},
	{"usage_daily", "cost_quota", "cost", nil},
	{"orders", "quota_credited", "credited", nil},
	{"redeem_codes", "quota", "amount", nil},
	{"tasks", "frozen_quota", "frozen", nil},
	{"tasks", "settled_quota", "settled", nil},
}

// LedgerRow 是单张表的搬迁结果。
type LedgerRow struct {
	Table  string `json:"table"`
	Column string `json:"column"`
	From   string `json:"from"`
	Rows   int64  `json:"rows"`
}

// LedgerReport 是一次账务搬迁的完整结果。
type LedgerReport struct {
	Skipped  bool        `json:"skipped"`
	DryRun   bool        `json:"dry_run"`
	Fx       float64     `json:"fx"`
	Rows     []LedgerRow `json:"rows"`
	Warnings []string    `json:"warnings,omitempty"`
	// Chain / Charge 是搬迁后自动做的「舍入残差」校正（见 ledger_align.go）。
	//
	// 逐列 round 一定会让 amount 与 balance_after 差 1 微。不做这一步，
	// 对账的链式判据会在每次启动时把这些行报成差异 —— 一条永不消失的 ERROR。
	Chain  ChainAlignReport  `json:"chain"`
	Charge ChargeAlignReport `json:"charge"`
}

// MigrateLedgerUnits 把存量账务从「额度」搬到「微」，幂等。
//
// 幂等策略（两重）：
//  1. data_migrations 标记；
//  2. 逐列判据「新列为 0 且旧列非 0」—— 重跑不会二次放大。
func MigrateLedgerUnits(gdb *gorm.DB, dryRun bool) (LedgerReport, error) {
	rep := LedgerReport{DryRun: dryRun}
	if !gdb.Migrator().HasTable(&model.User{}) {
		rep.Skipped = true
		return rep, nil
	}
	// 全新库（users 没有 quota 列）= 没有需要搬迁的存量。
	if !gdb.Migrator().HasColumn(&model.User{}, "quota") {
		rep.Skipped = true
		return rep, nil
	}
	if !dryRun {
		var n int64
		if err := gdb.Model(&model.DataMigration{}).
			Where("name = ?", MigrationNameLedgerUnits).Count(&n).Error; err != nil {
			return rep, fmt.Errorf("db: 查询迁移标记失败: %w", err)
		}
		if n > 0 {
			rep.Skipped = true
			return rep, nil
		}
	}

	fx, err := readCNYPerUSD(gdb)
	if err != nil {
		return rep, err
	}
	// 美元站的额度本身就是美元口径（1 额度 = 1/500000 美元），不该再乘人民币汇率。
	//
	// 币种**从库里读**，不能用 conf.SiteCurrency()：RunMigrations 跑在
	// LoadSystemConfig 之前，那个函数此刻只会返回默认 CNY —— 美元站会因此
	// 把余额放大 7.2 倍，而且没有任何报错。
	if readSiteCurrency(gdb) == conf.CurrencyUSD {
		fx = 1
	}
	rep.Fx = fx

	skip := func(m gorm.Migrator, p ledgerPair) bool {
		if !m.HasTable(p.Table) || !m.HasColumn(p.Table, p.New) || !m.HasColumn(p.Table, p.Old) {
			return true
		}
		for _, c := range p.AlsoConvert {
			if !m.HasColumn(p.Table, c) {
				return true
			}
		}
		return false
	}

	// 用户级「余额上限」单独处理：旧代码把全局默认值写进了每一行，
	// 照搬会把「跟随全局」变成「永久锁定旧上限」（见 userBalanceLimitSQL）。
	legacyDefault := legacyGlobalUpperLimit(gdb)
	if !gdb.Migrator().HasColumn(&model.User{}, "quota_upper_limit") {
		legacyDefault = -1 // 没有旧列 → 整段跳过
	}

	if !dryRun {
		err = gdb.Transaction(func(tx *gorm.DB) error {
			for _, p := range ledgerPairs {
				if skip(tx.Migrator(), p) {
					continue
				}
				// 一条 UPDATE 同时写「改名列」与「没改名的旧列」：
				// 后者的判据只能挂在改名列上（见 AlsoConvert 的注释）。
				set := []string{p.New + " = " + ledgerExpr(p.Old)}
				args := []any{fx}
				for _, c := range p.AlsoConvert {
					set = append(set, fmt.Sprintf(
						"%s = CASE WHEN %s <> 0 THEN %s ELSE %s END", c, c, ledgerExpr(c), c))
					args = append(args, fx)
				}
				res := tx.Exec(fmt.Sprintf("UPDATE %s SET %s WHERE %s",
					p.Table, strings.Join(set, ", "), ledgerWhere(p)), args...)
				if res.Error != nil {
					return fmt.Errorf("db: 搬迁 %s.%s 失败: %w", p.Table, p.New, res.Error)
				}
				if res.RowsAffected > 0 {
					rep.Rows = append(rep.Rows, LedgerRow{
						Table: p.Table, Column: p.New, From: p.Old, Rows: res.RowsAffected,
					})
				}
			}
			if legacyDefault >= 0 {
				expr, where, args := userBalanceLimitSQL(legacyDefault, fx)
				res := tx.Exec("UPDATE users SET balance_limit = "+expr+" WHERE "+where, args...)
				if res.Error != nil {
					return fmt.Errorf("db: 搬迁 users.balance_limit 失败: %w", res.Error)
				}
				if res.RowsAffected > 0 {
					rep.Rows = append(rep.Rows, LedgerRow{
						Table: "users", Column: "balance_limit",
						From: "quota_upper_limit", Rows: res.RowsAffected,
					})
					rep.Warnings = append(rep.Warnings,
						"只有「不是旧全局默认值」的用户级上限被搬过来了；其余用户回到「跟随全局 balance_limit」")
				}
			}
			// 换算完必须立刻校正「链」与「用量金额」：逐列 round 的残差会让
			// balance_after 链每行差 1 微，对账从此天天报差异（而那是舍入，不是账错）。
			// 放在同一个事务里：搬迁成功但校正失败 = 留下必然报警的脏链。
			if c, err := AlignLedgerChain(tx, false); err != nil {
				return err
			} else {
				rep.Chain = c
			}
			if c, err := AlignUsageCharge(tx, false, 2); err != nil {
				return err
			} else {
				rep.Charge = c
			}
			return tx.Create(&model.DataMigration{
				Name: MigrationNameLedgerUnits, AppliedAt: time.Now(),
			}).Error
		})
		if err == nil {
			if rep.Chain.FixedRows > 0 {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf(
					"已校正 %d 条流水的 balance_after（逐列取整造成的 1 微漂移，未改动任何余额与扣费金额）",
					rep.Chain.FixedRows))
			}
			if len(rep.Chain.Skipped) > 0 {
				rep.Warnings = append(rep.Warnings, FormatSkipped(rep.Chain.Skipped))
			}
			if rep.Charge.FixedRows > 0 {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf(
					"已把 %d 条用量的 charge 对齐到流水净额（同样只是 1 微的舍入差）",
					rep.Charge.FixedRows))
			}
		}
		return rep, err
	}

	// dry-run：只统计会影响多少行，不写库。
	for _, p := range ledgerPairs {
		if skip(gdb.Migrator(), p) {
			continue
		}
		var n int64
		q := fmt.Sprintf("SELECT count(*) FROM %s WHERE %s", p.Table, ledgerWhere(p))
		if err := gdb.Raw(q).Scan(&n).Error; err != nil {
			return rep, fmt.Errorf("db: 统计 %s.%s 失败: %w", p.Table, p.New, err)
		}
		if n > 0 {
			from := p.Old
			if from == "" {
				from = p.New
			}
			rep.Rows = append(rep.Rows, LedgerRow{Table: p.Table, Column: p.New, From: from, Rows: n})
		}
	}
	if legacyDefault >= 0 {
		// where 里没有 fx 占位符（它在 SET 表达式里），所以只传 args[1:]。
		_, where, args := userBalanceLimitSQL(legacyDefault, fx)
		var n int64
		if err := gdb.Raw("SELECT count(*) FROM users WHERE "+where, args[1:]...).Scan(&n).Error; err != nil {
			return rep, fmt.Errorf("db: 统计 users.balance_limit 失败: %w", err)
		}
		if n > 0 {
			rep.Rows = append(rep.Rows, LedgerRow{
				Table: "users", Column: "balance_limit", From: "quota_upper_limit", Rows: n,
			})
		}
	}
	if len(rep.Rows) > 0 {
		rep.Warnings = append(rep.Warnings,
			"逐行四舍五入会造成 1–2 微的汇总差（usages 之和 vs users.used），属预期；若差更多说明有行被漏迁",
			"搬迁后请抽查一条历史流水：amount / balance_after / frozen_after 必须仍与余额变动自洽")
	}
	return rep, nil
}

// userBalanceLimitSQL 生成「用户级余额上限」的搬迁语句与判据。
//
// 为什么不能像其它列那样无条件照搬：旧代码在注册时把**全局默认值**
// （`system_config.quota_upper_limit`，典型 500000000）写进了每个用户行。
// 无条件搬迁会让「跟随全局」变成「永久锁定旧上限」—— 运营以后调低全局上限，
// 这些用户仍然按旧值放行，且界面上看不出来（行里有值 = 有覆盖）。
// 所以只搬「与旧全局默认值不同」的行，那才是运营真正设过的个体覆盖。
//
// 返回 (set 表达式, where 判据, 参数)：SET 里的 `?` 在 WHERE 之前，参数顺序也随之。
func userBalanceLimitSQL(legacyDefault int64, fx float64) (expr, where string, args []any) {
	expr = fmt.Sprintf("round(quota_upper_limit::numeric * ? * 1000000 / %d)::bigint", legacyQuotaPerUSD)
	where = "balance_limit = 0 AND quota_upper_limit <> 0"
	args = []any{fx}
	if legacyDefault > 0 {
		where += " AND quota_upper_limit <> ?"
		args = append(args, legacyDefault)
	}
	return expr, where, args
}

// readSiteCurrency 从 system_config 读结算币种（`site.currency`）。
//
// 为什么不直接用 `conf.SiteCurrency()`：迁移跑在配置装载之前，那里只会返回默认值。
// 读不到/格式不对一律按 CNY 处理（旧库里没有美元站的历史数据）。
func readSiteCurrency(gdb *gorm.DB) string {
	if !gdb.Migrator().HasTable(&model.SystemConfig{}) {
		return conf.CurrencyCNY
	}
	var raw string
	if err := gdb.Raw(
		"SELECT value::text FROM system_config WHERE key = 'site' LIMIT 1",
	).Scan(&raw).Error; err != nil {
		return conf.CurrencyCNY
	}
	var site struct {
		Currency string `json:"currency"`
	}
	if err := json.Unmarshal([]byte(raw), &site); err != nil {
		return conf.CurrencyCNY
	}
	if site.Currency == conf.CurrencyUSD {
		return conf.CurrencyUSD
	}
	return conf.CurrencyCNY
}

// legacyGlobalUpperLimit 读旧的全局上限配置（额度）。读不到返回 0。
func legacyGlobalUpperLimit(gdb *gorm.DB) int64 {
	if !gdb.Migrator().HasTable(&model.SystemConfig{}) {
		return 0
	}
	var raw string
	if err := gdb.Raw(
		"SELECT value::text FROM system_config WHERE key = 'quota_upper_limit' LIMIT 1",
	).Scan(&raw).Error; err != nil {
		return 0
	}
	var v int64
	if _, err := fmt.Sscanf(strings.Trim(strings.TrimSpace(raw), `"`), "%d", &v); err != nil {
		return 0
	}
	return v
}

// ledgerExpr 是搬迁的 SQL 表达式（额度 → 微，四舍五入到 1 微）。
//
// 用 numeric 而不是 float 做中间量：float 在千万级额度上会丢精度，
// 而这里动的是「用户的余额」，差 1 微都比"看起来差不多"更该被修掉。
func ledgerExpr(src string) string {
	return fmt.Sprintf("round(%s::numeric * ? * 1000000 / %d)::bigint", src, legacyQuotaPerUSD)
}

// ledgerWhere 是「这一行确实来自旧口径，且还没迁过」的判据。
//
// `新列为 0 且旧列非 0`：
//   - 旧列非 0 → 搬迁前落库的行（新代码只写新列）；
//   - 新列非 0 → 已是新口径（或人工补过），绝不覆盖 —— 这就是「重跑不二次放大」。
//
// 同表里没改名的旧列（`balance_after`）不参与判据，它随这一行的改名搬迁一起换算。
func ledgerWhere(p ledgerPair) string {
	return fmt.Sprintf("%s = 0 AND %s <> 0", p.New, p.Old)
}

// ledgerMicro 是搬迁用的换算（供测试与报告复用）。
//
// 与 quotaToMicro 同式，但显式保留在这里，避免有人「顺手」把它改成 ×2：
// ×2 只在 1 额度 = 1/500000 美元且美元结算时成立。
func ledgerMicro(quota int64, fx float64) int64 {
	return int64(math.Round(float64(quota) * fx * float64(money.Scale) / legacyQuotaPerUSD))
}

// LogLedgerMigration 把搬迁结果写进启动日志。
func LogLedgerMigration(rep LedgerReport) {
	switch {
	case rep.Skipped, len(rep.Rows) == 0:
		return
	}
	tag := "账务单位搬迁完成"
	if rep.DryRun {
		tag = "账务单位搬迁（dry-run，未写库）"
	}
	logger.L().Info(tag, zap.Float64("fx", rep.Fx), zap.Int("columns", len(rep.Rows)))
	for _, r := range rep.Rows {
		logger.L().Info("账务列已搬迁",
			zap.String("table", r.Table), zap.String("column", r.Column),
			zap.String("from", r.From), zap.Int64("rows", r.Rows))
	}
	for _, w := range rep.Warnings {
		logger.L().Warn("账务搬迁告警", zap.String("detail", w))
	}
}
