package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lingdong-api/internal/relay"
)

// 本文件覆盖 B5-4（openai 适配器契约）的四类缺陷：
//   - P1-29 流式空闲超时（每帧重置，长时间无字节才算失败）
//   - P1-30 响应体上限（超限报错而非静默截断；TTS 走 32MiB 流式）
//   - P1-31 空流式响应（零数据帧）
//   - P2-01 Content-Type 归一 / P2-02 nil 配置

func TestMediaTypeOf(t *testing.T) {
	cases := []struct{ in, want string }{
		{"application/json", "application/json"},
		{"Application/JSON", "application/json"},
		{"application/json; charset=utf-8", "application/json"},
		{"Application/JSON ; charset=UTF-8", "application/json"},
		{"multipart/form-data; boundary=abc", "multipart/form-data"},
		{"MULTIPART/FORM-DATA; boundary=\"a b\"", "multipart/form-data"},
		{"application/problem+json", "application/problem+json"},
		// ParseMediaType 失败时退回「取分号前 + 小写」，仍好于裸比较。
		{"application/json; boundary=", "application/json"},
		{"", ""},
	}
	for _, c := range cases {
		if got := mediaTypeOf(c.in); got != c.want {
			t.Fatalf("mediaTypeOf(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestReadUpstreamBodyRejectsOversize(t *testing.T) {
	// 恰好等于上限：合法。
	buf, err := readUpstreamBody(strings.NewReader("12345"), 5)
	if err != nil || string(buf) != "12345" {
		t.Fatalf("等于上限应成功，实际 buf=%q err=%v", buf, err)
	}
	// 超一个字节：必须报错，且错误可被 errors.Is 识别（网关据此归因上游故障）。
	if _, err := readUpstreamBody(strings.NewReader("123456"), 5); !errors.Is(err, relay.ErrUpstreamBodyTooLarge) {
		t.Fatalf("超限应返回 ErrUpstreamBodyTooLarge，实际 %v", err)
	}
}

func TestCappedBodyStopsAtLimit(t *testing.T) {
	c := &cappedBody{rc: io.NopCloser(strings.NewReader("0123456789")), limit: 4}
	got, err := io.ReadAll(c)
	if !errors.Is(err, relay.ErrUpstreamBodyTooLarge) {
		t.Fatalf("读满上限后应报超限，实际 err=%v", err)
	}
	if string(got) != "0123" {
		t.Fatalf("超限前应完整交出 %q，实际 %q", "0123", got)
	}
}

func TestForwardTTSReturnsStream(t *testing.T) {
	audio := bytes.Repeat([]byte("A"), 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != relay.PathAudioSpeech {
			t.Errorf("路径错误：%s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(audio)
	}))
	defer srv.Close()

	cfg := &relay.ChannelConfig{BaseURL: srv.URL, APIKey: "sk-1"}
	raw, err := (OpenAI{}).Forward(context.Background(), cfg, &relay.RelayRequest{
		Model: "tts-1", Body: json.RawMessage(`{"model":"tts-1","input":"hi"}`),
	}, relay.PathAudioSpeech)
	if err != nil {
		t.Fatalf("TTS 转发失败：%v", err)
	}
	if raw.Stream == nil {
		t.Fatal("2xx 的 TTS 响应必须走 Stream（32MiB 预算 + 边收边发），不能整段进 Body")
	}
	if len(raw.Body) != 0 {
		t.Fatalf("走 Stream 时 Body 必须为空，实际 %d 字节", len(raw.Body))
	}
	defer raw.Stream.Close()
	got, err := io.ReadAll(raw.Stream)
	if err != nil {
		t.Fatalf("读取流失败：%v", err)
	}
	if !bytes.Equal(got, audio) {
		t.Fatalf("音频内容不一致：%d != %d 字节", len(got), len(audio))
	}
}

func TestForwardTTSErrorIsBuffered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer srv.Close()

	cfg := &relay.ChannelConfig{BaseURL: srv.URL, APIKey: "sk-1"}
	raw, err := (OpenAI{}).Forward(context.Background(), cfg, &relay.RelayRequest{
		Model: "tts-1", Body: json.RawMessage(`{"model":"tts-1","input":"hi"}`),
	}, relay.PathAudioSpeech)
	if err != nil {
		t.Fatalf("非 2xx 不应返回 error（交给网关判重试）：%v", err)
	}
	if raw.Stream != nil {
		t.Fatal("非 2xx 必须缓冲错误体（网关要脱敏与归因），不能流式")
	}
	if !strings.Contains(string(raw.Body), "boom") {
		t.Fatalf("错误体应保留原文，实际 %q", raw.Body)
	}
}

func TestForwardNormalizesJSONMediaType(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cfg := &relay.ChannelConfig{BaseURL: srv.URL, APIKey: "sk-1"}
	// 客户端发大小写混杂 + 分号前带空格的 Content-Type：仍必须替换 model。
	if _, err := (OpenAI{}).Forward(context.Background(), cfg, &relay.RelayRequest{
		Model: "upstream-m", Body: json.RawMessage(`{"model":"orig"}`),
		ContentType: "Application/JSON ; charset=UTF-8",
	}, relay.PathImagesGenerations); err != nil {
		t.Fatalf("转发失败：%v", err)
	}
	var p struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(gotBody, &p); err != nil {
		t.Fatalf("上游请求体不是 JSON：%v", err)
	}
	if p.Model != "upstream-m" {
		t.Fatalf("Content-Type 未归一导致跳过 model 替换：上游收到 %q", p.Model)
	}
}

func TestOpenAITestRejectsNilConfig(t *testing.T) {
	var ce *relay.ConfigError
	if err := (OpenAI{}).Test(context.Background(), nil); !errors.As(err, &ce) {
		t.Fatalf("nil 配置必须是 ConfigError（不能 panic 成 500），实际 %v", err)
	}
	ce = nil
	if err := (OpenAI{}).Test(context.Background(), &relay.ChannelConfig{APIKey: "sk-1"}); !errors.As(err, &ce) {
		t.Fatalf("空 BaseURL 必须是 ConfigError，实际 %v", err)
	}
}

func TestStreamChatIdleTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// 之后长时间不再发任何字节，直到客户端断开（或本用例收尾）。
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)

	t.Setenv("LD_STREAM_IDLE_TIMEOUT", "150ms")

	cfg := &relay.ChannelConfig{BaseURL: srv.URL, APIKey: "sk-1"}
	var frames int
	start := time.Now()
	res, err := (OpenAI{}).StreamChat(context.Background(), cfg, &relay.RelayRequest{
		Model: "m", Body: json.RawMessage(`{"model":"m","stream":true}`),
	}, func(f relay.SSEFrame) error {
		frames++
		return nil
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("空闲超时必须返回 error（否则调用方按成功估算结算，用户为截断内容付全价）")
	}
	if !strings.Contains(err.Error(), "空闲超时") {
		t.Fatalf("错误信息应指明是空闲超时，实际 %v", err)
	}
	if res == nil || res.Frames != 1 {
		t.Fatalf("应保留已收到的 1 帧（供已发内容结算），实际 %+v", res)
	}
	if frames != 1 {
		t.Fatalf("已收到的帧必须已回调给上游调用方，实际 %d 次", frames)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("空闲超时未生效（耗时 %s，上限 150ms）", elapsed)
	}
}

func TestStreamChatEmptyStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 只回 [DONE]：合法 SSE 但零数据帧。
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	cfg := &relay.ChannelConfig{BaseURL: srv.URL, APIKey: "sk-1"}
	res, err := (OpenAI{}).StreamChat(context.Background(), cfg, &relay.RelayRequest{
		Model: "m", Body: json.RawMessage(`{"model":"m","stream":true}`),
	}, func(relay.SSEFrame) error { return nil })
	if err != nil {
		t.Fatalf("只回 [DONE] 属于正常收尾，不应返回 error：%v", err)
	}
	if res.Frames != 0 {
		t.Fatalf("数据帧计数不应包含 [DONE]，实际 %d", res.Frames)
	}
}
