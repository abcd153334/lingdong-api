package adapters

// 火山方舟图片链路（Seedream 系列）。
//
// 两条通道、共用同一份请求构造：先用 SDK 的 typed 结构构造请求体 + extra_body 兜底未知字段，
// 再**自己**发 HTTP（非流式用 arkDo、流式用 relay.UpstreamStreamClient()），响应一律原始字节透传。
//
// 为什么非流式也不让 SDK 发（这是本文件最重要的设计约束之一，事故换来的）：
//   1. SDK 只给 typed 出口（Client.Do 要求 model.Response，raw body 选项未导出），
//      要拿原始字节只能自己发。
//   2. 拿不到原始字节就会退化成「typed 解析 → json.Marshal 重序列化」，而这条路有两颗雷：
//      - SDK 的 decode 对未知字段 `d.Skip()`，上游新增的任何字段都会被静默丢掉；
//      - `Opt*` 类型的 MarshalJSON 是**值接收者**且零值一个字节都不写。把
//        `res.ImageGenerationResponse`（值拷贝，不可寻址）交给 encoding/json 时，
//        SDK 自己的指针接收者 MarshalJSON 不生效，退回逐字段反射 ⇒ 撞上空字节 ⇒
//        `unexpected end of JSON input`。
//      真实症状：上游 200 且已出图计费，平台却报「响应序列化失败」并把结果丢掉。
//
// 为什么流式不能用 SDK 的 GenerateImagesStreaming（另一条设计约束）：
//  1. SDK 的事件 type 是**严格枚举**，只建模了 3 个事件；官方 Python 示例已经在处理
//     第 4 个 `image_generation.partial_image`。SDK 遇到未知 type 会在 Unmarshal 阶段报错。
//  2. utils.ImageGenerationStreamReader 一旦解析失败就 `return nil, err` —— **整条流结束**。
//  3. 后果：上游新增一个事件 → 平台判「流式渠道失败」→ 已生成的图全部丢弃 + 全额退费，
//     而调用记录里只有一句 JSON 解析错误，没有任何线索指向真因。
//
// 自己读 SSE 还顺带拿到两个必需能力：帧级原样透传（未知事件不丢）、
// completed 帧原文可直接落 call_io（SDK 只给 typed 出口，拿不到字节）。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"lingdong-api/internal/relay"
	"lingdong-api/internal/service/billing"
)

// arkImageKnownKeys 是 SDK typed 结构已覆盖的键（**唯一清单**）。
//
// 这张表必须与 images.CreateImageGenerationRequest 的字段一一对应：
//   - 多写一个键 → 真实字段被塞进 extra_body，出站体里同一字段出现两次
//     （JSON 允许重复键，上游取哪个由解析器决定，行为不可预测）；
//   - 少写一个键 → 该字段丢失（typed 未覆盖 + extra 未补）。
//
// 新增 SDK 字段时，**先改这张表，再加 golden 用例**（见 volcengine_ark_golden_test.go）。
const arkMaxStreamFrameBytes = 8 << 20

// Forward 承接图片生成（同步）。path 是**平台 canonical path**（/v1/images/generations）。
//
// 端点判定用 canonical path 而不是「是不是图片端点」：
// edits / variations 虽然也是图片，但 Ark 没有这两个端点（它用同一个 generations 接口
// 通过 `image` 字段做图生图）。返回 CallerParamError 而不是普通 error，是为了让网关
// 立刻 400 —— 换渠道不会变好，多试一次就是多一次真实出站。
func (VolcengineARK) Forward(ctx context.Context, cfg *relay.ChannelConfig,
	req *relay.RelayRequest, path string) (*relay.RawResponse, error) {

	if path != relay.PathImagesGenerations {
		return nil, &relay.CallerParamError{Msg: "该模型只用于图片生成（/v1/images/generations）"}
	}
	if req == nil {
		return nil, &relay.CallerParamError{Msg: "请求为空"}
	}
	// 同步出图的等待上限单独抬高（见 arkImageCfg）：缺省 60s 会把 90–180s 的 4K 组图判成超时，
	// 而上游往往已经出图并计费。配置校验放最前面，避免「配置坏了却报参数错」。
	icfg := arkImageCfg(cfg)
	if err := arkCheckConfig(icfg); err != nil {
		return nil, err
	}
	// 同步端点强制 stream=false：走到这里说明客户端没要流式，
	// 上游若回 SSE，网关会按 JSON 交付一坨无法解析的帧（且按整段 JSON 计费）。
	payload, err := arkImageBody(req.Body, req.Model, false, req.ImageSize)
	if err != nil {
		return nil, err
	}

	resp, err := arkDo(ctx, icfg, http.MethodPost, arkPathImages, payload)
	if err != nil {
		// 响应读不全 ≠ 上游没出图。b64_json 组图（4K × N）单响应可达数十 MB，
		// 超过读体上限时上游往往**已经出完图并计费**。
		//
		// 必须包成 AmbiguousSubmitError：网关据此 502 + 全额释放 + 打标对账，
		// **不换渠道重投**。落成普通 error 的代价是换一条渠道再生成一遍 ——
		// 平台付两次钱、用户只收到一份（且两次图还不一样）。
		if errors.Is(err, relay.ErrUpstreamBodyTooLarge) {
			return nil, &relay.AmbiguousSubmitError{Err: err}
		}
		return nil, arkImageOutboundError(ctx, err)
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return nil, arkStatusError(resp.Status, resp.Body, icfg)
	}
	// 2xx：**逐字节**透传上游响应体。
	// 这里绝不能再解析后重序列化 —— 解码一次就丢一次 schema 外字段，重序列化一次就多一次
	// 「Opt* 零值写空字节」的机会。计费与档位核对都在网关侧对这份原始字节做只读解析
	// （usage_estimate.go 的 actualImageCount / upstreamImageSpec），不需要适配器先拆一遍。
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	return &relay.RawResponse{StatusCode: resp.Status, ContentType: ct, Body: resp.Body}, nil
}

// arkImageOutboundError 给同步出图的**出站失败**分类（唯一处）。
//
// 为什么不能直接交给 arkError：
//   - 超时（上游可能已出图并计费）必须翻译成 ImageGenTimeoutError —— 网关据此停止
//     failover 并打 submit_ambiguous；包成普通 error 会换候选重投，等于让上游再生成一次
//     （平台付两次钱、用户可能先后收到两张不同的图）。
//   - 客户端先断开（ctx.Err() != nil）必须原样返回：网关靠 errors.Is 判 499 并全额释放，
//     翻译成超时会把它降级成 504。
//   - 其余传输错误原样返回（归因上游，允许换候选）。
func arkImageOutboundError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() == nil && arkIsTimeout(err) {
		return &relay.ImageGenTimeoutError{Waited: arkImageTimeout, Err: err}
	}
	return err
}

// arkValidateImageBody 只做「本地就能定性」的校验。
//
// 刻意**不做**的事（都交给上游 400 → arkError → CallerParamError）：
//   - 参考图 + 生成张数 ≤ 15 的上限：那是 Ark 的业务规则，且各模型不同。
//     平台侧再写一份，上游一调整就变成「我们比上游更严格」；
//   - size 取值合法性：档位/像素/auto 的合法集随模型变化；
//   - 「该模型是否支持 stream / 组图 / 图层拆分」：那是 models.capabilities 的事实，
//     由网关在预检期判一次（relay.relayRequest）。适配器再按模型名写白名单
//     就是第二真相源，运营改能力声明后两边会打架。
//
// arkImageBody 是 Ark 图片请求的**唯一**出站形态：只替换模型名，其余字节原样。
//
// 为什么不再解析成 typed 结构：平台的立场是「支持什么由模型与官方文档决定」，
// 平台既不该校验取值，也不该把「档位 + 画幅」换算成像素 —— 那会改写用户原话
// （要 2048x1152 却按 2K 发出去），且每加一个 SDK 字段就要同步一张键表。
//
// 仍然要做的两件"翻译"（不是校验）：
//   - 平台别名 `aspect_ratio` / `ratio` 上游没有，删掉（留着是未知键）；
//   - 把 size 对齐到**计费侧解析出的胜出值**（`ImageSize.Raw`），然后删掉别名。
func arkImageBody(body []byte, upstreamModel string, stream bool, im *billing.ImageSize) ([]byte, error) {
	merged, err := arkReplaceModel(body, upstreamModel)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if uerr := json.Unmarshal(bytes.TrimSpace(merged), &m); uerr != nil || m == nil {
		return nil, &relay.CallerParamError{Msg: "请求体必须是 JSON 对象"}
	}
	// stream 由调用方决定（同步强制 false / 流式强制 true）：
	// 客户端不传时上游默认 false，同步端点就会拿到流式响应，网关按 JSON 交付必然解析失败。
	if stream {
		m["stream"] = json.RawMessage("true")
	} else {
		// 显式写 false 而不是删键：删键依赖「上游默认 false」这个假设，
		// 一旦某版本默认 true，同步链路就会收到 SSE 而按 JSON 交付。
		m["stream"] = json.RawMessage("false")
	}
	// size 对齐到计费侧解析出的胜出值。
	//
	// 过去这里自己按 resolution → image_size 挑，而计费侧是 size → image_size → resolution：
	// 「resolution=1K + image_size=2K」于是变成「按 2K 收钱、按 1K 出图」。
	// `ImageSize.Raw` 就是那次解析的胜出原文，这里直接用，不再自己挑一遍。
	if im != nil {
		if want := strings.TrimSpace(im.Raw); want != "" {
			cur := ""
			if raw, ok := m["size"]; ok && arkHasValue(raw) {
				_ = json.Unmarshal(raw, &cur)
				cur = strings.TrimSpace(cur)
			}
			if cur != want {
				m["size"], _ = json.Marshal(want)
			}
		}
	}
	delete(m, "resolution")
	delete(m, "image_size")
	delete(m, "aspect_ratio")
	delete(m, "ratio")
	return json.Marshal(m)
}

// arkHasValue 判断一个 JSON 原始值是否「有内容」（缺失 / null / "" / [] / {} 都算没有）。
func arkHasValue(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false
	}
	switch string(trimmed) {
	case "null", `""`, "[]", "{}":
		return false
	}
	return true
}

func (VolcengineARK) StreamImages(ctx context.Context, cfg *relay.ChannelConfig,
	req *relay.RelayRequest, onFrame func(relay.ImageFrame) error) (*relay.ImageStreamResult, error) {

	if req == nil {
		return nil, &relay.CallerParamError{Msg: "请求为空"}
	}
	if err := arkCheckConfig(cfg); err != nil {
		return nil, err
	}
	// 强制 stream=true：走到这里说明客户端要的是流式（SSE），
	// 上游不认 stream 时会整段返回，由网关按 stream_json_fallback 兜底。
	payload, err := arkImageBody(req.Body, req.Model, true, req.ImageSize)
	if err != nil {
		return nil, err
	}

	u, err := relay.BuildRawURL(cfg, arkPathImages)
	if err != nil {
		return nil, err
	}
	// 流式必须用 UpstreamStreamClient（0 整体超时）：client(cfg) 的 60s 整体超时
	// 会在「出图较慢」时于中途掐断连接，表现为「已交付 1 张后失败」。
	// 长流的正确口径是「多久没收到字节」，由下面的空闲计时器负责。
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(streamCtx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("构造上游请求失败：%w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	relay.SetAuthHeader(httpReq, cfg)

	idle := relay.StreamIdleTimeout()
	timer := time.AfterFunc(idle, cancel)
	defer timer.Stop()

	resp, err := relay.UpstreamStreamClient().Do(httpReq)
	if err != nil {
		return nil, arkStreamErr(err, ctx, streamCtx, idle)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		msg := arkRedact(arkErrText(b), cfg)
		// 400/422 = 上游明确拒绝参数（例如该模型不支持 stream、size 非法）：
		// 必须翻译成 CallerParamError —— 网关会立刻 400 且**不记渠道健康**，
		// 也不会把它当成「流式渠道失败」去 failover 到另一条同样不支持的渠道。
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity {
			return nil, &relay.CallerParamError{Msg: msg}
		}
		return &relay.ImageStreamResult{StatusCode: resp.StatusCode, ErrorMsg: msg}, nil
	}
	// 上游已接受（2xx 且开始推流）：此时才允许网关写出 SSE 头并开始心跳。
	// 放在状态码判定**之后**：400/401/429 必须仍能走「未开流」路径返回正确状态码并换渠道。
	relay.NotifyAccepted(req)
	return arkPumpSSE(resp.Body, timer, idle, onFrame)
}

// arkStreamErr 把流式建连失败分类：区分「我们自己的空闲超时」与「真正的网络错误」。
//
// 不区分时的表现：空闲超时被报成 `context canceled`，运维会去查客户端断开，
// 而真实原因是上游写完响应头就不再推帧。
func arkStreamErr(err error, outer, stream context.Context, idle time.Duration) error {
	if outer.Err() != nil {
		// 客户端断开：必须原样返回，网关靠 errors.Is 判 499 并全额释放。
		return err
	}
	if stream.Err() != nil {
		return fmt.Errorf("上游流式连接空闲超过 %d 秒未返回数据", int(idle.Seconds()))
	}
	return err
}

// arkIdleReader 在每次成功读取后重置空闲计时器。
//
// 为什么不能用 http.Client.Timeout：那是**整体**超时，长流会被拦腰掐断。
// 也不能用连接级 SetReadDeadline（http.Client 已封装 Transport，拿不到 net.Conn）。
// 唯一可行且语义正确的做法是「无数据即取消」：每次读到字节就重置。
type arkIdleReader struct {
	rc    io.Reader
	timer *time.Timer
	idle  time.Duration
}

func (r *arkIdleReader) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	if n > 0 {
		r.timer.Reset(r.idle)
	}
	return n, err
}

// arkPumpSSE 读取原始 SSE 并逐帧回调。
//
// 四条规则，每条都对应一个真实失败模式：
//  1. 按行读，`\n` 与 `\r\n` 都接受 —— 上游改一次换行符就会拼出带 `\r` 的 JSON，
//     json.Unmarshal 直接失败，整条流当场结束；
//  2. 累积到空行才派发一帧 —— SSE 允许多行 `data:`，按行派发会把一条 JSON 拆成两帧；
//  3. `data: [DONE]` 终止 —— 不终止会让连接挂到空闲超时（白等 120 秒）；
//  4. 单帧超过 arkMaxStreamFrameBytes 立即中止 —— 防单帧把内存打爆。
func arkPumpSSE(body io.Reader, timer *time.Timer, idle time.Duration,
	onFrame func(relay.ImageFrame) error) (*relay.ImageStreamResult, error) {

	res := &relay.ImageStreamResult{StatusCode: http.StatusOK}
	br := bufio.NewReaderSize(&arkIdleReader{rc: body, timer: timer, idle: idle}, 64<<10)

	var (
		eventName string
		dataBuf   []byte
		lineBuf   []byte
		overflow  bool
		// foreign 累积「不像 SSE 字段」的行，用于收尾时识别「200 但其实是整段 JSON」（P3-2）。
		// 正常 SSE 的每一行都以 data:/event:/":" 开头，永远不会进这里，因此不增加常态内存。
		foreign         []byte
		foreignOverflow bool
	)
	// dispatch 派发已累积的一帧；返回 done=true 表示收到 [DONE]。
	dispatch := func() (bool, error) {
		if overflow {
			return false, fmt.Errorf("%w（图片流式单帧超过 %d 字节）", relay.ErrUpstreamBodyTooLarge, arkMaxStreamFrameBytes)
		}
		if len(dataBuf) == 0 {
			eventName = ""
			return false, nil
		}
		data := bytes.TrimRight(dataBuf, "\r\n")
		event := eventName
		eventName = ""
		dataBuf = nil
		if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			// [DONE] 必须转发给客户端（P3-1）：多数 SDK 以它作为「流正常结束」的判据，
			// 吞掉之后客户端会一直等自己的空闲超时，把一次成功的生成显示成「卡死」。
			// 不递增 res.Frames：那个计数是「上游真的推了内容帧」的口径，零帧判定依赖它。
			if onFrame != nil {
				if err := onFrame(relay.ImageFrame{Event: event, Data: data}); err != nil {
					return false, err
				}
			}
			return true, nil
		}
		frame := arkParseFrame(event, data)
		res.Frames++
		if frame.URL != "" {
			res.Images = append(res.Images, frame.URL)
		}
		if frame.Terminal {
			res.GeneratedImages = arkFrameImageCount(data)
			// 只保留 completed 帧原文：它是唯一含 usage 的一帧，
			// 也是排查「上游说生成了 3 张、用户只收到 1 张」时的凭据。
			res.UsageRaw = append([]byte(nil), data...)
			if m := arkFrameModel(data); m != "" {
				res.Model = m
			}
		}
		if onFrame != nil {
			if err := onFrame(frame); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	// handleLine 处理一行（已去掉行尾换行）。
	handleLine := func(line []byte) (bool, error) {
		switch {
		case len(line) == 0:
			return dispatch()
		case bytes.HasPrefix(line, []byte("event:")):
			eventName = strings.TrimSpace(string(line[len("event:"):]))
		case bytes.HasPrefix(line, []byte("data:")):
			chunk := bytes.TrimSpace(line[len("data:"):])
			if overflow {
				return false, nil
			}
			if len(dataBuf)+len(chunk)+1 > arkMaxStreamFrameBytes {
				// 超限后**继续读完该帧再报错**：立即返回会让连接停在半帧上，
				// 客户端收到的是被截断的 SSE（比一个明确的错误更难排查）。
				overflow = true
				dataBuf = nil
				return false, nil
			}
			if len(dataBuf) > 0 {
				dataBuf = append(dataBuf, '\n')
			}
			dataBuf = append(dataBuf, chunk...)
		default:
			// 注释行（": keep-alive"）与未知字段：忽略。
			// 上游心跳必须被忽略，否则会被当成空帧派发，客户端每 15 秒收到一条空事件。
			//
			// 但内容要**留一份**：整段 JSON 响应（上游忽略 stream=true）的每一行都落在这里，
			// 收尾时靠它把结果救回来（见 arkFinalizeRawJSON）。
			if bytes.HasPrefix(bytes.TrimSpace(line), []byte(":")) {
				return false, nil
			}
			if len(foreign)+len(line)+1 > arkMaxUpstreamBody {
				foreignOverflow = true
				return false, nil
			}
			foreign = append(foreign, line...)
			foreign = append(foreign, '\n')
		}
		return false, nil
	}

	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 {
			// 长行（b64 帧可达数 MB）会被 ReadSlice 切成多段返回 ErrBufferFull：
			// 必须先按行聚合，否则一条 JSON 会被当成多行 data 拼接，产物直接损坏。
			if len(lineBuf)+len(chunk) > arkMaxStreamFrameBytes+1024 {
				overflow = true
				lineBuf = nil
			} else {
				lineBuf = append(lineBuf, chunk...)
			}
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			// 行还没结束，继续读下一段。
			continue
		case errors.Is(err, io.EOF):
			// 上游没补空行就断开：把最后一行与最后一帧派发出去再收尾。
			if len(lineBuf) > 0 {
				line := bytes.TrimRight(lineBuf, "\r\n")
				lineBuf = nil
				if _, herr := handleLine(line); herr != nil {
					return res, herr
				}
			}
			if _, derr := dispatch(); derr != nil {
				return res, derr
			}
			return arkFinalizeRawJSON(res, foreign, foreignOverflow), nil
		case err != nil:
			return res, err
		}
		line := bytes.TrimRight(lineBuf, "\r\n")
		lineBuf = nil
		done, herr := handleLine(line)
		if herr != nil {
			return res, herr
		}
		if done {
			return res, nil
		}
	}
}

// arkFinalizeRawJSON 处理「200 但没有一帧 SSE」的收尾（P3-2 救助）。
//
// 场景：客户端要 stream，上游忽略 stream=true 直接回整段 JSON。此时每一行都落进
// 「未知字段」分支，res.Frames 为 0。若就这样返回，网关会判「零帧空流」→ 502 全额退费，
// 而上游**已经出图并计费** —— 用户白等、平台白付、图还被丢掉。
//
// 判据刻意保守：只有「一帧都没有」且「累积内容是一份合法 JSON」才认。
// 有任何一帧正常解析过，就说明上游确实在推 SSE，此时零帧结论成立（不能拿垃圾行冒充结果）。
func arkFinalizeRawJSON(res *relay.ImageStreamResult, foreign []byte, overflow bool) *relay.ImageStreamResult {
	if res == nil || res.Frames > 0 || overflow {
		return res
	}
	trimmed := bytes.TrimSpace(foreign)
	if len(trimmed) == 0 || !json.Valid(trimmed) {
		return res
	}
	res.RawJSON = trimmed
	if m := arkFrameModel(trimmed); m != "" {
		res.Model = m
	}
	return res
}

// arkParseFrame 从一帧 data 里旁路解析出「给网关用的少量元信息」。
//
// 只读必要字段，**不重排原字节**：未知事件、未知字段原样透传给客户端。
// 解析失败不报错（返回零值帧）：这一帧可能不是 JSON（上游心跳），
// 报文由客户端自己解释，平台无权因为它「不认识」就中断整条流。
func arkParseFrame(event string, data []byte) relay.ImageFrame {
	f := relay.ImageFrame{Event: event, Data: data}
	var probe struct {
		Type       string `json:"type"`
		URL        string `json:"url"`
		B64        string `json:"b64_json"`
		ImageIndex int    `json:"image_index"`
		Size       string `json:"size"`
	}
	if json.Unmarshal(data, &probe) == nil {
		if f.Event == "" {
			f.Event = probe.Type
		}
		f.URL = strings.TrimSpace(probe.URL)
		f.Index = probe.ImageIndex
		f.Size = strings.TrimSpace(probe.Size)
		// b64 与 url 二选一：两种都要算「拿到了图」，否则 b64 流会被判成零交付。
		f.HasImage = f.URL != "" || strings.TrimSpace(probe.B64) != ""
		f.Terminal = strings.HasSuffix(strings.TrimSpace(probe.Type), ".completed")
	}
	return f
}

// arkFrameImageCount 读 completed 帧的 usage.generated_images（缺失返回 0）。
func arkFrameImageCount(data []byte) int64 {
	var probe struct {
		Usage struct {
			GeneratedImages int64 `json:"generated_images"`
		} `json:"usage"`
	}
	if json.Unmarshal(data, &probe) != nil {
		return 0
	}
	return probe.Usage.GeneratedImages
}

// arkFrameModel 读 completed 帧的 model（仅用于记录，缺失返回空串）。
func arkFrameModel(data []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(data, &probe) != nil {
		return ""
	}
	return strings.TrimSpace(probe.Model)
}
