package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/model"
)

// 本文件覆盖管理台用户列表的服务端排序：排序值是**外部输入**，必须经白名单映射成
// 真实列名后才能进 ORDER BY。`group` 是 PostgreSQL 保留字，裸写会 42601 → 500。

// TestAdminUserOrderQuotesReservedColumns 守映射表本身：保留字列必须带双引号，
// 普通列原样，未知列报错（不能把参数名直接拼进 SQL）。
func TestAdminUserOrderQuotesReservedColumns(t *testing.T) {
	cases := []struct {
		field string
		desc  bool
		want  string
	}{
		{"group", true, `"group" DESC`},
		{"group", false, `"group" ASC`},
		{"created_at", true, "created_at DESC"},
		{"id", false, "id ASC"},
		// 缺省顺序必须与历史行为一致（没有 sort 参数时列表按创建时间倒序）。
		{"", false, "created_at DESC"},
	}
	for _, c := range cases {
		got, err := adminUserOrder(c.field, c.desc)
		if err != nil {
			t.Fatalf("field=%q 不应报错：%v", c.field, err)
		}
		if got != c.want {
			t.Fatalf("field=%q desc=%v: got %q want %q", c.field, c.desc, got, c.want)
		}
	}
	for _, bad := range []string{"password_hash", "group; DROP TABLE users", "Group", "1"} {
		if _, err := adminUserOrder(bad, false); err == nil {
			t.Fatalf("未知排序列 %q 必须报错（否则等于把外部输入拼进 ORDER BY）", bad)
		}
	}
}

// TestAdminListUsersSortByGroup 走真实 SQL：按 group 排序在过去必 500（42601）。
func TestAdminListUsersSortByGroup(t *testing.T) {
	gdb := withAdminUserDB(t)
	gin.SetMode(gin.TestMode)

	// 列表的 key_count 要 join api_keys；测试 schema 里没有这张表时整条接口会 500，
	// 与本用例要验证的排序无关，这里显式补齐。
	if err := gdb.AutoMigrate(&model.APIKey{}); err != nil {
		t.Fatalf("迁移 api_keys 失败: %v", err)
	}

	for _, g := range []string{"beta", "alpha"} {
		u := model.User{Username: "u-" + g, PasswordHash: "x", Group: g, Status: 1}
		if err := gdb.Create(&u).Error; err != nil {
			t.Fatalf("造用户失败: %v", err)
		}
	}

	r := gin.New()
	r.GET("/api/admin/users", AdminListUsers)

	call := func(query string) (int, []string) {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/admin/users"+query, nil))
		var body struct {
			Data struct {
				Items []struct {
					Group string `json:"group"`
				} `json:"items"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		groups := make([]string, 0, len(body.Data.Items))
		for _, it := range body.Data.Items {
			groups = append(groups, it.Group)
		}
		return w.Code, groups
	}

	code, groups := call("?sort=group:desc")
	if code != http.StatusOK {
		t.Fatalf("按 group 排序应 200，实际 %d（保留字未加引号会 500）", code)
	}
	if len(groups) != 2 || groups[0] != "beta" || groups[1] != "alpha" {
		t.Fatalf("group DESC 顺序应为 [beta alpha]，实际 %v", groups)
	}

	code, groups = call("?sort=group:asc")
	if code != http.StatusOK {
		t.Fatalf("按 group 升序应 200，实际 %d", code)
	}
	if len(groups) != 2 || groups[0] != "alpha" || groups[1] != "beta" {
		t.Fatalf("group ASC 顺序应为 [alpha beta]，实际 %v", groups)
	}

	// 过滤与排序同时用：两条路径都必须正确引号，且结果互不干扰。
	code, groups = call("?group=alpha&sort=group:desc")
	if code != http.StatusOK {
		t.Fatalf("过滤 + 排序应 200，实际 %d", code)
	}
	if len(groups) != 1 || groups[0] != "alpha" {
		t.Fatalf("过滤 group=alpha 应只剩 alpha，实际 %v", groups)
	}

	// 未知列仍按参数错拒绝，不能落成 500。
	if code, _ := call("?sort=password_hash:desc"); code != http.StatusBadRequest {
		t.Fatalf("未知排序列应 400，实际 %d", code)
	}
}
