package controller

// 用户列表的「按应用筛选」：语义是**持有关系**，不是归属。
//
// 三种"看着像、其实不该命中"的行必须被排除，否则运营会得到错误的用户名单：
//   - kind='user' 的自建密钥（app_id 恒 0，不代表任何应用）；
//   - 已软删的托管密钥（用户当前已不在该应用下持有密钥）；
//   - 其它应用下的密钥（这是筛选本身要解决的问题）。
// 另外 app_id=0 必须 400：用户池是全局的，不存在"属于平台直连的用户"。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/model"
)

func TestAdminListUsersFilterByApp(t *testing.T) {
	gdb := withAdminUserDB(t)
	gin.SetMode(gin.TestMode)
	if err := gdb.AutoMigrate(&model.APIKey{}); err != nil {
		t.Fatalf("迁移 api_keys 失败: %v", err)
	}

	mkUser := func(name string) *model.User {
		u := &model.User{Username: name, PasswordHash: "x", Group: "default", Status: 1}
		if err := gdb.Create(u).Error; err != nil {
			t.Fatalf("造用户 %s 失败: %v", name, err)
		}
		return u
	}
	mkKey := func(u *model.User, appID uint64, kind, hash string) *model.APIKey {
		k := &model.APIKey{UserID: u.ID, AppID: appID, Kind: kind, KeyHash: hash, Name: hash}
		if err := gdb.Create(k).Error; err != nil {
			t.Fatalf("造密钥 %s 失败: %v", hash, err)
		}
		return k
	}

	// 用户名统一带 appf- 前缀：本包的集成用例共用一个 schema 且不清表，
	// 用 keyword 把结果集收敛到本用例自己造的数据上。
	a := mkUser("appf-a") // 在应用 5 下持有托管密钥 → 命中
	b := mkUser("appf-b") // 在应用 9 下持有托管密钥 → 不命中 5
	c := mkUser("appf-c") // 只有自建密钥（app_id=5 但 kind=user）→ 不命中
	d := mkUser("appf-d") // 托管密钥已被软删 → 不命中
	mkUser("appf-e")      // 没有任何密钥 → 不命中

	mkKey(a, 5, model.KeyKindHosted, "appf-h-a")
	mkKey(b, 9, model.KeyKindHosted, "appf-h-b")
	mkKey(c, 5, model.KeyKindUser, "appf-u-c")
	gone := mkKey(d, 5, model.KeyKindHosted, "appf-h-d")
	if err := gdb.Delete(&model.APIKey{}, gone.ID).Error; err != nil {
		t.Fatalf("软删密钥失败: %v", err)
	}

	r := gin.New()
	r.GET("/api/admin/users", AdminListUsers)

	call := func(query string) (int, []string) {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/admin/users"+query, nil))
		var body struct {
			Data struct {
				Items []struct {
					Username string `json:"username"`
				} `json:"items"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		names := make([]string, 0, len(body.Data.Items))
		for _, it := range body.Data.Items {
			names = append(names, it.Username)
		}
		return w.Code, names
	}

	code, names := call("?keyword=appf-&app_id=5")
	if code != http.StatusOK {
		t.Fatalf("按应用筛选应 200，实际 %d", code)
	}
	if len(names) != 1 || names[0] != "appf-a" {
		t.Fatalf("应用 5 下应只剩 appf-a，实际 %v", names)
	}

	if code, names := call("?keyword=appf-&app_id=9"); code != http.StatusOK || len(names) != 1 || names[0] != "appf-b" {
		t.Fatalf("应用 9 下应只剩 appf-b，实际 code=%d %v", code, names)
	}

	// 不筛应用：五个人都在（证明上面的"只剩一个"确实来自筛选）。
	if code, names := call("?keyword=appf-"); code != http.StatusOK || len(names) != 5 {
		t.Fatalf("不筛应用应有 5 个用户，实际 code=%d %v", code, names)
	}

	// 用户池是全局的：app_id=0 有语义但没有对应身份 ⇒ 400，而不是静默返回全量。
	if code, _ := call("?keyword=appf-&app_id=0"); code != http.StatusBadRequest {
		t.Fatalf("app_id=0 应拒绝（400），实际 %d", code)
	}
	// 非法值同样 400：传错参数不能当成"没传"。
	if code, _ := call("?keyword=appf-&app_id=abc"); code != http.StatusBadRequest {
		t.Fatalf("app_id=abc 应 400，实际 %d", code)
	}
}
