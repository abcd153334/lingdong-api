package upload

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// pngHead 构造 PNG 签名 + IHDR（w×h）。
func pngHead(w, h uint32) []byte {
	b := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	b = binary.BigEndian.AppendUint32(b, 13) // IHDR 长度
	b = append(b, "IHDR"...)
	b = binary.BigEndian.AppendUint32(b, w)
	b = binary.BigEndian.AppendUint32(b, h)
	return b
}

// jpegHead 构造 SOI + APP0 + SOF0，并在 APP0 里塞一张"缩略图"的 SOF0。
//
// 塞缩略图是这一组用例的重点：EXIF 内嵌缩略图带自己的 SOF，
// 「搜 FFC0」的实现会把它当成原图尺寸，于是 8000×8000 被读成 160×120 放行。
func jpegHead(w, h uint16, thumbnail bool) []byte {
	b := []byte{0xFF, 0xD8}
	app0 := bytes.Repeat([]byte{0}, 14)
	if thumbnail {
		app0 = append(app0, 0xFF, 0xC0, 0x00, 0x11, 0x08,
			0x00, 0x78, 0x00, 0xA0, // 高 120、宽 160
			0x03, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	}
	b = append(b, 0xFF, 0xE0)
	b = binary.BigEndian.AppendUint16(b, uint16(len(app0)+2))
	b = append(b, app0...)
	b = append(b, 0xFF, 0xC0, 0x00, 0x11, 0x08)
	b = binary.BigEndian.AppendUint16(b, h)
	b = binary.BigEndian.AppendUint16(b, w)
	return append(b, 0x03, 0, 0, 0, 0, 0, 0, 0, 0, 0)
}

// webpVP8L 构造无损 WebP 头（尺寸在 14+14 位的小端位流里）。
func webpVP8L(w, h uint32) []byte {
	b := []byte("RIFF")
	b = binary.LittleEndian.AppendUint32(b, 0)
	b = append(b, "WEBP"...)
	b = append(b, "VP8L"...)
	b = binary.LittleEndian.AppendUint32(b, 5)
	b = append(b, 0x2F)
	return binary.LittleEndian.AppendUint32(b, (w-1)|((h-1)<<14))
}

// webpVP8 构造有损 WebP 头（起始码 + 14 位宽高）。
func webpVP8(w, h uint16) []byte {
	b := []byte("RIFF")
	b = binary.LittleEndian.AppendUint32(b, 0)
	b = append(b, "WEBP"...)
	b = append(b, "VP8 "...)
	b = binary.LittleEndian.AppendUint32(b, 10)
	b = append(b, 0x00, 0x00, 0x00, 0x9D, 0x01, 0x2A)
	b = binary.LittleEndian.AppendUint16(b, w&0x3FFF)
	return binary.LittleEndian.AppendUint16(b, h&0x3FFF)
}

// webpVP8X 构造扩展 WebP 头（24 位画布宽-1 / 高-1）。
func webpVP8X(w, h uint32) []byte {
	b := []byte("RIFF")
	b = binary.LittleEndian.AppendUint32(b, 0)
	b = append(b, "WEBP"...)
	b = append(b, "VP8X"...)
	b = binary.LittleEndian.AppendUint32(b, 10)
	b = append(b, 0x00, 0x00, 0x00, 0x00)
	b = append(b, byte(w-1), byte((w-1)>>8), byte((w-1)>>16))
	b = append(b, byte(h-1), byte((h-1)>>8), byte((h-1)>>16))
	return append(b, 0, 0)
}

// TestImageSize 守图标尺寸解析：三种格式 × 三种 WebP 形态都要能读出真实像素。
//
// 为什么值得单独守：这一层的输出直接决定「8000×8000 纯色 PNG」能否被拒。
// 解析错位不会报错，只会静默返回 0（被当成"解析不出 → 跳过校验"），
// 症状与"这层没实现"完全一样。
func TestImageSize(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		w, h int
		ok   bool
	}{
		{"png", pngHead(32, 64), 32, 64, true},
		{"png-8000", pngHead(8000, 8000), 8000, 8000, true},
		{"jpeg", jpegHead(48, 96, false), 48, 96, true},
		// 带内嵌缩略图：必须读到原图（48×96），而不是缩略图的 160×120。
		{"jpeg-thumb", jpegHead(48, 96, true), 48, 96, true},
		{"webp-vp8", webpVP8(200, 300), 200, 300, true},
		{"webp-vp8l", webpVP8L(150, 250), 150, 250, true},
		{"webp-vp8x", webpVP8X(500, 400), 500, 400, true},

		// 认不出来的必须是 ok=false（调用方跳过校验），而不是返回 0×0 的"合法尺寸"。
		{"empty", nil, 0, 0, false},
		{"gif", []byte("GIF89a"), 0, 0, false},
		{"png-truncated", pngHead(32, 64)[:16], 0, 0, false},
		{"png-zero", pngHead(0, 0), 0, 0, false},
		{"jpeg-no-sof", []byte{0xFF, 0xD8, 0xFF, 0xD9}, 0, 0, false},
		{"webp-bad-chunk", append([]byte("RIFF\x00\x00\x00\x00WEBPXXXX"), make([]byte, 16)...), 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, h, ok := ImageSize(c.data)
			if ok != c.ok || w != c.w || h != c.h {
				t.Fatalf("ImageSize(%s) = (%d,%d,%v)，期望 (%d,%d,%v)", c.name, w, h, ok, c.w, c.h, c.ok)
			}
		})
	}
}
