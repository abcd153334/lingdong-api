// Package icon 是「运营上传图标」的唯一读写实现（icons 表 + 对象存储）。
//
// 与 brandicon 的分工：
//   - brandicon：出厂内置图库（打进二进制）+ 图标 token 的**取值契约**（Normalize/Effective）；
//   - icon（本包）：运营上传的图片（落对象存储 + icons 表）。
//
// 两者都不认识「模型/供应商/渠道」，token 的语义由 brandicon 统一，本包只生产
// `upload:<ulid>` 这一种 token，也只消费 ulid。
//
// 刻意不提供删除：图标被 models/vendors/channels 的 icon 列引用，删掉之后这些实体
// 的图标会静默退回首字占位，而运营看到的是「我明明设过」。清理只能走运维脚本。
package icon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"time"

	"lingdong-api/internal/assets/brandicon"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/pgerr"
	"lingdong-api/internal/pkg/ulid"
	"lingdong-api/internal/service/storage"
	"lingdong-api/internal/service/upload"
)

// MaxBytes 是单张图标的上限（512KB）。
//
// 512KB 是「够用且不必操心」的量级：一个 512×512 的 png 通常 100–300KB，
// 而选择器一次要渲染 72 + N 个小图，原图超过这个量级说明运营传错了东西
// （截图、大照片）。上限同时是**内存上限**：Save 会把整张图读进内存算 sha256，
// 没有上限就是一次上传打爆一个进程。
const MaxBytes = 512 << 10

// MaxEdge 是单张图标的边长上限（像素）。
//
// 字节上限（MaxBytes）管不住这一维：纯色 PNG 的压缩比极高，8000×8000 也能压到
// 几十 KB，而选择器要同时渲染 72 + N 张图 —— 解码内存是按**像素**算的
// （8000×8000 RGBA ≈ 256MB），一张就够把标签页打死。
//
// 1024 是「够用」的上限：图标实际显示尺寸是 16–64px，2× 屏也只需要 128px，
// 1024 已经留了 8 倍余量；再大只可能是误传了截图或照片。
const MaxEdge = 1024

// MaxListLimit 是管理台图标列表的上限。
//
// 200 是「选择器里最近用过的图」的合理窗口：再多也翻不到，而每多一行就多一个
// 待渲染的小图请求。超出的老图仍在库里、仍被实体引用，只是不出现在列表里
// —— 所以列表**不是**图标的真相源，token 才是。
const MaxListLimit = 200

// 域内错误（由 controller 映射到 errs）。
var (
	// ErrEmpty 空文件。
	ErrEmpty = errors.New("icon: 文件内容为空")
	// ErrTooLarge 超过 MaxBytes。
	ErrTooLarge = errors.New("icon: 图标超过 512KB")
	// ErrTypeUnsupported 不是 png/jpeg/webp（含 SVG/GIF/BMP）。
	ErrTypeUnsupported = errors.New("icon: 只支持 PNG / JPEG / WebP")
	// ErrEdgeTooLarge 边长超过 MaxEdge（字节数可能远小于 MaxBytes）。
	ErrEdgeTooLarge = errors.New("icon: 图标边长超过 1024px")
	// ErrNotFound 图标不存在（或 id 形态非法）。
	ErrNotFound = errors.New("icon: 图标不存在")
	// ErrStorageUnavailable 对象存储不可用（配置了 s3 但凭证错等）。
	ErrStorageUnavailable = errors.New("icon: 图标存储不可用")
	// ErrStorageNotConfigured 平台还没选存储位置（system_config.storage 缺失）。
	ErrStorageNotConfigured = errors.New("icon: 平台尚未配置存储位置")
)

// Save 保存一张图标并返回其行（幂等：同一张图只留一行）。
//
// 只收一个 r：嗅探与 sha256 必须看到**同一份字节**。
// 早前的设计是「head + r 两个入参」，看起来能省掉一次读，
// 但两者一旦不构成前缀关系（调用方多传/少传几个字节），结果就是
// 按头部判成 png、按内容算出的 sha 却是另一张图 —— 落库的 mime 与内容不符，
// 而两边都没有报错。这里读满 MaxBytes+1 后统一判定，代价是最大 512KB 的内存。
//
// size 来自 multipart 头，可被客户端伪造，所以只用来**提前**拒绝；
// 最终以实际读到的字节数为准。
func Save(ctx context.Context, r io.Reader, size int64, uid uint64) (*model.Icon, error) {
	// 先用声明大小挡一次：超大文件不必读进内存。
	if size > MaxBytes {
		return nil, ErrTooLarge
	}
	// 按 MaxBytes+1 截断读取：既能拿到 sha256（流式 Put 之后再算就得读两遍对象），
	// 又能在"声明 1KB 实际 1GB"时立刻停手。
	body, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > MaxBytes {
		return nil, ErrTooLarge
	}
	// 空文件先于类型判定：空内容的正确答复是「文件为空」，
	// 而不是「不支持的文件类型」—— 后者会让人以为是自己选错了格式。
	if len(body) == 0 {
		return nil, ErrEmpty
	}
	ext, mime, ok := upload.SniffImage(body)
	if !ok {
		return nil, ErrTypeUnsupported
	}
	// 尺寸校验放在去重与落存储**之前**：一个 8000×8000 的纯色图能被 sha256 去重
	// 命中（同一张图传过），于是"先查重"会让它一路返回成功、连对象都不用写 ——
	// 校验被彻底绕过。放在类型判定之后是因为需要知道是哪个格式才能解析尺寸。
	if w, h, ok := upload.ImageSize(body); ok && (w > MaxEdge || h > MaxEdge) {
		return nil, ErrEdgeTooLarge
	}
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])

	// 幂等：同一张图重复上传直接复用已有行（运营会重复点，也常有同一 logo 的多个副本）。
	if row, err := bySHA(ctx, sha); err != nil {
		return nil, err
	} else if row != nil {
		return row, nil
	}

	rec := &model.Icon{
		// 小写：ULID 是大小写等价的 Crockford Base32，而 token 的正则与库里的值
		// 都取小写形态。生成处不归一，就会出现「上传成功、立刻 404」
		// （库里大写、URL 里被 Normalize 成小写）。
		ID: strings.ToLower(ulid.New()), Ext: ext, MIME: mime,
		Size: int64(len(body)), SHA256: sha, CreatedBy: uid, CreatedAt: time.Now().UTC(),
	}
	key := rec.ObjectKey()
	ref, perr := storage.PutActive(ctx, key, bytes.NewReader(body), rec.Size, mime)
	if perr != nil {
		if errors.Is(perr, storage.ErrNotConfigured) {
			return nil, ErrStorageNotConfigured
		}
		return nil, ErrStorageUnavailable
	}
	rec.StorageBackend = ref.Backend
	rec.StorageBucket = ref.Bucket
	// 用 map 落库：与项目其余写路径同口径（GORM 的 Create 会把「零值 + default 标签」
	// 的列替换成列默认值，见 model 包注释）。这里虽然没有 default 标签，
	// 仍统一走 map，避免以后给某列加 default 时踩同一个坑。
	err = db.DB.WithContext(ctx).Table(rec.TableName()).Create(map[string]any{
		"id": rec.ID, "ext": rec.Ext, "mime": rec.MIME, "size": rec.Size,
		"sha256": rec.SHA256, "created_by": rec.CreatedBy, "created_at": rec.CreatedAt,
		"storage_backend": rec.StorageBackend, "storage_bucket": rec.StorageBucket,
	}).Error
	if err != nil {
		// 并发上传同一张图：另一路先插入了同 sha 的行（uq_icons_sha256）。
		// 判据只用 pgerr.IsUnique（项目铁律：三种错误形态都要认）。
		if pgerr.IsUnique(err) {
			_ = deleteVerified(ctx, ref)
			if row, e := bySHA(ctx, sha); e == nil && row != nil {
				return row, nil
			}
		}
		// 其它落库失败：对象已经写进去了，必须删掉，否则留下一个永远不被引用的孤儿。
		_ = deleteVerified(ctx, ref)
		return nil, err
	}
	return rec, nil
}

// deleteVerified 是返回值的丢弃点：图标补偿删除失败只可能留下孤儿对象，
// 而对象 key 是确定性的（icons/{id}.{ext}），下次同图上传会覆盖它。
// 因此这里只需要「尽力删 + 不阻塞响应」，不需要第二套重试。
func deleteVerified(ctx context.Context, ref storage.Ref) error {
	_, err := storage.DeleteRefVerified(ctx, ref)
	return err
}

// Lookup 按 `/api/icons/u/{id}` 的路径参数取图标行（归一 id 后查库，不碰对象存储）。
//
// 控制器需要**先拿到元数据**再决定「302 到云地址」还是「自己流式输出」；
// 没有这一步就只能先 Open 一次对象流再丢弃，等于每个图标请求多一次云端往返。
func Lookup(ctx context.Context, id string) (*model.Icon, error) {
	// 先按 ulid 形态挡一次：/api/icons/u/:id 是公开路由，不校验形态等于让任意字符串
	// 都打进数据库（白白消耗连接），而 ulid 之外的值本来就不可能是合法 token。
	//
	// 必须用归一后的 id 去查库：库里的 id 恒为小写，直接拿原始串（可能大写）去查
	// 会永远查不到，症状是"上传成功、图片 404"。
	cid, ok := brandicon.CanonicalUploadID(id)
	if !ok {
		return nil, ErrNotFound
	}
	return Get(ctx, cid)
}

// OpenRow 打开**已知行**对应的对象（控制器已为 302 判断取过元数据时复用，不再查库）。
//
// 返回的 size 以**对象的实际长度**为准，而不是 icons.size：两者理论上一致，
// 但文件被截断/替换时以实际为准，Content-Length 才不会说假话。
func OpenRow(ctx context.Context, row *model.Icon) (io.ReadCloser, int64, error) {
	// 归属按记录走：存储配置从 none 切到 s3 之后，切换前写入的图标仍在本地盘上。
	// 按当前 active 去找的症状是「老图标全裂、新图标正常」，而库里每行都完好。
	rc, n, err := storage.OpenRef(ctx, storage.Ref{Backend: row.StorageBackend, Bucket: row.StorageBucket, Key: row.ObjectKey()})
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotFound) {
			return nil, 0, ErrNotFound
		}
		// 后端被删/凭证失效不能折成 404：404 会让运营以为「图标被删了」去重传，
		// 而真实状态是「对象还在桶里，补配置就能读」。
		return nil, 0, ErrStorageUnavailable
	}
	return rc, n, nil
}

// Validate 校验一个图标取值在**当前库里**可用，并返回归一后的 token。
//
// 与 brandicon.Normalize 的分工：Normalize 只管形状（前缀、字符集、内置 key 是否
// 在 catalog 里），它看不到 icons 表。于是一个形状合法但对象已被清理的
// `upload:<ulid>` 能通过 Normalize —— 落库的结果是「保存成功、对外 404 坏图」，
// 而运营看到的是「我明明设置过了」。
//
// **只在写路径**对「本次请求显式提交」的值调用。读路径上的历史值可能指向已清理
// 的对象，那时应当照常返回（前端走首字兜底），不该让整个列表 500；同理
// EditForm 回填出来的值（FillMissing）不校验，否则「只改价格」的保存会因为
// 图标对象早被运维删掉而整体 400。
func Validate(ctx context.Context, raw string) (string, error) {
	token, err := brandicon.Normalize(raw)
	if err != nil {
		return "", err
	}
	id, ok := brandicon.UploadID(token)
	if !ok {
		return token, nil // 内置图标：Normalize 已确认 key 在 catalog 里
	}
	if _, err := Get(ctx, id); err != nil {
		return "", err
	}
	return token, nil
}

// Get 按 id 取一行（不做存储访问）。
func Get(ctx context.Context, id string) (*model.Icon, error) {
	var row model.Icon
	if err := db.DB.WithContext(ctx).Where("id = ?", id).Limit(1).Find(&row).Error; err != nil {
		return nil, err
	}
	if row.ID == "" {
		return nil, ErrNotFound
	}
	return &row, nil
}

// List 返回最近上传的图标（新的在前）。
//
// 返回值保证是**非 nil 切片**：调用方直接拼进 JSON，nil 会序列化成 null，
// 前端 `.length` 抛错导致整个选择器白屏（项目铁律）。
func List(ctx context.Context, limit int) ([]model.Icon, error) {
	if limit <= 0 || limit > MaxListLimit {
		limit = MaxListLimit
	}
	out := make([]model.Icon, 0, limit)
	if err := db.DB.WithContext(ctx).Order("created_at DESC, id DESC").Limit(limit).Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// bySHA 按内容指纹回查（去重与并发兜底共用）。
//
// 未命中返回 (nil, nil) 而不是错误：调用方要把"没有"与"查失败"分开处理，
// 合成一个 error 会让正常的新上传变成 500。
func bySHA(ctx context.Context, sha string) (*model.Icon, error) {
	var row model.Icon
	if err := db.DB.WithContext(ctx).Where("sha256 = ?", sha).Limit(1).Find(&row).Error; err != nil {
		return nil, err
	}
	if row.ID == "" {
		return nil, nil
	}
	return &row, nil
}
