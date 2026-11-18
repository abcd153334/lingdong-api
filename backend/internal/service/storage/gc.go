package storage

import (
	"context"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/metrics"
)

// 孤儿对账：找出「桶里有对象、库里没有任何行」的东西。
//
// 它解决的是另一类问题（与到期清理不重叠）：
//   - 写入对象成功、落库失败且补偿删除也失败（进程崩溃/网络）；
//   - 历史上换过桶/换过后端后没有线索的残留；
//   - 探针与人工上传留下的测试对象。
//
// 默认 dry-run 只报告：删除是自治行为，必须先让运维看清「它会删什么」。
type GCOptions struct {
	// Buckets 是要扫描的位置白名单（Backend + Bucket）；不在白名单的桶一律不碰。
	Buckets []Ref
	// Prefixes 是受管前缀白名单（files/ artifacts/ icons/）。
	Prefixes []string
	// MinAge 是「对象至少存在这么久」才可能被判孤儿（挡住刚落库前的秒级窗口）。
	MinAge time.Duration
	// Limit 是每桶每轮最多处理的对象数（不是扫描数）。
	Limit  int
	DryRun bool
	// Exists 回答「这批 key 里哪些在库里有对应行」（由调用方注入，避免 storage 反向依赖 db）。
	Exists func(ctx context.Context, bucket string, keys []string) (map[string]bool, error)
}

// GCBucketReport 是单个桶的对账结果。
type GCBucketReport struct {
	Scanned      int   `json:"scanned"`
	Orphans      int   `json:"orphans"`
	Deleted      int   `json:"deleted"`
	Failed       int   `json:"failed"`
	SkippedYoung int   `json:"skipped_young"`
	SkippedShape int   `json:"skipped_shape"`
	Bytes        int64 `json:"bytes"`
}

// GCReport 是一轮对账的汇总。
type GCReport struct {
	At          time.Time                 `json:"at"`
	DryRun      bool                      `json:"dry_run"`
	ByBucket    map[string]GCBucketReport `json:"by_bucket"`
	Unsupported []string                  `json:"unsupported,omitempty"`
	Error       string                    `json:"error,omitempty"`
}

var (
	// 三类受管 key 的形状。形状不符的对象只报告不删：共享桶里可能还有别人的目录，
	// 把「不认识」当成「孤儿」是删库级事故。
	gcShapeFiles     = regexp.MustCompile(`^files/\d+/[0-9A-Za-z]{26}/[A-Za-z0-9._-]{1,80}$`)
	gcShapeArtifacts = regexp.MustCompile(`^artifacts/\d+/\d+/\d+$`)
	gcShapeIcons     = regexp.MustCompile(`^icons/[0-9A-Za-z]{26}\.[a-z0-9]{2,5}$`)
)

// gcShapeOK 判定 key 是否属于受管形状。
func gcShapeOK(key string) bool {
	switch {
	case strings.HasPrefix(key, "files/"):
		return gcShapeFiles.MatchString(key)
	case strings.HasPrefix(key, "artifacts/"):
		return gcShapeArtifacts.MatchString(key)
	case strings.HasPrefix(key, "icons/"):
		return gcShapeIcons.MatchString(key)
	}
	return false
}

// allowedShape 在受管形状之外，额外放行**调用方显式要求扫描**的前缀。
//
// 目前只有 `_selftest/`（真机删除验证的隔离目录）：它不在三张表里，
// 所以只可能整体是孤儿；但它必须显式出现在 Prefixes 里才会被放行，
// 避免共享桶里别人的 `_selftest/` 被当成垃圾。
func allowedShape(prefixes []string, key string) bool {
	if gcShapeOK(key) {
		return true
	}
	if strings.HasPrefix(key, "_selftest/") {
		for _, p := range prefixes {
			if p == "_selftest/" {
				return true
			}
		}
	}
	return false
}

// RunStorageGC 执行一轮孤儿对账。
//
// 顺序：列举 → 形状过滤 → 年龄过滤 → 查库 → （非 dry-run）删除并复核。
// 任何一步「不确定」都落到「不删」，并计入报告。
func RunStorageGC(ctx context.Context, opts GCOptions) (GCReport, error) {
	rep := GCReport{At: time.Now(), DryRun: opts.DryRun, ByBucket: map[string]GCBucketReport{}}
	if opts.MinAge <= 0 {
		opts.MinAge = 48 * time.Hour
	}
	if opts.Limit <= 0 {
		opts.Limit = 200
	}
	cutoff := time.Now().Add(-opts.MinAge)

	for _, target := range opts.Buckets {
		label := target.Backend + "/" + target.Bucket
		st, err := ForRef(Ref{Backend: target.Backend, Bucket: target.Bucket})
		if err != nil {
			rep.Unsupported = append(rep.Unsupported, label+"("+err.Error()+")")
			continue
		}
		lister, ok := st.(Lister)
		if !ok {
			rep.Unsupported = append(rep.Unsupported, label+"(不支持列举)")
			continue
		}
		br := GCBucketReport{}
		handled := 0

		for _, prefix := range opts.Prefixes {
			marker := ""
			for handled < opts.Limit {
				page, perr := lister.List(ctx, prefix, marker, 500)
				if perr != nil {
					rep.Error = "列举失败: " + perr.Error()
					break
				}
				for _, obj := range page.Objects {
					br.Scanned++
					if !allowedShape(opts.Prefixes, obj.Key) {
						br.SkippedShape++
						continue
					}
					// 时间未知（部分实现不给 LastModified）不删：把未知当很久以前会误删新对象。
					if obj.ModTime.IsZero() || !obj.ModTime.Before(cutoff) {
						br.SkippedYoung++
						continue
					}
					keys := []string{obj.Key}
					exists, xerr := opts.Exists(ctx, target.Bucket, keys)
					if xerr != nil {
						rep.Error = "查库失败: " + xerr.Error()
						break
					}
					if exists[obj.Key] {
						continue
					}
					br.Orphans++
					if opts.DryRun {
						continue
					}
					if _, derr := DeleteRefVerified(ctx, Ref{Backend: target.Backend, Bucket: target.Bucket, Key: obj.Key}); derr != nil {
						br.Failed++
						metrics.Inc(metrics.NameStorageGCTotal, "bucket", target.Bucket, "result", "failed")
						continue
					}
					br.Deleted++
					br.Bytes += obj.Size
					handled++
					metrics.Inc(metrics.NameStorageGCTotal, "bucket", target.Bucket, "result", "deleted")
				}
				if page.Next == "" || page.Next == marker {
					break
				}
				marker = page.Next
				if len(page.Objects) == 0 {
					break
				}
			}
			if rep.Error != "" || handled >= opts.Limit {
				break
			}
		}

		rep.ByBucket[label] = br
		metrics.Set(metrics.NameStorageOrphans, float64(br.Orphans), "bucket", target.Bucket)
		logger.L().Info("孤儿对账完成",
			zap.String("backend", target.Backend), zap.String("bucket", target.Bucket),
			zap.Bool("dry_run", opts.DryRun), zap.Int("scanned", br.Scanned),
			zap.Int("orphans", br.Orphans), zap.Int("deleted", br.Deleted),
			zap.Int("failed", br.Failed), zap.Int("skipped_shape", br.SkippedShape),
			zap.Int("skipped_young", br.SkippedYoung))
	}
	return rep, nil
}

// ActiveRef 返回当前生效后端与它的桶（供对账确定默认扫描位置）。
func ActiveRef() (Ref, error) {
	st, err := Active()
	if err != nil {
		return Ref{}, err
	}
	return Ref{Backend: st.Name(), Bucket: st.Bucket()}, nil
}
