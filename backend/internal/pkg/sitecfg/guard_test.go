package sitecfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件是「唯一模型」的回归护栏。
//
// 为什么值得一个测试：`site` 是整棵 JSONB blob，任何包都可以自己声明一个
// `struct{ APIBaseURL string `json:"api_base_url"` }` 再 Unmarshal。这种写法
// **编译通过、运行也不报错**——字段改名后它只是静默解析出零值，对外地址随即
// 回落 Request.Host（反代下恒为 127.0.0.1）。靠人 review 抓不住，只能靠扫描。
var bannedSiteTags = []string{
	`json:"api_base_url"`, // 单地址字段：已由 api_base_urls 取代
	`json:"logo_text"`,    // 已删除：降级改用站点名称首字
}

func TestNoForeignSiteStructs(t *testing.T) {
	root := filepath.Join("..", "..", "..") // backend/
	var hits []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// 测试数据与构建产物不参与
			if name := d.Name(); name == "node_modules" || name == ".git" || name == "data" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if filepath.Base(path) == "sitecfg.go" {
			return nil // 模型自己当然可以用这些 key（当前并不用，留出扩展余地）
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		text := string(raw)
		for _, tag := range bannedSiteTags {
			if strings.Contains(text, tag) {
				rel, _ := filepath.Rel(root, path)
				hits = append(hits, rel+" 含 "+tag)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描失败：%v", err)
	}
	if len(hits) > 0 {
		t.Fatalf("site 字段必须只在 pkg/sitecfg 声明，以下文件各自造了第二份模型：\n  %s",
			strings.Join(hits, "\n  "))
	}
}
