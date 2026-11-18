package logs

// 本文件是**用户端**错误文案的唯一定义点。
//
// 为什么不复用 usages.error_msg：那是上游原文（可能含上游域名、上游真实模型名、
// 上游返回的整段 JSON）。原文留在库里供管理台排障，用户侧只给「分类 + 是否扣费」。
//
// 三处调用方共用本函数（不得各写一份，否则三处文案会漂移）：
//  1. logs.PortalItem 组装（/api/portal/usage 与 /v1/usage 列表）；
//  2. logs.PortalDetail 组装（详情）；
//  3. relay.usageCSV（门户导出 CSV 的错误列）。

// PortalErrorSummary 把「状态 + HTTP 状态码 + 是否预检被拒」映射成一句分类文案。
//
// 成功返回空串（前端不渲染错误区）。
func PortalErrorSummary(status string, httpStatus int, isRejected bool) string {
	switch {
	case isRejected || status == "rejected":
		return "请求未通过校验，未消耗额度"
	case status == "pending":
		return "处理中"
	case status == "settled":
		return ""
	case httpStatus == 429:
		return "触发频率限制，请稍后重试"
	case httpStatus >= 400 && httpStatus < 500:
		return "请求未被接受（HTTP 4xx），本次未扣费"
	case httpStatus >= 500 || httpStatus == 0:
		// 0 表示没有拿到状态码（连接失败/超时）——对用户而言与 5xx 同义。
		return "服务暂时不可用，本次未扣费"
	default:
		return "本次未完成，未扣费"
	}
}
