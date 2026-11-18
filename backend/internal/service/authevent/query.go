package authevent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/service/logs"
)

// Filter 是认证事件的查询条件。
//
// 只收「排障时手上真的会有的」维度：事件名、用户、IP、结果、账号前缀，再叠时间窗。
// `Account` 用**前缀**匹配而不是 LIKE '%x%'：撞库者填的账号是没有规律的随机串，
// 中间匹配既用不上索引又几乎搜不中；而运营记账号永远记得开头。
type Filter struct {
	Event   string
	Result  string
	UID     uint64
	IP      string
	Account string
	From    time.Time
	To      time.Time
}

// Item 是认证事件明细行。
//
// **不含 uid 之外的任何用户资料**（邮箱/手机/昵称）：审计页是排障工具，
// 把 PII 再复制一份到一张保留 90 天的表里，只会扩大泄漏面。
type Item struct {
	ID        uint64     `json:"id"`
	UID       uint64     `json:"uid"`
	Account   string     `json:"account"`
	Event     string     `json:"event"`
	Label     string     `json:"label"`
	Result    string     `json:"result"`
	Reason    string     `json:"reason"`
	IP        string     `json:"ip"`
	UA        string     `json:"ua"`
	SessionID uint64     `json:"session_id"`
	Detail    model.JSON `json:"detail"`
	CreatedAt time.Time  `json:"created_at"`
}

// EventOption 是筛选器里的事件选项（label 为中文名，前端不再维护第二份映射）。
type EventOption struct {
	Event string `json:"event"`
	Label string `json:"label"`
}

// eventLabels 是事件的中文名。**与白名单同源放在本包**：
// 放到前端就意味着新增事件时有两处要改，而漏改的表现只是下拉里多出一个英文 key。
var eventLabels = map[string]string{
	EventRegister:           "注册",
	EventLogin:              "登录成功",
	EventLoginFailed:        "登录失败",
	EventLogout:             "登出",
	EventSessionRevoke:      "撤销会话",
	EventSessionRevokeAll:   "撤销全部会话",
	EventPasswordChange:     "修改密码",
	EventPasswordReset:      "找回密码",
	EventPasswordResetAdmin: "管理员重置密码",
	EventContactBind:        "绑定联系方式",
	EventCaptchaFailed:      "人机验证失败",
	EventRateLimited:        "触发限流",
	EventInviteRejected:     "邀请码被拒",
	EventLockDegraded:       "锁定降级",
	EventAuthUnavailable:    "认证不可用",
	EventLoginMFARequired:   "登录需二次验证",
	EventLoginMFAFailed:     "二次验证失败",
	EventLoginMFASuccess:    "二次验证通过",
	EventMFASetup:           "生成 TOTP 密钥",
	EventMFAEnable:          "开启二次验证",
	EventMFADisable:         "关闭二次验证",
	EventBackupCodeUsed:     "使用备份码",
	EventKeyIssue:           "签发 API KEY",
	EventKeyRevoke:          "吊销 API KEY",
	EventKeyAutoRenew:       "托管密钥自动续期",
	EventKeyAutoExpire:      "托管密钥闲置失效",
	EventKeyLimitRaised:     "托管密钥上限上调",
	EventKeyAdminDisable:    "平台禁用 API KEY",
	EventKeyAdminEnable:     "平台启用 API KEY",
	EventKeySelfRotate:      "接入方自举轮换密钥",
}

// Options 返回筛选器选项（按白名单顺序，避免 map 迭代导致下拉每次刷新都换序）。
func Options() []EventOption {
	order := []string{
		EventRegister, EventLogin, EventLoginFailed, EventLogout,
		EventSessionRevoke, EventSessionRevokeAll,
		EventPasswordChange, EventPasswordReset, EventPasswordResetAdmin, EventContactBind,
		EventLoginMFARequired, EventLoginMFASuccess, EventLoginMFAFailed,
		EventMFASetup, EventMFAEnable, EventMFADisable, EventBackupCodeUsed,
		EventCaptchaFailed, EventRateLimited, EventInviteRejected, EventLockDegraded, EventAuthUnavailable,
		EventKeyIssue, EventKeyRevoke, EventKeyAutoRenew, EventKeyAutoExpire, EventKeyLimitRaised,
		EventKeyAdminDisable, EventKeyAdminEnable, EventKeySelfRotate,
	}
	out := make([]EventOption, 0, len(order))
	for _, ev := range order {
		out = append(out, EventOption{Event: ev, Label: eventLabels[ev]})
	}
	return out
}

// List 分页查询认证事件（按 id 倒序，与时间倒序等价且天然稳定）。
func List(ctx context.Context, f Filter, page, size int) ([]Item, int64, error) {
	cond, args := where(f)

	var total int64
	if err := db.DB.WithContext(ctx).
		Raw("SELECT COUNT(*) FROM auth_events e WHERE 1=1"+cond, args...).Scan(&total).Error; err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return []Item{}, 0, nil
	}

	q := `SELECT e.id, e.uid, e.account, e.event, e.result, e.reason, e.ip, e.ua,
	             e.session_id, e.detail, e.created_at
	        FROM auth_events e WHERE 1=1` + cond + ` ORDER BY e.id DESC LIMIT ? OFFSET ?`
	// 用 int64 算再夹紧：page 已在入口被 MaxPage 拦住，这里是第二道防线 ——
	// 负数 OFFSET 会让 PG 报 2201X，最终折成 500。
	offset := (int64(page) - 1) * int64(size)
	if offset < 0 {
		offset = 0
	}
	args = append(args, size, offset)

	items := make([]Item, 0, size)
	if err := db.DB.WithContext(ctx).Raw(q, args...).Scan(&items).Error; err != nil {
		return nil, 0, err
	}
	for i := range items {
		items[i].Label = eventLabels[items[i].Event]
	}
	return items, total, nil
}

// Export 取导出用的数据（按当前筛选，最多 limit 行）。
func Export(ctx context.Context, f Filter, limit int) ([]Item, error) {
	cond, args := where(f)
	q := `SELECT e.id, e.uid, e.account, e.event, e.result, e.reason, e.ip, e.ua,
	             e.session_id, e.detail, e.created_at
	        FROM auth_events e WHERE 1=1` + cond + ` ORDER BY e.id DESC LIMIT ?`
	args = append(args, limit)

	items := make([]Item, 0, 512)
	if err := db.DB.WithContext(ctx).Raw(q, args...).Scan(&items).Error; err != nil {
		return nil, err
	}
	for i := range items {
		items[i].Label = eventLabels[items[i].Event]
	}
	return items, nil
}

// where 构造 WHERE 子句。
//
// 表别名固定 `e`：与 List/Export 的 SQL 共用同一份条件，避免两处写法漂移
// （「列表能看到但导不出来」就是漂移的典型症状）。
func where(f Filter) (string, []any) {
	var sb strings.Builder
	args := make([]any, 0, 8)
	if f.Event != "" {
		sb.WriteString(" AND e.event = ?")
		args = append(args, f.Event)
	}
	if f.Result != "" {
		sb.WriteString(" AND e.result = ?")
		args = append(args, f.Result)
	}
	if f.UID > 0 {
		sb.WriteString(" AND e.uid = ?")
		args = append(args, f.UID)
	}
	if f.IP != "" {
		sb.WriteString(" AND e.ip = ?")
		args = append(args, f.IP)
	}
	if f.Account != "" {
		sb.WriteString(" AND e.account LIKE ?")
		args = append(args, escapeLikePrefix(f.Account)+"%")
	}
	if !f.From.IsZero() {
		sb.WriteString(" AND e.created_at >= ?")
		args = append(args, f.From)
	}
	if !f.To.IsZero() {
		sb.WriteString(" AND e.created_at < ?")
		args = append(args, f.To)
	}
	return sb.String(), args
}

// escapeLikePrefix 转义 LIKE 的通配符。
//
// 不转义的话，输入 `%` 就变成「匹配所有人」——筛选器看起来正常、结果全错，
// 而这类「静默放宽」在审计页上等于把一次定向排查变成全量翻页。
func escapeLikePrefix(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// CSV 生成认证事件导出 CSV（时间/UID/账号/事件/结果/原因/IP/UA/会话）。
//
// 复用 logs.Esc 而不是另写一份：CSV 注入（字段里含逗号/引号/换行）只需要一种正确做法。
func CSV(items []Item) string {
	var sb strings.Builder
	sb.WriteString("时间,UID,账号,事件,结果,原因,IP,User-Agent,会话ID\n")
	for _, it := range items {
		fmt.Fprintf(&sb, "%s,%d,%s,%s,%s,%s,%s,%s,%d\n",
			it.CreatedAt.Format("2006-01-02 15:04:05"),
			it.UID,
			logs.Esc(it.Account), logs.Esc(it.Event), logs.Esc(it.Result), logs.Esc(it.Reason),
			logs.Esc(it.IP), logs.Esc(it.UA), it.SessionID)
	}
	return sb.String()
}
