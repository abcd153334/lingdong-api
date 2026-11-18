package spec

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"lingdong-api/internal/relay"
)

// 本文件是上游路径占位符的**唯一替换点**。
//
// 设计约束：保存期就决定了每个占位符的来源（validateUpstreamPath 收到 vars 集合），
// 运行期只负责取值、校验、编码 —— 不做"这个值没有就试另一个字段"的回退。
// 回退会让同一份配置在两次调用里取到不同的值，而没有任何门禁能发现。

// pathVarScope 标记一个占位符取值的来源，决定失败时算谁的错。
type pathVarScope int

const (
	// pathVarFromConfig 表示值来自渠道/绑定（错在运营配置）。
	pathVarFromConfig pathVarScope = iota
	// pathVarFromCaller 表示值来自调用方参数（错在用户，必须 400）。
	pathVarFromCaller
)

// pathVals 是一次请求的占位符取值集合与来源标记。
type pathVals struct {
	values map[string]string
	scopes map[string]pathVarScope
}

// noPathVals 用于没有占位符的端点（普通同步转发/轮询）。
func noPathVals() pathVals {
	return pathVals{values: map[string]string{}, scopes: map[string]pathVarScope{}}
}

// taskPathVals 用于轮询端点：唯一的占位符是上游任务号。
//
// task_id 来自上游响应，取不到时属于**上游/配置**问题（不是用户参数错），
// 所以标成 config —— 否则缺任务号会被记成 400，用户会以为是自己填错了。
func taskPathVals(taskID string) pathVals {
	pv := noPathVals()
	if taskID != "" {
		pv.values["task_id"] = taskID
		pv.scopes["task_id"] = pathVarFromConfig
	}
	return pv
}

// requestPathVals 用于提交端点：upstream_model 来自绑定，其余来自调用方参数。
func requestPathVals(ep Endpoint, src map[string]any, upstreamModel string) pathVals {
	pv := noPathVals()
	if upstreamModel != "" {
		pv.values["upstream_model"] = upstreamModel
		pv.scopes["upstream_model"] = pathVarFromConfig
	}
	for _, f := range ep.RequestFields {
		v, ok := src[f.Name]
		if !ok || isEmptyJSONValue(v) {
			continue
		}
		pv.values[f.Name] = scalarString(v)
		pv.scopes[f.Name] = pathVarFromCaller
	}
	return pv
}

// scalarString 把标量渲染成路径段。
//
// 对象/数组返回空串：它们进不了路径段，交给 buildPath 报「值为空」，
// 而不是悄悄序列化成 `[object Object]` 之类的东西发给上游。
func scalarString(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case json.Number:
		return x.String()
	default:
		return ""
	}
}

// buildPath 把 `{name}` 换成实际值。
//
// 值必须满足 pathVarValueRe：路径段里不许出现 `/`、`?`、`#`、`..`。
// 不满足时按来源分类报错——运营配置错走 ConfigError（不记渠道健康），
// 调用方参数错走 CallerParamError（400），两者的处置完全不同。
func buildPath(pattern string, values map[string]string, scopes map[string]pathVarScope) (string, error) {
	out := pattern
	for _, name := range pathPlaceholders(pattern) {
		raw, ok := values[name]
		if !ok || strings.TrimSpace(raw) == "" {
			return "", pathVarError(name, scopes[name],
				fmt.Sprintf("占位符 {%s} 的值为空", name))
		}
		v := strings.TrimSpace(raw)
		if !pathVarValueRe.MatchString(v) || strings.Contains(v, "..") {
			return "", pathVarError(name, scopes[name],
				fmt.Sprintf("占位符 {%s} 的取值不合法（只能是字母数字与 . _ ~ : @ -）", name))
		}
		out = strings.Replace(out, "{"+name+"}", url.PathEscape(v), 1)
	}
	return out, nil
}

func pathVarError(name string, scope pathVarScope, msg string) error {
	if scope == pathVarFromConfig {
		return &relay.ConfigError{Scope: "渠道规格配置错误", Field: "path." + name, Msg: msg}
	}
	return &relay.CallerParamError{Msg: msg}
}
