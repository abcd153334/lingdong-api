package apidoc

import "lingdong-api/internal/model"

// 本文件是**出厂种子**的唯一来源：把原来的 Go 常量（endpointDocs / 方言 / 预设 /
// 错误码）组装成「节点 + 接口」的种子行，由 docdocs.SeedDocs 写入数据库。
//
// 为什么种子留在 apidoc 而不是 docdocs：
//   - 契约常量在这里，种子只是它的另一种排版，放在一起才不会各自漂移；
//   - docdocs 依赖 apidoc，反过来不行（会成环）。
//
// 种子**只在首次缺失时写入**，之后由契约版本驱动刷新（docdocs.SeedDocs 负责）。
// 任何人改过的内容不会被启动覆盖 —— 这是「文档内容以库为准」的前提：
// 如果启动会无条件回写，运营每次发布都会被下一次重启回滚，而且没有任何报错。

// SeedEndpoint 是一个接口的出厂内容（= 一页文档的全部内容）。
type SeedEndpoint struct {
	// Page 是这篇文档的**身份**（族 + 端点 + 方言）：全站唯一。
	Page PageKey
	// Key 是 URL 段（由 Page 派生）：全站唯一，不只节点内唯一。
	//
	// 为什么全站唯一：三族各有「生成图片」这一页，它们打的是同一个网关路径但
	// 是三篇不同文档。标识若按节点内唯一，三族的 URL 段就是同一个字符串
	// （`ep-images-generations`），复制出去的链接看不出指向哪一族。
	Key string
	// DocID 是平台已登记端点身份（`images/generations` / `images/generations@jimeng`）。
	DocID string
	// Doc 是全部内容：名称/摘要/字段/返回/示例/响应/运行方式。
	Doc EndpointDoc
	// Presets 是右栏「调用方式」。
	Presets []Preset
	// Errors 是错误码（由契约派生，source=seed）。
	Errors []ErrorCode
}

// SeedNode 是一个出厂节点（协议篇 / 通用接口分组 / 模型族）。
type SeedNode struct {
	Key       string
	Kind      string
	Section   string
	Protocol  string
	Title     string
	Summary   string
	Sort      int
	Endpoints []SeedEndpoint
}

// SeedNodes 返回**全部**出厂节点：协议篇 + 通用接口 + 模型族。
//
// 这是文档站节点清单的唯一来源。族节点不再由「在售模型 × 厂商」动态分桶
// —— 那条路径的后果是「图像 / 音乐整块没有文档」（模型没填 endpoints 就没有接口，
// 没有接口就跳过建节点），而文档是内置契约，不该随供货情况消失。
func SeedNodes() []SeedNode {
	out := SeedDocs()
	for _, f := range FamilyDocs() {
		out = append(out, SeedNode{
			Key: f.Key, Kind: model.DocNodeFamily, Section: f.Section,
			Title: f.Title, Summary: f.Summary, Sort: f.Sort,
			Endpoints: SeedFamilyEndpoints(f),
		})
	}
	return out
}

// SeedDocs 返回与模型无关的出厂节点：协议篇 + 通用接口。
func SeedDocs() []SeedNode {
	nodes := []SeedNode{
		{
			Key: "openai", Kind: model.DocNodeProtocol, Section: SectionProtocols, Protocol: "openai",
			Title: "OpenAI 兼容", Summary: "对话、补全、智能体接口与内容审核，按 OpenAI 官方协议收发。", Sort: 10,
			// compact 属于 Responses 家族（同一套请求/响应形状），跟 openai 节点一起；
			// 放进别的节点会让读者以为它是另一个协议。
			Endpoints: seedEndpointsOf(KindChatCompletions, KindCompletions, KindResponses, KindResponsesCompact, KindModerations),
		},
		{
			Key: "claude", Kind: model.DocNodeProtocol, Section: SectionProtocols, Protocol: "claude",
			Title: "Claude 原生", Summary: "Anthropic Messages 协议（/v1/messages）。", Sort: 20,
			Endpoints: seedEndpointsOf(KindMessages),
		},
		{
			Key: "gemini", Kind: model.DocNodeProtocol, Section: SectionProtocols, Protocol: "gemini",
			Title: "Gemini 兼容", Summary: "Gemini 协议的模型列表、内容生成与向量化。", Sort: 40,
			Endpoints: seedEndpointsOf(KindGeminiModels, KindGeminiGenerate, KindGeminiEmbedContent),
		},
		{
			Key: "embeddings", Kind: model.DocNodeProtocol, Section: SectionProtocols, Protocol: "embeddings",
			Title: "Embeddings", Summary: "文本向量化与相关性重排。", Sort: 30,
			Endpoints: seedEndpointsOf(KindEmbeddings, KindRerank),
		},
	}
	nodes = append(nodes, generalSeedNodes()...)
	return nodes
}

// generalSeedNodes 按 Transport.Group 把通用接口分组成节点。
//
// 分组是显式表而不是按 Group 自动遍历：通用接口的节点名是产品措辞（「文件与产物」），
// 从功能组 id 推不出来；漏一个接口的表现是「文档站里没有这一页」，不会报错。
func generalSeedNodes() []SeedNode {
	out := []SeedNode{
		{Key: "files", Kind: model.DocNodeGeneral, Section: SectionGeneral, Title: "文件与产物",
			Summary: "上传素材、下载产物与产物直链代理。", Sort: 10},
		{Key: "tasks", Kind: model.DocNodeGeneral, Section: SectionGeneral, Title: "异步任务",
			Summary: "跨模型的统一任务列表与再生成。", Sort: 20},
		{Key: "models", Kind: model.DocNodeGeneral, Section: SectionGeneral, Title: "模型清单",
			Summary: "查询可用模型与默认模型。", Sort: 30},
		{Key: "account", Kind: model.DocNodeGeneral, Section: SectionGeneral, Title: "账户与用量",
			Summary: "余额、调用明细与用量汇总。", Sort: 40},
		{Key: "compat", Kind: model.DocNodeGeneral, Section: SectionGeneral, Title: "兼容接口",
			Summary: "按第三方任务协议接入的应用用的查询壳；提交仍走 POST /v1/video/generations，字段见各视频族节点。", Sort: 50},
	}
	byNode := map[string][]Kind{
		// 视频提交**不再有平台通用篇**：每族一份完整文档（族节点里就是提交 + 查询）。
		// 曾经的"通用主篇 + 族页只写增量"会让读者先看一篇没有模型名的页面，
		// 才知道自己能传什么 —— 那是把最该直接回答的问题推到第二跳。
		"files": {KindFilesUpload, KindFilesList, KindFilesGet, KindFilesContent,
			KindFilesDelete, KindFileDirect, KindMediaProxy},
		// video/regenerations 归「任务」而不是任一视频族：它作用于**已存在的任务**
		// （源任务在哪个渠道，能力就由那个渠道决定），放进某一族就是对其它族说谎。
		//
		// 查任务 / 取产物 / 取消三篇也在这里，而不是每族挂一份：
		// 它们由网关自己实现，返回的是同一个任务对象，与厂商协议无关。
		// 过去每族各存一份 ⇒ 3 个接口 24 篇文档，同一路径有 8 个 URL，
		// 读者在哪个族点进来看到的都是同一页，而搜索会给出 8 个结果。
		"tasks": {KindTasksList, KindTasksGet, KindTasksArtifacts, KindTasksArtifactContent,
			KindTasksCancel, KindVideoRegenerate},
		// 兼容接口：new-api 的视频查询壳。提交页仍在各视频族节点（字段按族不同），
		// 这里只放那一个"协议形状不同、数据同一个"的读面。
		// 「兼容接口」节点：给按第三方协议接入的应用。三条都是同一行的投影，
		// 但形状属于不同协议（封套 / 视频对象 / 字节流），各自的客户端只认自己那份。
		"compat": {KindVideoTaskGet, KindVideoGet, KindVideoContent},
		"models": {KindModelsList, KindModelsGet, KindModelsDefaults},
		"account": {KindAccount, KindUsageList, KindUsageSummary,
			KindBillingSubscription, KindBillingUsage},
	}
	for i := range out {
		for _, kk := range byNode[out[i].Key] {
			if _, ok := TransportOf(kk); !ok {
				continue
			}
			out[i].Endpoints = append(out[i].Endpoints, seedEndpointOf(kk, ""))
		}
	}
	return out
}

// seedEndpointsOf 是一组主篇接口的便捷构造。
func seedEndpointsOf(kinds ...Kind) []SeedEndpoint {
	out := make([]SeedEndpoint, 0, len(kinds))
	for _, k := range kinds {
		if _, ok := TransportOf(k); !ok {
			continue
		}
		out = append(out, seedEndpointOf(k, ""))
	}
	return out
}

// seedEndpointOf 把一条**平台自有**端点转成种子接口（内容来自 endpoints.go）。
func seedEndpointOf(k Kind, variant string) SeedEndpoint {
	page := PageKey{Kind: k, Variant: variant}
	return SeedEndpoint{
		Page:   page,
		Key:    PageSlugOf(page),
		DocID:  page.Transport().String(),
		Doc:    MustEndpointDoc(k),
		Errors: endpointErrors(k),
	}
}

// SeedFamilyEndpoints 把一族的接口文档展开成种子接口。
//
// 一族的页面顺序 = 它自己那份清单的顺序，**不再追加、不再收窄**：
// 过去这里会在族后面补三篇「查任务 / 取产物 / 取消」，再把所有字段过一遍减法，
// 于是同一路径在 8 个族里各有一篇、字段表由「删掉别家的」得到。
func SeedFamilyEndpoints(f FamilyDoc) []SeedEndpoint {
	eps := FamilyEndpointsOf(f)
	out := make([]SeedEndpoint, 0, len(eps))
	for _, e := range eps {
		doc := e.Spec
		// Kind 由身份决定，不允许两份写法（写错会让 relay 侧按错误的线格式渲染）。
		doc.Kind = e.Doc.Kind
		page := PageKey{Family: f.Key, Kind: e.Doc.Kind, Variant: e.Doc.Variant}
		out = append(out, SeedEndpoint{
			Page:    page,
			Key:     PageSlugOf(page),
			DocID:   e.Doc.String(),
			Doc:     doc,
			Presets: e.Presets,
			Errors:  endpointErrors(e.Doc.Kind),
		})
	}
	return out
}
