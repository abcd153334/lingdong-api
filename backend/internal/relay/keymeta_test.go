package relay

import (
	"context"
	"testing"

	"lingdong-api/internal/model"
	"lingdong-api/internal/service/keyhealth"
)

// 本文件守两条运行期归属/冷却的断链修复：
//
//  1. `callMeta.keyID` 曾经只有声明与读取、**从未赋值** —— usages.channel_key_id 恒空，
//     管理台「调用记录」的密钥ID搜索框与面板"复制密钥ID"全部指向空气；
//  2. 轮询/复查/取产物拿的是空 KeyRef，401 的 key 不会被冷却，坏 key 被反复打到任务超时退订。

// TestCallMetaNoteKeyKeepsLastNonEmpty 断言归属取「最后一次真正出站的那把」。
func TestCallMetaNoteKeyKeepsLastNonEmpty(t *testing.T) {
	var cm callMeta

	cm.noteKey(KeyRef{ID: "first"})
	if cm.keyID != "first" {
		t.Fatalf("应记录首次出站的 key，实际 %q", cm.keyID)
	}

	// 空 ref = 本次没有可用密钥（池空/全禁用）：不能把上一次的归属抹掉，
	// 否则"全员失败"的那一单会变成"走了哪把 key 不知道"。
	cm.noteKey(KeyRef{})
	if cm.keyID != "first" {
		t.Fatalf("空 ref 不得覆盖已有归属，实际 %q", cm.keyID)
	}

	cm.noteKey(KeyRef{ID: "last"})
	if cm.keyID != "last" {
		t.Fatalf("应取最后一次尝试的那把，实际 %q", cm.keyID)
	}

	// nil 接收者不得 panic：settle 路径允许 cm 为 nil（预检兜底记录）。
	var nilMeta *callMeta
	nilMeta.noteKey(KeyRef{ID: "x"})
}

// TestCoolKeyIfAuth 断言只有 401/403 才冷 key，其余一律不动。
//
// 复用 classify 的判据是关键：这里一旦自己判状态码，将来 classify 调整
// （例如把 403 归到 caller）就会分叉成两套规则。
func TestCoolKeyIfAuth(t *testing.T) {
	withRedis(t)
	ctx := context.Background()

	ch := &model.Channel{ID: 86001}
	ref := KeyRef{ID: "kid-auth"}
	// 冷却状态在 Redis 里带 TTL，跨进程/跨次运行都会残留：上一轮跑完留下的
	// auth 冷却会让本轮的「5xx 不该冷 key」直接失败（单跑能过、整包跑挂）。
	keyhealth.Clear(ctx, ch.ID, ref.ID)
	t.Cleanup(func() { keyhealth.Clear(ctx, ch.ID, ref.ID) })

	// 500：上游故障，不是这把 key 的错 —— 冷掉它等于把渠道问题算到 key 头上。
	CoolKeyIfAuth(ch, ref, &HTTPStatusError{Code: 500})
	if got := keyhealth.CoolingState(ctx, ch.ID, []string{ref.ID}); got != nil {
		t.Fatalf("5xx 不该冷 key：%v", got)
	}

	// 空 ref：本次没有可用密钥，绝不能做 key 级动作（否则会把全池一起冷掉）。
	CoolKeyIfAuth(ch, KeyRef{}, &HTTPStatusError{Code: 401})

	// 401：这枚 key 已失效。
	CoolKeyIfAuth(ch, ref, &HTTPStatusError{Code: 401})
	got := keyhealth.CoolingState(ctx, ch.ID, []string{ref.ID})
	if got[ref.ID] != keyhealth.ReasonAuth {
		t.Fatalf("401 应冷却为 auth，实际 %v", got)
	}

	// 403 同档（权限/额度被封）。
	ref2 := KeyRef{ID: "kid-forbidden"}
	CoolKeyIfAuth(ch, ref2, &HTTPStatusError{Code: 403})
	if keyhealth.CoolingState(ctx, ch.ID, []string{ref2.ID})[ref2.ID] != keyhealth.ReasonAuth {
		t.Fatal("403 应冷却为 auth")
	}
}
