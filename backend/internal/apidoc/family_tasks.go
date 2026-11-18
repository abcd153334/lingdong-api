package apidoc

// 本文件生成「异步模型族节点下的『查询任务状态』」那一篇。
//
// 为什么族里要有一篇：读者在「Seedream / 可灵 / SUNO」这些节点里提交异步任务后，
// 下一步就是拿任务号查状态。只把这篇放在「通用接口 → 异步任务」里，等于让人
// 提交完再自己去别的板块找查询接口 —— 链路断在半路。
//
// 为什么内容能"重复"却不违反「每族一份」：
// 任务对象是**网关自有的契约**（`pkg/taskfield` 是唯一字段表），所有族的返回
// 结构逐字相同 —— 这是事实，不是复制。族与族之间的差别在**怎么拿到任务号、
// 产物是什么、要轮询多久**，这些写进每族各自的导语里。
// 门禁 `TestSameKindAcrossFamiliesDiffers` 因此只比**厂商协议端点**
// （`Transport.EndpointKey != ""`），网关自有读面不在其列。

// familyPollNotes 是每族独有的导语：怎么拿到任务号、产物是什么。
//
// 键是族 key。缺一条就说明族新增了异步端点却没写这段说明 ——
// `TestAsyncFamilyHasPollNote` 会直接报错，不允许用一句通用话糊过去。
var familyPollNotes = map[string]string{
	FamilyImagesGptImage: "提交时带 `async: true` 会立刻返回 `200` 与任务号；拿这个 `id` 来查。" +
		"产物通常是 1 张图，用 `GET /v1/tasks/{id}/artifacts` 列清单后取 `content_url`。",
	FamilyImagesNanoBanana: "提交后直接返回任务号；产物是图片，清单条数等于提交时的 `n`，" +
		"每项自带可直接打开的 `content_url`。",
	FamilyVideosKling: "提交返回任务号后用它轮询；清单第 `0` 项是 mp4，" +
		"`content_url` 是免鉴权的短时链接，可直接播放或交给下载器。",
	FamilyVideosSeedance: "提交返回任务号后用它轮询；产物是 mp4（含音轨时同一文件），24 小时内可下载。",
	FamilyVideosMiniMax: "提交返回任务号后用它轮询；产物是一条 mp4，2K 档耗时明显更长，建议 10 秒一次，进入终态即停。",
	FamilyVideosMuse: "提交返回任务号后用它轮询；产物是一条 mp4，10 秒档通常 1–3 分钟，" +
		"时长档越长等待越久，进入终态即停。",
	FamilyMusicSuno: "一次任务通常出两首：清单是 `0` 与 `1` 两条音频；" +
		"歌词任务的产物是纯文本，同样在清单里。",
}

// taskReadFaceOf 返回某族节点下的「查询任务状态」那篇（不适用时 ok=false）。
//
// 只给**会产生任务**的族挂：同步族（语音识别、对话）没有任务可查，给它挂一篇
// 反而是伪造。判据取自传输契约，不在这里另立一份名单。
func taskReadFaceOf(f FamilyDoc) (FamilyEndpoint, bool) {
	if !familyEmitsTasks(f) {
		return FamilyEndpoint{}, false
	}
	note, ok := familyPollNotes[f.Key]
	if !ok {
		return FamilyEndpoint{}, false
	}
	// 字段表 / 返回字段 / 响应变体全部来自通用节点那一篇：任务对象只有一份定义
	// （`TaskFlatFields()`），这里只换称谓与导语。
	doc := MustEndpointDoc(KindTasksGet)
	doc.Name = "查询任务状态"
	doc.Summary = "本族提交任务后，用返回的任务号查进度与产物；建议 5~10 秒轮询一次，进入终态即停。"
	doc.Notes = append([]string{note}, doc.Notes...)
	// 运行形态必须在这里**显式覆盖**：上面是 `MustEndpointDoc` 的值拷贝，
	// 会把通用节点的 RunMode/RunHint 一起复制过来。不写这两行，6 篇查询页
	// 就继续继承旧值（none + "没有路径参数输入框"），读者点了调试只看到一句
	// "不适合在线调试" —— 而它明明是 GET + 任务号 + KEY，完全能发。
	doc.RunMode = RunJSON
	doc.RunHint = ""
	return FamilyEndpoint{Doc: DocumentKey{Kind: KindTasksGet}, Spec: doc}, true
}

// familyEmitsTasks 判断这一族会不会产生任务（提交后拿任务号、要轮询）。
//
// 两个判据缺一不可：
//   - `Phase == PhaseSubmit`：视频 / 音乐 / 再生成这类"提交即返回任务号"的端点；
//   - `AcceptsAsync`：图像生成那种默认同步、带 `async: true` 才转任务的端点。
//
// 只看 `AcceptsAsync` 会漏掉整个视频与音乐板块（它们压根没有同步形态，
// 不需要那个开关），表现是"可灵 / SUNO 节点下没有查询接口"且不报错。
//
// 直接读族自己的那张表，不走 `FamilyEndpointsOf`（后者会把本函数的结果也拼进去，
// 互相调用就成了无限递归）。
func familyEmitsTasks(f FamilyDoc) bool {
	for _, e := range familyEndpointTable[f.Key] {
		tr, ok := TransportOf(e.Doc.Kind)
		if !ok {
			continue
		}
		// `AcceptsAsync` 是端点级事实（这个 URL 长什么样），还要再过一道族级判据：
		// 方舟的 /images/generations 只有同步，Seedream 页不该有「查询任务状态」。
		if tr.Phase == PhaseSubmit || (tr.AcceptsAsync && f.AsyncSubmit) {
			return true
		}
	}
	return false
}
