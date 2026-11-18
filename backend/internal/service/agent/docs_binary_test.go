package agent

import (
	"context"
	"strings"
	"testing"
)

// 本文件覆盖"读不出正文的东西要明说"。
//
// 既然不限制格式，就必须把"这份我读不了"显式化：paste 路径过去不做任何
// 类型判断，粘一份 PDF 二进制会静默落库成乱码，模型读到满屏 %PDF-1.7
// 还以为是"文档没有内容"，然后凭猜设计方案。

func TestIsProbablyBinary(t *testing.T) {
	pdf := "%PDF-1.7\n" + strings.Repeat("\x00\x01\x02\x03", 400)
	if !isProbablyBinary(pdf) {
		t.Fatal("PDF 字节流应判为二进制")
	}
	zip := "PK\x03\x04" + strings.Repeat("\x00\x00\x00\x00", 300)
	if !isProbablyBinary(zip) {
		t.Fatal("ZIP(DOCX/XLSX) 字节流应判为二进制")
	}
	if isProbablyBinary("# 鉴权\nBearer {api_key}\n## 端点\nPOST /v1/chat\n") {
		t.Fatal("Markdown 不该被判为二进制")
	}
	if isProbablyBinary("{\"a\":1,\n\"b\":\t2}") {
		t.Fatal("JSON 不该被判为二进制")
	}
	if isProbablyBinary("model,endpoint,price\nkling,chat,1\n") {
		t.Fatal("CSV 不该被判为二进制")
	}
}

func TestCreateDocRejectsBinary(t *testing.T) {
	withAgentDB(t)
	binary := "%PDF-1.7\n" + strings.Repeat("\x00\x01\x02", 500)
	_, _, err := CreateDoc(context.Background(), 1, DocInput{Title: "手册", Content: binary})
	if err == nil {
		t.Fatal("二进制内容应被拒收")
	}
	if !strings.Contains(err.Error(), "二进制") {
		t.Fatalf("错误应说明是二进制：%v", err)
	}
}

func TestCreateDocAcceptsArbitraryText(t *testing.T) {
	gdb := withAgentDB(t)
	cases := []struct{ name, content string }{
		{"JSON", "{\n  \"endpoints\": [{\"path\": \"/v1/chat\"}]\n}"},
		{"CSV", "model,endpoint,price\nkling,chat,1\n"},
		{"无标题长文", strings.Repeat("这是一段没有任何标题的说明文字。\n", 20)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc, created, err := CreateDoc(context.Background(), 1, DocInput{Content: c.content})
			if err != nil {
				t.Fatalf("文本内容不该被拒：%v", err)
			}
			if !created || doc == nil || doc.ID == 0 {
				t.Fatal("应落库新文档")
			}
			_ = gdb
		})
	}
}
