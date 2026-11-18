package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/gateway"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/relay"
	"lingdong-api/internal/service/pricing"
	"lingdong-api/internal/service/task"
	"lingdong-api/internal/service/taskshell"
)

// SUNO 兼容面（`/suno/*`）：完全对标 new-api 的 SUNO 协议。
//
// 为什么要有这一层而不是"让客户端改用 /v1/audio/*"：已经按 new-api 写好的
// 客户端（SDK、脚本、工作流）只认 `/suno/submit/music` + `{code,message,data}`，
// 换 base_url 是它们唯一愿意做的改动。协议形状对齐的成本远低于要求对方改代码。
//
// 三条铁律：
//  1. **内部仍然落同一套 tasks**：这一层只做报文翻译，不新增任务模型、不新增读面；
//  2. **上游链接永不下发**：clip 的 audio_url 是平台自己的能力 URL（签名或
//     `/v1/tasks/.../content`），不是 CDN 直链；
//  3. **动作决定端点**：music → `audio/music/generations`、lyrics → `audio/lyrics`，
//     并且要校验模型确实声明了该端点 —— 否则拿作曲模型写词会被当作曲收钱。

// sunoActionEndpoints 是动作 → 内部端点键的唯一映射。
//
// 不新增 `suno/submit/music` 这种端点键：`EndpointKey` 是 `models.endpoints`
// 里的勾选项与计费/能力校验的判据，多出两个与原生入口完全等价的键，只会让
// 运营勾错一个就少一半流量。
var sunoActionEndpoints = map[string]string{
	taskshell.SunoActionMusic:  "audio/music/generations",
	taskshell.SunoActionLyrics: "audio/lyrics",
}

// SunoActionOf 把路径参数归一成动作（大小写不敏感）。未知动作返回空串。
//
// 导出：文档门禁要拿它校验 `action` 的枚举与控制器同源 —— 文档写 music/lyrics
// 而控制器只认大写，是"照文档发请求 400"的经典来源。
func SunoActionOf(raw string) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "MUSIC":
		return taskshell.SunoActionMusic, true
	case "LYRICS":
		return taskshell.SunoActionLyrics, true
	}
	return "", false
}

// SunoSubmit 处理 POST /suno/submit/{action}。
func SunoSubmit(c *gin.Context) {
	k := relay.APIKeyOf(c)
	u := relay.UserOf(c)
	if k == nil || u == nil {
		sunoFail(c, errs.SunoAuth)
		return
	}
	action, ok := SunoActionOf(c.Param("action"))
	if !ok {
		sunoFail(c, errs.SunoInvalidRequest.WithMessage(
			"action 只能是 music 或 lyrics（路径为 /suno/submit/music 或 /suno/submit/lyrics）"))
		return
	}
	body, berr := readJSONBodyDef(c, errs.SunoInvalidRequest.WithMessage(
		"/suno/submit 只接受 application/json，不接受 multipart"))
	if berr.Code != "" {
		sunoFail(c, berr)
		return
	}

	body, verr := normalizeSunoBody(body)
	if verr != nil {
		sunoFail(c, errs.SunoInvalidRequest.WithMessage(verr.Error()))
		return
	}

	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		sunoFail(c, errs.SunoInvalidRequest.WithMessage("请求体不是合法 JSON"))
		return
	}
	model, merr := resolveSunoModel(action, strings.TrimSpace(probe.Model))
	if merr.Code != "" {
		sunoFail(c, merr)
		return
	}
	// 把解析出来的模型写回请求体：task.ParseSubmit 只认 body 里的 model，
	// 而 new-api 的客户端经常根本不传 model（由 action 决定）。
	body, err := withJSONModel(body, model)
	if err != nil {
		sunoFail(c, errs.SunoInvalidRequest.WithMessage("请求体不是合法 JSON"))
		return
	}

	req, err := task.ParseSubmit(body)
	if err != nil {
		sunoFail(c, failDefOf(err))
		return
	}
	req.Params.MusicAction = action

	row, err := task.Submit(c.Request.Context(), task.SubmitInput{
		Key: k, Group: relay.EffectiveGroup(k, u), IP: c.ClientIP(),
		RequestID: c.GetHeader("X-Request-Id"), Req: req, ExpectModality: "music",
		EndpointKey:    sunoActionEndpoints[action],
		IdempotencyKey: submitIdempotencyKey(c),
		RawBody:        body, ReqSource: submitReqSource(c),
	})
	if err != nil {
		sunoFail(c, failDefOf(err))
		return
	}
	// 提交回执的 data 是**任务号字符串**（new-api 就是这个形状）。
	// 给任务对象会让按 new-api 写的客户端把整个对象当成 id 用。
	gateway.OKNewAPI(c, row.PublicID)
}

// SunoFetch 处理 GET /suno/fetch/{task_id}。
func SunoFetch(c *gin.Context) {
	k := relay.APIKeyOf(c)
	if k == nil {
		sunoFail(c, errs.SunoAuth)
		return
	}
	ref := strings.TrimSpace(c.Param("task_id"))
	t, err := task.GetByRef(c.Request.Context(), ref, task.Scope{KeyID: k.ID})
	if err != nil {
		sunoFail(c, errs.SunoTaskNotFound.WithMessage("任务不存在，或不属于当前 API Key"))
		return
	}
	arts, aerr := task.ArtifactsOf(c.Request.Context(), t.ID)
	if aerr != nil {
		sunoFail(c, errs.SunoServerError.WithMessage("读取产物清单失败"))
		return
	}
	gateway.OKNewAPI(c, sunoTaskOf(c, t, arts))
}

// SunoFetchBatchMax 是批量查询一次最多接受的任务号数量（与 new-api 一致）。
//
// 上限不是性能参数而是**契约**：客户端按 100 分批轮询，上限改了它们的分批逻辑
// 就会漏单。真要放开必须同时改文档与 all 客户端，所以钉死在这里。
const SunoFetchBatchMax = 100

// SunoFetchBatch 处理 POST /suno/fetch（body `{"ids":[...]}`）。
func SunoFetchBatch(c *gin.Context) {
	k := relay.APIKeyOf(c)
	if k == nil {
		sunoFail(c, errs.SunoAuth)
		return
	}
	body, berr := readJSONBodyDef(c, errs.SunoInvalidRequest.WithMessage(
		"/suno/fetch 只接受 application/json，请求体形如 {\"ids\":[\"task_xxx\"]}"))
	if berr.Code != "" {
		sunoFail(c, berr)
		return
	}
	var in struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		sunoFail(c, errs.SunoInvalidRequest.WithMessage("请求体不是合法 JSON，应为 {\"ids\":[...]}"))
		return
	}
	if len(in.IDs) == 0 {
		sunoFail(c, errs.SunoInvalidRequest.WithMessage("ids 不能为空"))
		return
	}
	if len(in.IDs) > SunoFetchBatchMax {
		sunoFail(c, errs.SunoInvalidRequest.WithMessage(fmt.Sprintf(
			"一次最多查询 %d 个任务（本次 %d 个），请分批", SunoFetchBatchMax, len(in.IDs))))
		return
	}
	ctx := c.Request.Context()
	rows, err := task.ListByRefs(ctx, in.IDs, task.Scope{KeyID: k.ID})
	if err != nil {
		// 与 new-api 一致：任一条取不到就整批 404。部分成功会让调用方
		// 分不清「这条还在排队」与「这条不是我的」。
		sunoFail(c, errs.SunoTaskNotFound.WithMessage("ids 中有任务不存在，或不属于当前 API Key"))
		return
	}
	ids := make([]uint64, 0, len(rows))
	for i := range rows {
		ids = append(ids, rows[i].ID)
	}
	grouped, aerr := task.ArtifactRowsByTask(ctx, ids)
	if aerr != nil {
		sunoFail(c, errs.SunoServerError.WithMessage("读取产物清单失败"))
		return
	}
	out := make([]taskshell.SunoTask, 0, len(rows))
	for i := range rows {
		out = append(out, sunoTaskOf(c, &rows[i], grouped[rows[i].ID]))
	}
	gateway.OKNewAPI(c, out)
}

// sunoTaskOf 把任务行投影成 /suno 的对象（唯一构造点）。
func sunoTaskOf(c *gin.Context, t *model.Task, arts []task.ArtifactEntry) taskshell.SunoTask {
	meta := task.MusicMetaOf(t)
	action := meta.Action
	if action == "" {
		action = taskshell.SunoActionMusic
	}
	// 版本串：请求里显式给了 `mv` 就用它；没给时回落到平台模型 ID
	// —— 模型 ID 就是绑定到的上游版本名（`chirp-hawk`），比空串有用得多。
	version := meta.MV
	if version == "" {
		version = t.Model
	}
	return taskshell.SunoOf(taskshell.SunoBuildInput{
		Task: t, Action: action,
		Title: meta.Title, Tags: meta.Tags, Prompt: meta.Prompt,
		Lyrics: meta.Lyrics, Version: version,
		Artifacts: arts,
		ContentURL: func(idx int) string {
			return artifactContentURL(c, t, idx)
		},
	})
}

// normalizeSunoBody 把 new-api 的报文翻译成平台契约（只改内存里的 body）。
//
// 三处语义错位，每一处都对应一次"照文档发请求却 400/静默失效"：
//  1. `gpt_description_prompt` 是 new-api 的字段名，上游与平台内部都只认 `prompt`；
//  2. 自定义模式下 new-api 把歌词放在 `prompt` 里，而适配器要求
//     `custom_mode=true` 必须带 `lyrics`（否则 400）；
//  3. `make_instrumental` 是上游必填项，缺省必须补 false。
//
// 不在这里做的两件事：
//   - 不改 `model`（由 resolveSunoModel 统一解析，写回走 withJSONModel）；
//   - 不吞掉不支持的字段（下面显式拒绝，见 unsupportedSunoFields）。
func normalizeSunoBody(body []byte) ([]byte, error) {
	m, err := decodeJSONMap(body)
	if err != nil {
		return nil, err
	}
	for _, f := range unsupportedSunoFields {
		if _, has := m[f.Key]; has {
			return nil, fmt.Errorf("%s", f.Reason)
		}
	}
	prompt := strings.TrimSpace(jsonString(m["prompt"]))
	if prompt == "" {
		// gpt_description_prompt 在 new-api 里是"灵感模式的描述词"，与 prompt 同义。
		if g := strings.TrimSpace(jsonString(m["gpt_description_prompt"])); g != "" {
			m["prompt"] = g
			prompt = g
		}
	}
	lyrics := strings.TrimSpace(jsonString(m["lyrics"]))
	if jsonBool(m["custom_mode"]) && lyrics == "" && prompt != "" {
		// 自定义模式：new-api 客户端把歌词写在 prompt 里。搬过去而不是二选一拒绝，
		// 因为这正是"同一个字段在两种模式承载两种东西"的官方语义。
		m["lyrics"] = prompt
	}
	if _, has := m["make_instrumental"]; !has {
		m["make_instrumental"] = false
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("请求体无法序列化")
	}
	return out, nil
}

// sunoUnsupported 是一个"协议里有、本轮不提供"的字段。
type sunoUnsupported struct {
	Key    string
	Reason string
}

// unsupportedSunoFields 是**显式拒绝**的字段清单。
//
// 为什么不静默忽略：适配器会把未声明的键当表单字段发给上游（ForwardUnmapped），
// 所以「文档没写」在读者眼里等于「能传」。一个真正不生效的字段被静默接受，
// 用户会拿着"我明明设了回调地址"来对账，而平台从未回调过一次。
var unsupportedSunoFields = []sunoUnsupported{
	{"notify_hook", "本平台不提供回调（notify_hook）：请轮询 GET /suno/fetch/{task_id} 获取结果"},
	{"callback_url", "本平台不提供回调（callback_url）：请轮询 GET /suno/fetch/{task_id} 获取结果"},
	{"continue_clip_id", "暂不支持续写（continue_clip_id）：当前只提供整首生成"},
	{"continue_at", "暂不支持续写（continue_at）：当前只提供整首生成"},
}

// resolveSunoModel 解析这次提交该用哪个平台模型（三级回落）。
//
// new-api 的客户端经常**不传 model**（动作就决定了模型），而平台计费必须有真实
// 模型行。三级顺序：
//  1. body 里的 model 是本平台在售 slug → 用它；
//  2. body 里是 new-api 的占位名（suno_music / suno_lyrics）→ 当作没传；
//  3. 取在售模型里**第一个**声明了该端点的 music 模型。
//
// 第 3 步必须校验端点声明：拿作曲模型去写词会被当作曲收钱（两边都是 music 模态，
// 只校验模态根本拦不住）。
// 返回 (模型 ID, 错误)。错误用零值 Def 表示"没有错误" —— errs.Def 是结构体，
//
//	不能返回 nil；用 *Def 又会让调用方每次都判指针。
func resolveSunoModel(action, raw string) (string, errs.Def) {
	want := sunoActionEndpoints[action]
	snap, err := pricing.Get()
	if err != nil || snap == nil {
		return "", errs.SunoServerError.WithMessage("模型目录暂不可用，请稍后重试")
	}
	raw = strings.TrimSpace(raw)
	if raw != "" && !isSunoPlaceholderModel(raw) {
		for _, m := range snap.Models {
			if m.ID != raw {
				continue
			}
			if !m.Enabled {
				return "", errs.SunoInvalidRequest.WithMessage(fmt.Sprintf("模型 %s 当前未上架", raw))
			}
			if !modelDeclaresEndpoint(m, want) {
				return "", errs.SunoInvalidRequest.WithMessage(fmt.Sprintf(
					"模型 %s 不承接该动作（%s），请换模型或改用正确的 action", raw, action))
			}
			return raw, errs.Def{}
		}
		return "", errs.SunoInvalidRequest.WithMessage(fmt.Sprintf("模型 %s 不存在", raw))
	}
	for _, m := range snap.Models {
		if !m.Enabled || strings.ToLower(strings.TrimSpace(m.Modality)) != "music" {
			continue
		}
		if modelDeclaresEndpoint(m, want) {
			return m.ID, errs.Def{}
		}
	}
	// 没有在售模型是**平台侧能力缺失**，不是客户端错误，也不是"服务端崩了"：
	// 用 ModelUnavailable（503）而不是 ServerError（500）。
	//   500 的语义是"别重试，等我修"；503 才是"退避重试 / 换个模型"。
	//   同代码里 video.go:262 遇到同类情况已经是 ModelUnavailable，这里对齐。
	// 对 /suno 而言 HTTP 仍是 503、body 的 code 仍是 server_error（七码不变，见 sunoDefOf）。
	if action == taskshell.SunoActionLyrics {
		return "", errs.ModelUnavailable.WithMessage("当前没有在售的写词模型，请稍后重试或联系平台")
	}
	return "", errs.ModelUnavailable.WithMessage("当前没有在售的音乐模型，请稍后重试或联系平台")
}

// isSunoPlaceholderModel 判断是不是 new-api 的占位模型名。
//
// new-api 用 suno_music / suno_lyrics 当"由动作决定模型"的标记，它们不是真实
// 模型；当成 slug 去查会得到一句"模型不存在"，而正确行为是回落到默认模型。
func isSunoPlaceholderModel(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "suno_music", "suno-music", "sunomusic",
		"suno_lyrics", "suno-lyrics", "sunolyrics":
		return true
	}
	return false
}

// modelDeclaresEndpoint 判断模型是否声明了该端点（没声明时按模态默认端点算）。
//
// 用 apidoc 的访问器而不是自己解 json：`models.endpoints` 为空时的默认值
// （music → audio/music/generations）是契约层的事实，两处各写一份必然分叉。
func modelDeclaresEndpoint(m pricing.ModelItem, want string) bool {
	eps := apidoc.DeclaredEndpoints(json.RawMessage(m.Endpoints))
	if len(eps) == 0 {
		eps = apidoc.DefaultEndpoints(m.Modality)
	}
	for _, e := range eps {
		if e == want {
			return true
		}
	}
	return false
}

// sunoFail 是 /suno 的唯一错误出口。
//
// 这里只做「交给协议面出口」这一件事：码表投影唯一在 gateway.sunoWire，
// 按状态码段收敛到 SUNO 的七个码。曾经在本包另有一份 sunoDefOf，两处边界不同
// （本地这份把 413/405/422 压成 400，gateway 那份又漏了它们），同一个错误
// 会因为「谁先发现」而得到不同状态码。
func sunoFail(c *gin.Context, def errs.Def) {
	gateway.FailDef(c, def)
}

// decodeJSONMap 解析成 map 且保留数字字面量（避免 int64 被 float64 改写）。
func decodeJSONMap(body []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, fmt.Errorf("请求体不是合法 JSON 对象")
	}
	return m, nil
}

// withJSONModel 把 model 写回请求体（保留其它字段原样）。
func withJSONModel(body []byte, model string) ([]byte, error) {
	m, err := decodeJSONMap(body)
	if err != nil {
		return nil, err
	}
	m["model"] = model
	return json.Marshal(m)
}

func jsonString(v any) string {
	s, _ := v.(string)
	return s
}

func jsonBool(v any) bool {
	b, _ := v.(bool)
	return b
}
