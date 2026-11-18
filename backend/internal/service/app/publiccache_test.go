package app

// 公开面行快照的单测。用可替换的 loader 计数，不依赖数据库 ——
// 这样"命中不查库"与"失效后重查"是**可证明**的，而不是靠读代码相信。

import (
	"context"
	"errors"
	"testing"

	"lingdong-api/internal/model"
)

func withLoader(t *testing.T, fn func(context.Context) (map[string]*model.App, error)) *int {
	t.Helper()
	ResetPublicCacheForTest()
	calls := new(int)
	old := loadPublicApps
	loadPublicApps = func(ctx context.Context) (map[string]*model.App, error) {
		*calls++
		return fn(ctx)
	}
	t.Cleanup(func() {
		loadPublicApps = old
		ResetPublicCacheForTest()
	})
	return calls
}

func okLoader(apps ...*model.App) func(context.Context) (map[string]*model.App, error) {
	return func(context.Context) (map[string]*model.App, error) {
		out := make(map[string]*model.App, len(apps))
		for _, a := range apps {
			out[a.Slug] = a
		}
		return out, nil
	}
}

// TestPublicSnapshotLoadsOnce 连打两次只加载一次。
//
// 删掉快照会怎样：匿名面每个请求都查一次 apps 表。这条面没有凭证、
// slug 可枚举，等于把一张表按请求量放大。
func TestPublicSnapshotLoadsOnce(t *testing.T) {
	calls := withLoader(t, okLoader(&model.App{ID: 1, Slug: "a"}))

	for i := 0; i < 3; i++ {
		if _, err := AppBySlug(context.Background(), "a"); err != nil {
			t.Fatalf("第 %d 次取应用失败: %v", i+1, err)
		}
	}
	if *calls != 1 {
		t.Fatalf("10s 内应只加载 1 次，实际 %d 次", *calls)
	}
}

// TestPublicSnapshotInvalidateReloads 失效后立刻重查，且新值生效。
//
// 删掉这条失效会怎样：管理端改完品牌，公开面最多 10 秒才变；而客户端还缓存
// 60 秒 —— 运营会认为"保存了没生效"。
func TestPublicSnapshotInvalidateReloads(t *testing.T) {
	holder := &model.App{ID: 1, Slug: "a", DisplayName: "旧名"}
	calls := withLoader(t, func(context.Context) (map[string]*model.App, error) {
		return map[string]*model.App{"a": holder}, nil
	})

	a, err := AppBySlug(context.Background(), "a")
	if err != nil || a.DisplayName != "旧名" {
		t.Fatalf("首次取值失败: %v / %+v", err, a)
	}

	holder = &model.App{ID: 1, Slug: "a", DisplayName: "新名"}
	InvalidateAppPublic()

	a, err = AppBySlug(context.Background(), "a")
	if err != nil {
		t.Fatalf("失效后取值失败: %v", err)
	}
	if a.DisplayName != "新名" {
		t.Fatalf("失效后应拿到新值，实际 %q", a.DisplayName)
	}
	if *calls != 2 {
		t.Fatalf("失效后应再加载 1 次（共 2 次），实际 %d 次", *calls)
	}
}

// TestPublicSnapshotFallsBackOnError 加载失败时沿用旧快照。
//
// 与维护态同一个取向：宁可多给 10 秒旧品牌，也不要因为一次抖动让所有终端拿不到
// 品牌。注意这与"无旧快照时失败"是两件事（见下一条）。
func TestPublicSnapshotFallsBackOnError(t *testing.T) {
	fail := false
	withLoader(t, func(context.Context) (map[string]*model.App, error) {
		if fail {
			return nil, errors.New("db down")
		}
		return map[string]*model.App{"a": {ID: 1, Slug: "a", DisplayName: "缓存值"}}, nil
	})

	if _, err := AppBySlug(context.Background(), "a"); err != nil {
		t.Fatalf("首次取值失败: %v", err)
	}
	fail = true
	InvalidateAppPublic()
	a, err := AppBySlug(context.Background(), "a")
	if err != nil {
		t.Fatalf("有旧快照时不应报错，实际 %v", err)
	}
	if a.DisplayName != "缓存值" {
		t.Fatalf("应沿用旧快照，实际 %q", a.DisplayName)
	}
}

// TestPublicSnapshotErrorIsNotNotFound 无旧快照且加载失败时，必须与"应用不存在"区分。
//
// 合成一个的错误码会怎样：数据库抖动期间所有终端都以为应用被删了，
// 而运营在后台看一切正常。
func TestPublicSnapshotErrorIsNotNotFound(t *testing.T) {
	withLoader(t, func(context.Context) (map[string]*model.App, error) {
		return nil, errors.New("db down")
	})

	_, err := AppBySlug(context.Background(), "a")
	if err == nil {
		t.Fatal("加载失败且无快照时应返回错误")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatal("快照不可用不能被当成\"应用不存在\"（那会回 404 而不是 500）")
	}
}

// TestPublicSnapshotMissIsNotFound 快照可用但没有该 slug → 业务上的不存在。
func TestPublicSnapshotMissIsNotFound(t *testing.T) {
	withLoader(t, okLoader(&model.App{ID: 1, Slug: "a"}))

	if _, err := AppBySlug(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("应返回 ErrNotFound，实际 %v", err)
	}
}
