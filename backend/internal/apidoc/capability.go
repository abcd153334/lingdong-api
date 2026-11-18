package apidoc

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"lingdong-api/internal/service/billing"
)

// Cap 是模型的**能力声明**（`models.capabilities` 的元素）。
//
// 为什么需要它：文档要按能力出「预设」（文生视频 / 图生视频 / 首尾帧 / 多模态），
// 而「这个模型到底收不收参考音频」既不在 endpoints（视频只有一个 video/generations），
// 也不在 spec_prices（那里只有价目，且没有参考音频价 ≠ 不支持参考音频）。
// 真实判断分散在适配器档案里，但 apidoc 不能 import relay（见 kind.go 的包注释）。
// 因此能力必须是一条**模型事实**，与 endpoints / spec_prices 同层。
//
// 这是封闭词表：新增能力必须同时加在这里（校验、预设、文档章节都读它）。
type Cap string

const (
	// 图像
	CapT2I         Cap = "t2i"              // 文生图
	CapI2I         Cap = "i2i"              // 图生图 / 编辑
	CapImageStream Cap = "image_stream"     // 流式出图（逐帧返回）
	CapImageLayer  Cap = "image_layer"      // 图层拆分（底图 + 图层）
	// CapImageGroup 表示「组图」（一次调用连续出多张关联图）。
	//
	// 必须与 CapImageNative 分开：image_native 只说明"收厂商原生报文"
	// （图层拆分也是原生报文），方舟 5.0 pro / flash 就是「有原生参数、有图层、
	// **不支持组图**」；lite 反过来「支持组图、不支持图层」。
	// 用 image_native 当组图判据会把 pro/flash 的组图请求按 15 张冻结。
	CapImageGroup Cap = "image_group"
	CapImageSearch Cap = "image_web_search" // 联网搜索（生成前检索）
	// CapImageNative 表示「收厂商原生图片报文」（组图 / 图层拆分 / 提示词优化等
	// 非 OpenAI 字段），与视频的 CapVideoNative 对称。
	//
	// 它同时是「即梦格式」那篇文档的**生效范围判据**：没有这个能力的模型
	// （gpt-image 系、nano-banana 系）收到那些字段只会忽略或 400，
	// 把它们列进同一篇文档的 model 枚举就是对一半模型说假话。
	CapImageNative Cap = "image_native"
	// 视频
	CapT2V         Cap = "t2v"          // 文生视频
	CapI2V         Cap = "i2v"          // 图生视频（单图首帧）
	CapFirstLast   Cap = "first_last"   // 首尾帧
	CapRefAudio    Cap = "ref_audio"    // 参考音频（配音 / 多模态）
	CapRefVideo    Cap = "ref_video"    // 参考视频（续写 / 视频重绘）
	CapVideoAudio  Cap = "video_audio"  // 有声视频（生成音轨）
	CapVideoNative Cap = "video_native" // 原生参数（content[] 透传）
	// 音乐
	CapMusic  Cap = "music"  // 作曲
	CapLyrics Cap = "lyrics" // 写词
)

// CapabilityDef 是一条能力的元信息。
type CapabilityDef struct {
	Key   Cap
	Label string
	// Modalities 是允许声明该能力的模态（校验用）。
	Modalities []string
	// Endpoint 声明该能力要求模型支持的端点写法（空串 = 无要求）。
	// 校验条件：声明了能力就必须声明这个端点，否则能力永远用不到（静默失效）。
	Endpoint string
}

// capabilityDefs 是能力词表的**唯一来源**。
//
// 排序即文档与预设的展示顺序：图像 → 视频（生成 → 图生 → 首尾帧 → 音频 → 视频）→ 音乐。
var capabilityDefs = []CapabilityDef{
	{CapT2I, "文生图", []string{"image"}, "images/generations"},
	{CapI2I, "图生图", []string{"image"}, ""},
	{CapImageStream, "流式出图", []string{"image"}, "images/generations"},
	{CapImageLayer, "图层拆分", []string{"image"}, "images/generations"},
	{CapImageGroup, "组图", []string{"image"}, "images/generations"},
	{CapImageSearch, "联网搜索", []string{"image"}, "images/generations"},
	{CapImageNative, "原生参数", []string{"image"}, "images/generations"},
	{CapT2V, "文生视频", []string{"video"}, "video/generations"},
	{CapI2V, "图生视频", []string{"video"}, "video/generations"},
	{CapFirstLast, "首尾帧", []string{"video"}, "video/generations"},
	{CapRefAudio, "参考音频", []string{"video"}, "video/generations"},
	{CapRefVideo, "参考视频", []string{"video"}, "video/generations"},
	{CapVideoAudio, "有声视频", []string{"video"}, "video/generations"},
	{CapVideoNative, "原生参数", []string{"video"}, "video/generations"},
	{CapMusic, "作曲", []string{"music"}, "audio/music/generations"},
	{CapLyrics, "写词", []string{"music"}, "audio/lyrics"},
}

var capabilityIndex = func() map[Cap]CapabilityDef {
	out := make(map[Cap]CapabilityDef, len(capabilityDefs))
	for _, d := range capabilityDefs {
		out[d.Key] = d
	}
	return out
}()

// CapabilityDefs 返回词表副本（顺序即展示顺序）。
func CapabilityDefs() []CapabilityDef {
	out := make([]CapabilityDef, len(capabilityDefs))
	copy(out, capabilityDefs)
	return out
}

// Capabilities 返回全部合法能力键（供管理台下拉与 AI 契约使用）。
func Capabilities() []Cap {
	out := make([]Cap, 0, len(capabilityDefs))
	for _, d := range capabilityDefs {
		out = append(out, d.Key)
	}
	return out
}

// DefaultCapabilities 是「未声明」时的安全兜底。
//
// 语义区分（关键）：
//   - `models.capabilities IS NULL` = 从未声明（如运营手工新建的模型）→ 给最小可用集，
//     否则文档里连「文生视频」预设都没有，用户以为这个模型不能用；
//   - `[]` = 明确声明「没有额外能力」→ 尊重声明，不给兜底。
//
// 兜底只给最基础的那一个（图/视频/音乐各一条），绝不给参考素材类能力：
// 猜错「支持参考音频」会让用户按文档传参然后 400，比少一个 TAB 糟得多。
func DefaultCapabilities(modality string) []Cap {
	switch NormalizeModality(modality) {
	case "image":
		return []Cap{CapT2I}
	case "video":
		return []Cap{CapT2V}
	case "music":
		return []Cap{CapMusic}
	default:
		return nil
	}
}

// NormalizeCapabilities 解析并归一 capabilities 列。
//
// 容错策略：未知词、模态不匹配的词**直接丢弃**（文档不能因为一条脏配置就 500）；
// 保存期由 ValidateCapabilities 拦截，两者分工明确。
func NormalizeCapabilities(modality string, raw json.RawMessage) []Cap {
	mod := NormalizeModality(modality)
	if len(raw) == 0 {
		return DefaultCapabilities(mod)
	}
	var keys []string
	if err := json.Unmarshal(raw, &keys); err != nil {
		return DefaultCapabilities(mod)
	}
	out := make([]Cap, 0, len(keys))
	seen := make(map[Cap]bool, len(keys))
	for _, k := range keys {
		c := Cap(strings.ToLower(strings.TrimSpace(k)))
		if c == "" || seen[c] {
			continue
		}
		d, ok := capabilityIndex[c]
		if !ok || !modalityAllowed(d, mod) {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return sortCaps(out)
}

// sortCaps 按词表顺序排序（保证签名与文档稳定；map 遍历无序会让每次刷新都变）。
func sortCaps(in []Cap) []Cap {
	if len(in) == 0 {
		return nil
	}
	order := make(map[Cap]int, len(capabilityDefs))
	for i, d := range capabilityDefs {
		order[d.Key] = i
	}
	out := append([]Cap(nil), in...)
	sort.SliceStable(out, func(i, j int) bool { return order[out[i]] < order[out[j]] })
	return out
}

func modalityAllowed(d CapabilityDef, modality string) bool {
	for _, m := range d.Modalities {
		if m == modality {
			return true
		}
	}
	return false
}

// HasCap 判断能力集合是否包含某条。
func HasCap(caps []Cap, c Cap) bool {
	for _, x := range caps {
		if x == c {
			return true
		}
	}
	return false
}

// ValidateCapabilities 是**保存期**校验（管理台与智能体共用）。
//
// 三条：词必须在表内、词必须与模态匹配、不得重复。
// 「能力要求的端点必须已声明」由 ValidateCapabilitiesAgainstEndpoints 单独做。
func ValidateCapabilities(modality string, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var keys []string
	if err := json.Unmarshal(raw, &keys); err != nil {
		return fmt.Errorf("能力声明必须是字符串数组")
	}
	return ValidateCapabilityKeys(modality, keys)
}

// ValidateCapabilityKeys 是 ValidateCapabilities 的切片版本。
//
// 为什么要有两个入口：管理台拿到的是 JSONB 原文，智能体的 plan 拿到的是已解析的
// `[]string`。让后者先 Marshal 回去只为了校验，等于把「序列化错误」混进
// 「配置错误」的报错里，运营会看到「能力声明必须是字符串数组」而明明填的是数组。
func ValidateCapabilityKeys(modality string, keys []string) error {
	mod := NormalizeModality(modality)
	seen := make(map[Cap]bool, len(keys))
	var bad []string
	for _, k := range keys {
		c := Cap(strings.ToLower(strings.TrimSpace(k)))
		d, ok := capabilityIndex[c]
		if c == "" || !ok {
			bad = append(bad, "未定义能力 "+string(c))
			continue
		}
		if !modalityAllowed(d, mod) {
			bad = append(bad, fmt.Sprintf("能力 %s 不适用于 %s 模态", c, mod))
			continue
		}
		if seen[c] {
			bad = append(bad, "能力重复 "+string(c))
			continue
		}
		seen[c] = true
	}
	if len(bad) > 0 {
		return fmt.Errorf("%s", strings.Join(bad, "；"))
	}
	return nil
}

// ValidateCapabilitiesAgainstEndpoints 额外校验「能力要求的端点已声明」。
//
// 单独一个函数而不是塞进 ValidateCapabilities：保存模型时 endpoints 可能为空
// （按模态派生默认端点），此时**必须跳过**而不是判非法——
// 空 endpoints 的语义是「用该模态的默认端点」，图像默认里就有 images/edits。
// 在这里报非法会让「不填端点的模型保存不了任何带端点的能力」，运营只能去填一个
// 本来就该留空的字段。
func ValidateCapabilitiesAgainstEndpoints(caps []Cap, endpoints []string) error {
	if len(endpoints) == 0 {
		return nil
	}
	have := make(map[string]bool, len(endpoints))
	for _, e := range endpoints {
		have[NormalizeEndpointKey(e)] = true
	}
	var bad []string
	for _, c := range caps {
		d, ok := capabilityIndex[c]
		if !ok || d.Endpoint == "" {
			continue
		}
		if !have[d.Endpoint] {
			bad = append(bad, fmt.Sprintf("能力 %s 需要端点 %s", c, d.Endpoint))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("%s", strings.Join(bad, "；"))
	}
	return nil
}

// CapKeys 把能力集合转成落库/传输用的字符串切片。
//
// **返回非 nil 空切片**：`[]` 与 NULL 在语义上不同——`[]` 表示「已声明、没有额外能力」
// （读路径尊重声明，不再兜底），NULL 表示「从未声明」（读路径给最小兜底）。
// 若这里返回 nil，一次「明确关掉某个 TAB」的保存会变成 null，下次启动又被兜底打开。
func CapKeys(caps []Cap) []string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		out = append(out, string(c))
	}
	return out
}

// CapLabels 把能力集合翻成中文标签（文档「能力速览」用）。
func CapLabels(caps []Cap) []string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		if d, ok := capabilityIndex[c]; ok {
			out = append(out, d.Label)
		}
	}
	return out
}

// SuggestCapabilities 从已有事实**推荐**一份能力清单。
//
// 只在两个地方用，**读路径永不用它**：
//  1. 启动时补齐 `models.capabilities IS NULL` 的行（`docdocs.EnsureCapabilities`，
//     收敛式、不落迁移标记；补完之后这一列就是唯一真相源）；
//  2. 管理台模型表单的「按事实推荐」按钮（运营点一下填进去，再改）。
//
// 为什么读路径不许用它：推荐值来自「端点 + 参考素材计价位」，而这两者对能力的
// 表达能力是不完整的（例：`images/edits` 端点存在不等于该模型真能改图；
// 没有参考音频价也不等于不收参考音频——可能含在时长价里）。一旦读时动态推导，
// 运营在管理台点「不支持参考视频」就会被代码覆盖回去，且**没有任何报错**。
//
// 推导规则（每条都能指到一条事实）：
//   - 图像：能出图 → t2i；能改图/变体 → i2i；
//   - 视频：能提交任务 → t2v；配了参考图价或免费张数 → i2v（不收参考图就不会配这个价）；
//     配了参考音频价 → ref_audio；参考视频按秒或按条计费 → ref_video；
//   - 音乐：music / lyrics 端点各自对应一条能力。
//
// **endpoints 必须传 DeclaredEndpoints 的结果，不能传 NormalizeEndpoints**：
// 后者带模态兜底（图像默认含 `images/edits`），会把所有图像模型推荐成支持图生图。
//
// first_last 推导不出来（它和 i2v 用同一个端点、同一个价），只能人工声明：
// 猜错会让用户传 image_tail 然后 400，比少一个 TAB 糟。
func SuggestCapabilities(modality string, endpoints []string, spec billing.SpecFacts) []Cap {
	mod := NormalizeModality(modality)
	have := make(map[string]bool, len(endpoints))
	for _, e := range endpoints {
		if k := NormalizeEndpointKey(e); k != "" {
			have[k] = true
		}
	}
	var out []Cap
	add := func(c Cap) { out = append(out, c) }

	switch mod {
	case "image":
		if have["images/generations"] {
			add(CapT2I)
		}
		if have["images/edits"] || have["images/variations"] {
			add(CapI2I)
		}
	case "video":
		if have["video/generations"] || have["video/regenerations"] {
			add(CapT2V)
		}
		if spec.HasRefImagePrice || spec.FreeRefImagesSet {
			add(CapI2V)
		}
		if spec.HasRefAudioPrice {
			add(CapRefAudio)
		}
		if len(spec.RefVideoSecondTiers) > 0 || spec.RefVideoPerClip {
			add(CapRefVideo)
		}
	case "music":
		if have["audio/music/generations"] {
			add(CapMusic)
		}
		if have["audio/lyrics"] {
			add(CapLyrics)
		}
	}
	if len(out) == 0 {
		return DefaultCapabilities(mod)
	}
	return sortCaps(out)
}
