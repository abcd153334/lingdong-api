package docdocs

import (
	"strings"
	"testing"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/model"
)

// TestGuideBlocksResolveBaseURL 守「说明页不能印出 {{base_url}}」。
//
// 这是线上最常见的一类坏文档：占位符语法完全正确、页面也能打开，
// 读者照着 `curl {{base_url}}/v1/models` 复制，命令直接报错。
func TestGuideBlocksResolveBaseURL(t *testing.T) {
	j := model.MustJSON([]apidoc.GuideBlock{
		{Type: apidoc.BlockPara, Text: "地址是 `{{base_url}}/v1`"},
		{Type: apidoc.BlockCode, Lang: "bash", Code: "curl {{base_url}}/v1/models"},
		{Type: apidoc.BlockBullets, Items: []string{"打开 {{base_url}}/docs"}},
	})
	got := guideBlocks(j, "https://api.example.com/")
	if len(got) != 3 {
		t.Fatalf("块数不对：%d", len(got))
	}
	for i, b := range got {
		all := b.Text + b.Code + strings.Join(b.Items, " ")
		if strings.Contains(all, "{{base_url}}") {
			t.Fatalf("第 %d 块仍残留占位符：%q", i, all)
		}
	}
	if !strings.Contains(got[0].Text, "https://api.example.com/v1") {
		t.Fatalf("基址没有替换成功：%q", got[0].Text)
	}
}

// TestGuideBlocksWithoutBaseKeepsPlaceholder 守「不猜域名」。
//
// 空基址时保留占位符（页面上看得见），而不是替换成空串 ——
// 后者会让句子凭空少一个词，没人能发现。
func TestGuideBlocksWithoutBaseKeepsPlaceholder(t *testing.T) {
	j := model.MustJSON([]apidoc.GuideBlock{{Type: apidoc.BlockPara, Text: "`{{base_url}}/v1`"}})
	got := guideBlocks(j, "")
	if len(got) != 1 || !strings.Contains(got[0].Text, "{{base_url}}") {
		t.Fatalf("空基址应保留占位符，实际 %+v", got)
	}
}

// TestProjectNodeBlocksAlwaysArray 守「非说明页也要有 blocks 键」。
func TestProjectNodeBlocksAlwaysArray(t *testing.T) {
	b := &NodeBundle{Node: model.DocNode{
		Key: "x", Kind: model.DocNodeFamily, Blocks: model.MustJSON([]any{}),
	}}
	n := ProjectNode(b, "")
	if n.Blocks == nil {
		t.Fatal("blocks 不能是 nil：JSON 里键会消失，前端 .map 直接崩")
	}
	if len(n.Blocks) != 0 {
		t.Fatalf("非说明页不该有正文块，实际 %d", len(n.Blocks))
	}
}

// TestSamplesViewReplacesBaseURLOnly 守「库里存占位符、页面换域名」。
//
// 示例代码是内容（库里那四段文本），域名是环境。替换只发生在投影期，
// 且只替换 `{{base_url}}` 一个记号 —— 站点没配地址时原样保留，
// 宁可在页面上看见占位符，也不猜一个域名。
func TestSamplesViewReplacesBaseURLOnly(t *testing.T) {
	j := model.MustJSON(docdocsSamples{
		Curl: "curl {{base_url}}/v1/images -H 'Authorization: Bearer $LD_API_KEY'",
		Go:   "baseURL := \"{{base_url}}\"",
	})
	got := samplesViewOf(j, "https://api.example.com/")
	if got == nil {
		t.Fatal("有内容时必须返回视图")
	}
	if strings.Contains(got.Curl, "{{base_url}}") || !strings.Contains(got.Curl, "https://api.example.com/v1/images") {
		t.Fatalf("基址没被替换（或尾斜杠没去掉）：%s", got.Curl)
	}
	if strings.Contains(got.Go, "{{base_url}}") {
		t.Fatalf("每一段都要替换：%s", got.Go)
	}

	// 空基址：保留占位符（不是替换成空串）。
	if s := samplesViewOf(j, ""); !strings.Contains(s.Curl, "{{base_url}}") {
		t.Fatalf("空基址必须保留占位符，实际 %s", s.Curl)
	}
	// 空对象：页面不显示代码块。
	if s := samplesViewOf(model.MustJSON(docdocsSamples{}), "https://x.test"); s != nil {
		t.Fatalf("四段全空必须返回 nil，实际 %+v", s)
	}
	if s := samplesViewOf(nil, "https://x.test"); s != nil {
		t.Fatal("没有示例代码时必须是 nil")
	}
}

// TestResolvePresetModel 守「$model 在播种期就换成字面量」。
//
// 投影期替换会把「后台改了预设体、页面还是旧模型名」变成常态：
// 库里写着占位符，而替换逻辑只认某个来源的值。
func TestResolvePresetModel(t *testing.T) {
	body := apidoc.Obj{
		{K: "model", V: "$model"},
		{K: "seconds", V: "$seconds"},
		{K: "prompt", V: "x"},
	}
	got := resolvePresetModel(body, "kling-v3")
	if v, _ := got.Get("model"); v != "kling-v3" {
		t.Fatalf("$model 应换成本篇 model 的示例值，实际 %v", v)
	}
	// 其它 `$xxx` 原样保留：它们不是占位符，是运营写的字面量。
	if v, ok := got.Get("seconds"); !ok || v != "$seconds" {
		t.Fatalf("非 $model 的占位串必须原样保留，实际 %v", v)
	}
	// 取不到值时删键：`"model": "$model"` 抄进请求必然 404。
	if _, ok := resolvePresetModel(body, nil).Get("model"); ok {
		t.Fatal("没有 model 示例值时必须删掉这个键")
	}
}

// TestExampleBodyOfUsesFieldExamples 守「参数表里的示例值就是请求体」。
func TestExampleBodyOfUsesFieldExamples(t *testing.T) {
	b := &EndpointBundle{Fields: []model.DocEndpointField{
		{ID: 1, In: "body", Name: "model", Example: model.MustJSON("kling-v3")},
		{ID: 2, In: "query", Name: "page", Example: model.MustJSON(2)},
		{ID: 3, In: "body", Name: "prompt", Example: model.MustJSON("一只猫")},
		{ID: 4, In: "body", Name: "no_example"},
	}}
	got := exampleBodyOf(b)
	if len(got) != 2 {
		t.Fatalf("只取 body 且有示例值的字段，实际 %+v", got)
	}
	if got[0].K != "model" || got[1].K != "prompt" {
		t.Fatalf("必须按字段顺序拼：%+v", got)
	}
}

// docdocsSamples 是测试用的四段代码字面量（与 apidoc.Samples 同形）。
type docdocsSamples struct {
	Curl   string `json:"curl"`
	Python string `json:"python"`
	Node   string `json:"node"`
	Go     string `json:"go"`
}

// TestProjectionReadsNotesFromRow 守「注意事项归数据库所有」。
//
// 针对一类反复出现的错误：投影期从契约读文案。旧实现是 `contractNotes(b.EP)` ——
// `doc_endpoints` 根本没有 notes 列，运营改不了它，改一行字要发版。现在必须做到
// 「这一行存了什么就渲染什么」，契约只是首次建库的种子。
func TestProjectionReadsNotesFromRow(t *testing.T) {
	id := "images/generations"
	b := &EndpointBundle{
		Node: model.DocNode{Key: "images-gpt-image", Title: "GPT Image", Section: "images"},
		EP: model.DocEndpoint{
			Key: "ep-images-generations", Name: "图片生成", Method: "POST",
			Path: "/v1/images/generations", TransportDocID: &id,
			Notes: model.MustJSON([]string{"运营写的第一条", "运营写的第二条"}),
		},
		Enums: map[uint64][]model.DocFieldEnum{},
	}
	got := projectEndpoint(b, "", modelIndex{})
	if len(got.Notes) != 2 || got.Notes[0] != "运营写的第一条" {
		t.Fatalf("注意事项应逐字来自库里的列，实际 %q", got.Notes)
	}

	// 反面：列是空的，而契约里这个端点**有**出厂注意事项。
	// 输出必须是空数组 —— 一旦有人把「空则回退读契约」加回来，这条立刻红。
	b.EP.Notes = model.MustJSON([]string{})
	if n := projectEndpoint(b, "", modelIndex{}).Notes; len(n) != 0 {
		t.Fatalf("列空时不得回退读契约，实际 %q", n)
	}
}

// TestProjectionReadsOnlyModelsFromRow 守「适用范围也是内容」。
//
// 旧实现用 `contractOnlyModels(kind, name, in)` 覆盖库里的值，后台那一栏是只读的。
// 一句「哪些模型才认这个字段」会因为上/下线一个型号而变，它必须能编辑。
func TestProjectionReadsOnlyModelsFromRow(t *testing.T) {
	id := "images/generations"
	b := &EndpointBundle{
		Node: model.DocNode{Key: "images-gpt-image", Title: "GPT Image", Section: "images"},
		EP:   model.DocEndpoint{Key: "ep-x", Name: "X", Method: "POST", Path: "/x", TransportDocID: &id},
		Fields: []model.DocEndpointField{
			{ID: 1, Name: "quality", In: "body", Type: "string",
				OnlyModels: "运营改过的口径"},
		},
		Enums: map[uint64][]model.DocFieldEnum{},
	}
	got := projectEndpoint(b, "", modelIndex{})
	if len(got.Fields) != 1 {
		t.Fatalf("字段数不对：%d", len(got.Fields))
	}
	if got.Fields[0].OnlyModels != "运营改过的口径" {
		t.Fatalf("only_models 应来自库里的列，实际 %q", got.Fields[0].OnlyModels)
	}
}

// TestProjectEndpointRunFacts 守「运行形态的事实只能来自绑定的契约端点」。
//
// 两件事都在这里被钉住：
//   - `phase_code` 是库列（sync/submit/read），不是把中文 `phase` 反解出来的 ——
//     中文文案一改，任何靠它做分支的前端都会静默走错分支；
//   - `accepts_async` 现算自 `Transport.AcceptsAsync`，不看手填的 badge。
//     未绑定契约的草稿页必须为 false：不能凭空承诺"支持异步"。
func TestProjectEndpointRunFacts(t *testing.T) {
	imgID := "images/generations"
	img := &EndpointBundle{
		Node: model.DocNode{Key: "images-gpt-image", Title: "GPT Image", Section: "images"},
		EP: model.DocEndpoint{
			Key: "ep-x", Name: "X", Method: "POST", Path: "/v1/images/generations",
			Phase: "sync", TransportDocID: &imgID,
		},
		Enums: map[uint64][]model.DocFieldEnum{},
	}
	got := projectEndpoint(img, "", modelIndex{})
	if got.PhaseCode != "sync" || !got.AcceptsAsync {
		t.Fatalf("图像生成应为 sync + 支持异步，实际 phase_code=%q accepts_async=%v",
			got.PhaseCode, got.AcceptsAsync)
	}
	if got.Phase != "同步" {
		t.Fatalf("中文文案应保持可读，实际 %q", got.Phase)
	}

	taskID := "tasks/get"
	task := &EndpointBundle{
		Node: model.DocNode{Key: "openai", Title: "通用接口", Section: "general"},
		EP: model.DocEndpoint{
			Key: "ep-y", Name: "查询任务状态", Method: "GET", Path: "/v1/tasks/{id}",
			Phase: "read", TransportDocID: &taskID,
		},
		Enums: map[uint64][]model.DocFieldEnum{},
	}
	got = projectEndpoint(task, "", modelIndex{})
	if got.PhaseCode != "read" || got.AcceptsAsync {
		t.Fatalf("任务查询应为 read + 不支持异步，实际 phase_code=%q accepts_async=%v",
			got.PhaseCode, got.AcceptsAsync)
	}

	// 未绑定契约：库里没有 transport_doc_id，能力必须是 false 而不是猜。
	loose := &EndpointBundle{
		Node:  model.DocNode{Key: "openai", Title: "通用接口", Section: "general"},
		EP:    model.DocEndpoint{Key: "ep-z", Name: "草稿", Method: "POST", Path: "/x"},
		Enums: map[uint64][]model.DocFieldEnum{},
	}
	if got := projectEndpoint(loose, "", modelIndex{}); got.AcceptsAsync {
		t.Fatal("未绑定契约时不得声明支持异步")
	}
}
