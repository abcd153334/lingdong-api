package upload

import (
	"context"
	"strconv"
	"testing"
	"time"

	"lingdong-api/internal/model"
)

// 本文件验证 sort 片段真的到达 SQL（不是只被 handler 接住然后丢掉）。

// TestListSortReachesSQL 默认（created_at DESC）与 size ASC 必须给出不同顺序。
//
// 特意用 `size`：它对外叫 size、落库叫 size_bytes，是唯一一个"对外名 ≠ 列名"
// 的排序字段。若 handler 的映射表写错，这里会直接报列不存在（而不是静默不排序）。
func TestListSortReachesSQL(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	// 时间必须相对 now 取：List 会过滤掉 expires_at <= now 的行，
	// 写死一个过去的时间点会让用例"造了 3 条却查出 0 条"。
	now := time.Now()
	expire := now.Add(24 * time.Hour)

	// 大小与插入顺序错开（300, 100, 200），保证两种排序首条就不同；
	// created_at 递增，于是默认倒序的第一条是最后插入的那条（200）。
	for i, size := range []int64{300, 100, 200} {
		row := &model.Upload{
			ID: "SORT-" + strconv.Itoa(i), UserID: 1, Filename: "f" + strconv.Itoa(i),
			SizeBytes: size, ContentType: "image/png",
			CreatedAt: now.Add(-time.Duration(3-i) * time.Hour), ExpiresAt: expire,
		}
		if err := gdb.Create(row).Error; err != nil {
			t.Fatalf("造上传件失败: %v", err)
		}
	}

	def, total, err := List(ctx, ListFilter{UserID: 1, Limit: 10}, "https://cdn.example.com")
	if err != nil {
		t.Fatalf("默认列表失败: %v", err)
	}
	if total != 3 || len(def) != 3 {
		t.Fatalf("默认列表条数不符: total=%d len=%d", total, len(def))
	}
	if def[0].SizeBytes != 200 || def[2].SizeBytes != 300 {
		t.Fatalf("默认应按 created_at 倒序（新的在前）: %d,%d,%d",
			def[0].SizeBytes, def[1].SizeBytes, def[2].SizeBytes)
	}

	asc, _, err := List(ctx, ListFilter{
		UserID: 1, Limit: 10, OrderBy: "size_bytes ASC, id ASC",
	}, "https://cdn.example.com")
	if err != nil {
		t.Fatalf("size 升序失败: %v", err)
	}
	if asc[0].SizeBytes != 100 || asc[2].SizeBytes != 300 {
		t.Fatalf("size_bytes ASC 应按字节数升序: %d,%d,%d",
			asc[0].SizeBytes, asc[1].SizeBytes, asc[2].SizeBytes)
	}
}
