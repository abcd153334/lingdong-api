package appplugins

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/model"
)

// 数据库集成用例：缺 LD_TEST_DSN 就 skip。注意 skip 不等于通过 ——
// 跑全量时必须确认这些用例真的执行了（否则"全绿"是假象）。
const pluginTestSchema = "ld_test_app_plugin"

func withPluginDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("LD_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 LD_TEST_DSN，跳过数据库集成用例")
	}
	silent := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), silent)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + pluginTestSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+pluginTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.AppPlugin{}, &model.AppPluginInstall{}, &model.AppPluginRecord{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	// 测试库不跑启动期的 EnsureIndexes，所以唯一索引要在这里补：
	// 少了它们，"重复安装 409""唯一值冲突 409"两条用例会假绿（都变成插入成功）。
	// 只补本案依赖的索引，其余字段语义仍由 db/indexes.go 唯一声明。
	for _, ddl := range []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_app_plugins_key ON app_plugins (key)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_plugin_installs_app_key ON app_plugin_installs (app_id, plugin_key)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_plugin_records_key ON app_plugin_records (app_id, plugin_key, entity, record_key)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_plugin_records_unique ON app_plugin_records (app_id, plugin_key, entity, unique_norm)`,
	} {
		if err := gdb.Exec(ddl).Error; err != nil {
			t.Fatalf("建测试索引失败: %v", err)
		}
	}
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + pluginTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return gdb
}

// mini 是一份最小可用定义：改字段只要改这一个字符串，避免在测试里
// 用 strings.Replace 拼 JSON（拼错时测试会以"语法错误"而不是"断言失败"结束）。
const mini = `{"apiVersion":1,"key":"mini","name":"迷你","version":"1.0.0","entities":[
	{"key":"a","name":"A","unique":"no","fields":[
		{"key":"no","label":"编号","type":"string","required":true,"maxLen":16},
		{"key":"memo","label":"备注","type":"text","maxLen":100}],
	 "list":{"columns":["no","memo"],"actions":["create","update","delete","export"]}}]}`

// miniPublic 用来说明对外白名单：a 只公开 no（memo 不公开），b 完全不公开。
const miniPublic = `{"apiVersion":1,"key":"mini","name":"迷你","version":"1.0.0","entities":[
	{"key":"a","name":"A","unique":"no","public":["no"],"fields":[
		{"key":"no","label":"编号","type":"string","required":true,"maxLen":16},
		{"key":"memo","label":"备注","type":"text","maxLen":100}],
	 "list":{"columns":["no","memo"],"actions":["create","update"]}},
	{"key":"b","name":"B","fields":[
		{"key":"no","label":"编号","type":"string","required":true,"maxLen":16}],
	 "list":{"columns":["no"],"actions":["create"]}}]}`

// miniWithoutMemo 删掉 memo：用来验证"删字段属于破坏性变更"。
const miniWithoutMemo = `{"apiVersion":1,"key":"mini","name":"迷你","version":"1.1.0","entities":[
	{"key":"a","name":"A","unique":"no","fields":[
		{"key":"no","label":"编号","type":"string","required":true,"maxLen":16}],
	 "list":{"columns":["no"],"actions":["create","update"]}}]}`

// miniLimited 容量 2：验证写入上限。
const miniLimited = `{"apiVersion":1,"key":"mini","name":"迷你","version":"1.0.0","limits":{"maxRecords":2},
	"entities":[{"key":"a","name":"A","fields":[
		{"key":"no","label":"编号","type":"string","required":true,"maxLen":16}],
	 "list":{"columns":["no"],"actions":["create","update","delete","export"]}}]}`

func importOK(t *testing.T, gdb *gorm.DB, raw string, expected int, force bool) *ImportResult {
	t.Helper()
	res, err := Import(context.Background(), gdb, ImportInput{
		Key: "mini", Raw: []byte(raw), ExpectedVersion: expected, Force: force, AdminID: 1,
	})
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if !res.Applied {
		t.Fatalf("导入应落库，实际 %+v", res)
	}
	return res
}

// TestImportCAS 两个 PUT 必须有一个拿到 409：否则 AI 的旧稿会静默覆盖运营的微调。
func TestImportCAS(t *testing.T) {
	gdb := withPluginDB(t)
	ctx := context.Background()

	first := importOK(t, gdb, mini, 0, false)
	if first.LockVersion != 1 || !first.Created {
		t.Fatalf("首次导入应创建且版本为 1，实际 %+v", first)
	}

	_, err := Import(ctx, gdb, ImportInput{Key: "mini", Raw: []byte(mini), ExpectedVersion: 0})
	if err != ErrVersionConflict {
		t.Fatalf("旧版本号必须 409，实际 %v", err)
	}

	second := importOK(t, gdb, mini, 1, false)
	if second.LockVersion != 2 || second.Created {
		t.Fatalf("第二次导入应更新且版本为 2，实际 %+v", second)
	}

	// 目录行不存在却带着版本号：说明前端拿的是已被删除的旧页面。
	_, err = Import(ctx, gdb, ImportInput{Key: "ghost", Raw: []byte(strings.Replace(mini, `"mini"`, `"ghost"`, 1)), ExpectedVersion: 3})
	if err != ErrVersionConflict {
		t.Fatalf("目标不存在却带版本号必须 409，实际 %v", err)
	}
}

// TestImportDryRunDoesNotWrite 预检必须真的不落库：否则"先看看影响面"就是
// "已经改了但显示没改"。
func TestImportDryRunDoesNotWrite(t *testing.T) {
	gdb := withPluginDB(t)
	ctx := context.Background()
	importOK(t, gdb, mini, 0, false)
	if _, err := Install(ctx, gdb, 7, "mini", map[string]any{}); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	createRecord(t, gdb, 7, "mini", "a", map[string]any{"no": "N-1", "memo": "留一条"})

	before, _, err := Get(ctx, gdb, "mini")
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	res, err := Import(ctx, gdb, ImportInput{
		Key: "mini", Raw: []byte(miniWithoutMemo), ExpectedVersion: before.LockVersion, DryRun: true,
	})
	if err != nil {
		t.Fatalf("预检不应报错: %v", err)
	}
	if res.Applied {
		t.Fatal("预检不得落库")
	}
	if len(res.Breaking) == 0 {
		t.Fatal("删字段必须报为破坏性变更")
	}
	if len(res.Affected) == 0 || res.Affected[0].Records != 1 {
		t.Fatalf("应报告受影响记录数，实际 %+v", res.Affected)
	}

	after, _, err := Get(ctx, gdb, "mini")
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	if after.LockVersion != before.LockVersion || after.Version != before.Version {
		t.Fatalf("预检后目录不得变化: before=%+v after=%+v", before, after)
	}
}

// TestImportBreakingNeedsForce 破坏性变更没有 force 必须拒绝，且要带 breaking 标记 ——
// 前端据它决定是否显示"我确认"的勾选框。
func TestImportBreakingNeedsForce(t *testing.T) {
	gdb := withPluginDB(t)
	ctx := context.Background()
	importOK(t, gdb, mini, 0, false)

	_, err := Import(ctx, gdb, ImportInput{
		Key: "mini", Raw: []byte(miniWithoutMemo), ExpectedVersion: 1,
	})
	invalid, ok := err.(*InvalidError)
	if !ok || !invalid.Breaking {
		t.Fatalf("应返回破坏性校验错误，实际 %v", err)
	}

	res := importOK(t, gdb, miniWithoutMemo, 1, true)
	if res.LockVersion != 2 {
		t.Fatalf("force 后应写入，实际 %+v", res)
	}
}

// TestInstallLifecycle 三态流转：启用 → 停用（可读不可写）→ 卸载（读写都拒）
// → 重新启用（数据仍在）→ 清除（归零）。
func TestInstallLifecycle(t *testing.T) {
	gdb := withPluginDB(t)
	ctx := context.Background()
	importOK(t, gdb, mini, 0, false)

	if _, err := Install(ctx, gdb, 7, "mini", map[string]any{}); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if _, err := Install(ctx, gdb, 7, "mini", map[string]any{}); err != ErrAlreadyInstalled {
		t.Fatalf("重复安装必须 409，实际 %v", err)
	}
	rec := createRecord(t, gdb, 7, "mini", "a", map[string]any{"no": "N-1"})

	// 停用：读放行、写拒绝。
	disabled := model.AppPluginInstallDisabled
	if _, err := Patch(ctx, gdb, 7, "mini", PatchInput{Status: &disabled, ExpectedVersion: 1}); err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	if _, _, err := ListRecords(ctx, gdb, 7, "mini", "a", "", 0, 10); err != nil {
		t.Fatalf("停用后仍应可读: %v", err)
	}
	if _, err := Create(ctx, gdb, 7, "mini", "a", json.RawMessage(`{"no":"N-2"}`)); err == nil {
		t.Fatal("停用后不得写入")
	}

	// 卸载：读写都拒，但描述符必须 200（否则"重新启用"的入口消失）。
	if err := Uninstall(ctx, gdb, 7, "mini"); err != nil {
		t.Fatalf("卸载失败: %v", err)
	}
	if _, _, err := ListRecords(ctx, gdb, 7, "mini", "a", "", 0, 10); err != ErrNotInstalled {
		t.Fatalf("卸载后读应报未安装，实际 %v", err)
	}
	d, err := GetDetail(ctx, gdb, 7, "mini")
	if err != nil {
		t.Fatalf("卸载后描述符必须仍可取: %v", err)
	}
	if d.Install.Status != model.AppPluginInstallRemoved || d.RecordCount != 1 {
		t.Fatalf("描述符应显示已卸载且保留记录数，实际 %+v", d.Install)
	}

	// 重新启用：记录仍在。
	enabled := model.AppPluginInstallEnabled
	// 卸载也会 +1（每次状态变化都推进版本），所以这里用 3 而不是 2。
	if _, err := Patch(ctx, gdb, 7, "mini", PatchInput{Status: &enabled, ExpectedVersion: 3}); err != nil {
		t.Fatalf("重新启用失败: %v", err)
	}
	items, total, err := ListRecords(ctx, gdb, 7, "mini", "a", "", 0, 10)
	if err != nil || total != 1 || len(items) != 1 {
		t.Fatalf("重新启用后记录应仍在，实际 total=%d err=%v", total, err)
	}
	if items[0].RecordKey != rec.RecordKey {
		t.Fatalf("记录身份不得变化: %s != %s", items[0].RecordKey, rec.RecordKey)
	}

	// 清除数据必须先卸载。
	if _, err := Purge(ctx, gdb, 7, "mini"); err != ErrPurgeNotAllowed {
		t.Fatalf("未卸载就清除必须拒绝，实际 %v", err)
	}
	if err := Uninstall(ctx, gdb, 7, "mini"); err != nil {
		t.Fatalf("再次卸载失败: %v", err)
	}
	n, err := Purge(ctx, gdb, 7, "mini")
	if err != nil || n != 1 {
		t.Fatalf("清除应删 1 条，实际 n=%d err=%v", n, err)
	}
}

// TestRecordUniqueAndVersion 唯一值与乐观锁是两个独立判据：混成一个码会让
// 前端不知道该"换个值"还是"刷新重试"。
func TestRecordUniqueAndVersion(t *testing.T) {
	gdb := withPluginDB(t)
	ctx := context.Background()
	importOK(t, gdb, mini, 0, false)
	if _, err := Install(ctx, gdb, 7, "mini", map[string]any{}); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	r1 := createRecord(t, gdb, 7, "mini", "a", map[string]any{"no": "N-1"})

	// 大小写不同也算同一个值：归一规则只有 trim+lower 一条。
	_, err := Create(ctx, gdb, 7, "mini", "a", json.RawMessage(`{"no":" n-1 "}`))
	if err != ErrRecordConflict {
		t.Fatalf("唯一值冲突必须 409，实际 %v", err)
	}

	if _, err := Update(ctx, gdb, 7, "mini", "a", r1.RecordKey,
		json.RawMessage(`{"no":"N-1","memo":"改"}`), 99); err != ErrVersionConflict {
		t.Fatalf("版本不符必须 409，实际 %v", err)
	}
	updated, err := Update(ctx, gdb, 7, "mini", "a", r1.RecordKey,
		json.RawMessage(`{"no":"N-1","memo":"改"}`), r1.Version)
	if err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	if updated.Data["memo"] != "改" || updated.Version != r1.Version+1 {
		t.Fatalf("更新结果不对: %+v", updated)
	}

	// 未声明字段必须拒：静默丢掉会让作者以为存进去了。
	_, err = Update(ctx, gdb, 7, "mini", "a", r1.RecordKey,
		json.RawMessage(`{"no":"N-1","hidden":"x"}`), updated.Version)
	invalid, ok := err.(*InvalidError)
	if !ok || !hasPath(invalid.Problems, "data.hidden") {
		t.Fatalf("未声明字段应定位到 data.hidden，实际 %v", err)
	}
}

// TestRecordLimit 容量上限是"防跑飞"的软阀，必须给出带上限的文案。
func TestRecordLimit(t *testing.T) {
	gdb := withPluginDB(t)
	ctx := context.Background()
	importOK(t, gdb, miniLimited, 0, false)
	if _, err := Install(ctx, gdb, 7, "mini", map[string]any{}); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	createRecord(t, gdb, 7, "mini", "a", map[string]any{"no": "N-1"})
	createRecord(t, gdb, 7, "mini", "a", map[string]any{"no": "N-2"})
	_, err := Create(ctx, gdb, 7, "mini", "a", json.RawMessage(`{"no":"N-3"}`))
	limit, ok := err.(*LimitError)
	if !ok || limit.Max != 2 {
		t.Fatalf("超限应返回 LimitError{Max:2}，实际 %v", err)
	}
}

// TestScopeIsolatesApps 同名插件在两个应用下互不可见：这是"每应用独立安装"的根。
func TestScopeIsolatesApps(t *testing.T) {
	gdb := withPluginDB(t)
	ctx := context.Background()
	importOK(t, gdb, mini, 0, false)
	if _, err := Install(ctx, gdb, 7, "mini", map[string]any{}); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if _, err := Install(ctx, gdb, 8, "mini", map[string]any{}); err != nil {
		t.Fatalf("第二个应用安装失败: %v", err)
	}
	createRecord(t, gdb, 7, "mini", "a", map[string]any{"no": "A-1"})
	if _, total, err := ListRecords(ctx, gdb, 8, "mini", "a", "", 0, 10); err != nil || total != 0 {
		t.Fatalf("应用 8 看不到应用 7 的数据，实际 total=%d err=%v", total, err)
	}
}

// TestKeywordSearchCoversUniqueField 关键词只查两列（编号 / 业务唯一值），
// 且三处（列表、导出计数、导出分批）必须同一批行。
//
// 第 ④ 条守的是作用域：搜索条件是 `(a OR b)`，只要有人把它改成 `Or()` 链
// 或拼 `gorm.Expr`，OR 的后半句就会脱离 app_id —— 一次搜索读出别的应用的数据。
func TestKeywordSearchCoversUniqueField(t *testing.T) {
	gdb := withPluginDB(t)
	ctx := context.Background()
	importOK(t, gdb, mini, 0, false)
	for _, app := range []uint64{7, 8} {
		if _, err := Install(ctx, gdb, app, "mini", map[string]any{}); err != nil {
			t.Fatalf("应用 %d 安装失败: %v", app, err)
		}
	}
	first := createRecord(t, gdb, 7, "mini", "a", map[string]any{"no": "T-1001", "memo": "登录后空白"})
	createRecord(t, gdb, 7, "mini", "a", map[string]any{"no": "T-1002"})

	// ① 业务唯一值能搜到；大小写不敏感（unique_norm 已 trim+lower）
	items, total, err := ListRecords(ctx, gdb, 7, "mini", "a", " t-1001 ", 0, 10)
	if err != nil || total != 1 || len(items) != 1 || items[0].RecordKey != first.RecordKey {
		t.Fatalf("按唯一值搜索应命中 1 条，实际 total=%d items=%d err=%v", total, len(items), err)
	}

	// ② 对外编号（ULID）仍可搜
	if _, total, err := ListRecords(ctx, gdb, 7, "mini", "a", first.RecordKey[:12], 0, 10); err != nil || total != 1 {
		t.Fatalf("按编号搜索应命中 1 条，实际 total=%d err=%v", total, err)
	}

	// ③ 不查 JSONB 正文：备注里的词搜不到（否则同一句话命中一片，说不清理由）
	if _, total, err := ListRecords(ctx, gdb, 7, "mini", "a", "空白", 0, 10); err != nil || total != 0 {
		t.Fatalf("关键词不应命中正文，实际 total=%d err=%v", total, err)
	}

	// ④ 作用域：应用 8 搜应用 7 的单号，必须 0 条
	if _, total, err := ListRecords(ctx, gdb, 8, "mini", "a", "T-1001", 0, 10); err != nil || total != 0 {
		t.Fatalf("关键词越过了应用作用域，实际 total=%d err=%v", total, err)
	}

	// ⑤ 导出与列表同源：两处各写一遍 WHERE 就会分叉成"导出比列表多"
	_, n, err := ExportPlan(ctx, gdb, 7, "mini", "a", "t-1001")
	if err != nil || n != 1 {
		t.Fatalf("导出计数应为 1，实际 n=%d err=%v", n, err)
	}
	rows, err := ExportBatch(ctx, gdb, 7, "mini", "a", "t-1001", 0, 100)
	if err != nil || len(rows) != 1 {
		t.Fatalf("导出行应为 1，实际 %d err=%v", len(rows), err)
	}
}

// TestCatalogDeleteKeepsInstallRecoverable 目录删除后数据不能变成"看不见、也删不掉"。
//
// 删除只允许「所有安装都已卸载」，但安装行与记录都留着：清单仍能列出（名字退化成
// key），清除数据仍可用（Purge 不依赖目录）。这条锁的是"没有死路"这个结论。
func TestCatalogDeleteKeepsInstallRecoverable(t *testing.T) {
	gdb := withPluginDB(t)
	ctx := context.Background()
	importOK(t, gdb, mini, 0, false)
	if _, err := Install(ctx, gdb, 7, "mini", map[string]any{}); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	createRecord(t, gdb, 7, "mini", "a", map[string]any{"no": "N-1"})

	// 还在用（启用中）→ 拒绝删除，且要带引用数（只说"仍在使用"运营无法判断影响面）。
	err := DeleteCatalog(ctx, gdb, "mini")
	var inUse *InUseError
	if !errors.As(err, &inUse) || inUse.Count != 1 {
		t.Fatalf("启用中删目录应报引用数 1，实际 %v", err)
	}

	if err := Uninstall(ctx, gdb, 7, "mini"); err != nil {
		t.Fatalf("卸载失败: %v", err)
	}
	if err := DeleteCatalog(ctx, gdb, "mini"); err != nil {
		t.Fatalf("全部卸载后应允许删目录: %v", err)
	}

	// 安装行仍在：清单照常列出，名字退化成 key，记录数仍在。
	items, err := ListInstalls(ctx, gdb, 7)
	if err != nil || len(items) != 1 {
		t.Fatalf("目录删除后清单仍应有一行，实际 %d err=%v", len(items), err)
	}
	if items[0].Name != "mini" || items[0].RecordCount != 1 || items[0].Status != model.AppPluginInstallRemoved {
		t.Fatalf("清单行应退化成 key 且保留记录数，实际 %+v", items[0])
	}

	// 清除数据不依赖目录 —— 否则这些记录会成为永久残留。
	if n, err := Purge(ctx, gdb, 7, "mini"); err != nil || n != 1 {
		t.Fatalf("目录删除后仍应能清除数据，实际 n=%d err=%v", n, err)
	}
	if items, _ := ListInstalls(ctx, gdb, 7); len(items) != 1 || items[0].RecordCount != 0 {
		t.Fatalf("清除后记录数应归零，实际 %+v", items)
	}
}

// TestReenableAlignsVersion 漂移横幅的语义是「自上次启用以来目录变过」：
// 重新启用即视为重新确认过，版本快照必须对齐到当前目录，否则横幅永不消失。
func TestReenableAlignsVersion(t *testing.T) {
	gdb := withPluginDB(t)
	ctx := context.Background()
	importOK(t, gdb, mini, 0, false) // 1.0.0
	if _, err := Install(ctx, gdb, 7, "mini", map[string]any{}); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	// 目录升到 1.1.0（删字段属破坏性变更，必须 force）
	importOK(t, gdb, miniWithoutMemo, 1, true)

	d, err := GetDetail(ctx, gdb, 7, "mini")
	if err != nil {
		t.Fatalf("取描述符失败: %v", err)
	}
	if d.Drift == nil || d.Drift.Installed != "1.0.0" || d.Drift.Current != "1.1.0" {
		t.Fatalf("升级后应提示漂移 1.0.0→1.1.0，实际 %+v", d.Drift)
	}

	disabled := model.AppPluginInstallDisabled
	if _, err := Patch(ctx, gdb, 7, "mini", PatchInput{Status: &disabled, ExpectedVersion: 1}); err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	enabled := model.AppPluginInstallEnabled
	if _, err := Patch(ctx, gdb, 7, "mini", PatchInput{Status: &enabled, ExpectedVersion: 2}); err != nil {
		t.Fatalf("重新启用失败: %v", err)
	}
	d2, err := GetDetail(ctx, gdb, 7, "mini")
	if err != nil {
		t.Fatalf("取描述符失败: %v", err)
	}
	if d2.Drift != nil {
		t.Fatalf("重新启用后漂移应消失，实际 %+v", d2.Drift)
	}
	if d2.Install.PluginVersion != "1.1.0" {
		t.Fatalf("重新启用应对齐到当前目录版本，实际 %s", d2.Install.PluginVersion)
	}
}

// TestListCatalogPutsExactKeyFirst：`q` 是 ILIKE 子串匹配，命中很多行时若只按 id 排序，
// 精确命中的那行会被挤出第一页 —— 列表页搜自己的插件却看不见它；导入流程也靠这一行取
// `lock_version`（取不到就会把"升级"当成"新建"，服务端只能回版本冲突）。
func TestListCatalogPutsExactKeyFirst(t *testing.T) {
	gdb := withPluginDB(t)
	ctx := context.Background()

	// 目标行**先**建（id 最小），再建 30 个同样含 "ticket" 的新行：只按 id DESC 排序时
	// 它会垫底，第一页 5 条里根本没有它。
	keys := []string{"ticket"}
	for i := 0; i < 30; i++ {
		keys = append(keys, "ticket-"+strconv.Itoa(i))
	}
	for _, key := range keys {
		raw := strings.Replace(mini, `"key":"mini"`, `"key":"`+key+`"`, 1)
		if _, err := Import(ctx, gdb, ImportInput{Key: key, Raw: []byte(raw)}); err != nil {
			t.Fatalf("导入 %s 失败: %v", key, err)
		}
	}

	items, total, err := ListCatalog(ctx, gdb, "ticket", 0, 5)
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if total != int64(len(keys)) {
		t.Fatalf("total 应含全部子串命中 %d，实际 %d", len(keys), total)
	}
	if len(items) == 0 || items[0].Key != "ticket" {
		t.Fatalf("精确命中的 key 必须排第一，实际首行 %q", items[0].Key)
	}
}

// TestExportPlan 导出前置判据：没声明 export 的实体拒绝，超上限拒绝。
func TestExportPlan(t *testing.T) {
	gdb := withPluginDB(t)
	ctx := context.Background()
	importOK(t, gdb, mini, 0, false)
	if _, _, err := ExportPlan(ctx, gdb, 7, "mini", "a", ""); err == nil {
		t.Fatal("未安装时应拒绝导出")
	}
	if _, err := Install(ctx, gdb, 7, "mini", map[string]any{}); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	e, n, err := ExportPlan(ctx, gdb, 7, "mini", "a", "")
	if err != nil || n != 0 || e.Key != "a" {
		t.Fatalf("导出计划不对: e=%+v n=%d err=%v", e, n, err)
	}
	if _, _, err := ExportPlan(ctx, gdb, 7, "mini", "nope", ""); err != ErrEntityNotFound {
		t.Fatalf("未知对象应 404，实际 %v", err)
	}
}

// TestCellText 导出文本口径：bool 写"是/否"（读者是运营），空值写空串。
func TestCellText(t *testing.T) {
	if got := CellText(&Field{Type: TypeBool}, true); got != "是" {
		t.Fatalf("bool 应输出是，实际 %q", got)
	}
	if got := CellText(&Field{Type: TypeString}, nil); got != "" {
		t.Fatalf("空值应输出空串，实际 %q", got)
	}
	if got := CellText(&Field{Type: TypeInt}, json.Number("12")); got != "12" {
		t.Fatalf("数字应原样输出，实际 %q", got)
	}
}

func createRecord(t *testing.T, gdb *gorm.DB, appID uint64, key, entity string, data map[string]any) *RecordItem {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("序列化数据失败: %v", err)
	}
	item, err := Create(context.Background(), gdb, appID, key, entity, raw)
	if err != nil {
		t.Fatalf("建记录失败: %v", err)
	}
	return item
}

// TestPublicListGates 免凭证读面的五道闸 + 字段投影。
//
// 每一条写错的后果都不是报错，而是"数据静默出去了"或"开关点了没反应"：
//   - 默认关着却能被读 → 装完就对外；
//   - 白名单外的列被带出 → 备注里的内部信息跟着标题一起公开；
//   - 停用/卸载/下架后仍能读 → 管理台的按钮形同虚设。
func TestPublicListGates(t *testing.T) {
	gdb := withPluginDB(t)
	ctx := context.Background()
	importOK(t, gdb, miniPublic, 0, false)
	if _, err := Install(ctx, gdb, 7, "mini", map[string]any{}); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	createRecord(t, gdb, 7, "mini", "a", map[string]any{"no": "N-1", "memo": "内部备注"})
	// 每次状态变化都推进版本（含卸载），硬编码版本号会让这条用例变成"数数"。
	// 从库里读当前版本：Case 要验的是闸门，不是版本算术。
	lockOf := func() int {
		inst, err := getInstall(ctx, gdb, 7, "mini")
		if err != nil {
			t.Fatalf("取安装行失败: %v", err)
		}
		return inst.LockVersion
	}

	// ① 默认关闭：装完不等于对外。
	if _, err := PublicList(ctx, gdb, 7, "mini", "a", 1, 10); err != ErrNotFound {
		t.Fatalf("未打开对外只读时应 404，实际 %v", err)
	}

	yes := true
	if _, err := Patch(ctx, gdb, 7, "mini", PatchInput{PublicEnabled: &yes, ExpectedVersion: lockOf()}); err != nil {
		t.Fatalf("打开对外只读失败: %v", err)
	}
	page, err := PublicList(ctx, gdb, 7, "mini", "a", 1, 10)
	if err != nil {
		t.Fatalf("打开后应可读: %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Data["no"] != "N-1" {
		t.Fatalf("返回内容不对: %+v", page)
	}
	// ② 只回白名单：memo 没声明公开，绝不能出现。
	if _, ok := page.Items[0].Data["memo"]; ok {
		t.Fatalf("未声明公开的字段被下发: %+v", page.Items[0].Data)
	}
	if page.Plugin.Key != "mini" || page.Entity.Key != "a" {
		t.Fatalf("自述信息不对: %+v", page)
	}

	// ③ 对象没声明 public：即使开关开着也不对外。
	if _, err := PublicList(ctx, gdb, 7, "mini", "b", 1, 10); err != ErrEntityNotFound {
		t.Fatalf("对象未声明公开字段时应 404，实际 %v", err)
	}

	// ④ 停用（冻结）→ 404。
	disabled := model.AppPluginInstallDisabled
	if _, err := Patch(ctx, gdb, 7, "mini", PatchInput{Status: &disabled, ExpectedVersion: lockOf()}); err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	if _, err := PublicList(ctx, gdb, 7, "mini", "a", 1, 10); err != ErrNotFound {
		t.Fatalf("停用后不应对外，实际 %v", err)
	}
	enabled := model.AppPluginInstallEnabled
	if _, err := Patch(ctx, gdb, 7, "mini", PatchInput{Status: &enabled, ExpectedVersion: lockOf()}); err != nil {
		t.Fatalf("重新启用失败: %v", err)
	}

	// ⑤ 目录下架 → 404（下架不只是管理台的一行字）。
	if err := SetStatus(ctx, gdb, "mini", model.AppPluginStatusDisabled); err != nil {
		t.Fatalf("下架失败: %v", err)
	}
	if _, err := PublicList(ctx, gdb, 7, "mini", "a", 1, 10); err != ErrNotFound {
		t.Fatalf("目录下架后不应对外，实际 %v", err)
	}
	if err := SetStatus(ctx, gdb, "mini", model.AppPluginStatusEnabled); err != nil {
		t.Fatalf("重新上架失败: %v", err)
	}

	// ⑥ 卸载 → 404。
	if err := Uninstall(ctx, gdb, 7, "mini"); err != nil {
		t.Fatalf("卸载失败: %v", err)
	}
	if _, err := PublicList(ctx, gdb, 7, "mini", "a", 1, 10); err != ErrNotFound {
		t.Fatalf("卸载后不应对外，实际 %v", err)
	}

	// ⑦ 分页越界收敛（不报错，也不能真的取 500 条）。
	if _, err := Patch(ctx, gdb, 7, "mini", PatchInput{Status: &enabled, ExpectedVersion: lockOf()}); err != nil {
		t.Fatalf("重新启用失败: %v", err)
	}
	p2, err := PublicList(ctx, gdb, 7, "mini", "a", 0, 500)
	if err != nil {
		t.Fatalf("越界分页应被收敛: %v", err)
	}
	if p2.Page != 1 || p2.PageSize != PublicMaxPageSize {
		t.Fatalf("分页应收敛到 1/%d，实际 %d/%d", PublicMaxPageSize, p2.Page, p2.PageSize)
	}
}
