package audit

import (
	"context"
	"fmt"
	"strings"
	"time"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/service/logs"
)

// Filter 是审计日志查询条件。
type Filter struct {
	Action  string
	AdminID uint64
	Keyword string
	From    time.Time
	To      time.Time
}

// Item 是审计明细行（Actor 为操作人用户名，缺失时退化为 #id）。
type Item struct {
	ID         uint64     `json:"id"`
	Action     string     `json:"action"`
	AdminID    uint64     `json:"admin_id"`
	Actor      string     `json:"actor"`
	TargetType string     `json:"target_type"`
	TargetID   string     `json:"target_id"`
	Detail     model.JSON `json:"detail"`
	CreatedAt  time.Time  `json:"created_at"`
}

// ActionStat 是动作聚合项。
type ActionStat struct {
	Action string `json:"action"`
	Value  int64  `json:"value"`
}

// List 分页查询审计明细（动作/操作人/关键字/时间组合过滤）。
func List(ctx context.Context, f Filter, page, size int) ([]Item, int64, error) {
	cond, args := auditWhere(f)

	var total int64
	if err := db.DB.WithContext(ctx).
		Raw("SELECT COUNT(*) FROM admin_audit a WHERE 1=1"+cond, args...).Scan(&total).Error; err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return []Item{}, 0, nil
	}

	q := `SELECT a.id, a.action, a.admin_id, a.target_type, a.target_id, a.detail, a.created_at,
	             COALESCE(NULLIF(u.username, ''), '#' || a.admin_id::text) AS actor
	        FROM admin_audit a LEFT JOIN users u ON u.id = a.admin_id
	       WHERE 1=1` + cond + ` ORDER BY a.id DESC LIMIT ? OFFSET ?`
	// 用 int64 算再夹紧：page 已在入口被 MaxPage 拦住，这里是第二道防线 ——
	// 负数 OFFSET 会让 PG 报 2201X，最终折成 500。
	offset := (int64(page) - 1) * int64(size)
	if offset < 0 {
		offset = 0
	}
	args = append(args, size, offset)

	items := []Item{}
	if err := db.DB.WithContext(ctx).Raw(q, args...).Scan(&items).Error; err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// Actions 返回库内实际出现过的动作集合（含出现次数），供筛选器数据驱动。
func Actions(ctx context.Context) ([]ActionStat, error) {
	out := []ActionStat{}
	err := db.DB.WithContext(ctx).
		Raw(`SELECT action, COUNT(*) AS value FROM admin_audit GROUP BY action ORDER BY value DESC`).
		Scan(&out).Error
	return out, err
}

// auditWhere 构造 WHERE 子句（审计条件独立于调用日志条件，避免误用）。
func auditWhere(f Filter) (string, []any) {
	var sb strings.Builder
	args := make([]any, 0, 8)
	if f.Action != "" {
		sb.WriteString(" AND a.action = ?")
		args = append(args, f.Action)
	}
	if f.AdminID > 0 {
		sb.WriteString(" AND a.admin_id = ?")
		args = append(args, f.AdminID)
	}
	if f.Keyword != "" {
		kw := "%" + f.Keyword + "%"
		sb.WriteString(" AND (a.target_type ILIKE ? OR a.target_id ILIKE ?)")
		args = append(args, kw, kw)
	}
	if !f.From.IsZero() {
		sb.WriteString(" AND a.created_at >= ?")
		args = append(args, f.From)
	}
	if !f.To.IsZero() {
		sb.WriteString(" AND a.created_at < ?")
		args = append(args, f.To)
	}
	return sb.String(), args
}

// CSV 生成审计导出 CSV（时间/操作人/动作/目标类型/目标ID/详情）。
func CSV(items []Item) string {
	var sb strings.Builder
	sb.WriteString("时间,操作人,动作,目标类型,目标ID,详情\n")
	for _, it := range items {
		fmt.Fprintf(&sb, "%s,%s,%s,%s,%s,%s\n",
			it.CreatedAt.Format("2006-01-02 15:04:05"),
			logs.Esc(it.Actor), logs.Esc(it.Action), logs.Esc(it.TargetType), logs.Esc(it.TargetID), logs.Esc(string(it.Detail)))
	}
	return sb.String()
}
