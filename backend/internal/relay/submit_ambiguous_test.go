package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"
)

// TestIsAmbiguousSubmit 锁死「上游是否可能已建单」的判定。
//
// 这个判定是资金安全与可用性的分界：
//   - 误判成 true → 本该换渠道成功的任务被直接失败退款（可用性损失，用户投诉）；
//   - 误判成 false → 上游已受理却又在另一条渠道建了一单（平台双倍成本 + 孤儿产物）。
//
// 所以每一条分支都必须有用例，尤其是「dial 失败不算歧义」这条反直觉的规则。
func TestIsAmbiguousSubmit(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil 不歧义", nil, false},

		// 拿到响应 → 按状态码与响应体判定。
		{"400 参数错", StatusError(400, `{"error":"bad"}`), false},
		{"401 鉴权", StatusError(401, `{"error":"no key"}`), false},
		{"404 路径错", StatusError(404, `{"error":"not found"}`), false},
		{"429 限流", StatusError(429, `{"error":"slow down"}`), false},
		{"500 且带响应体 = 明确报错", StatusError(500, `{"error":"boom"}`), false},
		{"502 且带响应体", StatusError(502, `{"error":"bad gateway"}`), false},
		{"500 且响应体为空 = 歧义", StatusError(500, ""), true},
		{"504 且响应体只有空白 = 歧义", StatusError(504, "   \n "), true},

		// 连接层错误。
		{"连接被拒绝（没发出去）", fmt.Errorf("submit: %w", syscall.ECONNREFUSED), false},
		{"主机不可达", fmt.Errorf("submit: %w", syscall.EHOSTUNREACH), false},
		{"网络不可达", fmt.Errorf("submit: %w", syscall.ENETUNREACH), false},
		{"dial 阶段超时不算歧义", &net.OpError{Op: "dial", Err: timeoutErr{}}, false},
		{"dial 阶段失败不算歧义", &net.OpError{Op: "dial", Err: errors.New("refused")}, false},
		{"读阶段超时算歧义", &net.OpError{Op: "read", Err: timeoutErr{}}, true},
		{"context 超时算歧义", context.DeadlineExceeded, true},
		{"EOF 算歧义", io.EOF, true},
		{"UnexpectedEOF 算歧义", io.ErrUnexpectedEOF, true},
		{"连接被重置算歧义", fmt.Errorf("submit: %w", syscall.ECONNRESET), true},
		{"EPIPE 算歧义", fmt.Errorf("submit: %w", syscall.EPIPE), true},

		// 普通业务错误（适配器自己拼的文案）：默认不歧义，允许换渠道重投。
		{"普通错误", errors.New("适配器未配置 base_url"), false},
		{"CallerParamError", &CallerParamError{Msg: "prompt 不能为空"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsAmbiguousSubmit(c.err); got != c.want {
				t.Fatalf("IsAmbiguousSubmit=%v，期望 %v（判错会导致要么白退款、要么双倍上游成本）", got, c.want)
			}
		})
	}
}

// TestHTTPStatusErrorText 保证错误文案与改造前逐字一致（运维与既有断言都依赖它）。
func TestHTTPStatusErrorText(t *testing.T) {
	if got := StatusError(503, "upstream down").Error(); got != "上游返回 503：upstream down" {
		t.Fatalf("默认文案变了：%q", got)
	}
	// Note 非空时必须**逐字**返回 Note，不能拼上状态码前缀。
	custom := &HTTPStatusError{Code: 500, Body: "raw", Note: "适配器 x：请检查 Base URL"}
	if got := custom.Error(); got != "适配器 x：请检查 Base URL" {
		t.Fatalf("Note 应原样返回，实际 %q", got)
	}
	var nilErr *HTTPStatusError
	if got := nilErr.Error(); got != "" {
		t.Fatalf("nil 错误应返回空串，实际 %q", got)
	}
	if got := StatusOf(errors.New("plain")); got != 0 {
		t.Fatalf("非 HTTPStatusError 应返回 0，实际 %d", got)
	}
	if got := StatusOf(fmt.Errorf("wrap: %w", StatusError(429, "x"))); got != 429 {
		t.Fatalf("应能穿过 %%w 包装取到状态码，实际 %d", got)
	}
}

// TestHTTPStatusErrorClass 验证「没带状态码归因」时的类别映射。
//
// 401/403 必须返回空串（走 internal）：classifyError 没有 auth 分支，
// 随便归成 channel 会把整条渠道熔断，而归成 caller 会让用户以为自己传错了参数。
func TestHTTPStatusErrorClass(t *testing.T) {
	cases := map[int]string{
		401: "",
		403: "",
		429: "rate_limited",
		400: "caller",
		404: "caller",
		422: "caller",
		500: "channel",
		503: "channel",
	}
	for code, want := range cases {
		if got := StatusError(code, "b").Class(); got != want {
			t.Fatalf("Class(%d)=%q，期望 %q", code, got, want)
		}
	}
}

// timeoutErr 实现 net.Error 且 Timeout()=true。
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }
