package docdocs

// ProblemDef 是一条体检结论的**目录项**（编码 + 人话 + 是否阻断）。
//
// 为什么需要目录：管理台红点、AI 的 doc_check、发布返回的 problems 都只带 code，
// 而"这个 code 到底要我改什么"必须有一处能查到。散落在各处的解释句会漂。
type ProblemDef struct {
	Code     string `json:"code"`
	Blocking bool   `json:"blocking"`
	Meaning  string `json:"meaning"`
	// Fix 是"怎么改"（一句话，能直接照着做）。
	Fix string `json:"fix"`
}

// problemCatalog 是全部 code 的唯一说明处（顺序 = 阅读顺序）。
var problemCatalog = []ProblemDef{
	{"E_UNBOUND", true, "没绑定平台已登记的端点", "用 doc_endpoint_get 看 bindable_transports，选一个填进 transport_doc_id（未绑定的文档只能存草稿）"},
	{"E_UNKNOWN_TRANSPORT", true, "transport_doc_id 在契约里不存在", "从 bindable_transports 里原样复制 doc_id，不要手写或改写大小写"},
	{"E_METHOD_DRIFT", true, "文档写的方法与契约不一致", "把 method 改成契约值（doc_endpoint_get 的 bindable_transports 里有）"},
	{"E_PATH_DRIFT", true, "文档写的路径与契约不一致", "把 path 改成契约值；不要为了「好看」改写路径"},
	{"E_ASYNC_FIELD_MISSING", true, "该端点支持 async:true，但请求字段里没有 async", "在 fields 里加一条 name=async、type=boolean 的字段"},
	{"E_ASYNC_UNEXPECTED", true, "该族的协议不支持 async，但页面写了 async 字段或任务对象", "删掉 async 字段与含 task_id 的响应变体（这一族的上游没有任务号可拿）"},
	{"E_ASYNC_VARIANT_MISSING", true, "该端点支持 async:true，但没有 202 响应变体", "在 response.variants 里加一个 status=202 的变体（返回任务号等）"},
	{"E_PATH_INVALID", true, "路径不以 / 开头或含空白", "写成 /v1/xxx 形式"},
	{"E_NO_SUCCESS_VARIANT", true, "没有任何 2xx 响应变体", "至少加一个 status=200（或 202）的变体"},
	{"E_POST_NO_FIELDS", true, "契约里有请求字段，文档里一个都没有", "重新保存这一篇以重建字段表（或用 doc_fields_write 补回来）"},
	{"E_FIELD_NAME_EMPTY", true, "有字段没写名字", "补上字段名，或删掉这一行"},
	{"E_FIELD_IN_UNKNOWN", true, "字段的 in 不在允许集合里", "只允许 body/query/path/form/header"},
	{"E_FIELD_REQUIRED_NOTE", true, "条件必填但没写条件", "填 required_note，说明什么条件下必须传"},
	{"E_FIELD_OPTIONAL_FORBIDDEN", true, "path 参数被标成可选", "path 参数一律 required"},
	{"E_FIELD_DUP", true, "同位置下有重名字段", "合并或改名（同名会让参数表出现两行一样的）"},
	{"E_NODE_NO_ENDPOINT", true, "节点下没有已发布的接口", "先发布节点下的接口（或先保存再发布），空节点对读者没有意义"},
	{"E_NODE_GUIDE_EMPTY", true, "说明页没有正文", "在 blocks 里补至少一段（标题 + 文字或代码块）"},
	{"E_TEXT_FORBIDDEN", true, "文案里出现内部用词或上游平台名", "改用对外的说法：写「模型提供方」而不是具体平台名，不写内部代号"},
	{"E_EXTERNAL_HOST", true, "文案里出现第三方域名", "示例地址一律写 https://api.example.com，不要出现别的站点"},
}

// ProblemCatalog 返回体检码目录（AI 与管理台共用）。
func ProblemCatalog() []ProblemDef {
	out := make([]ProblemDef, len(problemCatalog))
	copy(out, problemCatalog)
	return out
}

// ProblemMeaning 查一个 code 的含义（未知 code 返回空串，由调用方决定怎么说）。
func ProblemMeaning(code string) string {
	for _, p := range problemCatalog {
		if p.Code == code {
			return p.Meaning
		}
	}
	return ""
}
