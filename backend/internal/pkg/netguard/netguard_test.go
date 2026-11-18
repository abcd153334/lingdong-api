package netguard

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidateIPPolicy(t *testing.T) {
	allow := Policy{AllowPrivate: true, AllowLoopback: true}
	cases := []struct {
		ip      string
		blocked bool
		why     string
		// always 表示即使 AllowPrivate/AllowLoopback 也必须拒绝（未指定/组播地址无可连接语义）。
		always bool
	}{
		{"127.0.0.1", true, "回环默认拒绝", false},
		{"10.1.2.3", true, "私网默认拒绝", false},
		{"192.168.1.1", true, "私网默认拒绝", false},
		{"169.254.169.254", true, "云元数据地址", false},
		{"0.0.0.0", true, "未指定地址", true},
		{"224.0.0.1", true, "组播", true},
		{"8.8.8.8", false, "公网放行", false},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		err := (Policy{}).ValidateIP(ip)
		if c.blocked && err == nil {
			t.Errorf("%s 应被拒绝（%s）", c.ip, c.why)
		}
		if !c.blocked && err != nil {
			t.Errorf("%s 应被放行（%s），实际 %v", c.ip, c.why, err)
		}
		// 显式放行策略下，私网与回环放行；未指定/组播始终拒绝
		if err := allow.ValidateIP(ip); c.always {
			if err == nil {
				t.Errorf("%s 即使开启放行策略也必须拒绝", c.ip)
			}
		} else if err != nil {
			t.Errorf("%s 在 AllowPrivate/AllowLoopback 下应放行，实际 %v", c.ip, err)
		}
	}
}

func TestValidateURLLiterals(t *testing.T) {
	blocked := []string{
		"ftp://example.com/x",
		"http://localhost/x",
		"http://backend.internal/x",
		"http://printer.local/x",
		"http://127.0.0.1/x",
		"http://user:pass@example.com/x", // 内嵌凭据
		"http:///x",                      // 缺 host
		"",
	}
	for _, raw := range blocked {
		if err := ValidateURL(raw, Policy{}); err == nil {
			t.Errorf("%q 应被拒绝", raw)
		}
	}
	if err := ValidateURL("https://api.example.com/v1", Policy{}); err != nil {
		t.Errorf("正常公网地址应放行，实际 %v", err)
	}
	// 私网开关打开后允许字面量私网
	if err := ValidateURL("http://10.0.0.5:8000/v1", Policy{AllowPrivate: true, AllowLoopback: true}); err != nil {
		t.Errorf("开启私网策略后应放行，实际 %v", err)
	}
}

func TestValidateURLResolvedRejectsLoopbackHost(t *testing.T) {
	// localhost 应在字面量阶段就拒绝，不会触发解析
	if err := ValidateURLResolved(context.Background(), "http://localhost:8080/x", Policy{}); err == nil {
		t.Fatal("localhost 应被拒绝")
	}
}

func TestClientDoesNotFollowRedirect(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/a" {
			http.Redirect(w, r, "/b", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// 测试服务在 127.0.0.1 上，需显式放行回环；重定向策略与 IP 策略无关，必须仍然禁跳。
	c := Client(3*time.Second, Policy{AllowPrivate: true, AllowLoopback: true})
	resp, err := c.Get(srv.URL + "/a")
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("应返回 302 而不跟随，实际 %d", resp.StatusCode)
	}
	if hits != 1 {
		t.Fatalf("不应产生第二跳请求，实际命中 %d 次", hits)
	}
}

func TestClientBlocksLoopbackByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := Client(2*time.Second, Policy{})
	if _, err := c.Get(srv.URL); err == nil {
		t.Fatal("默认策略下不应连上回环地址")
	}
}

func TestTransportReusedPerPolicy(t *testing.T) {
	a := Client(5*time.Second, Policy{})
	b := Client(5*time.Second, Policy{})
	if a.Transport != b.Transport {
		t.Fatal("同超时同策略应复用 Transport（避免每请求新建连接池）")
	}
	priv := Client(5*time.Second, Policy{AllowPrivate: true})
	if a.Transport == priv.Transport {
		t.Fatal("不同策略不得复用 Transport")
	}
}

func TestPolicyFromEnv(t *testing.T) {
	if p := PolicyFromEnv(); p.AllowPrivate || p.AllowLoopback {
		t.Fatal("未设置环境变量时应为默认拒绝")
	}
	t.Setenv("LD_UPSTREAM_ALLOW_PRIVATE", "1")
	p := PolicyFromEnv()
	if !p.AllowPrivate || !p.AllowLoopback {
		t.Fatal("LD_UPSTREAM_ALLOW_PRIVATE=1 应同时放行私网与回环")
	}
	t.Setenv("LD_UPSTREAM_ALLOW_PRIVATE", "0")
	if p := PolicyFromEnv(); p.AllowPrivate {
		t.Fatal("显式 0 应保持拒绝")
	}
}

func TestErrBlockedMessage(t *testing.T) {
	if !strings.Contains(ErrBlocked.Error(), "netguard") {
		t.Fatalf("错误信息应可定位来源，实际 %q", ErrBlocked.Error())
	}
}
