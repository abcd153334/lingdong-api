package apikey

import (
	"errors"

	"lingdong-api/internal/pkg/errs"
)

// ErrDef 把本包的鉴权哨兵映射成对外的 HTTP 错误定义，是**唯一**的映射表。
//
// 为什么必须收敛到一处：改造前同一条哨兵在四个地方各写一份 switch
// （/v1 的 TokenAuth、/api/user 的 UserTokenAuth、签发端点、自举端点），
// 于是 `ErrKeyDisabled` 在凭证面是 401（"你带来的钥匙被禁"），
// 在签发面也被写成 401 —— 而那里调用方带的是会话票，"目标钥被封"被报成
// "你的登录失效"，浏览器端一处理就跳登录页。
//
// 返回 (Def, false) 表示"不是本包的哨兵"（典型是数据库错误）：
// 调用方必须自己决定兜底（认证面 → invalid_api_key，业务面 → internal），
// 不要在这里一律折成 401 —— 那会把存储抖动报成"凭证错了"。
//
// 响应**形状**（OpenAI 体 / 统一体）由各面的响应器决定，与本映射无关。
func ErrDef(err error) (errs.Def, bool) {
	switch {
	case errors.Is(err, ErrAuthUnavailable):
		// 依赖不可用 → 503：让接入方退避重试，而不是去换钥。
		return errs.ServiceUnavailable.WithMessage("认证服务暂时不可用，请稍后重试"), true
	case errors.Is(err, ErrKeyExpired):
		return errs.KeyExpired, true
	case errors.Is(err, ErrKeyRetired):
		return errs.KeyRetired, true
	case errors.Is(err, ErrKeyDisabled):
		return errs.KeyDisabled, true
	case errors.Is(err, ErrRotateConflict):
		return errs.KeyRotateBusy, true
	case errors.Is(err, ErrKeyGone):
		// 行已不存在：让调用方查自己的配置，而不是等平台解锁。
		return errs.InvalidAPIKey, true
	case errors.Is(err, ErrNoHostedScopes):
		// 契约里没有任何写端点 = 部署配置问题，不是调用方的错。
		return errs.ServiceUnavailable, true
	default:
		return errs.Def{}, false
	}
}

// DefOr 是 ErrDef 的"带兜底"形式：不是本包哨兵时返回 fallback。
//
// 兜底值必须由调用方给，不能在这里统一成 401 —— 认证面（拿它当"凭证错"）
// 与业务面（拿它当"服务端异常 500"）的正确兜底本来就不同。
func DefOr(err error, fallback errs.Def) errs.Def {
	if def, ok := ErrDef(err); ok {
		return def
	}
	return fallback
}
