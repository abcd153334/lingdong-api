package controller

import (
	"time"

	"lingdong-api/internal/model"
	"lingdong-api/internal/service/apikey"
)

// hostedKeyView 是托管密钥对外的**唯一**视图。
//
// 签发（门户 / 对外两个凭证面）与自举轮换都用它：同一个概念在两个入口必须是
// 同一个形状，接入方写一套解析就能同时处理两条路径；字段增减也只有一处要改。
// 改造前两处各写一份 gin.H，字段列表已经出现过一次漂移（窗口字段只加在了签发侧）。
type hostedKeyView struct {
	ID     uint64 `json:"id"`
	Prefix string `json:"prefix"`
	// Scopes 是这把钥**实际**带的权限点（契约 ∩ 平台策略），不是契约全集。
	Scopes []string `json:"scopes"`
	Status int      `json:"status"`
	// ExpiredAt 是有效期；null = 平台关闭了过期策略（永不过期）。
	// 活跃使用的钥会被自动续期，所以它通常是一个「滚动的远期时间」。
	ExpiredAt *time.Time `json:"expired_at"`
	// PreviousExpiresAt 是**上一把明文**的最后可用时间；null = 没有上一代。
	// 轮换响应里读它来排期；健康自检里非 null 说明还在窗口期。
	PreviousExpiresAt *time.Time `json:"previous_expires_at"`
	AmountLimit       int64      `json:"amount_limit"`
	Used              int64      `json:"used"`
	RPMLimit          int        `json:"rpm_limit"`
}

// newHostedKeyView 把模型投影成视图（唯一实现）。
func newHostedKeyView(k *model.APIKey, previousExpiresAt *time.Time) hostedKeyView {
	return hostedKeyView{
		ID: k.ID, Prefix: k.KeyPrefix,
		Scopes: apikey.DecodeStrings(k.Scopes), Status: int(k.Status),
		ExpiredAt: k.ExpiredAt, PreviousExpiresAt: previousExpiresAt,
		AmountLimit: k.AmountLimit, Used: k.Used, RPMLimit: k.RPMLimit,
	}
}

// hostedKeyIssueView 是签发/轮换响应的 key 段：公共视图 + 三个"本次动作"字段。
//
// 用匿名嵌入而不是复制字段：复制就会在下次加字段时漏掉一边。
type hostedKeyIssueView struct {
	hostedKeyView
	// Created 表示本次是否**新建**了记录（false = 命中已有密钥）。
	Created bool `json:"created"`
	// Rotated 表示本次是否真的换了新钥（plaintext 非空）。
	Rotated bool `json:"rotated"`
	// IssuedAt 是这把记录的签发时间（轮换不改它）。
	IssuedAt time.Time `json:"issued_at"`
}

// secondsUntil 把"可空时间点"换成"距它还有多少秒"；nil 返回 nil。
//
// 负值是有意义的（已过期 / 窗口已结束），不钳到 0：调用方据此区分
// "还剩 3 秒"与"已经过期 3 天"，而钳零会让这两种情况长得一样。
func secondsUntil(t *time.Time, now time.Time) *int64 {
	if t == nil {
		return nil
	}
	v := int64(t.Sub(now) / time.Second)
	return &v
}

// liveUntil 把已经过去的时间点归一成 nil。
//
// 为什么需要：`prev_key_expires_at` 在窗口结束后**不会被清理**（唯一写点是轮换，
// 而轮换只在有下一次轮换时才发生）。直接下发它会让"非 null = 还在窗口期"这条
// 文档承诺永久失真：窗口早结束了，字段还挂着一个过去的时间，剩余秒数还是负数。
// 判据只在这里用一次，且与 Authenticate 的 `After(now)` 同一口径。
func liveUntil(t *time.Time, now time.Time) *time.Time {
	if t == nil || !t.After(now) {
		return nil
	}
	return t
}
