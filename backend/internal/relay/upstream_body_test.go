package relay

import (
	"bytes"
	"errors"
	"testing"
)

// TestBodyCapFor 锁住「按端点族取上限」，并确认 chat 仍是 8 MiB。
//
// 改 MaxUpstreamBodyBytes 本身会让 spec/bridge_test.go 的
// 「JSON 端点超 8 MiB 必须报错」失效 —— 那份护栏是刻意保留的。
func TestBodyCapFor(t *testing.T) {
	cases := []struct {
		endpoint string
		want     int64
	}{
		{"images/generations", MaxImageBodyBytes},
		{"/v1/images/edits", MaxImageBodyBytes},
		{"images/variations", MaxImageBodyBytes},
		{"chat/completions", MaxUpstreamBodyBytes},
		{"/v1/chat/completions", MaxUpstreamBodyBytes},
		{"embeddings", MaxUpstreamBodyBytes},
		{"responses", MaxUpstreamBodyBytes},
		// 空串/未知端点必须回落到 JSON 上限，不能变成"无限读"。
		{"", MaxUpstreamBodyBytes},
	}
	for _, tc := range cases {
		if got := BodyCapFor(tc.endpoint); got != tc.want {
			t.Fatalf("BodyCapFor(%q)=%d，期望 %d", tc.endpoint, got, tc.want)
		}
	}
	if MaxUpstreamBodyBytes != 8<<20 {
		t.Fatalf("MaxUpstreamBodyBytes 被改动：%d", MaxUpstreamBodyBytes)
	}
	if MaxBinaryBodyBytes != 32<<20 {
		t.Fatalf("MaxBinaryBodyBytes 被改动：%d", MaxBinaryBodyBytes)
	}
}

// TestReadUpstreamBody 锁住「超限报错而不是静默截断」。
//
// 静默截断的后果：JSON 解析失败 → 被归因成「上游返回了坏数据」→ 换渠道重跑，
// 排查方向从一开始就错了。
func TestReadUpstreamBody(t *testing.T) {
	const limit = int64(16)

	// 恰好等于上限：必须成功（读 limit+1 才知道边界）。
	exact := bytes.Repeat([]byte("a"), int(limit))
	got, err := ReadUpstreamBody(bytes.NewReader(exact), limit)
	if err != nil {
		t.Fatalf("恰好等于上限时不应报错: %v", err)
	}
	if len(got) != int(limit) {
		t.Fatalf("读到 %d 字节，期望 %d", len(got), limit)
	}

	// 超一个字节：必须报哨兵错误，且不能被当成正常体返回。
	over := bytes.Repeat([]byte("a"), int(limit)+1)
	if _, err := ReadUpstreamBody(bytes.NewReader(over), limit); !errors.Is(err, ErrUpstreamBodyTooLarge) {
		t.Fatalf("超限应返回 ErrUpstreamBodyTooLarge，实际 %v", err)
	}

	// limit<=0 回落到 JSON 上限，避免调用方漏传导致无限读。
	small := []byte("{}")
	if got, err := ReadUpstreamBody(bytes.NewReader(small), 0); err != nil || string(got) != "{}" {
		t.Fatalf("limit=0 应回落到 JSON 上限: got=%q err=%v", got, err)
	}
}
