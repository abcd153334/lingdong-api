package db

import (
	"fmt"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/pkg/logger"
)

// MigrateDocsV8DropNodeModels 删除 `doc_node_models`（节点 ↔ 模型绑定表）。
//
// 为什么可以硬删：这张表存的是「这篇文档挂哪些模型」。族文档改成静态白名单后，
// 族归属由 `apidoc.FamilyDocOfModelID`（模型 ID 子串匹配）在读取时现算，
// 协议篇的模型清单由 `apidoc.AppliesDoc` 现算 —— 表里没有任何一列还是真相源。
//
// 留着它的代价不是"多一张表"：`uq_doc_node_models_family` 会继续要求
// 「一个模型只属于一个族」，而这条约束已经没人维护，随时会挡住正常的族调整。
//
// 与 M46 同款：靠 `DROP TABLE IF EXISTS` 自身幂等，不落 data_migrations 标记
// （标记会多出一个失败态：标记写进去了、删表失败 → 永远不再删）。
// 先看表在不在，只为了让日志能说清这次到底动了没有。
func MigrateDocsV8DropNodeModels(gdb *gorm.DB) (bool, error) {
	var exists bool
	if err := gdb.Raw(`SELECT to_regclass('doc_node_models') IS NOT NULL`).Scan(&exists).Error; err != nil {
		return false, fmt.Errorf("db: 检查 doc_node_models 是否存在失败: %w", err)
	}
	if !exists {
		return false, nil
	}
	// 不加 CASCADE：若真有别的对象依赖它，宁可让迁移失败并被人看见。
	if err := gdb.Exec(`DROP TABLE IF EXISTS doc_node_models`).Error; err != nil {
		return false, fmt.Errorf("db: 删除 doc_node_models 失败: %w", err)
	}
	return true, nil
}

// LogDocsV8DropNodeModels 打一行说明（删表只跑一次，出事后的唯一线索就是它）。
func LogDocsV8DropNodeModels(applied bool) {
	if !applied {
		return
	}
	logger.L().Info("已删除 doc_node_models（适用模型改为由 FamilyDoc.Match 在读取时现算）",
		zap.String("reason", "族文档已改为静态白名单，成员表不再有写入方"))
}
