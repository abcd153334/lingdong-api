package controller

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/assets/brandicon"
	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/pkg/siteurl"
	"lingdong-api/internal/pkg/uploadcfg"
	"lingdong-api/internal/service/audit"
	iconsvc "lingdong-api/internal/service/icon"
	"lingdong-api/internal/service/iconref"
	"lingdong-api/internal/service/storage"
)

// 约定：本文件的**控制台**接口（/api/admin/icons）成功也走 response.OK 信封。
//
// 曾经这两处写成裸 `c.JSON(http.StatusOK, ...)`：失败分支有信封、成功分支没有，
// 前端 parseResponse 拿不到 code 就把 200 判成失败 —— 表现是「图标库永远加载失败」，
// 而页面其它部分完全正常，排查时最容易怀疑网络。该事故由
// success_envelope_gate_test.go（AST 扫描）与 icon_admin_contract_test.go（真 handler）双守。
// 裸 c.JSON 只留给 /v1 网关与二进制响应。

// iconRateLimitPerMin 是图标图片路由的 IP 限流阈值（每分钟）。
//
// 600 而不是 60：门户首屏一次就要 30+ 张图（模型卡片、供应商胶囊、文档站成员表），
// 按 60rpm 分给 /f 直链的口径会把正常浏览打成 429 —— 而 429 的表现是
// "图标时有时无"，比整片不显示更难判断。
const iconRateLimitPerMin = 600

// iconCacheMaxAge 是图标响应的缓存时长（秒）。
//
// 一年 + 内容指纹 ETag：图标内容**永不变化**（内置的是编译进去的字节，
// 上传的是按 sha256 去重的一次性对象，id 与内容一一对应）。
// 刻意不加 `immutable`：那会逼出第二个缓存实现（现有 writeETag 只发 max-age），
// 而一年已经足够让浏览器不再回源。
const iconCacheMaxAge = 31536000

// BrandIconAsset 处理 GET/HEAD /api/icons/b/:key：内置图标（编译进二进制的 PNG）。
//
// 为什么在 engine 根而不在 /api 组：<img src> 带不了 Cookie 之外的任何凭证，
// 而组上的 RateLimit(60rpm) 会被门户首屏打爆（同 /v1/media 的理由与做法）。
func BrandIconAsset(c *gin.Context) {
	if !allowBucket(c.ClientIP(), "icon", iconRateLimitPerMin) {
		c.Header("Retry-After", "60")
		// 图片路由不返回 JSON 错误体：<img> 收到 JSON 只会显示坏图，
		// 而这里唯一有意义的信息就是状态码。
		c.Status(http.StatusTooManyRequests)
		return
	}
	key := strings.TrimSpace(c.Param("key"))
	raw, etag, ok := brandicon.Asset(key)
	if !ok {
		// 不返回 JSON 错误体：与 404 的语义一致，且避免把错误结构暴露给 <img>。
		c.Status(http.StatusNotFound)
		return
	}
	if iconNotModified(c, etag, "image/png") {
		return
	}
	if c.Request.Method == http.MethodHead {
		c.Header("Content-Length", strconv.Itoa(len(raw)))
		c.Status(http.StatusOK)
		return
	}
	c.Data(http.StatusOK, "image/png", raw)
}

// UploadIconAsset 处理 GET/HEAD /api/icons/u/:id：运营上传的图标。
//
// 地址仍是平台路径（iconref 只认 `/api/icons/u/{id}`），但**字节从哪来由记录的后端决定**：
// 云端记录直接 302 到对象存储公开地址（浏览器自己去取，省掉平台这一跳的带宽），
// 本地记录照旧流式输出。
func UploadIconAsset(c *gin.Context) {
	if !allowBucket(c.ClientIP(), "icon", iconRateLimitPerMin) {
		c.Header("Retry-After", "60")
		c.Status(http.StatusTooManyRequests)
		return
	}
	// 先取元数据再决定 302：直接 Open 会白开一次对象流（云端 = 一次跨网往返）再丢弃。
	rec, err := iconsvc.Lookup(c.Request.Context(), strings.TrimSpace(c.Param("id")))
	if err != nil {
		// 存储不可用不能折成 404：那会让运营以为「图标被删了」去重传，
		// 而真实状态是「对象还在桶里，补配置就能读」。
		if errors.Is(err, iconsvc.ErrStorageUnavailable) {
			c.Status(http.StatusServiceUnavailable)
			return
		}
		c.Status(http.StatusNotFound)
		return
	}
	// ETag 直接由库里的 sha256 派生（与 writeETag 同格式：前 16 字节 = 32 hex 字符）。
	// 不从流里重算：图标最大 512KB，每个请求重算一次哈希纯属浪费，
	// 而 sha256 列在写入时就等于对象内容 —— 真相源只有一个。
	etag := `"` + rec.SHA256 + `"`
	if len(rec.SHA256) >= 32 {
		etag = `"` + rec.SHA256[:32] + `"`
	}
	if iconNotModified(c, etag, rec.MIME) {
		return
	}
	if c.Request.Method == http.MethodHead {
		c.Header("Content-Length", strconv.FormatInt(rec.Size, 10))
		c.Status(http.StatusOK)
		return
	}
	// 302 只给 GET：HEAD 已经在上面按元数据答了 Content-Length，再跳一次没有意义。
	// 缓存 5 分钟而不是一年：切后端后旧地址必须尽快失效，否则浏览器会一直拿着
	// 「本机代理地址」或旧桶地址不放。
	if url, ok := storage.PublicURL(storage.Ref{Backend: rec.StorageBackend, Bucket: rec.StorageBucket, Key: rec.ObjectKey()}); ok {
		c.Header("Cache-Control", "public, max-age=300")
		c.Redirect(http.StatusFound, url)
		return
	}
	body, size, err := iconsvc.OpenRow(c.Request.Context(), rec)
	if err != nil {
		if errors.Is(err, iconsvc.ErrStorageUnavailable) {
			c.Status(http.StatusServiceUnavailable)
			return
		}
		c.Status(http.StatusNotFound)
		return
	}
	defer body.Close()
	c.Header("Content-Length", strconv.FormatInt(size, 10))
	c.Status(http.StatusOK)
	// 写失败无法再改状态码（响应头已发出），只能中断传输；
	// 浏览器侧会看到图裂，与 404 的可见结果一致。
	_, _ = io.Copy(c.Writer, body)
}

// iconNotModified 写图标响应的公共头，并处理条件请求（命中返回 304 + true）。
func iconNotModified(c *gin.Context, etag, mime string) bool {
	c.Header("Content-Type", mime)
	c.Header("Cache-Control", "public, max-age="+strconv.Itoa(iconCacheMaxAge))
	// nosniff 必须有：上传白名单只放 png/jpeg/webp，但少了这个头，
	// 浏览器仍可能按内容猜类型（例如被塞进 HTML 的 polyglot 文件）。
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Disposition", "inline")
	c.Header("ETag", etag)
	if match := c.GetHeader("If-None-Match"); match != "" && strings.Contains(match, etag) {
		c.Status(http.StatusNotModified)
		return true
	}
	return false
}

// adminIconListResp 是 GET /api/admin/icons 的响应体。
type adminIconListResp struct {
	Builtin  []adminIconBuiltin  `json:"builtin"`
	Uploaded []adminIconUploaded `json:"uploaded"`
}

// adminIconBuiltin 是一条内置图标（选择器用）。
//
// 必须返回 url：本项目的图片地址**只有一个拼接点**（service/iconref），
// 前端不再自己拼前缀。旧注释说"后端拼的 url 在跨域部署时会错"—— 那句话在
// 前端自己拼 `/icons/b/<key>` 的前提下才成立：前端拼的是"站内相对路径"，
// 而后端给的是**绝对地址**（由 siteurl.Base 推导，跨域部署下正是正确的那一个）。
type adminIconBuiltin struct {
	Key   string `json:"key"`
	Name  string `json:"name"`
	Group string `json:"group"`
	// URL 是这张图的绝对地址（已带 per-key 版本参数，见 iconref）。
	URL string `json:"url"`
	// Aliases 用于搜索（中文别名），保证是 [] 而非 null。
	Aliases []string `json:"aliases"`
}

// adminIconUploaded 是一条已上传图标。
type adminIconUploaded struct {
	ID   string `json:"id"`
	Icon string `json:"icon"` // upload:<id>：写回实体 icon 列用的令牌
	// URL 是这张图的绝对地址（前端直接 <img src>）。
	URL  string `json:"url"`
	Size int64  `json:"size"`
	// CreatedAt 是 unix 秒（与项目其余时间字段一致）。
	CreatedAt int64 `json:"created_at"`
}

// ListAdminIcons 处理 GET /api/admin/icons：内置图库 + 最近上传的图标。
func ListAdminIcons(c *gin.Context) {
	rows, err := iconsvc.List(c.Request.Context(), iconsvc.MaxListLimit)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	base := siteurl.Base(c)
	items := brandicon.List()
	resp := adminIconListResp{
		// 两个数组都必须预分配非 nil：nil 会序列化成 null，前端 .length 抛错 → 选择器白屏。
		Builtin:  make([]adminIconBuiltin, 0, len(items)),
		Uploaded: make([]adminIconUploaded, 0, len(rows)),
	}
	for _, it := range items {
		aliases := it.Aliases
		if aliases == nil {
			aliases = []string{}
		}
		resp.Builtin = append(resp.Builtin, adminIconBuiltin{
			Key: it.Key, Name: it.Name, Group: string(it.Group), Aliases: aliases,
			URL: iconref.URL(base, brandicon.BuiltinToken(it.Key)),
		})
	}
	for _, r := range rows {
		token := brandicon.UploadToken(r.ID)
		resp.Uploaded = append(resp.Uploaded, adminIconUploaded{
			ID: r.ID, Icon: token, URL: iconref.URL(base, token), Size: r.Size,
			CreatedAt: r.CreatedAt.Unix(),
		})
	}
	response.OK(c, resp)
}

// UploadAdminIcon 处理 POST /api/admin/icons：multipart 单文件上传（字段名 file）。
//
// 返回 {id, icon, mime, size}：`icon` 是可直接写进实体 icon 列的 token，
// 省掉前端自己拼 `upload:` 前缀（拼错的表现是"上传成功但保存后图标不显示"）。
func UploadAdminIcon(c *gin.Context) {
	uid := c.GetUint64("user_id")
	// 上限 = 图标上限 + 表单额外开销。用 MaxBytesReader 而不是只看 fh.Size：
	// fh.Size 是解析后的声明值，而 MaxBytesReader 在读流时就截断，
	// 否则一个伪造 Content-Length 的大文件会先把磁盘/内存吃满再被拒。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, iconsvc.MaxBytes+uploadcfg.UploadOverhead)
	fh, err := c.FormFile("file")
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			response.FailDefMsg(c, errs.FileTooLarge, "图标不能超过 512KB")
			return
		}
		response.FailDef(c, errs.FileMissing)
		return
	}
	f, err := fh.Open()
	if err != nil {
		response.FailDef(c, errs.FileMissing)
		return
	}
	defer f.Close()

	rec, err := iconsvc.Save(c.Request.Context(), f, fh.Size, uid)
	if err != nil {
		response.FailDef(c, iconFailDefOf(err))
		return
	}
	audit.RecordOrWarn(db.DB, uid, audit.ActionIconUpload, "icon", rec.ID,
		gin.H{"mime": rec.MIME, "size": rec.Size})
	token := brandicon.UploadToken(rec.ID)
	response.OK(c, gin.H{
		"id": rec.ID, "icon": token,
		// url 与 icon 成对返回：表单里"刚上传完还没保存"这一帧要立刻显示预览，
		// 而该 token 还没经过任何实体的读投影。前端拿它填预览，不自己拼路径。
		"url":  iconref.URL(siteurl.Base(c), token),
		"mime": rec.MIME, "size": rec.Size,
		// created_at 是给前端「前插进已上传分组」用的：图库接口有 200 条上限，
		// 只 invalidate 再重拉的话，新图可能排在 200 名之外 —— 用户会以为上传失败。
		"created_at": rec.CreatedAt.Unix(),
	})
}

// iconFailDefOf 把图标域错误映射为对外错误码。
//
// 复用文件域的码（415/413/400/502 语义完全一致），只把**文案**换成图标口径：
// 运营看到「不支持的文件类型」时不知道该传什么，
// 而「图标只支持 PNG / JPEG / WebP」是可以直接照做的。
func iconFailDefOf(err error) errs.Def {
	switch {
	case errors.Is(err, iconsvc.ErrEmpty):
		return errs.FileEmpty.WithMessage("请选择一个图片文件")
	case errors.Is(err, iconsvc.ErrTooLarge):
		// 与 MaxBytesReader 分支同一句话：同一类失败给出两个说法，
		// 运营会以为是两个问题（一个大、一个"格式不对"）。
		return errs.FileTooLarge.WithMessage("图标不能超过 512KB")
	case errors.Is(err, iconsvc.ErrTypeUnsupported):
		return errs.FileTypeUnsupported.WithMessage("图标只支持 PNG / JPEG / WebP")
	case errors.Is(err, iconsvc.ErrEdgeTooLarge):
		// 与 ErrTooLarge 分开说：两者的处置动作完全不同（一个去压缩/缩小，一个去裁剪），
		// 合成「文件太大」会让人反复压体积却依然传不上去（纯色图压完还是那么像素多）。
		return errs.FileTooLarge.WithMessage("图标边长不能超过 1024px，请缩小后再上传")
	case errors.Is(err, iconsvc.ErrStorageNotConfigured):
		return errs.StorageNotConfigured
	case errors.Is(err, iconsvc.ErrStorageUnavailable):
		return errs.StorageUnavailable
	case errors.Is(err, iconsvc.ErrNotFound):
		return errs.FileNotFound
	}
	return errs.Internal
}
