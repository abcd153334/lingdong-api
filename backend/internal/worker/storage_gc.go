package worker

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/service/storage"
)

// storageGCInterval：孤儿对账每天一轮。它不是时效性任务 ——
// 孤儿对象是「库外残留」，多留一天只影响成本，不影响正确性。
const storageGCInterval = 24 * time.Hour

// StorageGCJob 做「桶里有、库里没有」的对象对账。
//
// 默认**不跑**（LD_STORAGE_GC 未设置），设置了也默认 dry-run：
// 删除是自治行为，必须先让运维看清它会删什么，再显式切成 enforce。
type StorageGCJob struct{ last atomic.Int64 }

// NewStorageGCJob 构造对账任务。
func NewStorageGCJob() *StorageGCJob { return &StorageGCJob{} }

// Name 返回任务标识。
func (*StorageGCJob) Name() string { return "storage_gc" }

// Interval 返回节拍。
func (*StorageGCJob) Interval() time.Duration { return storageGCInterval }

// Run 执行一轮对账。
//
// 环境变量（唯一开关来源）：
//   - LD_STORAGE_GC=dry|enforce（未设置 = 不跑）
//   - LD_STORAGE_GC_SELFTEST=1（额外扫描 _selftest/ 前缀，供真机验证兜底清理）
func (j *StorageGCJob) Run(ctx context.Context) error {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("LD_STORAGE_GC")))
	if mode == "" {
		return nil
	}
	ref, err := storage.ActiveRef()
	if err != nil {
		return err
	}
	if ref.Bucket == "" {
		// 本地后端没有「桶」概念，也没有列举 API 的必要（磁盘水位由容量体检负责）。
		return nil
	}
	prefixes := []string{"files/", "artifacts/", "icons/"}
	if os.Getenv("LD_STORAGE_GC_SELFTEST") == "1" {
		prefixes = append(prefixes, "_selftest/")
	}
	rep, err := storage.RunStorageGC(ctx, storage.GCOptions{
		Buckets:  []storage.Ref{ref},
		Prefixes: prefixes,
		MinAge:   48 * time.Hour,
		Limit:    200,
		DryRun:   mode != "enforce",
		Exists:   tableKeysExist,
	})
	deleted := 0
	for _, br := range rep.ByBucket {
		deleted += br.Deleted
	}
	j.last.Store(int64(deleted))
	if err != nil {
		return err
	}
	if rep.Error != "" {
		logger.L().Warn("孤儿对账未完整执行", zap.String("error", rep.Error))
	}
	return nil
}

// LastCount 返回上一轮真正删掉的对象数。
func (j *StorageGCJob) LastCount() int { return int(j.last.Load()) }

// tableKeysExist 回答「这批 key 里哪些在库里有行」。
//
// 三张表各自承接一类前缀：uploads ← files/、files ← artifacts/、icons ← icons/。
// 用一张表查全部 key 会把「对象在 icons、行在 uploads」这种错配漏掉。
func tableKeysExist(ctx context.Context, bucket string, keys []string) (map[string]bool, error) {
	out := make(map[string]bool, len(keys))
	byTable := map[string][]string{}
	for _, k := range keys {
		if table := tableOfKey(k); table != "" {
			byTable[table] = append(byTable[table], k)
		}
	}
	for table, ks := range byTable {
		var found []string
		q := db.DB.WithContext(ctx).Table(table).Where("storage_key IN ?", ks)
		if bucket != "" {
			q = q.Where("storage_bucket = ?", bucket)
		}
		if err := q.Pluck("storage_key", &found).Error; err != nil {
			return nil, err
		}
		for _, k := range found {
			out[k] = true
		}
	}
	return out, nil
}

// tableOfKey 把对象 key 映射到承接它的表；不受管的前缀返回空串。
func tableOfKey(key string) string {
	switch {
	case strings.HasPrefix(key, "files/"):
		return "uploads"
	case strings.HasPrefix(key, "artifacts/"):
		return "files"
	case strings.HasPrefix(key, "icons/"):
		return "icons"
	}
	return ""
}
