// Command presetdump 把当前库里的「渠道 + 模型 + 绑定」导出为内置出厂预设（JSON）。
//
// 用法：
//
//	export DATABASE_URL=...            # 只读账号即可
//	go run ./tools/presetdump -summary
//	go run ./tools/presetdump -out internal/service/presets/presets_builtin.json
//
// 安全边界（本工具的**唯一职责**是导出可公开的配置事实）：
//   - 不读 channels.keys / key_meta / balance_cny / health；
//   - 丢弃租户私有 adapter_options（AutoDL workflows）与上传图标（upload:）；
//   - 输出前扫描 sk- / AKIA / BEGIN，命中即中止；
//   - 输出不含主键、时间戳、version、status。
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/model"
	channelsvc "lingdong-api/internal/service/channel"
)

type docVendor struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Color    string `json:"color"`
	Icon     string `json:"icon"`
	Priority int    `json:"priority"`
	Sort     int    `json:"sort"`
	Builtin  bool   `json:"builtin"`
}

type docChannel struct {
	AdapterID      string          `json:"adapter_id"`
	Name           string          `json:"name"`
	BaseURL        string          `json:"base_url"`
	Website        string          `json:"website"`
	Icon           string          `json:"icon"`
	AdapterOptions json.RawMessage `json:"adapter_options,omitempty"`
	Weight         int             `json:"weight"`
	Priority       int             `json:"priority"`
	MaxConcurrency int             `json:"max_concurrency"`
	RPMLimit       int             `json:"rpm_limit"`
}

type docModel struct {
	ID              string           `json:"id"`
	DisplayName     string           `json:"display_name"`
	Description     string           `json:"description"`
	Vendor          string           `json:"vendor"`
	Icon            string           `json:"icon"`
	Modality        string           `json:"modality"`
	BillingMode     string           `json:"billing_mode"`
	Prices          model.Prices     `json:"prices"`
	Spec            model.SpecPrices `json:"spec"`
	Endpoints       []string         `json:"endpoints"`
	Caps            []string         `json:"caps"`
	Tags            []model.ModelTag `json:"tags,omitempty"`
	ContextWindow   int              `json:"context_window"`
	MaxOutputTokens int              `json:"max_output_tokens"`
	BindingUpstream string           `json:"binding_upstream,omitempty"`
	BindingWeight   int              `json:"binding_weight"`
	BindingPriority int              `json:"binding_priority"`
}

type docGroup struct {
	MarkerKey string      `json:"marker_key"`
	Version   int         `json:"version"`
	Vendor    docVendor   `json:"vendor"`
	Vendors   []docVendor `json:"vendors,omitempty"`
	Channel   docChannel  `json:"channel"`
	Models    []docModel  `json:"models"`
}

func main() {
	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"), "数据库连接串（默认取 DATABASE_URL）")
	out := flag.String("out", "", "输出 JSON 路径（留空只打印摘要）")
	summary := flag.Bool("summary", false, "只打印每组摘要，不写文件")
	skipAdapters := flag.String("skip-adapters", "volcengine-ark", "逗号分隔：已在代码里内置的适配器（它们的组不要重复导出）")
	flag.Parse()

	if strings.TrimSpace(*dsn) == "" {
		fail("缺少 -dsn（或 DATABASE_URL）")
	}
	gdb, err := gorm.Open(postgres.Open(*dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		fail("连接数据库失败: %v", err)
	}

	skip := map[string]bool{}
	for _, a := range strings.Split(*skipAdapters, ",") {
		if a = strings.TrimSpace(a); a != "" {
			skip[a] = true
		}
	}
	groups, droppedKeys, skipped, err := dump(gdb, skip)
	if err != nil {
		fail("%v", err)
	}
	if *summary || *out == "" {
		for _, g := range groups {
			fmt.Printf("%-40s %-14s %-40s models=%d\n", g.MarkerKey, g.Channel.AdapterID, g.Channel.BaseURL, len(g.Models))
		}
		fmt.Fprintf(os.Stderr, "共 %d 组；丢弃密钥的渠道 %d 条；跳过 %s（密钥永不进种子）\n", len(groups), droppedKeys, strings.Join(skipped, ", "))
		return
	}

	raw, err := json.MarshalIndent(groups, "", "  ")
	if err != nil {
		fail("序列化失败: %v", err)
	}
	raw = append(raw, '\n')
	// 最后一道闸：数据里出现任何疑似凭据就拒绝写文件。
	if bad := secretPattern.FindIndex(raw); bad != nil {
		fail("输出命中疑似凭据（偏移 %d），已中止；请检查源数据", bad[0])
	}
	if err := os.WriteFile(*out, raw, 0o644); err != nil {
		fail("写文件失败: %v", err)
	}
	fmt.Fprintf(os.Stderr, "已写入 %s（%d 组）\n", *out, len(groups))
}

var secretPattern = regexp.MustCompile(`(?i)(sk-[a-z0-9]{8,}|AKIA[0-9A-Z]{16}|-----BEGIN )`)

func dump(gdb *gorm.DB, skip map[string]bool) ([]docGroup, int, []string, error) {
	var channels []model.Channel
	if err := gdb.Where("deleted_at IS NULL").Order("id").Find(&channels).Error; err != nil {
		return nil, 0, nil, fmt.Errorf("读取渠道失败: %w", err)
	}
	droppedKeys := 0
	skipped := make([]string, 0)
	groups := make([]docGroup, 0, len(channels))
	usedSlug := map[string]int{}

	for i := range channels {
		ch := &channels[i]
		// 已在代码里内置的组（火山方舟）不重复导出；自建规格渠道需要 adapter_specs 行，
		// 出厂种子无法单独表达，导出后会因"适配器不存在"导致新装启动失败。
		if skip[ch.AdapterID] || strings.HasPrefix(ch.AdapterID, "spec:") {
			skipped = append(skipped, fmt.Sprintf("%s(%s)", ch.Name, ch.AdapterID))
			continue
		}
		if hasKeys(ch.Keys) {
			droppedKeys++
		}
		var bindings []model.ModelBinding
		if err := gdb.Where("channel_id = ? AND deleted_at IS NULL", ch.ID).
			Order("model_id").Find(&bindings).Error; err != nil {
			return nil, 0, nil, fmt.Errorf("读取绑定失败(渠道 %d): %w", ch.ID, err)
		}
		vendorSeen := map[string]docVendor{}
		models := make([]docModel, 0, len(bindings))
		for _, b := range bindings {
			var m model.Model
			if err := gdb.Where("id = ?", b.ModelID).First(&m).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					continue
				}
				return nil, 0, nil, fmt.Errorf("读取模型 %s 失败: %w", b.ModelID, err)
			}
			v, err := vendorOf(gdb, m.Vendor)
			if err != nil {
				return nil, 0, nil, err
			}
			if v.ID != "" {
				vendorSeen[v.ID] = v
			}
			models = append(models, toDocModel(m, b))
		}

		slug := uniqueSlug(usedSlug, slugOf(ch))
		website := strings.TrimSpace(ch.Website)
		if website == "" {
			website = channelsvc.WebsiteFor(ch.AdapterID, ch.BaseURL)
		}
		g := docGroup{
			MarkerKey: "builtin_presets." + slug,
			Version:   1,
			Channel: docChannel{
				AdapterID: ch.AdapterID, Name: ch.Name, BaseURL: ch.BaseURL,
				Website: website, Icon: sanitizeIcon(ch.Icon),
				AdapterOptions: filterOptions(ch.AdapterID, ch.AdapterOptions),
				Weight:         ch.Weight, Priority: ch.Priority,
				MaxConcurrency: ch.MaxConcurrency, RPMLimit: ch.RPMLimit,
			},
			Models: models,
		}
		vendors := make([]docVendor, 0, len(vendorSeen))
		for _, v := range vendorSeen {
			vendors = append(vendors, v)
		}
		sort.Slice(vendors, func(a, b int) bool { return vendors[a].ID < vendors[b].ID })
		if len(vendors) > 0 {
			g.Vendor = vendors[0]
			g.Vendors = vendors
		}
		groups = append(groups, g)
	}
	return groups, droppedKeys, skipped, nil
}

func vendorOf(gdb *gorm.DB, id string) (docVendor, error) {
	if strings.TrimSpace(id) == "" {
		return docVendor{}, nil
	}
	var v model.Vendor
	if err := gdb.Where("id = ?", id).First(&v).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return docVendor{}, nil
		}
		return docVendor{}, fmt.Errorf("读取供应商 %s 失败: %w", id, err)
	}
	return docVendor{
		ID: v.ID, Name: v.Name, Color: v.Color, Icon: sanitizeIcon(v.Icon),
		Priority: v.Priority, Sort: v.Sort, Builtin: v.Builtin,
	}, nil
}

func toDocModel(m model.Model, b model.ModelBinding) docModel {
	caps := decodeStrings(m.Capabilities)
	outCaps := make([]string, 0, len(caps))
	for _, c := range caps {
		outCaps = append(outCaps, string(apidoc.Cap(c)))
	}
	tags := make([]model.ModelTag, 0)
	if len(m.Tags) > 0 {
		var parsed []model.ModelTag
		if err := json.Unmarshal(m.Tags, &parsed); err == nil {
			for _, t := range parsed {
				// 出厂标签不带到期日：否则新装当天就"已过期"，看起来像数据坏了。
				t.Until = ""
				tags = append(tags, t)
			}
		}
	}
	upstream := strings.TrimSpace(b.UpstreamModel)
	if upstream == m.ID {
		upstream = ""
	}
	weight, priority := b.Weight, b.Priority
	if weight <= 0 {
		weight = 1
	}
	if priority <= 0 {
		priority = 1
	}
	return docModel{
		ID: m.ID, DisplayName: m.DisplayName, Description: m.Description,
		Vendor: m.Vendor, Icon: sanitizeIcon(m.Icon), Modality: m.Modality,
		BillingMode: m.BillingMode, Prices: m.Prices, Spec: m.SpecPrices,
		Endpoints: inferEndpoints(m.Modality, decodeStrings(m.Endpoints)), Caps: outCaps, Tags: tags,
		ContextWindow: m.ContextWindow, MaxOutputTokens: m.MaxOutputTokens,
		BindingUpstream: upstream, BindingWeight: weight, BindingPriority: priority,
	}
}

// inferEndpoints 补齐库存里缺失的端点。
//
// 现网存在"模型能用但 endpoints 为空"的行（历史创建时没填）。出厂配置不能把
// 这个缺口一起复制：没有端点的模型在文档里没有入口，路由也找不到它。
// 映射与 apidoc/kind.go 的 modalityEndpoints 同源（那里未导出，这里只做补空）。
func inferEndpoints(modality string, endpoints []string) []string {
	if len(endpoints) > 0 {
		return endpoints
	}
	switch modality {
	case "music":
		return []string{"audio/music/generations"}
	case "video":
		return []string{"video/generations"}
	case "image":
		return []string{"images/generations"}
	case "chat":
		return []string{"chat/completions"}
	}
	return endpoints
}

// filterOptions 只保留「行为开关」，丢掉租户私有值。
//
// AutoDL 的 workflows.* 是账号里的工作流 ID —— 写进种子等于把别人的账号对象
// 发给所有新用户；metaso 的 fast_mode 则是产品事实（决定走标准还是优先通道），必须保留。
func filterOptions(adapterID string, raw model.JSON) json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || len(m) == 0 {
		return nil
	}
	keep := map[string]bool{}
	switch adapterID {
	case "metaso":
		keep = map[string]bool{"fast_mode": true, "context_ir_enabled": true, "resolution_map": true, "query_path": true}
	case "autodl":
		keep = map[string]bool{"resolution_map": true} // workflows 是租户私有
	case "duomiapi":
		keep = map[string]bool{"model_overrides": true, "paths": true, "metadata_allow": true, "suno_defaults": true}
	}
	out := map[string]json.RawMessage{}
	for k, v := range m {
		if keep[k] {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	enc, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return enc
}

// sanitizeIcon 只保留内置图标；上传图标在新装库不存在（会 404）。
func sanitizeIcon(icon string) string {
	if strings.HasPrefix(icon, "builtin:") {
		return icon
	}
	return ""
}

// hasKeys 判断 keys 列是否真的有内容（"[]" / 空 / null 都算没有）。
func hasKeys(raw model.JSON) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "[]" && s != "null" && s != "{}"
}

func decodeStrings(raw model.JSON) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

func slugOf(ch *model.Channel) string {
	host := ""
	if u, err := url.Parse(strings.TrimSpace(ch.BaseURL)); err == nil {
		host = u.Hostname()
	}
	host = strings.TrimPrefix(strings.ToLower(host), "www.")
	host = strings.TrimPrefix(host, "api.")
	label := strings.Split(host, ".")[0]
	if label == "" {
		label = ch.AdapterID
	}
	return sanitizeLabel(label)
}

var slugRx = regexp.MustCompile(`[^a-z0-9]+`)

func sanitizeLabel(s string) string {
	s = slugRx.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "group"
	}
	return s
}

func uniqueSlug(used map[string]int, base string) string {
	used[base]++
	if used[base] == 1 {
		return base
	}
	return fmt.Sprintf("%s-%d", base, used[base])
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "presetdump: "+format+"\n", args...)
	os.Exit(1)
}
