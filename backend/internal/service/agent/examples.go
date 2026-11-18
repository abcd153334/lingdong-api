package agent

import (
	"embed"
	"fmt"
	"path"
	"sort"
	"strings"
)

// 示例（few-shot）是**编译期**资产，与知识包同源发布。
//
// 为什么要有独立文件：正确 JSON 曾经有三份互相独立的副本（happyPlan / indexttsSpec /
// 知识包里的 markdown 代码块），而且已经漂移——md 用 prompt_text、e2e 用 text。
// 更糟的是 md 里那份"正例"带 `"spec":{...}` 省略号与 "summary"，照抄必然被
// validate_draft 拒（它 additionalProperties:false 且不收 summary）。
//
// 现在只有一份，被三处消费：
//  1. 单测：TestExamplesPassValidateDraft 逐份跑真实校验管线，必须 ok；
//  2. 给模型看：知识包写 `<!--example:名称-->`，ReadSkill 渲染时替换成文件正文；
//  3. e2e：直接读同一份，不再内联常量。
//
//go:embed testdata/examples/*.json
var exampleFS embed.FS

// ExampleNames 返回全部示例名（不含 .json 后缀），按名称升序。
func ExampleNames() []string {
	entries, err := exampleFS.ReadDir("testdata/examples")
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(out)
	return out
}

// Example 按名称读取一份示例（内容是可直接投喂 `validate_draft` 的
// `{"operations":[…]}`，不含 summary、不含占位符）。
func Example(name string) ([]byte, error) {
	raw, err := exampleFS.ReadFile(path.Join("testdata/examples", name+".json"))
	if err != nil {
		return nil, fmt.Errorf("示例 %q 不存在（可选：%s）", name, strings.Join(ExampleNames(), "、"))
	}
	return raw, nil
}

// exampleMarker 是知识包里的示例占位语法。
//
// 选 markdown 注释而不是自定义标签：渲染前后都是合法 markdown，
// 即使渲染失效，模型看到的也只是一句注释而不是半个 JSON。
func exampleMarker(name string) string { return "<!--example:" + name + "-->" }

// renderExamples 把知识包里的 `<!--example:名称-->` 换成代码块。
//
// 渲染发生在**读取时**而不是构建时：知识包正文里没有第二份副本，
// 示例文件一改，读到的就是新的（漂移不可能发生）。
// 未知名称原样保留 —— 由 TestSkillRendersExamples 在测试期挡住。
func renderExamples(body string) string {
	if !strings.Contains(body, "<!--example:") {
		return body
	}
	var b strings.Builder
	rest := body
	for {
		i := strings.Index(rest, "<!--example:")
		if i < 0 {
			b.WriteString(rest)
			break
		}
		j := strings.Index(rest[i:], "-->")
		if j < 0 {
			b.WriteString(rest)
			break
		}
		name := rest[i+len("<!--example:") : i+j]
		b.WriteString(rest[:i])
		if raw, err := Example(strings.TrimSpace(name)); err == nil {
			b.WriteString("```json\n")
			b.WriteString(bytesTrimRight(raw))
			b.WriteString("\n```")
		} else {
			b.WriteString(rest[i : i+j+len("-->")])
		}
		rest = rest[i+j+len("-->"):]
	}
	return b.String()
}

// bytesTrimRight 去掉尾部空白（示例文件以换行结尾，直接拼会多一个空行）。
func bytesTrimRight(raw []byte) string {
	return strings.TrimRight(string(raw), " \t\r\n")
}
