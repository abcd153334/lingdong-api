package relay

import (
	"os"
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/gateway"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/relay/claudeconv"
)

// Messages 处理 POST /v1/messages（Anthropic Messages 协议入站）。
//
// 它是 chat 能力的另一种线格式：模型白名单、计费公式、渠道选择、冻结与结算
// 全部复用对话补全那一套，唯一新增的是请求解码与响应编码（claudeconv）。
func Messages(c *gin.Context) {
	if !claudeMessagesEnabled() {
		// 灰度期关闭：明确 501 + Claude 形状，而不是 404 ——
		// 404 会让客户端以为地址写错了，反复检查 base_url。
		gateway.FailDefMsg(c, errs.NotImplemented,
			"该接口暂未开放，请稍后再试或改用 /v1/chat/completions")
		return
	}
	relayRequestIn(c, kindMessages, claudeconv.Codec{})
}

// MessagesCountTokens 明确不支持词块计数。
//
// 为什么不给个估算值：计数结果会被客户端用来管理上下文长度（决定何时压缩历史）。
// 我们的估算是按字符折的，对代码/JSON 误差可达 2 倍 —— 用户按它安排上下文，
// 到了真实上限会直接收到 400，比"没有这个接口"更难查。
// 501 是诚实的回答，客户端（含 Claude Code）会回退到本地估算。
func MessagesCountTokens(c *gin.Context) {
	gateway.FailDefMsg(c, errs.NotImplemented,
		"该接口暂不提供词块计数，请按本地估算管理上下文长度")
}

// claudeMessagesEnabled 判断 Messages 入站是否开放（LD_CLAUDE_MESSAGES）。
//
// 默认开；显式设 0/false/off 关闭 —— 关闭后所有请求立刻 501（秒级止血），
// 不需要回滚镜像。
func claudeMessagesEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LD_CLAUDE_MESSAGES"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}
