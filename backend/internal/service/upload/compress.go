package upload

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"

	"github.com/disintegration/imaging"
	"github.com/gen2brain/webp"
	"go.uber.org/zap"

	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/metrics"
	"lingdong-api/internal/pkg/uploadcfg"
)

// 压缩的边界常量。三个数字分别回答「压不压」「能解多大」「同时压几个」。
const (
	// minCompressBytes：小图压了常常更大，且不值得付解码成本。
	minCompressBytes = 64 << 10
	// maxCompressPixels：解码后 RGBA 的内存 ≈ 像素 × 4，16M 像素 ≈ 64MB。
	// 超过就原样上传 —— 解开一张 1 亿像素的图足以把进程顶到 OOM。
	maxCompressPixels = 16_000_000
	// compressSlots：全局同时压缩数。单请求峰值 ≈ 源图 + 解码位图 + 输出，
	// 2 路能把最坏情况钉在 300MB 以内，且不依赖「每个用户并发 3」这种乘数。
	compressSlots = 2
)

// compressSem 是压缩的全局并发闸。
var compressSem = make(chan struct{}, compressSlots)

// countCompress 记录一次压缩判定结果。
func countCompress(result string) {
	metrics.Inc(metrics.NameUploadCompress, "result", result)
}

// prepareBody 决定「写进存储的到底是哪些字节」。
//
// 返回的 body/size/ext/mime 就是落库与上传的唯一依据：
//   - 未启用压缩、类型不适用、尺寸过小、解码失败 → 原样返回（body 仍是流，零整图内存）；
//   - 压缩生效 → 返回压缩后的字节与新后缀/MIME。
//
// **压缩永不让上传失败**：任何一步出错都退回原始字节，只增加 failed 计数。
// 这条比「保证压得更小」重要得多 —— 压缩是优化，不是功能前提。
func prepareBody(ctx context.Context, s uploadcfg.Settings, ext string, head []byte, rest io.Reader, size int64) (
	io.Reader, int64, string, string, error) {

	src := io.MultiReader(bytes.NewReader(head), rest)
	mime := s.MimeFor(ext)

	target, outExt, ok := compressTarget(s, ext)
	if !ok || size < minCompressBytes {
		return src, size, ext, mime, nil
	}

	// 到这里必须整图驻留：先把已读的头与剩余流拼回一份完整字节。
	raw, err := io.ReadAll(io.LimitReader(src, size))
	if err != nil {
		return nil, 0, "", "", fmt.Errorf("读取上传内容失败: %w", err)
	}

	// 全局并发闸：拿不到就原样上传（宁可没压，也不要排队把请求拖死）。
	select {
	case compressSem <- struct{}{}:
		defer func() { <-compressSem }()
	case <-ctx.Done():
		countCompress("skipped")
		return bytes.NewReader(raw), int64(len(raw)), ext, mime, nil
	}

	out, reason := encodeAs(raw, ext, target, s.Compress.Quality)
	if reason != "" {
		countCompress(reason)
		return bytes.NewReader(raw), int64(len(raw)), ext, mime, nil
	}

	// 后缀没变时「取小」保护：重编码偶尔会更大（尤其是已是高质量的小图），
	// 那种情况下保留原始字节对用户永远更优。后缀变了说明是运营显式指定的格式，恒采用。
	if outExt == ext && len(out) >= len(raw) {
		countCompress("skipped")
		return bytes.NewReader(raw), int64(len(raw)), ext, mime, nil
	}
	countCompress("applied")
	return bytes.NewReader(out), int64(len(out)), outExt, s.MimeFor(outExt), nil
}

// compressTarget 决定目标格式与目标后缀；ok=false 表示这张图不压。
func compressTarget(s uploadcfg.Settings, ext string) (format, outExt string, ok bool) {
	if !s.Compress.Enabled || !uploadcfg.SniffableImage(ext) {
		return "", "", false
	}
	switch s.Compress.Format {
	case uploadcfg.FormatJPEG:
		// 优先沿用 jpg（更常见），没有就退回 jpeg。
		for _, e := range []string{"jpg", "jpeg"} {
			if _, ok := s.TypeOf(e); ok {
				return uploadcfg.FormatJPEG, e, true
			}
		}
		return "", "", false
	case uploadcfg.FormatWebP:
		if _, ok := s.TypeOf("webp"); !ok {
			return "", "", false
		}
		return uploadcfg.FormatWebP, "webp", true
	default: // original
		switch ext {
		case "png", "jpg", "jpeg":
			return ext, ext, true
		case "webp":
			return uploadcfg.FormatWebP, "webp", true
		}
		// bmp 等：同格式重编码没有收益（bmp 无压缩），不值得付解码成本。
		return "", "", false
	}
}

// encodeAs 把原始字节重编码为目标格式；reason 非空表示「没压成」（调用方原样上传）。
func encodeAs(raw []byte, ext, format string, quality int) (out []byte, reason string) {
	cfg, err := decodeConfig(raw, ext)
	if err != nil {
		return nil, "failed"
	}
	if int64(cfg.Width)*int64(cfg.Height) > maxCompressPixels {
		// 不是错误：这张图只是太大，原样传上去更安全。
		logger.L().Warn("图片过大，跳过压缩",
			zap.String("ext", ext), zap.Int("width", cfg.Width), zap.Int("height", cfg.Height))
		return nil, "skipped"
	}

	img, err := decodeImage(raw, ext)
	if err != nil {
		logger.L().Warn("图片解码失败，按原样上传",
			zap.String("ext", ext), zap.Int("bytes", len(raw)), zap.Error(err))
		return nil, "failed"
	}

	var buf bytes.Buffer
	switch format {
	case uploadcfg.FormatJPEG:
		// JPEG 没有 alpha 通道：透明像素的 RGB 通常是 0，直接编码会得到黑底。
		// 先压到白底上，视觉结果与浏览器里看到的一致。
		img = flattenOnWhite(img)
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
			logger.L().Warn("JPEG 编码失败，按原样上传", zap.Error(err))
			return nil, "failed"
		}
	case uploadcfg.FormatWebP:
		if err := webp.Encode(&buf, img, webp.Options{Quality: quality}); err != nil {
			logger.L().Warn("WebP 编码失败，按原样上传", zap.Error(err))
			return nil, "failed"
		}
	default: // png
		enc := png.Encoder{CompressionLevel: png.BestCompression}
		if err := enc.Encode(&buf, img); err != nil {
			logger.L().Warn("PNG 编码失败，按原样上传", zap.Error(err))
			return nil, "failed"
		}
	}
	return buf.Bytes(), ""
}

// decodeConfig 只读图头拿宽高（不解像素），用于解码前的内存闸。
func decodeConfig(raw []byte, ext string) (image.Config, error) {
	if ext == "webp" {
		return webp.DecodeConfig(bytes.NewReader(raw))
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	return cfg, err
}

// decodeImage 完整解码。JPEG 的 EXIF 方向由成像库应用（标准库不处理，
// 不应用的话竖拍照片重编码后会躺倒）。
func decodeImage(raw []byte, ext string) (image.Image, error) {
	if ext == "webp" {
		return webp.Decode(bytes.NewReader(raw))
	}
	return imaging.Decode(bytes.NewReader(raw), imaging.AutoOrientation(true))
}

// flattenOnWhite 把带透明通道的图压到白底上，供 JPEG 编码使用。
func flattenOnWhite(img image.Image) image.Image {
	if op, ok := img.(interface{ Opaque() bool }); ok && op.Opaque() {
		return img // 本来就没有透明像素，省一次整图拷贝
	}
	b := img.Bounds()
	bg := imaging.New(b.Dx(), b.Dy(), color.White)
	return imaging.Overlay(bg, img, image.Pt(0, 0), 1.0)
}
