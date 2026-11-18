package storage_test

// 真实对象存储的删除验证（默认跳过）。
//
// 运行方式（在 backend/ 目录）：
//   LD_OSS_TEST=1 go test ./internal/service/storage -run TestOSSLive -v
//
// 它做四件事，全部在 `_selftest/{日期}/{ulid}/` 前缀下，结束必清：
//  1. CheckBucket（顺带把桶的版本控制状态打出来 —— 开了版本控制时删除只产生标记）；
//  2. Put → Head 存在 → DeleteRefVerified → Head 不存在 → 再删一次应为 Gone（幂等）；
//  3. 留一个对象不删，断言孤儿对账的 dry-run 能发现它且**没有删**；
//  4. 按前缀全删并断言为空。
//
// 为什么必须有这一条：本地桩无法验证签名、桶策略、版本控制与强一致性，
// 而「到期硬删除」的成败恰恰只由这四项决定。

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/joho/godotenv"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/service/storage"
)

func TestOSSLiveDeleteVerified(t *testing.T) {
	if os.Getenv("LD_OSS_TEST") != "1" {
		t.Skip("未设置 LD_OSS_TEST=1，跳过真实对象存储用例")
	}
	// 开发机的连接串与主密钥在 backend/.env；生产/CI 应直接给环境变量。
	// 必须先 conf.Load()：解封凭据依赖 APP_ENCRYPTION_KEY，缺它时字段会被置空，
	// 表现为「配置不全」而不是「密钥没配」—— 正是 credential_unreadable 要区分的场景。
	_ = godotenv.Load("../../../.env")
	conf.Load()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("缺少 DATABASE_URL，无法读取封印的存储凭据")
	}
	gdb, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	if sqlDB, e := gdb.DB(); e == nil {
		defer sqlDB.Close()
	}
	cfg, err := db.LoadSystemConfig(gdb) // 与启动同一条路径：解封 + 标记坏凭据
	if err != nil {
		t.Fatalf("读取 system_config 失败: %v", err)
	}
	conf.System().Reload(cfg)

	ref, err := storage.ActiveRef()
	if err != nil {
		t.Skipf("当前存储不可构建（可能是未配置/凭据不可读）: %v", err)
	}
	if ref.Bucket == "" {
		t.Skip("当前是本地后端，真实 OSS 用例不适用")
	}
	ctx := context.Background()

	st, err := storage.ForRef(storage.Ref{Backend: ref.Backend, Bucket: ref.Bucket})
	if err != nil {
		t.Fatalf("构建存储失败: %v", err)
	}
	state, err := st.CheckBucket(ctx)
	if err != nil {
		t.Fatalf("CheckBucket 失败（AK 可能缺 GetBucketInfo/ListObjects 权限）: %v", err)
	}
	t.Logf("后端=%s 桶=%s 存在=%v 版本控制=%v", ref.Backend, ref.Bucket, state.Exists, state.Versioned)
	if !state.Exists {
		t.Fatalf("桶 %s 不存在", ref.Bucket)
	}

	prefix := fmt.Sprintf("_selftest/%s/%d/", time.Now().Format("20060102"), time.Now().UnixNano())
	key := prefix + "probe.txt"
	payload := []byte("lingdong self test")

	if err := st.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), "text/plain"); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	t.Cleanup(func() { cleanupPrefix(t, ref, prefix) })

	if _, err := st.Head(ctx, key); err != nil {
		t.Fatalf("写入后 Head 应存在: %v", err)
	}
	out, err := storage.DeleteRefVerified(ctx, storage.Ref{Backend: ref.Backend, Bucket: ref.Bucket, Key: key})
	if err != nil || out != storage.DeleteGone {
		t.Fatalf("删除应确认生效: out=%s err=%v", out, err)
	}
	if _, err := st.Head(ctx, key); err == nil {
		t.Fatal("删除后 Head 不应再读到对象")
	}
	out, err = storage.DeleteRefVerified(ctx, storage.Ref{Backend: ref.Backend, Bucket: ref.Bucket, Key: key})
	if err != nil || out != storage.DeleteGone {
		t.Fatalf("重复删除应幂等: out=%s err=%v", out, err)
	}

	// 留一个对象，验证对账能发现它、且 dry-run 不删。
	leftover := prefix + "orphan.txt"
	if err := st.Put(ctx, leftover, bytes.NewReader(payload), int64(len(payload)), "text/plain"); err != nil {
		t.Fatalf("写入第二个对象失败: %v", err)
	}
	rep, err := storage.RunStorageGC(ctx, storage.GCOptions{
		Buckets:  []storage.Ref{{Backend: ref.Backend, Bucket: ref.Bucket}},
		Prefixes: []string{"_selftest/"},
		MinAge:   time.Nanosecond, // 刚写入的对象也要进入候选，才能验证「dry-run 不删」
		Limit:    10,
		DryRun:   true,
		Exists:   func(context.Context, string, []string) (map[string]bool, error) { return map[string]bool{}, nil },
	})
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	label := ref.Backend + "/" + ref.Bucket
	if rep.ByBucket[label].Orphans == 0 {
		t.Fatalf("对账应发现 _selftest 下的对象: %+v", rep)
	}
	if rep.ByBucket[label].Deleted != 0 {
		t.Fatalf("dry-run 不允许删除: %+v", rep)
	}
	if _, err := st.Head(ctx, leftover); err != nil {
		t.Fatalf("dry-run 后对象应仍在: %v", err)
	}
}

// cleanupPrefix 按前缀删净测试对象；失败时打印前缀供人工用 storage_gc 兜底。
func cleanupPrefix(t *testing.T, ref storage.Ref, prefix string) {
	t.Helper()
	ctx := context.Background()
	st, err := storage.ForRef(storage.Ref{Backend: ref.Backend, Bucket: ref.Bucket})
	if err != nil {
		t.Logf("清理失败（构建存储）: %v，前缀=%s", err, prefix)
		return
	}
	lister, ok := st.(storage.Lister)
	if !ok {
		t.Logf("后端不支持列举，无法自清，前缀=%s", prefix)
		return
	}
	marker := ""
	for {
		page, err := lister.List(ctx, prefix, marker, 500)
		if err != nil {
			t.Logf("清理列举失败: %v，前缀=%s", err, prefix)
			return
		}
		for _, obj := range page.Objects {
			if _, derr := storage.DeleteRefVerified(ctx, storage.Ref{Backend: ref.Backend, Bucket: ref.Bucket, Key: obj.Key}); derr != nil {
				t.Logf("清理删除失败 key=%s: %v", obj.Key, derr)
			}
		}
		if page.Next == "" || page.Next == marker {
			break
		}
		marker = page.Next
	}
	page, err := lister.List(ctx, prefix, "", 10)
	if err == nil && len(page.Objects) > 0 {
		t.Logf("仍有 %d 个对象未清理，前缀=%s（可设 LD_STORAGE_GC_SELFTEST=1 让 storage_gc 处理）",
			len(page.Objects), prefix)
	}
}
