package auth

import (
	"context"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/service/apikey"
	"lingdong-api/internal/service/authevent"
	"lingdong-api/internal/service/verify"
)

// SendContactCode 给「换绑联系方式」发验证码（已登录场景）。
//
// targetUnchanged 只在这里生效：改造前这个判断被错误地放进了通用发码函数，
// 于是「给**自己**的邮箱发密码重置码」会被判成「目标地址未变更」而拒绝 ——
// 忘记密码功能整条链路因此不可用。
func (s *Service) SendContactCode(ctx context.Context, uid uint64, channel, target, ip string) (int, error) {
	if channel != verify.ChannelEmail && channel != verify.ChannelSMS {
		return 0, errs.InvalidParam
	}
	var u model.User
	if err := s.gdb.WithContext(ctx).First(&u, uid).Error; err != nil {
		return 0, errs.Unauthorized
	}
	normalized := verify.Normalize(channel, target)
	if err := verify.ValidateFormat(channel, normalized); err != nil {
		return 0, err
	}
	if sameContact(&u, channel, normalized) {
		return 0, errs.TargetUnchanged
	}
	return verify.Issue(ctx, verify.IssueInput{
		Scene: verify.SceneContact, Channel: channel, Target: normalized, UID: uid, IP: ip,
	})
}

// BindContact 消费验证码后绑定 / 换绑联系方式。
//
// 换绑是**账号接管的高危操作**：拿到验证码只证明「拥有新地址」，
// 因此成功后会撤销该用户**除当前会话外**的全部会话 ——
// 若账号已被他人登录，原主换绑邮箱的动作会立刻把对方踢下线。
func (s *Service) BindContact(ctx context.Context, uid, currentSessionID uint64, channel, target, code string) (string, string, int64, error) {
	if channel != verify.ChannelEmail && channel != verify.ChannelSMS {
		return "", "", 0, errs.InvalidParam
	}
	var u model.User
	if err := s.gdb.WithContext(ctx).First(&u, uid).Error; err != nil {
		return "", "", 0, errs.Unauthorized
	}
	normalized := verify.Normalize(channel, target)
	if err := verify.ValidateFormat(channel, normalized); err != nil {
		return "", "", 0, err
	}
	if sameContact(&u, channel, normalized) {
		return "", "", 0, errs.TargetUnchanged
	}
	if err := verify.Consume(ctx, verify.SceneContact, channel, normalized, code); err != nil {
		return "", "", 0, err
	}

	col := "email"
	if channel == verify.ChannelSMS {
		col = "phone"
	}
	if err := s.gdb.WithContext(ctx).Model(&model.User{}).
		Where("id = ?", uid).Update(col, normalized).Error; err != nil {
		// 唯一索引兜底：并发绑定同一地址时，预检（若有）会让两个请求都通过。
		return "", "", 0, mapDuplicate(err)
	}

	// 换绑后踢掉其他设备（保留当前），让原主能立刻切断可能已泄露的登录态。
	_ = s.gdb.WithContext(ctx).Model(&model.UserSession{}).
		Where("uid = ? AND id <> ? AND revoked_at IS NULL", uid, currentSessionID).
		Updates(map[string]any{
			"revoked_at":    time.Now(),
			"revoke_reason": model.SessionRevokeUserRevoked,
		}).Error

	// 换绑同样要禁用自建 API KEY：与改密同理，这是账号接管后的止血动作。
	// 托管密钥豁免（威胁模型正交，误禁会造成应用断供死局）。
	revokedKeys, keyErr := apikey.RevokeSelfBuilt(ctx, uid)
	if keyErr != nil {
		// 撤 KEY 失败不该让换绑整体失败（邮箱已经改了，会话也踢了），
		// 但必须留痕 —— 否则"换绑了但旧 KEY 还能花钱"会静默存在。
		logger.L().Warn("换绑后禁用 API KEY 失败", zap.Uint64("uid", uid), zap.Error(keyErr))
		revokedKeys = 0
	}

	authevent.Record(ctx, authevent.Event{
		UID: uid, Account: u.Username, Event: authevent.EventContactBind,
		Result: authevent.ResultSuccess, SessionID: currentSessionID,
		Detail: map[string]any{"channel": channel, "revoked_keys": revokedKeys},
	})

	var updated model.User
	if err := s.gdb.WithContext(ctx).First(&updated, uid).Error; err != nil {
		return "", "", revokedKeys, err
	}
	return updated.Email, updated.Phone, revokedKeys, nil
}

// sameContact 判断目标是否与当前绑定值相同。
func sameContact(u *model.User, channel, target string) bool {
	if channel == verify.ChannelSMS {
		return u.Phone != "" && u.Phone == target
	}
	return u.Email != "" && u.Email == target
}
