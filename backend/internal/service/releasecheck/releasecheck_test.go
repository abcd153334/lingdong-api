package releasecheck

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"lingdong-api/internal/conf"
)

// setManifestURL 写配置并在用例结束后清空（conf.System 是进程级单例）。
func setManifestURL(t *testing.T, url string) {
	t.Helper()
	raw, err := json.Marshal(url)
	if err != nil {
		t.Fatal(err)
	}
	conf.System().Set(ConfigKey, raw)
	t.Cleanup(func() {
		conf.System().Reload(map[string]json.RawMessage{})
	})
}

// newTestService 返回一个把 SSRF 校验替换为空实现的实例。
//
// 为什么必须替换：httptest 监听 127.0.0.1，会被公网策略拒掉 —— 那是**正确**行为，
// 但会让解析/限流/缓存这些逻辑完全不可测。校验本身由 pkg/ssrf 的用例守住。
func newTestService(srv *httptest.Server) *Service {
	s := New()
	s.client = srv.Client()
	s.validate = func(context.Context, string) error { return nil }
	return s
}

func TestCheckParsesManifest(t *testing.T) {
	var hits int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("应声明 Accept: application/json，实际 %q", got)
		}
		_, _ = w.Write([]byte(`{"schema":1,"latest":{"version":"v2.2.0","published_at":"2026-11-18","notes":"修了一处计费"}}`))
	}))
	defer srv.Close()
	setManifestURL(t, srv.URL)

	s := newTestService(srv)
	st := s.Check(context.Background(), true)
	if !st.OK() {
		t.Fatalf("应检查成功，实际 kind=%q", st.ErrorKind)
	}
	if st.Latest.Version != "v2.2.0" {
		t.Fatalf("版本解析错：%+v", st.Latest)
	}
	// tag 缺省 = version，这是清单最常见的写法，不能要求发布者重复写两遍。
	if st.Latest.Tag != "v2.2.0" {
		t.Fatalf("tag 缺省应等于 version，实际 %q", st.Latest.Tag)
	}

	// 冷却期内再点一次：不许再出站。
	st2 := s.Check(context.Background(), false)
	if !st2.OK() {
		t.Fatalf("冷却期内应复用缓存，实际 kind=%q", st2.ErrorKind)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("15s 冷却内只应出站一次，实际 %d 次", n)
	}
}

func TestStatusNeverFetches(t *testing.T) {
	var hits int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"schema":1,"latest":{"version":"v2.2.0"}}`))
	}))
	defer srv.Close()
	setManifestURL(t, srv.URL)

	s := newTestService(srv)
	st := s.Status()
	if !st.Configured {
		t.Fatal("已配置时应返回 Configured=true")
	}
	if st.ErrorKind != KindNone || st.Latest != nil {
		t.Fatalf("未检查过时不应有结果，实际 %+v", st)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("Status 绝不能发起请求，实际 %d 次", n)
	}
}

// TestURLChangeInvalidatesCache 守住"改了地址还显示旧地址的结果"。
//
// 这个 bug 的后果很具体：地址从 A 改成 B，界面还显示 A 的版本，
// 管理员据此点更新 → 更新到一个 B 里不存在的 tag。
func TestURLChangeInvalidatesCache(t *testing.T) {
	body := func(v string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"schema":1,"latest":{"version":"` + v + `"}}`))
		}
	}
	srvA := httptest.NewTLSServer(body("v2.2.0"))
	defer srvA.Close()
	srvB := httptest.NewTLSServer(body("v2.3.0"))
	defer srvB.Close()

	setManifestURL(t, srvA.URL)
	s := newTestService(srvA)
	if st := s.Check(context.Background(), true); st.Latest == nil || st.Latest.Version != "v2.2.0" {
		t.Fatalf("A 应返回 v2.2.0，实际 %+v", st)
	}

	setManifestURL(t, srvB.URL)
	// 换地址后 Status 必须回到"已配置、未检查"，不能继续回显 A 的结果。
	if st := s.Status(); st.Latest != nil {
		t.Fatalf("换地址后旧结果必须作废，实际 %+v", st.Latest)
	}
	s.client = srvB.Client()
	if st := s.Check(context.Background(), true); st.Latest == nil || st.Latest.Version != "v2.3.0" {
		t.Fatalf("B 应返回 v2.3.0，实际 %+v", st)
	}
}

func TestCheckRejects(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		status int
		kind   string
	}{
		{"schema 不是 1", `{"schema":2,"latest":{"version":"v2.2.0"}}`, 200, KindUnsupportedSchema},
		{"缺 latest", `{"schema":1}`, 200, KindBadJSON},
		{"版本非法", `{"schema":1,"latest":{"version":"latest"}}`, 200, KindInvalidVersion},
		{"tag 非法", `{"schema":1,"latest":{"version":"v2.2.0","tag":"v2.2.0;ls"}}`, 200, KindInvalidVersion},
		{"commit 非法", `{"schema":1,"latest":{"version":"v2.2.0","commit":"not-a-sha"}}`, 200, KindInvalidField},
		{"url 非 https", `{"schema":1,"latest":{"version":"v2.2.0","url":"javascript:alert(1)"}}`, 200, KindInvalidField},
		{"不是 JSON", `not json`, 200, KindBadJSON},
		{"HTTP 500", `{}`, 500, KindHTTPStatus},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			setManifestURL(t, srv.URL)
			s := newTestService(srv)
			st := s.Check(context.Background(), true)
			if st.ErrorKind != c.kind {
				t.Fatalf("期望 %q，实际 %q（body=%s）", c.kind, st.ErrorKind, c.body)
			}
			if st.OK() {
				t.Fatal("失败结果不能 OK()")
			}
		})
	}
}

// TestCheckRejectsNonHTTPS 守住"清单必须走 https"。
//
// 明文链路可以被中间人替换成"最新版本 v9.9.9"，而管理台会把这条信息
// 当作升级建议展示 —— 这就是一条把人骗去执行任意 tag 的路径。
func TestCheckRejectsNonHTTPS(t *testing.T) {
	setManifestURL(t, "http://example.com/release.json")
	s := New()
	st := s.Check(context.Background(), true)
	if st.ErrorKind != KindBlocked {
		t.Fatalf("http 地址必须被判 blocked，实际 %q", st.ErrorKind)
	}
}

func TestCheckRejectsOversize(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"schema":1,"latest":{"version":"v2.2.0","notes":"`))
		_, _ = w.Write([]byte(strings.Repeat("x", maxBytes)))
		_, _ = w.Write([]byte(`"}}`))
	}))
	defer srv.Close()
	setManifestURL(t, srv.URL)
	s := newTestService(srv)
	if st := s.Check(context.Background(), true); st.ErrorKind != KindTooLarge {
		t.Fatalf("超大清单应判 too_large，实际 %q", st.ErrorKind)
	}
}

func TestNotConfigured(t *testing.T) {
	// 语义在 v2.2.7 变了：空串不再是"未配置"，而是"用出厂默认清单"——
	// 否则每个克隆部署的实例都得先去管理台填地址才可能收到更新通知。
	// 要关闭检查必须显式填 off，这两者必须能区分。
	setManifestURL(t, OffValue)
	s := New()
	st := s.Status()
	if st.Configured || st.ErrorKind != KindNotConfigured {
		t.Fatalf("off 应显式标 not_configured，实际 %+v", st)
	}
	if st2 := s.Check(context.Background(), true); st2.Configured {
		t.Fatal("关闭检查时 Check 不应尝试出站")
	}
}

// TestEmptyFallsBackToDefault 守着"出厂即可检查更新"这条承诺。
func TestEmptyFallsBackToDefault(t *testing.T) {
	setManifestURL(t, "")
	if got := ManifestURL(); got != DefaultManifestURL {
		t.Fatalf("空串应回落到出厂清单 %q，实际 %q", DefaultManifestURL, got)
	}
	setManifestURL(t, "  "+OffValue+"  ")
	if got := ManifestURL(); got != "" {
		t.Fatalf("off（含空白）应关闭检查，实际 %q", got)
	}
	setManifestURL(t, "https://example.com/r.json")
	if got := ManifestURL(); got != "https://example.com/r.json" {
		t.Fatalf("显式地址应原样生效，实际 %q", got)
	}
}
