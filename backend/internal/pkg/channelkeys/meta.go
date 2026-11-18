package channelkeys

import (
	"encoding/json"

	"lingdong-api/internal/model"
)

// Meta 是单枚密钥的**人工属性**（不是密文、不是状态机）。
//
// 只放两样东西：别名与启用位。冷却/失败次数/最近错误都在 Redis（易失态），
// 落库反而要处理"库说冷却、Redis 说正常"的第二真相源问题。
type Meta struct {
	Alias string `json:"alias,omitempty"`
	// Enabled 必须是指针。
	//
	// 值类型 + omitempty 会把 false 当零值丢掉，表现为"点了禁用、保存成功、
	// 下次打开又是启用"。nil 表示未设置 = 启用，因此缺省配置不会写进 JSON。
	Enabled *bool `json:"enabled,omitempty"`
}

// IsEnabled 返回生效的启用状态（缺省 true）。
func (m Meta) IsEnabled() bool { return m.Enabled == nil || *m.Enabled }

// IsZero 表示这条属性全为默认值，无需落库。
func (m Meta) IsZero() bool { return m.Alias == "" && m.Enabled == nil }

// MetaMap 是 keyID → 属性。用 map 而不是数组：
// 数组要与 keys 逐位对齐，而"换序/删中间一项"正是历史上冷却串位的根因。
type MetaMap map[string]Meta

// ParseMeta 解析属性列；空/非法一律视为"全部默认"，绝不 panic。
//
// 解析失败按默认处理是刻意的：属性丢了只会让别名/禁用态回到默认，
// 而 panic 会让整个渠道列表打不开 —— 后者严重得多。
func ParseMeta(raw model.JSON) MetaMap {
	if len(raw) == 0 {
		return nil
	}
	var m MetaMap
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// MarshalMeta 序列化属性列；空 map 写 NULL（不写 `{}`，避免库里堆空对象）。
func MarshalMeta(m MetaMap) (model.JSON, error) {
	if len(m) == 0 {
		return nil, nil
	}
	return model.NewJSON(m)
}

// Effective 返回某把 key 生效的别名与启用状态（缺项 = 空别名 + 启用）。
func (m MetaMap) Effective(id string) (alias string, enabled bool) {
	meta, ok := m[id]
	if !ok {
		return "", true
	}
	return meta.Alias, meta.IsEnabled()
}

// EnabledIDs 返回启用中的 keyID 集合（用于选路与守卫）。
func (m MetaMap) EnabledIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, enabled := m.Effective(id); enabled {
			out = append(out, id)
		}
	}
	return out
}

// Reconcile 在"整体替换密钥池"后重建属性表：只保留仍然存在的 keyID。
//
// 为什么要裁剪而不是叠加：同一把 key 删掉又加回来时，旧别名会"复活"，
// 而运营看到的是一次干净的重新添加；留着孤儿条目还会让 JSONB 无限增长。
func Reconcile(old MetaMap, keepIDs []string) MetaMap {
	if len(old) == 0 || len(keepIDs) == 0 {
		return nil
	}
	keep := make(map[string]struct{}, len(keepIDs))
	for _, id := range keepIDs {
		keep[id] = struct{}{}
	}
	out := make(MetaMap, len(keepIDs))
	for id, meta := range old {
		if _, ok := keep[id]; !ok {
			continue
		}
		if meta.IsZero() {
			continue
		}
		out[id] = meta
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
