package taskstate

import "testing"

// TestVocabularyIsExhaustive 守「词表就是全部」。
//
// 加一个状态却忘了补编码表，症状是客户端拿到空字符串而不是报错 ——
// 这种错只有把表当穷举来断言才拦得住。
func TestVocabularyIsExhaustive(t *testing.T) {
	if len(All) != 6 {
		t.Fatalf("状态数量 %d，期望 6：加/删状态必须同步改编码表与本断言", len(All))
	}
	for i, s := range All {
		if s == "" {
			t.Fatalf("第 %d 个状态为空串", i)
		}
		if got := Word(DialectNewAPI, s); got == "" {
			t.Fatalf("状态 %q 在 new-api 词表里是空串", s)
		}
		if !Known(s) {
			t.Fatalf("状态 %q 不在 Known 里", s)
		}
	}
	// 顺序即展示顺序：排队 → 运行 → 成功/失败/超时/取消。
	want := []Status{Queued, Running, Succeeded, Failed, Expired, Canceled}
	for i := range want {
		if All[i] != want[i] {
			t.Fatalf("第 %d 项是 %q，期望 %q", i, All[i], want[i])
		}
	}
}

func TestPercentHasThreeBuckets(t *testing.T) {
	cases := map[Status]string{Queued: "0%", Running: "50%", Succeeded: "100%", Failed: "100%", Expired: "100%", Canceled: "100%"}
	for s, want := range cases {
		if got := Percent(s); got != want {
			t.Fatalf("%q 的进度是 %q，期望 %q", s, got, want)
		}
	}
}

// TestTerminalSet 守终态集合：`expired` 必须在内。
//
// 漏掉它的后果不是「少一个终态」，而是任务永远停在过期态继续被轮询 ——
// 上游成本持续发生，而平台已按超时退过款。
func TestTerminalSet(t *testing.T) {
	terminal := map[Status]bool{Succeeded: true, Failed: true, Expired: true, Canceled: true}
	for _, s := range All {
		if IsTerminal(s) != terminal[s] {
			t.Fatalf("%q 的终态判定是 %v，期望 %v", s, IsTerminal(s), terminal[s])
		}
	}
}

func TestNormalizeIsIdempotent(t *testing.T) {
	for _, s := range All {
		got, ok := Normalize(s)
		if !ok || got != s {
			t.Fatalf("平台态 %q 归一后变成 %q（ok=%v）", s, got, ok)
		}
	}
	// 常见上游写法。
	for raw, want := range map[string]Status{
		"SUCCEED": Succeeded, "completed": Succeeded, "in_progress": Running,
		"submitted": Queued, "cancelled": Canceled, " failure ": Failed,
	} {
		if got, ok := Normalize(raw); !ok || got != want {
			t.Fatalf("%q 归一成 %q（ok=%v），期望 %q", raw, got, ok, want)
		}
	}
	// 纯数字不收：那是厂商知识，由知道语义的适配器自己映射。
	if _, ok := Normalize("3"); ok {
		t.Fatal("数字码不该被平台表接受")
	}
}

// TestExpiredNeverLooksSuccessful 守一条业务底线：超时不能渲染成成功。
func TestExpiredNeverLooksSuccessful(t *testing.T) {
	if Word(DialectNewAPI, Expired) == Word(DialectNewAPI, Succeeded) {
		t.Fatal("expired 与 succeeded 编码成了同一个词")
	}
	if PortalError(Expired) == "" || PortalError(Expired) == PortalError(Succeeded) {
		t.Fatal("expired 必须有自己的分类文案")
	}
}
