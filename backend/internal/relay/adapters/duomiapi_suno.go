package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"lingdong-api/internal/pkg/taskstate"
	"lingdong-api/internal/relay"
)

// 本文件实现多米API 的**音乐**子协议（B6-1）：
//
//	suno.music   POST /api/suno/generate   (multipart)   GET /api/suno/feed?task_id={id}
//	suno.lyrics  POST /api/suno/submit/lyrics (JSON)     GET /api/suno/feed?task_id={id}
//
// 三个必须记住的事实：
//  1. **SUNO 一次出 2 首**（上游行为，不是参数）。计费按 per_task 一次任务算，
//     产物按实际成功的条数转存 —— 用户只拿到 1 首时不许按 2 首收（B3 已按成功数结算）。
//  2. **生成接口只收 multipart**，JSON 会 400。字段顺序敏感（continue_* 要跟在 mv 后面），
//     所以用有序切片构造表单，不用 map。
//  3. **歌词没有链接**：返回的是纯文本，落盘成 text/plain（TaskStatus.ResultText）。
//     把它当成「产物 URL 为空」会判成失败，用户拿不到已经生成好的歌词。

const (
	duomiSunoSubmitPath = "/api/suno/generate"
	duomiSunoFeedPath   = "/api/suno/feed"
	duomiSunoLyricsPath = "/api/suno/submit/lyrics"

	// duomiSunoMaxLyricsPrompt 是歌词 prompt 的上限（文档：≤2000 字符）。
	duomiSunoMaxLyricsPrompt = 2000
	// duomiMaxMusicPromptRunes 是音乐 prompt 的本地上限（防止出站体失控）。
	duomiMaxMusicPromptRunes = 4000
	// duomiMaxMusicTagsRunes / duomiMaxMusicTitleRunes 是自定义模式下的长度上限。
	duomiMaxMusicTagsRunes  = 500
	duomiMaxMusicTitleRunes = 200
)

// duomiMusicArtifactKeys 是音乐产物键（**不含** image_url：封面图不是产物）。
//
// 键序即优先级：`audio_url` 是 CDN 上的**导出文件**（可直接下载、长期有效）；
// `stream_audio_url` / `source_audio_url` 可能指向 audiopipe 的**临时串流令牌**
// （见 duomiMusicStreamHosts），只能当兜底，绝不能排在导出链接前面。
var duomiMusicArtifactKeys = []string{
	"audio_url", "source_audio_url", "stream_audio_url",
	"data", "result", "results", "list", "clips",
}

// duomiMusicStreamHosts 是**不能直接转存**的串流主机。
//
// 实测（2026-10-24）：`https://audiopipe.suno.ai/?item_id=<clip_id>` 在生成完成后
// 立刻返回 403 `{"detail":"Invalid or expired stream URL"}`，没有可用的签名参数，
// 平台侧无论怎么重试都下载不到。把它当产物 = 任务必然「产物转存失败」并全额退款，
// 用户明明已经生成好两首歌却什么都拿不到。
var duomiMusicStreamHosts = []string{"audiopipe.suno.ai"}

// duomiFilterMusicURLs 剔除不可转存的串流地址，保留真正能下载的导出链接。
//
// 过滤而不是「排到后面」：转存是逐条尝试、全失败即判任务失败，
// 留下一条必 403 的地址不会帮上忙，只会污染清单与产物数量。
func duomiFilterMusicURLs(urls []string) []string {
	if len(urls) == 0 {
		return urls
	}
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		if duomiIsMusicStreamURL(u) {
			continue
		}
		out = append(out, u)
	}
	return out
}

// duomiIsMusicStreamURL 判断链接是否落在串流主机上（按 host 精确匹配，不做后缀模糊）。
func duomiIsMusicStreamURL(raw string) bool {
	p, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || p.Host == "" {
		return false
	}
	host := strings.ToLower(p.Hostname())
	for _, h := range duomiMusicStreamHosts {
		if host == h {
			return true
		}
	}
	return false
}

// duomiLyricsArtifactKeys 是歌词产物键（文本产物，走 texts 通道）。
var duomiLyricsArtifactKeys = []string{
	"text", "lyric", "lyrics",
	"data", "result", "results", "ret_a",
}

// submitDuomiSuno 提交 SUNO 音乐/歌词任务。
func submitDuomiSuno(ctx context.Context, cfg *relay.ChannelConfig, opts *duomiOptions,
	profile duomiProfile, upstream string, body *duomiBody) (string, string, error) {
	if profile.Protocol == duomiProtoSunoLyrics {
		return submitDuomiSunoLyrics(ctx, cfg, opts, body)
	}
	return submitDuomiSunoMusic(ctx, cfg, opts, profile, upstream, body)
}

// duomiSunoMusicLocalOnly 是音乐任务出站时必须丢弃的键：全部都是**平台已消费**的映射键。
//
// 音乐接口是 multipart/form-data，字段名与平台契约不完全同名（custom_mode / lyrics /
// instrumental 由适配器翻译成 custom_mode / prompt / make_instrumental），
// 原键再发一次会让上游收到两套写法。
var duomiSunoMusicLocalOnly = []string{
	"model", "prompt", "lyrics", "instrumental", "custom_mode", "tags", "title", "mv",
	"seconds", "duration", "size", "resolution", "aspect_ratio", "input_reference", "image",
}

// duomiSunoLyricsLocalOnly 是歌词任务出站时必须丢弃的键。
var duomiSunoLyricsLocalOnly = []string{"model", "prompt", "lyrics"}

// duomiFormFieldsWithUnmapped 把请求体里未映射的顶层键追加成表单字段。
//
// **必须追加在末尾、且不动已有字段的顺序**：SUNO 的表单对顺序敏感
// （continue_at / continue_clip_id 必须跟在 mv 之后），排序会把这条约束打散。
// 追加部分按名字排序，保证同样的输入产出同样的报文（抓包与断言可逐字比对）。
//
// multipart 只能表达字符串：标量取原文，对象/数组取 JSON 文本 —— 这与表单本身的
// 表达能力一致，不是有损转换（上游要结构化值就该要求 JSON 端点）。
func duomiFormFieldsWithUnmapped(fields [][2]string, raw []byte, localOnly ...string) [][2]string {
	if len(raw) == 0 {
		return fields
	}
	// 已映射的键 + localOnly 都不追加；其余原样补。
	seen := make(map[string]bool, len(fields)+len(localOnly))
	for _, kv := range fields {
		seen[kv[0]] = true
	}
	for _, k := range localOnly {
		seen[k] = true
	}
	extra := make(map[string]any, 4)
	relay.ForwardUnmapped(extra, raw, localOnly...)

	keys := make([]string, 0, len(extra))
	for k := range extra {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch x := extra[k].(type) {
		case string:
			fields = append(fields, [2]string{k, x})
		case json.RawMessage:
			var sv string
			if json.Unmarshal(x, &sv) == nil {
				fields = append(fields, [2]string{k, sv})
			} else {
				fields = append(fields, [2]string{k, string(x)})
			}
		default:
			fields = append(fields, [2]string{k, fmt.Sprint(x)})
		}
	}
	return fields
}

// submitDuomiSunoMusic 提交音乐生成任务（multipart）。
func submitDuomiSunoMusic(ctx context.Context, cfg *relay.ChannelConfig, opts *duomiOptions,
	profile duomiProfile, upstream string, body *duomiBody) (string, string, error) {
	customMode := opts.suno.customMode
	if body.CustomMode != nil {
		customMode = *body.CustomMode
	}
	instrumental := opts.suno.instrumental
	if body.Instrumental != nil {
		instrumental = *body.Instrumental
	}
	lyrics := strings.TrimSpace(body.Lyrics)
	prompt := strings.TrimSpace(body.Prompt)

	// 纯音乐 + 歌词是互斥的产品形态：同时给等于「歌词永远不会被唱」，
	// 用户会以为平台吞了他的歌词。必须显式拒绝而不是静默丢弃。
	if instrumental && lyrics != "" {
		return "", "", &relay.CallerParamError{Msg: "instrumental=true（纯音乐）与 lyrics 互斥，请二选一"}
	}
	if lyrics != "" {
		// 给了歌词就一定是自定义模式：否则上游会把它当风格描述，生成的歌与歌词无关。
		customMode = true
	}
	if customMode && lyrics == "" {
		// 自定义模式下没有歌词 → 上游会用空 prompt 生成，等于白花钱。
		return "", "", &relay.CallerParamError{Msg: "自定义模式（custom_mode）必须提供 lyrics"}
	}
	if !customMode && prompt == "" {
		return "", "", &relay.CallerParamError{Msg: "音乐任务必须提供 prompt（风格/主题描述）"}
	}
	if n := len([]rune(prompt)); n > duomiMaxMusicPromptRunes {
		return "", "", &relay.CallerParamError{Msg: fmt.Sprintf(
			"prompt 过长（%d 字符，上限 %d）", n, duomiMaxMusicPromptRunes)}
	}
	if n := len([]rune(lyrics)); n > duomiMaxMusicPromptRunes {
		return "", "", &relay.CallerParamError{Msg: fmt.Sprintf(
			"lyrics 过长（%d 字符，上限 %d）", n, duomiMaxMusicPromptRunes)}
	}
	tags := strings.TrimSpace(body.Tags)
	if n := len([]rune(tags)); n > duomiMaxMusicTagsRunes {
		return "", "", &relay.CallerParamError{Msg: fmt.Sprintf(
			"tags 过长（%d 字符，上限 %d）", n, duomiMaxMusicTagsRunes)}
	}
	title := strings.TrimSpace(body.Title)
	if n := len([]rune(title)); n > duomiMaxMusicTitleRunes {
		return "", "", &relay.CallerParamError{Msg: fmt.Sprintf(
			"title 过长（%d 字符，上限 %d）", n, duomiMaxMusicTitleRunes)}
	}

	// 三级回落：请求体显式指定 > 渠道默认 > 绑定里的上游模型名。
	// 顺序不能反：渠道默认是运营给"这个渠道默认用哪个版本"设的兜底，
	// 一旦压过请求体，调用方就再也换不了版本。
	mv := strings.TrimSpace(body.MV)
	if mv == "" {
		mv = strings.TrimSpace(opts.suno.mv)
	}
	if mv == "" {
		mv = strings.TrimSpace(upstream)
	}
	// 自定义模式下 prompt 字段承载歌词（上游契约如此），非自定义模式承载风格描述。
	upstreamPrompt := prompt
	if customMode {
		upstreamPrompt = lyrics
	}

	fields := [][2]string{
		{"custom_mode", duomiBoolStr(customMode)},
		{"make_instrumental", duomiBoolStr(instrumental)},
		{"prompt", upstreamPrompt},
	}
	if mv != "" {
		fields = append(fields, [2]string{"mv", mv})
	}
	// tags / title 只在自定义模式有意义：非自定义模式下上游会忽略，传了也无害，
	// 但显式只在需要时下发能让抓包一眼看出模式。
	if customMode {
		if tags != "" {
			fields = append(fields, [2]string{"tags", tags})
		}
		if title != "" {
			fields = append(fields, [2]string{"title", title})
		}
	}

	fields = duomiFormFieldsWithUnmapped(fields, body.raw, duomiSunoMusicLocalOnly...)

	path := duomiOptPath(opts, "suno.submit", duomiSunoSubmitPath)
	buf, err := duomiDoMultipart(ctx, cfg, opts.authPrefix, path, fields)
	if err != nil {
		return "", "", err
	}
	if berr, bad := duomiEnvelopeError(buf); bad {
		return "", "", berr
	}
	id, ierr := duomiExtractTaskID(buf)
	if ierr != nil {
		return "", "", ierr
	}
	return id, string(duomiProtoSunoMusic), nil
}

// submitDuomiSunoLyrics 提交歌词生成任务（JSON，只收 prompt）。
func submitDuomiSunoLyrics(ctx context.Context, cfg *relay.ChannelConfig, opts *duomiOptions,
	body *duomiBody) (string, string, error) {
	// prompt 与 lyrics 都接受：调用方在「写歌」语境下常常把主题描述填进 lyrics。
	prompt := strings.TrimSpace(body.Prompt)
	if prompt == "" {
		prompt = strings.TrimSpace(body.Lyrics)
	}
	if prompt == "" {
		return "", "", &relay.CallerParamError{Msg: "歌词任务必须提供 prompt"}
	}
	if n := len([]rune(prompt)); n > duomiSunoMaxLyricsPrompt {
		return "", "", &relay.CallerParamError{Msg: fmt.Sprintf(
			"歌词 prompt 过长（%d 字符，上限 %d）", n, duomiSunoMaxLyricsPrompt)}
	}
	path := duomiOptPath(opts, "suno.lyrics", duomiSunoLyricsPath)
	payload := map[string]any{"prompt": prompt}
	relay.ForwardUnmapped(payload, body.raw, duomiSunoLyricsLocalOnly...)
	buf, err := duomiDo(ctx, cfg, opts.authPrefix, http.MethodPost, path, payload)
	if err != nil {
		return "", "", err
	}
	if berr, bad := duomiEnvelopeError(buf); bad {
		return "", "", berr
	}
	id, ierr := duomiExtractTaskID(buf)
	if ierr != nil {
		return "", "", ierr
	}
	return id, string(duomiProtoSunoLyrics), nil
}

// parseDuomiMusicTask 解析音乐任务（多产物：一次 2 首）。
func parseDuomiMusicTask(buf []byte) (*relay.TaskStatus, error) {
	env := duomiParseTaskEnvelope(buf, duomiMusicArtifactKeys, duomiModeAudio)
	// 只保留可转存的导出链接（串流令牌必 403，见 duomiMusicStreamHosts）。
	urls := duomiFilterMusicURLs(env.urls)
	out := &relay.TaskStatus{
		Status:     duomiStatusOf(env.status),
		Progress:   env.progress,
		Error:      env.errMsg,
		ResultURLs: urls,
	}
	if len(urls) > 0 {
		out.ResultURL = urls[0]
	}
	if err := duomiResolveMusicStatus(buf, out); err != nil {
		return nil, err
	}
	// 根级业务失败（`{"code":400,"msg":"..."}`）：feed 里既没有逐曲状态也没有状态，
	// 归一后仍是空串。不判失败的话任务会一直跑到超时，用户白等、上游早已拒绝。
	if out.Status == "" && strings.TrimSpace(out.Error) != "" {
		out.Status = taskstate.Failed
	}
	// 上游已判完成、但**还没给出导出链接**：SUNO 的音频是「先能串流、后能导出」，
	// 此时若按 succeeded 收尾，转存会因 0 条可用链接而整单失败（用户白等还拿不到歌）。
	// 保持 running 继续轮询，导出链接出现的那一轮再收尾；真拿不到就由 deadline 兜底。
	if out.Status == taskstate.Succeeded && len(urls) == 0 {
		out.Status = taskstate.Running
	}
	return out, nil
}

// duomiResolveMusicStatus 用「逐曲状态集合」修正整任务状态。
//
// 为什么不能只看响应根上的 status：SUNO 的 feed 是**曲目列表**，
// 根上通常没有状态。只按「有没有 audio_url」判断，会在第一首刚生成好、
// 第二首还在 streaming 时就判成功 —— 用户只拿到 1 首，第二首永远取不到。
func duomiResolveMusicStatus(buf []byte, out *relay.TaskStatus) error {
	if out.Status == taskstate.Failed || out.Status == taskstate.Canceled {
		return nil
	}
	states := duomiCollectKeyStrings(buf, "status", 0)
	pending, done, failed := false, false, false
	for _, s := range states {
		switch duomiNormalizeStatus(s) {
		// 逐曲状态已被 duomiNormalizeStatus 归一成**平台**词表，所以直接比常量。
		case taskstate.Succeeded:
			done = true
		case taskstate.Failed:
			failed = true
		case taskstate.Running:
			pending = true
		}
	}
	switch {
	case pending:
		// 还有曲目在生成：即使已有链接也必须继续等，否则第二首会丢。
		out.Status = taskstate.Running
	case done:
		out.Status = taskstate.Succeeded
	case failed && len(out.ResultURLs) == 0:
		out.Status = taskstate.Failed
	case len(out.ResultURLs) > 0:
		out.Status = taskstate.Succeeded
	default:
		// 既没有状态也没有产物：保持上游原样（空 → 由 worker 归一成 running）。
	}
	return nil
}

// parseDuomiLyricsTask 解析歌词任务（文本产物）。
func parseDuomiLyricsTask(buf []byte) (*relay.TaskStatus, error) {
	env := duomiParseTaskEnvelope(buf, duomiLyricsArtifactKeys, duomiModeText)
	out := &relay.TaskStatus{
		Status:   duomiStatusOf(env.status),
		Progress: env.progress,
		Error:    env.errMsg,
	}
	if len(env.texts) > 0 {
		out.ResultText = env.texts[0]
	}
	if out.Status == "" {
		switch {
		case out.ResultText != "":
			out.Status = taskstate.Succeeded
		case out.Error != "":
			out.Status = taskstate.Failed
		}
	}
	// 歌词任务没有链接产物：上游已完成但正文为空必须判失败，
	// 否则任务「成功」却没有产物，用户付费拿到空文件。
	if out.Status == taskstate.Succeeded && out.ResultText == "" {
		out.Status = taskstate.Failed
		out.Error = "上游返回成功但没有歌词正文"
	}
	return out, nil
}

// ---------------------------------------------------------------- 小工具

// duomiOptPath 取配置覆盖的路径，缺省用内置常量。
func duomiOptPath(opts *duomiOptions, key, fallback string) string {
	if p := strings.TrimSpace(opts.paths[key]); p != "" {
		return p
	}
	return fallback
}

// duomiSunoFeedPathFor 构造 feed 查询路径（任务号必须转义：它是 URL 查询参数，不是路径段）。
func duomiSunoFeedPathFor(opts *duomiOptions, id string) string {
	base := duomiOptPath(opts, "suno.feed", duomiSunoFeedPath)
	return duomiWithQuery(base, "task_id="+url.QueryEscape(id))
}

// duomiBoolStr 把布尔转成表单需要的字面量。
func duomiBoolStr(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// duomiCollectKeyStrings 收集任意层级下某键的字符串值（用于逐曲状态集合）。
func duomiCollectKeyStrings(raw json.RawMessage, key string, depth int) []string {
	if depth > 6 || len(raw) == 0 {
		return nil
	}
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	var out []string
	switch s[0] {
	case '[':
		var arr []json.RawMessage
		if json.Unmarshal(raw, &arr) != nil {
			return nil
		}
		for _, it := range arr {
			out = append(out, duomiCollectKeyStrings(it, key, depth+1)...)
		}
	case '{':
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil {
			return nil
		}
		if v, ok := m[key]; ok {
			var str string
			if json.Unmarshal(v, &str) == nil {
				out = append(out, str)
			}
		}
		for _, v := range m {
			out = append(out, duomiCollectKeyStrings(v, key, depth+1)...)
		}
	}
	return out
}
