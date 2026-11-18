// Package auth 实现账号注册、登录、密码管理与身份标识校验。
//
// 本包**不签发令牌**：登录成功后由 service/session 建立服务端会话，
// 票据通过 httpOnly Cookie 下发。任何"离线可验证的令牌"都不再存在。
package auth

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/pgerr"
	"lingdong-api/internal/service/apikey"
	"lingdong-api/internal/service/authcfg"
	"lingdong-api/internal/service/authevent"
	"lingdong-api/internal/service/captcha"
	"lingdong-api/internal/service/mfa"
	"lingdong-api/internal/service/session"
	"lingdong-api/internal/service/verify"
)

// 用户名规则。
//
// v2 收紧了这一条：**必须包含至少一个字母或下划线**。
// 原因不是审美——登录标识按形态分派（含 @ 当邮箱、纯数字当手机号），
// 允许纯数字用户名会让「123456」这类账号永远无法登录（会被当成手机号去查）。
// usernameShapeRe 只校验「字符集 + 长度」；「至少含一个字母或下划线」由
// usernameAlphaRe 单独判定。
//
// 为什么不用 `^(?=.*[A-Za-z_])[A-Za-z0-9_]{3,20}$` 一条写完：Go 的 regexp 是
// RE2，**不支持 Perl 前瞻** `(?=)`，MustCompile 会直接 panic —— 而且 panic 发生在
// 包 init 阶段，`go build` 完全正常，只有进程启动那一刻才炸（测试里则表现为
// 整个测试二进制崩掉，看起来像测试框架问题）。
var (
	usernameShapeRe = regexp.MustCompile(`^[A-Za-z0-9_]{3,20}$`)
	usernameAlphaRe = regexp.MustCompile(`[A-Za-z_]`)
)

// 密码长度上限。
//
// bcrypt 只取前 72 字节，超出部分**静默截断**：不加上限时，
// 一个 200 字符的密码与它前 72 字符的密码等价，而用户以为后者更安全。
const maxPasswordBytes = 72

// Service 是账号服务。
type Service struct {
	gdb *gorm.DB
}

// New 构造 auth 服务。
func New(gdb *gorm.DB) *Service { return &Service{gdb: gdb} }

// UserView 是登录返回的用户视图（余额为**微**，展示换算由前端做）。
type UserView struct {
	ID       uint64 `json:"id"`
	Username string `json:"username"`
	Role     int    `json:"role"`
	Group    string `json:"group"`
	Balance  int64  `json:"balance"`
	Email    string `json:"email"`
	Phone    string `json:"phone"`
	// AvatarURL 是头像 data URL，空串 = 未设置（前端回落默认头像）。
	// 登录响应就带上它，顶栏不必等 `/portal/me` 返回才显示头像。
	AvatarURL string `json:"avatar_url"`
	// MustChangePassword 只在"口令是系统交接的初始值"时为 true。
	MustChangePassword bool `json:"must_change_password"`
}

// ValidateUsername 校验用户名格式。
func ValidateUsername(s string) error {
	if !usernameShapeRe.MatchString(s) || !usernameAlphaRe.MatchString(s) {
		return errors.New("用户名需为 3-20 位字母、数字或下划线，且至少含一个字母或下划线")
	}
	return nil
}

// ValidatePassword 校验密码：8–72 字节且强度 >= 2。
//
// 规格的唯一来源：注册、改密、找回、管理员重置四处共用本函数。
// 前端不再重复实现一套（两套规则漂移的结果是「前端放行、后端拒绝」，
// 而用户看不懂为什么）。
func ValidatePassword(s string) error {
	if len(s) < 8 {
		return errors.New("密码长度至少 8 位")
	}
	if len(s) > maxPasswordBytes {
		return errors.New("密码过长（最多 72 字节）")
	}
	if PasswordStrength(s) < 2 {
		return errors.New("密码需包含大小写字母、数字、符号中的至少两类")
	}
	return nil
}

// PasswordStrength 返回强度等级（0-4）：大小写、数字、符号四类中命中的类数。
func PasswordStrength(s string) int {
	var lower, upper, digit, symbol bool
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= '0' && r <= '9':
			digit = true
		default:
			symbol = true
		}
	}
	n := 0
	for _, hit := range []bool{lower, upper, digit, symbol} {
		if hit {
			n++
		}
	}
	return n
}

// RegisterInput 是注册入参。
type RegisterInput struct {
	Username        string
	Channel         string // email | sms
	Target          string
	Code            string
	Password        string
	ConfirmPassword string
	AffCode         string
	Agreed          bool
	// 图形验证码（见 service/captcha）。
	CaptchaID     string
	CaptchaAnswer string
	IP            string
}

// Register 创建普通用户。
//
// 唯一性只信数据库约束：预检（Count）仅用于给出友好文案，
// 真正的正确性由唯一索引 + 23505 映射保证。改造前是「Count → Create」，
// 并发两次请求都能通过检查，于是同一邮箱能注册出两个账号。
func (s *Service) Register(ctx context.Context, in RegisterInput) (*model.User, error) {
	rp := authcfg.Register()
	if rp.Mode == authcfg.RegisterClosed {
		return nil, errs.RegisterClosed
	}
	if !db.RedisReady() {
		// 注册必须能过人机验证；没有 Redis 就记不住「这份解已经用过」，
		// 等于同一份解可以无限重放 → 不能放行。
		return nil, errs.AuthUnavailable
	}

	in.Username = strings.TrimSpace(in.Username)
	if err := ValidateUsername(in.Username); err != nil {
		return nil, errs.InvalidParam.WithMessage(err.Error())
	}
	if in.Password != in.ConfirmPassword {
		return nil, errs.InvalidParam.WithMessage("两次输入的密码不一致")
	}
	if err := ValidatePassword(in.Password); err != nil {
		return nil, errs.WeakPassword.WithMessage(err.Error())
	}
	if !in.Agreed {
		return nil, errs.AgreementRequired
	}

	// 人机验证：注册恒要求（Required 是唯一裁决点）。
	if captcha.Required(captcha.SceneRegister, 0) {
		if err := captcha.Verify(ctx, in.CaptchaID, in.CaptchaAnswer); err != nil {
			authevent.Record(ctx, authevent.Event{
				Event: authevent.EventCaptchaFailed, Result: authevent.ResultFail,
				Reason: errcode(err), IP: in.IP, Account: in.Username,
			})
			return nil, err
		}
	}

	channel := strings.TrimSpace(in.Channel)
	if channel == "" {
		channel = verify.ChannelEmail
	}
	target := verify.Normalize(channel, in.Target)

	// 只要填了联系方式就必须验证：未验证的地址绝不入库，
	// 否则等于允许占用他人邮箱（之后「找回密码」的信会发到别人那里）。
	//
	// 「渠道是否启用」只在**真的要用这个渠道**时判定：把它提到 target 判断之前，
	// 会让「不要求验证 + 未选任何渠道」的注册直接 400（报的还是「该绑定方式未启用」），
	// 而运营的意图明明是「注册时不收联系方式」。
	if target != "" {
		if !rp.MethodEnabled(channel) {
			return nil, errs.BindMethodDisabled
		}
		if err := verify.ValidateFormat(channel, target); err != nil {
			return nil, err
		}
		if strings.TrimSpace(in.Code) == "" {
			return nil, errs.ContactVerifyRequired
		}
		if err := verify.Consume(ctx, verify.SceneRegister, channel, target, in.Code); err != nil {
			return nil, err
		}
	} else if rp.ContactVerify == authcfg.ContactVerifyRequired {
		return nil, errs.ContactVerifyRequired
	}

	if rp.Mode == authcfg.RegisterInviteOnly {
		// 邀请码统一大写再比：生成侧恒为大写（affiliate.randCode），绑定侧也会 ToUpper。
		// 这里不做同一归一会让「小写输入」在邀请制下被判无效（403），
		// 而开放注册时却能正常绑定 —— 同一份码两种结果，纯属人为 bug。
		code := strings.ToUpper(strings.TrimSpace(in.AffCode))
		if code == "" {
			return nil, errs.InviteRequired
		}
		var cnt int64
		if err := s.gdb.WithContext(ctx).Model(&model.User{}).
			Where("aff_code = ?", code).Count(&cnt).Error; err != nil {
			return nil, err
		}
		if cnt == 0 {
			// 邀请制下非法邀请码必须报错（开放注册时则静默忽略，只是没有返利）。
			authevent.Record(ctx, authevent.Event{
				Event: authevent.EventInviteRejected, Result: authevent.ResultFail,
				Account: in.Username, IP: in.IP,
			})
			return nil, errs.InviteRequired
		}
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	u := model.User{
		Username:     in.Username,
		PasswordHash: string(hash),
		Role:         1,
		Status:       1,
		Group:        "default",
	}
	if channel == verify.ChannelSMS {
		u.Phone = target
	} else {
		u.Email = target
	}
	// aff_code 列带唯一索引：新用户必须保持 NULL，否则空串会互相冲突。
	if err := s.gdb.WithContext(ctx).Omit("aff_code").Create(&u).Error; err != nil {
		return nil, mapDuplicate(err)
	}

	// 事件在**创建成功之后**记录（事务已提交）：在事务内入队会记下回滚的注册。
	authevent.Record(ctx, authevent.Event{
		UID: u.ID, Account: u.Username, Event: authevent.EventRegister,
		Result: authevent.ResultSuccess, IP: in.IP,
	})
	return &u, nil
}

// LoginInput 是登录入参。
type LoginInput struct {
	Account  string
	Password string
	IP       string
	UA       string
	// CurrentTicket 是请求携带的旧会话票（会话固定防护：登录成功即撤销它）。
	//
	// 传**票据**而不是会话 id：登录路由挂在公开组（没有 UserAuth 中间件），
	// 那里没人把 Cookie 解析成会话 id，`middleware.SessionID(c)` 恒为 0 ——
	// 按 id 传参等于把这段防护写成死代码。解析放在这里还有一个好处：
	// 只有在「旧票确实属于同一个人」时才撤销，别人塞来的票不会误伤。
	CurrentTicket string
	// 图形验证码；只在 Required 为真时被读取。
	CaptchaID     string
	CaptchaAnswer string
}

// LoginResult 是登录第一步的结果。
//
// MFARequired 为 true 时**除 Challenge/ExpiresIn 外全部为空**：
// 这一步绝不建会话、绝不发 Cookie —— 密码只是第一因素，
// 在第二因素通过前发凭证等于把 2FA 降级成「登录后提示绑定」。
type LoginResult struct {
	User    *model.User
	Session *model.UserSession
	Ticket  string
	CSRF    string

	MFARequired bool
	Challenge   string
	ExpiresIn   int
}

// Login 校验凭据并建立会话。
//
// 失败处置的语义（与改造前的 429 硬锁的关键差异）：
//   - 单 IP 超限 → 429（这是唯一会返回 429 的分支）；
//   - 单账号达阈值 → 要求人机验证 + 服务端退避延迟，**不锁死账号**。
//     改造前「3 次错密码锁 60 秒」等于给攻击者一个锁死任意已知账号的开关。
func (s *Service) Login(ctx context.Context, in LoginInput) (*LoginResult, error) {
	if !db.RedisReady() {
		return nil, errs.AuthUnavailable
	}
	pol := authcfg.Login()
	fails := readFails(ctx, failNSLogin, in.Account, in.IP)

	if captcha.Required(captcha.SceneLogin, fails.max()) {
		if err := captcha.Verify(ctx, in.CaptchaID, in.CaptchaAnswer); err != nil {
			authevent.Record(ctx, authevent.Event{
				Event: authevent.EventCaptchaFailed, Result: authevent.ResultFail,
				Reason: errcode(err), IP: in.IP, Account: truncateAccount(in.Account),
			})
			return nil, err
		}
	}

	u, lookupErr := s.findByLogin(ctx, in.Account)
	credOK := lookupErr == nil && bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(in.Password)) == nil
	if !credOK {
		// 用户不存在时也跑一次 bcrypt：短路会让「账号不存在」比「密码错误」
		// 快几十毫秒，响应时间差本身就是账号枚举通道。
		if lookupErr != nil {
			_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(in.Password))
		}
		next := recordFail(ctx, failNSLogin, in.Account, in.IP)
		authevent.Record(ctx, authevent.Event{
			UID: uidOf(u), Account: truncateAccount(in.Account),
			Event: authevent.EventLoginFailed, Result: authevent.ResultFail,
			Reason: "invalid_credential", IP: in.IP, UA: in.UA,
		})
		if next.IP >= pol.IPFailLimit {
			return nil, errs.RateLimited
		}
		if next.Account >= pol.FailThreshold {
			// 真实 sleep：前端不再自己等，否则绕过前端直打接口就没有延迟了。
			backoffSleep(ctx, pol.DelayMax)
			// 退避与「要不要验证码」是两件事：off 也要睡，否则关掉验证码
			// 等于把爆破延迟一起送掉。但 off 时绝不能返回 captcha_required ——
			// 那样前端永远不会亮出验证码，用户被永久要求"完成验证"却无从完成。
			if !captcha.Required(captcha.SceneLogin, next.Account) {
				return nil, errs.InvalidCredential
			}
			return nil, errs.CaptchaRequired
		}
		return nil, errs.InvalidCredential
	}

	clearFails(ctx, failNSLogin, in.Account)
	if u.Status == 2 {
		// 密码正确但账号被禁用：这是一条**运维必须能看见**的事件。
		// 之前这里直接返回，认证事件表里查不到任何痕迹，
		// 用户报「我登不上」而管理员在后台只看到「无异常记录」。
		authevent.Record(ctx, authevent.Event{
			UID: u.ID, Account: u.Username, Event: authevent.EventLoginFailed,
			Result: authevent.ResultFail, Reason: "user_disabled",
			IP: in.IP, UA: in.UA,
		})
		return nil, errs.UserDisabled
	}

	// 密码正确之后才看第二因素：先判 MFA 会把「这个账号有没有开 2FA」
	// 变成不需要密码就能探测的事实（账号枚举 + 定向钓鱼）。
	if enabled, merr := mfa.New(s.gdb).IsEnabled(ctx, u.ID); merr != nil {
		return nil, merr
	} else if enabled {
		// 会话固定防护在**两阶段里也要成立**：第一步把「请求携带的旧会话 id」
		// 存进挑战，第二步建新会话时撤销它。既不能在这里直接撤（用户密码对、
		// 动态码输错会被顺手登出，多设备下表现为「另一台设备突然掉线」），
		// 也不能不撤（2FA 账号的旧票会活过第二步，攻击者预种的票永久有效）。
		sup := s.ticketSessionID(ctx, in.CurrentTicket, u.ID)
		ch, ttl, cerr := mfa.New(s.gdb).CreateChallenge(ctx, u.ID, in.IP, sup)
		if cerr != nil {
			return nil, cerr
		}
		authevent.Record(ctx, authevent.Event{
			UID: u.ID, Account: u.Username, Event: authevent.EventLoginMFARequired,
			Result: authevent.ResultSuccess, IP: in.IP, UA: in.UA,
		})
		return &LoginResult{MFARequired: true, Challenge: ch, ExpiresIn: ttl}, nil
	}
	return s.issueSession(ctx, u, in.IP, in.UA, s.ticketSessionID(ctx, in.CurrentTicket, u.ID))
}

// LoginMFA 完成登录第二步：兑换挑战 + 校验动态码/备份码，成功后建会话。
//
// 挑战里存的是 uid 与出口 IP 指纹，**不存密码**：第二因素通过后不需要再验密码，
// 但必须确认「同一个 uid、同一个 IP」——否则拿到 challenge 的人可以换 IP 爆破。
func (s *Service) LoginMFA(ctx context.Context, challenge, code, ip, ua string) (*LoginResult, error) {
	uid, sup, err := mfa.New(s.gdb).RedeemChallenge(ctx, challenge, ip, code)
	if err != nil {
		authevent.Record(ctx, authevent.Event{
			UID: uid, Event: authevent.EventLoginMFAFailed, Result: authevent.ResultFail,
			Reason: errcode(err), IP: ip, UA: ua,
		})
		return nil, err
	}
	var u model.User
	if err := s.gdb.WithContext(ctx).First(&u, uid).Error; err != nil {
		return nil, errs.SessionExpired
	}
	if u.Status == 2 {
		return nil, errs.UserDisabled
	}
	authevent.Record(ctx, authevent.Event{
		UID: u.ID, Account: u.Username, Event: authevent.EventLoginMFASuccess,
		Result: authevent.ResultSuccess, IP: ip, UA: ua,
	})
	return s.issueSession(ctx, &u, ip, ua, sup)
}

// ticketSessionID 解析请求携带的旧票据，返回会话 id（不属于本人则为 0）。
//
// 只在「旧票确实属于本人」时返回：别人（或攻击者）塞进来的票不能因为
// 受害者登录就被顺手清掉——那会让攻击者能远程把别人的会话踢下线。
//
// 返回 id 而不是在 service 里直接撤销，是因为 2FA 账号要等到第二步
// （动态码通过）才能确定这确实是一次成功登录。
func (s *Service) ticketSessionID(ctx context.Context, ticket string, uid uint64) uint64 {
	if strings.TrimSpace(ticket) == "" {
		return 0
	}
	row, err := session.New(s.gdb).Authenticate(ctx, ticket)
	if err != nil || row.UID != uid {
		return 0
	}
	return row.ID
}

// issueSession 建会话并发票据（登录两个阶段共用，保证两处安全属性一致）。
//
// supersededID 是登录前就在用的旧会话（0 = 没有）。会话固定防护：
// 登录成功后立刻撤销它，攻击者预先种下的票据不会变成有效登录态。
func (s *Service) issueSession(ctx context.Context, u *model.User, ip, ua string, supersededID uint64) (*LoginResult, error) {
	ticket, sess, err := session.New(s.gdb).Create(ctx, u.ID, ip, ua, supersededID)
	if err != nil {
		return nil, err
	}
	csrf, err := session.NewCSRFToken()
	if err != nil {
		return nil, errs.AuthUnavailable
	}
	authevent.Record(ctx, authevent.Event{
		UID: u.ID, Account: u.Username, Event: authevent.EventLogin,
		Result: authevent.ResultSuccess, IP: ip, UA: ua, SessionID: sess.ID,
	})
	return &LoginResult{User: u, Session: sess, Ticket: ticket, CSRF: csrf}, nil
}

// findByLogin 按标识形态分派查询（邮箱 / 手机号 / 用户名）。
//
// 三条独立查询各走各的索引，不用 OR 拼：`username = ? OR email = ?`
// 会让两个索引都退化成位图扫描，而登录是最高频的写路径之一。
func (s *Service) findByLogin(ctx context.Context, raw string) (*model.User, error) {
	account := strings.TrimSpace(raw)
	if account == "" {
		return nil, gorm.ErrRecordNotFound
	}
	var u model.User
	q := s.gdb.WithContext(ctx)
	switch {
	case strings.Contains(account, "@"):
		q = q.Where("email <> '' AND email = ?", verify.Normalize(verify.ChannelEmail, account))
	case isPhoneShape(account):
		q = q.Where("phone <> '' AND phone = ?", verify.Normalize(verify.ChannelSMS, account))
	default:
		// 用户名大小写敏感：注册时原样保存，登录也原样匹配。
		q = q.Where("username = ?", account)
	}
	if err := q.First(&u).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

// isPhoneShape 判断输入是否像手机号（可带 +，6–15 位数字）。
func isPhoneShape(s string) bool {
	body := strings.TrimPrefix(s, "+")
	if len(body) < 6 || len(body) > 15 {
		return false
	}
	for _, r := range body {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ChangePassword 校验旧密码后改密，并撤销该用户**全部**会话（含当前）。
func (s *Service) ChangePassword(ctx context.Context, uid uint64, ip, oldPassword, newPassword string) error {
	if err := s.Reauth(ctx, uid, ip, oldPassword); err != nil {
		return err
	}
	// 自助改密：密码凭证与托管密钥威胁模型正交，豁免托管密钥（避免应用断供死局）。
	return s.setPassword(ctx, uid, newPassword, model.SessionRevokePasswordChanged, false)
}

// Reauth 校验「当前密码」，供改密与敏感操作（绑定/开启二次验证）的重认证使用。
//
// 唯一的密码比对实现，也是唯一的失败计数点：会话被窃取后，攻击者最想拿的
// 就是「用旧密码改密」与「悄悄绑一个自己的验证器」，两处共用同一个
// `pwchange` 命名空间（与登录桶分开，否则连错旧密码会打爆同一出口所有人的登录）。
func (s *Service) Reauth(ctx context.Context, uid uint64, ip, password string) error {
	if !db.RedisReady() {
		return errs.AuthUnavailable
	}
	var u model.User
	if err := s.gdb.WithContext(ctx).First(&u, uid).Error; err != nil {
		return errs.Unauthorized
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		// 已登录会话也可能被用来爆破旧密码（例如令牌被窃取后想彻底接管）。
		// 走独立的 pwchange 命名空间计数，并在超限后要求稍后重试：
		// 这里是已认证操作，429 不会造成账号 DoS。
		//
		// IP 维度也要判：不判的话"连错 20 次旧密码"只会锁住自己这个命名空间，
		// 但会把同出口的**登录**桶打满（历史上两者共用同一个 ipFailKey）。
		pol := authcfg.Login()
		next := recordFail(ctx, failNSPassword, u.Username, ip)
		if next.Account >= pol.FailThreshold || (pol.IPFailLimit > 0 && next.IP >= pol.IPFailLimit) {
			return errs.RateLimited
		}
		return errs.InvalidCredential
	}
	clearFails(ctx, failNSPassword, u.Username)
	return nil
}

// ResetPassword 是「验证码已通过」之后的原子改密动作。
//
// 校验验证码与签发 ticket 在 controller（需要 IP、通道等上下文），
// 本函数只负责「改密 + 踢掉全部会话」，保证这两步落在同一个事务里。
func (s *Service) ResetPassword(ctx context.Context, uid uint64, newPassword string) error {
	// 自助找回：用户本人发起（通过验证码证明身份），与改密同口径豁免托管密钥。
	return s.setPassword(ctx, uid, newPassword, model.SessionRevokePasswordChanged, false)
}

// AdminResetPassword 是管理员重置他人密码（会踢掉该用户全部设备）。
//
// 重置后口令由管理员转述，属于"交接凭证"，因此强制该用户下次登录先改密。
// 管理员重置 = 账号认定失陷：API KEY（含托管）全部禁用；托管密钥的恢复通道
// 是管理台启停端点（AdminKeyStatus），不提供任何用户侧自愈出口。
func (s *Service) AdminResetPassword(ctx context.Context, uid uint64, newPassword string) error {
	if err := s.setPassword(ctx, uid, newPassword, model.SessionRevokePasswordChanged, true); err != nil {
		return err
	}
	return s.gdb.WithContext(ctx).Model(&model.User{}).Where("id = ?", uid).
		Update("must_change_password", true).Error
}

// setPassword 在**同一事务**里改密码并撤销会话与 API KEY。
//
// 为什么要同一事务：先改密码再撤销会话，中间失败会留下「密码已换、旧会话仍有效」
// 的窗口——而用户改密的动机通常就是怀疑凭证泄露。
//
// revokeHosted 决定止血范围：改密/换绑（凭证泄露≠托管密钥泄露，威胁模型正交）传 false
// 豁免托管密钥；管理员强制重置（账号认定失陷）传 true 全禁。
func (s *Service) setPassword(ctx context.Context, uid uint64, newPassword, reason string, revokeHosted bool) error {
	if err := ValidatePassword(newPassword); err != nil {
		return errs.WeakPassword.WithMessage(err.Error())
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return s.gdb.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 改密顺带清掉「初始口令」标记：这是用户自己选的新口令，
		// 不应该在下次登录时再被要求改一次。
		if err := tx.Model(&model.User{}).Where("id = ?", uid).
			Updates(map[string]any{
				"password_hash":        string(hash),
				"must_change_password": false,
			}).Error; err != nil {
			return err
		}
		// 同时禁用 API KEY：改密是"账号可能已被接管"的止血动作，
		// 只撤会话等于止一半 —— 攻击者用会话建的自建 KEY（或已泄露的 KEY）
		// 仍能继续按 user_id 消费，直到过期。托管密钥是否一并禁用由调用方定。
		if revokeHosted {
			if _, err := apikey.RevokeAllTx(tx, uid); err != nil {
				return err
			}
		} else {
			if _, err := apikey.RevokeSelfBuiltTx(tx, uid); err != nil {
				return err
			}
		}
		return tx.Model(&model.UserSession{}).
			Where("uid = ? AND revoked_at IS NULL", uid).
			Updates(map[string]any{"revoked_at": time.Now(), "revoke_reason": reason}).Error
	})
}

// mapDuplicate 把唯一约束冲突翻译成业务错误码。
//
// 预检（Count）只负责体验，正确性靠这里：并发注册时两个请求都能通过预检，
// 只有数据库能裁决。数据库错误原样上抛会让用户看到 500 而不是「用户名已存在」。
func mapDuplicate(err error) error {
	if err == nil {
		return nil
	}
	if !pgerr.IsUnique(err) {
		return err
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "email"):
		return errs.EmailTaken
	case strings.Contains(msg, "phone"):
		return errs.TargetTaken
	case strings.Contains(msg, "username"):
		return errs.UsernameTaken
	}
	// 未知唯一约束：宁可报「已存在」也不要 500，但要带上原文供排查。
	return errs.InvalidParam.WithMessage("账号信息已存在")
}

func uidOf(u *model.User) uint64 {
	if u == nil {
		return 0
	}
	return u.ID
}

// ViewOf 把用户映射成对外视图。
//
// 单一投影点：登录响应、/portal/me、注册回执都用它，
// 避免「某处多回了一个字段」（例如把 PasswordHash 或内部备注带出去）。
func ViewOf(u *model.User) UserView {
	if u == nil {
		return UserView{}
	}
	return UserView{
		ID:        u.ID,
		Username:  u.Username,
		Role:      int(u.Role),
		Group:     u.Group,
		Balance:   u.Balance,
		Email:     u.Email,
		Phone:     u.Phone,
		AvatarURL: u.Avatar,
		// 前端据此把用户强制引导到改密页；后端还有一道 middleware 门禁，
		// 前端跳转只是体验，不是安全边界。
		MustChangePassword: u.MustChangePassword,
	}
}

// errcode 取错误码（用于事件 reason）。
func errcode(err error) string {
	if def, ok := errs.AsDef(err); ok {
		return def.Code
	}
	return "error"
}

func truncateAccount(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 64 {
		return s[:64]
	}
	return s
}

// backoffSleep 在保持可取消的前提下退避一段随机时间（0..max）。
func backoffSleep(ctx context.Context, max time.Duration) {
	if max <= 0 {
		return
	}
	d := randomDelay(max)
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// dummyHash 是一个真实存在的 bcrypt 摘要（口令为随机串），仅用于「账号不存在」时
// 消耗与正常校验相同的 CPU 时间，抹掉登录接口的时序差。
var dummyHash = []byte("$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy")
