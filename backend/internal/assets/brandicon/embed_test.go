package brandicon

import (
	"bytes"
	"testing"
)

// 本文件守一条**只在线上才暴露**的缺陷：go:embed 少提交一个文件，编译照样通过。
//
// 症状是某个图标 404 → 前端静默退回首字占位 → 页面看起来"正常"，只是图标不对。
// 所以文件集与 catalog 必须双向对齐，两个方向都要断言。
func TestEmbedMatchesCatalog(t *testing.T) {
	if err := InitError(); err != nil {
		t.Fatalf("内嵌资源与 catalog 不一致：%v", err)
	}

	// assets 里到底有几个 png —— 直接用目录遍历，不看 catalog。
	entries, err := assets.ReadDir("icons")
	if err != nil {
		t.Fatalf("读取 embed 目录失败：%v", err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) < 5 || name[len(name)-4:] != ".png" {
			t.Errorf("icons/ 下出现非 png 文件：%s（图标只收 png，避免 svg 可执行）", name)
			continue
		}
		files = append(files, name[:len(name)-4])
	}

	if len(files) == 0 {
		t.Fatal("icons/ 为空：go:embed 的目录写错了？")
	}

	have := make(map[string]bool, len(files))
	for _, k := range files {
		have[k] = true
	}
	for _, it := range catalog {
		if !have[it.Key] {
			t.Errorf("catalog 有 %q，但 icons/%s.png 不存在（漏提交？）", it.Key, it.Key)
		}
		delete(have, it.Key)
	}
	for k := range have {
		t.Errorf("icons/%s.png 存在，但 catalog 没有登记（会永远选不到，且白占体积）", k)
	}

	if got, want := len(catalog), len(files); got != want {
		t.Fatalf("catalog %d 条、文件 %d 个，必须一一对应", got, want)
	}
}

// TestAssetsArePNG 断言每个图标都以 PNG 签名开头。
//
// 用 file 改名冒充 png 的情况不该发生，但一旦发生，浏览器会把它当坏图，
// 而 `go:embed` 只检查文件存在 —— 只能靠这里。
func TestAssetsArePNG(t *testing.T) {
	sig := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	for _, it := range catalog {
		raw, etag, ok := Asset(it.Key)
		if !ok {
			t.Errorf("%s: Asset 未命中", it.Key)
			continue
		}
		if len(raw) < len(sig) || !bytes.Equal(raw[:len(sig)], sig) {
			t.Errorf("%s: 不是 PNG 文件", it.Key)
		}
		if etag == "" {
			t.Errorf("%s: ETag 为空（缓存会退化成每次 200 全量）", it.Key)
		}
	}
}

// TestCatalogShape 守 catalog 的结构性约定（key 合法、名称非空、分组在白名单内）。
func TestCatalogShape(t *testing.T) {
	allowed := map[Group]bool{GroupLLM: true, GroupMedia: true, GroupPlatform: true}
	seen := map[string]bool{}
	for _, it := range catalog {
		if seen[it.Key] {
			t.Errorf("重复 key：%s", it.Key)
		}
		seen[it.Key] = true
		if !ValidBuiltinKey(it.Key) {
			t.Errorf("key 不合规（只允许小写字母/数字/连字符）：%q", it.Key)
		}
		if it.Name == "" {
			t.Errorf("%s: Name 不能为空（选择器里会显示空白）", it.Key)
		}
		if !allowed[it.Group] {
			t.Errorf("%s: 未知分组 %q", it.Key, it.Group)
		}
	}
}

// TestListIsCopy 断言 List 返回副本：调用方会 append，共享底层数组会串数据。
func TestListIsCopy(t *testing.T) {
	a := List()
	if len(a) != len(catalog) {
		t.Fatalf("List 长度 %d，catalog %d", len(a), len(catalog))
	}
	a[0].Name = "被改坏了"
	if List()[0].Name == "被改坏了" {
		t.Fatal("List 返回的是内部切片，调用方修改会污染全局")
	}
}
