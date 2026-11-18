package channel

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/channelkeys"
	"lingdong-api/internal/relay"
	"lingdong-api/internal/service/keyhealth"
)

// 本文件是「密钥池治理」的读写面：管理台看明细、改别名/启用态、增删密钥。
//
// 三条硬约定：
//  1. KeyView 里**没有**明文字段 —— 不是"忘了放"，而是让"泄漏明文"在编译期不可达；
//  2. 一切 key 级状态以 keyID 为键（明文 HMAC 指纹），下标只用于展示；
//  3. 密钥池与属性表（key_meta）必须在**同一次 UPDATE** 落库，否则会出现
//     「keys 删了、属性还在」的漂移，而没有任何机制能发现它。

// ErrUnknownKey 表示引用了池子里不存在的 keyID（多端并发编辑后常见）。
var ErrUnknownKey = errors.New("密钥不存在（可能已被他人删除，请刷新）")

// ErrLastEnabledKey 表示这次操作会让启用中的渠道一把可用密钥都不剩。
var ErrLastEnabledKey = errors.New("启用中的渠道必须至少保留一把可用密钥")

// KeyView 是管理台看到的一枚密钥。
type KeyView struct {
	Index int    `json:"index"`
	KeyID string `json:"key_id"`
	// Fingerprint 是前 6 + 后 4 的指纹；解密失败的条目为空（连指纹都算不出来）。
	Fingerprint string `json:"fingerprint"`
	Alias       string `json:"alias"`
	Enabled     bool   `json:"enabled"`
	DecryptOK   bool   `json:"decrypt_ok"`
	// 冷却与最近错误来自 Redis：Redis 不可用时为零值（前端展示为「—」），不影响列表可用性。
	CooldownReason       string `json:"cooldown_reason"`
	CooldownRemainingSec int    `json:"cooldown_remaining_sec"`
	LastErrorStatus      int    `json:"last_error_status"`
	LastErrorAt          string `json:"last_error_at"`
}

// KeysResult 是密钥池的完整视图（GET 与 PATCH 共用同一个响应形状）。
type KeysResult struct {
	Version int `json:"version"`
	// Items 永远是非 nil 切片：前端 `.length` 撞上 null 会整页白屏。
	Items []KeyView `json:"items"`
	// KeyCount 是物理条数；BrokenCount 是无法解密条数；KeyEnabledCount 是选路实际会用的条数。
	KeyCount        int `json:"key_count"`
	BrokenCount     int `json:"broken_count"`
	KeyEnabledCount int `json:"key_enabled_count"`
}

// MaxKeyOpsPerRequest 是一次 PATCH 允许携带的最大操作数。
//
// 上限的作用不是"防呆"而是防阻塞：一批 20 万个 add 会在一个事务里
// 逐条 Fernet 加密，把保存请求拖到超时，同时把面板渲染打穿。
const MaxKeyOpsPerRequest = 200

// KeyOp 是一次密钥池变更操作（PATCH 的原子输入）。
type KeyOp struct {
	// Op ∈ add | replace | replace_all | remove | meta | reset_cooldown
	Op string `json:"op"`
	// Key 是**本次带来的新明文**：add 新增，replace 替换 KeyID 指向的那把。
	// 一个含义只允许一个字段名 —— 再加一个 NewKey 就要回答"两个都填听谁的"。
	Key string `json:"key"`
	// Keys 只有 replace_all 使用（整池重建）。其余 op 带了会被拒。
	Keys []string `json:"keys"`
	// KeyID 用于 replace/remove/meta/reset_cooldown。
	KeyID string `json:"key_id"`
	// Index 是 remove 的兜底：解密失败的条目算不出 keyID，只能按下标删。
	Index   *int    `json:"index"`
	Alias   *string `json:"alias"`
	Enabled *bool   `json:"enabled"`
}

// Keys 返回密钥池明细（管理台）。
func Keys(ctx context.Context, id uint64) (*KeysResult, error) {
	c, err := get(id)
	if err != nil {
		return nil, err
	}
	res, err := buildKeys(ctx, c)
	return res, err
}

// PatchKeys 原子地应用一批密钥池变更。
//
// 并发：行锁 + 版本号双保险。只靠版本号时两个请求可能同时读到同一版本；
// 只靠行锁时"读版本-写"之间仍可能被外部无版本写入插入。两个都要。
func PatchKeys(ctx context.Context, id uint64, version int, ops []KeyOp) (*KeysResult, error) {
	var (
		saved  model.Channel
		resets []string
	)
	err := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var c model.Channel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&c, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if version > 0 && c.Version != version {
			return ErrConflict
		}
		plains, meta, res, err := applyOps(channelkeys.Decrypt(c.Keys), channelkeys.ParseMeta(c.KeyMeta), ops)
		if err != nil {
			return err
		}
		if len(plains) == 0 {
			return &relay.ConfigError{Field: "keys", Scope: "渠道", Msg: "密钥池不能为空"}
		}
		if c.Status == StatusEnabled && enabledCountOf(plains, meta) == 0 {
			return ErrLastEnabledKey
		}
		raw, err := EncryptKeys(plains)
		if err != nil {
			return err
		}
		metaRaw, err := channelkeys.MarshalMeta(meta)
		if err != nil {
			return err
		}
		upd := tx.Model(&model.Channel{}).Where("id = ?", id).Updates(map[string]any{
			"keys":     raw,
			"key_meta": metaRaw,
			"version":  gorm.Expr("version + 1"),
		})
		if upd.Error != nil {
			return upd.Error
		}
		if upd.RowsAffected == 0 {
			return ErrConflict
		}
		c.Keys, c.KeyMeta, c.Version = raw, metaRaw, c.Version+1
		saved, resets = c, res
		return nil
	})
	if err != nil {
		return nil, err
	}
	// 冷却清理只能在提交之后：先删 Redis 再回滚事务，会让"这次没改成"的 key 冷却凭空消失。
	for _, keyID := range resets {
		keyhealth.Clear(ctx, id, keyID)
	}
	// 视图必须在**这两件事之后**生成。
	//
	// 原来在事务内就 buildKeys：那时 Redis 里的冷却还没清，"重置冷却"的响应
	// 仍带着冷却徽标，前端 setQueryData 之后表现成"点了没反应"，
	// 要等下一次全量刷新才恢复。
	return buildKeys(ctx, &saved)
}

// applyOps 是纯函数：按 replace_all → remove → replace → add → meta → reset_cooldown 应用操作。
//
// 为什么先删后加：同一次提交里"删掉 A 再加同一把 key"必须得到确定结果；
// 顺序反了会变成"先加后删"，删掉的正是刚加的那把（ID 相同），用户看到的是"没加进去"。
//
// 重建时会**丢弃无法解密的 token**：算不出 keyID、也用不了，留着只会让
// 「总计 3 条、可用 2 条、另 1 条永远红着」的状态一直挂着。这是一次修复，
// 响应里的 key_count 会立刻反映出来。
func applyOps(dec channelkeys.Result, meta channelkeys.MetaMap,
	ops []KeyOp) ([]string, channelkeys.MetaMap, []string, error) {
	cur := append([]string(nil), plainsOf(dec)...)
	curMeta := channelkeys.MetaMap{}
	for k, v := range meta {
		curMeta[k] = v
	}
	var resets []string

	// 未识别的 op 必须先报错：静默忽略会表现为"点了没反应"而请求返回 200。
	for _, op := range ops {
		switch op.Op {
		case "add", "replace", "replace_all", "remove", "meta", "reset_cooldown":
		default:
			return nil, nil, nil, keyCfgErr(fmt.Sprintf("不支持的操作 %q", op.Op))
		}
	}

	// 坏列：channels.keys 不是合法的 []string 时，解密拿不到任何条目（plainsOf 返回空）。
	// 此时一条 add 会把整列替换成这一把 —— 运营以为"加了一把"，实际把其余全丢了。
	// 所以只允许显式重建，让"丢"变成一个需要主动选择的动作。
	if dec.Invalid {
		rebuild := len(ops) == 1 && ops[0].Op == "replace_all"
		if !rebuild {
			return nil, nil, nil, keyCfgErr("密钥列已损坏，无法逐条修改；请用「重建密钥池」重新粘贴全部密钥")
		}
	}

	// ⓪ replace_all：整池重建。**必须独占一批** ——
	// 与增量操作混在一起时"先删后加"的确定性直接失效（add 的那把该落在新池还是旧池？
	// 没有自然答案）。与其定义一个没人记得住的顺序，不如禁止混用。
	for _, op := range ops {
		if op.Op != "replace_all" {
			continue
		}
		if len(ops) != 1 {
			return nil, nil, nil, keyCfgErr("整池重建必须单独提交，不能与其它操作混在一批")
		}
		next := make([]string, 0, len(op.Keys))
		for i, raw := range op.Keys {
			k, err := channelkeys.NormalizeKey(raw)
			if err != nil {
				return nil, nil, nil, keyCfgErr(fmt.Sprintf("第 %d 条：%v", i+1, err))
			}
			next = append(next, k)
		}
		next, _ = channelkeys.Dedupe(next)
		if len(next) == 0 {
			return nil, nil, nil, keyCfgErr("新密钥池不能为空")
		}
		if len(next) > channelkeys.MaxKeysPerChannel {
			return nil, nil, nil, keyCfgErr(fmt.Sprintf("密钥池最多 %d 把，本次提交 %d 把",
				channelkeys.MaxKeysPerChannel, len(next)))
		}
		ids := idsOf(next)
		cur = next
		// 属性表按新池重建：同值 key 保留别名与启用态，其余条目被 GC。
		curMeta = channelkeys.Reconcile(curMeta, ids)
		// 同值 key 可能是"删了又加"：残留的退避计数必须清掉，否则新池里有一把
		// 明明刚验证过、却仍被冷却挡在门外（最长 10 分钟）。
		return cur, curMeta, append(resets, ids...), nil
	}

	// ① remove
	removeByID := func(id string) bool {
		for i, p := range cur {
			if channelkeys.ID([]byte(p)) == id {
				cur = append(cur[:i], cur[i+1:]...)
				delete(curMeta, id)
				return true
			}
		}
		return false
	}
	for _, op := range ops {
		if op.Op != "remove" {
			continue
		}
		id := strings.TrimSpace(op.KeyID)
		if id != "" {
			if !removeByID(id) {
				return nil, nil, nil, ErrUnknownKey
			}
			continue
		}
		// 只允许「解密失败」的条目用下标删：能算出 keyID 的必须用 keyID，
		// 否则并发换序会把别人的 key 删掉。
		if op.Index == nil {
			return nil, nil, nil, ErrUnknownKey
		}
		idx := *op.Index
		if idx < 0 || idx >= len(dec.Entries) || dec.Entries[idx].OK() {
			return nil, nil, nil, ErrUnknownKey
		}
		// 坏条目不在 cur 里（cur 只装解密成功的明文），重建时会**自动消失**，
		// 所以这里无需改动 cur —— 这个 op 的作用是"确认并触发一次重建"。
		// 校验 index 合法即可，删任何其它位置都是错的（会删掉一把好 key）。
	}

	// ② replace（原地替换明文，可顺带改别名）
	//
	// 四条边界必须显式处理，否则都是"看起来对、写的时候错"：
	//   - 只认 keyID，不认 index：坏条目算不出 keyID，无法证明"改的是哪把"；
	//   - 只接受 key/key_id/alias：启用态有独立开关，静默忽略会让人以为一起改了；
	//   - 新值已存在于池中（非本行）→ 报错：两行共享同一 keyID 时启用开关与冷却键
	//     只有一个，"关一个另一个也关"无法解释；
	//   - alias 允许随本次替换落库：管理台一次编辑同时改别名与明文，否则只能发两次
	//     非原子请求（replace 会换掉 keyID，前端算不出新 ID，meta 追不上）。
	//
	// 位置不变是硬要求：polling 的轮转游标按下标推进，把 key 挪到池尾
	// 等于让它晚一轮才被用到，而没人能从界面上看出来。
	for _, op := range ops {
		if op.Op != "replace" {
			continue
		}
		oldID := strings.TrimSpace(op.KeyID)
		if oldID == "" {
			return nil, nil, nil, keyCfgErr("修改密钥必须指定 key_id（请刷新后重试）")
		}
		if op.Enabled != nil || op.Index != nil || len(op.Keys) > 0 {
			return nil, nil, nil, keyCfgErr("修改密钥只接受 key、key_id 与 alias；启用态请用 meta 操作")
		}
		nk, err := channelkeys.NormalizeKey(op.Key)
		if err != nil {
			return nil, nil, nil, keyCfgErr(err.Error())
		}
		pos := indexOfID(cur, oldID)
		if pos < 0 {
			return nil, nil, nil, ErrUnknownKey
		}
		newID := channelkeys.ID([]byte(nk))
		if newID != oldID {
			if containsID(cur, newID) {
				return nil, nil, nil, keyCfgErr("新密钥与池中另一把完全相同；如要替换请先删除那一把")
			}
			cur[pos] = nk
			// 两个 ID 都要清：旧 ID 留着会带旧退避；新 ID 可能是"换走又换回"
			// 的那把，不清就会带着历史冷却继续被跳过。
			resets = append(resets, oldID, newID)
		}
		// 属性的搬运不能挂在 newID != oldID 分支里，三个陷阱都在这一小段：
		//   1. 同值（只差首尾空白）时上面不会改池子，但 alias 仍要落库；
		//   2. m 必须从旧条目**整体继承** —— 重建 Meta{Alias} 会把 Enabled=false
		//      悄悄改回启用，选路立刻开始用一把被停用的 key；
		//   3. 旧行没有属性条目（绝大多数行没有别名）时 m 是零值，不能因此跳过写入。
		m, had := curMeta[oldID]
		if op.Alias != nil {
			m.Alias = strings.TrimSpace(*op.Alias)
		}
		if newID != oldID {
			delete(curMeta, oldID)
		}
		if op.Alias != nil || had {
			if m.IsZero() {
				delete(curMeta, newID)
			} else {
				curMeta[newID] = m
			}
		}
	}

	// ③ add（按 keyID 幂等：重复添加同一把 key 不报错、也不重复落库）
	for _, op := range ops {
		if op.Op != "add" {
			continue
		}
		k, err := channelkeys.NormalizeKey(op.Key)
		if err != nil {
			return nil, nil, nil, keyCfgErr(err.Error())
		}
		if id := channelkeys.ID([]byte(k)); !containsID(cur, id) {
			cur = append(cur, k)
			// 真正追加时才清：场景是"某把 key 401 被冷却 → 运营删掉 → 上游重置后加回来"，
			// 不清就会带着旧退避继续被跳过，表现是"加回来了但还是不用它"。
			resets = append(resets, id)
		}
	}

	// ④ meta
	for _, op := range ops {
		if op.Op != "meta" {
			continue
		}
		id := strings.TrimSpace(op.KeyID)
		if id == "" || !containsID(cur, id) {
			return nil, nil, nil, ErrUnknownKey
		}
		m := curMeta[id]
		if op.Alias != nil {
			m.Alias = strings.TrimSpace(*op.Alias)
		}
		if op.Enabled != nil {
			v := *op.Enabled
			m.Enabled = &v
		}
		if m.IsZero() {
			delete(curMeta, id)
		} else {
			curMeta[id] = m
		}
	}

	// ⑤ reset_cooldown（只动 Redis，汇总后由调用方在事务提交后执行）
	for _, op := range ops {
		if op.Op != "reset_cooldown" {
			continue
		}
		id := strings.TrimSpace(op.KeyID)
		if id == "" || !containsID(cur, id) {
			return nil, nil, nil, ErrUnknownKey
		}
		resets = append(resets, id)
	}

	// 容量校验放在所有 op 之后：add 会撑大池子，先判再算就会漏掉"加完才超"的情况。
	if len(cur) > channelkeys.MaxKeysPerChannel {
		return nil, nil, nil, keyCfgErr(fmt.Sprintf("密钥池最多 %d 把，本次操作后会变成 %d 把",
			channelkeys.MaxKeysPerChannel, len(cur)))
	}
	return cur, curMeta, resets, nil
}

// keyCfgErr 是密钥池配置错误的唯一构造点（Field 固定为 keys）。
func keyCfgErr(msg string) *relay.ConfigError {
	return &relay.ConfigError{Field: "keys", Scope: "渠道", Msg: msg}
}

// idsOf 返回明文池对应的 keyID 列表（保序）。
func idsOf(plains []string) []string {
	out := make([]string, 0, len(plains))
	for _, p := range plains {
		out = append(out, channelkeys.ID([]byte(p)))
	}
	return out
}

// indexOfID 返回 keyID 在明文池里的下标（找不到 -1）。
func indexOfID(plains []string, id string) int {
	for i, p := range plains {
		if channelkeys.ID([]byte(p)) == id {
			return i
		}
	}
	return -1
}

// plainsOf 取解密成功的明文（保序）。
func plainsOf(dec channelkeys.Result) []string {
	out := make([]string, 0, len(dec.Entries))
	for _, e := range dec.Entries {
		if e.OK() {
			out = append(out, e.Plain)
		}
	}
	return out
}

func containsID(plains []string, id string) bool {
	for _, p := range plains {
		if channelkeys.ID([]byte(p)) == id {
			return true
		}
	}
	return false
}

// buildKeys 组装密钥池视图（GET 与 PATCH 共用，保证两个出口口径一致）。
func buildKeys(ctx context.Context, c *model.Channel) (*KeysResult, error) {
	dec := channelkeys.Decrypt(c.Keys)
	meta := channelkeys.ParseMeta(c.KeyMeta)
	ids := make([]string, 0, len(dec.Entries))
	for _, e := range dec.Entries {
		if e.OK() {
			ids = append(ids, e.ID)
		}
	}
	// Redis 不可用时三者都返回空，视图退化为"没有冷却信息"（降级而不是报错）。
	cooling := keyhealth.CoolingState(ctx, c.ID, ids)
	ttls := keyhealth.CoolingTTLs(ctx, c.ID, ids)
	lastErr := keyhealth.LastErrors(ctx, c.ID, ids)

	items := make([]KeyView, 0, len(dec.Entries))
	enabledCount, broken := 0, dec.Failed
	if dec.Invalid {
		broken = 1
	}
	for _, e := range dec.Entries {
		v := KeyView{Index: e.Index, DecryptOK: e.OK()}
		if e.OK() {
			alias, enabled := meta.Effective(e.ID)
			v.KeyID, v.Fingerprint, v.Alias, v.Enabled = e.ID, channelkeys.Fingerprint(e.Plain), alias, enabled
			if enabled {
				enabledCount++
			}
			if reason, ok := cooling[e.ID]; ok {
				v.CooldownReason = reason
				// 逐行取本行剩余时长：用全池最小值会让"还剩 12s"与"还剩 9min"长得一样，
				// 运维按错误的时间决定"等一等"还是"换掉"。
				v.CooldownRemainingSec = int(ttls[e.ID] / time.Second)
			}
			if s := lastErr[e.ID]; s != "" {
				v.LastErrorStatus, v.LastErrorAt = parseErrState(s)
			}
		}
		items = append(items, v)
	}
	return &KeysResult{
		Version:         c.Version,
		Items:           items,
		KeyCount:        dec.Total,
		BrokenCount:     broken,
		KeyEnabledCount: enabledCount,
	}, nil
}

// parseErrState 解析 `status|reason|unix` 现场串；格式不符返回零值（展示层不该因脏数据报错）。
func parseErrState(s string) (int, string) {
	parts := strings.Split(s, "|")
	if len(parts) != 3 {
		return 0, ""
	}
	status, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, ""
	}
	ts, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || ts <= 0 {
		return 0, ""
	}
	return status, time.Unix(ts, 0).UTC().Format(time.RFC3339)
}

// keyStats 返回 (总条数, 可用条数, 解密失败条数)。可用 = 解密成功且启用。
func keyStats(c *model.Channel) (total, enabled, broken int) {
	dec := channelkeys.Decrypt(c.Keys)
	meta := channelkeys.ParseMeta(c.KeyMeta)
	broken = dec.Failed
	if dec.Invalid {
		broken = 1
	}
	for _, e := range dec.Entries {
		if !e.OK() {
			continue
		}
		if _, ok := meta.Effective(e.ID); ok {
			enabled++
		}
	}
	return dec.Total, enabled, broken
}

// enabledCountOf 计算「这份明文池 + 这份属性表」里可用的条数（写前校验用）。
func enabledCountOf(plains []string, meta channelkeys.MetaMap) int {
	n := 0
	for _, p := range plains {
		if _, enabled := meta.Effective(channelkeys.ID([]byte(p))); enabled {
			n++
		}
	}
	return n
}
