package upload

import (
	"bytes"
)

// 类型白名单（后缀 → 类别）、MIME 与体积上限全部来自 `system_config.uploads`
// （唯一真相源：`internal/pkg/uploadcfg`）。本文件只保留**魔数嗅探**这一层安全契约：
// 后缀与 MIME 可以配置，但「这个字节序列到底是什么」永远由代码判定。

// ImageFileHint 是「image 族实际包含哪些格式」的**唯一文案来源**。
//
// 与 sniff 的 image 分支逐项对应（PNG / JPEG / GIF / BMP / WEBP）。
// 需要向用户说明「只收图片」的地方（例如图生图的参考图）必须引用它，
// 不要在别处再手写一遍：手写的那份一旦与嗅探表漂移，就会变成
// 「提示说只收 png/jpg，实际 gif 也能过」这种与自己行为矛盾的话。
const ImageFileHint = "png / jpg / jpeg / webp / gif / bmp"

// DetectFamily 暴露魔数嗅探结果，供「只收某一族」的入口复用同一张表。
//
// 存在的理由：图生图的参考图只收图片，但上传域是**通用文件**白名单
// （txt / csv / pdf / docx 都合法）。若调用方自己再写一份图片判定，
// 两份魔数表必然漂移；复用这里，判定与上传校验永远同源。
func DetectFamily(head []byte) (family string, danger bool) { return sniff(head) }

// sniff 依据文件头返回类型族；danger = true 表示可执行体/脚本（无论扩展名一律拒绝）。
func sniff(head []byte) (family string, danger bool) {
	switch {
	case bytes.HasPrefix(head, []byte("MZ")), // PE（.exe/.dll）
		bytes.HasPrefix(head, []byte{0x7F, 'E', 'L', 'F'}),    // ELF
		bytes.HasPrefix(head, []byte{0xFE, 0xED, 0xFA, 0xCE}), // Mach-O 32/64 大端
		bytes.HasPrefix(head, []byte{0xFE, 0xED, 0xFA, 0xCF}),
		bytes.HasPrefix(head, []byte{0xCE, 0xFA, 0xED, 0xFE}), // Mach-O 小端
		bytes.HasPrefix(head, []byte{0xCF, 0xFA, 0xED, 0xFE}),
		bytes.HasPrefix(head, []byte("#!")),                   // 脚本
		bytes.HasPrefix(head, []byte{0xCA, 0xFE, 0xBA, 0xBE}): // Java class
		return "", true
	case bytes.HasPrefix(head, []byte{0x89, 'P', 'N', 'G'}), // PNG
		bytes.HasPrefix(head, []byte{0xFF, 0xD8, 0xFF}), // JPEG
		bytes.HasPrefix(head, []byte("GIF8")):           // GIF
		return "image", false
	case isBMP(head):
		return "image", false
	case bytes.HasPrefix(head, []byte("RIFF")) && len(head) >= 12 && bytes.Equal(head[8:12], []byte("WEBP")):
		return "image", false
	case bytes.HasPrefix(head, []byte("RIFF")) && len(head) >= 12 && bytes.Equal(head[8:12], []byte("WAVE")):
		return "audio", false
	case bytes.HasPrefix(head, []byte("ID3")),
		len(head) >= 2 && head[0] == 0xFF && head[1]&0xE0 == 0xE0 && head[1]&0x06 != 0: // MP3 帧同步
		return "audio", false
	case bytes.HasPrefix(head, []byte("OggS")):
		return "audio", false
	case len(head) >= 8 && bytes.Equal(head[4:8], []byte("ftyp")):
		return "ftyp", false
	case bytes.HasPrefix(head, []byte{0x1A, 0x45, 0xDF, 0xA3}): // Matroska/WebM
		return "webm", false
	case bytes.HasPrefix(head, []byte("%PDF")):
		return "pdf", false
	case bytes.HasPrefix(head, []byte("PK\x03\x04")), // ZIP 容器（docx/xlsx）
		bytes.HasPrefix(head, []byte("PK\x05\x06")),
		bytes.HasPrefix(head, []byte("PK\x07\x08")):
		return "zip", false
	case isTextHead(head):
		return "text", false
	default:
		return "", false // 未知类型：调用方按「族不匹配」拒绝
	}
}

// isBMP 识别 BMP 头部：'BM' + 保留字段全零 + 合法头长度（避免把以 BM 开头的文本误判为图片）。
func isBMP(head []byte) bool {
	return len(head) >= 18 && bytes.HasPrefix(head, []byte{'B', 'M'}) &&
		bytes.Equal(head[6:10], []byte{0, 0, 0, 0}) &&
		(bytes.Equal(head[14:18], []byte{12, 0, 0, 0}) || bytes.Equal(head[14:18], []byte{40, 0, 0, 0}) ||
			bytes.Equal(head[14:18], []byte{108, 0, 0, 0}) || bytes.Equal(head[14:18], []byte{124, 0, 0, 0}))
}

// isTextHead 判断头部是否为文本（无 NUL、无控制字符）：供 csv/txt/json 使用。
func isTextHead(head []byte) bool {
	if len(head) == 0 {
		return false
	}
	for _, c := range head {
		if c == 0x00 {
			return false
		}
		if c < 0x20 && c != '\t' && c != '\n' && c != '\r' {
			return false
		}
	}
	return true
}

// SniffImage 只认 png/jpeg/webp（图标专用），返回扩展名与落库 MIME。
//
// 为什么图标单独一份：
//   - 比 uploads 更严 —— gif/bmp 不收。图标是 16–64px 的小图，
//     gif 会动（选择器里一跳一跳）、bmp 体积是 png 的 10 倍以上（512KB 上限下
//     几乎不能用），两者都没有实际收益；
//   - **SVG 必须先跌进 text 族再被拒**：`<svg>` 可内嵌 `<script>`，
//     用 <img> 引用时脚本不执行，但运维会把同一个 URL 直接点开看，
//     那一刻就是同源脚本。项目已有同样取舍（controller/task.go 不内联 image/svg+xml）。
//
// 复用同一张魔数表（sniff），不另写一份字节前缀判断 —— 两份魔数表的经典结局是
// 一份支持 webp 一份不支持，同一张图在两个入口一边能传一边被拒。
func SniffImage(head []byte) (ext, mime string, ok bool) {
	family, danger := sniff(head)
	if danger || family != "image" {
		return "", "", false
	}
	switch {
	case bytes.HasPrefix(head, []byte{0x89, 'P', 'N', 'G'}):
		return "png", "image/png", true
	case bytes.HasPrefix(head, []byte{0xFF, 0xD8, 0xFF}):
		return "jpg", "image/jpeg", true
	case bytes.HasPrefix(head, []byte("RIFF")) && len(head) >= 12 && bytes.Equal(head[8:12], []byte("WEBP")):
		return "webp", "image/webp", true
	}
	// GIF / BMP：family 是 image 但不在图标白名单内。
	return "", "", false
}
