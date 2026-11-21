// gen-plugin-doc 把 devdoc.Markdown() 落成 docs/应用插件开发手册.md。
//
// 为什么要生成而不是手写：类型矩阵、上限、动作词表、错误码全在代码里，
// 手写副本必然漂（这份能力已经漂过一次）。这里的产物供人直接阅读与 diff 审阅，
// 门禁跑 `-check`：不一致就以非零退出。
//
//	go run ./cmd/gen-plugin-doc          # 重新生成
//	go run ./cmd/gen-plugin-doc -check   # 只校验（CI/提交前）
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"lingdong-api/internal/service/appplugins/devdoc"
)

func main() {
	out := flag.String("out", filepath.Join("..", "docs", devdoc.FileName), "输出路径")
	check := flag.Bool("check", false, "只校验，不写文件")
	flag.Parse()

	want := devdoc.Markdown()
	got, err := os.ReadFile(*out)
	if err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "读取 %s 失败: %v\n", *out, err)
		os.Exit(2)
	}

	if *check {
		if string(got) != want {
			fmt.Fprintf(os.Stderr, "%s 与代码不一致，请运行 `go run ./cmd/gen-plugin-doc` 重新生成。\n", *out)
			os.Exit(1)
		}
		fmt.Printf("%s 已与代码一致（%d 字节）\n", *out, len(want))
		return
	}

	if err := os.WriteFile(*out, []byte(want), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "写入 %s 失败: %v\n", *out, err)
		os.Exit(2)
	}
	fmt.Printf("已写入 %s（%d 字节）\n", *out, len(want))
}
