package storage

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"lingdong-api/internal/conf"
)

func urlValues(m map[string][]string) url.Values {
	v := url.Values{}
	for k, vals := range m {
		for _, one := range vals {
			v.Add(k, one)
		}
	}
	return v
}

// s3Stub 是一个可编排的 S3 兼容桩：只实现本用例组需要的
// PUT / GET / HEAD / DELETE 与 ?versioning。
//
// 为什么不用真实云：本组用例要覆盖「删除谎报成功」「桶名写错」这类
// 真实后端**不可能制造**的故障形态；真实云的删除是强一致的，测不出来。
type s3Stub struct {
	mu        sync.Mutex
	objects   map[string][]byte
	lastPath  string
	bucketOK  bool
	versioned bool
	// deleteNoop 模拟「DELETE 返回 204 但对象还在」（网关/策略/版本控制的合成形态）。
	deleteNoop bool
	// deleteStatus 非 0 时 DELETE 直接回该状态码（对象不动），模拟删错桶。
	deleteStatus int
}

func newS3Stub(t *testing.T) (*s3Stub, *httptest.Server) {
	t.Helper()
	t.Setenv("LD_UPSTREAM_ALLOW_PRIVATE", "1")
	st := &s3Stub{objects: map[string][]byte{}, bucketOK: true}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		st.lastPath = r.URL.Path

		// 桶级请求：path-style 下是 /{bucket} 或 /{bucket}/ 且带 ?versioning
		if r.URL.Query().Has("versioning") {
			w.Header().Set("Content-Type", "application/xml")
			status := "Suspended"
			if st.versioned {
				status = "Enabled"
			}
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><VersioningConfiguration><Status>`+status+`</Status></VersioningConfiguration>`)
			return
		}
		isBucketPath := strings.Count(strings.Trim(r.URL.Path, "/"), "/") == 0
		if isBucketPath {
			if !st.bucketOK {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
			return
		}

		key := r.URL.Path
		switch r.Method {
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			st.objects[key] = b
			w.WriteHeader(http.StatusOK)
		case http.MethodHead:
			b, ok := st.objects[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Length", itoa(len(b)))
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			b, ok := st.objects[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(b)
		case http.MethodDelete:
			if st.deleteStatus != 0 {
				w.WriteHeader(st.deleteStatus)
				return
			}
			if !st.deleteNoop {
				delete(st.objects, key)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return st, srv
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// withStorageConfig 把 storage 配置换成给定内容（用例结束恢复）。
func withStorageConfig(t *testing.T, raw string) {
	t.Helper()
	prev := conf.System().All()
	conf.System().Reload(map[string]json.RawMessage{"storage": json.RawMessage(raw)})
	t.Cleanup(func() { conf.System().Reload(prev) })
}

const stubStorageCfg = `{"active":"s3","channels":{"s3":{"endpoint":"%s","bucket":"cfg-bucket","access_key":"ak","secret_key":"sk","path_style":"true"}}}`

// TestDeleteRefVerifiedOutcomes 覆盖删除复核的四种结局。
//
// 这是本次整改的核心用例组：旧实现只调 Delete 并相信 2xx/404，
// 于是「删错桶」「桶被策略拦」「版本控制」全部表现为「删干净了」。
func TestDeleteRefVerifiedOutcomes(t *testing.T) {
	ctx := context.Background()

	t.Run("正常删除", func(t *testing.T) {
		stub, srv := newS3Stub(t)
		withStorageConfig(t, strings.Replace(stubStorageCfg, "%s", srv.URL, 1))
		ref := Ref{Backend: "s3", Bucket: "b1", Key: "files/1/x/a.png"}
		stub.objects["/b1/"+ref.Key] = []byte("x")

		out, err := DeleteRefVerified(ctx, ref)
		if err != nil || out != DeleteGone {
			t.Fatalf("应确认删除: out=%s err=%v", out, err)
		}
		if _, ok := stub.objects["/b1/"+ref.Key]; ok {
			t.Fatal("对象应已删除")
		}
		// 幂等：再删一次仍是 Gone（复核 == 不存在）。
		out, err = DeleteRefVerified(ctx, ref)
		if err != nil || out != DeleteGone {
			t.Fatalf("重复删除应仍为 Gone: out=%s err=%v", out, err)
		}
	})

	t.Run("DELETE 报成功但对象还在", func(t *testing.T) {
		stub, srv := newS3Stub(t)
		withStorageConfig(t, strings.Replace(stubStorageCfg, "%s", srv.URL, 1))
		stub.deleteNoop = true
		ref := Ref{Backend: "s3", Bucket: "b1", Key: "files/1/x/b.png"}
		stub.objects["/b1/"+ref.Key] = []byte("x")

		out, err := DeleteRefVerified(ctx, ref)
		if out != DeleteUnverified || !errors.Is(err, ErrDeleteUnverified) {
			t.Fatalf("复核仍能读到时必须判未验证: out=%s err=%v", out, err)
		}
		if _, ok := stub.objects["/b1/"+ref.Key]; !ok {
			t.Fatal("桩不应该真删，用例前提被破坏")
		}
	})

	t.Run("DELETE 报 404 但对象还在（换桶事故）", func(t *testing.T) {
		stub, srv := newS3Stub(t)
		withStorageConfig(t, strings.Replace(stubStorageCfg, "%s", srv.URL, 1))
		stub.deleteStatus = http.StatusNotFound
		ref := Ref{Backend: "s3", Bucket: "old-bucket", Key: "files/1/x/c.png"}
		stub.objects["/old-bucket/"+ref.Key] = []byte("x")

		out, err := DeleteRefVerified(ctx, ref)
		if out == DeleteGone {
			t.Fatal("404 不能等于「删掉了」—— 这正是旧实现删行留对象的成因")
		}
		if !errors.Is(err, ErrDeleteUnverified) {
			t.Fatalf("错误应包 ErrDeleteUnverified: %v", err)
		}
	})

	t.Run("桶不存在", func(t *testing.T) {
		stub, srv := newS3Stub(t)
		withStorageConfig(t, strings.Replace(stubStorageCfg, "%s", srv.URL, 1))
		stub.bucketOK = false
		ref := Ref{Backend: "s3", Bucket: "typo-bucket", Key: "files/1/x/d.png"}

		out, err := DeleteRefVerified(ctx, ref)
		if out == DeleteGone {
			t.Fatal("桶不存在不能判删除成功")
		}
		if !errors.Is(err, ErrBackendGone) {
			t.Fatalf("错误应包 ErrBackendGone（配置事故）: %v", err)
		}
	})

	t.Run("桶开启版本控制", func(t *testing.T) {
		stub, srv := newS3Stub(t)
		withStorageConfig(t, strings.Replace(stubStorageCfg, "%s", srv.URL, 1))
		stub.versioned = true
		ref := Ref{Backend: "s3", Bucket: "b1", Key: "files/1/x/e.png"}
		stub.objects["/b1/"+ref.Key] = []byte("x")

		out, err := DeleteRefVerified(ctx, ref)
		if out != DeleteUnverified {
			t.Fatalf("版本控制下删除只产生标记，不能判成功: out=%s", out)
		}
		if err == nil || !strings.Contains(err.Error(), "版本控制") {
			t.Fatalf("错误文案应点明版本控制: %v", err)
		}
	})
}

// TestForRefRoutesByRecordBucket 守住「删除打在记录里的桶上」。
//
// 这是换桶事故的直接防线：配置里的 bucket 改成 B 之后，
// 历史记录仍然要去 A 删；只按 backend 路由会打到 B。
func TestForRefRoutesByRecordBucket(t *testing.T) {
	stub, srv := newS3Stub(t)
	withStorageConfig(t, strings.Replace(stubStorageCfg, "%s", srv.URL, 1))
	ctx := context.Background()

	rec := Ref{Backend: "s3", Bucket: "bucket-a", Key: "files/1/x/f.png"}
	stub.objects["/bucket-a/"+rec.Key] = []byte("x")
	// 新桶里放一个同名 key：如果路由错了，删掉的是它。
	stub.objects["/cfg-bucket/"+rec.Key] = []byte("y")

	if out, err := DeleteRefVerified(ctx, rec); err != nil || out != DeleteGone {
		t.Fatalf("删除失败: out=%s err=%v", out, err)
	}
	if _, ok := stub.objects["/bucket-a/"+rec.Key]; ok {
		t.Fatal("记录所在桶的对象必须被删掉")
	}
	if _, ok := stub.objects["/cfg-bucket/"+rec.Key]; !ok {
		t.Fatal("当前配置桶里的同名对象不应被动到")
	}

	// ForRef 必须接受记录里的桶；缺失桶是写路径 bug，不是「用当前桶」。
	if _, err := ForRef(Ref{Backend: "s3", Key: "files/1/x/g.png"}); !errors.Is(err, ErrBucketMissing) {
		t.Fatalf("缺桶应报 ErrBucketMissing（不得回退当前配置）: %v", err)
	}
	if _, err := ForRef(Ref{Bucket: "b1", Key: "k"}); !errors.Is(err, ErrBackendMissing) {
		t.Fatalf("缺后端应报 ErrBackendMissing: %v", err)
	}
}

// TestCanonicalQuery 守住 SigV4 查询串的编码与排序（List 的前提）。
//
// httptest 桩不验签，所以这里直接测纯函数：排序错、空格编码错（+ 而不是 %20）
// 都会让真实 S3/MinIO 返回 403，而那种失败在本地桩上永远测不出来。
func TestCanonicalQuery(t *testing.T) {
	q := urlValues(map[string][]string{
		"list-type":          {"2"},
		"prefix":             {"files/a b/"},
		"continuation-token": {"x/y+z"},
		"max-keys":           {"500"},
	})
	want := "continuation-token=x%2Fy%2Bz&list-type=2&max-keys=500&prefix=files%2Fa%20b%2F"
	if got := canonicalQuery(q); got != want {
		t.Fatalf("规范化查询串不符:\n got=%s\nwant=%s", got, want)
	}
	if got := canonicalQuery(nil); got != "" {
		t.Fatalf("空查询应为空串，实际 %q", got)
	}
}
