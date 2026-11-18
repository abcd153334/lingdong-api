package agent

import (
	"reflect"
	"sort"
	"strings"

	specdsl "lingdong-api/internal/relay/spec"
)

// 本文件是 spec DSL 的**机器契约**：schema_get(section="spec_dsl") 的唯一来源。
//
// 与 plan_schema.go 同一条铁律：字段清单由结构体反射生成，知识包 md 只写
// "为什么/哪里容易错"，不写字段表。md 里出现字段表就等于第二份真相源，
// 而它过期时不会报错 —— 只会让模型照着过期清单写出被拒绝的规格。
//
// 为什么不用 specdsl 内部的校验函数反向生成：那些函数是命令式的 if 判断，
// 生成不出"字段 → 类型"的表；而限额/枚举是常量，直接引用即可（同一份值）。

// specStructs 是 DSL 的层级结构（顺序即阅读顺序）。
var specStructs = []struct {
	key   string
	title string
	typ   reflect.Type
}{
	{"spec", "规格顶层", reflect.TypeOf(specdsl.Spec{})},
	{"endpoint", "端点（endpoints.<端点键>）", reflect.TypeOf(specdsl.Endpoint{})},
	{"auth", "鉴权（endpoint.auth）", reflect.TypeOf(specdsl.Auth{})},
	{"body", "请求体重建（endpoint.body）", reflect.TypeOf(specdsl.BodySpec{})},
	{"array", "数组映射（body.arrays.<名字>）", reflect.TypeOf(specdsl.ArraySpec{})},
	{"extract", "响应取值（endpoint.extract）", reflect.TypeOf(specdsl.Extract{})},
	{"errors", "成功判据（endpoint.errors）", reflect.TypeOf(specdsl.ErrSpec{})},
	{"async", "异步轮询（endpoint.async）", reflect.TypeOf(specdsl.AsyncSpec{})},
	{"request_field", "扩展字段（endpoint.request_fields[]）", reflect.TypeOf(specdsl.RequestField{})},
	{"url_selector", "产物选择器（extract.result_urls）", reflect.TypeOf(specdsl.URLSelector{})},
}

// specFieldNotes 只放"看名字猜不出来"的约束。类型与必填来自结构体反射。
var specFieldNotes = map[string]map[string]string{
	"spec": {
		"slug":      "格式 " + specdsl.SlugPattern + "；提案里由 operations[].key 覆盖，可省略",
		"label":     "展示名，最多 32 字",
		"vendor":    "vendors.id，只影响展示与归组，不参与选路",
		"endpoints": "键必须来自 schema_get(endpoints) 的清单；轮询端点是 <端点键>@poll",
	},
	"endpoint": {
		"method":                "POST / GET；省略则取该端点的事实值",
		"path":                  "上游路径，必须以 / 开头；轮询端点里恰好出现一个 {task_id}；提交端点可用 {upstream_model}（绑定里的上游型号名）与 request_fields 里的字段名作占位符",
		"auth":                  "缺省 = 渠道密钥放 Authorization: Bearer",
		"query":                 "常量查询参数，最多 8 个（值只能是字符串）",
		"body":                  "请求体重建规则；passthrough 时只能有 drop",
		"passthrough":           "原样转发请求体，仅 chat/completions 允许",
		"extract":               "从响应里取任务号/状态/产物；同步端点至少要一个结果字段",
		"errors":                "成功判据；缺省 HTTP 2xx 即成功",
		"async":                 "只出现在提交端点：声明轮询端点与形态（task / bridge）",
		"status_map":            "只出现在 @poll 端点：上游状态值 → 平台五态（queued/running/succeeded/failed/canceled）；succeeded/failed 必须有，其余缺省按运行中处理",
		"content_type":          "覆盖**请求**体类型，缺省 application/json",
		"response_content_type": "桥接下载体缺少 Content-Type 时的兜底响应类型（如 audio/wav）",
		"omit_model":            "不向上游写 model 字段（上游请求体里没有 model 时必填 true）",
		"request_fields":        "本接入额外接受的调用方参数（≤16）；只声明契约，真正发给上游仍靠 body.copy/arrays 与 path 占位符",
	},
	"auth": {
		"in":     "header / query / body；缺省 header",
		"name":   "in=header 时必须在 auth_headers 白名单内",
		"prefix": "如 \"Bearer \"（含空格）；缺省不加前缀",
	},
	"body": {
		"const":  "静态常量，最多 32 个；不能写计费维度字段（见 reserved_const_keys）",
		"copy":   "把调用方字段复制到上游字段；值可以是 \"$req.<字段>\" 或候选数组（按序取第一个有值的），最多 32 个",
		"arrays": "数组映射，最多 8 个",
		"drop":   "要丢弃的调用方字段；只有 passthrough 时能用",
	},
	"array": {
		"from": "必须是 $req.<字段>",
		"max":  "最多取几项，0–16；上游对张数通常有硬上限",
		"item": "每个元素的目标形状，值只能用 $value",
	},
	"extract": {
		"task_id":       "提交端点必填：上游任务号",
		"status":        "@poll 端点必填：任务状态",
		"progress":      "@poll 端点可选：进度 0-100",
		"error":         "@poll 端点可选：失败原因",
		"result_urls":   "多产物链接（$.data[*].url），顺序即产物顺序；可写成 {path,where} 按元素字段过滤（如 where.type=audio）",
		"result":        "同步端点的单产物链接",
		"text":          "同步端点的文本结果（歌词等）",
		"duration":      "**产物**时长取值路径，按秒计费用；必须是媒体时长，不是任务耗时（AutoDL 的 data.duration 是任务执行秒数，接了会超收数十倍）",
		"duration_unit": "duration 的单位：secs（缺省）/ ms；只有声明了 duration 才能写",
	},
	"errors": {
		"success":      "http_2xx / body_code",
		"code_path":    "success=body_code 时必填：业务码所在路径",
		"ok_codes":     "success=body_code 时必填：视为成功的码值",
		"message_path": "错误信息所在路径（用于把上游原话回给调用方）",
	},
	"request_field": {
		"name":     "调用方传的字段名（小写字母开头）；不能与平台契约字段同名",
		"type":     "string / number / integer / boolean / url",
		"desc":     "给用户看的说明，会进公网文档；不要写上上游地址或密钥前缀；url 类型必填说明",
		"enum":     "只有 string 可用",
		"default":  "与 required 互斥：必填字段不能有默认值",
		"required": "缺了平台直接 400（文案用 desc），比等上游拒绝再折成 503 快得多",
		"example":  "文档示例值，不参与任何校验",
		"max_len":  "字符串/URL 的最大长度，0 = 不限制",
	},
	"url_selector": {
		"path":  "取值路径，where 非空时必须包含 [*]",
		"where": "元素内字段精确相等过滤（≤4 条）；用于 results 混排时只取音频/只取图片",
	},
	"async": {
		"mode":         "task（默认，平台任务队列轮询）/ bridge（本次请求内等到结果）",
		"poll":         "必须恰好等于 <端点键>@poll，且该 poll 端点必须存在于同一规格",
		"timeout_secs": "只属于 bridge：等待上限秒数，缺省 120，最大 900",
		"interval_ms":  "只属于 bridge：轮询间隔毫秒，缺省 1000，范围 200–5000",
	},
}

// specRules 是结构体表达不了的跨字段约束（写"为什么"，不写字段表）。
var specRules = []string{
	"端点键必须是 schema_get(endpoints) 里的键；multipart 端点（audio/transcriptions 等）不支持规格，属于必须写代码适配",
	"passthrough 只允许 chat/completions；它只做原样转发，不能与 body.const/copy/arrays 同时出现",
	"body.drop 只能与 passthrough 同时使用（非透传时字段由 body 规则重建，drop 无意义）",
	"提交端点声明 async 时必须给 extract.task_id；async.poll 指向的端点必须真实存在",
	"异步提交端点（PhaseSubmit）只能用 mode=task；同步端点声明 async 时必须 mode=bridge（提交后在本次请求内等到结果）",
	"bridge 形态额外要求：@poll 端点给出 extract.result 或 result_urls，提交端点给 extract.task_id；且只有二进制端点与图像生成端点能用 bridge（其余响应契约承载不了产物）",
	"@poll 端点必须给 extract.status 与 status_map，且不接受 body / passthrough / 再声明 async",
	"status_map 必须给 succeeded 与 failed（终态判定）；queued/running/canceled 可省略；上游状态连续 3 次不认识会判配置错（不会再空等到超时）；同一个上游状态值不能同时映射到两个平台态",
	"同步端点至少要给 extract.result / result_urls / text 之一，否则拿不到产物",
	"auth.name 必须在 auth_headers 白名单内；不允许 Host / X-Forwarded-For 这类会改变语义的头",
	"计费维度字段不能写进 body.const（见 reserved_const_keys），否则同一模型的不同渠道会按不同口径计费",
	"规格 slug 创建后不可改；提案里写 operations[].key，服务端会覆盖到 spec.slug",
	"非标参数不要硬塞进已有字段：把上游多要的东西声明成 request_fields，再用 body.copy 或 path 占位符映射（把音色名当参考音频 URL 发出去是真实事故）",
	"同族多个工作流/型号只写一份规格：path 用 {upstream_model}，换 workflow 只改绑定的 upstream_model，不要复制整份规格",
	"request_fields 声明了就必须被消费（出现在 body.copy/arrays 或 path 占位符里），否则保存期会报「声明了但不会发给上游」",
	"上游 results 混排（封面/字幕/音轨）时必须用 result_urls.where 过滤，否则会取到第 0 个非目标产物",
}

// specEnums 是枚举值（全部引用常量，不写字面量）。
func specEnums() map[string]any {
	headers := specdsl.AuthHeaders()
	sort.Strings(headers)
	reserved := specdsl.ReservedConstKeys()
	sort.Strings(reserved)
	return map[string]any{
		"method":              []string{"POST", "GET"},
		"auth_in":             []string{"header", "query", "body"},
		"errors_success":      []string{"http_2xx", "body_code"},
		"auth_headers":        headers,
		"reserved_const_keys": reserved,
		"status_states":       specdsl.StatusStates,
		"async_modes":         []string{specdsl.AsyncModeTask, specdsl.AsyncModeBridge},
		"slug_pattern":        specdsl.SlugPattern,
		"request_field_types": specdsl.RequestFieldTypes,
		"duration_units":      []string{"secs", "ms"},
	}
}

// specLimits 是限额（直接引用 specdsl 常量：改动只改那一处）。
func specLimits() map[string]any {
	return map[string]any{
		"max_endpoints":               specdsl.MaxEndpoints,
		"max_const_keys":              specdsl.MaxConstKeys,
		"max_copy_keys":               specdsl.MaxCopyKeys,
		"max_arrays":                  specdsl.MaxArrays,
		"max_array_items":             specdsl.MaxArrayItems,
		"max_static_query":            specdsl.MaxStaticQuery,
		"max_path_len":                specdsl.MaxPathLen,
		"max_spec_bytes":              specdsl.MaxSpecBytes,
		"max_slug_len":                specdsl.MaxSlugLen,
		"max_label_runes":             specdsl.MaxLabelRunes,
		"max_json_path_depth":         specdsl.MaxJSONPathDeep,
		"max_copy_candidates":         specdsl.MaxCopyCandidates,
		"bridge_timeout_default_secs": specdsl.DefaultBridgeTimeoutSecs,
		"bridge_timeout_max_secs":     specdsl.MaxBridgeTimeoutSecs,
		"bridge_interval_min_ms":      specdsl.MinBridgeIntervalMs,
		"bridge_interval_max_ms":      specdsl.MaxBridgeIntervalMs,
		"bridge_max_artifact_bytes":   specdsl.BridgeMaxArtifactBytes,
		"max_request_fields":          specdsl.MaxRequestFields,
		"max_request_field_enum":      specdsl.MaxRequestFieldEnum,
		"max_path_vars":               specdsl.MaxPathVars,
		"max_selector_where":          specdsl.MaxSelectorWhere,
	}
}

// specParts 是合法的 part 取值（schema_get(spec_dsl, part)）。
var specParts = []string{"all", "spec", "endpoint", "auth", "body", "array", "extract", "errors", "async", "request_field", "url_selector", "limits", "enums"}

// SpecDictionary 生成 DSL 字段字典。part 为空等价于 all。
//
// 设计取舍：按 part 取是为了省上下文 —— 全套约 3KB，只问 endpoint 时不到 1KB。
func SpecDictionary(part string) (map[string]any, bool) {
	part = strings.ToLower(strings.TrimSpace(part))
	if part == "" {
		part = "all"
	}
	valid := false
	for _, p := range specParts {
		if p == part {
			valid = true
			break
		}
	}
	if !valid {
		return nil, false
	}
	out := map[string]any{"part": part}
	if part == "limits" || part == "all" {
		out["limits"] = specLimits()
	}
	if part == "enums" || part == "all" {
		out["enums"] = specEnums()
	}
	if part == "all" || isSpecStructPart(part) {
		structsOut := map[string]any{}
		for _, s := range specStructs {
			if part != "all" && s.key != part {
				continue
			}
			structsOut[s.key] = map[string]any{
				"title":  s.title,
				"fields": specFields(s.key, s.typ),
			}
		}
		out["structures"] = structsOut
	}
	// rules 是**跨字段**约束（status_map 必须给 succeeded+failed、bridge 只允许
	// 二进制/图像端点、request_fields 声明了就必须被消费…），与"问哪一层"无关：
	// 只问 limits 或 enums 的调用者也必须看到它们，否则会在完全看不到约束的情况下
	// 写出一份"字段全对、组合违规"的规格。代价是这两类回执多约 1.5KB。
	out["rules"] = specRules
	return out, true
}

func isSpecStructPart(part string) bool {
	for _, s := range specStructs {
		if s.key == part {
			return true
		}
	}
	return false
}

// specFields 按结构体生成 {字段: {type, required?, note?}}。
func specFields(key string, t reflect.Type) map[string]any {
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
		if note := specFieldNotes[key][name]; note != "" {
			item["note"] = note
		}
		fields[name] = item
	}
	return fields
}
