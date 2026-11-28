// Package upstream 提供「上游模型参考价」查询（models.dev 公开价目）。
//
// 入口只有一条：GET /api/admin/upstream/search?keyword=&vendor=（只读、不写库、不审计）。
// 它回答的是「这个模型在上游挂牌多少钱」，**不是**这条渠道的结算价 —— 渠道可能有折扣、
// 可能按人民币结算、可能是逆向代理。这个区别必须由调用方（管理台面板）如实展示。
//
// 数据口径（三个量纲，别混）：
//   - 上游 cost 一律 USD / 1M tokens（models.dev 的原始口径）；
//   - 本平台结算价是「微（1e-6 结算币种）/ 1M tokens」；
//   - 两者之间隔着站点币种与汇率，换算只在 search.go 的 toMicro 一处发生。
//
// 历史包袱：这里曾是「全量拉取 → 与本地 models 做 diff」的预览包（G14），
// 那条链路没有任何回填通道、又要求读库，已整体删除；现在不依赖 DB。
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"lingdong-api/internal/pkg/ssrf"
)

// ErrSourceUnavailable 表示上游价目当前拿不到（无可用快照且本轮拉取失败/冷却中）。
var ErrSourceUnavailable = errors.New("upstream source unavailable")

const (
	sourceName   = "models.dev"
	modelsDevURL = "https://models.dev/api.json"
	// fetchTimeout 是单次上游拉取超时。价目表约 7MB，30s 是宽松上限。
	fetchTimeout = 30 * time.Second
	// maxPayloadBytes 是拉取体积上限：实测约 6.8MB，留 4 倍余量防内存放大。
	maxPayloadBytes = 32 << 20
	// defaultModality 是名称无法归类时的兜底模态。
	defaultModality = "chat"
)

// devCost 是 models.dev 的 cost 结构（实测 {"input":2.5,"output":10,"cache_read":1.25}，USD/1M）。
type devCost struct {
	Input     *float64 `json:"input"`
	Output    *float64 `json:"output"`
	CacheRead *float64 `json:"cache_read"`
}

type devModel struct {
	// Name 是人类可读名（部分来源提供，如 "GLM-5.3-Flash"）。缺省时回落键名。
	Name   string   `json:"name"`
	Cost   *devCost `json:"cost"`
	Status string   `json:"status"` // 实测线上多数没有该字段；mock 数据用 deprecated 演示停售
}

type devProvider struct {
	ID     string              `json:"id"`
	Name   string              `json:"name"`
	Models map[string]devModel `json:"models"`
}

// priceTriple 是折算成本地量纲之前的上游三价（USD / 1M）。nil = 该档未报价。
type priceTriple struct {
	input  *float64
	output *float64
	cache  *float64
}

// entry 是压平后的索引项。
//
// 为什么不在缓存里留原始 map：models.dev 有 300+ 来源、约 1 万模型，原始 JSON 含
// description/limits/modalities 等几十个字段（解析后常驻内存远大于 1MB）。
// 搜索只需要下面这些，压平一次、常驻一份。
type entry struct {
	Key          string // 上游原始键（如 openai/gpt-4o）
	Name         string // 展示名：上游 name 优先，否则取键的最后一段
	Fold         string // Name 的折叠形式（小写 + 去分隔符），匹配用
	KeyFold      string // Key 的折叠形式
	ProviderID   string
	ProviderName string
	Modality     string
	Deprecated   bool
	Price        *priceTriple // nil = 上游没给有效单价（仍保留：用来区分「未收录」与「没报价」）
}

// snapshot 是一次成功拉取的结果。只存 USD 价：汇率是热更新配置，
// 若把折算结果算进快照，改汇率后 10 分钟内面板的数字会与标注的汇率对不上。
type snapshot struct {
	entries   []entry
	fetchedAt time.Time
}

// fetchIndex 是拉取的唯一入口，测试可替换（不连公网）。
var fetchIndex = fetchAndIndex

// fetchAndIndex 拉取全量价目并压平成索引（SSRF 校验 + 超时 + 体积上限）。
func fetchAndIndex(ctx context.Context) ([]entry, error) {
	if err := ssrf.ValidatePublicURLResolved(ctx, modelsDevURL); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsDevURL, nil)
	if err != nil {
		return nil, err
	}
	// 没有 UA 的自动化请求更容易被 CDN 拦；带上便于上游定位来源。
	req.Header.Set("User-Agent", "lingdong-api/upstream-price")
	resp, err := ssrf.SafeClient(fetchTimeout).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("上游返回 HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxPayloadBytes))
	if err != nil {
		return nil, err
	}
	var providers map[string]devProvider
	if err := json.Unmarshal(raw, &providers); err != nil {
		return nil, fmt.Errorf("上游响应解析失败: %w", err)
	}
	if len(providers) == 0 {
		return nil, errors.New("上游返回空数据集")
	}
	return buildEntries(providers), nil
}

// buildEntries 把 models.dev 的嵌套结构压平成索引。
//
// 顺序必须确定（来源 id 排序、模型键排序）：搜索结果的排序在分数相同时要可复现，
// 否则同一个请求两次返回不同顺序，e2e 与人工核对都会被"抖动"吃掉。
func buildEntries(providers map[string]devProvider) []entry {
	provIDs := make([]string, 0, len(providers))
	for id := range providers {
		provIDs = append(provIDs, id)
	}
	sort.Strings(provIDs)

	out := make([]entry, 0, 1024)
	for _, pid := range provIDs {
		p := providers[pid]
		keys := make([]string, 0, len(p.Models))
		for k := range p.Models {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			m := p.Models[k]
			name := strings.TrimSpace(m.Name)
			if name == "" {
				name = normName(k)
			}
			if name == "" {
				continue
			}
			prices, _ := convertCost(m.Cost)
			out = append(out, entry{
				Key:          k,
				Name:         name,
				Fold:         fold(name),
				KeyFold:      fold(k),
				ProviderID:   pid,
				ProviderName: providerDisplayName(p, pid),
				Modality:     guessModality(name),
				Deprecated:   strings.EqualFold(m.Status, "deprecated"),
				Price:        prices,
			})
		}
	}
	return out
}

func providerDisplayName(p devProvider, id string) string {
	if n := strings.TrimSpace(p.Name); n != "" {
		return n
	}
	return id
}

// convertCost 把上游 cost 转成三价；第二个返回值为 free（input=0 且无输出价）。
// 无 cost / input 缺失 / input<0 / (input=0 且有输出价) 均视为无有效单价 → nil。
func convertCost(c *devCost) (*priceTriple, bool) {
	if c == nil || c.Input == nil || math.IsInf(*c.Input, 0) || math.IsNaN(*c.Input) || *c.Input < 0 {
		return nil, false
	}
	in := *c.Input
	out := positive(c.Output)
	cache := positive(c.CacheRead)
	if in == 0 {
		if out != nil {
			return nil, false
		}
		zero := 0.0
		return &priceTriple{input: &zero}, true
	}
	return &priceTriple{input: &in, output: out, cache: cache}, false
}

func positive(v *float64) *float64 {
	if v == nil || *v <= 0 || math.IsInf(*v, 0) || math.IsNaN(*v) {
		return nil
	}
	return v
}

// normName 取最后一个 "/" 之后的部分（上游存在 provider/model 形式的键）。
func normName(k string) string {
	if i := strings.LastIndex(k, "/"); i >= 0 {
		return k[i+1:]
	}
	return k
}

// fold 是匹配用的折叠形式：小写 + 去掉空格与常见分隔符。
//
// 为什么要折叠：上游键是 gpt-4o，而人会输入「GPT 4o」「gpt_4o」。
// 只做小写精确匹配的话，手动搜索框对着一半输入都是 0 结果 —— 那正是这次要修的病。
func fold(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		switch r {
		case ' ', '\t', '-', '_', '.', '/':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// guessModality 按名称归类模态（上游没有可靠的模态字段，名称是唯一线索）。
func guessModality(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "embed"):
		return "embedding"
	case strings.Contains(n, "rerank"), strings.Contains(n, "reranker"):
		return "rerank"
	case strings.Contains(n, "moderation"), strings.Contains(n, "guard"):
		return "moderation"
	case strings.Contains(n, "tts"), strings.Contains(n, "whisper"), strings.Contains(n, "speech"),
		strings.Contains(n, "audio"), strings.Contains(n, "voice"):
		return "audio"
	case strings.Contains(n, "video"), strings.Contains(n, "sora"), strings.Contains(n, "kling"),
		strings.Contains(n, "veo"), strings.Contains(n, "seedance"), strings.Contains(n, "wan2"):
		return "video"
	case strings.Contains(n, "image"), strings.Contains(n, "dall-e"), strings.Contains(n, "dalle"),
		strings.Contains(n, "flux"), strings.Contains(n, "seedream"), strings.Contains(n, "imagen"),
		strings.Contains(n, "midjourney"), strings.Contains(n, "sd3"),
		// nano-banana 与 gemini-*-image 是同一族的两套命名：前者不含 image，
		// 只按 "image" 判会让它在结果里被归成 chat。
		strings.Contains(n, "nano"), strings.Contains(n, "banana"):
		return "image"
	default:
		return defaultModality
	}
}
