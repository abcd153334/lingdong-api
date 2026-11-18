package adapters

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"lingdong-api/internal/relay"
)

// 火山方舟（Ark / 豆包）适配器。
//
// 设计要点（详见 docs/火山方舟接入方案.md）：
//   - 请求体用官方 Go SDK 的 typed 结构构造（typed + extra_body 兜底未知字段），
//     但**出站与响应都由我们自己处理**：SDK 没有导出「拿原始响应字节」的选项，
//     用它发送就等于必须把 typed 响应二次序列化 —— 那会丢 schema 外字段，
//     还会踩 `Opt*` 零值 MarshalJSON 产出空字节（真实事故，见 volcengine_ark_images.go 文件头）；
//   - 图片同步走 arkDo；图片流式自己读原始 SSE：SDK 的流式解析是严格枚举，
//     上游新增一个事件就会解析失败并**终止整条流**（已生成的图全部丢弃）；
//   - 视频提交/轮询用原始 JSON 自建 HTTP：SDK 的 TaskStatus 枚举缺 `expired`、
//     字段名与官方文档已漂移（`file_format` vs `output_format`）。
//
// 编译期接口断言：任何一个不满足，构建即失败（避免「以为实现了」）。
var (
	_ relay.Adapter            = VolcengineARK{}
	_ relay.TaskAdapter        = VolcengineARK{}
	_ relay.DetailedTaskPoller = VolcengineARK{}
	_ relay.ImageStreamer      = VolcengineARK{}
	_ relay.EndpointSupporter  = VolcengineARK{}
)

// 刻意**不实现**的两个接口，理由必须留在代码里，否则下一个人会「顺手补上」：
//
//  1. OptionsSupporter：Ark 没有任何需要运营配置的私有参数（region 在 base_url、
//     版本在路径）。实现它等于开一个「存了没人读」的口子。
//  2. DeclarativeSupporter：request_patch / response_path 对走 SDK 的适配器无效，
//     path_overrides 在 BuildRawURL 里也不生效。声明支持却只生效一半，
//     是最难查的一类配置问题；fail-closed 更诚实——运营配了会在保存期被拒绝，
//     而不是「保存成功但没效果」。

func init() {
	_ = relay.RegisterAdapter(VolcengineARK{})
}

// VolcengineARK 是火山方舟适配器（值类型，无状态：一切运行态来自 ChannelConfig）。
type VolcengineARK struct{}

// Name 返回适配器标识（对齐 channels.adapter_id）。
func (VolcengineARK) Name() string { return "volcengine-ark" }

// Caps 返回能力矩阵子集。
//
// 不声明 "text"：Ark 的对话端点不在本适配器的实现范围内（Chat 直接返回不支持）。
// 声明了却实现不了，会让「模型绑到这个渠道」在保存期通过、运行期 100% 失败。
func (VolcengineARK) Caps() []string {
	return []string{"image", "edit_image", "video", "passthrough"}
}

// SupportsEndpoint 只回答**同步端点**。
//
// 视频不在这里：relay 的 EndpointKind 没有 video 值（adapter.go 的导出常量只有 12 个
// 同步端点），视频候选由 relay.TaskEndpointSupported(adapterID, "video/generations") 过滤。
// 写成 KindVideoSubmit 是编译错误（那是 apidoc.Kind，不同包不同值域）。
func (VolcengineARK) SupportsEndpoint(k relay.EndpointKind) bool {
	return k == relay.KindImagesGenerations
}

// AsyncEndpoints：方舟只有视频异步任务。显式声明而不是吃默认值 ——
// 默认值是给第三方插件的兜底，内置适配器写出来才能自文档化「这家到底支持哪些模态」。
func (VolcengineARK) AsyncEndpoints() []string { return []string{"video/generations"} }

// 出网路径常量（唯一一处）。平台 canonical path 只用于「判端点」，不出网：
// 出网 URL = cfg.BaseURL（含 /api/v3）+ 下面的常量。
const (
	arkPathImages     = "/images/generations"
	arkPathTaskSubmit = "/contents/generations/tasks"
	arkPathTaskList   = "/contents/generations/tasks"
	arkPathTaskGetFmt = "/contents/generations/tasks/%s"

	// arkDefaultBase 是渠道默认值（同时下发给管理台，见 controller 的 default_base_url）。
	//
	// 必须以 /api/v3 结尾：平台 canonical path 是 /v1/...，而 Ark 数据面在 /api/v3 下，
	// 两个前缀必须由 base_url 承担 —— 这也是「用 openai 适配器调方舟永远 404」的根因。
	arkDefaultBase = "https://ark.cn-beijing.volces.com/api/v3"

	// arkMaxUpstreamBody 是 Ark 非流式响应的读取上限。
	//
	// 图片 b64_json 响应可达数十 MB（4K 图 × 4 张），沿用 openai 适配器的 8 MiB 会在
	// 解析前就被截断，表现为「JSON 不完整」。这里给 64 MiB 并把上限判定交给
	// ErrUpstreamBodyTooLarge，而不是静默截断。
	arkMaxUpstreamBody = 64 << 20
)

// errARKNoEndpoint 表示该端点不由本适配器承接。
var errARKNoEndpoint = errors.New("火山方舟渠道不支持该端点")

// arkImageTimeout 是同步出图的等待上限（P1-1）。
//
// 为什么必须比缺省的 60s 大得多：4K 组图 / 图层拆分在高峰实测 90–180s，
// 60s 会把它判成超时 —— 而上游往往已经出图并计费，等于「钱付了、图丢了」。
// 5 分钟覆盖官方文档给出的最慢档，同时仍在网关与反向代理的读超时之内。
//
// 注意：这里只是**我们愿意等多久**，与上游重试无关。重试策略只能有一处，且必须在平台侧
// （网关的 failover）：SDK 默认会自己重试 2 次，图片生成是计费调用，
// 「超时但上游已出图」的重试会重复计费，一次请求可能出现 3 倍成本。
// 所以本适配器不构造 SDK 客户端，出站一律走 arkDo —— 它不重试、不吞状态码。
const arkImageTimeout = 5 * time.Minute

// arkImageCfg 返回**复制并抬高超时**的配置，只用于同步图片。
//
// 为什么不改 DefaultAdapterTimeout：那是全适配器共享的缺省值，
// 抬高它等于让所有 OpenAI 兼容渠道都多挂 4 分钟连接，副作用远超收益。
func arkImageCfg(cfg *relay.ChannelConfig) *relay.ChannelConfig {
	if cfg == nil {
		return cfg
	}
	out := *cfg
	if out.Timeout < arkImageTimeout {
		out.Timeout = arkImageTimeout
	}
	return &out
}

// arkIsTimeout 判断错误是否为「等超时了」（而不是上游明确拒绝）。
//
// 三种来源都要覆盖：context 截止、os.ErrDeadlineExceeded（net 包的包装）、
// 以及 net.Error.Timeout()（SDK/HTTP 传输层自己判定的超时）。
func arkIsTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// Test 探测渠道连通性（鉴权 + 可达性）。
//
// 用只读、免费、且在官方数据面契约里的「任务列表」：
//   - 不用 /models：Ark 数据面在 /api/v3 下，官方文档没有把模型列表列进数据面契约，
//     失败时无法区分「渠道坏了」与「这个路径本来就不存在」；
//   - 不用出图：一次真实出图 = 一次真实成本。
//
// 400 也算通过：探活要回答的是「密钥对不对、域名通不通」，不是「分页参数对不对」。
// 把 400 判失败，上游改一次参数名就会让所有渠道显示红点。
func (VolcengineARK) Test(ctx context.Context, cfg *relay.ChannelConfig) error {
	resp, err := arkDo(ctx, cfg, http.MethodGet, arkPathTaskList+"?page_num=1&page_size=1", nil)
	if err != nil {
		// 网络层错误（域名不通 / TLS / 超时）在这里返回，必须带上可读原因：
		// 运营在管理台只看得到这一句。
		return fmt.Errorf("无法连接上游：%w", err)
	}
	switch {
	case resp.Status >= 200 && resp.Status < 300:
		return nil
	case resp.Status == http.StatusBadRequest:
		return nil
	case resp.Status == http.StatusUnauthorized || resp.Status == http.StatusForbidden:
		return errors.New("API Key 无效或无权限（HTTP " + fmt.Sprint(resp.Status) + "）")
	case resp.Status == http.StatusNotFound:
		return errors.New("接口路径不存在（HTTP 404），请确认 Base URL 填到 /api/v3 为止")
	default:
		return fmt.Errorf("上游返回 HTTP %d", resp.Status)
	}
}

// ---------------------------------------------------------------- 同步端点（仅图片）

// Chat 不承接同步对话（Ark 的对话协议不在本适配器范围内）。
func (VolcengineARK) Chat(context.Context, *relay.ChannelConfig, *relay.RelayRequest) (*relay.RelayResponse, error) {
	return nil, errARKNoEndpoint
}

// Embeddings 不承接向量请求。
func (VolcengineARK) Embeddings(context.Context, *relay.ChannelConfig, *relay.RelayRequest) (*relay.RelayResponse, error) {
	return nil, errARKNoEndpoint
}

// Completions 不承接旧版补全。
func (VolcengineARK) Completions(context.Context, *relay.ChannelConfig, *relay.RelayRequest) (*relay.RelayResponse, error) {
	return nil, errARKNoEndpoint
}

// StreamChat 不承接流式对话。
func (VolcengineARK) StreamChat(context.Context, *relay.ChannelConfig, *relay.RelayRequest, func(relay.SSEFrame) error) (*relay.StreamResult, error) {
	return nil, errARKNoEndpoint
}
