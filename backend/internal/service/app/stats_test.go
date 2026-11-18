package app

// 统计视图与品牌投影的纯函数用例（不依赖数据库）。

import (
	"testing"
	"time"

	"lingdong-api/internal/model"
)

// TestOnlineFromThresholds 锁住三个灯的边界与误差计算。
//
// 这三条阈值只影响"运营看到的灯"，不影响计费或准入 —— 但灯错了会直接导致
// 误判"应用挂了"。边界值取等号，与实现里的 `age <= window` 一致。
func TestOnlineFromThresholds(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time {
		v := now.Add(-d)
		return &v
	}

	cases := []struct {
		name     string
		lastUsed *time.Time
		want     string
	}{
		{"从未调用", nil, "offline"},
		{"正好 5 分钟", at(onlineWindow), "online"},
		{"5 分钟零 1 秒", at(onlineWindow + time.Second), "idle"},
		{"正好 60 分钟", at(idleWindow), "idle"},
		{"60 分钟零 1 秒", at(idleWindow + time.Second), "offline"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := onlineFrom(tc.lastUsed, usage24h{}, now).Online; got != tc.want {
				t.Fatalf("Online = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// TestOnlineFromUsageNumbers 24h 数字与错误率直接透传。
//
// 0 次调用时错误率必须是 0 而不是 NaN：NaN 序列化成 JSON 会直接让整个响应
// 编码失败（前端拿到 500，运营看到"统计坏了"）。
func TestOnlineFromUsageNumbers(t *testing.T) {
	now := time.Now()
	ua := usage24h{Calls: 1000, Errors: 4, Charge: 12345}

	v := onlineFrom(nil, ua, now)
	if v.Calls24h != 1000 || v.Errors24h != 4 || v.Charge24h != 12345 {
		t.Fatalf("数字未透传: %+v", v)
	}
	if v.ErrorRate != 0.004 {
		t.Fatalf("ErrorRate = %v，期望 0.004", v.ErrorRate)
	}

	if got := onlineFrom(nil, usage24h{}, now).ErrorRate; got != 0 {
		t.Fatalf("零调用时 ErrorRate 必须为 0，实际 %v", got)
	}
}

// TestAdminBrandOfCarriesIconToken 管理端品牌必须带 logo_icon，终端不下发。
//
// 品牌页签的图标控件靠令牌回填（URL 无法反解出令牌）；而终端只需要可渲染的
// logo_url。两者共用 BrandOf 的 9 个字段，令牌只在管理端那一层附加。
func TestAdminBrandOfCarriesIconToken(t *testing.T) {
	a := &model.App{
		Slug: "default", Name: "后台名", DisplayName: "",
		LogoIcon: "upload:abc", Tagline: "标语",
	}

	admin := AdminBrandOf(a, "https://cdn.example.com/i.png")
	if admin.LogoIcon != "upload:abc" {
		t.Fatalf("管理端品牌缺图标令牌: %+v", admin)
	}
	if admin.LogoURL != "https://cdn.example.com/i.png" {
		t.Fatalf("管理端品牌缺 logo_url: %+v", admin)
	}
	// 展示名回落：display_name 空 → 后台名称。
	if admin.DisplayName != "后台名" {
		t.Fatalf("展示名应回落后台名称，实际 %q", admin.DisplayName)
	}

	pub := BrandOf(a, "https://cdn.example.com/i.png")
	if pub.DisplayName != "后台名" {
		t.Fatalf("公开面与后台必须用同一条回落规则，实际 %q", pub.DisplayName)
	}
}
