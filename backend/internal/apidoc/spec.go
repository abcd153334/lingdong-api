package apidoc

// SpecEndpoint 是「渠道可声明端点」的视图（规格 DSL 与 schema 目录共用）。
//
// 与 Transport 的差别只在筛选口径：这里只保留**有上游路径**的端点——
// 查询/取消/列表这类读面端点没有可转发的上游路径，不能出现在规格里
// （声明了也永远不会被调用，只会在 diff 里制造"配了但没用"的噪音）。
type SpecEndpoint struct {
	// Kind 是文档稳定 id（chat/completions）。
	Kind Kind
	// Key 是 models.endpoints 与 spec.endpoints 共用的写法（chat/completions）。
	Key string
	// Method / Path 是上游真实请求形态。
	Method string
	Path   string
	// Modality 是端点默认模态。
	Modality string
	// Multipart 表示请求体是 multipart/form-data。
	Multipart bool
	// Binary 表示响应体是二进制。
	Binary bool
	// Async 表示该端点提交后需轮询（submit 语义）。
	Async bool
	// AcceptsAsync 表示该端点**默认同步、可加 `async: true` 转任务**
	// （目前只有 images/generations）。
	//
	// 与 Async 的区别是「强制 vs 可选」：Async=true 的端点只有任务形态，
	// AcceptsAsync=true 的端点两条形态都有，所以规格可以：
	//   - 不声明 async → 只能同步用（上游恰好是同步的最简单情形）；
	//   - 声明 async 且 mode=task → async:true 走任务队列，**同步面由适配器在请求内等**；
	//   - 声明 async 且 mode=bridge → 只有同步面，async:true 会 ConfigError。
	AcceptsAsync bool
	// BridgeAllowed 表示该端点可以声明 `async.mode=bridge`（同步端点在上游其实是异步时，
	// 由适配器在本次请求内等结果）。异步提交端点恒为 false：分钟级任务必须交给任务队列。
	BridgeAllowed bool
	// SpecSupported 表示规格 DSL 能否表达该端点。
	//
	// 目前唯一的否定原因是 multipart：DSL 只重建 JSON 请求体，
	// 无法把调用方的文件字段重新编码成上游要求的 multipart 结构。
	// 这类端点必须写内置适配器（L3），强行放进 DSL 只会在运行期
	// 把「文件字段缺失」表现成上游 400。
	SpecSupported bool
	// UnsupportedReason 是不能进 DSL 的原因（展示给运营/AI，避免反复试错）。
	UnsupportedReason string
}

// specEndpointExcluded 是「有 EndpointKey 但不属于渠道可声明端点」的排除表。
//
// 每一条都有具体理由，删任何一条都会让规格里出现无法工作的端点：
//   - video/regenerations：必须携带已存在任务的上游 ID，渠道无法凭空生成；
//   - audio/lyrics：上游通常是 music 提交的附加产物，没有独立提交语义。
var specEndpointExcluded = map[Kind]bool{
	KindVideoRegenerate: true,
	KindAudioLyrics:     true,
}

// SpecEndpointTable 返回全部有上游路径的端点（含 DSL 不支持的，用于文档与目录展示）。
func SpecEndpointTable() []SpecEndpoint {
	out := make([]SpecEndpoint, 0, len(endpointTable))
	for _, e := range endpointTable {
		if e.t.EndpointKey == "" || e.t.Phase == PhaseRead || specEndpointExcluded[e.kind] {
			continue
		}
		se := SpecEndpoint{
			Kind:          e.kind,
			Key:           e.t.EndpointKey,
			Method:        e.t.Method,
			Path:          e.t.Path,
			Modality:      e.t.Modality,
			Multipart:     e.t.Multipart,
			Binary:        e.t.Binary,
			Async:         e.t.Phase == PhaseSubmit,
			AcceptsAsync:  e.t.AcceptsAsync,
			SpecSupported: true,
			// 同步端点才能桥接：它对外是同步合同，而上游只有"提交+查询"时，
			// 由适配器在本次请求内等完。异步端点由平台任务队列轮询，不走桥接。
			BridgeAllowed: e.t.Phase != PhaseSubmit,
		}
		if e.t.Multipart {
			se.SpecSupported = false
			se.UnsupportedReason = "文件上传类端点（multipart）需要内置适配器，规格只支持 JSON 请求体"
		}
		out = append(out, se)
	}
	return out
}

// SpecEndpoints 返回**规格可以声明**的端点（顺序与 endpointTable 一致）。
func SpecEndpoints() []SpecEndpoint {
	all := SpecEndpointTable()
	out := make([]SpecEndpoint, 0, len(all))
	for _, e := range all {
		if e.SpecSupported {
			out = append(out, e)
		}
	}
	return out
}

// SpecEndpointByKey 按 models.endpoints 写法查端点（含不支持的，便于给出原因）。
func SpecEndpointByKey(key string) (SpecEndpoint, bool) {
	for _, e := range SpecEndpointTable() {
		if e.Key == key {
			return e, true
		}
	}
	return SpecEndpoint{}, false
}

// ArtifactShape 表示「桥接拿到产物后必须以什么形状交付给调用方」。
//
// 这是保存期校验与运行期打包的**同一份**判据：只在一边写，另一处就会漂成
// 「保存时允许、运行期回错形状」或者反过来。
type ArtifactShape string

const (
	// ArtifactNone 表示该端点的响应契约承载不了"下载一个产物"。
	ArtifactNone ArtifactShape = ""
	// ArtifactBinary 表示响应体就是产物字节（audio/speech、文件内容）。
	ArtifactBinary ArtifactShape = "binary"
	// ArtifactImageJSON 表示响应体是 OpenAI 图像契约 {"created":…,"data":[{"url":…}]}。
	ArtifactImageJSON ArtifactShape = "image_json"
)

// Artifact 返回该端点的产物交付形状。
func (e SpecEndpoint) Artifact() ArtifactShape {
	if e.Binary {
		return ArtifactBinary
	}
	switch e.Kind {
	case KindImagesGenerations:
		// 图像生成的响应是 JSON 直链；桥接若回字节，SDK 解析直接失败，
		// 且 relay 的直链改写（rewriteMediaURLs）只认 JSON 体，字节流拿不到落地。
		return ArtifactImageJSON
	}
	return ArtifactNone
}
