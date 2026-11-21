package appplugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/pagination"
)

/**
 * 列表查询（排序/筛选）与"读写形状一致"的回归。
 *
 * 这些用例守的不是"能跑通"，而是几条口径：
 *  1. 排序/筛选**只认 manifest 白名单**，白名单外是 400（不是静默忽略）；
 *  2. 排序值类型坏掉（force 改过字段类型）时**不许 500**，坏行排末尾；
 *  3. 导出与列表同一批行（共用一个 BuildQuery）；
 *  4. 读出来的 data 只含当前 manifest 的字段 —— 否则"看完就改"必然 422。
 */

// sortableManifest 覆盖四种可筛选/可排序类型 + 批量删除动作 + 内置默认排序。
const sortableManifest = `{"apiVersion":1,"key":"mini","name":"迷你","version":"1.0.0","entities":[
	{"key":"a","name":"A","unique":"no","fields":[
		{"key":"no","label":"编号","type":"string","required":true,"maxLen":16},
		{"key":"n","label":"数量","type":"int"},
		{"key":"due","label":"截止","type":"date"},
		{"key":"done","label":"完成","type":"bool"},
		{"key":"memo","label":"备注","type":"text","maxLen":100}],
	 "list":{"columns":["no","n","due","done"],"actions":["create","update","delete","export","batch_delete"],
		"sort":[{"field":"n","order":"asc"},{"field":"due","order":"asc"},{"field":"record_key","order":"desc"}],
		"filters":["done","n","due","no"]}}]}`

func importRaw(t *testing.T, gdb *gorm.DB, raw, key string, expected int) *ImportResult {
	t.Helper()
	res, err := Import(context.Background(), gdb, ImportInput{Key: key, Raw: []byte(raw), ExpectedVersion: expected, AdminID: 1})
	if err != nil {
		t.Fatalf("导入 %s 失败: %v", key, err)
	}
	return res
}

// paramsOf 把 URL 参数翻成 pagination.Params：`sort` 的解析必须与 HTTP 层同源
// （pagination.Parse -> ParseSort），否则测试会绕过"格式校验"这一环。
func paramsOf(t *testing.T, v url.Values) pagination.Params {
	t.Helper()
	p := pagination.Params{Page: 1, PageSize: 50}
	if raw := v.Get("sort"); raw != "" {
		f, desc, err := pagination.ParseSort(raw)
		if err != nil {
			t.Fatalf("排序参数 %q 非法: %v", raw, err)
		}
		p.Field, p.Desc = f, desc
	}
	return p
}

func listWith(t *testing.T, gdb *gorm.DB, appID uint64, key, entity string, params url.Values) ([]RecordItem, int64) {
	t.Helper()
	if params == nil {
		params = url.Values{}
	}
	items, total, err := ListRecords(context.Background(), gdb, appID, key, entity,
		paramsOf(t, params), params)
	if err != nil {
		t.Fatalf("列表失败（%v）: %v", params, err)
	}
	return items, total
}

// installLock 读安装行的乐观锁版本（Patch 的 CAS 是**安装行**的版本，
// 与目录行的 lock_version 不是同一个数）。
func installLock(t *testing.T, gdb *gorm.DB, key string) int {
	t.Helper()
	inst, err := getInstall(context.Background(), gdb, 7, key)
	if err != nil {
		t.Fatalf("取安装行失败: %v", err)
	}
	return inst.LockVersion
}

func nosOf(items []RecordItem) string {
	out := make([]string, 0, len(items))
	for i := range items {
		out = append(out, fmt.Sprint(items[i].Data["no"]))
	}
	return strings.Join(out, ",")
}

// seedSortable 建三条记录：A(5, 03-01, 未完成) / B(2, 01-15, 完成) / C(9, 12-31, 完成)。
func seedSortable(t *testing.T, gdb *gorm.DB) {
	t.Helper()
	importRaw(t, gdb, sortableManifest, "mini", 0)
	if _, err := Install(context.Background(), gdb, 7, "mini", map[string]any{}); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	createRecord(t, gdb, 7, "mini", "a", map[string]any{"no": "AAA", "n": 5, "due": "2026-03-01", "done": false})
	createRecord(t, gdb, 7, "mini", "a", map[string]any{"no": "BBB", "n": 2, "due": "2026-01-15", "done": true})
	createRecord(t, gdb, 7, "mini", "a", map[string]any{"no": "CCC", "n": 9, "due": "2026-12-31", "done": true})
}

func TestListSortByDeclaredFields(t *testing.T) {
	gdb := withPluginDB(t)
	seedSortable(t, gdb)

	items, total := listWith(t, gdb, 7, "mini", "a", url.Values{"sort": {"n:asc"}})
	if total != 3 || nosOf(items) != "BBB,AAA,CCC" {
		t.Fatalf("按数量升序应为 BBB,AAA,CCC，实际 %q（total=%d）", nosOf(items), total)
	}
	items, _ = listWith(t, gdb, 7, "mini", "a", url.Values{"sort": {"due:desc"}})
	if nosOf(items) != "CCC,AAA,BBB" {
		t.Fatalf("按日期降序应为 CCC,AAA,BBB，实际 %q", nosOf(items))
	}
	// 内置键：编号（ULID 字典序 ≈ 创建顺序）
	items, _ = listWith(t, gdb, 7, "mini", "a", url.Values{"sort": {"record_key:asc"}})
	if nosOf(items) != "AAA,BBB,CCC" {
		t.Fatalf("按编号升序应为创建顺序，实际 %q", nosOf(items))
	}
}

// 白名单外的排序/筛选必须是 400（旧实现是"解析完静默丢弃"，用户以为生效了）。
func TestListRejectsUndeclaredSortAndFilter(t *testing.T) {
	gdb := withPluginDB(t)
	seedSortable(t, gdb)

	cases := []url.Values{
		{"sort": {"memo:asc"}},   // memo 存在但没进 sort 白名单
		{"sort": {"nope:asc"}},   // 字段不存在
		{"f_memo": {"x"}},        // 没进 filters 白名单
		{"f_n": {"abc.."}},       // int 区间不合法
		{"f_done": {"maybe"}},    // bool 取值不合法
		{"f_n": {"5"}},           // 区间缺少 `..`
	}
	for _, p := range cases {
		_, _, err := ListRecords(context.Background(), gdb, 7, "mini", "a", paramsOf(t, p), p)
		var qe *QueryError
		if !errors.As(err, &qe) {
			t.Fatalf("%v 应返回参数错误，实际 %v", p, err)
		}
	}
}

func TestListFilters(t *testing.T) {
	gdb := withPluginDB(t)
	seedSortable(t, gdb)

	cases := []struct {
		name   string
		params url.Values
		want   string
	}{
		{"bool 等值", url.Values{"f_done": {"1"}, "sort": {"n:asc"}}, "BBB,CCC"},
		{"bool 等值 0", url.Values{"f_done": {"0"}}, "AAA"},
		{"int 区间", url.Values{"f_n": {"3..6"}}, "AAA"},
		{"int 半开区间", url.Values{"f_n": {"..3"}}, "BBB"},
		{"date 区间", url.Values{"f_due": {"2026-01-01..2026-02-01"}}, "BBB"},
		{"字符串包含（大小写不敏感）", url.Values{"f_no": {"bb"}}, "BBB"},
		// `_` 在 LIKE 里是通配符：不转义会命中 axb；这里没有任何行含下划线。
		{"字符串包含的 LIKE 转义", url.Values{"f_no": {"a_b"}}, ""},
	}
	for _, c := range cases {
		items, total := listWith(t, gdb, 7, "mini", "a", c.params)
		if got := nosOf(items); got != c.want {
			t.Fatalf("%s：期望 %q，实际 %q（total=%d）", c.name, c.want, got, total)
		}
	}
}

// force 改过字段类型后，库里会有不符合当前类型的旧值：排序不许 500。
func TestListSortGuardsDirtyValues(t *testing.T) {
	gdb := withPluginDB(t)
	seedSortable(t, gdb)

	dirty := model.JSON(`{"no":"DDD","n":"not-a-number","due":"2026-05-01"}`)
	if err := gdb.Model(&model.AppPluginRecord{}).Where("app_id = ? AND plugin_key = ?", 7, "mini").
		Update("data", dirty).Error; err != nil {
		t.Fatalf("写入脏值失败: %v", err)
	}
	// 把 DDD 之外的一条改成脏值更精确；这里三条都变成脏值也不影响断言 ——
	// 关键是"不 500 且脏值排在后面"。
	items, total := listWith(t, gdb, 7, "mini", "a", url.Values{"sort": {"n:asc"}})
	if total != 3 || len(items) != 3 {
		t.Fatalf("脏值不应让查询失败，实际 total=%d", total)
	}
}

// 导出与列表必须同一批行（同一组筛选下 total 相等）。
func TestExportTotalsMatchFilteredList(t *testing.T) {
	gdb := withPluginDB(t)
	seedSortable(t, gdb)

	params := url.Values{"f_done": {"1"}}
	_, total := listWith(t, gdb, 7, "mini", "a", params)
	_, q, n, err := ExportPlan(context.Background(), gdb, 7, "mini", "a",
		pagination.Params{Page: 1, PageSize: 20}, params)
	if err != nil {
		t.Fatalf("导出计划失败: %v", err)
	}
	if n != total || n != 2 {
		t.Fatalf("导出应与列表同源：列表 %d / 导出 %d（期望 2）", total, n)
	}
	rows, err := ExportBatch(context.Background(), gdb, 7, "mini", "a", func() *Entity {
		_, m, _ := loadForRead(context.Background(), gdb, 7, "mini")
		e, _ := m.EntityByKey("a")
		return e
	}(), q, 0, 100)
	if err != nil || len(rows) != 2 {
		t.Fatalf("导出行应为 2，实际 %d err=%v", len(rows), err)
	}
}

// 升级删字段后：读出来的 data 不含该字段，原样回写必须成功（旧实现必然 422）。
func TestListItemProjectsToCurrentManifest(t *testing.T) {
	gdb := withPluginDB(t)
	importRaw(t, gdb, mini, "mini", 0)
	if _, err := Install(context.Background(), gdb, 7, "mini", map[string]any{}); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	rec := createRecord(t, gdb, 7, "mini", "a", map[string]any{"no": "N-1", "memo": "写过备注"})

	// v1.1.0 删掉 memo（破坏性变更，需 force）
	res, err := Import(context.Background(), gdb, ImportInput{Key: "mini", Raw: []byte(miniWithoutMemo), ExpectedVersion: 1, Force: true})
	if err != nil || !res.Applied {
		t.Fatalf("强制升级失败: %v", err)
	}

	items, total := listWith(t, gdb, 7, "mini", "a", nil)
	if total != 1 || len(items) != 1 {
		t.Fatalf("升级后应仍能读到 1 条，实际 %d", total)
	}
	if _, ok := items[0].Data["memo"]; ok {
		t.Fatalf("未声明字段不应下发：%v", items[0].Data)
	}
	// 把读到的 data 原样写回：这是管理台"打开编辑 → 保存"的真实路径。
	raw, _ := json.Marshal(items[0].Data)
	if _, err := Update(context.Background(), gdb, 7, "mini", "a", rec.RecordKey, raw, items[0].Version); err != nil {
		t.Fatalf("按当前形状回写应成功，实际 %v", err)
	}
}

// 批量删除：去重、计数、上限、动作闸。
func TestBatchDeleteRecords(t *testing.T) {
	gdb := withPluginDB(t)
	seedSortable(t, gdb)
	items, _ := listWith(t, gdb, 7, "mini", "a", nil)
	keys := []string{items[0].RecordKey, items[1].RecordKey, items[0].RecordKey, "01NOTEXIST0000000000000000"}

	n, err := BatchDelete(context.Background(), gdb, 7, "mini", "a", keys)
	if err != nil || n != 2 {
		t.Fatalf("应删除 2 条（重复与不存在的键不计），实际 n=%d err=%v", n, err)
	}
	if _, total := listWith(t, gdb, 7, "mini", "a", nil); total != 1 {
		t.Fatalf("删除后应剩 1 条，实际 %d", total)
	}

	tooMany := make([]string, 0, MaxBatchDelete+1)
	for i := 0; i <= MaxBatchDelete; i++ {
		tooMany = append(tooMany, fmt.Sprintf("01KEY%020d", i))
	}
	var qe *QueryError
	if _, err := BatchDelete(context.Background(), gdb, 7, "mini", "a", tooMany); !errors.As(err, &qe) {
		t.Fatalf("超过上限应返回参数错误，实际 %v", err)
	}
}

// ---- 配置升级兼容（P0-3 / P1-3）----

const cfgAB = `{"apiVersion":1,"key":"cfgp","name":"配置","version":"1.0.0","config":[
	{"key":"a","label":"甲","type":"string","required":true},
	{"key":"b","label":"乙","type":"string"}],
	"entities":[{"key":"x","name":"X","fields":[
		{"key":"no","label":"编号","type":"string","required":true,"maxLen":16}],
	 "list":{"columns":["no"],"actions":["create"]}}]}`

const cfgAOnly = `{"apiVersion":1,"key":"cfgp","name":"配置","version":"1.1.0","config":[
	{"key":"a","label":"甲","type":"string","required":true}],
	"entities":[{"key":"x","name":"X","fields":[
		{"key":"no","label":"编号","type":"string","required":true,"maxLen":16}],
	 "list":{"columns":["no"],"actions":["create"]}}]}`

const cfgABY = `{"apiVersion":1,"key":"cfgp","name":"配置","version":"1.2.0","config":[
	{"key":"a","label":"甲","type":"string","required":true},
	{"key":"b","label":"乙","type":"string"},
	{"key":"y","label":"丙","type":"string","required":true}],
	"entities":[{"key":"x","name":"X","fields":[
		{"key":"no","label":"编号","type":"string","required":true,"maxLen":16}],
	 "list":{"columns":["no"],"actions":["create"]}}]}`

// 升级删掉一个配置键后，任何保存都必须还能成功（否者连"关掉对外只读"都做不到）。
func TestPatchDropsConfigKeysRemovedFromManifest(t *testing.T) {
	gdb := withPluginDB(t)
	ctx := context.Background()
	importRaw(t, gdb, cfgAB, "cfgp", 0)
	if _, err := Install(ctx, gdb, 7, "cfgp", map[string]any{"a": "1", "b": "2"}); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	importRaw(t, gdb, cfgAOnly, "cfgp", 1)

	disp := int16(model.AppPluginInstallDisabled)
	// 第一次保存就会把"当前 manifest 不承认的键"清掉，并回报 dropped。
	_, dropped, err := Patch(ctx, gdb, 7, "cfgp", PatchInput{Status: &disp, ExpectedVersion: installLock(t, gdb, "cfgp")})
	if err != nil {
		t.Fatalf("停用应成功（存量含已删配置键）: %v", err)
	}
	if len(dropped) != 1 || dropped[0] != "b" {
		t.Fatalf("应报告被清理的键 b，实际 %v", dropped)
	}
	yes := true
	row, dropped2, err := Patch(ctx, gdb, 7, "cfgp", PatchInput{PublicEnabled: &yes, ExpectedVersion: installLock(t, gdb, "cfgp")})
	if err != nil {
		t.Fatalf("改对外只读应成功: %v", err)
	}
	if len(dropped2) != 0 {
		t.Fatalf("已经清过的键不应重复报告，实际 %v", dropped2)
	}
	if strings.Contains(string(row.Config), `"b"`) {
		t.Fatalf("被清理的键不应留在库里：%s", row.Config)
	}
}

// 必填口径：转启用全量校验；保持启用只拦"显式清空"；缺项要能被页面看见。
func TestRequiredConfigRules(t *testing.T) {
	gdb := withPluginDB(t)
	ctx := context.Background()
	importRaw(t, gdb, cfgAB, "cfgp", 0)
	if _, err := Install(ctx, gdb, 7, "cfgp", map[string]any{"a": "1", "b": "2"}); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	// v1.2.0 新增必填 y：升级不阻断，但受影响清单要提示"已安装的应用需要补填"。
	res := importRaw(t, gdb, cfgABY, "cfgp", 1)
	found := false
	for _, a := range res.Affected {
		if a.Field == "config.y" && strings.Contains(a.Reason, "补填") {
			found = true
		}
	}
	if !found {
		t.Fatalf("升级应提示新增必填配置的影响面，实际 %+v", res.Affected)
	}

	// ① 详情里能看到缺项（否则管理员无从下手）
	d, err := GetDetail(ctx, gdb, 7, "cfgp")
	if err != nil {
		t.Fatalf("取详情失败: %v", err)
	}
	if len(d.ConfigMissing) != 1 || d.ConfigMissing[0] != "y" {
		t.Fatalf("config_missing 应为 [y]，实际 %v", d.ConfigMissing)
	}

	// ② 保持启用时改开关：允许（缺 y 不拦"改其它项"）
	yes := true
	if _, _, err := Patch(ctx, gdb, 7, "cfgp", PatchInput{PublicEnabled: &yes, ExpectedVersion: installLock(t, gdb, "cfgp")}); err != nil {
		t.Fatalf("缺必填项时改开关应允许: %v", err)
	}

	// ③ 显式传空：拒绝（必填项不允许被清空）
	empty := map[string]any{"a": ""}
	if _, _, err := Patch(ctx, gdb, 7, "cfgp", PatchInput{Config: &empty, ExpectedVersion: installLock(t, gdb, "cfgp")}); err == nil {
		t.Fatal("显式清空必填项应被拒绝")
	}

	// ④ 停用再启用：全量校验，缺 y 必须拦
	disp := int16(model.AppPluginInstallDisabled)
	if _, _, err := Patch(ctx, gdb, 7, "cfgp", PatchInput{Status: &disp, ExpectedVersion: installLock(t, gdb, "cfgp")}); err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	en := int16(model.AppPluginInstallEnabled)
	if _, _, err := Patch(ctx, gdb, 7, "cfgp", PatchInput{Status: &en, ExpectedVersion: installLock(t, gdb, "cfgp")}); err == nil {
		t.Fatal("缺必填项时重新启用必须被拒绝")
	}
}
