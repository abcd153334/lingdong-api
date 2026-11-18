package adapters

// 内置适配器的展示元数据（唯一来源）。
//
// 为什么是静态表而不是让每个适配器实现 Label()/Vendor()：
// 每个内置适配器各写一遍等于把同一张表拆成多处，评审时看不到全貌，
// 也很容易出现「新加一个适配器忘了写 label」——而缺 label 的后果是管理台
// 显示一个英文 id。覆盖性由 TestBuiltinMetaCoversRegistry 守（缺一个就红）。
//
// 这里**只放展示信息**：能力矩阵来自 Adapter.Caps()，端点支持来自
// EndpointSupporter，二者都不许在这里重复声明。
type Meta struct {
	// Label 是中文展示名。
	Label string
	// Vendor 是厂商标识（与 vendors.id 同一套写法；未登记也能显示）。
	Vendor string
	// Desc 是一句话说明（它在管理台卡片上，不对外）。
	Desc string
	// DefaultBaseURL 是该适配器的渠道默认 Base URL（空 = 没有默认值，必须由运营填写）。
	//
	// 为什么放这里而不是前端常量：Base URL 是**适配器事实**（路径前缀由上游协议决定），
	// 前端再抄一份的结果是上游改前缀时只改了一处，另一处继续拼出 404。
	DefaultBaseURL string
	// Icon 是图标**令牌**（"" | builtin:<key>），不是地址。
	//
	// 地址由 service/iconref 在出口处换算：令牌是存储表示，不下发给展示方。
	// 空 = 没有合适的图标，前端退首字占位 —— 宁可空，也不要一个必然 404 的地址。
	//
	// 为什么写真实 key 而不是"留空让前端按 vendor 找"：vendor 是模型厂商
	// （bytedance/openai），适配器是接入方式（火山方舟/多米/AutoDL），两者不是一回事；
	// 且 duomi/autodl/metaso 在 vendors 表里根本没有行，回落必然落空。
	Icon string
}

var metas = map[string]Meta{
	"openai": {
		Label:  "OpenAI 兼容",
		Vendor: "openai",
		Desc:   "标准 OpenAI 协议（含流式对话、嵌入、TTS、ASR、重排、审核、图像、视频）",
		Icon:   "builtin:openai-gpt",
	},
	"duomiapi": {
		Label:  "多米 API",
		Vendor: "duomi",
		Desc:   "多米聚合（视频/图像/音乐），部分能力为同步轮询桥接",
		Icon:   "builtin:duomi",
	},
	"autodl": {
		Label:  "AutoDL",
		Vendor: "autodl",
		Desc:   "AutoDL 大规模模型平台（视频/图像，异步任务）",
		Icon:   "builtin:autodl",
	},
	"metaso": {
		Label:  "秘塔 AI",
		Vendor: "metaso",
		Desc:   "秘塔 AI 搜索增强（视频/多模态，异步任务）",
		// 图库没有 metaso 条目（catalog 与 icons/ 都没有）。显式写空串而不是
		// 猜一个不存在的 key：坏 key 会被 iconref 拒成 ""，前端只会安静退首字，
		// 半年后没人分得清是笔误还是缺图。
		Icon: "",
	},
	"volcengine-ark": {
		Label: "火山方舟",
		// Vendor 与 models.vendor 用同一套写法：方舟是豆包系列模型的接入入口，
		// 这里再引入一个 volcengine 会让同一家公司在清单里出现两个厂商。
		Vendor:         "bytedance",
		Desc:           "火山方舟（豆包图片/视频）：图片支持流式出图，视频为异步任务",
		DefaultBaseURL: arkDefaultBase,
		// 用方舟自己的标而不是 bytedance（Vendor 借的是模型厂商，图标应该认接入方）。
		Icon: "builtin:volcengine",
	},
}

// MetaOf 返回内置适配器的展示元数据。
func MetaOf(name string) (Meta, bool) {
	m, ok := metas[name]
	return m, ok
}

// Metas 返回全部元数据（供测试与清单接口使用，按名字排序由调用方负责）。
func Metas() map[string]Meta { return metas }
