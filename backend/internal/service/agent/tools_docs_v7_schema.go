package agent

import "encoding/json"

// 文档工具的**参数契约**（3 个带字段表的工具用）。
//
// 字段/响应/示例的形状与 `docdocs.FieldView` 等结构体一一对应：这里手写
// （而不是反射生成）是因为「必填」需要按模型直觉来定 —— 反射会把所有非
// omitempty 的列都标成 required，模型就得为每一行补一堆空串。
// `tools_docs_v7_schema_test.go` 用反射比对属性名与结构体 json tag，
// 结构体加了列而这里漏掉会直接红。

func docFieldSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"description":          "一个请求字段（或返回字段）。数组顺序 = 页面上参数表的顺序。",
		"properties": map[string]any{
			"name":     map[string]any{"type": "string", "description": "字段名，如 model / prompt / size"},
			"type":     map[string]any{"type": "string", "enum": []string{"string", "integer", "number", "boolean", "array", "object", "file", "binary", "none", "any"}},
			"in":       map[string]any{"type": "string", "enum": []string{"body", "query", "path", "form", "header"}, "description": "参数位置；返回字段不用填"},
			"required": map[string]any{"type": "string", "enum": []string{"required", "optional", "conditional"}},
			"required_note": map[string]any{
				"type": "string", "description": "conditional 必填时必须写清条件，否则发布被拦",
			},
			"default": map[string]any{"type": "string", "description": "默认值（没有就留空）"},
			"desc":    map[string]any{"type": "string", "description": "说明：填什么、影响什么、边界与例外。可多行（换行即分段）"},
			"enum": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"properties": map[string]any{
						"value":      map[string]any{"type": "string"},
						"desc":       map[string]any{"type": "string", "description": "这个值是什么意思"},
						"deprecated": map[string]any{"type": "boolean"},
					},
					"required": []string{"value"},
				},
				"description": "枚举候选值（值后面那句说明就是页面上的「可选值」）",
			},
			"value_source": map[string]any{"type": "string", "description": "枚举来源说明（static/模型清单等），一般留空"},
			"constraints": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"properties": map[string]any{
						"kind":  map[string]any{"type": "string"},
						"value": map[string]any{"type": "number"},
						"text":  map[string]any{"type": "string"},
					},
					"required": []string{"kind"},
				},
			},
			"tier":        map[string]any{"type": "string", "description": "档位（standard/basic/…），一般留空"},
			"passthrough": map[string]any{"type": "boolean", "description": "是否原样透传给上游"},
			"sketch_skip": map[string]any{"type": "boolean", "description": "返回字段专用：勾上表示不进响应骨架"},
			"example":     map[string]any{"description": "该字段在示例里的真值（任意 JSON）"},
		},
		"required": []string{"name", "type"},
	}
}

func docKVSchema(desc string) map[string]any {
	// 允许对象也允许 [{k,v}]：模型写对象最自然，管理台回传的是 KV 数组。
	return map[string]any{
		"type":        "object",
		"description": desc + "（写成普通 JSON 对象即可；键序会按你写的顺序保存）",
	}
}

func docResponseSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"kind":         map[string]any{"type": "string", "enum": []string{"json", "sse", "binary", "none"}},
			"envelope":     map[string]any{"type": "boolean", "description": "是否包在 {code,data,message} 里"},
			"content_type": map[string]any{"type": "string"},
			"variants": map[string]any{
				"type":        "array",
				"description": "状态码变体；异步端点必须有 202（否则发布被拦）",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"properties": map[string]any{
						"status":       map[string]any{"type": "integer"},
						"label":        map[string]any{"type": "string", "description": "如「成功」「已受理」"},
						"content_type": map[string]any{"type": "string"},
						"kind":         map[string]any{"type": "string", "enum": []string{"json", "sse", "binary", "none"}, "description": "这一条响应的线格式（202 是 json，下载产物是 binary）"},
						"envelope":     map[string]any{"type": "boolean", "description": "这一条响应是否包在 {code,data,message} 里"},
						"returns": map[string]any{
							"type": "array", "items": docFieldSchema(),
							"description": "该状态码下的返回字段",
						},
						"example": docKVSchema("响应示例"),
						"raw":     map[string]any{"type": "string", "description": "SSE 等非 JSON 响应的原文"},
						"notes":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					},
					"required": []string{"status", "label"},
				},
			},
		},
		"required": []string{"variants"},
	}
}

// docSamplesSchema 是一段示例代码（四语言，四段同源生成）。
//
// 里面只允许 `{{base_url}}` 占位符，投影期统一替换成站点地址 ——
// 写死域名等于换域名后整站示例都在教用户调旧地址。
func docSamplesSchema(desc string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"description":          desc,
		"properties": map[string]any{
			"curl":   map[string]any{"type": "string"},
			"python": map[string]any{"type": "string"},
			"node":   map[string]any{"type": "string"},
			"go":     map[string]any{"type": "string"},
		},
	}
}

func docPresetsSchema() map[string]any {
	return map[string]any{
		"type": "array",
		"items": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"key":     map[string]any{"type": "string", "description": "标识，节点内唯一"},
				"label":   map[string]any{"type": "string"},
				"summary": map[string]any{"type": "string"},
				"body":    docKVSchema("该用法的请求体"),
				"fields":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"uploads": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"note":    map[string]any{"type": "string"},
				"samples": docSamplesSchema("这个用法的四段代码（留空=页面不显示）"),
			},
			"required": []string{"key", "label"},
		},
	}
}

func docErrorsSchema() map[string]any {
	return map[string]any{
		"type": "array",
		"items": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"status": map[string]any{"type": "integer"},
				"code":   map[string]any{"type": "string"},
				"desc":   map[string]any{"type": "string", "description": "什么情况下出现、怎么处理"},
				"source": map[string]any{"type": "string", "description": "留空即记为本次写入方；从 doc_endpoint_get 读到的值要原样带回来"},
			},
			"required": []string{"status", "code"},
		},
	}
}

func docEndpointCreateParams() json.RawMessage {
	m := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"node":    map[string]any{"type": "string"},
			"key":     map[string]any{"type": "string", "description": "同一节点内唯一，创建后不可改"},
			"name":    map[string]any{"type": "string"},
			"summary": map[string]any{"type": "string"},
			"transport_doc_id": map[string]any{
				"type":        "string",
				"description": "平台已登记端点（如 images/generations）；留空=只能存草稿，发布会被拒",
			},
			"method":        map[string]any{"type": "string"},
			"path":          map[string]any{"type": "string"},
			"run_mode":      map[string]any{"type": "string", "enum": []string{"json", "sse", "binary", "none"}},
			"fields":        map[string]any{"type": "array", "items": docFieldSchema()},
			"samples":       docSamplesSchema("主示例的四段代码（留空=页面不显示代码块）"),
			"samples2":      docSamplesSchema("第二种写法的四段代码"),
			"example_title": map[string]any{"type": "string"},
			"example_title2": map[string]any{
				"type": "string", "description": "第二段示例的场景名（如「流式输出」）",
			},
		},
		"required": []string{"node", "key", "name"},
	}
	b, _ := json.Marshal(m)
	return b
}

func docEndpointUpdateParams() json.RawMessage {
	m := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"node":             map[string]any{"type": "string"},
			"key":              map[string]any{"type": "string"},
			"version":          map[string]any{"type": "integer", "description": "读到的 version；不一致返回 version_conflict"},
			"name":             map[string]any{"type": "string"},
			"summary":          map[string]any{"type": "string"},
			"method":           map[string]any{"type": "string"},
			"path":             map[string]any{"type": "string"},
			"transport_doc_id": map[string]any{"type": "string"},
			"aliases":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"run_mode":         map[string]any{"type": "string", "enum": []string{"json", "sse", "binary", "none"}},
			"run_hint":         map[string]any{"type": "string"},
			"max_body":         map[string]any{"type": "string"},
			"hidden":           map[string]any{"type": "boolean"},
			"sort":             map[string]any{"type": "integer"},
			"fields":           map[string]any{"type": "array", "items": docFieldSchema()},
			"response":         docResponseSchema(),
			"samples":          docSamplesSchema("主示例的四段代码"),
			"samples2":         docSamplesSchema("第二种写法的四段代码"),
			"example_title":    map[string]any{"type": "string"},
			"example_title2": map[string]any{
				"type": "string", "description": "第二段示例的场景名",
			},
			"presets": docPresetsSchema(),
			"errors":  docErrorsSchema(),
		},
		"required": []string{"node", "key"},
	}
	b, _ := json.Marshal(m)
	return b
}

func docFieldsWriteParams() json.RawMessage {
	m := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"node":    map[string]any{"type": "string"},
			"key":     map[string]any{"type": "string"},
			"version": map[string]any{"type": "integer", "description": "先 doc_endpoint_get 读到的 version（乐观锁，必填）"},
			"fields":  map[string]any{"type": "array", "items": docFieldSchema()},
		},
		"required": []string{"node", "key", "version", "fields"},
	}
	b, _ := json.Marshal(m)
	return b
}
