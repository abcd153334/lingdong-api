package db

import (
	"os"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// TestLedgerCheckConstraints 锁死「资金列的非负约束真的在库里」。
//
// 这些约束是账务的第二道防线：应用层行锁保证并发正确，数据库保证「物理上不可能的
// 值根本写不进去」。没有它，一次人工改库或一个把 -1 当"未知"写进列的实现，
// 只能靠 24 小时一轮的只读对账事后发现 —— 那时钱已经动了。
func TestLedgerCheckConstraints(t *testing.T) {
	dsn := os.Getenv("LD_TEST_DSN")
	if dsn == "" {
		if os.Getenv("LD_REQUIRE_TEST_DSN") == "1" {
			t.Fatal("LD_REQUIRE_TEST_DSN=1 但未设置 LD_TEST_DSN：集成用例不允许被静默跳过")
		}
		t.Skip("未设置 LD_TEST_DSN，跳过数据库集成用例")
	}
	gdb, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}

	EnsureLedgerCheckConstraints(gdb)

	for _, c := range ledgerChecks {
		var exists bool
		if err := gdb.Raw(`SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conname = ?)`, c.name).
			Scan(&exists).Error; err != nil {
			t.Fatalf("查询约束 %s 失败: %v", c.name, err)
		}
		if !exists {
			t.Fatalf("约束 %s 未建立（%s）", c.name, c.reason)
		}
	}

	// 行为验证：负冻结必须被数据库直接拒绝（23514 = check_violation）。
	tx := gdb.Begin()
	defer tx.Rollback()
	// 只给非空列填占位值：目的是证明「负冻结在写库这一层就被拦下」。
	err = tx.Exec(`INSERT INTO users (username, password_hash, balance, frozen) VALUES (?, ?, 0, -1)`,
		"ck_neg_frozen", "x").Error
	if err == nil {
		t.Fatal("数据库接受了 frozen=-1：非负约束没有生效")
	}
	if !strings.Contains(err.Error(), "ck_users_frozen_nonneg") {
		t.Fatalf("负冻结应被 ck_users_frozen_nonneg 拒绝，实际错误：%v", err)
	}
}
