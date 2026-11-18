package apicontract

import "lingdong-api/internal/pkg/taskfield"

// 本文件把「异步任务对象」的字段表接到唯一声明处 pkg/taskfield。
//
// 为什么不手写第二份：任务对象是**唯一**同时出现在两套对外文档里的形状
// （文档站的 GET /v1/tasks/{id} 与开放接口的 tasks.list / tasks.detail /
// tasks.cancel / usage.detail.task）。改造前两边各手写一遍，已经漂移：
// id 一处 string 一处 integer、created_at 一处 Unix 秒一处 RFC3339、
// contents[].url 一处有一处没有、result_url 两处都有而运行期根本不返回。
// 漂移的字段表比没有更糟 —— 接入方会照着它写解析分支，然后拿到永远 undefined 的键。
//
// 为什么表不放在 apidoc：apidoc → billing → apikey → 本包，反向 import 即导入环；
// 放在本包又会把「文档站依赖开放接口参考」锁死。叶子包方向中立，两边各派生一次。
//
// taskIDExample 是控制面文档示例里的任务号，与字段表同源（taskfield.ExampleID）。
//
// 一个变量而不是各示例写字面量：任务号形态改过（十进制 → `task_` + ULID），
// 示例是接入方唯一照抄的东西，漏改一处就会让人按旧形态写解析。
var taskIDExample = taskfield.ExampleID

// prefix 是字段在响应里的位置（`items[].` / `task.` / 空）。
func taskReturns(prefix string) []Return {
	fs := taskfield.Fields()
	out := make([]Return, 0, len(fs))
	for _, f := range fs {
		out = append(out, Return{
			Name: prefix + f.Name,
			Type: Type(f.Type),
			Desc: f.Desc,
			Note: f.Note,
			Enum: f.Enum,
		})
	}
	return out
}

// portalTaskReturns 是门户与应用面的任务对象：公共字段 + 资金字段。
//
// frozen / settled 只在门户与应用面下发（taskview.PortalItem 嵌入 View 后追加），
// `/v1` 的任务对象没有它们 —— 所以在这里补两行，而不是把资金字段塞进公共表。
// 顺序与 JSON 一致：嵌入字段在前，追加字段在后。
func portalTaskReturns(prefix string) []Return {
	return append(taskReturns(prefix),
		ret(prefix+"frozen", TypeInteger, "预扣金额（微）。任务结束时释放或结算"),
		ret(prefix+"settled", TypeInteger, "结算金额（微）。尚未结算时为 0"),
	)
}
