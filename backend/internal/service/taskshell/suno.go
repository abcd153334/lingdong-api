// Package taskshell 的 SUNO 兼容投影（new-api 的 `/suno/*` 形状）。
//
// 为什么单独一份而不是复用 TaskFlat：new-api 的 SUNO 协议面返回的是
// `{code,message,data}` 信封，`data` 是**任务对象**，对象里再嵌一层 `data` ——
// 音乐是 clip 数组、歌词是单个对象。这三层形状与 `/v1/tasks/{id}` 的扁平对象
// 不是同一件事：把两者强行合并，要么 `/v1` 多出一堆 SUNO 专有字段，
// 要么 `/suno` 丢掉客户端真正要读的 clip 字段。
//
// 唯一真相仍然是 `model.Task` + 产物清单：这里只做**投影**，不查库、不拼存储路径。
package taskshell

import (
	"strconv"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/taskstate"
)

// SUNO 动作（对外是小写；`/suno/submit/{action}` 里大小写不敏感）。
const (
	SunoActionMusic  = "music"
	SunoActionLyrics = "lyrics"
)

// SunoTask 是 `/suno/fetch` 的任务对象（new-api nativeTask 同构）。
type SunoTask struct {
	CreatedAt int64 `json:"created_at"`
	// UpdatedAt 与 created_at 一起给：客户端用「一段时间没变」判卡死，
	// 只给 created_at 时这个判断在长任务上永远不成立。
	UpdatedAt  int64  `json:"updated_at"`
	TaskID     string `json:"task_id"`
	Platform   string `json:"platform"`
	Status     string `json:"status"`
	FailReason string `json:"fail_reason"`
	// SubmitTime 与 created_at 同值：new-api 的 SUNO 客户端读这个键。
	SubmitTime int64 `json:"submit_time"`
	// FinishTime 只在终态给值：运行中给「现在」会让每次查询的完成时间都在变。
	FinishTime int64 `json:"finish_time"`
	Progress   string `json:"progress"`
	// Action 是 music / lyrics：同一个任务号在两种动作下产物形状不同，
	// 不给这个键，客户端只能靠 data 是数组还是对象去猜。
	Action string `json:"action"`
	// Data 是内层载荷：music 成功 → clip 数组；lyrics 成功 → 单个歌词对象；
	// 非终态或失败 → null（new-api 同口径，客户端据此不必先判状态）。
	Data any `json:"data"`
}

// SunoClip 是 music 任务的一条音频产物（对齐 new-api dto.SunoSong 的字段名）。
//
// 只有 `audio_url` 有值：封面与视频版平台**不转存**（它们在上游是独立 CDN 资源，
// 存下来就等于对外发上游直链）。字段仍然保留且恒在，是为了让按 new-api
// 写的客户端不必为「键可能不存在」再写一套分支。
type SunoClip struct {
	ID             string       `json:"id"`
	VideoURL       string       `json:"video_url"`
	AudioURL       string       `json:"audio_url"`
	ImageURL       string       `json:"image_url"`
	ImageLargeURL  string       `json:"image_large_url"`
	MajorModelVer  string       `json:"major_model_version"`
	ModelName      string       `json:"model_name"`
	Status         string       `json:"status"`
	Title          string       `json:"title"`
	Text           string       `json:"text"`
	Metadata       SunoMetadata `json:"metadata"`
}

// SunoMetadata 是 clip 的元信息（对齐 new-api dto.SunoMetadata）。
//
// `duration` / `audio_prompt_id` / `error_type` 恒为 null：平台不采它们
// （上游不保证给，且给了也不参与计费）。写成 null 而不是省掉键：
// new-api 的客户端读 `metadata.duration` 时不会先判 `metadata` 是否存在。
type SunoMetadata struct {
	Tags                 string `json:"tags"`
	Prompt               string `json:"prompt"`
	GptDescriptionPrompt string `json:"gpt_description_prompt"`
	AudioPromptID        any    `json:"audio_prompt_id"`
	Duration             any    `json:"duration"`
	ErrorType            any    `json:"error_type"`
	ErrorMessage         any    `json:"error_message"`
}

// SunoLyrics 是 lyrics 任务的交付对象（对齐 new-api dto.SunoLyrics）。
type SunoLyrics struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Title  string `json:"title"`
	Text   string `json:"text"`
}

// SunoBuildInput 是投影所需的全部事实。
//
// 刻意把 title/tags/prompt/版本串作为参数传入而不是在里面解析
// `tasks.request_params`：那会把「params 的 json 结构」变成 taskshell 的
// 隐式依赖，日后改字段名不会有任何编译错误。
type SunoBuildInput struct {
	Task    *model.Task
	Action  string
	Title   string
	Lyrics  string
	Tags    string
	Prompt  string
	Version string
	// Artifacts 是已按序号升序的产物清单（文本产物在最后）。
	Artifacts []model.Artifact
	// ContentURL 由调用方给：签名要站点基址，只有 HTTP 层知道。
	ContentURL func(idx int) string
}

// SunoOf 把任务行投影成 `/suno/fetch` 的任务对象。
func SunoOf(in SunoBuildInput) SunoTask {
	t := in.Task
	out := SunoTask{
		CreatedAt:  t.CreatedAt.Unix(),
		UpdatedAt:  t.UpdatedAt.Unix(),
		TaskID:     t.PublicID,
		Platform:   PlatformValue,
		Status:     taskstate.Word(taskstate.DialectNewAPI, t.Status),
		FailReason: failReasonOf(t),
		SubmitTime: t.CreatedAt.Unix(),
		Progress:   taskstate.Percent(t.Status),
		Action:     in.Action,
	}
	if taskstate.IsTerminal(t.Status) {
		out.FinishTime = t.UpdatedAt.Unix()
	}
	out.Data = sunoData(in)
	return out
}

// failReasonOf 成功时给空串（new-api 同口径），失败时给分类文案。
func failReasonOf(t *model.Task) string {
	if t.Status == taskstate.Succeeded {
		return ""
	}
	return taskstate.PortalError(t.Status)
}

// sunoData 决定内层 data：只有**成功**的任务才有载荷。
//
// 失败给 null 而不是空数组：空数组会让客户端以为"生成完了但一首都没有"，
// 而真实原因是失败 —— 那两条分支的处置完全不同（一个该退款，一个该重试）。
func sunoData(in SunoBuildInput) any {
	t := in.Task
	if !taskstate.IsTerminal(t.Status) || t.Status != taskstate.Succeeded {
		return nil
	}
	if in.Action == SunoActionLyrics {
		text := t.ResultText
		if text == "" {
			text = in.Lyrics
		}
		return SunoLyrics{
			ID:     t.PublicID + ":0",
			Status: "complete",
			Title:  in.Title,
			Text:   text,
		}
	}

	clips := make([]SunoClip, 0, len(in.Artifacts))
	for _, a := range in.Artifacts {
		if a.Kind != "audio" {
			// 文本产物不属于 music 的 clip（歌词任务走上面的分支）。
			continue
		}
		clips = append(clips, SunoClip{
			ID:            t.PublicID + ":" + strconv.Itoa(a.Index),
			Status:        "complete",
			AudioURL:      in.ContentURL(a.Index),
			Title:         in.Title,
			Text:          in.Lyrics,
			MajorModelVer: in.Version,
			ModelName:     in.Version,
			Metadata: SunoMetadata{
				Tags:                 in.Tags,
				Prompt:               in.Prompt,
				GptDescriptionPrompt: in.Prompt,
			},
		})
	}
	return clips
}
