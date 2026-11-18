package middleware

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/pkg/uploadcfg"
)

// 本文件守住 P0-2：`/v1/*` 的请求体门必须由**上传配置**推导。
//
// 改造前门写死 25 MiB，而配置里的全局上限默认 50 MiB ——
// 结果是「运营配 50MB，API 侧只能收 25MB，门户却能收 50MB」，
// 超限还表现为没头没尾的读错误，而不是 413。

// readThroughBodyLimit 构造一个挂了 BodyLimit 的 gin 服务，把 body 读进内存
// 后返回（读取错误原样带回，供断言 MaxBytesError）。
func readThroughBodyLimit(t *testing.T, path, ctype string, body []byte) (int64, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(BodyLimit())
	// 用 NoRoute 挂读取逻辑：让测试能覆盖任意真实路径（/v1/images/edits 等），
	// 不必为每条路径注册路由（BodyLimit 只看路径前缀，与路由表无关）。
	var n int64
	var readErr error
	r.NoRoute(func(c *gin.Context) {
		var b bytes.Buffer
		_, readErr = io.Copy(&b, c.Request.Body)
		n = int64(b.Len())
	})
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	r.ServeHTTP(httptest.NewRecorder(), req)
	return n, readErr
}

// withUploadsConfig 临时替换 uploads 配置，并让请求体门缓存立即失效。
func withUploadsConfig(t *testing.T, limits map[string]int64) {
	t.Helper()
	prev := conf.System().All()
	t.Cleanup(func() {
		conf.System().Reload(prev)
		uploadcfg.ResetCapCache()
	})
	var s = uploadcfg.Defaults()
	s.Limits = limits
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("构造配置失败: %v", err)
	}
	conf.System().Set("uploads", raw)
	uploadcfg.ResetCapCache()
}

func TestBodyLimitRelayGateAdmitsMultipartRange(t *testing.T) {
	// 17 MiB multipart：旧实现（16 MiB 门）在这里就 MaxBytesError。
	body := make([]byte, (17<<20)+1)
	n, err := readThroughBodyLimit(t, "/v1/images/edits", "multipart/form-data; boundary=b", body)
	if err != nil {
		t.Fatalf("17 MiB multipart 应通过 /v1 门到达 handler：%v", err)
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		t.Fatal("不应出现 MaxBytesError：门必须 ≥ 配置推导出的上限")
	}
	if n != int64(len(body)) {
		t.Fatalf("body 应完整：读 %d / 发 %d", n, len(body))
	}
}

// TestBodyLimitFollowsUploadsConfig 是本次整改的核心断言：
// 门跟着配置走 —— 把视频类调到 3MB，5MB 的请求必须被拦、2MB 必须放过。
func TestBodyLimitFollowsUploadsConfig(t *testing.T) {
	withUploadsConfig(t, map[string]int64{
		uploadcfg.KindImage: 2, uploadcfg.KindAudio: 2, uploadcfg.KindVideo: 3,
		uploadcfg.KindDocument: 2, uploadcfg.KindText: 1,
	})

	small := make([]byte, 2<<20)
	if _, err := readThroughBodyLimit(t, "/v1/files", "multipart/form-data; boundary=b", small); err != nil {
		t.Fatalf("2 MiB 应放行（门 = 3MB + 1MB 开销）：%v", err)
	}

	big := make([]byte, 5<<20)
	_, err := readThroughBodyLimit(t, "/v1/files", "multipart/form-data; boundary=b", big)
	var mbe *http.MaxBytesError
	if !errors.As(err, &mbe) {
		t.Fatalf("5 MiB 应被门拦下（MaxBytesError），实际 err=%v", err)
	}
}

func TestBodyLimitPortalJSONUnchanged(t *testing.T) {
	// 门户 JSON 门 1 MiB 语义不变：2 MiB JSON 必须被拦。
	body := []byte(`{"pad":"` + strings.Repeat("x", 2<<20) + `"}`)
	_, err := readThroughBodyLimit(t, "/api/portal/other", "application/json", body)
	var mbe *http.MaxBytesError
	if !errors.As(err, &mbe) {
		t.Fatalf("门户 2 MiB JSON 应被 1 MiB 门拦下，实际 err=%v", err)
	}
}

// TestBodyLimitDefaultTierBounded 守住 C2：非 /v1、非 JSON、非 multipart 的
// 请求体以前**完全不设上限**（把 Content-Type 写成 text/plain 就绕开了 1 MiB 门）。
// 现在必须落到上传总上限这一档：既不无限，也不把 multipart 上传压到 1 MiB。
func TestBodyLimitDefaultTierBounded(t *testing.T) {
	withUploadsConfig(t, map[string]int64{
		uploadcfg.KindImage: 2, uploadcfg.KindAudio: 2, uploadcfg.KindVideo: 3,
		uploadcfg.KindDocument: 2, uploadcfg.KindText: 1,
	})

	// 2 MiB text/plain 必须放行：这一档是「上传总上限」，不是门户 JSON 的 1 MiB。
	small := make([]byte, 2<<20)
	if _, err := readThroughBodyLimit(t, "/api/portal/other", "text/plain", small); err != nil {
		t.Fatalf("2 MiB text/plain 应放行（档位 = 上传总上限）：%v", err)
	}

	// 5 MiB 必须被拦：以前这里是无限的。
	big := make([]byte, 5<<20)
	_, err := readThroughBodyLimit(t, "/api/portal/other", "text/plain", big)
	var mbe *http.MaxBytesError
	if !errors.As(err, &mbe) {
		t.Fatalf("5 MiB text/plain 应被门拦下（MaxBytesError），实际 err=%v", err)
	}

	// 完全没有 Content-Type 的请求也要被同一档兜住（curl -d 不带 header 就长这样）。
	_, err = readThroughBodyLimit(t, "/api/portal/other", "", big)
	if !errors.As(err, &mbe) {
		t.Fatalf("5 MiB 无 Content-Type 也应被拦下，实际 err=%v", err)
	}
}
