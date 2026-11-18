package stats

import (
	"context"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/logger"
)

// 业务时区：全系统「今日 / 本月 / 趋势分桶」的唯一日界来源。
//
// 为什么必须集中到这一个文件：`usage_daily.date` 由 `created_at` 派生，而
// `created_at` 是 timestamptz —— 同一个时刻在不同时区下属于**不同的日期**。
// 改造前这里是两套口径并存：
//   - SQL 侧一律 `CURRENT_DATE`（= **数据库会话时区**，来自容器 TZ 或服务端设置）
//   - Go 侧拼趋势骨架时用 `time.UTC` 兜底（见旧 UserOverview 的 time.Date(..., time.UTC)）
//
// 两者不一致时（跨时区部署、CI 用 UTC、DB 是 UTC 而容器是 +08），每天
// 00:00–08:00 的调用会被归到前一天，趋势图骨架与 SQL 分桶还会错开一格。
// 表现是「每个数字看着都合理，但加起来永远对不上账」——没有任何报错。
//
// 现在：所有日界由本文件给出，SQL 只接收「Go 算好的 timestamptz」或
// 「(col AT TIME ZONE $tz)::date」，不再读 DB 会话时区。

// defaultTimezone 是 LD_TZ 缺省值。取 Asia/Shanghai 而不是 UTC：
// 这是面向国内用户的运营站点，「今天」对用户来说是北京时间的今天。
const defaultTimezone = "Asia/Shanghai"

// nowFn 是包级时钟，仅供测试注入固定时刻；生产恒为 time.Now。
var nowFn = time.Now

var (
	locOnce sync.Once
	locVal  *time.Location
)

// Location 返回业务时区。非法 LD_TZ 回落 UTC 并打 Error（不 panic）：
//
// 配置写错（比如填成 "+08:00"）时，宁可让统计口径整体退化成 UTC，
// 也不能让整个进程起不来 —— 那会把「一个时区配错」放大成「全站不可用」。
func Location() *time.Location {
	locOnce.Do(func() {
		name := strings.TrimSpace(conf.EnvConfig().Timezone)
		if name == "" {
			name = defaultTimezone
		}
		l, err := time.LoadLocation(name)
		if err != nil {
			logger.L().Error("业务时区非法，已回落到 UTC（请检查 LD_TZ）",
				zap.String("tz", name), zap.Error(err))
			locVal = time.UTC
			return
		}
		locVal = l
	})
	return locVal
}

// resetLocationForTest 让测试在改 LD_TZ 后重新解析。生产代码不得调用。
func resetLocationForTest() {
	locOnce = sync.Once{}
	locVal = nil
}

// BizNow 返回业务时区下的当前时刻。
func BizNow() time.Time { return nowFn().In(Location()) }

// DayRange 返回 now 所在业务日的半开区间 [今日0点, 明日0点)。
//
// 半开区间是硬约束：用闭区间（<= 明日0点）会把次日零点整的那一条同时算进两天。
func DayRange(now time.Time) (start, end time.Time) {
	l := now.Location()
	start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, l)
	return start, start.AddDate(0, 0, 1)
}

// MonthRange 返回自然月区间 [月初0点, 明日0点)。
//
// 上界取「明日」而不是「下月1日」：本月统计要包含今天，而今天只有一部分，
// 用明日0点做上界正好把「今天已经发生的」框进来。
func MonthRange(now time.Time) (start, end time.Time) {
	l := now.Location()
	start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, l)
	_, end = DayRange(now)
	return start, end
}

// DayKey 把时刻格式化成业务日键（"2006-01-02"）。
//
// 先 In(Location()) 再 Format —— 直接 Format 一个 UTC 时刻会得到 UTC 日期，
// 这正是旧实现里趋势骨架与 usage_daily 错开一天的原因。
func DayKey(t time.Time) string { return t.In(Location()).Format("2006-01-02") }

// TrendWindow 返回「含今天在内的最近 n 个业务日」半开区间 [起始日0点, 明日0点)。
//
// 看板趋势、失败榜、渠道榜、d7/d30 排行都是同一个窗口语义（近 7 天 = 今天 + 前 6 天）。
// 它们原来各自写 `created_at >= CURRENT_DATE - 6`，而趋势骨架用另一套算法 ——
// 会话时区与业务时区不一致时，同一张看板上「趋势跑 7 个业务日、失败榜跑 7 个 UTC 日」，
// 两个数字永远对不上，且没有任何报错。
func TrendWindow(now time.Time, n int) (start, end time.Time) {
	if n < 1 {
		n = 1
	}
	start, end = DayRange(now)
	start = start.AddDate(0, 0, -(n - 1))
	return start, end
}

// CheckSessionTimezone 在启动时核对「数据库会话时区」与「业务时区」的偏移是否一致。
//
// 只告警不阻断：改时区需要重启或改 DSN，阻断启动会让一次配置疏漏升级为服务不可用。
// 偏移一致就算通过（不比较时区名）—— Asia/Shanghai 与 +08 是同一个偏移，
// 而 PRC、Asia/Chongqing 都是合法别名，比名字只会制造假告警。
func CheckSessionTimezone(ctx context.Context) {
	var offset string
	if err := db.DB.WithContext(ctx).Raw(`SELECT to_char(now(), 'OF')`).Scan(&offset).Error; err != nil {
		logger.L().Warn("无法读取数据库会话时区，日界一律以业务时区为准", zap.Error(err))
		return
	}
	want := BizNow().Format("-07")
	if strings.TrimSpace(offset) != want {
		logger.L().Warn("数据库会话时区与业务时区不一致：依赖 CURRENT_DATE 的旧查询会与门户统计错开一天",
			zap.String("db_offset", offset), zap.String("biz_offset", want),
			zap.String("fix", "把数据库会话时区设成与 LD_TZ 相同，或改用本文件的 DayRange/MonthRange"))
	}
}
