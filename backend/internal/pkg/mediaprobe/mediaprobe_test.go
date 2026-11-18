package mediaprobe

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"lingdong-api/internal/pkg/netguard"
)

// TestMain 打开「允许回环」：本包用例全部指向 httptest 起的 127.0.0.1 服务。
//
// 用包内变量而不是 LD_UPSTREAM_ALLOW_PRIVATE：后者是给渠道 base_url 的开关，
// 本包已刻意不再读它（否则用户可控 URL 能借它打内网）。测试要指向回环，
// 只能从这里换策略，生产路径永远是零值（拒绝私网/回环）。
func TestMain(m *testing.M) {
	probePolicy = netguard.Policy{AllowPrivate: true, AllowLoopback: true}
	os.Exit(m.Run())
}

// ---- mp4 构造 ----

func box(typ string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out, uint32(8+len(payload)))
	copy(out[4:8], typ)
	copy(out[8:], payload)
	return out
}

// mvhdV0 构造 mvhd（version 0）：4+4+4+4 = creation/modification/timescale/duration。
func mvhdV0(timescale, duration uint32) []byte {
	p := make([]byte, 20)
	p[0] = 0 // version
	binary.BigEndian.PutUint32(p[12:], timescale)
	binary.BigEndian.PutUint32(p[16:], duration)
	return box("mvhd", p)
}

// mvhdV1 构造 mvhd（version 1）：8+8+4+8。
func mvhdV1(timescale uint32, duration uint64) []byte {
	p := make([]byte, 32)
	p[0] = 1
	binary.BigEndian.PutUint32(p[20:], timescale)
	binary.BigEndian.PutUint64(p[24:], duration)
	return box("mvhd", p)
}

// mehdV0 构造 mvex/mehd（fragmented mp4 的总时长）。
func mehdV0(duration uint32) []byte {
	p := make([]byte, 8)
	p[0] = 0
	binary.BigEndian.PutUint32(p[4:], duration)
	return box("mvex", box("mehd", p))
}

func ftyp() []byte {
	return box("ftyp", []byte("isom\x00\x00\x00\x00isomiso2"))
}

func mdat(n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte(i % 251)
	}
	return box("mdat", p)
}

// ---- 分段服务 ----

type rangeServerOpts struct {
	// ignoreRange 模拟不支��� Range 的服务器（永远 200 全量）。
	ignoreRange bool
	// redirect 非空时，首次请求 302 到该路径。
	redirect string
}

func newRangeServer(t *testing.T, data []byte, opts rangeServerOpts) (*httptest.Server, *int64) {
	t.Helper()
	var hits int64
	mux := http.NewServeMux()
	mux.HandleFunc("/v.mp4", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		if opts.redirect != "" && r.URL.Path == "/v.mp4" {
			http.Redirect(w, r, opts.redirect, http.StatusFound)
			return
		}
		serveRange(w, r, data, opts.ignoreRange)
	})
	mux.HandleFunc("/moved.mp4", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		serveRange(w, r, data, opts.ignoreRange)
	})
	mux.HandleFunc("/missing.mp4", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

func serveRange(w http.ResponseWriter, r *http.Request, data []byte, ignoreRange bool) {
	total := int64(len(data))
	rng := r.Header.Get("Range")
	if ignoreRange || rng == "" {
		w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}
	spec := strings.TrimPrefix(rng, "bytes=")
	var start, end int64
	if strings.HasPrefix(spec, "-") {
		n, err := strconv.ParseInt(strings.TrimPrefix(spec, "-"), 10, 64)
		if err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		start = total - n
		if start < 0 {
			start = 0
		}
		end = total - 1
	} else {
		parts := strings.SplitN(spec, "-", 2)
		s, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		start = s
		end = total - 1
		if len(parts) == 2 && parts[1] != "" {
			e, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				http.Error(w, "bad range", http.StatusBadRequest)
				return
			}
			if e < end {
				end = e
			}
		}
	}
	if start >= total {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	chunk := data[start : end+1]
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
	w.Header().Set("Content-Length", strconv.Itoa(len(chunk)))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(chunk)
}

// ---- 用例 ----

// TestDurationMoovAtHead：ffmpeg faststart（moov 在前）。
func TestDurationMoovAtHead(t *testing.T) {
	data := append(append(append([]byte{}, ftyp()...), box("moov", mvhdV0(1000, 10500))...), mdat(64)...)
	srv, _ := newRangeServer(t, data, rangeServerOpts{})
	got, err := Duration(context.Background(), srv.URL+"/v.mp4")
	if err != nil || got != 11 { // 10.5s → 向上取整 11
		t.Fatalf("want 11/nil, got %d/%v", got, err)
	}
}

// TestDurationMoovAtTail：moov 在 mdat 之后（默认 ffmpeg 输出），必须走尾段回扫。
func TestDurationMoovAtTail(t *testing.T) {
	data := append(append(append([]byte{}, ftyp()...), mdat(300<<10)...), box("moov", mvhdV0(90000, 90000*7))...)
	srv, hits := newRangeServer(t, data, rangeServerOpts{})
	got, err := Duration(context.Background(), srv.URL+"/v.mp4")
	if err != nil || got != 7 {
		t.Fatalf("want 7/nil, got %d/%v", got, err)
	}
	if n := atomic.LoadInt64(hits); n < 2 {
		t.Fatalf("moov 在尾部应至少请求两次（首段+尾段），实际 %d 次", n)
	}
}

// TestDurationMoovBeyondHead：moov 起点在首段内但整段跨出（走精确区间请求）。
func TestDurationMoovBeyondHead(t *testing.T) {
	big := make([]byte, 400<<10) // moov 自身 400 KiB > 首段剩余空间
	moovPayload := append(mvhdV0(1000, 42000), big...)
	data := append(append(append([]byte{}, ftyp()...), mdat(900<<10)...), box("moov", moovPayload)...)
	srv, _ := newRangeServer(t, data, rangeServerOpts{})
	got, err := Duration(context.Background(), srv.URL+"/v.mp4")
	if err != nil || got != 42 {
		t.Fatalf("want 42/nil, got %d/%v", got, err)
	}
}

// TestDurationMvhdV1：64 位时长字段。
func TestDurationMvhdV1(t *testing.T) {
	data := append(append([]byte{}, ftyp()...), box("moov", mvhdV1(600, 600*90))...)
	srv, _ := newRangeServer(t, data, rangeServerOpts{})
	got, err := Duration(context.Background(), srv.URL+"/v.mp4")
	if err != nil || got != 90 {
		t.Fatalf("want 90/nil, got %d/%v", got, err)
	}
}

// TestDurationFragmentedMehd：mvhd.duration=0 时回落到 mvex/mehd。
func TestDurationFragmentedMehd(t *testing.T) {
	moov := append(mvhdV0(1000, 0), mehdV0(13000)...)
	data := append(append([]byte{}, ftyp()...), box("moov", moov)...)
	srv, _ := newRangeServer(t, data, rangeServerOpts{})
	got, err := Duration(context.Background(), srv.URL+"/v.mp4")
	if err != nil || got != 13 {
		t.Fatalf("want 13/nil, got %d/%v", got, err)
	}
}

// TestDurationNoMoov：完全没有 moov → 必须报错，不能返回 0。
func TestDurationNoMoov(t *testing.T) {
	data := append(append([]byte{}, ftyp()...), mdat(1000)...)
	srv, _ := newRangeServer(t, data, rangeServerOpts{})
	if got, err := Duration(context.Background(), srv.URL+"/v.mp4"); err == nil || got != 0 {
		t.Fatalf("want error/0, got %d/%v", got, err)
	}
}

// TestDurationFragmentedWithoutMehd：mvhd=0 且无 mehd → 明确报错（时长未知不能按 0 计）。
func TestDurationFragmentedWithoutMehd(t *testing.T) {
	data := append(append([]byte{}, ftyp()...), box("moov", mvhdV0(1000, 0))...)
	srv, _ := newRangeServer(t, data, rangeServerOpts{})
	if _, err := Duration(context.Background(), srv.URL+"/v.mp4"); err == nil {
		t.Fatal("want error, got nil")
	}
}

// TestDurationIgnoresRange：服务器忽略 Range 且文件很大 → ErrUnsupported。
func TestDurationIgnoresRange(t *testing.T) {
	data := append(append(append([]byte{}, ftyp()...), mdat(3<<20)...), box("moov", mvhdV0(1000, 5000))...)
	srv, _ := newRangeServer(t, data, rangeServerOpts{ignoreRange: true})
	_, err := Duration(context.Background(), srv.URL+"/v.mp4")
	if err == nil || !strings.Contains(err.Error(), "不支持分段下载") {
		t.Fatalf("want ErrUnsupported 文案, got %v", err)
	}
}

// TestDurationRedirect：CDN 302 是常态，必须跟随（禁重定向的 Client 会一律失败）。
func TestDurationRedirect(t *testing.T) {
	data := append(append([]byte{}, ftyp()...), box("moov", mvhdV0(1000, 3000))...)
	srv, _ := newRangeServer(t, data, rangeServerOpts{redirect: "/moved.mp4"})
	got, err := Duration(context.Background(), srv.URL+"/v.mp4")
	if err != nil || got != 3 {
		t.Fatalf("want 3/nil, got %d/%v", got, err)
	}
}

// TestDurationHTTPError：404 → 文案含状态码，不返回 0。
func TestDurationHTTPError(t *testing.T) {
	srv, _ := newRangeServer(t, []byte("x"), rangeServerOpts{})
	_, err := Duration(context.Background(), srv.URL+"/missing.mp4")
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("want 404 文案, got %v", err)
	}
}

// TestDurationRejectsBadInput：内联 data: / 非 http 协议 / 空串一律拒绝，且不产生任何出站。
func TestDurationRejectsBadInput(t *testing.T) {
	cases := []string{
		"data:video/mp4;base64,AAAA",
		"file:///etc/passwd",
		"",
		"not a url",
	}
	for _, c := range cases {
		if _, err := Duration(context.Background(), c); err == nil {
			t.Fatalf("%q 应被拒绝", c)
		}
	}
}

// TestBatchDedupAndConcurrency：同 URL 只探测一次，结果按原 URL 返回。
func TestBatchDedupAndConcurrency(t *testing.T) {
	data := append(append([]byte{}, ftyp()...), box("moov", mvhdV0(1000, 4000))...)
	srv, hits := newRangeServer(t, data, rangeServerOpts{})
	u := srv.URL + "/v.mp4"
	got, err := Batch(context.Background(), []string{u, u, ""})
	if err != nil {
		t.Fatalf("Batch err: %v", err)
	}
	if got[u] != 4 || len(got) != 1 {
		t.Fatalf("want {%s:4}, got %v", u, got)
	}
	if n := atomic.LoadInt64(hits); n != 1 {
		t.Fatalf("去重后应只请求 1 次，实际 %d", n)
	}
}

// TestBatchFailFast：任一素材读不到时长，整批失败（不能给部分结果让调用方按 0 计费）。
func TestBatchFailFast(t *testing.T) {
	data := append(append([]byte{}, ftyp()...), box("moov", mvhdV0(1000, 4000))...)
	srv, _ := newRangeServer(t, data, rangeServerOpts{})
	_, err := Batch(context.Background(), []string{srv.URL + "/v.mp4", srv.URL + "/missing.mp4"})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "参考视频") {
		t.Fatalf("错误文案应指明是参考视频：%v", err)
	}
}

// TestBatchEmpty：空输入返回空 map，不报错（纯文生视频路径）。
func TestBatchEmpty(t *testing.T) {
	got, err := Batch(context.Background(), []string{"", "  "})
	if err != nil || len(got) != 0 {
		t.Fatalf("want empty/nil, got %v/%v", got, err)
	}
}

// TestParseContentRange 覆盖各种 Content-Range 写法。
func TestParseContentRange(t *testing.T) {
	cases := []struct {
		in    string
		start int64
		total int64
	}{
		{"bytes 0-1023/4096", 0, 4096},
		{"bytes 2048-4095/4096", 2048, 4096},
		{"bytes */4096", -1, 4096},
		{"bytes 0-1023/*", 0, -1},
		{"garbage", -1, -1},
	}
	for _, c := range cases {
		s, tot := parseContentRange(c.in)
		if s != c.start || tot != c.total {
			t.Fatalf("%q → want (%d,%d), got (%d,%d)", c.in, c.start, c.total, s, tot)
		}
	}
}

// TestMaskURL 确认签名参数不会出现在错误文案里。
func TestMaskURL(t *testing.T) {
	got := MaskURL("https://u:p@cdn.example.com/a/b.mp4?sign=secret#frag")
	if strings.Contains(got, "secret") || strings.Contains(got, "frag") || strings.Contains(got, "u:p@") {
		t.Fatalf("MaskURL 泄露了敏感部分：%s", got)
	}
}
