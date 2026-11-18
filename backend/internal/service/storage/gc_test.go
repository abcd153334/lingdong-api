package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lingdong-api/internal/conf"
)

// TestRunStorageGCOnlyDeletesConfirmedOrphans 覆盖孤儿对账的四道闸。
//
// 这是唯一会「自主删对象」的路径，所以每道闸都必须有反例：
// 形状不符不删、太新不删、库里有行不删、dry-run 不删。
func TestRunStorageGCOnlyDeletesConfirmedOrphans(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STORAGE_DIR", dir)
	conf.Load()

	orphan := "files/7/01J8ZQ7W6Y5X4V3T2S1R0Q9P8N/a.png"
	young := "files/7/01J8ZQ7W6Y5X4V3T2S1R0Q9P8M/b.png"
	badShape := "files/7/short/c.png"
	kept := "artifacts/1/2/0"
	old := time.Now().Add(-72 * time.Hour)

	for _, key := range []string{orphan, young, badShape, kept} {
		p := filepath.Join(dir, filepath.FromSlash(key))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("写对象失败: %v", err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatalf("改时间失败: %v", err)
		}
	}
	// young 保持「刚写入」的时间。
	fresh := filepath.Join(dir, filepath.FromSlash(young))
	now := time.Now()
	if err := os.Chtimes(fresh, now, now); err != nil {
		t.Fatalf("改时间失败: %v", err)
	}

	exists := func(_ context.Context, _ string, keys []string) (map[string]bool, error) {
		out := map[string]bool{}
		for _, k := range keys {
			if k == kept {
				out[k] = true
			}
		}
		return out, nil
	}
	opts := GCOptions{
		Buckets:  []Ref{{Backend: "none"}},
		Prefixes: []string{"files/", "artifacts/", "icons/"},
		MinAge:   48 * time.Hour,
		Limit:    100,
		DryRun:   true,
		Exists:   exists,
	}

	rep, err := RunStorageGC(context.Background(), opts)
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	br := rep.ByBucket["none/"]
	if br.Orphans != 1 {
		t.Fatalf("应只发现 1 个孤儿（files 里那条），实际 %+v", br)
	}
	if br.SkippedShape != 1 || br.SkippedYoung != 1 {
		t.Fatalf("形状不符与太新的对象必须被跳过并计数，实际 %+v", br)
	}
	if br.Deleted != 0 {
		t.Fatalf("dry-run 不允许删除，实际 %+v", br)
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(orphan))); err != nil {
		t.Fatalf("dry-run 后对象应仍在: %v", err)
	}

	// enforce：只删确认过的孤儿。
	opts.DryRun = false
	rep2, err := RunStorageGC(context.Background(), opts)
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	br2 := rep2.ByBucket["none/"]
	if br2.Deleted != 1 || br2.Failed != 0 {
		t.Fatalf("应删除 1 个孤儿，实际 %+v", br2)
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(orphan))); !os.IsNotExist(err) {
		t.Fatalf("孤儿对象应被删除: %v", err)
	}
	for _, key := range []string{young, badShape, kept} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(key))); err != nil {
			t.Fatalf("不该动的对象被删了: %s (%v)", key, err)
		}
	}
}

// TestGCShapeOK 把三类 key 的形状规则钉死（形状即安全边界）。
func TestGCShapeOK(t *testing.T) {
	ok := []string{
		"files/7/01J8ZQ7W6Y5X4V3T2S1R0Q9P8N/a.png",
		"artifacts/1/35/0",
		"icons/01J8ZQ7W6Y5X4V3T2S1R0Q9P8N.png",
	}
	bad := []string{
		"files/7/short/a.png",                     // id 长度不对
		"files/7/01J8ZQ7W6Y5X4V3T2S1R0Q9P8N/../x", // 路径段
		"artifacts/1/35",                          // 缺一段
		"icons/01J8ZQ7W6Y5X4V3T2S1R0Q9P8N",        // 无扩展名
		"other/whatever",
		"_selftest/x",
	}
	for _, k := range ok {
		if !gcShapeOK(k) {
			t.Fatalf("应被识别为受管形状: %s", k)
		}
	}
	for _, k := range bad {
		if gcShapeOK(k) {
			t.Fatalf("不应被识别为受管形状: %s", k)
		}
	}
}
