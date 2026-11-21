package devdoc

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/service/appplugins"
)

// TestMarkdownCoversEveryCapability 文档必须覆盖代码里的**每一个**能力项。
//
// 生成的表格不会漏（它就是遍历常量画出来的），会漏的是"新增了一类能力，
// 但正文里没提"。这里把词表逐项钉住：加类型/加动作/加错误码都必须同步正文。
func TestMarkdownCoversEveryCapability(t *testing.T) {
	doc := Markdown()

	headings := []string{
		"# 应用插件开发手册",
		"## 1. 这是什么",
		"## 2. 五分钟上手",
		"## 3. manifest 全字段",
		"## 4. 字段类型矩阵",
		"## 5. 列表：搜索、筛选、排序、分页",
		"## 6. 表单布局",
		"## 7. 动作与权限",
		"## 8. 管理端接口",
		"## 9. 对外只读面",
		"## 10. 升级与破坏性变更",
		"## 11. 错误码",
		"## 12. 上限",
		"## 13. 安全边界",
		"## 14. 给 AI 的提示词模板",
		"## 15. 完整示例",
	}
	for _, h := range headings {
		if !strings.Contains(doc, h) {
			t.Errorf("文档缺少章节：%s", h)
		}
	}

	// 每个类型都要在表格里出现（用带反引号的写法，免得 "int" 命中普通英文单词）。
	for _, s := range appplugins.TypeSpecs {
		if !strings.Contains(doc, "`"+s.Type+"`") {
			t.Errorf("类型 %q 未出现在文档里", s.Type)
		}
	}
	// 动作词表同理：加一个动作必须在"动作与权限"表里能看到。
	for _, a := range []string{
		appplugins.ActionCreate, appplugins.ActionUpdate, appplugins.ActionDelete,
		appplugins.ActionExport, appplugins.ActionBatchDelete,
	} {
		if !strings.Contains(doc, "`"+a+"`") {
			t.Errorf("动作 %q 未出现在文档里", a)
		}
	}
	// 对外错误码：契约的一部分，下载到的手册里必须有码表。
	for _, d := range []errs.Def{
		errs.PluginNotFound, errs.PluginManifestInvalid, errs.PluginAlreadyInstalled,
		errs.PluginNotInstalled, errs.PluginDisabled, errs.PluginEntityNotFound,
		errs.PluginRecordNotFound, errs.PluginRecordConflict, errs.PluginVersionConflict,
		errs.PluginLimitExceeded, errs.PluginInUse,
	} {
		if !strings.Contains(doc, "`"+d.Code+"`") {
			t.Errorf("错误码 %q 未出现在文档里", d.Code)
		}
	}
	// 记录接口的路由必须逐条列出（少了哪条，接入方就只能猜）。
	for _, p := range []string{
		"/records", "/records/export", "/records/batch-delete", "/p/:plugin_key/:entity",
	} {
		if !strings.Contains(doc, p) {
			t.Errorf("接口路径 %q 未出现在文档里", p)
		}
	}
}

// TestGoldenDocInRepo docs/ 下的副本必须与代码逐字节一致。
//
// 与 `go run ./cmd/gen-plugin-doc -check` 等价，但把它变成 `go test` 的一部分：
// 门禁里少跑一条命令，"文档过期"就不会被漏掉。
func TestGoldenDocInRepo(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "..", "docs", FileName)
	got, err := os.ReadFile(path)
	if err != nil {
		// 与 TestSampleMatchesDoc 同一口径：docs/ 按公开仓白名单不入库，
		// clone 出来必然没有它 → skip；目录在而文件不在 → 仍然是失败。
		if errors.Is(err, fs.ErrNotExist) {
			if _, dirErr := os.Stat(filepath.Dir(path)); errors.Is(dirErr, fs.ErrNotExist) {
				t.Skipf("跳过：%s 不存在（公开仓不含 docs/，仅保留文档的检出会校验）", filepath.Dir(path))
			}
		}
		t.Fatalf("读取 %s 失败（应先运行生成器）: %v", path, err)
	}
	if string(got) != Markdown() {
		t.Fatalf("%s 与代码不一致；重新生成：cd backend && go run ./cmd/gen-plugin-doc", path)
	}
}
