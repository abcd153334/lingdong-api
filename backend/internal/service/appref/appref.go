// Package appref 是「某条记录属于哪个应用」的**唯一派生点**。
//
// 为什么需要它：全站只有两处真的存了 app —— `api_keys.app_id` 与 `orders.app_id`。
// 调用记录（usages / usages_archive）、异步任务（tasks）只有 `key_id`，应用要靠
// `key_id → api_keys.app_id` 派生；订单与用户列表则要把 `app_id` 反向补成展示名。
// 这条派生链以前散在各处（应用详情页一段 SQL、应用列表一段 GORM 聚合），
// 再复制第三份必然漂移，所以这里收成一个叶子包：**只依赖 db + model**。
//
// 为什么不放进 service/app：那个包依赖 service/stats → billing/pricing。
// 让日志/任务查询路径编译期背上一整个计费域，是为了一个 20 行的映射表买单，
// 包边界会被打穿（依赖方向也会变得难以解释）。
//
// 两个函数都必须用**原生 SQL**：`api_keys` 是软删表（`deleted_at`），
// 用 GORM 默认作用域会把已删密钥过滤掉 —— 而历史调用记录/任务恰恰多半指向
// 早已轮换掉的密钥，结果是"昨天的日志一条应用名都没有"，看起来像数据丢了。
// apps 本身没有软删，所以 LEFT JOIN 不会额外丢行。
package appref

import (
	"context"

	"gorm.io/gorm"
)

// Ref 是一个应用的展示引用。
type Ref struct {
	ID   uint64
	Slug string
	// Name 是**展示名**：apps.display_name 非空时取它，否则回落后台名 `name`。
	// 与后台列表卡片（display_name || name）保持同一口径，避免同一个应用
	// 在卡片上叫「官网门户」、在日志里叫「web-portal」。
	Name string
}

// Names 批量取应用展示信息（含停用应用：历史订单/日志会指向它们，
// 筛不出来等于对账断链）。ids 会去重；空入参返回空 map 而不是 nil。
//
// 出错时返回 error 而不是静默给空名字：这些名字是随行数据一起展示的，
// 静默失败会让整页应用列变成空白，运营只会当成"数据丢了"。
func Names(ctx context.Context, gdb *gorm.DB, ids []uint64) (map[uint64]Ref, error) {
	out := map[uint64]Ref{}
	uniq := dedupe(ids)
	if len(uniq) == 0 {
		return out, nil
	}
	var rows []struct {
		ID   uint64
		Slug string
		Name string
	}
	if err := gdb.WithContext(ctx).
		Raw(`SELECT id, slug, COALESCE(NULLIF(display_name, ''), name) AS name
		       FROM apps WHERE id IN ?`, uniq).Scan(&rows).Error; err != nil {
		return out, err
	}
	for _, r := range rows {
		out[r.ID] = Ref{ID: r.ID, Slug: r.Slug, Name: r.Name}
	}
	return out, nil
}

// RefsOfKeys 批量把密钥 id 映射到它所属的应用。
//
// 返回 map[keyID]Ref：`app_id = 0`（平台直连的用户自建密钥）与查不到的密钥
// 都会得到 `Ref{}`（ID 为 0、名字为空）—— 调用方据此显示「平台直连」或「—」，
// **不要**用它来区分"没有应用"和"查不到"，那是两个不同的业务含义。
func RefsOfKeys(ctx context.Context, gdb *gorm.DB, keyIDs []uint64) (map[uint64]Ref, error) {
	out := map[uint64]Ref{}
	uniq := dedupe(keyIDs)
	if len(uniq) == 0 {
		return out, nil
	}
	var rows []struct {
		KeyID uint64
		AppID uint64
		Slug  string
		Name  string
	}
	// LEFT JOIN 而不是 JOIN：密钥可以没有应用（app_id=0），这些行也必须出现在结果里，
	// 否则调用方无法区分"这把密钥属于平台直连"与"这把密钥被漏查了"。
	if err := gdb.WithContext(ctx).
		Raw(`SELECT k.id AS key_id,
		            k.app_id AS app_id,
		            COALESCE(a.slug, '') AS slug,
		            COALESCE(NULLIF(a.display_name, ''), a.name, '') AS name
		       FROM api_keys k
		       LEFT JOIN apps a ON a.id = k.app_id
		      WHERE k.id IN ?`, uniq).Scan(&rows).Error; err != nil {
		return out, err
	}
	for _, r := range rows {
		out[r.KeyID] = Ref{ID: r.AppID, Slug: r.Slug, Name: r.Name}
	}
	return out, nil
}

// dedupe 去重并保持首次出现顺序（IN 列表里重复值只会让 SQL 变长，不影响结果）。
func dedupe(ids []uint64) []uint64 {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[uint64]struct{}, len(ids))
	out := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
