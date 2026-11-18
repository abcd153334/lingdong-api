package upload

import (
	"bytes"
	"encoding/base64"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/gen2brain/webp"
)

// avatarDataURLBody 剥掉 data URL 前缀并解码。
func avatarDataURLBody(t *testing.T, s string) []byte {
	t.Helper()
	const prefix = "data:image/webp;base64,"
	if !strings.HasPrefix(s, prefix) {
		t.Fatalf("头像必须以 %s 开头，实际 %q", prefix, s[:min(len(s), 40)])
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, prefix))
	if err != nil {
		t.Fatalf("base64 解码失败: %v", err)
	}
	return raw
}

func TestEncodeAvatarNormalizesToSquare(t *testing.T) {
	// 300×200 的非方形图：居中方形裁切后必须是 128×128，且体积在入库上限内。
	src := image.NewNRGBA(image.Rect(0, 0, 300, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 300; x++ {
			src.Set(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 120, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatalf("构造 png 失败: %v", err)
	}

	got, err := EncodeAvatar(buf.Bytes())
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	body := avatarDataURLBody(t, got)
	cfg, err := webp.DecodeConfig(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("输出不是 webp: %v", err)
	}
	if cfg.Width != AvatarEdge || cfg.Height != AvatarEdge {
		t.Fatalf("输出尺寸必须 %d×%d，实际 %d×%d", AvatarEdge, AvatarEdge, cfg.Width, cfg.Height)
	}
	if len(got) > AvatarMaxDataURL {
		t.Fatalf("入库字符串超过上限：%d > %d", len(got), AvatarMaxDataURL)
	}
}

func TestEncodeAvatarAcceptsJPEGAndWebP(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			src.Set(x, y, color.RGBA{R: 20, G: 160, B: 90, A: 255})
		}
	}
	var jpg bytes.Buffer
	if err := jpeg.Encode(&jpg, src, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("构造 jpeg 失败: %v", err)
	}
	var wp bytes.Buffer
	if err := webp.Encode(&wp, src, webp.Options{Quality: 90}); err != nil {
		t.Fatalf("构造 webp 失败: %v", err)
	}
	for name, raw := range map[string][]byte{"jpeg": jpg.Bytes(), "webp": wp.Bytes()} {
		if _, err := EncodeAvatar(raw); err != nil {
			t.Fatalf("%s 应被接受：%v", name, err)
		}
	}
}

// TestEncodeAvatarRejectsNonWhitelisted 守的是「svg 能内嵌脚本、gif 会动」这两条：
// 头像只收 png/jpeg/webp，靠 SniffImage 的同一张魔数表，不另写白名单。
func TestEncodeAvatarRejectsNonWhitelisted(t *testing.T) {
	var gifBuf bytes.Buffer
	if err := gif.Encode(&gifBuf, image.NewPaletted(image.Rect(0, 0, 8, 8), color.Palette{color.Black, color.White}), nil); err != nil {
		t.Fatalf("构造 gif 失败: %v", err)
	}
	cases := map[string][]byte{
		"gif":  gifBuf.Bytes(),
		"svg":  []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"text": []byte("not an image"),
	}
	for name, raw := range cases {
		if _, err := EncodeAvatar(raw); !errors.Is(err, ErrAvatarType) {
			t.Fatalf("%s 必须以 ErrAvatarType 拒绝，实际 %v", name, err)
		}
	}
	if _, err := EncodeAvatar(nil); !errors.Is(err, ErrAvatarEmpty) {
		t.Fatalf("空文件必须 ErrAvatarEmpty，实际 %v", err)
	}
}

// TestEncodeAvatarPixelGateRunsBeforeDecode 只喂一个合法 IHDR（没有像素数据）：
// 若实现是「先解码再判像素」，这里会拿到 ErrAvatarBroken；拿到 ErrAvatarPixels
// 才证明像素闸在解码之前 —— 否则一张 1 亿像素的图会先把进程顶到 OOM。
func TestEncodeAvatarPixelGateRunsBeforeDecode(t *testing.T) {
	raw := pngIHDR(20000, 20000) // 400M 像素
	if _, err := EncodeAvatar(raw); !errors.Is(err, ErrAvatarPixels) {
		t.Fatalf("必须命中 ErrAvatarPixels，实际 %v", err)
	}
}

// TestEncodeAvatarFlattensTransparency 守「透明头像压白底」：
// 否则暗色主题下会出现半透明轮廓，同色头像在两种主题下观感不一致。
func TestEncodeAvatarFlattensTransparency(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 32, 32)) // 全透明
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatalf("构造透明 png 失败: %v", err)
	}
	got, err := EncodeAvatar(buf.Bytes())
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	img, err := webp.Decode(bytes.NewReader(avatarDataURLBody(t, got)))
	if err != nil {
		t.Fatalf("解码输出失败: %v", err)
	}
	_, _, _, a := img.At(0, 0).RGBA()
	if a != 0xffff {
		t.Fatalf("透明像素必须被压成不透明白底，实际 alpha=%d", a)
	}
}

// pngIHDR 只构造签名 + IHDR（无 IDAT）：足够让 image.DecodeConfig 读出宽高。
func pngIHDR(w, h uint32) []byte {
	ihdr := make([]byte, 0, 13)
	put := func(v uint32) {
		ihdr = append(ihdr, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	}
	put(w)
	put(h)
	ihdr = append(ihdr, 8, 6, 0, 0, 0) // 8bit RGBA

	var out bytes.Buffer
	out.Write([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	chunk := func(typ string, data []byte) {
		var length [4]byte
		n := uint32(len(data))
		length[0], length[1], length[2], length[3] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
		out.Write(length[:])
		out.WriteString(typ)
		out.Write(data)
		sum := crc32.ChecksumIEEE(append([]byte(typ), data...))
		out.Write([]byte{byte(sum >> 24), byte(sum >> 16), byte(sum >> 8), byte(sum)})
	}
	chunk("IHDR", ihdr)
	return out.Bytes()
}
