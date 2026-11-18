package spec

import (
	"fmt"
	"strings"

	"lingdong-api/internal/relay"
)

// 本文件是「本接入扩展字段」的**运行期唯一入口**（校验与默认值注入）。
//
// 搬运（真正发给上游）仍然只有 body.copy / body.arrays / path 占位符三条路，
// 这里绝不参与拼请求体：一旦这里也开始"顺手带上某个字段"，字段就会出现
// 两条来源，而运营看到的文档只描述其中一条。

// applyRequestDefaults 把声明里的缺省值补进请求视图。
//
// 只补**没有给**的字段（缺键、null、空白字符串都算没给，与 body.copy 的
// isEmptyJSONValue 同一判据），已经传了值的绝不覆盖——覆盖会让用户显式
// 传的空串被静默替换成默认值，而请求看起来完全正常。
func applyRequestDefaults(ep Endpoint, src map[string]any) {
	if len(ep.RequestFields) == 0 || src == nil {
		return
	}
	for _, f := range ep.RequestFields {
		if f.Default == nil {
			continue
		}
		if v, ok := src[f.Name]; ok && !isEmptyJSONValue(v) {
			continue
		}
		src[f.Name] = f.Default
	}
}

// checkRequestFields 校验调用方传的扩展字段（必填 / 类型 / 枚举 / 长度）。
//
// 三条调用点必须齐全（forward / bridge 提交前 / SubmitTask 之前）：
// 漏掉任何一处，那个形态的请求就会把缺字段的请求直接发给上游，
// 表现成"参数不合法"或一句被脱敏的上游原话。
func checkRequestFields(ep Endpoint, src map[string]any) error {
	for _, f := range ep.RequestFields {
		v, ok := src[f.Name]
		if !ok || isEmptyJSONValue(v) {
			if f.Required {
				return &relay.CallerParamError{Msg: missingFieldMsg(f)}
			}
			continue
		}
		if err := checkFieldValueType(f, v); err != nil {
			return &relay.CallerParamError{Msg: fmt.Sprintf("参数 %s %v", f.Name, err)}
		}
	}
	return nil
}

// missingFieldMsg 拼「缺少必填参数 x（说明）」。
//
// 带上 desc 是刻意的：用户看到 reference_audio 这个名字时并不知道要填什么，
// 而 desc 是规格作者写给人看的那一句话。
func missingFieldMsg(f RequestField) string {
	if d := strings.TrimSpace(f.Desc); d != "" {
		return fmt.Sprintf("缺少必填参数 %s（%s）", f.Name, d)
	}
	return fmt.Sprintf("缺少必填参数 %s", f.Name)
}
