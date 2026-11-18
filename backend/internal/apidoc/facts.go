package apidoc

import (
	"encoding/json"
	"time"

	"lingdong-api/internal/model"
	"lingdong-api/internal/service/billing"
)

// ModelFacts 是文档投影需要的**全部模型事实**，且全部已归一。
//
// 为什么不直接把 *model.Model 传给投影：模型上有二十多个计费比例字段，
// 投影只用得到其中一小部分；而少了归一（模态空串、端点写法、spec_prices 解析）
// 就会让投影与网关目录各说各话。这个结构是「模型事实的唯一读取口」，
// 由 FactsOf 一次性完成归一 + 解析。
type ModelFacts struct {
	ID          string
	DisplayName string
	Vendor      string
	// Icon 是模型**生效**的图标 token（已回落供应商）。
	//
	// 回落必须在装载期算完（`docdocs` 装载时补）：投影层拿不到供应商表，
	// 让每个投影各自回落就是 N 份 COALESCE，最后必有几处漏掉。
	Icon string
	// Description 是 models.description（一句人话差异说明，如「出图更快、单价更高」）。
	//
	// 它是文档组「成员表」的默认说明：同组模型参数完全一样，用户选型的唯一依据
	// 就是这句话。为空时成员表只显示 ID —— 不是错误，但运营该补。
	Description string
	// Modality 已归一：空串按 chat 处理（与模态闸门一致）。
	Modality string
	// Endpoints 已归一、去重、保序；未声明时按模态派生默认端点。
	Endpoints []string
	// Capabilities 是该模型的能力声明（`models.capabilities`），已归一：
	// 词表内、模态匹配、按词表顺序排序。未声明时按模态给最小兜底（见 DefaultCapabilities）。
	Capabilities []Cap
	// BillingMode 是计费方式（tokens / call / spec），与 models.billing_mode 同值。
	BillingMode string
	// Spec 是 spec_prices 的唯一解析结果（billing.SpecFacts）。
	//
	// 这里**不允许**再 unmarshal 一次：四处各自解析是 B0 前的真实状况，
	// 后果是「预估按 A 口径、详情按 B 口径」。
	Spec billing.SpecFacts
	// SpecRequests 是**本接入声明**的扩展请求字段（键 = 端点键，不含 @poll）。
	//
	// 来源是规格 DSL 的 endpoint.request_fields：它是"调用方在本模型上还能传什么"
	// 的唯一事实。合并进投影后，文档字段表、调试台表单、`field_keys` 都从同一处读，
	// 不再需要各处单独问"这个模型是不是规格接入"。
	SpecRequests map[string][]SpecRequestField
	// Tags 是营销标签（已过滤到期项），文档成员表直接渲染 label + color_hex。
	//
	// 复用 model.ModelTagView 而不是拷一份：它的形状就是对外契约本身，
	// 拷贝只会多一处需要同步的字段表。
	Tags []model.ModelTagView
}

// SpecRequestField 是一个扩展字段的投影形态（从 specdsl.RequestField 拷字段而来）。
//
// 为什么拷贝而不是直接引用 specdsl 的类型：apidoc 是文档契约层，不该把
// 运行期 DSL 的结构（以及它未来的字段）带进公网投影；拷贝也天然保证了
// 「哪些字段能下发」是显式列出来的。
type SpecRequestField struct {
	Name     string
	Type     string
	Required bool
	Default  any
	Enum     []string
	Desc     string
	Example  any
	MaxLen   int
}

// FactsOf 从模型行派生文档事实。
//
// 纯函数，无 IO；nil 输入返回零值（调用方自己判 ID 是否为空）。
func FactsOf(m *model.Model) ModelFacts {
	if m == nil {
		return ModelFacts{}
	}
	return ModelFacts{
		ID:          m.ID,
		DisplayName: m.DisplayName,
		Vendor:      m.Vendor,
		Icon:        m.Icon,
		Description: m.Description,
		Modality:    NormalizeModality(m.Modality),
		// 只认**显式声明**：模态兜底会让没填端点的模型凭空"支持"两个接口，
		// 文档列得出来、渠道却跑不通（whisper 出现在语音合成页就是这么来的）。
		Endpoints:    DeclaredEndpoints(json.RawMessage(m.Endpoints)),
		Capabilities: NormalizeCapabilities(m.Modality, json.RawMessage(m.Capabilities)),
		BillingMode:  m.BillingMode,
		// 取生效价目：非 spec 模式的历史残留 spec_prices 不能参与分组判据，
		// 否则一个「按次」模型会因为残留档位价被分到「按张」文档组里。
		Spec: billing.ParseSpecFacts(m.EffectiveSpecPrices()),
		// 文档站与门户必须同一套过期口径：这里用 ActiveTags，不自己比时钟。
		Tags: model.ActiveTags(model.DecodeTags(m.Tags), time.Now()),
	}
}

// HasEndpoint 判断模型是否声明了某端点（已归一的写法）。
func (f ModelFacts) HasEndpoint(key string) bool {
	key = NormalizeEndpointKey(key)
	for _, e := range f.Endpoints {
		if e == key {
			return true
		}
	}
	return false
}

// HasCap 判断模型是否声明了某能力。
//
// 归一化在装载期已完成（NormalizeCapabilities），这里只做包含判断：
// 再解析一次会让「文档按装配后的事实判定」和「管理台按声明判定」分裂成两个答案。
func (f ModelFacts) HasCap(c Cap) bool {
	for _, x := range f.Capabilities {
		if x == c {
			return true
		}
	}
	return false
}

// Kinds 返回模型声明的端点里**已登记契约**的那些 Kind（按端点表顺序）。
//
// 未登记的写法会被跳过而不是报错：`models.endpoints` 是运营手填的自由文本，
// 库里有历史脏值（如已下线的写法）。文档页只展示能展示的，脏值由
// 管理台的覆盖率体检单独报出来，不该让整页 500。
func (f ModelFacts) Kinds() []Kind {
	var out []Kind
	for _, k := range Kinds() {
		t, ok := TransportOf(k)
		if !ok || t.EndpointKey == "" {
			continue
		}
		if f.HasEndpoint(t.EndpointKey) {
			out = append(out, k)
		}
	}
	return out
}

// ExtraEndpoints 返回模型声明的端点里「不属于该模态默认端点」的那部分写法。
//
// 用于文档里标注「这个模型额外支持哪些端点」——例如一个 chat 模型同时声明了
// embeddings，用户必须知道它可以被两种方式调用。
func (f ModelFacts) ExtraEndpoints() []string {
	def := make(map[string]bool, 4)
	for _, e := range DefaultEndpoints(f.Modality) {
		def[NormalizeEndpointKey(e)] = true
	}
	var out []string
	for _, e := range f.Endpoints {
		if !def[e] {
			out = append(out, e)
		}
	}
	return out
}

// kindIsRead 判断该端点是不是「读面」（查询任务 / 取产物 / 取消）。
func kindIsRead(k Kind) bool {
	t, ok := TransportOf(k)
	return ok && t.Phase == PhaseRead
}

// hasKind 判断模型是否声明了某个端点。
//
// 声明即能力：`models.endpoints` 是运营在管理台点的，与 `Transport` 的
// 「平台有没有这条路由」是两件事。协议篇的模型清单靠它过滤。
func hasKind(f ModelFacts, k Kind) bool {
	for _, mk := range f.Kinds() {
		if mk == k {
			return true
		}
	}
	return false
}

// HasTaskFace 判断该模型是否支持「异步任务」这一面（提交后拿任务号，再查询取产物）。
//
// 两种形态都必须算进来：
//   - `Phase==PhaseSubmit`：视频 / 音乐 / 再生成，端点本身就是任务；
//   - `AcceptsAsync`：图像，同一路径加 `async:true` 才走任务。
//
// **这是唯一判据，不要在别处各写一遍 `HasPhase(PhaseSubmit)`**。漏掉 AcceptsAsync
// 的后果不是「少一个入口」而是一连串静默错位：侧栏不显示「查询任务状态」、
// 文档错误码少掉 410/409/504 三条、异步读面清单为空 —— 每一处单看都「没报错」。
func HasTaskFace(f ModelFacts) bool {
	for _, k := range f.Kinds() {
		t, ok := TransportOf(k)
		if !ok {
			continue
		}
		if t.Phase == PhaseSubmit || t.AcceptsAsync {
			return true
		}
	}
	return false
}
