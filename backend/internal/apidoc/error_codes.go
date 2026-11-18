package apidoc

import (
	"sort"

	"lingdong-api/internal/pkg/errs"
)

// 本文件是「接口会返回哪些错误码」的唯一来源：种子的 `doc_endpoint_errors` 由它填充。
//
// 从 endpoint_doc.go 抽出来：那份文件是 v6 的端点文档投影层（页面结构、相关接口、
// 面包屑），v7 之后整层删掉，但错误码是**契约事实**、与页面形态无关，
// 所以单独留下 —— 它与「模型级错误码」（project.go 里按模型能力裁剪的那套）不同：
// 这里是端点级视角，精确到「调这个接口会撞上哪些码」。

// endpointErrors 返回该端点**会真实返回**的错误码。
//
// 与模型级 `errorCodesFor` 的分工：那个按「模型有没有异步端点」裁剪，
// 结果会给不相干的端点印一遍（同一模型既有同步出图又有任务产物）。
// 端点文档是端点级视角，必须精确到「调这个接口会撞上哪些码」。
func endpointErrors(k Kind) []ErrorCode {
	tr, ok := TransportOf(k)
	if !ok {
		return nil
	}
	// 两条非 /v1 的公开路径：不带 KEY，因此没有 401/403，错误体也不是 JSON。
	switch k {
	case KindFileDirect:
		// 这个路径由 NoRoute 前缀分发接住（任意方法都进同一函数），非 GET/HEAD
		// 也返回 410 —— 所以**没有** 405，写成 405 会让读者去处理一个不会出现的码。
		return sortedErrors([]ErrorCode{
			{410, "file_expired", "文件不存在 / 已过期 / 地址缺文件名，一律按过期处理（纯文本响应）。"},
			{429, errs.CodeRateLimited, "按 IP 限流（纯文本响应）。"},
		})
	case KindMediaProxy:
		return sortedErrors([]ErrorCode{
			{404, "not_found", "签名不合法（纯文本响应，不是 JSON）。"},
			{405, "method_not_allowed", "只接受 GET / HEAD（纯文本响应）。"},
			{410, "file_expired", "链接已过期，请重新获取（纯文本响应）。"},
			{429, errs.CodeRateLimited, "按 IP 限流（纯文本响应）。"},
		})
	}

	base := []ErrorCode{
		{401, "invalid_api_key", "KEY 无效、已过期或被禁用。"},
		{429, errs.CodeRateLimited, "请求过于频繁，稍后重试。"},
		{500, "internal_error", "服务端异常，本次不计费。"},
	}

	switch tr.Group {
	case "model":
		// 模型清单只读本地目录，不走上游：没有 502/503，也没有按模型的 403。
		if k == KindModelsGet {
			base = append(base, ErrorCode{404, "model_not_found", "模型不存在或未在售。"})
		}
	case "acct":
		// 账户与用量同样只读本地账本。
		base = append(base, ErrorCode{403, errs.CodeScopeDenied, "该 KEY 未开放账户查询权限。"})
	case "file":
		base = append(base,
			ErrorCode{404, "file_not_found", "文件号不存在，或不属于当前 KEY。"},
			ErrorCode{410, "file_expired", "临时文件保留 24 小时。"},
			ErrorCode{416, "range_not_satisfiable", "Range 起点超过文件长度。"},
			ErrorCode{503, "storage_unavailable", "存储服务不可用，稍后重试。"},
		)
		if k == KindFilesUpload {
			// 上传的错误码**不是**通用的 invalid_request：每一种失败都有自己的码，
			// 客户端据此决定「换格式重试」还是「今天别再传了」，合并成一个码就没法分流。
			base = append(base,
				ErrorCode{403, "file_disabled", "平台关闭了临时文件上传。"},
				ErrorCode{400, "file_missing", "没有携带 file 字段。"},
				ErrorCode{400, "file_empty", "文件内容为空。"},
				ErrorCode{413, "file_too_large", "超过单文件上限。"},
				ErrorCode{415, "file_type_unsupported", "扩展名 / MIME / 魔术字节不在白名单内。"},
				ErrorCode{429, "file_daily_limit_exceeded", "超出每日上传次数。"},
				ErrorCode{429, "file_concurrency_limit", "并发上传数已达上限（带 Retry-After）。"},
				ErrorCode{429, "file_storage_full", "账号在存容量已达上限。"},
			)
		}
	case "task":
		base = append(base,
			ErrorCode{404, "not_found", "任务不存在，或不属于当前 KEY。"},
			ErrorCode{410, "file_gone", "产物已过保留期（7 天）。"},
			ErrorCode{416, "range_not_satisfiable", "Range 起点超过产物长度。"},
		)
		if k == KindTasksCancel {
			base = append(base, ErrorCode{409, "task_not_cancelable", "任务已开始生成或已结束，不能取消。"})
		}
	default:
		// 走模型的端点（lang / vec / image / video / audio / music / safe）。
		base = append(base,
			ErrorCode{400, errs.CodeInvalidParam, "参数不合法（字段缺失、取值越界、素材地址不可访问）。"},
			// 协议面按兼容要求是 403 `insufficient_user_quota`（见 gateway.WireOf）；
			// 这里描述的是**内部码**与它所在的状态档，供控制面与预检文案使用。
			ErrorCode{403, "insufficient_user_quota", "额度不足，请充值。"},
			ErrorCode{403, "model_not_allowed", "该 KEY 没有这个模型的权限。"},
			ErrorCode{403, "modality_not_allowed", "该模态不在 KEY 的白名单里。"},
			ErrorCode{404, "model_not_found", "模型不存在或已下线。"},
			ErrorCode{413, "request_too_large", "请求体超过上限。"},
			ErrorCode{503, errs.CodeServiceUnavailable, "本次调用未能完成，未产生费用。"},
			ErrorCode{503, "model_unavailable", "该模型暂不可用，请更换模型或稍后重试。"},
		)
	}
	if tr.Phase == PhaseSubmit {
		base = append(base,
			ErrorCode{409, "task_not_cancelable", "取消时任务已开始生成（仅取消接口会返回）。"},
			ErrorCode{504, "gateway_timeout", "等待超时，已全额释放额度。"},
		)
	}
	return sortedErrors(base)
}

// sortedErrors 去重并按「状态码升序 + 码名升序」排序。
//
// 排序而不是按追加顺序：错误表是给人查的，300 夹在 400 与 401 之间会让人
// 以为漏了一行；去重是因为同一状态码可能有多个码（403 有两个）。
func sortedErrors(in []ErrorCode) []ErrorCode {
	seen := make(map[string]bool, len(in))
	out := make([]ErrorCode, 0, len(in))
	for _, e := range in {
		key := e.Code
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Status != out[j].Status {
			return out[i].Status < out[j].Status
		}
		return out[i].Code < out[j].Code
	})
	return out
}

// samplesEndpoint 把端点文档投影还原成示例生成所需的端点投影。
//
// 为什么不直接让示例生成器吃 EndpointDocProjection：`BuildSamples` 只认
// EndpointProjection 的字段/返回/传输/示例这一小组事实，两处各写一份
// 「示例需要什么」等于给示例生成器开第二条入口，而第二条入口迟早与第一条分叉。
