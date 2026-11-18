package relay

// 预检期失败记录（C1 · 决策 3）。
//
// 为什么用「响应层拦截」而不是在 15 个 early-return 处各插一行：
//   - 那些位置分布在 relayRequest、TokenAuth 与未来的新端点里，逐个手插必然漏；
//     漏掉的那一处就是"用户报障但日志里查不到"的那一次。
//   - `gateway.Fail*` 已经把业务错误码写进 gin.Context（`error_code`），
//     响应层读它就能拿到**精确的**失败原因，不需要再解析响应体、也不需要包装 Writer
//     （包装 Writer 会在 gzip/SSE 顺序上留下风险，这条路径刻意避开）。
//   - 身份问题按方案口径处理：401 时上下文里没有 user_id，就落 user_id=0 ——
//     这类记录的价值是"错误率与错误码分布"，不是归属。
//
// 不重复记录：真实计费行（settled/released）由 settleSession / releaseSession
// 打上 usageRecordedKey 标记，本中间件看到标记就跳过。

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/service/calllog"
)

// usageRecordedKey 标记「本次请求已经产生了一条真实 usage 行」。
const usageRecordedKey = "call_usage_recorded"

// markUsageRecorded 由结算/释放路径调用（表示已有真实记录，预检层不得重复写）。
func markUsageRecorded(c *gin.Context) {
	if c != nil {
		c.Set(usageRecordedKey, true)
	}
}

// usageRecorded 报告本次请求是否已有真实 usage 行。
func usageRecorded(c *gin.Context) bool {
	return c != nil && c.GetBool(usageRecordedKey)
}

// PreflightRecorder 是网关预检失败的记录中间件（挂在 /v1 组、TokenAuth 之前）。
//
// 必须挂在 TokenAuth **之前**：否则 401/403/429 这些由鉴权中间件直接返回的响应
// 根本进不了本中间件，而那恰恰是预检失败里占比最高的一类。
func PreflightRecorder() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if !calllog.Enabled() || usageRecorded(c) {
			return
		}
		status := c.Writer.Status()
		if status < 400 || !isRelayPath(c.Request.URL.Path) {
			return
		}
		code := c.GetString("error_code")
		msg := "预检失败（HTTP " + strconv.Itoa(status) + "）"
		if def, ok := errs.Get(code); ok {
			msg = def.Message
		}
		calllog.Record(calllog.Reject{
			UserID:     c.GetUint64("user_id"),
			KeyID:      c.GetUint64("key_id"),
			Model:      c.GetString("gateway_model"),
			Modality:   c.GetString("gateway_modality"),
			HTTPStatus: status,
			ErrorMsg:   msg,
			ErrorCode:  code,
			RequestID:  c.GetString("request_id"),
			IP:         c.ClientIP(),
		})
	}
}

// isRelayPath 判断是否网关转发端点（只记这些；/v1/models 之类的元数据端点 404 不需要入账）。
//
// 集合由 apidoc 的端点表派生（= docKindOf 全部同步转发端点的 path），不再手写 12 个字面量：
// 新增一个同步端点时，忘记往这里加会让「预检被拒的请求」静默不入账，
// 表现为调用记录里少了一条本该出现的 rejected 行。
//
// 注意取的是 docKindOf 的**值**（apidoc.Kind），不是键（endpointKind 是 iota 整数，
// 与文档层的字符串 id 无强转关系）；这里刻意不写成独立的 apidoc.Kinds() 全表，
// 因为全表还包含 models / 视频等不进 relayRequest 的端点，混进来会让不相干的 401 也入账。
var relayPathSet = func() map[string]bool {
	m := make(map[string]bool, len(docKindOf))
	for _, k := range docKindOf {
		t, ok := apidoc.TransportOf(k)
		if !ok || t.Path == "" {
			continue
		}
		m[t.Path] = true
		// 别名路径也是真实可命中的入口（例如 /dashboard/billing/* 是
		// /v1/dashboard/billing/* 的 new-api 老习惯路径）。只收**不含占位符**的：
		// 含占位符的别名由下面的 relayPrefixSet 按前缀匹配。
		for _, alias := range t.Aliases {
			if alias != "" && !strings.Contains(alias, "{") {
				m[alias] = true
			}
		}
	}
	return m
}()

// relayPrefixSet 是「含占位符的契约路径」的静态前缀（`{` 之前的部分）。
//
// 为什么需要它：真实请求路径里永远没有花括号，所以精确集合对
// `/v1beta/models/gemini-x:generateContent` 与 `/v1/engines/m/embeddings`
// **恒不命中** —— 这两条端点的 401/403/429 会静默不入账，调用记录里少一整类记录，
// 而预检失败恰恰是「我的 key 为什么不能用」的唯一线索。
var relayPrefixSet = func() []string {
	out := make([]string, 0, 4)
	for _, k := range docKindOf {
		t, ok := apidoc.TransportOf(k)
		if !ok || t.Path == "" {
			continue
		}
		if i := strings.IndexByte(t.Path, '{'); i > 0 {
			out = append(out, t.Path[:i])
		}
		for _, alias := range t.Aliases {
			if i := strings.IndexByte(alias, '{'); i > 0 {
				out = append(out, alias[:i])
			}
		}
	}
	return out
}()

func isRelayPath(p string) bool {
	if relayPathSet[p] {
		return true
	}
	for _, pre := range relayPrefixSet {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}
