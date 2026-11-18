package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/model"
	"lingdong-api/internal/service/task"
	"lingdong-api/internal/service/taskview"
)

// 本文件覆盖 B6 新增的两条「按序号取产物」入口的**路径参数守卫**。
//
// 为什么这些用例要在无数据库环境下跑：序号解析必须发生在 task.Get 之前。
// 反过来说，只要用例能返回 400/404 而不是 panic，就证明拿库之前已经拦住了 ——
// 这也是「index 允许 0」这条规则唯一能低成本锁死的地方
// （uintParam 会拒 0，误用它会让第 1 个产物永远下载不了）。

// callContentAt 构造一次 GET /tasks/:id/contents/:index 调用（不注入数据库）。
func callContentAt(t *testing.T, h gin.HandlerFunc, target string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/tasks/:id/contents/:index", func(c *gin.Context) {
		c.Set("user_id", uint64(1))
		h(c)
	})
	req := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestPortalTaskContentAtRejectsBadIndex(t *testing.T) {
	cases := []string{
		"/tasks/1/contents/abc", // 非数字
		"/tasks/1/contents/-1",  // 负数
		"/tasks/1/contents/1.5", // 小数
	}
	for _, p := range cases {
		if w := callContentAt(t, PortalTaskContentAt, p); w.Code != http.StatusBadRequest {
			t.Fatalf("%s 应 400，实际 %d（%s）", p, w.Code, w.Body.String())
		}
	}
}

func TestPortalTaskContentAtRejectsBadID(t *testing.T) {
	// id 非正整数 → **400**（参数非法），不是 404。
	//
	// 合成 404 会让按 error_code 分支的接入方把"我把 id 拼错了"读成
	// "任务不见了"，进而去查对账或重建任务；而这与"资源确实不存在"
	// （task_not_found）必须能区分。序号非法仍走 400，口径一致。
	for _, p := range []string{"/tasks/abc/contents/0", "/tasks/0/contents/0"} {
		if w := callContentAt(t, PortalTaskContentAt, p); w.Code != http.StatusBadRequest {
			t.Fatalf("%s 应 400，实际 %d（%s）", p, w.Code, w.Body.String())
		}
	}
}

func TestAdminTaskContentAtRejectsBadParams(t *testing.T) {
	if w := callContentAt(t, AdminTaskContentAt, "/tasks/1/contents/x"); w.Code != http.StatusBadRequest {
		t.Fatalf("非法序号应 400，实际 %d（%s）", w.Code, w.Body.String())
	}
	if w := callContentAt(t, AdminTaskContentAt, "/tasks/0/contents/0"); w.Code != http.StatusBadRequest {
		t.Fatalf("非法任务号应 400（与门户/开放 API 同口径），实际 %d（%s）", w.Code, w.Body.String())
	}
}

// TestTaskArtifactURLFollowsRecordBackend 守住「产物地址由记录的后端派生」。
//
// 与上传件同一条规则（storage.URLOr）：云端给对象存储地址，本地给平台入口。
// 写反的表现是「产物地址指向一个不存在的云对象」或「云上的产物还要过我们中转」，
// 两者都不会报错，只会让人以为存储没生效。
func TestTaskArtifactURLFollowsRecordBackend(t *testing.T) {
	prev := conf.System().All()
	t.Cleanup(func() { conf.System().Reload(prev) })
	conf.System().Set("storage", []byte(`{"active":"oss","channels":{
		"none":{},
		"oss":{"endpoint":"oss-cn-shanghai.aliyuncs.com","bucket":"ling-ai",
		       "access_key_id":"ak","access_key_secret":"sk"}}}`))

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/api/user/tasks/9", nil)

	row := &model.Task{ID: 9, PublicID: "task_01HZZZZZZZZZZZZZZZZZZZZZZZ", Status: task.StatusSucceeded}
	view := task.ViewOf(row, task.ViewOptions{
		// 直接喂路径而不是 c.FullPath()：这个 ctx 没走路由，FullPath 是空串。
		ContentBase: taskview.ContentBaseFor("/api/user/tasks/9"),
		Artifacts: []task.ArtifactEntry{
			{Index: 0, Backend: "oss", Bucket: "ling-ai", Key: "artifacts/1/9/0", Kind: "image"},
			{Index: 1, Backend: "none", Key: "artifacts/1/9/1", Kind: "image"},
		},
	})
	if want := "https://ling-ai.oss-cn-shanghai.aliyuncs.com/artifacts/1/9/0"; view.Contents[0].URL != want {
		t.Fatalf("云产物地址不符\n got=%s\nwant=%s", view.Contents[0].URL, want)
	}
	if !strings.HasSuffix(view.Contents[1].URL, "/api/user/tasks/"+row.PublicID+"/contents/1") {
		t.Fatalf("本地产物应给平台入口，实际 %s", view.Contents[1].URL)
	}
}
