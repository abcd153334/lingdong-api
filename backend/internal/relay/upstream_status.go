package relay

// 上游 4xx 的对外映射（唯一实现）。
//
// 为什么必须区分：非 429 的 4xx 曾经一律回 503，而主流 SDK 把 503 当"服务故障"
// **立即重试**。调用方参数写错（模型名拼错、图片尺寸不支持）时，一次错误请求会被
// 放大成 N 次真实出站 —— 每次都要付上游成本，而问题永远不会自己变好。
// 429 早已单独原样透传（见 dispatch 里的 RateLimited 分支），4xx 属于同一类错误。
//
// 为什么**不透传上游原文**：上游报文里有上游模型名、上游主机与产品名（见
// dispatch 里 setErr 处的注释），原样回给调用方等于把供应链交给终端用户。
// 调用方可凭 request_id 到「调用记录」查看完整原因，那里按渠道权限脱敏过。
//
// 只映射到**已登记**的 errs.Def：新建裸码会绕开码表（文档、预检文案、门禁都挂在
// 登记的码上）。因此 413 对外是 400 —— 文案里保留真实上游码，便于定位。

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/gateway"
	"lingdong-api/internal/pkg/errs"
)

// upstreamCallerDef 判断某个上游状态码是否属于「调用方参数问题」，并给出对应码。
func upstreamCallerDef(status int) (errs.Def, bool) {
	switch status {
	case http.StatusNotFound:
		return errs.NotFound, true
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return errs.InvalidParam, true
	}
	return errs.Def{}, false
}

// failUpstreamCaller 在状态码属于调用方错误时直接写出响应并返回 true。
//
// 返回 false 时调用方必须继续走原有的 503 分支 —— 401/403 不能透传
// （那是在告诉全网"我们渠道的 key 坏了"），5xx 也不属于调用方问题。
func failUpstreamCaller(c *gin.Context, status int) bool {
	def, ok := upstreamCallerDef(status)
	if !ok {
		return false
	}
	msg := fmt.Sprintf("上游拒绝了本次请求（HTTP %d），本次未扣费", status)
	if status == http.StatusRequestEntityTooLarge {
		msg += "：请求体超过该渠道上限，请压缩或拆分后重试"
	} else {
		msg += "；请检查参数后重试"
	}
	gateway.FailDef(c, def.WithMessage(msg))
	return true
}
