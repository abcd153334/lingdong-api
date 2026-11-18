package relay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// 图片参考图的字段折叠：`image_urls`（nano 系官方字段名）→ `image`（平台唯一入口）。
//
// 为什么必须是一个函数而不是两处代码：
//
//	同一个请求体有两条入口 —— 同步（relayRequestIn 直接解析 relayHead）和
//	异步（task.Submit → normalizeSubmitBody）。此前折叠只写在异步那一条上，
//	于是 `image_urls` 在同步链路上既不被 relayHead 读到（它只认 `image`）、
//	又被适配器的 localOnly 白名单当"平台内部键"删掉 —— 用户按官方文档传参考图，
//	拿到一张纯文生图，账单一切正常，没有任何报错。
//
// 语义边界：
//   - 只做「字段名归一」，不判断模型是否支持图生图（那是能力闸门的事）；
//   - `image` 与 `image_urls` 同时出现是**冲突**（400），不静默取其一；
//   - `image` 允许字符串（单张）与字符串数组（多张）两种形态，数组不再拆开。
func FoldImageURLsMap(m map[string]any) error {
	raw, has := m["image_urls"]
	if !has {
		return nil
	}
	if _, hasImage := m["image"]; hasImage {
		return fmt.Errorf("image_urls 与 image 只能二选一（image 一张写字符串、多张写数组）")
	}
	arr, ok := raw.([]any)
	if !ok {
		return fmt.Errorf("image_urls 必须是字符串数组（单张请用 image）")
	}
	for _, it := range arr {
		s, ok := it.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return fmt.Errorf("image_urls 的每个元素都必须是非空字符串（公网直链或 data: 内联）")
		}
	}
	if len(arr) > 0 {
		m["image"] = arr
	}
	delete(m, "image_urls")
	return nil
}

// FoldImageURLsBody 是 FoldImageURLsMap 的字节版（同步网关只有原文，没有 map）。
//
// 解析失败时原样返回原文与 nil：报文本身不是合法 JSON 的判定归调用方
// （它会用同一个 json.Unmarshal 报出 invalid_param），这里不该抢答。
func FoldImageURLsBody(body []byte) ([]byte, error) {
	// UseNumber 不是洁癖：map[string]any 会把整数解成 float64，
	// 重新序列化后 12345678901234 会变成 1.2345678901234e+13 —— 出站报文被改写。
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return body, nil
	}
	if _, has := m["image_urls"]; !has {
		// 绝大多数请求没有这个键：原文返回，连键序都不动（出站报文与客户端一致）。
		return body, nil
	}
	if err := FoldImageURLsMap(m); err != nil {
		return body, err
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body, nil
	}
	return out, nil
}
