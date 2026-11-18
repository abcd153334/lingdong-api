package releaseupdate

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/model"
)

const testSchema = "ld_test_release_update"

// indexDDL 必须与 db/indexes.go 里 uq_release_update_* 的定义逐字一致。
// 集成用例要验的正是"两条 active 插不进去"，没有这条索引等于没测。
const indexDDL = `
CREATE UNIQUE INDEX IF NOT EXISTS uq_release_update_active
  ON release_update_jobs ((true))
  WHERE status IN ('queued','fetching','building','backing_up','switching','verifying','rolling_back');
CREATE UNIQUE INDEX IF NOT EXISTS uq_release_update_request
  ON release_update_jobs (request_id);
`

func withDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("LD_TEST_DSN")
	if dsn == "" {
		if os.Getenv("LD_REQUIRE_TEST_DSN") == "1" {
			t.Fatal("LD_REQUIRE_TEST_DSN=1 但未设置 LD_TEST_DSN：集成用例不允许被静默跳过")
		}
		t.Skip("未设置 LD_TEST_DSN，跳过数据库集成用例")
	}
	silent := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), silent)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := admin.Exec("DROP SCHEMA IF EXISTS " + testSchema + " CASCADE").Error; err != nil {
		t.Fatalf("清理测试 schema 失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA " + testSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+testSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.ReleaseUpdateJob{}, &model.ReleaseAgentState{},
		&model.AdminAudit{}, &model.Task{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	if err := gdb.Exec(indexDDL).Error; err != nil {
		t.Fatalf("建索引失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + testSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return gdb
}

func in(target string) SubmitInput {
	return SubmitInput{
		RequestID:   "req-" + target + "-0001",
		TargetTag:   target,
		FromVersion: "v2.1.0",
		AdminID:     7,
	}
}

// TestSubmitRejectsSecondActive 锁死"同时只能有一个进行中的更新"。
//
// 靠的是部分唯一索引，不是"先查再插"——后者在并发下会两条都通过，
// 然后两个 agent 抢同一台机器。
func TestSubmitRejectsSecondActive(t *testing.T) {
	gdb := withDB(t)
	svc := New(gdb)
	ctx := context.Background()

	first, err := svc.Submit(ctx, in("v2.2.0"))
	if err != nil {
		t.Fatalf("第一次提交应成功：%v", err)
	}
	if first.Status != StatusQueued {
		t.Fatalf("新任务应为 queued，实际 %q", first.Status)
	}

	second := in("v2.3.0")
	second.RequestID = "req-v2.3.0-0001"
	if _, err := svc.Submit(ctx, second); !errors.Is(err, ErrInProgress) {
		t.Fatalf("第二条 active 必须被拒（ErrInProgress），实际 %v", err)
	}
}

// TestSubmitIdempotentByRequestID 锁死"重试不会产生第二个任务"。
func TestSubmitIdempotentByRequestID(t *testing.T) {
	gdb := withDB(t)
	svc := New(gdb)
	ctx := context.Background()

	a, err := svc.Submit(ctx, in("v2.2.0"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.Submit(ctx, in("v2.2.0"))
	if err != nil {
		t.Fatalf("同 request_id 重试不应报错：%v", err)
	}
	if a.ID != b.ID {
		t.Fatalf("同 request_id 应返回同一行（%d vs %d）", a.ID, b.ID)
	}
}

// TestStaleQueuedBecomesFailedAndUnblocks 锁死 P0：queued 不能永久占位。
//
// 真实故障：agent 没装/没起，行停在 queued，部分唯一索引让之后每一次提交都是 409，
// 只能手工改库才能恢复。
func TestStaleQueuedBecomesFailedAndUnblocks(t *testing.T) {
	gdb := withDB(t)
	svc := New(gdb)
	ctx := context.Background()

	job, err := svc.Submit(ctx, in("v2.2.0"))
	if err != nil {
		t.Fatal(err)
	}
	// 把 requested_at 推到 2 分钟前（模拟"提交后没有任何 agent 来领"）。
	if err := gdb.Model(&model.ReleaseUpdateJob{}).Where("id = ?", job.ID).
		Update("requested_at", time.Now().Add(-2*time.Minute)).Error; err != nil {
		t.Fatal(err)
	}

	active, last, err := svc.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if active != nil {
		t.Fatalf("超时的 queued 不应再算 active：%+v", active)
	}
	if last == nil || last.Status != StatusFailed || last.FailureCode != "agent_offline" {
		t.Fatalf("应落 failed(agent_offline)，实际 %+v", last)
	}

	// 关键：不再被唯一索引挡死。
	next := in("v2.3.0")
	next.RequestID = "req-v2.3.0-0001"
	if _, err := svc.Submit(ctx, next); err != nil {
		t.Fatalf("上一笔超时后应能再次提交：%v", err)
	}
}

// TestSweepBuildHeartbeatTimeout 锁死"构建期间心跳停了就判失联"。
func TestSweepBuildHeartbeatTimeout(t *testing.T) {
	gdb := withDB(t)
	svc := New(gdb)
	ctx := context.Background()

	job, err := svc.Submit(ctx, in("v2.2.0"))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-5 * time.Minute)
	if err := gdb.Model(&model.ReleaseUpdateJob{}).Where("id = ?", job.ID).Updates(map[string]any{
		"status": StatusBuilding, "started_at": &old, "last_heartbeat_at": &old,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.Get(ctx, job.ID)
	if got.Status != StatusFailed || got.FailureCode != "agent_lost" {
		t.Fatalf("构建期心跳超时应落 failed(agent_lost)，实际 %q/%q", got.Status, got.FailureCode)
	}
}

// TestSweepSwitchInterruptedNeedsManual 锁死"切换中断不能自动重试"。
//
// 此时容器可能停在"旧已停、新未起"：允许人再点一次，等于让第二个 agent
// 对着一个不确定的状态再切一遍。
func TestSweepSwitchInterruptedNeedsManual(t *testing.T) {
	gdb := withDB(t)
	svc := New(gdb)
	ctx := context.Background()

	job, err := svc.Submit(ctx, in("v2.2.0"))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * time.Minute)
	if err := gdb.Model(&model.ReleaseUpdateJob{}).Where("id = ?", job.ID).Updates(map[string]any{
		"status": StatusSwitching, "started_at": &old, "last_heartbeat_at": &old,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.Get(ctx, job.ID)
	if got.Status != StatusManualNeeded || got.FailureCode != "interrupted_during_switch" {
		t.Fatalf("切换中断应转人工，实际 %q/%q", got.Status, got.FailureCode)
	}
}

// TestCancelOnlyQueued 锁死"执行中不能取消"。
func TestCancelOnlyQueued(t *testing.T) {
	gdb := withDB(t)
	svc := New(gdb)
	ctx := context.Background()

	job, err := svc.Submit(ctx, in("v2.2.0"))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Cancel(ctx, job.ID); err != nil {
		t.Fatalf("queued 应可取消：%v", err)
	}
	if err := svc.Cancel(ctx, job.ID); !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("已取消的任务再次取消应返回不可取消，实际 %v", err)
	}

	// 进入执行之后再取消：必须拒绝。
	j2 := in("v2.3.0")
	j2.RequestID = "req-v2.3.0-0001"
	job2, err := svc.Submit(ctx, j2)
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.Model(&model.ReleaseUpdateJob{}).Where("id = ?", job2.ID).
		Update("status", StatusBuilding).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.Cancel(ctx, job2.ID); !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("building 状态必须拒绝取消，实际 %v", err)
	}
}

// TestRecentFailureWindow 锁死"同一版本刚失败要显式确认"。
func TestRecentFailureWindow(t *testing.T) {
	gdb := withDB(t)
	svc := New(gdb)
	ctx := context.Background()

	job, err := svc.Submit(ctx, in("v2.2.0"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := gdb.Model(&model.ReleaseUpdateJob{}).Where("id = ?", job.ID).Updates(map[string]any{
		"status": StatusFailed, "failure_code": "build_failed", "finished_at": &now,
	}).Error; err != nil {
		t.Fatal(err)
	}

	retry := in("v2.2.0")
	retry.RequestID = "req-v2.2.0-0002"
	if _, err := svc.Submit(ctx, retry); !errors.Is(err, ErrFailedRecently) {
		t.Fatalf("10 分钟内同版本重试应要求 force，实际 %v", err)
	}
	retry.Force = true
	if _, err := svc.Submit(ctx, retry); err != nil {
		t.Fatalf("force 应放行：%v", err)
	}
}

// TestAuditTerminalOnce 锁死"终态审计只写一次"（读路径会反复调用）。
func TestAuditTerminalOnce(t *testing.T) {
	gdb := withDB(t)
	svc := New(gdb)
	ctx := context.Background()

	job, err := svc.Submit(ctx, in("v2.2.0"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := gdb.Model(&model.ReleaseUpdateJob{}).Where("id = ?", job.ID).Updates(map[string]any{
		"status": StatusSuccess, "finished_at": &now,
	}).Error; err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if _, _, err := svc.Current(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var n int64
	if err := gdb.Model(&model.AdminAudit{}).
		Where("action = ?", "release.update.result").Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("结果审计应只写一行，实际 %d 行（读路径不能重复审计）", n)
	}
}
