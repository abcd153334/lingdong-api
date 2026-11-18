package adapters

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"lingdong-api/internal/relay"
)

// ttsWAV 造一段最小的 16-bit PCM WAV（byteRate 给定），用于验证时长探测。
func ttsWAV(dataLen int, byteRate uint32) []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+dataLen))
	b.WriteString("WAVE")
	b.WriteString("fmt ")
	_ = binary.Write(&b, binary.LittleEndian, uint32(16))
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))
	_ = binary.Write(&b, binary.LittleEndian, uint32(8000))
	_ = binary.Write(&b, binary.LittleEndian, byteRate)
	_ = binary.Write(&b, binary.LittleEndian, uint16(2))
	_ = binary.Write(&b, binary.LittleEndian, uint16(16))
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(dataLen))
	b.Write(make([]byte, dataLen))
	return b.Bytes()
}

// TestTTSStreamProbesDuration 锁死「流式转发也要拿到产物时长」。
//
// 背景：TTS 走 Stream（不整段落内存），旧实现于是一点字节都探测不到，
// 按秒计费的模型只能按最低消费收 —— 长文本朗读系统性少收，且账单上看不出来。
// 现在先窥探 8 KiB 头再边收边发，时长必须仍然算得对，且字节一个都不能少。
func TestTTSStreamProbesDuration(t *testing.T) {
	// 8000 B/s × 32000 B = 4 秒。
	audio := ttsWAV(32000, 8000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
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
		t.Fatal("2xx 的 TTS 响应必须走 Stream")
	}
	defer raw.Stream.Close()
	if raw.DurationSecs != 4 {
		t.Fatalf("应从 WAV 头算出 4 秒，实际 %d", raw.DurationSecs)
	}
	got, err := io.ReadAll(raw.Stream)
	if err != nil {
		t.Fatalf("读取流失败：%v", err)
	}
	// 头部被预读后必须原样回放：少一个字节，客户端拿到的 WAV 就解不出声。
	if !bytes.Equal(got, audio) {
		t.Fatalf("音频内容不一致：%d != %d 字节", len(got), len(audio))
	}
}

// TestTTSStreamProbesDurationFromContentLength 锁死 MP3 走 Content-Length 的口径。
//
// MP3 的时长 = 首帧比特率 × 总字节数，头里没有总长；上游给了 Content-Length
// 就必须用上，否则按秒计费的常见格式（mp3）整类退化成最低消费。
func TestTTSStreamProbesDurationFromContentLength(t *testing.T) {
	// MPEG1 Layer3、128kbps → 16000 B/s；32000 字节 = 2 秒。
	const total = 32000
	body := make([]byte, total)
	copy(body, []byte{0xFF, 0xFB, 0x90, 0x00})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Content-Length", "32000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	cfg := &relay.ChannelConfig{BaseURL: srv.URL, APIKey: "sk-1"}
	raw, err := (OpenAI{}).Forward(context.Background(), cfg, &relay.RelayRequest{
		Model: "tts-1", Body: json.RawMessage(`{"model":"tts-1","input":"hi"}`),
	}, relay.PathAudioSpeech)
	if err != nil {
		t.Fatalf("TTS 转发失败：%v", err)
	}
	defer raw.Stream.Close()
	if raw.DurationSecs != 2 {
		t.Fatalf("应按 Content-Length 算出 2 秒，实际 %d", raw.DurationSecs)
	}
	got, _ := io.ReadAll(raw.Stream)
	if len(got) != total {
		t.Fatalf("字节数应为 %d，实际 %d", total, len(got))
	}
}

// TestReadHeadToleratesEOF 锁死短音频的预读：凑不满 8 KiB 不是错误。
func TestReadHeadToleratesEOF(t *testing.T) {
	head, err := readHead(bytes.NewReader([]byte("short")), 4096)
	if err != nil {
		t.Fatalf("提前 EOF 不应报错：%v", err)
	}
	if string(head) != "short" {
		t.Fatalf("应返回已读到的字节，实际 %q", head)
	}
}

// TestPeekedBodyClosesUpstream 锁死 Close 透传：
//
// 用 io.MultiReader + NopCloser 会丢掉上游连接的 Close，长文本朗读下
// 连接池会被耗干（表现为"跑一段时间后所有请求超时"）。
func TestPeekedBodyClosesUpstream(t *testing.T) {
	up := &closeSpy{Reader: bytes.NewReader([]byte("tail"))}
	p := &peekedBody{head: []byte("head"), rc: up}
	got, err := io.ReadAll(p)
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	if string(got) != "headtail" {
		t.Fatalf("应先回放头部再接底层流，实际 %q", got)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("关闭失败：%v", err)
	}
	if !up.closed {
		t.Fatal("Close 必须透传到上游流")
	}
}

type closeSpy struct {
	io.Reader
	closed bool
}

func (c *closeSpy) Close() error {
	c.closed = true
	return nil
}
