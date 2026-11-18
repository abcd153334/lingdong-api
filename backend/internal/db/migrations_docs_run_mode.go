package db

import (
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
)

// M51：把「被一句过期理由判死」的那批接口恢复成可在线调试。
//
// 背景：种子里的 `run_hint` 写着「右栏没有路径参数输入框」，但调试面板
// （`web/src/docs/TryPanel.tsx`）早就有路径参数输入区了。这句过期的说明让
// 15 篇接口在文档站上一律显示"该接口不适合在线调试"——其中 7 篇是
// 「查询任务状态」，于是"提交任务 → 查状态"这条链路断在第二步。
//
// 分批处理（判据只看"浏览器能不能构造这个请求"）：
//   - 响应是二进制的（`Transport.Binary`）→ `binary`，面板按 blob 预览/下载；
//   - 其余 GET/DELETE + 路径参数 → `json`；
//   - `files/direct` 仍 `none`：直链挂在**文件域**、不在 API 域，浏览器发不出去。
//     这是唯一真实原因，文案随之改准。
//
// 被放开的行必须同时清空 `run_hint`：这一列会在接口正文、Markdown 导出、
// AI 工具面三处展示，留着就等于继续对读者说"这个接口不能调"。
//
// 幂等：`data_migrations` 标记；且 UPDATE 带 `run_mode = 'none'`，
// 运营在后台手工改过的行不会被覆盖。失败阻断启动（与 M49/M50 一致）。
const migrationDocsRunMode = "m51_docs_run_mode"

// binaryRunModes 是响应为二进制的端点（`Transport.Binary = true`）。
// 契约层有一致性测试守着 `Binary ⇒ RunMode ∈ {binary, none}`，
// 这三篇标成 json 会让 `go test ./internal/apidoc` 直接失败。
const binaryRunModes = `'files/content','tasks/content-at','media/proxy'`

// MigrateDocsRunMode 只执行一次（`data_migrations` 标记），返回是否真的执行过。
func MigrateDocsRunMode(gdb *gorm.DB) (bool, error) {
	if gdb.Migrator().HasTable(&model.DataMigration{}) {
		var n int64
		if err := gdb.Model(&model.DataMigration{}).
			Where("name = ?", migrationDocsRunMode).Count(&n).Error; err != nil {
			return false, fmt.Errorf("db: 查询调试形态迁移标记失败: %w", err)
		}
		if n > 0 {
			return false, nil
		}
	}

	// 一条 UPDATE 同时处理三件事（拆成三条就会漏一个分支）：
	//   1. binary 端点 → binary；
	//   2. files/direct → 保持 none，只换文案（它真的发不出去，理由要写对）；
	//   3. 其余命中行 → json，并清空那句过期的 run_hint。
	const q = `
		UPDATE doc_endpoints
		   SET run_mode = CASE
		         WHEN transport_doc_id IN (` + binaryRunModes + `) THEN 'binary'
		         WHEN transport_doc_id = 'files/direct' THEN 'none'
		         ELSE 'json'
		       END,
		       run_hint = CASE
		         WHEN transport_doc_id = 'files/direct'
		           THEN '文件直链由上传接口返回，请直接打开响应里的 url。'
		         ELSE ''
		       END
		 WHERE transport_doc_id IN (
		         'files/get','files/delete','files/content','files/direct',
		         'tasks/get','tasks/cancel','tasks/content-at','models/get','media/proxy'
		       )
		   AND run_mode = 'none'`

	err := gdb.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(q).Error; err != nil {
			return fmt.Errorf("更新接口调试形态: %w", err)
		}
		return tx.Create(&model.DataMigration{
			Name: migrationDocsRunMode, AppliedAt: time.Now(),
		}).Error
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// LogDocsRunMode 只在真的执行过时打一行（正常重启不刷日志）。
func LogDocsRunMode(applied bool) {
	if !applied {
		return
	}
	logger.L().Warn("接口调试形态已按「浏览器能否构造请求」重算（M51）",
		zap.String("at", time.Now().Format(time.RFC3339)))
}
