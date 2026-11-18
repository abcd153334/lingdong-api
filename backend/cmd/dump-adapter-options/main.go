// 命令 dump-adapter-options 把「渠道私有配置结构」生成成前端常量。
//
// 为什么要有这个命令：同一份键集在两处手写，删字段时前端不知道 —— 2026-11-04
// 的 fixed_tier 事故就是这么发生的（后端删了、前端还在写、运行期整渠道 503）。
// 生成物 + 门禁（本命令 -check 与 adapters.TestGeneratedOptionsSchemaIsUpToDate）
// 之后，漂移会在提交前暴露，而不是在线上第一个请求。
//
// 用法（在 backend 目录）：
//
//	go run ./cmd/dump-adapter-options            # 写入默认路径
//	go run ./cmd/dump-adapter-options -check     # 只校验生成物是否最新（CI 用）
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"lingdong-api/internal/relay/adapters"
)

const defaultOut = "../web/src/lib/adapterOptions.gen.ts"

func main() {
	out := flag.String("out", defaultOut, "输出文件路径")
	check := flag.Bool("check", false, "只校验生成物是否与后端一致（不一致时退出码 1）")
	flag.Parse()

	content, err := adapters.RenderOptionsSchemaTS()
	if err != nil {
		fmt.Fprintln(os.Stderr, "生成失败:", err)
		os.Exit(1)
	}

	if *check {
		old, err := os.ReadFile(*out)
		if err != nil {
			fmt.Fprintln(os.Stderr, "读取生成物失败:", err)
			os.Exit(1)
		}
		if !bytes.Equal(old, content) {
			fmt.Fprintf(os.Stderr, "生成物已过期: %s\n请运行 go run ./cmd/dump-adapter-options 并提交\n", *out)
			os.Exit(1)
		}
		fmt.Println("生成物是最新的")
		return
	}

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "创建目录失败:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, content, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "写入失败:", err)
		os.Exit(1)
	}
	fmt.Println("已生成", *out)
}
