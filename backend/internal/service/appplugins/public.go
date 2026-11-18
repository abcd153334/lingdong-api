package appplugins

// 对外只读面：GET /api/apps/:slug/p/:plugin/:entity。
//
// 这是整个插件域**唯一**一条免凭证入口，所以它的每个"不对外"都必须返回同一个
// 404（`plugin_not_found`）：区分"没这个插件 / 没装 / 停用了 / 没开对外 / 目录下架"
// 就是一份给匿名调用方的枚举面。要排障请看管理台，那里什么都能看到。

import (
	"context"
	"encoding/json"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
)

// PublicMaxPageSize 是对外列表的单页上限。
//
// 比管理台（100）更小：免凭证面没有"谁在翻页"的记录，一页 500 条等于把整张表
// 让匿名方一次拖走。20 条默认值能让终端首屏与一次续页都够用。
const (
	PublicDefaultPageSize = 20
	PublicMaxPageSize     = 50
)

// PublicItem 是对外的一条记录。刻意不含 version/lock_version：
// 乐观锁只在写入时才有意义，对外读出去只会让人以为"这个数字要带回来"。
type PublicItem struct {
	RecordKey string         `json:"record_key"`
	Data      map[string]any `json:"data"`
	UpdatedAt string         `json:"updated_at"`
}

// PublicRef 是插件/对象的自述（客户端据此确认自己读的是哪一份数据）。
type PublicRef struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// PublicPage 是对外读取的唯一响应形状。
type PublicPage struct {
	Plugin   PublicRef    `json:"plugin"`
	Entity   PublicRef    `json:"entity"`
	Items    []PublicItem `json:"items"`
	Total    int64        `json:"total"`
	Page     int          `json:"page"`
	PageSize int          `json:"page_size"`
}

// PublicList 读一个应用下某插件的某个数据对象（只回白名单字段）。
//
// 五道闸，顺序就是失败代价从高到低：
//  1. 这个应用装了这个插件（否则连"有没有"都不说）；
//  2. 安装态是启用（停用=冻结，卸载=摘掉入口，两者都不对外）；
//  3. 该安装显式打开了对外只读（`public_enabled`）；
//  4. 目录仍上架（下架 = 不再推荐，但对已开的对外面也一并收紧 —— 否则
//     "下架"只是管理台的一行字）；
//  5. 对象声明了 public 白名单（没声明 = 不公开，哪怕开关是开的）。
func PublicList(ctx context.Context, gdb *gorm.DB, appID uint64, key, entity string, page, pageSize int) (*PublicPage, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = PublicDefaultPageSize
	}
	if pageSize > PublicMaxPageSize {
		pageSize = PublicMaxPageSize
	}

	inst, err := getInstall(ctx, gdb, appID, key)
	if err != nil {
		return nil, ErrNotFound
	}
	if inst.Status != model.AppPluginInstallEnabled || !inst.PublicEnabled {
		return nil, ErrNotFound
	}
	cat, m, err := Get(ctx, gdb, key)
	if err != nil {
		return nil, ErrNotFound
	}
	if cat.Status != model.AppPluginStatusEnabled {
		return nil, ErrNotFound
	}

	e, ok := m.EntityByKey(entity)
	if !ok || len(e.Public) == 0 {
		return nil, ErrEntityNotFound
	}
	// 白名单里可能有"升级时已删掉"的字段（校验只在导入时跑）：
	// 这里按现 manifest 过滤，投影出来的 data 永远只有仍然存在的列。
	fields := make([]string, 0, len(e.Public))
	for _, k := range e.Public {
		if f, ok := e.field(k); ok {
			fields = append(fields, f.Key)
		}
	}
	if len(fields) == 0 {
		return nil, ErrEntityNotFound
	}

	q := gdb.WithContext(ctx).Model(&model.AppPluginRecord{}).
		Where("app_id = ? AND plugin_key = ? AND entity = ?", appID, key, e.Key)
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, err
	}
	var rows []model.AppPluginRecord
	// 与管理台同序（id DESC = 最新在前）：两边顺序不一致会让人以为"对外少了几条"。
	if err := q.Order("id DESC").Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&rows).Error; err != nil {
		return nil, err
	}

	items := make([]PublicItem, 0, len(rows))
	for i := range rows {
		all, _ := DecodeObject(json.RawMessage(rows[i].Data))
		data := make(map[string]any, len(fields))
		for _, k := range fields {
			if v, ok := all[k]; ok {
				data[k] = v
			}
		}
		items = append(items, PublicItem{
			RecordKey: rows[i].RecordKey,
			Data:      data,
			UpdatedAt: rows[i].UpdatedAt.Format("2006-01-02 15:04:05"),
		})
	}
	return &PublicPage{
		Plugin:   PublicRef{Key: cat.Key, Name: m.Name, Version: cat.Version},
		Entity:   PublicRef{Key: e.Key, Name: e.Name},
		Items:    items,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	}, nil
}
