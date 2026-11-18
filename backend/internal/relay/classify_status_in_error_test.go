package relay

import (
	"errors"
	"strconv"
	"testing"
)

// 本文件守「状态码只存在于错误里」时的归因。
//
// 背景：多米把鉴权/限流失败放在 HTTP 200 的响应信封里（业务码 401/403/429），
// 适配器把它折成 HTTPStatusError 后，调用方只拿得到 err 而没有 status 参数。
// 改造前 classify(0, err, false) 会直接走 classifyError，而 HTTPStatusError.Class()
// 对 401/403 刻意返回空串 → 归因为 internal/upstream：
// 一枚坏 key 能把整条渠道打成禁用，且该 key 永不冷却。

// TestClassifyReadsStatusFromError 状态码在错误里时，归因结果与「直接传状态码」一致。
func TestClassifyReadsStatusFromError(t *testing.T) {
	cases := []struct {
		code int
		want outcome
	}{
		{401, outcomeAuth},
		{403, outcomeAuth},
		{429, outcomeRateLimit},
		{400, outcomeCaller},
		{404, outcomeCaller},
		{500, outcomeUpstream},
	}
	for _, c := range cases {
		err := &HTTPStatusError{Code: c.code, Note: "业务码 " + strconv.Itoa(c.code)}
		if got := classify(0, err, false); got != c.want {
			t.Fatalf("code=%d：classify(0,err)=%v，期望 %v", c.code, got, c.want)
		}
		// 显式传状态码时结果必须相同 —— 两条入口不能得出不同结论。
		if got := classify(c.code, nil, false); got != c.want {
			t.Fatalf("code=%d：classify(code,nil)=%v，期望 %v", c.code, got, c.want)
		}
	}
}

// TestClassifyStillUsesClassWhenNoStatus 没有状态码时仍看适配器自述类别。
func TestClassifyStillUsesClassWhenNoStatus(t *testing.T) {
	if got := classify(0, &CallerParamError{Msg: "n 超出上限"}, false); got != outcomeCaller {
		t.Fatalf("CallerParamError 应归因为 caller，实际 %v", got)
	}
	if got := classify(0, errors.New("dial tcp: timeout"), false); got != outcomeUpstream {
		t.Fatalf("裸错误应归因为 upstream，实际 %v", got)
	}
}
