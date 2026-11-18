package adapters

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"lingdong-api/internal/pkg/netfetch"
	"lingdong-api/internal/pkg/syncwait"
	"lingdong-api/internal/pkg/taskstate"
	"lingdong-api/internal/relay"
	"lingdong-api/internal/service/billing"
)

// 本文件实现多米API 的**同步图像桥接**（OpenAI 原生 /v1/images/generations 与 /v1/images/edits）。
//
// 为什么必须有这一层：
//   - 多米的图像接口**只有异步**（官方文档：「暂时仅支持异步」）。实测即使不带 ?async=true，
//     POST /v1/images/generations 也是 1 秒返回 {"id":"..."} 的任务号，不会同步出图；
//   - 而 OpenAI 官方 SDK 与绝大多数第三方客户端（LobeChat / Cherry Studio / NextChat …）
//     调用的是同步端点，期待响应里直接带 url 或 b64_json。
//
// 改造前的表现是「渠道 N（duomiapi）不支持端点 images/generations」：适配器把 SupportsEndpoint
// 全关（防止 chat 请求被误派），连带把图像同步端点也关死了，于是所有图像模型都只能走平台私有的
// /v1/images/generations/async，标准客户端一个都用不了。
//
// 桥接的语义边界（三条，改动前先看）：
//  1. **同步返回上游产物链接，不落平台产物表**：OpenAI 契约里 url 本来就是临时链接，
//     调用方应当立即下载。落库/转存是异步任务链路的职责，两者不混。
//  2. **等待有硬上限**（默认 600s，对齐 OpenAI SDK 的默认 timeout）：到点必须放弃并
//     全额释放预扣，不能让一个 HTTP 请求无限挂着。超时返回 504 而不是 500 —— 客户端能区分
//     「上游太慢」和「我们内部炸了」。
//  3. **失败归因要分档**：内容审核拒绝是调用方的锅（400，不记渠道健康度），
//     上游内部错误才记渠道（502）。混在一起会让一条健康渠道被违规 prompt 熔断。

const (
	// duomiSyncImageDefaultWait 是同步等待的默认上限。
	//
	// 取值依据（全部是实测值，不是拍脑袋）：
	//   - nano-banana 文生图 296s、gemini 图像 329s；
	//   - gpt-image 单张 **540s**，且这是"顺利"的一次，排队高峰会更久。
	//
	// 取 780s 是给 gpt-image 留 1.44 倍余量。**必须小于 nginx 的 proxy_read_timeout
	// （deploy/nginx.conf 里是 960s）**：代理先断的话，应用这边即使算出结果也写不出去，
	// 客户端只会拿到 nginx 的 504 HTML。
	//
	// 之前取 600s 且与 nginx 完全相等，是「上游成功、平台也结算了、用户却 504」的直接原因：
	// 600s 恰好是 gpt-image 的实测耗时，随便一点抖动就同时越过两条线。
	duomiSyncImageDefaultWait = 780 * time.Second
	// duomiSyncImageMinWait 是环境变量覆盖的合法下限（仅用于挡掉 0 与负数）。
	//
	// 不把下限设高：压测/应急时运维确实会把等待压到秒级（单测也是这么用的），
	// 设小了只会误杀自己，不产生资源泄漏。真正危险的是**上限**。
	duomiSyncImageMinWait = 1 * time.Second
	// duomiSyncImageMaxWait 是环境变量覆盖的合法上限。
	//
	// **必须小于 nginx 的 960s**：写成 1800s 只会让 nginx 先断，
	// 用户拿到 504 HTML 而应用还在傻等 —— 调大之前必须先调大代理。
	duomiSyncImageMaxWait = 900 * time.Second
	// duomiSyncImagePollMin/Max 是轮询间隔的起止值（指数退避）。
	//
	// 起始 1s 是为了抓住「秒回」的小图，退到 5s 是为了不让一个 10 分钟的任务
	// 打出上百次查询（上游按 QPS 限流的账号会被自己的轮询打限）。
	duomiSyncImagePollMin = 1 * time.Second
	duomiSyncImagePollMax = 5 * time.Second
	// duomiSyncImagePollErrMax 是「查询本身连续失败」的容忍次数。
	//
	// 单次查询超时/5xx 不能立刻判死：上游任务还在跑，判死等于把已付费的图丢掉。
	// 连续失败到这个次数才认定查询链路真的坏了。
	duomiSyncImagePollErrMax = 5
	// duomiSyncImageMaxDownload 是 b64_json 回填时的下载上限（防止上游给超大文件打爆内存）。
	duomiSyncImageMaxDownload = 32 << 20
	// duomiSyncDownloadTimeout 是单次产物下载的超时。
	//
	// 与任务等待上限分开：下载卡住不该继续占着任务等待的预算，
	// 30s 足够几十 MB 的图，超了就是 CDN 那边出了问题。
	duomiSyncDownloadTimeout = 30 * time.Second
)

// 同步桥的请求体视图**就是 duomiBody**（与异步链路同一份声明）。
//
// 历史：这里曾有一个只声明 8 个字段的 duomiSyncImageRequest，于是同一个字段
// 在同步链和异步链各解释一次，且同步链认不出 mask_url / output_format 之类的键 ——
// 用户传了蒙版，上游收到的是一次没有蒙版的生成请求，账单上看不出任何异常。

// NeedsPublicURL 声明 edits 端点必须先做「文件 → 公网直链」转换。
//
// generations 是纯 JSON（image 直接给链接），不需要转换。
func (DuoMi) NeedsPublicURL(kind relay.EndpointKind) bool {
	return kind == relay.KindImagesEdits
}

// duomiForwardImage 是 generations / edits 的共同桥接入口。
//
// edit=true 表示这是 /v1/images/edits（必须有参考图，走图生图/编辑子协议）。
func duomiForwardImage(ctx context.Context, cfg *relay.ChannelConfig, req *relay.RelayRequest, edit bool) (*relay.RawResponse, error) {
	if cfg == nil {
		return nil, duomiCfgErr("channel", "渠道配置为空")
	}
	// 配置脏了必须在组装请求之前失败：按默认配置出网等于用错误的路径/鉴权跑生产。
	if cfg.OptionsErr != nil {
		return nil, cfg.OptionsErr
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, duomiCfgErr("keys", "渠道未配置密钥")
	}
	opts, err := parseDuomiOptions(cfg.Options)
	if err != nil {
		return nil, err
	}

	var in duomiBody
	if err := json.Unmarshal(req.Body, &in); err != nil {
		return nil, &relay.CallerParamError{Msg: "请求体不是合法的 JSON 对象"}
	}
	profile, modelID, err := duomiResolveProfile(opts, req.ModelID, in.Model)
	if err != nil {
		return nil, err
	}
	upstream := strings.TrimSpace(req.Model)
	if upstream == "" {
		upstream = profile.Upstream
	}
	// 同步桥以前没有这道判空（异步有）：空上游名会被原样写进 payload["model"]，
	// 上游按非法模型拒绝 → 400 被归因成渠道故障 → 一条健康渠道被熔断。
	// 与异步同口径，且文案指向绑定页（上游真名的唯一来源）。
	if upstream == "" {
		return nil, duomiCfgErr("upstream_model", fmt.Sprintf(
			"模型 %s 未配置上游模型名：请在「模型 → 绑定」里填写 upstream_model（绑定优先于内置档案）", modelID))
	}

	body, berr := duomiBuildSyncImageBody(&in, profile, upstream, edit)
	if berr != nil {
		return nil, berr
	}
	// 尺寸只认网关注入的归一结果（`imageSizeFor` 对图片端点恒非 nil）。
	// 适配器不读 in 里的尺寸：读一次就是第二份真相源。
	var im billing.ImageSize
	if req.ImageSize != nil {
		im = *req.ImageSize
	}
	// req.Body 是客户端原文：未在本适配器映射里出现的顶层键要原样补进上游报文。
	body.raw = req.Body
	taskID, subKind, serr := submitDuomiImage(ctx, cfg, opts, profile, upstream, body, im)
	if serr != nil {
		return nil, serr
	}
	status, werr := duomiWaitImageTask(ctx, cfg, opts, subKind, taskID)
	if werr != nil {
		return nil, werr
	}
	return duomiOpenAIImageResponse(ctx, status, in.ResponseFormat, modelID)
}

// duomiBuildSyncImageBody 把 OpenAI 请求映射成上游子协议的入参。
//
// 映射规则按**协议**分，不按模型名硬编码：新增型号只要在 model_overrides 里登记协议即可。
//
// 只写「本协议真正要发给上游」的键；没写到的顶层键由调用方用 relay.ForwardUnmapped
// 原样补上（见 submitDuomiGPTImage / submitDuomiNano）。
func duomiBuildSyncImageBody(in *duomiBody, profile duomiProfile,
	upstream string, edit bool) (*duomiBody, error) {
	// 只有图像协议能走这两个端点。视频/音乐模型被 /v1/images/generations 命中时，
	// 报「模型不是图像模型」比让它打到上游再 404 更容易定位。
	switch profile.Protocol {
	case duomiProtoNano, duomiProtoGPTImage:
	default:
		return nil, &relay.CallerParamError{Msg: fmt.Sprintf(
			"模型 %s 不是图像模型（协议 %s），不能通过 /v1/images/* 调用", upstream, profile.Protocol)}
	}

	// 尺寸不在这里搬运：网关已经把 `size` / `image_size` / `resolution` / `aspect_ratio`
	// 统一解析成 `RelayRequest.ImageSize` 并直接传给提交层。本函数再解一次就是第二份真相源
	// （改造前它只搬 `size`，于是客户端按官方字段名传 `image_size` 时「计费认、出站不认」）。
	//
	// n 也不再本地判死：一次能出几张由上游决定（上游不支持就由它自己报错），
	// 平台侧只保证计费按 n 冻结、按实际交付结算（见 billing.ImageCountHint / ResolveImageCount）。
	body := &duomiBody{
		Model:          upstream,
		Prompt:         in.Prompt,
		N:              in.N,
		MaskURL:        strings.TrimSpace(in.MaskURL),
		Quality:        strings.TrimSpace(in.Quality),
		Background:     strings.TrimSpace(in.Background),
		ResponseFormat: in.ResponseFormat,
	}

	switch profile.Protocol {
	case duomiProtoGPTImage:
		if hasSyncImage(in.Image) {
			// 与异步链路（duomiImageField）同口径：data: 内联 / 相对路径 /
			// 非 http(s) 一律拒绝并引导先 /v1/files 上传。原实现原样下发，
			// 上游返回的错误不可归因（P6，与 nano 系 503-505 行为对齐）。
			if _, ierr := duomiImageField(in.Image); ierr != nil {
				return nil, ierr
			}
			body.Image = in.Image
		}
	default: // nano
		// nano 的编辑端点只认 image_urls 数组；**有没有图决定走哪个端点**。
		// quality / background / mask 是 gpt-image 的字段，nano 认不认由上游回答
		// （它们没有被本函数写进 payload，因此会走 ForwardUnmapped 原样下发）。
		urls, ierr := duomiSyncImageURLs(in.Image)
		if ierr != nil {
			return nil, ierr
		}
		body.ImageURLs = urls
	}

	// `/v1/images/edits` 的语义就是「改图」：没有参考图时不能悄悄退化成文生图，
	// 否则调用方会拿到一张与输入无关的新图，还以为编辑生效了。
	if edit && !syncBodyHasRef(body) {
		return nil, &relay.CallerParamError{Msg: "该端点必须提供参考图（image / 文件上传）"}
	}
	return body, nil
}

// hasSyncImage 判断 OpenAI 的 image 字段是否有值（缺失 / null / 空串 / 空数组都算没有）。
func hasSyncImage(raw json.RawMessage) bool {
	switch s := strings.TrimSpace(string(raw)); s {
	case "", "null", `""`, "[]":
		return false
	}
	return true
}

// syncBodyHasRef 判断映射后的上游体里是否真的有参考图。
func syncBodyHasRef(body *duomiBody) bool {
	return len(body.ImageURLs) > 0 || hasSyncImage(body.Image)
}

// duomiSyncImageURLs 把 OpenAI 的 image 字段（字符串 / 数组 / 单元素数组）取成直链列表。
func duomiSyncImageURLs(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		if strings.TrimSpace(one) == "" {
			return nil, nil
		}
		u, err := duomiPublicURL(one, "image")
		if err != nil {
			return nil, err
		}
		return []string{u}, nil
	}
	var arr []string
	if json.Unmarshal(raw, &arr) == nil {
		return duomiRefImageURLs(arr, duomiMaxNanoRefImages, "image")
	}
	return nil, &relay.CallerParamError{Msg: "image 必须是字符串或字符串数组（公网直链）"}
}

// duomiWaitImageTask 轮询图像任务到终态。
//
// 返回 error 的三种来源必须分清（下游按类型决定 HTTP 状态与渠道归因）：
//   - ctx 取消 → 直接用 ctx.Err()，dispatch 会按「客户端断开」处理（499）；
//   - 等待超时 → ErrDuomiSyncTimeout（504 + 全额释放）；
//   - 上游任务失败 → 审核类给 CallerParamError（400），其余给普通错误（502）。
func duomiWaitImageTask(ctx context.Context, cfg *relay.ChannelConfig, opts *duomiOptions,
	subKind, id string) (*relay.TaskStatus, error) {
	path, parse := duomiPollTarget(opts, subKind, id)

	// 循环来自 pkg/syncwait（与规格 bridge 同一份实现）：
	// 截止时刻只算一次、到点放弃、连续查询失败不判死 —— 这些语义写成两份就会分叉。
	var last *relay.TaskStatus
	st, err := syncwait.Wait(ctx, syncwait.Options{
		Wait:         duomiSyncImageWait(),
		Interval:     duomiSyncImagePollMin,
		MaxInterval:  duomiSyncImagePollMax,
		ErrStreakMax: duomiSyncImagePollErrMax,
	}, syncwait.Hooks{
		Timeout: func(waited time.Duration, taskID string) error {
			return &relay.SyncWaitTimeoutError{Waited: waited, Detail: "上游任务号 " + taskID, TaskID: taskID}
		},
		Abandoned: func(waited, limit time.Duration, taskID string, cause error) error {
			return &relay.SyncAbandonedError{TaskID: taskID, Waited: waited, WaitLimit: limit, Err: cause}
		},
	}, id, func(ctx context.Context) (syncwait.State, error) {
		buf, derr := duomiDo(ctx, cfg, opts.authPrefix, http.MethodGet, path, nil)
		if derr != nil {
			return syncwait.State{}, derr
		}
		cur, perr := parse(buf)
		if perr != nil {
			// 解析失败按「查询失败」计数：上游在 200 里返回一段代理错误页是常见现象，
			// 立刻判死会把已经出好的图丢掉。
			return syncwait.State{}, fmt.Errorf("解析上游任务 %s 的状态失败：%w", id, perr)
		}
		last = cur
		return syncwait.State{Status: cur.Status, Detail: cur.Error}, nil
	})
	if err != nil {
		return nil, err
	}
	if st.Status == taskstate.Failed {
		return nil, relay.TaskFailure(st.Detail, id)
	}
	if last == nil {
		return nil, fmt.Errorf("上游任务 %s 轮询成功但缺少状态快照", id)
	}
	// 上游说成功但一个产物都没有 = 上游违约。判成功会让用户白付钱。
	if len(last.ResultURLs) == 0 && last.ResultText == "" {
		return nil, fmt.Errorf("上游任务已完成但没有返回任何产物（任务号 %s）", id)
	}
	return last, nil
}

// ErrDuomiSyncTimeout 是历史别名，实际使用 relay 包的类型。
//
// 保留别名是为了让已有调用点（含单测）读起来仍有「多米同步超时」的语义，
// 但**判定必须走 relay.SyncWaitTimeoutError**：dispatch 用 errors.As 认的是父类型。
type ErrDuomiSyncTimeout = relay.SyncWaitTimeoutError

// duomiSyncImageWait 返回同步等待上限（可由环境变量覆盖，便于压测/应急）。
//
// 只夹**上限**：运维把 86400 写进环境变量时，如果原样生效，一个请求会占住连接与冻结额度
// 一整天（而 nginx 早在 960s 就断了），表现得像"额度不释放"的服务端故障。
// 写小则不过问——压测时就是要秒级超时，替运维"纠正"反而让测试跑不起来。
func duomiSyncImageWait() time.Duration {
	raw := strings.TrimSpace(os.Getenv("LD_DUOMI_SYNC_IMAGE_WAIT_SEC"))
	if raw == "" {
		return duomiSyncImageDefaultWait
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return duomiSyncImageDefaultWait
	}
	d := time.Duration(n) * time.Second
	if d < duomiSyncImageMinWait {
		return duomiSyncImageMinWait
	}
	if d > duomiSyncImageMaxWait {
		return duomiSyncImageMaxWait
	}
	return d
}

// duomiOpenAIImageResponse 把任务产物折成 OpenAI 标准图像响应。
//
//	{"created": 1700000000, "data": [{"url": "..."}]}
//	{"created": 1700000000, "data": [{"b64_json": "..."}]}   // 仅 response_format=b64_json
func duomiOpenAIImageResponse(ctx context.Context, st *relay.TaskStatus,
	format, modelID string) (*relay.RawResponse, error) {
	wantB64 := strings.EqualFold(strings.TrimSpace(format), "b64_json")

	data := make([]map[string]any, 0, len(st.ResultURLs))
	for _, u := range st.ResultURLs {
		if wantB64 {
			b64, err := duomiDownloadB64(ctx, u)
			if err != nil {
				// 下载失败不能静默降级成 url：调用方按 b64_json 解析会拿到空图。
				return nil, err
			}
			data = append(data, map[string]any{"b64_json": b64})
			continue
		}
		data = append(data, map[string]any{"url": u})
	}
	payload := map[string]any{
		"created": time.Now().Unix(),
		"data":    data,
	}
	// 模型的响应里带上实际用到的模型名：多客户端（LobeChat 等）会读它做展示与后续编辑溯源。
	if strings.TrimSpace(modelID) != "" {
		payload["model"] = modelID
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("构造图像响应失败：%w", err)
	}
	return &relay.RawResponse{StatusCode: http.StatusOK, ContentType: "application/json", Body: buf}, nil
}

// duomiDownloadB64 拉取产物并编码成 base64。
//
// 下载走 pkg/netfetch：允许有限重定向（产物链接常经 CDN 302，用默认的 DenyRedirect
// 会拿到空体，用户端表现是「b64_json 解出来是 0 字节」），并带体积上限。
func duomiDownloadB64(ctx context.Context, url string) (string, error) {
	buf, _, err := netfetch.Get(ctx, url, netfetch.Options{
		Timeout:      duomiSyncDownloadTimeout,
		MaxRedirects: 3,
		MaxBytes:     duomiSyncImageMaxDownload,
	})
	if errors.Is(err, netfetch.ErrTooLarge) {
		return "", fmt.Errorf("产物超过 %d MB，无法按 b64_json 返回；请改用 response_format=url",
			duomiSyncImageMaxDownload>>20)
	}
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}
