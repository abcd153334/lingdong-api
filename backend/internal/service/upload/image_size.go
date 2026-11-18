package upload

import "encoding/binary"

// ImageSize 从文件头解析 PNG / JPEG / WebP 的像素尺寸（宽, 高）。
//
// 为什么必须有这一层：`MaxBytes`（512KB）是**字节**上限，它拦不住
// 「8000×8000 纯色 PNG」——纯色 + 最高压缩比实测只有几十 KB，字节校验一路绿灯。
// 这种图上传成功、库里每个字段都正常，直到选择器一次性渲染 72+N 张缩略图时
// 把浏览器的解码内存打满：表现是「打开图标选择器就卡死 / 页面崩溃」，
// 而服务端没有一条错误日志可查。
//
// 解析失败返回 ok=false，**由调用方跳过尺寸校验**（不因此拒图）：
// 三个格式的尺寸都在固定位置，解析不出来只可能是文件被截断或伪造，
// 而截断的图在渲染时本来就会裂掉 —— 为它另编一个错误码没有额外收益。
func ImageSize(data []byte) (w, h int, ok bool) {
	switch {
	case len(data) >= 24 && string(data[:4]) == "\x89PNG":
		return pngSize(data)
	case len(data) >= 4 && data[0] == 0xFF && data[1] == 0xD8:
		return jpegSize(data)
	case len(data) >= 16 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		// 门槛只到 fourcc（16 字节）：三种 WebP 形态各自的最短长度不同
		// （VP8L 只需 25 字节），在这里卡 30 会把无损 WebP 全部判成"解析不出"。
		return webpSize(data)
	}
	return 0, 0, false
}

// pngSize 读 IHDR：PNG 规范要求 IHDR 是第一个 chunk（偏移 12 的 4 字节类型 + 16 起的宽高）。
func pngSize(b []byte) (int, int, bool) {
	if len(b) < 24 || string(b[12:16]) != "IHDR" {
		return 0, 0, false
	}
	w := int(binary.BigEndian.Uint32(b[16:20]))
	h := int(binary.BigEndian.Uint32(b[20:24]))
	if w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// jpegSize 逐段走到帧头（SOF）取尺寸。
//
// 为什么是"逐段走"而不是"搜 FFC0/FFC2"：EXIF 里嵌着一张完整的缩略图 JPEG，
// 它有自己的 SOF。直接搜标记会把缩略图（160×120）当成原图的尺寸 ——
// 于是 8000×8000 的照片被判成 160×120 放行，校验等于没做。
func jpegSize(b []byte) (int, int, bool) {
	i := 2 // 跳过 SOI
	for i+9 < len(b) {
		if b[i] != 0xFF {
			return 0, 0, false // 段结构已错乱，不再猜
		}
		m := b[i+1]
		switch {
		case m == 0xFF: // 标记前的填充字节
			i++
			continue
		case m == 0xD8, m == 0x01, m >= 0xD0 && m <= 0xD7: // 无长度字段的独立标记
			i += 2
			continue
		case m == 0xDA: // SOS 之后是熵编码数据，段头不再成立
			return 0, 0, false
		}
		segLen := int(binary.BigEndian.Uint16(b[i+2 : i+4]))
		if segLen < 2 {
			return 0, 0, false
		}
		// SOF0–SOF15 除 DHT(C4) / JPG(C8) / DAC(CC)：段内 1 字节精度、2 字节高、2 字节宽。
		if m >= 0xC0 && m <= 0xCF && m != 0xC4 && m != 0xC8 && m != 0xCC {
			h := int(binary.BigEndian.Uint16(b[i+5 : i+7]))
			w := int(binary.BigEndian.Uint16(b[i+7 : i+9]))
			if w <= 0 || h <= 0 {
				return 0, 0, false
			}
			return w, h, true
		}
		i += 2 + segLen
	}
	return 0, 0, false
}

// webpSize 按 chunk 类型解析三种 WebP 形态（有损 / 无损 / 扩展）。
//
// 三种都要认：只认 VP8 的话，运营从设计工具导出的 VP8L（"无损"）会被判成
// 「没有尺寸」而跳过校验，正好放过体积最大的一类。
func webpSize(b []byte) (int, int, bool) {
	switch string(b[12:16]) {
	case "VP8 ": // 有损：帧标签 3 字节 + 起始码 9D 01 2A + 14 位宽 / 14 位高
		if len(b) < 30 || b[23] != 0x9D || b[24] != 0x01 || b[25] != 0x2A {
			return 0, 0, false
		}
		w := int(binary.LittleEndian.Uint16(b[26:28]) & 0x3FFF)
		h := int(binary.LittleEndian.Uint16(b[28:30]) & 0x3FFF)
		if w == 0 || h == 0 {
			return 0, 0, false
		}
		return w, h, true
	case "VP8L": // 无损：0x2F 签名 + 紧接 14 位(宽-1) / 14 位(高-1) 的小端位流
		if len(b) < 25 || b[20] != 0x2F {
			return 0, 0, false
		}
		bits := binary.LittleEndian.Uint32(b[21:25])
		return int(bits&0x3FFF) + 1, int((bits>>14)&0x3FFF) + 1, true
	case "VP8X": // 扩展：1 字节标志 + 3 字节保留 + 3 字节(画布宽-1) + 3 字节(画布高-1)
		if len(b) < 30 {
			return 0, 0, false
		}
		w := int(uint32(b[24])|uint32(b[25])<<8|uint32(b[26])<<16) + 1
		h := int(uint32(b[27])|uint32(b[28])<<8|uint32(b[29])<<16) + 1
		return w, h, true
	}
	return 0, 0, false
}
