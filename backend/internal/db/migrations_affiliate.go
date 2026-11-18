package db

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/affcfg"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/money"
)

// MigrationNameAffiliateConfig 是「邀请返利配置归一」的数据动作标识。
//
// # 为什么需要它，以及为什么不是上一版那条判据
//
// `Seed` 用 `OnConflict{DoNothing}`，已存在的 `aff` 行不会被更新；配置结构换代后
// 老库读出来就是一堆缺键 → 开关开着、一分不发、不报错。
//
// 上一版（M41）用「行里有没有 inviter_rate」当判据，已被真实数据证伪：新版管理台
// 保存过一次之后，行里同时存在旧键（reward_for_inviter…）与新键，判据命中便永远
// 跳过 —— 旧键再也不会被清理，而它们正是「保存成功、门户全 0」的现场证据。
//
// 现在改为**按结果归一**：删未知键、补齐缺失键、把越界值换回出厂值，
// 三者都为零才跳过。这样无论行是纯旧结构、混合结构还是半截结构，跑一次就收敛。
const MigrationNameAffiliateConfig = "m42_affiliate_config_normalize"

// AffConfigReport 是一次归一的结果（都是**键名**，不带值：里面可能有运营配置）。
type AffConfigReport struct {
	// Skipped 为 true 表示无需处理（没有 aff 行，或已完全收敛）。
	Skipped bool `json:"skipped"`
	// Updated 为 true 表示本次真的写回了库。
	Updated bool `json:"updated"`
	// Removed 是被删掉的未知键（旧结构残留）。
	Removed []string `json:"removed,omitempty"`
	// Added 是补上的缺失键。
	Added []string `json:"added,omitempty"`
	// Fixed 是越界/类型错误后被换回出厂值的键。
	Fixed []string `json:"fixed,omitempty"`
}

// MigrateAffiliateConfig 把 `system_config.aff` 收敛成唯一合法形态，幂等。
//
// 值域与出厂值全部来自 affcfg，本函数不重复声明任何键名或阈值。
// 越界值换回**出厂值**而不是边界值：`hold_hours=0` 的语义是「关掉冷却」，
// 收敛成 24（边界）会让一次配置错误变成「刚好够套利」的冷却期。
func MigrateAffiliateConfig(gdb *gorm.DB) (AffConfigReport, error) {
	var rep AffConfigReport
	if !gdb.Migrator().HasTable("system_config") {
		rep.Skipped = true
		return rep, nil
	}

	var row struct {
		Value model.JSON
	}
	// 列名 `key` 在 PG 里不是保留字，但 GORM 建表时是带引号的，这里保持一致。
	err := gdb.Table("system_config").Select(`"value"`).
		Where(`"key" = ?`, "aff").Take(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			rep.Skipped = true
			return rep, nil
		}
		return rep, fmt.Errorf("db: 读取邀请返利配置失败: %w", err)
	}

	cur := map[string]any{}
	if len(row.Value) > 0 {
		// 解析失败按空对象处理：缺键会被补齐成出厂值，比拒绝启动安全。
		_ = json.Unmarshal(row.Value, &cur)
	}

	out, removed, added, fixed := normalizeAffMap(cur)
	if len(removed) == 0 && len(added) == 0 && len(fixed) == 0 {
		rep.Skipped = true
		return rep, nil
	}

	val, err := model.NewJSON(out)
	if err != nil {
		return rep, fmt.Errorf("db: 构造邀请返利配置失败: %w", err)
	}
	if err := gdb.Table("system_config").
		Where(`"key" = ?`, "aff").
		Updates(map[string]any{"value": val, "updated_at": time.Now()}).Error; err != nil {
		return rep, fmt.Errorf("db: 归一邀请返利配置失败: %w", err)
	}
	rep.Updated = true
	rep.Removed, rep.Added, rep.Fixed = removed, added, fixed
	return rep, nil
}

// normalizeAffMap 是归一算法的唯一实现（迁移与单测共用）。
func normalizeAffMap(cur map[string]any) (out map[string]any, removed, added, fixed []string) {
	d := affcfg.Defaults()
	out = make(map[string]any, len(affcfg.Keys()))

	if v, ok := cur["enabled"].(bool); ok {
		out["enabled"] = v
	} else {
		out["enabled"] = d.Enabled
		if _, exists := cur["enabled"]; exists {
			fixed = append(fixed, "enabled")
		} else {
			added = append(added, "enabled")
		}
	}

	// 每个数值键：缺失 → 出厂值；类型/域不合法 → 出厂值；合法 → 原值。
	for _, f := range []struct {
		key string
		def int64
		lo  int64
		hi  int64
	}{
		{"inviter_rate", d.InviterRate, 0, affcfg.MaxRate},
		{"invitee_amount", d.InviteeAmount, 0, money.MaxAmount},
		{"max_rewards", d.MaxRewards, affcfg.MinMaxRewards, affcfg.MaxMaxRewards},
		{"hold_hours", d.HoldHours, affcfg.MinHoldHours, affcfg.MaxHoldHours},
		{"min_transfer", d.MinTransfer, 0, money.MaxAmount},
		{"daily_invite_cap", d.DailyInviteCap, affcfg.MinDailyInviteCap, affcfg.MaxDailyInviteCap},
	} {
		raw, ok := cur[f.key]
		if !ok || raw == nil {
			out[f.key] = f.def
			added = append(added, f.key)
			continue
		}
		n, ok := asInt64(raw)
		if !ok || n < f.lo || n > f.hi {
			out[f.key] = f.def
			fixed = append(fixed, f.key)
			continue
		}
		out[f.key] = n
	}

	known := map[string]bool{}
	for _, k := range affcfg.Keys() {
		known[k] = true
	}
	for k := range cur {
		if !known[k] {
			removed = append(removed, k)
		}
	}
	return out, removed, added, fixed
}

// asInt64 接受 JSON 解码可能产生的三种数值形态。
func asInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case int64:
		return t, true
	case int:
		return int64(t), true
	case float64:
		// JSON 数字经 map[string]any 解码后是 float64；金额上界 2e15 仍在 2^53 内，
		// 整数在此范围内可精确表示。超出这个范围的值一律判为非法，
		// 避免 int64 转换在越界时得到实现相关的脏值。
		if t < -9e15 || t > 9e15 || t != float64(int64(t)) {
			return 0, false
		}
		return int64(t), true
	case json.Number:
		n, err := t.Int64()
		return n, err == nil
	default:
		return 0, false
	}
}

// LogAffiliateConfig 把归一结果写进启动日志。
func LogAffiliateConfig(rep AffConfigReport) {
	if !rep.Updated {
		return
	}
	logger.L().Warn("邀请返利配置已归一（旧键删除 / 缺失补齐 / 越界回退出厂值）",
		zap.String("removed_keys", strings.Join(rep.Removed, ",")),
		zap.String("added_keys", strings.Join(rep.Added, ",")),
		zap.String("fixed_keys", strings.Join(rep.Fixed, ",")))
	// 配置被改写意味着实际生效值可能与运营预期不同，必须显式提示去复核。
	logger.L().Warn("请到管理台「系统配置 → 计费 → 邀请返利」复核比例、笔数与冷却期后重新保存一次")
}
