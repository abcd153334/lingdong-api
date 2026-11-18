package apidoc

// 本文件是 `/v1` **任务面唯一对外结构**（扁平）的字段表与示例。
//
// 为什么只有这一份：任务对象曾经有三套对外形状 —— 富对象（contents/usage/
// modality…）、OpenAI Video 对象、new-api 扁平结构。同一个任务在两个接口里
// 字段不同，接入方得写两套解析，而我们每加一个出口就要再抄一遍字段。
// 现在所有 `/v1` 任务路径共用 `TaskFlat`（service/taskshell），这里只声明一次。
//
// 与控制面的关系：门户/管理台仍用 `taskview.View`（含 frozen/settled/渠道归属），
// 那是**登录态受众面**，不是对外 API。两份字段表服务两类读者，不合并。

// flatExampleID 是扁平对象示例里的任务号。
//
// 一份字面量而不是每处 copy：任务号形态换过一次（十进制 → `task_` + ULID），
// 示例是读者唯一照抄的东西，漏改一处就会有人按旧形态写解析。
const flatExampleID = "task_01J9HA7JPQ9A0Z6JZ3V8M9W6PZ"

// TaskFlatFields 是扁平任务对象的字段表（提交、查询、列表共用）。
func TaskFlatFields() []Field {
	return []Field{
		ret("id", TypeString, "任务 ID（`task_` 前缀），与 `task_id` 同值。").
			withExample(flatExampleID),
		ret("task_id", TypeString, "任务 ID，与 `id` 同值（两个键都发，避免客户端取空）。").
			withExample(flatExampleID),
		ret("platform", TypeString, "平台标识，恒为 `lingdong`。").
			withExample("lingdong"),
		ret("model", TypeString, "模型 ID（与请求里的 `model` 一致）。").
			withExample("kling-v2"),
		ret("status", TypeString, "任务状态。").
			withEnum("SUBMITTED", "IN_PROGRESS", "SUCCESS", "FAILURE").
			withExample("SUBMITTED").
			withNote("`FAILURE` 同时覆盖「生成失败」「超时全额返还」「已取消」三种情况，" +
				"具体原因看 `fail_reason`。"),
		ret("progress", TypeString, "进度：`0%` / `50%` / `100%`。").
			withExample("0%"),
		ret("fail_reason", TypeString, "失败原因（分类文案，成功时为空串）。").
			withNote("不含任何内部标识与服务商原文。").
			withExample(""),
		ret("created_at", TypeInteger, "创建时间（Unix 秒）。").withExample(int64(1762318830)),
		ret("finished_at", TypeInteger, "完成时间（Unix 秒，未进入终态为 0）。").
			withNote("与 `finish_time` 同值：兼容协议的两套响应用了两个键名，这里同时下发。").
			withExample(int64(0)),
		ret("finish_time", TypeInteger, "同上（与 `finished_at` 同值）。").withExample(int64(0)),
		ret("result_url", TypeString, "主产物地址。").
			withNote("只有 `GET /v1/video/generations/{id}` 会带；完整产物清单走 " +
				"`GET /v1/tasks/{id}/artifacts`。").
			withExample("https://api.example.com/v1/media/eyJ2Ijoy..."),
	}
}

// TaskArtifactFields 是产物清单（`GET /v1/tasks/{id}/artifacts`）的字段表。
func TaskArtifactFields() []Field {
	return []Field{
		ret("task_id", TypeString, "任务 ID。").
			withExample(flatExampleID),
		ret("artifacts", TypeArray, "产物清单（没有产物时是空数组）。").withExample([]any{}),
		ret("artifacts[].key", TypeString, "产物键：0 起的稠密序号，同时是下载路径的最后一段。").
			withExample("0"),
		ret("artifacts[].type", TypeString, "产物大类。").
			withEnum("image", "video", "audio", "text", "file").withExample("video"),
		ret("artifacts[].mime_type", TypeString, "MIME 类型。").withExample("video/mp4").
			withNote("键恒在；未提供 MIME 时是空字符串（按 `type` 渲染即可）。"),
		ret("artifacts[].content_url", TypeString, "取数地址。").
			withNote("**免鉴权**的短时链接，可直接给 `<img>` / `<video>` / 下载器；" +
				"过期后重新调本接口拿新链接，或用带鉴权的 " +
				"`GET /v1/tasks/{id}/artifacts/{key}/content`。").
			withExample("https://api.example.com/v1/media/eyJ2Ijoy..."),
	}
}

// flatExampleModelVideo 是视频族示例里用的模型 ID。
//
// 示例里的 model 必须是一个**真实在售 slug**：写死一个不存在的名字（曾经是 `kling-v2`），
// 读者复制示例会得到 ModelNotFound，而且每个族页上显示的都是同一个视频模型名 ——
// 图片页的「已受理」示例里出现 kling，是最容易被截图当笑话的那类不准确。
const flatExampleModelVideo = "kling-v3"

// TaskExampleFlat 是扁平任务对象的示例（按状态给不同真值）。模型名按视频族默认。
//
// 必须给**真值**而不是占位：读者要用它判断「任务号是字符串还是数字」
// 「进度是 50 还是 "50%"」「status 是大写还是小写」—— 这三件事每一条
// 都曾经在文档与运行期之间漂移过。
func TaskExampleFlat(status string) Obj {
	return TaskExampleFlatFor(status, flatExampleModelVideo)
}

// TaskExampleFlatFor 与 TaskExampleFlat 相同，但由调用方指定 model。
//
// 每个族页自己的示例要用自己族的模型：一个 image 页的「已受理」示例里写着视频模型，
// 读者第一眼就会怀疑整页是不是抄来的。
func TaskExampleFlatFor(status, model string) Obj {
	o := Obj{
		{K: "id", V: flatExampleID},
		{K: "task_id", V: flatExampleID},
		{K: "platform", V: "lingdong"},
		{K: "model", V: model},
		{K: "status", V: status},
		{K: "progress", V: "0%"},
		{K: "fail_reason", V: ""},
		{K: "created_at", V: int64(1762318830)},
		{K: "finished_at", V: int64(0)},
		{K: "finish_time", V: int64(0)},
	}
	switch status {
	case "SUCCESS":
		o = o.With("progress", "100%").
			With("finished_at", int64(1762319000)).
			With("finish_time", int64(1762319000))
	case "FAILURE":
		o = o.With("progress", "100%").
			With("fail_reason", "任务处理失败，已全额返还").
			With("finished_at", int64(1762319000)).
			With("finish_time", int64(1762319000))
	}
	return o
}

// TaskExampleFlatAccepted 是「刚受理」的示例（提交接口的 200）。
func TaskExampleFlatAccepted() Obj { return TaskExampleFlat("SUBMITTED") }

// TaskExampleFlatAcceptedFor 同上，模型名由调用方给（族页用自己族的 model）。
func TaskExampleFlatAcceptedFor(model string) Obj { return TaskExampleFlatFor("SUBMITTED", model) }

// TaskExampleArtifactList 是产物清单示例（status=SUCCESS 的任务）。
func TaskExampleArtifactList() Obj {
	return Obj{
		{K: "task_id", V: flatExampleID},
		{K: "artifacts", V: []any{Obj{
			{K: "key", V: "0"},
			{K: "type", V: "video"},
			{K: "mime_type", V: "video/mp4"},
			{K: "content_url", V: "https://api.example.com/v1/media/eyJ2Ijoy..."},
		}}},
	}
}
