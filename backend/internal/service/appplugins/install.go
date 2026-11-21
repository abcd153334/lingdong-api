package appplugins

import (
	"context"
	"encoding/json"
	"sort"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/pgerr"
)

// InstallItem 是"某应用已安装的插件"一行。
type InstallItem struct {
	PluginKey     string `json:"plugin_key"`
	Name          string `json:"name"`
	Version       string `json:"version"`
	PluginVersion string `json:"plugin_version"`
	Icon          string `json:"icon"`
	Status        int16  `json:"status"`
	// PublicEnabled 是对外只读开关的当前值（清单与描述符都要带上，
	// 否则管理台的开关只能靠"猜上次点的是什么"）。
	PublicEnabled bool           `json:"public_enabled"`
	Config        map[string]any `json:"config"`
	LockVersion   int            `json:"lock_version"`
	RecordCount   int64          `json:"record_count"`
	UpdatedAt     string         `json:"updated_at"`
}

// Drift 表示目录已升级、而本应用仍按旧版本写入过数据。
//
// 它不是错误：manifest 的演进是常态，锁死版本会让"修一个 label"也要所有应用重装。
// 但必须让人看见 —— 否则"字段怎么变了"只能靠猜。
type Drift struct {
	Installed string `json:"installed"`
	Current   string `json:"current"`
}

// Detail 是泛化路由页面唯一的数据来源：安装态 + 编译后的页面描述 + 漂移提示。
//
// 一次返回三样而不是让前端打三个接口：页面要么三者都拿到，要么都不显示；
// 分三次请求必然出现"描述到了、安装态还在路上"的半截渲染。
type Detail struct {
	Install         InstallItem `json:"install"`
	Plugin          *Descriptor `json:"plugin"`
	Drift           *Drift      `json:"drift"`
	RecordCount     int64       `json:"record_count"`
	CatalogDisabled bool        `json:"catalog_disabled"`
	// ConfigMissing 是"当前 manifest 声明为必填、但这条安装里没有值"的键。
	//
	// 为什么需要：升级新增一个必填配置后，已启用的安装会处于"启用着但配置不全"的状态。
	// 不给读者看的话，管理员只能从"保存成功但行为不对"里倒推，而"缺哪一项"无从下手。
	ConfigMissing []string `json:"config_missing"`
}

// Install 把目录里的插件装到某个应用上。
//
// 并发靠 uq_plugin_installs_app_key 收敛，不做"先查再插"（TOCTOU）：
// 两个管理员同时点安装，一个成功一个 409，这是正确行为。
func Install(ctx context.Context, gdb *gorm.DB, appID uint64, key string, cfgRaw map[string]any) (*model.AppPluginInstall, error) {
	cat, m, err := Get(ctx, gdb, key)
	if err != nil {
		return nil, err
	}
	if cat.Status != model.AppPluginStatusEnabled {
		return nil, ErrDisabled
	}
	cfg, ps := normalizeFields(m.Config, cfgRaw, "config.", true)
	if len(ps) > 0 {
		return nil, &InvalidError{Problems: ps}
	}
	raw, err := MustJSON(cfg)
	if err != nil {
		return nil, err
	}
	row := &model.AppPluginInstall{
		AppID: appID, PluginKey: key, PluginVersion: cat.Version,
		Status: model.AppPluginInstallEnabled, Config: model.JSON(raw), LockVersion: 1,
	}
	if err := gdb.WithContext(ctx).Create(row).Error; err != nil {
		if pgerr.IsUnique(err) {
			return nil, ErrAlreadyInstalled
		}
		return nil, err
	}
	return row, nil
}

// PatchInput 是配置/启停的部分更新。ExpectedVersion 必填：
// 两个标签页同时改配置时，后写者必须收到 409，而不是静默覆盖。
type PatchInput struct {
	Config *map[string]any
	Status *int16
	// PublicEnabled 是对外只读开关（见 model.AppPluginInstall.PublicEnabled）。
	// 与 status 同一个 PATCH：它也是"这条安装的一个开关"，拆接口只会让人漏调一个。
	PublicEnabled   *bool
	ExpectedVersion int
}

// Patch 改配置或启停。配置是**部分键集**覆盖（同 systemconfig 的分页签保存）：
// 只覆盖传入的键，未传的保持原值，避免"改一个键把其它键清空"。
// 返回值 dropped 是"存量里有、当前 manifest 已不承认"的配置键（已被裁剪掉）。
// 调用方要把它写进审计：清理是正确行为，但必须留痕，否则"我的配置怎么没了"无据可查。
func Patch(ctx context.Context, gdb *gorm.DB, appID uint64, key string, in PatchInput) (*model.AppPluginInstall, []string, error) {
	var out model.AppPluginInstall
	var dropped []string
	err := gdb.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		inst, err := lockInstall(tx, appID, key)
		if err != nil {
			return err
		}
		if in.ExpectedVersion <= 0 || inst.LockVersion != in.ExpectedVersion {
			return ErrVersionConflict
		}
		cat, m, err := Get(ctx, tx, key)
		if err != nil {
			return err
		}

		nextStatus := inst.Status
		if in.Status != nil {
			switch *in.Status {
			case model.AppPluginInstallEnabled, model.AppPluginInstallDisabled, model.AppPluginInstallRemoved:
				nextStatus = *in.Status
			default:
				return ErrVersionConflict
			}
			if nextStatus == model.AppPluginInstallEnabled && cat.Status != model.AppPluginStatusEnabled {
				// 目录已下架时不允许"重新上架服务"：否则下架形同虚设。
				return ErrDisabled
			}
		}

		// 存量配置先按**当前** manifest 裁剪：升级删掉一个配置键后，库里那份 map
		// 仍然带着旧键，直接喂给 normalizeFields 会对每个旧键报"没有这个字段" ——
		// 表现为"这个插件的任何保存都 422，连关掉对外只读都不行"。
		cfg := map[string]any{}
		if len(inst.Config) > 0 {
			if decoded, err := DecodeObject(json.RawMessage(inst.Config)); err == nil {
				for k, v := range decoded {
					if _, ok := m.ConfigField(k); ok {
						cfg[k] = v
					} else {
						dropped = append(dropped, k)
					}
				}
				sort.Strings(dropped)
			}
		}
		if in.Config != nil {
			for k, v := range *in.Config {
				cfg[k] = v
			}
		}
		// 必填口径三条（缺一条都会把正常操作堵死）：
		//  1. 从非启用转启用：全量校验（"启用着但缺必填"是造出来的中间态）；
		//  2. 保持启用：只拒绝"本次显式传空"的必填项 —— 否则升级新增一个必填键后，
		//     连改对外只读开关都 422（用户只是点了个开关，缺的那项要等运维补）；
		//  3. 停用/卸载态：不校验（允许先存半截配置，启用时再拦）。
		enabling := nextStatus == model.AppPluginInstallEnabled && inst.Status != model.AppPluginInstallEnabled
		clean, ps := normalizeFields(m.Config, cfg, "config.", enabling)
		if len(ps) > 0 {
			return &InvalidError{Problems: ps}
		}
		if nextStatus == model.AppPluginInstallEnabled {
			for i := range m.Config {
				f := &m.Config[i]
				if !f.Required || clean[f.Key] != nil {
					continue
				}
				if in.Config != nil {
					if _, touched := (*in.Config)[f.Key]; touched {
						return &InvalidError{Problems: []Problem{problem("config."+f.Key, "必填项不能清空")}}
					}
				}
				if enabling {
					return &InvalidError{Problems: []Problem{problem("config."+f.Key, "启用前必须填写")}}
				}
			}
		}
		raw, err := MustJSON(clean)
		if err != nil {
			return err
		}
		upd := map[string]any{
			"config":       model.JSON(raw),
			"status":       nextStatus,
			"lock_version": gorm.Expr("lock_version + 1"),
		}
		if in.PublicEnabled != nil {
			upd["public_enabled"] = *in.PublicEnabled
		}
		if nextStatus == model.AppPluginInstallEnabled {
			// 重新启用时把版本快照对齐到当前目录：漂移横幅的语义是
			// "自上次启用以来目录变过"，重新启用即视为重新确认过。
			upd["plugin_version"] = cat.Version
		}
		if err := tx.Model(&model.AppPluginInstall{}).Where("id = ?", inst.ID).Updates(upd).Error; err != nil {
			return err
		}
		return tx.Where("id = ?", inst.ID).Limit(1).Find(&out).Error
	})
	if err != nil {
		return nil, nil, err
	}
	return &out, dropped, nil
}

// Uninstall 只把状态置为"已卸载"，不删配置、不删记录。
//
// 刻意不走 Patch 的版本校验：卸载是"摘掉入口"，重复点应当幂等成功。
// 记录与配置全留着 —— 重装后接着用，这是"卸载 ≠ 删除数据"的唯一实现。
func Uninstall(ctx context.Context, gdb *gorm.DB, appID uint64, key string) error {
	return gdb.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		inst, err := lockInstall(tx, appID, key)
		if err != nil {
			return err
		}
		if inst.Status == model.AppPluginInstallRemoved {
			return nil
		}
		return tx.Model(&model.AppPluginInstall{}).Where("id = ?", inst.ID).
			Updates(map[string]any{
				"status":       model.AppPluginInstallRemoved,
				"lock_version": gorm.Expr("lock_version + 1"),
			}).Error
	})
}

// Purge 物理删除该应用下这个插件的全部记录。**不可逆**，所以要求先卸载：
// 在用的数据不该被一次误点抹掉。
func Purge(ctx context.Context, gdb *gorm.DB, appID uint64, key string) (int64, error) {
	var deleted int64
	err := gdb.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		inst, err := lockInstall(tx, appID, key)
		if err != nil {
			return err
		}
		if inst.Status != model.AppPluginInstallRemoved {
			return ErrPurgeNotAllowed
		}
		res := tx.Where("app_id = ? AND plugin_key = ?", appID, key).
			Delete(&model.AppPluginRecord{})
		if res.Error != nil {
			return res.Error
		}
		deleted = res.RowsAffected
		return nil
	})
	return deleted, err
}

// GetDetail 取页面所需的全部数据。安装行必须存在（含"已卸载"）：
// 已卸载也要能打开页面，否则"重新启用 / 清除数据"没有入口。
func GetDetail(ctx context.Context, gdb *gorm.DB, appID uint64, key string) (*Detail, error) {
	inst, err := getInstall(ctx, gdb, appID, key)
	if err != nil {
		return nil, err
	}
	cat, m, err := Get(ctx, gdb, key)
	if err != nil {
		return nil, err
	}
	var count int64
	if err := gdb.WithContext(ctx).Model(&model.AppPluginRecord{}).
		Where("app_id = ? AND plugin_key = ?", appID, key).Count(&count).Error; err != nil {
		return nil, err
	}
	d := &Detail{
		Install: InstallItem{
			PluginKey: key, Name: m.Name, Version: cat.Version,
			PluginVersion: inst.PluginVersion, Icon: m.Icon,
			Status: inst.Status, PublicEnabled: inst.PublicEnabled,
			LockVersion: inst.LockVersion,
			RecordCount: count, UpdatedAt: inst.UpdatedAt.Format("2006-01-02 15:04:05"),
		},
		Plugin:          m.Describe(),
		CatalogDisabled: cat.Status != model.AppPluginStatusEnabled,
		RecordCount:     count,
	}
	if len(inst.Config) > 0 {
		if cfg, err := DecodeObject(json.RawMessage(inst.Config)); err == nil {
			d.Install.Config = cfg
		}
	}
	if d.Install.Config == nil {
		d.Install.Config = map[string]any{}
	}
	// 必填但没值：升级新增必填项后，已启用的安装会停在这个状态。
	// 恒为数组（空也是 []），前端直接 .length。
	d.ConfigMissing = make([]string, 0, len(m.Config))
	for i := range m.Config {
		f := &m.Config[i]
		if !f.Required {
			continue
		}
		if v, ok := d.Install.Config[f.Key]; !ok || v == nil {
			d.ConfigMissing = append(d.ConfigMissing, f.Key)
		}
	}
	if inst.PluginVersion != "" && inst.PluginVersion != cat.Version {
		d.Drift = &Drift{Installed: inst.PluginVersion, Current: cat.Version}
	}
	return d, nil
}

// ListInstalls 返回某应用已装插件（含已卸载，供"清除数据"入口）。
func ListInstalls(ctx context.Context, gdb *gorm.DB, appID uint64) ([]InstallItem, error) {
	var rows []model.AppPluginInstall
	if err := gdb.WithContext(ctx).Where("app_id = ?", appID).
		Order("id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	counts := map[string]int64{}
	var agg []struct {
		PluginKey string
		N         int64
	}
	if err := gdb.WithContext(ctx).Model(&model.AppPluginRecord{}).
		Select("plugin_key, count(*) AS n").
		Where("app_id = ?", appID).Group("plugin_key").Scan(&agg).Error; err != nil {
		return nil, err
	}
	for _, a := range agg {
		counts[a.PluginKey] = a.N
	}
	items := make([]InstallItem, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		it := InstallItem{
			PluginKey: r.PluginKey, Name: r.PluginKey, Version: r.PluginVersion,
			PluginVersion: r.PluginVersion, Status: r.Status, PublicEnabled: r.PublicEnabled,
			LockVersion: r.LockVersion, RecordCount: counts[r.PluginKey],
			UpdatedAt: r.UpdatedAt.Format("2006-01-02 15:04:05"),
		}
		if cat, m, err := Get(ctx, gdb, r.PluginKey); err == nil {
			it.Name = m.Name
			it.Version = cat.Version
			it.Icon = m.Icon
		}
		if len(r.Config) > 0 {
			if cfg, err := DecodeObject(json.RawMessage(r.Config)); err == nil {
				it.Config = cfg
			}
		}
		if it.Config == nil {
			it.Config = map[string]any{}
		}
		items = append(items, it)
	}
	return items, nil
}

// getInstall 按 (app, key) 取安装行，不存在返回 ErrNotInstalled。
func getInstall(ctx context.Context, gdb *gorm.DB, appID uint64, key string) (model.AppPluginInstall, error) {
	var inst model.AppPluginInstall
	if err := gdb.WithContext(ctx).
		Where("app_id = ? AND plugin_key = ?", appID, key).Limit(1).Find(&inst).Error; err != nil {
		return inst, err
	}
	if inst.ID == 0 {
		return inst, ErrNotInstalled
	}
	return inst, nil
}

// lockInstall 同上但加行锁：配置/启停/卸载/清除都要在锁内做状态判断。
func lockInstall(tx *gorm.DB, appID uint64, key string) (model.AppPluginInstall, error) {
	var inst model.AppPluginInstall
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("app_id = ? AND plugin_key = ?", appID, key).Limit(1).Find(&inst).Error; err != nil {
		return inst, err
	}
	if inst.ID == 0 {
		return inst, ErrNotInstalled
	}
	return inst, nil
}

// loadForWrite 是记录写操作的唯一闸门：三个状态各自对应一句人话错误。
//
// 为什么"停用"只拦写不拦读：停用常是"先冻结、再排查"，连读都拦会让人
// 无法查历史；而"已卸载"连读都拦，因为入口本身已经从页面上消失了。
func loadForWrite(ctx context.Context, gdb *gorm.DB, appID uint64, key string) (model.AppPluginInstall, *Manifest, error) {
	inst, err := getInstall(ctx, gdb, appID, key)
	if err != nil {
		return inst, nil, err
	}
	switch inst.Status {
	case model.AppPluginInstallEnabled:
	case model.AppPluginInstallDisabled:
		return inst, nil, disabledErr("插件已停用，请先启用后再写入")
	default:
		return inst, nil, ErrNotInstalled
	}
	cat, m, err := Get(ctx, gdb, key)
	if err != nil {
		return inst, nil, err
	}
	if cat.Status != model.AppPluginStatusEnabled {
		return inst, nil, disabledErr("插件已下架，暂时不能写入")
	}
	return inst, m, nil
}

// loadForRead 允许"启用"与"停用"两种状态读；已卸载走 ErrNotInstalled。
func loadForRead(ctx context.Context, gdb *gorm.DB, appID uint64, key string) (model.AppPluginInstall, *Manifest, error) {
	inst, err := getInstall(ctx, gdb, appID, key)
	if err != nil {
		return inst, nil, err
	}
	if inst.Status == model.AppPluginInstallRemoved {
		return inst, nil, ErrNotInstalled
	}
	_, m, err := Get(ctx, gdb, key)
	if err != nil {
		return inst, nil, err
	}
	return inst, m, nil
}
