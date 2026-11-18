package relay

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// buildWAV 造一段最小的 16-bit PCM WAV（byteRate = 8000B/s）。
func buildWAV(dataLen int, byteRate uint32) []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+dataLen))
	b.WriteString("WAVE")
	b.WriteString("fmt ")
	_ = binary.Write(&b, binary.LittleEndian, uint32(16))
	_ = binary.Write(&b, binary.LittleEndian, uint16(1)) // PCM
	_ = binary.Write(&b, binary.LittleEndian, uint16(1)) // mono
	_ = binary.Write(&b, binary.LittleEndian, uint32(8000))
	_ = binary.Write(&b, binary.LittleEndian, byteRate)
	_ = binary.Write(&b, binary.LittleEndian, uint16(2)) // block align
	_ = binary.Write(&b, binary.LittleEndian, uint16(16))
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(dataLen))
	b.Write(make([]byte, dataLen))
	return b.Bytes()
}

// TestAudioDurationWAV 锁死按秒计费的时长来源：WAV 头 → 秒数。
//
// 8000B/s × 3.0s = 24000B → 3 秒；尾数 0.6 秒必须四舍五入到 4 秒（截断会系统性少收）。
func TestAudioDurationWAV(t *testing.T) {
	cases := []struct {
		name string
		data int
		want int64
	}{
		{"3 秒", 24000, 3},
		{"3.6 秒进位", 28800, 4},
		{"不足 1 秒按 1 秒", 800, 1},
	}
	for _, c := range cases {
		if got := AudioDurationSecs(buildWAV(c.data, 8000)); got != c.want {
			t.Fatalf("%s：时长应为 %d，实际 %d", c.name, c.want, got)
		}
	}
}

// TestAudioDurationFLAC 锁死 FLAC 的 STREAMINFO 解析（TTS 常见的无损输出）。
func TestAudioDurationFLAC(t *testing.T) {
	// sampleRate=44100, totalSamples=88200 → 2 秒。
	stream := make([]byte, 34)
	stream[10] = byte((44100 >> 12) & 0xFF)
	stream[11] = byte((44100 >> 4) & 0xFF)
	stream[12] = byte((44100 << 4) & 0xF0)
	total := uint64(88200)
	stream[13] = byte(total>>32) & 0x0F
	stream[14] = byte(total >> 24)
	stream[15] = byte(total >> 16)
	stream[16] = byte(total >> 8)
	stream[17] = byte(total)

	body := append([]byte{'f', 'L', 'a', 'C', 0x00, 0x00, 0x00, 34}, stream...)
	if got := AudioDurationSecs(body); got != 2 {
		t.Fatalf("FLAC 时长应为 2 秒，实际 %d", got)
	}
}

// TestAudioDurationUnknownReturnsZero 锁死"认不出就别猜"：
// 返回 0 让结算走最低消费 + anomaly，而不是把随机字节算成 17 秒。
func TestAudioDurationUnknownReturnsZero(t *testing.T) {
	if got := AudioDurationSecs([]byte("not an audio file at all")); got != 0 {
		t.Fatalf("不可识别格式应返回 0，实际 %d", got)
	}
	if got := AudioDurationSecs(nil); got != 0 {
		t.Fatalf("空体应返回 0，实际 %d", got)
	}
}

// TestAudioDurationSizedHeadOnly 锁死「只凭头部 + Content-Length 也能算时长」。
//
// 这条是内置适配器（流式转发 TTS）按秒计费的前提：以前整段落内存才探得到，
// 长文本朗读会撞 8 MiB 上限；现在只窥探 8 KiB 头，就必须仍然算得对。
func TestAudioDurationSizedHeadOnly(t *testing.T) {
	full := buildWAV(28800, 8000) // 3.6 秒 → 4 秒
	head := full[:4096]

	// WAV 的 data 块长度写在头里：不给 total 也能算。
	if got := AudioDurationSecsSized(head, 0); got != 4 {
		t.Fatalf("WAV 头部应算出 4 秒，实际 %d", got)
	}
	// 给了 total 也必须一致（不能因为 total 参与而变成别的数）。
	if got := AudioDurationSecsSized(head, int64(len(full))); got != 4 {
		t.Fatalf("WAV 头部 + total 应算出 4 秒，实际 %d", got)
	}

	// 流式写出的 WAV 常把 data size 写成 0（长度未知）：此时只能靠 total。
	unknown := buildWAV(28800, 8000)
	binary.LittleEndian.PutUint32(unknown[40:44], 0)
	if got := AudioDurationSecsSized(unknown[:4096], 0); got != 0 {
		t.Fatalf("data 长度未知且没有 total 时应返回 0（不能拿头部长度冒充总长），实际 %d", got)
	}
	if got := AudioDurationSecsSized(unknown[:4096], int64(len(unknown))); got != 4 {
		t.Fatalf("data 长度未知但给了 total 应算出 4 秒，实际 %d", got)
	}
}

// TestAudioDurationMP3NeedsTotal 锁死 MP3 的「没有总长就别猜」。
//
// 首帧比特率 × 总字节数才是时长。拿 4 KiB 头部去算会得到「0.25 秒」这种
// 看起来很确定、实际错两个数量级的数字 —— 那比返回 0（走兜底）更危险。
func TestAudioDurationMP3NeedsTotal(t *testing.T) {
	// MPEG1 Layer3、128kbps、44.1kHz 的首帧头。
	frame := []byte{0xFF, 0xFB, 0x90, 0x00}
	head := make([]byte, 4096)
	copy(head, frame)

	if got := AudioDurationSecsSized(head, 0); got != 0 {
		t.Fatalf("MP3 没有总字节数时应返回 0，实际 %d", got)
	}
	// 128kbps → 16000 B/s；32000 字节 = 2 秒。
	if got := AudioDurationSecsSized(head, 32000); got != 2 {
		t.Fatalf("MP3 给了总字节数应算出 2 秒，实际 %d", got)
	}
}

// TestAudioDurationSizedClampsTotal 锁死「total 小于已读头长度」的自愈。
//
// 调用方把 total 传小（例如只算了剩余部分）时，绝不能算出负的 data 长度。
func TestAudioDurationSizedClampsTotal(t *testing.T) {
	full := buildWAV(8000, 8000)
	if got := AudioDurationSecsSized(full, 10); got != 1 {
		t.Fatalf("total 小于头部长度时应以头部为准算出 1 秒，实际 %d", got)
	}
}
