package channel

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/channelkeys"
)

// 本文件守 applyOps 的语义：这是**唯一**会改变密钥池的纯函数，
// PATCH 端点与表单保存（UpdateTx 的去重/属性重建）都依赖它。

func decOf(t *testing.T, keys ...string) channelkeys.Result {
	t.Helper()
	raw, err := channelkeys.Encrypt(keys)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	return channelkeys.Decrypt(raw)
}

func idOf(key string) string { return channelkeys.ID([]byte(key)) }

// boolPtr 用来构造"显式传了 enabled"的 op（nil = 未传 = 不改）。
func boolPtr(v bool) *bool { return &v }

// containsStr 判断切片里是否含某个值（resets 的顺序不属于契约，只断言"含"）。
func containsStr(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// TestApplyOpsRemoveThenAdd 断言顺序：先删后加（同一把 key 删了再加 = 最终存在且顺序在末尾）。
func TestApplyOpsRemoveThenAdd(t *testing.T) {
	dec := decOf(t, "sk-a", "sk-b")
	plains, meta, resets, err := applyOps(dec, channelkeys.MetaMap{"x": {}}, []KeyOp{
		{Op: "add", Key: "sk-a"}, // 已存在 → 幂等跳过
		{Op: "remove", KeyID: idOf("sk-a")},
		{Op: "add", Key: "sk-a"}, // 删了再加 → 追加到末尾
		{Op: "meta", KeyID: idOf("sk-b"), Alias: strPtr("备用")},
		{Op: "reset_cooldown", KeyID: idOf("sk-b")},
	})
	if err != nil {
		t.Fatalf("applyOps 失败: %v", err)
	}
	if len(plains) != 2 || plains[0] != "sk-b" || plains[1] != "sk-a" {
		t.Fatalf("应先删后加，实际 %v", plains)
	}
	// resets 现在有两类来源：真正追加的 key（清残留退避）与显式 reset_cooldown。
	// sk-a 是"删了再加"，必须清 —— 否则它带着上一次 401 的退避进池，加回来也不被使用。
	if !containsStr(resets, idOf("sk-b")) {
		t.Fatalf("reset 应收集到 sk-b，实际 %v", resets)
	}
	if !containsStr(resets, idOf("sk-a")) {
		t.Fatalf("新追加的 key 也应清一次残留冷却，实际 %v", resets)
	}
	if meta[idOf("sk-b")].Alias != "备用" {
		t.Fatalf("别名未写入: %v", meta)
	}
	// 孤儿属性（原 meta 里的 x）必须被 Reconcile 之外的逻辑保留原样：
	// applyOps 不做 GC，GC 由 UpdateTx 的 Reconcile 负责，两者职责不能混。
	if _, ok := meta["x"]; !ok {
		t.Fatal("applyOps 不应越权删除属性（GC 是 Reconcile 的职责）")
	}
}

// TestApplyOpsRejectsUnknown 断言未知 keyID 一律报错（不能静默忽略）。
func TestApplyOpsRejectsUnknown(t *testing.T) {
	dec := decOf(t, "sk-a")
	for _, ops := range [][]KeyOp{
		{{Op: "remove", KeyID: "nope"}},
		{{Op: "meta", KeyID: "nope", Alias: strPtr("x")}},
		{{Op: "reset_cooldown", KeyID: "nope"}},
		{{Op: "remove"}}, // 既无 key_id 也无 index
	} {
		if _, _, _, err := applyOps(dec, nil, ops); !errors.Is(err, ErrUnknownKey) {
			t.Errorf("%+v 应报 ErrUnknownKey，实际 %v", ops, err)
		}
	}
}

// TestApplyOpsRemoveBrokenByIndex 断言「解密失败的条目只能按下标删」，且删对了位置。
//
// 这是唯一一处下标参与写操作：坏 token 算不出 keyID，不按下标删就永远清不掉。
func TestApplyOpsRemoveBrokenByIndex(t *testing.T) {
	raw, err := channelkeys.Encrypt([]string{"sk-a", "sk-broken", "sk-c"})
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	var tokens []string
	if err := json.Unmarshal(raw, &tokens); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	tokens[1] = "not-a-token"
	broken, _ := json.Marshal(tokens)
	dec := channelkeys.Decrypt(model.JSON(broken))

	idx := 1
	plains, _, _, err := applyOps(dec, nil, []KeyOp{{Op: "remove", Index: &idx}})
	if err != nil {
		t.Fatalf("按下标删坏条目失败: %v", err)
	}
	if len(plains) != 2 || plains[0] != "sk-a" || plains[1] != "sk-c" {
		t.Fatalf("删错位置，实际 %v", plains)
	}

	// 能算出 keyID 的条目**不允许**用下标删：并发换序会删掉别人的 key。
	idx0 := 0
	if _, _, _, err := applyOps(dec, nil, []KeyOp{{Op: "remove", Index: &idx0}}); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("可用条目按下标删应报错，实际 %v", err)
	}
}

// TestApplyOpsAddDedup 断言同一把 key 重复添加只保留一条。
func TestApplyOpsAddDedup(t *testing.T) {
	dec := channelkeys.Result{}
	plains, _, _, err := applyOps(dec, nil, []KeyOp{
		{Op: "add", Key: "sk-dup"},
		{Op: "add", Key: "sk-dup"},
		{Op: "add", Key: "sk-other"},
	})
	if err != nil {
		t.Fatalf("applyOps 失败: %v", err)
	}
	if len(plains) != 2 {
		t.Fatalf("重复 key 应只留一条，实际 %v", plains)
	}
}

// TestApplyOpsUnknownOp 断言未识别的 op 报错（静默忽略会表现为"点了没反应但返回 200"）。
func TestApplyOpsUnknownOp(t *testing.T) {
	if _, _, _, err := applyOps(decOf(t, "sk-a"), nil, []KeyOp{{Op: "rotate"}}); err == nil {
		t.Fatal("未知 op 必须报错")
	}
}

// TestKeyStatsAndEnabledCount 断言「物理条数 / 可用条数 / 坏条数」三个数的口径。
//
// 三个数混用会让"5 把 key 全坏"看起来完全正常（key_count=5），
// 而实际上一把都用不了 —— 这正是改造前完全没有暴露的形态。
func TestKeyStatsAndEnabledCount(t *testing.T) {
	raw, err := channelkeys.Encrypt([]string{"sk-a", "sk-b", "sk-c"})
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	off := false
	meta, err := channelkeys.MarshalMeta(channelkeys.MetaMap{idOf("sk-b"): {Enabled: &off}})
	if err != nil {
		t.Fatalf("序列化属性失败: %v", err)
	}
	c := &model.Channel{Keys: raw, KeyMeta: meta}
	total, enabled, broken := keyStats(c)
	if total != 3 || enabled != 2 || broken != 0 {
		t.Fatalf("应为 3/2/0，实际 %d/%d/%d", total, enabled, broken)
	}

	// 全部禁用 → 可用 0
	all, _ := channelkeys.MarshalMeta(channelkeys.MetaMap{
		idOf("sk-a"): {Enabled: &off}, idOf("sk-b"): {Enabled: &off}, idOf("sk-c"): {Enabled: &off},
	})
	if _, en, _ := keyStats(&model.Channel{Keys: raw, KeyMeta: all}); en != 0 {
		t.Fatalf("全禁用应可用 0，实际 %d", en)
	}

	// enabledCountOf 与 keyStats 必须同口径（一个用于写前校验，一个用于展示）
	plains := []string{"sk-a", "sk-b", "sk-c"}
	if got := enabledCountOf(plains, channelkeys.ParseMeta(meta)); got != 2 {
		t.Fatalf("enabledCountOf 应为 2，实际 %d", got)
	}
}

// TestKeyStatsBroken 断言密钥列被写坏时按 1 条坏处理，不 panic。
func TestKeyStatsBroken(t *testing.T) {
	c := &model.Channel{Keys: model.JSON(`"not-an-array"`)}
	total, enabled, broken := keyStats(c)
	if total != 0 || enabled != 0 || broken != 1 {
		t.Fatalf("损坏列应为 0/0/1，实际 %d/%d/%d", total, enabled, broken)
	}
}

// ───────────────────────── replace：原地替换明文 ─────────────────────────

// TestApplyOpsReplaceKeepsPositionAndMeta 是 replace 的全部承诺：
// 位置不变、别名与启用态跟着"这一行"走、两个 keyID 的冷却都清。
func TestApplyOpsReplaceKeepsPositionAndMeta(t *testing.T) {
	off := false
	dec := decOf(t, "sk-a", "sk-b", "sk-c")
	meta := channelkeys.MetaMap{idOf("sk-b"): {Alias: "备用", Enabled: &off}}

	plains, outMeta, resets, err := applyOps(dec, meta, []KeyOp{
		{Op: "replace", KeyID: idOf("sk-b"), Key: "sk-b-new"},
	})
	if err != nil {
		t.Fatalf("replace 失败: %v", err)
	}
	// 位置不变：polling 的轮转游标按下标推进，挪到池尾会让它晚一轮才被用到。
	if len(plains) != 3 || plains[0] != "sk-a" || plains[1] != "sk-b-new" || plains[2] != "sk-c" {
		t.Fatalf("应原地替换，实际 %v", plains)
	}
	m, ok := outMeta[idOf("sk-b-new")]
	if !ok {
		t.Fatalf("新 keyID 应继承别名与启用态: %+v", outMeta)
	}
	if m.Alias != "备用" || m.Enabled == nil || *m.Enabled {
		t.Fatalf("别名/启用态未继承: %+v", m)
	}
	if _, stale := outMeta[idOf("sk-b")]; stale {
		t.Fatalf("旧 keyID 的属性必须删除: %+v", outMeta)
	}
	// 旧 ID 留着会带旧退避；新 ID 可能是"换走又换回"，也要清。
	if !containsStr(resets, idOf("sk-b")) || !containsStr(resets, idOf("sk-b-new")) {
		t.Fatalf("两个 keyID 的冷却都要清，实际 %v", resets)
	}
}

// TestApplyOpsReplaceSamePlaintextIsNoop 断言"只差首尾空白"是空操作而不是新增一条。
func TestApplyOpsReplaceSamePlaintextIsNoop(t *testing.T) {
	dec := decOf(t, "sk-a", "sk-b")
	plains, _, resets, err := applyOps(dec, nil, []KeyOp{
		{Op: "replace", KeyID: idOf("sk-a"), Key: "  sk-a  "},
	})
	if err != nil {
		t.Fatalf("同值替换应成功: %v", err)
	}
	if len(plains) != 2 || plains[0] != "sk-a" {
		t.Fatalf("同值替换不得改动池子: %v", plains)
	}
	if len(resets) != 0 {
		t.Fatalf("空操作不该清冷却: %v", resets)
	}
}

// TestApplyOpsReplaceWritesAlias 断言"一次编辑同时改别名与明文"是**一条** op 的原子结果。
//
// 三个反例都是静默丢别名：同值替换提前 continue、旧行没有属性条目、重建 Meta 丢启用态。
func TestApplyOpsReplaceWritesAlias(t *testing.T) {
	off := false

	// ① 旧行没有任何属性条目：别名必须能写进新 keyID，且不得凭空造出 enabled。
	dec := decOf(t, "sk-a", "sk-b")
	_, outMeta, resets, err := applyOps(dec, nil, []KeyOp{
		{Op: "replace", KeyID: idOf("sk-a"), Key: "sk-a-new", Alias: strPtr(" 主账号 ")},
	})
	if err != nil {
		t.Fatalf("replace 带 alias 失败: %v", err)
	}
	m, ok := outMeta[idOf("sk-a-new")]
	if !ok || m.Alias != "主账号" {
		t.Fatalf("别名应随替换落到新 keyID（并 trim）: %+v", outMeta)
	}
	if m.Enabled != nil {
		t.Fatalf("没传 enabled 就不该写启用位: %+v", m)
	}
	// 换了明文才清冷却；只改别名（见 ④）不能清。
	if !containsStr(resets, idOf("sk-a")) || !containsStr(resets, idOf("sk-a-new")) {
		t.Fatalf("两个 keyID 的冷却都要清: %v", resets)
	}

	// ② 旧行是停用态：别名随替换写入时，停用态不能被"重建 Meta"悄悄改回启用。
	meta := channelkeys.MetaMap{idOf("sk-b"): {Alias: "旧名", Enabled: &off}}
	_, outMeta, _, err = applyOps(dec, meta, []KeyOp{
		{Op: "replace", KeyID: idOf("sk-b"), Key: "sk-b-new", Alias: strPtr("新名")},
	})
	if err != nil {
		t.Fatalf("replace 带 alias 失败: %v", err)
	}
	m, ok = outMeta[idOf("sk-b-new")]
	if !ok || m.Alias != "新名" || m.Enabled == nil || *m.Enabled {
		t.Fatalf("别名要改、停用态要留: %+v", outMeta)
	}

	// ③ alias 传空串 = 清除；若这一行只剩别名（没有启用位），整条属性归零后删除。
	//    注意停用态的行清别名并不会被重新启用（②已覆盖），属性表里仍留 enabled:false。
	onlyAlias := channelkeys.MetaMap{idOf("sk-b"): {Alias: "旧名"}}
	_, outMeta, _, err = applyOps(dec, onlyAlias, []KeyOp{
		{Op: "replace", KeyID: idOf("sk-b"), Key: "sk-b-2", Alias: strPtr("")},
	})
	if err != nil {
		t.Fatalf("replace 带空别名失败: %v", err)
	}
	if _, stale := outMeta[idOf("sk-b-2")]; stale {
		t.Fatalf("空别名 + 无其它属性时应删除条目: %+v", outMeta)
	}

	// ④ 同值替换（只差首尾空白）不清冷却，但别名仍要生效 —— 否则"只改名"会被静默丢弃。
	_, outMeta, resets, err = applyOps(dec, nil, []KeyOp{
		{Op: "replace", KeyID: idOf("sk-a"), Key: "  sk-a  ", Alias: strPtr("只改名")},
	})
	if err != nil {
		t.Fatalf("同值 + 别名失败: %v", err)
	}
	if m, ok := outMeta[idOf("sk-a")]; !ok || m.Alias != "只改名" {
		t.Fatalf("同值时别名也要落库: %+v", outMeta)
	}
	if len(resets) != 0 {
		t.Fatalf("没换明文就不该清冷却: %v", resets)
	}
}

// TestApplyOpsReplaceRejectsConflicts 四条边界，都是"看起来能跑，实际会出鬼"的类型。
func TestApplyOpsReplaceRejectsConflicts(t *testing.T) {
	dec := decOf(t, "sk-a", "sk-b")

	cases := []struct {
		name string
		op   KeyOp
		want string
	}{
		{"缺 key_id", KeyOp{Op: "replace", Key: "sk-x"}, "key_id"},
		{"替换成池中另一把", KeyOp{Op: "replace", KeyID: idOf("sk-a"), Key: "sk-b"}, "完全相同"},
		{"只差空白的新值", KeyOp{Op: "replace", KeyID: idOf("sk-a"), Key: "sk a"}, "空格"},
		{"夹带 enabled", KeyOp{Op: "replace", KeyID: idOf("sk-a"), Key: "sk-x", Enabled: boolPtr(true)}, "只接受"},
	}
	for _, c := range cases {
		_, _, _, err := applyOps(dec, nil, []KeyOp{c.op})
		if err == nil {
			t.Fatalf("%s 应报错", c.name)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s 文案应含 %q，实际 %v", c.name, c.want, err)
		}
	}

	// 目标不存在 = 并发里被别人删了，必须是 ErrUnknownKey（前端据此提示"刷新"）。
	if _, _, _, err := applyOps(dec, nil, []KeyOp{{Op: "replace", KeyID: "nope", Key: "sk-x"}}); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("未知目标应报 ErrUnknownKey，实际 %v", err)
	}
}

// TestApplyOpsReplaceOrdering 断言 replace 在 remove 之后、add 之前。
func TestApplyOpsReplaceOrdering(t *testing.T) {
	dec := decOf(t, "sk-a", "sk-b")

	// remove + replace 同一把 = 矛盾输入：先删则 replace 报未知（而不是"先改再删"）。
	if _, _, _, err := applyOps(dec, nil, []KeyOp{
		{Op: "remove", KeyID: idOf("sk-a")},
		{Op: "replace", KeyID: idOf("sk-a"), Key: "sk-a2"},
	}); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("remove 后 replace 应报 ErrUnknownKey，实际 %v", err)
	}

	// replace A→X 与 add X 同批：replace 先跑，add 走到幂等分支，最终只有一把 X。
	plains, _, _, err := applyOps(dec, nil, []KeyOp{
		{Op: "replace", KeyID: idOf("sk-a"), Key: "sk-x"},
		{Op: "add", Key: "sk-x"},
	})
	if err != nil {
		t.Fatalf("replace+add 应幂等成功: %v", err)
	}
	if len(plains) != 2 {
		t.Fatalf("不得出现两把 sk-x，实际 %v", plains)
	}
}

// ───────────────────────── replace_all：整池重建 ─────────────────────────

// TestApplyOpsReplaceAll 断言整池重建保留同值 key 的属性、清冷却、拒绝混批与空池。
func TestApplyOpsReplaceAll(t *testing.T) {
	dec := decOf(t, "sk-a", "sk-b")
	meta := channelkeys.MetaMap{idOf("sk-b"): {Alias: "老备用"}}

	plains, outMeta, resets, err := applyOps(dec, meta, []KeyOp{
		{Op: "replace_all", Keys: []string{"sk-a", " sk-b ", "sk-c", "sk-c"}},
	})
	if err != nil {
		t.Fatalf("整池重建失败: %v", err)
	}
	if len(plains) != 3 || plains[1] != "sk-b" {
		t.Fatalf("应归一+去重保序，实际 %v", plains)
	}
	// 同值 key 的别名必须活着：整池轮换时把别名全清掉，面板第二天就认不出哪把是哪把。
	if outMeta[idOf("sk-b")].Alias != "老备用" {
		t.Fatalf("同值 key 的属性应保留: %+v", outMeta)
	}
	if !containsStr(resets, idOf("sk-c")) {
		t.Fatalf("新池里的 key 也应清一次残留冷却，实际 %v", resets)
	}

	if _, _, _, err := applyOps(dec, meta, []KeyOp{
		{Op: "replace_all", Keys: []string{"sk-a"}},
		{Op: "add", Key: "sk-b"},
	}); err == nil {
		t.Fatal("整池重建与增量操作混批必须报错（顺序语义不可定义）")
	}
	if _, _, _, err := applyOps(dec, meta, []KeyOp{{Op: "replace_all", Keys: nil}}); err == nil {
		t.Fatal("空池重建必须报错")
	}
}

// TestApplyOpsInvalidColumnOnlyAllowsReplaceAll 守「坏列不许被一条 add 静默清空」。
//
// keys 列不是合法 []string 时解密拿不到任何条目；此时 add 一条会把整列替换成那一把，
// 运营以为在"加一把"，实际把其余全丢了。
func TestApplyOpsInvalidColumnOnlyAllowsReplaceAll(t *testing.T) {
	bad := channelkeys.Result{Invalid: true}

	if _, _, _, err := applyOps(bad, nil, []KeyOp{{Op: "add", Key: "sk-x"}}); err == nil {
		t.Fatal("坏列时 add 必须报错")
	}
	plains, _, _, err := applyOps(bad, nil, []KeyOp{{Op: "replace_all", Keys: []string{"sk-1", "sk-2"}}})
	if err != nil {
		t.Fatalf("坏列必须允许显式重建: %v", err)
	}
	if len(plains) != 2 {
		t.Fatalf("重建后应为 2 把，实际 %v", plains)
	}
}

// TestApplyOpsCapacityLimit 断言 100 把是硬上限，且报错带实际值。
func TestApplyOpsCapacityLimit(t *testing.T) {
	keys := make([]string, 0, channelkeys.MaxKeysPerChannel+1)
	for i := 0; i <= channelkeys.MaxKeysPerChannel; i++ {
		keys = append(keys, fmt.Sprintf("sk-%03d", i))
	}

	// 100 把：正好通过。
	if _, _, _, err := applyOps(channelkeys.Result{}, nil, []KeyOp{{Op: "replace_all", Keys: keys[:100]}}); err != nil {
		t.Fatalf("100 把应通过: %v", err)
	}
	// 101 把：拒绝，且文案要带实际值（"超限"三个字没法让人判断该删多少）。
	_, _, _, err := applyOps(channelkeys.Result{}, nil, []KeyOp{{Op: "replace_all", Keys: keys}})
	if err == nil || !strings.Contains(err.Error(), "101") {
		t.Fatalf("101 把应报错并带实际值，实际 %v", err)
	}

	// add 也要吃到同一个上限：99 把后再加 2 把 = 101。
	dec := decOf(t, keys[:99]...)
	if _, _, _, err := applyOps(dec, nil, []KeyOp{
		{Op: "add", Key: keys[99]}, {Op: "add", Key: keys[100]},
	}); err == nil {
		t.Fatal("add 撑过上限必须报错")
	}
}
