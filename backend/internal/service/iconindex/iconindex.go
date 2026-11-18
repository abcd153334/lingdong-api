// Package iconindex 是「实体 id → 图标 token」的批量解析器。
//
// 只有一处入口 `Resolve`：任何契约要在历史行里显示图标，都在**已经拿到
// id 列表**之后调它一次，而不是在每条 SQL 里各自 JOIN 一次 models/vendors。
// 后者的结果是一致的，但每加一个列表接口就多一份 JOIN、一次回落规则、
// 一处"忘了过滤软删"的机会 —— 而图标错了没人报错，只是"看起来怪"。
//
// 与 pricing 快照的分工：`pricing.Get()` 是 10s TTL 的**在售**目录
// （价格/上下架都要准，不缓存就是每请求打库）。本包服务的对象是
// **历史日志与任务**，它们引用的模型可能早已下架甚至软删，
// 走快照会一律变成首字占位 —— 历史记录里"当时用的是谁"必须留下来，
// 所以这里直查库、且**不加 deleted_at 过滤**。
package iconindex

import (
	"context"

	"go.uber.org/zap"

	"lingdong-api/internal/assets/brandicon"
	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/logger"
)

// batchSize 是单条 IN 的最大参数个数。
//
// 不设上限的话，日志导出（5000 行）会拼出 5000 个占位符：PG 的参数上限是
// 65535，暂时不炸，但每条 32 字节的 text 参数会让解析计划退化成 seq scan。
const batchSize = 500

// Index 是只读快照。**零值可用**（任何 id 都返回 ""）：DB 不可用时调用方
// 不必写 `if resolved != nil`，图标自然消失，页面照常渲染。
type Index struct {
	models   map[string]string
	channels map[uint64]string
}

// Model 返回模型**生效**的图标 token（自有 → 供应商回落 → ""）。
func (ix Index) Model(id string) string { return ix.models[id] }

// Channel 返回渠道的图标 token（渠道没有回落语义，为空即首字占位）。
func (ix Index) Channel(id uint64) string { return ix.channels[id] }

// Resolve 批量解析模型与渠道图标。
//
// **故意不返回 error**：图标是装饰，DB 抖动时降级为"没有图标"是正确行为，
// 让列表接口因为图标查不到而 500 才是错的。失败只留一条 Warn。
//
// 入参允许nil / 重复 / 不存在：调用方拿到的往往是"这一页的 id"，其中
// 既有下架也有已删除的，逐个校验只会让调用点写出一堆 if。
func Resolve(ctx context.Context, modelIDs []string, channelIDs []uint64) Index {
	ix := Index{}
	if db.DB == nil {
		return ix
	}

	if ids := dedupeStrings(modelIDs); len(ids) > 0 {
		ix.models = make(map[string]string, len(ids))
		for _, chunk := range chunkStrings(ids, batchSize) {
			var rows []struct {
				ID         string
				Icon       string
				VendorIcon string
			}
			// 回落规则只在 Go 里算（brandicon.Effective）：SQL 写 COALESCE 就是
			// 第二个派生点，改一处永远漏一处。
			// LEFT JOIN 而非 JOIN：供应商被软删时模型自己的图标仍要显示。
			err := db.DB.WithContext(ctx).Raw(`
				SELECT m.id AS id,
				       COALESCE(m.icon, '') AS icon,
				       COALESCE(v.icon, '') AS vendor_icon
				  FROM models m
				  LEFT JOIN vendors v ON v.id = m.vendor
				 WHERE m.id IN ?`, chunk).Scan(&rows).Error
			if err != nil {
				logger.L().Warn("iconindex: 解析模型图标失败，本次降级为无图标", zap.Error(err))
				return Index{}
			}
			for _, r := range rows {
				ix.models[r.ID] = brandicon.Effective(r.Icon, r.VendorIcon)
			}
		}
	}

	if ids := dedupeUint64(channelIDs); len(ids) > 0 {
		ix.channels = make(map[uint64]string, len(ids))
		for _, chunk := range chunkUint64(ids, batchSize) {
			var rows []struct {
				ID   uint64
				Icon string
			}
			err := db.DB.WithContext(ctx).Raw(
				`SELECT id, COALESCE(icon, '') AS icon FROM channels WHERE id IN ?`, chunk,
			).Scan(&rows).Error
			if err != nil {
				logger.L().Warn("iconindex: 解析渠道图标失败，本次降级为无图标", zap.Error(err))
				return Index{}
			}
			for _, r := range rows {
				ix.channels[r.ID] = brandicon.Effective(r.Icon, "")
			}
		}
	}

	return ix
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func dedupeUint64(in []uint64) []uint64 {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[uint64]struct{}, len(in))
	out := make([]uint64, 0, len(in))
	for _, v := range in {
		if v == 0 {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func chunkStrings(in []string, size int) [][]string {
	var out [][]string
	for i := 0; i < len(in); i += size {
		end := i + size
		if end > len(in) {
			end = len(in)
		}
		out = append(out, in[i:end])
	}
	return out
}

func chunkUint64(in []uint64, size int) [][]uint64 {
	var out [][]uint64
	for i := 0; i < len(in); i += size {
		end := i + size
		if end > len(in) {
			end = len(in)
		}
		out = append(out, in[i:end])
	}
	return out
}
