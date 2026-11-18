package worker

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"lingdong-api/internal/conf"
)

// TestCollectCapacityWarnOnLowFree 剩余空间低于绝对下限时必须告警。
//
// 用「min_free_gb 极大」制造告警，而不是靠真实磁盘快满：后者在 CI 上不可重现。
func TestCollectCapacityWarnOnLowFree(t *testing.T) {
	cfg := conf.Env{StorageDir: t.TempDir(), CapacityWarnPercent: 0, CapacityWarnMinFreeGB: 1 << 20}
	snap := collectCapacityWith(context.Background(), time.Now(), cfg)

	if snap.Error != "" {
		t.Fatalf("磁盘读取不应失败: %s", snap.Error)
	}
	if snap.DiskTotal == 0 {
		t.Fatal("总容量为 0，disk.Usage 未生效")
	}
	if snap.FreeRatio <= 0 || snap.FreeRatio > 1 {
		t.Fatalf("可用占比越界: %v", snap.FreeRatio)
	}
	if !snap.Warn || snap.Warning == "" {
		t.Fatalf("应触发告警: %+v", snap)
	}
}

// TestCollectCapacityNoWarn 阈值放宽时不得误报（否则告警会被无视）。
func TestCollectCapacityNoWarn(t *testing.T) {
	cfg := conf.Env{StorageDir: t.TempDir(), CapacityWarnPercent: 100, CapacityWarnMinFreeGB: 0}
	snap := collectCapacityWith(context.Background(), time.Now(), cfg)
	if snap.Warn {
		t.Fatalf("阈值内不应告警: %+v", snap)
	}
}

// TestCollectCapacityFallsBackWhenDirMissing STORAGE_DIR 不存在时回落到可读路径。
//
// 真实场景：storage.active=s3 时本地目录根本不会创建，若不做回落，
// 容量监控会永远读不到磁盘 → 每晚一条 Error 日志 → 最后没人再看这个告警。
func TestCollectCapacityFallsBackWhenDirMissing(t *testing.T) {
	cfg := conf.Env{
		StorageDir:          filepath.Join(os.TempDir(), "ld-no-such-dir-9f3a2b"),
		CapacityWarnPercent: 100,
	}
	snap := collectCapacityWith(context.Background(), time.Now(), cfg)
	if snap.Error != "" {
		t.Fatalf("目录不存在时应回落到可读路径，实际报错: %s", snap.Error)
	}
	if snap.DiskTotal == 0 {
		t.Fatal("回落路径也没有读到磁盘容量")
	}
}

// TestCapacitySnapshotDefaults 尚未体检过时返回「不可用」而不是零值误导。
func TestCapacitySnapshotDefaults(t *testing.T) {
	// 直接换一个空的 atomic.Value（与 capacityLatest 同类型，赋值安全）。
	capacityLatest = atomic.Value{}
	snap := Capacity()
	if !snap.At.IsZero() || snap.FreeRatio != -1 {
		t.Fatalf("初始快照语义不符: %+v", snap)
	}
}
