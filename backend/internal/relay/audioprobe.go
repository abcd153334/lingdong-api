package relay

import (
	"encoding/binary"
)

// 本文件从音频字节里读出**时长**（秒），供按秒计费使用。
//
// 为什么必须自己读头：按秒计费的唯一正确口径是"产物有多长"，
// 而请求参数（要多少秒）与产物（实际多少秒）经常不一致——
// 上游会把文本切成多段、会自动续尾静音、会在长文本上截断。
// 没有真实时长就只能退回"按次/按字"，长短文本混用必然错账。
//
// 覆盖范围与取舍：
//   - WAV：读 fmt 块算 byteRate，再除 data 块长度 —— 精确；
//   - FLAC：读 STREAMINFO 的 sampleRate 与 totalSamples —— 精确；
//   - MP3：按首帧比特率估算（CBR/ABR 够用；VBR 会低估，仍优于按 0 计）；
//   - 其它（opus/ogg/aac 裸流）：返回 0，由调用方按最低消费兜底并记 anomaly。
//     宁可少收一次已知偏差，也不要靠猜把金额算成一个没有出处的数字。

// AudioDurationSecs 返回**完整**音频字节的时长（秒），识别不了返回 0。
func AudioDurationSecs(body []byte) int64 {
	return AudioDurationSecsSized(body, int64(len(body)))
}

// AudioDurationSecsSized 只凭**头部若干字节 + 产物总字节数**算时长。
//
// 为什么需要这个变体：TTS 的响应是几十 MB 的二进制体，网关边收边发（不整段落内存），
// 拿不到完整字节；但 RIFF 的 data 块长度、FLAC 的 STREAMINFO、MP3 的总字节数
// 都在头部（或可由 Content-Length 得到）里就写着。不这么算，按秒计费的音频走
// 内置适配器时永远探测不到时长 → 全部落到最低消费，长文本系统性少收。
//
// total 是产物应有的总字节数（HTTP Content-Length；未知时传 0）：
//   - WAV/FLAC 不依赖它（长度写在头里）；
//   - MP3 依赖它（比特率 × 字节数），未知时返回 0 而不是拿头部长度去算，
//     否则 4 KB 的头部会被算成 0.25 秒。
func AudioDurationSecsSized(head []byte, total int64) int64 {
	if len(head) < 16 {
		return 0
	}
	if total < 0 || (total > 0 && total < int64(len(head))) {
		// total 不可信（负数，或比已读到的头部还小）：当成"未知"而不是就地纠正。
		//
		// 为什么不是"取 max(total, len(head))"：那等于把 8 KiB 头部当成整个产物，
		// WAV 会被算成 0.5 秒、MP3 会被算成 0.25 秒 —— 看起来确定、实际错两个数量级。
		// 未知就返回 0，由计费层按文本估算并打标记。
		total = 0
	}
	if d := wavDuration(head, total); d > 0 {
		return d
	}
	if d := flacDuration(head); d > 0 {
		return d
	}
	if d := mp3Duration(head, total); d > 0 {
		return d
	}
	return 0
}

// wavDuration 解析 RIFF/WAVE：时长 = data 块长度 / byteRate。
//
// total 是产物总字节数：data 块声明长度未知（流式写出的 WAV 常见 size=0 或
// 0xFFFFFFFF）时用它减去 data 起始偏移。
func wavDuration(b []byte, total int64) int64 {
	if len(b) < 44 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return 0
	}
	var byteRate uint32
	var dataLen uint32
	for off := 12; off+8 <= len(b); {
		id := string(b[off : off+4])
		size := binary.LittleEndian.Uint32(b[off+4 : off+8])
		body := off + 8
		switch id {
		case "fmt ":
			if size >= 16 && body+12 <= len(b) {
				// byteRate 在 fmt 块的第 8..11 字节（channels×sampleRate×bits/8）。
				byteRate = binary.LittleEndian.Uint32(b[body+8 : body+12])
			}
		case "data":
			known := total > int64(body)
			avail := total - int64(body)
			switch {
			case size != 0 && size != 0xFFFFFFFF:
				// 头部声明了长度：以声明为准；只有 total 更小（上游截断）时才以 total 为准，
				// 否则会把截断的产物按完整时长收费。
				if known && int64(size) > avail {
					dataLen = uint32(avail)
				} else {
					dataLen = size
				}
			case known:
				// 长度未知（流式写出的 WAV 常见）：按「总字节数 - data 起始偏移」算。
				dataLen = uint32(avail)
			default:
				dataLen = 0
			}
		}
		if size == 0 || size == 0xFFFFFFFF {
			break
		}
		// 块按偶数字节对齐。
		off = body + int(size) + int(size&1)
	}
	if byteRate == 0 || dataLen == 0 {
		return 0
	}
	return roundSecs(float64(dataLen) / float64(byteRate))
}

// flacDuration 解析 fLaC 的 STREAMINFO：时长 = totalSamples / sampleRate。
func flacDuration(b []byte) int64 {
	if len(b) < 42 || string(b[0:4]) != "fLaC" {
		return 0
	}
	// 第一个元数据块必须是 STREAMINFO（type 0），头部 4 字节 + 34 字节数据。
	hdr := b[4:8]
	if hdr[0]&0x7F != 0 {
		return 0
	}
	data := b[8:42]
	// sampleRate 占 20 bit（data[10] 起的最高 20 位）。
	sampleRate := uint32(data[10])<<12 | uint32(data[11])<<4 | uint32(data[12])>>4
	if sampleRate == 0 {
		return 0
	}
	totalSamples := uint64(data[13]&0x0F)<<32 | uint64(data[14])<<24 | uint64(data[15])<<16 |
		uint64(data[16])<<8 | uint64(data[17])
	if totalSamples == 0 {
		return 0
	}
	return roundSecs(float64(totalSamples) / float64(sampleRate))
}

// mp3Bitrates 是 MPEG1 Layer3 的比特率表（kbps，索引 = 头里的 4 bit）。
var mp3Bitrates = [...]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}

// mp3SampleRates 是 MPEG1 的采样率表。
var mp3SampleRates = [...]int{44100, 48000, 32000, 0}

// mp3Duration 按首帧比特率估算时长（CBR/ABR）。
//
// 跳过 ID3v2 头再找同步字：音频前面普遍带一段标签，直接扫会读到标签里的假同步字。
// total 用产物总字节数而不是头部长度：VBR 会低估（仍优于按 0 计），
// 但拿头部 4KB 去算会把整段音频算成零点几秒 —— 那是把"不确定"变成"确定错"。
func mp3Duration(b []byte, total int64) int64 {
	off := 0
	if len(b) > 10 && string(b[0:3]) == "ID3" {
		size := int(b[6]&0x7F)<<21 | int(b[7]&0x7F)<<14 | int(b[8]&0x7F)<<7 | int(b[9]&0x7F)
		off = 10 + size
	}
	if off < 0 || off+4 > len(b) {
		return 0
	}
	if b[off] != 0xFF || b[off+1]&0xE0 != 0xE0 {
		return 0
	}
	version := (b[off+1] >> 3) & 0x03 // 3 = MPEG1
	layer := (b[off+1] >> 1) & 0x03   // 1 = Layer3
	if version != 3 || layer != 1 {
		return 0
	}
	bitrateIdx := (b[off+2] >> 4) & 0x0F
	rateIdx := (b[off+2] >> 2) & 0x03
	if mp3Bitrates[bitrateIdx] == 0 || mp3SampleRates[rateIdx] == 0 {
		return 0
	}
	bitrate := mp3Bitrates[bitrateIdx] * 1000
	bytes := total - int64(off)
	if bytes <= 0 {
		return 0
	}
	return roundSecs(float64(bytes*8) / float64(bitrate))
}

// roundSecs 四舍五入到整秒并至少 1 秒。
func roundSecs(f float64) int64 {
	if f <= 0 {
		return 0
	}
	n := int64(f + 0.5)
	if n < 1 {
		n = 1
	}
	return n
}
