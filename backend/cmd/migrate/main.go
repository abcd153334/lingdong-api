// 计费整改的存量数据迁移工具：价目（M28）、套餐售价（M29）、账务（M30）。
//
// 为什么单独做一个入口而不是"重启服务让它自己跑"：
// 迁移直接改钱（模型的全部价目 + 所有用户的余额与历史流水），运营必须在
// **落库前**看到逐行结果。这个工具的默认行为是 dry-run，只有显式 -apply 才写库，
// 写库前自动把这些表整体备份成 `*_backup_*`。
//
// 用法：
//
//	go run ./cmd/migrate -dsn "$DATABASE_URL"            # 只算不写，打印报告
//	go run ./cmd/migrate -dsn "$DATABASE_URL" -apply     # 备份 + 落库
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/logger"
)

func main() {
	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"), "PostgreSQL 连接串（默认读 DATABASE_URL）")
	apply := flag.Bool("apply", false, "真正写库（默认只算不写）")
	skipBackup := flag.Bool("no-backup", false, "跳过备份表（不建议：迁移不可逆）")
	flag.Parse()

	logger.Init("info")
	defer logger.Sync()

	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "缺少 -dsn 或 DATABASE_URL")
		os.Exit(2)
	}
	gdb, err := db.Init(*dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if *apply && !*skipBackup {
		stamp := time.Now().Format("20060102_150405")
		for _, src := range []string{
			"models", "packages",
			// M30 会改这些表的金额列，整表备份（含余额与流水，事后可逐行对比）。
			"users", "api_keys", "transactions", "usages", "usage_daily",
			"orders", "redeem_codes", "tasks",
		} {
			table := src + "_backup_m30_" + stamp
			if err := gdb.Exec("CREATE TABLE " + table + " AS SELECT * FROM " + src).Error; err != nil {
				fmt.Fprintf(os.Stderr, "备份 %s 失败，已中止迁移: %v\n", src, err)
				os.Exit(1)
			}
			fmt.Printf("已备份 %s → %s\n", src, table)
		}
	}

	rep, err := db.MigrateBillingUnits(gdb, !*apply)
	if err != nil {
		fmt.Fprintf(os.Stderr, "迁移失败: %v\n", err)
		os.Exit(1)
	}
	out, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Println(string(out))
	if rep.Skipped {
		fmt.Println("已迁移过（data_migrations 有标记），本次未做任何改动。")
	}

	// M29：套餐售价回填（price_cny_cent → price_cent）。
	// 与 M28 同一条命令执行：两者都是「计费单位换锚」留下的存量数据，
	// 分两次跑只会出现"模型能用了但套餐全买不了"的中间态。
	prep, err := db.MigratePackagesPrice(gdb, !*apply)
	if err != nil {
		fmt.Fprintf(os.Stderr, "套餐售价回填失败: %v\n", err)
		os.Exit(1)
	}
	pout, _ := json.MarshalIndent(prep, "", "  ")
	fmt.Println(string(pout))
	if prep.Skipped {
		fmt.Println("套餐售价已回填过（或无需回填），本次未做任何改动。")
	}

	// M30：存量账务从「额度」搬到「微」。
	// 这一步不动就上线，存量用户余额会全部显示 0（钱并没丢，但界面上看不到）。
	lrep, err := db.MigrateLedgerUnits(gdb, !*apply)
	if err != nil {
		fmt.Fprintf(os.Stderr, "账务单位搬迁失败: %v\n", err)
		os.Exit(1)
	}
	lout, _ := json.MarshalIndent(lrep, "", "  ")
	fmt.Println(string(lout))
	if lrep.Skipped {
		fmt.Println("账务单位已搬迁过（或无需搬迁），本次未做任何改动。")
	}
}
