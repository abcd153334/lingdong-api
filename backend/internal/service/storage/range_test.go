package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

// TestLocalOpenRange 验证本地存储的区间读取（B1-4：播放器拖动进度条依赖它）。
func TestLocalOpenRange(t *testing.T) {
	s := &localStore{root: t.TempDir()}
	ctx := context.Background()
	content := []byte("0123456789abcdef")
	if err := s.Put(ctx, "artifacts/1/2/0", bytes.NewReader(content), int64(len(content)), "video/mp4"); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	cases := []struct {
		name   string
		offset int64
		length int64
		want   string
	}{
		{"中段区间", 4, 6, "456789"},
		{"到结尾", 10, 0, "abcdef"},
		{"从头到结尾", 0, 0, "0123456789abcdef"},
		{"长度超出剩余", 12, 100, "cdef"},
		{"单字节", 0, 1, "0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rc, err := s.OpenRange(ctx, "artifacts/1/2/0", c.offset, c.length)
			if err != nil {
				t.Fatalf("OpenRange 失败: %v", err)
			}
			defer rc.Close()
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("读取失败: %v", err)
			}
			if string(got) != c.want {
				t.Fatalf("区间内容 = %q，期望 %q", got, c.want)
			}
		})
	}

	if _, err := s.OpenRange(ctx, "artifacts/missing", 0, 1); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("对象不存在应返回 ErrObjectNotFound，实际 %v", err)
	}
}

// TestLocalStoreImplementsRangeOpener 锁死「本地后端必须支持 Range」这个契约：
// 一旦有人改动结构体导致能力丢失，这里会立刻报错而不是线上播放器静默退回整段下载。
func TestLocalStoreImplementsRangeOpener(t *testing.T) {
	var s Store = &localStore{root: t.TempDir()}
	if _, ok := s.(RangeOpener); !ok {
		t.Fatal("localStore 必须实现 RangeOpener")
	}
}
