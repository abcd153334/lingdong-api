package task

import (
	"context"
	"testing"

	"lingdong-api/internal/model"
)

// TestIdempotencyKeyIsScopedByEndpoint 钉死「幂等键只对同一端点有效」。
//
// 事故形态（改造前）：幂等作用域只有 (key_id, idempotency_key)。客户端用同一个
// Idempotency-Key 先提交音乐、再提交视频时，第二次请求会**直接拿回那首音乐任务**
// —— 状态 200、不报错，但模态不符、扣费与产物全错，客户端还会拿它去轮询视频产物。
//
// 判据只有 findByIdempotencyKey 一处：它必须按 (KEY, 端点, 键) 命中，
// 并且仍能认出改造前的历史行（endpoint=”，旧行为只按 KEY+键去重）。
func TestIdempotencyKeyIsScopedByEndpoint(t *testing.T) {
	gdb := withTestDB(t)
	if err := gdb.AutoMigrate(&model.Task{}); err != nil {
		t.Fatalf("迁移 tasks 表失败: %v", err)
	}
	ctx := context.Background()

	mk := func(endpoint, idem string, keyID uint64) *model.Task {
		row := &model.Task{
			KeyID: keyID, UserID: 7, Model: "m", Modality: "music", Kind: "music",
			Status: StatusQueued, IdempotencyKey: idem, IdempotencyEndpoint: endpoint,
		}
		if err := gdb.Create(row).Error; err != nil {
			t.Fatalf("造行失败(%s): %v", endpoint, err)
		}
		return row
	}

	music := mk("audio/music/generations", "same-key", 11)
	video := mk("video/generations", "same-key", 11)

	t.Run("同键不同端点互不命中", func(t *testing.T) {
		got, ok := findByIdempotencyKey(ctx, 11, "video/generations", "same-key")
		if !ok || got.ID != video.ID {
			t.Fatalf("视频端点应命中视频任务，实际 %v ok=%v", got, ok)
		}
		got, ok = findByIdempotencyKey(ctx, 11, "audio/music/generations", "same-key")
		if !ok || got.ID != music.ID {
			t.Fatalf("音乐端点应命中音乐任务，实际 %v ok=%v", got, ok)
		}
	})

	t.Run("重试同键同端点仍命中首次任务", func(t *testing.T) {
		got, ok := findByIdempotencyKey(ctx, 11, "video/generations", "same-key")
		if !ok || got.ID != video.ID {
			t.Fatalf("同端点重试必须返回同一条，实际 %v ok=%v", got, ok)
		}
	})

	t.Run("历史行（无端点）仍按旧口径命中", func(t *testing.T) {
		// 上线瞬间的历史行没有端点：若不认它，第一次重试就会绕过旧行再建一单，
		// 而幂等键存在的意义正是「别建第二单」。
		legacy := mk("", "legacy-key", 12)
		got, ok := findByIdempotencyKey(ctx, 12, "video/generations", "legacy-key")
		if !ok || got.ID != legacy.ID {
			t.Fatalf("历史行应被命中，实际 %v ok=%v", got, ok)
		}
	})

	t.Run("换一把钥不共享幂等键", func(t *testing.T) {
		if _, ok := findByIdempotencyKey(ctx, 13, "video/generations", "same-key"); ok {
			t.Fatal("不同 KEY 的同名幂等键不得互相命中")
		}
	})
}
