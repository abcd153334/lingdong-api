package controller

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/db"
	"lingdong-api/internal/gateway"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/memlimit"
	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/pkg/siteurl"
	"lingdong-api/internal/pkg/uploadcfg"
	"lingdong-api/internal/relay"
	"lingdong-api/internal/service/upload"
)

// directRateLimitPerMin 是直链域的 IP 限流阈值（每分钟），仅 Redis 可用时生效。
//
// 60 对「下游视觉模型抓图」太紧：模型/聚合层的取图出口 IP 高度集中，
// 一次批量商品图（每单 2~3 张、连续几十单）就能把共享配额打满，
// 表现为「图片在浏览器里完全可访问，上游却报 unsupported image」——
// 因为它拿到的是 429 的 JSON 而不是图片字节。
// 300 仍能挡住脚本刷取，但给正常抓取留出余量；降级路径（Redis 不可用）会自动按 /6 收敛。
const directRateLimitPerMin = 300

// FileUpload 处理 POST /v1/files：multipart 单文件上传（字段名 file），返回 201 + 文件对象。
func FileUpload(c *gin.Context) {
	k := relay.APIKeyOf(c)
	u := relay.UserOf(c)
	if k == nil || u == nil {
		gateway.FailDef(c, errs.InvalidAPIKey)
		return
	}
	// 请求体门与中间件共用同一份推导值（配置里各类型上限的最大值 + 表单开销），
	// 这里不再有第二个数字：精确的单文件上限由 upload.Create 按后缀判定。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, uploadcfg.RequestCapCached())
	fh, err := c.FormFile("file")
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			gateway.FailDef(c, errs.FileTooLarge)
			return
		}
		gateway.FailDef(c, errs.FileMissing)
		return
	}
	f, err := fh.Open()
	if err != nil {
		gateway.FailDef(c, errs.FileMissing)
		return
	}
	defer f.Close()
	rec, err := upload.Create(c.Request.Context(), upload.CreateInput{
		UserID: u.ID, KeyID: k.ID, Filename: fh.Filename, Size: fh.Size, Reader: f,
	})
	if err != nil {
		if errors.Is(err, upload.ErrConcurrency) {
			c.Header("Retry-After", "1")
		}
		if errors.Is(err, upload.ErrDailyLimit) {
			c.Header("Retry-After", "60")
		}
		gateway.FailDef(c, uploadFailDefOf(err))
		return
	}
	gateway.OKStatus(c, http.StatusCreated, upload.View(rec, k.Name, siteBase(c)))
}

// FileList 处理 GET /v1/files：仅返回本 KEY 的未过期件（T21 分页）。
func FileList(c *gin.Context) {
	k := relay.APIKeyOf(c)
	u := relay.UserOf(c)
	if k == nil || u == nil {
		gateway.FailDef(c, errs.InvalidAPIKey)
		return
	}
	p, err := pagination.Parse(c)
	if err != nil {
		gateway.FailDef(c, errs.InvalidParam)
		return
	}
	items, total, err := upload.List(c.Request.Context(), upload.ListFilter{
		UserID: u.ID, KeyID: k.ID, Offset: p.Offset(), Limit: p.Limit(),
	}, siteBase(c))
	if err != nil {
		gateway.FailDef(c, errs.Internal)
		return
	}
	// Files 列表按 OpenAI 契约补 object:"list"（缺它时 SDK 的 files.list 解析不到分页）。
	gateway.OK(c, gin.H{"object": "list", "data": items, "total": total})
}

// FileGet 处理 GET /v1/files/{id}：单个文件的元信息（OpenAI files.retrieve 对应物）。
//
// 为什么必须有它：上传返回的直链 24 小时失效且不带归属校验，把直链存进业务库
// 第二天就全挂。有了「文件号 → 元信息」，业务侧只需存 id，随时能换到新直链。
func FileGet(c *gin.Context) {
	k := relay.APIKeyOf(c)
	u := relay.UserOf(c)
	if k == nil || u == nil {
		gateway.FailDef(c, errs.InvalidAPIKey)
		return
	}
	keyID := k.ID
	item, err := upload.Get(c.Request.Context(), strings.TrimSpace(c.Param("id")),
		upload.Scope{UserID: u.ID, KeyID: &keyID}, siteBase(c))
	if err != nil {
		gateway.FailDef(c, uploadFailDefOf(err))
		return
	}
	gateway.OK(c, item)
}

// FileContent 处理 GET /v1/files/{id}/content：带**归属校验**的文件下载。
//
// 与直链 `/f/{id}/{name}` 的分工：
//   - 直链：无鉴权、24 小时过期，给厂商拉素材、给 <img> 用；
//   - 本接口：要 Authorization、按 KEY 校验归属，用于「私有产物」这类不能
//     把 URL 当凭证的场景（URL 一旦漏出去就再也收不回来）。
//
// 两个入口共用 `writeArtifact`：Range / 206 / 416 / nosniff / Content-Disposition
// 只有一份实现，避免「直链能拖动、带鉴权的那条不能拖动」这种不对称。
func FileContent(c *gin.Context) {
	k := relay.APIKeyOf(c)
	u := relay.UserOf(c)
	if k == nil || u == nil {
		gateway.FailDef(c, errs.InvalidAPIKey)
		return
	}
	keyID := k.ID
	start, length, ranged := parseRangeHeader(c.GetHeader("Range"))
	d, err := upload.OpenScoped(c.Request.Context(), strings.TrimSpace(c.Param("id")),
		upload.Scope{UserID: u.ID, KeyID: &keyID}, start, length, ranged)
	if err != nil {
		gateway.FailDef(c, uploadFailDefOf(err))
		return
	}
	defer d.Body.Close()
	if ranged && d.Total > 0 && start >= d.Total {
		c.Header("Content-Range", "bytes */"+strconv.FormatInt(d.Total, 10))
		gateway.FailDef(c, errs.RangeNotSatisfiable)
		return
	}
	if c.Request.Method == http.MethodHead {
		c.Header("Accept-Ranges", "bytes")
		// HEAD 必须给出完整元信息。
		//
		// 部分抓图器（含视觉模型/聚合网关的取图器）会先 HEAD 探测再决定是否下载：
		// 缺少 Content-Type / Content-Length 时它会认为「这个地址不是图片」，
		// 于是回一个与内容无关的 "unsupported image"。之前这里只写 Accept-Ranges，
		// 是这条链路上的一个确定缺陷（线上实测有 CT/CL，说明线上二进制领先于源码）。
		if d.Upload.ContentType != "" {
			c.Header("Content-Type", d.Upload.ContentType)
		}
		if d.Size > 0 {
			c.Header("Content-Length", strconv.FormatInt(d.Size, 10))
		}
		c.Header("X-Content-Type-Options", "nosniff")
		c.Status(http.StatusOK)
		return
	}
	writeArtifact(c, d.Body, d.Size, d.Upload.ContentType, start, d.Total, ranged)
}

// FileDelete 处理 DELETE /v1/files/{id}：仅本 KEY 归属可删（跨 KEY 走门户），成功 204。
func FileDelete(c *gin.Context) {
	k := relay.APIKeyOf(c)
	u := relay.UserOf(c)
	if k == nil || u == nil {
		gateway.FailDef(c, errs.InvalidAPIKey)
		return
	}
	keyID := k.ID
	if err := upload.Delete(c.Request.Context(), strings.TrimSpace(c.Param("id")),
		upload.Scope{UserID: u.ID, KeyID: &keyID}); err != nil {
		gateway.FailDef(c, uploadFailDefOf(err))
		return
	}
	c.Status(http.StatusNoContent)
}

// FileDirect 处理 /f/{id}/{safe_name} 直链（无鉴权）：
// 命中且未过期 → 200 代理流；其余一切情况 → 410 file_expired（不区分存在性、不维护 tombstone）。
func FileDirect(c *gin.Context) {
	if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
		response.FailDef(c, errs.FileExpired)
		return
	}
	if !allowDirect(c.ClientIP()) {
		c.Header("Retry-After", "60")
		response.FailDef(c, errs.RateLimited)
		return
	}
	start, length, ranged := parseRangeHeader(c.GetHeader("Range"))
	id := directID(c.Request.URL.Path)
	// 云端记录直接 302 到对象存储的公开地址；本地记录走代理。
	//
	// 只在「无 Range 的 GET」上 302：带 Range 时浏览器会丢掉跨域重定向的 Range 头，
	// 播放器拖动进度条会每次从头开始；HEAD 302 也没有意义（拿不到 Content-Length）。
	// 这里是**旧链接兼容入口**：新下发的地址已经是对象存储地址，不再经过本端点。
	if c.Request.Method == http.MethodGet && !ranged {
		url, ok, err := upload.DirectURL(c.Request.Context(), id)
		if err != nil {
			response.FailDef(c, uploadFailDefOf(err))
			return
		}
		if ok {
			c.Redirect(http.StatusFound, url)
			return
		}
	}
	var d *upload.Direct
	var err error
	if ranged {
		d, err = upload.OpenDirectRange(c.Request.Context(), id, start, length)
	} else {
		d, err = upload.OpenDirect(c.Request.Context(), id)
	}
	if err != nil {
		response.FailDef(c, uploadFailDefOf(err))
		return
	}
	defer d.Body.Close()
	// 起点越界必须 416 且带 Content-Range: bytes */total，播放器据此回退到 0。
	if ranged && d.Total > 0 && start >= d.Total {
		c.Header("Content-Range", "bytes */"+strconv.FormatInt(d.Total, 10))
		response.FailDef(c, errs.RangeNotSatisfiable)
		return
	}
	if c.Request.Method == http.MethodHead {
		c.Header("Accept-Ranges", "bytes")
		c.Status(http.StatusOK)
		return
	}
	// 直链一律内联展示：Content-Disposition 用 safe_name（存储路径不参与路由/拼接）。
	c.Header("Content-Disposition", "inline; filename=\""+d.Upload.SafeName+"\"")
	writeArtifact(c, d.Body, d.Size, d.Upload.ContentType, start, d.Total, ranged)
}

// directID 从 /f/{id}/{safe_name} 解出 file_id（ULID 26 位封顶，形态不符一律判过期）。
// 缺 safe_name 段（如 /f/{id}）同样判过期：契约只承认 /f/{id}/{name} 这一种形态。
func directID(path string) string {
	rest := strings.TrimPrefix(path, "/f/")
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return ""
	}
	id := rest[:i]
	if id == "" || len(id) > 26 || strings.ContainsAny(id, "%?&") {
		return ""
	}
	return id
}

// allowDirect 直链 IP 限流（固定窗口；Redis 不可用时放行）。
func allowDirect(ip string) bool { return allowBucket(ip, "f", directRateLimitPerMin) }

// allowBucket 是「无鉴权资源路由」的 IP 固定窗口限流：scope 区分资源域，
// 各域独立计数（/f 直链与 /api/icons 各自一份额度）。
//
// 为什么必须有 scope：两个域共用一个计数桶时，门户刷一遍模型广场（30+ 张图）
// 就会把同一 IP 的直链额度一起用光 —— 症状是"看完列表后下载产物 429"，
// 而两者毫无关系。
//
// Redis 不可用时**放行**：这些路由服务的是静态资源，限流的目的是防止被打，
// 而"Redis 挂了就全站图片 404"是更严重的故障。
// key/limit 由调用方给常量，不拼接任何用户输入。
func allowBucket(ip, scope string, limit int) bool {
	if !db.RedisReady() {
		return degradedAllow(scope, ip, limit)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	key := "ld:rl:" + scope + ":" + ip + ":" + strconv.FormatInt(time.Now().Unix()/60, 10)
	n, err := db.RDB.Incr(ctx, key).Result()
	if err != nil {
		return degradedAllow(scope, ip, limit)
	}
	if n == 1 {
		db.RDB.Expire(ctx, key, time.Minute)
	}
	return n <= int64(limit)
}

// degradedAllow 是 Redis 不可用期间的进程内兜底。
//
// 这几个调用点（/f 直链、/v1/media 产物代理、/api/icons 图标）都是**匿名可打**的，
// 而且都会真读存储/回源 —— Redis 一挂就全放行，等于把最贵的读放大面直接开给公网。
//
// 阈值取 limit/6（下限 5）：单副本计数在多副本下会放大成 limit/6×副本数，
// 仍然远低于原阈值；宁可降级期少放，也不要"降级 = 不限制"。
// 进程重启即清零、不跨副本共享，这一点必须写进运维口径（进程内状态不跨副本共享，多副本必须靠 Redis 承担）。
// degradedAllow 是直链/图标两个匿名资源面的降级判定。
// 实现已收敛到 pkg/memlimit.DegradedAllow（三条路径共用一个口径）。
func degradedAllow(scope, ip string, limit int) bool {
	return memlimit.DegradedAllow(scope, ip, limit)
}

// siteBase 取对外站点前缀（唯一推导点在 pkg/siteurl）。
//
// 这里保留薄封装而不是让调用方直接写 siteurl.Base(c)：上传域的 4 处调用点
// 都只关心「拼直链的前缀」，逐个改名只是制造 diff。
func siteBase(c *gin.Context) string {
	if v := siteurl.Base(c); v != "" {
		return v
	}
	return absURL(c, "")
}

// uploadFailDefOf 把上传域错误映射为对外错误码定义。
func uploadFailDefOf(err error) errs.Def {
	switch {
	case errors.Is(err, upload.ErrDisabled):
		return errs.FileDisabled
	case errors.Is(err, upload.ErrMissing):
		return errs.FileMissing
	case errors.Is(err, upload.ErrEmpty):
		return errs.FileEmpty
	case errors.Is(err, upload.ErrTooLarge):
		return errs.FileTooLarge
	case errors.Is(err, upload.ErrTypeUnsupported):
		return errs.FileTypeUnsupported
	case errors.Is(err, upload.ErrDailyLimit):
		return errs.FileDailyLimit
	case errors.Is(err, upload.ErrStorageFull):
		return errs.FileStorageFull
	case errors.Is(err, upload.ErrConcurrency):
		return errs.FileConcurrency
	case errors.Is(err, upload.ErrNotFound):
		return errs.FileNotFound
	case errors.Is(err, upload.ErrExpired):
		return errs.FileExpired
	case errors.Is(err, upload.ErrStorageNotConfigured):
		return errs.StorageNotConfigured
	case errors.Is(err, upload.ErrStorageUnavailable):
		return errs.StorageUnavailable
	case errors.Is(err, upload.ErrStorageGone):
		return errs.StorageBackendMissing
	}
	return errs.Internal
}
