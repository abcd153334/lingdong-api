package errs

// 本文件守的是**注册表与定义表的一致性**。
//
// registry 是"错误码总表"：`Get(code)` 查它，`All()` 导出它。而具体的 Def 是
// 140 个独立变量。两者靠人手同步，而漏登记的代价是静默的 ——
// 编译通过、接口正常工作、日志也看不出问题，只有 `Get(code)` 返回 false，
// 表现为"这个已知错误在中转层被当作陌生命令处理"。
//
// 实测曾漏 16 条（captcha_*、mfa_*、register_closed 等）。没有这条测试时，
// 它们永远不会被人发现：因为这些码的 handler 走的是 `response.FailDef(c, def)`
// （直接用变量），完全不经过 registry。
//
// 为什么读源码而不是反射：Go 无法枚举包内变量。读 `errs.go` 文本、正则提取
// 「变量名 = Def{CodeXxx」与「变量名.Code:」，是唯一能自动发现新增遗漏的办法。
// 代价是这条测试与文件格式耦合 —— 但格式由 gofmt 固定，风险可接受。

import (
	"os"
	"regexp"
	"testing"
)

var (
	// 匹配 `\tName = Def{CodeXxx,` 形式的定义（gofmt 会用空格对齐 `=`）。
	// 必须带 (?m)：`^` 否则只匹配整个文件的开头，一条都抓不到。
	defDeclRe = regexp.MustCompile(`(?m)^\t([A-Za-z0-9_]+)\s*=\s*Def\{(Code[A-Za-z0-9_]+)`)
	// 匹配 registry 里的 `Name.Code:` 键。
	regKeyRe = regexp.MustCompile(`([A-Za-z0-9_]+)\.Code:`)
)

func TestEveryDefIsRegistered(t *testing.T) {
	raw, err := os.ReadFile("errs.go")
	if err != nil {
		t.Fatalf("读 errs.go 失败：%v", err)
	}
	src := string(raw)

	regStart := regexp.MustCompile(`(?s)var registry = map\[string\]Def\{(.*?)\n\}`).FindStringSubmatch(src)
	if regStart == nil {
		t.Fatal("找不到 registry 定义（实现变了？）")
	}
	registered := map[string]bool{}
	for _, m := range regKeyRe.FindAllStringSubmatch(regStart[1], -1) {
		registered[m[1]] = true
	}
	if len(registered) < 100 {
		t.Fatalf("registry 只解析出 %d 条，明显不对", len(registered))
	}

	declared := 0
	for _, m := range defDeclRe.FindAllStringSubmatch(src, -1) {
		name := m[1]
		declared++
		if !registered[name] {
			t.Errorf("Def %s 未登记进 registry：errs.Get(%q) 会返回 false", name, m[2])
		}
	}
	if declared < 100 {
		t.Fatalf("只解析出 %d 个 Def 定义，正则与文件格式已失配", declared)
	}

	// 反向：registry 里不能有指向不存在变量的键（那说明删除 Def 时忘了摘键）。
	// 变量声明集合用同一份正则提取。
	declaredNames := map[string]bool{}
	for _, m := range defDeclRe.FindAllStringSubmatch(src, -1) {
		declaredNames[m[1]] = true
	}
	for name := range registered {
		if !declaredNames[name] {
			t.Errorf("registry 含 %s.Code，但找不到对应的 Def 定义（删变量时忘摘键？）", name)
		}
	}

	// 键与 Def.Code 必须一致：`X = Def{CodeA} ; registry 里写成 {"a": X}` 这种
	// 手写错误会让 Get("a") 返回一个 code 为别的值的 Def。
	if d, ok := Get(InvalidParam.Code); !ok || d.Code != InvalidParam.Code {
		t.Errorf("Get(InvalidParam.Code) = %+v, %v", d, ok)
	}
}
