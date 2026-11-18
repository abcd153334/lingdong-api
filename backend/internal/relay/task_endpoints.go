package relay

import (
	"context"
	"errors"
	"strings"

	"lingdong-api/internal/apidoc"
)

// 本文件是「异步能力」的唯一判据：某适配器承接 models.endpoints 词表里的哪几个端点。
//
// 词表**不在本文件声明**：唯一真相源是 apidoc.TaskEndpointKeys()（按 Phase=submit &&
// CapabilityChecked 派生，见 apidoc/kind.go）。在这里再抄一份端点字符串，加一个异步端点
// 就要改两处，而且没有任何测试能发现漏改 —— 那正是「多真相源」的经典形态。
//
// 为什么要从「一个布尔」升级成「端点集合」：改造前 SupportsTask() 只回答「能不能做异步」，
// 而异步有四类端点（视频/再生成/图像异步/音乐/歌词）。布尔无法表达「多米能做音乐、
// openai 只能做视频」，于是音乐模型能绑到 openai，保存体检通过、首次调用把音乐 body
// 打到视频端点 —— 错误与真实原因无关，且要到用户投诉才被发现。

// AsyncEndpointSupplier 是**可选**能力接口：适配器声明自己承接哪几个异步端点。
//
// 值是 models.endpoints 的写法（如 "video/generations"），必须落在 apidoc.TaskEndpointKeys()
// 内（TestAsyncEndpointsInVocabulary 守）。未实现本接口但实现了 TaskAdapter 的适配器，
// 视为只承接 video/generations —— 与改造前「实现 TaskAdapter 即支持异步」逐字等价，
// 第三方插件零迁移。
type AsyncEndpointSupplier interface {
	AsyncEndpoints() []string
}

// defaultAsyncEndpoints 是「实现了 TaskAdapter 但没声明端点」时的兜底。
//
// 只给第三方/插件用；内置适配器一律显式声明（一行，且能自文档化「这家到底支持哪些模态」）。
var defaultAsyncEndpoints = []string{"video/generations"}

// AsyncEndpointsOf 返回某适配器承接的异步端点集合（返回包级切片，调用方只读）。
func AsyncEndpointsOf(adapterID string) []string {
	a, ok := GetAdapter(adapterID)
	if !ok {
		return nil
	}
	if s, ok := a.(AsyncEndpointSupplier); ok {
		return s.AsyncEndpoints()
	}
	if _, ok := a.(TaskAdapter); ok {
		return defaultAsyncEndpoints
	}
	return nil
}

// TaskEndpointSupported 是异步能力的**唯一判据**：保存期探针、提交期选路、提交闸门共用。
//
// endpointKey == "" 表示只要求「能做异步」（重试路径没有入站端点可比）。
//
// 刻意**不看** capRoutingEnforced()：那个开关管的是同步 Forward 的能力路由，
// 复用等于给另一个功能加第二个含义；而且「关掉能力校验就允许把音乐打到视频端点」
// 不是应急能力，是事故。
func TaskEndpointSupported(adapterID, endpointKey string) bool {
	eps := AsyncEndpointsOf(adapterID)
	if endpointKey == "" {
		return len(eps) > 0
	}
	want := apidoc.NormalizeEndpointKey(endpointKey)
	for _, e := range eps {
		if apidoc.NormalizeEndpointKey(e) == want {
			return true
		}
	}
	return false
}

// SubmitTaskOf 是提交的**唯一管线**：能力闸门只在这里，适配器内部不再各写一份。
//
// 为什么在包装层而不在每个适配器首行：提交只有两个调用点（task.Submit / task.Retry），
// 而适配器有 6 个（含规格），每个写一遍闸门 = 加一个适配器就漏一次；而且闸门一旦漏，
// 表现是「上游 502」而不是「配置错」，排查方向完全错。
//
// 越界端点返回 ConfigError：零出站、不记渠道健康、不冷 key。提交方据此继续换渠道
// （不同渠道的 model_overrides 不同，换一个可能就承接了），由它决定终局口径。
func SubmitTaskOf(ctx context.Context, adapterID string, ta TaskAdapter, cfg *ChannelConfig,
	req *TaskSubmitRequest) (string, error) {
	if ta == nil {
		return "", errors.New("relay: 适配器不支持异步任务")
	}
	if req == nil {
		return "", &ConfigError{Scope: "渠道配置错误", Field: "task", Msg: "提交参数为空"}
	}
	if key := strings.TrimSpace(req.EndpointKey); key != "" && !TaskEndpointSupported(adapterID, key) {
		return "", &ConfigError{
			Scope: "渠道配置错误",
			Field: "endpoints." + apidoc.NormalizeEndpointKey(key),
			Msg: "该渠道的适配器（" + adapterID + "）不承接端点 " + apidoc.NormalizeEndpointKey(key) +
				"，可用异步端点：" + strings.Join(AsyncEndpointsOf(adapterID), " / "),
		}
	}
	return ta.SubmitTask(ctx, cfg, req)
}
