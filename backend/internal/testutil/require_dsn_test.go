package testutil

import (
	"os"
	"strings"
	"testing"
)

// TestIntegrationDSNConfigured 是 CI 的「不许假绿」闸门。
//
// 背景：全仓 40+ 个集成用例在拿不到 LD_TEST_DSN 时走 t.Skip —— 这对本地开发是
// 正确的（没有测试库也能跑单测），但在 CI 里会把「一个集成用例都没跑」伪装成
// 「全绿」。这条用例只在 LD_REQUIRE_TEST_DSN=1 时生效，缺 DSN 直接失败。
//
// 为什么不逐个改那 40 处：改动面大、每处的建 schema 方式都不同，容易改出
// 「skip 变 fatal 但 schema 没建好」的假红。先用一条守卫兜住 CI 的语义，
// 再逐步把这些样板收敛到 testutil.Open（见 §B4）。
func TestIntegrationDSNConfigured(t *testing.T) {
	if os.Getenv("LD_REQUIRE_TEST_DSN") != "1" {
		t.Skip("未设置 LD_REQUIRE_TEST_DSN，跳过 CI DSN 守卫")
	}
	if strings.TrimSpace(os.Getenv("LD_TEST_DSN")) == "" {
		t.Fatal("LD_REQUIRE_TEST_DSN=1 但 LD_TEST_DSN 为空：本次运行里所有集成用例都会静默跳过，" +
			"这个绿灯不代表任何集成覆盖")
	}
}
