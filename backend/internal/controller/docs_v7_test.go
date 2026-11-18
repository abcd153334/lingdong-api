package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/service/docdocs"
)

// 本文件守两条**会白屏 / 会误导**的对外契约：
//
//  1. 文档接口的 JSON 里，数组字段必须是 `[]`，不能是 `null`。
//     Go 把 nil slice 序列化成 null，前端（TS 里声明成 `T[]`）直接 `.length`，
//     整页被 ErrorBoundary 换成 "Something went wrong!"。HTTP 是 200、字段存在，
//     所以接口层看不出任何异常 —— 只有浏览器里才炸。
//  2. 草稿对公网必须 404。文档站是「发布了才算数」，否则运营改到一半的半成品
//     会被搜索引擎抓到。
//
// 为什么放在 controller 而不是 docdocs：服务层能保证自己返回非 nil 切片，
// 但 handler 用 `gin.H` 拼的一层（或未来新增的字段）绕过了那个保证，
// 只有真的走一遍 HTTP 序列化才看得见。

const ctrlDocsV7Schema = "ld_test_ctrl_docs_v7"

// docArrayKeys 是全项目「前端当数组用」的 JSON 键。
//
// 新增一个数组字段就加进来 —— 漏在这张表里，它变成 null 时门禁看不见。
var docArrayKeys = map[string]bool{
	"sections": true, "nodes": true, "endpoints": true, "members": true,
	"fields": true, "variants": true, "returns": true, "errors": true,
	"examples": true, "presets": true, "aliases": true, "models": true,
	"blocks": true, "items": true, "enum": true, "constraints": true,
	"notes": true, "values": true, "uploads": true, "tags": true,
}

func withDocsV7DB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("LD_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 LD_TEST_DSN，跳过文档站接口用例")
	}
	silent := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), silent)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := admin.Exec("DROP SCHEMA IF EXISTS " + ctrlDocsV7Schema + " CASCADE").Error; err != nil {
		t.Fatalf("清理测试 schema 失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA " + ctrlDocsV7Schema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+ctrlDocsV7Schema), silent)
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
	if _, err := docdocs.SeedSections(); err != nil {
		t.Fatalf("写入板块种子失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + ctrlDocsV7Schema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

// docsV7Router 只挂公开的三条文档路由（与本文件要验的东西一一对应）。
func docsV7Router() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/docs/tree", DocsTree)
	r.GET("/api/docs/nodes/:key", DocsNodeV7)
	r.GET("/api/docs/nodes/:key/endpoints/:ep", DocsEndpointV7)
	return r
}

// seedDocsV7 造「一个模型族 + 一篇接口」，返回节点 key 与接口 key。
//
// 状态直接写库而不是走 Publish*：本文件验的是 HTTP 契约（null 数组 / 可见性），
// 发布闸门本身在 docdocs 的写层用例里逐条守（未绑定、缺 202、节点草稿…），
// 在两层各写一份只会让"改发布规则要改两处"。
func seedDocsV7(t *testing.T, gdb *gorm.DB, status string) (string, string) {
	t.Helper()
	if err := gdb.Create(&model.Model{
		ID: "kling-v1", DisplayName: "KLING 1.0", Vendor: "kling", Modality: "video",
		BillingMode: model.BillingModeCall, Enabled: true,
		Endpoints: model.MustJSON([]string{"video/generations"}),
	}).Error; err != nil {
		t.Fatalf("建模型失败: %v", err)
	}
	n, err := docdocs.CreateNode(docdocs.NodeCreateInput{
		Key: "videos-kling", Kind: model.DocNodeFamily,
		NodeInput: docdocs.NodeInput{
			Title: "视频 · KLING", Section: apidoc.SectionOfKind(apidoc.Kind("video/submit")),
		},
	}, 1)
	if err != nil {
		t.Fatalf("建节点失败: %v", err)
	}
	tr, ok := apidoc.TransportOfDocID("video/submit")
	if !ok {
		t.Fatal("契约里没有 video/submit（改动端点身份后本用例要跟着改）")
	}
	if _, err := docdocs.CreateEndpoint(n.Key, docdocs.EndpointCreateInput{
		Key: "create", EndpointInput: docdocs.EndpointInput{
			Name: "创建视频任务", Summary: "提交一个视频生成任务。",
			Method: tr.Method, Path: tr.Path, TransportID: "video/submit",
			Fields: []docdocs.FieldView{
				{Name: "model", In: "body", Type: "string", Required: "required", Tier: "standard",
					Enum: []docdocs.EnumView{{Value: "kling-v1", Desc: "KLING 1.0"}}},
				{Name: "prompt", In: "body", Type: "string", Required: "required", Tier: "standard"},
			},
			Response: docdocs.AdminResponse{Kind: "json", Variants: []docdocs.AdminVariant{
				{Status: 200, Label: "成功", Returns: []docdocs.FieldView{
					{Name: "task_id", In: "body", Type: "string", Tier: "standard"},
				}},
			}},
		},
	}, 1); err != nil {
		t.Fatalf("建接口失败: %v", err)
	}
	// 直接置状态（见函数头注释）。
	var row model.DocNode
	if err := gdb.Where("key = ?", n.Key).Take(&row).Error; err != nil {
		t.Fatalf("读回节点失败: %v", err)
	}
	if err := gdb.Model(&model.DocNode{}).Where("id = ?", row.ID).
		Update("status", status).Error; err != nil {
		t.Fatalf("改节点状态失败: %v", err)
	}
	if err := gdb.Model(&model.DocEndpoint{}).Where("node_id = ?", row.ID).
		Update("status", status).Error; err != nil {
		t.Fatalf("改接口状态失败: %v", err)
	}
	return n.Key, "create"
}

// findNullArrays 递归返回「键是数组语义、值却是 null」的路径。
func findNullArrays(v any, path string) []string {
	var out []string
	switch node := v.(type) {
	case map[string]any:
		for k, child := range node {
			p := path + "." + k
			if child == nil && docArrayKeys[k] {
				out = append(out, p)
				continue
			}
			out = append(out, findNullArrays(child, p)...)
		}
	case []any:
		for _, child := range node {
			out = append(out, findNullArrays(child, path+"[]")...)
		}
	}
	return out
}

// getDocsData 发一次请求并解出 `data`；期望 404 时用 getDocsStatus。
func getDocsData(t *testing.T, r *gin.Engine, path string) any {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s 返回 %d：%s", path, w.Code, w.Body.String())
	}
	var payload struct {
		Data any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("GET %s 响应不是合法 JSON：%v", path, err)
	}
	return payload.Data
}

func getDocsStatus(r *gin.Engine, path string) int {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w.Code
}

func TestDocsV7ResponsesHaveNoNullArrays(t *testing.T) {
	gdb := withDocsV7DB(t)
	node, ep := seedDocsV7(t, gdb, model.DocPublished)
	r := docsV7Router()

	paths := []string{
		"/api/docs/tree",
		"/api/docs/nodes/" + node,
		"/api/docs/nodes/" + node + "/endpoints/" + ep,
	}
	for _, p := range paths {
		data := getDocsData(t, r, p)
		for _, bad := range findNullArrays(data, p) {
			t.Errorf("%s 里 %s 是 null（前端 .length 会抛错、整页白屏）", p, bad)
		}
	}

	// 内容真的到了：字段表、响应变体、适用模型（数组，可以为空）。
	epData, _ := getDocsData(t, r, paths[2]).(map[string]any)
	if fields, _ := epData["fields"].([]any); len(fields) == 0 {
		t.Fatal("接口页没有任何请求字段（种子与投影之间断了一环）")
	}
	resp, _ := epData["response"].(map[string]any)
	if variants, _ := resp["variants"].([]any); len(variants) == 0 {
		t.Fatal("接口页没有任何响应变体")
	}
	// 适用模型是派生量：模型没建绑定渠道时为空数组 —— 键必须在、且不能是 null。
	nodeData, _ := getDocsData(t, r, paths[1]).(map[string]any)
	if _, ok := nodeData["models"].([]any); !ok {
		t.Fatalf("节点适用模型必须是数组（0 个也是 []），实际 %v", nodeData["models"])
	}
	if notes, ok := epData["notes"].([]any); !ok || notes == nil {
		t.Fatalf("接口级注意事项必须是数组，实际 %v", epData["notes"])
	}
}

func TestDocsV7DraftIsInvisible(t *testing.T) {
	gdb := withDocsV7DB(t)
	node, ep := seedDocsV7(t, gdb, model.DocDraft)
	r := docsV7Router()

	// 草稿节点与草稿接口一律 404：草稿只在管理台预览里出现。
	if got := getDocsStatus(r, "/api/docs/nodes/"+node); got != http.StatusNotFound {
		t.Fatalf("草稿节点应 404，实际 %d", got)
	}
	if got := getDocsStatus(r, "/api/docs/nodes/"+node+"/endpoints/"+ep); got != http.StatusNotFound {
		t.Fatalf("草稿接口应 404，实际 %d", got)
	}
	// 目录里也不该出现这个节点的接口。
	tree, _ := getDocsData(t, r, "/api/docs/tree").(map[string]any)
	for _, s := range tree["sections"].([]any) {
		sec, _ := s.(map[string]any)
		for _, n := range sec["nodes"].([]any) {
			nd, _ := n.(map[string]any)
			if nd["key"] == node {
				if eps, _ := nd["endpoints"].([]any); len(eps) > 0 {
					t.Fatalf("草稿节点的接口不应出现在目录里：%v", eps)
				}
			}
		}
	}
}

func TestDocsV7UnknownIs404(t *testing.T) {
	withDocsV7DB(t)
	r := docsV7Router()
	if got := getDocsStatus(r, "/api/docs/nodes/nope"); got != http.StatusNotFound {
		t.Fatalf("未知节点应 404，实际 %d", got)
	}
	if got := getDocsStatus(r, "/api/docs/nodes/nope/endpoints/nope"); got != http.StatusNotFound {
		t.Fatalf("未知接口应 404，实际 %d", got)
	}
}
