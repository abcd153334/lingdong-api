package docdocs

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

// 写层用例：守三条最容易悄悄坏掉的语义。
//
//  1. 整篇保存 = 全量替换（不是追加）：漏删一行在页面上表现为「多了一个参数」，
//     而体检不会报（它只查重名与非法位置），所以必须有断言。
//  2. 保存一定退回草稿：编辑一个字立刻对外生效，只会在事故里被发现。
//  3. 未绑定 → 不能发布：这是「不许伪造能力」的闸门，任何写路径都不得绕过。
const writeTestSchema = "ld_test_docdocs_write"

func withWriteDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("LD_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 LD_TEST_DSN，跳过文档写层集成用例")
	}
	silent := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), silent)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := admin.Exec("DROP SCHEMA IF EXISTS " + writeTestSchema + " CASCADE").Error; err != nil {
		t.Fatalf("清理测试 schema 失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA " + writeTestSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+writeTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(
		&model.Model{},
		&model.DocSection{}, &model.DocNode{}, &model.DocEndpoint{},
		&model.DocEndpointField{}, &model.DocFieldEnum{}, &model.DocResponseVariant{},
		&model.DocPreset{}, &model.DocEndpointError{},
	); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	// 板块是节点的归属前提（保存时校验必须命中），测试 schema 里也要有那 7 行。
	if _, err := SeedSections(); err != nil {
		t.Fatalf("写入板块种子失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + writeTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

// newDraftNode 建一篇通用节点 + 一篇未绑定的接口草稿。
func newDraftNode(t *testing.T) (string, string) {
	t.Helper()
	n, err := CreateNode(NodeCreateInput{
		Key: "test-node", Kind: model.DocNodeGeneral, NodeInput: NodeInput{
			Title: "测试节点", Section: apidoc.SectionGeneral,
		},
	}, 1)
	if err != nil {
		t.Fatalf("建节点失败: %v", err)
	}
	if n.Status != model.DocDraft {
		t.Fatalf("新节点必须是草稿，实际 %s", n.Status)
	}
	e, err := CreateEndpoint("test-node", EndpointCreateInput{
		Key: "create", EndpointInput: EndpointInput{
			Name: "测试接口", Method: "POST", Path: "/v1/test",
			Fields: []FieldView{fv("prompt", "body", true)},
			Response: AdminResponse{Variants: []AdminVariant{
				{Status: 200, Label: "成功", Returns: []FieldView{fv("url", "body", false)}},
			}},
		},
	}, 1)
	if err != nil {
		t.Fatalf("建接口失败: %v", err)
	}
	if e.Status != model.DocDraft {
		t.Fatalf("新接口必须是草稿，实际 %s", e.Status)
	}
	return "test-node", "create"
}

// curVersion 取当前版本号（乐观锁要求保存必须带它）。
// ep 传空时按节点读。
func curVersion(t *testing.T, node, ep string) int {
	t.Helper()
	if ep == "" {
		n, err := LoadAdminNode(node)
		if err != nil {
			t.Fatalf("读节点失败: %v", err)
		}
		return n.Version
	}
	e, err := LoadAdminEndpoint(node, ep)
	if err != nil {
		t.Fatalf("读接口失败: %v", err)
	}
	return e.Version
}

func fv(name, in string, required bool) FieldView {
	req := "optional"
	if required {
		req = "required"
	}
	return FieldView{Name: name, In: in, Type: "string", Required: req, Tier: "standard"}
}

func TestSaveEndpointReplacesChildren(t *testing.T) {
	withWriteDB(t)
	node, ep := newDraftNode(t)

	// 第一次保存：两个请求字段 + 一个枚举。
	cur, err := LoadAdminEndpoint(node, ep)
	if err != nil {
		t.Fatalf("读回草稿失败: %v", err)
	}
	got, err := SaveEndpoint(node, ep, EndpointInput{
		Name: "测试接口", Method: "POST", Path: "/v1/test", Version: cur.Version,
		Fields: []FieldView{
			{Name: "model", In: "body", Type: "string", Required: "required",
				Enum: []EnumView{{Value: "a", Desc: "A"}, {Value: "b"}}},
			fv("prompt", "body", true),
		},
		Response: AdminResponse{Kind: "json", Variants: []AdminVariant{
			{Status: 200, Label: "成功", Returns: []FieldView{fv("url", "body", false)}},
		}},
	}, model.DocOriginManual, 1)
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if len(got.Fields) != 2 || len(got.Fields[0].Enum) != 2 {
		t.Fatalf("字段/枚举没存进去: %+v", got.Fields)
	}

	// 第二次保存：只留一个字段 —— 必须变成 1 行，而不是旧的两行还在。
	got, err = SaveEndpoint(node, ep, EndpointInput{
		Name: "测试接口", Method: "POST", Path: "/v1/test", Version: got.Version,
		Fields: []FieldView{fv("model", "body", true)},
		Response: AdminResponse{Kind: "json", Variants: []AdminVariant{
			{Status: 200, Label: "成功", Returns: []FieldView{}},
		}},
	}, model.DocOriginManual, 1)
	if err != nil {
		t.Fatalf("二次保存失败: %v", err)
	}
	if len(got.Fields) != 1 || got.Fields[0].Name != "model" {
		t.Fatalf("整篇保存必须全量替换，实际 %+v", got.Fields)
	}
	var enumCount int64
	if err := db.DB.Model(&model.DocFieldEnum{}).Count(&enumCount).Error; err != nil {
		t.Fatalf("统计枚举失败: %v", err)
	}
	if enumCount != 0 {
		t.Fatalf("旧字段的枚举必须一并清掉（孤儿枚举行会让页面显示不存在的候选值），剩 %d 行", enumCount)
	}
	// 返回字段属于被替换的变体，旧行同样不能残留。
	var returnRows int64
	if err := db.DB.Model(&model.DocEndpointField{}).Where("variant_id IS NOT NULL").Count(&returnRows).Error; err != nil {
		t.Fatalf("统计返回字段失败: %v", err)
	}
	if returnRows != 0 {
		t.Fatalf("变体返回字段必须随变体一起重建，残留 %d 行", returnRows)
	}
}

// 保存 = 立即生效：已发布的仍是发布态，草稿仍是草稿。
//
// 这条以前是反的（保存一律退回草稿），于是运营改完点保存，前台不是「变了」
// 而是「整篇 404」—— 谁都在怀疑文档到底有没有连到后台。
func TestSaveKeepsPublishedStatus(t *testing.T) {
	withWriteDB(t)
	node, _ := newDraftNode(t)
	tr, ok := apidoc.TransportOfDocID("images/generations")
	if !ok {
		t.Fatal("契约里必须有 images/generations")
	}
	if _, err := CreateEndpoint(node, EndpointCreateInput{
		Key: "gen", EndpointInput: EndpointInput{
			Name: "生成图片", Method: tr.Method, Path: tr.Path,
			TransportID: "images/generations",
			Fields:      []FieldView{fv("prompt", "body", true), fv("async", "body", false)},
			Response: AdminResponse{Kind: "json", Variants: []AdminVariant{
				{Status: 200, Label: "成功", Returns: []FieldView{fv("url", "body", false)}},
				{Status: 202, Label: "已受理", Returns: []FieldView{fv("task_id", "body", false)}},
			}},
		},
	}, 1); err != nil {
		t.Fatalf("建接口失败: %v", err)
	}
	if _, _, err := PublishEndpoint(node, "gen", 1); err != nil {
		t.Fatalf("发布失败: %v", err)
	}

	got, err := SaveEndpoint(node, "gen", EndpointInput{
		Name: "改了一个字", Method: tr.Method, Path: tr.Path,
		TransportID: "images/generations", Version: curVersion(t, node, "gen"),
		Fields: []FieldView{fv("prompt", "body", true), fv("async", "body", false)},
		Response: AdminResponse{Kind: "json", Variants: []AdminVariant{
			{Status: 200, Label: "成功", Returns: []FieldView{fv("url", "body", false)}},
			{Status: 202, Label: "已受理", Returns: []FieldView{fv("task_id", "body", false)}},
		}},
	}, model.DocOriginManual, 1)
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if got.Status != model.DocPublished {
		t.Fatalf("已发布的接口保存后必须仍是发布态（否则前台会整篇消失），实际 %s", got.Status)
	}
	if got.Name != "改了一个字" {
		t.Fatalf("内容必须已经写进库，实际 %q", got.Name)
	}
}

// 发布态的内容不许被改坏：体检不过就整体回滚，库里一个字都不变。
func TestSavePublishedRollsBackOnBlockingProblem(t *testing.T) {
	withWriteDB(t)
	node, _ := newDraftNode(t)
	tr, ok := apidoc.TransportOfDocID("images/generations")
	if !ok {
		t.Fatal("契约里必须有 images/generations")
	}
	if _, err := CreateEndpoint(node, EndpointCreateInput{
		Key: "gen", EndpointInput: EndpointInput{
			Name: "生成图片", Method: tr.Method, Path: tr.Path,
			TransportID: "images/generations",
			Fields:      []FieldView{fv("prompt", "body", true), fv("async", "body", false)},
			Response: AdminResponse{Kind: "json", Variants: []AdminVariant{
				{Status: 200, Label: "成功", Returns: []FieldView{fv("url", "body", false)}},
				{Status: 202, Label: "已受理", Returns: []FieldView{fv("task_id", "body", false)}},
			}},
		},
	}, 1); err != nil {
		t.Fatalf("建接口失败: %v", err)
	}
	if _, _, err := PublishEndpoint(node, "gen", 1); err != nil {
		t.Fatalf("发布失败: %v", err)
	}

	// 改掉路径 → 与契约漂移（E_PATH_DRIFT，阻断）。
	_, err := SaveEndpoint(node, "gen", EndpointInput{
		Name: "改坏了", Method: tr.Method, Path: "/v1/not-the-real-path",
		TransportID: "images/generations", Version: curVersion(t, node, "gen"),
		Fields: []FieldView{fv("prompt", "body", true)},
	}, model.DocOriginManual, 1)
	if err == nil {
		t.Fatal("发布态的文档改成漂移路径必须被拦下")
	}
	if blocked, ok := err.(*BlockedError); !ok || !hasCode(blocked.Problems, "E_PATH_DRIFT") {
		t.Fatalf("必须点名 E_PATH_DRIFT，实际 %v", err)
	}
	after, err := LoadAdminEndpoint(node, "gen")
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if after.Name != "生成图片" || after.Path != tr.Path || after.Status != model.DocPublished {
		t.Fatalf("被拦下时库里必须原样不动，实际 name=%q path=%q status=%s",
			after.Name, after.Path, after.Status)
	}
}

func TestWriteUnboundCannotPublish(t *testing.T) {
	withWriteDB(t)
	node, ep := newDraftNode(t)

	_, _, err := PublishEndpoint(node, ep, 1)
	var blocked *BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("未绑定的文档必须拒绝发布，实际 err=%v", err)
	}
	if !hasCode(blocked.Problems, "E_UNBOUND") {
		t.Fatalf("拒绝理由必须点名 E_UNBOUND，实际 %+v", blocked.Problems)
	}
}

func TestBoundEndpointPublishes(t *testing.T) {
	withWriteDB(t)
	node, _ := newDraftNode(t)
	tr, ok := apidoc.TransportOfDocID("images/generations")
	if !ok {
		t.Fatal("契约里必须有 images/generations")
	}
	// 没有已发布接口的节点不能发布：发布出去就是一张白纸。
	if _, err := PublishNode(node, 1); err == nil {
		t.Fatal("空节点（无已发布接口）必须拒绝发布")
	} else if blocked, ok := err.(*BlockedError); !ok || !hasCode(blocked.Problems, "E_NODE_NO_ENDPOINT") {
		t.Fatalf("必须点名 E_NODE_NO_ENDPOINT，实际 %v", err)
	}
	if _, err := CreateEndpoint(node, EndpointCreateInput{
		Key: "gen", EndpointInput: EndpointInput{
			Name: "生成图片", Method: tr.Method, Path: tr.Path,
			TransportID: "images/generations",
			Fields:      []FieldView{fv("prompt", "body", true), fv("async", "body", false)},
			Response: AdminResponse{Kind: "json", Variants: []AdminVariant{
				{Status: 200, Label: "成功", Returns: []FieldView{fv("url", "body", false)}},
				{Status: 202, Label: "已受理", Returns: []FieldView{fv("task_id", "body", false)}},
			}},
		},
	}, 1); err != nil {
		t.Fatalf("建接口失败: %v", err)
	}
	// 先发接口（节点仍是草稿，只告警），再发节点 —— 这是唯一走得通的顺序。
	got, _, err := PublishEndpoint(node, "gen", 1)
	if err != nil {
		t.Fatalf("绑定契约且字段齐全时必须能发布，实际 %v", err)
	}
	if _, err := PublishNode(node, 1); err != nil {
		t.Fatalf("有已发布接口后节点必须能发布，实际 %v", err)
	}
	if got.Status != model.DocPublished {
		t.Fatalf("发布后状态应为 published，实际 %s", got.Status)
	}
	if got.Phase == "" || got.Badge == "" {
		t.Fatalf("phase/badge 必须由绑定契约派生（手填迟早与真实端点不一致），实际 %q/%q",
			got.Phase, got.Badge)
	}
}

// TestEndpointPublishUnderDraftNodeIsAllowed 守「两条闸门不能互为前提」。
//
// 节点发布要求「有已发布接口」，接口发布不能再要求「节点已发布」——
// 否则新节点永远发不出去（谁先谁死）。所以接口侧只留一条告警，真正决定
// 可见性的是节点状态：节点草稿时它确实不在目录里。
// 一个动作上线一整页：接口发布时节点若还是草稿，一并发布。
//
// 以前是两层各自发布，运营点完「发布」页面还是不出现（节点草稿），
// 而界面上没有任何东西提示他还要点第二次 —— 「我明明发布了」就是这么来的。
func TestPublishEndpointAlsoPublishesDraftNode(t *testing.T) {
	withWriteDB(t)
	node, _ := newDraftNode(t)
	tr, _ := apidoc.TransportOfDocID("images/generations")
	if _, err := CreateEndpoint(node, EndpointCreateInput{
		Key: "gen", EndpointInput: EndpointInput{
			Name: "生成图片", Method: tr.Method, Path: tr.Path,
			TransportID: "images/generations",
			Fields:      []FieldView{fv("async", "body", false)},
			Response: AdminResponse{Variants: []AdminVariant{
				{Status: 200, Label: "成功"}, {Status: 202, Label: "已受理"},
			}},
		},
	}, 1); err != nil {
		t.Fatalf("建接口失败: %v", err)
	}
	ep, nb, err := PublishEndpoint(node, "gen", 1)
	if err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	if ep.Status != model.DocPublished {
		t.Fatalf("接口应是发布态，实际 %s", ep.Status)
	}
	if nb == nil || nb.Status != model.DocPublished {
		t.Fatalf("节点必须跟着发布（否则页面对读者不存在），实际 %+v", nb)
	}
	tree, err := LoadTree()
	if err != nil {
		t.Fatalf("装载目录失败: %v", err)
	}
	found := false
	for _, n := range tree.Nodes {
		if n.Key == node {
			found = true
		}
	}
	if !found {
		t.Fatal("发布后节点必须出现在公开目录里")
	}
}

func TestVersionConflict(t *testing.T) {
	withWriteDB(t)
	node, ep := newDraftNode(t)

	cur, err := LoadAdminEndpoint(node, ep)
	if err != nil {
		t.Fatalf("读回草稿失败: %v", err)
	}
	if _, err := SaveEndpoint(node, ep, EndpointInput{
		Name: "A", Method: "POST", Path: "/v1/test", Version: cur.Version,
	}, model.DocOriginManual, 1); err != nil {
		t.Fatalf("第一次保存失败: %v", err)
	}
	// 拿着旧 version 再存 → 409，而不是静默覆盖别人的改动。
	if _, err := SaveEndpoint(node, ep, EndpointInput{
		Name: "B", Method: "POST", Path: "/v1/test", Version: cur.Version,
	}, model.DocOriginManual, 1); !errors.Is(err, ErrDocVersion) {
		t.Fatalf("旧 version 必须报冲突，实际 %v", err)
	}
}

func TestPreviewReflectsUnsavedInput(t *testing.T) {
	withWriteDB(t)
	node, ep := newDraftNode(t)

	out, err := PreviewEndpointInput(node, ep, EndpointInput{
		Name: "还没保存的名字", Method: "POST", Path: "/v1/test",
		Fields: []FieldView{fv("brand_new", "body", true)},
		Response: AdminResponse{Kind: "json", Variants: []AdminVariant{
			{Status: 200, Label: "成功", Returns: []FieldView{fv("ok", "body", false)}},
		}},
		// 示例代码是内容：预览显示的就是**输入里那一段**，只把占位符换成站点地址。
		Samples: &SamplesView{Curl: "curl {{base_url}}/v1/test"},
	}, "https://api.example.com")
	if err != nil {
		t.Fatalf("预览失败: %v", err)
	}
	if out.Name != "还没保存的名字" {
		t.Fatalf("预览必须反映未保存的输入，实际 name=%q", out.Name)
	}
	var names []string
	for _, f := range out.Fields {
		names = append(names, f.Name)
	}
	if len(names) != 1 || names[0] != "brand_new" {
		t.Fatalf("预览里的字段应与输入一致，实际 %v", names)
	}
	// 示例代码来自输入（内容），预览期只替换基址 —— 与保存后逐字一致。
	if out.Samples == nil || out.Samples.Curl != "curl https://api.example.com/v1/test" {
		t.Fatalf("预览里的示例代码必须与输入一致（只换基址），实际 %+v", out.Samples)
	}
	// 且不能写库。
	var cnt int64
	if err := db.DB.Model(&model.DocEndpointField{}).Where("name = ?", "brand_new").
		Count(&cnt).Error; err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if cnt != 0 {
		t.Fatal("预览不许落库")
	}
}

func TestSaveEndpointRejectsDuplicateEnum(t *testing.T) {
	withWriteDB(t)
	node, ep := newDraftNode(t)
	_, err := SaveEndpoint(node, ep, EndpointInput{
		Name: "x", Method: "POST", Path: "/v1/test", Version: curVersion(t, node, ep),
		Fields: []FieldView{{Name: "model", In: "body", Type: "string",
			Enum: []EnumView{{Value: "a"}, {Value: "a"}}}},
	}, model.DocOriginManual, 1)
	if err == nil {
		t.Fatal("重复枚举值必须拒绝保存（唯一索引会让它在库里报 23505，用户看不懂）")
	}
}

// TestWriteFieldsKeepsReturnFields 守数据丢失级缺陷：局部写请求字段不能删返回字段。
//
// 旧实现按 endpoint_id 全删，症状是「AI 补了几个请求字段，页面上所有返回字段
// 全空了」—— 体检不会报（它只查重名与位置），只有读者会发现。
func TestWriteFieldsKeepsReturnFields(t *testing.T) {
	gdb := withWriteDB(t)
	node, ep := newDraftNode(t)
	var before int64
	if err := gdb.Model(&model.DocEndpointField{}).Where("variant_id IS NOT NULL").Count(&before).Error; err != nil {
		t.Fatalf("统计返回字段失败: %v", err)
	}
	if before == 0 {
		t.Fatal("前置条件不成立：新草稿应当有 1 个返回字段")
	}
	if _, err := WriteFields(node, ep, []FieldView{fv("prompt", "body", true)},
		curVersion(t, node, ep), model.DocOriginAI, 1); err != nil {
		t.Fatalf("局部写失败: %v", err)
	}
	var after int64
	if err := gdb.Model(&model.DocEndpointField{}).Where("variant_id IS NOT NULL").Count(&after).Error; err != nil {
		t.Fatalf("统计返回字段失败: %v", err)
	}
	if after != before {
		t.Fatalf("局部写请求字段不该动返回字段：写前 %d 行，写后 %d 行", before, after)
	}
}

// TestConcurrentSaveConflicts 守「乐观锁真的在 SQL 里」。
//
// 两个请求带同一版本号并发保存：必须恰好一个成功、一个 409。
// 读出来比较再无条件更新时，两个都会成功，后写的静默覆盖前一个。
func TestConcurrentSaveConflicts(t *testing.T) {
	withWriteDB(t)
	node, ep := newDraftNode(t)
	ver := curVersion(t, node, ep)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = SaveEndpoint(node, ep, EndpointInput{
				Name: fmt.Sprintf("并发-%d", i), Method: "POST", Path: "/v1/test", Version: ver,
			}, model.DocOriginManual, 1)
		}(i)
	}
	wg.Wait()
	ok, conflict := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrDocVersion):
			conflict++
		default:
			t.Fatalf("并发保存出现意外错误: %v", err)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("必须恰好一个成功、一个冲突，实际成功 %d / 冲突 %d", ok, conflict)
	}
}

// TestUniqueConflictMapsTo409 守「重名不是 500」。
//
// 索引必须手动建：`uq_doc_nodes_key` 是**部分唯一索引**（`WHERE key <> ”`），
// AutoMigrate 表达不了它，而这里要验证的正是「真实驱动报错里的约束名能否被解析」。
func TestUniqueConflictMapsTo409(t *testing.T) {
	gdb := withWriteDB(t)
	if err := gdb.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_doc_nodes_key ON doc_nodes (key) WHERE key <> ''`).Error; err != nil {
		t.Fatalf("建唯一索引失败: %v", err)
	}
	in := NodeCreateInput{Key: "dup-node", Kind: model.DocNodeGeneral,
		NodeInput: NodeInput{Title: "重复", Section: apidoc.SectionGeneral}}
	if _, err := CreateNode(in, 1); err != nil {
		t.Fatalf("首次建节点失败: %v", err)
	}
	if _, err := CreateNode(in, 1); !errors.Is(err, ErrDocKeyTaken) {
		t.Fatalf("重名必须映射成「标识被占用」（409），实际 %v", err)
	}
}

// TestMapWriteErrorCoversEveryConstraint 守「映射表与索引名逐字一致」。
//
// 约束名的唯一来源是 `db/indexes.go`；这里用真实 PG 报错文本回放，
// 避免「映射表写了一个不存在的索引名」这种只在线上才暴露的错误。
func TestMapWriteErrorCoversEveryConstraint(t *testing.T) {
	toErr := func(name string) error {
		return errors.New(`ERROR: duplicate key value violates unique constraint "` + name + `" (SQLSTATE 23505)`)
	}
	if err := mapWriteError(toErr("uq_doc_nodes_key")); !errors.Is(err, ErrDocKeyTaken) {
		t.Fatalf("uq_doc_nodes_key 应映射成 ErrDocKeyTaken，实际 %v", err)
	}
	for _, name := range []string{
		"uq_doc_endpoints_transport", "uq_doc_node_models_family", "uq_doc_fields_req",
		"uq_doc_fields_resp", "uq_doc_response_variants", "uq_doc_presets_key", "uq_doc_endpoint_errors",
	} {
		var inErr *InputError
		if err := mapWriteError(toErr(name)); !errors.As(err, &inErr) {
			t.Fatalf("%s 应映射成输入错误（400），实际 %v", name, err)
		}
	}
	// 非唯一冲突原样返回：不要把「数据库挂了」说成「名字重复」。
	raw := errors.New("connection reset by peer")
	if err := mapWriteError(raw); err != raw {
		t.Fatalf("非唯一冲突必须原样返回，实际 %v", err)
	}
}

// TestSketchSkipRoundTrip 守「保存一次不丢列」。
func TestSketchSkipRoundTrip(t *testing.T) {
	withWriteDB(t)
	node, ep := newDraftNode(t)
	ret := fv("url", "body", false)
	ret.SketchSkip = true
	if _, err := SaveEndpoint(node, ep, EndpointInput{
		Name: "x", Method: "POST", Path: "/v1/test", Version: curVersion(t, node, ep),
		Fields: []FieldView{fv("prompt", "body", true)},
		Response: AdminResponse{Kind: "json", Variants: []AdminVariant{
			{Status: 200, Label: "成功", Returns: []FieldView{ret}},
		}},
	}, model.DocOriginManual, 1); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	got, err := LoadAdminEndpoint(node, ep)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if len(got.Response.Variants) == 0 || len(got.Response.Variants[0].Returns) == 0 {
		t.Fatal("返回字段丢失")
	}
	if !got.Response.Variants[0].Returns[0].SketchSkip {
		t.Fatal("sketch_skip 在保存后丢失（编辑一次就会被清零）")
	}
}

// TestErrorSourcePreserved 守「错误码来源不被改写」。
//
// 播种写 seed、AI 写 ai、人工写 manual；保存一次就全变 manual 的话，
// 「哪些是出厂、哪些是人写的」这条线索永久消失。
func TestErrorSourcePreserved(t *testing.T) {
	withWriteDB(t)
	node, ep := newDraftNode(t)
	if _, err := SaveEndpoint(node, ep, EndpointInput{
		Name: "x", Method: "POST", Path: "/v1/test", Version: curVersion(t, node, ep),
		Fields: []FieldView{fv("prompt", "body", true)},
		Response: AdminResponse{Kind: "json", Variants: []AdminVariant{
			{Status: 200, Label: "成功", Returns: []FieldView{}},
		}},
		Errors: []AdminError{{Status: 400, Code: "invalid_request", Desc: "参数不对", Source: model.DocOriginSeed}},
	}, model.DocOriginManual, 1); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	got, err := LoadAdminEndpoint(node, ep)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if len(got.Errors) != 1 || got.Errors[0].Source != model.DocOriginSeed {
		t.Fatalf("错误码来源应保持 seed，实际 %+v", got.Errors)
	}
}

// 后台改一个字，前台就是那个字 —— 这条链路是本系统唯一的验收标准。
//
// 走的是真实路径：SaveEndpoint（= 管理台保存）→ PublicEndpoint（= 文档站的读口）。
// 中间不允许有任何一层把库里的话替换成代码里的话。
func TestAdminEditShowsUpOnPublicPage(t *testing.T) {
	withWriteDB(t)
	node, _ := newDraftNode(t)
	tr, ok := apidoc.TransportOfDocID("images/generations")
	if !ok {
		t.Fatal("契约里必须有 images/generations")
	}
	if _, err := CreateEndpoint(node, EndpointCreateInput{
		Key: "gen", EndpointInput: EndpointInput{
			Name: "生成图片", Summary: "旧简介", Method: tr.Method, Path: tr.Path,
			TransportID: "images/generations",
			Notes:       []string{"旧的注意事项"},
			Fields: []FieldView{{
				Name: "size", In: "body", Type: "string", Required: "optional", Tier: "standard",
				Desc: "旧说明", OnlyModels: "旧标注",
			}, fv("async", "body", false)},
			Response: AdminResponse{Kind: "json", Variants: []AdminVariant{
				{Status: 200, Label: "成功", Returns: []FieldView{fv("url", "body", false)}},
				{Status: 202, Label: "已受理", Returns: []FieldView{fv("task_id", "body", false)}},
			}},
		},
	}, 1); err != nil {
		t.Fatalf("建接口失败: %v", err)
	}
	if _, _, err := PublishEndpoint(node, "gen", 1); err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	if _, err := PublishNode(node, 1); err != nil {
		t.Fatalf("发布节点失败: %v", err)
	}

	// 管理台把每一处文案都改成 123（含以前根本没有输入框的那几列）。
	if _, err := SaveEndpoint(node, "gen", EndpointInput{
		Name: "生成图片", Summary: "123", Method: tr.Method, Path: tr.Path,
		TransportID: "images/generations", Version: curVersion(t, node, "gen"),
		Notes: []string{"123"},
		Fields: []FieldView{{
			Name: "size", In: "body", Type: "string", Required: "optional", Tier: "V",
			Desc: "123", OnlyModels: "123", Passthrough: true,
		}, fv("async", "body", false)},
		Response: AdminResponse{Kind: "json", Variants: []AdminVariant{
			{Status: 200, Label: "成功", Returns: []FieldView{fv("url", "body", false)}},
			{Status: 202, Label: "已受理", Returns: []FieldView{fv("task_id", "body", false)}},
		}},
	}, model.DocOriginManual, 1); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	b, err := LoadEndpoint(node, "gen")
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if b.EP.Status != model.DocPublished {
		t.Fatalf("保存后必须仍在发布态，实际 %s", b.EP.Status)
	}
	page := ProjectEndpoint(b, "")
	if page.Summary != "123" {
		t.Fatalf("简介应显示库里的新值，实际 %q", page.Summary)
	}
	if len(page.Notes) != 1 || page.Notes[0] != "123" {
		t.Fatalf("注意事项应显示库里的新值，实际 %q", page.Notes)
	}
	var size *FieldView
	for i := range page.Fields {
		if page.Fields[i].Name == "size" {
			size = &page.Fields[i]
		}
	}
	if size == nil {
		t.Fatal("页面里找不到 size 字段")
	}
	if size.Desc != "123" || size.OnlyModels != "123" {
		t.Fatalf("字段的说明/仅模型标注都必须来自库：%+v", *size)
	}
	// 层级与「原样转发」是 chip 的来源，同样逐字生效。
	if size.Tier != "V" || !size.Passthrough {
		t.Fatalf("层级/原样转发必须来自库：tier=%q passthrough=%v", size.Tier, size.Passthrough)
	}
}
