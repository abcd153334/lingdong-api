package upload

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"math/rand"
	"testing"

	"lingdong-api/internal/pkg/uploadcfg"
)

// 本文件是「压缩真的发生了」的证据链。
//
// 图片样本**现场生成**（噪声图），不依赖任何外部文件：
// 纯色图会被 PNG 压得比 WebP 还小，测不出收益，也无法证明取小规则。

// noisePNG 生成一张噪声 PNG（近似照片，压缩器压不动）。
func noisePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	rnd := rand.New(rand.NewSource(7))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := img.PixOffset(x, y)
			img.Pix[i] = uint8(rnd.Intn(256))
			img.Pix[i+1] = uint8(rnd.Intn(256))
			img.Pix[i+2] = uint8(rnd.Intn(256))
			img.Pix[i+3] = 0xff
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatalf("生成 PNG 样本失败: %v", err)
	}
	return b.Bytes()
}

// noiseJPEG 生成一张噪声 JPEG。
func noiseJPEG(t *testing.T, w, h, quality int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	rnd := rand.New(rand.NewSource(11))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := img.PixOffset(x, y)
			img.Pix[i] = uint8(rnd.Intn(256))
			img.Pix[i+1] = uint8(rnd.Intn(256))
			img.Pix[i+2] = uint8(rnd.Intn(256))
			img.Pix[i+3] = 0xff
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatalf("生成 JPEG 样本失败: %v", err)
	}
	return b.Bytes()
}

// runPrepare 走一遍准备逻辑并取回最终字节。
func runPrepare(t *testing.T, s uploadcfg.Settings, ext string, data []byte) (out []byte, outExt, mime string) {
	t.Helper()
	body, _, gotExt, gotMime, err := prepareBody(context.Background(), s, ext, nil, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("prepareBody 失败: %v", err)
	}
	out, err = io.ReadAll(body)
	if err != nil {
		t.Fatalf("读回结果失败: %v", err)
	}
	return out, gotExt, gotMime
}

// webpSettings 是「统一转 WebP」的配置。
func webpSettings(quality int) uploadcfg.Settings {
	s := uploadcfg.Defaults()
	s.Compress = uploadcfg.Compress{Enabled: true, Quality: quality, Format: uploadcfg.FormatWebP}
	return s
}

// TestCompressToWebP 是本次功能的主线：PNG 上传后落成 WebP，体积显著更小。
func TestCompressToWebP(t *testing.T) {
	in := noisePNG(t, 640, 480)
	out, ext, mime := runPrepare(t, webpSettings(60), "png", in)

	if ext != "webp" || mime != "image/webp" {
		t.Fatalf("应转成 webp/image/webp，实际 ext=%s mime=%s", ext, mime)
	}
	if len(out) >= len(in) {
		t.Fatalf("体积应显著变小：in=%d out=%d", len(in), len(out))
	}
	// 产出的确实是可解码的 WebP（不是空壳或错误页）。
	cfg, err := decodeConfig(out, "webp")
	if err != nil {
		t.Fatalf("输出不是合法 WebP: %v", err)
	}
	if cfg.Width != 640 || cfg.Height != 480 {
		t.Fatalf("尺寸被改动：%dx%d", cfg.Width, cfg.Height)
	}
}

// TestCompressQualityAffectsSize 守住「压缩质量」这个旋钮真的生效。
func TestCompressQualityAffectsSize(t *testing.T) {
	in := noiseJPEG(t, 640, 480, 95)

	low, _, _ := runPrepare(t, webpSettings(20), "jpg", in)
	high, _, _ := runPrepare(t, webpSettings(95), "jpg", in)
	if len(low) >= len(high) {
		t.Fatalf("低质量应更小：q20=%d q95=%d", len(low), len(high))
	}
}

// TestCompressPNGToJPEGFlattensAlpha JPEG 没有 alpha：透明区必须压成白底而不是黑底。
func TestCompressPNGToJPEGFlattensAlpha(t *testing.T) {
	// 左半全透明（RGB 全 0），右半噪声不透明 —— 噪声是为了让样本超过压缩下限，
	// 纯色图会被 PNG 压到 1KB 级，根本进不了压缩链路。
	const side = 600
	img := image.NewNRGBA(image.Rect(0, 0, side, side))
	rnd := rand.New(rand.NewSource(3))
	for y := 0; y < side; y++ {
		for x := 0; x < side; x++ {
			if x < side/2 {
				img.Set(x, y, color.NRGBA{0, 0, 0, 0})
			} else {
				img.Set(x, y, color.NRGBA{uint8(rnd.Intn(256)), uint8(rnd.Intn(256)), uint8(rnd.Intn(256)), 255})
			}
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatalf("生成透明 PNG 失败: %v", err)
	}
	in := b.Bytes()
	if len(in) < minCompressBytes {
		t.Fatalf("样本需超过压缩下限，实际 %d", len(in))
	}

	s := uploadcfg.Defaults()
	s.Compress = uploadcfg.Compress{Enabled: true, Quality: 90, Format: uploadcfg.FormatJPEG}
	out, ext, mime := runPrepare(t, s, "png", in)
	if ext != "jpg" || mime != "image/jpeg" {
		t.Fatalf("应转 jpg/image/jpeg，实际 ext=%s mime=%s", ext, mime)
	}

	got, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("输出不是合法 JPEG: %v", err)
	}
	// 透明区的 RGB 是 0；没有白底压平的话这里会是黑的。
	r, g, bl, _ := got.At(10, 10).RGBA()
	if r < 0x8000 || g < 0x8000 || bl < 0x8000 {
		t.Fatalf("透明区应被压成白底，实际 rgb=(%d,%d,%d)", r>>8, g>>8, bl>>8)
	}
}

// TestCompressOriginalKeepsSmaller 同格式重编码时「取小」：结果永不比原始大。
func TestCompressOriginalKeepsSmaller(t *testing.T) {
	s := uploadcfg.Defaults()
	s.Compress = uploadcfg.Compress{Enabled: true, Quality: 90, Format: uploadcfg.FormatOriginal}

	// PNG 重编码通常不会更小 —— 此时必须原样保留，而不是把文件撑大。
	in := noisePNG(t, 400, 300)
	out, ext, mime := runPrepare(t, s, "png", in)
	if ext != "png" || mime != "image/png" {
		t.Fatalf("原格式应保持 png，实际 ext=%s mime=%s", ext, mime)
	}
	if len(out) > len(in) {
		t.Fatalf("重编码不应把文件撑大：in=%d out=%d", len(in), len(out))
	}
}

// TestCompressSkips 覆盖所有「不该压」的分支：结果必须与原字节逐字节相等。
func TestCompressSkips(t *testing.T) {
	s := webpSettings(60)
	small := noisePNG(t, 40, 40) // < 64KiB

	cases := []struct {
		name string
		s    uploadcfg.Settings
		ext  string
		data []byte
	}{
		{"开关关闭", func() uploadcfg.Settings {
			x := webpSettings(60)
			x.Compress.Enabled = false
			return x
		}(), "png", noisePNG(t, 300, 300)},
		{"小图不压", s, "png", small},
		{"gif 不压（动画会丢帧）", s, "gif", append([]byte("GIF89a"), make([]byte, 200<<10)...)},
		{"目标后缀未启用", func() uploadcfg.Settings {
			x := webpSettings(60)
			delete(x.Types, "webp")
			return x
		}(), "png", noisePNG(t, 300, 300)},
		{"解码失败", s, "png", append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 200<<10)...)},
		{"像素超上限", s, "jpg", bigGrayJPEG(t)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, ext, _ := runPrepare(t, c.s, c.ext, c.data)
			if ext != c.ext {
				t.Fatalf("后缀不应改变: got %s want %s", ext, c.ext)
			}
			if !bytes.Equal(out, c.data) {
				t.Fatalf("应原样上传：in=%d out=%d", len(c.data), len(out))
			}
		})
	}
}

// bigGrayJPEG 生成一张超过像素上限的灰度 JPEG（灰度编码很便宜，不会拖慢用例）。
func bigGrayJPEG(t *testing.T) []byte {
	t.Helper()
	const side = 4200 // 17.6M 像素 > 16M 上限
	img := image.NewGray(image.Rect(0, 0, side, side))
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 60}); err != nil {
		t.Fatalf("生成大图失败: %v", err)
	}
	return b.Bytes()
}

// TestWithExt 守住「格式变了后缀要跟着变」这条文件名规则。
func TestWithExt(t *testing.T) {
	cases := map[string]string{
		"a.png":         "a.webp",
		"报告.2024.png":   "报告.2024.webp",
		`C:\tmp\b.JPEG`: "b.webp",
		"noext":         "noext.webp",
		".hidden":       ".hidden.webp", // 首字符就是点的名字没有后缀可换，直接追加
	}
	for in, want := range cases {
		if got := withExt(in, "webp"); got != want {
			t.Fatalf("withExt(%q) = %q, want %q", in, got, want)
		}
	}
}
