package apidoc

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// 本文件定义契约层的字段模型（L1 的第二个维度：传输形态在 kind.go，字段结构在这里）。
//
// 三条边界（改之前先读）：
//  1. **不含任何上游私有键**。这里描述的是「平台对外归一后的字段」，与厂商无关。
//     厂商专属字段一律标 TierVendor，并在 Desc 里写清「只对哪些模型有效」。
//  2. **不含金额**。价格是 models.spec_prices 的事实，由 project.go 从 billing.SpecFacts
//     注入。契约里写死价格，改价后文档必然失真（这是 v1 方案被判否的原因之一）。
//  3. **不落库**。L2b（model_docs）只存人话文案；字段结构永远只有这一份。
//
// 为什么要区分 In（body/query/path/form）：运行面板要按位置把控件生成到不同区，
// 而"字段名里带（查询参数）"这种写法在 data.ts 里已经证明了不可靠——它既不能
// 被代码消费，也经不起 AI 编排机器读。

// Type 是字段的 JSON 类型（file/binary 不是 JSON 类型，但运行面板需要区分）。
type Type string

const (
	TypeString  Type = "string"
	TypeInteger Type = "integer"
	TypeNumber  Type = "number"
	TypeBoolean Type = "boolean"
	TypeArray   Type = "array"
	TypeObject  Type = "object"
	// TypeFile 表示 multipart 表单项里的文件（运行面板生成 file 控件）。
	TypeFile Type = "file"
	// TypeBinary 只用于返回字段：响应体是二进制流。
	TypeBinary Type = "binary"
	// TypeNone 只用于返回字段：响应体为空（如 204）。
	//
	// 与 TypeBinary 分开是必要的：204 没有响应体，示例里若按"二进制"处理，
	// 用户会去读一个空的 Body，而按"JSON"处理更糟——会直接解析失败。
	TypeNone Type = "none"
	// TypeAny 用于「类型随厂商」的透传字段，运行面板给 JSON 编辑器。
	TypeAny Type = "any"
)

// In 是字段出现的位置。运行面板据此分区，Markdown 据此标注。
type In string

const (
	InBody  In = "body"
	InQuery In = "query"
	InPath  In = "path"
	InForm  In = "form"
)

// Required 是三态必填。为什么不是 bool：video 的 prompt 与 base_video 是
// 「二选一」，music 的 prompt 与 lyrics 也是——写成 bool 就只能二选一错一个，
// 而写成「必填」会让用户以为两个都要给。
type Required string

const (
	// ReqRequired 必填。
	ReqRequired Required = "required"
	// ReqOptional 选填。
	ReqOptional Required = "optional"
	// ReqConditional 条件必填，条件写在 Field.RequiredNote。
	ReqConditional Required = "conditional"
)

// Tier 是字段的来源分层（对外展示为徽标，回答「这个字段是谁认的」）。
type Tier string

const (
	// TierPlatform 平台字段：网关强校验（计费/路由/白名单），填错直接报错。
	TierPlatform Tier = "P"
	// TierStandard 标准字段：OpenAI 标准，透传。
	TierStandard Tier = "S"
	// TierVendor 厂商扩展：透传，只有对应厂商的模型认识。
	TierVendor Tier = "V"
	// TierAlias 兼容别名：new-api 写法的等价字段，网关自动改写。
	TierAlias Tier = "N"
)

// EnumSource 说明枚举值的来源。
//
// **这是 NeedOwnDoc 判定的核心依据**（见 judge.go）：只有当枚举来自模型事实
// （即同一字段在不同模型上取值不同）时，这个端点的文档才算「模型专属」。
// 若把「有枚举」当成差异，size 的 1024x1024 这类静态枚举会让全部模型都被判成
// 需要独立文档，判定直接失效。
type EnumSource string

const (
	// EnumStatic 静态枚举：写在 Field.Enum 里，所有模型一样。
	EnumStatic EnumSource = "static"
	// EnumImageTier 图片档位：取值与单价来自 spec_prices 的档位表。
	EnumImageTier EnumSource = "image_tier"
	// EnumVideoTier 视频清晰度：取值与秒价来自 spec_prices 的清晰度档位表。
	EnumVideoTier EnumSource = "video_tier"
	// EnumRefVideoTier 参考视频档位：来自 ref_video_second_price 的键。
	EnumRefVideoTier EnumSource = "ref_video_tier"
	// EnumGroupMembers 成员模型：取值 = 本篇文档的全部成员 ID。
	//
	// 与上面三个「随模型事实变化」的枚举不同：它不是单个模型的事实，而是**文档组**的成员表，
	// 因此 IsModelDriven 必须对它返回 false —— 否则 judge.go 规则 7 会把每个含 model 字段的
	// 端点判成「需要独立文档」，所有分组退化成单模型。
	EnumGroupMembers EnumSource = "group_members"
)

// IsModelDriven 表示枚举随**该模型自己的**事实变化。
//
// EnumGroupMembers 刻意不算：它随「文档组」变化，与「这个模型是否需要独立文档」无关。
func (s EnumSource) IsModelDriven() bool {
	return s != "" && s != EnumStatic && s != EnumGroupMembers
}

// Constraint 是数值/长度约束。运行面板据此生成 min/max/step 与前置校验。
type Constraint struct {
	// Kind 是约束种类：min / max / min_len / max_len / multiple_of。
	Kind  string
	Value float64
	// Text 是给人看的写法（如 "≤ 4000 字符"），Markdown 直接用它，避免二次格式化。
	Text string
}

// Field 是契约里的一个字段。
//
// Name 是**投影后的字段路径**，规则：
//   - 顶层字段：`prompt`
//   - 数组元素字段：`input_reference[].type`
//   - 嵌套对象：`metadata.aigc_watermark`
//
// 这套写法同时被三处消费：model_docs.field_notes 的 key 校验（V4）、
// 运行面板的 data-field 锚点、Markdown 的字段表。三者必须完全一致，
// 所以它是常量而不是"大概这么写"。
type Field struct {
	Name string
	Type Type
	In   In
	// Required 是三态必填；ReqConditional 时 RequiredNote 必须非空（测试守）。
	Required     Required
	RequiredNote string
	// Default 是默认值的人话写法（"1"、"模型默认"、"自动检测"）。
	// **不参与表单预填**：预填只认 EndpointDoc.Example，避免两处默认值分叉。
	Default string
	// Desc 是这个字段的**全部说明文字**（多行）。
	//
	// 曾经的 Desc + Note + Groups 三层结构已经合并：页面上读者看到的是一段
	// 连续的说明（写法、边界、比例→像素的换算表），后台编辑它的地方就是
	// 一个多行文本框。分成三层只会让「哪句话在哪个输入框里」变成一道谜题，
	// 而那张参考表在后台曾经根本没有输入框。
	Desc string
	// Enum 是静态枚举值（EnumSource=static 时有效）。
	Enum []string
	// EnumSource 见上。
	EnumSource EnumSource
	// Constraints 是数值/长度约束。
	Constraints []Constraint
	// Tier 是字段来源分层。
	Tier Tier
	// OnlyModels 是「哪些模型才认这个字段」的人话标注（如 "gpt-image 系列"），
	// 只用于 TierVendor 字段的展示，不做任何逻辑判定。
	//
	// 投影期会被统一覆盖成「部分模型」（见 projectField）：手写模型名会随能力
	// 声明漂移，而文档不会跟着变 —— 读者照着它写死分支，能力一改就错。
	OnlyModels string
	// Passthrough 表示平台**不校验**这个字段，只把它原样转发给模型提供方。
	//
	// 为什么需要它：`output_compression`、`partial_images` 这类字段在 relay 里
	// 没有任何读取点，文档若按"平台能力"的口吻写，读者会以为设了就一定生效。
	// 用布尔而不是在 Note 里加一句：Note 会被运营的 field_notes 叠加，
	// 同一句话散落三处容易改漏；布尔值可以被测试精确断言。
	Passthrough bool
	// SketchSkip 表示该字段只进字段表、不进响应骨架。
	//
	// 只为「同一位置二选一」的字段准备：对话补全的 `choices[].message.content`
	// 与 `choices[].delta.content` 不会同时出现（一个非流式、一个流式），
	// 拼进同一份骨架会造出一个永远不存在的结构 —— 而照着它写的解析代码必崩。
	SketchSkip bool
	// Example 是响应示例里这个字段的真值（nil = 用类型占位）。
	//
	// 为什么需要它：占位示例（`"<string>"` / `0`）能让读者看清结构，但看不出
	// 「任务号长什么样」「时间戳是秒还是毫秒」「hash 有没有前缀」。异步接口尤其如此 ——
	// 用户拿 `"<string>"` 写不出任何判断分支。
	//
	// 只对**返回字段**有意义；请求字段的初值只认 `EndpointDoc.Example`。
	// 值必须是真实形状（假数据但真类型），否则示例会诱导用户写错解析代码。
	Example any
}

// EndpointDoc 是一个端点的完整文档契约。
type EndpointDoc struct {
	Kind    Kind
	Name    string
	Summary string
	// Fields 是请求字段（In 区分 body/query/path/form）。
	Fields []Field
	// Returns 是返回字段。
	Returns []Field
	// Example 是**基础示例请求体**（保序）。运行面板的初值、curl 示例、
	// Markdown 示例全部取自这里，前端不得自己拼默认 JSON。
	//
	// 里面的 model 值写的是示例模型名；Project 会用模型事实里的真实 id 覆盖它。
	Example Obj
	// Example2 是第二个场景示例（可选），用于「同一接口的另一种用法」。
	Example2 Obj
	// ExampleTitle2 是 Example2 的场景名（如 "new-api 兼容写法"）。
	ExampleTitle2 string
	// Notes 是接口级注意事项（计费口径、坑、限制）。
	Notes []string
	// Response 是响应的结构性描述（页面与 Markdown 的唯一来源）。
	//
	// 只有「需要显式声明」的四件事才填：封套、流式、二进制类型、手写示例。
	// 其余一律从 Returns 派生（见 response.go）—— 让契约只写一次字段，别写两遍结构。
	Response ResponseSpec
	// RunMode 声明运行面板怎么发这个请求：json / sse / binary / none。
	RunMode RunMode
	// RunHint 解释「为什么这个接口不能在线试跑」，RunMode=none 时才有意义。
	//
	// 存在的理由：`RunNone` 的正确含义是「右栏发不出去」（缺真实任务号、
	// 或响应是二进制流），而不是「这个接口没有响应」。只把按钮藏掉而不给理由，
	// 用户会以为文档没写完 —— 而把它改成 RunJSON 更糟：按钮可点、id 为空、
	// 必然 404，用户会以为接口坏了。
	RunHint string
	// MaxBody 是请求体上限的人话写法（如 "25MB"），空串表示未限制。
	MaxBody string
}

// RunMode 是运行面板的发送方式。
type RunMode string

const (
	// RunJSON 发 JSON、按 JSON 渲染响应（含图片墙等结构化预览）。
	RunJSON RunMode = "json"
	// RunSSE 发 JSON、按事件流渲染（打字机）。
	RunSSE RunMode = "sse"
	// RunBinary 响应是二进制（音频/视频/文件），按 blob 渲染。
	RunBinary RunMode = "binary"
	// RunNone 不适合在线试跑（如 204 的取消、需要真实任务号的查询），只展示文档。
	RunNone RunMode = "none"
)

// Obj 是保序的 JSON 对象。
//
// 为什么不用 map[string]any：Go 的 map 序列化后键序随机，
// 示例里 `model` 会跑到 `prompt` 后面、`messages` 跑到 `stream` 后面。
// 文档示例是给人抄的，键序每次刷新都变会让人以为内容也变了。
type Obj []KV

// KV 是 Obj 的一项。
type KV struct {
	K string
	V any
}

// Get 取键值。
func (o Obj) Get(k string) (any, bool) {
	for _, kv := range o {
		if kv.K == k {
			return kv.V, true
		}
	}
	return nil, false
}

// With 返回替换某键后的副本（不改原对象）。
//
// 用于 Project 把示例里的 model 换成真实模型 id：契约里的示例是静态字面量，
// 而模型文档里必须出现用户真正要填的那个 id，否则「照着示例抄」必然 404。
func (o Obj) With(k string, v any) Obj {
	// 注意是 0 长度 + 容量提示，不是 make(Obj, len(o))：
	// 后者会把长度设成 len(o) 并塞进零值 KV，再 append 就变成「前面一排空键」。
	out := make(Obj, 0, len(o)+1)
	found := false
	for _, kv := range o {
		if kv.K == k {
			out = append(out, KV{K: k, V: v})
			found = true
			continue
		}
		out = append(out, kv)
	}
	if !found {
		out = append(Obj{{K: k, V: v}}, out...)
	}
	return out
}

// MarshalJSON 按声明顺序输出。
func (o Obj) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, kv := range o {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := marshalNoEsc(kv.K)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		vb, err := marshalNoEsc(kv.V)
		if err != nil {
			return nil, err
		}
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// marshalNoEsc 序列化单个值且**不转义 HTML**。
//
// 为什么不能用 json.Marshal：它会把 `<string>` 这类占位文本写成 `\u003cstring\u003e`。
// 文档示例和响应结构都要给人读，转义后既看不懂，复制进 Python 还得靠转义还原。
func marshalNoEsc(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// UnmarshalJSON 只在测试里用（把 JSON 字面量读回 Obj 便于断言键序）。
//
// 用 Decoder.Token() 逐键消费而不是先解进 map：json 包解 map 会丢键序，
// 那样「示例键序没被改动」这条断言就永远是绿的，等于没测。
// 嵌套对象递归读成 Obj，数组元素里的对象仍按 raw 保存（数组顺序由 JSON 保证）。
func (o *Obj) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("apidoc.Obj 只能解析 JSON 对象")
	}
	out := make(Obj, 0, 8)
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := kt.(string)
		if !ok {
			return fmt.Errorf("apidoc.Obj 的键不是字符串")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		var val any = raw
		if len(raw) > 0 && raw[0] == '{' {
			var sub Obj
			if err := json.Unmarshal(raw, &sub); err == nil {
				val = sub
			}
		}
		out = append(out, KV{K: key, V: val})
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	*o = out
	return nil
}
