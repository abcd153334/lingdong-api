package apidoc

import "lingdong-api/internal/pkg/modality"

// Section 是文档站侧栏的 L1 板块。
//
// 为什么独立一张表、不再从 Groups() 派生：
//
//	Groups() 是「模型广场」的分类（语言模型 / 图片模型 / 向量与检索 …），它的名字
//	服务的是"卖什么"，而文档站要的是"调什么"——同一个能力在两边叫不同名字是常态
//	（模型广场叫「语言模型」，文档站要拆成「聊天（Chat）」与「补全（Completions）」）。
//	早先直接取 Groups() 的 Name，导致改一个板块名要动两个产品面，且 chat 与
//	completions 永远挤在同一块里。
//
// 名称与英文名成对出现（参考实现是「音频（Audio）」）：英文名只是标题里的括号内容，
// **不再是排序键**。顺序由产品决定（语言 → 图片 → 视频 → 音频 → 音乐）：协议篇讲
// 「怎么调」，其余讲「调什么」，必须紧跟「开始使用 / 通用接口」之后；
// 若按英文名 ASCII 排序，「协议类」会落到音频之后，与读者的心智顺序相反。
type Section struct {
	ID string `json:"id"`
	// En 是英文名（标题里的括号内容，不参与排序）。
	En string `json:"en"`
	// Name 是中文名。
	Name string `json:"name"`
	// Short 是标题前缀用的短名（「图像生成」而不是「图片模型生成」）。
	Short string `json:"-"`
	Color string `json:"color"`
	Desc  string `json:"desc"`
	// Container 是它属于哪个容器（start / general / ai）。
	//
	// 容器是一个纯展示层：把能力板块收进「AI 模型接口」一项，否则侧栏一级
	// 会同时出现「开始使用 / 通用接口 / 语言 / 图片 / …」7 项，其中 5 项同一性质。
	Container string `json:"container"`
}

// 两个容器 id（start/general 各自是容器也是板块，AI 容器下挂 9 个能力板块）。
const (
	ContainerStart   = "start"
	ContainerGeneral = "general"
	ContainerAI      = "ai"
)

// 两个文档站专有板块（模型广场没有对应分类）。
const (
	// SectionStart 是「开始使用」：鉴权、Base URL、通用约定。
	SectionStart = "start"
	// SectionGeneral 是「通用接口」：模型清单 / 文件与产物 / 任务列表 / 账户与用量。
	SectionGeneral = "general"
	// SectionProtocols 是「语言」：**固定**的协议文档（OpenAI 兼容 / Claude 原生 / Embeddings），
	// 与具体模型解耦 —— 这几篇讲的是"请求长什么样"，不会因为上架一个模型就多一篇。
	SectionProtocols = "protocols"
)

// sectionOrderList 是**全部板块的唯一顺序表**（侧栏从上到下的真实顺序）。
//
// 为什么明写顺序而不是每次 sort：排序若在渲染期算，前后端各算一次就有两套结果
// （Go 的 sort.Slice 不稳定，相等键的顺序会随实现变）。这里声明一次，
// 测试 `TestSectionProductOrder` 守「能力板块的产品顺序与中文名」，改错就红。
//
// 为什么 start/general 也在表里：它们与能力板块共用同一个 `Section` 结构与同一套
// 折叠渲染，只放在另一张表里就会重演「容器有顺序、板块有顺序、两张表不同步」的旧问题
// （`TestEverySectionInOrderList` 守：定义表里的每个板块都必须在这张表里）。
var sectionOrderList = []string{
	SectionStart,     // 开始使用
	SectionGeneral,   // 通用接口
	SectionProtocols, // 语言（固定 3 篇，见 protocols.go）
	"images",         // 图片
	"videos",         // 视频
	"audio",          // 音频
	"music",          // 音乐
}

// containersMeta 是容器的声明（顺序即渲染顺序；容器不带 sort）。
//
// 容器只是展示分组：`start` 与 `general` 各自只有一个板块（名字与容器相同，
// 前端因此不重复画板块行），`ai` 装 9 个能力板块。
var containersMeta = []struct {
	ID   string
	Name string
}{
	{ID: ContainerStart, Name: "开始使用"},
	{ID: ContainerGeneral, Name: "通用接口"},
	{ID: ContainerAI, Name: "AI 模型接口"},
}

// aiSectionOrder 返回能力板块的顺序（从唯一顺序表派生，不再维护第二份清单）。
//
// 判据是 `Section.Container == ContainerAI`：新增能力板块只要在定义表里写对容器，
// 这里自动包含；漏写容器则由 `TestEverySectionInOrderList` 直接报红。
func aiSectionOrder() []string {
	out := make([]string, 0, len(sectionOrderList))
	for _, id := range sectionOrderList {
		if s, ok := SectionOf(id); ok && s.Container == ContainerAI {
			out = append(out, id)
		}
	}
	return out
}

// docOnlySections 是全部板块定义（含两个非模型板块）。
var docOnlySections = map[string]Section{
	SectionStart: {ID: SectionStart, En: "", Name: "开始使用", Short: "开始", Color: "#0ea5e9",
		Desc: "密钥、地址与通用约定", Container: ContainerStart},
	SectionGeneral: {ID: SectionGeneral, En: "", Name: "通用接口", Short: "通用", Color: "#6366f1",
		Desc: "模型清单、文件、任务与账户", Container: ContainerGeneral},
	"audio": {ID: "audio", En: "Audio", Name: "音频", Short: "语音", Color: "#f59e0b",
		Desc: "语音合成 / 语音识别 / 翻译", Container: ContainerAI},
	"images": {ID: "images", En: "Images", Name: "图片", Short: "图片", Color: "#8b5cf6",
		Desc: "文生图 / 图生图 / 异步出图", Container: ContainerAI},
	"music": {ID: "music", En: "Music", Name: "音乐", Short: "音乐", Color: "#14b8a6",
		Desc: "音乐生成 / 歌词生成", Container: ContainerAI},
	SectionProtocols: {ID: SectionProtocols, En: "Protocols", Name: "语言", Short: "语言", Color: "#6366f1",
		Desc: "OpenAI 兼容 / Claude 原生 / Embeddings —— 固定协议文档", Container: ContainerAI},
	"videos": {ID: "videos", En: "Videos", Name: "视频", Short: "视频", Color: "#ec4899",
		Desc: "异步任务：提交 → 查询 → 下载", Container: ContainerAI},
}

// functionalGroups 是 Groups() 里**不是模型类型**的功能组，不进侧栏一级。
// 它们只在「通用接口」板块里作为子组出现（模型清单 / 文件 / 任务 / 账户）。
var functionalGroups = map[string]bool{"file": true, "task": true, "acct": true, "model": true}

// groupSections 是「导航组 → 板块」的唯一映射。
//
// Transport.Group 仍是 lang/vec/image/video/audio/music/safe（它们同时被
// `GroupOf` 用来取模型广场配色与排序），所以保留一层映射而不是去改 endpointTable
// 的 40 行取值 —— 改那些值会牵动 `endpointErrors` 的 switch 与 `groupRank` 排序。
//
// lang/vec/safe 全部落 `protocols`：语言类不再按端点拆板块（那会让「补全」「重排」
// 「审核」三个几乎没模型的板块长年空着），它们只是同一篇协议文档里的章节。
var groupSections = map[string]string{
	"lang":  SectionProtocols,
	"vec":   SectionProtocols,
	"image": "images",
	"video": "videos",
	"audio": "audio",
	"music": "music",
	"safe":  SectionProtocols,
}

// sectionOrder 返回侧栏渲染顺序（唯一顺序表的副本，调用方改不到内部表）。
func sectionOrder() []string {
	return append([]string(nil), sectionOrderList...)
}

// Sections 返回文档站侧栏板块（顺序即渲染顺序）。
func Sections() []Section {
	out := make([]Section, 0, len(docOnlySections))
	for _, id := range sectionOrder() {
		if s, ok := SectionOf(id); ok {
			out = append(out, s)
		}
	}
	return out
}

// SectionOf 返回板块定义；未知或功能组返回 false。
func SectionOf(id string) (Section, bool) {
	if s, ok := docOnlySections[id]; ok {
		return s, true
	}
	if functionalGroups[id] {
		return Section{}, false
	}
	return Section{}, false
}

// SectionNames 返回 id → 名称（投影与校验共用，避免各处再拼一次）。
func SectionNames() map[string]string {
	out := make(map[string]string, len(docOnlySections))
	for _, s := range Sections() {
		out[s.ID] = s.Name
	}
	return out
}

// SectionOfGroup 把导航组翻成板块（**兜底**，仅用于没有显式板块的端点）。
//
// lang/vec 是一对多，真实板块由 SectionOfKind 决定；这里返回的是"该组所属的
// 第一个板块"，只应出现在「按组批量处理」的场景（如管理台筛选）。
func SectionOfGroup(group string) string {
	if functionalGroups[group] {
		return SectionGeneral
	}
	if sec, ok := groupSections[group]; ok {
		return sec
	}
	return SectionGeneral
}

// SectionOfKind 按端点决定所属板块（**文档站板块归属的唯一实现**）。
//
// 判据只有 `groupSections` 一份映射：音乐与音频虽然共用 `music` 组名也能正确落位，
// 新增端点只要在 endpointTable 写对 Group，板块归属自动成立。
func SectionOfKind(k Kind) string {
	t, ok := TransportOf(k)
	if !ok {
		return SectionGeneral
	}
	return SectionOfGroup(t.Group)
}

// SectionOfModel 返回模型所属板块（模型文档的落点）。
//
// 语言类模态（chat / embedding / rerank / moderation）统一返回 SectionProtocols：
// 它们**不建模型文档**，只出现在固定的协议篇里（见 DocModeOf）。
// 未知模态返回空串 —— 不落任何板块，避免生成一个 `section=general` 的幽灵族。
func SectionOfModel(f ModelFacts) string {
	if modality.Language(f.Modality) {
		return SectionProtocols
	}
	switch NormalizeModality(f.Modality) {
	case "image":
		return "images"
	case "video":
		return "videos"
	case "audio":
		return "audio"
	case "music":
		return "music"
	}
	return ""
}

// SectionNameOf 取板块名（未知板块返回 id 本身）。
func SectionNameOf(id string) string {
	if s, ok := SectionOf(id); ok {
		return s.Name
	}
	return id
}

// SectionEnOf 取板块英文名（未知返回空串）。
func SectionEnOf(id string) string {
	if s, ok := SectionOf(id); ok {
		return s.En
	}
	return ""
}

// DocMode 是「一个模型的文档落在哪」的唯一判据。
//
// 只有两种文档形态：协议篇（语言/向量类，与模型解耦）与模型族（厂商 × 板块）。
// 早先还有第三种「OpenAI 兼容单例组」，它让语言模型也必须先有个"族"，
// 结果 Claude 原生协议连章节都进不去（/v1/messages 没有 EndpointKey）。
type DocMode int

const (
	// DocModeNone 是不建文档（未知/空模态）。
	DocModeNone DocMode = iota
	// DocModeProtocol 落固定协议篇（openai / claude / embeddings）。
	DocModeProtocol
	// DocModeFamily 落模型族（图像 / 视频 / 音频 / 音乐）。
	DocModeFamily
)

// DocModeOf 判定模型该进协议篇还是模型族。
func DocModeOf(f ModelFacts) DocMode {
	switch SectionOfModel(f) {
	case SectionProtocols:
		return DocModeProtocol
	case "":
		return DocModeNone
	}
	return DocModeFamily
}

// IsAISection 判断板块是否属于「AI 模型能力」（start/general 不是）。
//
// 判据只有 aiSectionOrder 一份：新增能力板块时只要登记在这里，
// 容器归属、英文名校验、读面挂载三处自动跟上。
func IsAISection(id string) bool {
	s, ok := SectionOf(id)
	return ok && s.Container == ContainerAI
}
