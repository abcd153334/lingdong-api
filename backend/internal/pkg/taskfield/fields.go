// Package taskfield 是**异步任务对象对外字段表的唯一声明**。
//
// 为什么单独成包：同一份任务对象出现在两套对外文档里 ——
// 文档站的 `GET /v1/tasks/{id}`（internal/apidoc）与开放接口参考的
// `tasks.list / tasks.detail / tasks.cancel / usage.detail.task`（internal/apicontract）。
// 两边以前各写一遍，已经漂移：`id` 一处 string 一处 integer、`created_at`
// 一处 Unix 秒一处 RFC3339、`contents[].url` 一处有一处没有、`result_url`
// 两处都有而运行期根本不返回。漂移的字段表比没有更糟：接入方会照着它写解析
// 分支，然后拿到永远 undefined 的键。
//
// 为什么不把表放在其中一边：apidoc 依赖 billing → apikey → apicontract，
// 让 apicontract 反过来 import apidoc 就是导入环；而让 apidoc import apicontract
// 会把「文档站依赖开放接口参考」这条方向锁死，两边的端点清单若要合并就再也转不回来。
// 放成叶子包，两边各自派生一次，方向中立。
//
// 唯一的真值仍是运行期的 taskview.View：门禁在
// `service/taskview/view_shape_test.go`（比对完整路径集合 + 序列化键集合），
// 这里加字段而结构体没有（或反之）会立刻红。
//
// 只放**对外可见**字段：`upstream_task_id` / `channel_id` / `platform` /
// `error_msg` 属于供应链与排障信息，只在管理台出现，永远不进这份表。
package taskfield

import (
	"lingdong-api/internal/pkg/modality"
)

// Field 是一个对外字段。
//
// Name 是线上真实键，嵌套用点号（`contents[].url`）——这套写法同时被字段表、
// 运行面板锚点、Markdown 导出消费，必须逐字一致。
type Field struct {
	Name string
	// Type 取值与 apidoc/apicontract 的 Type 词表一致
	// （string / integer / number / boolean / array / object）。
	Type string
	Desc string
	// Note 是「会踩的坑」，可空。
	Note string
	// Enum 是封闭取值（非空即静态枚举）。
	Enum []string
	// Example 是示例里的真值（nil = 由消费方按类型给占位）。
	//
	// 为什么不能只给占位：读者要判断「任务号是字符串还是数字」「时间戳是秒还是
	// 毫秒」，`"<string>"` 与 `0` 一个都答不了。
	Example any
}

// ExampleID 是示例里的任务号，多处复用（contents 的 url 里也带它）。
//
// 导出而不是各文档写一遍字面量：任务号换了形态（十进制 → `task_` + ULID）之后，
// 两份文档里的示例会一处改一处漏，而示例是接入方唯一会照抄的东西。
const ExampleID = "task_01J9HA7JPQ9A0Z6JZ3V8M9W6PZ"

// Fields 返回任务对象的完整字段表。
//
// 顺序即示例键序：先标识、再状态、再时间、再错误、最后产物与用量。
func Fields() []Field {
	return []Field{
		{Name: "id", Type: "string", Desc: "任务 ID（`task_` 前缀的不透明字符串）。查询进度、下载产物都用它。",
			Note:    "不是数字，别按整数解析；原样回传即可。",
			Example: ExampleID},
		{Name: "object", Type: "string", Desc: "固定为 task。", Example: "task"},
		{Name: "status", Type: "string", Desc: "任务状态。",
			Enum:    []string{"queued", "running", "succeeded", "failed", "expired", "canceled"},
			Example: "queued"},
		{Name: "model", Type: "string", Desc: "模型 ID。", Example: "MiniMax-H3"},
		{Name: "modality", Type: "string", Desc: "模型大类。",
			Enum:    modality.Copy(),
			Example: "video"},
		{Name: "kind", Type: "string", Desc: "任务子类型。",
			Note:    "timeout 表示轮询网络失败超限；其余与 modality 同值。",
			Example: "video"},
		{Name: "model_icon_url", Type: "string", Desc: "模型图标地址（绝对地址，可能为空）。",
			Example: "https://api.example.com/icons/minimax.png"},
		{Name: "progress", Type: "integer", Desc: "进度百分比：排队 0、运行 50、终态 100。",
			Example: 0},
		{Name: "created_at", Type: "integer", Desc: "创建时间（Unix 秒）。", Example: 1762318830},
		{Name: "completed_at", Type: "integer", Desc: "完成时间（Unix 秒）。只有终态才有。",
			Example: 1762319000},
		{Name: "deadline_at", Type: "integer", Desc: "超时时间（Unix 秒）。", Example: 1762322400},
		// error 恒为对象（成功是 null）：缺字段与 null 在客户端是两种分支，
		// 少下发一次就要多写一个 ?. —— 所以形状写死，不给「有时是字符串」的可能。
		{Name: "error", Type: "object", Desc: "失败原因；成功时为 null（恒为对象，不是字符串）。"},
		{Name: "error.message", Type: "string", Desc: "失败分类文案。",
			Note:    "对用户可读、不含任何内部标识；服务商原文只在管理台可见。",
			Example: "任务处理失败，已全额返还"},
		{Name: "contents", Type: "array", Desc: "已转存的产物清单。",
			Note: "恒为数组（没有产物时是 []）；" +
				"序号即产物取数端点（tasks.content）里的 index。",
			Example: []any{}},
		{Name: "contents[].index", Type: "integer", Desc: "产物序号（0 起、稠密编号）。", Example: 0},
		{Name: "contents[].url", Type: "string",
			Desc: "取数地址（绝对地址，带鉴权，不能直接丢给浏览器打开）。",
			// 示例走**控制面**入口（/api/user/...）：这份字段表服务的正是开放接口，
			// 写 /v1/tasks/... 会把人引到协议面那套端点（那边只有 artifacts）。
			Example: "https://api.example.com/api/user/tasks/" + ExampleID + "/contents/0"},
		{Name: "contents[].content_type", Type: "string", Desc: "产物 MIME 类型。", Example: "video/mp4"},
		{Name: "contents[].bytes", Type: "integer", Desc: "产物字节数。", Example: 3145728},
		{Name: "contents[].kind", Type: "string", Desc: "产物渲染档。",
			Enum:    []string{"image", "video", "audio", "text", "file"},
			Example: "video"},
		{Name: "usage", Type: "object", Desc: "本次任务的原始用量（数量，不是金额）。"},
		{Name: "usage.images", Type: "integer", Desc: "出图张数。", Example: 0},
		{Name: "usage.seconds", Type: "integer", Desc: "视频/音频秒数。", Example: 5},
		{Name: "usage.ref_images", Type: "integer", Desc: "参考图张数。", Example: 0},
	}
}
