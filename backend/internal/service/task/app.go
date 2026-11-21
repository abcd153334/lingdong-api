package task

// 任务归属应用的批量补名。
//
// 为什么和 FillModelIcons 分两个文件：图标要解析"自有 → 供应商回落"的
// 多级规则（iconindex 负责），应用归属只有一条 key_id → api_keys.app_id 的
// 映射（appref 负责）。合成一个函数会让两个真相源挤在一起。

import (
	"context"

	"lingdong-api/internal/db"
	"lingdong-api/internal/service/appref"
)

// FillApp 批量补齐任务所属应用的 id 与展示名（原地修改）。
//
// 口径与日志/订单一致：应用由 key_id 派生，app_id=0 表示平台直连（用户自建密钥）。
// 补名失败**不阻断**列表：这是展示字段，与 FillModelIcons / KeyNames 同一口径 ——
// 一个名字查不到不该让整页 500。AppID 仍会是 0，前端显示「—」。
func FillApp(ctx context.Context, items []AdminView) {
	if len(items) == 0 {
		return
	}
	keyIDs := make([]uint64, 0, len(items))
	for i := range items {
		if items[i].KeyID != 0 {
			keyIDs = append(keyIDs, items[i].KeyID)
		}
	}
	refs, err := appref.RefsOfKeys(ctx, db.DB, keyIDs)
	if err != nil {
		return
	}
	for i := range items {
		if ref, ok := refs[items[i].KeyID]; ok {
			items[i].AppID, items[i].AppName = ref.ID, ref.Name
		}
	}
}
