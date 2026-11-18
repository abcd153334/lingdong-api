package task

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

// TestPurgeArtifactsDeletesExpiredOnly 覆盖产物到期清理的四种结局。
//
// 这条路径此前**没有任何测试**（PurgeArtifacts 零覆盖），而它做的是物理删除：
// 一旦把「未过期」「后端不可用」「位置缺失」中的任何一类误判为可删，
// 用户的产物会连同对象一起消失，且任务页只会显示「0 个产物」。
func TestPurgeArtifactsDeletesExpiredOnly(t *testing.T) {
	gdb := withTestDB(t)
	if err := gdb.AutoMigrate(&model.File{}); err != nil {
		t.Fatalf("迁移 files 表失败: %v", err)
	}
	dir := t.TempDir()
	t.Setenv("STORAGE_DIR", dir)
	conf.Load()
	ctx := context.Background()
	now := time.Now()

	putObject := func(key string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(key))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("写对象失败: %v", err)
		}
	}

	expired := model.File{
		TaskID: 1, ArtifactIndex: 0, UserID: 1, Filename: "old.png", SizeBytes: 1,
		StorageBackend: "none", StorageKey: "files/1/t1/old.png",
		ExpiresAt: ptrTime(now.Add(-time.Hour)),
	}
	future := model.File{
		TaskID: 1, ArtifactIndex: 1, UserID: 1, Filename: "new.png", SizeBytes: 1,
		StorageBackend: "none", StorageKey: "files/1/t1/new.png",
		ExpiresAt: ptrTime(now.Add(time.Hour)),
	}
	noKey := model.File{
		TaskID: 2, ArtifactIndex: 0, UserID: 1, Filename: "nokey.png", SizeBytes: 1,
		StorageBackend: "none", StorageKey: "",
		ExpiresAt: ptrTime(now.Add(-time.Hour)),
	}
	deadBackend := model.File{
		TaskID: 3, ArtifactIndex: 0, UserID: 1, Filename: "lost.png", SizeBytes: 1,
		StorageBackend: "oss", StorageBucket: "b", StorageKey: "artifacts/1/3/0",
		ExpiresAt: ptrTime(now.Add(-time.Hour)),
	}
	for _, row := range []*model.File{&expired, &future, &noKey, &deadBackend} {
		if err := gdb.Create(row).Error; err != nil {
			t.Fatalf("造产物行失败: %v", err)
		}
	}
	putObject(expired.StorageKey)
	putObject(future.StorageKey)

	st, err := PurgeArtifacts(ctx, 100)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	// 可删的两条：过期 + 空 key（无对象可删）。未过期与后端不可用的必须保留。
	if st.Deleted != 2 {
		t.Fatalf("应删除 2 条（过期 + 空 key），实际 %+v", st)
	}
	if st.Blocked["backend"] == 0 {
		t.Fatalf("后端不可用的到期行必须计入 blocked（不能静默），实际 %+v", st)
	}
	if cnt := countFiles(t, gdb, future.ID); cnt != 1 {
		t.Fatal("未过期产物不得删除")
	}
	if cnt := countFiles(t, gdb, deadBackend.ID); cnt != 1 {
		t.Fatal("后端不可用的到期产物必须保留（对象还在桶里，删行等于制造孤儿）")
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(expired.StorageKey))); !os.IsNotExist(err) {
		t.Fatalf("过期对象应被真正删除: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(future.StorageKey))); err != nil {
		t.Fatalf("未过期对象不应被动: %v", err)
	}

	// 再跑一轮：没有新的可删项，且 blocked 仍可见（不是「一切正常」）。
	st2, err := PurgeArtifacts(ctx, 100)
	if err != nil {
		t.Fatalf("第二轮清理失败: %v", err)
	}
	if st2.Deleted != 0 {
		t.Fatalf("第二轮不应再删任何东西: %+v", st2)
	}
	if st2.Blocked["backend"] != 1 {
		t.Fatalf("blocked 应恒为可见的 1 条: %+v", st2)
	}
}

func countFiles(t *testing.T, _ *gorm.DB, id uint64) int64 {
	t.Helper()
	var n int64
	if err := db.DB.Model(&model.File{}).Where("id = ?", id).Count(&n).Error; err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	return n
}

func ptrTime(t time.Time) *time.Time { return &t }
