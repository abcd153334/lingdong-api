package stats

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
)

/*
日界（业务时区）的回归用例。

这一组用例都在验证同一件事：**「今天」是谁说了算**。
改造前 Go 侧用 time.UTC 拼骨架、SQL 侧用 CURRENT_DATE（= DB 会话时区），
两套口径在每天 00:00–08:00 之间会错开一天，而页面上的每个数字看起来都合理。
*/

// TestDayRangeAndMonthRange 半开区间与业务时区换算（纯函数，不需要数据库）。
func TestDayRangeAndMonthRange(t *testing.T) {
	loc := Location()
	// 凌晨 00:30 —— 正是旧实现在 UTC 会话下会把「今天」判成昨天的时段
	now := time.Date(2026, 3, 15, 0, 30, 0, 0, loc)

	start, end := DayRange(now)
	if !start.Equal(time.Date(2026, 3, 15, 0, 0, 0, 0, loc)) {
		t.Fatalf("今日起点=%s，期望业务时区的 03-15 00:00", start)
	}
	if !end.Equal(time.Date(2026, 3, 16, 0, 0, 0, 0, loc)) {
		t.Fatalf("今日终点=%s，期望业务时区的 03-16 00:00（半开）", end)
	}

	ms, me := MonthRange(now)
	if !ms.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, loc)) {
		t.Fatalf("月初=%s", ms)
	}
	// 月上界是「明日」而不是「下月 1 日」：本月统计要含今天已发生的部分
	if !me.Equal(end) {
		t.Fatalf("月上界应等于今日终点: month=%s day=%s", me, end)
	}

	// DayKey 必须换算到业务时区再格式化：同一时刻在 UTC 下是前一日的 16:30
	if got := DayKey(now); got != "2026-03-15" {
		t.Fatalf("DayKey=%s，期望 2026-03-15", got)
	}
	if got := DayKey(now.UTC()); got != "2026-03-15" {
		t.Fatalf("DayKey(UTC 时刻)=%s，必须先 In(业务时区) 再格式化", got)
	}
}

// TestUserOverviewDayBoundary 锁定今日窗口的边界归属（左闭右开）。
func TestUserOverviewDayBoundary(t *testing.T) {
	gdb := withTestDB(t)

	loc := Location()
	// 00:30 是最能暴露「用 UTC 日界」的时刻：此刻 UTC 还停在前一天 16:30
	now := time.Date(2026, 3, 15, 0, 30, 0, 0, loc)
	prevNow := nowFn
	nowFn = func() time.Time { return now }
	t.Cleanup(func() { nowFn = prevNow })

	u := &model.User{Username: "day_edge", PasswordHash: "x", Group: "default", Role: 1, Status: 1, AffCode: "day-edge"}
	if err := gdb.Create(u).Error; err != nil {
		t.Fatalf("造用户失败: %v", err)
	}

	seedAt := func(at time.Time, charge int64) {
		t.Helper()
		if err := gdb.Create(&model.Usage{
			CreatedAt: at, UserID: u.ID, KeyID: 1, ChannelID: 1,
			Model: "gpt-4o", Modality: "chat", Status: "settled", HTTPStatus: 200,
			Kind: "relay", CallKind: "sync", Charge: charge,
		}).Error; err != nil {
			t.Fatalf("造 usage 失败: %v", err)
		}
	}
	day := func(d, h, m, s int) time.Time { return time.Date(2026, 3, d, h, m, s, 0, loc) }

	seedAt(day(15, 0, 5, 0), 100)   // 今日 00:05 —— 属今日（UTC 下是「昨日」16:05）
	seedAt(day(14, 23, 55, 0), 200) // 昨日 23:55 —— 不属今日
	seedAt(day(16, 0, 0, 0), 400)   // 明日 00:00 整 —— 右开区间，不属今日、也不属本月
	seedAt(day(1, 0, 0, 0), 800)    // 本月 1 日 00:00 整 —— 属本月（但属于聚合区，本用例不建聚合）

	// 昨天那一桶进聚合表，用来验证「本月 = 聚合(月初~昨日) + 今日」
	if err := gdb.Exec(`INSERT INTO usage_daily (date, model, modality, user_id, requests, errors, charge)
		VALUES (?::date, 'gpt-4o', 'chat', ?, 1, 0, 700)`, DayKey(day(14, 0, 0, 0)), u.ID).Error; err != nil {
		t.Fatalf("造聚合失败: %v", err)
	}

	ctx := context.Background()
	ov, err := UserOverview(ctx, u.ID, 7)
	if err != nil {
		t.Fatalf("聚合失败: %v", err)
	}

	// 只有 00:05 那条算今天。用 UTC 日界会得到 0 —— 这正是本用例的判别点。
	if ov.Today.Requests != 1 {
		t.Fatalf("今日请求=%d，期望 1（用 UTC 日界会得 0）", ov.Today.Requests)
	}
	if ov.Today.Charge != 100 {
		t.Fatalf("今日消耗=%d，期望 100", ov.Today.Charge)
	}
	// 本月 = 昨日聚合 700 + 今日 100；明日 00:00 整那 400 不得计入，本月 1 日那 800 只在聚合表里、
	// 本用例没给它建聚合行，因此也不出现（明细不进本月区间，除了今日）。
	if ov.Month.Charge != 800 || ov.Month.Requests != 2 {
		t.Fatalf("本月=%+v，期望 charge=800 requests=2（明日 0 点计入或聚合重复计数）", ov.Month)
	}

	// 趋势骨架必须覆盖到今日，且末点等于今日费用
	last := ov.Trend[len(ov.Trend)-1]
	if last.Date != "2026-03-15" {
		t.Fatalf("趋势末点日期=%s，期望 2026-03-15", last.Date)
	}
	if last.Charge != ov.Today.Charge {
		t.Fatalf("趋势末点(%d) != 今日费用(%d)", last.Charge, ov.Today.Charge)
	}
	if ov.Trend[0].Date != "2026-03-09" {
		t.Fatalf("d7 首点=%s，期望 2026-03-09", ov.Trend[0].Date)
	}
}

// TestUserOverviewSessionTZIndependent 业务结果不受 DB 会话时区影响。
//
// 这是「单一真相源」的直接断言：同一个库、同一批数据，把会话时区从 UTC 换成 +08
// 后结果必须逐字段相同。若有人把 AT TIME ZONE / DayRange 改回 CURRENT_DATE，
// 两个时区下的「今日」会差一天，用例立刻红。
func TestUserOverviewSessionTZIndependent(t *testing.T) {
	gdb := withTestDB(t)

	loc := Location()
	now := time.Date(2026, 3, 15, 0, 30, 0, 0, loc)
	prevNow := nowFn
	nowFn = func() time.Time { return now }
	t.Cleanup(func() { nowFn = prevNow })

	u := &model.User{Username: "tz_free", PasswordHash: "x", Group: "default", Role: 1, Status: 1, AffCode: "tz-free"}
	if err := gdb.Create(u).Error; err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	if err := gdb.Create(&model.Usage{
		CreatedAt: time.Date(2026, 3, 15, 0, 10, 0, 0, loc), UserID: u.ID, KeyID: 1, ChannelID: 1,
		Model: "gpt-4o", Modality: "chat", Status: "settled", HTTPStatus: 200,
		Kind: "relay", CallKind: "sync", Charge: 500,
	}).Error; err != nil {
		t.Fatalf("造 usage 失败: %v", err)
	}
	if err := gdb.Exec(`INSERT INTO usage_daily (date, model, modality, user_id, requests, errors, charge)
		VALUES (?::date, 'gpt-4o', 'chat', ?, 1, 0, 300)`, DayKey(time.Date(2026, 3, 5, 12, 0, 0, 0, loc)), u.ID).Error; err != nil {
		t.Fatalf("造聚合失败: %v", err)
	}

	forceSessionTZ(t, gdb, "UTC")
	ctx := context.Background()
	utcRun, err := UserOverview(ctx, u.ID, 7)
	if err != nil {
		t.Fatalf("UTC 会话聚合失败: %v", err)
	}

	forceSessionTZ(t, gdb, loc.String())
	localRun, err := UserOverview(ctx, u.ID, 7)
	if err != nil {
		t.Fatalf("业务时区会话聚合失败: %v", err)
	}

	if utcRun.Today != localRun.Today || utcRun.Month != localRun.Month {
		t.Fatalf("会话时区改变影响了统计结果:\n  utc=%+v/%+v\n  biz=%+v/%+v",
			utcRun.Today, utcRun.Month, localRun.Today, localRun.Month)
	}
	// 顺带锁定绝对值：改成 UTC 日界时这里会变成 0 个请求
	if localRun.Today.Requests != 1 || localRun.Today.Charge != 500 {
		t.Fatalf("今日=%+v，期望 1 请求 / 500 消耗", localRun.Today)
	}
	if localRun.Month.Charge != 800 {
		t.Fatalf("本月消耗=%d，期望 800（聚合 300 + 今日 500）", localRun.Month.Charge)
	}
}

// TestRollupUsesBusinessTimezone 聚合表的 date 桶按业务时区折算，而不是会话时区。
func TestRollupUsesBusinessTimezone(t *testing.T) {
	gdb := withTestDB(t)
	loc := Location()
	// 会话时区压成 UTC：此时 03-15 00:30(+08) 的 UTC 日期是 03-14，两者必须分得开
	forceSessionTZ(t, gdb, "UTC")

	u := &model.User{Username: "rollup_tz", PasswordHash: "x", Group: "default", Role: 1, Status: 1, AffCode: "rollup-tz"}
	if err := gdb.Create(u).Error; err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	for _, at := range []time.Time{
		time.Date(2026, 3, 15, 0, 30, 0, 0, loc), // 业务日 03-15；UTC 日 03-14
		time.Date(2026, 3, 14, 22, 0, 0, 0, loc), // 业务日 03-14；UTC 日 03-14
	} {
		if err := gdb.Create(&model.Usage{
			CreatedAt: at, UserID: u.ID, KeyID: 1, ChannelID: 1,
			Model: "gpt-4o", Modality: "chat", Status: "settled", HTTPStatus: 200,
			Kind: "relay", CallKind: "sync", Charge: 100,
		}).Error; err != nil {
			t.Fatalf("造 usage 失败: %v", err)
		}
	}

	ctx := context.Background()
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, loc)
	end := time.Date(2026, 4, 1, 0, 0, 0, 0, loc)
	if _, err := RollupDailyWindow(ctx, start, end); err != nil {
		t.Fatalf("聚合失败: %v", err)
	}

	var rows []struct {
		Date string
		N    int64
	}
	if err := gdb.Raw(`SELECT to_char(date, 'YYYY-MM-DD') AS date, SUM(requests) AS n
	                     FROM usage_daily GROUP BY date ORDER BY date`).Scan(&rows).Error; err != nil {
		t.Fatalf("读取聚合失败: %v", err)
	}
	if len(rows) != 2 || rows[0].Date != "2026-03-14" || rows[1].Date != "2026-03-15" {
		t.Fatalf("聚合日桶=%+v，期望 03-14 与 03-15 各一行（按 UTC 分会并成 03-14 一行）", rows)
	}
	if rows[0].N != 1 || rows[1].N != 1 {
		t.Fatalf("日桶计数=%+v，期望各 1", rows)
	}
}

// TestRollupWindowFillsMonth 宽窗聚合能把整个月的桶补齐（worker 每日首轮的机制）。
func TestRollupWindowFillsMonth(t *testing.T) {
	gdb := withTestDB(t)
	loc := Location()

	u := &model.User{Username: "rollup_wide", PasswordHash: "x", Group: "default", Role: 1, Status: 1, AffCode: "rollup-wide"}
	if err := gdb.Create(u).Error; err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	oldAt := time.Date(2026, 3, 2, 12, 0, 0, 0, loc)
	if err := gdb.Create(&model.Usage{
		CreatedAt: oldAt, UserID: u.ID, KeyID: 1, ChannelID: 1,
		Model: "gpt-4o", Modality: "chat", Status: "settled", HTTPStatus: 200,
		Kind: "relay", CallKind: "sync", Charge: 800,
	}).Error; err != nil {
		t.Fatalf("造 usage 失败: %v", err)
	}

	ctx := context.Background()
	dayStart := time.Date(2026, 3, 15, 0, 0, 0, 0, loc)
	dayEnd := dayStart.AddDate(0, 0, 1)

	// 窄窗（只覆盖最近几天）不碰 03-02 —— 这正是「只按 RollupLookbackDays 回补」的缺口
	if _, err := RollupDailyWindow(ctx, dayStart.AddDate(0, 0, -RollupLookbackDays()), dayEnd); err != nil {
		t.Fatalf("窄窗聚合失败: %v", err)
	}
	var n int64
	if err := gdb.Raw(`SELECT COUNT(*) FROM usage_daily WHERE date = ?::date`, DayKey(oldAt)).Scan(&n).Error; err != nil {
		t.Fatalf("读取聚合失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("窄窗不应覆盖 03-02，实际行数=%d", n)
	}

	// 宽窗（月初~明日）补齐整个月
	monthStart := time.Date(2026, 3, 1, 0, 0, 0, 0, loc)
	if _, err := RollupDailyWindow(ctx, monthStart, dayEnd); err != nil {
		t.Fatalf("宽窗聚合失败: %v", err)
	}
	var got int64
	if err := gdb.Raw(`SELECT COALESCE(SUM(charge), 0) FROM usage_daily WHERE date = ?::date`, DayKey(oldAt)).Scan(&got).Error; err != nil {
		t.Fatalf("读取聚合失败: %v", err)
	}
	if got != 800 {
		t.Fatalf("宽窗未补齐 03-02 桶: charge=%d，期望 800", got)
	}

	// 幂等：宽窗重复执行不叠加
	if _, err := RollupDailyWindow(ctx, monthStart, dayEnd); err != nil {
		t.Fatalf("二次宽窗聚合失败: %v", err)
	}
	if err := gdb.Raw(`SELECT COALESCE(SUM(charge), 0) FROM usage_daily WHERE date = ?::date`, DayKey(oldAt)).Scan(&got).Error; err != nil {
		t.Fatalf("二次读取聚合失败: %v", err)
	}
	if got != 800 {
		t.Fatalf("宽窗二次聚合非幂等: charge=%d", got)
	}
}

// forceSessionTZ 把**当前连接**的时区固定下来。
//
// 必须配合 SetMaxOpenConns(1)：`SET TIME ZONE` 是连接级设置，池里换一条连接就失效，
// 那种「有时生效」的测试会变成随机绿的假保护。设置后回读校验，不一致直接跳过
// （说明该环境的连接池行为不支持本用例，而不是用例本身有错）。
func forceSessionTZ(t *testing.T, gdb *gorm.DB, tz string) {
	t.Helper()
	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatalf("取连接池失败: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	// tz 只来自代码内常量或 Location().String()，不存在注入面
	if err := gdb.Exec("SET TIME ZONE '" + tz + "'").Error; err != nil {
		t.Fatalf("设置会话时区失败: %v", err)
	}
	var got string
	if err := gdb.Raw("SHOW TIME ZONE").Scan(&got).Error; err != nil {
		t.Fatalf("读取会话时区失败: %v", err)
	}
	if got != tz {
		t.Skipf("连接池未能固定会话时区（设置 %s，实际 %s），跳过该环境", tz, got)
	}
}
