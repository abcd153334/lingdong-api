package relay

// 本文件是同步产物直链的**唯一改写出口**。
//
// 为什么放在 relay 出口而不是各适配器里：
//   - 适配器只该关心「怎么把上游的响应变成 OpenAI 形状」，一旦它们各自改写，
//     新增一个渠道就要重新实现一遍签名，漏掉的那个渠道会继续泄漏上游域名；
//   - call_io / 调用记录需要的是**上游原文**（排查时要知道到底谁给的图、给的什么地址），
//     而用户看到的必须是平台地址——这两件事只有在「采集完、写出前」这唯一时点能分开。

import (
	"encoding/json"

	"lingdong-api/internal/pkg/mediaurl"
)

// returnsImageURLs 判断端点响应是否带 `data[].url`。
//
// 只列同步图像三兄弟：chat/completions 的 markdown 里也可能有图片地址，
// 但那是**模型输出的一部分**，改写会改变回答内容本身（用户会看到一段被替换过的 prose）。
func returnsImageURLs(k endpointKind) bool {
	switch k {
	case kindImagesGenerations, kindImagesEdits, kindImagesVariations:
		return true
	}
	return false
}

// rewriteImageFrameURL 把**单帧** SSE data 里的 url 换成本站能力地址。
//
// 为什么只改 url 一个键：同帧里还有 image_index / size / model / type，
// 前端靠 image_index 排序、靠 size 展示，丢字段会直接错位。
//
// 代价说明（不要误以为是零代价）：含 url 的帧会走一次 map 往返，**键顺序变成
// 字典序**（JSON 对象本身无序，客户端不该依赖顺序）；值逐字节保留（RawMessage 直传），
// 不含 url 的帧走零拷贝透传，字节完全保真。
//
// 返回 ok=false 表示「本帧不需要/无法改写」，调用方必须原样透传：
// 把解析失败当成错误会让一帧心跳把整条流打断。
func rewriteImageFrameURL(data []byte, base string) ([]byte, bool) {
	if base == "" || len(data) == 0 {
		return data, false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return data, false
	}
	raw, ok := m["url"]
	if !ok {
		return data, false
	}
	var u string
	if json.Unmarshal(raw, &u) != nil || u == "" {
		return data, false
	}
	link, ok := mediaurl.Link(base, u, mediaurl.TTL())
	if !ok {
		return data, false
	}
	enc, err := json.Marshal(link)
	if err != nil {
		return data, false
	}
	m["url"] = enc
	out, err := json.Marshal(m)
	if err != nil {
		return data, false
	}
	return out, true
}
