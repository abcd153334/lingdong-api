package task

import (
	"context"
	"errors"
	"testing"
	"time"

	"lingdong-api/internal/model"
)

// TestOpenArtifactAtDecidesFallback 钉死「过期」与「从未转存」的分界。
//
// 这是保留期能否成立的唯一开关：控制器只在 ErrContentNotStored 时回源上游。
// 旧实现把「本地打不开」一律当可回源，于是产物过期后用户仍能拿到文件。
//
// 尤其注意第三种情况：**行已被清理删除**但 artifact_count 仍为 1。
// 若用「有没有 files 行」判断「从未转存」，清理会亲手把过期产物变回「可回源」。
func TestOpenArtifactAtDecidesFallback(t *testing.T) {
	gdb := withTestDB(t)
	if err := gdb.AutoMigrate(&model.File{}); err != nil {
		t.Fatalf("迁移 files 表失败: %v", err)
	}
	ctx := context.Background()
	now := time.Now()

	expiredAt := now.Add(-time.Hour)
	futureAt := now.Add(time.Hour)

	t.Run("从未转存（artifact_count=0）", func(t *testing.T) {
		task := &model.Task{ID: 9001, UserID: 1, Model: "m", Modality: "image", Status: StatusSucceeded, ArtifactCount: 0}
		_, err := OpenArtifactAt(ctx, task, 0, 0, 0)
		if !errors.Is(err, ErrContentNotStored) {
			t.Fatalf("没有任何产物行时应判「从未转存」（允许回源），实际 %v", err)
		}
	})

	t.Run("已过期（行还在）", func(t *testing.T) {
		row := &model.File{
			TaskID: 9002, ArtifactIndex: 0, UserID: 1, Filename: "a.png", SizeBytes: 1,
			StorageBackend: "none", StorageKey: "files/1/t9002/a.png", ExpiresAt: &expiredAt,
		}
		if err := gdb.Create(row).Error; err != nil {
			t.Fatalf("造行失败: %v", err)
		}
		task := &model.Task{ID: 9002, UserID: 1, Model: "m", Modality: "image", Status: StatusSucceeded, ArtifactCount: 1}
		_, err := OpenArtifactAt(ctx, task, 0, 0, 0)
		if !errors.Is(err, ErrContentGone) {
			t.Fatalf("过期必须是终态（410 且不回源），实际 %v", err)
		}
		if errors.Is(err, ErrContentNotStored) {
			t.Fatal("过期不得退化成「从未转存」——那会重新打开回源")
		}
	})

	t.Run("已清理（行被删，count 仍为 1）", func(t *testing.T) {
		task := &model.Task{ID: 9003, UserID: 1, Model: "m", Modality: "image", Status: StatusSucceeded, ArtifactCount: 1}
		// 模拟 PurgeArtifacts 已把行删掉（artifact_count 不动）。
		_, err := OpenArtifactAt(ctx, task, 0, 0, 0)
		if !errors.Is(err, ErrContentGone) || errors.Is(err, ErrContentNotStored) {
			t.Fatalf("已清理的产物必须判「不可读且不回源」，实际 %v", err)
		}
	})

	t.Run("序号越界", func(t *testing.T) {
		row := &model.File{
			TaskID: 9004, ArtifactIndex: 0, UserID: 1, Filename: "b.png", SizeBytes: 1,
			StorageBackend: "none", StorageKey: "files/1/t9004/b.png", ExpiresAt: &futureAt,
		}
		if err := gdb.Create(row).Error; err != nil {
			t.Fatalf("造行失败: %v", err)
		}
		task := &model.Task{ID: 9004, UserID: 1, Model: "m", Modality: "image", Status: StatusSucceeded, ArtifactCount: 1}
		// 越界 = 这个序号从来没存在过（count=1 只有 0 号）→ 404「序号不存在」，
		// 与「曾经存在但已清理」（410）必须分开：前者要重新取清单，后者别再试。
		if _, err := OpenArtifactAt(ctx, task, 1, 0, 0); !errors.Is(err, ErrArtifactNotFound) {
			t.Fatalf("越界应判「序号不存在」，实际 %v", err)
		}
		if _, err := OpenArtifactAt(ctx, task, -1, 0, 0); !errors.Is(err, ErrArtifactNotFound) {
			t.Fatalf("负下标应判「序号不存在」，实际 %v", err)
		}
	})

	t.Run("存储后端缺失（可重试）", func(t *testing.T) {
		// 记录指向一个已从配置里消失的后端：对象还在桶里，补配置就能读。
		// 折成 410 的事故形态是「运维把桶配错，所有人看到文件已过期」。
		row := &model.File{
			TaskID: 9006, ArtifactIndex: 0, UserID: 1, Filename: "c.png", SizeBytes: 9,
			StorageBackend: "gone-backend", StorageKey: "files/1/t9006/c.png", ExpiresAt: &futureAt,
		}
		if err := gdb.Create(row).Error; err != nil {
			t.Fatalf("造行失败: %v", err)
		}
		task := &model.Task{ID: 9006, UserID: 1, Model: "m", Modality: "image", Status: StatusSucceeded, ArtifactCount: 1}
		_, err := OpenArtifactAt(ctx, task, 0, 0, 0)
		if !errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("后端缺失必须判可重试（503），实际 %v", err)
		}
		if errors.Is(err, ErrContentGone) {
			t.Fatal("不得折成 410：那会让客户端永久放弃，运维也查不到配置问题")
		}

		// HEAD 与 GET 必须给出同一结论，但 HEAD **不许打开对象**。
		// 用同一行做判据：OpenArtifactAt 会因后端缺失失败，而 HeadArtifact
		// 只读库里的元信息 —— 它成功，就证明没有走存储。
		size, ct, herr := HeadArtifact(ctx, task, 0)
		if herr != nil {
			t.Fatalf("HEAD 不应打开对象，实际失败 %v", herr)
		}
		if size != 9 || ct == "" {
			t.Fatalf("HEAD 应给出记录里的元信息，实际 size=%d ct=%q", size, ct)
		}
	})

	// 过期行在中间时，序号必须按「可读集合」重排 —— 与清单同一份切片。
	t.Run("过期行不占号", func(t *testing.T) {
		rows := []*model.File{
			{TaskID: 9005, ArtifactIndex: 0, UserID: 1, Filename: "0.png", SizeBytes: 10,
				StorageBackend: "none", StorageKey: "files/1/t9005/0.png", ExpiresAt: &expiredAt},
			{TaskID: 9005, ArtifactIndex: 1, UserID: 1, Filename: "1.png", SizeBytes: 20,
				StorageBackend: "none", StorageKey: "files/1/t9005/1.png", ExpiresAt: &futureAt},
		}
		for _, row := range rows {
			if err := gdb.Create(row).Error; err != nil {
				t.Fatalf("造行失败: %v", err)
			}
		}
		task := &model.Task{ID: 9005, UserID: 1, Model: "m", Modality: "image", Status: StatusSucceeded, ArtifactCount: 2}
		// 第 0 个可读产物应是 internal index=1 那条（对象不存在 → 不可读，但不是「越界」）。
		_, err := OpenArtifactAt(ctx, task, 0, 0, 0)
		if errors.Is(err, ErrContentNotStored) {
			t.Fatal("有可读行时不得判「从未转存」")
		}
		list, lerr := ArtifactsOf(ctx, 9005)
		if lerr != nil {
			t.Fatalf("清单失败: %v", lerr)
		}
		if len(list) != 1 || list[0].Index != 0 || list[0].Bytes != 20 {
			t.Fatalf("清单应只含未过期那条且序号为 0，实际 %+v", list)
		}
	})
}
