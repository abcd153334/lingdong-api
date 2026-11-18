package docdocs

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestProblemCatalogCoversAllCodes 守「每个体检码都能被解释」。
//
// 判据是**源码扫描**而不是人工清单：新增一条 Problem 却忘了登记目录，
// 管理台只会显示一个 E_XXX 干码，运营看不懂，AI 也修不对。
func TestProblemCatalogCoversAllCodes(t *testing.T) {
	// 只看定义 Problem 的文件：别处的 E_ 前缀字符串不一定是体检码。
	// 新增一个产出 Problem 的文件（如 v7_guard.go 的用词守卫）必须加进来，
	// 否则它产出的码会绕过"每个码都有解释"这条门禁。
	files := []string{"v7_check.go", "v7_write.go", "v7_guard.go"}
	re := regexp.MustCompile(`Code: +"(E_[A-Z_]+)"`)
	used := map[string]bool{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读不到 %s: %v", f, err)
		}
		for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
			used[m[1]] = true
		}
	}
	have := map[string]bool{}
	for _, p := range problemCatalog {
		if p.Meaning == "" || p.Fix == "" {
			t.Errorf("%s 缺说明或改法（目录项必须能直接照着改）", p.Code)
		}
		have[p.Code] = true
	}
	var missing, stale []string
	for c := range used {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	for c := range have {
		if !used[c] {
			stale = append(stale, c)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("这些体检码没有目录项（运营/AI 只能看到一个干码）: %s", strings.Join(missing, ", "))
	}
	if len(stale) > 0 {
		t.Errorf("目录里这些码已经没有实现（会误导人去改不存在的问题）: %s", strings.Join(stale, ", "))
	}
}
