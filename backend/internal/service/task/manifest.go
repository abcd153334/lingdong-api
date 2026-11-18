package task

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/relay"
	"lingdong-api/internal/service/billing"
)

// 本文件是 B3 的「产物清单 + 上游用量」载体，从 task.go 拆出来是为了让
// Succeed 的主流程保持可读：两者的细节都只需要在成功路径上被关心一次。

// artifactEntry 是 tasks.artifact_manifest 里的单条产物。
//
// 字段刻意用单字母 JSON 键：清单在 tasks 表里会随产物数线性增长，
// 一次生成 10 张图时键名带来的体积差是 3~4 倍，而这张表是全库热点。
type artifactEntry struct {
	// Index 是稠密产物序号（0..n-1），与 files.artifact_index 一致。
	Index int `json:"i"`
	// URL 是上游源链接。**仅内部使用**：既不下发，也不写进任何响应。
	URL string `json:"u,omitempty"`
	// Text 标记文本产物（歌词没有下载链接，内容是本次轮询拿到的 ResultText）。
	Text bool `json:"t,omitempty"`
	// FileID 是本地 files.id；0 表示「还没转存成功」，下一轮要重试。
	FileID uint64 `json:"f"`
}

// parseArtifactManifest 容错解析清单（非法 JSON 按空清单处理）。
func parseArtifactManifest(raw model.JSON) []artifactEntry {
	if len(raw) == 0 {
		return nil
	}
	var list []artifactEntry
	if err := json.Unmarshal(raw, &list); err != nil {
		// 脏数据不阻断成功路径：清单坏了顶多退化成「重新转存一遍」，
		// 而这里返回值错误会让一个已经成功的任务永远结算不了。
		logger.L().Warn("artifact_manifest 解析失败，按空清单重来", zap.Error(err))
		return nil
	}
	return list
}

// marshalArtifactManifest 序列化清单（空清单返回 nil → 列写 NULL）。
func marshalArtifactManifest(list []artifactEntry) model.JSON {
	if len(list) == 0 {
		return nil
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return nil
	}
	return model.JSON(raw)
}

// mergeArtifactManifest 把「本次轮询看到的产物」合并进已有清单。
//
// 合并规则（顺序即语义）：
//  1. 按上游 URL 匹配已有条目 → 复用它的序号与已转存的 FileID（**不重下**）；
//  2. 新出现的 URL → 追加（序号在最后重新稠密编号）；
//  3. 文本产物固定排最后：它没有 URL，位置一旦漂移就会让 /contents/{i} 指向错的产物。
//
// 为什么必须按 URL 而不是按下标匹配：上游返回顺序不保证稳定，
// 按下标会「第 1 张和第 2 张互换」，用户拿到的文件名与内容对不上。
func mergeArtifactManifest(existing []artifactEntry, urls []string, text string) []artifactEntry {
	byURL := make(map[string]artifactEntry, len(existing))
	var textEntry *artifactEntry
	for i := range existing {
		e := existing[i]
		if e.Text {
			cp := e
			textEntry = &cp
			continue
		}
		if e.URL != "" {
			byURL[e.URL] = e
		}
	}

	out := make([]artifactEntry, 0, len(urls)+1)
	for _, u := range urls {
		if e, ok := byURL[u]; ok {
			out = append(out, e)
			continue
		}
		out = append(out, artifactEntry{URL: u})
	}
	if strings.TrimSpace(text) != "" {
		if textEntry != nil {
			out = append(out, *textEntry)
		} else {
			out = append(out, artifactEntry{Text: true})
		}
	}
	for i := range out {
		out[i].Index = i
	}
	return out
}

// persistArtifactManifest 把清单单独落库（不等 CAS、不参与结算事务）。
//
// 这是 P0-07 的根因修复：原实现把「产物事实」和「账务结果」绑在同一次 CAS 里，
// 一旦结算失败，产物清单也一起丢 —— 用户实际拿到了图，平台却认为没有产物，
// 下一轮重新转存时又因为唯一索引冲突全部失败。
// 现在先把清单写死，结算失败只需补账，不必重下。
func persistArtifactManifest(ctx context.Context, taskID uint64, list []artifactEntry) error {
	if taskID == 0 {
		return nil
	}
	err := db.DB.WithContext(ctx).Model(&model.Task{}).Where("id = ?", taskID).
		Update("artifact_manifest", marshalArtifactManifest(list)).Error
	if err != nil {
		logger.L().Error("产物清单落库失败（结算会继续，下轮重写）",
			zap.Uint64("task_id", taskID), zap.Error(err))
	}
	return err
}

// persistUpstreamUsage 把上游 usage 原文落库（结算前调用，供事后对账）。
//
// 只写原文（`up.Raw`），**不再写 platform 归一后的结构**：归一值已经落在
// usages.video_seconds / usage_source / video_seconds_source 上，而 raw 列一旦
// 两种含义混存，事后就分不清「上游报了什么」与「平台算成什么」——
// 这正是核账时要回答的第一个问题。没有原文就留空，不拿合成物冒充。
//
// 写失败只告警不阻断：用量原文是「辅助证据」，结算本身用的是合并后的结果，
// 为了一条证据写不进去而让整笔交易卡住，代价不成比例。
func persistUpstreamUsage(ctx context.Context, taskID uint64, up relay.TaskUsage) {
	if taskID == 0 || len(up.Raw) == 0 {
		return
	}
	if err := db.DB.WithContext(ctx).Model(&model.Task{}).Where("id = ?", taskID).
		Update("upstream_usage", model.JSON(up.Raw)).Error; err != nil {
		logger.L().Warn("上游用量原文落库失败（不影响结算）",
			zap.Uint64("task_id", taskID), zap.Error(err))
	}
}

// manifestStats 统计清单里「已成功转存」的主产物与数量。
//
// 主产物定义：file_id > 0 的**第一个**条目（与改造前 mainID 语义一致）。
func manifestStats(list []artifactEntry) (mainID uint64, count int) {
	for _, e := range list {
		if e.FileID == 0 {
			continue
		}
		if mainID == 0 {
			mainID = e.FileID
		}
		count++
	}
	return mainID, count
}

// ────────────────────────────────────────────────────────────────────────────
// 成功路径并发锁
// ────────────────────────────────────────────────────────────────────────────

// succeedLockTTL 是成功路径锁的有效期。
//
// 取 10 分钟：一次任务最多 10 个产物、每个最多 512MB 下载，最坏情况确实可能跑上几分钟。
// 锁过期意味着「这一轮放弃，下一轮重来」，比两个协程同时转存同一批产物（唯一索引冲突）安全。
const succeedLockTTL = 10 * time.Minute

// lockSucceed 给「任务成功收尾」加一把分布式锁。
//
// 为什么必须加：worker 与「用户主动查任务」两条路径都可能触发 Succeed，
// 而 Succeed 现在包含「下载产物 + 结算」。两个协程同时进来会出现
// 「同一个产物被下载两次、第二个撞 (task_id, artifact_index) 唯一索引」。
//
// Redis 不可用时放行（单实例部署语义，与 order.lockTopup / worker.manager 一致）：
// 宁可偶发重复下载，也不能因为 Redis 挂了让所有任务永久卡在 running。
func lockSucceed(ctx context.Context, taskID uint64) (func(), bool) {
	noop := func() {}
	if !db.RedisReady() {
		return noop, true
	}
	key := fmt.Sprintf("task:succeed:%d", taskID)
	ok, err := db.RDB.SetNX(ctx, key, "1", succeedLockTTL).Result()
	if err != nil {
		logger.L().Warn("任务成功路径取锁失败，按无锁继续",
			zap.Uint64("task_id", taskID), zap.Error(err))
		return noop, true
	}
	if !ok {
		return nil, false
	}
	return func() {
		if err := db.RDB.Del(ctx, key).Err(); err != nil {
			logger.L().Warn("任务成功路径锁释放失败（会自动过期）",
				zap.Uint64("task_id", taskID), zap.Error(err))
		}
	}, true
}

// ────────────────────────────────────────────────────────────────────────────
// 上游用量合并（决策 D / B3-3）
// ────────────────────────────────────────────────────────────────────────────

// taskUsageSettleMode 返回上游用量的生效方式（LD_TASK_USAGE_SETTLE，默认 "1"）。
//
//   - "1"：上游用量参与结算（取 max(上游, 请求)，不少收）；
//   - "shadow"：只落库 + 写 anomaly，**不改结算**（灰度观察口径偏差）；
//   - "0"：完全忽略上游用量（回退到改造前行为）。
//
// 灰度顺序必须是 shadow → 1：直接切 1 时若某适配器的解析有偏差，
// 会立刻在账单上体现，而 shadow 阶段能先比对 24 小时的偏差幅度。
func taskUsageSettleMode() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LD_TASK_USAGE_SETTLE"))) {
	case "0", "off", "false", "no":
		return "0"
	case "shadow":
		return "shadow"
	default:
		return "1"
	}
}

// mergeUpstreamUsage 把上游自报用量合并进计费用量，返回新用量与需要写进 anomaly 的标记。
//
// 视频秒数分**两种口径**，混成一条就是确定性错账：
//
//	显式秒数（SecondsAuto=false）：取 max(上游, 请求)。
//	  上游自动续长（按档位取整）时按上游收；上游报得比请求短（重试后只生成了几秒）
//	  仍按请求收 —— 用户点的是 10 秒的任务，平台不能因为上游偷懒就退钱，
//	  否则可以被刷成「大额冻结 → 上游短生成 → 全额退」。
//	智能时长（SecondsAuto=true）：**以上游为准**。
//	  请求侧的秒数是冻结预算（model.max_seconds），不是用户要的时长。
//	  按预算收等于「选了智能时长就按最贵收」—— 用户选它本意是"让模型决定"，
//	  结果反而被收最贵，这是可举证的错账。
//
// 秒数一律用 Round 而不是 Ceil：Ceil 会把 121 帧（5.04 秒）算成 6 秒，多收 19%，
// 而计费粒度是秒，四舍五入才与用户观感一致（Ark 的 duration 是整数，Round 对它无影响）。
// 上限一律 clamp 到 MaxVideoSeconds：上游报 86400 秒时必须截断，
// 否则一次异常响应就能把账单算成天文数字（Settle 侧还有 charge_capped 兜底）。
//
// 图像**不在这里处理**：图像按「实际成功转存数」计费（用户拿不到的图不该收费），
// 那个数字只有转存结束后才知道，由调用方用 applyArtifactActuals 处理。
func mergeUpstreamUsage(
	usage billing.Usage, up relay.TaskUsage, p params,
) (billing.Usage, []string) {
	var anomalies []string
	mode := taskUsageSettleMode()
	if mode == "0" {
		return usage, anomalies
	}

	// ---- 视频秒数 ----
	if up.OutputSeconds > 0 {
		upSecs := int64(math.Round(up.OutputSeconds))
		if upSecs < 1 {
			upSecs = 1
		}
		var merged int64
		if p.SecondsAuto {
			merged = upSecs
			if upSecs < p.Seconds {
				// 智能时长下上游比预算短是正常结果，只留痕（便于观察上游的选长行为）。
				anomalies = append(anomalies, model.AnomalyUsageUpstreamLower)
			}
		} else {
			switch {
			case upSecs > p.Seconds:
				anomalies = append(anomalies, model.AnomalyUsageUpstreamHigher)
			case upSecs < p.Seconds:
				// 保留请求值（不少收），但必须留痕：大量出现说明上游在偷工减料。
				anomalies = append(anomalies, model.AnomalyUsageUpstreamLower)
			}
			merged = p.Seconds
			if upSecs > merged {
				merged = upSecs
			}
		}
		if merged < 1 {
			merged = 1
		}
		// 上限跟着**段数**走（段数 × 单条上限），不是单条上限：
		// 本字段是多段参考素材的累计秒数，10 段 10 分钟 = 6000 秒按 3600 截
		// 就是白送 40%；但完全放开又会放过上游谎报（曾见 86400）。
		merged = billing.ClampRefVideoSeconds(merged, usage.RefVideos)
		if mode == "1" {
			usage.VideoSeconds = merged
		}
	} else if p.SecondsAuto {
		// 智能时长但上游既没给 duration 也没给 frames（适配器已尝试用帧数换算）：
		// 只能回落到冻结预算，但**必须留痕** —— 否则「某条渠道永远不报时长，
		// 就永远按上限收」这件事没人会发现，直到月底对账整体偏高。
		anomalies = append(anomalies, model.AnomalyUsageUpstreamMissing)
	}

	// ---- 参考视频秒数 ----
	if up.InputSeconds > 0 {
		upSecs := int64(math.Ceil(up.InputSeconds))
		merged := usage.RefVideoSeconds
		if upSecs > merged {
			merged = upSecs
			anomalies = append(anomalies, model.AnomalyUsageUpstreamInputHigher)
		}
		if merged > billing.MaxVideoSeconds {
			merged = billing.MaxVideoSeconds
		}
		if mode == "1" {
			usage.RefVideoSeconds = merged
		}
	}

	// ---- 上游图片张数只做对照（结算以成功转存数为准）----
	if up.ImageCount > 0 && up.ImageCount != usage.ImageN {
		anomalies = append(anomalies, model.AnomalyUpstreamImageCountMismatch)
	}

	return usage, anomalies
}

// applyArtifactActuals 按「实际成功转存数」修正图像张数。
//
// 为什么不用上游 ImageCount：用户只看得到 GET /v1/tasks/{id}/artifacts。
// 上游说生成了 4 张、但只成功转存 2 张时，后两张用户永远拿不到——
// 按 4 张收费就是「收钱不交货」，且无法向用户解释。
//
// 只在 ImageN > 0（本单按张数计价）时生效：音乐/歌词任务同样有多个产物，
// 但按任务计费，改张数会让价格凭空变化。
//
// okCount 为 0 时的语义由 `billing.BillableCount` 按价目 kind 分派：
//   - 按条（音乐/歌词）：确实一条都没交付 → 免单（退全部冻结）；
//   - 按张（图片）：视为"没有交付证据"，保持意图，由全失败闸门转 Fail 全额释放。
//
// 这两个分支曾经写反过（注释说"宁可少收"，行为却是按意图照收），也曾经由
// 调用方各自 if/else 分派，导致回收路径与结算路径口径不一致 —— 现在只剩一处。
func applyArtifactActuals(usage billing.Usage, okCount int, _ params, snap billing.Snapshot) billing.Usage {
	// 用 snapshot 的价目 kind 判定而不是 modality：同一模态可能同时存在 per_task 存量模型，
	// 而计费口径只由价目 kind 决定 —— 拿 modality 判会把存量单也改成按条。
	// okCount 是**事实**（成功转存数），所以 known=true。
	usage.ImageN = billing.BillableCount(snap.SpecPrice, usage.ImageN, int64(okCount), true)
	return usage
}
