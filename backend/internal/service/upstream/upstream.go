// Package upstream 实现上游价目源的真实拉取与本地差异预览（设计 §4.9.6 / G14）。
// 入口：GET /api/admin/upstream/sources（来源登记 + 最近一次拉取状态）、
// POST /api/admin/upstream/preview（真实拉取 → 与本地模型 diff，只读不写入）。
// 差异口径对齐原型 prototype/admin-models.html:557-590：同名跨供应商去重取首个登记项、
// 仅带有效 token 单价的条目进同步（已停售仍列出供识别）、一致判定阈值 1%。
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
	"sync"
	"time"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/money"
	"lingdong-api/internal/pkg/ssrf"
)

// ErrSourceUnavailable 表示来源当前不可用（上游已下线 / 拉取失败）。
var ErrSourceUnavailable = errors.New("upstream source unavailable")

const (
	modelsDevID  = "models_dev"
	modelsDevURL = "https://models.dev/api.json"
	// fetchTimeout 是单次上游拉取超时（拉取只在管理台手动触发，取宽松值）。
	fetchTimeout = 30 * time.Second
	// maxPayloadBytes 是拉取体积上限（models.dev 实测约 5.8MB，留足余量并防内存放大）。
	maxPayloadBytes = 32 << 20
	// sameEpsilon 是「本地价 == 上游价」的判定阈值（相对差，对齐原型 admin-models.html:588）。
	sameEpsilon = 0.01
	// defaultModality 是上游条目无法归类时的兜底模态。
	defaultModality = "chat"
	// basellmID 是已下线的 new-api 内置源。
	basellmID = "basellm"
)

// Source 是上游价目源登记项（对齐 prototype/assets/mock-data.js L61-L63）。
// OK 表示「最近一次真实拉取是否成功」；进程内首次拉取前，OK 为登记态（models.dev 已实测可拉取）。
type Source struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Err  string `json:"err,omitempty"`
}

var (
	mu       sync.RWMutex
	registry = []Source{
		{ID: modelsDevID, Name: "models.dev", OK: true},
		{ID: basellmID, Name: "basellm", OK: false, Err: "上游已下线"},
	}
)

// Sources 返回全部上游来源登记项（OK/Err 反映最近一次真实拉取结果）。
func Sources() []Source {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Source, len(registry))
	copy(out, registry)
	return out
}

// setStatus 记录最近一次拉取结果，供 Sources 下发（OK=false 时前端会禁用该来源）。
func setStatus(id string, ok bool, errMsg string) {
	mu.Lock()
	defer mu.Unlock()
	for i := range registry {
		if registry[i].ID == id {
			registry[i].OK = ok
			registry[i].Err = errMsg
			return
		}
	}
}

// PreviewItem 是一条上游模型差异预览项（字段口径对齐前端 UpstreamPreviewItem）。
type PreviewItem struct {
	Name        string   `json:"name"`
	Vendor      string   `json:"vendor"`
	Modality    string   `json:"modality"`
	InputPrice  *float64 `json:"input_price"`
	OutputPrice *float64 `json:"output_price"`
	CachePrice  *float64 `json:"cache_price"`
	Status      string   `json:"status"` // new/diff/same/deprecated
}

// devCost 是 models.dev 的 cost 结构（实测：{"input":2.5,"output":10,"cache_read":1.25}，单位 USD/1M tokens）。
type devCost struct {
	Input     *float64 `json:"input"`
	Output    *float64 `json:"output"`
	CacheRead *float64 `json:"cache_read"`
}

type devModel struct {
	Cost   *devCost `json:"cost"`
	Status string   `json:"status"` // 实测线上无该字段；mock 数据用 deprecated 演示停售
}

type devProvider struct {
	ID     string              `json:"id"`
	Name   string              `json:"name"`
	Models map[string]devModel `json:"models"`
}

// localModel 是参与 diff 的本地模型投影。
type localModel struct {
	ID       string
	Vendor   string
	Modality string
	// InputMicro 是本地输入价（**微 / 百万 tokens**，结算币种）。nil = 未配价。
	//
	// 不能用已删除的 model_ratio（倍率）来比价：倍率与美元单价之间还隔着
	// 「站点币种 + 汇率」两层，拿倍率×2 去和上游 USD/1M 比永远得到 diff。
	//
	// 显式 column tag 是必须的：字段名 InputMicro 的默认列名是 input_micro，
	// 与库列 price_input 对不上，Select 出来的值会静默落空（每条都判 new）。
	InputMicro *int64 `gorm:"column:price_input"`
}

// Preview 真实拉取指定来源并计算与本地模型的差异预览（只读，不落库）。
// 拉取失败时返回 ErrSourceUnavailable（不返回空列表掩盖失败），并把失败状态记入 Sources。
func Preview(ctx context.Context, sourceID string) ([]PreviewItem, error) {
	switch sourceID {
	case modelsDevID:
		providers, err := fetchModelsDev(ctx)
		if err != nil {
			setStatus(modelsDevID, false, briefErr(err))
			return nil, fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
		}
		setStatus(modelsDevID, true, "")
		items, err := diffWithLocal(ctx, providers)
		if err != nil {
			return nil, err
		}
		return items, nil
	case basellmID:
		return nil, fmt.Errorf("%w: basellm 上游已下线", ErrSourceUnavailable)
	default:
		return nil, fmt.Errorf("%w: 未知来源 %q", ErrSourceUnavailable, sourceID)
	}
}

// fetchModelsDev 拉取 models.dev 全量价目表（SSRF 校验 + 超时 + 体积上限）。
func fetchModelsDev(ctx context.Context) (map[string]devProvider, error) {
	if err := ssrf.ValidatePublicURLResolved(ctx, modelsDevURL); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsDevURL, nil)
	if err != nil {
		return nil, err
	}
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
	var out map[string]devProvider
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("上游响应解析失败: %w", err)
	}
	if len(out) == 0 {
		return nil, errors.New("上游返回空数据集")
	}
	return out, nil
}

// diffWithLocal 按原型口径计算差异：本地驱动（本地模型 vs 上游同名条目）
// + 来源驱动（与本地供应商相关的来源里，本地缺失的 token 型模型 → 新增）。
func diffWithLocal(ctx context.Context, providers map[string]devProvider) ([]PreviewItem, error) {
	locals, err := loadLocalModels(ctx)
	if err != nil {
		return nil, err
	}
	localByID := make(map[string]localModel, len(locals))
	vendorSet := make(map[string]bool, len(locals))
	for _, l := range locals {
		localByID[strings.ToLower(normName(l.ID))] = l
		vendorSet[strings.ToLower(l.Vendor)] = true
	}

	provIDs := make([]string, 0, len(providers))
	for id := range providers {
		provIDs = append(provIDs, id)
	}
	sort.Strings(provIDs)

	// 跨供应商同名去重：本地供应商一致 → openai → 供应商 id 字典序（与原型「取首个登记项」等价且确定）。
	type pick struct {
		prov string
		name string
		m    devModel
	}
	best := map[string]pick{}
	for _, pid := range provIDs {
		p := providers[pid]
		for key, m := range p.Models {
			name := normName(key)
			nk := strings.ToLower(name)
			cur, ok := best[nk]
			if !ok || better(pid, p.Name, name, cur.prov, providers, localByID) {
				best[nk] = pick{prov: pid, name: name, m: m}
			}
		}
	}

	names := make([]string, 0, len(best))
	for nk := range best {
		names = append(names, nk)
	}
	sort.Strings(names)

	items := make([]PreviewItem, 0, len(names))
	for _, nk := range names {
		c := best[nk]
		deprecated := strings.EqualFold(c.m.Status, "deprecated")
		prices, free := convertCost(c.m.Cost)
		// 无有效 token 单价的条目不进同步（已停售仍列出供识别，对齐原型 admin-models.html:584）。
		if !deprecated && (prices == nil || free) {
			continue
		}
		local, hasLocal := localByID[nk]
		// 来源驱动的「新增」仅限与本地上游供应商相关的来源，避免一次返回上游全量模型。
		if !hasLocal && !relatedProvider(c.prov, providers, vendorSet) {
			continue
		}

		var in, out, cache *float64
		if prices != nil {
			in, out, cache = prices.input, prices.output, prices.cache
		}
		status := "same"
		switch {
		case deprecated:
			status = "deprecated"
		case !hasLocal || local.InputMicro == nil:
			status = "new"
		case in != nil && *in > 0 && math.Abs(usdPer1M(*local.InputMicro)-*in)/(*in) < sameEpsilon:
			status = "same"
		default:
			status = "diff"
		}

		modality := defaultModality
		if hasLocal && local.Modality != "" {
			modality = local.Modality
		} else if guessed := guessModality(c.name); guessed != "" {
			modality = guessed
		}
		items = append(items, PreviewItem{
			Name:        c.name,
			Vendor:      c.prov,
			Modality:    modality,
			InputPrice:  in,
			OutputPrice: out,
			CachePrice:  cache,
			Status:      status,
		})
	}

	// 输出顺序：新增 → 有差异 → 一致 → 已停售，组内按名称升序（可复现，便于人工核对）。
	rank := map[string]int{"new": 0, "diff": 1, "same": 2, "deprecated": 3}
	sort.SliceStable(items, func(i, j int) bool {
		if rank[items[i].Status] != rank[items[j].Status] {
			return rank[items[i].Status] < rank[items[j].Status]
		}
		return items[i].Name < items[j].Name
	})
	return items, nil
}

type priceTriple struct {
	input  *float64
	output *float64
	cache  *float64
}

// convertCost 把上游 cost 转成预览价；第二个返回值为 free（input=0 且无输出价，原型同口径）。
// 无 cost / input 缺失 / input<0 / (input=0 且有输出价) 均视为无有效单价 → 返回 nil。
// usdPer1M 把「结算币种微 / 百万 tokens」换算成「USD / 百万 tokens」。
//
// 上游 models.dev 的 cost 一律是美元/1M，本地价是结算币种微 —— 两者之间隔着
// 币种与汇率，必须先统一到同一量纲再比，否则每条都判成 diff。
// 汇率只用于这种「对外比价」，不参与任何扣费。
func usdPer1M(micro int64) float64 {
	v := float64(micro) / float64(money.Scale)
	if conf.SiteCurrency() == conf.CurrencyUSD {
		return v
	}
	fx := conf.CnyPerUsd()
	if fx <= 0 {
		fx = conf.DefaultCnyPerUsd
	}
	return v / fx
}

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

// better 判断 pid 是否比当前已选来源更适合代表该模型（本地供应商一致 → openai → id 字典序最小）。
func better(pid, pname, name, curPID string, providers map[string]devProvider, localByID map[string]localModel) bool {
	if pid == curPID {
		return false
	}
	r := func(id, pn string) int {
		if l, ok := localByID[strings.ToLower(name)]; ok && l.Vendor != "" &&
			(strings.EqualFold(id, l.Vendor) || strings.EqualFold(pn, l.Vendor)) {
			return 0
		}
		if strings.EqualFold(id, "openai") {
			return 1
		}
		return 2
	}
	ri, rc := r(pid, pname), r(curPID, providers[curPID].Name)
	if ri != rc {
		return ri < rc
	}
	return pid < curPID
}

// relatedProvider 判断某来源是否与本地已有供应商相关（用于限制「新增」条目范围）。
func relatedProvider(pid string, providers map[string]devProvider, vendorSet map[string]bool) bool {
	if vendorSet[strings.ToLower(pid)] {
		return true
	}
	if p, ok := providers[pid]; ok && p.Name != "" && vendorSet[strings.ToLower(p.Name)] {
		return true
	}
	return false
}

// loadLocalModels 读取本地模型投影（不加载定价明细，仅 diff 所需列）。
func loadLocalModels(ctx context.Context) ([]localModel, error) {
	var locals []localModel
	if err := db.DB.WithContext(ctx).Model(&model.Model{}).
		Select("id, vendor, modality, price_input").Find(&locals).Error; err != nil {
		return nil, err
	}
	return locals, nil
}

// normName 取最后一个 "/" 之后的部分（上游存在 provider/model 形式的键，对齐原型 admin-models.html:557）。
func normName(k string) string {
	if i := strings.LastIndex(k, "/"); i >= 0 {
		return k[i+1:]
	}
	return k
}

// guessModality 在本地无反查时按名称归类（对齐原型 SYNC_MODALITY 兜底表的通用化）。
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
		// 只按 "image" 判会让它在发现页被归成默认模态（chat），导入后必被端点闸门拒。
		strings.Contains(n, "nano"), strings.Contains(n, "banana"):
		return "image"
	default:
		return defaultModality
	}
}

// briefErr 截断错误信息（存入 Sources.err，避免超长内容下发）。
func briefErr(err error) string {
	s := err.Error()
	const max = 200
	if len(s) > max {
		return s[:max]
	}
	return s
}
