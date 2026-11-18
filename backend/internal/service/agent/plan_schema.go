package agent

import (
	"encoding/json"
	"reflect"
	"strings"
)

// 本文件是 plan 的**机器契约**：tool schema 与 schema_get(plan) 都从这里生成。
//
// 为什么不在提示词里手写字段清单：字段一改，md 与 prompt 就过期，而模型会照着
// 过期清单写提案 —— 症状是 plan_invalid，但没人知道是"文档错了"。
// 由结构体反射生成，字段名只有一个来源。

// 字段说明只放"模型必须知道、但看名字猜不出来"的那些。
// 类型、必填、枚举全部来自结构体与常量，不在这里重复。
var planFieldNotes = map[string]map[string]string{
	"vendor": {
		"name":  "供应商名称，同时是 vendors 的主键；写错会让门户按供应商筛选时对不上",
		"color": "展示色值，如 #4F7CFF",
		"icon":  "图标 token，只能是 builtin:<key>（key 见 schema_get(icons)）；留空 = 显示首字",
	},
	"adapter_spec": {
		"label":  "展示名，最多 32 字；可省略则用 spec.label",
		"vendor": "vendors.id，仅展示与归组，不参与选路",
		"spec":   "DSL 原文；字段与限额见 schema_get(spec_dsl)。**spec.slug 由 operations[].key 覆盖，可以不写**",
	},
	"channel": {
		"name":            "渠道名，平台内唯一（重名会被拒）；建议「厂商-线路」",
		"adapter_id":      "必须是 adapter_list / spec_list 里出现的 id（spec 形态为 spec:<slug>）",
		"base_url":        "http(s) 公网地址；内网需平台开启 LD_UPSTREAM_ALLOW_PRIVATE",
		"keys":            "**仅新建必填**；更新时不接受（改密钥请到管理台「密钥池」面板，逐条增删改）",
		"groups":          "必须存在于平台的分组折扣表（system_config.group_discount 的键集），写错会导致该渠道永不被选中",
		"weight":          "1..1000，默认 5；数值越大越优先被选中",
		"priority":        "1..1000，默认 1；越小越优先",
		"multi_key_mode":  "多密钥轮询方式（如 polling / random）；省略沿用库内值",
		"auto_ban":        "连续失败是否自动禁用；省略沿用库内值",
		"rpm_limit":       "≥0，默认 60；0 = 不限流（**省略**与写 0 不同）",
		"max_concurrency": "≥0，默认 10；0 = 不限并发（**省略**与写 0 不同）",
		"adapter_config":  "只允许 auth_header / auth_prefix / path_overrides；其余请人工配置",
		"icon":            "图标 token（builtin:<key>）；留空 = 首字占位。上传图只能由人工在管理台设置",
	},
	"model": {
		"display_name":          "门户展示名",
		"vendor":                "必须是 vendor_list 里已存在的分组 id（写错会让门户供应商名为空）",
		"modality":              "见 schema_get(all).modalities；决定可用端点与计费方式",
		"billing_mode":          "tokens 按量 / call 按次 / spec 按规格；必须与 modality 匹配",
		"enabled":               "是否上架；新建省略 = 上架",
		"endpoints":             "端点键必须来自 schema_get(endpoints)；留空则按模态派生",
		"capabilities":          "封闭词表；影响文档右栏的预设 TAB",
		"tags":                  "营销标签（最多 3 个，数组顺序即展示顺序）；每项 {label, color, until?}，color 取 12 色板 key 或 #RRGGBB，until 为空表示长期有效",
		"spec_prices":           "billing_mode=spec 必填；内部金额单位是**微**（1 单位货币 = 1e6 微）；档位键大小写有严格规定（见 40-pricing 知识包）",
		"call_price":            "按次计费的单价，单位微/次（billing_mode=call）",
		"input_price":           "按量计费的输入单价，单位微/百万 tokens（币种见 pricing_basis）；0 = 显式免费",
		"output_price":          "按量计费的输出单价，单位微/百万 tokens；省略则按输入单价计",
		"cache_price":           "缓存读取单价，单位微/百万 tokens（提示词缓存命中时计费）",
		"cache_create_price":    "缓存写入单价，单位微/百万 tokens",
		"cache_create_price_1h": "1 小时缓存写入单价，单位微/百万 tokens（与上一条是不同 lane，别混用）",
		"image_price":           "图像 token 通道单价，单位微/百万 tokens（图像类多模态模型）",
		"audio_price":           "音频输入单价，单位微/百万 tokens；音频类模型**不能**只配 input_price，否则永久 0 元",
		"audio_output_price":    "音频输出单价，单位微/百万 tokens",
		"description":           "门户上的模型简介（纯文本，最多 200 字）",
		"icon":                  "图标 token（builtin:<key>）；留空 = 回落到所属供应商的图标，再回落到首字。上传图（upload:<id>）只能由人工在管理台设置",
		"context_window":        "该模型的真实上下文窗口（token），0/省略 = 继承助手全局设置；填小了会被上游 400，填大了浪费预算",
		"max_output_tokens":     "该模型单次输出上限，0/省略 = 继承助手全局设置；不能超过上游硬限制",
	},
	"model_bindings": {
		"bindings": "整体替换：提交的集合就是最终集合；channel_key 指向本提案内新建的渠道",
	},
}

// planOperationsSchema 生成 propose_change.operations 的 JSON Schema。
func planOperationsSchema() json.RawMessage {
	// 五个分支必须带 title：模型要在"字段集合"之间选一个，而分支名（vendor/…）
	// 只有放到 title 里才与 entity 的 enum 对得上；否则它只能靠"哪个能解析通过"试。
	data := []any{
		objectSchema(reflect.TypeOf(VendorData{}), "vendor", "供应商"),
		objectSchema(reflect.TypeOf(AdapterSpecData{}), "adapter_spec", "声明式规格"),
		objectSchema(reflect.TypeOf(ChannelData{}), "channel", "渠道"),
		objectSchema(reflect.TypeOf(ModelData{}), "model", "模型"),
		objectSchema(reflect.TypeOf(BindingData{}), "model_bindings", "模型绑定"),
	}
	op := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"op": map[string]any{
				"type": "string", "enum": []string{OpCreate, OpUpdate, OpDisable, OpReplace},
				"description": "create 新建 / update 修改（需 expect_version）/ disable 停用 / replace 仅用于 model_bindings 的整体替换",
			},
			"entity": map[string]any{
				"type": "string", "enum": []string{EntityVendor, EntityAdapterSpec, EntityChannel, EntityModel, EntityModelBindings},
				// 没有 description 时，模型只能靠 enum 的英文名猜"data 该长什么样"，
				// 而 data 是 oneOf 五分支 —— 猜错的表现是"字段不在允许列表里"。
				"description": "要写的实体；data 的形状由它决定：vendor=供应商、adapter_spec=声明式规格、channel=渠道、model=模型、model_bindings=模型与渠道的绑定",
			},
			"key": map[string]any{
				"type":        "string",
				"description": "提案内的引用名：create 必填；model/vendor 的 key 就是主键；replace model_bindings 时是模型 ID",
			},
			"id": map[string]any{
				"type":        "integer",
				"description": "已存在实体的主键，仅 channel / adapter_spec 用（update、disable）",
			},
			"expect_version": map[string]any{
				"type":        "integer",
				"description": "update 必填：取 model_get / channel_get / spec_get 返回的 version，用于乐观锁",
			},
			"model": map[string]any{
				"type": "string", "description": "disable model_bindings 时指定模型 ID",
			},
			"channel_id": map[string]any{
				"type": "integer", "description": "disable model_bindings 时指定渠道 ID",
			},
			"data": map[string]any{
				"description": "实体载荷；形状由 entity 决定（vendor / adapter_spec / channel / model / model_bindings 之一）",
				"oneOf":       data,
			},
		},
		"required": []string{"op", "entity"},
	}
	b, _ := json.Marshal(op)
	return b
}

// objectSchema 按结构体生成对象 schema。
//
// 规则：
//   - 指针字段 = 非必填（nil 表示"这次不改"）；
//   - `,omitempty` = 非必填；
//   - json.RawMessage = 任意对象（由服务端做二级校验）；
//   - 其余 = 必填。
//
// title（可选）是这一分支的中文名：模型在 five-way oneOf 之间选择时，
// 只有 title 能与 entity 的 enum 对上；缺了它只能靠"哪个解析得通"试错。
func objectSchema(t reflect.Type, entity string, title ...string) map[string]any {
	props := map[string]any{}
	required := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, omit, ptr := jsonName(f)
		if name == "" || name == "-" {
			continue
		}
		props[name] = fieldSchema(f.Type)
		if note := planFieldNotes[entity][name]; note != "" {
			props[name].(map[string]any)["description"] = note
		}
		if !ptr && !omit {
			required = append(required, name)
		}
	}
	out := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           props,
	}
	if len(title) > 0 && title[0] != "" {
		out["title"] = title[0]
	}
	if len(required) > 0 {
		out["required"] = required
	}
	// update 的语义必须写在 schema 里：否则模型会把"没提的字段"当成要清空。
	if entity == "channel" || entity == "model" {
		out["description"] = "update 时只提交要改的字段；省略的字段保持库内现值（不会被重置或清空）"
	}
	return out
}

func jsonName(f reflect.StructField) (name string, omitempty bool, pointer bool) {
	tag := f.Tag.Get("json")
	if tag == "" {
		return "", false, false
	}
	parts := strings.Split(tag, ",")
	name = strings.TrimSpace(parts[0])
	for _, p := range parts[1:] {
		if strings.TrimSpace(p) == "omitempty" {
			omitempty = true
		}
	}
	pointer = f.Type.Kind() == reflect.Ptr
	return name, omitempty, pointer
}

func fieldSchema(t reflect.Type) map[string]any {
	if t.Kind() == reflect.Ptr {
		return fieldSchema(t.Elem())
	}
	// json.RawMessage 是 []byte 的别名：先判类型名，避免被当成 array of integer。
	if t.Name() == "RawMessage" {
		return map[string]any{"type": "object", "description": "任意 JSON 对象（由服务端严格校验字段）"}
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return map[string]any{"type": "string"}
		}
		return map[string]any{"type": "array", "items": fieldSchema(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": fieldSchema(t.Elem())}
	case reflect.Struct:
		return objectSchema(t, "")
	default:
		return map[string]any{}
	}
}

// planEntityOps 是每个实体允许的操作。写在字典里而不是只靠校验器报错：
// 模型能事先知道"vendor 只能 create"，就不会写出必然被拒的提案。
var planEntityOps = map[string][]string{
	EntityVendor:        {OpCreate},
	EntityAdapterSpec:   {OpCreate, OpUpdate, OpDisable},
	EntityChannel:       {OpCreate, OpUpdate, OpDisable},
	EntityModel:         {OpCreate, OpUpdate, OpDisable},
	EntityModelBindings: {OpReplace, OpDisable},
}

// planEntityList 是实体与结构体的对应（顺序即文档顺序，也是测试的遍历源）。
var planEntityList = []struct {
	name string
	typ  reflect.Type
}{
	{EntityVendor, reflect.TypeOf(VendorData{})},
	{EntityAdapterSpec, reflect.TypeOf(AdapterSpecData{})},
	{EntityChannel, reflect.TypeOf(ChannelData{})},
	{EntityModel, reflect.TypeOf(ModelData{})},
	{EntityModelBindings, reflect.TypeOf(BindingData{})},
}

// planDictionaryRules 是结构体表达不了的跨实体规则（写"为什么"，不写字段表）。
var planDictionaryRules = []string{
	"新建模型必须同时给出 model_bindings，否则门户永远看不到它",
	"channel_key 只能指向本提案内 create 的渠道；已有渠道用 channel_id",
	"disable model_bindings 用 model + channel_id，不用 key",
	"is_free / is_default 是派生字段，不能写",
	"同一个实体在一条提案里只能出现一次",
	"adapter_spec 的 slug 来自 operations[].key（data.spec.slug 可省略，服务端会用 key 覆盖）",
	"operations 的顺序无所谓：服务端按 供应商→规格→渠道→模型→绑定 依赖重排",
	"update 的 data 只写要改的字段，省略 = 保持库内现值",
	"vendor 只支持 create；要改名请在管理台操作",
	"channel 不支持 adapter_options；adapter_config 只允许 auth_header / auth_prefix / path_overrides",
}

// PlanDictionary 是 schema_get(plan) 的唯一来源。
//
// 与 tool schema 同源（同一个结构体），组织成"按实体看字段"的形式：
// 模型能一次读懂"这个实体有哪些字段、哪个必填、能配哪些操作"。
// entity 为空 = 全部实体；指定实体时只回该实体（省上下文）。
func PlanDictionary(entity string) (map[string]any, bool) {
	entity = strings.TrimSpace(entity)
	if entity != "" {
		for _, e := range planEntityList {
			if e.name != entity {
				continue
			}
			out := map[string]any{
				"entity":     e.name,
				"op_allowed": planEntityOps[e.name],
				"fields":     planFields(e.name, e.typ),
			}
			if e.name == EntityAdapterSpec {
				out["spec_dsl"] = "字段见 schema_get(spec_dsl)"
			}
			return out, true
		}
		return nil, false
	}
	entities := map[string]any{}
	for _, e := range planEntityList {
		entities[e.name] = map[string]any{
			"op_allowed": planEntityOps[e.name],
			"fields":     planFields(e.name, e.typ),
		}
	}
	return map[string]any{
		"ops": map[string]string{
			OpCreate:  "新建（key 必填）",
			OpUpdate:  "修改（id 或 key + expect_version 必填；data 只提交要改的字段）",
			OpDisable: "停用（不删除：删除会丢历史归属）",
			OpReplace: "仅 model_bindings：整体替换绑定集合",
		},
		"entities": entities,
		"rules":    planDictionaryRules,
	}, true
}

// planFields 按结构体生成 {字段: {type, required?, note?}}。
func planFields(entity string, t reflect.Type) map[string]any {
	fields := map[string]any{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, omit, ptr := jsonName(f)
		if name == "" || name == "-" {
			continue
		}
		item := map[string]any{"type": typeName(f.Type)}
		if !ptr && !omit {
			item["required"] = true
		}
		if note := planFieldNotes[entity][name]; note != "" {
			item["note"] = note
		}
		fields[name] = item
	}
	return fields
}

func typeName(t reflect.Type) string {
	if t.Kind() == reflect.Ptr {
		return typeName(t.Elem()) + "（可选）"
	}
	if t.Name() == "RawMessage" {
		return "object"
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice:
		return "array<" + typeName(t.Elem()) + ">"
	case reflect.Map:
		return "object<string," + typeName(t.Elem()) + ">"
	case reflect.Struct:
		return "object"
	}
	return "any"
}
