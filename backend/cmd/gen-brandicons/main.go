// Command gen-brandicons 把素材目录里的 png 复制成内置图标的 ASCII slug 文件名。
//
// 为什么要留一个生成器而不是手工复制：
//   - key 的推导规则（全小写 + 特例表）必须可复现，否则下次补图会出现
//     `AlibabaQwen.png` / `alibaba-qwen.png` 两种写法并存；
//   - catalog.go 是手写的展示名与中文别名（不能自动生成），因此本工具只负责
//     "文件落地"，并在结束时打印 key 清单供人工对照 catalog。
//
// 用法（在 backend/ 目录执行，与其它 cmd 工具一致）：
//
//	go run ./cmd/gen-brandicons
//
// 源目录是仓库根的 `API文档/ai-logo`，只在开发机存在、不参与编译；
// 真正被打包的是产物 internal/assets/brandicon/icons/*.png（go:embed）。
// `go:embed` 不允许引用包目录之外的路径，所以必须先复制进来。
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// srcDir 相对 backend/（本命令的约定工作目录）。
	srcDir = "../API文档/ai-logo"
	// dstDir 必须与 go:embed 的路径一致。
	dstDir = "internal/assets/brandicon/icons"
)

// rename 是 filename → key 的**唯一**特例表。
//
// 只允许"中文名拉丁化"这一类：key 会直接进 URL 路径，中文会导致
// percent-encoding 在 nginx / 浏览器 / ETag 多处不一致。
var rename = map[string]string{
	"火山引擎": "volcengine",
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "失败：", err)
		os.Exit(1)
	}
}

func run() error {
	if _, err := os.Stat(srcDir); err != nil {
		return fmt.Errorf("找不到素材目录 %s（请在 backend/ 目录下执行）：%w", srcDir, err)
	}
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return fmt.Errorf("读取素材目录 %s: %w", srcDir, err)
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}

	var keys []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".png") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		key, ok := rename[base]
		if !ok {
			key = strings.ToLower(base)
		}
		if !validKey(key) {
			return fmt.Errorf("%s → key %q 不合规（只允许小写字母/数字/连字符）", e.Name(), key)
		}
		if err := copyFile(filepath.Join(srcDir, e.Name()), filepath.Join(dstDir, key+".png")); err != nil {
			return err
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return fmt.Errorf("%s 里没有 png", srcDir)
	}
	sort.Strings(keys)
	fmt.Printf("已复制 %d 个图标到 %s\n", len(keys), dstDir)
	fmt.Println("请确认 catalog.go 覆盖了以下全部 key（多一个少一个 embed_test 都会红）：")
	for _, k := range keys {
		fmt.Println(" ", k)
	}
	return nil
}

// validKey 与 brandicon.ValidBuiltinKey 同规则。
//
// 这里刻意不 import 那个包：生成器要在 catalog 出错时仍能跑。
func validKey(k string) bool {
	if k == "" || len(k) > 64 {
		return false
	}
	for i, r := range k {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-' && i > 0:
		default:
			return false
		}
	}
	return true
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
