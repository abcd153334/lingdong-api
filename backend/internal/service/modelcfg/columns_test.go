package modelcfg

import (
	"reflect"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm/schema"

	"lingdong-api/internal/model"
)

// TestUpdatesColumnsMatchModel 是「列名只有一个来源」的守卫。
//
// 为什么必须有它：落库用的是 map[string]any，键是**手写字面量**。
// 字面量写错时编译器不管、运行时也可能不报（写进一个恰好存在的影子列），
// 症状是"保存成功、价格永远不生效"。历史上就有一处：
// modelcfg 写 `create_cache_ratio1h`，管理台写 `create_cache_ratio_1h`，
// 而 GORM 的默认命名只认前者 —— 两条路径对同一个模型产生了不同价格。
//
// 价格列清单**不再手写**：直接从 model.Prices 的 gorm column tag 反射出来。
// 手写清单在改造后仍然列着 model_ratio / completion_ratio 这些已删除的列，
// 测试一边报"列不存在"一边漏掉真正的 9 条 lane —— 清单本身成了漂移源。
func TestUpdatesColumnsMatchModel(t *testing.T) {
	s, err := schema.Parse(&model.Model{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatalf("解析 Model 失败: %v", err)
	}
	real := make(map[string]bool, len(s.DBNames))
	for _, name := range s.DBNames {
		real[name] = true
	}

	updates := Updates(&model.Model{})
	for col := range updates {
		if !real[col] {
			t.Errorf("Updates 写了不存在的列 %q（models 实际列：%v）", col, s.DBNames)
		}
	}

	// 9 条价格 lane + 规格价 + 计费方式，一个都不能漏：
	// 漏一列 = «只改输入价» 会把那一列写成 NULL（价格凭空消失）。
	required := priceColumns(t)
	required = append(required, "spec_prices", "billing_mode", "tags")
	for _, col := range required {
		if _, ok := updates[col]; !ok {
			t.Errorf("列 %q 不在 Updates 里：改价会静默不生效", col)
		}
		if !real[col] {
			t.Errorf("列 %q 不在 models 表里：写进去会 500", col)
		}
	}

	// 带下划线的列名必须与库列一致（设计文档、pricing_snapshot、billing 全用这个）。
	if !real["price_cache_create_1h"] {
		t.Fatal("models 缺少 price_cache_create_1h 列：1h 缓存价无处可落")
	}
	if real["create_cache_ratio1h"] || real["price_cache_create1h"] {
		t.Fatal("models 仍有驼峰影子列：它会让两套写法各自成功、互不可见")
	}

	// icon 与 display_name 同类：Updates 是全列写，列不在 map 里就等于
	// "每次保存都把图标写成零值"，而 UI 只会显示成"图标丢了"。
	if _, ok := updates["icon"]; !ok {
		t.Error("列 \"icon\" 不在 Updates 里：保存模型会静默清空图标")
	}
	if !real["icon"] {
		t.Fatal("models 缺少 icon 列：图标无处可落")
	}
	// icon 必须用显式 column tag 才能进 Updates（gormColumnOf 只认显式 tag）。
	// 靠 GORM 默认命名当下也能得到 icon；但字段一旦改名（如 IconToken），
	// 默认命名会变成 icon_token 而库列仍是 icon —— 两套写法静默分叉。
	f, ok := reflect.TypeOf(model.Model{}).FieldByName("Icon")
	if !ok {
		t.Fatal("model.Model 缺 Icon 字段")
	}
	if tag := f.Tag.Get("gorm"); !strings.Contains(tag, "column:icon") {
		t.Fatalf("model.Model.Icon 缺显式 column:icon tag，实际 %q", tag)
	}
}

// TestPresetGroupNotWrittenByModelcfg 守「内置标记只有一个所有者」。
//
// preset_group 由 presets 层用显式 UPDATE 写。一旦它回到全列写里，
// 管理台保存一次价目就会把标记清空（Input 里没有这个字段 → 零值覆盖），
// 表现是「恢复出厂」突然找不到任何内置行，且完全没有报错线索。
func TestPresetGroupNotWrittenByModelcfg(t *testing.T) {
	if _, ok := Updates(&model.Model{})["preset_group"]; ok {
		t.Fatal("preset_group 不该由 modelcfg 全列写：它会让每次保存都清空内置标记")
	}
}

// priceColumns 从 model.Prices 反射出全部 gorm 列名（唯一真相源）。
func priceColumns(t *testing.T) []string {
	t.Helper()
	rt := reflect.TypeOf(model.Prices{})
	out := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("gorm")
		if !strings.Contains(tag, "column:") {
			t.Fatalf("model.Prices.%s 缺 column tag：GORM 默认命名会与库列分叉",
				rt.Field(i).Name)
		}
		for _, part := range strings.Split(tag, ";") {
			if strings.HasPrefix(part, "column:") {
				out = append(out, strings.TrimPrefix(part, "column:"))
			}
		}
	}
	// 9 条 lane（8 条 token + 按次），数量变了说明有 lane 被悄悄删掉。
	if len(out) != 9 {
		t.Fatalf("价格 lane 应为 9 条，实际 %d：%v", len(out), out)
	}
	return out
}
