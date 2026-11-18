package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ---------- 本地磁盘（none） ----------

// localStore 是本地磁盘后端，根目录来自 STORAGE_DIR。
//
// 它**不读 storage 配置**：配置坏掉时仍要能读本地对象（For("none") 永远可构建），
// 这也是它取代旧 Local() 回落的原因 —— 韧性没有降低，但回落路径只有一条。
type localStore struct{ root string }

func (s *localStore) Name() string { return "none" }

// Bucket 本地后端没有桶概念（落库时 *_bucket 列即为空串）。
func (s *localStore) Bucket() string { return "" }

// CheckBucket 对本地后端恒为「可用」。
//
// 根目录不存在不是问题：那意味着还没有任何对象，删除请求本来就无事可做
// （Head 会正常返回 ErrObjectNotFound）。这里只拦「根目录存在但不是目录」这种
// 真正的配置错误。
func (s *localStore) CheckBucket(_ context.Context) (BucketState, error) {
	st, err := os.Stat(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return BucketState{Exists: true}, nil
		}
		return BucketState{}, err
	}
	if !st.IsDir() {
		return BucketState{}, fmt.Errorf("storage: STORAGE_DIR 不是目录: %s", s.root)
	}
	return BucketState{Exists: true}, nil
}

// Head 只取元信息（删除复核用）；目录不算对象。
func (s *localStore) Head(_ context.Context, key string) (ObjectInfo, error) {
	p, err := s.path(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	st, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return ObjectInfo{}, ErrObjectNotFound
		}
		return ObjectInfo{}, err
	}
	if st.IsDir() {
		return ObjectInfo{}, ErrObjectNotFound
	}
	return ObjectInfo{Size: st.Size(), ModTime: st.ModTime()}, nil
}

// List 按前缀分页列举本地对象（key 为斜杠分隔的相对路径）。
func (s *localStore) List(_ context.Context, prefix, marker string, limit int) (ListPage, error) {
	if limit <= 0 {
		limit = 500
	}
	var keys []string
	err := filepath.WalkDir(s.root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(s.root, p)
		if rerr != nil {
			return nil
		}
		key := filepath.ToSlash(rel)
		if strings.HasPrefix(key, prefix) && key > marker {
			keys = append(keys, key)
		}
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return ListPage{}, nil
		}
		return ListPage{}, err
	}
	sort.Strings(keys)
	page := ListPage{}
	for _, key := range keys {
		if len(page.Objects) >= limit {
			page.Next = page.Objects[len(page.Objects)-1].Key
			break
		}
		p, err := s.path(key)
		if err != nil {
			continue
		}
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		page.Objects = append(page.Objects, ListedObject{Key: key, Size: st.Size(), ModTime: st.ModTime()})
	}
	return page, nil
}

func (s *localStore) path(key string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(key))
	if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return "", fmt.Errorf("%w: %q", ErrInvalidKey, key)
	}
	return filepath.Join(s.root, clean), nil
}

// Put 写入对象：先写 .part 再原子改名，避免半成品被读到。
//
// 长度按 size 截断读取（与 OSS/COS 驱动同构），并校验实际字节数：
// 客户端声明的长度与真实内容不一致时必须失败，否则「上传成功但文件被截断」
// 会一路走到用户手里。
func (s *localStore) Put(ctx context.Context, key string, r io.Reader, size int64, _ string) error {
	if size <= 0 {
		return ErrNeedLength
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, size))
	if err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if n != size {
		os.Remove(tmp)
		return fmt.Errorf("%w: 声明 %d 字节，实际读取 %d 字节", ErrNeedLength, size, n)
	}
	return os.Rename(tmp, p)
}

func (s *localStore) Open(_ context.Context, key string) (io.ReadCloser, int64, error) {
	p, err := s.path(key)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, ErrObjectNotFound
		}
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

func (s *localStore) Delete(_ context.Context, key string) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// OpenRange 按区间打开本地对象（Seek + LimitReader，不读整文件）。
func (s *localStore) OpenRange(_ context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	p, err := s.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrObjectNotFound
		}
		return nil, err
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			return nil, err
		}
	}
	if length <= 0 {
		return f, nil
	}
	return &limitedReadCloser{Reader: io.LimitReader(f, length), Closer: f}, nil
}

// limitedReadCloser 把 LimitReader 与底层句柄的关闭合并（LimitReader 本身没有 Close）。
type limitedReadCloser struct {
	io.Reader
	io.Closer
}
