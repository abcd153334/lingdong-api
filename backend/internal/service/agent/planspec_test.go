package agent

import (
	"errors"
	"strings"
	"testing"
)

// fakeChecker 是校验用的事实来源替身（不连库）。
type fakeChecker struct {
	vendors   map[string]bool
	chNames   map[string]bool
	models    map[string]int // id → version（存在即 0 也行）
	specs     map[string]bool
	specRefs  map[string]int64
	caps      map[string]map[string]bool
	endpoints map[string][]string
}

func (f *fakeChecker) VendorExists(id string) (bool, error) { return f.vendors[id], nil }
func (f *fakeChecker) ChannelNameExists(n string) (bool, error) {
	return f.chNames[n], nil
}
func (f *fakeChecker) ChannelExists(id uint64) (bool, error) { return false, nil }
func (f *fakeChecker) ModelExists(id string) (bool, error) {
	_, ok := f.models[id]
	return ok, nil
}
func (f *fakeChecker) ModelVersion(id string) (int, error) {
	v, ok := f.models[id]
	if !ok {
		return 0, errors.New("not found")
	}
	return v, nil
}
func (f *fakeChecker) ChannelVersion(id uint64) (int, error) { return 1, nil }
func (f *fakeChecker) AdapterSpecExists(slug string) (bool, error) {
	return f.specs[slug], nil
}
func (f *fakeChecker) AdapterSpecRefs(slug string) (int64, error) { return f.specRefs[slug], nil }
func (f *fakeChecker) BoundEndpoints(modelID string) (map[string]bool, error) {
	out := map[string]bool{}
	for k, v := range f.caps[modelID] {
		out[k] = v
	}
	return out, nil
}
func (f *fakeChecker) ModelEndpoints(modelID string) ([]string, error) {
	return f.endpoints[modelID], nil
}

func emptyChecker() *fakeChecker {
	return &fakeChecker{
		vendors: map[string]bool{}, chNames: map[string]bool{}, models: map[string]int{},
		specs: map[string]bool{}, specRefs: map[string]int64{},
		caps: map[string]map[string]bool{}, endpoints: map[string][]string{},
	}
}

const validChatSpec = `{"endpoints":{"chat/completions":{"path":"/v1/chat/completions","passthrough":true}}}`

// happyPlan 是一条完整的"新建规格 + 渠道 + 模型 + 绑定"提案（覆盖全部实体）。
func happyPlan() string {
	return `{
		"summary": "接入中转 A 的对话模型",
		"operations": [
			{"op":"create","entity":"vendor","key":"relayco","data":{"name":"中转公司","color":"#0af"}},
			{"op":"create","entity":"adapter_spec","key":"relay-a","data":{"label":"中转 A","vendor":"relayco","spec":` + validChatSpec + `}},
			{"op":"create","entity":"channel","key":"relay-a-cn","data":{
				"name":"中转A-国内","adapter_id":"spec:relay-a","base_url":"https://api.a.example",
				"keys":["sk-1"],"groups":["default"]}},
			{"op":"create","entity":"model","key":"relay-chat","data":{
				"display_name":"中转对话","vendor":"relayco","modality":"chat","billing_mode":"tokens",
				"enabled":true,"endpoints":["chat/completions"],
				"input_price":500000,"output_price":1500000}},
			{"op":"replace","entity":"model_bindings","key":"relay-chat","data":{
				"bindings":[{"channel_key":"relay-a-cn","upstream_model":"gpt-4o-mini"}]}}
		]
	}`
}

func mustPlan(t *testing.T, raw string) *Plan {
	t.Helper()
	p, err := DecodePlan([]byte(raw))
	if err != nil {
		t.Fatalf("提案应可解析: %v", err)
	}
	return p
}

// TestPlanValidateHappyPath 断言一条完整提案能通过全部结构校验。
func TestPlanValidateHappyPath(t *testing.T) {
	p := mustPlan(t, happyPlan())
	if err := p.ValidateStructure(ProposalKindChange); err != nil {
		t.Fatalf("应通过校验: %v", err)
	}
	// 执行顺序必须被重排成 依赖方向，且同一实体内保持原序。
	got := []string{}
	for _, item := range p.Ordered() {
		op := item.Op
		got = append(got, op.Entity)
	}
	want := "vendor,adapter_spec,channel,model,model_bindings"
	if strings.Join(got, ",") != want {
		t.Fatalf("执行顺序应为 %s，实际 %s", want, strings.Join(got, ","))
	}
}

// TestPlanHashStable 断言 plan_hash 与 map 顺序无关（同一份内容两次算出同一个值）。
func TestPlanHashStable(t *testing.T) {
	a := mustPlan(t, happyPlan())
	// 键顺序打乱、数值写法不同（1 与 1.0 不比较，这里只验证顺序无关）。
	b := mustPlan(t, `{"operations":[
		{"op":"create","entity":"vendor","key":"relayco","data":{"color":"#0af","name":"中转公司"}},
		{"op":"create","entity":"adapter_spec","key":"relay-a","data":{"label":"中转 A","vendor":"relayco","spec":`+validChatSpec+`}},
		{"op":"create","entity":"channel","key":"relay-a-cn","data":{"name":"中转A-国内","adapter_id":"spec:relay-a","base_url":"https://api.a.example","keys":["sk-1"],"groups":["default"]}},
		{"op":"create","entity":"model","key":"relay-chat","data":{"display_name":"中转对话","vendor":"relayco","modality":"chat","billing_mode":"tokens","enabled":true,"endpoints":["chat/completions"],"input_price":500000,"output_price":1500000}},
		{"op":"replace","entity":"model_bindings","key":"relay-chat","data":{"bindings":[{"channel_key":"relay-a-cn","upstream_model":"gpt-4o-mini"}]}}
	],"summary":"接入中转 A 的对话模型"}`)
	ha, err := PlanHash(a)
	if err != nil {
		t.Fatalf("计算 hash 失败: %v", err)
	}
	hb, err := PlanHash(b)
	if err != nil {
		t.Fatalf("计算 hash 失败: %v", err)
	}
	if ha != hb {
		t.Fatalf("同一份内容应得到同一 hash：%s vs %s", ha, hb)
	}
}

// TestPlanValidateRules 逐条覆盖 13 条校验规则（每条一个用例）。
func TestPlanValidateRules(t *testing.T) {
	base := happyPlan()

	cases := []struct {
		name  string
		plan  string
		want  string // 期望错误里出现的片段
		extra func(t *testing.T, p *Plan, err error)
	}{
		{
			// 规则 1
			name: "未知操作",
			plan: `{"summary":"x","operations":[{"op":"delete","entity":"channel","id":1}]}`,
			want: "不支持的操作",
		},
		{
			// 规则 1
			name: "未知实体",
			plan: `{"summary":"x","operations":[{"op":"create","entity":"plugin","key":"a","data":{}}]}`,
			want: "不支持的实体",
		},
		{
			// 规则 2：create 缺 key
			name: "create 缺 key",
			plan: `{"summary":"x","operations":[{"op":"create","entity":"vendor","data":{"name":"a"}}]}`,
			want: "create 必须提供 key",
		},
		{
			// 规则 2：update 缺 expect_version（乐观锁的前提）
			name: "update 缺 expect_version",
			plan: `{"summary":"x","operations":[{"op":"update","entity":"channel","id":3,"data":{"name":"a","adapter_id":"openai","base_url":"https://x"}}]}`,
			want: "expect_version",
		},
		{
			// 规则 3：同一实体出现两次
			name: "同一实体重复",
			plan: `{"summary":"x","operations":[
				{"op":"create","entity":"vendor","key":"v1","data":{"name":"a"}},
				{"op":"create","entity":"vendor","key":"v1","data":{"name":"b"}}]}`,
			want: "出现了两次",
		},
		{
			// 规则 4：spec 渠道不得携带声明式补丁
			name: "adapter_config 携带高风险子键",
			plan: `{"summary":"x","operations":[{"op":"create","entity":"channel","key":"c","data":{
				"name":"c","adapter_id":"openai","base_url":"https://x",
				"adapter_config":{"request_patch":{"temperature":0.2}}}}]}`,
			want: "只允许",
		},
		{
			// 规则 5：端点键必须已知
			name: "未知端点",
			plan: `{"summary":"x","operations":[{"op":"create","entity":"model","key":"m","data":{
				"display_name":"m","vendor":"v","modality":"chat","billing_mode":"tokens",
				"endpoints":["chat/flux"]}}]}`,
			want: "未知端点",
		},
		{
			// 规则 8：未知键（AI 编造字段）
			name: "渠道载荷含编造字段",
			plan: `{"summary":"x","operations":[{"op":"create","entity":"channel","key":"c","data":{
				"name":"c","adapter_id":"openai","base_url":"https://x","channel_priority":3}}]}`,
			// 实现已把对外错误文案改成中文（M20：可见文案必须中文），
			// 断言跟着改 —— 这条曾经写 "unknown field"，改文案之后成了假红。
			want: "不认识的字段",
		},
		{
			// 规则 9：只建模型不建绑定
			name: "新建模型缺绑定",
			plan: `{"summary":"x","operations":[{"op":"create","entity":"model","key":"m","data":{
				"display_name":"m","vendor":"v","modality":"chat","billing_mode":"tokens"}}]}`,
			want: "必须同时给出 model_bindings",
		},
		{
			// 规则 9（续）：绑定为空
			name: "绑定为空",
			plan: `{"summary":"x","operations":[
				{"op":"create","entity":"model","key":"m","data":{"display_name":"m","vendor":"v","modality":"chat","billing_mode":"tokens"}},
				{"op":"replace","entity":"model_bindings","key":"m","data":{"bindings":[]}}]}`,
			want: "绑定不能为空",
		},
		{
			// 规则 13：派生字段不许写
			name: "写入 is_free",
			plan: `{"summary":"x","operations":[{"op":"create","entity":"model","key":"m","data":{
				"display_name":"m","vendor":"v","modality":"chat","billing_mode":"tokens","is_free":true}}]}`,
			want: "派生字段",
		},
		{
			// 规则 1 的补充：计费方式与模态不匹配（audio 只能按次）
			name: "audio 按量计费",
			plan: `{"summary":"x","operations":[{"op":"create","entity":"model","key":"m","data":{
				"display_name":"m","vendor":"v","modality":"audio","billing_mode":"tokens"}}]}`,
			want: "不支持计费方式",
		},
		{
			// 规则 5b：能力必须是封闭词表内的词（AI 极擅长发明 t2v_hd 这种名）
			name: "能力词不在词表内",
			plan: `{"summary":"x","operations":[{"op":"create","entity":"model","key":"m","data":{
				"display_name":"m","vendor":"v","modality":"video","billing_mode":"spec",
				"capabilities":["t2v_hd"],
				"spec_prices":{"kind":"per_video", "max_seconds": 10,"second_price":{"720p":9000}}}}]}`,
			want: "未定义能力",
		},
		{
			// 规则 5b（续）：图像能力挂到视频模型上
			name: "能力与模态不匹配",
			plan: `{"summary":"x","operations":[{"op":"create","entity":"model","key":"m","data":{
				"display_name":"m","vendor":"v","modality":"video","billing_mode":"spec",
				"capabilities":["t2i"],
				"spec_prices":{"kind":"per_video", "max_seconds": 10,"second_price":{"720p":9000}}}}]}`,
			want: "不适用于",
		},
		{
			// 规则 5b（续）：重复声明
			name: "能力重复",
			plan: `{"summary":"x","operations":[{"op":"create","entity":"model","key":"m","data":{
				"display_name":"m","vendor":"v","modality":"video","billing_mode":"spec",
				"capabilities":["i2v","i2v"],
				"spec_prices":{"kind":"per_video", "max_seconds": 10,"second_price":{"720p":9000}}}}]}`,
			want: "能力重复",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := DecodePlan([]byte(c.plan))
			if err != nil {
				if strings.Contains(err.Error(), c.want) {
					return
				}
				t.Fatalf("解析失败（期望含 %q）: %v", c.want, err)
			}
			err = p.ValidateStructure(ProposalKindChange)
			if err == nil {
				t.Fatalf("应被拒绝，实际通过")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误应含 %q，实际 %q", c.want, err.Error())
			}
			if c.extra != nil {
				c.extra(t, p, err)
			}
		})
	}

	// happyPlan 仍应通过（确认上面没有把正常路径改坏）。
	if err := mustPlan(t, base).ValidateStructure(ProposalKindChange); err != nil {
		t.Fatalf("正常提案应通过: %v", err)
	}
}

// TestPlanAcceptsCapabilities 断言合法能力声明能通过校验（避免规则 5b 过度拦截）。
//
// 覆盖两件事：
//  1. 词表内的能力 + 匹配的模态 → 通过；
//  2. **空能力也能通过** —— 不填能力只意味着右栏少几个 TAB，不该阻断建模型。
func TestPlanAcceptsCapabilities(t *testing.T) {
	withCaps := `{
		"summary": "接入可灵",
		"operations": [
			{"op":"create","entity":"adapter_spec","key":"relay-v","data":{"label":"中转 V","spec":` + validChatSpec + `}},
			{"op":"create","entity":"channel","key":"c1","data":{
				"name":"视频渠道","adapter_id":"spec:relay-v","base_url":"https://api.v.example"}},
			{"op":"create","entity":"model","key":"kling","data":{
				"display_name":"可灵","vendor":"kuaishou","modality":"video","billing_mode":"spec",
				"endpoints":["video/generations"],
				"capabilities":["t2v","i2v","first_last","ref_audio"],
				"spec_prices":{"kind":"per_video", "max_seconds": 10,"second_price":{"720p":9000}}}},
			{"op":"replace","entity":"model_bindings","key":"kling","data":{
				"bindings":[{"channel_key":"c1","upstream_model":"kling-v3"}]}}
		]
	}`
	if err := mustPlan(t, withCaps).ValidateStructure(ProposalKindChange); err != nil {
		t.Fatalf("合法能力声明应通过: %v", err)
	}

	// 省略 capabilities 的等价提案也必须通过。
	withoutCaps := strings.Replace(withCaps,
		`"capabilities":["t2v","i2v","first_last","ref_audio"],`, ``, 1)
	if strings.Contains(withoutCaps, "capabilities") {
		t.Fatal("用例构造失败：没把 capabilities 去掉")
	}
	if err := mustPlan(t, withoutCaps).ValidateStructure(ProposalKindChange); err != nil {
		t.Fatalf("不填能力应通过（只影响文档 TAB 数量）: %v", err)
	}
}
