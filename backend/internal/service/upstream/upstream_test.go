package upstream

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lingdong-api/internal/conf"
)

// ---------- 纯函数 ----------

func TestNormName(t *testing.T) {
	cases := map[string]string{
		"openai/gpt-4o": "gpt-4o",
		"gpt-4o":        "gpt-4o",
		"a/b/c":         "c",
	}
	for in, want := range cases {
		if got := normName(in); got != want {
			t.Fatalf("normName(%q)=%q，期望 %q", in, got, want)
		}
	}
}

func TestFold(t *testing.T) {
	// 折叠是"人能搜到"的前提：上游是 gpt-4o，人会输入 GPT 4o / gpt_4o / GPT.4O。
	for _, in := range []string{"GPT 4o", "gpt-4o", "gpt_4o", "GPT.4O", "gpt/4o"} {
		if got := fold(in); got != "gpt4o" {
			t.Fatalf("fold(%q)=%q，期望 gpt4o", in, got)
		}
	}
}

func TestConvertCost(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	p, free := convertCost(&devCost{Input: f(2.5), Output: f(10), CacheRead: f(1.25)})
	if free || p == nil || p.input == nil || *p.input != 2.5 || p.output == nil || *p.output != 10 || p.cache == nil || *p.cache != 1.25 {
		t.Fatalf("正常 cost 转换错误: %+v free=%v", p, free)
	}
	if p, free := convertCost(&devCost{Input: f(0)}); !free || p == nil || *p.input != 0 || p.output != nil {
		t.Fatalf("免费 cost 判定错误: %+v free=%v", p, free)
	}
	for name, c := range map[string]*devCost{
		"nil":      nil,
		"input缺失":  {Output: f(1)},
		"input负数":  {Input: f(-1)},
		"零输入有输出": {Input: f(0), Output: f(1)},
	} {
		if p, free := convertCost(c); p != nil || free {
			t.Fatalf("%s 应判为无有效单价，实际 %+v free=%v", name, p, free)
		}
	}
	p, _ = convertCost(&devCost{Input: f(1), Output: f(-2), CacheRead: f(-3)})
	if p.output != nil || p.cache != nil {
		t.Fatalf("负输出/负缓存应归零为 nil: %+v", p)
	}
}

func TestGuessModality(t *testing.T) {
	cases := map[string]string{
		"text-embedding-3-large": "embedding",
		"bge-reranker-v2":        "rerank",
		"llama-guard-4":          "moderation",
		"whisper-1":              "audio",
		"sora-2":                 "video",
		"dall-e-3":               "image",
		"gpt-4o":                 "chat",
	}
	for in, want := range cases {
		if got := guessModality(in); got != want {
			t.Fatalf("guessModality(%q)=%q，期望 %q", in, got, want)
		}
	}
}

// ---------- 搜索 ----------

func f(v float64) *float64 { return &v }

// fixture 用 buildEntries 造索引：顺带覆盖"压平 + 名称回落 + 模态推断"。
func fixture(providers map[string]devProvider) []entry { return buildEntries(providers) }

func searchUSD(t *testing.T, entries []entry, keyword, vendor string) ([]Item, int, int, string) {
	t.Helper()
	return searchIn(entries, keyword, vendor, conf.CurrencyUSD, 1, searchLimit)
}

func TestBuildEntriesUsesUpstreamNameAndFallsBackToKey(t *testing.T) {
	entries := fixture(map[string]devProvider{
		"openai": {ID: "openai", Name: "OpenAI", Models: map[string]devModel{
			"gpt-4o": {Name: "GPT-4o", Cost: &devCost{Input: f(2.5)}},
			"o3":     {Cost: &devCost{Input: f(1)}},
		}},
	})
	byKey := map[string]entry{}
	for _, e := range entries {
		byKey[e.Key] = e
	}
	if byKey["gpt-4o"].Name != "GPT-4o" {
		t.Fatalf("有 name 时应取 name，实际 %q", byKey["gpt-4o"].Name)
	}
	if byKey["o3"].Name != "o3" {
		t.Fatalf("无 name 时应回落键名，实际 %q", byKey["o3"].Name)
	}
	if byKey["o3"].ProviderName != "OpenAI" {
		t.Fatalf("来源名应取 provider.name，实际 %q", byKey["o3"].ProviderName)
	}
}

func TestSearchExactBeforeDateSnapshot(t *testing.T) {
	entries := fixture(map[string]devProvider{
		"openai": {ID: "openai", Name: "OpenAI", Models: map[string]devModel{
			"gpt-4o":            {Cost: &devCost{Input: f(2.5)}},
			"gpt-4o-2024-08-06": {Cost: &devCost{Input: f(2.5)}},
			"gpt-4o-mini":       {Cost: &devCost{Input: f(0.15)}},
		}},
	})
	items, total, withPrice, _ := searchUSD(t, entries, "gpt-4o", "")
	if total != 3 || withPrice != 3 {
		t.Fatalf("total=%d with_price=%d，期望 3/3", total, withPrice)
	}
	if items[0].Name != "gpt-4o" {
		t.Fatalf("精确命中必须排第一，实际首条 %q（%+v）", items[0].Name, items)
	}
}

func TestSearchFoldMatchesHumanSpelling(t *testing.T) {
	entries := fixture(map[string]devProvider{
		"openai": {ID: "openai", Name: "OpenAI", Models: map[string]devModel{
			"gpt-4o": {Cost: &devCost{Input: f(2.5)}},
		}},
	})
	for _, kw := range []string{"GPT 4o", "gpt_4o", " gpt-4o "} {
		items, _, _, _ := searchUSD(t, entries, kw, "")
		if len(items) != 1 || items[0].Name != "gpt-4o" {
			t.Fatalf("关键词 %q 应命中 gpt-4o，实际 %+v", kw, items)
		}
	}
}

func TestSearchDedupesAcrossProviders(t *testing.T) {
	entries := fixture(map[string]devProvider{
		"openai":     {ID: "openai", Name: "OpenAI", Models: map[string]devModel{"gpt-4o": {Cost: &devCost{Input: f(2.5)}}}},
		"azure":      {ID: "azure", Name: "Azure", Models: map[string]devModel{"gpt-4o": {Cost: &devCost{Input: f(2.8)}}}},
		"openrouter": {ID: "openrouter", Name: "OpenRouter", Models: map[string]devModel{"gpt-4o": {Cost: &devCost{Input: f(3)}}}},
	})
	items, total, withPrice, _ := searchUSD(t, entries, "gpt-4o", "")
	if total != 1 || withPrice != 1 || len(items) != 1 {
		t.Fatalf("同名跨来源应去重为 1 组，实际 total=%d with=%d items=%d", total, withPrice, len(items))
	}
	if items[0].ProviderID != "openai" {
		t.Fatalf("无 vendor 偏好时应取 openai，实际 %q", items[0].ProviderID)
	}
	if items[0].ProviderCount != 3 {
		t.Fatalf("provider_count 应为 3（如实告知另有报价），实际 %d", items[0].ProviderCount)
	}

	// 传了 vendor 就优先自己的供应商
	items, _, _, _ = searchUSD(t, entries, "gpt-4o", "azure")
	if items[0].ProviderID != "azure" {
		t.Fatalf("vendor=azure 时应取 azure，实际 %q", items[0].ProviderID)
	}
}

func TestSearchPrefersLiveOverDeprecated(t *testing.T) {
	entries := fixture(map[string]devProvider{
		"openai": {ID: "openai", Name: "OpenAI", Models: map[string]devModel{
			"legacy-model": {Cost: &devCost{Input: f(1)}, Status: "deprecated"},
		}},
		"azure": {ID: "azure", Name: "Azure", Models: map[string]devModel{
			"legacy-model": {Cost: &devCost{Input: f(2)}},
		}},
	})
	items, _, _, _ := searchUSD(t, entries, "legacy-model", "")
	if len(items) != 1 || items[0].Deprecated {
		t.Fatalf("同名停售与非停售并存时应选非停售，实际 %+v", items)
	}
}

func TestSearchCountsNoPriceButDoesNotReturnIt(t *testing.T) {
	entries := fixture(map[string]devProvider{
		"zhipu": {ID: "zhipu", Name: "Zhipu", Models: map[string]devModel{
			"glm-5":     {Cost: &devCost{Input: f(0.6)}},
			"glm-5-air": {}, // 上游收录但没报价
		}},
	})
	items, total, withPrice, _ := searchUSD(t, entries, "glm-5", "")
	if total != 2 {
		t.Fatalf("total 应把无价条目算进去（区分「没收录」与「没报价」），实际 %d", total)
	}
	if withPrice != 1 {
		t.Fatalf("with_price 应为 1，实际 %d", withPrice)
	}
	if len(items) != 1 || items[0].Name != "glm-5" {
		t.Fatalf("无价条目不应出现在 items，实际 %+v", items)
	}
}

func TestSearchKeepsFreeButKeepsMicroZero(t *testing.T) {
	entries := fixture(map[string]devProvider{
		"openai": {ID: "openai", Name: "OpenAI", Models: map[string]devModel{
			"free-model": {Cost: &devCost{Input: f(0)}},
		}},
	})
	items, _, _, _ := searchUSD(t, entries, "free-model", "")
	if len(items) != 1 || items[0].InputUSD == nil || *items[0].InputUSD != 0 {
		t.Fatalf("免费条目应保留并给出 0 价（面板据此禁用填入），实际 %+v", items)
	}
	if items[0].InputMicro == nil || *items[0].InputMicro != 0 {
		t.Fatalf("0 美元应折算成 0 微，实际 %v", items[0].InputMicro)
	}
}

func TestSearchLimitAndTotals(t *testing.T) {
	models := map[string]devModel{}
	for i := 0; i < 30; i++ {
		models["demo-"+string(rune('a'+i%26))+string(rune('0'+i/26))] = devModel{Cost: &devCost{Input: f(1)}}
	}
	entries := fixture(map[string]devProvider{"acme": {ID: "acme", Name: "Acme", Models: models}})
	items, total, withPrice, _ := searchUSD(t, entries, "demo", "")
	if total != 30 || withPrice != 30 {
		t.Fatalf("total/with_price 应为 30/30，实际 %d/%d", total, withPrice)
	}
	if len(items) != searchLimit {
		t.Fatalf("返回条数应被截断到 %d，实际 %d", searchLimit, len(items))
	}
}

func TestSearchSuffixFallback(t *testing.T) {
	entries := fixture(map[string]devProvider{
		"openai": {ID: "openai", Name: "OpenAI", Models: map[string]devModel{
			"gpt-4o": {Cost: &devCost{Input: f(2.5)}},
		}},
	})
	items, _, _, matched := searchUSD(t, entries, "gpt-4o-2024-08-06", "")
	if len(items) != 1 || items[0].Name != "gpt-4o" {
		t.Fatalf("应剥离日期后缀后命中 gpt-4o，实际 %+v", items)
	}
	if matched != "gpt-4o" {
		t.Fatalf("matched_keyword 应回传实际关键词，实际 %q", matched)
	}
}

func TestSearchDoesNotFuzzyMatchUnrelated(t *testing.T) {
	entries := fixture(map[string]devProvider{
		"openai": {ID: "openai", Name: "OpenAI", Models: map[string]devModel{
			"gpt-4o": {Cost: &devCost{Input: f(2.5)}},
		}},
	})
	items, total, _, _ := searchUSD(t, entries, "claude", "")
	if total != 0 || len(items) != 0 {
		t.Fatalf("无关关键词不应命中，实际 total=%d items=%+v", total, items)
	}
	if items == nil {
		t.Fatal("items 必须是空切片而不是 nil（前端序列化成 []）")
	}
}

func TestConvertMicro(t *testing.T) {
	restore := conf.SetBillingForTest(conf.CurrencyCNY, 7.2)
	defer restore()

	if got := toMicro(f(2.5), conf.CurrencyCNY, 7.2); got == nil || *got != 18_000_000 {
		t.Fatalf("$2.5 × 7.2 应为 18_000_000 微，实际 %v", got)
	}
	if got := toMicro(f(2.5), conf.CurrencyUSD, 1); got == nil || *got != 2_500_000 {
		t.Fatalf("美元站不应折算，实际 %v", got)
	}
	// 极小美元单价：四舍五入到 0 会变成"显式免费"，必须钳到 1 微。
	if got := toMicro(f(1e-9), conf.CurrencyCNY, 7.2); got == nil || *got != 1 {
		t.Fatalf("极小单价应钳到 1 微，实际 %v", got)
	}
	if got := toMicro(f(0), conf.CurrencyCNY, 7.2); got == nil || *got != 0 {
		t.Fatalf("0 美元应保持 0 微，实际 %v", got)
	}
	if got := toMicro(nil, conf.CurrencyCNY, 7.2); got != nil {
		t.Fatalf("缺价应为 nil，实际 %v", got)
	}
	// 超过 money.MaxAmount：返回 nil 而不是截断（截断值同样是错价）。
	big := 1e12
	if got := toMicro(&big, conf.CurrencyCNY, 7.2); got != nil {
		t.Fatalf("超上限应为 nil，实际 %v", got)
	}
}

// ---------- 缓存 / 单飞 / 降级 ----------

// setFetcher 只替换拉取函数，**不**清缓存：硬过期用例需要"先成功后失败"的连续状态。
// 每个缓存用例自己在开头 resetCacheForTest()。
func setFetcher(t *testing.T, fn func(context.Context) ([]entry, error)) {
	t.Helper()
	prev := fetchIndex
	fetchIndex = fn
	t.Cleanup(func() {
		fetchIndex = prev
		resetCacheForTest()
	})
}

func sampleEntries() []entry {
	return fixture(map[string]devProvider{
		"openai": {ID: "openai", Name: "OpenAI", Models: map[string]devModel{
			"gpt-4o": {Cost: &devCost{Input: f(2.5)}},
		}},
	})
}

func TestCacheHitsWithinTTL(t *testing.T) {
	resetCacheForTest()
	var calls atomic.Int32
	setFetcher(t, func(context.Context) ([]entry, error) {
		calls.Add(1)
		return sampleEntries(), nil
	})
	for i := 0; i < 3; i++ {
		if _, _, err := index(context.Background()); err != nil {
			t.Fatalf("第 %d 次取快照失败: %v", i+1, err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("TTL 内应只拉一次，实际 %d 次", got)
	}
}

func TestCacheStaleOnRefreshFailure(t *testing.T) {
	resetCacheForTest()
	var fail atomic.Bool
	setFetcher(t, func(context.Context) ([]entry, error) {
		if fail.Load() {
			return nil, errors.New("boom")
		}
		return sampleEntries(), nil
	})
	if _, _, err := index(context.Background()); err != nil {
		t.Fatalf("首次拉取失败: %v", err)
	}
	// 把快照做成"过期但未硬过期"，再让拉取失败
	cacheMu.Lock()
	cached.fetchedAt = time.Now().Add(-cacheTTL - time.Minute)
	cacheMu.Unlock()
	fail.Store(true)

	snap, stale, err := index(context.Background())
	if err != nil {
		t.Fatalf("刷新失败但有旧快照时应返回旧数据: %v", err)
	}
	if !stale || snap == nil || len(snap.entries) == 0 {
		t.Fatalf("应标记 stale 并返回旧快照，实际 stale=%v snap=%+v", stale, snap)
	}
}

func TestCacheCooldownSkipsUpstream(t *testing.T) {
	resetCacheForTest()
	var calls atomic.Int32
	setFetcher(t, func(context.Context) ([]entry, error) {
		calls.Add(1)
		return nil, errors.New("boom")
	})
	if _, _, err := index(context.Background()); err == nil {
		t.Fatal("首次失败（无快照）应报错")
	}
	before := calls.Load()
	for i := 0; i < 5; i++ {
		if _, _, err := index(context.Background()); !errors.Is(err, ErrSourceUnavailable) {
			t.Fatalf("冷却期内无快照应返回 ErrSourceUnavailable，实际 %v", err)
		}
	}
	if got := calls.Load(); got != before {
		t.Fatalf("冷却期内不应再打上游，实际又拉了 %d 次", got-before)
	}
}

func TestCacheHardStaleStopsServing(t *testing.T) {
	resetCacheForTest()
	setFetcher(t, func(context.Context) ([]entry, error) { return sampleEntries(), nil })
	if _, _, err := index(context.Background()); err != nil {
		t.Fatalf("首次拉取失败: %v", err)
	}
	cacheMu.Lock()
	cached.fetchedAt = time.Now().Add(-hardStale - time.Minute)
	cacheMu.Unlock()

	setFetcher(t, func(context.Context) ([]entry, error) { return nil, errors.New("boom") })
	if _, _, err := index(context.Background()); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("硬过期后刷新失败应报错，实际 %v", err)
	}
}

func TestCacheSingleflight(t *testing.T) {
	resetCacheForTest()
	release := make(chan struct{})
	var calls atomic.Int32
	setFetcher(t, func(context.Context) ([]entry, error) {
		calls.Add(1)
		<-release
		return sampleEntries(), nil
	})

	var wg sync.WaitGroup
	errs := make([]error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = index(context.Background())
		}(i)
	}
	// 等所有 goroutine 进入等待，再放行
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个等待者失败: %v", i, err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("并发应只拉一次，实际 %d 次", got)
	}
}

// 共享拉取不能挂在首个调用方的 ctx 上：他关掉面板不能把其它等待者一起打挂。
func TestCacheIgnoresCallerCancel(t *testing.T) {
	resetCacheForTest()
	release := make(chan struct{})
	setFetcher(t, func(ctx context.Context) ([]entry, error) {
		<-release
		return sampleEntries(), nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, _, err := index(ctx); done <- err }()

	time.Sleep(20 * time.Millisecond)
	cancel()
	close(release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("调用方取消不应拖垮共享拉取，实际 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("共享拉取被调用方取消后卡死")
	}
}

// ---------- 真实上游（默认跳过） ----------

func TestFetchModelsDevLive(t *testing.T) {
	if os.Getenv("LD_LIVE_UPSTREAM") != "1" {
		t.Skip("未设置 LD_LIVE_UPSTREAM=1，跳过真实上游拉取用例")
	}
	entries, err := fetchAndIndex(context.Background())
	if err != nil {
		t.Fatalf("真实拉取失败: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("真实拉取返回空索引")
	}
	found := false
	for _, e := range entries {
		if e.ProviderID == "openai" && e.Name == "gpt-4o" && e.Price != nil && e.Price.input != nil {
			found = true
			t.Logf("openai/gpt-4o input=%v USD/1M，索引条目 %d", *e.Price.input, len(entries))
		}
	}
	if !found {
		t.Fatal("真实索引里没有 openai/gpt-4o 的有效价目")
	}
}
