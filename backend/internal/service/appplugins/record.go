package appplugins

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/pgerr"
)

// RecordItem 是记录对外的形状。
//
// 注意它**不含内部 id**：对外身份只有 record_key。内部 id 只用于稳定排序（id DESC），
// 下发它会让前端把两个身份混用，进而出现"用 id 拼 URL 404"的经典问题。
type RecordItem struct {
	RecordKey string         `json:"record_key"`
	Data      map[string]any `json:"data"`
	Version   int            `json:"version"`
	CreatedAt string         `json:"created_at"`
	UpdatedAt string         `json:"updated_at"`
}

// keywordQuery 是关键词过滤的唯一实现：列表、导出计数、导出分批三处必须完全一致，
// 否则「导出当前筛选」与屏幕上的列表不是同一批行（运营会当成丢数据）。
//
// 只查两列：record_key（对外编号）与 unique_norm（业务唯一值，已 trim+lower）。
// 不碰 `data::text`：那是全表 TOAST 读放大，而且会把「备注里提过这个词」也算命中，
// 结果无法向使用者解释。
//
// 括号显式写死：GORM v2 会给每个 Where 表达式各自套括号（实测 SQL 是
// `... AND (record_key ILIKE $4 OR unique_norm ILIKE $5)`），但那是实现细节 ——
// 换成 `Or()` 链或拼 `gorm.Expr` 就会变成后半句脱离 app_id 作用域，
// 一次搜索读到别的应用的数据。作用域由 TestKeywordSearchCoversUniqueField 守。
func keywordQuery(q *gorm.DB, s string) *gorm.DB {
	s = strings.TrimSpace(s)
	if s == "" {
		return q
	}
	return q.Where("(record_key ILIKE ? OR unique_norm ILIKE ?)", "%"+s+"%", "%"+strings.ToLower(s)+"%")
}

// ListRecords 分页查记录。排序固定 id DESC（最新建的在前）：
// 允许按 data->>'x' 排序必然是全表 Sort + TOAST 读放大，v1 不做。
func ListRecords(ctx context.Context, gdb *gorm.DB, appID uint64, key, entity, keyword string, offset, limit int) ([]RecordItem, int64, error) {
	_, m, err := loadForRead(ctx, gdb, appID, key)
	if err != nil {
		return nil, 0, err
	}
	if _, ok := m.EntityByKey(entity); !ok {
		return nil, 0, ErrEntityNotFound
	}
	q := gdb.WithContext(ctx).Model(&model.AppPluginRecord{}).
		Where("app_id = ? AND plugin_key = ? AND entity = ?", appID, key, entity)
	q = keywordQuery(q, keyword)
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []model.AppPluginRecord
	if err := q.Order("id DESC").Offset(offset).Limit(limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	items := make([]RecordItem, 0, len(rows))
	for i := range rows {
		items = append(items, toItem(&rows[i]))
	}
	return items, total, nil
}

func toItem(r *model.AppPluginRecord) RecordItem {
	data, _ := DecodeObject(json.RawMessage(r.Data))
	if data == nil {
		data = map[string]any{}
	}
	return RecordItem{
		RecordKey: r.RecordKey, Data: data, Version: r.LockVersion,
		CreatedAt: r.CreatedAt.Format("2006-01-02 15:04:05"),
		UpdatedAt: r.UpdatedAt.Format("2006-01-02 15:04:05"),
	}
}

// Create 新建一条记录。
//
// 容量校验放在事务里、并在插入前做：`count` 与 `insert` 之间理论上仍有窗口，
// 但上限是"防跑飞"的软阀（多进一两条不影响正确性），为此加表级锁不值得。
func Create(ctx context.Context, gdb *gorm.DB, appID uint64, key, entity string, raw json.RawMessage) (*RecordItem, error) {
	_, m, err := loadForWrite(ctx, gdb, appID, key)
	if err != nil {
		return nil, err
	}
	e, ok := m.EntityByKey(entity)
	if !ok {
		return nil, ErrEntityNotFound
	}
	if !e.ActionAllowed(ActionCreate) {
		return nil, disabledErr("这个数据对象不允许新建")
	}
	values, err := DecodeData(raw)
	if err != nil {
		return nil, &InvalidError{Problems: []Problem{problem("data", "数据不是合法的 JSON 对象")}}
	}
	clean, ps := normalizeFields(e.Fields, values, "data.")
	if len(ps) > 0 {
		return nil, &InvalidError{Problems: ps}
	}
	payload, err := MustJSON(clean)
	if err != nil {
		return nil, err
	}
	row := &model.AppPluginRecord{
		AppID: appID, PluginKey: key, Entity: entity,
		RecordKey: NewRecordKey(), UniqueNorm: uniqueNormOf(e, clean),
		Data: model.JSON(payload), LockVersion: 1,
	}
	limit := int64(DefaultMaxRecords)
	if m.Limits != nil && m.Limits.MaxRecords > 0 {
		limit = int64(m.Limits.MaxRecords)
	}
	err = gdb.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var n int64
		if err := tx.Model(&model.AppPluginRecord{}).
			Where("app_id = ? AND plugin_key = ? AND entity = ?", appID, key, entity).
			Count(&n).Error; err != nil {
			return err
		}
		if n >= limit {
			return &LimitError{Max: limit}
		}
		return tx.Create(row).Error
	})
	if err != nil {
		if pgerr.IsUnique(err) {
			return nil, recordConflict(err)
		}
		return nil, err
	}
	item := toItem(row)
	return &item, nil
}

// Update 整条替换（不是 PATCH）：required 字段在更新时同样强制，
// 否则"建的时候必填、改的时候可以清空"会让数据在两条路径上不一致。
func Update(ctx context.Context, gdb *gorm.DB, appID uint64, key, entity, recordKey string, raw json.RawMessage, version int) (*RecordItem, error) {
	_, m, err := loadForWrite(ctx, gdb, appID, key)
	if err != nil {
		return nil, err
	}
	e, ok := m.EntityByKey(entity)
	if !ok {
		return nil, ErrEntityNotFound
	}
	if !e.ActionAllowed(ActionUpdate) {
		return nil, disabledErr("这个数据对象不允许修改")
	}
	values, err := DecodeData(raw)
	if err != nil {
		return nil, &InvalidError{Problems: []Problem{problem("data", "数据不是合法的 JSON 对象")}}
	}
	clean, ps := normalizeFields(e.Fields, values, "data.")
	if len(ps) > 0 {
		return nil, &InvalidError{Problems: ps}
	}
	payload, err := MustJSON(clean)
	if err != nil {
		return nil, err
	}
	var out model.AppPluginRecord
	err = gdb.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		q := tx.Model(&model.AppPluginRecord{}).
			Where("app_id = ? AND plugin_key = ? AND entity = ? AND record_key = ?",
				appID, key, entity, recordKey)
		if version > 0 {
			q = q.Where("lock_version = ?", version)
		}
		res := q.Updates(map[string]any{
			"data":         model.JSON(payload),
			"unique_norm":  uniqueNormOf(e, clean),
			"lock_version": gorm.Expr("lock_version + 1"),
		})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			// 分清"不存在"与"版本旧了"：前者让用户刷新列表，后者只刷新这一条。
			var n int64
			if err := tx.Model(&model.AppPluginRecord{}).
				Where("app_id = ? AND plugin_key = ? AND entity = ? AND record_key = ?",
					appID, key, entity, recordKey).Count(&n).Error; err != nil {
				return err
			}
			if n == 0 {
				return ErrRecordNotFound
			}
			return ErrVersionConflict
		}
		return tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("app_id = ? AND plugin_key = ? AND entity = ? AND record_key = ?",
				appID, key, entity, recordKey).Limit(1).Find(&out).Error
	})
	if err != nil {
		if pgerr.IsUnique(err) {
			return nil, recordConflict(err)
		}
		return nil, err
	}
	item := toItem(&out)
	return &item, nil
}

// Delete 硬删一条记录，并把删除前的值返回给调用方做审计快照。
//
// 为什么返回快照：记录是物理删除，审计是唯一的事后依据（同公告删除的做法）。
func Delete(ctx context.Context, gdb *gorm.DB, appID uint64, key, entity, recordKey string) (map[string]any, error) {
	_, m, err := loadForWrite(ctx, gdb, appID, key)
	if err != nil {
		return nil, err
	}
	e, ok := m.EntityByKey(entity)
	if !ok {
		return nil, ErrEntityNotFound
	}
	if !e.ActionAllowed(ActionDelete) {
		return nil, disabledErr("这个数据对象不允许删除")
	}
	var snapshot map[string]any
	err = gdb.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row model.AppPluginRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("app_id = ? AND plugin_key = ? AND entity = ? AND record_key = ?",
				appID, key, entity, recordKey).Limit(1).Find(&row).Error; err != nil {
			return err
		}
		if row.ID == 0 {
			return ErrRecordNotFound
		}
		snapshot, _ = DecodeObject(json.RawMessage(row.Data))
		return tx.Where("id = ?", row.ID).Delete(&model.AppPluginRecord{}).Error
	})
	if err != nil {
		return nil, err
	}
	if snapshot == nil {
		snapshot = map[string]any{}
	}
	return snapshot, nil
}

// ExportRow 是导出的一行（带内部 id 供 keyset 翻页；不下发给前端）。
type ExportRow struct {
	ID   uint64
	Data map[string]any
}

// ExportPlan 校验导出前置条件并返回实体定义 + 总行数。
//
// 三件事必须在这里一次判完：实体存在、声明了 export、行数在上限内。
// 导出是"一次请求搬走全部数据"，把上限判断放到流式写入中间会让用户
// 拿到一个半截 CSV —— 那比直接报错更糟。
func ExportPlan(ctx context.Context, gdb *gorm.DB, appID uint64, key, entity, keyword string) (*Entity, int64, error) {
	_, m, err := loadForRead(ctx, gdb, appID, key)
	if err != nil {
		return nil, 0, err
	}
	e, ok := m.EntityByKey(entity)
	if !ok {
		return nil, 0, ErrEntityNotFound
	}
	if !e.ActionAllowed(ActionExport) {
		return nil, 0, disabledErr("这个数据对象不允许导出")
	}
	q := gdb.WithContext(ctx).Model(&model.AppPluginRecord{}).
		Where("app_id = ? AND plugin_key = ? AND entity = ?", appID, key, entity)
	q = keywordQuery(q, keyword)
	var n int64
	if err := q.Count(&n).Error; err != nil {
		return nil, 0, err
	}
	if n > MaxExportRows {
		return nil, 0, &LimitError{Max: MaxExportRows, Export: true}
	}
	return e, n, nil
}

// ExportBatch 用 keyset 翻页取一批（id > afterID），避免 OFFSET 越翻越慢。
func ExportBatch(ctx context.Context, gdb *gorm.DB, appID uint64, key, entity, keyword string, afterID uint64, limit int) ([]ExportRow, error) {
	q := gdb.WithContext(ctx).Model(&model.AppPluginRecord{}).
		Where("app_id = ? AND plugin_key = ? AND entity = ?", appID, key, entity)
	q = keywordQuery(q, keyword)
	if afterID > 0 {
		q = q.Where("id > ?", afterID)
	}
	var rows []model.AppPluginRecord
	if err := q.Order("id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]ExportRow, 0, len(rows))
	for i := range rows {
		data, _ := DecodeObject(json.RawMessage(rows[i].Data))
		if data == nil {
			data = map[string]any{}
		}
		out = append(out, ExportRow{ID: rows[i].ID, Data: data})
	}
	return out, nil
}

// recordConflict 把 23505 收敛成一句人话。两个唯一索引的语义不同：
// 撞 record_key 说明算法出错（几乎不可能），撞 unique_norm 才是"值被占用"。
func recordConflict(err error) error {
	if name, ok := pgerr.Unique(err); ok && name == "uq_plugin_records_key" {
		return disabledErr("记录编号冲突，请重试")
	}
	return ErrRecordConflict
}

// CellText 把单元格渲染成 CSV 里的文本。
//
// bool 写成"是/否"而不是 true/false：导出的读者是运营，不是程序；
// 而其余类型原样输出（数字不带千分位，日期已是 YYYY-MM-DD）。
func CellText(f *Field, v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "是"
		}
		return "否"
	case json.Number:
		return t.String()
	default:
		return fmt.Sprint(t)
	}
}
