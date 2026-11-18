package appplugins

import (
	"strings"
	"testing"
)

// mustSample 每次重新解码：manifest 里有切片，复用同一个对象做"改一个字段再校验"
// 会悄悄把上一轮的改动带进下一轮。
func mustSample(t *testing.T) *Manifest {
	t.Helper()
	m, ps := Decode([]byte(SampleManifest))
	if len(ps) > 0 {
		t.Fatalf("示例 manifest 自己就不合法: %+v", ps)
	}
	if problems := m.Validate("ticket"); len(problems) > 0 {
		t.Fatalf("示例 manifest 自己就不合法: %+v", problems)
	}
	return m
}

func hasPath(problems []Problem, path string) bool {
	for _, p := range problems {
		if p.Path == path {
			return true
		}
	}
	return false
}

// TestDecodeRejectsUnknownField 未知字段必须拒：宽容解析会让"多写的字段"静默失效，
// 而作者以为它生效了（planspec.go:76-79 记过这个坑）。
func TestDecodeRejectsUnknownField(t *testing.T) {
	raw := strings.Replace(SampleManifest, `"apiVersion": 1,`, `"apiVersion": 1, "webhook": "https://x",`, 1)
	_, ps := Decode([]byte(raw))
	if len(ps) == 0 {
		t.Fatal("带未知字段的 manifest 必须被拒")
	}
	if !hasPath(ps, "webhook") {
		t.Fatalf("问题应定位到 webhook，实际 %+v", ps)
	}
}

// TestDecodeRejectsOversize 超 64KB 直接拒：不拒的话一次导入会把校验拖到超时。
func TestDecodeRejectsOversize(t *testing.T) {
	big := strings.Repeat("x", MaxManifestBytes+1)
	_, ps := Decode([]byte(big))
	if len(ps) == 0 || !strings.Contains(ps[0].Message, "KB") {
		t.Fatalf("超长 manifest 应报体积问题，实际 %+v", ps)
	}
}

// TestValidateProblems 逐条覆盖"看起来对、落地会错"的写法。
func TestValidateProblems(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Manifest)
		want   string
	}{
		{"apiVersion", func(m *Manifest) { m.APIVersion = 2 }, "apiVersion"},
		{"key 与路径不一致", func(m *Manifest) { m.Key = "other" }, "key"},
		{"key 非法", func(m *Manifest) { m.Key = "Ticket" }, "key"},
		{"版本号格式", func(m *Manifest) { m.Version = "v1" }, "version"},
		{"没有实体", func(m *Manifest) { m.Entities = nil }, "entities"},
		{"实体标识重复", func(m *Manifest) { m.Entities = append(m.Entities, m.Entities[0]) }, "entities[1].key"},
		{"字段标识重复", func(m *Manifest) { m.Entities[0].Fields = append(m.Entities[0].Fields, m.Entities[0].Fields[0]) }, "entities[0].fields[5].key"},
		{"保留字段名", func(m *Manifest) { m.Entities[0].Fields[0].Key = "record_key"; m.Entities[0].Unique = "" }, "entities[0].fields[0].key"},
		{"字段类型非法", func(m *Manifest) { m.Entities[0].Fields[0].Type = "money" }, "entities[0].fields[0].type"},
		{"枚举没有取值", func(m *Manifest) { m.Entities[0].Fields[2].Options = nil }, "entities[0].fields[2].options"},
		{"列引用不存在字段", func(m *Manifest) { m.Entities[0].List.Columns = []string{"nope"} }, "entities[0].list.columns[0]"},
		{"没有列", func(m *Manifest) { m.Entities[0].List.Columns = nil }, "entities[0].list.columns"},
		{"动作非法", func(m *Manifest) { m.Entities[0].List.Actions = []string{"publish"} }, "entities[0].list.actions[0]"},
		{"unique 指向 text", func(m *Manifest) { m.Entities[0].Unique = "detail" }, "entities[0].unique"},
		{"unique 指向不存在", func(m *Manifest) { m.Entities[0].Unique = "nope" }, "entities[0].unique"},
		{"unique 超长", func(m *Manifest) { m.Entities[0].Fields[0].MaxLen = 200 }, "entities[0].unique"},
		{"容量超基座", func(m *Manifest) { m.Limits.MaxRecords = MaxRecordsCap + 1 }, "limits.maxRecords"},
		{"图标不存在", func(m *Manifest) { m.Icon = "builtin:no-such-icon" }, "icon"},
		{"图标前缀非法", func(m *Manifest) { m.Icon = "https://x/y.png" }, "icon"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := mustSample(t)
			tc.mutate(m)
			ps := m.Validate("ticket")
			if !hasPath(ps, tc.want) {
				t.Fatalf("期望定位到 %s，实际 %+v", tc.want, ps)
			}
		})
	}
}

// TestValidateCollectsAll 一次收全：逐个报会让 AI/运营改 5 轮。
func TestValidateCollectsAll(t *testing.T) {
	m := mustSample(t)
	m.APIVersion = 2
	m.Version = "v1"
	m.Entities[0].List.Actions = []string{"publish"}
	if got := len(m.Validate("ticket")); got < 3 {
		t.Fatalf("应一次报出多个问题，实际 %d 条", got)
	}
}

// TestDescribeResolvesColumns 编译产物里的列必须已经带 label/type ——
// 前端不该知道"columns 里写的是字段名"。
func TestDescribeResolvesColumns(t *testing.T) {
	m := mustSample(t)
	d := m.Describe()
	if len(d.Entities) != 1 {
		t.Fatalf("应有一个数据对象，实际 %d", len(d.Entities))
	}
	cols := d.Entities[0].List.Columns
	if len(cols) != 4 {
		t.Fatalf("应有 4 列，实际 %d", len(cols))
	}
	if cols[0].Key != "ticketNo" || cols[0].Label != "单号" || cols[0].Type != TypeString {
		t.Fatalf("第一列解析错误: %+v", cols[0])
	}
	if d.Entities[0].Unique != "ticketNo" {
		t.Fatalf("唯一字段应下发，实际 %q", d.Entities[0].Unique)
	}
	for _, f := range d.Entities[0].Fields {
		if f.Label == "" {
			t.Fatalf("字段 %s 缺少显示名", f.Key)
		}
	}
}

// TestNormalizeFillsDefaults 缺省值必须被写回 manifest：
// 库里存一份"没有 maxLen"的定义，读取时就只能各自猜。
func TestNormalizeFillsDefaults(t *testing.T) {
	raw := `{"apiVersion":1,"key":"mini","name":"迷你","version":"1.0.0","entities":[
		{"key":"a","name":"A","fields":[{"key":"t","label":"T","type":"text"}],
		 "list":{"columns":["t"],"actions":["create"]}}]}`
	m, ps := Decode([]byte(raw))
	if len(ps) > 0 {
		t.Fatalf("应可解码: %+v", ps)
	}
	if m.Entities[0].Fields[0].MaxLen != 1000 {
		t.Fatalf("text 默认长度应为 1000，实际 %d", m.Entities[0].Fields[0].MaxLen)
	}
	if m.Limits == nil || m.Limits.MaxRecords != DefaultMaxRecords {
		t.Fatalf("容量应补默认值，实际 %+v", m.Limits)
	}
}
