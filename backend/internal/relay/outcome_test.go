package relay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/channelkeys"
	"lingdong-api/internal/service/keyhealth"
)

// withRedis 连接本机 Redis（独立 DB 15）用于归因断言；不可用时跳过。
func withRedis(t *testing.T) {
	t.Helper()
	if db.RedisReady() {
		return
	}
	db.InitRedis("127.0.0.1:6379", "", 15)
	if !db.RedisReady() {
		t.Skip("未找到可用 Redis，跳过归因集成用例")
	}
}

// TestClassify 穷举归属判定：这是 E2 的全部规则，必须一眼能核对。
func TestClassify(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		err     error
		aborted bool
		want    outcome
	}{
		{"2xx", 200, nil, false, outcomeSuccess},
		{"201", 201, nil, false, outcomeSuccess},
		{"401 这枚 key 失效", 401, nil, false, outcomeAuth},
		{"403 权限/额度", 403, nil, false, outcomeAuth},
		{"429 限流", 429, nil, false, outcomeRateLimit},
		{"400 参数错", 400, nil, false, outcomeCaller},
		{"404 路径错", 404, nil, false, outcomeCaller},
		{"413 体积超限", 413, nil, false, outcomeCaller},
		{"422 语义错", 422, nil, false, outcomeCaller},
		{"409 冲突", 409, nil, false, outcomeCaller},
		{"500 上游内部", 500, nil, false, outcomeUpstream},
		{"503 上游不可用", 503, nil, false, outcomeUpstream},
		{"502 网关", 502, nil, false, outcomeUpstream},
		{"连接失败（无状态码）", 0, errors.New("dial tcp: i/o timeout"), false, outcomeUpstream},
		{"客户端断开优先于一切", 200, nil, true, outcomeClientAbort},
		{"客户端断开且带错误", 0, errors.New("context canceled"), true, outcomeClientAbort},
		{"异常 3xx 不归因调用方", 302, nil, false, outcomeUpstream},
	}
	for _, c := range cases {
		if got := classify(c.status, c.err, c.aborted); got != c.want {
			t.Errorf("%s: classify(%d,%v,%v) = %v, want %v", c.name, c.status, c.err, c.aborted, got, c.want)
		}
	}
}

// TestDecideAfterResponse 锁定 S5 的 failover 规则（B1 只加归因，不改规则）。
func TestDecideAfterResponse(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		attempt     int
		maxAttempts int
		aborted     bool
		want        failoverDecision
	}{
		{"2xx 直接返回", 200, 0, 4, false, decideReturn},
		{"5xx 还有额度 → 换渠道", 500, 0, 4, false, decideRetry},
		{"429 还有额度 → 换渠道", 429, 0, 4, false, decideRetry},
		{"5xx 最后一次 → 透传", 500, 3, 4, false, decideReturn},
		{"429 单次额度 → 透传", 429, 0, 1, false, decideReturn},
		{"400 不重试", 400, 0, 4, false, decideReturn},
		{"401 不重试（只冷却该 key）", 401, 0, 4, false, decideReturn},
		{"客户端断开 → 终止", 500, 0, 4, true, decideAbort},
	}
	for _, c := range cases {
		if got := decideAfterResponse(c.status, c.attempt, c.maxAttempts, c.aborted); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// TestRecordAttemptAttribution 是 E2 的行为级验收：
// 鉴权/限流只冷却那一枚 key，调用方错误与客户端断开完全不记渠道，只有 upstream 才记渠道失败。
func TestRecordAttemptAttribution(t *testing.T) {
	withRedis(t)
	ctx := context.Background()
	const chID uint64 = 91001
	k0, k1 := "kid-0", "kid-1"
	cleanup := func() {
		db.RDB.Del(ctx, coolKeyOf(chID, k0), coolKeyOf(chID, k1),
			fmt.Sprintf("ld:ch:hreq:%d", chID), fmt.Sprintf("ld:ch:hfail:%d", chID))
	}
	cleanup()
	t.Cleanup(cleanup)
	cand := Candidate{Channel: &model.Channel{ID: chID, Name: "t"}, Weight: 1}
	ref0 := KeyRef{Key: "sk-0", Index: 0, ID: k0}
	ref1 := KeyRef{Key: "sk-1", Index: 1, ID: k1}

	// ① 401 → 只冷却 key#1，渠道滑窗不应出现样本
	recordAttempt(cand, ref1, outcomeAuth, 401)
	if !coolingOf(t, chID, k1) {
		t.Fatal("401 应冷却该枚密钥")
	}
	if coolingOf(t, chID, k0) {
		t.Fatal("401 不得波及同渠道其它密钥")
	}
	if channelSamples(t, chID) != 0 {
		t.Fatal("401 不应计入渠道失败（E2：3 key 坏 2 不该熔断整条渠道）")
	}

	// ② 成功 → 解除该 key 冷却
	recordAttempt(cand, ref1, outcomeSuccess, 200)
	if coolingOf(t, chID, k1) {
		t.Fatal("成功后应解除冷却")
	}

	// ③ 429 → 冷却（时长更短），仍不记渠道
	recordAttempt(cand, ref0, outcomeRateLimit, 429)
	if !coolingOf(t, chID, k0) {
		t.Fatal("429 应冷却该枚密钥")
	}
	if channelSamples(t, chID) != 0 {
		t.Fatal("429 不应计入渠道失败")
	}

	// ④ 400 → 既不冷却也不记渠道（用户打错字不能把渠道打挂）
	recordAttempt(cand, ref0, outcomeCaller, 400)
	if channelSamples(t, chID) != 0 {
		t.Fatal("调用方错误不应计入渠道失败")
	}

	// ⑤ 客户端断开 → 什么都不记
	recordAttempt(cand, ref0, outcomeClientAbort, 0)
	if channelSamples(t, chID) != 0 {
		t.Fatal("客户端断开不应计入渠道失败")
	}

	// ⑥ 5xx → 计入渠道失败
	recordAttempt(cand, ref0, outcomeUpstream, 502)
	if channelSamples(t, chID) != 1 {
		t.Fatalf("5xx 应计入渠道失败，实际样本数 %d", channelSamples(t, chID))
	}

	// ⑦ 没有可用密钥（KeyRef 全空）时不得做任何 key 级动作：
	//    否则会把"配置事故"一起算成"这枚 key 坏了"，把账号池全部冷却。
	recordAttempt(cand, KeyRef{}, outcomeAuth, 401)
	if coolingOf(t, chID, "") {
		t.Fatal("空 KeyRef 不应写入 key 级冷却键")
	}
}

// coolKeyOf 与 keyhealth 的键格式保持一致（测试直接断言 Redis 键的存在性）。
func coolKeyOf(chID uint64, keyID string) string {
	return fmt.Sprintf("ld:ch:kcd:%d:%s", chID, keyID)
}

// coolingOf 判断某枚密钥是否在冷却中。
func coolingOf(t *testing.T, chID uint64, keyID string) bool {
	t.Helper()
	n, err := db.RDB.Exists(context.Background(), coolKeyOf(chID, keyID)).Result()
	if err != nil {
		t.Fatalf("查询冷却键失败: %v", err)
	}
	return n > 0
}

// channelSamples 返回渠道滑窗内的样本数。
func channelSamples(t *testing.T, chID uint64) int64 {
	t.Helper()
	n, err := db.RDB.ZCard(context.Background(), fmt.Sprintf("ld:ch:hreq:%d", chID)).Result()
	if err != nil {
		t.Fatalf("查询渠道滑窗失败: %v", err)
	}
	return n
}

// entryOf 构造一条已解密的密钥条目（ID 手写，不依赖加解密，便于穷举判定）。
func entryOf(i int, plain, id string) channelkeys.Entry {
	return channelkeys.Entry{Index: i, Plain: plain, ID: id}
}

// TestSelectUsable 穷举「一把都用不了」的四种形态。
//
// 这些分支只在故障时才会走到，却是"对外报 429 还是 503""要不要熔断渠道"
// 的唯一判据；不穷举就只能等线上第一次配错 key 时才发现。
func TestSelectUsable(t *testing.T) {
	withRedis(t)
	ctx := context.Background()
	const chID uint64 = 91002
	cleanup := func() {
		db.RDB.Del(ctx, coolKeyOf(chID, "a"), coolKeyOf(chID, "b"), coolKeyOf(chID, "c"))
	}
	cleanup()
	t.Cleanup(cleanup)

	// ① 库里两条 token 全部解密失败 → broken
	broken := channelkeys.Result{
		Total: 2, Failed: 2,
		Entries: []channelkeys.Entry{
			{Index: 0, Token: "x", Err: errors.New("bad")},
			{Index: 1, Token: "y", Err: errors.New("bad")},
		},
	}
	_, err := selectUsable(chID, broken, nil, nil)
	var noKey *NoUsableKeyError
	if !errors.As(err, &noKey) || noKey.Reason() != "broken" || noKey.Broken != 2 {
		t.Fatalf("全解密失败应为 broken，实际 %v", err)
	}

	// ② 全部被禁用 → disabled
	disabled := channelkeys.Result{Total: 2, Entries: []channelkeys.Entry{entryOf(0, "p0", "a"), entryOf(1, "p1", "b")}}
	off := false
	meta := channelkeys.MetaMap{"a": {Enabled: &off}, "b": {Enabled: &off}}
	_, err = selectUsable(chID, disabled, meta, nil)
	if !errors.As(err, &noKey) || noKey.Reason() != "disabled" || noKey.Disabled != 2 {
		t.Fatalf("全禁用应为 disabled，实际 %v", err)
	}

	// ③ 只剩一把可用但本次已试过 → ErrNoCandidate（换下一个渠道，不是配置事故）
	one := channelkeys.Result{Total: 1, Entries: []channelkeys.Entry{entryOf(0, "p0", "a")}}
	if _, err = selectUsable(chID, one, nil, map[string]struct{}{"a": {}}); !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("已试完应为 ErrNoCandidate，实际 %v", err)
	}

	// ④ 部分冷却 → 只返回未冷却的
	three := channelkeys.Result{Total: 3, Entries: []channelkeys.Entry{
		entryOf(0, "p0", "a"), entryOf(1, "p1", "b"), entryOf(2, "p2", "c")}}
	db.RDB.Set(ctx, coolKeyOf(chID, "b"), keyhealth.ReasonRateLimit, time.Minute)
	got, err := selectUsable(chID, three, nil, nil)
	if err != nil || len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
		t.Fatalf("应过滤掉冷却中的 b，实际 %v / %v", got, err)
	}

	// ⑤ 全部冷却（纯限流）→ AllKeysCoolingError 且带 Retry-After
	db.RDB.Set(ctx, coolKeyOf(chID, "a"), keyhealth.ReasonRateLimit, time.Minute)
	db.RDB.Set(ctx, coolKeyOf(chID, "c"), keyhealth.ReasonRateLimit, time.Minute)
	_, err = selectUsable(chID, three, nil, nil)
	var cooling *AllKeysCoolingError
	if !errors.As(err, &cooling) || cooling.Auth {
		t.Fatalf("纯限流冷却应为 AllKeysCoolingError{Auth:false}，实际 %v", err)
	}
	if cooling.RetryAfter <= 0 {
		t.Fatalf("应带剩余冷却时长，实际 %v", cooling.RetryAfter)
	}

	// ⑥ 其中一枚是鉴权失败 → Auth=true（对外必须 5xx，而不是「等几秒」）
	db.RDB.Set(ctx, coolKeyOf(chID, "c"), keyhealth.ReasonAuth, time.Minute)
	if _, err = selectUsable(chID, three, nil, nil); !errors.As(err, &cooling) || !cooling.Auth {
		t.Fatalf("含鉴权冷却时应标记 Auth，实际 %v", err)
	}

	// ⑦ 单 key 全冷却同样要拦：这是最需要保护账号的场景
	single := channelkeys.Result{Total: 1, Entries: []channelkeys.Entry{entryOf(0, "p0", "a")}}
	if _, err = selectUsable(chID, single, nil, nil); !errors.As(err, &cooling) {
		t.Fatalf("单 key 冷却时应报错，否则冷却形同虚设，实际 %v", err)
	}
}

// TestPickKeySkipsCoolingKey 断言冷却中的密钥不会被选中（E2 的核心收益）。
//
// 关键：冷却键用的是**明文的 keyID**，不是数组下标 —— 这条用例同时守住
// 「编辑保存/换序后冷却仍然跟着同一把 key 走」。
func TestPickKeySkipsCoolingKey(t *testing.T) {
	withRedis(t)
	t.Setenv("APP_ENCRYPTION_KEY", "unit-test-master-key")
	conf.Load()
	plains := []string{"sk-aaa-111", "sk-bbb-222", "sk-ccc-333"}
	raw, err := channelkeys.Encrypt(plains)
	if err != nil {
		t.Fatalf("加密密钥失败: %v", err)
	}
	ch := &model.Channel{ID: 91003, Name: "pick", Keys: raw, MultiKeyMode: "random", Status: 1}
	ctx := context.Background()
	ids := make([]string, len(plains))
	for i, p := range plains {
		ids[i] = channelkeys.ID([]byte(p))
	}
	t.Cleanup(func() {
		db.RDB.Del(ctx, coolKeyOf(ch.ID, ids[0]), coolKeyOf(ch.ID, ids[1]), coolKeyOf(ch.ID, ids[2]))
	})

	// 冷却前两把 → 30 次抽样必须全部落在第三把
	db.RDB.Set(ctx, coolKeyOf(ch.ID, ids[0]), keyhealth.ReasonRateLimit, time.Minute)
	db.RDB.Set(ctx, coolKeyOf(ch.ID, ids[1]), keyhealth.ReasonRateLimit, time.Minute)
	for i := 0; i < 30; i++ {
		ref, err := PickKey(ch, nil)
		if err != nil {
			t.Fatalf("PickKey 失败: %v", err)
		}
		if ref.Index != 2 || ref.Key != plains[2] || ref.ID != ids[2] {
			t.Fatalf("应只选中未冷却的第 3 把，实际 %+v", ref)
		}
	}

	// 全部冷却 → *AllKeysCoolingError
	db.RDB.Set(ctx, coolKeyOf(ch.ID, ids[2]), keyhealth.ReasonRateLimit, time.Minute)
	if _, err := PickKey(ch, nil); err == nil {
		t.Fatal("全部冷却时应返回错误")
	} else {
		var cooling *AllKeysCoolingError
		if !errors.As(err, &cooling) {
			t.Fatalf("应为 *AllKeysCoolingError，实际 %T", err)
		}
	}
}

// TestPickKeySurvivesReorder 是本轮改造的核心回归锁：换序后冷却仍跟着同一把 key。
//
// 改造前冷却键是数组下标，把 [A,B,C] 保存成 [C,A,B] 后，原本冷却 B 的那条键
// 会作用到 A 头上 —— 好 key 被雪藏、坏 key 继续打，且没有任何报错。
func TestPickKeySurvivesReorder(t *testing.T) {
	withRedis(t)
	t.Setenv("APP_ENCRYPTION_KEY", "unit-test-master-key")
	conf.Load()
	plains := []string{"sk-first", "sk-second"}
	raw, err := channelkeys.Encrypt(plains)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	ch := &model.Channel{ID: 91004, Name: "reorder", Keys: raw, MultiKeyMode: "random", Status: 1}
	ctx := context.Background()
	badID := channelkeys.ID([]byte(plains[0]))
	t.Cleanup(func() {
		db.RDB.Del(ctx, coolKeyOf(ch.ID, badID), coolKeyOf(ch.ID, channelkeys.ID([]byte(plains[1]))))
	})

	// 冷却第 1 把，然后把池子倒序保存（模拟运营编辑后重排）
	db.RDB.Set(ctx, coolKeyOf(ch.ID, badID), keyhealth.ReasonAuth, time.Minute)
	reversed, err := channelkeys.Encrypt([]string{plains[1], plains[0]})
	if err != nil {
		t.Fatalf("重新加密失败: %v", err)
	}
	ch.Keys = reversed

	for i := 0; i < 20; i++ {
		ref, err := PickKey(ch, nil)
		if err != nil {
			t.Fatalf("PickKey 失败: %v", err)
		}
		if ref.Key != plains[1] {
			t.Fatalf("换序后仍必须避开被冷却的那把，实际选中 %q", ref.Key)
		}
	}
}

// TestRedactUpstream 覆盖密钥/凭据/主机名的脱敏（P2-6：error_msg 会被门户用户看到）。
func TestRedactUpstream(t *testing.T) {
	ch := &model.Channel{BaseURL: "https://api.internal-proxy.example.com/v1"}
	key := "sk-live-1234567890abcdef"
	raw := `{"error":{"message":"upstream https://api.internal-proxy.example.com/v1/chat rejected key sk-live-1234567890abcdef; Authorization: Bearer abcdefghijklmnop; other sk-otherSECRET999"}}`
	got := redactUpstream(raw, ch, key)

	for _, leak := range []string{"sk-live-1234567890abcdef", "api.internal-proxy.example.com", "abcdefghijklmnop", "sk-otherSECRET999"} {
		if strings.Contains(got, leak) {
			t.Fatalf("脱敏后仍包含 %q：%s", leak, got)
		}
	}
	// 可读信息必须保留（用户要能自查「上下文超长」这类问题）
	if !strings.Contains(got, "rejected key") {
		t.Fatalf("脱敏不应抹掉可读错误信息：%s", got)
	}
	if strings.Count(got, "<redacted-key>") < 2 {
		t.Fatalf("密钥应被替换为占位符：%s", got)
	}
	// 空文本/无渠道不应 panic
	if redactUpstream("", ch, key) != "" || redactUpstream("x", nil, "") != "x" {
		t.Fatal("边界输入处理不符")
	}
}

// TestRedactUpstreamBody 断言回传给客户端的错误体同样脱敏，且未命中时不复制。
func TestRedactUpstreamText(t *testing.T) {
	ch := &model.Channel{BaseURL: "https://api.example.com"}
	clean := `{"error":{"message":"rate limited"}}`
	if got := redactUpstream(clean, ch, "sk-x"); got != clean {
		t.Fatalf("无敏感信息不应改写：%s", got)
	}
	dirty := `{"error":{"message":"bad key sk-abcdefgh12345"}}`
	if got := redactUpstream(dirty, ch, "sk-abcdefgh12345"); strings.Contains(got, "sk-abcdefgh12345") {
		t.Fatalf("应脱敏：%s", got)
	}
}

// TestReadRequestTooLarge 覆盖 P2-5：超限必须判定为超限（调用方据此返回 413），不能静默截断。
func TestReadRequestTooLarge(t *testing.T) {
	body, tooLarge, err := readRequest(strings.NewReader("0123456789"), 10)
	if err != nil || tooLarge || len(body) != 10 {
		t.Fatalf("恰好等于上限应通过：len=%d tooLarge=%v err=%v", len(body), tooLarge, err)
	}
	_, tooLarge, err = readRequest(strings.NewReader("0123456789A"), 10)
	if err != nil || !tooLarge {
		t.Fatalf("多 1 字节应判定超限：tooLarge=%v err=%v", tooLarge, err)
	}
}

// pluginErrStub 模拟插件适配器的 CallError（只实现 Class()，不引入 plugin 包依赖）。
type pluginErrStub struct{ kind string }

func (e pluginErrStub) Error() string { return "stub:" + e.kind }
func (e pluginErrStub) Class() string { return e.kind }

// TestClassifyErrorFromAdapter 断言「适配器自述的错误类别」被正确翻译成归因。
//
// 这张表决定了插件故障会不会熔断渠道：
//   - channel → 计渠道健康（真上游问题）
//   - rate_limited → 只冷却 key
//   - sensitive_input → 归调用方
//   - 其余（含空 kind / 未知 kind / busy / timeout / protocol）→ 内部错误，不动渠道健康
//
// 空 kind 归「内部」是刻意的：插件抛普通异常时最可能是插件自身 bug，
// 而旧实现会把它记为上游故障，调试一个插件就能把渠道熔断。
func TestClassifyErrorFromAdapter(t *testing.T) {
	cases := []struct {
		kind string
		want outcome
	}{
		{"channel", outcomeUpstream},
		{"rate_limited", outcomeRateLimit},
		{"sensitive_input", outcomeCaller},
		{"timeout", outcomeInternal},
		{"protocol", outcomeInternal},
		{"busy", outcomeInternal},
		{"", outcomeInternal},
		{"whatever_plugin_invented", outcomeInternal},
	}
	for _, c := range cases {
		if got := classifyError(pluginErrStub{kind: c.kind}); got != c.want {
			t.Errorf("kind=%q：归因应为 %v，实际 %v", c.kind, c.want, got)
		}
	}

	// 不带 Class() 的普通 error（内置适配器）必须保持旧语义：上游故障。
	if got := classifyError(errors.New("dial tcp: connection refused")); got != outcomeUpstream {
		t.Fatalf("普通 error 应归 upstream，实际 %v", got)
	}
}

// TestSyncWaitTimeoutIsNotChannelFault 守住「等不到不等于渠道坏」。
//
// 同步桥接等到上限是**我们的策略**：上游没错，只是还没跑完。
// 记成 upstream 会让「模型越慢，越容易熔断自己的渠道」——
// gpt-image 单张 540s，在阈值附近反复超时就能把一条完全健康的渠道打挂，
// 熔断后连同一渠道上的快模型也用不了。
func TestSyncWaitTimeoutIsNotChannelFault(t *testing.T) {
	err := &SyncWaitTimeoutError{Waited: 780 * time.Second, Detail: "上游任务号 x", TaskID: "x"}
	if got := classify(0, err, false); got != outcomeInternal {
		t.Fatalf("同步等待超时应归 internal（不影响渠道健康），实际 %v", got)
	}
	if got := classifyError(err); got != outcomeInternal {
		t.Fatalf("classifyError 同样应归 internal，实际 %v", got)
	}
}

// TestSyncAbandonedIsClientAbort 守住「用户关页面不算渠道故障」。
func TestSyncAbandonedIsClientAbort(t *testing.T) {
	err := &SyncAbandonedError{TaskID: "x", Err: context.Canceled}
	if got := classify(0, err, true); got != outcomeClientAbort {
		t.Fatalf("客户端断开应归 client_abort，实际 %v", got)
	}
}

// TestShouldSwitchKey 穷举「换 key 重试」的判据。
//
// 判据错一格的代价很具体：多换一次 = 多一次上游调用（可能多付钱）；
// 该换不换 = 一把废 key 拖死整条渠道。
func TestShouldSwitchKey(t *testing.T) {
	cases := []struct {
		name   string
		o      outcome
		status int
		on     bool
		budget int
		want   bool
	}{
		{"401 且开关开", outcomeAuth, 401, true, 3, true},
		{"429 且开关开", outcomeRateLimit, 429, true, 3, true},
		{"连接失败（无状态码）", outcomeUpstream, 0, true, 3, true},
		{"上游 5xx 不换（换也还是 5xx）", outcomeUpstream, 502, true, 3, false},
		{"调用方参数错不换", outcomeCaller, 400, true, 3, false},
		{"开关关闭一律不换", outcomeAuth, 401, false, 3, false},
		{"预算用尽不换", outcomeAuth, 401, true, 0, false},
		{"成功不换", outcomeSuccess, 200, true, 3, false},
	}
	for _, c := range cases {
		if got := shouldSwitchKey(c.o, c.status, c.on, c.budget); got != c.want {
			t.Errorf("%s：期望 %v 实际 %v", c.name, c.want, got)
		}
	}
}

// TestInsertKeyRetry 断言重试插在**当前位置之后**（不插队）且排除集合累积。
//
// 插到队尾会让低优先级渠道先被尝试，「优先级」这个配置就在故障路径上失效了 ——
// 而这恰好是运营最需要它生效的时刻。
func TestInsertKeyRetry(t *testing.T) {
	mk := func(id uint64) Candidate { return Candidate{Channel: &model.Channel{ID: id}} }
	q := []attempt{{cand: mk(1)}, {cand: mk(2)}, {cand: mk(3)}}

	out := insertKeyRetry(q, 0, q[0].cand, nil, "kid-a")
	if len(out) != 4 {
		t.Fatalf("应插入一条，实际 %d 条", len(out))
	}
	if out[1].cand.Channel.ID != 1 || out[2].cand.Channel.ID != 2 {
		t.Fatalf("重试必须紧邻当前项插入，实际顺序 %d,%d", out[1].cand.Channel.ID, out[2].cand.Channel.ID)
	}
	if _, ok := out[1].exclude["kid-a"]; !ok {
		t.Fatal("重试项必须排除已试过的 key")
	}

	// 第二次重试要在已有排除集合上累积，不能只剩最新那把
	out2 := insertKeyRetry(out, 1, out[1].cand, out[1].exclude, "kid-b")
	if len(out2[2].exclude) != 2 {
		t.Fatalf("排除集合应累积到 2 条，实际 %v", out2[2].exclude)
	}
	// 原队列的排除集合不被就地修改（避免已发生的尝试被"追溯"成试过）
	if len(out[1].exclude) != 1 {
		t.Fatalf("不得修改原切片里的 map，实际 %v", out[1].exclude)
	}
}
