// Package testutil 是集成用例的公共基座。
//
// 存在理由：全仓 45+ 个测试文件各自抄一遍「读 LD_TEST_DSN → 空则 Skip → 开静默 gorm」
// 这段样板，抄出三种偏差（有的不 Load conf、有的不静默日志淹没输出、有的忘了 defer 关闭）。
// 收敛成一份后，连库口径只有一个。
package testutil

import (
	"os"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// DSN 返回集成用例要连的测试库连接串；没有可用配置时按环境决定跳过还是失败。
//
// 为什么要有 LD_REQUIRE_TEST_DSN：缺失 DSN 时默认 Skip 是本地开发的便利，
// 但在 CI 里它会把"集成用例一个都没跑"伪装成"全绿"——那是最危险的一种绿灯。
// CI 必须设 LD_REQUIRE_TEST_DSN=1，让缺配置变成红。
func DSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("LD_TEST_DSN"))
	if dsn == "" {
		if os.Getenv("LD_REQUIRE_TEST_DSN") == "1" {
			t.Fatal("LD_REQUIRE_TEST_DSN=1 但 LD_TEST_DSN 为空：集成用例不能静默跳过")
		}
		t.Skip("未设置 LD_TEST_DSN，跳过数据库集成用例")
	}
	return dsn
}

// Open 打开测试库并注册关闭；日志静默（集成用例的输出要留给断言失败信息）。
func Open(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := DSN(t)
	gdb, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatalf("取连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return gdb
}
