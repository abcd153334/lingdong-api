package netguard

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

type captureTap struct {
	mu        sync.Mutex
	body      []byte
	url       string
	status    int
	truncated bool
	calls     int
}

func (c *captureTap) NoteUpstreamRequest(_ context.Context, _ string, url, _ string, body []byte, truncated bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.url = url
	c.body = body
	c.truncated = truncated
}

func (c *captureTap) NoteUpstreamResponse(_ context.Context, status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = status
}

// TestWrapTapDoesNotConsumeBody 是最重要的一条：
//
// 如果实现读的是 req.Body 而不是 req.GetBody()，上游会收到空体 ——
// 而钱已经冻结，最终表现为「上游莫名 400 + 全额退款」，极难归因。
func TestWrapTapDoesNotConsumeBody(t *testing.T) {
	const payload = `{"model":"gpt-image-2","prompt":"hi"}`
	var upstreamGot string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		upstreamGot = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tap := &captureTap{}
	client := &http.Client{Transport: WrapTap(http.DefaultTransport)}
	req, err := http.NewRequestWithContext(
		WithBodyTap(context.Background(), tap), http.MethodPost, srv.URL+"/v1/images?async=true",
		strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if upstreamGot != payload {
		t.Fatalf("上游收到的 body 被消费了：%q", upstreamGot)
	}
	if string(tap.body) != payload {
		t.Fatalf("tap 未拿到完整出站体：%q", tap.body)
	}
	if tap.status != http.StatusOK {
		t.Fatalf("tap 未记录状态码：%d", tap.status)
	}
	if !strings.Contains(tap.url, "async=true") {
		t.Fatalf("tap 未记录真实 URL：%q", tap.url)
	}
}

// TestWrapTapSkipsStreamingBody GetBody 为空（流式上传）时必须跳过，不能 panic 也不能读 Body。
func TestWrapTapSkipsStreamingBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tap := &captureTap{}
	client := &http.Client{Transport: WrapTap(http.DefaultTransport)}
	// io.LimitReader 让 NewRequest 走 default 分支 → GetBody == nil
	req, err := http.NewRequestWithContext(
		WithBodyTap(context.Background(), tap), http.MethodPut, srv.URL, io.LimitReader(strings.NewReader("abc"), 3))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if tap.calls != 1 {
		t.Fatalf("仍应记录一次尝试（用于标 tap-skipped），实际 %d", tap.calls)
	}
	if len(tap.body) != 0 {
		t.Fatalf("流式体不应被读取：%q", tap.body)
	}
}

// TestWrapTapSkipsBinaryBody 二进制 multipart（图片编辑/音频上传）不能进留痕：
//
// 留存表是 PostgreSQL text 列，写入无效 UTF-8 会让**整条调用记录**插入失败
// （invalid byte sequence for encoding "UTF8"），排障时反而什么都看不到。
// 正确行为与流式体一致：记录一次尝试，body 留空（前端标「未采集」）。
func TestWrapTapSkipsBinaryBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// PNG 魔数 + 非法 UTF-8 字节，模拟 multipart 里的文件内容。
	binary := append([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, 0xff, 0xfe, 0x00, 0x01)

	tap := &captureTap{}
	client := &http.Client{Transport: WrapTap(http.DefaultTransport)}
	req, err := http.NewRequestWithContext(
		WithBodyTap(context.Background(), tap), http.MethodPost, srv.URL+"/v1/images/edits",
		bytes.NewReader(binary))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if tap.calls != 1 {
		t.Fatalf("应记录一次尝试，实际 %d", tap.calls)
	}
	if len(tap.body) != 0 {
		t.Fatalf("二进制体不该进留痕：%q", tap.body)
	}
}

// TestWrapTapNoTapIsTransparent 没有 tap 时（探活/支付/存储）必须零改动直通。
func TestWrapTapNoTapIsTransparent(t *testing.T) {
	const payload = "hello"
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := &http.Client{Transport: WrapTap(http.DefaultTransport)}
	resp, err := client.Post(srv.URL, "text/plain", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got != payload {
		t.Fatalf("无 tap 时请求体应原样到达，实际 %q", got)
	}
}

// TestWrapTapIgnoresBodylessRequests 轮询 GET 不能覆盖提交 POST 的留痕。
//
// 真实场景：同步图像桥接在**同一个 ctx** 上先 POST 提交、再反复 GET 轮询。
// 改造前那次 GET 也会进 tap，把刚记下的 POST 正文覆盖成空 —— 落库时只能标成
// tap-skipped，`upstream_raw` 永久为空、`upstream_url` 变成轮询地址、
// `attempts` 变成轮询次数（实测 8~19 次），排障时看到的是「上游什么都没收到」。
func TestWrapTapIgnoresBodylessRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	const payload = `{"model":"x","prompt":"hi"}`
	tap := &captureTap{}
	ctx := WithBodyTap(context.Background(), tap)
	client := &http.Client{Transport: WrapTap(http.DefaultTransport)}

	post, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/api/gemini/nano-banana",
		strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(post)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// 之后在同一 ctx 上轮询：没有请求体，不得进入留痕。
	get, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/gemini/nano-banana/task-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp2, err := client.Do(get)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()

	if tap.calls != 1 {
		t.Fatalf("轮询 GET 不应进留痕，期望 1 次，实际 %d", tap.calls)
	}
	if string(tap.body) != payload {
		t.Fatalf("留痕体必须是提交 POST 的正文，实际 %q", tap.body)
	}
	if !strings.Contains(tap.url, "nano-banana") || strings.Contains(tap.url, "task-1") {
		t.Fatalf("留痕 URL 必须是提交地址，实际 %q", tap.url)
	}
}

// TestSanitizeTapURL 凭据不能进留痕（部分上游把 key 放 query）。
func TestSanitizeTapURL(t *testing.T) {
	u, err := url.Parse("https://api.example.com/v1/x?api_key=secret&page=2#frag")
	if err != nil {
		t.Fatal(err)
	}
	got := sanitizeTapURL(u)
	if strings.Contains(got, "secret") {
		t.Fatalf("query 里的凭据未脱敏：%s", got)
	}
	if !strings.Contains(got, "page=2") {
		t.Fatalf("普通参数应保留：%s", got)
	}
	if strings.Contains(got, "frag") {
		t.Fatalf("fragment 不该出现在留痕里：%s", got)
	}
}
