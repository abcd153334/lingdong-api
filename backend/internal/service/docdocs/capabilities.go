// 本文件负责把 `models.capabilities` 补齐成可用的事实。
//
// 为什么不放在 db 包做（那里的 M 系列迁移才是「改数据」的常规位置）：
// `db → billing → db` 是**已存在的循环**（billing 反向依赖 db 拿配置），
// 而能力推荐必须复用 `billing.SpecFacts`（参考素材计价位）与 `apidoc` 词表。
// 放在 service 层既能 import 两者，又不制造新循环。

package docdocs

import (
	"encoding/json"

	"go.uber.org/zap"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
)

// EnsureCapabilities 给 `models.capabilities IS NULL` 的行按事实补一份**推荐值**。
//
// 三个设计决定（改之前先读）：
//
//  1. **收敛式补齐，不落 data_migrations 标记**。
//     能力缺失不是「一次性历史问题」——运营在管理台新建模型时漏点一次，
//     这个模型就会在族页里少一个能力点，而症状只是「少了一个」，没人会去查库。
//     每次启动补齐 NULL 行，新模型第二天自然是好的；`[]` 与已有值一律不动，
//     所以人工声明永远优先（唯一真相源仍是这一列）。
//
//  2. **只用显式声明的端点推导**（`apidoc.DeclaredEndpoints`）。
//     图像模态的默认端点包含 `images/edits`，拿兜底值推导会把每个图像模型
//     都标成「支持图生图」——用户照文档改图然后 400，比少一个 TAB 糟得多。
//
//  3. **失败不阻断启动**（只记 ERROR）。文档站是次要路径；能力为空只影响
//     族页的能力描述，让整个 API 起不来的代价大得多。
//
// 返回补齐的行数（仅用于启动日志）。
func EnsureCapabilities() (int, error) {
	var rows []model.Model
	// 只取需要的列：models 有二十多个计费比例字段，全查回来只为算能力没有意义。
	// 但 `billing_mode` **必须**在列里：`apidoc.FactsOf` 要靠它决定 spec_prices
	// 是否生效（非 spec 模式的历史残留价目不能参与推导，否则会给「按次」模型
	// 推出一堆按张/按秒才有的能力）。
	if err := db.DB.
		Select("id", "modality", "billing_mode", "endpoints", "spec_prices", "capabilities").
		Where("capabilities IS NULL").
		Find(&rows).Error; err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}

	var fixed, failed int
	// filled 记下「谁被自动填了」，只为日志：这是**猜**出来的值，运营需要看得到
	// 哪些模型的能力还没被人工确认过（猜错会让文档多出一个不该有的预设，或
	// 把只会改图的模型标成「图像生成」）。
	filled := make([]string, 0, len(rows))
	for i := range rows {
		m := &rows[i]
		declared := apidoc.DeclaredEndpoints(json.RawMessage(m.Endpoints))
		// 用 FactsOf 而不是自己解析：spec_prices 的唯一解析点是 billing.SpecFacts，
		// 这里再 unmarshal 一次就会出现「推荐按 A 口径、计费按 B 口径」。
		facts := apidoc.FactsOf(m)
		caps := apidoc.SuggestCapabilities(facts.Modality, declared, facts.Spec)
		raw, err := json.Marshal(apidoc.CapKeys(caps))
		if err != nil {
			failed++
			logger.L().Warn("序列化模型能力失败", zap.String("model", m.ID), zap.Error(err))
			continue
		}
		// CAS：`capabilities IS NULL` 必须在 WHERE 里。
		// 少了它，两个副本同时启动时后者会覆盖前者——而前者可能正是运营刚保存的值。
		res := db.DB.Model(&model.Model{}).
			Where("id = ? AND capabilities IS NULL", m.ID).
			Update("capabilities", model.JSON(raw))
		if res.Error != nil {
			failed++
			logger.L().Warn("补齐模型能力失败", zap.String("model", m.ID), zap.Error(res.Error))
			continue
		}
		fixed += int(res.RowsAffected)
		if res.RowsAffected > 0 {
			filled = append(filled, m.ID+"="+string(raw))
		}
	}

	if fixed > 0 || failed > 0 {
		logger.L().Info("模型能力补齐完成（自动推荐值，未经人工确认）",
			zap.Int("filled", fixed), zap.Int("failed", failed),
			zap.Int("candidates", len(rows)), zap.Strings("models", filled))
	}
	return fixed, nil
}
