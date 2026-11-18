package apidoc

import (
	"testing"

	"lingdong-api/internal/gateway"
	"lingdong-api/internal/pkg/errs"
)

// TestEndpointErrorCodesAreRegistered 守「文档里印的每个错误码都真实存在」。
//
// 这条门禁防的具体事故：`endpointErrors` 是手写的 switch，写一个 `upstream_timeout`
// 这种"听起来很合理"的码进去，文档就会印出来 —— 而网关从来不返回它。
// 接入方按它写重试分支，永远命中不到；真实错误落在别的码上，被当未知错误。
//
// 允许两个来源：
//   - `errs.Get`：内部码（绝大多数）；
//   - `gateway.WireCodes()`：协议面被覆盖出来的码（如 insufficient_user_quota，
//     它在内部叫 insufficient_balance，只在协议面存在）。
//
// 除此之外没有任何白名单：新增一条 override 要同步改 gateway，而不是改这个测试。
func TestEndpointErrorCodesAreRegistered(t *testing.T) {
	wire := make(map[string]bool, len(gateway.WireCodes()))
	for _, c := range gateway.WireCodes() {
		wire[c] = true
	}
	var bad []string
	for _, k := range Kinds() {
		for _, ec := range endpointErrors(k) {
			if _, ok := errs.Get(ec.Code); ok {
				continue
			}
			if wire[ec.Code] {
				continue
			}
			bad = append(bad, string(k)+" → "+ec.Code)
		}
	}
	if len(bad) > 0 {
		t.Fatalf("文档印了不存在的错误码（要么登记进 errs，要么从 endpointErrors 删掉）：\n%v", bad)
	}
}
