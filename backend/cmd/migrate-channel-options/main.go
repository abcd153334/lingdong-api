// 命令 migrate-channel-options 清理渠道私有配置里的已删除字段。
//
// 它之所以是独立命令而不是一次性的裸 SQL：
//   - 清理后必须用**运行期同一个 parser** 复验（只删键无法证明"改完能跑"）；
//   - 需要先建备份表，且备份失败要中止；
//   - 需要可重复执行（幂等），供部署脚本与本地环境共用。
//
// 用法：
//
//	go run ./cmd/migrate-channel-options -dry-run   # 只看会改什么
//	go run ./cmd/migrate-channel-options            # 真正执行
package main

import (
	"flag"
	"fmt"
	"os"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	_ "lingdong-api/internal/relay/adapters" // 注册内置适配器：校验要用运行期的 parser
	channelsvc "lingdong-api/internal/service/channel"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "只统计将清理的渠道，不写库")
	flag.Parse()

	env := conf.Load()
	gdb, err := db.Init(env.DatabaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "连接数据库失败:", err)
		os.Exit(1)
	}

	rep, err := channelsvc.MigrateChannelOptions(gdb, *dryRun)
	if err != nil {
		fmt.Fprintln(os.Stderr, "清理失败:", err)
		os.Exit(1)
	}
	channelsvc.LogChannelOptions(rep)

	mode := "已执行"
	if *dryRun {
		mode = "dry-run"
	}
	fmt.Printf("%s：备份=%v 扫描=%d 清理=%d 失败=%v\n",
		mode, rep.BackedUp, rep.Scanned, rep.Cleaned, rep.Failed)
	if len(rep.Failed) > 0 {
		// 非零退出让 CI/部署脚本能感知"仍需人工处理"，而不是把它当成成功。
		os.Exit(2)
	}
}
