package upload

import (
	"bytes"
	"encoding/base64"
	"errors"

	"github.com/disintegration/imaging"
	"github.com/gen2brain/webp"
)

// 头像的边界。四个数字分别回答「原图能多大」「输出多大」「能解多少像素」「入库字符串上限」。
const (
	// AvatarMaxBytes 是**原图**上限（请求体门与解码内存都按它算）。
	AvatarMaxBytes = 2 << 20
	// AvatarEdge 是输出边长。40px 的显示位（2x 屏 80px）用 128 已有 1.6 倍余量。
	AvatarEdge = 128
	// AvatarMaxPixels 与压缩共用同一档（16M 像素 ≈ 64MB RGBA）：解码前就要拦住。
	AvatarMaxPixels = 16_000_000
	// AvatarMaxDataURL 是入库字符串上限。128px q82 实测 3–8KB，24KB 是 3 倍余量；
	// 真正要防的是「一张照片压不进 24KB」时把 100KB 的 base64 写进 users 行。
	AvatarMaxDataURL = 24_000
)

const (
	avatarQuality    = 82
	avatarQualityLow = 60
)

// 头像编码的哨兵错误。controller 按它们映射 HTTP 状态，**不在这里拼中文文案** ——
// 文案属于接口层（同一种失败在不同端点可能要说不同的话）。
var (
	ErrAvatarEmpty  = errors.New("avatar: 文件为空")
	ErrAvatarType   = errors.New("avatar: 只支持 PNG / JPEG / WebP")
	ErrAvatarPixels = errors.New("avatar: 图片尺寸过大")
	ErrAvatarBroken = errors.New("avatar: 图片解析失败")
	ErrAvatarTooBig = errors.New("avatar: 编码后体积仍过大")
)

// EncodeAvatar 把任意一张受支持的图片编码成入库用的 data URL。
//
// 复用同包的嗅探 / 解码 / 白底压平 / 并发闸，不另写一套：
// `imaging.Decode` **没有 WebP 解码器**，另建一个 service 包必然把「webp 走哪条分支」
// 抄两遍，然后一份支持一份不支持。
//
// 顺序有硬要求：
//  1. 先 SniffImage 定类型 —— 它已经挡掉 svg/gif/bmp（svg 可内嵌脚本，gif 会动）；
//  2. 再 decodeConfig 读宽高判像素 —— 先解码再判等于没判（100M 像素直接 OOM）；
//  3. 解码、裁切、编码都在 compressSem 里 —— 与上传压缩共用同一个全局 2 路图像闸。
func EncodeAvatar(raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", ErrAvatarEmpty
	}
	ext, _, ok := SniffImage(raw)
	if !ok {
		return "", ErrAvatarType
	}
	cfg, err := decodeConfig(raw, ext)
	if err != nil {
		return "", ErrAvatarBroken
	}
	if int64(cfg.Width)*int64(cfg.Height) > AvatarMaxPixels {
		return "", ErrAvatarPixels
	}

	compressSem <- struct{}{}
	defer func() { <-compressSem }()

	img, err := decodeImage(raw, ext)
	if err != nil {
		return "", ErrAvatarBroken
	}
	// 居中方形裁切（非方形图会裁掉两侧）+ 白底压平：透明头像在暗色主题下会出现
	// 「半透明轮廓」，压白后颜色可预期、体积也更小。filter 必须显式给，
	// imaging 的零值 filter 是最近邻。
	square := imaging.Fill(flattenOnWhite(img), AvatarEdge, AvatarEdge, imaging.Center, imaging.Lanczos)

	// 一次降质重试就够：128px 的图在 q60 下通常只有 2–3KB，还超标说明素材本身极端。
	for _, q := range []int{avatarQuality, avatarQualityLow} {
		var buf bytes.Buffer
		if err := webp.Encode(&buf, square, webp.Options{Quality: q}); err != nil {
			return "", err
		}
		if buf.Len() > AvatarMaxDataURL {
			continue
		}
		return "data:image/webp;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
	}
	return "", ErrAvatarTooBig
}
