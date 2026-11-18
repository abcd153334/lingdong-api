package adapters

import (
	"os"
	"testing"
)

// TestMain 为适配器测试打开「允许私网/回环」开关。
//
// 原因：本包的传输用例全部指向 httptest 起的 127.0.0.1 服务。生产默认策略是拒绝回环/私网
// （见 internal/pkg/netguard），测试环境必须显式声明「我确实要连本机」，否则会误判为回归。
// 注意这里用 os.Setenv 而不是 t.Setenv：TestMain 阶段还没有 *testing.T。
func TestMain(m *testing.M) {
	if os.Getenv("LD_UPSTREAM_ALLOW_PRIVATE") == "" {
		_ = os.Setenv("LD_UPSTREAM_ALLOW_PRIVATE", "1")
	}
	os.Exit(m.Run())
}
