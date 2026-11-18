package response

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/pkg/errs"
)

// TestEncodeMatchesOK 锁住「Encode 与 OK 逐字节相同」。
//
// 带 ETag 的端点走 Encode，其余端点走 OK。两条路径一旦分叉（多一个换行、
// HTML 不转义、信封少一个字段），同一份数据在两种响应里的 ETag 与体积就会不同，
// 而表现只是"某些请求永远拿不到 304"，极难归因。
func TestEncodeMatchesOK(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 覆盖三种易漂移的输入：HTML 特殊字符（转义策略）、nil 切片（null vs []）、
	// 嵌套结构（字段顺序由 struct 决定，两路必须一致）。
	payloads := []any{
		gin.H{"ok": true},
		gin.H{"html": `<a href="x">&</a>`, "n": nil, "list": []string{}},
		struct {
			A string   `json:"a"`
			B []int    `json:"b"`
			C *string  `json:"c"`
			D struct{} `json:"d"`
		}{A: "<x>", B: []int{1, 2}},
	}

	for i, p := range payloads {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		OK(c, p)

		raw, err := Encode(p)
		if err != nil {
			t.Fatalf("case %d: Encode 失败: %v", i, err)
		}
		if !bytes.Equal(w.Body.Bytes(), raw) {
			t.Fatalf("case %d: Encode 与 OK 字节不一致\nOK    =%s\nEncode=%s", i, w.Body.Bytes(), raw)
		}
	}
}

// TestFailHelpersAlwaysWriteErrorCode 锁住「失败响应必须带 error_code」。
//
// 这条契约有两个消费者：前端按码分流（apiClient.errorCode）、以及 OpenAPI 的
// errorSchema 把 error_code 列为 required。任何一处 helper 忘了写码，两边同时失真。
func TestFailHelpersAlwaysWriteErrorCode(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name  string
		call  func(c *gin.Context)
		want  errs.Def
		wantM string
	}{
		{
			name:  "FailDef",
			call:  func(c *gin.Context) { FailDef(c, errs.InvalidParam) },
			want:  errs.InvalidParam,
			wantM: errs.InvalidParam.Message,
		},
		{
			name:  "FailDefMsg 改文案但保留码",
			call:  func(c *gin.Context) { FailDefMsg(c, errs.InvalidParam, "标题不能为空") },
			want:  errs.InvalidParam,
			wantM: "标题不能为空",
		},
		{
			name: "FailDefData 带 data 也必须带码",
			call: func(c *gin.Context) {
				FailDefData(c, errs.AgentProbeRequired, gin.H{"targets": []string{"x"}})
			},
			want:  errs.AgentProbeRequired,
			wantM: errs.AgentProbeRequired.Message,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

			tc.call(c)

			if w.Code != tc.want.HTTP {
				t.Fatalf("HTTP 状态 = %d，期望 %d", w.Code, tc.want.HTTP)
			}
			var body Body
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("响应不是合法 JSON: %v (%s)", err, w.Body.String())
			}
			if body.ErrorCode != tc.want.Code {
				t.Fatalf("error_code = %q，期望 %q", body.ErrorCode, tc.want.Code)
			}
			if body.Code != tc.want.HTTP {
				t.Fatalf("body.code = %d，期望与 HTTP 一致（%d）", body.Code, tc.want.HTTP)
			}
			if body.Message != tc.wantM {
				t.Fatalf("message = %q，期望 %q", body.Message, tc.wantM)
			}
		})
	}
}
