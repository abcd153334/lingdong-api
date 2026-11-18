package relay

import (
	"errors"
	"fmt"
	"testing"
)

// TestClassifyPoll 锁死「轮询失败该走哪条处置」。
//
// 分档判错的两个方向都贵：
//   - 把 404 当 transient → 等满整轮判超时才退款，error_msg 还是"连续失败"（排障方向错）；
//   - 把 429 当 transient → 一次上游限流把整批在途任务判超时退款（上游仍在生成计费）。
func TestClassifyPoll(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want PollFailureClass
	}{
		{"404 任务不存在", &HTTPStatusError{Code: 404}, PollFailureNotFound},
		{"410 已清理", &HTTPStatusError{Code: 410}, PollFailureNotFound},
		{"401 鉴权", &HTTPStatusError{Code: 401}, PollFailureAuth},
		{"403 鉴权", &HTTPStatusError{Code: 403}, PollFailureAuth},
		{"429 限流", &HTTPStatusError{Code: 429}, PollFailureRateLimited},
		{"500 上游故障", &HTTPStatusError{Code: 500}, PollFailureTransient},
		{"503 上游故障", &HTTPStatusError{Code: 503}, PollFailureTransient},
		{"网络错误（无状态码）", errors.New("connection reset by peer"), PollFailureTransient},
		{"包裹后的 404 也要认", fmt.Errorf("查状态失败: %w", &HTTPStatusError{Code: 404}), PollFailureNotFound},
		{"其它 4xx（多为路径写错）", &HTTPStatusError{Code: 400}, PollFailureOther},
	}
	for _, c := range cases {
		if got := ClassifyPoll(c.err); got != c.want {
			t.Fatalf("%s：ClassifyPoll=%v，期望 %v", c.name, got, c.want)
		}
	}
}
