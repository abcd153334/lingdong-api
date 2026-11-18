package spec

import (
	"os"
	"testing"
)

// TestMain 为规格适配器测试打开「允许私网/回环」开关。
//
// 原因：运行时用例全部指向 httptest 起的 127.0.0.1 服务。生产默认策略是拒绝回环/私网
// （见 internal/pkg/netguard），测试环境必须显式声明「我确实要连本机」，
// 否则每条用例都会因为出站策略被拒而失败，看起来像功能回归。
// 用 os.Setenv 而不是 t.Setenv：TestMain 阶段还没有 *testing.T。
func TestMain(m *testing.M) {
	if os.Getenv("LD_UPSTREAM_ALLOW_PRIVATE") == "" {
		_ = os.Setenv("LD_UPSTREAM_ALLOW_PRIVATE", "1")
	}
	os.Exit(m.Run())
}
