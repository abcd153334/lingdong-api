package relay

import (
	"context"
	"errors"
	"net"
	"os"
	"time"
)

// IsTimeoutError 判断错误是否为「等待超时」（而不是上游明确报错或客户端断开）。
//
// 唯一实现：适配器（arkIsTimeout）与网关（图片超时包装）共用同一套判据 ——
// 各写一份的历史后果是「一边认为是超时、另一边认为是上游故障」，
// 于是同一次慢请求在一处触发 409、在另一处触发换渠道重跑（付两次上游成本）。
//
// 三种形态都要认：
//   - context.DeadlineExceeded：我们自己的 ctx 超时；
//   - os.ErrDeadlineExceeded：net 包在连接级超时上单独用的错误；
//   - net.Error.Timeout()：DNS/拨号/读超时（SDK 常包成自定义 error，需 errors.As 穿透）。
func IsTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// wrapImageTimeout 把「同步出图的等待超时」包成 ImageGenTimeoutError（其余情况原样返回）。
//
// 为什么必须包：裸超时经 classifyError 落 outcomeUpstream —— 会先换同渠道的另一把
// key、再换渠道重跑，等于为同一张图付两次上游成本，用户还可能先后拿到两张不同的图。
// 包过之后 Class()=="slow" 让归因走 internal（不换 key、不熔断），并在 dispatch 的
// 专用分支以 409 + submit_ambiguous + 全额释放收尾。
//
// 只对图片族生效：chat/responses 超时换渠道是正确行为（token 计价，没有
// "已生成未交付"的歧义）；aborted（客户端已断开）也不包 —— 那是 499 的语义。
func wrapImageTimeout(kind endpointKind, err error, aborted bool, waited time.Duration) error {
	if err == nil || aborted || !isImageKind(kind) || !IsTimeoutError(err) {
		return err
	}
	var already *ImageGenTimeoutError
	if errors.As(err, &already) {
		return err
	}
	return &ImageGenTimeoutError{Waited: waited, Err: err}
}
