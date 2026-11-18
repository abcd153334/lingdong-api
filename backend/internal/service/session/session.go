// Package session 是登录态的唯一实现：票据签发、校验、续期、撤销、设备列表。
//
// 为什么不再用 JWT：无状态令牌**无法吊销**。改造前的「登出」是一个空实现，
// 用户点了退出、令牌在 24 小时内依然有效；改密也只能靠 token_version 把所有设备
// 一起踢下线。要支持「列设备 / 撤销某一台 / 退出全部 / 改密踢掉其他设备」，
// 就必须有一个服务端实体。
package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/service/authcfg"
)

// Cookie / Header 名（唯一来源：任何地方需要读写会话都从这里取常量）。
const (
	// CookieName 是会话票据的 Cookie 名。
	CookieName = "ld_session"
	// CSRFCookieName 是双提交令牌的 Cookie 名（前端可读，用于回填请求头）。
	CSRFCookieName = "ld_csrf"
	// CSRFHeaderName 是提交令牌的请求头名。
	CSRFHeaderName = "X-CSRF-Token"
)

// TicketPrefix 让票据在日志/截图里一眼可辨（也便于误贴到别处时被识别）。
//
// 导出它是为了**凭证形态分派**：`/api/user/*` 的 `Authorization: Bearer` 既可能是
// 会话票（lds_）也可能是 API KEY（ld-），只有前缀能无歧义地分流。
// 不要拿它当鉴权判据本身 —— 通过前缀只说明「这是哪一类凭证」，是否有效由
// session.Authenticate / apikey.Authenticate 裁决。
const TicketPrefix = "lds_"

// IsTicket 判断给定凭证是否为本包签发的会话票形态。
func IsTicket(s string) bool { return strings.HasPrefix(s, TicketPrefix) }

// Service 是会话服务。
type Service struct {
	gdb *gorm.DB
}

// New 构造会话服务。
func New(gdb *gorm.DB) *Service { return &Service{gdb: gdb} }

// Create 新建一条会话并返回票据明文。
//
// supersededID 是请求携带的旧会话（没有则 0）：登录成功时把它标成 superseded 并撤销，
// 防止「会话固定」——攻击者先给受害者种一个自己知道的票据，等受害者登录后
// 那个票据就变成有效登录态。
func (s *Service) Create(ctx context.Context, uid uint64, ip, ua string, supersededID uint64) (string, *model.UserSession, error) {
	pol := authcfg.Session()
	now := time.Now()

	ticket, hash, err := newTicket()
	if err != nil {
		return "", nil, errs.AuthUnavailable
	}
	row := model.UserSession{
		UID:        uid,
		TicketHash: hash,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(pol.AbsoluteTTL),
		IP:         trimTo(ip, 45),
		UA:         trimTo(ua, 255),
	}
	if err := s.gdb.WithContext(ctx).Create(&row).Error; err != nil {
		return "", nil, err
	}

	if supersededID != 0 && supersededID != row.ID {
		_ = s.Revoke(ctx, supersededID, model.SessionRevokeSuperseded)
	}
	// 超额会话：按「最久没活动」淘汰。失败不影响登录（只是多一条会话）。
	_ = s.trimExcess(ctx, uid, pol.MaxActive)
	return ticket, &row, nil
}

// trimExcess 把该用户的有效会话裁到 limit 条以内（保留最近活跃的）。
func (s *Service) trimExcess(ctx context.Context, uid uint64, limit int) error {
	if limit <= 0 {
		return nil
	}
	var rows []model.UserSession
	if err := s.gdb.WithContext(ctx).
		Where("uid = ? AND revoked_at IS NULL", uid).
		Order("last_seen_at DESC, id DESC").
		Find(&rows).Error; err != nil {
		return err
	}
	if len(rows) <= limit {
		return nil
	}
	ids := make([]uint64, 0, len(rows)-limit)
	for _, r := range rows[limit:] {
		ids = append(ids, r.ID)
	}
	return s.gdb.WithContext(ctx).Model(&model.UserSession{}).
		Where("id IN ?", ids).
		Updates(map[string]any{
			"revoked_at":    time.Now(),
			"revoke_reason": model.SessionRevokeMaxActive,
		}).Error
}

// Authenticate 按票据取会话并校验有效性。
//
// 不在这里加载用户：中间件需要自己决定「用户不存在 / 被禁用」返回什么，
// 而这两个分支的响应语义不同（401 vs 403），混在会话层会让它多一个 DB 依赖。
func (s *Service) Authenticate(ctx context.Context, ticket string) (*model.UserSession, error) {
	ticket = strings.TrimSpace(ticket)
	if ticket == "" {
		return nil, errs.Unauthorized
	}
	var row model.UserSession
	// 等值查唯一索引：天然常量时间，不需要逐字节比较。
	err := s.gdb.WithContext(ctx).Where("ticket_hash = ?", hashTicket(ticket)).First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.SessionExpired
		}
		return nil, err
	}
	if row.RevokedAt != nil {
		return nil, errs.SessionExpired
	}
	now := time.Now()
	if !row.ExpiresAt.After(now) {
		return nil, errs.SessionExpired
	}
	pol := authcfg.Session()
	if now.Sub(row.LastSeenAt) > pol.IdleTTL {
		// 空闲超时：顺手落库，让设备列表能显示「因闲置退出」。
		_ = s.Revoke(ctx, row.ID, model.SessionRevokeIdle)
		return nil, errs.SessionExpired
	}
	return &row, nil
}

// 「本进程上次为某会话写 last_seen_at 的时刻」判定表。
//
// 为什么需要它：已登录请求（门户首页、列表接口）频率很高，每个请求都 UPDATE
// 会显著放大 WAL。多实例下只是让时间戳略旧，不影响正确性（SQL 里还有
// last_seen_at 条件兜底）。
//
// 三件事必须一起做，缺一个都会出问题：
//   - 用互斥锁而不是 sync.Map：Load 再 Store 不是原子的，同一会话的几十个并发
//     请求会同时判成「该写」，节流被完全废掉（恰恰是并发高峰最容易打到）；
//   - 单次判定与登记在同一个临界区，写失败时回滚登记，否则失败后 5 分钟不再重试；
//   - 定期清扫早于节流窗口的键：超过 TouchInterval 的登记本来就没用了，
//     不清就会随历史会话数无界增长（sync.Map 从不淘汰）。
var (
	touchMu    sync.Mutex
	touchSeen  = make(map[uint64]time.Time)
	touchSwept time.Time
)

// claimTouch 判定「这条会话这一刻要不要续期」，要写就顺手登记。
func claimTouch(id uint64, now time.Time, interval time.Duration) bool {
	touchMu.Lock()
	defer touchMu.Unlock()
	if last, ok := touchSeen[id]; ok && now.Sub(last) < interval {
		return false
	}
	touchSeen[id] = now
	// 清扫节奏与节流窗口一致：旧于窗口的登记下次访问必然要重写，留着没有意义。
	if now.Sub(touchSwept) >= interval {
		for k, v := range touchSeen {
			if now.Sub(v) >= interval {
				delete(touchSeen, k)
			}
		}
		touchSwept = now
	}
	return true
}

// releaseTouch 撤销登记（写库失败时调，让下次请求立刻重试）。
func releaseTouch(id uint64) {
	touchMu.Lock()
	delete(touchSeen, id)
	touchMu.Unlock()
}

// Touch 节流地续期 last_seen_at 与 expires_at。
//
// 续期是滑动窗口：剩余有效期不足一半时才把 expires_at 推到 now+absolute_ttl，
// 避免每个请求都改过期时间（那会让「绝对有效期」失去意义）。
func (s *Service) Touch(ctx context.Context, row *model.UserSession) {
	pol := authcfg.Session()
	now := time.Now()
	if !claimTouch(row.ID, now, pol.TouchInterval) {
		return
	}

	// 条件写：影响 0 行是正常情况（另一实例刚写过）。
	err := s.gdb.WithContext(ctx).Exec(`
		UPDATE user_sessions
		SET last_seen_at = ?, expires_at = CASE WHEN expires_at < ? THEN ? ELSE expires_at END
		WHERE id = ? AND revoked_at IS NULL AND last_seen_at < ?`,
		now, now.Add(pol.AbsoluteTTL/2), now.Add(pol.AbsoluteTTL), row.ID, now.Add(-pol.TouchInterval)).Error
	if err != nil {
		// 续期失败不影响本次请求（会话本身仍有效），下次再试。
		releaseTouch(row.ID)
	}
}

// Revoke 撤销单条会话（幂等：已撤销的不再改写原因）。
func (s *Service) Revoke(ctx context.Context, id uint64, reason string) error {
	return s.gdb.WithContext(ctx).Model(&model.UserSession{}).
		Where("id = ? AND revoked_at IS NULL", id).
		Updates(map[string]any{"revoked_at": time.Now(), "revoke_reason": reason}).Error
}

// RevokeOwned 校验归属后撤销；返回 0 行表示「不属于该用户或已失效」。
//
// 一定要带 uid 条件：只按 id 撤销等于允许任何登录用户踢掉别人的所有设备
// （id 是自增的，可枚举）。
func (s *Service) RevokeOwned(ctx context.Context, uid, id uint64, reason string) (int64, error) {
	res := s.gdb.WithContext(ctx).Model(&model.UserSession{}).
		Where("id = ? AND uid = ? AND revoked_at IS NULL", id, uid).
		Updates(map[string]any{"revoked_at": time.Now(), "revoke_reason": reason})
	return res.RowsAffected, res.Error
}

// RevokeAll 撤销该用户全部有效会话。
//
// exceptID 非 0 时保留那一条（「开启 2FA 后只踢其他设备」与「退出全部」是两条
// 不同语义，不能用一个 includeCurrent 布尔糊在一起——布尔只有两个值，
// 而这里需要表达「踢全部」与「踢除某一条」两件事）。
func (s *Service) RevokeAll(ctx context.Context, uid uint64, reason string, exceptID uint64) (int64, error) {
	q := s.gdb.WithContext(ctx).Model(&model.UserSession{}).
		Where("uid = ? AND revoked_at IS NULL", uid)
	if exceptID != 0 {
		q = q.Where("id <> ?", exceptID)
	}
	res := q.Updates(map[string]any{"revoked_at": time.Now(), "revoke_reason": reason})
	return res.RowsAffected, res.Error
}

// List 返回该用户的有效会话（按最近活跃倒序）。
//
// 返回值一定是非 nil 切片：nil 会序列化成 JSON null，前端对 null 调 .length 会整页崩
// （HTTP 200，后端日志干净）。
func (s *Service) List(ctx context.Context, uid uint64) ([]model.UserSession, error) {
	rows := make([]model.UserSession, 0, 8)
	err := s.gdb.WithContext(ctx).
		Where("uid = ? AND revoked_at IS NULL AND expires_at > ?", uid, time.Now()).
		Order("last_seen_at DESC, id DESC").
		Find(&rows).Error
	return rows, err
}

// newTicket 生成票据与其哈希。
func newTicket() (string, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	ticket := TicketPrefix + base64.RawURLEncoding.EncodeToString(raw)
	return ticket, hashTicket(ticket), nil
}

// hashTicket 返回 hex(sha256(ticket))。
//
// 不用 bcrypt：票据是 256 bit 随机值，不存在被猜解的可能，而 bcrypt 会给
// **每一个已登录请求**加约 60ms 开销。
func hashTicket(ticket string) string {
	sum := sha256.Sum256([]byte(ticket))
	return hex.EncodeToString(sum[:])
}

// NewCSRFToken 生成双提交令牌（32 字节，base64url）。
func NewCSRFToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// trimTo 截断到 n 字节（UA / IP 是外部输入，不能任意长度落库）。
func trimTo(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	// 按字节截断可能切坏多字节字符，但这两列只用于人眼辨识，不影响逻辑。
	return s[:n]
}
