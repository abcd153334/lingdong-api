// Package authevent 是认证事件流（登录/注册/改密/撤销…）的唯一写入点。
//
// 三条硬规则：
//  1. **永不阻断主流程**：入队失败只丢弃并计数，不让「记日志」把登录搞挂；
//  2. **绝不在业务事务内写**：必须在事务提交后入队，否则会记录一个已回滚的注册；
//  3. **事件名走白名单**：未登记的字面量拒绝写入并记 ERROR ——
//     拼错的事件名不会报错，只会让「以为记了其实没记」在事后排查时才暴露。
package authevent

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
)

// 事件枚举（白名单）。
const (
	EventRegister           = "auth.register"
	EventLogin              = "auth.login"
	EventLoginFailed        = "auth.login_failed"
	EventLogout             = "auth.logout"
	EventSessionRevoke      = "auth.session_revoke"
	EventSessionRevokeAll   = "auth.session_revoke_all"
	EventPasswordChange     = "auth.password_change"
	EventPasswordReset      = "auth.password_reset"
	EventPasswordResetAdmin = "auth.password_reset_by_admin"
	EventContactBind        = "auth.contact_bind"
	EventCaptchaFailed      = "auth.captcha_failed"
	EventRateLimited        = "auth.rate_limited"
	EventInviteRejected     = "auth.invite_rejected"
	EventLockDegraded       = "auth.lock_degraded"
	EventAuthUnavailable    = "auth.auth_unavailable"

	// 二次验证（TOTP）。登录被拦成两步时，第一步与第二步各自留痕：
	// 只有 login_mfa_required 而没有 success，说明「密码对了但第二因素一直没过」，
	// 这正是撞库者在已有密码的前提下换码重试的形态。
	EventLoginMFARequired = "auth.login_mfa_required"
	EventLoginMFAFailed   = "auth.login_mfa_failed"
	EventLoginMFASuccess  = "auth.login_mfa_success"
	EventMFASetup         = "auth.mfa_setup"
	EventMFAEnable        = "auth.mfa_enable"
	EventMFADisable       = "auth.mfa_disable"
	EventBackupCodeUsed   = "auth.backup_code_used"

	// API KEY 的签发与吊销。
	//
	// 这两条不写 admin_audit（门户自助操作按 v2.0 §4.9.8 不入管理审计），
	// 但**必须**留痕：KEY 是长期有效、能直接花钱的凭证。没有它，用户
	// "我的额度怎么突然少了一大截"只能靠猜，而"某把 KEY 被谁在什么时候
	// 建出来/删掉"是唯一的排查起点。
	// detail 里带 key_id / kind / hosted，足以把事件对到具体那把钥匙。
	EventKeyIssue  = "auth.key_issue"
	EventKeyRevoke = "auth.key_revoke"

	// 管理员对 KEY 的兜底启停（管理台 AppKeysDialog → AdminKeyStatus）。
	// 与签发/吊销分开：用户看到"签发 API KEY"却找不到自己签过的钥匙，
	// 会以为是别人替他建了一把 —— 必须能一眼区分「平台处置」与「自助操作」。
	EventKeyAdminDisable = "auth.key_admin_disable"
	EventKeyAdminEnable  = "auth.key_admin_enable"

	// 托管密钥的自动生命周期事件（滑动续期 / 顶额自动提额）。
	// 这两个是无感自愈动作的留痕：不记它们，"过期了为什么又能用""上限怎么变大了"
	// 就无法与「自动策略」区分开，资金排查会缺一块拼图。
	EventKeyAutoRenew   = "auth.key_auto_renew"
	EventKeyLimitRaised = "auth.key_limit_raised"
	// EventKeyAutoExpire 是「真闲置超期」被落成禁用状态。
	//
	// 与 auto_renew 成对：一个是"还有救，续上"，一个是"确实没在用了，标记失效"。
	// 不记它的后果是门户里突然出现一把"平台禁用"的钥，而没有任何事件能说明
	// 这是系统判定而不是管理员动作。
	EventKeyAutoExpire = "auth.key_auto_expire"

	// EventKeySelfRotate 是接入方用**自己的托管密钥**换新明文（自举轮换）。
	//
	// 与 EventKeyIssue（门户签发/轮换）分开：前者是无人值守的自动动作，
	// 后者背后有真人会话。混在一起时，"某天凌晨密钥被换了三次"会被误判成
	// 有人登录平台操作过。detail 里的 via_prev 还能区分"用主钥换的"与
	// "用窗口期旧钥自救换的"——后者说明某个实例没拿到上一条明文。
	EventKeySelfRotate = "auth.key_self_rotate"
)

// 结果取值。
const (
	ResultSuccess = "success"
	ResultFail    = "fail"
)

var allowedEvents = map[string]bool{
	EventRegister: true, EventLogin: true, EventLoginFailed: true,
	EventLogout: true, EventSessionRevoke: true, EventSessionRevokeAll: true,
	EventPasswordChange: true, EventPasswordReset: true, EventPasswordResetAdmin: true,
	EventContactBind: true, EventCaptchaFailed: true, EventRateLimited: true,
	EventInviteRejected: true, EventLockDegraded: true, EventAuthUnavailable: true,
	EventLoginMFARequired: true, EventLoginMFAFailed: true, EventLoginMFASuccess: true,
	EventMFASetup: true, EventMFAEnable: true, EventMFADisable: true, EventBackupCodeUsed: true,
	// API KEY 系列事件。KeyIssue/KeyRevoke 曾漏登记：controller 一直在写，
	// Record 却全部拒绝丢弃（只留 ERROR 日志）—— 密钥安全事件零留痕的活缺陷。
	EventKeyIssue: true, EventKeyRevoke: true,
	EventKeyAutoRenew: true, EventKeyLimitRaised: true,
	EventKeyAutoExpire:   true,
	EventKeyAdminDisable: true, EventKeyAdminEnable: true,
	EventKeySelfRotate: true,
}

// Event 是一条待写入的认证事件。
type Event struct {
	UID       uint64
	Account   string
	Event     string
	Result    string
	Reason    string
	IP        string
	UA        string
	SessionID uint64
	Detail    map[string]any
}

const (
	queueCap   = 4096
	batchSize  = 50
	flushEvery = 200 * time.Millisecond
)

var (
	queue   chan Event
	started atomic.Bool
	dropped atomic.Int64
	once    sync.Once
	stopCh  chan struct{}
	doneCh  chan struct{}
)

// Start 启动后台写协程。由应用启动流程调用一次（重复调用无副作用）。
func Start() {
	once.Do(func() {
		queue = make(chan Event, queueCap)
		stopCh = make(chan struct{})
		doneCh = make(chan struct{})
		started.Store(true)
		go loop()
	})
}

// Stop 停止接收并尽力 flush（最多等 5 秒）。
func Stop() {
	if !started.Load() {
		return
	}
	close(stopCh)
	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		logger.L().Warn("认证事件写入未在 5 秒内完成，放弃剩余队列")
	}
}

// Record 非阻塞入队。
//
// 必须在**业务事务提交之后**调用：事务里入队会在回滚后留下一条
// 「注册成功」的假记录，比不记更糟。
func Record(_ context.Context, ev Event) {
	if !allowedEvents[ev.Event] {
		// 宁可丢也不写未知事件：未知事件名会让统计口径出现幽灵分类。
		logger.L().Error("未登记的认证事件被拒绝写入", zap.String("event", ev.Event))
		return
	}
	if !started.Load() {
		return
	}
	if ev.Result == "" {
		ev.Result = ResultSuccess
	}
	select {
	case queue <- ev:
	default:
		// 队列满：宁可丢事件也不能阻塞登录。计数并在下一次丢弃时告警。
		if n := dropped.Add(1); n%100 == 1 {
			logger.L().Warn("认证事件队列已满，开始丢弃", zap.Int64("dropped", n))
		}
	}
}

// Dropped 返回累计丢弃条数（测试与监控用）。
func Dropped() int64 { return dropped.Load() }

func loop() {
	defer close(doneCh)
	ticker := time.NewTicker(flushEvery)
	defer ticker.Stop()

	buf := make([]Event, 0, batchSize)
	flush := func() {
		if len(buf) == 0 {
			return
		}
		write(buf)
		buf = buf[:0]
	}
	for {
		select {
		case ev := <-queue:
			buf = append(buf, ev)
			if len(buf) >= batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-stopCh:
			// 关闭前把队列里剩下的都取出来（不阻塞：用非阻塞循环）。
			for {
				select {
				case ev := <-queue:
					buf = append(buf, ev)
					if len(buf) >= batchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

// write 批量落库。
//
// 失败只记 Warn 不重试：审计是观测手段，重试会在数据库抖动时把队列反复打满，
// 最终连正常的登录日志都丢。
func write(batch []Event) {
	if db.DB == nil {
		return
	}
	rows := make([]model.AuthEvent, 0, len(batch))
	now := time.Now()
	for _, ev := range batch {
		row := model.AuthEvent{
			UID:       ev.UID,
			Account:   truncate(ev.Account, 64),
			Event:     ev.Event,
			Result:    ev.Result,
			Reason:    truncate(ev.Reason, 32),
			IP:        truncate(ev.IP, 45),
			UA:        truncate(ev.UA, 255),
			SessionID: ev.SessionID,
			CreatedAt: now,
		}
		if len(ev.Detail) > 0 {
			row.Detail = model.JSON(marshalDetail(ev.Detail))
		}
		rows = append(rows, row)
	}
	if err := db.DB.CreateInBatches(rows, batchSize).Error; err != nil {
		logger.L().Warn("认证事件写入失败", zap.Int("count", len(rows)), zap.Error(err))
	}
}

func marshalDetail(m map[string]any) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
