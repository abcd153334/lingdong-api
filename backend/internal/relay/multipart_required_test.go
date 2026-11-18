package relay

import (
	"errors"
	"strings"
	"testing"

	"lingdong-api/internal/pkg/errs"
)

// 本文件覆盖 P7：multipart 端点收到非 multipart 请求体时，错误必须可归因。
//
// 修复前 parseMultipartHead 的 boundary 错误与 JSON 解析错误一起被吞成通用
// invalid_param，接入方按 OpenAI SDK 习惯把图片编辑/语音转写打成 JSON 时，
// 只能反复检查字段而得不到「改成 multipart 表单」这个真正的处置动作。

func TestParseMultipartHeadRejectsNonMultipartAsSentinel(t *testing.T) {
	for _, ctype := range []string{
		"application/json",
		"",
		"multipart/form-data", // 无 boundary 参数
		"multipart/form-data; charset=utf-8",
	} {
		_, err := parseMultipartHead(ctype, []byte(`{"model":"m"}`))
		if !errors.Is(err, errNotMultipart) {
			t.Errorf("ctype=%q 应返回 errNotMultipart 哨兵，实际 %v", ctype, err)
		}
	}
}

func TestParseMultipartHeadWellFormedNoError(t *testing.T) {
	// 合法 multipart（无 model 字段）：不报错，Model 留空由 dispatch 判 invalid_param。
	body := "--BND\r\nContent-Disposition: form-data; name=\"size\"\r\n\r\n1024x1024\r\n--BND--\r\n"
	head, err := parseMultipartHead("multipart/form-data; boundary=BND", []byte(body))
	if err != nil {
		t.Fatalf("合法 multipart 不应报错：%v", err)
	}
	if head.Model != "" {
		t.Errorf("无 model part 时 Model 应为空，实际 %q", head.Model)
	}
	if head.Size != "1024x1024" {
		t.Errorf("size part 未提取：%q", head.Size)
	}
}

func TestMultipartRequiredDefShape(t *testing.T) {
	// registry 门禁（TestEveryDefIsRegistered）守登记，这里守语义：400 + 可归因文案。
	if errs.MultipartRequired.HTTP != 400 {
		t.Errorf("MultipartRequired 应为 400，实际 %d", errs.MultipartRequired.HTTP)
	}
	if !strings.Contains(errs.MultipartRequired.Message, "multipart/form-data") {
		t.Errorf("文案必须指出正确请求形态：%q", errs.MultipartRequired.Message)
	}
	if got, ok := errs.Get(errs.CodeMultipartRequired); !ok || got.Code != errs.CodeMultipartRequired {
		t.Fatalf("registry 未登记 multipart_required，Get 查不到（ok=%v got=%v）", ok, got)
	}
}
