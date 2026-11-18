package task

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// 本文件是**视频提交入口专属**的归一：把素材写成 5 个自解释的顶层字段
// （`image` / `audio` / `video` / `first_frame` / `last_frame`），再折叠进
// `input_reference`。
//
// 为什么不改 normalizeSubmitBody：那个函数被图像 / 音乐 / 视频共用，`image`
// 在图像端点上是一个真字段（部分适配器直接转发），在那里删键会改图像模型行为。
//
// 三条规矩：
//  1. **顶层键无条件删除**。AutoDL 有未知顶层键闸门（`autodl.go` 的
//     `autodlRejectUnknownKeys`），metaso 的 `metasoLocalOnly` 也不含这五个键 ——
//     留着会被判未知键 400，或被原样透传给上游。
//  2. **折叠不改变计费类别**。五个字段落在既有五个 type 上
//     （image/audio/video/first_frame/last_frame），`refKind` 一行不用改。
//     这是"新增写法不改金额"的结构性保证。
//  3. **已经写在 input_reference 里的项原样保留**：老写法继续有效，
//     新写法只是它的便捷入口；两者同义时去重，冲突时报错（不静默择一）。
//
// 「生成模式」不在这里判定：它由素材构成决定（有音频→多模态、有首尾帧→首尾帧、
// 有图→图生、否则文生），判定点在**各适配器**里 —— metaso 靠 content 组成，
// AutoDL 靠 `planAutoDLRequest` 的 mode 推断。网关只负责把素材摆成统一的内部形状。

// videoRefTypes 是 `input_reference[].type` 的封闭集。
//
// 前三项是平台规范写法，后三项是厂商原生 role（原生 content[] 转换而来）。
// **未知 type 必须报错而不是兜底成图片**：`refKind` 对不认识的 type 会退回
// "按哪个链接非空推断"，于是 `type:"imag"` 会被当成参考图按张计费 ——
// 拼错一个字母就多收钱，而账单上看不出原因。这张表就是计费防线。
var videoRefTypes = map[string]bool{
	"image": true, "audio": true, "video": true,
	"first_frame": true, "last_frame": true, "base_video": true,
	"reference_image": true, "reference_audio": true, "reference_video": true,
}

// videoRefTypeLink 是每种 type 必须使用的链接键。
//
// 写成表而不是 if 链：`type=image` 配 `audio_url` 这类错配只有一处能拦，
// 而它的后果是"按图片计费、给上游发音频链接"——两边都说不通。
var videoRefTypeLink = map[string]string{
	"image":           "image_url",
	"reference_image": "image_url",
	"first_frame":     "image_url",
	"last_frame":      "image_url",
	"audio":           "audio_url",
	"reference_audio": "audio_url",
	"video":           "video_url",
	"reference_video": "video_url",
	"base_video":      "video_url",
}

// 素材 type 的字面量（与 videoRefTypes 的键同值，单独抽出以免拼错）。
const (
	refTypeFirstFrameName = "first_frame"
	refTypeLastFrameName  = "last_frame"
	refTypeBaseVideoName  = "base_video"
)

// videoRefFolder 描述一个顶层素材字段如何折叠成 input_reference 的一项。
type videoRefFolder struct {
	// Key 是顶层字段名。
	Key string
	// Type 是折叠后写进 `type` 的值。
	Type string
	// Link 是该 type 对应的链接键（同时用于判重）。
	Link string
}

// videoRefFolders 的**顺序即折叠顺序**，也是一个隐式契约：
// 同类素材的相对顺序决定 AutoDL 的 ref_image_0..N 下标，
// 所以顺序变了就是"同一份请求喂给上游的图换了位"。不要重排。
var videoRefFolders = []videoRefFolder{
	{"image", "image", "image_url"},
	{"audio", "audio", "audio_url"},
	{"video", "video", "video_url"},
	{"first_frame", refTypeFirstFrameName, "image_url"},
	{"last_frame", refTypeLastFrameName, "image_url"},
}

// normalizeVideoBody 做视频入口的别名收敛与素材折叠。
//
// 与 normalizeSubmitBody 的三处差别（都是有意的）：
//   - 别名映射后**删除原键**（见 TestVideoAliasesAreDeletedFromBody）；
//   - 支持 `image` / `audio` / `video` 写成**字符串数组**（一个字段装多份素材）；
//   - `input_reference[].type` 收紧为封闭集，且与链接键必须自洽。
func normalizeVideoBody(body []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	// 厂商原生体（非空 content[]）整体原样返回：它的语义是"除 model 外逐字节转发"，
	// 往里塞顶层别名就是给上游送它不认识的字段。
	if arr, ok := m["content"].([]any); ok && len(arr) > 0 {
		return body, nil
	}

	// ── 别名：映射后删原键 ──
	if _, has := m["seconds"]; !has {
		if f, ok := jsonNumber(m["duration"]); ok {
			m["seconds"] = json.Number(strconv.FormatFloat(f, 'f', -1, 64))
		}
	}
	delete(m, "duration")

	// `size` 是 resolution 的兼容写法，**必须先折算再删键**。
	// 改造前的洞：只删不折，于是 `size=480p` 到了适配器手里 resolution 是空的，
	// AutoDL 直接 400「不支持的分辨率「」」—— 而文档承诺「自动映射」。真机 D6 抓到。
	if _, has := m["resolution"]; !has {
		if sz, ok := m["size"].(string); ok && strings.TrimSpace(sz) != "" {
			m["resolution"] = strings.TrimSpace(sz)
		} else if _, hasSize := m["size"]; !hasSize {
			w, wok := jsonNumber(m["width"])
			h, hok := jsonNumber(m["height"])
			if wok && hok && w > 0 && h > 0 {
				m["resolution"] = fmt.Sprintf("%dx%d", int(w), int(h))
			}
		}
	}
	delete(m, "size")
	delete(m, "width")
	delete(m, "height")

	if err := foldVideoMaterialFields(m); err != nil {
		return nil, err
	}
	if err := validateVideoRefs(m); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

// foldVideoMaterialFields 把五个顶层素材字段折叠进 input_reference。
//
// 折叠出的项放在**队首**（与改造前 `image` 别名的行为一致）：同类素材的相对顺序
// 决定上游的下标，保持队首就不会让"同时用了 input_reference 与别名"的请求换序。
func foldVideoMaterialFields(m map[string]any) error {
	var folded []any
	for _, f := range videoRefFolders {
		raw, exists := m[f.Key]
		if !exists {
			continue
		}
		// 先删键：无论值是空、是非法类型还是正常折叠，这个顶层键都不该出站。
		delete(m, f.Key)

		links, err := videoRefLinks(raw, f.Key)
		if err != nil {
			return err
		}
		for _, url := range links {
			skip, err := checkVideoRefDuplicate(m, f.Type, f.Link, url)
			if err != nil {
				return err
			}
			if skip {
				continue // 同款同链接已在数组里：不重复落库，也不重复计费
			}
			folded = append(folded, map[string]any{"type": f.Type, f.Link: url})
		}
	}
	if len(folded) == 0 {
		return nil
	}
	existing, _ := m["input_reference"].([]any)
	m["input_reference"] = append(folded, existing...)
	return nil
}

// videoRefLinks 把一个顶层字段读成链接列表。
//
// 接受字符串（单份）或字符串数组（多份）；空值返回空列表。其它类型一律报错 ——
// 静默跳过会让用户以为素材传上了（而计费按实际张数）。
func videoRefLinks(v any, field string) ([]string, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		if s := strings.TrimSpace(x); s != "" {
			return []string{s}, nil
		}
		return nil, nil
	case []any:
		out := make([]string, 0, len(x))
		for i, item := range x {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("%w：%s 的第 %d 项必须是字符串（公网直链）",
					ErrInvalidParam, field, i+1)
			}
			s = strings.TrimSpace(s)
			if s == "" {
				return nil, fmt.Errorf("%w：%s 的第 %d 项是空字符串", ErrInvalidParam, field, i+1)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%w：%s 只能是字符串或字符串数组", ErrInvalidParam, field)
	}
}

// checkVideoRefDuplicate 处理"顶层字段与 input_reference 里已有同款素材"的三种情况。
//
//   - 同 type 同链接：已经在列表里，跳过（幂等，避免同一张图既写 image 又写 input_reference 时算两遍）；
//   - 首/尾帧同 type 不同链接：报错。改造前是静默保留数组里那个，用户以为换了图；
//   - 其它类型同 type 不同链接：允许（它们本来就是列表，没有"选哪个"的歧义）。
func checkVideoRefDuplicate(m map[string]any, typ, link, url string) (bool, error) {
	refs, ok := m["input_reference"].([]any)
	if !ok {
		return false, nil
	}
	for _, raw := range refs {
		obj, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(refString(obj["type"])), typ) {
			continue
		}
		if strings.TrimSpace(refString(obj[link])) == url {
			return true, nil
		}
		if typ == refTypeFirstFrameName || typ == refTypeLastFrameName {
			return false, fmt.Errorf("%w：%s 出现了两个不同链接，只保留一个", ErrInvalidParam, typ)
		}
	}
	return false, nil
}

// validateVideoRefs 校验素材数组的每一项。
//
// 四类问题在这里一次拦住（都对应一类真实故障）：
//  1. 项不是对象 —— 落库与出站两边都会炸在别处，错误信息不可解释；
//  2. 出现 `role` —— 该写法已废弃（用途改用顶层 `first_frame` / `last_frame` 表达），
//     静默忽略会让 `role=first_frame` 退化成按张收费的参考图；
//  3. `type` 不在封闭集，或与链接键不自洽；
//  4. 一项里填了多个链接 / 一个都没填 —— 按哪条出站、按哪条计费没有隐式优先级。
func validateVideoRefs(m map[string]any) error {
	refs, ok := m["input_reference"].([]any)
	if !ok {
		if _, exists := m["input_reference"]; exists {
			return fmt.Errorf("%w：input_reference 必须是数组", ErrInvalidParam)
		}
		return nil
	}
	baseCount := 0
	for i, raw := range refs {
		obj, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("%w：input_reference 的第 %d 项必须是对象", ErrInvalidParam, i+1)
		}
		if _, has := obj["role"]; has {
			return fmt.Errorf("%w：input_reference[].role 已废弃 —— 用途请改用顶层 "+
				"first_frame / last_frame，参考图请写 image，参考音频写 audio，参考视频写 video",
				ErrInvalidParam)
		}
		typ := strings.ToLower(strings.TrimSpace(refString(obj["type"])))
		if typ == "" {
			// 不写 type 是历史写法，交给 refKind 按链接推断（行为不变）。
			if err := checkSingleVideoRefLink(obj, i); err != nil {
				return err
			}
			continue
		}
		typ = normalizeRefTypeName(typ)
		if !videoRefTypes[typ] {
			return fmt.Errorf("%w：input_reference[].type 不支持 %q（可用：image / audio / video / "+
				"first_frame / last_frame / base_video）", ErrInvalidParam, typ)
		}
		if err := checkSingleVideoRefLink(obj, i); err != nil {
			return err
		}
		want := videoRefTypeLink[typ]
		for _, link := range []string{"image_url", "audio_url", "video_url"} {
			if link == want {
				continue
			}
			if strings.TrimSpace(refString(obj[link])) != "" {
				return fmt.Errorf("%w：input_reference 的第 %d 项 type=%s 却填了 %s，应为 %s",
					ErrInvalidParam, i+1, typ, link, want)
			}
		}
		if typ == refTypeBaseVideoName {
			baseCount++
			if baseCount > 1 {
				return fmt.Errorf("%w：input_reference 里最多只能有一个底片（base_video）", ErrInvalidParam)
			}
		}
	}
	return nil
}

// checkSingleVideoRefLink 要求一项素材「有且只有一个链接」。
func checkSingleVideoRefLink(obj map[string]any, i int) error {
	n := 0
	for _, link := range []string{"image_url", "audio_url", "video_url"} {
		if strings.TrimSpace(refString(obj[link])) != "" {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("%w：input_reference 的第 %d 项必须且只能填一个地址"+
			"（image_url / audio_url / video_url 三选一），当前有 %d 个", ErrInvalidParam, i+1, n)
	}
	return nil
}

// normalizeRefTypeName 把连字符写法折成下划线写法（与 inputRefType 同一口径）。
func normalizeRefTypeName(t string) string {
	switch t {
	case "first-frame":
		return refTypeFirstFrameName
	case "last-frame":
		return refTypeLastFrameName
	case "base-video":
		return refTypeBaseVideoName
	}
	return t
}

// refString 把任意 JSON 值读成字符串（非字符串返回空串）。
func refString(v any) string {
	s, _ := v.(string)
	return s
}
