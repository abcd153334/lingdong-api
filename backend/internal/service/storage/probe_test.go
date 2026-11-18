package storage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestProbeChannelRequiresAnonymousRead 守住「保存时就验证桶是公开读」。
//
// 背景：对外地址是不带签名的永久直链。桶若私有，我们带签名能读、浏览器打开是 403，
// 而配置页只会显示「保存成功」—— 这正是本次事故里最难排查的形态。
// 探针必须用**匿名 GET** 读回，而不是复用带签名的客户端。
func TestProbeChannelRequiresAnonymousRead(t *testing.T) {
	// netguard 默认拒绝回环：本用例用一个真实 HTTP 桩，必须显式放行私网。
	t.Setenv("LD_UPSTREAM_ALLOW_PRIVATE", "1")

	var mu sync.Mutex
	objects := map[string][]byte{}
	publicRead := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Path
		switch r.Method {
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			objects[key] = b
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			if !publicRead {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			mu.Lock()
			b, ok := objects[key]
			mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(b)
		case http.MethodDelete:
			mu.Lock()
			delete(objects, key)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	// path_style 让地址形如 {endpoint}/{bucket}/{key}：虚拟主机风格对 IP 端点无解。
	ch := map[string]interface{}{
		"endpoint": srv.URL, "bucket": "ld", "access_key": "ak", "secret_key": "sk",
		"path_style": "true",
	}
	ctx := context.Background()

	// ① 私有读：探针必须失败，并明确指向「桶需公开读」。
	_, err := ProbeChannel(ctx, "s3", ch)
	if err == nil {
		t.Fatal("私有读桶应判探针失败")
	}
	if !strings.Contains(err.Error(), "公开读") {
		t.Fatalf("错误文案应提示桶需公开读，实际: %v", err)
	}
	// 读回失败也必须把探针对象删掉，否则桶里会留下 _healthcheck 垃圾。
	mu.Lock()
	left := len(objects)
	mu.Unlock()
	if left != 0 {
		t.Fatalf("探针失败后仍残留对象 %d 个", left)
	}

	// ② 公开读：写 → 匿名读回比对 → 删除，全链路通过且不留对象。
	publicRead = true
	name, err := ProbeChannel(ctx, "s3", ch)
	if err != nil {
		t.Fatalf("公开读桶应通过，实际: %v", err)
	}
	if name != "s3" {
		t.Fatalf("返回的后端名不符: %q", name)
	}
	mu.Lock()
	left = len(objects)
	mu.Unlock()
	if left != 0 {
		t.Fatalf("探针对象未清理，残留 %d 个", left)
	}
}
