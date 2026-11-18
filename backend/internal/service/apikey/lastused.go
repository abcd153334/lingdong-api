package apikey

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/service/authevent"
)

// last_used_at 的唯一写管线：Authenticate 热路径 → touchKey（节流）→ 单行 UPDATE。
// expired_at 的续期唯一实现：renewExpiredAt（条件 UPDATE，过期救回与临期预续共用）。

const (
	// touchInterval 是同一 KEY 两次 last_used_at 落库的最小间隔。
	// 热路径每请求都会路过这里，落库必须按 KEY 节流；5 分钟精度对
	// "闲置过期"（天级语义）绰绰有余，写放大 ≈ 活跃 KEY 数 × 12/h/副本。
	touchInterval = 5 * time.Minute
	// touchMaxEntries 是节流 map 的软上限，超过即顺带清理陈旧条目，
	// 防止海量低频 KEY 把进程内 map 撑大（条目 16B 级，8192 仅 128KB，纯防御）。
	touchMaxEntries = 8192
	// touchStale 是清理时保留条目的最大年龄。
	touchStale = time.Hour
	// sweepInterval 是两次陈旧条目清理之间的最小间隔。
	//
	// 没有它的话，map 一旦越过软上限，"每次 touch 都全量遍历"——活跃期就是
	// 每请求一次 O(8192)。条目只有 16B 级，5 分钟的膨胀完全可以忽略。
	sweepInterval = 5 * time.Minute
	// renewAhead 是临期预续窗口：剩余寿命小于它就续满一整个周期，
	// 避免"每 5 分钟活跃一次但每次都在过期边缘"的 KEY 反复走救回路径。
	renewAhead = 30 * 24 * time.Hour
	// touchTimeout 是节流写与续期写的单条超时；超时只放弃本次，不影响请求结果。
	touchTimeout = 2 * time.Second
)

var (
	touchMu   sync.Mutex
	touchAt   = map[uint64]time.Time{}
	lastSweep time.Time
)

// touchKey 节流落库 last_used_at，顺带给临期的托管密钥续期。
//
// 同步执行且不开 goroutine：单行主键 UPDATE 带 2s 超时，最坏拖慢请求 2s 一次
// （每 KEY 每 5 分钟至多一次）；异步则要为退出flush和并发上限各付一套复杂度。
// 错误只记日志：last_used_at 丢失最坏后果是"闲置判定偏保守"（可能误过期救回失败），
// 不值得为它让请求失败。
func touchKey(k *model.APIKey, now time.Time) {
	touchMu.Lock()
	last, seen := touchAt[k.ID]
	if seen && now.Sub(last) < touchInterval {
		touchMu.Unlock()
		return
	}
	touchAt[k.ID] = now
	// 超限后**不是每次**都全量扫：那会让活跃期的每个请求都付一次 O(n)。
	if len(touchAt) > touchMaxEntries && now.Sub(lastSweep) > sweepInterval {
		for id, t := range touchAt {
			if now.Sub(t) > touchStale {
				delete(touchAt, id)
			}
		}
		lastSweep = now
	}
	touchMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), touchTimeout)
	defer cancel()
	if err := db.DB.WithContext(ctx).Model(&model.APIKey{}).
		Where("id = ?", k.ID).
		Update("last_used_at", now).Error; err != nil {
		logger.L().Debug("记录密钥最近使用失败（下次使用会重试）",
			zap.Uint64("key_id", k.ID), zap.Error(err))
		return
	}
	k.LastUsedAt = &now

	// 临期预续：仅托管密钥。剩余寿命不足"提前量"时续满一整个周期。
	//
	// ahead 必须与谓词用**同一个**值，这是这条路径能生效的关键：
	// 曾用 ahead=0，而这条路径的前提恰恰是"expired_at 还没到"，两个条件互斥
	// ⇒ 预续永远 0 行，续期只发生在"过期后的第一次请求"。
	pol := hostedPolicyOf()
	ahead := renewAheadFor(pol)
	if k.IsHosted() && k.ExpiredAt != nil && now.Add(ahead).After(*k.ExpiredAt) {
		if renewed, err := renewExpiredAt(k, now, pol, ahead); err != nil {
			logger.L().Debug("托管密钥临期续期失败（过期救回路径会兜底）",
				zap.Uint64("key_id", k.ID), zap.Error(err))
		} else if renewed {
			logger.L().Info("托管密钥临期已自动续期",
				zap.Uint64("key_id", k.ID), zap.Uint64("user_id", k.UserID))
		}
	}
}

// renewAheadFor 返回临期预续的提前量：min(renewAhead, ExpireDays/2)。
//
// 为什么不能恒取 renewAhead：`expire_days` 是运营可配的，一旦配得比 30 天短
// （例如 7 天），"剩余 < 30 天"就恒为真 —— 每次 touch（每钥每 5 分钟）都会
// 命中预续谓词，产出一条 UPDATE 和一条 key_auto_renew 事件。功能上仍然正确
// （有效期一直续着），但写放大从"每天几次"变成"每天几百次"，事件流也被刷屏。
// 取一半后：续满 → 剩余 = ExpireDays > ExpireDays/2 → 不会再命中，直到又临近一半。
func renewAheadFor(pol HostedPolicy) time.Duration {
	ahead := renewAhead
	if pol.ExpireDays > 0 {
		if half := time.Duration(pol.ExpireDays) * 24 * time.Hour / 2; half < ahead {
			ahead = half
		}
	}
	return ahead
}

// markExpired 把「真闲置超期」的托管密钥落成显式禁用（status=2 + reason=expired）。
//
// 为什么必须落库：只判不写的话，门户里这把钥始终显示"启用中"，而应用每次调用
// 都 401 —— 用户与运营都只能看到一条错误码，既无法解释也无法统计。
// 落库后它是「可被轮换恢复」的一类禁用（`CanRotateRecover`），不是永久封禁。
//
// cutoff 是调用方算好的**闲置截止线**（now - ExpireDays）：行里的
// COALESCE(last_used_at, created_at) 早于它才算闲置超期。
// 传进来而不是在这里重算，是为了让"判定"与"写入"用同一个值。曾经这里自己写
// `COALESCE(...) < now-ExpireDays`，与 Go 侧的时间比较构成两条等价但独立的
// 不等式 —— 任一侧改动（例如把基准换成别的时间点）就会静默分叉。
//
// 谓词仍保留闲置条件（不只是 status=1）：这条写发生在鉴权之后、落库之前，
// 中间可能有人改过状态 —— 管理员刚解禁并刷新了 last_used_at，一个基于**旧**
// 基准的在途请求不能把刚解禁的钥又打回禁用。
//
// 写失败只记日志：本次判定仍然拒绝（不是 fail-open），下次请求会重试写入。
func markExpired(k *model.APIKey, now, cutoff time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), touchTimeout)
	defer cancel()
	res := db.DB.WithContext(ctx).Model(&model.APIKey{}).
		Where("id = ? AND status = ? AND COALESCE(last_used_at, created_at) < ?",
			k.ID, KeyStatusEnabled, cutoff).
		Updates(map[string]any{
			"status":          KeyStatusDisabled,
			"disabled_reason": model.KeyDisabledExpired,
			"updated_at":      now,
		})
	if res.Error != nil {
		logger.L().Debug("标记托管密钥闲置失效失败（下次请求会重试）",
			zap.Uint64("key_id", k.ID), zap.Error(res.Error))
		return
	}
	if res.RowsAffected == 0 {
		return // 已被并发请求标记，或期间有人刷新了 last_used_at（等价于"不该禁"）
	}
	k.Status = KeyStatusDisabled
	k.DisabledReason = model.KeyDisabledExpired
	authevent.Record(context.Background(), authevent.Event{
		UID: k.UserID, Event: authevent.EventKeyAutoExpire, Result: authevent.ResultSuccess,
		Detail: map[string]any{"key_id": k.ID, "kind": k.Kind, "last_used_at": k.LastUsedAt},
	})
}

// renewExpiredAt 把托管密钥的有效期续到 now+ExpireDays，条件是
// `expired_at < now+ahead`。所有续期都走这里（救回与临期预续共用）。
//
// ahead 的两种取值就是两种语义，没有第三套：
//
//	ahead = 0      救回：只对**已经过期**的钥生效（Authenticate 的过期分支）
//	ahead = renewAhead  临期预续：剩余寿命不足 renewAhead 就续满
//
// 参数化是为了消灭一个"看起来对、实际恒定不生效"的分支：曾经的实现只有
// `expired_at < now`，而预续的调用前提是 "expired_at 还没到但快到了"，
// 两个条件互斥 ⇒ 预续永远 0 行，续期只发生在"过期后的第一次请求"。
//
// 条件 UPDATE 保证不回退——两个并发请求各自算出的新值都是 now+ExpireDays 量级，
// 谁先落库都成立；后到的因谓词不再成立而 0 行，等价于"已被续过"。
// 不为此加行锁：单条 UPDATE 原子，锁只买来"少记一条信息性事件"，不值。
//
// ExpireDays<=0 时调用方已在 Authenticate 放行（过期功能整体关闭），这里不写库。
func renewExpiredAt(k *model.APIKey, now time.Time, pol HostedPolicy, ahead time.Duration) (bool, error) {
	if pol.ExpireDays <= 0 {
		return false, nil
	}
	newExp := now.AddDate(0, 0, pol.ExpireDays)
	ctx, cancel := context.WithTimeout(context.Background(), touchTimeout)
	defer cancel()
	res := db.DB.WithContext(ctx).Model(&model.APIKey{}).
		Where("id = ? AND kind = ? AND expired_at < ?", k.ID, model.KeyKindHosted, now.Add(ahead)).
		Updates(map[string]any{"expired_at": newExp, "updated_at": now})
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected == 0 {
		return false, nil // 并发请求已续过，或剩余寿命仍充足（等价成功）
	}
	newT := newExp
	k.ExpiredAt = &newT
	authevent.Record(context.Background(), authevent.Event{
		UID: k.UserID, Event: authevent.EventKeyAutoRenew, Result: authevent.ResultSuccess,
		Detail: map[string]any{
			"key_id": k.ID, "kind": k.Kind,
			"expired_at": newExp.Format(time.RFC3339),
			// ahead>0 表示这次是"临期预续"，不是"过期后救回"。
			// 两者在排障时含义完全不同：前者是健康状态，后者说明已经掉过一次线。
			"preemptive": ahead > 0,
		},
	})
	return true, nil
}
