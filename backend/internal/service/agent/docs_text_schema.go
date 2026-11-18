package agent

import (
	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/service/docdocs"
)

// docsTextDictionary 是 `schema_get(section="docs_text")` 的返回体。
//
// 这里**不再列字段与上限**：v7 的字段形状由工具参数 schema 定义
// （`doc_endpoint_update` / `doc_fields_write`），写一份字段表在这里就是第三份真相，
// 而它一定会漂（旧版就漂过：md 里写着 member_notes ≤80 字，代码里根本没这个校验）。
//
// 留在这一节的只有三件结构体表达不了的事：
//  1. 流程（先读后写、数组整体替换、发布不是你的动作）；
//  2. 写作判据（给谁看、不写什么）；
//  3. 体检码目录（发布闸门到底拦什么，来自 docdocs.ProblemCatalog 唯一来源）。
func docsTextDictionary() map[string]any {
	return map[string]any{
		"flow": []string{
			"doc_tree 找到 node/key（可先按 section 过滤）",
			"doc_endpoint_get 读现有内容 + bindable_transports + problems（改之前必须读）",
			"doc_endpoint_update 只提交要改的键；fields/response/examples/presets/errors 是**整体替换**，不是合并",
			"doc_fields_write 只换请求字段（字段很多时用它，避免整篇回传）",
			"doc_check 复核：blocking 必须为 0，否则这篇发不出去",
			"发布由人工在管理台点击；doc_* 工具没有发布能力（也不要要求运营「帮忙发一下」来跳过检查）",
		},
		"field_shape": "字段/响应变体/示例/预设/错误码的形状见对应工具的 parameters（JSON Schema）。不要凭记忆编字段名；" +
			"位置只允许 body/query/path/form/header。",
		"rules": []string{
			"只写**平台真实登记过**的端点：transport_doc_id 只能取自 doc_endpoint_get 的 bindable_transports，未绑定的文档只能存草稿",
			"异步端点（accepts_async=true）必须同时有 async 字段与 202 响应变体，否则发布被拦",
			"示例体写成普通 JSON 对象即可，键序按书写顺序保存；示例地址一律用 https://api.example.com",
			"不要写金额：价格随时会变，文档里写死必然失真",
			"不要用内部视角用词（后台/运营配置/渠道/上游这类），见 forbidden_words",
			"每次改动只动需要的键：整篇回传会把没读到的字段一起写回，等于覆盖别人刚改的内容",
		},
		"blocking_codes":  docdocs.ProblemCatalog(),
		"forbidden_words": apidoc.ForbiddenWords(),
		"note":            "字段与上限的唯一来源是工具参数 schema；体检码的唯一来源是 docdocs.ProblemCatalog。本节只写判据。",
	}
}
