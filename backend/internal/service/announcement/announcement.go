// Package announcement 提供站点公告时间线的读取与「服务端已读」锚定（S10）。
// 公告数据即 `system_config.announcements` 键；已读锚为公告 id（键 `id:<id>`）。
//
// 读模型只有一份：`ItemView` = 配置条目 + 当前用户的已读状态。
// 未读数**不再单独计算**（旧实现的 `UnreadCount` 让同一次请求把已读表查两遍，
// 且接口同时下发 `unread` 与条目列表两处表达）—— 前端一律从 `items[].read` 派生。
package announcement

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/model"
	"lingdong-api/internal/service/stats"
)

// Item 是单条公告（配置侧模型，管理台编辑的就是它）。
type Item struct {
	ID          int64  `json:"id"`
	Content     string `json:"content"`
	PublishDate string `json:"publishDate"`
	Type        string `json:"type"`
	Extra       string `json:"extra"`
}

// ItemView 是门户读模型：条目 + 当前用户的已读状态。
//
// 必须与 Item 分开：管理台保存的是 `announcements` 配置键，若把 `read` 塞进 Item，
// 管理员保存配置时会把「某个用户的已读状态」写回 system_config。
type ItemView struct {
	Item
	Read bool `json:"read"`
}

// Config 是公告键结构。
type Config struct {
	Enabled bool   `json:"enabled"`
	Items   []Item `json:"items"`
}

// Load 读取当前公告配置并按时效倒序返回（无效时间戳的条目排在末尾，保持原始相对顺序）。
func Load() Config {
	var c Config
	_ = conf.System().Unmarshal("announcements", &c)
	c.Items = sortByDate(c.Items)
	return c
}

// sortByDate 按发布时间倒序排列；解析失败的时间戳视为零值排在最后。
func sortByDate(items []Item) []Item {
	type row struct {
		it  Item
		ts  time.Time
		idx int
	}
	rows := make([]row, len(items))
	for i, it := range items {
		rows[i] = row{it: it, ts: parseDate(it.PublishDate), idx: i}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.ts.Equal(b.ts) {
			return a.idx < b.idx
		}
		return a.ts.After(b.ts)
	})
	out := make([]Item, len(rows))
	for i, r := range rows {
		out[i] = r.it
	}
	return out
}

// parseDate 依次按 RFC3339、本地无时区格式解析发布时间。
//
// 无时区串（"2026-10-20T09:00"）必须按**业务时区**解释，不能按服务器 time.Local：
// 容器 TZ=UTC 而 LD_TZ=Asia/Shanghai 时，同一个串会落在不同的绝对时刻上，排序随之错位 ——
// 而门户首页横幅取的正是「排序后的第一条未读」。
func parseDate(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, stats.Location()); err == nil {
			return t
		}
	}
	return time.Time{}
}

// Key 是公告的已读锚（无有效 id 时返回空串，表示不可锚定）。
func Key(it Item) string {
	if it.ID <= 0 {
		return ""
	}
	return fmt.Sprintf("id:%d", it.ID)
}

// ListFor 读取配置并标注该用户的已读状态（门户唯一读入口）。
//
// 关闭公告时**不查库**：既省一次往返，也保证「没开公告」不会因为已读表里有脏行而报错。
func ListFor(ctx context.Context, gdb *gorm.DB, userID uint64) (Config, []ItemView, error) {
	cfg := Load()
	if !cfg.Enabled {
		return Config{Enabled: false, Items: []Item{}}, []ItemView{}, nil
	}
	views, err := Annotate(ctx, gdb, userID, cfg.Items)
	if err != nil {
		return cfg, nil, err
	}
	return cfg, views, nil
}

// Annotate 给条目打上该用户的已读标记。
//
// 一次 Pluck 出全部命中的锚，不做逐条查询（公告上限 100 条）。
// `ID <= 0` 的条目恒为已读：它们没有锚、`MarkRead` 会直接跳过，
// 若照抄「未命中即未读」，徽标会永远清不掉 —— 用户点了也写不进已读。
func Annotate(ctx context.Context, gdb *gorm.DB, userID uint64, items []Item) ([]ItemView, error) {
	views := make([]ItemView, 0, len(items))
	keys := make([]string, 0, len(items))
	for _, it := range items {
		if k := Key(it); k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 || userID == 0 {
		for _, it := range items {
			views = append(views, ItemView{Item: it, Read: Key(it) == ""})
		}
		return views, nil
	}

	var read []string
	err := gdb.WithContext(ctx).Model(&model.AnnouncementRead{}).
		Where("user_id = ? AND announcement IN ?", userID, keys).
		Pluck("announcement", &read).Error
	if err != nil {
		return nil, fmt.Errorf("announcement: 查询已读失败: %w", err)
	}
	seen := make(map[string]struct{}, len(read))
	for _, k := range read {
		seen[k] = struct{}{}
	}
	for _, it := range items {
		k := Key(it)
		_, ok := seen[k]
		views = append(views, ItemView{Item: it, Read: k == "" || ok})
	}
	return views, nil
}

// MarkReadAll 把「当前配置里的全部公告」标记为已读。
//
// 键由服务端从配置派生，**不接受客户端传入**：客户端提交 keys 等于让它回答
// 「哪些公告存在」，既多一条真相源，又能往已读表里写任意 id。
func MarkReadAll(ctx context.Context, gdb *gorm.DB, userID uint64, items []Item) error {
	if userID == 0 {
		// 防御：user_id=0 会让所有匿名请求共享同一行已读记录。
		return errors.New("announcement: 未登录用户不可写已读")
	}
	keys := make([]string, 0, len(items))
	for _, it := range items {
		if k := Key(it); k != "" {
			keys = append(keys, k)
		}
	}
	return MarkRead(ctx, gdb, userID, keys)
}

// MarkRead 幂等写入已读记录（键需形如 `id:<id>`）。
func MarkRead(ctx context.Context, gdb *gorm.DB, userID uint64, keys []string) error {
	rows := make([]model.AnnouncementRead, 0, len(keys))
	seen := map[string]bool{}
	now := time.Now()
	for _, k := range keys {
		if k == "" || !strings.HasPrefix(k, "id:") || seen[k] {
			continue
		}
		seen[k] = true
		rows = append(rows, model.AnnouncementRead{UserID: userID, Announcement: k, CreatedAt: now})
	}
	if len(rows) == 0 {
		return nil
	}
	err := gdb.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&rows).Error
	if err != nil {
		return fmt.Errorf("announcement: 写入已读失败: %w", err)
	}
	return nil
}
