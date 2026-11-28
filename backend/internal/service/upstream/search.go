package upstream

import (
	"context"
	"errors"
	"math"
	"regexp"
	"sort"
	"strings"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/pkg/money"
)

// searchLimit 是一次返回的条数上限。
//
// 面板是"查某个模型"，不是浏览目录：20 条足够覆盖一个家族（gpt-4o / -mini / -2024-xx），
// 又不会把 760px 的面板撑成目录页。上限只在这里定义，前端不再传 limit。
const searchLimit = 20

// ErrKeywordRequired 表示没有可查的关键词（handler 会先拦，这里是服务层的兜底）。
var ErrKeywordRequired = errors.New("upstream keyword required")

// Item 是一条上游参考价。
//
// 双量纲是刻意的：*_usd 是上游挂牌价（可对账），*_micro 是按当前站点汇率折算后的本地量纲
// （可直接填进 models.price_*）。两者都可为 null —— null 表示上游没报这一档。
type Item struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	ProviderID    string   `json:"provider_id"`
	ProviderName  string   `json:"provider_name"`
	Modality      string   `json:"modality"`
	Deprecated    bool     `json:"deprecated"`
	ProviderCount int      `json:"provider_count"`
	InputUSD      *float64 `json:"input_usd"`
	OutputUSD     *float64 `json:"output_usd"`
	CacheUSD      *float64 `json:"cache_usd"`
	InputMicro    *int64   `json:"input_micro"`
	OutputMicro   *int64   `json:"output_micro"`
	CacheMicro    *int64   `json:"cache_micro"`
}

// Meta 描述本次结果的来源与口径。
//
// 只放"UI 真的会渲染"的字段：来源名、拉取时刻、是否过期、实际换算用的币种与汇率、
// 实际命中的关键词、命中总数与其中有报价的数量。
type Meta struct {
	Source         string `json:"source"`
	FetchedAt      string `json:"fetched_at"`
	Stale          bool   `json:"stale"`
	Currency       string `json:"currency"`
	FxRate         float64 `json:"fx_rate"`
	MatchedKeyword string `json:"matched_keyword"`
	Total          int    `json:"total"`
	WithPrice      int    `json:"with_price"`
}

// Result 是 GET /admin/upstream/search 的 data 部分。
type Result struct {
	Items []Item `json:"items"`
	Meta  Meta   `json:"meta"`
}

// suffixRe 用于"关键词没命中时剥一次日期/版本后缀再试"。
//
// 只在 total==0 时用，且只剥一次：gpt-4o 与 gpt-4o-2024-08-06 是两个不同价目，
// 绝不能把日期快照当成同名模型的默认答案（那会把一个错价填进表单）。
var suffixRe = regexp.MustCompile(`-(latest|preview|\d{4}-\d{2}-\d{2}|\d{4})$`)

// Search 按关键词查上游参考价。
func Search(ctx context.Context, keyword, vendor string) (Result, error) {
	kw := strings.TrimSpace(keyword)
	if kw == "" {
		return Result{}, ErrKeywordRequired
	}
	snap, stale, err := index(ctx)
	if err != nil {
		return Result{}, err
	}
	// 币种与汇率与后端换算同源（conf 读 system_config，热更新）；
	// 折算在本次调用内完成，不进快照 —— 否则改汇率后 10 分钟内数字与标注不符。
	currency := conf.SiteCurrency()
	fx := 1.0
	if currency != conf.CurrencyUSD {
		fx = conf.CnyPerUsd()
	}
	items, total, withPrice, matched := searchIn(snap.entries, kw, vendor, currency, fx, searchLimit)
	return Result{
		Items: items,
		Meta: Meta{
			Source:         sourceName,
			FetchedAt:      snap.fetchedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			Stale:          stale,
			Currency:       currency,
			FxRate:         fx,
			MatchedKeyword: matched,
			Total:          total,
			WithPrice:      withPrice,
		},
	}, nil
}

// hit 是"分组代表 + 匹配强度 + 同名来源数"，排序用它，转成 Item 后丢弃。
type hit struct {
	entry     entry
	rank      int
	providers int
}

// searchIn 是搜索的纯函数部分（无 IO，便于表驱动测试）。
//
// 顺序是定死的，别调换：
//  1. 匹配打分（fold 之后：精确 > 前缀 > 子串 > 键子串）；
//  2. 按折叠名分组（同名跨来源去重）；
//  3. 组内选代表（有报价 > 非停售 > 供应商偏好 > 输入价低 > 来源 id）；
//  4. 统计 total / with_price（都按"组"计，含有价与无价）；
//  5. 排序 + 截断。
//
// 先截断再统计会让"共 37 个匹配"变成"20 个"；先排序再去重会让 provider_count 丢失。
func searchIn(entries []entry, keyword, vendor, currency string, fx float64, limit int) (items []Item, total, withPrice int, matched string) {
	matched = keyword
	groups, ranks := collect(entries, keyword, vendor)
	if len(groups) == 0 {
		if stripped := strings.TrimSpace(suffixRe.ReplaceAllString(strings.ToLower(keyword), "")); stripped != "" && stripped != strings.ToLower(keyword) {
			if g2, r2 := collect(entries, stripped, vendor); len(g2) > 0 {
				groups, ranks, matched = g2, r2, stripped
			}
		}
	}

	total = len(groups)
	chosen := make([]hit, 0, len(groups))
	for nameKey, group := range groups {
		best := group[0]
		if best.Price != nil {
			withPrice++
		}
		chosen = append(chosen, hit{
			entry:     best,
			rank:      ranks[nameKey],
			providers: uniqueProviders(group),
		})
	}
	// 结果顺序：匹配强度 → 有价优先 → 非停售 → 名字短的在前 → 字典序。
	// 用 SliceStable + 完整比较键：同一请求两次必须给同一顺序（e2e 与人工核对都依赖）。
	sort.SliceStable(chosen, func(i, j int) bool {
		a, b := chosen[i], chosen[j]
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		ah, bh := a.entry.Price != nil, b.entry.Price != nil
		if ah != bh {
			return ah
		}
		if a.entry.Deprecated != b.entry.Deprecated {
			return !a.entry.Deprecated
		}
		if len(a.entry.Name) != len(b.entry.Name) {
			return len(a.entry.Name) < len(b.entry.Name)
		}
		return a.entry.Name < b.entry.Name
	})

	items = make([]Item, 0, min(len(chosen), limit))
	for _, h := range chosen {
		if len(items) >= limit {
			break
		}
		// 上游没报这一档的条目不进结果（面板里一行空白价没有任何用处），
		// 但它已经计入 total：这样 UI 能区分「查不到」与「收录了但没报价」。
		if h.entry.Price == nil {
			continue
		}
		it := toItem(h.entry, currency, fx)
		it.ProviderCount = h.providers
		items = append(items, it)
	}
	return items, total, withPrice, matched
}

// collect 匹配 + 分组 + 组内选代表。
func collect(entries []entry, keyword, vendor string) (map[string][]entry, map[string]int) {
	kwFold := fold(keyword)
	kwRaw := strings.ToLower(strings.TrimSpace(keyword))
	if kwFold == "" {
		// 关键词全是分隔符（如 "-"）：退回原串匹配，否则空前缀会把全库都算命中。
		kwFold = kwRaw
	}
	groups := make(map[string][]entry, 16)
	ranks := make(map[string]int, 16)
	for _, e := range entries {
		r := rankOf(e, kwFold, kwRaw)
		if r < 0 {
			continue
		}
		key := e.Fold
		if key == "" {
			key = strings.ToLower(e.Name)
		}
		if _, seen := ranks[key]; !seen {
			ranks[key] = r
		}
		if ranks[key] > r {
			ranks[key] = r
		}
		groups[key] = append(groups[key], e)
	}
	for key := range groups {
		g := groups[key]
		sort.SliceStable(g, func(i, j int) bool { return betterEntry(g[i], g[j], vendor) })
		groups[key] = g
	}
	return groups, ranks
}

// rankOf 返回匹配强度：0 精确 / 1 前缀 / 2 名称子串 / 3 键子串，-1 不匹配。
func rankOf(e entry, kwFold, kwRaw string) int {
	switch {
	case e.Fold == kwFold:
		return 0
	case strings.HasPrefix(e.Fold, kwFold):
		return 1
	case strings.Contains(e.Fold, kwFold):
		return 2
	case strings.Contains(e.KeyFold, kwFold), strings.Contains(strings.ToLower(e.Key), kwRaw):
		return 3
	}
	return -1
}

// betterEntry 判断 a 是否比 b 更适合代表这个模型去报价。
//
// 顺序有讲究：先"有没有报价"（没报价的行留着没用），再"是否停售"（停售价不可成交），
// 再看是不是你的供应商，最后才比价 —— 比价放前面会让"某个小供应商报 0.1 美元"顶掉正规报价。
func betterEntry(a, b entry, vendor string) bool {
	ah, bh := a.Price != nil, b.Price != nil
	if ah != bh {
		return ah
	}
	if a.Deprecated != b.Deprecated {
		return !a.Deprecated
	}
	ra := providerRank(a, vendor)
	rb := providerRank(b, vendor)
	if ra != rb {
		return ra < rb
	}
	ai, bi := inputOrMax(a), inputOrMax(b)
	if ai != bi {
		return ai < bi
	}
	if a.ProviderID != b.ProviderID {
		return a.ProviderID < b.ProviderID
	}
	return a.Key < b.Key
}

// providerRank：0 = 你自己的供应商（表单里的 vendor），1 = openai（同名模型最常见的挂牌方），2 = 其它。
func providerRank(e entry, vendor string) int {
	v := strings.TrimSpace(vendor)
	if v != "" && (strings.EqualFold(e.ProviderID, v) || strings.EqualFold(e.ProviderName, v)) {
		return 0
	}
	if strings.EqualFold(e.ProviderID, "openai") {
		return 1
	}
	return 2
}

func inputOrMax(e entry) float64 {
	if e.Price == nil || e.Price.input == nil {
		return math.MaxFloat64
	}
	return *e.Price.input
}

// toItem 把索引项转成响应项：原样带上 USD 价，并把三价折算成本地微。
func toItem(e entry, currency string, fx float64) Item {
	it := Item{
		ID:            e.Key,
		Name:          e.Name,
		ProviderID:    e.ProviderID,
		ProviderName:  e.ProviderName,
		Modality:      e.Modality,
		Deprecated:    e.Deprecated,
		ProviderCount: 1,
	}
	if e.Price != nil {
		it.InputUSD = e.Price.input
		it.OutputUSD = e.Price.output
		it.CacheUSD = e.Price.cache
		it.InputMicro = toMicro(e.Price.input, currency, fx)
		it.OutputMicro = toMicro(e.Price.output, currency, fx)
		it.CacheMicro = toMicro(e.Price.cache, currency, fx)
	}
	return it
}

// uniqueProviders 数「这个名字在多少家来源有报价」。
//
// 面板要显示「另有 N 家」，否则用户会以为只有一家挂牌 —— 而实际上同一个 gpt-4o
// 在十几家来源都有，价格还各不相同。选谁报价由 betterEntry 决定，这里只负责如实报数。
func uniqueProviders(group []entry) int {
	seen := make(map[string]struct{}, len(group))
	for _, e := range group {
		seen[e.ProviderID] = struct{}{}
	}
	return len(seen)
}

// toMicro 把 USD / 1M 折算成本地「微 / 1M」。
//
// 三个必须守住的边界：
//  1. usd>0 但四舍五入到 0 微时钳到 1 —— 本地 0 微的语义是"显式免费"，
//     一次填入就把付费模型变成免费，是最坏的一类事故；
//  2. NaN/Inf 一律 null，不要造出一个"看起来正常"的数；
//  3. 超过 money.MaxAmount 返回 null 而不是截断 —— 截断值同样是错价。
func toMicro(usd *float64, currency string, fx float64) *int64 {
	if usd == nil {
		return nil
	}
	v := *usd
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return nil
	}
	if currency != conf.CurrencyUSD {
		if fx <= 0 {
			return nil
		}
		v *= fx
	}
	micro := math.Round(v * float64(money.Scale))
	if math.IsNaN(micro) || math.IsInf(micro, 0) || micro > float64(money.MaxAmount) {
		return nil
	}
	if *usd > 0 && micro < 1 {
		micro = 1
	}
	out := int64(micro)
	return &out
}

