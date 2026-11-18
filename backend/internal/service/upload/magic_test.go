package upload

import (
	"bytes"
	"testing"
)

// TestSniffImage 守「图标只收 png/jpeg/webp」这条白名单。
//
// 为什么要单独守：这道判断同时是**安全边界**（SVG 可内嵌 <script>，
// 用 <img> 引用时脚本不执行，但运维会把同一个 URL 直接点开看，那一刻就是同源脚本）
// 与**可用性边界**（gif 会动，选择器里一跳一跳；bmp 体积是 png 的十倍以上）。
// 这两类问题都不会在"上传成功"时暴露，只会在上线后被看到。
func TestSniffImage(t *testing.T) {
	png := append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, bytes.Repeat([]byte{0}, 16)...)
	jpeg := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte{0}, 16)...)
	webp := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), bytes.Repeat([]byte{0}, 8)...)

	cases := []struct {
		name string
		head []byte
		ext  string
		mime string
		ok   bool
	}{
		{"png", png, "png", "image/png", true},
		{"jpeg", jpeg, "jpg", "image/jpeg", true},
		{"webp", webp, "webp", "image/webp", true},

		// 下面四个都是 sniff 认定的 image 族，但不在图标白名单内。
		{"gif", []byte("GIF89a......"), "", "", false},
		{"bmp", append([]byte{'B', 'M', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 40, 0, 0, 0}, make([]byte, 8)...), "", "", false},
		// SVG 会落进 text 族 —— 必须靠 family != image 拒掉，而不是靠"文件头不像图片"。
		{"svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), "", "", false},
		{"html", []byte("<!doctype html><html></html>"), "", "", false},

		// 空头与可执行体。
		{"empty", nil, "", "", false},
		{"exe", []byte("MZ\x90\x00"), "", "", false},
		{"elf", []byte{0x7F, 'E', 'L', 'F'}, "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ext, mime, ok := SniffImage(c.head)
			if ok != c.ok || ext != c.ext || mime != c.mime {
				t.Fatalf("SniffImage(%s) = (%q,%q,%v)，期望 (%q,%q,%v)",
					c.name, ext, mime, ok, c.ext, c.mime, c.ok)
			}
		})
	}
}
