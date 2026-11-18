package apicontract_test

// 本文件守的是 `docs/开放API参考.md` 与契约**不漂移**。
//
// 为什么需要它：那份 3000+ 行的参考文档是接入方唯一会逐字读的东西，而它
// 是"手工从契约导出"的产物。2026-11-10 核对时它已经漂了三处：
//   - 缺 8 个错误码（`app_maintenance`、`portal_disabled`、`channel_conflict` 等）；
//   - 留着 1 个**已被删除**的码 `aff_disabled`（接入方照着写分支，永远不命中）；
//   - 当时新增的聚合端点完全没有章节（该端点后来按缓存策略拆成了
//     profile / notices / release 三条，见 docs/开放API参考.md 的「应用」组）。
//
// 漂移没有报错、没有告警、没有页面异常 —— 只有"接入方照着文档写，然后失败"。
//
// 这里不比对全文（那要求把排版的每个空格都复刻进生成器，改一个表格列宽就全红），
// 只比对**会让人照着写出错代码**的那几件事：
//  1. 端点章节是否存在；
//  2. 每个声明的入参与返回字段是否在章节里出现；
//  3. 每个声明的失败码是否在章节里出现；
//  4. 附录错误码表与 `errs` 是否双向一致。

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"lingdong-api/internal/apicontract"
	"lingdong-api/internal/pkg/errs"
)

// referenceDocPath 是文档相对本包测试工作目录的位置
// （`backend/internal/apicontract` → 上三级是仓库根）。
const referenceDocPath = "../../../docs/开放API参考.md"

// loadReferenceDoc 读取参考文档；缺失时**失败而不是 skip**。
//
// 原因与集成测试的 DSN 相反：DSN 缺失时 skip 是合理的（本机没有测试库），
// 而这份文档是本仓库的一部分，"读不到"意味着目录结构变了，那时门禁应当喊出来。
func loadReferenceDoc(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(referenceDocPath)
	if err != nil {
		t.Fatalf("读不到 %s（%v）：这份参考文档必须随仓库一起交付，"+
			"否则接入方拿不到与自己版本匹配的字段说明", referenceDocPath, err)
	}
	return string(raw)
}

// endpointSections 把文档按 `### 标题` 切成章节。
//
// 只切到下一个 `###` 或 `##` 为止：附录里的 `### HTTP 400` 也是 `###`，
// 但那些标题与端点标题不会重名，不影响按标题取章节。
func endpointSections(doc string) map[string]string {
	out := map[string]string{}
	lines := strings.Split(doc, "\n")
	var title string
	var buf []string
	flush := func() {
		if title != "" {
			out[title] = strings.Join(buf, "\n")
		}
		title, buf = "", nil
	}
	for _, ln := range lines {
		switch {
		case strings.HasPrefix(ln, "### "):
			flush()
			title = strings.TrimSpace(strings.TrimPrefix(ln, "### "))
		case strings.HasPrefix(ln, "## "):
			flush()
		default:
			if title != "" {
				buf = append(buf, ln)
			}
		}
	}
	flush()
	return out
}

// TestReferenceDocCoversEveryEndpoint 保证每条端点都在文档里有完整章节。
//
// 不测的后果：新增端点只改契约、不改文档。文档页（运行期生成）是新的，
// 而接入方下载的 Markdown 手册里根本没有这条——两边看起来都"正常"。
func TestReferenceDocCoversEveryEndpoint(t *testing.T) {
	doc := loadReferenceDoc(t)
	sections := endpointSections(doc)

	for _, e := range apicontract.Rules() {
		body, ok := sections[e.Title]
		if !ok {
			t.Errorf("文档缺少章节 `### %s`（%s %s，Key=%s）", e.Title, e.Method, e.Path, e.Key)
			continue
		}
		// 路径与方法必须逐字出现：文档写错一个路径段，接入方拿到的是 404。
		if !strings.Contains(body, "`"+e.Path+"`") {
			t.Errorf("`%s` 章节里没有路径 %s", e.Title, e.Path)
		}
		if !strings.Contains(body, "`"+e.Method+"`") {
			t.Errorf("`%s` 章节里没有方法 %s", e.Title, e.Method)
		}

		d := e.Doc()
		for _, p := range d.Params {
			if !strings.Contains(body, "`"+p.Name+"`") {
				t.Errorf("`%s` 章节缺少入参 `%s`", e.Title, p.Name)
			}
		}
		for _, r := range d.Returns {
			if !strings.Contains(body, "`"+r.Name+"`") {
				t.Errorf("`%s` 章节缺少返回字段 `%s`", e.Title, r.Name)
			}
		}
		for _, ec := range d.Errors {
			if !strings.Contains(body, "`"+ec.Code+"`") {
				t.Errorf("`%s` 章节缺少失败码 `%s`", e.Title, ec.Code)
			}
		}
	}
}

// TestReferenceDocHeaderCounts 保证开头那句"共 N 个端点，分 M 组"是真的。
//
// 不测的后果：接入方用这个数字判断"文档是不是最新的"。数字不涨，他就不重读；
// 数字乱涨（比如手工 +1 加错），他会怀疑整份文档。
func TestReferenceDocHeaderCounts(t *testing.T) {
	doc := loadReferenceDoc(t)
	re := regexp.MustCompile(`共 \*\*(\d+)\*\* 个端点，分 (\d+) 组`)
	m := re.FindStringSubmatch(doc)
	if m == nil {
		t.Fatalf("文档开头找不到「共 **N** 个端点，分 M 组」这一行（格式变了？）")
	}
	wantN := len(apicontract.Rules())
	wantM := len(apicontract.Groups())
	if m[1] != itoa(wantN) {
		t.Errorf("文档声明 %s 个端点，契约实际 %d 个", m[1], wantN)
	}
	if m[2] != itoa(wantM) {
		t.Errorf("文档声明 %s 组，契约实际 %d 组", m[2], wantM)
	}
}

// itoa 只处理非负小数，避免为一个断言引入 strconv。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestReferenceDocErrorCodesMatchErrs 保证附录错误码表与 errs 双向一致。
//
// 两个方向各自对应一类真实事故：
//   - errs 有、文档无 -> 接入方拿到一个没见过的 error_code，只能落到 default 分支；
//   - 文档有、errs 无 -> 接入方为它写了一个永远不命中的分支（`aff_disabled` 就是这样）。
func TestReferenceDocErrorCodesMatchErrs(t *testing.T) {
	doc := loadReferenceDoc(t)
	idx := strings.Index(doc, "## 附录：错误码总表")
	if idx < 0 {
		t.Fatalf("文档没有「附录：错误码总表」章节")
	}
	appendix := doc[idx:]

	rowRe := regexp.MustCompile("(?m)^\\| `([a-z0-9_]+)` \\|")
	inDoc := map[string]bool{}
	for _, m := range rowRe.FindAllStringSubmatch(appendix, -1) {
		inDoc[m[1]] = true
	}
	inCode := map[string]bool{}
	for _, d := range errs.All() {
		inCode[d.Code] = true
	}

	var missing, stale []string
	for c := range inCode {
		if !inDoc[c] {
			missing = append(missing, c)
		}
	}
	for c := range inDoc {
		if !inCode[c] {
			stale = append(stale, c)
		}
	}
	if len(missing) > 0 {
		t.Errorf("errs 里有 %d 个错误码没写进附录（接入方会落到 default 分支）: %v",
			len(missing), sortStrings(missing))
	}
	if len(stale) > 0 {
		t.Errorf("附录里有 %d 个错误码在 errs 里不存在（接入方为它写永不命中的分支）: %v",
			len(stale), sortStrings(stale))
	}
}

// sortStrings 是插入排序：这里的规模只有个位数，不值得 import sort 只为一条报错好看。
func sortStrings(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
