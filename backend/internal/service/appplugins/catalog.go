package appplugins

import (
	"context"
	"fmt"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/pgerr"
)

// ImportInput 是一次导入请求。
//
// ExpectedVersion 必须在**每次**导入时带上（首次为 0）：AI 导入与运营微调
// 是两条独立入口，谁后写谁覆盖的代价是"运营刚改好的字段被 AI 旧稿抹掉"，
// 而这种覆盖在审计里看不出来（都记成 import）。
type ImportInput struct {
	Key             string
	Raw             []byte
	ExpectedVersion int
	Force           bool
	DryRun          bool
	AdminID         uint64
}

// Affected 是"这次变更会影响多少条存量记录"。
//
// 粒度到 entity × field 而不是一句总数：运营需要判断"改这一个字段值不值得"，
// 只给总数等于让人盲签。
type Affected struct {
	Entity  string `json:"entity"`
	Field   string `json:"field"`
	Records int64  `json:"records"`
	Reason  string `json:"reason"`
}

// ImportResult 同时服务于"预检"与"导入"两种调用：Applied 区分是否真的落库。
type ImportResult struct {
	Key         string     `json:"key"`
	Name        string     `json:"name"`
	Version     string     `json:"version"`
	LockVersion int        `json:"lock_version"`
	Created     bool       `json:"created"`
	Applied     bool       `json:"applied"`
	Problems    []Problem  `json:"problems"`
	Breaking    []Problem  `json:"breaking"`
	Affected    []Affected `json:"affected"`
}

// Import 是**目录写入的唯一入口**：校验、并发（CAS）、破坏性变更拦截、投影列同批写
// 全在这里，绕过它就等于绕过所有约束。
func Import(ctx context.Context, gdb *gorm.DB, in ImportInput) (*ImportResult, error) {
	m, ps := Decode(in.Raw)
	if m == nil {
		// 连 JSON 都不是：预检也要如实返回，不能 200 + 空问题列表。
		res := &ImportResult{Key: in.Key, Problems: ps}
		if in.DryRun {
			return res, nil
		}
		return nil, &InvalidError{Problems: ps}
	}
	ps = append(ps, m.Validate(in.Key)...)

	cur, exists, err := findCatalog(ctx, gdb, in.Key)
	if err != nil {
		return nil, err
	}
	if exists {
		if in.ExpectedVersion != cur.LockVersion {
			return nil, ErrVersionConflict
		}
	} else if in.ExpectedVersion != 0 {
		// 目标行不存在却带着版本号：说明前端拿的是已被删除的旧页面。
		return nil, ErrVersionConflict
	}

	var old *Manifest
	if exists {
		if old, _ = Decode(cur.Manifest); old == nil {
			// 库里的 manifest 解不开只可能是被外部改坏；当作"没有旧定义"处理会导致
			// 破坏性变更检测全部失效，所以明确报错让人去查库。
			return nil, fmt.Errorf("appplugins: 库里的插件定义无法解析（key=%s）", in.Key)
		}
	}

	breaking, affected, err := diffManifest(ctx, gdb, in.Key, old, m)
	if err != nil {
		return nil, err
	}

	res := &ImportResult{
		Key: in.Key, Name: m.Name, Version: m.Version,
		LockVersion: cur.LockVersion, Created: !exists,
		Problems: ps, Breaking: breaking, Affected: affected,
	}
	if in.DryRun {
		return res, nil
	}
	if len(ps) > 0 {
		return nil, &InvalidError{Problems: ps}
	}
	if len(breaking) > 0 && !in.Force {
		return nil, &InvalidError{Problems: breaking, Breaking: true}
	}

	raw, err := MustJSON(m)
	if err != nil {
		return nil, err
	}

	err = gdb.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 事务内重读 + 复核版本：前面那次读与这次写之间可能被另一个导入插进来。
		var row model.AppPlugin
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("key = ?", in.Key).Limit(1).Find(&row).Error; err != nil {
			return err
		}
		curLock := 0
		if row.ID != 0 {
			curLock = row.LockVersion
		}
		if curLock != in.ExpectedVersion {
			return ErrVersionConflict
		}
		if row.ID == 0 {
			row = model.AppPlugin{
				Key: in.Key, Name: m.Name, Version: m.Version, APIVersion: int16(m.APIVersion),
				Manifest: model.JSON(raw), Status: model.AppPluginStatusEnabled,
				Source: "import", LockVersion: 1, UpdatedBy: in.AdminID,
			}
			if err := tx.Create(&row).Error; err != nil {
				if pgerr.IsUnique(err) {
					return ErrVersionConflict
				}
				return err
			}
			res.LockVersion = row.LockVersion
			return nil
		}
		upd := map[string]any{
			"name":         m.Name,
			"version":      m.Version,
			"api_version":  m.APIVersion,
			"manifest":     model.JSON(raw),
			"lock_version": gorm.Expr("lock_version + 1"),
			"updated_by":   in.AdminID,
		}
		// 不用 map 之外的写法：Status 保持原值（下架状态不因重新导入而自动上架）。
		if err := tx.Model(&model.AppPlugin{}).Where("id = ?", row.ID).Updates(upd).Error; err != nil {
			return err
		}
		res.LockVersion = curLock + 1

		// unique 指向变了：必须重算存量行的 unique_norm，否则唯一性判据与
		// manifest 声明的字段不是同一件事（旧值还在旧列里，新值没人填）。
		if old == nil || !sameUnique(old, m) {
			if err := recomputeUniqueNorm(tx, in.Key, m); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	res.Applied = true
	return res, nil
}

// diffManifest 计算破坏性变更与影响面。
//
// 判定口径（与文档一致）：删实体 / 删字段 / 改字段类型 / 换 unique 都算破坏性 ——
// 它们的共同点是"存量数据里存在新定义不承认的东西"，需要显式确认。
// 加字段、改 label、放宽长度、加列都不算。
func diffManifest(ctx context.Context, gdb *gorm.DB, key string, old, next *Manifest) ([]Problem, []Affected, error) {
	if old == nil {
		return nil, nil, nil
	}
	var breaking []Problem
	var affected []Affected
	for i := range old.Entities {
		oe := &old.Entities[i]
		ne, ok := next.EntityByKey(oe.Key)
		if !ok {
			n, err := countRecords(ctx, gdb, key, oe.Key, "")
			if err != nil {
				return nil, nil, err
			}
			breaking = append(breaking, problem(
				fmt.Sprintf("entities[%d]", i),
				"对象「%s」将被移除，%d 条存量记录会保留但不再显示", oe.Name, n))
			continue
		}
		for j := range oe.Fields {
			of := &oe.Fields[j]
			nf, ok := ne.field(of.Key)
			switch {
			case !ok:
				n, err := countRecords(ctx, gdb, key, oe.Key, of.Key)
				if err != nil {
					return nil, nil, err
				}
				breaking = append(breaking, problem(
					fmt.Sprintf("entities[%d].fields[%d]", i, j),
					"字段「%s」将被移除，%d 条记录里这个字段不再显示", of.Label, n))
				if n > 0 {
					affected = append(affected, Affected{Entity: oe.Key, Field: of.Key, Records: n, Reason: "字段被移除"})
				}
			case of.Type != nf.Type:
				n, err := countRecords(ctx, gdb, key, oe.Key, of.Key)
				if err != nil {
					return nil, nil, err
				}
				breaking = append(breaking, problem(
					fmt.Sprintf("entities[%d].fields[%d].type", i, j),
					"字段「%s」的类型从 %s 改为 %s，%d 条记录的旧值可能无法按新类型显示", of.Label, of.Type, nf.Type, n))
				if n > 0 {
					affected = append(affected, Affected{Entity: oe.Key, Field: of.Key, Records: n, Reason: "类型变更"})
				}
			}
		}
		if oe.Unique != ne.Unique {
			breaking = append(breaking, problem(
				fmt.Sprintf("entities[%d].unique", i),
				"唯一字段从 %q 改为 %q：会按新字段重算全部记录的唯一值，冲突则整体失败", oe.Unique, ne.Unique))
		}
	}
	return breaking, affected, nil
}

// countRecords 数"带某个字段"的存量记录数；field 为空时数该实体全部。
func countRecords(ctx context.Context, gdb *gorm.DB, key, entity, field string) (int64, error) {
	q := gdb.WithContext(ctx).Model(&model.AppPluginRecord{}).
		Where("plugin_key = ? AND entity = ?", key, entity)
	if field != "" {
		q = q.Where("jsonb_exists(data, ?)", field)
	}
	var n int64
	err := q.Count(&n).Error
	return n, err
}

// recomputeUniqueNorm 按新 manifest 重算全部记录的业务唯一值。
//
// 用一条 SQL 而不是逐行读改写：行数可能上万，逐行会把导入拖到超时。
// 冲突由 uq_plugin_records_unique 直接抛出（23505），事务回滚后整体失败 ——
// 这正是想要的语义："换唯一字段后有两行撞了，这次升级不成立"。
func recomputeUniqueNorm(tx *gorm.DB, key string, m *Manifest) error {
	for i := range m.Entities {
		e := &m.Entities[i]
		var err error
		if e.Unique == "" {
			err = tx.Exec(`UPDATE app_plugin_records SET unique_norm = NULL
				WHERE plugin_key = ? AND entity = ?`, key, e.Key).Error
		} else {
			err = tx.Exec(`UPDATE app_plugin_records SET unique_norm = lower(btrim(data->>?))
				WHERE plugin_key = ? AND entity = ?`, e.Unique, key, e.Key).Error
		}
		if err != nil {
			if pgerr.IsUnique(err) {
				return ErrRecordConflict
			}
			return err
		}
	}
	return nil
}

// sameUnique 判断两份 manifest 的 unique 声明是否完全一致。
func sameUnique(a, b *Manifest) bool {
	if len(a.Entities) != len(b.Entities) {
		return false
	}
	for i := range a.Entities {
		nb, ok := b.EntityByKey(a.Entities[i].Key)
		if !ok || nb.Unique != a.Entities[i].Unique {
			return false
		}
	}
	return true
}

// findCatalog 按 key 取目录行；不存在时返回 (零值, false, nil)。
// 不返回 gorm.ErrRecordNotFound：那会让每个调用点都要先判 GORM 的哨兵，最终总有人漏掉。
func findCatalog(ctx context.Context, gdb *gorm.DB, key string) (model.AppPlugin, bool, error) {
	var row model.AppPlugin
	if err := gdb.WithContext(ctx).Where("key = ?", key).Limit(1).Find(&row).Error; err != nil {
		return row, false, err
	}
	return row, row.ID != 0, nil
}

// Get 取目录详情（含 manifest）。controller 的详情面用它。
func Get(ctx context.Context, gdb *gorm.DB, key string) (model.AppPlugin, *Manifest, error) {
	row, ok, err := findCatalog(ctx, gdb, key)
	if err != nil {
		return row, nil, err
	}
	if !ok {
		return row, nil, ErrNotFound
	}
	m, _ := Decode(row.Manifest)
	if m == nil {
		return row, nil, fmt.Errorf("appplugins: 库里的插件定义无法解析（key=%s）", key)
	}
	return row, m, nil
}

// ListItem 是目录列表行：**不含 manifest**（大字段不进列表，同旧插件系统
// 列表不 Select source_code 的教训）。
type ListItem struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	APIVersion  int16  `json:"api_version"`
	Status      int16  `json:"status"`
	Source      string `json:"source"`
	Icon        string `json:"icon"`
	IconURL     string `json:"icon_url"`
	Installs    int64  `json:"installs"`
	LockVersion int    `json:"lock_version"`
	UpdatedAt   string `json:"updated_at"`
}

// ListCatalog 返回目录页。q 同时匹配 key 与 name（前缀 + 包含都由 ILIKE 覆盖）。
func ListCatalog(ctx context.Context, gdb *gorm.DB, q string, offset, limit int) ([]ListItem, int64, error) {
	base := gdb.WithContext(ctx).Model(&model.AppPlugin{})
	s := strings.TrimSpace(q)
	if s != "" {
		like := "%" + s + "%"
		base = base.Where("key ILIKE ? OR name ILIKE ?", like, like)
	}
	var total int64
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	list := base
	if s != "" {
		// 精确命中的那一行排第一。`q` 是 ILIKE 子串匹配，命中几十上百行时单按 id 排序
		// 会把目标行挤出第一页 —— 列表页搜自己的插件却看不见它；导入流程也靠这一行取
		// `lock_version`，取不到就会把"升级"当成"新建"（服务端只能回版本冲突）。
		// 两条排序写在**同一个** expression 里：GORM 的 OrderBy 合并时后一条会覆盖
		// 带 Expression 的前一条（只剩 id DESC），分成两次 Order 就等于没加。
		list = list.Order(clause.OrderBy{Expression: clause.Expr{
			SQL:  "CASE WHEN key = ? THEN 0 ELSE 1 END, id DESC",
			Vars: []any{s},
		}})
	} else {
		list = list.Order("id DESC")
	}
	// Count 之前不加 order：聚合语句带 ORDER BY 在 PG 上会直接报错。
	var rows []model.AppPlugin
	if err := list.Offset(offset).Limit(limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	items := make([]ListItem, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		m, _ := Decode(r.Manifest)
		icon := ""
		if m != nil {
			icon = m.Icon
		}
		var installs int64
		gdb.WithContext(ctx).Model(&model.AppPluginInstall{}).
			Where("plugin_key = ? AND status <> ?", r.Key, model.AppPluginInstallRemoved).
			Count(&installs)
		items = append(items, ListItem{
			Key: r.Key, Name: r.Name, Version: r.Version, APIVersion: r.APIVersion,
			Status: r.Status, Source: r.Source, Icon: icon,
			Installs: installs, LockVersion: r.LockVersion,
			UpdatedAt: r.UpdatedAt.Format("2006-01-02 15:04:05"),
		})
	}
	return items, total, nil
}

// SetStatus 上架/下架目录。下架只拦新装与新写，不拦已安装的读与导出。
func SetStatus(ctx context.Context, gdb *gorm.DB, key string, status int16) error {
	if status != model.AppPluginStatusEnabled && status != model.AppPluginStatusDisabled {
		return ErrNotFound
	}
	res := gdb.WithContext(ctx).Model(&model.AppPlugin{}).
		Where("key = ?", key).Updates(map[string]any{"status": status})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteCatalog 删除目录行。仍有非"已卸载"安装时拒绝：删了会让那些应用的
// 插件页突然 404，而数据还在库里。
func DeleteCatalog(ctx context.Context, gdb *gorm.DB, key string) error {
	return gdb.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row model.AppPlugin
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("key = ?", key).Limit(1).Find(&row).Error; err != nil {
			return err
		}
		if row.ID == 0 {
			return ErrNotFound
		}
		var n int64
		if err := tx.Model(&model.AppPluginInstall{}).
			Where("plugin_key = ? AND status <> ?", key, model.AppPluginInstallRemoved).
			Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return &InUseError{Count: n}
		}
		return tx.Where("key = ?", key).Delete(&model.AppPlugin{}).Error
	})
}
