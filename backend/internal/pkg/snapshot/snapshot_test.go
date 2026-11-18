package snapshot

// 本文件守的是这份并发逻辑里最容易写错的三件事：
//   1. TTL 内只加载一次；
//   2. 失败后沿用旧值，且不因为"每个请求重试一次"把故障放大；
//   3. 没有任何旧值时，失败在退避窗口内不再打库。
// 第 2、3 条是 pull request 评审中最容易被改回去的地方。

import (
	"context"
	"errors"
	"testing"
	"time"
)

func newTestSnap(t *testing.T, load func(context.Context) (map[string]int, error)) (*Snapshot[string, int], *int) {
	t.Helper()
	calls := new(int)
	s := New("test", 10*time.Millisecond, func(ctx context.Context) (map[string]int, error) {
		*calls++
		return load(ctx)
	})
	t.Cleanup(s.ResetForTest)
	return s, calls
}

func TestLoadsOnceWithinTTL(t *testing.T) {
	s, calls := newTestSnap(t, func(context.Context) (map[string]int, error) {
		return map[string]int{"a": 1}, nil
	})
	for i := 0; i < 5; i++ {
		got, err := s.Get(context.Background())
		if err != nil || got["a"] != 1 {
			t.Fatalf("第 %d 次取值失败: %v / %+v", i+1, err, got)
		}
	}
	if *calls != 1 {
		t.Fatalf("TTL 内应只加载 1 次，实际 %d", *calls)
	}
}

func TestInvalidateReloads(t *testing.T) {
	s, calls := newTestSnap(t, func(context.Context) (map[string]int, error) {
		return map[string]int{"a": 1}, nil
	})
	if _, err := s.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Invalidate()
	if _, err := s.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *calls != 2 {
		t.Fatalf("Invalidate 后应重新加载，实际 %d 次", *calls)
	}
}

// TestKeepsOldValueOnErrorWithoutRetryStorm 有旧值时失败：沿用旧值且不再重试。
//
// 改回"失败不推进 loadedAt"会怎样：DB 抖动期间每个请求都拿一次写锁、查一次
// 全表 —— 快照从"保护数据库"变成"放大故障"。
func TestKeepsOldValueOnErrorWithoutRetryStorm(t *testing.T) {
	fail := false
	s, calls := newTestSnap(t, func(context.Context) (map[string]int, error) {
		if fail {
			return nil, errors.New("db down")
		}
		return map[string]int{"a": 1}, nil
	})
	if _, err := s.Get(context.Background()); err != nil {
		t.Fatal(err)
	}

	fail = true
	s.Invalidate()
	for i := 0; i < 3; i++ {
		got, err := s.Get(context.Background())
		if err != nil {
			t.Fatalf("有旧值时不应返回错误，实际 %v", err)
		}
		if got["a"] != 1 {
			t.Fatalf("应沿用旧值，实际 %+v", got)
		}
	}
	if *calls != 2 {
		t.Fatalf("失败后应只重试 1 次（共 2 次），实际 %d 次", *calls)
	}
}

// TestUnavailableBacksOff 无旧值时失败：返回 ErrUnavailable 并退避。
func TestUnavailableBacksOff(t *testing.T) {
	s, calls := newTestSnap(t, func(context.Context) (map[string]int, error) {
		return nil, errors.New("db down")
	})
	for i := 0; i < 3; i++ {
		if _, err := s.Get(context.Background()); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("第 %d 次应返回 ErrUnavailable，实际 %v", i+1, err)
		}
	}
	if *calls != 1 {
		t.Fatalf("退避窗口内应只尝试 1 次，实际 %d 次", *calls)
	}
}

// TestRecoversAfterBackoff 退避到期后允许恢复。
func TestRecoversAfterBackoff(t *testing.T) {
	fail := true
	s, calls := newTestSnap(t, func(context.Context) (map[string]int, error) {
		if fail {
			return nil, errors.New("db down")
		}
		return map[string]int{"a": 1}, nil
	})
	if _, err := s.Get(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("首次应失败，实际 %v", err)
	}

	fail = false
	s.mu.Lock()
	s.nextTry = time.Now().Add(-time.Second)
	s.mu.Unlock()

	got, err := s.Get(context.Background())
	if err != nil || got["a"] != 1 {
		t.Fatalf("退避到期后应恢复，实际 %v / %+v", err, got)
	}
	if *calls != 2 {
		t.Fatalf("应重新加载 1 次（共 2 次），实际 %d", *calls)
	}
}

func TestExpireForTestReloads(t *testing.T) {
	s, calls := newTestSnap(t, func(context.Context) (map[string]int, error) {
		return map[string]int{"a": 1}, nil
	})
	if _, err := s.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.ExpireForTest()
	if _, err := s.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *calls != 2 {
		t.Fatalf("TTL 到期后应重新加载，实际 %d 次", *calls)
	}
}
