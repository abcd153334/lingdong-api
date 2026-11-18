package app

// 公告「此刻生效」判据的一致性守卫。
//
// 这条判据有两个实现（SQL 的 noticeActiveWhere 与 Go 的 ActiveOf），
// 它们必须同义。纯 Go 部分在这里覆盖全部边界；SQL 与 Go 的对拍需要真库
// （见 TestActiveWhereMatchesGo，缺 LD_TEST_DSN 会 skip —— CI 必须带
// LD_REQUIRE_TEST_DSN=1，否则这条对拍等于不存在）。

import (
	"strings"
	"testing"
	"time"

	"lingdong-api/internal/model"
)

func TestActiveOfBoundaries(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ns := func(d time.Duration) *time.Time {
		v := now.Add(d)
		return &v
	}

	cases := []struct {
		name string
		row  model.AppNotice
		want bool
	}{
		{"停用：窗口内也不下发", model.AppNotice{Enabled: false, StartsAt: ns(-time.Hour), EndsAt: ns(time.Hour)}, false},
		{"无窗口：启用即下发", model.AppNotice{Enabled: true}, true},
		{"起点等于现在：下发（闭区间）", model.AppNotice{Enabled: true, StartsAt: ns(0)}, true},
		{"起点晚 1ns：不下发", model.AppNotice{Enabled: true, StartsAt: ns(time.Nanosecond)}, false},
		{"终点等于现在：仍下发（闭区间）", model.AppNotice{Enabled: true, EndsAt: ns(0)}, true},
		{"终点早 1ns：已过期", model.AppNotice{Enabled: true, EndsAt: ns(-time.Nanosecond)}, false},
		{"窗口完整包含现在", model.AppNotice{Enabled: true, StartsAt: ns(-time.Hour), EndsAt: ns(time.Hour)}, true},
		{"未来才开始", model.AppNotice{Enabled: true, StartsAt: ns(time.Hour), EndsAt: ns(2 * time.Hour)}, false},
	}
	for _, c := range cases {
		if got := ActiveOf(c.row, now); got != c.want {
			t.Errorf("%s: ActiveOf=%v，期望 %v", c.name, got, c.want)
		}
	}
}

// TestActiveWherePlaceholderCount 守住占位符个数。
//
// 往 noticeActiveWhere 里加一个 `?` 会静默改变**所有**调用点的参数序号，
// 编译期完全看不出来。这里把个数钉死。
func TestActiveWherePlaceholderCount(t *testing.T) {
	if n := strings.Count(noticeActiveWhere, "?"); n != 3 {
		t.Fatalf("noticeActiveWhere 必须有 3 个占位符（enabled, now, now），实际 %d 个", n)
	}
}
