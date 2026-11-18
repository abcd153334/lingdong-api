package app

import (
	"context"
	"strings"
	"time"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
)

// NoticeInput 是公告的写入形态（创建与更新同一份）。
//
// Enabled **只在创建时生效**：上下架是独立动作，走 SetNoticeEnabled。
// 让全量更新也写这一列，会出现"运营 A 刚下架、运营 B 的旧弹窗一保存又上架"
// 的跨端点覆盖（lost update），而 B 的界面上根本没有这个开关。
type NoticeInput struct {
	Title    string
	Content  string
	Level    string
	Enabled  bool
	StartsAt *time.Time
	EndsAt   *time.Time
}

// noticeActiveWhere 是「此刻生效」的**唯一** SQL 判据；
// ActiveOf 是同一判据的 Go 版。两处必须同义（见 notice_active_test.go）。
//
// 占位符顺序固定为 (enabled, now, now)：调用方只负责拼 app_id 那一段，
// 不再出现"同一段字符串、两处各自记参数顺序"。仅包内使用，不跨包导出 ——
// 一旦有人往里面加 `?`，所有调用点的参数序号都会静默错位。
const noticeActiveWhere = "enabled = ? AND (starts_at IS NULL OR starts_at <= ?) AND (ends_at IS NULL OR ends_at >= ?)"

// publicNoticeOrder 让"严重"公告优先占用有限的对外条数。
//
// 只按 created_at 倒序的后果：一个应用有 25 条生效公告时，创建得早的 critical
// 会被后建的 5 条 info 挤出对外窗口 —— 紧急通知静默消失，而管理台的
// notice_count 仍显示 25。
const publicNoticeOrder = "CASE level WHEN 'critical' THEN 0 WHEN 'warn' THEN 1 ELSE 2 END, created_at DESC, id DESC"

// ActiveOf 判断一条公告"此刻是否应当下发"。
//
// 判据是 `enabled AND 落在窗口内`，不是任何状态机：定时上架只能靠窗口表达，
// 靠定时任务改状态会引入"到点了但任务没跑"的静默失败。
func ActiveOf(n model.AppNotice, now time.Time) bool {
	return n.Enabled &&
		(n.StartsAt == nil || !n.StartsAt.After(now)) &&
		(n.EndsAt == nil || !n.EndsAt.Before(now))
}

// NormalizeNotice 去空白、归一 level、校验时间窗；返回错误文案（"" = 通过）。
func NormalizeNotice(in NoticeInput) (NoticeInput, string) {
	in.Title = strings.TrimSpace(in.Title)
	in.Content = strings.TrimSpace(in.Content)
	in.Level = strings.TrimSpace(strings.ToLower(in.Level))
	if in.Level == "" {
		in.Level = model.AppNoticeLevelInfo
	}

	switch {
	case in.Title == "":
		return in, "公告标题必填"
	case !maxRunes(in.Title, 100):
		return in, "公告标题不超过 100 个字符"
	case in.Content == "":
		return in, "公告内容必填"
	case !maxRunes(in.Content, 2000):
		return in, "公告内容不超过 2000 个字符"
	case !model.AppNoticeLevels[in.Level]:
		return in, "公告级别只能是 info / warn / critical"
	}
	if in.StartsAt != nil && in.EndsAt != nil && !in.EndsAt.After(*in.StartsAt) {
		return in, "结束时间必须晚于开始时间"
	}
	return in, ""
}

// ListNotices 分页列出某应用的全部公告（含已停用），按创建时间倒序。
func ListNotices(ctx context.Context, gdb *gorm.DB, appID uint64, offset, limit int) ([]model.AppNotice, int64, error) {
	q := gdb.WithContext(ctx).Model(&model.AppNotice{}).Where("app_id = ?", appID)
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	rows := make([]model.AppNotice, 0, limit)
	if err := q.Order("created_at DESC, id DESC").Offset(offset).Limit(limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// CreateNotice 新建公告。
//
// 用 map 插入而不是"结构体 Create + 补写 enabled"：enabled 带 `default:true`，
// GORM 对「零值 + default」的列会用列默认值替换，于是"新建一条停用的公告"
// 必须再发一条 UPDATE。那条 UPDATE 失败时接口回 500、库里却留着一条
// **已启用并对外下发**的公告 —— 运营只会以为没建成，再建一条。
//
// 注意：map 分支**不会**自动填 created_at/updated_at（GORM 的时间填充只在
// 结构体分支），必须显式给，否则两列落 NULL，列表按 created_at 排序会乱。
func CreateNotice(ctx context.Context, gdb *gorm.DB, appID uint64, in NoticeInput) (*model.AppNotice, error) {
	now := gdb.NowFunc()
	row := map[string]any{
		"app_id": appID, "title": in.Title, "content": in.Content, "level": in.Level,
		"enabled": in.Enabled, "starts_at": in.StartsAt, "ends_at": in.EndsAt,
		"created_at": now, "updated_at": now,
	}
	if err := gdb.WithContext(ctx).Model(&model.AppNotice{}).Create(row).Error; err != nil {
		return nil, err
	}
	return &model.AppNotice{
		ID: idOfInserted(row), AppID: appID, Title: in.Title, Content: in.Content,
		Level: in.Level, Enabled: in.Enabled, StartsAt: in.StartsAt, EndsAt: in.EndsAt,
		CreatedAt: now, UpdatedAt: now,
	}, nil
}

// UpdateNotice 全量更新一条公告（上下架不在这里改，见 SetNoticeEnabled）。
//
// 用 map 而不是 Save：Save 会把零值的 StartsAt/EndsAt（清空时间窗）写回，
// 也会覆盖 AppID 这类不该由本方法触碰的列。
//
// 用 RowsAffected 判存在，不再先 COUNT：先查后写之间被并发删除时，
// 旧写法会返回成功（影响 0 行）并记一条"改了不存在的公告"的审计。
func UpdateNotice(ctx context.Context, gdb *gorm.DB, appID, id uint64, in NoticeInput) error {
	res := gdb.WithContext(ctx).Model(&model.AppNotice{}).
		Where("id = ? AND app_id = ?", id, appID).
		Updates(map[string]any{
			"title": in.Title, "content": in.Content, "level": in.Level,
			"starts_at": in.StartsAt, "ends_at": in.EndsAt,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNoticeNotFound
	}
	return nil
}

// SetNoticeEnabled 只改上下架开关。
func SetNoticeEnabled(ctx context.Context, gdb *gorm.DB, appID, id uint64, enabled bool) error {
	res := gdb.WithContext(ctx).Model(&model.AppNotice{}).
		Where("id = ? AND app_id = ?", id, appID).Update("enabled", enabled)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNoticeNotFound
	}
	return nil
}

// DeleteNotice 物理删除（公告不涉及引用完整性，删前由 controller 记入审计）。
func DeleteNotice(ctx context.Context, gdb *gorm.DB, appID, id uint64) error {
	res := gdb.WithContext(ctx).Model(&model.AppNotice{}).Where("id = ? AND app_id = ?", id, appID).Delete(&model.AppNotice{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNoticeNotFound
	}
	return nil
}

// EffectiveNotices 返回"此刻应当下发"的公告：启用且在时间窗内。
func EffectiveNotices(ctx context.Context, gdb *gorm.DB, appID uint64, now time.Time, limit int) ([]model.AppNotice, error) {
	rows := make([]model.AppNotice, 0, limit)
	err := gdb.WithContext(ctx).
		Where("app_id = ?", appID).
		Where(noticeActiveWhere, true, now, now).
		Order(publicNoticeOrder).
		Limit(limit).Find(&rows).Error
	return rows, err
}

// ActiveCountsByApp 批量统计每个应用此刻生效的公告数（列表页用，禁止逐行查）。
func ActiveCountsByApp(ctx context.Context, gdb *gorm.DB, appIDs []uint64, now time.Time) (map[uint64]int64, error) {
	out := make(map[uint64]int64, len(appIDs))
	if len(appIDs) == 0 {
		return out, nil
	}
	var rows []struct {
		AppID uint64
		Cnt   int64
	}
	if err := gdb.WithContext(ctx).
		Table("app_notices").
		Select("app_id, COUNT(*) AS cnt").
		Where("app_id IN ?", appIDs).
		Where(noticeActiveWhere, true, now, now).
		Group("app_id").Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.AppID] = r.Cnt
	}
	return out, nil
}

// idOfInserted 取 map 插入后 RETURNING 回填的自增 id。
//
// 不用裸断言：驱动在个别路径下会把整数扫成 int64，裸断言直接 panic。
func idOfInserted(row map[string]any) uint64 {
	switch v := row["id"].(type) {
	case uint64:
		return v
	case int64:
		return uint64(v)
	case int:
		return uint64(v)
	default:
		return 0
	}
}

// NoticeView 是对外公告视图。
type NoticeView struct {
	ID       uint64     `json:"id"`
	Title    string     `json:"title"`
	Content  string     `json:"content"`
	Level    string     `json:"level"`
	StartsAt *time.Time `json:"starts_at"`
	EndsAt   *time.Time `json:"ends_at"`
}

// ViewsOf 把公告行投影成对外视图（数组恒非 nil）。
func ViewsOf(rows []model.AppNotice) []NoticeView {
	out := make([]NoticeView, 0, len(rows))
	for _, r := range rows {
		out = append(out, NoticeView{
			ID: r.ID, Title: r.Title, Content: r.Content, Level: r.Level,
			StartsAt: r.StartsAt, EndsAt: r.EndsAt,
		})
	}
	return out
}
