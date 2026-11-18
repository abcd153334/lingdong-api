package relay

import (
	"os"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

// 本文件是 SelectCandidates 的 characterization 测试（B1 前置）：
// 先把「当前选路行为」逐条钉住，再动 relay 的错误分类/健康窗口，避免改出新问题。

const selectTestSchema = "ld_test_relay_select"

func withSelectDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("LD_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 LD_TEST_DSN，跳过选路集成用例")
	}
	silent := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), silent)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + selectTestSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+selectTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.Model{}, &model.ModelBinding{}, &model.Channel{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + selectTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

// mkChannel 造一个渠道（默认启用、default 分组）。
func mkChannel(t *testing.T, gdb *gorm.DB, name string, priority, weight int, status int16, groups ...string) *model.Channel {
	t.Helper()
	if len(groups) == 0 {
		groups = []string{"default"}
	}
	ch := model.Channel{
		Name: name, AdapterID: "builtin:openai", BaseURL: "https://" + strings.ReplaceAll(name, " ", "-") + ".example.com",
		Keys: model.MustJSON([]string{"enc"}), MultiKeyMode: "random",
		Groups: model.MustJSON(groups), Weight: weight, Priority: priority, Status: status,
	}
	if err := gdb.Create(&ch).Error; err != nil {
		t.Fatalf("创建渠道 %s 失败：%v", name, err)
	}
	return &ch
}

// mkBinding 造一条模型-渠道绑定。
func mkBinding(t *testing.T, gdb *gorm.DB, modelID string, channelID uint64, weight int, enabled bool, upstream string) {
	t.Helper()
	// 用 map 写入：enabled 列 default:true，结构体 Create 会丢 false 零值（Select("*") 也不生效）。
	row := map[string]any{
		"model_id": modelID, "channel_id": channelID, "weight": weight,
		"priority": 1, "enabled": enabled, "upstream_model": upstream,
	}
	if err := gdb.Model(&model.ModelBinding{}).Create(row).Error; err != nil {
		t.Fatalf("创建绑定失败：%v", err)
	}
}

// TestSelectCandidatesFiltersAndOrdering 锁定过滤条件与优先级排序。
func TestSelectCandidatesFiltersAndOrdering(t *testing.T) {
	gdb := withSelectDB(t)

	fast := mkChannel(t, gdb, "fast", 1, 5, 1)
	slow := mkChannel(t, gdb, "slow", 2, 0, 1)
	disabled := mkChannel(t, gdb, "disabled", 1, 5, 2)
	otherGroup := mkChannel(t, gdb, "other-group", 1, 5, 1, "vip")

	mkBinding(t, gdb, "gpt-4o", fast.ID, 5, true, "")
	mkBinding(t, gdb, "gpt-4o", slow.ID, 0, true, "gpt-4o-2024")
	mkBinding(t, gdb, "gpt-4o", disabled.ID, 5, true, "")
	mkBinding(t, gdb, "gpt-4o", otherGroup.ID, 5, true, "")
	// 绑定停用：即使渠道可用也不得入选
	paused := mkChannel(t, gdb, "paused-binding", 1, 5, 1)
	mkBinding(t, gdb, "gpt-4o", paused.ID, 5, false, "")

	got, err := SelectCandidates("gpt-4o", "default")
	if err != nil {
		t.Fatalf("选路失败：%v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应只剩 2 个候选（停用渠道/分组不符/绑定停用均排除），实际 %d：%+v", len(got), names(got))
	}
	// 优先级升序：priority=1 的 fast 排在 priority=2 的 slow 之前
	if got[0].Channel.Name != "fast" || got[1].Channel.Name != "slow" {
		t.Fatalf("优先级排序不符：%v", names(got))
	}
	// 上游模型名：空 → 回落模型 ID；非空 → 用绑定值
	if got[0].UpstreamModel != "gpt-4o" {
		t.Errorf("空 upstream 应回落模型 ID，实际 %q", got[0].UpstreamModel)
	}
	if got[1].UpstreamModel != "gpt-4o-2024" {
		t.Errorf("应使用绑定的上游模型名，实际 %q", got[1].UpstreamModel)
	}
}

// TestSelectCandidatesNoBinding 锁定「无绑定返回 nil,nil（不报错）」的既有契约：
// 调用方据此在冻结前拦截，返回 error 会变成 500。
func TestSelectCandidatesNoBinding(t *testing.T) {
	withSelectDB(t)
	got, err := SelectCandidates("no-such-model", "default")
	if err != nil {
		t.Fatalf("无绑定不应返回错误：%v", err)
	}
	if got != nil {
		t.Fatalf("无绑定应返回 nil，实际 %+v", names(got))
	}
}

// TestSelectCandidatesExcludesSoftDeletedChannel 锁定软删渠道不参与选路。
func TestSelectCandidatesExcludesSoftDeletedChannel(t *testing.T) {
	gdb := withSelectDB(t)
	ch := mkChannel(t, gdb, "gone", 1, 1, 1)
	mkBinding(t, gdb, "m1", ch.ID, 1, true, "")
	if err := gdb.Delete(&model.Channel{}, ch.ID).Error; err != nil {
		t.Fatalf("软删失败：%v", err)
	}
	got, err := SelectCandidates("m1", "default")
	if err != nil {
		t.Fatalf("选路失败：%v", err)
	}
	if len(got) != 0 {
		t.Fatalf("软删渠道不应入选，实际 %v", names(got))
	}
}

// TestSelectCandidatesWeightedKeepsAll 锁定「同优先级层内按权重随机但不会饿死低权重成员」：
// 有效权重 = weight + 10，因此 weight=0 的成员依然可被选中。
func TestSelectCandidatesWeightedKeepsAll(t *testing.T) {
	gdb := withSelectDB(t)
	heavy := mkChannel(t, gdb, "heavy", 1, 20, 1)
	light := mkChannel(t, gdb, "light", 1, 0, 1)
	mkBinding(t, gdb, "m2", heavy.ID, 20, true, "")
	mkBinding(t, gdb, "m2", light.ID, 0, true, "")

	hits := map[string]int{}
	for i := 0; i < 200; i++ {
		got, err := SelectCandidates("m2", "default")
		if err != nil {
			t.Fatalf("第 %d 次选路失败：%v", i, err)
		}
		if len(got) == 0 {
			t.Fatalf("第 %d 次选路返回空", i)
		}
		hits[got[0].Channel.Name]++
	}
	if hits["light"] == 0 {
		t.Fatalf("低权重成员被饿死（200 次全中 heavy）：%+v", hits)
	}
	if hits["heavy"] == 0 {
		t.Fatalf("高权重成员从未被选中：%+v", hits)
	}
}

// names 提取候选渠道名（断言失败时便于阅读）。
func names(cs []Candidate) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		if c.Channel != nil {
			out = append(out, c.Channel.Name)
		}
	}
	return out
}
