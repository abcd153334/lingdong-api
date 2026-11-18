package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
)

// 本文件是"严格解析失败"的翻译层。
//
// 为什么需要它：`DisallowUnknownFields` 的报错是 `json: unknown field "slug"` ——
// 模型看到这句话只知道"有个字段不认识"，不知道**该写到哪**、**还允许写什么**，
// 只能整份重写再撞一次。把它翻成"不认识的字段 + 允许的字段 + 常见错位提示"，
// 模型通常一次就能改对；运营看工具卡也知道卡在哪。
//
// 三条实现约束：
//  1. **一次报全**：同一层的所有未知键一起给（不是挤牙膏），嵌套层也各自报；
//  2. **不改错误形状**：对外仍是 ValidationError{Index,Field,Message}，
//     本类型只在包内传递，工具层把 Path/Allowed 摊到结果里；
//  3. **不做二次真相源**：允许字段来自结构体反射，错位提示只写"语义指路"。

// fieldSite 是一处"不认识的键"。
type fieldSite struct {
	Path    string
	Unknown []string
	Allowed []string
}

// FieldSpecError 是严格解析失败的可行动形态。
type FieldSpecError struct {
	// Path 是第一个出错位置（如 data.slug / data.bindings[0]）。
	Path string
	// Unknown / Allowed 是第一个位置的未知键与该层允许键（工具结果直接用）。
	Unknown []string
	Allowed []string
	// Hint 是"这个字段应该写在哪"的提示（命中错位表时才有）。
	Hint string
	// Kind: unknown_field | type_mismatch | not_object
	Kind  string
	sites []fieldSite
}

func (e *FieldSpecError) Error() string { return e.Message() }

// Message 是给人和模型看的正文（不含 Path 前缀）。
func (e *FieldSpecError) Message() string {
	if e.Kind == "type_mismatch" || e.Kind == "not_object" {
		return e.Hint
	}
	parts := make([]string, 0, len(e.sites))
	for _, s := range e.sites {
		msg := fmt.Sprintf("不认识的字段 %s（允许的字段：%s）",
			quoteKeys(s.Unknown), strings.Join(s.Allowed, "、"))
		parts = append(parts, msg)
	}
	out := strings.Join(parts, "；")
	if e.Hint != "" {
		out += "。提示：" + e.Hint
	}
	return out
}

func quoteKeys(keys []string) string {
	q := make([]string, 0, len(keys))
	for _, k := range keys {
		q = append(q, `"`+k+`"`)
	}
	return strings.Join(q, "、")
}

// IsFieldSpecError 便于调用方判断（errors.As 的简写）。
func asFieldSpecError(err error) *FieldSpecError {
	var fe *FieldSpecError
	if errors.As(err, &fe) {
		return fe
	}
	return nil
}

// misplacedHints 是"这个字段不属于这里"的语义指路。
//
// 只写**模型真的会写错**的那几个：宁可少而准，不要堆一张看着很全的表
// （那张表本身就是第二份字段清单，会漂）。
var misplacedHints = map[string]map[string]string{
	EntityAdapterSpec: {
		"slug":      "规格标识写在 operations[].key，不要放在 data 里；data.spec.slug 可省略（服务端会用 key 覆盖）",
		"name":      "规格的展示名字段叫 label，不是 name",
		"endpoints": "端点定义在 data.spec.endpoints.<端点键> 里，不在 data 顶层",
		"path":      "路径写在 data.spec.endpoints.<端点键>.path",
		"auth":      "鉴权写在 data.spec.endpoints.<端点键>.auth",
		"version":   "版本由服务端维护，不能写",
		"status":    "状态由服务端维护，不能写",
	},
	EntityChannel: {
		"api_key":         "密钥写在 keys 数组里（可多条）",
		"key":             "密钥写在 keys 数组里（可多条）",
		"token":           "密钥写在 keys 数组里（可多条）",
		"baseUrl":         "字段名是 base_url（下划线）",
		"baseurl":         "字段名是 base_url（下划线）",
		"adapter_options": "不支持 adapter_options；L1 只能改 adapter_config 的 auth_header / auth_prefix / path_overrides，其余请人工配置",
	},
	EntityModel: {
		"price":           "按量计费用 input_price / output_price / cache_* ；按规格计费用 spec_prices",
		"per_image_price": "按张计费请用 spec_prices（档位键见 schema_get(spec_prices)）",
		"channel_id":      "模型不直接挂渠道；另写一条 replace model_bindings",
		"bindings":        "绑定单独作为一条 replace model_bindings 操作",
		"endpoint":        "字段名是 endpoints（字符串数组）",
		"is_free":         "is_free 是派生字段（由价格推导），不能写",
		"is_default":      "is_default 是派生字段，不能写",
	},
	EntityModelBindings: {
		"channel_key":    "channel_key 写在 bindings[i] 里",
		"channel_id":     "channel_id 写在 bindings[i] 里",
		"upstream_model": "upstream_model 写在 bindings[i] 里",
	},
	EntityVendor: {
		"id":   "供应商的主键就是 name，不需要单独给 id",
		"key":  "供应商只支持 create，主键是 name",
		"logo": "供应商只有 name 与 color 两个字段",
	},
}

// typeMaps 缓存"结构体 → {json 名: 字段类型}"，避免每次失败都反射一遍。
var typeMaps sync.Map

func jsonFieldMap(t reflect.Type) map[string]reflect.Type {
	if v, ok := typeMaps.Load(t); ok {
		return v.(map[string]reflect.Type)
	}
	m := make(map[string]reflect.Type, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := jsonName(f)
		if name == "" || name == "-" {
			continue
		}
		m[name] = f.Type
	}
	typeMaps.Store(t, m)
	return m
}

func derefType(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}

func isRawMessageType(t reflect.Type) bool {
	return t != nil && t.Name() == "RawMessage"
}

// decodeEntity 严格解析一个实体载荷，失败时给出可行动错误。
//
// path 是字段前缀（如 "data"），entity 用于查错位提示。
func decodeEntity(raw json.RawMessage, v any, path, entity string) error {
	ptr := reflect.TypeOf(v)
	if ptr == nil || ptr.Kind() != reflect.Ptr {
		// 传进来的都是指针；退化为原来的严格解析。
		return strictUnmarshal(raw, v)
	}
	t := derefType(ptr)

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return errors.New("缺少 data")
	}
	if trimmed[0] != '{' {
		return &FieldSpecError{Kind: "not_object", Path: path,
			Hint: path + " 必须是一个对象"}
	}

	var sites []fieldSite
	walkObject(trimmed, t, path, entity, &sites)
	if len(sites) > 0 {
		fe := &FieldSpecError{Kind: "unknown_field", sites: sites}
		fe.Path = sites[0].Path
		fe.Unknown = sites[0].Unknown
		fe.Allowed = sites[0].Allowed
		// 只有顶层错位才提示：嵌套层（bindings[i] 之类）看允许字段就够。
		if sites[0].Path == path {
			fe.Hint = hintFor(entity, sites[0].Unknown, path)
		}
		return fe
	}

	if err := strictUnmarshal(raw, v); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) {
			field := te.Field
			if field == "" {
				field = path
			} else {
				field = path + "." + field
			}
			return &FieldSpecError{Kind: "type_mismatch", Path: field,
				Hint: fmt.Sprintf("字段 %s 需要 %s 类型，收到的是 %s", field, te.Type.String(), te.Value)}
		}
		return err
	}
	return nil
}

// unknownFieldsIn 只报未知键、不做解码（用于 spec DSL 这类二级结构）。
func unknownFieldsIn(raw json.RawMessage, typ reflect.Type, path, entity string) *FieldSpecError {
	t := derefType(typ)
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	var sites []fieldSite
	walkObject(trimmed, t, path, entity, &sites)
	if len(sites) == 0 {
		return nil
	}
	fe := &FieldSpecError{Kind: "unknown_field", sites: sites}
	fe.Path = sites[0].Path
	fe.Unknown = sites[0].Unknown
	fe.Allowed = sites[0].Allowed
	fe.Hint = hintFor(entity, sites[0].Unknown, sites[0].Path)
	return fe
}

func walkObject(raw json.RawMessage, t reflect.Type, path, entity string, sites *[]fieldSite) {
	t = derefType(t)
	if t == nil || t.Kind() != reflect.Struct || isRawMessageType(t) {
		return
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	fields := jsonFieldMap(t)
	var unknown []string
	for k, v := range m {
		ft, ok := fields[k]
		if !ok {
			unknown = append(unknown, k)
			continue
		}
		walkValue(v, ft, path+"."+k, entity, sites)
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		allowed := make([]string, 0, len(fields))
		for k := range fields {
			allowed = append(allowed, k)
		}
		sort.Strings(allowed)
		*sites = append(*sites, fieldSite{Path: path, Unknown: unknown, Allowed: allowed})
	}
}

func walkValue(raw json.RawMessage, ft reflect.Type, path, entity string, sites *[]fieldSite) {
	ft = derefType(ft)
	if ft == nil || isRawMessageType(ft) {
		// json.RawMessage 是不透明载荷（如 data.spec）：由它自己的解析器负责。
		return
	}
	switch ft.Kind() {
	case reflect.Struct:
		walkObject(raw, ft, path, entity, sites)
	case reflect.Slice:
		elem := derefType(ft.Elem())
		if elem == nil || elem.Kind() != reflect.Struct {
			return
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return
		}
		for i := range arr {
			walkObject(arr[i], elem, fmt.Sprintf("%s[%d]", path, i), entity, sites)
		}
	case reflect.Map:
		elem := derefType(ft.Elem())
		if elem == nil || elem.Kind() != reflect.Struct {
			return
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			walkObject(m[k], elem, path+"."+k, entity, sites)
		}
	}
}

// hintFor 查错位提示（只在顶层 path 生效，嵌套层由"允许的字段"自解释）。
func hintFor(entity string, unknown []string, path string) string {
	if entity == "" || len(unknown) == 0 {
		return ""
	}
	m := misplacedHints[entity]
	if m == nil {
		return ""
	}
	hints := make([]string, 0, len(unknown))
	for _, k := range unknown {
		if h, ok := m[k]; ok {
			hints = append(hints, h)
		}
	}
	return strings.Join(hints, "；")
}
