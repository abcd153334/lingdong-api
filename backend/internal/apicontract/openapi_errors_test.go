package apicontract

import (
	"strings"
	"testing"

	"lingdong-api/internal/pkg/errs"
)

// TestResponsesRenderDeclaredErrors 守住「契约里声明了错误码，OpenAPI 里就得看得到」。
//
// 背景：全部端点一共声明了一百多条 ErrCode，但 responsesOf 过去只吐一个泛化的
// `4XX`，声明的内容只进了管理台目录 JSON 与测试，接入方拿到的 openapi.json 里
// 一条具体状态码都没有。SDK 生成器因此只能生成一个 `else` 分支。
func TestResponsesRenderDeclaredErrors(t *testing.T) {
	doc := OpenAPI("https://api.example.com")

	checked := 0
	for _, e := range catalogOrdered() {
		d := e.Doc()
		if len(d.Errors) == 0 {
			continue
		}
		item, ok := doc.Paths[toOpenAPIPath(e.Path)]
		if !ok {
			t.Fatalf("端点 %s 的路径 %s 不在 OpenAPI 里", e.Key, e.Path)
		}
		op, ok := item[strings.ToLower(e.Method)]
		if !ok {
			t.Fatalf("端点 %s 的 %s 未渲染", e.Key, e.Method)
		}

		for _, ec := range d.Errors {
			def, ok := errs.Get(ec.Code)
			if !ok {
				t.Fatalf("端点 %s 声明了不存在的错误码 %q", e.Key, ec.Code)
			}
			st := itoaStatus(def.HTTP)
			resp, ok := op.Responses[st]
			if !ok {
				t.Fatalf("端点 %s 声明了 %q（HTTP %d），但 OpenAPI 没有 %s 响应",
					e.Key, ec.Code, def.HTTP, st)
			}
			if !strings.Contains(resp.Description, ec.Code) {
				t.Fatalf("端点 %s 的 %s 响应描述里没有 %q：%s",
					e.Key, st, ec.Code, resp.Description)
			}
			// 声明了具体状态码的同时，4XX 通配必须保留（其它未声明码仍可能出现）。
			if _, ok := op.Responses["4XX"]; !ok {
				t.Fatalf("端点 %s 丢了 4XX 通配响应", e.Key)
			}
			checked++
		}
	}
	if checked < 100 {
		t.Fatalf("只校验到 %d 条声明错误码，明显少于契约规模（148 左右）", checked)
	}
}

func itoaStatus(v int) string {
	if v == 0 {
		return "0"
	}
	var b [4]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
