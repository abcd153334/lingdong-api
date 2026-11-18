package netfetch

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestGetRejectsTooLarge 锁死「超限报错而不是静默截断」。
//
// 截断的产物会被当成完整音频/图片计费：用户拿到半首歌，平台照常收钱，
// 账单和产物各说一套。必须让调用方明确知道超限。
func TestGetRejectsTooLarge(t *testing.T) {
	t.Setenv("LD_UPSTREAM_ALLOW_PRIVATE", "1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, 2048))
	}))
	defer srv.Close()

	if _, _, err := Get(context.Background(), srv.URL, Options{Timeout: 5 * time.Second, MaxBytes: 1024}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("应返回 ErrTooLarge，实际 %v", err)
	}
	// 上限内必须正常取回，且不怕调用方把 MaxBytes 设成 0（取默认值）。
	buf, _, err := Get(context.Background(), srv.URL, Options{Timeout: 5 * time.Second})
	if err != nil || len(buf) != 2048 {
		t.Fatalf("上限内应取回 2048 字节，实际 %d err=%v", len(buf), err)
	}
}

// TestGetStatusError 锁死非 2xx 的归因：调用方要能区分「上游 403」和「网络不通」。
func TestGetStatusError(t *testing.T) {
	t.Setenv("LD_UPSTREAM_ALLOW_PRIVATE", "1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("signature mismatch"))
	}))
	defer srv.Close()

	_, _, err := Get(context.Background(), srv.URL, Options{Timeout: 5 * time.Second})
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusForbidden {
		t.Fatalf("应是 403 的 StatusError，实际 %v", err)
	}
	// 上游错误体不外泄：里面常有内部地址或签名信息。
	if strings.Contains(err.Error(), "signature") {
		t.Fatalf("不应把上游错误体拼进错误文案：%v", err)
	}
}

// TestGetRejectsEmptyURL 锁死空链接：产物链接取错（extract 路径写错）时
// 必须立刻报错，而不是发出一个请求到 "" 再等超时。
func TestGetRejectsEmptyURL(t *testing.T) {
	if _, _, err := Get(context.Background(), "  ", Options{}); err == nil {
		t.Fatal("空链接必须报错")
	}
}

// TestDecodeDataURIContentType 锁死 data URI 的 mime 解析与兜底。
func TestDecodeDataURIContentType(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString([]byte("RIFF....WAVE"))
	buf, ct, err := DecodeDataURI("data:audio/wav;base64,"+payload, "audio/mpeg", 0)
	if err != nil {
		t.Fatalf("不应出错：%v", err)
	}
	if ct != "audio/wav" {
		t.Fatalf("mime 应取 data URI 里的值，实际 %q", ct)
	}
	if string(buf) != "RIFF....WAVE" {
		t.Fatalf("解码内容不符：%q", buf)
	}
	// 没有 mime 时用调用方给的兜底类型。
	if _, ct, err = DecodeDataURI("data:;base64,"+payload, "audio/mpeg", 0); err != nil || ct != "audio/mpeg" {
		t.Fatalf("缺 mime 应回落兜底类型，实际 %q err=%v", ct, err)
	}
}

// TestDecodeDataURIRejectsNonBase64 锁死「只接受 base64」：
// 非 base64 的 data URI 是 URL 编码文本，当音频写回客户端只会是乱码。
func TestDecodeDataURIRejectsNonBase64(t *testing.T) {
	if _, _, err := DecodeDataURI("data:audio/wav,hello", "", 0); err == nil {
		t.Fatal("非 base64 的 data URI 必须被拒绝")
	}
	if _, _, err := DecodeDataURI("data:audio/wav", "", 0); err == nil {
		t.Fatal("缺逗号的 data URI 必须被拒绝")
	}
}

// TestDecodeDataURITooLarge 锁死体积上限在**解码之前**生效。
//
// 解完再判会先申请出超限内存：一个 100 MB 的内联产物能把进程打爆。
func TestDecodeDataURITooLarge(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString(make([]byte, 64<<10))
	if _, _, err := DecodeDataURI("data:audio/wav;base64,"+payload, "", 1024); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("应返回 ErrTooLarge，实际 %v", err)
	}
}
