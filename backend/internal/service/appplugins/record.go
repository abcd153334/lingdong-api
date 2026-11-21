package appplugins

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/pagination"
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
// 通配符必须转义：用户搜 `a_b` 时 `_` 在 LIKE 里是"任意一个字符"，
// 不转义会命中 `axb`（搜索框里写着下划线、结果却给出不相干的行）。
//
// 括号显式写死：GORM v2 会给每个 Where 表达式各自套括号，但那是实现细节 ——
// 换成 `Or()` 链或拼 `gorm.Expr` 就会变成后半句脱离 app_id 作用域，
// 一次搜索读到别的应用的数据。作用域由 TestKeywordSearchCoversUniqueField 守。
func keywordQuery(q *gorm.DB, s string) *gorm.DB {
	s = strings.TrimSpace(s)
	if s == "" {
		return q
	}
	pat := "%" + escapeLike(s) + "%"
	return q.Where(`(record_key ILIKE ? ESCAPE '\' OR unique_norm ILIKE ? ESCAPE '\')`, pat, strings.ToLower(pat))
}

// escapeLike 转义 LIKE 的三个元字符。反斜杠必须**先**替换，否则会把刚加上的
// 转义符再转义一遍（`%` → `\%` → `\\%`）。
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// ListQuery 是列表与导出**共用**的查询条件。
//
// 为什么合成一个结构体：筛选条件如果只接在列表上，"导出当前筛选"就会变成
// "导出全部"，而运营只会觉得数据丢了。BuildQuery 是唯一拼装点。
type ListQuery struct {
	Keyword   string
	SortField string
	Desc      bool
	// Filters 的 key 已过白名单，value 保留原始形态（区间是 `a..b`）。
	Filters map[string]string
}

// QueryError 是查询参数不合法：controller 映射成 400，消息直接给管理员看
// （"不支持按 xx 排序"比"参数错误"可行动）。
type QueryError struct{ Msg string }

func (e *QueryError) Error() string { return e.Msg }

// parseListQuery 解析并校验 list 查询参数。
//
// 判据与 manifest 校验同源（SortAllowed / FilterAllowed）：白名单外的字段一律 400，
// 不再"解析完静默丢弃" —— 那是"声明了没人读"的另一种形态。
func parseListQuery(e *Entity, field string, desc bool, raw url.Values) (ListQuery, error) {
	q := ListQuery{Keyword: strings.TrimSpace(raw.Get("keyword")), Filters: map[string]string{}}

	sortDeclared := func(k string) bool {
		for _, sk := range e.List.Sort {
			if sk.Field == k {
				return true
			}
		}
		return false
	}
	sortKeys := func() string {
		out := make([]string, 0, len(e.List.Sort))
		for _, sk := range e.List.Sort {
			out = append(out, sk.Field)
		}
		if len(out) == 0 {
			return "（未声明可排序字段）"
		}
		return strings.Join(out, "、")
	}

	switch {
	case field != "":
		if !sortDeclared(field) {
			return q, &QueryError{Msg: fmt.Sprintf("不支持按 %q 排序（可用：%s）", field, sortKeys())}
		}
		if _, ok := e.SortAllowed(field); !ok {
			return q, &QueryError{Msg: fmt.Sprintf("字段 %q 不能排序", field)}
		}
		q.SortField, q.Desc = field, desc
	case len(e.List.Sort) > 0:
		// 没传就按 manifest 的首项：声明顺序即优先级。
		q.SortField = e.List.Sort[0].Field
		q.Desc = e.List.Sort[0].Order != "asc"
	}

	// 遍历顺序固定（按 name 排序）：同时传两个非法筛选时，报错文案必须稳定，
	// 否则测试与用户看到的提示每次都不一样。
	keys := make([]string, 0, len(raw))
	for k := range raw {
		if strings.HasPrefix(k, "f_") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		field := strings.TrimPrefix(k, "f_")
		filterDeclared := false
		for _, d := range e.List.Filters {
			if d == field {
				filterDeclared = true
				break
			}
		}
		if !filterDeclared {
			list := strings.Join(e.List.Filters, "、")
			if list == "" {
				list = "（未声明可筛选字段）"
			}
			return q, &QueryError{Msg: fmt.Sprintf("不支持按 %q 筛选（可用：%s）", field, list)}
		}
		v, err := normalizeFilterValue(e, field, raw.Get(k))
		if err != nil {
			return q, err
		}
		if v != "" {
			q.Filters[field] = v
		}
	}
	return q, nil
}

// normalizeFilterValue 按算子校验并规范化一个筛选值。
//
// 返回值语义：空串 = 这个筛选没有实际条件（两侧都为空的区间），BuildQuery 会跳过。
func normalizeFilterValue(e *Entity, field, v string) (string, error) {
	v = strings.TrimSpace(v)
	op, ok := e.FilterAllowed(field)
	if !ok {
		return "", &QueryError{Msg: fmt.Sprintf("字段 %q 不能筛选", field)}
	}
	f, _ := e.field(field)
	switch op {
	case FilterOpEq:
		if v == "" {
			return "", nil
		}
		if f.Type == TypeBool {
			switch strings.ToLower(v) {
			case "1", "true":
				return "true", nil
			case "0", "false":
				return "false", nil
			default:
				return "", &QueryError{Msg: fmt.Sprintf("%s 的取值只能是 1/0", f.Label)}
			}
		}
		for _, o := range f.Options {
			if o == v {
				return v, nil
			}
		}
		return "", &QueryError{Msg: fmt.Sprintf("%s 只能取：%s", f.Label, strings.Join(f.Options, "、"))}
	case FilterOpRange:
		lo, hi, ok := strings.Cut(v, "..")
		if !ok {
			return "", &QueryError{Msg: fmt.Sprintf("%s 的区间格式是 起..止（任一侧可空）", f.Label)}
		}
		lo, hi = strings.TrimSpace(lo), strings.TrimSpace(hi)
		if lo == "" && hi == "" {
			return "", nil
		}
		for _, side := range []string{lo, hi} {
			if side == "" {
				continue
			}
			if f.Type == TypeDate {
				if _, err := parseDate(side); err != nil {
					return "", &QueryError{Msg: fmt.Sprintf("%s 的日期格式是 YYYY-MM-DD", f.Label)}
				}
				continue
			}
			n, err := strconv.ParseInt(side, 10, 64)
			if err != nil || n > 1<<53 || n < -(1<<53) {
				return "", &QueryError{Msg: fmt.Sprintf("%s 需要整数", f.Label)}
			}
		}
		return lo + ".." + hi, nil
	default: // FilterOpContains
		return v, nil
	}
}

// BuildQuery 是记录查询的**唯一**拼装点：列表、计数、导出三处共用。
func BuildQuery(gdb *gorm.DB, appID uint64, key, entity string, e *Entity, q ListQuery) *gorm.DB {
	tx := gdb.Model(&model.AppPluginRecord{}).
		Where("app_id = ? AND plugin_key = ? AND entity = ?", appID, key, entity)
	tx = keywordQuery(tx, q.Keyword)
	for _, field := range e.List.Filters {
		v, ok := q.Filters[field]
		if !ok || v == "" {
			continue
		}
		tx = applyFilter(tx, e, field, v)
	}
	return tx
}

func applyFilter(tx *gorm.DB, e *Entity, field, v string) *gorm.DB {
	op, _ := e.FilterAllowed(field)
	switch op {
	case FilterOpEq:
		return tx.Where("data->>? = ?", field, v)
	case FilterOpContains:
		return tx.Where(`data->>? ILIKE ? ESCAPE '\'`, field, "%"+escapeLike(v)+"%")
	case FilterOpRange:
		lo, hi, _ := strings.Cut(v, "..")
		f, _ := e.field(field)
		if f.Type == TypeInt {
			// 类型守卫：manifest 允许 force 改类型，库里必然有不符合当前类型的历史值；
			// 直接 `::numeric` 会让整页 500（22P02）。守卫失败的行值为 NULL，自然落在区间外。
			expr := "(CASE WHEN jsonb_typeof(data->?) = 'number' THEN (data->>?)::numeric END)"
			if lo != "" {
				tx = tx.Where(expr+" >= ?::numeric", field, field, lo)
			}
			if hi != "" {
				tx = tx.Where(expr+" <= ?::numeric", field, field, hi)
			}
			return tx
		}
		// 日期是定长 ISO 字符串，文本比较即时间比较；正则守卫排除脏值。
		expr := `(CASE WHEN data->>? ~ '^\d{4}-\d{2}-\d{2}$' THEN data->>? END)`
		if lo != "" {
			tx = tx.Where(expr+" >= ?", field, field, lo)
		}
		if hi != "" {
			tx = tx.Where(expr+" <= ?", field, field, hi)
		}
		return tx
	}
	return tx
}

// applyOrder 追加排序。任何排序都以 `, id DESC` 兜底：
// 排序值相同（枚举只有三个值）时分页必须稳定，否则翻页会重复/漏行。
func applyOrder(tx *gorm.DB, e *Entity, q ListQuery) *gorm.DB {
	if q.SortField == "" {
		return tx.Order("id DESC")
	}
	dir := "DESC"
	if !q.Desc {
		dir = "ASC"
	}
	switch q.SortField {
	case SortFieldCreatedAt, SortFieldUpdatedAt, SortFieldRecordKey:
		// 内置键是真列，不需要守卫，也不吃 JSONB 的代价。
		return tx.Order(clause.OrderBy{Expression: clause.Expr{
			SQL: q.SortField + " " + dir + " NULLS LAST, id DESC",
		}})
	}
	f, ok := e.field(q.SortField)
	if !ok || f == nil {
		return tx.Order("id DESC")
	}
	switch f.Type {
	case TypeInt:
		return tx.Order(clause.OrderBy{Expression: clause.Expr{
			SQL:  "(CASE WHEN jsonb_typeof(data->?) = 'number' THEN (data->>?)::numeric END) " + dir + " NULLS LAST, id DESC",
			Vars: []any{f.Key, f.Key},
		}})
	case TypeDate:
		return tx.Order(clause.OrderBy{Expression: clause.Expr{
			SQL:  `(CASE WHEN data->>? ~ '^\d{4}-\d{2}-\d{2}$' THEN data->>? END) ` + dir + " NULLS LAST, id DESC",
			Vars: []any{f.Key, f.Key},
		}})
	case TypeEnum:
		// 按 manifest 里 options 的顺序，而不是字母序：界面上看到的就是这个顺序，
		// 点表头却按字母重排会让人以为排序坏了。
		ph := strings.TrimSuffix(strings.Repeat("?,", len(f.Options)), ",")
		vars := make([]any, 0, len(f.Options)+1)
		for _, o := range f.Options {
			vars = append(vars, o)
		}
		vars = append(vars, f.Key)
		return tx.Order(clause.OrderBy{Expression: clause.Expr{
			SQL:  "array_position(ARRAY[" + ph + "]::text[], data->>?) " + dir + " NULLS LAST, id DESC",
			Vars: vars,
		}})
	default:
		return tx.Order(clause.OrderBy{Expression: clause.Expr{
			SQL:  "(data->>?) " + dir + " NULLS LAST, id DESC",
			Vars: []any{f.Key},
		}})
	}
}

// ListRecords 分页查记录（列表/搜索/筛选/排序的唯一入口）。
func ListRecords(ctx context.Context, gdb *gorm.DB, appID uint64, key, entity string, p pagination.Params, raw url.Values) ([]RecordItem, int64, error) {
	_, m, err := loadForRead(ctx, gdb, appID, key)
	if err != nil {
		return nil, 0, err
	}
	e, ok := m.EntityByKey(entity)
	if !ok {
		return nil, 0, ErrEntityNotFound
	}
	q, err := parseListQuery(e, p.Field, p.Desc, raw)
	if err != nil {
		return nil, 0, err
	}
	tx := BuildQuery(gdb.WithContext(ctx), appID, key, entity, e, q)
	var total int64
	if err := tx.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []model.AppPluginRecord
	if err := applyOrder(tx, e, q).Offset(p.Offset()).Limit(p.Limit()).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	items := make([]RecordItem, 0, len(rows))
	for i := range rows {
		items = append(items, toItem(&rows[i], e))
	}
	return items, total, nil
}

// toItem 按**当前 manifest** 投影 data。
//
// 为什么必须投影：写入口拒绝未声明键（util.go 的 normalizeFields），而 JSONB 里
// 保留着升级前删掉的字段。原样下发会让"把列表里的 data 原样 PUT 回来"必然 422
// —— 管理员看得见数据却改不了，报错还是"没有这个字段"。
func toItem(r *model.AppPluginRecord, e *Entity) RecordItem {
	raw, _ := DecodeObject(json.RawMessage(r.Data))
	data := make(map[string]any, len(e.Fields))
	for i := range e.Fields {
		if v, ok := raw[e.Fields[i].Key]; ok {
			data[e.Fields[i].Key] = v
		}
	}
	return RecordItem{
		RecordKey: r.RecordKey, Data: data, Version: r.LockVersion,
		CreatedAt: r.CreatedAt.Format("2006-01-02 15:04:05"),
		UpdatedAt: r.UpdatedAt.Format("2006-01-02 15:04:05"),
	}
}

// BatchDelete 批量删除（单事务）。不存在的 key 不计错误：返回实际删除行数，
// 与单条删除一致（幂等友好，重复点一次不会报错）。
func BatchDelete(ctx context.Context, gdb *gorm.DB, appID uint64, key, entity string, recordKeys []string) (int64, error) {
	_, m, err := loadForWrite(ctx, gdb, appID, key)
	if err != nil {
		return 0, err
	}
	e, ok := m.EntityByKey(entity)
	if !ok {
		return 0, ErrEntityNotFound
	}
	if !e.ActionAllowed(ActionDelete) || !e.ActionAllowed(ActionBatchDelete) {
		return 0, disabledErr("这个数据对象不允许批量删除")
	}
	// 去重 + 上限：一次请求的 SQL 参数与审计体积都必须有界。
	seen := map[string]bool{}
	keys := make([]string, 0, len(recordKeys))
	for _, k := range recordKeys {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return 0, nil
	}
	if len(keys) > MaxBatchDelete {
		return 0, &QueryError{Msg: fmt.Sprintf("一次最多删除 %d 条（本次 %d 条）", MaxBatchDelete, len(keys))}
	}
	var n int64
	err = gdb.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Where("app_id = ? AND plugin_key = ? AND entity = ? AND record_key IN ?",
			appID, key, entity, keys).Delete(&model.AppPluginRecord{})
		if res.Error != nil {
			return res.Error
		}
		n = res.RowsAffected
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// Create 新建一条记录。
//
// 容量校验放在事务里、并在插入前做：`count` 与 `insert` 之间理论上仍有窗口，
// 但上限是"防跑飞"的软阀（多进一两条不影响正确性），为此加表级锁不值得。
// applyDefaults 在写入前为**必填且带默认值**的字段补值（缺键或 null 才算缺）。
//
// 两条边界：
//   - 只补必填：可选字段补默认值会让"清空"永久不可达 —— 前端 toPayload 会去掉空值，
//     服务端再把默认值填回来，用户看到的是"删不掉的字段"。
//   - 显式空串**不**补：那是"我要清空它"，必填的就该报"必填"，
//     而不是悄悄换成默认值（谁也没法从界面上看出是被换掉的）。
//
// 默认值本身在导入期已按字段类型校验过（validateFields），这里不再做第二套判断。
func applyDefaults(e *Entity, raw map[string]any) {
	for i := range e.Fields {
		f := &e.Fields[i]
		if !f.Required || f.Default == nil {
			continue
		}
		if v, ok := raw[f.Key]; ok && v != nil {
			continue
		}
		raw[f.Key] = f.Default
	}
}

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
	applyDefaults(e, values)
	clean, ps := normalizeFields(e.Fields, values, "data.", true)
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
	item := toItem(row, e)
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
	applyDefaults(e, values)
	clean, ps := normalizeFields(e.Fields, values, "data.", true)
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
	item := toItem(&out, e)
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

// ExportPlan 校验导出前置条件，返回实体定义 + 解析好的筛选条件 + 总行数。
//
// 三件事必须在这里一次判完：实体存在、声明了 export、行数在上限内。
// 导出是"一次请求搬走全部数据"，把上限判断放到流式写入中间会让用户
// 拿到一个半截 CSV —— 那比直接报错更糟。
//
// 返回 ListQuery 是为了让随后每一批都复用同一份筛选：导出与屏幕必须同一批行。
func ExportPlan(ctx context.Context, gdb *gorm.DB, appID uint64, key, entity string, p pagination.Params, raw url.Values) (*Entity, ListQuery, int64, error) {
	_, m, err := loadForRead(ctx, gdb, appID, key)
	if err != nil {
		return nil, ListQuery{}, 0, err
	}
	e, ok := m.EntityByKey(entity)
	if !ok {
		return nil, ListQuery{}, 0, ErrEntityNotFound
	}
	if !e.ActionAllowed(ActionExport) {
		return nil, ListQuery{}, 0, disabledErr("这个数据对象不允许导出")
	}
	q, err := parseListQuery(e, p.Field, p.Desc, raw)
	if err != nil {
		return nil, ListQuery{}, 0, err
	}
	var n int64
	if err := BuildQuery(gdb.WithContext(ctx), appID, key, entity, e, q).Count(&n).Error; err != nil {
		return nil, ListQuery{}, 0, err
	}
	if n > MaxExportRows {
		return nil, ListQuery{}, 0, &LimitError{Max: MaxExportRows, Export: true}
	}
	return e, q, n, nil
}

// ExportBatch 用 keyset 翻页取一批（id > afterID），避免 OFFSET 越翻越慢。
//
// 排序**恒为 id ASC**（创建顺序），不跟随屏幕排序：keyset 依赖单调列，
// 按 data 排序只能 OFFSET 全量或内存排序，5 万行会先吃满内存。
func ExportBatch(ctx context.Context, gdb *gorm.DB, appID uint64, key, entity string, e *Entity, q ListQuery, afterID uint64, limit int) ([]ExportRow, error) {
	tx := BuildQuery(gdb.WithContext(ctx), appID, key, entity, e, q)
	if afterID > 0 {
		tx = tx.Where("id > ?", afterID)
	}
	var rows []model.AppPluginRecord
	if err := tx.Order("id ASC").Limit(limit).Find(&rows).Error; err != nil {
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
