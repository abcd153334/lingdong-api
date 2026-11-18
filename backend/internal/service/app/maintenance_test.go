package app

// 本文件守的是维护模式闸的**失败策略**。它不是普通缓存：判断在 TokenAuth 里，
// 是每个 /v1 请求的必经之路，写错的代价是全应用断流或维护失效。

import (
	"context"
	"errors"
	"testing"
)

func TestMaintenanceOfUsesCache(t *testing.T) {
	ResetMaintenanceForTest()
	old := loadMaintenance
	t.Cleanup(func() { loadMaintenance = old; ResetMaintenanceForTest() })

	calls := 0
	loadMaintenance = func(context.Context) (map[uint64]string, error) {
		calls++
		return map[uint64]string{7: "升级中，预计 03:00 恢复"}, nil
	}
	ctx := context.Background()

	if msg, blocked := MaintenanceOf(ctx, 7); !blocked || msg != "升级中，预计 03:00 恢复" {
		t.Fatalf("维护中的应用应被拦下并带回文案，实际 blocked=%v msg=%q", blocked, msg)
	}
	if _, blocked := MaintenanceOf(ctx, 8); blocked {
		t.Fatal("未开启维护的应用不应被拦")
	}
	if calls != 1 {
		t.Fatalf("10 秒内应只加载一次，实际 %d 次", calls)
	}

	// 平台直连（appID=0）短路：不应触发加载。
	if _, blocked := MaintenanceOf(ctx, 0); blocked {
		t.Fatal("appID=0 不属于任何应用，不应被拦")
	}
	if calls != 1 {
		t.Fatalf("appID=0 不该读缓存/查库，实际加载 %d 次", calls)
	}
}

func TestMaintenanceOfReloadsAfterTTL(t *testing.T) {
	ResetMaintenanceForTest()
	old := loadMaintenance
	t.Cleanup(func() { loadMaintenance = old; ResetMaintenanceForTest() })

	calls := 0
	loadMaintenance = func(context.Context) (map[uint64]string, error) {
		calls++
		return map[uint64]string{}, nil
	}
	ctx := context.Background()
	MaintenanceOf(ctx, 1)
	if calls != 1 {
		t.Fatalf("首次应加载一次，实际 %d", calls)
	}
	// 把快照做旧，模拟 TTL 到期。
	maintSnap.ExpireForTest()
	MaintenanceOf(ctx, 1)
	if calls != 2 {
		t.Fatalf("TTL 到期后应重新加载，实际 %d", calls)
	}
	// Invalidate 立即生效：管理端刚打开维护，本实例必须马上拦住。
	InvalidateMaintenance()
	MaintenanceOf(ctx, 1)
	if calls != 3 {
		t.Fatalf("Invalidate 后应重新加载，实际 %d", calls)
	}
}

func TestMaintenanceOfKeepsLastSnapshotOnError(t *testing.T) {
	ResetMaintenanceForTest()
	old := loadMaintenance
	t.Cleanup(func() { loadMaintenance = old; ResetMaintenanceForTest() })

	loadMaintenance = func(context.Context) (map[uint64]string, error) {
		return map[uint64]string{7: "维护中"}, nil
	}
	ctx := context.Background()
	if _, blocked := MaintenanceOf(ctx, 7); !blocked {
		t.Fatal("前置条件失败：应先建立快照")
	}

	// 加载失败但有旧快照：沿用旧值（宁可多拦 10 秒，也不能把维护中的应用放开）。
	loadMaintenance = func(context.Context) (map[uint64]string, error) {
		return nil, errors.New("db down")
	}
	InvalidateMaintenance()
	if msg, blocked := MaintenanceOf(ctx, 7); !blocked || msg != "维护中" {
		t.Fatalf("加载失败应沿用旧快照，实际 blocked=%v msg=%q", blocked, msg)
	}
}

func TestMaintenanceOfFailsOpenWithoutSnapshot(t *testing.T) {
	ResetMaintenanceForTest()
	old := loadMaintenance
	t.Cleanup(func() { loadMaintenance = old; ResetMaintenanceForTest() })

	loadMaintenance = func(context.Context) (map[uint64]string, error) {
		return nil, errors.New("db down")
	}
	// 无任何快照时失败：放行。把 DB 抖动放大成"全应用 503"远比
	// "维护晚 10 秒生效"严重。
	if _, blocked := MaintenanceOf(context.Background(), 7); blocked {
		t.Fatal("无快照且加载失败时应放行")
	}
}
