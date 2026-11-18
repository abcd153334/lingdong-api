package apidoc

import (
	"sort"
	"strings"
)

// 本文件是**模型族**的唯一声明处：族有哪些、叫什么、在哪个板块、是不是已上架。
//
// 族**不携带接口清单**（曾经的 `Kinds` / `Variants` / `Caps` / `ReadFaces` 都已删除）：
// 接口文档按「族」各自成篇（见 family_endpoints.go），族的页面顺序就是那份清单的顺序，
// 再在族上写一遍端点列表就是第二个真相源 —— 加一族少写一条，页面就少一篇，且不报错。
//
// 为什么族必须写在代码里（而不是从 `models` 表按「板块 × 厂商」推）：
//
//  1. 文档是**系统内置的对外契约**。一个族有没有文档，不该取决于此刻有没有
//     在售模型、模型有没有填 `endpoints`。生产库实测：27 个在售模型里 15 个
//     `endpoints` 为空 —— 按模型推的结果是「图像 / 音乐整块没有文档」。
//  2. 厂商字段是运营手填的自由文本，库里有 `vg_908be1ac` 这种脏值；
//     按它拼 key 会把同一族拆成两篇（`videos-bytedance` 与 `videos-vg-908be1ac`）。
//  3. 方言归属（即梦 / muse）是**协议归属**，不是运行时能力推断。早先按
//     「能力 + 模型 ID 前缀」猜，视频即梦那一条漏了前缀，结果 MiniMax 的页面
//     里出现了即梦格式的字段表 —— 读者照着传，上游静默忽略或直接 400。
//
// 因此：族 = 常量。新增一家厂商的文档 = 在这里加一条 + 在 family_endpoints.go 里
// 写它那几篇，不查库、不改播种逻辑。

// 族 key 常量。写常量而不是在两张表里各写一遍字面量：
// 族 key 同时是 URL 段、节点 key、`familyEndpointTable` 的索引，
// 任何一处打错都只会表现为「这一族没有文档」，不会报错。
const (
	FamilyImagesGptImage   = "images-gpt-image"
	FamilyImagesNanoBanana = "images-nano-banana"
	FamilyImagesSeedream   = "images-seedream"

	FamilyVideosKling    = "videos-kling"
	FamilyVideosSeedance = "videos-seedance"
	FamilyVideosMiniMax  = "videos-minimax"
	FamilyVideosMuse     = "videos-muse"

	FamilyMusicSuno = "music-suno"

	FamilyAudioIndexTTS = "audio-indextts"
	FamilyAudioWhisper  = "audio-whisper"
)

// FamilyDoc 是一个模型族的身份声明。
type FamilyDoc struct {
	// Key 是族节点的稳定身份（也是 URL 段）：`images-seedream`。
	//
	// 写死而不是拼出来：它一旦跟着数据变，旧链接就会整批 404，
	// 而且「同一族两篇文档」这种重复在库里看不出来。
	Key string
	// Section 是所属板块（images / videos / music / audio）。
	Section string
	// Title 是侧栏与页头显示名。
	Title string
	// Summary 是一句话说明（节点页副标题）。
	Summary string
	// Sort 是板块内顺序。
	Sort int
	// Match 是「哪些模型属于这一族」的唯一判据：对模型 ID 做**子串**匹配。
	//
	// 为什么是子串不是前缀：平台对外 ID 常带厂商前缀（`doubao-seedream-5-0`），
	// 而族名是 `seedream`；按前缀匹配会漏掉全部带前缀的模型。
	Match []string
	// PendingKinds 是这一族**将来会写、现在还没写**的端点（只在 Deferred 时有意义）。
	//
	// 它不参与任何渲染：唯一用途是让「每篇登记过的文档都得有节点收录」这条门禁
	// 知道"这条路由是故意空着的"，而不是漏写。上架时把端点写进 family_*.go 并删掉这里。
	PendingKinds []Kind
	// AsyncSubmit 表示这一族的提交端点支持 `async: true` 转任务（**协议属性**）。
	//
	// 为什么必须按族声明，而不是继续用端点契约里的 AcceptsAsync：
	// 那个字段说的是「这个端点长得像能异步」（/v1/images/generations 在多米是异步、
	// 在方舟只有同步），把它当族级事实会让 Seedream 的页面写出 `async` 字段和
	// 「已受理」响应 —— 读者照着发，网关没有渠道能承接，最后是一个 502。
	//
	// 视频 / 音乐族不读这个字段（它们的 `Phase == PhaseSubmit` 本身就是"提交即任务"）。
	AsyncSubmit bool
	// Deferred 表示这一族**暂不上架**：工作目录里没有权威的官方接口文档，
	// 内容无法核实，因此不建节点、不出现在文档站。
	//
	// 为什么用标志而不是直接删掉声明：删掉之后「这一族存在过」这件事就没有任何
	// 记录，下一个人会重新按印象编一份内容；而 Deferred 会一直出现在
	// `TestEveryActiveFamilyHasDocs` 的豁免名单里，逼着补齐文档才能上架。
	Deferred bool
}

// familyDocList 是全部族的声明，**顺序即侧栏顺序**（再按板块聚簇排序）。
var familyDocList = []FamilyDoc{
	// ── 图像 ──
	{
		Key: FamilyImagesGptImage, Section: "images", Title: "GPT Image", Sort: 10,
		Summary: "OpenAI 图像协议：文生图、图生图与变化图，支持 async 异步出图。",
		Match:   []string{"gpt-image"},
		// 多米的 gpt-image 有原生异步端点（/v1/images/generations?async=true + /v1/tasks/{id}）。
		AsyncSubmit: true,
	},
	{
		Key: FamilyImagesNanoBanana, Section: "images", Title: "Nano Banana", Sort: 20,
		Summary: "Gemini 图像协议：文生图与多图编辑。",
		Match:   []string{"nano-banana", "gemini"},
		AsyncSubmit: true,
	},
	{
		Key: FamilyImagesSeedream, Section: "images", Title: "Seedream", Sort: 30,
		Summary: "Seedream 原生报文：size 与 sequential_image_generation 表达尺寸与张数。",
		Match:   []string{"seedream"},
		// 方舟的 images/generations 只有同步形态（官方文档里没有任务号、没有查询接口）。
		// 写 true 就是替上游承诺一个它没有的能力：发布闸门会要求页面写 async 字段与
		// 「已受理」变体，而真发 async:true 时没有任何渠道能承接。
		AsyncSubmit: false,
	},

	// ── 视频 ──
	{
		Key: FamilyVideosKling, Section: "videos", Title: "可灵", Sort: 10,
		Summary: "可灵视频任务：提交 → 查询 → 下载。",
		Match:   []string{"kling"},
	},
	{
		Key: FamilyVideosSeedance, Section: "videos", Title: "Seedance", Sort: 20,
		Summary: "Seedance 视频任务：主篇与即梦原生报文两套写法。",
		Match:   []string{"seedance"},
	},
	{
		Key: FamilyVideosMiniMax, Section: "videos", Title: "MiniMax H3", Sort: 30,
		Summary: "MiniMax H3 视频任务：一个请求体覆盖文生 / 图生 / 多图参考 / 首尾帧 / 配音。",
		Match:   []string{"minimax", "hailuo"},
	},
	{
		Key: FamilyVideosMuse, Section: "videos", Title: "Muse", Sort: 40,
		Summary: "Muse 视频任务：图生视频，官方只收 5 个入参（模型 / 描述 / 时长 / 画幅 / 参考图）。",
		Match:   []string{"muse"},
	},

	// ── 音乐 ──
	{
		Key: FamilyMusicSuno, Section: "music", Title: "SUNO", Sort: 10,
		Summary: "音乐与歌词生成：一次任务出两首，产物按实际转存数计费。",
		Match:   []string{"suno", "chirp"},
	},

	// ── 音频 ──
	{
		Key: FamilyAudioIndexTTS, Section: "audio", Title: "IndexTTS", Sort: 10,
		Summary:      "语音合成：请求直接返回音频二进制。",
		Match:        []string{"indextts"},
		Deferred:     true, // 同上
		PendingKinds: []Kind{KindAudioSpeech},
	},
	{
		// 这一族当前没有在售模型（库里的 whisper-1 未启用），但两个端点
		// `/v1/audio/transcriptions`、`/v1/audio/translations` 是真实路由、
		// 真实契约（OpenAI 官方协议）。不建族就是「路由存在、文档查不到」。
		Key: FamilyAudioWhisper, Section: "audio", Title: "语音识别", Sort: 20,
		Summary: "OpenAI 语音识别协议：转写与翻译，multipart 上传音频。",
		Match:   []string{"whisper", "transcribe"},
	},
}

// FamilyDocs 返回**已上架**的族（顺序 = 侧栏顺序：先按板块，再按 Sort）。
//
// 排序在代码里做一次而不是靠字面量顺序：两侧栏顺序表（`sectionOrderList`）已经
// 是板块的唯一顺序来源，族再维护一份「声明顺序」就会与它对不上，
// 而表现只是「音频板块跑到视频后面」，没有任何报错。
func FamilyDocs() []FamilyDoc {
	out := make([]FamilyDoc, 0, len(familyDocList))
	for _, f := range familyDocList {
		if f.Deferred {
			continue
		}
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := sectionRank(out[i].Section), sectionRank(out[j].Section)
		if a != b {
			return a < b
		}
		return out[i].Sort < out[j].Sort
	})
	return out
}

// AllFamilyDocs 返回全部族（含暂未上架的），只给测试与「这一族存在吗」的查表用。
func AllFamilyDocs() []FamilyDoc {
	out := make([]FamilyDoc, len(familyDocList))
	copy(out, familyDocList)
	return out
}

// sectionRank 返回板块在侧栏顺序表里的下标（未知板块排最后）。
func sectionRank(id string) int {
	for i, s := range sectionOrderList {
		if s == id {
			return i
		}
	}
	return len(sectionOrderList)
}

// FamilyDocOf 按 key 取族（含暂未上架的族：库里可能还留着它的旧节点）。
func FamilyDocOf(key string) (FamilyDoc, bool) {
	for _, f := range familyDocList {
		if f.Key == key {
			return f, true
		}
	}
	return FamilyDoc{}, false
}

// FamilyOfKey 按族 key 取声明（文档节点 key 与族 key 同名）。
func FamilyOfKey(key string) (FamilyDoc, bool) {
	key = strings.TrimSpace(key)
	for _, f := range familyDocList {
		if f.Key == key {
			return f, true
		}
	}
	return FamilyDoc{}, false
}

// FamilyAllowsAsync 回答「这一族的提交端点能不能 async:true」。
//
// 未知 key 一律 false：拿不到族声明的页面不该凭空承诺异步（草稿页尤其）。
// 视频 / 音乐族的 `AsyncSubmit` 是零值 false，但它们本来就不靠这个字段 ——
// 调用方只在 `Transport.AcceptsAsync` 为真时才会问到这里。
func FamilyAllowsAsync(key string) bool {
	f, ok := FamilyOfKey(key)
	return ok && f.AsyncSubmit
}

// FamilyDocOfModelID 判断一个模型 ID 属于哪一族（不属任何族返回 false）。
//
// **这是族归属的唯一实现**：投影的「适用模型」与族节点成员都从这里出发，
// 不允许各写一份前缀匹配。
//
// 多族同时命中时取声明顺序在前的一条，并由 `TestFamilyMatchDisjoint` 兜住 ——
// 正常数据下不会发生（族名互不为子串）。
func FamilyDocOfModelID(id string) (FamilyDoc, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return FamilyDoc{}, false
	}
	for _, f := range familyDocList {
		for _, m := range f.Match {
			if strings.Contains(id, m) {
				return f, true
			}
		}
	}
	return FamilyDoc{}, false
}
