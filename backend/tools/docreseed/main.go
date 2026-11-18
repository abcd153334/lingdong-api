// 一次性维护工具：把当前契约重播进现有库（不重启进程）。
//
// 为什么需要它：迁移与播种都在**启动时**执行，而线上实例是常驻进程。
// 没有这个工具就只能停机重启才能让内容生效；而重启常驻服务必须由人工在
// 普通终端里做，数据迁移本身没有这个限制（它只写库，不改进程内存）。
//
// 用法（在 backend 目录）：
//
//	go run ./tools/docreseed "postgres://user:pass@host:5432/db?sslmode=disable"
//
// 它按**启动路径的同一顺序**做三件事：
//  1. `db.AutoMigrate`（建表/加列/建索引 —— 迁移可能依赖新列，必须先跑）；
//  2. `db.RunMigrations`（M50…M53，带 data_migrations 标记，跑过的自动跳过）；
//  3. `docdocs.SeedDocs()`（只补缺页；M53 已把旧的种子接口页删掉，这里重建）。
//
// 幂等：迁移有标记、播种只补缺。重复执行不会重复插入。
package main

import (
	"fmt"
	"os"

	"lingdong-api/internal/db"
	"lingdong-api/internal/service/docdocs"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "用法: go run ./tools/docreseed <DATABASE_URL>")
		os.Exit(2)
	}
	gdb, err := db.Init(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "连接数据库失败:", err)
		os.Exit(1)
	}
	// 迁移可能引用新列（如 tasks.public_id），AutoMigrate 必须在前。
	if err := db.AutoMigrate(gdb); err != nil {
		fmt.Fprintln(os.Stderr, "AutoMigrate 失败:", err)
		os.Exit(1)
	}
	if err := db.RunMigrations(gdb); err != nil {
		fmt.Fprintln(os.Stderr, "迁移失败:", err)
		os.Exit(1)
	}
	rep, err := docdocs.SeedDocs()
	if err != nil {
		fmt.Fprintln(os.Stderr, "播种失败:", err)
		os.Exit(1)
	}
	fmt.Printf("播种：板块 %d、节点 %d、接口 %d、字段 %d、跳过 %d\n",
		rep.Sections, rep.Nodes, rep.Endpoints, rep.Fields, rep.Skipped)
	if len(rep.Damaged) > 0 {
		fmt.Printf("有 %d 篇未通过体检（保持草稿）：%v\n", len(rep.Damaged), rep.Damaged)
	}
	blocking := 0
	for _, p := range docdocs.CheckAll() {
		if p.Blocking {
			blocking++
			fmt.Printf("阻断：%s %s %s\n", p.Code, p.Field, p.Message)
		}
	}
	if blocking > 0 {
		fmt.Fprintf(os.Stderr, "体检有 %d 条阻断项\n", blocking)
		os.Exit(1)
	}
	fmt.Println("文档体检通过")
}
