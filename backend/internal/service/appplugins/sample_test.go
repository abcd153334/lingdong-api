package appplugins

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestSampleMatchesDoc 示例与文档必须逐字段一致。
//
// 为什么要有这条：示例是给 AI 的 few-shot，文档是给人看的；两份不同步时，
// AI 会照着文档写出示例里没有的字段（或被示例里的旧字段误导），
// 而错误要到"导入被拒"那一步才暴露。
//
// 文档名写死（不用 devdoc.FileName）：devdoc 反过来 import 本包，
// 这里引用它就成环了。改文件名要同时改本行与 devdoc。
func TestSampleMatchesDoc(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "docs", "应用插件开发手册.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读不到插件文档（路径 %s）: %v", path, err)
	}
	block, ok := firstJSONBlock(string(raw))
	if !ok {
		t.Fatal("文档里找不到 json 代码块")
	}
	var fromDoc, fromCode any
	if err := json.Unmarshal([]byte(block), &fromDoc); err != nil {
		t.Fatalf("文档里的示例不是合法 JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(SampleManifest), &fromCode); err != nil {
		t.Fatalf("示例常量不是合法 JSON: %v", err)
	}
	if !reflect.DeepEqual(fromDoc, fromCode) {
		t.Fatal("文档示例与 appplugins.SampleManifest 不一致：改一处必须改另一处")
	}
}

// firstJSONBlock 取第一个 ```json 围栏里的内容。
func firstJSONBlock(s string) (string, bool) {
	const fence = "```json"
	i := strings.Index(s, fence)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(fence):]
	j := strings.Index(rest, "```")
	if j < 0 {
		return "", false
	}
	return strings.TrimSpace(rest[:j]), true
}

// TestSampleIsValid 示例本身必须过自己的校验（否则"填入示例 → 校验"第一步就红）。
func TestSampleIsValid(t *testing.T) {
	m, ps := Decode([]byte(SampleManifest))
	if len(ps) > 0 {
		t.Fatalf("示例解码失败: %+v", ps)
	}
	if problems := m.Validate("ticket"); len(problems) > 0 {
		t.Fatalf("示例校验失败: %+v", problems)
	}
	// 每个声明字段都要在示例里被用到：声明了不用就是伪造能力。
	d := m.Describe()
	for _, e := range d.Entities {
		if len(e.List.Columns) == 0 || len(e.Fields) == 0 {
			t.Fatalf("示例实体 %s 不完整", e.Key)
		}
	}
	if len(d.Config) == 0 {
		t.Fatal("示例应演示插件级配置")
	}
}
