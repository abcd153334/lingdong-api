// implant 是破窗工具：在**没有 HTTP 后门**的前提下，处理「管理员把自己锁在门外」。
//
// 为什么必须有它：二次验证绑定在密钥丢失、换手机、误开「强制管理员 2FA」这三种情况下
// 都会让管理台彻底进不去。常规做法是加一个「重置 2FA」的接口，但那等于给整条
// 认证链留一个可以绕过第二因素的入口 —— 只要那个接口的鉴权写错一次，2FA 就白做了。
// 所以这里做成**只能在服务器上执行**的离线命令：能登服务器的人本来就能改库。
//
// 用法：
//
//	go run ./cmd/implant -dsn "$DATABASE_URL" reset-mfa -username alice
//	go run ./cmd/implant -dsn "$DATABASE_URL" list-mfa
//
// 注意：本工具不写审计流水（它跑在库外，也没有操作者身份）。执行后会往 stderr
// 打一条 ERROR 级日志，运维应在变更单里记录这次操作。
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

func main() {
	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"), "PostgreSQL 连接串（默认读 DATABASE_URL）")
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	logger.Init("info")
	defer logger.Sync()

	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "缺少 -dsn 或 DATABASE_URL")
		os.Exit(2)
	}
	gdb, err := db.Init(*dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "连接数据库失败:", err)
		os.Exit(1)
	}

	switch args[0] {
	case "reset-mfa":
		username := ""
		if len(args) > 1 && args[1] == "-username" && len(args) > 2 {
			username = strings.TrimSpace(args[2])
		}
		if username == "" {
			fmt.Fprintln(os.Stderr, "用法: implant reset-mfa -username <用户名>")
			os.Exit(2)
		}
		resetMFA(gdb, username)
	case "list-mfa":
		listMFA(gdb)
	case "fix-image-default":
		apply := false
		for _, a := range args[1:] {
			if a == "-apply" {
				apply = true
			}
		}
		fixImageDefault(gdb, apply)
	case "fix-ledger":
		apply := false
		for _, a := range args[1:] {
			if a == "-apply" {
				apply = true
			}
		}
		fixLedger(gdb, apply)
	default:
		usage()
		os.Exit(2)
	}
}

// resetMFA 删除某用户的 TOTP 密钥与全部备份码。
//
// 只删这两张表的行，**不动**会话：管理员重置完可以立刻用原密码登录，
// 再走绑定流程。若要一并踢下线，用管理台的「重置密码」（那条路径会撤销会话）。
func resetMFA(gdb *gorm.DB, username string) {
	var u model.User
	if err := gdb.Where("username = ?", username).First(&u).Error; err != nil {
		fmt.Fprintf(os.Stderr, "找不到用户 %s: %v\n", username, err)
		os.Exit(1)
	}
	err := gdb.Transaction(func(tx *gorm.DB) error {
		// 先删备份码再删密钥：反过来的话中途失败会留下「有码无密钥」的孤儿行。
		if err := tx.Where("uid = ?", u.ID).Delete(&model.UserMFABackupCode{}).Error; err != nil {
			return err
		}
		return tx.Where("uid = ?", u.ID).Delete(&model.UserMFA{}).Error
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "重置失败（已回滚）:", err)
		os.Exit(1)
	}
	// ERROR 级而不是 INFO：这是「绕过第二因素」的操作，必须是日志里显眼的一行。
	logger.L().Error("已通过 implant 重置用户二次验证（破窗操作）",
		zap.Uint64("uid", u.ID), zap.String("username", u.Username))
	fmt.Printf("已重置 %s（uid=%d）的二次验证：密钥与备份码均已删除，请尽快重新绑定。\n", u.Username, u.ID)
}

// listMFA 打印「谁是管理员 + 有没有开二次验证」，用于判断强制策略会不会锁死。
func listMFA(gdb *gorm.DB) {
	type row struct {
		ID      uint64
		Usernme string
		Role    int16
		Status  int16
		Enabled bool
	}
	var rows []row
	err := gdb.Raw(`
		SELECT u.id, u.username AS usernme, u.role, u.status,
		       COALESCE(m.enabled, false) AS enabled
		FROM users u
		LEFT JOIN user_mfa m ON m.uid = u.id
		WHERE u.deleted_at IS NULL
		ORDER BY u.role, u.id`).Scan(&rows).Error
	if err != nil {
		fmt.Fprintln(os.Stderr, "查询失败:", err)
		os.Exit(1)
	}
	bound := 0
	for _, r := range rows {
		role := "普通用户"
		if r.Role == 0 {
			role = "管理员"
		}
		state := "未绑定"
		if r.Enabled {
			state = "已绑定"
			if r.Role == 0 && r.Status == 1 {
				bound++
			}
		}
		fmt.Printf("#%-4d %-24s %-8s %s\n", r.ID, r.Usernme, role, state)
	}
	fmt.Printf("\n在任且已绑定的管理员：%d\n", bound)
	if bound == 0 {
		fmt.Println("警告：当前没有任何已绑定的管理员；此时把 auth_mfa.policy 设为 required_for_admin 会让管理台对所有人 403。")
	}
}

// fixLedger 校正账务链与用量金额（默认 dry-run）。
//
// 为什么做成离线命令而不是管理台按钮：它会改写历史流水。管理台按钮意味着
// 「任何拿到管理员会话的人都能重写账本」，而这件事的正确触发时机是「版本升级后
// 发现对账报舍入残差」，操作者本来就能登录服务器。
//
// 安全性由 `db.AlignLedgerChain` 内置的两条闸保证：
//  1. 只改 balance_after 这一列快照，不动余额、不动 amount；
//  2. 偏移超过舍入量级（行数 + 2 微）的用户整体跳过 —— 那是真账务差异，得人工查。
func fixLedger(gdb *gorm.DB, apply bool) {
	chain, err := db.AlignLedgerChain(gdb, !apply)
	if err != nil {
		fmt.Fprintln(os.Stderr, "余额链校正失败:", err)
		os.Exit(1)
	}
	charge, err := db.AlignUsageCharge(gdb, !apply, 2)
	if err != nil {
		fmt.Fprintln(os.Stderr, "用量金额校正失败:", err)
		os.Exit(1)
	}

	mode := "dry-run（不写库）"
	if apply {
		mode = "已落库"
	}
	fmt.Printf("== 账务链校正 %s ==\n", mode)
	fmt.Printf("余额链：%d 个用户、%d 行不自洽，最大偏移 %d 微；本次改写 %d 行\n",
		chain.Users, chain.BadRows, chain.MaxDrift, chain.FixedRows)
	for i, s := range chain.Samples {
		if i >= 10 {
			fmt.Printf("  ...（其余 %d 条见管理台对账接口）\n", len(chain.Samples)-10)
			break
		}
		fmt.Printf("  用户 %-4d 流水 #%-6d 记录 %d → 自洽值 %d（差 %+d）\n",
			s.UserID, s.TxnID, s.Stored, s.Expected, s.Drift)
	}
	for _, w := range chain.Skipped {
		fmt.Println("  跳过：" + w)
	}
	fmt.Printf("用量金额：%d 行与流水差 ≤%d 微，本次改写 %d 行\n",
		charge.BadRows, charge.Tolerance, charge.FixedRows)
	for i, s := range charge.Samples {
		if i >= 10 {
			break
		}
		fmt.Printf("  用量 #%-6d 用户 %-4d charge %d → 流水净额 %d\n",
			s.UsageID, s.UserID, s.Stored, s.Ledger)
	}

	if !apply {
		if chain.BadRows > 0 || charge.BadRows > 0 {
			fmt.Println("\n以上为预演结果；确认无误后加 -apply 落库：")
			fmt.Println("  implant -dsn <DSN> fix-ledger -apply")
		}
		return
	}
	// 改写后立刻复查一次：让操作者一眼看到「现在还剩多少」，而不是靠自己再跑一遍。
	verify, err := db.AlignLedgerChain(gdb, true)
	if err != nil {
		fmt.Fprintln(os.Stderr, "复查失败:", err)
		os.Exit(1)
	}
	verifyCharge, err := db.AlignUsageCharge(gdb, true, 2)
	if err != nil {
		fmt.Fprintln(os.Stderr, "复查失败:", err)
		os.Exit(1)
	}
	fmt.Printf("\n复查：余额链剩余不自洽 %d 行（跳过 %d 个用户），用量金额剩余 %d 行\n",
		verify.BadRows, len(verify.Skipped), verifyCharge.BadRows)
	if verify.BadRows > 0 || verifyCharge.BadRows > 0 {
		fmt.Println("仍有余量差异：请按上面的「跳过」原因人工核查，不要反复执行本命令。")
	}
	logger.L().Error("已通过 implant 校正账务链（历史流水快照被改写）",
		zap.Int64("chain_fixed", chain.FixedRows),
		zap.Int64("charge_fixed", charge.FixedRows),
		zap.Int64("chain_remaining", verify.BadRows))
}

// fixImageDefault 给图片模型补齐「默认档」（spec_prices.resolution_fixed）。
//
// 为什么需要它：默认档是「不传 size / auto」时的计费档。运行期**不再有兜底**——
// 没配默认档时，这类请求会被拒绝（ErrImageDefaultTierUnset），因为替用户挑一档
// 等于替运营定价。这条命令把缺失/失效的默认档补成**最低价档**（**不改任何档位单价**：
// 那是定价决策），适合「还没想清楚各档定位、先让缺省请求能跑」的场景。
//
// 幂等：默认档已经命中价目的模型直接跳过。
func fixImageDefault(gdb *gorm.DB, apply bool) {
	type row struct {
		ID         string
		SpecPrices []byte
	}
	var rows []row
	if err := gdb.Raw(`SELECT id, spec_prices FROM models WHERE modality = 'image' ORDER BY id`).
		Scan(&rows).Error; err != nil {
		fmt.Fprintln(os.Stderr, "读取图片模型失败:", err)
		os.Exit(1)
	}
	mode := "dry-run（不写库）"
	if apply {
		mode = "已落库"
	}
	fmt.Printf("== 图片默认档补齐 %s ==\n", mode)
	fixed := 0
	for _, r := range rows {
		sp, err := model.DecodeSpecPrices(r.SpecPrices)
		if err != nil {
			fmt.Printf("  跳过 %s：价目无法解码（%v）\n", r.ID, err)
			continue
		}
		if len(sp.TierPrice) == 0 {
			continue
		}
		if t := strings.TrimSpace(sp.ResolutionFixed); t != "" {
			if _, ok := sp.TierPrice[strings.ToUpper(t)]; ok {
				continue
			}
		}
		lowest, lowUnit := "", int64(0)
		for k, v := range sp.TierPrice {
			if lowest == "" || v < lowUnit {
				lowest, lowUnit = k, v
			}
		}
		if lowest == "" {
			continue
		}
		fmt.Printf("  %-40s 默认档 %q → %s\n", r.ID, sp.ResolutionFixed, lowest)
		fixed++
		if !apply {
			continue
		}
		if err := gdb.Exec(`UPDATE models SET spec_prices = jsonb_set(spec_prices, '{resolution_fixed}', to_jsonb(?::text)) WHERE id = ?`,
			lowest, r.ID).Error; err != nil {
			fmt.Fprintf(os.Stderr, "  写库失败 %s: %v\n", r.ID, err)
		}
	}
	fmt.Printf("共 %d 个模型需要补默认档\n", fixed)
}

func usage() {
	fmt.Fprint(os.Stderr, `implant —— 认证破窗工具

用法:
  implant -dsn <DSN> reset-mfa -username <用户名>   删除该用户的 TOTP 密钥与备份码
  implant -dsn <DSN> list-mfa                     列出用户的二次验证绑定情况
  implant -dsn <DSN> fix-ledger [-apply]          校正账务链（默认 dry-run，不写库）
  implant -dsn <DSN> fix-image-default [-apply]   给图片模型补齐默认档（取最低价档）

DSN 默认读环境变量 DATABASE_URL。
`)
}
