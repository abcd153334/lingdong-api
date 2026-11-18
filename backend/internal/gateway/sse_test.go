package gateway

import (
	"bytes"
	"strings"
	"testing"
)

// TestFrameIsSSEShaped 守 SSE 帧的**字节形状**。
//
// 这条此前完全没有断言（writeFrame 是 40 行手写拼串）。形状错的失败模式很隐蔽：
// 少一个空行 ⇒ 客户端把两帧粘成一帧（表现为"最后一个字丢了"）；
// `data:` 后多一个空格以外的空白 ⇒ 某些 SDK 把前导空格算进 JSON（解析失败）；
// 没有 `event:` 行 ⇒ 按事件名分流的客户端全部走 default 分支。
func TestFrameIsSSEShaped(t *testing.T) {
	got := string(Frame("delta", []byte(`{"x":1}`)))
	want := "event: delta\ndata: {\"x\":1}\n\n"
	if got != want {
		t.Fatalf("帧形状不符\n got=%q\nwant=%q", got, want)
	}

	// 无事件名：只写 data 行（OpenAI 的默认帧）。
	if got := string(Frame("", []byte(`[DONE]`))); got != "data: [DONE]\n\n" {
		t.Fatalf("无事件名的帧不符: %q", got)
	}

	// 两者都空：仍然是**一个**合法的空帧（两个换行），不是空字符串。
	// 空字符串会让上层误判"没写出去"，从而重复写一次心跳。
	if got := string(Frame("", nil)); got != "\n\n" {
		t.Fatalf("空帧应为两个换行，实际 %q", got)
	}
}

// TestStreamHeadersBlockBuffering 守 SSE 的三个必要响应头。
//
// 缺 `X-Accel-Buffering: no` 时，Nginx 会把整条流缓冲到结束才吐给客户端 ——
// 用户看到的是"一直在转圈，然后一次性出现全部内容"，看起来像服务端卡住。
// 缺 `no-cache` 时中间缓存会把第一帧当完整响应缓存下来。
func TestStreamHeadersBlockBuffering(t *testing.T) {
	h := StreamHeaders()
	if h["Content-Type"] != "text/event-stream" {
		t.Fatalf("Content-Type 必须是 text/event-stream，实际 %q", h["Content-Type"])
	}
	if !strings.Contains(h["Cache-Control"], "no-cache") {
		t.Fatalf("Cache-Control 必须含 no-cache，实际 %q", h["Cache-Control"])
	}
	if h["X-Accel-Buffering"] != "no" {
		t.Fatalf("X-Accel-Buffering 必须是 no，实际 %q", h["X-Accel-Buffering"])
	}
	// 返回副本：调用方拼头时改动不能污染下一次请求。
	h["Content-Type"] = "text/plain"
	if StreamHeaders()["Content-Type"] != "text/event-stream" {
		t.Fatal("StreamHeaders 必须返回副本")
	}
}

// TestFrameTrimsPayloadTrailingNewline 守「payload 自带换行不会切出空帧」。
//
// SSE 里换行即「一行结束」。payload 尾随一个 \n 时，`data: {…}\n` 已经结束了
// data 字段，再追加本函数的 `\n\n` 就多出一个空行 —— 客户端按空行切帧，
// 于是每帧后面多一个空事件（解析器报 unexpected end of JSON input）。
func TestFrameTrimsPayloadTrailingNewline(t *testing.T) {
	got := string(Frame("x", []byte("{\"a\":1}\n")))
	if got != "event: x\ndata: {\"a\":1}\n\n" {
		t.Fatalf("尾随换行必须被修剪，实际 %q", got)
	}
	if bytes.Contains(Frame("x", []byte("{\"a\":1}\n\n")), []byte("\n\n\n")) {
		t.Fatal("不许出现连续三个换行（会切出空帧）")
	}
	// 内容本身不能被动：中间换行不在修剪范围（虽然我们的 payload 都是单行）。
	if !bytes.Contains(Frame("", []byte("a")), []byte("data: a")) {
		t.Fatal("payload 内容必须原样保留")
	}
}
