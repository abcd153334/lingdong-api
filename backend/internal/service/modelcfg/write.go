package modelcfg

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"lingdong-api/internal/assets/brandicon"
	"lingdong-api/internal/model"
	iconsvc "lingdong-api/internal/service/icon"
)

// checkIcon 校验「本次显式提交」的图标在库里可用（见 icon.Validate）。
//
// 形状错误与对象缺失对外同一个答复：重选一张。DB 故障原样上抛（500），
// 不折成「你填错了」—— 那会让运维去反复重试一个与他无关的动作。
func checkIcon(tx *gorm.DB, raw *string) error {
	if raw == nil {
		return nil
	}
	ctx := tx.Statement.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := iconsvc.Validate(ctx, *raw); err != nil {
		if errors.Is(err, brandicon.ErrInvalid) || errors.Is(err, iconsvc.ErrNotFound) {
			return fe("icon", "图标不合法：只支持内置图标或站内上传的图片")
		}
		return err
	}
	return nil
}

// GetTx 取单个模型（默认作用域：不含软删）。
func GetTx(tx *gorm.DB, id string) (*model.Model, error) {
	var row model.Model
	if err := tx.Where("id = ?", id).Limit(1).Find(&row).Error; err != nil {
		return nil, err
	}
	if row.ID == "" {
		return nil, ErrNotFound
	}
	return &row, nil
}

// requireVendor 校验供应商分组存在。
//
// `models.vendor` 是裸字符串无外键：写一个不存在的 id 会成功落库，
// 而门户按 vendor 查显示名与主题色 —— 结果是卡片供应商名空白、分组筛选失效，
// 全程没有任何报错。只能在写入期拦。
func requireVendor(tx *gorm.DB, vendorID string) error {
	if vendorID == "" {
		return fe("vendor", "必须指定供应商分组")
	}
	var n int64
	if err := tx.Model(&model.Vendor{}).Where("id = ?", vendorID).Count(&n).Error; err != nil {
		return err
	}
	if n == 0 {
		return fe("vendor", "供应商分组 %q 不存在，请先新建（vendor_list 可查现有分组）", vendorID)
	}
	return nil
}

// CreateTx 新建模型 + 绑定（同一事务）。
//
// 墓碑处理：models 的主键就是模型名，软删的行仍然占着主键。不物理清理的话，
// "删掉再同名重建"永远失败，而错误只是主键冲突——运营看不出原因。
func CreateTx(tx *gorm.DB, in Input, bindings []Binding) (*model.Model, error) {
	in.ID = trimSpace(in.ID)
	if in.ID == "" {
		return nil, fe("id", "模型 ID 不能为空")
	}
	if err := requireVendor(tx, in.Vendor); err != nil {
		return nil, err
	}
	// 新建必须显式给计费方式：空串会让"忘了填"变成"按 token 计费"，
	// 而音频/图像模型的正确口径是按次/按规格 —— 差别是"永久 0 元"。
	if !in.BillingModeSet {
		return nil, fe("billing_mode", "新建模型必须指定计费方式（按量 / 按次 / 按规格）")
	}

	var dead model.Model
	if err := tx.Unscoped().Where("id = ? AND deleted_at IS NOT NULL", in.ID).
		Limit(1).Find(&dead).Error; err != nil {
		return nil, err
	}
	if dead.ID != "" {
		if err := tx.Unscoped().Delete(&model.Model{}, "id = ?", in.ID).Error; err != nil {
			return nil, err
		}
	}
	var live int64
	if err := tx.Model(&model.Model{}).Where("id = ?", in.ID).Count(&live).Error; err != nil {
		return nil, err
	}
	if live > 0 {
		return nil, ErrModelExists
	}

	// 绑定可以为空：提案是"建模型 + 建绑定"两条 op，第一条执行时绑定还不存在。
	// 「新建模型必须有绑定」是**提案级**规则（plan.ValidateStructure 规则 9），
	// 不是创建单行的规则。
	nb := bindings
	if len(bindings) > 0 {
		var err error
		nb, err = NormalizeBindings(tx, in.Modality, bindings)
		if err != nil {
			return nil, err
		}
	}

	if err := checkIcon(tx, in.Icon); err != nil {
		return nil, err
	}
	m, err := Build(in)
	if err != nil {
		return nil, err
	}
	if len(nb) > 0 {
		if err := CheckCoverage(tx, nb, decodeStrings(m.Endpoints)); err != nil {
			return nil, err
		}
	}

	row := Updates(m)
	row["id"] = m.ID
	row["version"] = 1
	row["created_at"] = nowFn()
	row["updated_at"] = nowFn()
	// 用 map 落库：GORM 对 `default:true` 的 enabled 会丢 false。
	if err := tx.Table("models").Create(row).Error; err != nil {
		return nil, err
	}
	if err := ReplaceBindingsTx(tx, m.ID, nb); err != nil {
		return nil, err
	}
	m.Version = 1
	return m, nil
}

// UpdateTx 更新模型（不含绑定）。expectVersion > 0 时做乐观锁。
func UpdateTx(tx *gorm.DB, in Input, expectVersion int) (*model.Model, error) {
	cur, err := GetTx(tx, in.ID)
	if err != nil {
		return nil, err
	}
	if in.Vendor != "" && in.Vendor != cur.Vendor {
		if err := requireVendor(tx, in.Vendor); err != nil {
			return nil, err
		}
	}
	FillMissing(&in, cur)
	// 只校验本次显式提交的值：FillMissing 补出来的库内值可能指向早已被清理的
	// 图标对象，那时一次「只改价格」的保存不该整体 400。
	if in.Icon != nil && (cur.Icon != *in.Icon) {
		if err := checkIcon(tx, in.Icon); err != nil {
			return nil, err
		}
	}
	m, err := Build(in)
	if err != nil {
		return nil, err
	}
	updates := Updates(m)
	updates["version"] = gorm.Expr("version + 1")
	updates["updated_at"] = nowFn()

	q := tx.Model(&model.Model{}).Where("id = ?", in.ID)
	if expectVersion > 0 {
		q = q.Where("version = ?", expectVersion)
	}
	res := q.Updates(updates)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, ErrVersionConflict
	}
	var v int
	if err := tx.Model(&model.Model{}).Where("id = ?", in.ID).
		Select("version").Scan(&v).Error; err != nil {
		return nil, err
	}
	m.Version = v
	return m, nil
}

// SetEnabledTx 启停模型（绑定不变）。
func SetEnabledTx(tx *gorm.DB, id string, enabled bool) error {
	// 启用前必须有一条真能路由的绑定（P0-13）。
	//
	// 这条校验放在**写之前**：写完之后再回滚虽然也能拦住，但返回给调用方的
	// 错误会变成"提交失败但字段已变"，前端刷新后状态对不上。
	//
	// 人工启停与 AI 提案 apply 都走这里 —— 两条入口共用一份判据，
	// 否则"AI 能把模型改成不可用状态"这类不一致永远只在一条路径上暴露。
	if enabled {
		n, err := CountUsableBindings(tx, id)
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNoUsableBinding
		}
	}
	res := tx.Model(&model.Model{}).Where("id = ?", id).
		Updates(map[string]any{
			"enabled": enabled, "version": gorm.Expr("version + 1"), "updated_at": nowFn(),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchTx 只推进版本代际（关联数据变更后调用）。
//
// 为什么绑定变更也要 +1：绑定是"模型能不能被路由"的一部分，而 AI 提案用
// expect_version 做乐观锁。人工改了绑定却不加版本，AI 侧就会拿着过期的
// version 通过 CAS 并把绑定整体覆盖回去 —— 提案确认期间的人工改动被静默吞掉。
func TouchTx(tx *gorm.DB, id string) error {
	res := tx.Model(&model.Model{}).Where("id = ?", id).
		Updates(map[string]any{
			"version": gorm.Expr("version + 1"), "updated_at": nowFn(),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteTx 删除模型：软删模型行 + 硬删绑定。
//
// 绑定必须硬删：软删行会继续占用 (model_id, channel_id) 唯一索引，
// 之后同名模型重建再绑定同一渠道会直接 23505。
func DeleteTx(tx *gorm.DB, id string) error {
	res := tx.Where("id = ?", id).Delete(&model.Model{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return tx.Unscoped().Where("model_id = ?", id).Delete(&model.ModelBinding{}).Error
}

// UpsertVendorTx 新建供应商分组（与人工路径同口径：ID 即小写标识，重名拒绝）。
//
// icon 走 brandicon.Normalize：AI 提案里的图标与人工提交的图标必须是同一套契约，
// 否则一份写错的 key 会安静落库，表现为门户上该供应商永远显示首字。
func UpsertVendorTx(tx *gorm.DB, id, name, color, icon string) error {
	id = trimSpace(id)
	name = trimSpace(name)
	if id == "" || name == "" {
		return fe("vendor", "供应商标识与名称必填")
	}
	if err := checkIcon(tx, &icon); err != nil {
		return err
	}
	icon, err := brandicon.Normalize(icon)
	if err != nil {
		return fe("icon", "图标不合法：只支持内置图标或站内上传的图片")
	}
	var n int64
	if err := tx.Model(&model.Vendor{}).Where("name = ?", name).Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return fe("vendor", "供应商名称 %q 已存在", name)
	}
	row := map[string]any{
		"id": id, "name": name, "color": color, "icon": icon, "sort": 100, "builtin": false,
	}
	return tx.Table("vendors").Clauses(clause.OnConflict{DoNothing: true}).Create(row).Error
}
