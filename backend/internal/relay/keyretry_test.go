package relay

import (
	"errors"
	"testing"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/channelkeys"
)

// TestSwitchKeyRetryLoop 用真实的 PickKey + insertKeyRetry 走一遍「换 key 重试」的循环骨架。
//
// 为什么不去起一个假上游做全链路：转发主体（relay.go 主循环）依赖候选查询、
// 冻结额度、账单会话、适配器注册表，全部搭起来要几百行脚手架，而**能出错的地方
// 恰好不在那里** —— 出错的只会是这三件事：
//
//	① 第二次选到的是不是「另一把」；
//	② 队列有没有插队（低优先级渠道挤掉当前位置）；
//	③ 试完之后是不是干净地报 ErrNoCandidate（而不是把它当配置事故回 503）。
//
// 这三条在这里都能穷举，比端到端更可靠，也不会因为上游抖动而偶发红。
//
// 断言的是**结果集**而不是具体某一把：multi_key_mode=random 时首轮选中是随机的，
// 写死「先 A 后 B」的用例有一半概率在别人的机器上红。
func TestSwitchKeyRetryLoop(t *testing.T) {
	withRedis(t)
	t.Setenv("APP_ENCRYPTION_KEY", "unit-test-master-key")
	conf.Load()

	plains := []string{"sk-retry-aaa", "sk-retry-bbb"}
	raw, err := channelkeys.Encrypt(plains)
	if err != nil {
		t.Fatalf("加密密钥失败: %v", err)
	}
	ch := &model.Channel{
		ID: 91011, Name: "retry", Keys: raw, KeyMeta: nil,
		MultiKeyMode: "random", Status: 1, SwitchKeyRetry: true,
	}
	want := map[string]bool{}
	for _, p := range plains {
		want[channelkeys.ID([]byte(p))] = true
	}

	cand := Candidate{Channel: ch, UpstreamModel: "gpt-4o"}
	queue := []attempt{{cand: cand}}
	const budget = 2 // 换 key 预算：最多试两把

	tried := make([]KeyRef, 0, budget)
	for qi := 0; qi < len(queue) && len(tried) < budget; qi++ {
		ref, err := PickKey(queue[qi].cand.Channel, queue[qi].exclude)
		if err != nil {
			t.Fatalf("第 %d 轮选 key 失败: %v", qi+1, err)
		}
		tried = append(tried, ref)

		// 模拟第一把返回 401：鉴权失败必须换 key，且预算只按已试次数递减。
		if len(tried) == 1 {
			if !shouldSwitchKey(outcomeAuth, 401, ch.SwitchKeyRetry, budget-len(tried)) {
				t.Fatal("401 且开关打开时应换 key 重试")
			}
			queue = insertKeyRetry(queue, qi, queue[qi].cand, queue[qi].exclude, ref.ID)
		}
	}

	if len(tried) != budget {
		t.Fatalf("应恰好尝试 %d 把，实际 %d", budget, len(tried))
	}
	// ② 队列长度：原 1 项 + 插入 1 项，且插入项紧邻原位置。
	if len(queue) != 2 || queue[1].cand.Channel.ID != ch.ID {
		t.Fatalf("重试项未紧邻插入: %+v", queue)
	}
	// ① 第二把必须是另一把，且两把合起来正好覆盖整个池。
	if tried[0].ID == tried[1].ID {
		t.Fatalf("换 key 失败，两次选中同一把: %+v", tried)
	}
	for _, r := range tried {
		if !want[r.ID] {
			t.Fatalf("选中了池外的 key: %+v", r)
		}
	}
	// 首项的 exclude 不能被就地改写（否则「本渠道已试过什么」会随队列增长悄悄变形）。
	if len(queue[0].exclude) != 0 {
		t.Fatalf("原始尝试的 exclude 被就地修改: %#v", queue[0].exclude)
	}
	// 成功那把的 ID 就是要写进 usages.channel_key_id 的值 —— 归属取最后一轮。
	if !want[tried[1].ID] {
		t.Fatal("成功轮的 keyID 不属于本渠道")
	}

	// ③ 两把都试完之后必须报 ErrNoCandidate：调用方据此换下一个候选渠道，
	// 而不是把它算成「渠道配置全坏」对外回 503。
	exhausted := map[string]struct{}{tried[0].ID: {}, tried[1].ID: {}}
	if _, err := PickKey(ch, exhausted); !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("试完应返回 ErrNoCandidate，实际 %v", err)
	}
}

// TestSwitchKeyRetryOffKeepsSingleAttempt 开关关闭时行为必须与改造前逐字相同。
//
// 这条是「默认零行为差异」的守卫：新逻辑一旦漏在开关外面（比如恒为 true），
// 401 的请求会变成一次额外的上游调用，运营看到的是账单里 attempts 凭空变大。
func TestSwitchKeyRetryOffKeepsSingleAttempt(t *testing.T) {
	for _, c := range []struct {
		name   string
		o      outcome
		status int
	}{
		{"401", outcomeAuth, 401},
		{"429", outcomeRateLimit, 429},
		{"连接失败", outcomeUpstream, 0},
		{"上游 5xx", outcomeUpstream, 502},
	} {
		if shouldSwitchKey(c.o, c.status, false, 2) {
			t.Fatalf("%s 在开关关闭时不应换 key", c.name)
		}
	}
}
