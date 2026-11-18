package apidoc

// 本文件定义右栏「调用方式」（预设）的数据结构。
//
// **预设表本身不在这里**：每个预设都属于某一族的一篇文档，写在那篇文档旁边
// （见 family_*.go 的 `Presets`）。曾经有一张全局 `presetTable`，靠
// `Endpoint + Variant` 两个键去"认领"，还带一个从未被读取的 `Requires`：
//
//   - 归属靠两处协商 ⇒ 同一路由有几族就有几次误配（Seedream 的页面上
//     出现过「流式出图」，而网关对非流式适配器是主动剥掉 stream 的）；
//   - `Requires` 声明了"这个 TAB 该不该出现"的判据，却没有任何代码读它 ——
//     等于伪造了一层保障：评审时以为已经过滤，实际不过滤。
//
// 现在「这个预设属于哪篇文档」由所在位置直接决定，不需要键，也无法误配。
//
// 两条边界仍然成立：
//   - **不含金额**：预设只决定"发什么请求"，不决定价格；
//   - **Body 里的 model 是占位符**：投影期替换成该文档的代表模型。

// PresetModelToken 是 Body 里 model 的占位符。
//
// 不能在表里写死模型 id：一篇文档服务一族模型，写死就是给每个模型配一份表，
// 而其中 90% 的内容逐字相同。
const PresetModelToken = "$model"

// 两个「随模型事实变化」的取值占位符（投影期解析，表里保持稳定）。
const (
	presetTokenSeconds    = "$seconds"
	presetTokenResolution = "$resolution"
)

// Preset 是一个运行预设。
type Preset struct {
	// Key 稳定且唯一：前端 TAB 的选中态与 e2e 断言都靠它，改 key 等于改前端契约。
	Key     string `json:"key"`
	Label   string `json:"label"`
	Summary string `json:"summary"`
	// Body 是预填的请求体（保序，model 为 PresetModelToken）。
	Body Obj `json:"body"`
	// Fields 是该预设会写入的字段（含 Uploads）：前端据此高亮「这个 TAB 改了哪些参数」。
	Fields []string `json:"fields"`
	// Uploads 是需要用户选本地文件的 form 字段（图片编辑的 multipart）。
	//
	// 单独声明而不是"运行时看 TypeFile"：右栏要在**发送之前**就知道缺文件，
	// 否则用户点了发送，收到的是一条 400，而原因在另一个控件里。
	Uploads []string `json:"uploads,omitempty"`
	// Note 是 TAB 上的一句话提示（坑/限制），可空。
	Note string `json:"note,omitempty"`
}
