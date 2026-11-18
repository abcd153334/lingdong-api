package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"lingdong-api/internal/db"
	"lingdong-api/internal/relay"
	specdsl "lingdong-api/internal/relay/spec"
	"lingdong-api/internal/service/modelcfg"
)

// errTrialDone 是试跑成功的哨兵：返回它才能让事务回滚。
//
// 直接把 err 当成成功标记有点反直觉，但这是唯一"跑完所有写库逻辑却不留痕迹"的做法：
// 试跑不能提交（会产生真实渠道/模型），也不能只跑一半（后面的 op 依赖前面的结果）。
var errTrialDone = errors.New("agent: trial finished")

// trialApply 在事务里把 plan 完整执行一遍，然后回滚。
//
// 它解决的是"提案期校验 vs 应用期校验"这份双份规则的问题：
// 写库路径里有大量规则（渠道必填项、适配器可解析、价格符号与半免费一致性、
// 绑定渠道存在/启用/模态匹配、vendor 存在、墓碑清理、端点覆盖），
// 这些规则每加一条，校验器就要跟着加一条，漏一条就是"提案通过、应用失败"。
// 现在只保留一句话：**能跑通就是合法**。
//
// 三条前提（改动前必须确认）：
//  1. applyOp 不出网、不广播缓存、不写审计、不重算文档（见其函数头）；
//  2. specsvc 的事务路径不失效注册表（gdb != db.DB 时跳过）；
//  3. 允许消耗自增序列（回滚不回退 sequence，ID 会出现空洞）。
//
// 第 3 条是刻意的取舍：用临时表或影子结构会引入第二套数据结构，
// 而空洞对业务没有任何影响。
// trialApply 返回试跑期收集到的非阻断告警（事务已回滚，告警仍在 Go 栈上）。
func trialApply(ctx context.Context, plan *Plan) ([]string, error) {
	sink := &applySink{}
	err := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		snap := newSnapshot()
		createdChannels := map[string]uint64{}
		for _, item := range plan.Ordered() {
			op := item.Op
			// 试跑不写 owner：它会整体回滚，写 0 更诚实地表达"这不是一次真实创建"。
			if err := applyOp(ctx, tx, &op, snap, createdChannels, sink, 0); err != nil {
				// Index 用原始位置：模型据此修改提案，重排后的位置会指错操作。
				return &OpError{Index: item.Index, Field: fieldOf(err), Err: err}
			}
		}
		return errTrialDone
	})
	if errors.Is(err, errTrialDone) {
		return sink.Warnings, nil
	}
	if err == nil {
		// 理论上不可达（上面的 return 一定非 nil）；宁可报错也不要静默通过。
		return nil, errors.New("agent: 试跑未回滚，已中止")
	}
	return nil, toValidationError(err)
}

// OpError 是"应用期错误"在试跑期的包装（带 op 序号与字段）。
type OpError struct {
	Index int
	Field string
	Err   error
}

func (e *OpError) Error() string { return e.Err.Error() }
func (e *OpError) Unwrap() error { return e.Err }

// toValidationError 把试跑错误翻译成带 `operations[i].data.<field>` 的校验错误。
//
// 这一步决定 AI 能不能自己修：只回一句 "第 3 个操作失败：..." 它只能整份重写，
// 而回 `operations[2].data.name：渠道名称不能为空` 它能只改一个字段。
func toValidationError(err error) error {
	var oe *OpError
	if !errors.As(err, &oe) {
		return err
	}
	field := fmt.Sprintf("operations[%d]", oe.Index)
	if oe.Field != "" {
		// channel/spec 的字段错误直接落在 data 下；模型侧也是 data 下的字段。
		if strings.HasPrefix(oe.Field, "bindings") || strings.HasPrefix(oe.Field, "data") {
			field += ".data." + strings.TrimPrefix(oe.Field, "data.")
		} else {
			field += ".data." + oe.Field
		}
	}
	return &ValidationError{Index: oe.Index, Field: field, Message: oe.Err.Error()}
}

// fieldOf 提取底层错误的字段名（三个来源的字段错误统一成一条）。
func fieldOf(err error) string {
	var mfe *modelcfg.FieldError
	if errors.As(err, &mfe) {
		return mfe.Field
	}
	var rce *relay.ConfigError
	if errors.As(err, &rce) {
		return rce.Field
	}
	var sfe *specdsl.FieldError
	if errors.As(err, &sfe) {
		return sfe.Field
	}
	return ""
}
