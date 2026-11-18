package spec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/pkg/netfetch"
	"lingdong-api/internal/pkg/syncwait"
	"lingdong-api/internal/pkg/taskstate"
	"lingdong-api/internal/relay"
)

// 本文件实现规格 DSL 的**同步桥接**（AsyncSpec.Mode = bridge）。
//
// 解决什么问题：一批厂商（自建 TTS、生图服务、部分国产视频）只有「提交 + 查询」
// 两个端点，而平台对外的合同（如 /v1/audio/speech）是同步的。改造前这类厂商
// 只能写内置适配器（改代码 + 发版），规格 DSL 完全用不上——这就是"AI 无法完成
// 非标 API 接入"的结构性原因。
//
// 三条不可放宽的语义边界：
//  1. **一次请求内等完，且有硬上限**：到点必须放弃，返回 relay.SyncWaitTimeoutError
//     （网关据此 504 + 全额释放，且不换渠道重试——换渠道等于为一个注定超时的请求
//     再付一次上游成本）；
//  2. **产物下载后作为响应体返回**，不落平台产物表：同步端点的语义就是"这次响应里
//     给你东西"，落库/转存是异步任务链路的职责，两者不能混；
//  3. **状态机复用 a.poll**：桥接与异步任务必须同一套 status_map / extract 语义，
//     否则同一个上游状态在两条链路上会被判成不同结果（典型后果：桥接把失败当成功、
//     给用户返回 0 字节的音频并照常扣费）。

const (
	// bridgeArtifactTimeout 是单个产物下载的超时（几十 MB 内的音频足够）。
	bridgeArtifactTimeout = 60 * time.Second
	// bridgeMaxRedirects 是产物下载允许的跳转次数：对象存储普遍 302 到 CDN，
	// 但无限跳转会被恶意上游用来把请求引到任意地址。
	bridgeMaxRedirects = 3
	// bridgePollErrMax 是「查询本身连续失败」的容忍次数。
	// 单次查询超时/5xx 不能立刻判死：上游任务还在跑，判死等于丢掉已经付过钱的产物。
	bridgePollErrMax = 5
	// bridgeMaxInterval 是轮询间隔的递增上限（1s → 2s → … → 5s）。
	//
	// 不接受"运营把 interval_ms 填 200ms、timeout 填 900s"这种组合：
	// 那会打出 4500 次查询，把上游按 QPS 限流的账号自己打限。
	// 递增只影响长任务，秒回的小任务仍在第一个周期就抓到结果。
	bridgeMaxInterval = 5 * time.Second
)

// bridge 执行一次同步桥接：提交 → 轮询到终态 → 按端点契约交付产物。
func (a *Adapter) bridge(ctx context.Context, cfg *relay.ChannelConfig, ep Endpoint, key string,
	req *relay.RelayRequest, src map[string]any, pv pathVals) (*relay.RawResponse, error) {

	if _, ok := a.byKey[ep.Async.Poll]; !ok {
		return nil, &relay.ConfigError{
			Scope: "渠道规格配置错误", Field: "endpoints." + key + ".async.poll",
			Msg: fmt.Sprintf("规格 %s 的桥接端点 %s 声明了轮询端点 %q，但它不存在",
				a.spec.Slug, key, ep.Async.Poll),
		}
	}

	body, err := a.buildBody(cfg, ep, req, src)
	if err != nil {
		return nil, err
	}
	submitRaw, err := a.doJSON(ctx, cfg, ep, key, body, pv)
	if err != nil {
		// 提交阶段的网络/5xx 错误意味着**上游可能已经受理**：换渠道重试会再付
		// 一次上游成本，而台账上看不出来。typing 成 AmbiguousSubmitError 后
		// relay 会 502 且不换渠道，并打 submit_ambiguous 供对账。
		if isAmbiguousSubmit(ctx, err) {
			return nil, &relay.AmbiguousSubmitError{Err: err}
		}
		return nil, err
	}
	taskID := strings.TrimSpace(firstString(submitRaw, ep.Extract.TaskID))
	if taskID == "" {
		return nil, &relay.ConfigError{
			Scope: "渠道规格配置错误", Field: "endpoints." + key + ".extract.task_id",
			Msg: fmt.Sprintf("提交响应里没有任务号（取值路径 %s），无法继续等待结果", ep.Extract.TaskID),
		}
	}

	status, err := a.bridgeWait(ctx, cfg, key, taskID, ep.Async)
	if err != nil {
		return nil, err
	}

	// 产物链接：poll 端点的 extract 已经把它们归一进 ResultURLs（含单产物 result）。
	if len(status.ResultURLs) == 0 {
		return nil, &relay.ConfigError{
			Scope: "渠道规格配置错误", Field: "endpoints." + ep.Async.Poll + ".extract",
			Msg: "上游任务已完成但没有返回任何产物链接，无法生成同步响应",
		}
	}
	dur := int64(status.Usage.AudioSeconds)
	// 交付形状必须与端点契约一致（唯一判据 apidoc.SpecEndpoint.Artifact）：
	// 二进制端点回字节；图像生成端点回 OpenAI JSON，交给 relay 的直链改写落地。
	if fact, ok := apidoc.SpecEndpointByKey(key); ok && fact.Artifact() == apidoc.ArtifactImageJSON {
		return &relay.RawResponse{
			StatusCode:   http.StatusOK,
			ContentType:  "application/json",
			Body:         imageJSON(status.ResultURLs),
			DurationSecs: dur,
		}, nil
	}
	artifact, ctype, err := a.fetchArtifact(ctx, cfg, status.ResultURLs[0], ep.ResponseType)
	if err != nil {
		return nil, err
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	return &relay.RawResponse{
		StatusCode:   http.StatusOK,
		ContentType:  ctype,
		Body:         artifact,
		DurationSecs: dur,
	}, nil
}

// isAmbiguousSubmit 判断「提交失败」是否可能已被上游受理。
//
// 判据只有三类：
//  1. 上游回了 5xx（它自己出错，但任务可能已经建了）；
//  2. 传输层失败（连接重置/读超时）→ 没有拿到任何响应；
//  3. 被排除的：客户端主动断连（ctx 已取消，不是我们的锅）、
//     调用方参数错 / 配置错 / 体过大（都没让上游开工）。
func isAmbiguousSubmit(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var ue *upstreamError
	if errors.As(err, &ue) {
		return ue.status >= 500
	}
	var ce *relay.ConfigError
	var cp *relay.CallerParamError
	if errors.As(err, &ce) || errors.As(err, &cp) {
		return false
	}
	if errors.Is(err, relay.ErrUpstreamBodyTooLarge) {
		return false
	}
	return true
}

// imageJSON 把产物链接折成 OpenAI 图像契约。
// 只回链接不回字节：上游的对象存储直链有效期很短，而 relay 出口的
// rewriteMediaURLs 会把 data[].url 拉回站内直链（那是唯一改写点）。
// 在这里自己下载再编码成 base64 等于绕开那条管线。
func imageJSON(urls []string) []byte {
	data := make([]map[string]string, 0, len(urls))
	for _, u := range urls {
		data = append(data, map[string]string{"url": u})
	}
	out, err := json.Marshal(map[string]any{
		"created": time.Now().Unix(),
		"data":    data,
	})
	if err != nil {
		return []byte(`{"data":[]}`)
	}
	return out
}

// bridgeUnknownStatusMax 是「上游状态连续几次不认识就判配置错」的阈值。
//
// 3 次（约 1s→2s→5s，合计 ~8 秒）足够排除抖动；不设阈值时，一个拼错的
// status_map 会让请求一直等到 timeout_secs（默认 120s，最长 900s），
// 而上游任务早就在跑并计费了。
const bridgeUnknownStatusMax = 3

// bridgeWait 在等待上限内反复查询，直到上游给出终态。
//
// 循环本身来自 pkg/syncwait（与内置同步适配器同一份实现）：等待语义只有一份，
// 超时/放弃/连续失败三种处置才不会在两条链路上分叉。
func (a *Adapter) bridgeWait(ctx context.Context, cfg *relay.ChannelConfig, submitKey, taskID string,
	async *AsyncSpec) (*relay.TaskStatus, error) {

	// poll 的成功结果要连产物链接一起带回，所以在这里闭包捕获最近一次快照。
	var last *relay.TaskStatus
	unknown := 0
	st, err := syncwait.Wait(ctx, syncwait.Options{
		Wait:         time.Duration(asyncTimeoutSecs(async)) * time.Second,
		Interval:     time.Duration(asyncIntervalMs(async)) * time.Millisecond,
		MaxInterval:  bridgeMaxInterval,
		ErrStreakMax: bridgePollErrMax,
	}, syncwait.Hooks{
		Timeout: func(waited time.Duration, id string) error {
			return &relay.SyncWaitTimeoutError{Waited: waited, Detail: "上游任务号 " + id, TaskID: id}
		},
		Abandoned: func(waited, limit time.Duration, id string, cause error) error {
			return &relay.SyncAbandonedError{TaskID: id, Waited: waited, WaitLimit: limit, Err: cause}
		},
	}, taskID, func(ctx context.Context) (syncwait.State, error) {
		cur, perr := a.poll(ctx, cfg, submitKey, taskID)
		if perr != nil {
			return syncwait.State{}, perr
		}
		last = cur
		// 状态词不认识是**确定性配置错**：不计数重试，直接喊停（syncwait.ErrTerminal）。
		// 报错必须带上游原文，否则运营不知道该往 status_map 里加什么。
		if !cur.StatusMatched {
			unknown++
			if unknown >= bridgeUnknownStatusMax {
				return syncwait.State{}, fmt.Errorf("%w: %w", syncwait.ErrTerminal, &relay.ConfigError{
					Scope: "渠道规格配置错误",
					Field: "endpoints." + PollKey(submitKey) + ".status_map",
					Msg: fmt.Sprintf("上游状态 %q 连续 %d 次未识别，请把它加进 status_map（succeeded/failed/queued/running/canceled）",
						cur.RawStatus, unknown),
				})
			}
		} else {
			unknown = 0
		}
		// canceled 必须折成终态失败：syncwait 只认 succeeded/failed，
		// 原样返回 "canceled" 会被当成"仍在运行"，一直等到超时。
		if cur.Status == taskstate.Canceled {
			return syncwait.State{Status: taskstate.Failed, Detail: "上游任务已取消"}, nil
		}
		return syncwait.State{Status: cur.Status, Detail: cur.Error}, nil
	})
	// 连续查询失败是**上游故障**（不是配置错误）：按普通错误返回，走 502 并记渠道健康度。
	if err != nil {
		// ErrTerminal 包着的配置错要先取出来：它被 syncwait 原样上抛，
		// 不能落到下面的 StreakError 分支被误报成"连续失败"。
		var ce *relay.ConfigError
		if errors.As(err, &ce) {
			return nil, ce
		}
		var streak *syncwait.StreakError
		if errors.As(err, &streak) {
			return nil, fmt.Errorf("渠道 %s 的轮询端点连续失败：%w", cfg.Name, err)
		}
		return nil, err
	}
	if st.Status == taskstate.Failed {
		return nil, relay.TaskFailure(st.Detail, taskID)
	}
	if last == nil {
		// syncwait 只在有终态时才返回，这里只是防御：没有快照就没有产物链接。
		return nil, &relay.ConfigError{
			Scope: "渠道规格配置错误", Field: "endpoints." + PollKey(submitKey),
			Msg: "轮询返回了成功但没有任何任务状态，无法取回产物",
		}
	}
	return last, nil
}

// fetchArtifact 取回产物字节（含 data: 内联形态）。
//
// 下载与解码都走 pkg/netfetch：里面写死了出站 netguard 与体积上限，
// 漏掉任何一条都是事故（SSRF / OOM / 把截断的音频当成完整产物计费）。
func (a *Adapter) fetchArtifact(ctx context.Context, cfg *relay.ChannelConfig, url, fallbackType string) ([]byte, string, error) {
	raw := strings.TrimSpace(url)
	if raw == "" {
		return nil, "", &relay.ConfigError{Scope: "渠道规格配置错误", Field: "extract", Msg: "产物链接为空"}
	}
	if strings.HasPrefix(raw, "data:") {
		buf, ctype, err := netfetch.DecodeDataURI(raw, fallbackType, BridgeMaxArtifactBytes)
		if errors.Is(err, netfetch.ErrTooLarge) {
			return nil, "", relay.ErrUpstreamBodyTooLarge
		}
		if err != nil {
			return nil, "", &relay.ConfigError{Scope: "渠道规格配置错误", Field: "extract", Msg: err.Error()}
		}
		return buf, ctype, nil
	}
	buf, ctype, err := netfetch.Get(ctx, raw, netfetch.Options{
		Timeout:      bridgeArtifactTimeout,
		MaxRedirects: bridgeMaxRedirects,
		MaxBytes:     BridgeMaxArtifactBytes,
	})
	if errors.Is(err, netfetch.ErrTooLarge) {
		return nil, "", relay.ErrUpstreamBodyTooLarge
	}
	if err != nil {
		return nil, "", err
	}
	if ctype == "" {
		ctype = strings.TrimSpace(fallbackType)
	}
	return buf, ctype, nil
}

// asyncTimeoutSecs 返回桥接的等待上限（未声明时用默认值）。
func asyncTimeoutSecs(a *AsyncSpec) int {
	if a != nil && a.TimeoutSecs > 0 {
		return a.TimeoutSecs
	}
	return DefaultBridgeTimeoutSecs
}

// asyncIntervalMs 返回桥接的轮询间隔（未声明时用默认值）。
func asyncIntervalMs(a *AsyncSpec) int {
	if a != nil && a.IntervalMs > 0 {
		return a.IntervalMs
	}
	return DefaultBridgeIntervalMs
}
