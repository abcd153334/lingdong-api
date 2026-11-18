package task

import (
	"os"
	"testing"

	"lingdong-api/internal/pkg/mediaprobe"
)

// TestMain 打开「探测放行回环」：probe_test.go / regenerate_test.go 里的素材地址
// 全部指向 httptest 起的 127.0.0.1 服务，而生产策略（正确地产）拒绝回环。
//
// 必须在这里开而不是改环境变量：mediaprobe 已刻意不再读 LD_UPSTREAM_ALLOW_PRIVATE
// （那个开关是给渠道 base_url 的，用户可控 URL 借它就能打内网）。
func TestMain(m *testing.M) {
	mediaprobe.AllowLoopbackForTest()
	code := m.Run()
	mediaprobe.ResetPolicyForTest()
	os.Exit(code)
}
