// Package upload 实现「文件上传」域（v2.0 §4.3.2）：四重限额 + 扩展名/魔数双校验 +
// 多后端存储落库 + 24h 有效期直链（/f/）+ 到期清理。
//
// 与任务产物（files 表）分层隔离：uploads 为开发者临时素材，files 为任务持久产物，两者互不转换。
package upload

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"io"
	"path/filepath"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/memlimit"
	"lingdong-api/internal/pkg/metrics"
	"lingdong-api/internal/pkg/ulid"
	"lingdong-api/internal/pkg/uploadcfg"
	"lingdong-api/internal/service/storage"
)

// 域内错误（由 controller 映射到 errs 文件域错误码）。
var (
	ErrDisabled           = errors.New("文件上传已关闭")
	ErrMissing            = errors.New("缺少上传文件")
	ErrEmpty              = errors.New("文件内容为空")
	ErrTooLarge           = errors.New("文件超过单文件上限")
	ErrTypeUnsupported    = errors.New("不支持的文件类型")
	ErrDailyLimit         = errors.New("今日上传次数已达上限")
	ErrStorageFull        = errors.New("在存容量已达上限")
	ErrConcurrency        = errors.New("并发上传数已达上限")
	ErrNotFound           = errors.New("文件不存在")
	ErrExpired            = errors.New("文件已过期")
	ErrStorageUnavailable = errors.New("存储服务不可用")
	// ErrStorageGone 表示这条记录当初写入的后端已从配置里消失（配置问题，不是抖动）。
	//
	// 与 ErrStorageUnavailable 分开的理由：那一个是「稍后重试可能成功」，
	// 这一个是「重试一万次也不会成功，要管理员把后端加回来」。
	ErrStorageGone = errors.New("文件所在的存储后端已不可用")
	// ErrStorageNotConfigured 表示平台还没选存储位置（system_config.storage 缺失/active 为空）。
	//
	// 与 ErrStorageUnavailable 分开：那个是「后端抖了」，这个是「写哪都没定」。
	// **绝不回落本地磁盘** —— 否则文件会写进容器层，重建即消失，而管理台一切正常。
	ErrStorageNotConfigured = errors.New("平台尚未配置存储位置")
)

// storageDead 判断错误是否属于「后端没了」（配置问题）。
func storageDead(err error) bool {
	return errors.Is(err, storage.ErrBackendGone) || errors.Is(err, storage.ErrBackendMissing)
}

// concurrencyKeyPrefix 是上传并发闸的 Redis 键前缀（用户维度）。
const concurrencyKeyPrefix = "ld:up:cc:"

// Item 是 uploads 的对外视图（api.upload 对象）。
//
// 只保留调用方需要的字段：storage_backend（s3/minio/none）属于**部署内部信息**，
// 一个词就能说明对象存哪儿、是不是自建 —— 用户端零收益，故不下发。
// 需要看落盘后端的场景是管理台运维，它读的是 uploads 表本身，不经过这里。
type Item struct {
	ID               string    `json:"id"`
	Filename         string    `json:"filename"`
	SafeName         string    `json:"safe_name"`
	SizeBytes        int64     `json:"size_bytes"`
	ContentType      string    `json:"content_type"`
	URL              string    `json:"url"`
	KeyID            uint64    `json:"key_id"`
	KeyName          string    `json:"key_name,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	ExpiresAt        time.Time `json:"expires_at"`
	RemainingSeconds int64     `json:"remaining_seconds"`
}

// CreateInput 是一次上传请求；Size 为 multipart 声明的字节数（用于限额预检与对象长度）。
type CreateInput struct {
	UserID   uint64
	KeyID    uint64
	Filename string
	Size     int64
	Reader   io.Reader
}

// Create 执行上传：总闸 → 类型 → 限额 → 并发闸 → 日限/在存 → 魔数 → （压缩）→ 落存储 → 落库。
//
// 类型与体积全部来自 `system_config.uploads`（唯一真相源 `uploadcfg`）：
// 代码里不再有扩展名白名单，也没有「全局单文件上限」这种与配置并行的第二口径。
func Create(ctx context.Context, in CreateInput) (*model.Upload, error) {
	s := uploadcfg.Current()
	if !s.Enabled {
		return nil, ErrDisabled
	}
	ext := uploadcfg.NormalizeExt(filepath.Ext(in.Filename))
	if _, ok := s.TypeOf(ext); !ok {
		return nil, ErrTypeUnsupported
	}
	if in.Size <= 0 {
		return nil, ErrEmpty
	}
	if in.Size > s.LimitBytes(ext) {
		return nil, ErrTooLarge
	}

	release, ok := acquireConcurrency(s.Concurrency, in.UserID)
	if !ok {
		return nil, ErrConcurrency
	}
	defer release()

	if err := checkDailyLimit(ctx, in.quotaSubject(), s.DailyLimitPerKey); err != nil {
		return nil, err
	}
	if err := checkUserTotal(ctx, in.UserID, s.UserTotalMB, in.Size); err != nil {
		return nil, err
	}

	// 嗅探文件头：先判类型合法性，再流式写入存储（避免整文件驻留内存）。
	head := make([]byte, 512)
	n, err := io.ReadFull(in.Reader, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, ErrEmpty
	}
	if n == 0 {
		return nil, ErrEmpty
	}
	if !typeGate(s, ext, head[:n]) {
		return nil, ErrTypeUnsupported
	}

	// 压缩在这一步决定字节：未启用/不适用时仍是「头 + 流」，零整图内存。
	body, size, outExt, mime, perr := prepareBody(ctx, s, ext, head[:n], in.Reader, in.Size)
	if perr != nil {
		return nil, perr
	}

	id := ulid.New()
	name := in.Filename
	if outExt != ext {
		// 变了格式就把后缀同步过去，否则下载下来是「.webp 内容的 .png 文件」。
		name = withExt(in.Filename, outExt)
	}
	safeName := SafeName(name)
	key := storageKey(in.UserID, id, safeName)
	// 长度只认实际要写的那一份（压缩后是压缩体，未压缩是声明大小）：
	// Put 内部按 io.LimitReader(r, size) 读取，多出来的字节根本进不了对象。
	ref, err := storage.PutActive(ctx, key, body, size, mime)
	if err != nil {
		if errors.Is(err, storage.ErrNotConfigured) {
			return nil, ErrStorageNotConfigured
		}
		return nil, ErrStorageUnavailable
	}

	now := time.Now().UTC()
	rec := &model.Upload{
		ID: id, UserID: in.UserID, KeyID: in.KeyID,
		Filename: truncateRunes(name, 255), SafeName: safeName,
		SizeBytes: size, ContentType: mime,
		StorageBackend: ref.Backend, StorageBucket: ref.Bucket, StorageKey: ref.Key,
		CreatedAt: now, ExpiresAt: now.Add(time.Duration(s.RetentionHours) * time.Hour),
	}
	if err := db.DB.WithContext(ctx).Create(rec).Error; err != nil {
		// 落库失败不留孤儿对象：补偿失败只记指标（对象位置已无从查证，
		// 但至少不能把它伪装成「上传成功」）。
		if _, derr := storage.DeleteRefVerified(ctx, ref); derr != nil {
			logStoreOrphan(ref, derr)
		}
		return nil, err
	}
	return rec, nil
}

// logStoreOrphan 记录「对象已写入、记录没落库、补偿删除也没成功」的孤儿。
//
// 不重试、不阻塞响应：这条路径本身已经在返回数据库错误；
// 唯一的补救是让 key 出现在日志里（对象存储侧没有 DB 行就再也找不到它）。
func logStoreOrphan(ref storage.Ref, err error) {
	logger.L().Warn("上传补偿删除失败，可能留下孤儿对象",
		zap.String("backend", ref.Backend), zap.String("bucket", ref.Bucket),
		zap.String("key", ref.Key), zap.Error(err))
}

// typeGate 判定「后缀 + 文件头」是否匹配。
//
// 双校验的唯一实现：后缀决定「属于哪一类」，文件头决定「真实是什么」，
// 两者必须同族。抽成函数是为了让测试打的是同一条判据（复制一份必然漂移）。
func typeGate(s uploadcfg.Settings, ext string, head []byte) bool {
	kind, ok := s.TypeOf(ext)
	if !ok {
		return false
	}
	family, danger := sniff(head)
	return !danger && uploadcfg.FamiliesForKind(kind)[family]
}

// withExt 把文件名换成指定后缀（压缩改变输出格式时用）。
//
// 只换最后一段后缀，其余保持原样：`报告.2024.png` 变成 `报告.2024.webp`，
// 而不是在整串里替换所有 `.png`。
func withExt(filename, outExt string) string {
	base := filepath.Base(strings.ReplaceAll(filename, "\\", "/"))
	if base == "." || base == "/" {
		base = ""
	}
	if i := strings.LastIndex(base, "."); i > 0 {
		base = base[:i]
	}
	if base == "" {
		base = "file"
	}
	return base + "." + outExt
}

// QuotaSubject 是「日限额按谁计数」的唯一判据。
//
// 有 API KEY 按 **KEY** 计：这是对外承诺（一把 KEY 一天 N 次，与用户名下有几把 KEY 无关）。
// 门户上传没有 KEY，必须按**账号**计。
//
// 为什么必须显式区分：改造前门户传 KeyID=0 进同一个 `key_id = ?` 条件，语义变成
// 「所有门户上传共用一个桶」—— A 传满之后 B 就传不了了。这个 bug 只在多人使用时出现，
// 单人验收永远看不到，而它会稳定地把「平台不能用」反馈给后来的人。
type QuotaSubject struct {
	KeyID  uint64
	UserID uint64
}

// of 返回该上传归属的日限额计数主体。
func (in CreateInput) quotaSubject() QuotaSubject {
	return QuotaSubject{KeyID: in.KeyID, UserID: in.UserID}
}

// countQuery 构造「本自然日内、属于该主体」的计数查询。
// 列名由代码给出，不拼接任何用户输入。
func (q QuotaSubject) countQuery(ctx context.Context) *gorm.DB {
	m := db.DB.WithContext(ctx).Model(&model.Upload{}).Where("created_at >= ?", dayStart())
	if q.KeyID != 0 {
		return m.Where("key_id = ?", q.KeyID)
	}
	// 账号主体统计**该用户当日全部上传**（含走 API KEY 的那些）：
	// 只数 key_id = 0 会让人用「一半门户一半 API」绕过门户的日限。
	return m.Where("user_id = ?", q.UserID)
}

// checkDailyLimit 按计数主体统计「UTC+8 自然日」内上传次数。
func checkDailyLimit(ctx context.Context, subject QuotaSubject, limit int64) error {
	if limit <= 0 {
		return nil
	}
	var n int64
	if err := subject.countQuery(ctx).Count(&n).Error; err != nil {
		return err
	}
	if n >= limit {
		return ErrDailyLimit
	}
	return nil
}

// checkUserTotal 校验「在存容量 + 本次声明大小」是否超出账号上限。
func checkUserTotal(ctx context.Context, userID uint64, totalMB int64, incoming int64) error {
	if totalMB <= 0 {
		return nil
	}
	var used int64
	if err := db.DB.WithContext(ctx).Model(&model.Upload{}).
		Where("user_id = ? AND expires_at > ?", userID, time.Now()).
		Select("COALESCE(SUM(size_bytes),0)").Scan(&used).Error; err != nil {
		return err
	}
	if used+incoming > totalMB<<20 {
		return ErrStorageFull
	}
	return nil
}

// uploadCCLeakTTL 是上传并发计数的自愈上限。
//
// 与 KEY 并发闸（middleware/auth.go 的 ccLeakTTL）同一个理由：计数只有 INCR/DECR
// 一对，进程被 SIGKILL 时 DECR 不会执行，计数卡在正值 —— 没有 TTL 的话该用户
// **永久**被拒上传，只能人工 DEL。这里曾漏了 TTL（真机压测发现），
// 而 KEY 闸一直有，两者不一致的症状是"同一个用户调 API 正常、传文件永远 429"。
const uploadCCLeakTTL = 2 * time.Hour

// uploadAcquireScript 一次往返完成「计数 + 补 TTL」。
//
// 为什么不能只在 n==1 时挂 TTL：旧版本泄漏的键**已经存在且没有 TTL**，
// 只在首次创建时设置意味着它们永远不会被修复（原子自愈比"新键才有 TTL"重要）。
// 每次 acquire 都检查 pttl 并把缺失的 TTL 补上，泄漏最多再活 2h。
var uploadAcquireScript = redis.NewScript(`
local n = redis.call("incr", KEYS[1])
if redis.call("pttl", KEYS[1]) < 0 then
  redis.call("pexpire", KEYS[1], ARGV[1])
end
return n`)

// uploadReleaseScript 归还额度，减到负数时钳回 0。
//
// 钳零与 KEY 闸同理：TTL 到期后键消失，迟到的 DECR 会把它重建成 -1，
// 下一次 INCR 从 0 开始 —— 等于白送一个并发额度。
var uploadReleaseScript = redis.NewScript(`
local v = redis.call("decr", KEYS[1])
if v < 0 then redis.call("set", KEYS[1], 0) return 0 end
return v`)

// acquireConcurrency 上传并发闸（用户维度）。
//
// Redis 不可用/脚本报错时走**降额放行**（进程内 limit/6），不是全放行：
// 上传是真写存储、真耗带宽的路径，Redis 一挂就无限并发，等于把最脆弱的时候
// 交给任何一个登录用户。取向与 Key 维度两道闸（fail-local）一致。
func acquireConcurrency(limit int64, userID uint64) (func(), bool) {
	if limit <= 0 {
		return func() {}, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	key := fmt.Sprintf("%s%d", concurrencyKeyPrefix, userID)
	if !db.RedisReady() {
		// 降级用**进程内信号量**而不是按分钟计数：这是「同时几个在传」，
		// 换成速率会把「连着传 10 个、任何时刻只有 1 个在传」的用户拦下来。
		return memlimit.Concurrent("upload:"+strconv.FormatUint(userID, 10), int(limit))
	}
	n, err := uploadAcquireScript.Run(ctx, db.RDB, []string{key},
		int64(uploadCCLeakTTL/time.Millisecond)).Int64()
	if err != nil {
		metrics.Inc(metrics.NameRateLimitDegraded, "scope", "upload", "result", "allow")
		return memlimit.Concurrent("upload:"+strconv.FormatUint(userID, 10), int(limit))
	}
	if n > limit {
		uploadReleaseScript.Run(context.Background(), db.RDB, []string{key})
		return func() {}, false
	}
	return func() { uploadReleaseScript.Run(context.Background(), db.RDB, []string{key}) }, true
}

// dayStart 返回 UTC+8 自然日 00:00 的 UTC 时刻。
func dayStart() time.Time {
	now := time.Now().In(time.FixedZone("CST", 8*3600))
	y, m, d := now.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, now.Location()).UTC()
}

// Direct 直链取回所需的存储句柄与记录。
type Direct struct {
	Upload *model.Upload
	Body   io.ReadCloser
	// Size 是本次返回的字节数（区间请求时为区间长度）。
	Size int64
	// Total 是对象总长度（区间响应算 Content-Range 用；未知为 -1）。
	Total int64
}

// OpenDirect 按 file_id 打开直链内容；不存在/已过期/对象缺失一律返回 ErrExpired（不泄露存在性）。
//
// 不带归属校验：直链域（`/f/{id}/{name}`）靠「URL 本身就是凭证 + 24 小时过期」保护，
// 挂 TokenAuth 会让 `<img src>` 无法使用。要归属校验的走 OpenScoped。
func OpenDirect(ctx context.Context, id string) (*Direct, error) {
	return openDirect(ctx, id, nil, 0, 0, false)
}

// OpenDirectRange 按区间打开直链内容（HTTP Range / 播放器拖动）。
func OpenDirectRange(ctx context.Context, id string, offset, length int64) (*Direct, error) {
	if offset < 0 {
		offset = 0
	}
	return openDirect(ctx, id, nil, offset, length, true)
}

// OpenScoped 按**归属**打开文件内容（`GET /v1/files/{id}/content` 用）。
//
// 与 OpenDirect 的区别只有一个：多一道 user/key 归属校验。两个入口刻意共用
// `openDirect` —— 拆成两份实现时，「过期判据」「对象缺失判据」迟早一处改一处漏，
// 而漏的那一处会把别人的文件当自己的给出去。
//
// 归属不符与不存在一律返回 ErrNotFound（不是 ErrExpired）：调用方据此返回 404，
// 与「不属于你 = 不存在」的统一语义一致，也不泄露「这个 id 存在」。
func OpenScoped(ctx context.Context, id string, scope Scope, offset, length int64, ranged bool) (*Direct, error) {
	if offset < 0 {
		offset = 0
	}
	return openDirect(ctx, id, &scope, offset, length, ranged)
}

// Get 按归属取单个文件的元信息（`GET /v1/files/{id}`）。
//
// 「过期即不存在」：过期件返回 ErrNotFound，与 Delete 一致。返回 410 会让客户端
// 区分出「曾经存在」，而文件号本身不是敏感信息，没有必要泄露这个时间维度。
func Get(ctx context.Context, id string, scope Scope, base string) (Item, error) {
	var rec model.Upload
	if err := db.DB.WithContext(ctx).Where("id = ?", id).First(&rec).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return Item{}, ErrNotFound
		}
		return Item{}, err
	}
	if rec.UserID != scope.UserID || (scope.KeyID != nil && rec.KeyID != *scope.KeyID) {
		return Item{}, ErrNotFound
	}
	if !rec.ExpiresAt.After(time.Now()) {
		return Item{}, ErrNotFound
	}
	return View(&rec, keyNames(ctx, []model.Upload{rec})[rec.KeyID], base), nil
}

// openDirect 是取回内容的唯一实现（全量与区间共用同一套校验，避免两处判断漂移）。
func openDirect(ctx context.Context, id string, scope *Scope, offset, length int64, ranged bool) (*Direct, error) {
	if id == "" || len(id) > 26 {
		return nil, ErrExpired
	}
	var rec model.Upload
	if err := db.DB.WithContext(ctx).Where("id = ?", id).First(&rec).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrExpired
		}
		return nil, err
	}
	if scope != nil && (rec.UserID != scope.UserID || (scope.KeyID != nil && rec.KeyID != *scope.KeyID)) {
		return nil, ErrNotFound
	}
	if !rec.ExpiresAt.After(time.Now()) {
		return nil, ErrExpired
	}
	// 归属按记录走：切换过后端/桶之后，这条记录仍在**当初写入的那个位置**上。
	ref := storage.Ref{Backend: rec.StorageBackend, Bucket: rec.StorageBucket, Key: rec.StorageKey}
	if !ranged {
		body, size, err := storage.OpenRef(ctx, ref)
		if err != nil {
			switch {
			case errors.Is(err, storage.ErrObjectNotFound):
				return nil, ErrExpired
			case storageDead(err):
				return nil, ErrStorageGone
			}
			return nil, ErrStorageUnavailable
		}
		return &Direct{Upload: &rec, Body: body, Size: size, Total: size}, nil
	}
	total := rec.SizeBytes
	if total > 0 {
		body, err := storage.OpenRangeRef(ctx, ref, offset, length)
		switch {
		case err == nil:
			size := length
			if size <= 0 {
				size = total - offset
			}
			return &Direct{Upload: &rec, Body: body, Size: size, Total: total}, nil
		case errors.Is(err, storage.ErrRangeUnsupported):
			// 落到下面的「全量 + 跳过前缀」：语义正确，只是多读一些字节。
		case errors.Is(err, storage.ErrObjectNotFound):
			return nil, ErrExpired
		case storageDead(err):
			return nil, ErrStorageGone
		default:
			return nil, ErrStorageUnavailable
		}
	}
	body, size, err := storage.OpenRef(ctx, ref)
	if err != nil {
		switch {
		case errors.Is(err, storage.ErrObjectNotFound):
			return nil, ErrExpired
		case storageDead(err):
			return nil, ErrStorageGone
		}
		return nil, ErrStorageUnavailable
	}
	if offset > 0 {
		if _, err := io.CopyN(io.Discard, body, offset); err != nil {
			body.Close()
			return nil, ErrExpired
		}
	}
	out := &Direct{Upload: &rec, Body: body, Total: size}
	switch {
	case length > 0:
		out.Size = length
		out.Body = &limitedReadCloser{Reader: io.LimitReader(body, length), Closer: body}
	case size > 0:
		out.Size = size - offset
	default:
		out.Size = -1
	}
	return out, nil
}

// limitedReadCloser 把 LimitReader 与底层流句柄的关闭合并。
type limitedReadCloser struct {
	io.Reader
	io.Closer
}

// Scope 是文件归属范围：KeyID 为空表示不限 KEY（门户/管理台）。
type Scope struct {
	UserID uint64
	KeyID  *uint64
}

// ListFilter 是上传列表筛选。
type ListFilter struct {
	UserID uint64
	KeyID  uint64 // 0 表示全部 KEY
	Offset int
	Limit  int
	// OrderBy 是已过白名单校验的 ORDER BY 片段（空 = 历史默认 `created_at DESC, id DESC`）。
	//
	// 白名单与"对外字段名 → 列"的映射都在 handler（pkg/sorting），service 只认片段。
	OrderBy string
}

// List 返回未过期上传件与总数（默认按创建时间倒序）；base 为直链站点前缀。
func List(ctx context.Context, f ListFilter, base string) ([]Item, int64, error) {
	q := db.DB.WithContext(ctx).Model(&model.Upload{}).
		Where("user_id = ? AND expires_at > ?", f.UserID, time.Now())
	if f.KeyID > 0 {
		q = q.Where("key_id = ?", f.KeyID)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	// 显式回落：`Order("")` 在 GORM 里是语法错误，不是"不排序"。
	orderBy := f.OrderBy
	if orderBy == "" {
		orderBy = "created_at DESC, id DESC"
	}
	var rows []model.Upload
	if err := q.Order(orderBy).Offset(f.Offset).Limit(f.Limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	names := keyNames(ctx, rows)
	items := make([]Item, 0, len(rows))
	for i := range rows {
		items = append(items, View(&rows[i], names[rows[i].KeyID], base))
	}
	return items, total, nil
}

// Delete 删除上传件：先清存储对象再删记录。
// 「过期 = 不存在」单一语义（v2.0 §4.3.2）：已过期件与不存在/非本归属件一律返回 ErrNotFound。
func Delete(ctx context.Context, id string, scope Scope) error {
	var rec model.Upload
	if err := db.DB.WithContext(ctx).Where("id = ?", id).First(&rec).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	if rec.UserID != scope.UserID || (scope.KeyID != nil && rec.KeyID != *scope.KeyID) {
		return ErrNotFound
	}
	if !rec.ExpiresAt.After(time.Now()) {
		return ErrNotFound
	}
	// 用户主动删除与自动清理走**同一个**删除判据：404 不等于删掉了。
	if _, derr := storage.DeleteRefVerified(ctx, storage.Ref{
		Backend: rec.StorageBackend, Bucket: rec.StorageBucket, Key: rec.StorageKey,
	}); derr != nil {
		if storageDead(derr) {
			return ErrStorageGone
		}
		return ErrStorageUnavailable
	}
	return db.DB.WithContext(ctx).Delete(&model.Upload{}, "id = ?", id).Error
}

// Stats 是账号维度的上传用量（门户配额条）。
type Stats struct {
	TodayCount int64 `json:"today_count"`
	TotalBytes int64 `json:"total_bytes"`
}

// StatsOf 返回账号当日上传次数与在存容量。
//
// 计数口径与门户上传的日限额**同一处**（QuotaSubject）：两处各写一个 WHERE，
// 就会变成「配额条显示还有额度、实际已经传不上去」。
func StatsOf(ctx context.Context, userID uint64) (Stats, error) {
	var st Stats
	if err := (QuotaSubject{UserID: userID}).countQuery(ctx).Count(&st.TodayCount).Error; err != nil {
		return st, err
	}
	if err := db.DB.WithContext(ctx).Model(&model.Upload{}).
		Where("user_id = ? AND expires_at > ?", userID, time.Now()).
		Select("COALESCE(SUM(size_bytes),0)").Scan(&st.TotalBytes).Error; err != nil {
		return st, err
	}
	return st, nil
}

// PurgeExpired 清理到期上传件（对象 + 记录）。
//
// 循环、复核与统计只有 storage.PurgeExpired 一份实现；这里只回答
// 「取哪些行、删哪张表、被挡住多少」—— 与产物清理共用同一套删除判据。
//
// 候选集按「后端仍可构建 + 位置完整」过滤：查询是 ORDER BY expires_at LIMIT batch，
// 若最老的 N 行所属后端不可构建，逐条跳过后每轮都会选中同一批，后面的正常行永远排不上。
// 被过滤掉的行数单独 COUNT 并写进指标（静默才是真问题）。
func PurgeExpired(ctx context.Context, batch int) (storage.PurgeStats, error) {
	usable := storage.UsableBackends()
	return storage.PurgeExpired(ctx, batch, storage.PurgeOps{
		Kind: "upload",
		Pick: func(ctx context.Context, limit int) ([]storage.PurgeRow, error) {
			var rows []model.Upload
			err := db.DB.WithContext(ctx).
				Where("expires_at <= now()").
				Where("storage_backend IN ?", usable).
				Where("storage_backend = 'none' OR storage_bucket <> ''").
				Order("expires_at ASC, id ASC").Limit(limit).Find(&rows).Error
			if err != nil {
				return nil, err
			}
			out := make([]storage.PurgeRow, 0, len(rows))
			for i := range rows {
				out = append(out, storage.PurgeRow{
					ID:    rows[i].ID,
					Bytes: rows[i].SizeBytes,
					Ref: storage.Ref{
						Backend: rows[i].StorageBackend,
						Bucket:  rows[i].StorageBucket,
						Key:     rows[i].StorageKey,
					},
				})
			}
			return out, nil
		},
		DeleteRow: func(ctx context.Context, id string) (int64, error) {
			res := db.DB.WithContext(ctx).Delete(&model.Upload{}, "id = ?", id)
			return res.RowsAffected, res.Error
		},
		Blocked: func(ctx context.Context) (map[string]int64, error) {
			return countBlockedUploads(ctx, usable)
		},
	})
}

// countBlockedUploads 统计「到期但没进候选集」的行，按原因分档。
//
// 两个原因必须分开：backend = 后端不可构建（配置/凭据），bucket = 云后端缺桶标识
// （写路径漏记）。前者改配置即可恢复，后者只能人工确认这些对象到底在哪个桶。
func countBlockedUploads(ctx context.Context, usable []string) (map[string]int64, error) {
	out := map[string]int64{}
	var n int64
	if err := db.DB.WithContext(ctx).Model(&model.Upload{}).
		Where("expires_at <= now() AND storage_backend NOT IN ?", usable).
		Count(&n).Error; err != nil {
		return nil, err
	}
	if n > 0 {
		out["backend"] = n
	}
	n = 0
	if err := db.DB.WithContext(ctx).Model(&model.Upload{}).
		Where("expires_at <= now() AND storage_backend IN ? AND storage_backend <> 'none' AND storage_bucket = ''", usable).
		Count(&n).Error; err != nil {
		return nil, err
	}
	if n > 0 {
		out["bucket"] = n
	}
	return out, nil
}

// DirectURL 返回该上传件的公开地址（GET /f/ 的 302 目标）。
//
// 语义只有两种：
//   - (url, true, nil)   记录在云后端 → 直接 302，浏览器此后直连对象存储；
//   - ("", false, nil)   记录在本地后端 → 没有对外地址，调用方走代理；
//   - ("", false, err)   记录不存在/已过期（ErrExpired）或数据库错误。
//
// 地址是**永久直链**（桶公开读），所以没有 TTL、没有"剩余寿命太短就降级"的分支
// —— 少一套状态就少一类「点开就 403」。
func DirectURL(ctx context.Context, id string) (string, bool, error) {
	if id == "" || len(id) > 26 {
		return "", false, ErrExpired
	}
	var rec model.Upload
	if err := db.DB.WithContext(ctx).Where("id = ?", id).First(&rec).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", false, ErrExpired
		}
		return "", false, err
	}
	if !rec.ExpiresAt.After(time.Now()) {
		return "", false, ErrExpired
	}
	url, ok := storage.PublicURL(storage.Ref{
		Backend: rec.StorageBackend, Bucket: rec.StorageBucket, Key: rec.StorageKey,
	})
	return url, ok, nil
}

// View 把记录转为对外视图；base 为直链站点前缀。
//
// URL 按**记录的后端**派生：云后端给对象存储的公开地址，本地后端给平台直链。
// 这是"存储位置以配置为准"在对外接口上的落点 —— 地址本身就是位置的证据，
// 不需要再去翻库或管理台核对。
func View(u *model.Upload, keyName, base string) Item {
	return Item{
		ID: u.ID, Filename: u.Filename, SafeName: u.SafeName, SizeBytes: u.SizeBytes,
		ContentType: u.ContentType,
		URL: storage.URLOr(
			storage.Ref{Backend: u.StorageBackend, Bucket: u.StorageBucket, Key: u.StorageKey},
			BuildURL(base, u.ID, u.SafeName),
		),
		KeyID: u.KeyID, KeyName: keyName,
		CreatedAt: u.CreatedAt, ExpiresAt: u.ExpiresAt,
		RemainingSeconds: int64(time.Until(u.ExpiresAt).Seconds()),
	}
}

// BuildURL 拼接平台直链：{base}/f/{id}/{safe_name}；base 为空时返回相对路径。
// 只有**本地后端**会下发它；云后端的地址由 storage.PublicURL 生成。
func BuildURL(base, id, safeName string) string {
	return fmt.Sprintf("%s/f/%s/%s", strings.TrimRight(base, "/"), id, safeName)
}

// SafeName 生成直链展示名：非 [A-Za-z0-9._-] 一律替换为 _，截断 80，空/点开头加前缀。
// 仅用于 URL 展示与 Content-Disposition，不参与路由匹配与存储路径（存储路径固定为 files/{uid}/{id}）。
func SafeName(filename string) string {
	base := filepath.Base(strings.ReplaceAll(filename, "\\", "/"))
	// filepath.Base("") 返回 "."、filepath.Base("/") 返回 "/"：此二者均非真实文件名，归一为空。
	if base == "." || base == "/" {
		base = ""
	}
	var b strings.Builder
	for i := 0; i < len(base); i++ {
		c := base[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '_' || c == '-' {
			b.WriteByte(c)
		} else {
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > 80 {
		out = out[:80]
	}
	if out == "" || strings.HasPrefix(out, ".") {
		out = "f_" + out
	}
	return out
}

// storageKey 生成对象 key：files/{user_id}/{file_id}/{safe_name}。
//
// 末段必须是文件名：云端地址是**跨域直链**，浏览器会忽略 `<a download>`，
// 下载名只能取自 URL 末段（对象存储上的 Content-Disposition 需要签名参数，
// 公开读地址带 response-content-disposition 会被拒）。
//
// safeName 已由 SafeName 归一（只留 [A-Za-z0-9._-]），但仍要兜住 ".."/"."：
// 归一后它们可能原样保留，而 ".." 段会被对象存储/网关折叠成上级路径，
// 导致「写进去的 key」与「库里记的 key」不一致 —— 那种 404 几乎无法排查。
func storageKey(userID uint64, id, safeName string) string {
	if safeName == "" || safeName == "." || safeName == ".." {
		safeName = "file"
	}
	// 名字会占一段路径，截断到 80（SafeName 已做过一次，这里防调用方直传）。
	if r := []rune(safeName); len(r) > 80 {
		safeName = string(r[:80])
	}
	return fmt.Sprintf("files/%d/%s/%s", userID, id, safeName)
}

// keyNames 批量取 KEY 名称（门户列表展示所属 KEY）。
func keyNames(ctx context.Context, rows []model.Upload) map[uint64]string {
	ids := make([]uint64, 0, len(rows))
	for i := range rows {
		if rows[i].KeyID > 0 {
			ids = append(ids, rows[i].KeyID)
		}
	}
	out := map[uint64]string{}
	if len(ids) == 0 {
		return out
	}
	var keys []model.APIKey
	if err := db.DB.WithContext(ctx).Where("id IN ?", ids).Find(&keys).Error; err != nil {
		return out
	}
	for i := range keys {
		out[keys[i].ID] = keys[i].Name
	}
	return out
}

// truncateRunes 按字符（而非字节）截断，避免切断多字节字符。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
