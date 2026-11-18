// 一次性账务冲正工具：把某条计费记录多收的部分退回用户。
//
// 为什么单独做入口而不是直接写 SQL：改余额必须走 `billing.Adjust` —— 它在一个事务里
// 锁用户行、校验金额上限、写 `transactions` 流水（type=adjust + balance_after）。
// 手写 UPDATE 会让余额与流水脱节，而对账里"钱变了但没有流水"是最难查的一类。
//
// 用法（默认 dry-run，只打印将要发生什么）：
//
//	go run ./cmd/oncefix -dsn "$DATABASE_URL" -user 1 -amount 479980 \
//	    -ref 1550 -reason "..." -apply
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/service/billing"
)

func main() {
	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"), "PostgreSQL 连接串（默认读 DATABASE_URL）")
	user := flag.Uint64("user", 0, "用户 ID")
	amount := flag.Int64("amount", 0, "冲正金额（微，正数=退给用户）")
	ref := flag.String("ref", "", "关联的计费记录号（usages.id），写进流水 ref_id")
	reason := flag.String("reason", "", "冲正原因（写进流水，200 字内）")
	apply := flag.Bool("apply", false, "真正写库（默认只算不写）")
	flag.Parse()

	logger.Init("info")
	defer logger.Sync()

	if *dsn == "" || *user == 0 || *amount == 0 || *ref == "" || *reason == "" {
		fmt.Fprintln(os.Stderr, "用法: oncefix -dsn ... -user 1 -amount 479980 -ref 1550 -reason \"...\" [-apply]")
		os.Exit(2)
	}
	gdb, err := db.Init(*dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	ctx := context.Background()
	// 幂等：同一条 usage 已经冲正过就不再退第二次（按 ref_type+ref_id+reason 判断）。
	var n int64
	if err := gdb.Model(&model.Transaction{}).
		Where("type = ? AND reason = ?", model.TxnAdjust, *reason).Count(&n).Error; err != nil {
		fmt.Fprintln(os.Stderr, "查询历史冲正失败:", err)
		os.Exit(1)
	}
	if n > 0 {
		fmt.Printf("已存在冲正记录（usage #%s），不重复执行。\n", *ref)
		return
	}

	var u model.User
	if err := gdb.First(&u, *user).Error; err != nil {
		fmt.Fprintln(os.Stderr, "用户不存在:", err)
		os.Exit(1)
	}
	fmt.Printf("用户 #%d 当前余额 %d µ，冲正 %d µ ⇒ 预期 %d µ\n", u.ID, u.Balance, *amount, u.Balance+*amount)
	if !*apply {
		fmt.Println("（dry-run，未写库；加 -apply 执行）")
		return
	}
	if err := billing.New(gdb).Adjust(ctx, *user, *amount, *reason); err != nil {
		fmt.Fprintln(os.Stderr, "冲正失败:", err)
		os.Exit(1)
	}
	var after model.User
	_ = gdb.First(&after, *user).Error
	// 只写 Adjust 自己那一行流水（type=adjust）。再补一条"说明行"会让
	// transactions 的 balance_after 链出现两个同额却只变一次余额的行，
	// 对账的逐行累加会立刻不平 —— 溯源信息放在 reason 里就够了。
	fmt.Printf("已冲正：余额 %d → %d µ（流水 reason 里带上 usage #%s）\n", u.Balance, after.Balance, *ref)
}
