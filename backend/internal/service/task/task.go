// Package task 是异步任务状态推进的唯一入口（v2.0 §4.7）：
// 提交（白名单 → 计价 → 冻结 → 落库 queued → 上游提交 → CAS running）、
// CAS 状态推进、成功结算、失败/超时/取消的全额释放（E3 异步退款）。
package task

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/mediaprobe"
	"lingdong-api/internal/pkg/metrics"
	"lingdong-api/internal/pkg/netguard"
	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/pkg/pgerr"
	"lingdong-api/internal/pkg/ssrf"
	"lingdong-api/internal/pkg/taskstate"
	"lingdong-api/internal/pkg/textx"
	"lingdong-api/internal/relay"
	"lingdong-api/internal/service/apikey"
	"lingdong-api/internal/service/billing"
	"lingdong-api/internal/service/callio"
	"lingdong-api/internal/service/channellimit"
	"lingdong-api/internal/service/pricing"
	"lingdong-api/internal/service/storage"
	"lingdong-api/internal/service/taskview"
)

// 任务状态机（v2.0 §4.7.1）：queued → running → succeeded | failed | expired | canceled。
//
// 词表只有一份（pkg/taskstate）；这里保留导出名，本包与测试的调用点不用改。
const (
	StatusQueued    = taskstate.Queued
	StatusRunning   = taskstate.Running
	StatusSucceeded = taskstate.Succeeded
	StatusFailed    = taskstate.Failed
	StatusExpired   = taskstate.Expired
	StatusCanceled  = taskstate.Canceled
)

// advanceFrom 是 CAS 允许推进的起始状态（终态不可回退，迟到结果被拒）。
var advanceFrom = []string{StatusQueued, StatusRunning}

// lateFrom 是「迟到成功」允许推进的起始状态（终态复查窗口）。
//
// 与 advanceFrom **不相交**是整条复查链路幂等性的结构性保证：
// 同一个 `UPDATE ... WHERE status IN (from)` 永远只可能在一条路径上命中，
// 不需要额外的锁来防「正常收尾」与「迟到复查」同时推进同一条任务
// （Redis 不可用时 lockSucceed 会放行，那时就只剩这层保证在兜底）。
//
// 改这个变量必须同时跑 TestLateFromDisjointAdvanceFrom。
// canceled 也必须在里面：取消与提交存在竞态窗口 —— Cancel 读到 queued 且无单号，
// CAS 成功并全额退款，而提交线程刚刚把 upstream_task_id 写进去（它随后的
// queued→running CAS 会失败，走 cancel_race 分支只留痕）。上游可能真的在跑，
// 此时只有复查能把产物捞回来并补收；不进 lateFrom 就永远没有候选。
var lateFrom = []string{StatusExpired, StatusFailed, StatusCanceled}

// ─── 终态复查窗口（W1） ───────────────────────────────────────────────────

const (
	// recheckWindowDefault 是复查窗口的默认长度。
	//
	// 6 小时不是拍脑袋：视频任务的上游排队高峰经常在 20 分钟以上，
	// 平台侧 deadline 是 1 小时；超时判失败后上游仍在跑的窗口通常再延 1~2 小时。
	// 给到 6 小时足以覆盖「最慢的那批」，同时把索引里的行数压在同一量级。
	recheckWindowDefault = 6 * time.Hour
	// recheckWindowMax 是硬上限。误配 1000h 会让候选集变成「所有历史失败任务」，
	// 第一次上线就是一次出站洪峰 —— 截断比报警更安全（截断是可见的，洪峰是不可见的）。
	recheckWindowMax = 72 * time.Hour
	// recheckFirstDelay 是进入终态后第一次复查的延迟。
	//
	// 不能是 0：上游在「我们判超时」的同一秒内几乎不可能刚好完成，
	// 立刻查一轮只会制造一次必然落空的出站。
	recheckFirstDelay = 2 * time.Minute
)

// recheckDelays 是复查退避序列，下标 = 已复查次数（tasks.recheck_count）。
//
// 2m → 5m → 5m → 15m → 15m → 30m → 30m → 之后固定 60m。
// 6h 窗口内约 12 次，而不是「每 5 分钟一次」的 72 次：
// 后者在上游集体超时时会把出站量翻倍，而这批任务**没有人在等**。
var recheckDelays = []time.Duration{
	2 * time.Minute, 5 * time.Minute, 5 * time.Minute,
	15 * time.Minute, 15 * time.Minute, 30 * time.Minute, 30 * time.Minute,
}

// recheckDelay 返回第 count 次复查之后应等待多久（超出序列固定 1 小时）。
func recheckDelay(count int) time.Duration {
	if count < 0 {
		count = 0
	}
	if count < len(recheckDelays) {
		return recheckDelays[count]
	}
	return time.Hour
}

// recheckWindow 读 LD_TASK_RECHECK_HOURS（单位小时，可带小数）。
//
// 非法值回落到默认值而不是 0：因为 0 表示「窗口为空」＝该功能静默关闭，
// 一次手滑的配置就会让资金找回能力悄无声息地消失，且没有任何报错。
func recheckWindow() time.Duration {
	raw := strings.TrimSpace(os.Getenv("LD_TASK_RECHECK_HOURS"))
	if raw == "" {
		return recheckWindowDefault
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v <= 0 {
		logger.L().Warn("LD_TASK_RECHECK_HOURS 非法，按默认值处理",
			zap.String("value", raw), zap.Duration("fallback", recheckWindowDefault))
		return recheckWindowDefault
	}
	d := time.Duration(v * float64(time.Hour))
	if d > recheckWindowMax {
		logger.L().Warn("LD_TASK_RECHECK_HOURS 超过硬上限，已截断",
			zap.String("value", raw), zap.Duration("cap", recheckWindowMax))
		return recheckWindowMax
	}
	return d
}

// recheckFieldsOf 生成「开窗」字段（进入终态时随 CAS 一起写入）。
//
// 只在**有上游单号**时开窗：没单号就没有可复查的对象，
// 开窗只会让 idx_tasks_recheck 里多一批必然查不到东西的垃圾行，
// 而且它们要占满 6 小时的候选名额才自然消失。
//
// 为什么需要 ctx 回查一次库（方案原文只传 *model.Task）：调用方手里的视图可能过期。
// 典型场景是「提交线程刚写完 upstream_task_id，另一条路径拿着旧视图判了失败」，
// 此时内存里看是空串、库里其实有单号 —— 不开窗就等于判了死刑，迟到成功永不可达。
// 为了不在常见路径上产生 N+1，**只在内存值为空时**才回查（罕见分支）。
func recheckFieldsOf(ctx context.Context, t *model.Task) map[string]any {
	if t == nil {
		return nil
	}
	tid := strings.TrimSpace(t.UpstreamTaskID)
	if tid == "" && t.ID > 0 {
		var cur string
		if err := db.DB.WithContext(ctx).Model(&model.Task{}).
			Select("COALESCE(upstream_task_id, '')").Where("id = ?", t.ID).
			Scan(&cur).Error; err == nil {
			tid = strings.TrimSpace(cur)
		}
	}
	if tid == "" {
		return nil
	}
	now := time.Now()
	return map[string]any{
		"recheck_until": now.Add(recheckWindow()),
		"recheck_at":    now.Add(recheckFirstDelay),
		// 每次进入终态都是一次新的复查周期：计数归零，退避从头开始。
		// 不归零会让「重试后又失败」的任务直接从 60m 档起步，白丢一段窗口。
		"recheck_count": 0,
	}
}

// withRecheckWindow 把开窗字段合并进 CAS 的 extra（无单号时原样返回）。
func withRecheckWindow(ctx context.Context, t *model.Task, extra map[string]any) map[string]any {
	f := recheckFieldsOf(ctx, t)
	if f == nil {
		return extra
	}
	if extra == nil {
		extra = make(map[string]any, len(f))
	}
	for k, v := range f {
		extra[k] = v
	}
	return extra
}

// closeRecheckWindow 是「关窗」字段：置 NULL 后该行立即从 idx_tasks_recheck 上消失。
//
// 刻意**不动 recheck_count**：它是「这条任务被复查过几次」的观测值，
// 迟到成功最有价值的信息恰好是「查了 7 次才捞回来」。
// 清掉它，就只能从 `late_success_charged` 知道发生过，说不出过程有多曲折。
func closeRecheckWindow() map[string]any {
	return map[string]any{"recheck_until": nil, "recheck_at": nil}
}

// RecheckLease 是复查任务被领取后的租约时长。
//
// 领取 = 把 recheck_at 推到 now()+lease。进程崩溃或 leader 切换后，
// 这条任务在租约到期时自动重新变成候选，不需要额外的 locked_by 列或清理任务。
// 也正因为租约会自动过期，查上游超时不会让任务永久卡死。
const RecheckLease = 10 * time.Minute

// RecheckableStatuses 返回可复查的终态集合（供 worker 拼 SQL）。
//
// 返回**副本**：调用方改了它不能反过来影响 CAS 用的 lateFrom ——
// 那会让「迟到推进」与「正常推进」的起点重叠，幂等保证被静默破坏，
// 表现为偶发的「同一任务既被正常收尾又被迟到收尾」。
func RecheckableStatuses() []string {
	out := make([]string, len(lateFrom))
	copy(out, lateFrom)
	return out
}

// RecheckDelay 暴露退避序列（worker 需要它来决定下次复查时刻）。
func RecheckDelay(count int) time.Duration { return recheckDelay(count) }

// IsTerminal 判断是否已是终态（终态任务不再推进、不再轮询，completed_at 才算有意义）。
// IsTerminal 判断任务是否已到终态（不再推进）。
//
// 判据只有 taskview.IsTerminal 一处：调用记录详情（logs 包）也要判断同一个问题，
// 两处各写一份 switch，新增一个终态就会漏掉一边。
func IsTerminal(status string) bool {
	return taskview.IsTerminal(status)
}

// 任务类别（§4.7.3：kind=video 常规；kind=timeout 为轮询网络失败超限）。
const (
	KindVideo   = "video"
	KindTimeout = "timeout"
)

// TaskTimeout 是任务投递超时（提交时写 deadline_at）；S10 接入 system_config.task.timeout_s。
const TaskTimeout = time.Hour

// statusClientClosed 是用户取消的 http_status 语义值（门户调用记录口径）。
const statusClientClosed = 499

// 任务域错误（由控制器映射为对外错误码）。
var (
	ErrInvalidParam       = errors.New("task: 参数不合法")
	ErrReferenceInvalid   = errors.New("task: 素材链接不合法")
	ErrModelNotFound      = errors.New("task: 模型不存在或未启用")
	ErrModelNotAllowed    = errors.New("task: 模型不在 KEY 白名单")
	ErrModalityNotAllowed = errors.New("task: 模态不在 KEY 白名单")
	ErrNoChannel          = errors.New("task: 无可用渠道")
	// ErrAsyncUnsupported 表示「这个模型的渠道只做同步，接不了 async:true 的任务」。
	//
	// 必须与 ErrUpstreamSubmit(502) 分开：这不是服务故障，是调用方式与模型能力不匹配。
	// 以前它表现为一个 502「渠道配置错误」——用户以为是平台挂了，运维去查渠道，
	// 而真实答案在请求体里（去掉 async 就能用）。
	ErrAsyncUnsupported = errors.New("task: 该模型的渠道不支持异步任务")
	ErrUpstreamSubmit   = errors.New("task: 上游提交失败")
	// ErrRateLimited 表示候选渠道全部被限流（一次上游都没打出去）。
	// 与 ErrUpstreamSubmit 必须分开：前者对外 429（等几秒再来），后者 5xx（报障）。
	ErrRateLimited   = errors.New("task: 渠道限流")
	ErrNotCancelable = errors.New("task: 任务不可取消")
	ErrNotRetryable  = errors.New("task: 任务不可重试")
	ErrNotFound      = errors.New("task: 任务不存在")
	ErrContentGone   = errors.New("task: 产物不可用")
	// ErrNotTerminal 表示任务还在 queued/running，不允许物理删除（B7）。
	ErrNotTerminal = errors.New("task: 任务尚未终结，不可物理删除")
	// ErrArtifactPurgeFailed 表示产物对象删不掉，任务行已保留待下轮重试（B7）。
	ErrArtifactPurgeFailed = errors.New("task: 任务产物删除失败，请稍后重试")
	// ErrArtifactsLost 表示上游声称成功、但一个产物都没转存到本地。
	//
	// 与普通失败分开：此时上游**已经出图并计费**，判失败退款就是纯亏。
	// 迟到复查路径据此「不判成功、不扣款、按退避再试」——
	// 很多情况下只是上游签名链接暂时不可达，几分钟后就正常了。
	ErrArtifactsLost = errors.New("task: 上游产物全部转存失败")
)

// RefSummary 是素材摘要（§4.7.2：仅 kind/sha256/size_bytes，不含素材本体）。
type RefSummary struct {
	Kind      string `json:"kind"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

// SubmitRequest 是异步任务提交的解析视图（§4.7.2 / §4.7.6）。
type SubmitRequest struct {
	Model      string
	Prompt     string
	Seconds    int64
	Resolution string // 归一化后的档位键：480p/720p/1080p（无法识别则原样，交由计价拒绝）
	Refs       []RefSummary
	Body       []byte // 原始请求体（转发用：model 字段由适配器按绑定别名替换）
	Params     params // 落库快照（仅参数 + 素材摘要）

	// RefVideoSeconds 是参考视频累计秒数（由 Submit 在冻结前探测后回填）。
	// 单值口径：不放进 RefSummary，避免「冻结」与「结算」出现两条聚合路径。
	RefVideoSeconds int64

	// SourceTaskRef 是再生成请求里的**平台**任务号原文（`task_` 前缀，不落库：
	// 落库的是换好的上游号）。
	//
	// 存原文而不是当场解析成数值：解析要查库、要判归属，而 Parse* 是纯函数；
	// 归属边界在 applyRegeneration 的 SQL 里（user_id + app_id 一起判）。
	SourceTaskRef string

	// links 为素材原始链接（与 Refs 同序；不落库、不对外），供 Submit 做 DNS 解析后 SSRF 校验（S8 补全）。
	links []string
	// hasInlineVideo 表示含 data: 内联参考视频（读不到时长 → 按秒计费时必须在冻结前拒）。
	hasInlineVideo bool
}

// inputRefItem 是 input_reference 的一项（type + 三种链接字段）。
type inputRefItem struct {
	Type     string `json:"type"`
	ImageURL string `json:"image_url"`
	AudioURL string `json:"audio_url"`
	VideoURL string `json:"video_url"`
}

// refCollect 是一次素材解析的产物（三个视图同序，供计费与重放各自消费）。
type refCollect struct {
	refs       []RefSummary
	links      []string
	storedRefs []InputRefSummary
	hasInline  bool
	// hasInlineVideo 只标内联**参考视频**：参考视频按秒计费但内联体读不到时长，
	// 必须在冻结前拒（内联图片不受影响，它有固定单价）。
	hasInlineVideo bool
}

// InputRefSummary 是素材引用摘要（type + 可重放的 URL）。
//
// 与 RefSummary 的分工：RefSummary 只服务计费（kind/sha256/size），
// InputRefSummary 只服务「重试时重建上游请求」。两者并存是因为
// usageOf 的口径不能动（存量任务与对账都依赖它），而重放需要 type 与 URL。
type InputRefSummary struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

// params 是 tasks.request_params 的存储视图。
type params struct {
	Model      string         `json:"model"`
	Prompt     string         `json:"prompt,omitempty"`
	Seconds    int64          `json:"seconds"`
	Resolution string         `json:"resolution"`
	References []RefSummary   `json:"references,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"` // 厂商私有参数透传快照（D3：对齐 new-api 响应模型）
	// InputRef 是 http(s) 素材引用（不含内联素材），供重试重建请求（v2 修复错账）。
	InputRef []InputRefSummary `json:"input_ref,omitempty"`
	// HasInline 表示请求里含 data: 内联素材：这类任务无法重放（内联体不落库），必须拒绝重试。
	HasInline bool `json:"has_inline,omitempty"`
	// Seed 是随机种子（顶层 seed 或 metadata.seed），任务表无独立列，靠它重放。
	Seed *int64 `json:"seed,omitempty"`
	// RefVideoSeconds 是参考视频累计秒数（冻结与结算共用这一个值）。
	RefVideoSeconds int64 `json:"ref_video_seconds,omitempty"`
	// Kind 是任务子类型："regeneration" 表示再生成；生成留空（旧任务天然兼容）。
	Kind string `json:"kind,omitempty"`
	// SecondsAuto 表示上游使用「智能时长」（如 Ark 的 duration=-1）。
	//
	// 为什么要独立字段而不是用 Seconds=-1 表达：
	//   - Seconds 是**计费基准**，必须是正数（0 或负会被 clamp 成 0 → 0 元冻结 → 白送）；
	//   - 上游参数需要的是 -1。两个语义必须分开存，否则总有一边是错的。
	// 重放（videoReplay）与结算（usageOf）分别读这两个字段。
	SecondsAuto bool `json:"seconds_auto,omitempty"`
	// SourceUpstream 是再生成源任务的**上游**任务号（重放用，平台任务号换成它才能再次提交）。
	SourceUpstream string `json:"source_upstream,omitempty"`
	// SeqImageGen / MaxImages / LayerDecomp 是「一次调用可能产出几张」的三个入参。
	//
	// 必须落库：提交期用它算冻结张数，重放与结算都要复原同一个数。
	// 以前异步链路根本不解析它们（直接 400 拒掉组图），因为冻结口径只有 relay 有。
	SeqImageGen string `json:"seq_image_gen,omitempty"`
	MaxImages   int64  `json:"max_images,omitempty"`
	LayerDecomp bool   `json:"layer_decomp,omitempty"`
	// Anomaly 是提交期产生的对账标记（档位未配价、未传时长…），结算时并进 usages.anomaly。
	//
	// 参数不再被拦截之后，"平台按兜底口径计费"这件事只能靠标记留证：
	// 否则「请求 1440p、按 1080p 收」永远没人发现。
	Anomaly string `json:"anomaly,omitempty"`

	// ---- 多模态参数（图像 / 音乐 / 歌词）----
	//
	// 为什么要整体落库：tasks 表只存参数快照，不存原始请求体。这些字段不落库的话，
	// 重试（replayedBody）与结算（usageOf）都读不到——前者会把「2 张 2K」重放成「1 张默认」，
	// 后者会按错档位计价。全部带 omitempty，旧任务的反序列化结果与改造前逐字节一致。
	// ImageSpec 是**归一后的计费档位**（1K/2K/3K/4K；空 = 价目表推不出默认档）。
	//
	// 它只服务计费，绝不写回重放体：把用户的 2048x1152 重放成 "2K" 会让上游改出方形图，
	// 把原本没写 size 的请求重放成 "2K" 会让上游默认档被替换（nano-banana v1 甚至直接 400）。
	ImageSpec string `json:"image_spec,omitempty"`
	// SizeRaw / ResolutionRaw / ImageSizeRaw 是用户原文，重放体只认它们。
	SizeRaw       string `json:"size_raw,omitempty"`
	ResolutionRaw string `json:"resolution_raw,omitempty"`
	ImageSizeRaw  string `json:"image_size_raw,omitempty"`
	// ImageRatio / ImageExplicit / ImageSource 是归一结果的留痕（比例、是否用户显式指定、来源）。
	ImageRatio    string `json:"image_ratio,omitempty"`
	ImageExplicit bool   `json:"image_explicit,omitempty"`
	ImageSource   string `json:"image_source,omitempty"`
	// ImageN 是单次生成张数（0 视为未传，落库前归一为 1）。
	ImageN int64 `json:"image_n,omitempty"`
	// AspectRatio / Quality 只透传给上游，不参与计价（计价维度必须收敛在档位×张数上）。
	AspectRatio string `json:"aspect_ratio,omitempty"`
	Quality     string `json:"quality,omitempty"`
	// Background 是图像编辑参数（透明背景），只透传。
	//
	// 蒙版不在这里：它的平台契约名是 mask_url，原文随 tasks.submit_body 一起落库，
	// 再在快照里存一份同义字段就是第二个出处（改名时必然只改一处）。
	Background string `json:"background,omitempty"`
	// MusicMV / Tags / Title 是音乐生成参数（模型版本 / 风格标签 / 标题）。
	MusicMV string `json:"music_mv,omitempty"`
	// MusicAction 是这条音乐任务的**动作**（music / lyrics）。
	//
	// 必须落库：`/suno/fetch` 要回的是动作，而任务行里只有模态（都是 music）。
	// 靠产物是音频还是文本来反推在**未完成时**推不出来 —— 客户端恰恰在未完成时
	// 查得最频繁。
	MusicAction string `json:"music_action,omitempty"`
	// MusicClips 是**按条计费**（per_clip）音乐任务在冻结期写入的计费条数。
	//
	// 为什么落在 params 而不是 usages 新列：结算期 `usageOf(t)` 只有这一行可读，
	// 而 ImageN 已经在冻结/结算/回收三处打通 —— 把"件数"复用成同一个字段，
	// 口径就只可能有一套（per_image 是张、per_clip 是条）。
	MusicClips int64  `json:"music_clips,omitempty"`
	Tags       string `json:"tags,omitempty"`
	Title      string `json:"title,omitempty"`
}

// KindRegeneration 是再生成任务的 params.Kind 取值。
const KindRegeneration = "regeneration"

// SubmitInput 是提交入参（Key 提供归属与白名单，Group 为调用分组）。
type SubmitInput struct {
	Key       *model.APIKey
	Group     string
	IP        string
	RequestID string
	Req       *SubmitRequest
	// ExpectModality 是调用入口声明的模态（video/image/music/audio）：提交前与模型模态比对。
	//
	// 为什么必须在服务层再校验一次：relayRequest 的模态闸门只覆盖同步端点；
	// 异步端点直接进 Submit，若不校验，一个 quota_type=0 的 chat 模型被拿去建视频任务时
	// Estimate 恒为 0（异步链路不做 token 估算），跑完就是免费出片。
	ExpectModality string
	// EndpointKey 是**入站端点**（models.endpoints 写法，如 "video/generations"），
	// 由 controller 按路由填写，是能力判定与适配器出站的唯一依据。
	//
	// 为什么不让服务层按 modality 推：一个模态可能对应多个异步端点（video 有 generations
	// 与 regenerations，music 有 music 与 lyrics），按模态推会把再生成当普通生成提交。
	// 空串 = 重试路径：只要求「该适配器能做异步」。
	EndpointKey string
	// IdempotencyKey 是客户端声明的幂等键（B4-2，来自 Idempotency-Key 头）。
	//
	// 语义：同一 KEY + 同一幂等键只建一个任务，重复提交直接返回首次创建的任务行。
	// 为什么这个能力必须存在：提交是「冻结 + 上游建单」两步，客户端超时重试时
	// 第一次可能已经成功——不幂等就是上游双倍成本 + 用户被冻结两次。
	// 空串 = 不启用（既有行为逐字不变）。
	IdempotencyKey string
	// RawBody 是客户端提交的原始请求体，只用于调用记录（call_io）。
	//
	// 为什么在 controller 传而不是 service 里回读 gin.Context：
	// service 层不应该知道 HTTP 细节；而且 body 在 controller 里已经读出来
	// （readSubmitBody），再读一次只会拿到空。
	RawBody []byte
	// ReqSource 标注 RawBody 的形态：raw（客户端 JSON 原样）/
	// rebuilt（multipart 被平台重构成 JSON，文件已转 data:）。
	ReqSource string
}

// Scope 限定任务归属（0 表示该维度不限制）。
//
// 注意：**零值不等于「全表」**。空 Scope 在过去等价于「不加任何条件」，
// 那是一个危险默认 —— 未来新增一个 handler 忘了传归属，就是越权读写别人的任务，
// 而且不留痕迹。现在必须显式写 AllScope()，零值会被拒绝（见 ErrScopeRequired）。
type Scope struct {
	UserID uint64
	KeyID  uint64
	// All 是**显式**的「不限定归属」：只给管理端与签名媒体路径用。
	All bool
}

// AllScope 是显式的「不限定归属」。调用它就等于在代码里写明"我知道我在跨用户读"，
// 评审时能被看见；空 Scope 则会让评审以为「这里限定了，只是值恰好是 0」。
func AllScope() Scope { return Scope{All: true} }

// bounded 报告本作用域是否显式限定了归属。
func (s Scope) bounded() bool { return s.All || s.UserID != 0 || s.KeyID != 0 }

// ErrScopeRequired 表示调用方没给任何归属限制（编程错误，不是用户错误）。
//
// 对外不暴露：handler 应当把它当成 500 处理，并在日志里带上调用点。
var ErrScopeRequired = errors.New("task: 查询任务必须显式限定归属（管理端请用 AllScope()）")

// applyScope 把作用域条件挂到查询上。
func (s Scope) applyScope(q *gorm.DB) *gorm.DB {
	if s.UserID != 0 {
		q = q.Where("user_id = ?", s.UserID)
	}
	if s.KeyID != 0 {
		q = q.Where("key_id = ?", s.KeyID)
	}
	return q
}

// rejectEmptyScope 是所有「按 id/号查任务」入口的统一闸门。
//
// 返回 error 而不是 panic：panic 会丢掉整个请求上下文（谁在调、参数是什么），
// 而这正是一个越权 bug 最需要的信息。
func rejectEmptyScope(op string, scope Scope, id uint64) error {
	if scope.bounded() {
		return nil
	}
	logger.L().Error("任务查询未限定归属，已拒绝（请改用 task.AllScope()）",
		zap.String("op", op), zap.Uint64("task_id", id), zap.Bool("task_scope_missing", true))
	metrics.Inc(metrics.NameTaskScopeMissing, "op", op)
	return ErrScopeRequired
}

// ListFilter 是任务列表筛选（T21 分页 + 状态/模型/用户）。
//
// After 是游标（只取 id 更小的记录，配合 id DESC 的倒序），用于 /v1/tasks 的
// after/has_more 契约；与 Page 互斥，给了 After 就忽略 Page。
type ListFilter struct {
	Status   string
	Model    string
	Modality string
	UserID   uint64
	// AppID / AppScoped 按「任务归属的应用」过滤。
	// 归因链 tasks.key_id → api_keys.app_id（appref 是唯一派生点），不落冗余列。
	// AppScoped 让「只看平台直连（app_id=0）」可表达 —— 缺了它，0 只能当「没传」。
	AppID     uint64
	AppScoped bool
	Page      int
	PageSize  int
	After     uint64
	// OrderBy 是已过白名单校验的 ORDER BY 片段（空 = 历史默认 `id DESC`）。
	//
	// 只认"能安全拼进 SQL 的片段"：对外字段名到列的映射与白名单校验都在 handler
	// （pkg/sorting），service 不再看 `sort` 原文。
	// 给了 After（游标分页）时它会被忽略 —— 见 List 里的说明。
	OrderBy string
}

// ParseSubmit 解析并校验提交请求：参数 + 素材摘要（素材默认透传原 URL，仅做 SSRF 校验）。
// 入口先做 new-api 兼容别名归一（D2：duration→seconds、width×height→resolution、image→首帧参考），
// 归一后的请求体同时用于解析与转发，上游只会收到本平台规范字段。
//
// 这里**只做解析，不做模态相关的必填校验**：同一个端点承载视频/图像/音乐，
// "seconds 必填"只对视频成立，而 modality 要到 Submit 里查模型才知道。
// 模态校验见 validateModality（非法值仍在此处 400，例如 seconds="abc"）。
func ParseSubmit(body []byte) (*SubmitRequest, error) {
	body, err := normalizeSubmitBody(body)
	if err != nil {
		// 归一阶段自己的参数错（image_urls 与 image 冲突、元素类型错）必须原样上抛：
		// 折成通用的「参数不合法」会让用户只能靠猜。
		if errors.Is(err, ErrInvalidParam) {
			return nil, err
		}
		return nil, ErrInvalidParam
	}
	var raw struct {
		Model   string          `json:"model"`
		Prompt  string          `json:"prompt"`
		Seconds json.RawMessage `json:"seconds"`
		// Duration 是厂商原生时长字段（Ark 的 duration）。
		//
		// 必须认它：原生体（content[] 形态）里没有 seconds，只有 duration。
		// 不认的后果是「照着官方文档粘贴的请求在本平台 400 必须指定 seconds」，
		// 而用户看不出这两者是同一个语义。
		Duration    json.RawMessage `json:"duration"`
		Size        string          `json:"size"`
		ImageSize   string          `json:"image_size"`
		Resolution  string          `json:"resolution"`
		Seed        json.RawMessage `json:"seed"`
		Metadata    map[string]any  `json:"metadata"`
		InputRef    []inputRefItem  `json:"input_reference"`
		N           json.RawMessage `json:"n"`
		AspectRatio string          `json:"aspect_ratio"`
		Quality     string          `json:"quality"`
		// 组图 / 图层：决定「一次调用最多产出几张」，冻结要用，必须一并解析。
		SeqImageGen string          `json:"sequential_image_generation"`
		LayerDecomp json.RawMessage `json:"layer_decomposition"`
		SeqOptions  struct {
			MaxImages json.RawMessage `json:"max_images"`
		} `json:"sequential_image_generation_options"`
		Background string `json:"background"`
		MV         string `json:"mv"`
		Tags       string `json:"tags"`
		Title      string `json:"title"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, ErrInvalidParam
	}
	if strings.TrimSpace(raw.Model) == "" {
		return nil, ErrInvalidParam
	}
	// 三态：缺失（present=false）不再报错，但**类型非法仍 400**。
	// 两者必须分开：把 "" / "abc" 当成"未传"会静默按 0 秒冻结（等于白送），
	// 把"未传"当非法又会挡掉图像/音乐（它们本来就没有秒数概念）。
	secs, present, ok := parseSeconds(raw.Seconds)
	if !ok {
		return nil, ErrInvalidParam
	}
	if !present {
		// seconds 缺失时回落到 duration（原生体的写法）。两者都是「时长」，
		// 但计费基准只有一个：先到先得，绝不相加或取最大。
		secs, _, ok = parseSeconds(raw.Duration)
		if !ok {
			return nil, ErrInvalidParam
		}
	}
	// 档位此处**不做归一**：视频档位（480p/720p/1080p）与图像档位（1K/2K/4K）
	// 语义完全不同，在解析阶段统一压成视频档会让图像档位信息不可恢复
	// （1024x1024 → 1080p 之后，图像永远命中不了 1K 档）。归一延后到 validateModality。
	resolution := firstNonEmpty(raw.Resolution, raw.Size)
	// 图像尺寸的解析与归一都在 billing（与同步链路同一个函数），这里只保留原文。
	//
	// 原文必须留：重放体要用它（见 params.SizeRaw 的注释），而归一后的档位只进计费。
	// 归一本身在 validateImageModality 做 —— 那里才有模型价目表（决定「卖哪几档」）。

	col, err := collectRefs(raw.InputRef)
	if err != nil {
		return nil, err
	}
	// 厂商原生体（Ark 的 content[]）：素材写在 content 数组里，input_reference 为空。
	//
	// 必须在**提交时**抽取并落库（而不是留给适配器）：结算是读 request_params 的，
	// 素材个数不落库 = 参考图/参考视频全部计 0（配了 ref_image_price 也全免），
	// 并且 SSRF 校验被整条绕过（原生体能让上游去抓内网地址）。
	//
	// 两条素材写法互斥：同时接受会造出「按 content 计费、按 input_reference 出站」的
	// 双重口径（Ark 原生通道只认 content，input_reference 会被原样发过去成未知字段）。
	if items, native, nerr := nativeContentItems(body); nerr != nil {
		return nil, nerr
	} else if native {
		if len(raw.InputRef) > 0 {
			return nil, fmt.Errorf("%w：content[] 与 input_reference 只能二选一（content[] 是厂商原生体写法）", ErrInvalidParam)
		}
		if col, err = collectRefs(items); err != nil {
			return nil, err
		}
	}

	seed, serr := parseSeed(raw.Seed, raw.Metadata)
	if serr != nil {
		return nil, ErrInvalidParam
	}
	// 张数宽松解析：类型不符按未传（图像之外的模态根本不认这个字段）。
	n := parseLooseInt(raw.N)

	return &SubmitRequest{
		Model: raw.Model, Prompt: raw.Prompt, Seconds: secs, Resolution: resolution,
		Refs: col.refs, Body: body, links: col.links, hasInlineVideo: col.hasInlineVideo,
		Params: params{
			Model: raw.Model, Prompt: raw.Prompt, Seconds: secs, Resolution: resolution,
			References: col.refs, Metadata: raw.Metadata,
			InputRef: col.storedRefs, HasInline: col.hasInline, Seed: seed,
			ImageN:  n,
			SizeRaw: strings.TrimSpace(raw.Size), ResolutionRaw: strings.TrimSpace(raw.Resolution),
			ImageSizeRaw: strings.TrimSpace(raw.ImageSize),
			SeqImageGen:  strings.TrimSpace(raw.SeqImageGen), MaxImages: parseLooseInt(raw.SeqOptions.MaxImages),
			LayerDecomp: looseTrue(raw.LayerDecomp),
			AspectRatio: strings.TrimSpace(raw.AspectRatio), Quality: strings.TrimSpace(raw.Quality),
			Background: strings.TrimSpace(raw.Background),
			MusicMV:    strings.TrimSpace(raw.MV), Tags: strings.TrimSpace(raw.Tags), Title: strings.TrimSpace(raw.Title),
		},
	}, nil
}

// ParseVideoSubmit 是**视频提交**的解析入口。
//
// 与 ParseSubmit 的唯一差别：在通用归一之前，把厂商原生的顶层 `image_urls`
// 折叠成 `input_reference`（type=image）。
//
// 为什么必须折叠而不是原样透传（那是本轮整改前的做法）：
//   - 平台只对 `in.Req.links` 做 SSRF 校验，而 links 只从 input_reference 归集 ——
//     顶层数组会被整条绕过，上游可以拿它去抓内网地址；
//   - 结算读的是落库的 input_reference（参考图张数与单价），顶层数组不落库
//     ⇒ 参考图全部计 0，而账单照收。
//
// 为什么只在视频入口做：图像端点的 `image_urls` 是另一套语义（部分适配器把它当
// 上游原生字段直接转发），在这里一并折叠会改变那些模型的行为。
func ParseVideoSubmit(body []byte) (*SubmitRequest, error) {
	body, err := foldVideoImageURLs(body)
	if err != nil {
		return nil, err
	}
	// 视频入口专属归一（别名删键 + role 翻译），详见 video_ref.go。
	// 放在 ParseSubmit 之前：里面会再跑一次 normalizeSubmitBody，那时别名已删、
	// role 已翻译，它变成幂等空操作。
	body, err = normalizeVideoBody(body)
	if err != nil {
		return nil, err
	}
	return ParseSubmit(body)
}

// foldVideoImageURLs 把顶层 image_urls 折叠成 input_reference 并删掉原键。
//
// 三条规则（每条都对应一类真实故障）：
//  1. 有 `content[]` 时原样返回：那是厂商原生体，`image_urls` 不属于它的语义，
//     折叠进去会造出两套素材口径；
//  2. 与 `input_reference` 同时出现 ⇒ 400：两种素材写法并存时，"按哪套计费、
//     按哪套出站"没有隐式优先级 —— content[] 与 input_reference 已经是这个口径；
//  3. 元素必须是非空字符串：静默跳过会让用户以为图传上了（而计费按张数）。
func foldVideoImageURLs(body []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, ErrInvalidParam
	}
	if _, hasContent := m["content"]; hasContent {
		return body, nil
	}
	raw, ok := m["image_urls"]
	if !ok {
		return body, nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%w：image_urls 必须是字符串数组", ErrInvalidParam)
	}
	if _, hasRef := m["input_reference"]; hasRef {
		return nil, fmt.Errorf("%w：image_urls 与 input_reference 只能二选一", ErrInvalidParam)
	}
	// `image` 现在也接受数组，两者是同一个语义的两种载体。
	// 同时给就是"两份多图列表"，按哪份出站、按哪份计费没有隐式优先级 ——
	// 与 image_urls + input_reference 同一口径。
	if _, hasImage := m["image"]; hasImage {
		return nil, fmt.Errorf("%w：image_urls 与 image 只能二选一", ErrInvalidParam)
	}
	refs := make([]any, 0, len(arr))
	for _, v := range arr {
		s, isStr := v.(string)
		if !isStr || strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("%w：image_urls 的每个元素都必须是非空字符串（公网直链）", ErrInvalidParam)
		}
		refs = append(refs, map[string]any{"type": "image", "image_url": strings.TrimSpace(s)})
	}
	delete(m, "image_urls")
	if len(refs) > 0 {
		m["input_reference"] = refs
	}
	return json.Marshal(m)
}

// ParseRegenerate 解析再生成请求（POST /v1/video/regenerations）。
//
// 与 ParseSubmit 的差别只有三处：**不要求 seconds**（由源任务或 base_video 决定）、
// 要求 source_task_id 或 base_video 之一、params.Kind 固定为 regeneration。
func ParseRegenerate(body []byte) (*SubmitRequest, error) {
	body, err := normalizeSubmitBody(body)
	if err != nil {
		return nil, ErrInvalidParam
	}
	var raw struct {
		Model        string          `json:"model"`
		Prompt       string          `json:"prompt"`
		Size         string          `json:"size"`
		Resolution   string          `json:"resolution"`
		SourceTaskID json.RawMessage `json:"source_task_id"`
		Metadata     map[string]any  `json:"metadata"`
		InputRef     []inputRefItem  `json:"input_reference"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, ErrInvalidParam
	}
	if strings.TrimSpace(raw.Model) == "" {
		return nil, ErrInvalidParam
	}
	// 档位同样不在这里归一：配了 resolution_fixed 的模型（可灵）请求根本不传档位，
	// 在这里要求必填会让再生成直接 400。归一与兜底统一由 validateModality 负责。
	resolution := firstNonEmpty(raw.Resolution, raw.Size)
	srcRef := strings.Trim(strings.TrimSpace(string(raw.SourceTaskID)), `"`)
	if srcRef == "null" {
		srcRef = ""
	}
	if srcRef != "" && !IsPublicID(srcRef) {
		// 形态错就是参数错：让它走到「二选一」分支会被答成"缺 source_task_id"，
		// 而真实原因是号写错了（例如把上游单号填了进来）。
		return nil, fmt.Errorf("%w：source_task_id 必须是本平台的任务号（task_ 开头）", ErrInvalidParam)
	}
	col, err := collectRefs(raw.InputRef)
	if err != nil {
		return nil, err
	}
	hasBaseVideo := false
	for _, r := range col.storedRefs {
		if r.Type == "base_video" {
			hasBaseVideo = true
			break
		}
	}
	if srcRef == "" && !hasBaseVideo {
		return nil, fmt.Errorf("%w：再生成需要 source_task_id 或 base_video 之一", ErrInvalidParam)
	}
	if srcRef != "" && hasBaseVideo {
		return nil, fmt.Errorf("%w：source_task_id 与 base_video 只能二选一", ErrInvalidParam)
	}

	return &SubmitRequest{
		Model: raw.Model, Prompt: raw.Prompt, Resolution: resolution,
		Refs: col.refs, Body: body, links: col.links, SourceTaskRef: srcRef,
		Params: params{
			Model: raw.Model, Prompt: raw.Prompt, Resolution: resolution,
			References: col.refs, Metadata: raw.Metadata,
			InputRef: col.storedRefs, HasInline: col.hasInline, Kind: KindRegeneration,
		},
	}, nil
}

// collectRefs 把 input_reference 解析成三个视图（计费 / 链接 / 重放）。
func collectRefs(items []inputRefItem) (*refCollect, error) {
	out := &refCollect{
		refs:       make([]RefSummary, 0, len(items)),
		links:      make([]string, 0, len(items)),
		storedRefs: make([]InputRefSummary, 0, len(items)),
	}
	for _, r := range items {
		kind, link := refKind(r.Type, r.ImageURL, r.AudioURL, r.VideoURL)
		if kind == "" || strings.TrimSpace(link) == "" {
			return nil, ErrInvalidParam
		}
		// 素材链接按形态分三档，**判定只有这一处**（出站抓取与重放都按它走）：
		//   - inline：data: 内联体，不落 URL 也不出网；
		//   - asset：上游素材库 ID（Ark 的 asset://，预置素材/虚拟人像），既不是 URL
		//     也不该被 SSRF 拒绝 —— 它是上游自己的标识，平台只负责原样转发；
		//   - remote：http(s)，必须过 SSRF 校验（防内网探测 / DNS rebinding）。
		switch refLinkScheme(link) {
		case "inline":
		case "asset":
		case "remote":
			if ssrf.ValidatePublicURL(link) != nil {
				return nil, ErrReferenceInvalid
			}
		default:
			// file:// / ftp:// / 无 scheme 等一律拒：上游抓不到，且这类写法
			// 唯一的现实用途就是绕开 SSRF 校验（形如 file:///etc/passwd）。
			return nil, ErrReferenceInvalid
		}
		out.refs = append(out.refs, RefSummary{Kind: kind, SHA256: refDigest(link), SizeBytes: refSize(link)})
		out.links = append(out.links, link)
		if refLinkScheme(link) == "inline" {
			// 内联素材不落 URL：9 张图 + 3 段音频的 base64 会把 request_params 撑到几 MB。
			// 但必须记住「有过内联素材」，否则重试会静默退化成纯文本请求。
			out.hasInline = true
			if kind == "video" {
				// 参考视频按秒计费：内联体读不到时长，必须在冻结前拒（见 probeRefVideos）。
				out.hasInlineVideo = true
			}
			continue
		}
		out.storedRefs = append(out.storedRefs, InputRefSummary{Type: inputRefType(r.Type, kind), URL: link})
	}
	return out, nil
}

// refLinkScheme 归一素材链接的形态：inline / asset / remote / ""（非法）。
//
// 三档判定必须唯一：SSRF 校验、参考视频探测、素材落库都读它，
// 任何一处另写 `strings.HasPrefix(link, "data:")` 都会漏掉 asset:// 那一档。
func refLinkScheme(link string) string {
	l := strings.ToLower(strings.TrimSpace(link))
	switch {
	case strings.HasPrefix(l, "data:"):
		return "inline"
	case strings.HasPrefix(l, "asset://"):
		return "asset"
	case strings.HasPrefix(l, "http://"), strings.HasPrefix(l, "https://"):
		return "remote"
	default:
		return ""
	}
}

// nativeContentItems 把厂商原生体的 content[] 归一成平台素材项（type + 链接）。
//
// 只做**形态**归一，不做语义判定：SSRF、内联体、素材摘要全部交给 collectRefs，
// 保证原生体与规范体走同一条素材管道（两条管道的结果必然是「一条漏校验、一条漏计费」）。
//
// 「是不是原生体」的判据与适配器完全一致（content 能解成非空数组），
// 判定分叉的后果是「平台按规范体计费、适配器按原生体出站」—— 两边都自洽，账是错的。
//
// 不认识的内容类型直接 400：静默跳过等于「用户以为素材进去了、平台按 0 个素材计费」。
func nativeContentItems(body []byte) ([]inputRefItem, bool, error) {
	var probe struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, false, nil
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(probe.Content, &raws); err != nil || len(raws) == 0 {
		return nil, false, nil
	}
	items := make([]inputRefItem, 0, len(raws))
	for _, raw := range raws {
		var it struct {
			Type     string          `json:"type"`
			Role     string          `json:"role"`
			ImageURL json.RawMessage `json:"image_url"`
			VideoURL json.RawMessage `json:"video_url"`
			AudioURL json.RawMessage `json:"audio_url"`
		}
		if err := json.Unmarshal(raw, &it); err != nil {
			return nil, false, fmt.Errorf("%w：content[] 的元素必须是对象", ErrInvalidParam)
		}
		switch strings.ToLower(strings.TrimSpace(it.Type)) {
		case "text", "draft_task":
			// 文本与样片任务 ID 不是素材：不占素材费，也不需要落库重放。
			continue
		case "image_url":
			u := contentURL(it.ImageURL)
			if u == "" {
				return nil, false, fmt.Errorf("%w：content[] 的 image_url 缺少 url", ErrInvalidParam)
			}
			items = append(items, inputRefItem{Type: inputRefType(it.Role, "image"), ImageURL: u})
		case "video_url":
			u := contentURL(it.VideoURL)
			if u == "" {
				return nil, false, fmt.Errorf("%w：content[] 的 video_url 缺少 url", ErrInvalidParam)
			}
			items = append(items, inputRefItem{Type: "video", VideoURL: u})
		case "audio_url":
			u := contentURL(it.AudioURL)
			if u == "" {
				return nil, false, fmt.Errorf("%w：content[] 的 audio_url 缺少 url", ErrInvalidParam)
			}
			items = append(items, inputRefItem{Type: "audio", AudioURL: u})
		default:
			return nil, false, fmt.Errorf("%w：content[] 含不支持的内容类型 %q（支持 text / image_url / video_url / audio_url / draft_task）",
				ErrInvalidParam, it.Type)
		}
	}
	return items, true, nil
}

// contentURL 读取 content 元素里的链接：官方形态是 {"url": "..."}，
// 也接受裸字符串写法（客户端常把它当字段而不是对象传）。
func contentURL(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var obj struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return strings.TrimSpace(obj.URL)
	}
	return ""
}

// inputRefType 归一素材类型（供重放；别名与适配器保持一致）。
func inputRefType(typ, kind string) string {
	switch t := strings.ToLower(strings.TrimSpace(typ)); t {
	case "first-frame":
		return "first_frame"
	case "last-frame":
		return "last_frame"
	case "":
		return kind
	default:
		return t
	}
}

// parseSeed 解析随机种子（顶层 seed 优先，其次 metadata.seed），必须为整数。
//
// 任务表没有 seed 列：不在这里记住它，重试就会换一个随机种子，
// 同一提示词重试出完全不同的结果，用户无法复现。
func parseSeed(raw json.RawMessage, md map[string]any) (*int64, error) {
	if s := strings.TrimSpace(string(raw)); s != "" && s != "null" {
		text := strings.Trim(strings.TrimSpace(s), `"`)
		// 先按整数精确解析：走 float64 会把超过 2^53 的种子静默改写
		// （9007199254740993 → …992），而那正是「用户指定了同一个种子、
		// 却拿到不同结果」的来源——错得很小，但不可复现。
		if n, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64); err == nil {
			return &n, nil
		}
		v, err := strconv.ParseFloat(text, 64)
		if err != nil || v != float64(int64(v)) {
			return nil, ErrInvalidParam
		}
		n := int64(v)
		return &n, nil
	}
	v, ok := md["seed"]
	if !ok {
		return nil, nil
	}
	switch t := v.(type) {
	case float64:
		if t != float64(int64(t)) {
			return nil, ErrInvalidParam
		}
		n := int64(t)
		return &n, nil
	case json.Number:
		i, err := t.Int64()
		if err != nil {
			return nil, ErrInvalidParam
		}
		return &i, nil
	default:
		return nil, ErrInvalidParam
	}
}

// jsonNumber 读取一个数值型字段（UseNumber 之后的 json.Number / 字符串写法 / 老式 float64）。
//
// 为什么三种都认：请求体来自任意客户端，`"duration":"5"` 与 `"duration":5` 都见过；
// 只认其中一种会让另一种静默失效（时长丢失 → 按 0 秒冻结 → 白送一次生成）。
func jsonNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case float64:
		return t, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}

// normalizeSubmitBody 做 new-api 兼容别名归一（D2）：
//   - duration → seconds（seconds 未传时）
//   - width × height → resolution "WxH"（resolution/size 均未传时）
//   - image（URL/base64 字符串）→ input_reference 首元素（首帧参考）
//
// 其余字段（含 metadata、厂商扩展）原样保留；非 JSON 交由上层解析报错。
//
// 厂商原生体（非空 `content[]`）**整体原样返回**，一个别名都不注入：
// 原生通道的语义是"除 model 外逐字节转发"，注入 seconds/resolution 就是往 Ark
// 请求体里塞它不认识的顶层字段；注入 image 更糟——凭空多出一份素材，
// 而平台侧并不会为它计费（原生体禁重试、素材从 content 抽取）。
func normalizeSubmitBody(body []byte) ([]byte, error) {
	// UseNumber：默认解码成 float64 会把大整数改写（seed 1234567890123 →
	// 1.234567890123e+12），归一后的请求体再发给上游就是另一个种子/另一份参数。
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if arr, ok := m["content"].([]any); ok && len(arr) > 0 {
		return body, nil // 与适配器的 arkIsNative 同一判据，见 nativeContentItems
	}
	if _, hasSeconds := m["seconds"]; !hasSeconds {
		if f, ok := jsonNumber(m["duration"]); ok {
			m["seconds"] = json.Number(strconv.FormatFloat(f, 'f', -1, 64))
		}
	}
	if _, hasRes := m["resolution"]; !hasRes {
		if _, hasSize := m["size"]; !hasSize {
			w, wok := jsonNumber(m["width"])
			h, hok := jsonNumber(m["height"])
			if wok && hok && w > 0 && h > 0 {
				m["resolution"] = fmt.Sprintf("%dx%d", int(w), int(h))
			}
		}
	}
	if s, ok := m["image"].(string); ok && strings.TrimSpace(s) != "" {
		ref := map[string]any{"type": "image", "image_url": strings.TrimSpace(s)}
		switch refs := m["input_reference"].(type) {
		case []any:
			m["input_reference"] = append([]any{ref}, refs...)
		default:
			m["input_reference"] = []any{ref}
		}
	}
	// 数组形态的 `image`（多张参考图）同样折进 input_reference，**顶层键保留**给适配器读。
	//
	// 为什么数组也要折：collectRefs 只认 input_reference，数组形态此前只留在顶层，
	// 于是异步链路数不到参考图（RefImages=0，与同步链路口径不一致），
	// 而且这些 URL **绕过了 SSRF 校验**（内联/相对路径能一路进到适配器）。
	// 顶层键不能删：同步适配器读的是 image / image_urls 原文（见 duomiSyncImageURLs）。
	if arr, ok := m["image"].([]any); ok && len(arr) > 0 {
		refs, _ := m["input_reference"].([]any)
		have := make(map[string]bool, len(refs))
		for _, r := range refs {
			if obj, ok := r.(map[string]any); ok {
				if u, ok := obj["image_url"].(string); ok {
					have[u] = true
				}
			}
		}
		added := make([]any, 0, len(arr))
		for _, it := range arr {
			u, ok := it.(string)
			if !ok || strings.TrimSpace(u) == "" || have[strings.TrimSpace(u)] {
				continue
			}
			have[strings.TrimSpace(u)] = true
			added = append(added, map[string]any{"type": "image", "image_url": strings.TrimSpace(u)})
		}
		if len(added) > 0 {
			m["input_reference"] = append(added, refs...)
		}
	}
	// image_urls 是 nano 系的**官方字段名**，平台规范写法是 image（一张写字符串、多张写数组）。
	//
	// 必须在这里折成 image 并删键，理由是三条链路读的是同一份归一结果：
	//   - 不折，同步网关只认 `image`（relayHead.Image），用户按官方文档传 image_urls
	//     会被静默当成"没有参考图" → 图生图退化成文生图，账单一切正常；
	//   - 不折，异步链路的计费侧数不到参考图（RefImages=0），与同步口径不一致；
	//   - gpt-image 的出站白名单还把 image_urls 列为"平台内部键"直接丢掉。
	// 两套写法同时出现是**冲突**而不是取其一：静默择一会让用户以为另一份也生效了。
	// image_urls 是 nano 系的**官方字段名**，平台规范写法是 image（一张写字符串、多张写数组）。
	//
	// 折叠实现只此一处（relay.FoldImageURLsMap）：同步网关对同一份报文也要做同样的折叠，
	// 写在两个地方就会分叉 —— 异步认、同步不认，用户按文档传参考图会拿到纯文生图。
	if err := relay.FoldImageURLsMap(m); err != nil {
		return nil, fmt.Errorf("%w：%s", ErrInvalidParam, err)
	}
	// first_frame / last_frame 是首尾帧的便捷写法，等价于 input_reference 里同 type 的元素。
	//
	// 必须折叠进 input_reference **并删掉顶层键**：
	//   - 只有走 input_reference 才做 SSRF 校验、才计入素材费；留在顶层等于开了一条绕过素材管控的通道；
	//   - 不删原键则上游会同时收到两套写法，各适配器行为不可控。
	for _, kv := range [][2]string{{"first_frame", "first_frame"}, {"last_frame", "last_frame"}} {
		v, ok := m[kv[0]].(string)
		if !ok || strings.TrimSpace(v) == "" {
			continue
		}
		refs, _ := m["input_reference"].([]any)
		if !hasInputRefType(refs, kv[1]) {
			refs = append(refs, map[string]any{"type": kv[1], "image_url": strings.TrimSpace(v)})
			m["input_reference"] = refs
		}
		delete(m, kv[0])
	}
	return json.Marshal(m)
}

// hasInputRefType 判断 input_reference 里是否已有指定 type 的元素（避免折叠出重复首帧/尾帧）。
func hasInputRefType(refs []any, typ string) bool {
	for _, r := range refs {
		obj, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if t, ok := obj["type"].(string); ok && strings.EqualFold(strings.TrimSpace(t), typ) {
			return true
		}
	}
	return false
}

// endpointLabelOf 生成「跳过渠道」文案里的端点名。
//
// 端点为空（重试路径）时退回模态：只写 "不承接端点 ，跳过" 会让人以为少打了一个字。
func endpointLabelOf(endpointKey, modality string) string {
	if k := strings.TrimSpace(endpointKey); k != "" {
		return k
	}
	return modality + "/*"
}

// Submit 执行异步任务提交（§4.7.2）。
func Submit(ctx context.Context, in SubmitInput) (*model.Task, error) {
	if in.Key == nil || in.Req == nil {
		return nil, ErrInvalidParam
	}
	// 幂等键归一（B4-2）：非法值直接 400，不做「截断后使用」——截断会把两个不同的键
	// 变成同一个键，把两次**不同**的请求折叠成一次，用户会凭空丢失一个任务。
	idem, err := normalizeIdempotencyKey(in.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	// 幂等作用域必须带端点：同一个键打到不同端点不是「重试」。
	// 用入站端点（models.endpoints 写法）而不是 URL：别名（/v1/videos 与
	// /v1/video/generations）共享端点身份，否则同一端点换个别名就能再开一单。
	idemScope := strings.TrimSpace(in.EndpointKey)
	if idemScope == "" {
		idemScope = strings.TrimSpace(in.ExpectModality)
	}
	// 命中已有任务必须在**任何出站与冻结之前**返回：重复提交若先冻结再回滚，
	// 用户会看到余额先减后加（并发重试时甚至直接报「余额不足」），
	// 而这次请求本来只需要原样把上次的结果还给他。
	if idem != "" {
		if existing, ok := findByIdempotencyKey(ctx, in.Key.ID, idemScope, idem); ok {
			logger.L().Info("重复提交命中幂等键，返回首次创建的任务",
				zap.Uint64("task_id", existing.ID), zap.Uint64("key_id", in.Key.ID),
				zap.String("idempotency_key", idem), zap.String("status", existing.Status))
			return existing, nil
		}
	}
	// 再生成：把平台任务号换成上游任务号、并按源视频时长确定计费秒数。
	// 必须在 SSRF 校验与计费之前：缺 min 秒数会让 Estimate 按 0 秒冻结（等于白送）。
	if in.Req.Params.Kind == KindRegeneration {
		if err := applyRegeneration(ctx, in); err != nil {
			return nil, err
		}
	}
	// 素材链接做 DNS 解析后的公网校验（防 DNS rebinding；S8 补全 §⑨ R4）。
	//
	// 只校验 remote 档：data: 是内联体（不出网），asset:// 是上游素材库 ID（不是 URL，
	// 由上游自己解析）—— 把这两档一起送进 URL 校验，asset:// 素材会被判成"非法链接"。
	for _, link := range in.Req.links {
		if refLinkScheme(link) != "remote" {
			continue
		}
		if err := ssrf.ValidatePublicURLResolved(ctx, link); err != nil {
			return nil, ErrReferenceInvalid
		}
	}
	cat, err := pricing.Get()
	if err != nil {
		return nil, err
	}
	item := relay.FindModel(cat, in.Req.Model)
	if item == nil || !item.Enabled {
		return nil, ErrModelNotFound
	}
	modality := item.Modality
	if modality == "" {
		modality = "video"
	}
	// 模态闸门（P0-02）：入口声明的模态必须与模型一致。
	// 兜底默认值是给「存量 modality 为空的视频模型」用的，不能拿它替代校验。
	if want := strings.TrimSpace(in.ExpectModality); want != "" && modality != want {
		return nil, fmt.Errorf("%w：模型 %s 的模态是 %s，不能用于 %s 任务", ErrInvalidParam, in.Req.Model, modality, want)
	}
	if !apikey.AllowsModel(in.Key, in.Req.Model, modality) {
		if models := apikey.DecodeStrings(in.Key.ModelsAllowed); len(models) > 0 && !contains(models, in.Req.Model) {
			return nil, ErrModelNotAllowed
		}
		return nil, ErrModalityNotAllowed
	}

	group := strings.TrimSpace(in.Group)
	if group == "" {
		group = "default"
	}
	snap := relay.BuildSnapshot(item, relay.GroupDiscountOf(cat, group))
	// 模态校验必须紧跟 BuildSnapshot：resolution_fixed 只在 spec_prices 原文里，
	// 而它决定冻结档位。放在 Estimate 之前，任何"按错档冻结"都不可能发生。
	// 模型能力只影响**冻结张数**（组图/图层），不用于拒绝参数：
	// 上游认不认由上游回答，但"要不要按 15 张冻结"是平台自己的钱，必须按能力算。
	if err := validateModality(in.Req, snap, modality,
		apidoc.NormalizeCapabilities(modality, json.RawMessage(item.Capabilities))); err != nil {
		return nil, err
	}
	// 音乐按条计费：冻结期还不知道会交付几首，按上限占额度（2 首 / 歌词 1 篇）。
	// 不写这一条，usageOfRequest 的 ImageN 是 0 —— perClipCharge 会按 0 条算成免费，
	// 冻结额 0，余额闸门形同不存在。
	if modality == "music" && billing.IsPerClip(snap.SpecPrice) {
		in.Req.Params.MusicClips = billing.MusicClipFreezeCount(snap.SpecPrice)
	}
	// 参考视频按秒计费时，秒数必须在**冻结之前**拿到（否则冻结额与真实费用不符）。
	// 放在这里而不是 ParseSubmit：ParseSubmit 是纯函数、无 ctx、不出网。
	if err := probeRefVideos(ctx, in.Req, snap); err != nil {
		return nil, err
	}
	usage := usageOfRequest(in.Req)
	amount, perr := billing.Estimate(snap, 0, 0, usage)
	if perr != nil {
		return nil, perr
	}

	// 路由预检：候选为空必须在冻结前拦截（铁律：不建不可恢复的冻结）。
	candidates, err := relay.SelectCandidates(in.Req.Model, group)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, ErrNoChannel
	}
	// 异步能力预检：候选渠道里至少要有一个承接入站端点，否则这次 async 必然 502。
	// 放在冻结之前 —— 钱、任务行、出站尝试都不该为"注定无人承接"的请求发生。
	if key := strings.TrimSpace(in.EndpointKey); key != "" {
		supported := false
		for _, c := range candidates {
			if c.Channel != nil && relay.TaskEndpointSupported(c.Channel.AdapterID, key) {
				supported = true
				break
			}
		}
		if !supported {
			return nil, fmt.Errorf("%w：模型 %s 的渠道只支持同步调用，请去掉 async 按同步请求",
				ErrAsyncUnsupported, in.Req.Model)
		}
	}

	sess, err := billing.New(db.DB).Freeze(ctx, billing.FreezeInput{
		UserID: in.Key.UserID, KeyID: in.Key.ID, Model: in.Req.Model, Modality: modality, Group: group,
		IP: in.IP, RequestID: in.RequestID, Snapshot: snap, Amount: amount,
		// 计费维度随冻结一起落库：回收/补结只有这一行可读（见 FreezeInput.Estimate）。
		Estimate: usage,
		// 异步任务的耗时口径是「提交 → 终态」（含排队），起点只能是提交时刻。
		StartedAt: time.Now(), CallKind: billing.CallKindAsync,
	})
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(TaskTimeout)
	row := &model.Task{
		UserID: in.Key.UserID, KeyID: in.Key.ID, UsageID: sess.UsageID,
		Model: in.Req.Model, Modality: modality, Status: StatusQueued, Kind: modality,
		RequestParams: mustJSON(in.Req.Params), PricingSnapshot: mustJSON(snap),
		// 落库存的是**归一后、发上游前**的原文：重试时原样重发，
		// 未声明的字段（output_format / 厂商私有键 / music lyrics）不会再被重建吞掉。
		SubmitBody: model.JSON(in.Req.Body),
		Frozen:     amount, DeadlineAt: &deadline, IdempotencyKey: idem,
		IdempotencyEndpoint: idemScope,
	}
	if err := db.DB.WithContext(ctx).Create(row).Error; err != nil {
		// 并发重复提交：两个请求都通过了上面的「查不到」，唯一索引只会放行一个。
		// 输的那一方必须释放自己刚冻结的额度，并把赢家的任务行原样返回，
		// 否则客户端会同时收到「余额被扣」和「提交失败」两个结论。
		if idem != "" && pgerr.IsUnique(err) {
			releaseSession(ctx, sess, 409, "重复提交（幂等键冲突）")
			if existing, ok := findByIdempotencyKey(ctx, in.Key.ID, idemScope, idem); ok {
				logger.L().Info("并发重复提交，返回首次创建的任务",
					zap.Uint64("task_id", existing.ID), zap.String("idempotency_key", idem))
				return existing, nil
			}
		}
		releaseSession(ctx, sess, 500, "任务落库失败")
		return nil, err
	}
	// 反向锚点：usages 只有 task_id 才能在 tasks 被保留策略物理删除后仍说清「属于哪个任务」。
	// 绑定失败不影响业务（tasks.usage_id 仍是真相源），只 Warn。
	if err := billing.New(db.DB).BindTask(ctx, sess.UsageID, row.ID); err != nil {
		logger.L().Warn("任务与调用记录绑定失败", zap.Uint64("task_id", row.ID),
			zap.Uint64("usage_id", sess.UsageID), zap.Error(err))
	}

	// 调用记录：请求参数（C1-R3）。
	//
	// 同步写而不是丢给后台协程：用户提交完立刻打开「调用记录」是常态，
	// 异步补写会让那一瞬间看到"未采集"，而这恰恰是排障的第一现场。
	// WithoutCancel：客户端拿到 202 就断开时请求 ctx 已取消，直接用它写库会静默失败。
	if len(in.RawBody) > 0 {
		callio.Store(context.WithoutCancel(ctx), callio.Rec{
			UsageID: sess.UsageID, UserID: in.Key.UserID,
			ReqRaw: in.RawBody, ReqSource: in.ReqSource,
		})
	}

	// 上游提交：候选按优先级顺序 failover；成功后 CAS queued→running 并落 upstream_task_id。
	var lastErr string
	// limitedOnly 统计「一次上游都没打出去、纯粹被限流/全冷却拦下」的候选数（P1-5）。
	// 终局要用它区分 429 与 502：502 会让客户端以为平台崩了立刻重试，把限流打成雪崩。
	limitedOnly := 0
	reachedUpstream := 0
	// configOnly 统计「适配器在本地就判定配置错误、零出站」的候选数。
	//
	// 终局必须单独分档：这类失败是平台的配置问题（模型未登记、协议被开关关闭），
	// 不是上游故障。归到「上游提交失败」会让 SDK 按 5xx 语义重试，也会让运维
	// 去查上游状态页 —— 而真正要看的是渠道 adapter_options 与模型绑定。
	configOnly := 0
	// 出站报文采集器：一次任务可能 failover 多次，tap 记最后一次尝试并累加次数。
	tap := &submitBodyTap{}
	for _, cand := range candidates {
		// 能力按**端点**判定：只做视频的渠道不该接下音乐任务 —— 提示词会被打到视频端点，
		// 报错是 502 且与真实原因无关。EndpointKey 为空（重试路径）时只要求「能做异步」。
		if !relay.TaskEndpointSupported(cand.Channel.AdapterID, in.EndpointKey) {
			lastErr = fmt.Sprintf("渠道适配器不承接端点 %s，跳过：%s", endpointLabelOf(in.EndpointKey, modality), cand.Channel.AdapterID)
			continue
		}
		ta, ok := relay.GetTaskAdapter(cand.Channel.AdapterID)
		if !ok {
			lastErr = "适配器未实现异步任务能力：" + cand.Channel.AdapterID
			continue
		}
		ref, err := relay.PickKey(cand.Channel, nil)
		if err != nil {
			var cooling *relay.AllKeysCoolingError
			if errors.As(err, &cooling) && !cooling.Auth {
				limitedOnly++
			}
			lastErr = err.Error()
			continue
		}
		// 渠道限流（P1-5）：视频提交同样是真实出站，必须计入 rpm 与并发。
		// 提交是短请求，占并发槽是合理的；长时间占用的是轮询，它走 AcquireRPM。
		release, lerr := channellimit.Acquire(ctx, cand.Channel)
		if lerr != nil {
			limitedOnly++
			lastErr = lerr.Error()
			continue
		}
		allowsAuto := billing.AllowsAutoDuration(snap.SpecPrice)
		submitReq := &relay.TaskSubmitRequest{
			UpstreamModel: cand.UpstreamModel, ModelID: in.Req.Model,
			Body: in.Req.Body, Kind: in.Req.Params.Kind,
			// 时长由平台显式告知适配器（不让它从 body 猜：body 里可能同时有
			// seconds 与 duration，猜哪一个是不可判定的）。三种状态：
			//   - 用户写了秒数 → 下发该秒数；
			//   - 用户没写、模型声明支持智能时长 → 下发 -1（AutoDuration）；
			//   - 用户没写、模型不认 -1 → **不下发时长**，由上游用自己的默认值
			//     （OmitDuration）。绝不能补一个"猜的"秒数：那会让用户拿到他没要的时长。
			AutoDuration: in.Req.Params.SecondsAuto && allowsAuto,
			OmitDuration: in.Req.Params.SecondsAuto && !allowsAuto,
			// 归一后的秒数是计费基准的同一份数据：适配器直接用它，
			// 不再从 body 里猜（body 在重放时可能是 duration:-1）。
			Seconds: in.Req.Params.Seconds,
			// 入站端点由平台给：适配器据此判断"这次该打哪个上游端点"，
			// 不按模型模态猜（同一适配器可能同时承接视频/图像/音乐）。
			EndpointKey: in.EndpointKey,
		}
		reachedUpstream++
		upstreamID, err := func() (string, error) {
			defer release()
			// tap 挂在 ctx 上：适配器的 client 从 req.Context() 取它，
			// 不消费请求体（读 GetBody，不改 req.Body）。
			return relay.SubmitTaskOf(netguard.WithBodyTap(ctx, tap), cand.Channel.AdapterID, ta,
				relay.ChannelConfigFor(cand.Channel, ref.Key), submitReq)
		}()
		// 每次尝试后补写一次（同一条 usage 的 upsert，最后一次为准）。
		persistUpstreamIO(ctx, sess.UsageID, in.Key.UserID, tap)
		if err != nil {
			// 归属记录（B4-5）：提交阶段的失败此前完全不影响渠道健康 —— 一条一直在
			// 提交阶段超时/5xx 的渠道永远不会被熔断，每个任务都先撞它一次再换渠道。
			relay.RecordChannelAttempt(cand.Channel, ref, err)
			// 平台配置错误（适配器本地拒绝、零出站）：换渠道**可能**有用
			// （不同渠道的 model_overrides 不同），所以继续 failover，不能像
			// 调用方参数错那样直接终止；但要单独计数并在终局改口径。
			var cfgErr *relay.ConfigError
			if errors.As(err, &cfgErr) {
				configOnly++
				logger.L().Error("渠道配置错误，任务未出网（继续换下一渠道）",
					zap.Uint64("task_id", row.ID),
					zap.Uint64("channel_id", cand.Channel.ID),
					zap.String("adapter", cand.Channel.AdapterID),
					zap.String("model", in.Req.Model),
					zap.Error(cfgErr))
			}
			// 调用方参数错：换候选没有意义（每个渠道都会拒绝同一份参数），
			// 而且会把「用户的锅」变成 502「上游提交失败」——直接 400 并带回原因。
			if errors.Is(err, relay.ErrCallerParam) {
				// 与同步链路走同一个出口：取 Msg（剥掉包装前缀）+ 脱敏 + 按字符截断。
				msg := relay.CallerMessageOf(err, cand.Channel, ref.Key)
				_ = failWith(ctx, row, 400, msg)
				row.Status = StatusFailed
				row.ErrorMsg = sanitizeReason(msg)
				return nil, fmt.Errorf("%w：%s", ErrInvalidParam, msg)
			}
			// 提交结果歧义（B4-2）：超时/连接重置/5xx 且响应体为空 —— 上游可能**已经建单**。
			//
			// 这时换下一个渠道重投是纯亏损：上游多跑一次真实生成，平台只收一次钱，
			// 而且多出来的那个任务再也无人查询（产物白扔、成本照付）。
			// 因此必须停止 failover：全额退款 + 打上 submit_ambiguous 标记供对账。
			// 客户端可以用同一个幂等键重试：真建成了就能拿回那单，没建成才会真正新建。
			if relay.IsAmbiguousSubmit(err) {
				logger.L().Error("上游提交结果不确定，已停止换渠道重投并全额退款",
					zap.Bool("submit_ambiguous", true),
					zap.Uint64("task_id", row.ID), zap.Uint64("channel_id", cand.Channel.ID),
					zap.String("adapter", cand.Channel.AdapterID), zap.String("model", in.Req.Model),
					zap.Error(err))
				msg := "上游提交结果不确定（可能已受理），为避免重复计费未自动重试；请用同一幂等键重试"
				_ = failWithAnomaly(ctx, row, 504, msg, "submit_ambiguous")
				row.Status = StatusFailed
				row.ErrorMsg = sanitizeReason(msg)
				return nil, fmt.Errorf("%w：%s", ErrUpstreamSubmit, msg)
			}
			// 换渠道前就脱敏：上游报错体常回显主机名与 key，一旦原样带进下一轮循环，
			// 最终的 lastErr 会落进 tasks.error_msg（门户可见）。
			lastErr = relay.RedactUpstream(err.Error(), cand.Channel, ref.Key)
			continue
		}
		// 契约：提交成功必须带回上游任务号（见 relay.TaskAdapter.SubmitTask）。
		//
		// 空串会让这条任务永远不被轮询选中（查询条件要求 upstream_task_id 非空），
		// 15 分钟后被 orphan 回收判失败退款 —— 而上游可能已经建单：纯平台损失。
		// 按「渠道配置错误」分档（继续换渠道，终局文案点名适配器），与适配器
		// 本地拒绝走同一条终局，运维一眼能看出该查适配器而不是上游。
		if strings.TrimSpace(upstreamID) == "" {
			configOnly++
			lastErr = "适配器未返回上游任务号：" + cand.Channel.AdapterID
			logger.L().Error("适配器提交成功但未返回上游任务号，视为配置错误",
				zap.Bool("empty_upstream_id", true),
				zap.Uint64("task_id", row.ID), zap.Uint64("channel_id", cand.Channel.ID),
				zap.String("adapter", cand.Channel.AdapterID), zap.String("model", in.Req.Model))
			continue
		}
		extra := map[string]any{
			"channel_id":       cand.Channel.ID,
			"platform":         cand.Channel.AdapterID,
			"upstream_task_id": upstreamID,
		} // 适配器回写的「非致命事实」：只留痕，不改变任何行为。
		//
		// 必须在 CAS **之前**：取消/竞态路径同样需要这条证据 ——
		// 它记录的是一次「按 body 计费、可能按提示词交付」的调用，
		// 且 usages 行此时已经存在（appendUsageAnomaly 只加列，不改状态）。
		if len(submitReq.Notes) > 0 {
			for _, note := range submitReq.Notes {
				logger.L().Warn("视频请求的提示词里带了计价参数，平台只按 body 计费",
					zap.Bool("weak_param_pricing_conflict", true),
					zap.String("note", note),
					zap.Uint64("task_id", row.ID), zap.Uint64("channel_id", cand.Channel.ID),
					zap.String("model", in.Req.Model))
			}
			appendUsageAnomaly(ctx, row.UsageID, model.AnomalyWeakParamPricingConflict)
		}
		// 子协议标记必须落库：轮询是另一次调用，没有它多路径协议必然查错端点。
		// 只在非空时写，保证单路径适配器的 tasks 行与改造前逐字节一致。
		//
		// 只写**列**（tasks.subtask_kind），不再回写 request_params 里的副本：
		// request_params 在任务行插入时就已序列化，此时改内存对象既落不了库，
		// 又会让人误以为「params 里有子协议」（重试曾因此恒判可重放）。
		if sk := strings.TrimSpace(submitReq.SubKind); sk != "" {
			extra["subtask_kind"] = sk
			row.SubTaskKind = sk
		}
		ok, casErr := cas(ctx, row.ID, []string{StatusQueued}, StatusRunning, extra)
		if casErr == nil && ok {
			row.ChannelID = cand.Channel.ID
			row.Platform = cand.Channel.AdapterID
			row.UpstreamTaskID = upstreamID
			row.Status = StatusRunning
			return row, nil
		}
		// CAS 失败 = 状态在提交期间被别人推进了，最典型的是用户点了取消
		// （Cancel 只认 queued，而提交期间任务恰好还是 queued）。
		//
		// 此时上游**已经建单**，但平台已经退款：这是一笔纯粹的平台损失，
		// 必须留痕（cancel_race），否则只能在月底对账时发现「上游成本 > 平台收入」而查不出原因。
		// 返回值也要如实反映终态：否则客户端拿到 202 running，之后永远查不到这个任务。
		if st, serr := currentStatus(ctx, row.ID); serr == nil && st == StatusCanceled {
			logger.L().Error("任务在提交期间被取消，上游已建单但平台已退款",
				zap.Bool("cancel_race", true),
				zap.Uint64("task_id", row.ID), zap.Uint64("usage_id", row.UsageID),
				zap.Uint64("channel_id", cand.Channel.ID), zap.String("adapter", cand.Channel.AdapterID),
				zap.String("upstream_task_id", upstreamID))
			appendUsageAnomaly(ctx, row.UsageID, "cancel_race")
			row.Status = StatusCanceled
			row.ErrorMsg = "任务已取消"
			return row, nil
		}
		if casErr != nil {
			logger.L().Warn("任务提交完成但状态推进失败", zap.Uint64("task_id", row.ID), zap.Error(casErr))
		}
		// 上游已经建单，但 CAS 没成功（状态被推进了，或 DB 抖动）。
		//
		// 单号必须**单独落库、且不依赖状态**：否则这条任务永远没有 upstream_task_id，
		// 轮询的 where 要求非空 → 永远不会被选中；15 分钟后 orphan 扫描（同样要求单号为空）
		// 把它判 failed 并全额退款 —— 而上游真的在生成并计费。这是纯平台损失，
		// 且能在月底「上游成本 > 平台收入」里体现却定位不到单。
		//
		// 条件带 COALESCE(...)='' ：不覆盖已经写进去的单号（并发路径只有一次真正建单）。
		patch := map[string]any{
			"upstream_task_id": upstreamID,
			"channel_id":       cand.Channel.ID,
			"platform":         cand.Channel.AdapterID,
		}
		if perr := db.DB.WithContext(ctx).Model(&model.Task{}).
			Where("id = ? AND COALESCE(upstream_task_id, '') = ''", row.ID).
			Updates(patch).Error; perr != nil {
			logger.L().Error("任务提交成功但单号落库失败（上游成本无法追回）",
				zap.Bool("submit_orphaned", true),
				zap.Uint64("task_id", row.ID), zap.Uint64("usage_id", row.UsageID),
				zap.String("upstream_task_id", upstreamID), zap.Error(perr))
		} else {
			logger.L().Error("任务提交成功但状态推进失败：已补记上游单号",
				zap.Bool("submit_orphaned", true),
				zap.Uint64("task_id", row.ID), zap.Uint64("usage_id", row.UsageID),
				zap.Uint64("channel_id", cand.Channel.ID), zap.String("adapter", cand.Channel.AdapterID),
				zap.String("upstream_task_id", upstreamID), zap.Error(casErr))
			// 只有任务已被推进到终态（usage 随之释放）时才追加标记：
			// 仍是 queued 的话，后面的 Release/Settle 会用本次结算的 anomaly 整列覆盖，
			// 写了也留不住，反而让人以为「标记已落库」。
			if st, serr := currentStatus(ctx, row.ID); serr == nil && st != StatusQueued {
				appendUsageAnomaly(ctx, row.UsageID, model.AnomalySubmitOrphaned)
			}
		}
		row.UpstreamTaskID = upstreamID
		row.ChannelID = cand.Channel.ID
		row.Platform = cand.Channel.AdapterID
		row.Status = StatusQueued
		return row, nil
	}

	// 全部候选提交失败：CAS failed + 全额释放（任务行已在库，可追溯）。
	//
	// 分档（P1-5/P1-3）：一次上游都没打出去、纯粹被渠道限流拦下 → 429。
	// 这不是「上游提交失败」，用户等几秒重来就能成功；报 502 会让 SDK 立刻重试，
	// 把渠道限流打成雪崩。
	if reachedUpstream == 0 && limitedOnly > 0 {
		msg := "渠道限流：" + lastErr
		_ = failWith(ctx, row, 429, msg)
		row.Status = StatusFailed
		row.ErrorMsg = sanitizeReason(msg)
		return nil, fmt.Errorf("%w：%s", ErrRateLimited, lastErr)
	}
	// 全部候选都因平台配置错误失败：文案必须说清楚是配置问题。
	//
	// 状态码保持 502（不动 controller/video.go 的映射与前端分支），但 error_msg
	// 会写进 tasks 详情（门户可见），用户与运维都能看到「未知的多米模型 xxx」
	// 而不是一句无从下手的「上游提交失败」。
	if reachedUpstream == 0 && configOnly > 0 {
		msg := "渠道配置错误（请联系管理员）：" + lastErr
		_ = failWith(ctx, row, 502, msg)
		row.Status = StatusFailed
		row.ErrorMsg = sanitizeReason(msg)
		return nil, fmt.Errorf("%w：%s", ErrUpstreamSubmit, lastErr)
	}
	_ = failWith(ctx, row, 502, "上游提交失败："+lastErr)
	row.Status = StatusFailed
	row.ErrorMsg = sanitizeReason("上游提交失败：" + lastErr)
	return nil, fmt.Errorf("%w：%s", ErrUpstreamSubmit, lastErr)
}

// maxIdempotencyKeyLen 是幂等键长度上限（与 tasks.idempotency_key 列宽一致）。
const maxIdempotencyKeyLen = 64

// normalizeIdempotencyKey 归一客户端幂等键（去首尾空白 + 长度 + 字符校验）。
//
// 只接受可见 ASCII，三个原因：
//  1. 幂等键进唯一索引，同形异码（NFC/NFD、全角/半角）会生成「看起来一样但字节不同」的键，
//     幂等静默失效——而这正是它要防的事故；
//  2. 幂等键会写日志，控制字符能伪造日志行；
//  3. 超长直接拒绝而不是截断：截断会把两个不同的键折叠成同一个，凭空吞掉一次提交。
func normalizeIdempotencyKey(raw string) (string, error) {
	// 灰度开关（LD_SUBMIT_IDEMPOTENT，默认开）：置 0 时**整体忽略**客户端幂等键，
	// 退回改造前的行为（每次提交都建新任务）。保留这个开关是为了在出现
	// 「客户端复用同一个键做不同请求」这类误用时能立刻止血，而不必回滚整批代码。
	if !submitIdempotentEnabled() {
		return "", nil
	}
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	if len(v) > maxIdempotencyKeyLen {
		return "", fmt.Errorf("%w：Idempotency-Key 最长 %d 个字符（当前 %d）",
			ErrInvalidParam, maxIdempotencyKeyLen, len(v))
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; c < 0x20 || c > 0x7e {
			return "", fmt.Errorf("%w：Idempotency-Key 只能包含可见 ASCII 字符（字母/数字/连字符等）", ErrInvalidParam)
		}
	}
	return v, nil
}

// submitIdempotentEnabled 读取客户端幂等开关（LD_SUBMIT_IDEMPOTENT，默认开）。
//
// 每次调用都读 env（不缓存）：灰度/止血时改环境变量重启即可生效，
// 且单测里用 t.Setenv 就能精确控制这一条路径，不需要额外的注入点。
func submitIdempotentEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LD_SUBMIT_IDEMPOTENT"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// findByIdempotencyKey 按「KEY + 端点 + 幂等键」查已有任务（未命中返回 false，不返回错误）。
//
// 查不到与查询失败都按未命中处理：宁可多建一单（有唯一索引兜底），
// 也不要因为一次读抖动把正常提交挡成 500。
//
// 历史行（endpoint=”）仍能被查到：条件写成「端点相等 或 旧行为只按 KEY+键」，
// 否则上线后第一次重试会绕过旧行、再建一单（那正是幂等键要防的事）。
func findByIdempotencyKey(ctx context.Context, keyID uint64, endpoint, idem string) (*model.Task, bool) {
	if keyID == 0 || idem == "" {
		return nil, false
	}
	var t model.Task
	q := db.DB.WithContext(ctx).Where("key_id = ? AND idempotency_key = ?", keyID, idem)
	if endpoint != "" {
		q = q.Where("(idempotency_endpoint = ? OR idempotency_endpoint = '')", endpoint)
	}
	if err := q.First(&t).Error; err != nil {
		return nil, false
	}
	return &t, true
}

// currentStatus 读取任务当前状态（只查一列，用于 CAS 失败后的归因）。
func currentStatus(ctx context.Context, id uint64) (string, error) {
	var row struct{ Status string }
	err := db.DB.WithContext(ctx).Model(&model.Task{}).
		Select("status").Where("id = ?", id).Take(&row).Error
	return row.Status, err
}

// appendUsageAnomaly 给**已关闭会话**的 usage 追加一个对账标记（B4-3 cancel_race 用）。
//
// 为什么不复用 billing.Settle 的 Anomaly 入参：这条路径上会话已经关闭
// （Cancel 已经释放过额度），再走 Settle 会被 ErrSessionClosed 拒绝，标记永远写不进去。
//
// 标记**前置**而不是追加：列宽 255，追加在极限情况下会被截断吃掉关键标记
// （历史缺陷：charge_capped 就被截断吃掉了）。已存在同一标记时不重复写。
func appendUsageAnomaly(ctx context.Context, usageID uint64, flag string) {
	flag = strings.TrimSpace(flag)
	if usageID == 0 || flag == "" {
		return
	}
	// 指标先于落库自增：落库失败也必须留下「这件事发生过」的证据（否则对账时只剩日志）。
	metrics.Inc(metrics.NameUsageAnomaly, "kind", flag)
	var us model.Usage
	if err := db.DB.WithContext(ctx).Select("id", "anomaly").First(&us, usageID).Error; err != nil {
		logger.L().Warn("用量标记写入失败：读取用量行异常",
			zap.Uint64("usage_id", usageID), zap.String("flag", flag), zap.Error(err))
		return
	}
	v := flag
	if prev := strings.TrimSpace(us.Anomaly); prev != "" {
		if strings.Contains(","+prev+",", ","+flag+",") {
			return
		}
		v = flag + "," + prev
	}
	v = textx.TruncateRunes(v, 255)
	if err := db.DB.WithContext(ctx).Model(&model.Usage{}).Where("id = ?", usageID).
		Update("anomaly", v).Error; err != nil {
		logger.L().Warn("用量标记写入失败", zap.Uint64("usage_id", usageID), zap.String("flag", flag), zap.Error(err))
	}
}

// validateModality 按模态做提交前的最终校验，并把归一后的计费参数**回写**到请求与落库快照。
//
// 位置固定：必须在 BuildSnapshot 之后（要读 spec_prices 的 resolution_fixed），
// 且在 probeRefVideos / usageOfRequest / Estimate 之前（冻结金额必须基于最终档位）。
//
// 为什么必须"回写"而不是只校验：结算 usageOf 只读落库的 request_params。
// 只在内存里改 req.Resolution 会让任务"提交成功 → 跑完 → 每轮结算 ErrInvalidTier"
// （worker 只 warn 不失败）→ 一路卡到 deadline 全额退款、产物白送。
func validateModality(req *SubmitRequest, snap billing.Snapshot, modality string, caps []apidoc.Cap) error {
	switch modality {
	case "image":
		return validateImageModality(req, snap, caps)
	case "audio", "music":
		// music 是 B6-1 新增的第 8 类模态（SUNO 生成/歌词）。
		// 必须显式列出来：default 分支是**视频**校验（要求 seconds>0 + 命中秒价档），
		// 音乐任务没有秒数概念，落进 default 会被判「必须指定 seconds」而永远提交不了。
		//
		// 音乐按次计费、不看档位也不看张数，因此**不做任何参数拒绝**：
		// 一次出几个结果由上游决定（SUNO 历来就是一次 2 首）。
		return nil
	default:
		return validateVideoModality(req, snap)
	}
}

// validateVideoModality 视频：确定**计费档位**与**时长基准** —— 一律不拒绝参数。
//
// 两条口径分开存，混起来必有一边是错的：
//   - `req.Resolution` 是**用户原话**（出站照发），`params.Resolution` 是计费档位；
//   - `req.Seconds` 是计费基准（正数，冻结按它），`params.SecondsAuto` 决定结算是否以上游为准。
//
// 用户写了一个价目里没有的档位、或干脆没写时长，都不是错误：上游会用自己的默认值，
// 平台按「固定档 → 最高价档 → 预算」兜底计费并留标记。以前这里 400，
// 结果是「用户照官方文档传 1440p，平台说本模型不支持」—— 而支持与否只有上游说了算。
func validateVideoModality(req *SubmitRequest, snap billing.Snapshot) error {
	raw := strings.TrimSpace(req.Resolution)
	tier, flag := resolveVideoTier(raw, snap.SpecPrice)

	// 出站只发用户原话：没写就不发（由模型用自己的默认），绝不替用户填一个档位。
	req.Resolution = raw
	req.Params.Resolution = tier
	req.Params.ResolutionRaw = raw
	req.Params.Anomaly = joinTaskFlags(req.Params.Anomaly, flag)

	if req.Seconds > 0 {
		return nil
	}
	// 没写秒数 / 写了 -1：按模型上限冻结，结算以上游实际时长为准。
	//
	// `SecondsAuto=true` 只表达「结算以上游为准」；**是否下发 -1** 由适配器按
	// `spec_prices.allow_auto_duration` 决定（见 Submit 的 AutoDuration/OmitDuration），
	// 否则会给不认 -1 的上游发一个必然 400 的参数。
	budget := billing.AutoDurationBudget(snap.SpecPrice)
	req.Seconds, req.Params.Seconds = budget, budget
	req.Params.SecondsAuto = true
	req.Params.Anomaly = joinTaskFlags(req.Params.Anomaly, model.AnomalyVideoSecondsMissing)
	return nil
}

// resolveVideoTier 决定**计费档位**（有序，第一个命中即返回），并给出对账标记。
//
//  1. 请求给了档位且价目里有 → 用它；
//  2. 请求给了但价目里没有 → 固定档优先，其次单价最高档，标记 video_tier_unpriced；
//  3. 未给 → 固定档，其次最高价档，标记 video_tier_missing。
//
// 绝不返回错误：档位是**计价维度**，不是参数合法性判据。
func resolveVideoTier(requested string, spec model.SpecPrices) (string, string) {
	if v := strings.TrimSpace(requested); v != "" {
		tier := normalizeVideoResolution(v)
		if billing.SupportsVideoTier(spec, tier) {
			return tier, ""
		}
		return videoTierFallback(spec), model.AnomalyVideoTierUnpriced
	}
	if t := billing.FixedTier(spec); t != "" {
		return normalizeVideoResolution(t), ""
	}
	return videoTierFallback(spec), model.AnomalyVideoTierMissing
}

// videoTierFallback 在有价目里挑一个能计价的档位（固定档优先，其次单价最高）。
func videoTierFallback(spec model.SpecPrices) string {
	if t := billing.FixedTier(spec); t != "" {
		return normalizeVideoResolution(t)
	}
	return billing.HighestVideoTier(spec)
}

// joinTaskFlags 追加对账标记（去重、保序、逗号分隔）。
func joinTaskFlags(prev, add string) string {
	add = strings.TrimSpace(add)
	if add == "" {
		return prev
	}
	if prev == "" {
		return add
	}
	for _, f := range strings.Split(prev, ",") {
		if f == add {
			return prev
		}
	}
	return prev + "," + add
}

// validateImageModality 图像：档位归一 + 冻结张数 —— 一律不拒绝参数。
//
// 两条曾经在这里 400 的规则已删除：
//  1. 组图 / 图层参数（"产出张数无法在提交期定价"）—— 现在张数规则下沉到
//     billing.ImageCountHint，提交期就能算出冻结张数；结算按实际产物数收敛
//     （applyArtifactActuals），所以允许它们。
//  2. n 超 max_n —— 改成**静默夹紧**并打 image_n_capped：用户拿到的图就是夹紧后的张数，
//     而拒绝会让一次本该成功的生成彻底失败。
func validateImageModality(req *SubmitRequest, snap billing.Snapshot, caps []apidoc.Cap) error {
	// 冻结张数：与同步链路**同一个函数**（billing.ImageCountHint）。
	//
	// limit 是模型的单张上限（0 = 未配置 → 按引擎上限夹紧，默认仍只冻 1 张）；
	// 超过上限时夹紧而不是拒绝 —— 拒绝会让一次本该成功的生成彻底失败。
	limit := imageMaxNOf(snap.SpecPrice)
	if limit > 0 && req.Params.ImageN > limit {
		req.Params.Anomaly = joinTaskFlags(req.Params.Anomaly, model.AnomalyImageNCapped)
	}
	if limit == 0 {
		// 漏配 max_n 能跑（上限按引擎兜），但必须可被筛出来。
		req.Params.Anomaly = joinTaskFlags(req.Params.Anomaly, model.AnomalyImageMaxNUnset)
	}
	// 组图 / 图层只在模型具备该能力时参与冻结（与同步链路同一口径）：
	// 这两个参数的缺省语义来自方舟（组图 15 张、图层 17 张），nano / gpt-image
	// 收到它们只会 400，按 15 张冻结等于用一次必然失败的请求占住用户额度。
	seqAuto := strings.EqualFold(req.Params.SeqImageGen, "auto") && apidoc.HasCap(caps, apidoc.CapImageGroup)
	layer := req.Params.LayerDecomp && apidoc.HasCap(caps, apidoc.CapImageLayer)
	req.Params.ImageN = billing.ImageCountHint(billing.ImageCountInput{
		N:           req.Params.ImageN,
		SeqAuto:     seqAuto,
		MaxImages:   req.Params.MaxImages,
		LayerDecomp: layer,
		RefImages:   countImageRefs(req.Refs),
		MaxN:        limit,
	})

	// 尺寸解析 + 档位归一：与同步链路**同一个函数**，且同样不拒绝任何写法。
	size := billing.ParseImageSize(billing.ImageDecl{
		Size:        req.Params.SizeRaw,
		ImageSize:   req.Params.ImageSizeRaw,
		Resolution:  req.Params.ResolutionRaw,
		AspectRatio: req.Params.AspectRatio,
	})
	// 画幅写错在提交期就拒绝（与同步链路同一判据、同一时点）：留到上游只会
	// 得到一句"上游返回业务错误"，而真正的原因在请求体里。
	if size.InvalidRatio {
		return fmt.Errorf("%w：画幅比例写法不合法，请写成 16:9 这样的比例；留空或 auto 表示由模型决定", ErrInvalidParam)
	}
	// 「档位 + 画幅」换算不出像素时，**建单前**就拒绝（与同步链路同一判据）。
	// 留到出站构造才拒的话，客户端拿到 400 的同时任务列表里会多一条 failed 记录 ——
	// 钱退了，但"我被拒了为什么还有单"这个问题没有答案。
	if msg := apidoc.GPTImageSizeProblem(req.Model, size.DeclaredTier, size.Ratio); msg != "" {
		return fmt.Errorf("%w：%s", ErrInvalidParam, msg)
	}
	// 解释不出的档位只留标记（价目全空时 Tier 为空 → 结算报未配价）。
	req.Params.Anomaly = joinTaskFlags(req.Params.Anomaly, billing.ResolveImageTier(snap.SpecPrice, &size))
	req.Params.ImageSpec = size.Tier
	req.Params.ImageRatio = size.Ratio
	req.Params.ImageExplicit = size.Explicit
	req.Params.ImageSource = size.Source
	return nil
}

// looseTrue 宽松判真（`true` / `"true"` / `1` / `"1"` 都算真）。
//
// 与 relay.jsonTrue 同一口径：同一个开关在不同客户端有三种写法，
// 只认 bool 会让 `"true"` 被当成 false —— 组图请求按 1 张冻结、上游出 15 张。
func looseTrue(raw json.RawMessage) bool {
	t := strings.ToLower(strings.TrimSpace(strings.Trim(string(bytes.TrimSpace(raw)), `"`)))
	return t == "true" || t == "1"
}

// imageMaxNOf 取图片模型的单张上限（**0 = 未配置**）。
//
// 0 与"1"必须区分：上限只用于夹紧用户显式要的张数，默认冻结张数由
// billing.ImageCountHint 单独决定（1 张）。混成一个数会让「漏配 max_n」
// 要么把普通请求冻成 128 张（402），要么把用户要的 3 张削成 1 张。
func imageMaxNOf(sp model.SpecPrices) int64 {
	if sp.MaxN != nil && *sp.MaxN > 0 {
		return *sp.MaxN
	}
	return 0
}

// probeRefVideos 在冻结之前探明参考视频的累计秒数，并写回请求与落库快照。
//
// 三条硬约束（§2.4.2）：
//  1. 按价目短路：模型没配任何参考视频价 → 秒数不影响金额 → 一次也不出站
//     （否则给所有视频模型平白加 8 秒延迟）；
//  2. 失败即 400（ErrInvalidParam），**不降级按 0**——降级等于开放免费额度；
//  3. 秒数累计后饱和到 MaxVideoSeconds，与计费侧 clamp 口径一致。
func probeRefVideos(ctx context.Context, req *SubmitRequest, snap billing.Snapshot) error {
	if !billing.RefVideoBillable(snap.SpecPrice) {
		return nil
	}
	// 内联视频读不到时长 → 无法按秒计费。必须在冻结前拒，不能"按 0 秒"放行（等于白送）。
	if req.hasInlineVideo {
		return fmt.Errorf("%w：参考视频不支持内联 data: 素材，请先用 /v1/files 上传后改用直链", ErrInvalidParam)
	}
	// 素材库 ID（asset://）同理：它是上游的标识，平台读不到时长。
	// 与内联的差别只在"拒绝理由"：这里要让用户知道改成直链才行，而不是怀疑链接写错了。
	for _, r := range req.Params.InputRef {
		if r.Type == "video" && refLinkScheme(r.URL) == "asset" {
			return fmt.Errorf("%w：参考视频不支持素材库 ID（asset://），请改用可访问的直链", ErrInvalidParam)
		}
	}
	urls := videoRefURLs(req.Params)
	if len(urls) == 0 {
		return nil
	}
	secs, err := mediaprobe.Batch(ctx, urls)
	if err != nil {
		// 细节只进日志：错误文案会区分"连不上 / 403 / 404 / 支持 206"，
		// 原样回给调用方等于把内网可达性变成一个可读的探测结果（盲 SSRF oracle）。
		logger.L().Warn("参考视频时长探测失败",
			zap.Bool("media_probe_failed", true), zap.Error(err))
		return fmt.Errorf("%w：参考视频时长无法读取，请确认素材地址可公开访问且为 MP4/MOV", ErrInvalidParam)
	}
	var total int64
	for _, u := range urls {
		total += secs[u]
	}
	if total > billing.MaxVideoSeconds {
		total = billing.MaxVideoSeconds
	}
	req.RefVideoSeconds = total
	req.Params.RefVideoSeconds = total
	return nil
}

// videoRefURLs 返回**可探测**的参考视频直链（顺序与 InputRef 一致）。
//
// 只收 remote 档：mediaprobe 只会发 HTTP，把 asset:// 或 data: 传进去必然报错，
// 报错又被包成"参考视频时长无法读取"，用户会去找视频文件的问题。
func videoRefURLs(p params) []string {
	out := make([]string, 0, len(p.InputRef))
	for _, r := range p.InputRef {
		if r.Type == "video" && refLinkScheme(r.URL) == "remote" {
			out = append(out, r.URL)
		}
	}
	return out
}

// applyRegeneration 把再生成请求补全成可提交的形态（就地改写 in.Req）。
//
// 三件事，顺序不能换：
//  1. 解析源任务（归属 + 状态 + 平台），拿到**上游**任务号与时长；
//     `WHERE id=? AND user_id=?` 是权限边界，同时也是「不区分不存在与非本人」的隐私边界；
//  2. 把上游任务号写进请求体（适配器只认上游号，平台号发出去必然查不到）；
//  3. 用源视频时长（或 base_video 探测时长）回填计费秒数——再生成的收费口径就是
//     「输出秒数 × 档位单价」，没有秒数就等于免费。
func applyRegeneration(ctx context.Context, in SubmitInput) error {
	req := in.Req
	var (
		upstreamID string
		secs       int64
	)
	if req.SourceTaskRef != "" {
		var src model.Task
		// 归属边界必须同时包含 user_id 与 **应用**：只按 user_id 查会让
		// 应用 A 的托管密钥把同一用户在应用 B 下完成的任务当成源任务，
		// 用 B 的上游任务号重建，再凭自己的 key 取回派生产物 —— 跨应用内容越权。
		//
		// 用 JOIN 而不是先查后用 KeyID 比对：多一次往返就多一个"忘了比"的位置。
		// app_id 取当前 KEY 上的值（平台直连为 0），与计费归因同一口径。
		err := db.DB.WithContext(ctx).
			Joins("JOIN api_keys AS k ON k.id = tasks.key_id AND k.app_id = ?", in.Key.AppID).
			Where("tasks.public_id = ? AND tasks.user_id = ?", req.SourceTaskRef, in.Key.UserID).
			First(&src).Error
		if err != nil {
			// 「不存在」与「不属于本应用」同码同文案：区分二者会变成
			// 跨应用存在性探针（试出 B 应用有哪些任务号）。
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("%w：源任务不存在", ErrNotFound)
			}
			return err
		}
		if src.Status != StatusSucceeded {
			return fmt.Errorf("%w：只有已完成的任务可以再生成（当前状态 %s）", ErrInvalidParam, src.Status)
		}
		if strings.TrimSpace(src.UpstreamTaskID) == "" || strings.TrimSpace(src.Platform) == "" {
			return fmt.Errorf("%w：源任务缺少上游任务号，无法再生成", ErrInvalidParam)
		}
		// 判据是**端点能力**而不是平台名：写死 "metaso" 会让「以后哪家上游支持了再生成」
		// 必须改服务层，且这里漏改的表现是「静默当普通生成重投」（用户付了升档的钱）。
		if !relay.TaskEndpointSupported(src.Platform, "video/regenerations") {
			return fmt.Errorf("%w：源任务来自 %s，该适配器不支持再生成", ErrInvalidParam, src.Platform)
		}
		upstreamID = src.UpstreamTaskID
		secs = parseParams(src.RequestParams).Seconds
		if secs <= 0 {
			return fmt.Errorf("%w：源任务没有记录时长，无法再生成", ErrInvalidParam)
		}
	} else {
		// base_video 形态：时长只能靠探测（客户端只给了链接）。
		urls := baseVideoURLs(req.Params)
		if len(urls) == 0 {
			return fmt.Errorf("%w：再生成需要 source_task_id 或 base_video 之一", ErrInvalidParam)
		}
		durations, perr := mediaprobe.Batch(ctx, urls)
		if perr != nil {
			logger.L().Warn("base_video 时长探测失败",
				zap.Bool("media_probe_failed", true), zap.Error(perr))
			return fmt.Errorf("%w：无法确定 base_video 的时长，请确认素材地址可公开访问且为 MP4/MOV", ErrInvalidParam)
		}
		secs = durations[urls[0]]
		if secs <= 0 {
			return fmt.Errorf("%w：无法确定 base_video 的时长", ErrInvalidParam)
		}
	}
	if secs > billing.MaxVideoSeconds {
		secs = billing.MaxVideoSeconds
	}
	// 再生成的目标档恒为 2K（上游只提供 2K 升档），而 2K 的上限是 15 秒（见
	// adapters.metasoMaxSeconds2K）。源任务若是 1080p/30s，冻结会按 30 秒算钱、
	// 上游却只可能产出 15 秒 —— 收了下发项的钱却给不出对应时长。
	// 必须在**冻结之前**拒单，否则要先占额度再退，凭空制造一次资金波动。
	if secs > regenerationMaxSeconds2K {
		return fmt.Errorf("%w：再生成的目标档为 2K，时长上限 %d 秒（源视频约 %d 秒）；请改用不超过 %d 秒的素材",
			ErrInvalidParam, regenerationMaxSeconds2K, secs, regenerationMaxSeconds2K)
	}

	req.Seconds = secs
	req.Params.Seconds = secs
	req.Params.Kind = KindRegeneration
	req.Params.SourceUpstream = upstreamID

	// 计费档位**必须**钉成 2K，不能沿用请求里的 resolution。
	//
	// 再生成的产物档位是上游决定的（metaso 只提供 2K 升档），但取价读的是
	// `req.Resolution`：客户端传 "480p" 就能按 480p 秒价买走 2K 产物，
	// 而上一段刚用「2K 的 15 秒上限」证明了产物是 2K —— 自相矛盾。
	// 两处都写：req.Resolution 供计费取档，req.Params.Resolution 供结算复原
	// （结算读 request_params，只改内存会让「按 2K 冻结、按 480p 结算」）。
	// 若该模型价目里没有 2K 档，validateModality 会明确 400 —— fail-closed 好过按错档收钱。
	req.Resolution = regenerationResolution
	req.Params.Resolution = regenerationResolution

	// 上游只认上游任务号：把请求体里的 source_task_id 换掉，并去掉与再生成无关的字段。
	var doc map[string]any
	if err := json.Unmarshal(req.Body, &doc); err != nil {
		return ErrInvalidParam
	}
	if upstreamID != "" {
		doc["source_task_id"] = upstreamID
	}
	// 出站体也必须写同一个档位：适配器（metaso.go）自己从 body.Resolution 取档，
	// 只改 req.Resolution 会让「发给上游 480p、按 2K 收费」再次分叉。
	doc["resolution"] = regenerationResolution
	delete(doc, "size") // size 与 resolution 等价且优先生效，留着会盖掉上面的值
	delete(doc, "seconds")
	delete(doc, "duration")
	body, err := json.Marshal(doc)
	if err != nil {
		return ErrInvalidParam
	}
	req.Body = body
	return nil
}

// regenerationMaxSeconds2K 是「再生成」的时长上限（秒），**平台侧**的冻结前守卫。
//
// 它不是 metaso 的 2K 档上限（那个在 adapters 里，管的是任意 2K 生成），而是
// 「当前所有支持 video/regenerations 的上游里，最严的那个 2K 时长上限」。
// 今天只有 metaso，两者恰好都是 15；新增支持再生成的上游时，这里取各家最小公倍，
// 绝不能跟着某一家放大 —— 放大会让冻结额按更长的时长算，随后被渠道侧拒单，凭空多一次退款。
//
// 放在这里是因为它必须发生在**冻结之前**（此刻还没选渠道，拿不到渠道侧校验）；
// 一旦渠道侧才知道就晚了：冻结已经按 30 秒算钱，拒单只能走退款。
const regenerationMaxSeconds2K = 15

// regenerationResolution 是「再生成」产物的档位，也是**计费取价的唯一档位**。
//
// 与 regenerationMaxSeconds2K 一样，它描述的是上游事实而不是用户意图：
// metaso 的再生成只有 2K 一档。计费若跟着请求参数走，就等于让用户自己选价格。
const regenerationResolution = "2k"

// baseVideoURLs 返回 base_video 的可重放链接（再生成形态只有一个）。
func baseVideoURLs(p params) []string {
	for _, r := range p.InputRef {
		if r.Type == "base_video" {
			return []string{r.URL}
		}
	}
	return nil
}

// Cancel 取消任务（§4.7.3）：仅 queued 且未提交上游可取消 → CAS canceled + 全额释放。
//
// **运行中的任务不支持取消**（上游已建单）。为什么不做「本地标取消」：上游仍在生成并计费，
// 平台却已退款并停止轮询 —— 结果是钱付了、产物也没人去取，与 submit_ambiguous
// 的取舍正好相反（那里是宁可等也不丢）。要真取消必须逐家上游有 cancel 契约，
// 现在拿不到；与其伪造功能，不如把这个边界写进对外文档（见 /v1/tasks/{id} 的 DELETE）。
//
// 真正的闸门是下面那条 CAS：`WHERE id=? AND status='queued'`，RowsAffected=0 即 409 且**不退款**。
// 前面的状态预检只是快速路径（省一次 UPDATE + 一次 release 重建）；
// 去掉 CAS 只留预检就会出现「读到 queued，写之前被提交线程推到 running」→ 已在上游跑的任务被退款。
func Cancel(ctx context.Context, taskID uint64, scope Scope) error {
	t, err := Get(ctx, taskID, scope)
	if err != nil {
		return err
	}
	if t.Status != StatusQueued || t.UpstreamTaskID != "" {
		return ErrNotCancelable
	}
	updated, err := cas(ctx, t.ID, []string{StatusQueued}, StatusCanceled, map[string]any{"error_msg": "任务已取消"})
	if err != nil {
		return err
	}
	if !updated {
		return ErrNotCancelable
	}
	releaseFrozen(ctx, t, statusClientClosed, "任务已取消")
	// 竞态兜底：上面读到的 t 可能还没单号，但提交线程此刻可能刚写好。
	// CAS 成功之后重读一次，有单号就开复查窗口（否则「钱退了、上游还在跑」
	// 的产物永远无人认领）。
	openRecheckAfterCancel(ctx, t.ID)
	return nil
}

// openRecheckAfterCancel 在取消成功后补开复查窗口（无上游单号时什么都不做）。
//
// 为什么不在 CAS 之前用 withRecheckWindow：那个函数虽然会重查 DB，但此刻单号
// 往往还没写入（提交线程与本函数并发），开不出来；必须放在 CAS 成功之后。
func openRecheckAfterCancel(ctx context.Context, taskID uint64) {
	var t model.Task
	if err := db.DB.WithContext(ctx).Select("id", "upstream_task_id").First(&t, taskID).Error; err != nil {
		return
	}
	extra := withRecheckWindow(ctx, &t, nil)
	if len(extra) == 0 {
		return
	}
	if err := db.DB.WithContext(ctx).Model(&model.Task{}).Where("id = ?", taskID).Updates(extra).Error; err != nil {
		logger.L().Warn("取消后开复查窗口失败（该单的上游产物可能无法追回）",
			zap.Uint64("task_id", taskID), zap.Error(err))
	}
}

// RetryReport 是重试结果的摘要（供控制器写审计）。
//
// 为什么要回传而不是在服务层直接写审计：admin_audit 的写入依赖「谁在操作」（adminID），
// 服务层拿不到；而且审计动作枚举只允许控制器使用（见 service/audit 的权威清单）。
type RetryReport struct {
	TaskID uint64
	// Resnapshot 为 true 表示本次重试**按当前价目重新快照**，且价目与创建时不同。
	Resnapshot bool
	// BeforeFrozen / AfterFrozen 是重快照前后的预扣额（Resnapshot=false 时两者相同）。
	BeforeFrozen int64
	AfterFrozen  int64
}

// Retry 重试失败/超时任务（§4.7.3，管理台）：仅 failed（kind=video 失败 / kind=timeout 超时）可重试，
// 其余状态（queued/running/succeeded/expired/canceled）一律 ErrNotRetryable（控制器映射 409，重复重试幂等）。
// 流程：CAS failed→queued 抢占重试权 → 重新 Freeze（余额/KEY 额度不足则回滚状态且不动账）
// → 同步重新提交上游。必须同步重提交：轮询 Worker 跳过无上游任务 ID 的任务，只改状态不会自动接手（必然缺陷）。
func Retry(ctx context.Context, taskID uint64, scope Scope) (*RetryReport, error) {
	t, err := Get(ctx, taskID, scope)
	if err != nil {
		return nil, err
	}
	if t.Status != StatusFailed {
		return nil, ErrNotRetryable
	}
	// 重试必须能**完整复原**上游请求，否则会出现「按素材计费、只发一段文本」的错账：
	//   - 含内联素材（data:）的任务：内联体不落库，重放必然丢素材；
	//   - v2 之前创建的任务：request_params 里只有 kind/sha256/size，没有链接与 type。
	// 两类都必须在 CAS 与重新冻结**之前**拦掉（否则会先占额度再退，白白制造资金波动）。
	p := parseParams(t.RequestParams)
	if p.HasInline || (len(p.References) > 0 && len(p.InputRef) == 0) {
		return nil, fmt.Errorf("%w：该任务包含无法重放的素材（内联上传或旧版本创建），请重新提交", ErrNotRetryable)
	}
	// 原生参数（Ark 的 content[]）无法重放：params 只存平台规范字段，厂商原生体不在其中。
	// 把原始 body 落库会造出与 InputRef 并存的第二份请求真相源（同一件事两个出处），
	// 所以选择「禁重试」而不是「再存一份」——fail-closed 且没有歧义。
	//
	// 判据必须读 **tasks.subtask_kind 这一列**，不能读 params.SubKind：
	// 子协议是适配器在 SubmitTask 里回写、由 Submit 落库的，而 request_params 在
	// 任务行插入时就已序列化完毕 —— params 里的那个副本永远是空串（恒定放行）。
	// 是否存在"不可重放的子协议"由适配器声明（relay.UnreplayableSupplier），
	// 任务层硬比 "ark.native" 就是把这句知识复制一份，改适配器时必然漏改。
	if !relay.TaskReplayable(t.Platform, t.SubTaskKind) {
		return nil, fmt.Errorf("%w：该任务使用了厂商原生参数（content[]），无法重放，请重新提交", ErrNotRetryable)
	}
	// 模型下架校验（B4-3）：重试是**真实出站 + 真实上游成本**，不能对着一个已经下线的模型重放。
	//
	// 只查 tasks.model 字符串看不出「下架」——下架的方式是 models.enabled=false 或整行删除，
	// 两种都必须回定价目录才能发现。少了这道校验，运营下线一个高成本模型后，
	// 在途的失败任务仍能被反复重试，把下线的意图完全绕过。
	cat, err := pricing.Get()
	if err != nil {
		return nil, err
	}
	item := relay.FindModel(cat, t.Model)
	if item == nil {
		return nil, fmt.Errorf("%w：模型 %s 已不存在，无法重试", ErrNotRetryable, t.Model)
	}
	if !item.Enabled {
		return nil, fmt.Errorf("%w：模型 %s 已下架，无法重试", ErrNotRetryable, t.Model)
	}
	// 旧任务（升级前创建）没有 ref_video_seconds：直接重放会按 0 秒计费（少收）。
	// 只在「价目里参考视频非免费 + 素材里确有视频 + 不是再生成」时补探一次，仍在 CAS 与冻结之前。
	if err := ensureRetryRefVideoSeconds(ctx, t); err != nil {
		return nil, err
	}
	// CAS failed→queued 抢占重试权；未命中说明已被并发重试/推进 → 幂等拒绝。
	//
	// kind 必须写回**原模态**而不是恒写 "video"：图像/音乐任务重试一次就会变回 video，
	// 管理台详情抽屉的"常规"标签与排障分类全部错位（FailTimeout 会把 kind 改成 timeout，
	// 不在这儿复原就再也回不去了）。subtask_kind 一并清空，由重新提交回写新值。
	modality := t.Modality
	if modality == "" {
		modality = KindVideo
	}
	queued, err := cas(ctx, t.ID, []string{StatusFailed}, StatusQueued, retryResetFields(modality, time.Now().Add(TaskTimeout)))
	if err != nil {
		return nil, err
	}
	if !queued {
		return nil, ErrNotRetryable
	}

	// 旧会话必须先退掉，再冻结新会话。
	//
	// 失败路径的释放是独立事务，可能在 CAS 之后、Release 之前崩溃（或 DB 抖动），
	// 于是旧 usage 仍是 pending。此时直接 Freeze 会让用户被冻结**两份**钱，
	// 且旧的那份只能等回收任务 20 分钟后才退。
	//
	// 放在 CAS 之后：CAS 是并发重试的唯一互斥点，先释放再 CAS 失败会多造一次
	// 无谓的资金往返（虽然幂等）。释放失败必须中止 —— 否则就回到二次冻结。
	if _, err := billing.New(db.DB).ReleasePendingByTask(ctx, t.ID, 0, billing.SettleInput{
		CallKind:    billing.CallKindAsync,
		TaskID:      t.ID,
		StatusCode:  409,
		ErrorMsg:    "retry_superseded: 旧会话被重试取代，冻结退回",
		UsageSource: "none",
		Anomaly:     model.AnomalyReclaimedRetrySuperseded,
	}); err != nil {
		_ = revertRetry(ctx, t.ID, "重试失败：旧会话释放失败")
		return nil, err
	}

	group := usageGroupOf(ctx, t.UsageID)
	// 重新快照（B4-3）：**按当前价目**重算，而不是沿用创建时落库的 pricing_snapshot。
	//
	// 为什么必须重快照：重试可能发生在失败后的任意时刻（几秒到几小时），
	// 期间运营完全可能调价。沿用旧快照意味着「调价对失败任务无效」——
	// 涨价时平台少收，降价时用户多付，两头都是错账，且事后无法解释。
	// 分组倍率同理取当前值（用户可能被调整了分组）。
	//
	// 代价：用户可能被按新价重扣。这是可接受的——重试是运营的显式动作，
	// 且新价会随审计日志（pricing_resnapshot）留在案卷里。
	prevSnap, prevErr := snapshotOf(t)
	snap := relay.BuildSnapshot(item, relay.GroupDiscountOf(cat, group))
	resnapshot := prevErr != nil || !sameSnapshot(prevSnap, snap)
	// 智能时长校验（按**当前**价目）：原任务用 duration=-1 提交，而运营可能已经关掉了
	// 该模型的 allow_auto_duration。此时重放会把 -1 发给一个不接受它的上游，
	// 结果是必然 400 → 渠道健康被扣分 → 一条正常渠道被熔断。
	//
	// 不自动降级成"上限秒数"：那等于悄悄改扣款额（用户以为还是智能时长）。
	// 必须让运营重新提交，由用户自己决定具体秒数。
	if p.SecondsAuto && !billing.AllowsAutoDuration(snap.SpecPrice) {
		_ = revertRetry(ctx, t.ID, "重试失败：该模型已不支持智能时长")
		return nil, fmt.Errorf("%w：模型 %s 已不支持智能时长（allow_auto_duration 已关闭），请重新提交并指定秒数", ErrNotRetryable, t.Model)
	}
	beforeFrozen := t.Frozen
	amount, perr := billing.Estimate(snap, 0, 0, usageOf(t))
	if perr != nil {
		_ = revertRetry(ctx, t.ID, "重试失败："+perr.Error())
		return nil, perr
	}
	// amount==0 是「显式免费」（0=免费一等公民），放行；价目被删成 nil 时
	// Estimate 已在上面返回 ErrPriceMissing 拦截，不会走到这里。
	sess, err := billing.New(db.DB).Freeze(ctx, billing.FreezeInput{
		UserID: t.UserID, KeyID: t.KeyID, Model: t.Model, Modality: t.Modality, Group: group,
		Snapshot: snap, Amount: amount,
		// 重试也要落维度：否则重试出来的会话被回收时同样按 1 张兜底。
		Estimate: usageOf(t),
	})
	if err != nil {
		_ = revertRetry(ctx, t.ID, "重试冻结失败")
		return nil, err
	}
	// 切换到新计费会话（旧会话已终态）并记录新预扣额。
	// 同时回写 request_params：ensureRetryRefVideoSeconds 可能补上了参考视频秒数，
	// 不回写的话下一次结算（usageOf 读库）又会按 0 秒算（少收）。
	// pricing_snapshot 也必须回写：结算与展示全部读它，不写就等于「按新价冻结、按旧价结算」。
	if ok, err := cas(ctx, t.ID, []string{StatusQueued}, StatusQueued, map[string]any{
		"usage_id": sess.UsageID, "frozen": amount, "request_params": t.RequestParams,
		"pricing_snapshot": mustJSON(snap),
	}); err != nil || !ok {
		releaseSession(ctx, sess, 500, "重试会话挂载失败")
		_ = revertRetry(ctx, t.ID, "重试失败：会话挂载失败")
		if err != nil {
			return nil, err
		}
		// CAS 返回 false 是"任务在冻结窗口内已离开 queued"（用户取消／超时过期）。
		// 必须在这里停手：继续往下走会真实提交上游（平台成本白付），
		// 而且 tasks.usage_id 仍指向已释放的旧会话，与新会话互指不同行，对账无法复现。
		logger.L().Info("重试期间任务已离开队列，放弃本次重试",
			zap.Uint64("task_id", t.ID), zap.String("status_hint", "canceled/expired"))
		return nil, ErrNotRetryable
	}
	t.UsageID = sess.UsageID
	t.Frozen = amount
	// 重试会产生新的 usage 行：旧行保留（它的 task_id 仍指向本任务），新行也要绑定，
	// 否则重试后的那条记录在 usages 里查不到归属。
	if bindErr := billing.New(db.DB).BindTask(ctx, sess.UsageID, t.ID); bindErr != nil {
		logger.L().Warn("重试任务与调用记录绑定失败", zap.Uint64("task_id", t.ID),
			zap.Uint64("usage_id", sess.UsageID), zap.Error(bindErr))
	}
	if resnapshot {
		logger.L().Info("重试按当前价目重新快照",
			zap.Uint64("task_id", t.ID), zap.String("model", t.Model),
			zap.Int64("before_frozen", beforeFrozen), zap.Int64("after_frozen", amount))
	}

	candidates, err := relay.SelectCandidates(t.Model, group)
	if err != nil {
		releaseSession(ctx, sess, 503, "重试路由失败")
		_ = revertRetry(ctx, t.ID, "重试失败："+err.Error())
		return nil, err
	}
	// 重试只在**同一方言（AdapterID）**内换渠道。
	//
	// 原文重发是重试的核心：只有原样重发才不会丢字段。而原文里必然带着原厂商的
	// 私有键（`mask_url` / `output_format` / `watermark` …），换到别家方言时这些键
	// 要么是未知字段、要么语义不同 —— 上游回 400 的话，task.go 的调用方参数错分支
	// 会**整次中止并退款**（不是「换下一个候选」），也就是说一次跨方言重试会把
	// 整个重试打死。同方言换渠道（多半是同一上游的不同 key/出口）才是有意义的重试。
	//
	// 首次提交成功时已把平台写入 tasks.platform（running["platform"]），
	// 它为空的老行不做限制（那些行也没有 submit_body，本来就只能重建）。
	if p := strings.TrimSpace(t.Platform); p != "" {
		kept := candidates[:0]
		for _, c := range candidates {
			if c.Channel.AdapterID == p {
				kept = append(kept, c)
			}
		}
		candidates = kept
	}
	if len(candidates) == 0 {
		releaseSession(ctx, sess, 503, "重试无可用渠道")
		_ = revertRetry(ctx, t.ID, "重试失败：无可用渠道")
		return nil, ErrNoChannel
	}

	report := &RetryReport{TaskID: t.ID, Resnapshot: resnapshot, BeforeFrozen: beforeFrozen, AfterFrozen: amount}

	// 上游同步重提交：直接重发 tasks.submit_body（提交时归一后的原文）。
	//
	// 原文重发意味着跨厂商重试会把 A 厂商的私有键送到 B 厂商 —— 这是「透传」的
	// 固有代价，且首次提交同样存在。要让重试只在同一方言内换渠道，
	// 由候选筛选负责（见上面的 AdapterID 过滤）。
	kind := p.Kind
	// 重试是否允许「智能时长」由当前价目决定（与首次提交同一判据、同一快照）。
	allowsAuto := billing.AllowsAutoDuration(snap.SpecPrice)
	var lastErr string
	// 与 Submit 同口径：区分「根本没打出去（限流）」与「上游真的失败」。
	limitedOnly := 0
	reachedUpstream := 0
	// 重放同样是真实出站：不留出站报文的话，「重试打到哪个渠道、发了什么」
	// 在详情页里永远是空的（提交时的记录只覆盖第一次尝试）。
	tap := &submitBodyTap{}
	for _, cand := range candidates {
		// 重试的候选已被限定为「首次受理这条任务的那个适配器」，端点能力在首次提交时
		// 已经证明过，所以这里只要求「该适配器还能做异步」（key 为空）。
		if !relay.TaskEndpointSupported(cand.Channel.AdapterID, "") {
			lastErr = fmt.Sprintf("渠道未声明 %s 异步能力，跳过：%s", modality, cand.Channel.AdapterID)
			continue
		}
		ta, ok := relay.GetTaskAdapter(cand.Channel.AdapterID)
		if !ok {
			lastErr = "适配器未实现异步任务能力：" + cand.Channel.AdapterID
			continue
		}
		ref, err := relay.PickKey(cand.Channel, nil)
		if err != nil {
			var cooling *relay.AllKeysCoolingError
			if errors.As(err, &cooling) && !cooling.Auth {
				limitedOnly++
			}
			lastErr = err.Error()
			continue
		}
		// 与 Submit 同口径计入渠道限流（P1-5）：重试提交也是真实出站。
		release, lerr := channellimit.Acquire(ctx, cand.Channel)
		if lerr != nil {
			limitedOnly++
			lastErr = lerr.Error()
			continue
		}
		body := replayedBody(t)
		submitReq := &relay.TaskSubmitRequest{
			UpstreamModel: cand.UpstreamModel, ModelID: t.Model, Body: body, Kind: kind,
			// 时长必须与首次提交同口径地显式告知适配器：原文里的 seconds 是客户端写的，
			// 而「用户没写秒数」的两种分支（智能时长 -1 / 不下发）原文里根本没有。
			AutoDuration: p.SecondsAuto && allowsAuto,
			OmitDuration: p.SecondsAuto && !allowsAuto,
			Seconds:      p.Seconds,
		}
		reachedUpstream++
		upstreamID, err := func() (string, error) {
			defer release()
			return relay.SubmitTaskOf(netguard.WithBodyTap(ctx, tap), cand.Channel.AdapterID, ta,
				relay.ChannelConfigFor(cand.Channel, ref.Key), submitReq)
		}()
		persistUpstreamIO(ctx, t.UsageID, t.UserID, tap)
		if err != nil {
			// 与 Submit 同口径：提交阶段的失败要进渠道健康，否则一条提交必挂的渠道
			// 会因为「重试每次都先撞它一次」而持续浪费每一次重试。
			relay.RecordChannelAttempt(cand.Channel, ref, err)
			if errors.Is(err, relay.ErrCallerParam) {
				msg := truncate(relay.RedactUpstream(err.Error(), cand.Channel, ref.Key), 300)
				releaseFrozen(ctx, t, 400, msg)
				_ = revertRetry(ctx, t.ID, msg)
				return nil, fmt.Errorf("%w：%s", ErrInvalidParam, msg)
			}
			// 提交结果歧义：绝不换渠道重投（详见 Submit 里的同一段说明）。
			if relay.IsAmbiguousSubmit(err) {
				logger.L().Error("重试提交结果不确定，已停止换渠道重投并全额退款",
					zap.Bool("submit_ambiguous", true),
					zap.Uint64("task_id", t.ID), zap.Uint64("channel_id", cand.Channel.ID),
					zap.String("adapter", cand.Channel.AdapterID), zap.String("model", t.Model),
					zap.Error(err))
				msg := "上游提交结果不确定（可能已受理），为避免重复计费未自动重试"
				releaseFrozenMarked(ctx, t, 504, msg, "submit_ambiguous")
				_ = revertRetry(ctx, t.ID, msg)
				return nil, fmt.Errorf("%w：%s", ErrUpstreamSubmit, msg)
			}
			lastErr = relay.RedactUpstream(err.Error(), cand.Channel, ref.Key)
			continue
		}
		// 与首次提交同一契约：空任务号必须当失败（否则任务永远不被轮询选中）。
		if strings.TrimSpace(upstreamID) == "" {
			lastErr = "适配器未返回上游任务号：" + cand.Channel.AdapterID
			logger.L().Error("重试提交成功但未返回上游任务号",
				zap.Bool("empty_upstream_id", true),
				zap.Uint64("task_id", t.ID), zap.Uint64("channel_id", cand.Channel.ID),
				zap.String("adapter", cand.Channel.AdapterID))
			continue
		}
		running := map[string]any{
			"channel_id":       cand.Channel.ID,
			"platform":         cand.Channel.AdapterID,
			"upstream_task_id": upstreamID,
		}
		// 重试换渠道可能换了子协议（同模型不同上游实现），必须跟着回写，否则轮询查错端点。
		if sk := strings.TrimSpace(submitReq.SubKind); sk != "" {
			running["subtask_kind"] = sk
		}
		if ok, err := cas(ctx, t.ID, []string{StatusQueued}, StatusRunning, running); err != nil || !ok {
			// 状态被并发推进：释放刚建的会话，避免资金悬挂。
			releaseFrozen(ctx, t, 502, "重试提交后状态已变更")
			return nil, ErrNotRetryable
		}
		return report, nil
	}

	// 全部候选提交失败：全额释放新会话 + CAS failed（任务行保留可追溯，且仍可再次重试）。
	// 分档与 Submit 一致：纯粹被限流拦下 → 429，其余 → 502。
	if reachedUpstream == 0 && limitedOnly > 0 {
		releaseFrozen(ctx, t, 429, "渠道限流："+lastErr)
		_ = revertRetry(ctx, t.ID, "渠道限流："+lastErr)
		return nil, fmt.Errorf("%w：%s", ErrRateLimited, lastErr)
	}
	releaseFrozen(ctx, t, 502, "上游提交失败："+lastErr)
	_ = revertRetry(ctx, t.ID, "上游提交失败："+lastErr)
	return nil, fmt.Errorf("%w：%s", ErrUpstreamSubmit, lastErr)
}

// sameSnapshot 判断两份计费快照是否等价（用于识别「重试期间价目是否变过」）。
//
// 比较的是**序列化后字节**：快照是纯数据结构（含 json.RawMessage 的价目原文），
// 逐字段比较既写不全也容易在新增字段时漏比。序列化失败按「不等价」处理——
// 宁可多报一次 resnapshot 审计，也不能漏报一次调价。
func sameSnapshot(a, b billing.Snapshot) bool {
	ab, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

// ensureRetryRefVideoSeconds 为「升级前创建的任务」补上参考视频秒数（就地改写 t.RequestParams）。
//
// 为什么必须在 Retry 里做：重放不重新解析请求体，usageOf(t) 只读落库的 seconds 快照。
// 旧任务没有该字段 → 结算按 0 秒 → 少收。失败一律 ErrNotRetryable（用户并没有在传参，
// 报 400「参数不合法」会把责任指错人）。
func ensureRetryRefVideoSeconds(ctx context.Context, t *model.Task) error {
	p := parseParams(t.RequestParams)
	if p.RefVideoSeconds > 0 || p.Kind == KindRegeneration {
		return nil
	}
	urls := videoRefURLs(p)
	if len(urls) == 0 {
		return nil
	}
	snap, err := snapshotOf(t)
	if err != nil {
		return fmt.Errorf("%w：%s", ErrNotRetryable, err)
	}
	if !billing.RefVideoBillable(snap.SpecPrice) {
		return nil // 参考视频免费，秒数不影响金额
	}
	secs, perr := mediaprobe.Batch(ctx, urls)
	if perr != nil {
		logger.L().Warn("重试补探参考视频时长失败",
			zap.Bool("media_probe_failed", true), zap.Error(perr))
		return fmt.Errorf("%w：该任务缺少参考视频时长且无法重新读取，请重新提交", ErrNotRetryable)
	}
	var total int64
	for _, u := range urls {
		total += secs[u]
	}
	if total > billing.MaxVideoSeconds {
		total = billing.MaxVideoSeconds
	}
	p.RefVideoSeconds = total
	t.RequestParams = mustJSON(p)
	return nil
}

// revertRetry 把重试抢占的 queued 回滚为 failed（冻结/路由/提交失败路径，不动账）。
func revertRetry(ctx context.Context, id uint64, reason string) error {
	_, err := cas(ctx, id, []string{StatusQueued}, StatusFailed, map[string]any{"error_msg": sanitizeReason(reason)})
	return err
}

// usageGroupOf 读回原计费会话的分组（tasks 表未存 group；读不到时回落 default）。
func usageGroupOf(ctx context.Context, usageID uint64) string {
	var us model.Usage
	if err := db.DB.WithContext(ctx).First(&us, usageID).Error; err != nil || strings.TrimSpace(us.Group) == "" {
		return "default"
	}
	return us.Group
}

// replayedBody 返回重试时要重发的请求体。
//
// **原文重发**：tasks.submit_body 存的是提交时归一后、真正发往上游的那份报文，
// 重试把它原样取出即可。适配器会用 req.UpstreamModel 覆盖 model、用
// req.Seconds / AutoDuration / OmitDuration 决定时长，所以这两类「平台决定项」
// 不需要在这里改写 —— 交给唯一的那一处去改，重试与首次提交才不会分叉。
//
// 改造前的做法是「按 request_params 快照重建」（imageReplay / videoReplay /
// audioReplay 三个函数，约 120 行）：重建必然丢字段 —— 客户端的 output_format、
// 厂商新增参数、音乐任务的 lyrics 全都消失，于是重试成为一个容易 400、
// 且产出与首次不同的动作。三个函数与它们依赖的 params.Vendor 一并删除。
//
// 两种例外仍需单独处理：
//   - submit_body 为空：本列上线前创建的行，没有原文可发，返回 "{}" 让适配器
//     按它自己的默认值组装（会失败，但那是一条「数据缺失」的明确错误，
//     比用一份猜出来的报文去赌上游行为要好）；
//   - 再生成：客户端的 source_task_id 是**平台**任务号，上游要的是它自己的单号，
//     这个替换只发生在服务端（params.SourceUpstream），原文里没有。
func replayedBody(t *model.Task) []byte {
	p := parseParams(t.RequestParams)
	// 再生成的重放体与生成完全不同：只有 model / source_task_id / resolution。
	// 缺少 SourceUpstream 时不能瞎拼（会重放成一次「无源再生成」并 400）。
	// 注意：applyRegeneration 只接受 metaso 源任务，本期多米不支持再生成。
	if p.Kind == KindRegeneration {
		m := map[string]any{"model": t.Model, "resolution": p.Resolution}
		if p.SourceUpstream != "" {
			m["source_task_id"] = p.SourceUpstream
		}
		if len(p.InputRef) > 0 {
			m["input_reference"] = replayRefs(p.InputRef)
		}
		return marshalBody(m)
	}
	if len(t.SubmitBody) == 0 {
		return []byte("{}")
	}
	return []byte(t.SubmitBody)
}

// replayRefs 把素材摘要还原成 input_reference 数组（首帧/尾帧与普通参考图都走 image_url）。
// 只服务再生成的重放：普通任务直接重发原文，不需要重建素材。
func replayRefs(refs []InputRefSummary) []any {
	out := make([]any, 0, len(refs))
	for _, r := range refs {
		field := "image_url"
		switch r.Type {
		case "audio":
			field = "audio_url"
		case "video", "base_video":
			field = "video_url"
		}
		out = append(out, map[string]any{"type": r.Type, field: r.URL})
	}
	return out
}

// marshalBody 序列化重放体（失败时给 "{}"，让适配器报出真实原因而不是空指针）。
func marshalBody(m map[string]any) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// Succeed 结算成功任务（§4.7.2）：产物转存（best-effort）→ 账务先行（Settle）→ CAS succeeded。
//
// 入参是整个 TaskStatus 而不是单个链接：图像一次出多张、音乐一次出两首，
// 只传一个 URL 会让其余产物永久丢失（上游链接是短效的，事后补不回来）。
// 产物本身不落上游链接，对外只暴露 GET /v1/tasks/{id}/artifacts/{index}/content。
//
// 会话已终态（迟到结果）时跳过账务，仅推进状态；结算失败则保持 running，交由超时兜底释放。
//
// 注意本函数是**正常路径**（起点 queued/running，先记账后 CAS）。
// 迟到路径走 SucceedLate，那里顺序相反，原因见 settleLate 的注释。
func Succeed(ctx context.Context, t *model.Task, st *relay.TaskStatus) error {
	return succeedWith(ctx, t, st, advanceFrom, false)
}

// SucceedLate 是「任务已判失败/超时、上游其实成功了」的收尾（W1 终态复查窗口）。
//
// 与 Succeed 共用全部产物转存逻辑，只在两处不同：
//  1. CAS 起点是 lateFrom（expired/failed），与 advanceFrom 不相交；
//  2. **先 CAS 后记账**（资金安全的取舍，见 settleLate）。
//
// 已经进入 succeeded 的任务不会走这里（lateFrom 不含它），
// 所以「正常成功」与「迟到成功」在状态机上只有一个出口，不会彼此覆盖。
func SucceedLate(ctx context.Context, t *model.Task, st *relay.TaskStatus) error {
	return succeedWith(ctx, t, st, lateFrom, true)
}

func succeedWith(ctx context.Context, t *model.Task, st *relay.TaskStatus, from []string, late bool) error {
	if t == nil || t.ID == 0 {
		return ErrNotFound
	}
	// ① 并发锁：worker 与「用户主动查任务」两条路径都可能进来，
	// 而本函数包含「下载产物 + 结算」，并发进来会重复下载并撞唯一索引。
	unlock, ok := lockSucceed(ctx, t.ID)
	if !ok {
		// 另一路正在收尾：直接返回，由那一轮负责落终态。
		return nil
	}
	defer unlock()

	// ② 重新加载：调用方可能持有一份过期视图（manifest 已经写过一轮）。
	// 用库里的最新值，避免把「上一轮已转存的 f」当成 0 又下了一遍。
	var fresh model.Task
	if err := db.DB.WithContext(ctx).Where("id = ?", t.ID).First(&fresh).Error; err == nil {
		// 只覆盖本函数会用到的字段，其余（如调用方补的 Status）保持不动。
		t.ArtifactManifest = fresh.ArtifactManifest
		t.ArtifactCount = fresh.ArtifactCount
		t.ResultFileID = fresh.ResultFileID
		t.Frozen = fresh.Frozen
		t.UsageID = fresh.UsageID
		t.ChannelID = fresh.ChannelID
		t.Platform = fresh.Platform
		t.RequestParams = fresh.RequestParams
		t.PricingSnapshot = fresh.PricingSnapshot
		t.UserID = fresh.UserID
		t.KeyID = fresh.KeyID
	}

	// ③ 归一上游产物并合并清单（按 URL 复用已转存条目，不重下）。
	urls := artifactURLs(st)
	txt := strings.TrimSpace(st.ResultText)
	man := mergeArtifactManifest(parseArtifactManifest(t.ArtifactManifest), urls, txt)

	// ④ 逐条补转存：f>0 直接跳过；失败保留 f=0 供下一轮补。
	// 注意这里**绝不删除已存在的对象**——已转存的产物是用户已经拿到的事实。
	//
	// 并发上限 4（各写各的下标，不存在数据竞争）：单件可达 512MiB 且超时 10 分钟，
	// 串行时 4 件产物要 40 分钟，全压在同一次 Succeed 里（它还持着分布式锁）。
	// 每件独立超时也在这里体现：共用外层 ctx 会让第 5 件继承前 4 件的耗时。
	var wg sync.WaitGroup
	sem := make(chan struct{}, artifactTransferConcurrency)
	for i := range man {
		if man[i].FileID > 0 {
			continue
		}
		entry := &man[i]
		// 文本产物（歌词）没有网络下载，直接落盘，不值得占并发槽。
		if entry.Text {
			if id, ok := saveTextArtifactAt(ctx, t, txt, entry.Index); ok {
				entry.FileID = id
			}
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(e *artifactEntry) {
			defer wg.Done()
			defer func() { <-sem }()
			if id, ok := transferArtifactWithRetry(ctx, t, e.URL, e.Index); ok {
				e.FileID = id
			}
		}(entry)
	}
	wg.Wait()

	// ⑤ 立即落清单（独立 UPDATE，不参与结算事务）——
	// 这是 P0-07 的根因修复：账务失败也不能丢「产物已转存」这个事实。
	_ = persistArtifactManifest(ctx, t.ID, man)
	mainID, okCount := manifestStats(man)
	t.ArtifactManifest = marshalArtifactManifest(man)
	t.ArtifactCount = okCount
	if mainID > 0 {
		id := mainID
		t.ResultFileID = &id
	}

	// ⑤.5 调用记录：只补上游用量原文。
	//
	// 为什么不再写 call_io.media：
	//  1. 产物已经由 files 表承载，调用详情按 (task_id, files) 实时查 task.contents；
	//     再往 call_io 写一份快照就是第二个真相源 —— 产物过期后 files 被过滤，
	//     快照却按 call_io 自己的保留期继续存在，界面会列出点开即 410 的条目。
	//  2. call_io.media 从此只承载**同步调用**的媒体（上游直链 / 内联数据的元信息），
	//     异步任务的产物入口只有 /tasks/{id}/contents/{i} 一套。
	//
	// Patch=true 必须保留：提交时已写过 req_raw，全量覆盖会把它清空（P0-4）。
	// 放在 ⑥ 之前是刻意的：产物全部转存失败时 ⑥ 会走 Fail，
	// 而 Fail 只补 resp_error（Patch），不会删掉这里已经写好的用量原文。
	rec := callio.Rec{UsageID: t.UsageID, UserID: t.UserID, Patch: true, RespSource: "usage"}
	if st != nil {
		if raw, jerr := json.Marshal(st.Usage); jerr == nil {
			rec.RespRaw = raw
		}
	}
	callio.Store(context.WithoutCancel(ctx), rec)

	// ⑥ 全失败闸门：上游声称有产物、但一个都没转存成功 → 判失败并全额释放。
	// 不拦的后果：用户看到「已完成」却下载不到任何东西，平台还照常收钱。
	//
	// 迟到路径**不能再走 Fail**：任务已经是 expired/failed，Fail 的 CAS 起点是
	// advanceFrom，必然命中 0 行 —— 等于什么都没做，还多一条误导性日志。
	// 正确处置是「什么都不改、按退避再试」：上游签名链接暂时不可达是常见情形，
	// 几分钟后往往就恢复了，而窗口还开着。
	// 按条计费的音乐即便**一条产物都没解析出来**也必须走失败全额释放：
	// 上游明确回成功、但没有任何可交付的音频时，按 2 条收费无法向用户解释。
	// 图片不能套这条：`len(man)==0` 在图片侧意味着"没有产物清单"，由 3× 冻结与
	// 单张保底处理，改动会牵动既有口径。
	snapForPay, _ := snapshotOf(t)
	zeroDelivery := okCount == 0 && (len(man) > 0 || billing.IsPerClip(snapForPay.SpecPrice))
	if zeroDelivery {
		if late {
			logger.L().Warn("迟到复查：上游声称成功但产物全部转存失败，按退避重试",
				zap.Bool("task_recheck_artifacts_lost", true),
				zap.Uint64("task_id", t.ID), zap.Int("artifacts", len(man)))
			return ErrArtifactsLost
		}
		logger.L().Warn("任务产物全部转存失败，按失败处理",
			zap.Bool("all_artifacts_failed", true),
			zap.Uint64("task_id", t.ID), zap.Int("artifacts", len(man)))
		if len(man) == 0 {
			// 文案要区分"有产物但下载失败"与"根本没有交付"：前者让用户重试，
			// 后者重试也不会更糟，但必须让用户知道这次没扣钱。
			return Fail(ctx, t, "没有拿到任何可交付的产物，本次不计费")
		}
		return Fail(ctx, t, "产物转存失败，请重新生成")
	}

	extra := map[string]any{"artifact_count": okCount}
	if mainID > 0 {
		extra["result_file_id"] = mainID
	}
	// 歌词正文随成功一起落库（/suno/fetch 要原样返回 text）。
	//
	// 存正文而不是"每次去对象存储读"：批量查询最多 100 条，逐条读存储会把
	// 一次查询变成 100 次 IO；正文本身很小（上游限 2000 字符）。
	if st != nil && strings.TrimSpace(st.ResultText) != "" {
		extra["result_text"] = textx.TruncateRunes(strings.TrimSpace(st.ResultText), 8192)
	}

	// ⑦ 用量：请求参数 → 合并上游实际值 → 按成功转存数修正图像张数。
	p := parseParams(t.RequestParams)
	usage := usageOf(t)
	var anomalies []string
	if st != nil {
		if mode := taskUsageSettleMode(); mode != "0" {
			persistUpstreamUsage(ctx, t.ID, st.Usage)
			var more []string
			usage, more = mergeUpstreamUsage(usage, st.Usage, p)
			anomalies = append(anomalies, more...)
		}
	}
	usage = applyArtifactActuals(usage, okCount, p, snapForPay)

	if late {
		return settleLate(ctx, t, st, from, extra, usage, anomalies)
	}

	sess, err := billing.New(db.DB).Reopen(ctx, t.UsageID)
	if err != nil {
		if errors.Is(err, billing.ErrSessionClosed) {
			// 迟到结果：账务已由超时/取消路径释放。M2：按快照重算补收（上限=原冻结额），
			// 不再白送 —— 平台已真实消耗上游成本，迟到成功不能免费交付。
			charge, lerr := billing.New(db.DB).LateSettle(ctx, t.UsageID, usage)
			if lerr != nil {
				// 补收失败（快照损坏/用户或账目已不存在）：退回「仅推进状态」的旧行为，
				// 留痕供对账，不因补收失败丢产物。
				logger.L().Warn("任务迟到成功但补收失败（按免费交付，需对账）",
					zap.Bool("task_late_success", true),
					zap.Uint64("task_id", t.ID), zap.Uint64("usage_id", t.UsageID),
					zap.Error(lerr))
				settleLateCAS(ctx, t.ID, from, extra, t.UsageID, 0)
				return nil
			}
			extra["settled"] = charge
			settleLateCAS(ctx, t.ID, from, extra, t.UsageID, charge)
			return nil
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// usage 行已归档：钱无处可记，但产物已经转存好了，必须照常交付。
			logger.L().Warn("任务成功但计费记录已归档：不记账，仅交付产物",
				zap.Bool("late_success_usage_archived", true),
				zap.Uint64("task_id", t.ID), zap.Uint64("usage_id", t.UsageID))
			settleLateCAS(ctx, t.ID, from, extra, t.UsageID, 0)
			return nil
		}
		return err
	}
	// settled_quota 必须用 Settle 的**实际入账额**，不能自己再算一遍：
	// Settle 内部还有两道降额（全局截顶 / 余额下限），自己算的结果会让
	// 任务详情与调用记录永久对不上（500 vs 250），对账时无法判断哪个是真的。
	res, err := billing.New(db.DB).Settle(ctx, sess, settleInputOf(t, st, usage, anomalies))
	if err != nil {
		return err
	}
	// 上游用量与实际请求不一致的标记由 Settle 一次写入，这里补对应的指标：
	// 没有它，看板上只能看到「有一条异常」，看不到「哪一类异常在变多」。
	incAnomalyMetrics(anomalies)
	extra["settled"] = res.Charge
	updated, cerr := cas(ctx, t.ID, from, StatusSucceeded, extra)
	if cerr != nil {
		return cerr
	}
	if !updated {
		// 资金已结算但状态未推进（并发推进/状态已被改写）：留痕供对账定位。
		logger.L().Error("任务结算后状态推进失败（资金已动，状态未落）",
			zap.Bool("task_settle_cas_miss", true),
			zap.Uint64("task_id", t.ID), zap.Uint64("usage_id", t.UsageID),
			zap.Int64("settled", res.Charge))
	}
	return nil
}

// settleLate 是迟到成功的账务收尾：**先 CAS、后记账**（与正常路径恰好相反）。
//
// 为什么顺序必须反过来（这是本函数最容易被「顺手统一成一致」改错的地方）：
//   - 迟到路径的竞争源是**用户主动 Retry**（`UPDATE tasks SET status=queued
//     WHERE status='failed'`），概率不低，用户就是看着"失败"才点的重试。
//   - 先记账后 CAS：CAS 输掉时钱已经补扣了，而任务已被重试 → **用户被收两次**
//     （旧任务补收 + 新任务冻结），且两条记录单看都完全正常，事后极难发现。
//   - 先 CAS 后记账：CAS 输掉时一分未扣，代价只是一次白下载产物。
//
// 资金安全优先级：宁可少收，不可多收。少收由对账判据 T4
// （tasks.succeeded 但 usages.released）兜底可见，多收没有任何地方能发现。
func settleLate(ctx context.Context, t *model.Task, st *relay.TaskStatus,
	from []string, extra map[string]any, usage billing.Usage, anomalies []string) error {

	extra2 := make(map[string]any, len(extra)+3)
	for k, v := range extra {
		extra2[k] = v
	}
	for k, v := range closeRecheckWindow() {
		extra2[k] = v
	}
	// error_msg 必须清空：它此刻还写着「任务超时未完成」，而状态马上变成 succeeded。
	// 留着会造出「已完成（任务超时未完成）」这种自相矛盾的详情页。
	extra2["error_msg"] = ""

	updated, cerr := cas(ctx, t.ID, from, StatusSucceeded, extra2)
	if cerr != nil {
		return cerr
	}
	if !updated {
		// 用户抢先重试、或任务已被别的路径改判。钱一分未动，只白下载了一次产物。
		logger.L().Warn("迟到成功状态推进失败（任务已被重试或改判），不记账",
			zap.Bool("late_success_race_lost", true),
			zap.Uint64("task_id", t.ID), zap.Uint64("usage_id", t.UsageID))
		appendUsageAnomaly(ctx, t.UsageID, model.AnomalyLateSuccessRaceLost)
		return nil
	}

	// 状态与产物已经落定，从这里开始只会「少收」，不会再「多收」。
	//
	// 先覆盖调用记录里的失败文案：那一条此刻还写着「任务超时未完成」（由
	// releaseFrozenMarked 在判失败时写的），而用户点开调用记录会看到
	// 「已完成的任务」配一段「超时未完成」的错误 —— 自相矛盾。
	// Patch=true：提交时已写过 req_raw，全量覆盖会把它清空。
	callio.Store(context.WithoutCancel(ctx), callio.Rec{
		UsageID: t.UsageID, UserID: t.UserID, Patch: true,
		RespError: []byte("late_success: 上游迟到成功，产物已交付"),
	})

	bill := billing.New(db.DB)
	state, serr := bill.UsageStateOf(ctx, t.UsageID)
	if serr != nil {
		if errors.Is(serr, billing.ErrUsageGone) {
			// 账目已归档：扣款无从谈起（也没有 PricingSnapshot 可依），但产物必须交出去。
			logger.L().Warn("迟到成功但计费记录已归档：不补收，产物照常交付",
				zap.Bool("late_success_usage_archived", true),
				zap.Uint64("task_id", t.ID), zap.Uint64("usage_id", t.UsageID))
			markLateNote(ctx, t.ID, "late_success: 计费记录已归档，未补收")
			return nil
		}
		return serr
	}

	switch state.Status {
	case model.UsageStatusPending:
		// 释放没跑成功、冻结还在账上 → 走**正常结算**。
		// 比 LateSettle 准：Settle 会算差额并把多冻的部分退回去，
		// 而 LateSettle 只会上限到原冻结额（多冻的一分不退，等于多收）。
		sess, rerr := bill.Reopen(ctx, t.UsageID)
		if rerr != nil {
			if errors.Is(rerr, billing.ErrSessionClosed) {
				// UsageStateOf 与 Reopen 之间被别处结算了：不再动账。
				logger.L().Warn("迟到结算竞态：复查期间账目已被别处结清，本次不动账",
					zap.Bool("late_settle_race", true), zap.Uint64("task_id", t.ID))
				return nil
			}
			return rerr
		}
		res, err := bill.Settle(ctx, sess, settleInputOf(t, st, usage, anomalies))
		if err != nil {
			return err
		}
		incAnomalyMetrics(anomalies)
		markTaskSettled(ctx, t.ID, res.Charge)
		return nil

	case model.UsageStatusReleased:
		// 已退款：按快照补收，上限 = 原冻结额。
		charge, lerr := bill.LateSettle(ctx, t.UsageID, usage)
		switch {
		case lerr == nil:
			markTaskSettled(ctx, t.ID, charge)
			metrics.Inc(metrics.NameTaskRecheckTotal, "result", "settled")
			if charge > 0 {
				metrics.Add(metrics.NameTaskRecheckCharge, float64(charge))
			}
			logger.L().Info("迟到成功已补收",
				zap.Bool("late_success", true),
				zap.Uint64("task_id", t.ID), zap.Uint64("usage_id", t.UsageID),
				zap.Int64("charge", charge), zap.Int64("frozen", t.Frozen))
			return nil

		case errors.Is(lerr, billing.ErrAlreadySettled):
			// 另一路已经补收完成（并发/上一轮成功但状态没读到）。
			// **不重复扣**，只把实际入账额回填到任务上，否则任务详情会显示 0。
			if again, e2 := bill.UsageStateOf(ctx, t.UsageID); e2 == nil &&
				again.Status == model.UsageStatusSettled {
				markTaskSettled(ctx, t.ID, again.Charge)
			}
			logger.L().Warn("迟到补收：该笔已由别处结算，本次不动账",
				zap.Bool("late_success_already_settled", true), zap.Uint64("task_id", t.ID))
			return nil

		case errors.Is(lerr, billing.ErrUserGone):
			// 用户行物理不存在：钱无处可记。产物已交付，只能留痕。
			logger.L().Warn("迟到成功但用户记录已不存在：不补收，产物照常交付",
				zap.Bool("late_success_user_gone", true),
				zap.Uint64("task_id", t.ID), zap.Uint64("usage_id", t.UsageID))
			appendUsageAnomaly(ctx, t.UsageID, model.AnomalyLateSuccessUserGone)
			markLateNote(ctx, t.ID, "late_success: 用户记录已不存在，未补收")
			return nil

		case errors.Is(lerr, billing.ErrUsageGone):
			// 复核与补收之间账目被归档：不补收、不报错。
			logger.L().Warn("迟到补收时计费记录已归档：不补收",
				zap.Bool("late_success_usage_archived", true), zap.Uint64("task_id", t.ID))
			markLateNote(ctx, t.ID, "late_success: 计费记录已归档，未补收")
			return nil

		default:
			// 真·异常（DB 抖动等）：本金未动，上抛让 Job 记 error 指标并按退避重试。
			// 重试时任务已是 succeeded，CAS 会 miss → 记 race_lost，不会重复扣款。
			return lerr
		}

	case model.UsageStatusSettled:
		// 已被回收 worker（ReclaimSettle）收过：只回填金额，绝不重复扣。
		appendUsageAnomaly(ctx, t.UsageID, model.AnomalyLateSuccessAfterReclaim)
		markTaskSettled(ctx, t.ID, state.Charge)
		logger.L().Info("迟到成功：该笔已被回收任务结算过，仅回填金额",
			zap.Bool("late_success_after_reclaim", true),
			zap.Uint64("task_id", t.ID), zap.Uint64("usage_id", t.UsageID),
			zap.Int64("charge", state.Charge))
		return nil

	default:
		// rejected（或将来新增的终态）：不扣款，照常交付。
		// 这里**不写**「按冻结额硬扣」的兜底：那条路绕过 PricingSnapshot，
		// 扣多少全凭一个已经不生效的历史数字，是凭空扣款。
		logger.L().Warn("迟到成功但账目状态不允许补收，按免费交付",
			zap.Bool("late_success_not_billable", true),
			zap.Uint64("task_id", t.ID), zap.String("usage_status", state.Status))
		markLateNote(ctx, t.ID, "late_success: 账目状态不允许补收（"+state.Status+"），未扣款")
		return nil
	}
}

// markTaskSettled 把实际入账额回填到 tasks.settled。
//
// 条件带 `status='succeeded'`：只有真的交付了才把金额挂上去。少了这个条件，
// 会出现「任务已失败但 settled>0」——对账判据 T5（charged_but_failed）必然命中，
// 而它命中的其实是我们自己写坏的数。
func markTaskSettled(ctx context.Context, taskID uint64, charge int64) {
	res := db.DB.WithContext(ctx).Model(&model.Task{}).
		Where("id = ? AND status = ?", taskID, StatusSucceeded).
		Updates(map[string]any{"settled": charge})
	if res.Error != nil {
		logger.L().Error("回填任务结算额失败（资金已动，任务金额未落）",
			zap.Bool("task_settle_cas_miss", true),
			zap.Uint64("task_id", taskID), zap.Int64("settled", charge), zap.Error(res.Error))
		return
	}
	if res.RowsAffected == 0 {
		logger.L().Warn("回填任务结算额时状态已变（不影响资金，仅详情页金额为 0）",
			zap.Bool("task_settle_cas_miss", true),
			zap.Uint64("task_id", taskID), zap.Int64("settled", charge))
	}
}

// markLateNote 给「迟到成功但没补收」的任务写一句人话原因。
//
// 为什么写进 tasks.error_msg 而不是只打日志：用户会在任务详情里看到「已完成」
// 却不理解为什么账单没扣钱（或者反过来）；运营排查时也需要一眼看到处置结果，
// 而不是去翻一周前的日志。只在状态仍为 succeeded 时写，不覆盖别人已落的终态原因。
func markLateNote(ctx context.Context, taskID uint64, note string) {
	if err := db.DB.WithContext(ctx).Model(&model.Task{}).
		Where("id = ? AND status = ?", taskID, StatusSucceeded).
		Update("error_msg", textx.TruncateRunes(note, 500)).Error; err != nil {
		logger.L().Warn("写迟到成功说明失败", zap.Uint64("task_id", taskID), zap.Error(err))
	}
}

// settleLateCAS 推进「迟到成功」的终态，并把 CAS 未命中留痕。
//
// 未命中不是"幂等成功"：它意味着 Expire / FailTimeout / Cancel 已经抢到了这一行，
// 而那一刻钱可能已经补扣过（迟到路径是「先 CAS 后记账」，但补收失败的兜底分支
// 与归档分支都发生在钱之后）。"钱动了、状态没动"必须能被对账定位 ——
// 以前这里 `_, _ = cas(...)` 直接丢结果，于是「扣了钱、任务显示失败、日志空白」。
func settleLateCAS(ctx context.Context, taskID uint64, from []string, extra map[string]any, usageID uint64, settled int64) {
	ok, err := cas(ctx, taskID, from, StatusSucceeded, extra)
	if err != nil {
		logger.L().Error("迟到成功：状态推进失败",
			zap.Bool("task_late_settle_cas_error", true),
			zap.Uint64("task_id", taskID), zap.Uint64("usage_id", usageID), zap.Error(err))
		return
	}
	if !ok {
		logger.L().Warn("迟到成功：状态已被其它路径改写（资金已动，状态未落）",
			zap.Bool("task_late_settle_cas_miss", true),
			zap.Uint64("task_id", taskID), zap.Uint64("usage_id", usageID),
			zap.Int64("settled", settled))
	}
}

// settleInputOf 组装正常结算（Settle）的入参。
//
// 抽成一处只为一件事：这些字段的口径一旦分散在多处手抄，
// 将来加一个新字段必然漏抄一处，而漏抄的表现是「某类渠道少记一项」——
// 账单看起来正常，只有对账能发现。
func settleInputOf(t *model.Task, st *relay.TaskStatus, usage billing.Usage, anomalies []string) billing.SettleInput {
	return billing.SettleInput{
		StatusCode: 200, ChannelID: t.ChannelID, AdapterID: t.Platform, Usage: usage,
		UsageSource: usageSourceOf(st),
		// 提交期的标记（档位未配价、未传时长…）必须一起落账：
		// 参数不再被拦截后，"平台按兜底口径计费"只能靠它留证。
		Anomaly: strings.Join(append(
			strings.Split(parseParams(t.RequestParams).Anomaly, ","), anomalies...), ","),
		// 上游 usage 原文。tasks.upstream_usage 保留完整原文（完整存证），
		// usages.raw_usage 是同一份内容的截断视图（billing 侧的 4000 字业务上限），
		// 用于调用记录里直接展示 —— 两份同源、用途不同，不是两个真相。
		RawUsage:           upstreamUsageRaw(t, st),
		CallKind:           billing.CallKindAsync,
		Attempts:           1,
		TaskID:             t.ID,
		VideoSecondsSource: videoSecondsSourceOf(st),
	}
}

// incAnomalyMetrics 为本次结算产生的每个 anomaly 打一个计数。
func incAnomalyMetrics(anomalies []string) {
	for _, a := range anomalies {
		if a = strings.TrimSpace(a); a != "" {
			metrics.Inc(metrics.NameUsageAnomaly, "kind", a)
		}
	}
}

// usageSourceOf 判定本次结算的用量来源（对外映射 unknown/upstream/mixed/estimated）。
//
// 为什么必须准确：对账时要能区分「按上游真实用量收的」与「按请求参数估的」。
// 两者的偏差允许范围完全不同——前者不该有偏差，后者天然可能差 20%。
func usageSourceOf(st *relay.TaskStatus) string {
	// 返回值必须落在 billing.normalizeSource 的枚举内（upstream/mixed/estimated/none），
	// 否则会被归一成空串、对外显示 unknown，对账时看不出「这笔是按什么收的」。
	// 按请求参数计费属于「估算」语义 → estimated。
	const estimated = "estimated"
	if st == nil {
		return estimated
	}
	hasUpstream := st.Usage.OutputSeconds > 0 || st.Usage.InputSeconds > 0 ||
		st.Usage.ImageCount > 0 || st.Usage.AudioSeconds > 0
	if !hasUpstream || taskUsageSettleMode() != "1" {
		return estimated
	}
	return "upstream"
}

// upstreamUsageRaw 取本次结算要写进 usages.raw_usage 的上游 usage 原文。
//
// **优先本次轮询刚拿到的 `st.Usage.Raw`，而不是 `t.UpstreamUsage`**：`t` 是这一轮
// 开始时从库里读出来的快照，`persistUpstreamUsage` 写的是库里的行、不会回填内存，
// 所以直接读 `t.UpstreamUsage` 拿到的是「上一轮的原文」或者空串 —— 而这条记录恰恰是
// 用来回答「这次到底按什么算的」。库里那份只作兜底（persist 失败时原文仍在内存里）。
func upstreamUsageRaw(t *model.Task, st *relay.TaskStatus) string {
	if st != nil && len(st.Usage.Raw) > 0 {
		return string(st.Usage.Raw)
	}
	if t == nil {
		return ""
	}
	return rawUsageText(t.UpstreamUsage)
}

// rawUsageText 把 tasks.upstream_usage（jsonb）转成 usages.raw_usage 需要的原文。
//
// 空值返回空串：raw_usage 列 NOT NULL DEFAULT ”，传 "null" 反而会被当成
// 「上游确实回了一个 null」，与"没上报"混为一谈。
func rawUsageText(raw model.JSON) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	return string(raw)
}

// videoSecondsSourceOf 说明视频秒数的来源，用于解释"为什么按 N 秒收"。
//
//   - upstream：上游自报秒数（结算口径可信度最高）；
//   - request：上游没报，按请求参数计（可能有偏差，对账时优先怀疑这一档）。
func videoSecondsSourceOf(st *relay.TaskStatus) string {
	if st != nil && st.Usage.OutputSeconds > 0 {
		return "upstream"
	}
	return "request"
}

// retryResetFields 返回「重试时必须重置的列」。
//
// 单独提出来的理由：这些字段漏清任何一个都不会报错，只会静默算错：
//   - artifact_manifest / artifact_count / result_file_id 漏清 → 上一轮的产物冒充这一轮的结果，
//     全失败闸门被旧产物绕过（「这轮什么也没转存成功，却按成功结算」）；
//   - upstream_usage 漏清 → 上一轮上游报的秒数被当成本轮用量；
//   - channel_id / platform / upstream_task_id / subtask_kind 漏清 → 轮询会去查上一轮的上游任务；
//   - kind 必须写回**原模态**（否则图像/音乐任务重试一次就变回 video，管理台分类全部错位）。
//
// artifact_count 用 0、result_file_id 用 nil：GORM 的 map 更新不会被 default 回填干扰，
// 这两个值能按原样落库（结构体更新则会丢零值）。
// retryResetFields 是重试时要把任务恢复成「全新一次提交」所需的全部字段。
//
// **这个函数漏清一个字段不会报错，只会静默算错**，所以每加一个与任务生命周期
// 相关的列，都必须回来问一句「重试后它还成立吗」：
//   - recheck_until / recheck_at：新提交会重新拿到 deadline，旧窗口的终点毫无意义；
//     不清会在重试后又失败时让两个窗口重叠，复查频率翻倍。
//   - recheck_count：新周期从第 0 档退避开始，否则直接跳到 60m 档白丢一段窗口。
func retryResetFields(modality string, deadline time.Time) map[string]any {
	return map[string]any{
		"error_msg": "", "kind": modality, "deadline_at": deadline,
		"channel_id": uint64(0), "platform": "", "upstream_task_id": "", "subtask_kind": "",
		"artifact_manifest": nil, "artifact_count": 0, "result_file_id": nil, "upstream_usage": nil,
		"recheck_until": nil, "recheck_at": nil, "recheck_count": 0,
	}
}

// 产物转存参数：下载超时、单产物上限、files 到期清理天数兜底值。
const (
	artifactFetchTimeout = 10 * time.Minute
	artifactMaxBytes     = 512 << 20
	artifactRetentionDef = 7
)

// artifactURLs 归一产物链接列表：ResultURLs 优先，为空时折叠兼容字段 ResultURL。
func artifactURLs(st *relay.TaskStatus) []string {
	out := make([]string, 0, len(st.ResultURLs)+1)
	for _, u := range st.ResultURLs {
		if s := strings.TrimSpace(u); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		if s := strings.TrimSpace(st.ResultURL); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// artifactKey 返回产物在存储后端的对象 key：**一律** artifacts/{uid}/{tid}/{idx}（含 idx=0）。
//
// 为什么不再有「单产物省掉 idx」的分支（P1-09）：那条分支让「先出一个产物、后补一个」
// 的任务在本地存储后端必然失败——第一个产物写成了 artifacts/{uid}/{tid} 这个**普通文件**，
// 第二个产物 Put 时 MkdirAll(filepath.Dir(...)) 要把它当目录用，返回 ENOTDIR
// （S3/MinIO 没有目录概念，所以测试环境测不出来，本地必现）。
//
// 存量对象不迁移：读取走 files.storage_path 这个绝对路径，历史 key 继续可读；
// 只有新写入按新规则。迁移对象反而会在灰度期制造「文件丢失」的假象。
func artifactKey(t *model.Task, idx int) string {
	return fmt.Sprintf("artifacts/%d/%d/%d", t.UserID, t.ID, idx)
}

// artifactExpiry 返回产物到期时间（未开启保留策略时返回 nil）。
func artifactExpiry() *time.Time {
	days, keep := fileRetentionDays()
	if !keep {
		return nil
	}
	e := time.Now().Add(time.Duration(days) * 24 * time.Hour)
	return &e
}

// artifactTransferConcurrency 是单任务多产物转存的并发度。
//
// 取 4：单件上限 512MiB，10 并发最坏 5GiB 在飞，会抢出口带宽与连接池；
// 而真实任务通常 1–4 件产物（SUNO 两首、多图 2–4 张、多段视频 2 段），4 并发一轮就完。
const artifactTransferConcurrency = 4

// artifactRetryDelay 是「瞬时失败」后重试一次的间隔。
//
// 只重试一次：转存失败本来就有"下一轮补"的兜底，这里只是为了吃掉一次连接抖动，
// 不是为了在 Succeed 里做长重试（那会把分布式锁持有时间拉长）。
const artifactRetryDelay = 200 * time.Millisecond

// transferArtifactWithRetry 在瞬时失败时重试一次（确定性失败不重试）。
func transferArtifactWithRetry(ctx context.Context, t *model.Task, url string, idx int) (uint64, bool) {
	for attempt := 0; attempt < 2; attempt++ {
		id, ok, transient := transferArtifactAt(ctx, t, url, idx)
		if ok || !transient {
			return id, ok
		}
		if attempt == 0 {
			select {
			case <-ctx.Done():
				return 0, false
			case <-time.After(artifactRetryDelay):
			}
		}
	}
	return 0, false
}

// transferArtifactAt 把单个上游产物转存到当前存储后端并落 files 行。
//
// 第三项 transient 表示「这次失败值得重试一次」：网络错误 / 上游 5xx / 存储瞬时故障。
// 4xx、SSRF 拦截、超上限、长度未知都是确定性结论 —— 重试只会再打一次上游。
//
// 删除语义（B3）：只有「本次新写入的对象」在 **files 插入失败且不是唯一键冲突** 时才删除。
// 唯一键冲突意味着另一轮已经转存过同一个 (task_id, artifact_index) ——
// 那一行很可能正被用户下载，删对象等于让在途下载 500。
func transferArtifactAt(ctx context.Context, t *model.Task, artifactURL string, idx int) (uint64, bool, bool) {
	link := strings.TrimSpace(artifactURL)
	if link == "" {
		return 0, false, false
	}
	if ssrf.ValidatePublicURLResolved(ctx, link) != nil {
		return 0, false, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return 0, false, false
	}
	resp, err := ssrf.SafeClient(artifactFetchTimeout).Do(req)
	if err != nil {
		return 0, false, true
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		// 5xx 是瞬时（上游/网关抖），4xx 是确定性（链接过期、权限）。
		return 0, false, resp.StatusCode >= 500
	}
	if resp.ContentLength <= 0 {
		// 长度未知时不转存（S3 兼容端点要求 Content-Length），交由 /content 实时代理。
		//
		// 必须显式计数：这条分支以前完全静默，线上表现是「产物列表永远为空」而
		// 没有任何信号说明是"上游没给 Content-Length"——回源通路本来能兜住它。
		metrics.Inc(metrics.NameArtifactStoreFailures, "reason", "no_length")
		return 0, false, false
	}
	if resp.ContentLength > artifactMaxBytes {
		// 超上限不转存：先拒再传，而不是传完靠计数器发现超限再删——那时已经把
		// 几百 MB 拖过一遍网络，还留下一个必然要删的对象。
		return 0, false, false
	}
	ct := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	if ct == "" {
		ct = "application/octet-stream"
	}
	key := artifactKey(t, idx)
	// 长度只认 resp.ContentLength：Put 内部按 io.LimitReader(r, size) 读取，
	// 外面再套一层 LimitReader 只会在「声明长度 > 实际上限」时把读取截断，
	// 让驱动报出一个长度不符的错误 —— 那会被折叠成「存储不可用」，看不出真因。
	ref, err := storage.PutActive(ctx, key, resp.Body, resp.ContentLength, ct)
	if err != nil {
		logStoreFailure(t, idx, err)
		return 0, false, true
	}

	row := &model.File{
		TaskID: t.ID, ArtifactIndex: idx, UserID: t.UserID,
		Filename: artifactName(link, t.ID), SizeBytes: resp.ContentLength,
		StorageBackend: ref.Backend, StorageBucket: ref.Bucket, StorageKey: ref.Key,
		ContentType: ct, ExpiresAt: artifactExpiry(),
	}
	if err := db.DB.WithContext(ctx).Create(row).Error; err != nil {
		// 唯一键冲突 = 另一轮已写成功：复用它，**绝不删对象**（可能正在被下载）。
		if pgerr.IsUnique(err) {
			if id, ok := existingArtifactID(ctx, t.ID, idx); ok {
				return id, true, false
			}
		}
		if _, derr := storage.DeleteRefVerified(ctx, ref); derr != nil {
			logger.L().Warn("产物补偿删除失败，可能留下孤儿对象",
				zap.String("backend", ref.Backend), zap.String("bucket", ref.Bucket),
				zap.String("key", ref.Key), zap.Error(derr))
		}
		return 0, false, true
	}
	return row.ID, true, false
}

// existingArtifactID 在 files 插入撞唯一键后回查既有行。
//
// 单独成函数是为了让两个调用点（链接产物 / 文本产物）用同一套回查口径：
// 一边按 (task_id, artifact_index) 查、另一边按别的条件查，就会出现
// 「链接产物复用成功、文本产物判定失败」的不对称行为。
func existingArtifactID(ctx context.Context, taskID uint64, idx int) (uint64, bool) {
	var exist model.File
	if err := db.DB.WithContext(ctx).
		Where("task_id = ? AND artifact_index = ?", taskID, idx).First(&exist).Error; err != nil {
		return 0, false
	}
	return exist.ID, true
}

// 唯一索引冲突（23505）判据见 pkg/pgerr：本包曾留一份同名实现，
// 与 billing / order 各写一份的后果是"同一件事有的地方当幂等、有的地方当故障"。

// storeFailWindow 是产物转存失败日志的去重窗口。
//
// 转存失败的调用方是「任务成功结算」这一条路径，重试/复查会让同一原因在短时间内
// 反复出现（每个终态任务一条）；不去重会在配置事故时把日志刷成一片，
// 真正有用的那一条反而被冲掉。计数不受去重影响（指标每一次都加）。
const storeFailWindow = time.Minute

var (
	storeFailMu   sync.Mutex
	storeFailSeen = map[string]time.Time{}
)

// logStoreFailure 记录一次产物转存失败：同一原因 1 分钟只打一条 Error，其余只计数。
//
// 转存失败**不改任务终态**（用户仍能通过 /contents 实时回源取到产物），
// 所以除了这条日志与计数，没有任何地方会报警 —— 少了它，存储配置事故的表现是
// 「产物列表永远为空」，而用户只是觉得"有点慢"。
func logStoreFailure(t *model.Task, idx int, err error) {
	reason := storeFailureReason(err)
	metrics.Inc(metrics.NameArtifactStoreFailures, "reason", reason)

	now := time.Now()
	storeFailMu.Lock()
	if last, ok := storeFailSeen[reason]; ok && now.Sub(last) < storeFailWindow {
		storeFailMu.Unlock()
		return
	}
	if len(storeFailSeen) > 64 {
		// 原因档只有几个，这里纯属防御：错误原文若带上游地址，键会更碎。
		storeFailSeen = map[string]time.Time{}
	}
	storeFailSeen[reason] = now
	storeFailMu.Unlock()

	logger.L().Error("任务产物转存失败（按原因 1 分钟去重；产物仍可实时回源）",
		zap.Uint64("task_id", t.ID), zap.Int("index", idx),
		zap.String("reason", reason), zap.Error(err))
}

// storeFailureReason 把存储错误折成有限的几个档（指标标签用，不含上游地址）。
func storeFailureReason(err error) string {
	switch {
	case errors.Is(err, storage.ErrNotConfigured):
		return "not_configured"
	case errors.Is(err, storage.ErrBackendGone), errors.Is(err, storage.ErrBackendMissing):
		return "backend_missing"
	case storage.IsTimeout(err):
		return "timeout"
	case errors.Is(err, storage.ErrUnavailable):
		return "unavailable"
	}
	return "error"
}

// saveTextArtifactAt 把文本产物（歌词）直接落盘为 text/plain 文件。
//
// 文本没有可下载的链接，不能用 transferArtifactAt；复用同一个 /content 下载通路
// （ArtifactAt 只认 storage_path + content_type），因此不需要新表、新列。
func saveTextArtifactAt(ctx context.Context, t *model.Task, text string, idx int) (uint64, bool) {
	const ct = "text/plain; charset=utf-8"
	key := artifactKey(t, idx)
	size := int64(len(text))
	ref, err := storage.PutActive(ctx, key, strings.NewReader(text), size, ct)
	if err != nil {
		logStoreFailure(t, idx, err)
		return 0, false
	}
	row := &model.File{
		TaskID: t.ID, ArtifactIndex: idx, UserID: t.UserID,
		Filename: fmt.Sprintf("task-%d-%d.txt", t.ID, idx), SizeBytes: size,
		StorageBackend: ref.Backend, StorageBucket: ref.Bucket, StorageKey: ref.Key,
		ContentType: ct, ExpiresAt: artifactExpiry(),
	}
	if err := db.DB.WithContext(ctx).Create(row).Error; err != nil {
		if pgerr.IsUnique(err) {
			if id, ok := existingArtifactID(ctx, t.ID, idx); ok {
				return id, true // 同上：唯一键冲突不删对象
			}
		}
		if _, derr := storage.DeleteRefVerified(ctx, ref); derr != nil {
			logger.L().Warn("产物补偿删除失败，可能留下孤儿对象",
				zap.String("backend", ref.Backend), zap.String("bucket", ref.Bucket),
				zap.String("key", ref.Key), zap.Error(derr))
		}
		return 0, false
	}
	return row.ID, true
}

// ArtifactStream 是一次产物读取的结果（可能只覆盖对象的一段区间）。
type ArtifactStream struct {
	Body        io.ReadCloser
	Size        int64 // 本次返回的字节数（区间长度；未知为 -1）
	Total       int64 // 对象总长度（未知为 -1）
	ContentType string
}

// ErrRangeNotSatisfiable 表示 Range 起点超出对象长度（对外 416）。
var ErrRangeNotSatisfiable = errors.New("task: 请求的字节范围不可满足")

// ErrContentNotStored 表示该任务从来没有成功转存过任何产物（允许回源上游一次）。
//
// 与 ErrContentGone 的分工是**行为**而不是文案：两者对外都是 410，
// 但只有「从未转存」允许回源 —— 过期/已清理的产物回源会让保留期彻底失效。
//
// 判据必须是 tasks.artifact_count 而不是「有没有 files 行」：清理会物理删除 files 行，
// 用行数判断会让「已清理」重新变成「从未转存」。
var ErrContentNotStored = errors.New("task: 产物从未转存")

// ErrArtifactNotFound 表示该 key/序号不在产物清单里（对外 404）。
//
// 与 ErrContentGone（410）分开：410 是「这份产物曾经存在、现在永久没了」，
// 而这个序号**从来不在清单里**（写错序号、或多产物任务按单产物枚举）。
// 合成 410 时客户端会以为任务已过期，于是不再重新取清单 —— 而清单就在那里。
var ErrArtifactNotFound = errors.New("task: 产物序号不存在")

// ErrStorageUnavailable 表示产物记录指向的存储后端已不可用（配置缺失/桶缺失/后端已移除）。
//
// 必须与 ErrContentGone（410）分开：410 的语义是「这份产物永久没了，别再试」，
// 而这里对象还在桶里，补好配置就能读 —— 对外必须是 503（可重试/报障）。
// 折叠成 410 的事故形态：运维把桶配错，所有人看到的都是「文件已过期」，
// 于是没人去查配置，产物被当成已清理的数据。
//
// 与 /v1/files/{id}/content 的口径一致（见 controller/file.go 的 uploadFailDefOf）。
var ErrStorageUnavailable = errors.New("task: 产物所在的存储后端不可用")

// artifactOpenErr 把存储层的打开失败归一到「可重试」与「已消失」两类。
//
// 判据只有这一处：读路径（区间/全量）各自写一遍时，必然出现
// 「区间读报 503、全量读报 410」这种同一份产物两种结论。
func artifactOpenErr(err error) error {
	switch {
	case errors.Is(err, storage.ErrBackendMissing),
		errors.Is(err, storage.ErrBackendGone),
		errors.Is(err, storage.ErrBucketMissing),
		errors.Is(err, storage.ErrNotConfigured):
		return ErrStorageUnavailable
	default:
		return ErrContentGone
	}
}

// readableArtifacts 返回该任务当前可读的产物行（产物读取的唯一入口）。
//
// 过滤判据只有 model.Readable 一处，与清单/调用记录共用：
// 两处各写一遍过期判断时，只要有一处忘掉，就会出现
// 「清单列了 2 个、点第 2 个 410」或「序号错位」。
func readableArtifacts(ctx context.Context, taskID uint64) ([]model.File, error) {
	var rows []model.File
	if err := db.DB.WithContext(ctx).
		Where("task_id = ?", taskID).
		Order("artifact_index ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return model.Readable(rows, time.Now()), nil
}

// HeadArtifact 只返回产物的元信息（字节数与 MIME），**不打开对象**。
//
// 为什么必须单独一条路：产物是 512MB 级视频，而 HEAD 探活（`curl -I`、CDN 预热、
// 播放器预检）是常态。c.DataFromReader 在 HEAD 下只抑制响应体，存储侧仍会把
// 整个对象读一遍 —— 一次探测等于一次全量下载，还会白烧出站流量。
//
// 与 OpenArtifactAt 的分工只在「读不读字节」，判定完全一致（可读切片 + 同一序号），
// 所以两个方法对同一个 idx 不会给出不同结论。
//
// 不与上游回源联动：一次探测不该打出站请求。从未转存的产物在 HEAD 上直接按
// 「不可用」处理，GET 才走回源。
func HeadArtifact(ctx context.Context, t *model.Task, idx int) (int64, string, error) {
	rows, err := readableArtifacts(ctx, t.ID)
	if err != nil {
		logger.L().Warn("读取产物清单失败", zap.Uint64("task_id", t.ID), zap.Error(err))
		return 0, "", err
	}
	if idx < 0 || idx >= len(rows) {
		if t.ArtifactCount == 0 {
			return 0, "", ErrContentNotStored
		}
		if idx >= 0 && idx < t.ArtifactCount {
			return 0, "", ErrContentGone
		}
		return 0, "", ErrArtifactNotFound
	}
	f := rows[idx]
	size := f.SizeBytes
	if size < 0 {
		size = 0
	}
	return size, f.EffectiveContentType(), nil
}

// OpenArtifactAt 打开指定产物的 [start, start+length) 区间；length <= 0 表示到对象末尾。
//
// 为什么需要它：产物是 512MB 级视频，播放器拖动进度条会发 Range 请求；
// 不支持 Range 就只能整段重下（每次拖动都是 512MB 出口流量）。
//
// 不支持区间读取的存储后端（未实现 storage.RangeOpener）会退化为「打开全量 + 跳过前缀」，
// 语义仍然正确，只是多读了一些字节。
func OpenArtifactAt(ctx context.Context, t *model.Task, idx int, start, length int64) (*ArtifactStream, error) {
	// 序号直接索引「可读切片」：清单也是按同一份切片稠密编号的，
	// 因此不存在「SQL 序号」与「过滤后序号」两套口径。
	rows, err := readableArtifacts(ctx, t.ID)
	if err != nil {
		// 数据库故障不是「产物没了」：410 会让客户端永久放弃，
		// 而这只是我们这边一时读不到（原样上抛 → 500/503，可重试）。
		logger.L().Warn("读取产物清单失败", zap.Uint64("task_id", t.ID), zap.Error(err))
		return nil, err
	}
	if idx < 0 || idx >= len(rows) {
		if t.ArtifactCount == 0 {
			return nil, ErrContentNotStored
		}
		// 稠密编号是 0..artifact_count-1：落在区间内说明**曾经存在**，
		// 现在读不到只可能是被清理/过期（410，别再试）；
		// 落在区间外说明这个序号从来没出现过（404，去重新取清单）。
		if idx >= 0 && idx < t.ArtifactCount {
			return nil, ErrContentGone
		}
		return nil, ErrArtifactNotFound
	}
	f := rows[idx]
	// 归属按记录走：切换过后端/桶之后，这条产物仍在**当初写入的那个位置**上。
	ref := storage.Ref{Backend: f.StorageBackend, Bucket: f.StorageBucket, Key: f.StorageKey}
	if start < 0 {
		start = 0
	}
	total := f.SizeBytes
	if total < 0 {
		total = -1
	}
	// 起点越界必须 416：返回 200 + 空体会让播放器认为「文件是空的」。
	if total > 0 && start >= total {
		return &ArtifactStream{Total: total, ContentType: f.EffectiveContentType()}, ErrRangeNotSatisfiable
	}
	want := int64(-1)
	if length > 0 {
		want = length
		if total > 0 && start+want > total {
			want = total - start
		}
	}
	if total > 0 {
		body, err := storage.OpenRangeRef(ctx, ref, start, want)
		switch {
		case err == nil:
			size := want
			if size < 0 && total > start {
				size = total - start
			}
			return &ArtifactStream{Body: body, Size: size, Total: total, ContentType: f.EffectiveContentType()}, nil
		case errors.Is(err, storage.ErrRangeUnsupported):
			// 落到下面的「全量 + 跳过前缀」：语义正确，只是多读一些字节。
		default:
			return nil, artifactOpenErr(err)
		}
	}
	// 降级：打开全量，跳过前 start 字节。
	body, size, err := storage.OpenRef(ctx, ref)
	if err != nil {
		return nil, artifactOpenErr(err)
	}
	if start > 0 {
		if _, err := io.CopyN(io.Discard, body, start); err != nil {
			body.Close()
			return nil, ErrContentGone
		}
	}
	out := &ArtifactStream{Body: body, Total: size, ContentType: f.EffectiveContentType()}
	switch {
	case want > 0:
		out.Size = want
		out.Body = &limitedReadCloser{Reader: io.LimitReader(body, want), Closer: body}
	case size > 0:
		out.Size = size - start
	default:
		out.Size = -1
	}
	return out, nil
}

// limitedReadCloser 把 LimitReader 与底层流句柄的关闭合并。
type limitedReadCloser struct {
	io.Reader
	io.Closer
}

// ArtifactEntry 是对外产物清单的一项。
//
// 类型别名而不是再定义一份结构体：调用详情（logs.ArtifactRef）与开放 API 用的是
// 同一份产物，形状必须逐字相同（含 kind），否则前端要写三套映射。
type ArtifactEntry = model.Artifact

// ArtifactsOf 返回任务已转存的产物清单（按 artifact_index 升序；已过期的项不计入）。
//
// 归一规则（跳过过期、稠密重编号、生效 MIME、渲染档）只有 model.ArtifactListing 一处；
// 这里只负责查库。
func ArtifactsOf(ctx context.Context, taskID uint64) ([]ArtifactEntry, error) {
	var rows []model.File
	if err := db.DB.WithContext(ctx).
		Where("task_id = ?", taskID).
		Order("artifact_index ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return model.ArtifactListing(rows, time.Now()), nil
}

// ArtifactRowsByTask 批量返回一页任务的产物（task_id → 清单，序号升序）。
//
// 列表页的 result_url 需要每条任务的首个产物，逐条调 ArtifactsOf 就是 N+1
// （一页 20 条 = 20 次查询）。这里一次 IN 查询解决；空 ids 直接返回空 map，不打库。
// 归一仍走 model.ArtifactListing，与详情接口同一套规则。
func ArtifactRowsByTask(ctx context.Context, ids []uint64) (map[uint64][]ArtifactEntry, error) {
	out := make(map[uint64][]ArtifactEntry, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var rows []model.File
	if err := db.DB.WithContext(ctx).
		Where("task_id IN ?", ids).
		Order("task_id ASC, artifact_index ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	grouped := make(map[uint64][]model.File, len(ids))
	for i := range rows {
		grouped[rows[i].TaskID] = append(grouped[rows[i].TaskID], rows[i])
	}
	now := time.Now()
	for tid, rs := range grouped {
		out[tid] = model.ArtifactListing(rs, now)
	}
	return out, nil
}

// OpenUpstreamArtifact 按设计 §4.7.5「降级代理模式实时拉上游」打开第 idx 个上游产物流。
//
// 调用条件被刻意收窄到**一个**：该任务一条产物都没转存成功（ErrContentNotStored）。
// 旧实现的调用条件是「本地打开失败」—— 过期、对象缺失、存储抖动都会命中，
// 于是产物过期后仍能从上游拿到文件，保留期形同虚设。
//
// idx 必须透传：多产物任务（SUNO 一次两首、多米多视频）的链接本来就都在
// artifact_manifest 里（零出站可取），硬编码 0 会让第 2 件起永远 410 ——
// 明明是"链接在手、只是没转存"，用户看到的是"产物不存在"。
// 非成功态、链接缺失、非公网地址或上游非 2xx 一律返回 ErrContentGone。
func OpenUpstreamArtifact(ctx context.Context, t *model.Task, idx int) (io.ReadCloser, int64, string, error) {
	url, err := ResultURLAt(ctx, t, idx)
	if err != nil {
		return nil, 0, "", err
	}
	if ssrf.ValidatePublicURLResolved(ctx, url) != nil {
		return nil, 0, "", ErrContentGone
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, "", ErrContentGone
	}
	resp, err := ssrf.SafeClient(artifactFetchTimeout).Do(req)
	if err != nil {
		return nil, 0, "", ErrContentGone
	}
	if resp.StatusCode/100 != 2 {
		resp.Body.Close()
		return nil, 0, "", ErrContentGone
	}
	ct := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	if ct == "" {
		ct = "application/octet-stream"
	}
	size := resp.ContentLength
	if size <= 0 {
		size = -1
	}
	return resp.Body, size, ct, nil
}

// PurgeArtifacts 清理到期任务产物（对象 + 记录）。
//
// 循环、复核与统计只有 storage.PurgeExpired 一份实现；这里只回答
// 「取哪些行、删哪张表、被挡住多少」—— 与上传件清理共用同一套删除判据。
//
// 候选集按「后端仍可构建 + 位置完整」过滤（防饿死），被过滤掉的行数单独 COUNT。
// 注意：只删 files 行，**不动 tasks.artifact_count** —— 那个计数是「这任务曾经有产物」
// 的唯一记录，删掉它会让已清理的产物重新被当成「从未转存」而回源上游。
func PurgeArtifacts(ctx context.Context, batch int) (storage.PurgeStats, error) {
	usable := storage.UsableBackends()
	return storage.PurgeExpired(ctx, batch, storage.PurgeOps{
		Kind: "artifact",
		Pick: func(ctx context.Context, limit int) ([]storage.PurgeRow, error) {
			var rows []model.File
			err := db.DB.WithContext(ctx).
				Where("expires_at IS NOT NULL AND expires_at <= now()").
				Where("storage_backend IN ?", usable).
				Where("storage_backend = 'none' OR storage_bucket <> ''").
				Order("expires_at ASC, id ASC").Limit(limit).Find(&rows).Error
			if err != nil {
				return nil, err
			}
			out := make([]storage.PurgeRow, 0, len(rows))
			for i := range rows {
				out = append(out, storage.PurgeRow{
					// files 的主键是 uint64，统一转字符串交给通用循环。
					ID:    strconv.FormatUint(rows[i].ID, 10),
					Bytes: rows[i].SizeBytes,
					Ref: storage.Ref{
						Backend: rows[i].StorageBackend,
						Bucket:  rows[i].StorageBucket,
						Key:     rows[i].StorageKey,
					},
				})
			}
			return out, nil
		},
		DeleteRow: func(ctx context.Context, id string) (int64, error) {
			res := db.DB.WithContext(ctx).Delete(&model.File{}, "id = ?", id)
			return res.RowsAffected, res.Error
		},
		Blocked: func(ctx context.Context) (map[string]int64, error) {
			return countBlockedArtifacts(ctx, usable)
		},
	})
}

// countBlockedArtifacts 统计「到期但没进候选集」的产物行，按原因分档。
func countBlockedArtifacts(ctx context.Context, usable []string) (map[string]int64, error) {
	out := map[string]int64{}
	var n int64
	if err := db.DB.WithContext(ctx).Model(&model.File{}).
		Where("expires_at IS NOT NULL AND expires_at <= now() AND storage_backend NOT IN ?", usable).
		Count(&n).Error; err != nil {
		return nil, err
	}
	if n > 0 {
		out["backend"] = n
	}
	n = 0
	if err := db.DB.WithContext(ctx).Model(&model.File{}).
		Where("expires_at IS NOT NULL AND expires_at <= now() AND storage_backend IN ? AND storage_backend <> 'none' AND storage_bucket = ''", usable).
		Count(&n).Error; err != nil {
		return nil, err
	}
	if n > 0 {
		out["bucket"] = n
	}
	return out, nil
}

// fileRetentionDays 读取 files 任务产物保留天数；未配置/非正值表示不设过期（不自动清理）。
//
// 缺键按「不删」处理：旧实现缺键默认 7 天并立即开始删产物 —— 一个没跑过 seed 的库
// 会在无人配置的情况下静默删除数据。「没配置」不能等价于「删 7 天前的产物」。
// 管理接口的校验要求该键 ≥1，所以「关闭自动清理」只能靠不写这个键（运维手工）。
func fileRetentionDays() (int64, bool) {
	raw, ok := conf.System().Get("file_retention_days")
	if !ok {
		logger.L().Warn("file_retention_days 未配置：产物不设过期时间（不会自动清理）")
		return 0, false
	}
	var days int64
	if err := json.Unmarshal(raw, &days); err != nil || days <= 0 {
		return 0, false
	}
	return days, true
}

// artifactName 由产物链接推导文件名（缺省用 task-{id}）。
func artifactName(link string, taskID uint64) string {
	if u, err := url.Parse(link); err == nil {
		if base := path.Base(u.Path); base != "" && base != "/" && base != "." {
			return truncate(base, 255)
		}
	}
	return fmt.Sprintf("task-%d", taskID)
}

// Fail 任务失败：CAS failed + 全额释放（E3 异步退款；迟到结果不结算不退款）。
func Fail(ctx context.Context, t *model.Task, reason string) error {
	return failWith(ctx, t, 502, reason)
}

// FailTimeout 轮询网络失败超限：CAS failed(kind=timeout) + 全额释放（§4.7.3）。
//
// 开复查窗口：这类任务的典型成因是「上游响应慢/连接抖动」，
// 而上游那边很可能已经出图了。没有窗口，产物和钱一起丢。
func FailTimeout(ctx context.Context, t *model.Task, reason string) error {
	extra := withRecheckWindow(ctx, t, map[string]any{
		"error_msg": sanitizeReason(reason), "kind": KindTimeout,
	})
	updated, err := cas(ctx, t.ID, advanceFrom, StatusFailed, extra)
	if err != nil {
		return err
	}
	if !updated {
		return nil
	}
	releaseFrozen(ctx, t, 504, reason)
	return nil
}

// FailOrphanSubmit 回收「冻结成功但从未拿到上游任务号」的任务（B4-1）。
//
// 与 FailTimeout 的唯一区别在 CAS 条件：**多带 `COALESCE(upstream_task_id,”) = ”`**。
//
// 为什么这个附加条件不可省：调用方先 SELECT 出「queued 且无上游号」的候选，
// 再逐条推进。两步之间提交线程完全可能刚好 CAS 到 running 并写上上游任务号——
// 那条任务此刻正在上游真实生成。只判 status 的 CAS 会把它判失败并全额退款：
// 上游钱照付、产物丢弃、用户以为失败了。带上上游号条件后，这种情况命中 0 行、静默跳过。
func FailOrphanSubmit(ctx context.Context, t *model.Task, reason string) error {
	res := db.DB.WithContext(ctx).Model(&model.Task{}).
		Where("id = ? AND status = ? AND COALESCE(upstream_task_id, '') = ''", t.ID, StatusQueued).
		Updates(map[string]any{
			"status": StatusFailed, "error_msg": sanitizeReason(reason), "kind": KindTimeout,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		// 已被提交线程接手（或已被别处推进）：不是异常，什么都不做。
		return nil
	}
	releaseFrozen(ctx, t, 504, reason)
	return nil
}

// Expire 超时未完成：CAS expired + 全额释放（§4.7.3）。
//
// 这条是复查窗口最主要的来源：平台侧 deadline 到了、上游还在跑，
// 几乎是「迟到成功」的定义。开窗后由 TaskRecheckJob 负责把产物捞回来并补收。
func Expire(ctx context.Context, t *model.Task) error {
	extra := withRecheckWindow(ctx, t, map[string]any{"error_msg": "任务超时未完成"})
	updated, err := cas(ctx, t.ID, advanceFrom, StatusExpired, extra)
	if err != nil {
		return err
	}
	if !updated {
		return nil
	}
	releaseFrozen(ctx, t, 504, "任务超时未完成")
	return nil
}

// Get 按 id + 归属范围取任务（不存在/越权一律 ErrNotFound，不泄露存在性）。
func Get(ctx context.Context, id uint64, scope Scope) (*model.Task, error) {
	if err := rejectEmptyScope("get", scope, id); err != nil {
		return nil, err
	}
	var t model.Task
	q := scope.applyScope(db.DB.WithContext(ctx).Where("id = ?", id))
	if err := q.First(&t).Error; err != nil {
		return nil, ErrNotFound
	}
	return &t, nil
}

// IsPublicID 判断一个字符串是否是合法的对外任务号。
//
// 只认 `task_` + 26 位：ULID 是 Crockford base32 的 26 字符，迁移回填的
// 十六进制串也是 26 位。**不解析内容**（不把号反解成 id）—— 号是不透明的，
// 反解只会给未来换算法留下一个隐式约束。
func IsPublicID(raw string) bool {
	s := strings.TrimSpace(raw)
	return len(s) == len(model.PublicIDPrefix)+26 && strings.HasPrefix(s, model.PublicIDPrefix)
}

// GetByRef 按对外任务号取任务（归属由 scope 限定，越权与不存在同码）。
//
// 所有 HTTP 入口都走这一个函数：路径参数、游标、再生成源任务号都是同一套号，
// 各自写一份 WHERE 就一定会有一处忘了带 scope —— 那就是越权读别人的产物。
func GetByRef(ctx context.Context, ref string, scope Scope) (*model.Task, error) {
	if !IsPublicID(ref) {
		return nil, ErrNotFound
	}
	if err := rejectEmptyScope("get_by_ref", scope, 0); err != nil {
		return nil, err
	}
	var t model.Task
	q := scope.applyScope(db.DB.WithContext(ctx).Where("public_id = ?", ref))
	if err := q.First(&t).Error; err != nil {
		return nil, ErrNotFound
	}
	return &t, nil
}

// ListByRefs 按一组对外任务号批量取任务（保持入参顺序，去重）。
//
// 为什么必须一条 SQL：`/suno/fetch` 最多收 100 个 id，逐条 GetByRef 就是 100 次
// 往返 + 100 次越权判定；而批量查询有明确的性能承诺（客户端每 2 秒轮一次）。
//
// 语义与 GetByRef 一致：**任一条**不存在或越权 → 整批 ErrNotFound。
// 不做"能取几条给几条"：那样调用方无法区分"某条还在排队"与"某条根本不是我的"。
func ListByRefs(ctx context.Context, refs []string, scope Scope) ([]model.Task, error) {
	if len(refs) == 0 {
		return nil, ErrNotFound
	}
	seen := make(map[string]bool, len(refs))
	keys := make([]string, 0, len(refs))
	for _, r := range refs {
		k := strings.TrimSpace(r)
		if k == "" || seen[k] {
			continue
		}
		if !IsPublicID(k) {
			return nil, ErrNotFound
		}
		seen[k] = true
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil, ErrNotFound
	}
	if err := rejectEmptyScope("list_by_refs", scope, 0); err != nil {
		return nil, err
	}
	var rows []model.Task
	q := scope.applyScope(db.DB.WithContext(ctx).Where("public_id IN ?", keys))
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) != len(keys) {
		// 差了任何一条都判整批不可取：调用方据此认为"输入里有非法号"。
		return nil, ErrNotFound
	}
	byKey := make(map[string]model.Task, len(rows))
	for _, r := range rows {
		byKey[r.PublicID] = r
	}
	out := make([]model.Task, 0, len(keys))
	for _, k := range keys {
		out = append(out, byKey[k])
	}
	return out, nil
}

// ResolveRef 把对外任务号换成内部数值 id（只用于「必须用 id 的地方」，如列表游标）。
func ResolveRef(ctx context.Context, ref string) (uint64, error) {
	if !IsPublicID(ref) {
		return 0, ErrNotFound
	}
	var id uint64
	if err := db.DB.WithContext(ctx).Model(&model.Task{}).
		Where("public_id = ?", ref).Select("id").Scan(&id).Error; err != nil {
		return 0, err
	}
	if id == 0 {
		return 0, ErrNotFound
	}
	return id, nil
}

// List 返回任务列表与总数（scope 限定归属，filter 叠加筛选；按 id 倒序）。
//
// 「先 Count 再 Find」必须各建一条链：GORM 的 *gorm.DB 在 Count 后会被复用
// （statement 已带上 COUNT 的 select/order），沿用同一条链第二次查询会拿到错结果。
func List(ctx context.Context, scope Scope, f ListFilter) ([]model.Task, int64, error) {
	if err := rejectEmptyScope("list", scope, 0); err != nil {
		return nil, 0, err
	}
	build := func() *gorm.DB {
		q := scope.applyScope(db.DB.WithContext(ctx).Model(&model.Task{}))
		if f.UserID != 0 {
			q = q.Where("user_id = ?", f.UserID)
		}
		if f.AppScoped {
			// 归因链 tasks.key_id → api_keys.app_id。子查询而不是 JOIN：
			// Count 与 Find 共用 build()，JOIN 会让两者都要多一份去重顾虑。
			q = q.Where("key_id IN (SELECT id FROM api_keys WHERE app_id = ?)", f.AppID)
		}
		if f.Status != "" {
			q = q.Where("status = ?", f.Status)
		}
		if f.Model != "" {
			q = q.Where("model = ?", f.Model)
		}
		if f.Modality != "" {
			q = q.Where("modality = ?", f.Modality)
		}
		if f.After != 0 {
			q = q.Where("id < ?", f.After)
		}
		return q
	}

	var total int64
	if err := build().Count(&total).Error; err != nil {
		return nil, 0, err
	}
	size := f.PageSize
	page := f.Page
	if size <= 0 {
		page, size = normalizePage(f.Page, f.PageSize)
	}
	// 显式回落：`Order("")` 在 GORM 里会拼出空的 ORDER BY 片段，是语法错误
	// 而不是"不排序"。默认值必须在这里给出，不能指望每个调用方都传。
	orderBy := f.OrderBy
	if orderBy == "" {
		orderBy = "id DESC"
	}
	q := build()
	if f.After != 0 {
		// 游标分页：不再偏移，直接取 after 之后的 size 条。
		//
		// 这里**强制 id DESC 并忽略 f.OrderBy**：`id < after` 这个游标条件只有在
		// "按 id 单调倒序"下才成立，换成 created_at 就会漏行/重复行。
		// 该分支只由 /v1/tasks 兼容端点使用，那个端点不声明 sort。
		q = q.Order("id DESC").Limit(size)
	} else {
		if page < 1 {
			page = 1
		}
		q = q.Order(orderBy).Offset((page - 1) * size).Limit(size)
	}
	var rows []model.Task
	if err := q.Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// ResultURLAt 实时向上游解析第 idx 个产物链接（未转存/已清理时的降级代理）。
//
// 走 relay.PollTaskOf 而不是 ta.PollTask：多路径协议（可灵 5 条提交路径对应不同查询路径）
// 必须带上 subtask_kind 才能查对端点。非成功态、越界、链接非 http(s) 一律 ErrContentGone。
func ResultURLAt(ctx context.Context, t *model.Task, idx int) (string, error) {
	if t.Status != StatusSucceeded {
		return "", ErrContentGone
	}
	// 清单优先：零出站、顺序与对外序号一一对应。只有 B3 之前的老任务没有清单。
	if u, ok := ResultURLAtFromManifest(t, idx); ok {
		if u == "" {
			return "", ErrContentGone
		}
		return u, nil
	}
	if t.UpstreamTaskID == "" {
		return "", ErrContentGone
	}
	ta, ok := relay.GetTaskAdapter(t.Platform)
	if !ok {
		return "", ErrContentGone
	}
	var ch model.Channel
	if err := db.DB.WithContext(ctx).First(&ch, t.ChannelID).Error; err != nil {
		return "", ErrContentGone
	}
	ref, err := relay.PickKey(&ch, nil)
	if err != nil {
		return "", ErrContentGone
	}
	st, err := relay.PollTaskOf(ctx, ta, relay.ChannelConfigFor(&ch, ref.Key), &relay.TaskPollRequest{
		UpstreamTaskID: t.UpstreamTaskID, SubKind: t.SubTaskKind,
	})
	if err != nil || st == nil {
		if err != nil {
			// 取产物是用户正在等的同步请求：一枚 401 的 key 会让他每次都失败，
			// 而失败文案只是"产物不存在"，排查方向完全错。冷掉它，下一次换一把。
			relay.CoolKeyIfAuth(&ch, ref, err)
		}
		return "", ErrContentGone
	}
	urls := artifactURLs(st)
	if idx < 0 || idx >= len(urls) {
		return "", ErrContentGone
	}
	u := urls[idx]
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return "", ErrContentGone
	}
	return u, nil
}

// ResultURLAtFromManifest 从产物清单取第 idx 个产物的上游链接（**零上游请求**）。
//
// 为什么优先用清单：清单是转存时的快照，顺序与 /contents/{i} 的对外序号一一对应；
// 而实时查上游拿到的是「此刻上游返回的顺序」，两者可能不同（上游重排、部分产物过期被摘）。
// 只有清单缺失（B3 之前的老任务）时才回退到实时查询。
func ResultURLAtFromManifest(t *model.Task, idx int) (string, bool) {
	man := parseArtifactManifest(t.ArtifactManifest)
	if len(man) == 0 {
		return "", false
	}
	if idx < 0 || idx >= len(man) {
		return "", true // 有清单但越界：确定没有，不要再去问上游
	}
	e := man[idx]
	if e.Text || strings.TrimSpace(e.URL) == "" {
		return "", true
	}
	if !strings.HasPrefix(e.URL, "http://") && !strings.HasPrefix(e.URL, "https://") {
		return "", true
	}
	return e.URL, true
}

// cas 是状态推进的唯一实现（§4.7.1）：UPDATE ... WHERE id=? AND status IN (from)，返回是否命中。
func cas(ctx context.Context, id uint64, from []string, to string, extra map[string]any) (bool, error) {
	updates := map[string]any{"status": to}
	for k, v := range extra {
		updates[k] = v
	}
	res := db.DB.WithContext(ctx).Model(&model.Task{}).Where("id = ? AND status IN ?", id, from).Updates(updates)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// failWith 是失败路径的公共实现（CAS failed + 全额释放）。
func failWith(ctx context.Context, t *model.Task, status int, reason string) error {
	return failWithAnomaly(ctx, t, status, reason, "")
}

// failWithAnomaly 是 failWith 的带对账标记版本（B4-2：submit_ambiguous）。
//
// anomaly 只在「本次真的把状态推到了 failed」时随释放一起落库；
// CAS 未命中说明状态已被别处推进（那次的释放会带上它自己的标记），此处不再重复写。
//
// 开复查窗口：本函数的两个调用方都会走到「上游可能已经建单」的场景
// （提交歧义 = 不知道受没受理；产物全失败 = 上游已经出图）。
// 无单号时 withRecheckWindow 返回原 extra，不会写窗口字段。
func failWithAnomaly(ctx context.Context, t *model.Task, status int, reason, anomaly string) error {
	extra := withRecheckWindow(ctx, t, map[string]any{"error_msg": sanitizeReason(reason)})
	updated, err := cas(ctx, t.ID, advanceFrom, StatusFailed, extra)
	if err != nil {
		return err
	}
	if !updated {
		return nil
	}
	releaseFrozenMarked(ctx, t, status, reason, anomaly)
	return nil
}

// releaseFrozen 全额释放预扣（资金 + KEY 额度回退）：会话已终态时静默跳过。
func releaseFrozen(ctx context.Context, t *model.Task, status int, reason string) {
	releaseFrozenMarked(ctx, t, status, reason, "")
}

// releaseFrozenMarked 是全量释放的实体（带 anomaly 透传）。
func releaseFrozenMarked(ctx context.Context, t *model.Task, status int, reason, anomaly string) {
	// 调用记录：上游错误原文（C1-R3）。
	//
	// 放在 Release **之前**：即便释放失败（DB 抖动），失败原因也已经留下 ——
	// 否则详情页只显示"已释放"，说不出为什么失败。
	// Patch=true：提交时已写过 req_raw，全量覆盖会把它清空。
	if t.UsageID > 0 {
		callio.Store(context.WithoutCancel(ctx), callio.Rec{
			UsageID: t.UsageID, UserID: t.UserID, Patch: true,
			RespError: []byte(sanitizeReason(reason)),
		})
	}
	// 统一走 ReleaseByUsageID：冻结额从账本反推，不依赖调用方手里的 Session。
	// released=false 表示这笔早已终态（已被结算/回收抢先）——幂等，无需告警。
	if _, err := billing.New(db.DB).ReleaseByUsageID(ctx, t.UsageID, billing.SettleInput{
		CallKind: billing.CallKindAsync, TaskID: t.ID,
		StatusCode: status, ChannelID: t.ChannelID, AdapterID: t.Platform,
		ErrorMsg: sanitizeReason(reason), UsageSource: "none", Anomaly: anomaly,
	}); err != nil {
		logger.L().Error("任务预扣释放失败（等待回收任务兜底）",
			zap.Bool("billing_release_failed", true),
			zap.Uint64("task_id", t.ID), zap.Uint64("usage_id", t.UsageID), zap.Error(err))
	}
}

// releaseSession 释放尚未落库任务的内存会话（提交失败路径）。
//
// 这是**唯一**允许直接用内存 Session 的释放路径：任务行还没建，
// 账本里除了 usage 之外没有任何可反查的锚点，而 sess 刚刚由 Freeze 返回（必然新鲜）。
func releaseSession(ctx context.Context, sess *billing.Session, status int, reason string) {
	if err := billing.New(db.DB).Release(ctx, sess, billing.SettleInput{
		// 任务行尚未落库，没有 task_id 可绑；CallKind 仍必填，否则会被记成 sync。
		CallKind:   billing.CallKindAsync,
		StatusCode: status, ErrorMsg: sanitizeReason(reason), UsageSource: "none",
	}); err != nil && !errors.Is(err, billing.ErrSessionClosed) {
		logger.L().Error("任务预扣释放失败（未落库路径）",
			zap.Bool("billing_release_failed", true),
			zap.Uint64("usage_id", sess.UsageID), zap.Error(err))
	}
}

// UsageOf 返回任务的对外用量视图（与结算完全同源）。
//
// 必须是同一份解析：若展示另写一套，就会出现「账单按 5 秒、接口显示 10 秒」这类
// 无法解释的差异，用户只会认更有利的那个。
func UsageOf(t *model.Task) billing.Usage { return usageOf(t) }

// usageOf 从 request_params 复原结算所需用量（视频秒数/档位、图像档位/张数、三类素材个数）。
//
// 图像两项对视频模型恒为空/0，videoCharge 完全不读它们，存量计费逐字不变。
func usageOf(t *model.Task) billing.Usage {
	p := parseParams(t.RequestParams)
	u := billing.Usage{
		VideoResolution: p.Resolution, VideoSeconds: p.Seconds, RefVideoSeconds: p.RefVideoSeconds,
		ImageSpec: p.ImageSpec, ImageN: billableUnits(p),
		ImageRatio: p.ImageRatio, ImageSpecExplicit: p.ImageExplicit, ImageSpecSource: p.ImageSource,
	}
	countRefs(&u, p.References)
	return u
}

// usageOfRequest 由提交请求构造冻结用用量（必须与 usageOf **完全同构**，
// 否则会出现「冻结 1 张、结算 2 张」这类双口径漂移）。
func usageOfRequest(req *SubmitRequest) billing.Usage {
	// VideoResolution 取**计费档位**（Params.Resolution），不是出站原话（req.Resolution）：
	// 参数不再被拦截后，用户可能写一个价目里没有的档位（1440p），
	// 出站照发，但冻结必须按兜底档算，否则 Estimate 会 ErrInvalidTier。
	u := billing.Usage{
		VideoResolution: req.Params.Resolution, VideoSeconds: req.Seconds, RefVideoSeconds: req.RefVideoSeconds,
		ImageSpec: req.Params.ImageSpec, ImageN: billableUnits(req.Params),
		ImageRatio: req.Params.ImageRatio, ImageSpecExplicit: req.Params.ImageExplicit,
		ImageSpecSource: req.Params.ImageSource,
	}
	countRefs(&u, req.Refs)
	return u
}

// MusicMeta 是音乐/歌词任务对外投影需要的请求侧字段（`/suno/fetch` 用）。
type MusicMeta struct {
	Action string
	Title  string
	Tags   string
	Prompt string
	Lyrics string
	MV     string
}

// MusicMetaOf 从任务行取请求侧元信息（解析失败的旧行返回零值，不报错）。
func MusicMetaOf(t *model.Task) MusicMeta {
	p := parseParams(t.RequestParams)
	m := MusicMeta{
		Action: p.MusicAction, Title: p.Title, Tags: p.Tags,
		Prompt: p.Prompt, MV: p.MusicMV,
	}
	// lyrics 不在 params 里：它只随 submit_body 落库（重试要原样重发）。
	// 从原文里读而不是再落一列：同一份歌词存两处，改名/截断时必然分叉。
	if len(t.SubmitBody) > 0 {
		var raw struct {
			Lyrics string `json:"lyrics"`
		}
		if err := json.Unmarshal(t.SubmitBody, &raw); err == nil {
			m.Lyrics = strings.TrimSpace(raw.Lyrics)
		}
	}
	if m.Prompt == "" {
		m.Prompt = m.Lyrics
	}
	if m.Action == "" {
		// 历史行没落 MusicAction：有文本产物或正文的判定词，其余按作曲。
		// 这是**兼容**而不是第二真相源 —— 新写入的行一定有 action。
		if t.ResultText != "" {
			m.Action = "lyrics"
		} else {
			m.Action = "music"
		}
	}
	return m
}

// billableUnits 返回这一单的**计费件数**：图片是张数，音乐/歌词是按条计费的条数。
//
// 冻结与结算共用它，两条 path 不可能读不同的字段（那正是"冻结 2 首、结算按 1 首"
// 这类漂移的来源）。
func billableUnits(p params) int64 {
	if p.MusicClips > 0 {
		return p.MusicClips
	}
	return p.ImageN
}

// countRefs 按类别累计素材个数（**唯一实现**：冻结与结算各写一份 switch 时，
// 新增一个类别只改一半就会出现"冻结按 2 张、结算按 0 张"）。
// countImageRefs 只数图片素材。
//
// 同步链路（relay.imageRefCount）只能看见 image 一个字段，看不见音视频；
// 图片模态的参考图额度（组图「参考图+生成 ≤ 15」）也只由图片构成。
// 异步这里若用 countRefs 数全部素材，同一份请求在两条链路上会得到不同的
// ref_images，进而算出不同的冻结张数。
func countImageRefs(refs []RefSummary) int {
	n := 0
	for _, r := range refs {
		if r.Kind == "image" {
			n++
		}
	}
	return n
}

func countRefs(u *billing.Usage, refs []RefSummary) {
	for _, r := range refs {
		switch r.Kind {
		case "image":
			u.RefImages++
		case "video":
			u.RefVideos++
		case "audio":
			u.RefAudios++
		case "frame":
			// 首尾帧是视频自身的首/末帧，上游已含在"档位×秒数"的价里，不单收参考图费；
			// 计成 image 就是每单多加 N 张 ref_image_price（图生视频普遍带首帧 → 普遍多收）。
		}
	}
}

// snapshotOf 解析任务落库的 pricing_snapshot。
func snapshotOf(t *model.Task) (billing.Snapshot, error) {
	var s billing.Snapshot
	if err := json.Unmarshal(t.PricingSnapshot, &s); err != nil {
		return s, fmt.Errorf("task: 读取 pricing_snapshot 失败: %w", err)
	}
	return s, nil
}

// parseParams 容错解析 request_params（非法按零值）。
func parseParams(raw model.JSON) params {
	var p params
	_ = json.Unmarshal(raw, &p)
	return p
}

// mustJSON 序列化 JSONB 列（结构固定，序列化不会失败）。
func mustJSON(v any) model.JSON {
	b, err := json.Marshal(v)
	if err != nil {
		return model.JSON("{}")
	}
	return model.JSON(b)
}

// parseSeconds 解析时长（seconds 或厂商原生的 duration），饱和到 [-1, MaxVideoSeconds]。
//
// 返回三态，缺一不可：
//   - present=false：字段缺失 / null —— **不是错误**（图像、音乐没有秒数概念）；
//   - ok=false：字段存在但类型/格式非法（"abc" / 1.5 / 对象 / -2）—— 必须 400，
//     把它当"未传"会让一次明确写错的请求静默按 0 秒冻结，等于白送一次生成；
//   - present && ok：n ∈ {-1} ∪ [0, MaxVideoSeconds]。
//
// **-1 必须原样保留**：它是「智能时长」（Ark 的 duration=-1，视频编辑任务强制要求）。
// 历史实现把所有负数钳成 0，后果是那个能力在本平台根本调不起来 —— 0 秒会被
// validateVideoModality 判「必须指定 seconds」而 400，用户永远看不到真因。
// -1 以外的负数仍然是格式非法：它们没有任何上游语义。
func parseSeconds(raw json.RawMessage) (n int64, present bool, ok bool) {
	s := strings.TrimSpace(string(raw))
	if len(raw) == 0 || s == "" || s == "null" {
		return 0, false, true
	}
	var i int64
	if err := json.Unmarshal(raw, &i); err != nil {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return 0, true, false
		}
		v, perr := strconv.ParseInt(strings.TrimSpace(str), 10, 64)
		if perr != nil {
			return 0, true, false
		}
		i = v
	}
	if i < 0 {
		if i == -1 {
			return -1, true, true
		}
		return 0, true, false
	}
	if i > billing.MaxVideoSeconds {
		i = billing.MaxVideoSeconds
	}
	return i, true, true
}

// parseLooseInt 宽松解析一个可选的整数字段：缺失 / null / 类型不符一律返回 0。
//
// 只用于「各模态只认其中一部分」的参数（n / 张数之类）：在这里 400 会挡住
// 那些压根不用该字段的模态，改成在 validateModality 按模态精确校验。
func parseLooseInt(raw json.RawMessage) int64 {
	s := strings.TrimSpace(string(raw))
	if len(raw) == 0 || s == "" || s == "null" {
		return 0
	}
	var i int64
	if err := json.Unmarshal(raw, &i); err == nil {
		return i
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		if v, perr := strconv.ParseInt(strings.TrimSpace(str), 10, 64); perr == nil {
			return v
		}
	}
	return 0
}

// firstNonEmpty 返回第一个 trim 后非空的参数（保持顺序语义，不做任何归一）。
func firstNonEmpty(items ...string) string {
	for _, v := range items {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// normalizeVideoResolution 把请求 size/resolution 归一化为视频档位键：
// 显式档位原样小写；"WxH" 按短边映射（≤480→480p、≤720→720p、其余→1080p）；无法识别则原样返回（由计价拒绝）。
func normalizeVideoResolution(v string) string {
	s := strings.ToLower(strings.TrimSpace(v))
	if s == "" {
		return ""
	}
	switch s {
	case "480p", "720p", "1080p":
		return s
	case "768p":
		// 与 adapters.autodl.tierOf 逐字一致：768p 是上游档位名，吸附到平台 720p 档。
		// 两处不同步会出现「按 720p 冻结、按 768p 生成」或反向的错账。
		return "720p"
	}
	if i := strings.IndexAny(s, "x*×"); i > 0 {
		// 分隔符按 rune 宽度前移：× 是 3 字节，写 i+1 会切在字符中间，
		// 导致 "720×1280"（中文文档常见写法）永远识别不了。
		_, size := utf8.DecodeRuneInString(s[i:])
		if size > 0 && i+size < len(s) {
			w, e1 := strconv.Atoi(strings.TrimSpace(s[:i]))
			h, e2 := strconv.Atoi(strings.TrimSpace(s[i+size:]))
			if e1 == nil && e2 == nil && w > 0 && h > 0 {
				short := w
				if h < short {
					short = h
				}
				switch {
				case short <= 480:
					return "480p"
				case short <= 720:
					return "720p"
				default:
					return "1080p"
				}
			}
		}
	}
	return s
}

// refKind 判定素材类别与链接（显式 type 优先，否则按已填链接字段推断）。
//
// kind 是**计费类别**，只有 image / video / audio / frame 四种：
// frame 是首尾帧（视频自己的第一帧/最后一帧），它与「参考图」在计费上不是一回事——
// 上游把首尾帧算作生成输入的一部分（已含在档位×秒数的价里），
// 把它计成参考图就是每单多加 N 张 ref_image_price。所以它必须是独立类别，
// 而不是 image 的一个变体（变体在 usageOf 的 switch 里会被静默归进 image）。
func refKind(typ, image, audio, video string) (string, string) {
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "image":
		return "image", image
	case "audio":
		return "audio", audio
	case "video":
		return "video", video
	case "first_frame", "first-frame":
		return "frame", image
	case "last_frame", "last-frame":
		return "frame", image
	}
	if strings.TrimSpace(image) != "" {
		return "image", image
	}
	if strings.TrimSpace(audio) != "" {
		return "audio", audio
	}
	if strings.TrimSpace(video) != "" {
		return "video", video
	}
	return "", ""
}

// refDigest 返回素材摘要 sha256：data: URL 取解码内容，其余取 URL 文本（素材本体落地在 S8 补全）。
func refDigest(link string) string {
	h := sha256.New()
	if b, ok := decodeDataURL(link); ok {
		h.Write(b)
	} else {
		h.Write([]byte(link))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// refSize 返回素材字节数：data: URL 取解码长度，其余未下载按 0。
func refSize(link string) int64 {
	if b, ok := decodeDataURL(link); ok {
		return int64(len(b))
	}
	return 0
}

// decodeDataURL 解码 data: URL 负载（非 data: 或解码失败返回 false）。
func decodeDataURL(link string) ([]byte, bool) {
	if !strings.HasPrefix(link, "data:") {
		return nil, false
	}
	i := strings.Index(link, ",")
	if i < 0 {
		return nil, false
	}
	payload := link[i+1:]
	if strings.Contains(link[:i], ";base64") {
		b, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			b, err = base64.RawURLEncoding.DecodeString(payload)
			if err != nil {
				return nil, false
			}
		}
		return b, true
	}
	return []byte(payload), true
}

// normalizePage 归一化分页参数（T21：page ≥ 1、page_size ≤ 100）。
func normalizePage(page, size int) (int, int) {
	if page < pagination.DefaultPage {
		page = pagination.DefaultPage
	}
	if size < 1 {
		size = pagination.DefaultPageSize
	}
	if size > pagination.MaxPageSize {
		size = pagination.MaxPageSize
	}
	return page, size
}

// truncate 按字符数截断（统一收敛到 textx，避免与 billing/relay 各写一份）。
func truncate(s string, n int) string {
	return textx.TruncateRunes(s, n)
}

// sanitizeReason 是任务域**所有落库错误文案的唯一出口**（B7 §2.10）：先脱敏，再截断。
//
// 顺序不可颠倒：先截断后脱敏会把「被切断一半的密钥」留在库里 —— 截断点落在 token 中间时
// 正则匹配不到完整形态，脱敏直接失效。先脱敏则截断只可能切断 `<redacted-key>` 占位符。
//
// 这里只做通用层（Bearer / sk-xxx / ak-xxx）。能拿到渠道与密钥的调用点（提交循环、
// 轮询）必须额外用 relay.RedactUpstream(msg, ch, key) 把上游主机名一起抹掉 ——
// 那是更精确的一层，不能靠这里的兜底代替。
func sanitizeReason(reason string) string {
	out := truncate(relay.RedactUpstream(reason, nil, ""), 500)
	// 截断点可能正好落在占位符中间，留下 `<reda` 这种残片：不是泄漏，但看起来像数据损坏，
	// 而且会让人误以为脱敏没生效。只裁掉**末尾**不完整的占位符前缀，中间完整的绝不动。
	for _, ph := range redactPlaceholders {
		for n := len(ph) - 1; n > 0; n-- {
			if strings.HasSuffix(out, ph[:n]) {
				return strings.TrimSuffix(out, ph[:n])
			}
		}
	}
	return out
}

// redactPlaceholders 是 relay 侧脱敏会写入的占位符。
//
// 直接引用 relay 的导出常量，不再本地抄一份字面量：抄错一个字（曾写成
// `<upstream-host>`）不会报错，只会让末尾残片裁剪静默失效。
var redactPlaceholders = []string{relay.RedactedKeyPlaceholder, relay.RedactedHostPlaceholder}

// contains 判断字符串是否在列表内。
func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// imageGroupParam 返回请求体里第一个「会改变图片产出张数」的组图/图层参数键。
//
// 这几个键的语义是「一次调用出 N 张」（sequential 最多 15、图层拆分 1+16），
// 而异步链路的冻结只认 n。判据必须是键本身而不是解析后的值：
// 只要出现就说明调用方期待多产物，与 params.ImageN 的口径已经不一致。
