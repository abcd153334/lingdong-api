package docdocs

import (
	"encoding/json"
	"strings"
	"testing"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/model"
)

// 这一组用例守的是「一条直线」：预览 = 保存，保存 = 页面，板块 = 库里的字。
// 每条都对应一个已经被踩过的坑，不是"看起来应该有"的测试。

// TestPreviewMatchesSavedProjection 预览与保存后的投影必须逐字相等。
//
// 曾经预览自己拼了一遍行结构：漏 aliases、不设 phase、把顶层 kind/envelope
// 广播给所有变体。于是「预览好看、发布后变形」，而两边都自称同一篇文档。
func TestPreviewMatchesSavedProjection(t *testing.T) {
	withWriteDB(t)
	node, _ := newDraftNode(t)
	tr, _ := apidoc.TransportOfDocID("images/generations")
	in := EndpointInput{
		Name: "生成图片", Summary: "一句话简介", Method: tr.Method, Path: tr.Path,
		TransportID: "images/generations",
		Aliases:     []string{"/v1/images/create"},
		Notes:       []string{"计费按张", "超时 300 秒"},
		Fields: []FieldView{
			{Name: "model", In: "body", Type: "string", Required: "required", Tier: "standard",
				Example: "gpt-image-2"},
		},
		Samples:       &SamplesView{Curl: "curl {{base_url}}/v1/images/generations"},
		ExampleTitle2: "流式输出",
		Response: AdminResponse{Kind: "json", Variants: []AdminVariant{
			{Status: 200, Label: "成功", Kind: "binary", ContentType: "image/png",
				Returns: []FieldView{{Name: "b64_json", In: "body", Type: "string"}}},
			{Status: 202, Label: "已受理", Kind: "json", Envelope: true,
				Returns: []FieldView{{Name: "task_id", In: "body", Type: "string"}}},
		}},
	}
	created, err := CreateEndpoint(node, EndpointCreateInput{Key: "gen", EndpointInput: in}, 1)
	if err != nil {
		t.Fatalf("建接口失败: %v", err)
	}
	// 第二次保存要带当前版本（乐观锁），否则会被拒 —— 这正是要守的语义。
	in.Version = curVersion(t, node, "gen")
	saved, err := SaveEndpoint(node, "gen", in, model.DocOriginManual, 1)
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if saved.Version == created.Version {
		t.Fatal("保存必须推进版本号（否则 409 永远不会出现）")
	}

	b, err := LoadEndpoint(node, "gen")
	if err != nil {
		t.Fatalf("装载失败: %v", err)
	}
	stored := ProjectEndpoint(b, "https://api.example.com")

	preview, err := PreviewEndpointInput(node, "gen", in, "https://api.example.com")
	if err != nil {
		t.Fatalf("预览失败: %v", err)
	}
	gotJSON, _ := json.Marshal(preview)
	wantJSON, _ := json.Marshal(stored)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("预览与保存后的投影必须逐字相同\n preview=%s\n  saved=%s", gotJSON, wantJSON)
	}

	// 逐条点名最容易分叉的四处（失败时看得懂是哪一处）。
	if len(preview.Aliases) != 1 || preview.Aliases[0] != "/v1/images/create" {
		t.Fatalf("aiases 丢了：%+v", preview.Aliases)
	}
	// phase 由**绑定的契约端点**派生（images/generations 是同步端点）：
	// 手填的「异步」标签迟早与真实端点不一致，而侧栏会继续对外承诺不存在的能力。
	if preview.Phase != "同步" {
		t.Fatalf("phase 应由绑定端点派生，实际 %q", preview.Phase)
	}
	if len(preview.Response.Variants) != 2 {
		t.Fatalf("变体数量不对：%+v", preview.Response.Variants)
	}
	if preview.Response.Variants[0].ContentType != "image/png" {
		t.Fatalf("逐变体 content_type 丢了：%+v", preview.Response.Variants[0])
	}
	if !strings.Contains(preview.Samples.Curl, "https://api.example.com") {
		t.Fatalf("示例代码的基址没被替换：%+v", preview.Samples)
	}
}

// TestUnpublishLastEndpointDraftsNode 撤下最后一篇，节点一起转草稿。
//
// 否则留下「节点已发布、页面空白」的悬挂态：侧栏有一个点不进去的分组。
func TestUnpublishLastEndpointDraftsNode(t *testing.T) {
	withWriteDB(t)
	node, _ := newDraftNode(t)
	tr, _ := apidoc.TransportOfDocID("images/generations")
	if _, err := CreateEndpoint(node, EndpointCreateInput{
		Key: "gen", EndpointInput: EndpointInput{
			Name: "生成图片", Method: tr.Method, Path: tr.Path, TransportID: "images/generations",
			Fields: []FieldView{fv("async", "body", false)},
			Response: AdminResponse{Variants: []AdminVariant{
				{Status: 200, Label: "成功"}, {Status: 202, Label: "已受理"},
			}},
		},
	}, 1); err != nil {
		t.Fatalf("建接口失败: %v", err)
	}
	if _, _, err := PublishEndpoint(node, "gen", 1); err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	ep, nb, err := UnpublishEndpoint(node, "gen", 1)
	if err != nil {
		t.Fatalf("下架失败: %v", err)
	}
	if ep.Status != model.DocDraft {
		t.Fatalf("接口应变草稿，实际 %s", ep.Status)
	}
	if nb.Status != model.DocDraft {
		t.Fatalf("没有已发布接口时节点必须一并转草稿，实际 %s", nb.Status)
	}
}

// TestDeleteLastEndpointDraftsNode 删掉最后一篇已发布接口，节点转草稿。
func TestDeleteLastEndpointDraftsNode(t *testing.T) {
	withWriteDB(t)
	node, _ := newDraftNode(t)
	tr, _ := apidoc.TransportOfDocID("images/generations")
	if _, err := CreateEndpoint(node, EndpointCreateInput{
		Key: "gen", EndpointInput: EndpointInput{
			Name: "生成图片", Method: tr.Method, Path: tr.Path, TransportID: "images/generations",
			Fields: []FieldView{fv("async", "body", false)},
			Response: AdminResponse{Variants: []AdminVariant{
				{Status: 200, Label: "成功"}, {Status: 202, Label: "已受理"},
			}},
		},
	}, 1); err != nil {
		t.Fatalf("建接口失败: %v", err)
	}
	if _, _, err := PublishEndpoint(node, "gen", 1); err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	if err := DeleteEndpoint(node, "gen"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	nb, err := LoadAdminNode(node)
	if err != nil {
		t.Fatalf("读回节点失败: %v", err)
	}
	if nb.Status != model.DocDraft {
		t.Fatalf("删掉最后一篇接口后节点必须转草稿，实际 %s", nb.Status)
	}
}

// TestNodeSectionMustExist 板块是节点的归属前提，未知 key 必须 400 而不是静默存下。
func TestNodeSectionMustExist(t *testing.T) {
	withWriteDB(t)
	if _, err := CreateNode(NodeCreateInput{
		Key: "x", Kind: model.DocNodeGeneral,
		NodeInput: NodeInput{Title: "x", Section: "no-such-section"},
	}, 1); err == nil {
		t.Fatal("未知板块必须被拒（否则这篇节点在侧栏无处归属）")
	}
}

// TestSectionRenameShowsInCatalog 改板块名，公开目录立刻是新名字。
//
// 这条是"板块入库"的验收点：名字只有一个来源（doc_sections）。
func TestSectionRenameShowsInCatalog(t *testing.T) {
	gdb := withDocDocsDB(t)
	if _, err := SeedDocs(); err != nil {
		t.Fatalf("播种失败: %v", err)
	}
	idx, err := SectionIndex()
	if err != nil {
		t.Fatalf("读板块失败: %v", err)
	}
	sec, ok := idx["images"]
	if !ok {
		t.Fatal("播种后必须有图片板块")
	}
	if _, err := SaveSection(sec.Key, SectionInput{
		Name: "图像", En: sec.En, Desc: sec.Desc, Color: sec.Color,
		Sort: sec.Sort, Hidden: sec.Hidden, Version: sec.Version,
	}, 1); err != nil {
		t.Fatalf("保存板块失败: %v", err)
	}
	tree, err := LoadTree()
	if err != nil {
		t.Fatalf("装载目录失败: %v", err)
	}
	cat, err := ProjectCatalog(tree, "")
	if err != nil {
		t.Fatalf("投影目录失败: %v", err)
	}
	found := false
	for _, s := range cat.Sections {
		if s.ID == sec.Key {
			found = true
			if s.Name != "图像" {
				t.Fatalf("目录里的板块名必须来自库，实际 %q", s.Name)
			}
		}
	}
	if !found {
		t.Fatal("目录里找不到图片板块")
	}
	// 库里只有一处真相：改回原名不应影响其它板块。
	var cnt int64
	if err := gdb.Model(&model.DocSection{}).Where("name = ?", "图像").Count(&cnt).Error; err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("板块名只应改到一行，实际 %d", cnt)
	}
}

// TestSeedWritesSamplesForEveryEndpoint 出厂文档必须带示例代码。
//
// 示例代码从"投影期现算"改成"入库的内容"之后，漏生成的表现是整站
// 每篇接口页都少一块 —— 而体检不会报，因为它不是必填项。
func TestSeedWritesSamplesForEveryEndpoint(t *testing.T) {
	gdb := withDocDocsDB(t)
	if _, err := SeedDocs(); err != nil {
		t.Fatalf("播种失败: %v", err)
	}
	var eps []model.DocEndpoint
	if err := gdb.Find(&eps).Error; err != nil {
		t.Fatalf("读接口失败: %v", err)
	}
	if len(eps) == 0 {
		t.Fatal("播种没有产出任何接口")
	}
	for _, e := range eps {
		if len(e.Samples) == 0 {
			t.Fatalf("%s/%s 没有示例代码（页面上会少一块）", e.Key, *e.TransportDocID)
		}
		var s SamplesView
		if err := json.Unmarshal(e.Samples, &s); err != nil {
			t.Fatalf("%s 的示例代码不是合法 JSON: %v", e.Key, err)
		}
		if s.Curl == "" || s.Python == "" || s.Node == "" || s.Go == "" {
			t.Fatalf("%s 的四段示例不全：%+v", e.Key, s)
		}
		// 只允许占位符，不许把当时的域名烘进库。
		if !strings.Contains(s.Curl, SampleBaseURL) {
			t.Fatalf("%s 的 curl 里没有 {{base_url}}（域名被写死了）: %s", e.Key, s.Curl)
		}
	}
	// 预设同样要带自己的示例。
	var presets []model.DocPreset
	if err := gdb.Find(&presets).Error; err != nil {
		t.Fatalf("读预设失败: %v", err)
	}
	for _, p := range presets {
		if len(p.Samples) == 0 {
			t.Fatalf("预设 %s 没有示例代码", p.Key)
		}
	}
}

// TestGuardScansStoredSamples 示例代码里的第三方域名必须被拦下。
//
// 示例代码入库之后，它就成了"可以绕过文案检查的一整个文本框"：
// 一段写死 evil.example.org 的 curl，页面上看起来只是示例。
func TestGuardScansStoredSamples(t *testing.T) {
	withWriteDB(t)
	node, _ := newDraftNode(t)
	tr, _ := apidoc.TransportOfDocID("images/generations")
	if _, err := CreateEndpoint(node, EndpointCreateInput{
		Key: "gen", EndpointInput: EndpointInput{
			Name: "生成图片", Method: tr.Method, Path: tr.Path, TransportID: "images/generations",
			Fields: []FieldView{fv("async", "body", false)},
			Samples: &SamplesView{
				Curl: "curl https://cdn.acme-vendor.io/v1/images -d '{}'",
			},
			Response: AdminResponse{Variants: []AdminVariant{
				{Status: 200, Label: "成功"}, {Status: 202, Label: "已受理"},
			}},
		},
	}, 1); err != nil {
		t.Fatalf("建接口失败: %v", err)
	}
	_, _, err := PublishEndpoint(node, "gen", 1)
	blocked, ok := err.(*BlockedError)
	if !ok {
		t.Fatalf("示例代码里写第三方域名必须拦住发布，实际 %v", err)
	}
	if !hasCode(blocked.Problems, "E_EXTERNAL_HOST") {
		t.Fatalf("必须点名 E_EXTERNAL_HOST，实际 %+v", blocked.Problems)
	}
}
