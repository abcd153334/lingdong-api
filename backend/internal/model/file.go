package model

import (
	"strings"
	"time"
)

// File 对应 files 表（v2.0 §4.2.10）。任务产物，expires_at 到期由 cleaner 清理。
type File struct {
	ID     uint64 `gorm:"primaryKey"`
	TaskID uint64 `gorm:"index"`
	// ArtifactIndex 是同一任务内的产物序号（单产物恒 0，多产物按成功顺序稠密编号 0..n-1）。
	//
	// 不能靠 ORDER BY id 推断：转存失败重试会产生 id 空洞与乱序，序号必须是落库时确定的。
	ArtifactIndex int    `gorm:"not null;default:0"`
	UserID        uint64 `gorm:"index"`
	Filename      string `gorm:"size:255"`
	SizeBytes     int64  `gorm:"not null;default:0"`
	// StorageBackend 是**写入时**激活的后端标识（none/s3/minio/oss/cos）。
	//
	// 读/删必须按它路由，永远不看当前配置里的 active：切换后端之后，
	// 切换前写入的产物仍留在旧后端上，按 active 去找只会全部 404。
	StorageBackend string `gorm:"size:16;not null;default:''"`
	// StorageBucket 是写入时该后端的桶（本地后端为空串），与 backend/key 一起定位对象。
	StorageBucket string `gorm:"size:128;not null;default:''"`
	// StorageKey 是该后端内的对象 key（跨后端不通用）。
	StorageKey  string     `gorm:"size:512"`
	ContentType string     `gorm:"size:100"`
	ExpiresAt   *time.Time `gorm:"type:timestamptz;index"`
	CreatedAt   time.Time  `gorm:"type:timestamptz"`
}

func (File) TableName() string { return "files" }

// ReadableAt 是「这条产物现在是否可读」的**唯一判据**。
//
// 清单（model.Readable/ArtifactListing）与取数（task.OpenArtifactAt）都走它：
// 两处各写一遍 `expires_at > now` 时，只要有一处忘掉，就会出现
// 「清单列了 3 个、点第 2 个 410」或「清单跳过、OFFSET 没跳过」的错位。
func (f *File) ReadableAt(now time.Time) bool {
	return f.ExpiresAt == nil || f.ExpiresAt.After(now)
}

// EffectiveContentType 是**对外展示/下发**用的 Content-Type：
// 存储值为空或 application/octet-stream 时，按文件名扩展名补全。
//
// 为什么补在读侧：写侧取的是上游下载响应头，很多 CDN 直接给 octet-stream，
// 而文件其实是 png/mp4 —— 按它判类型，门户会把图片显示成「文件」，播放器也打不开。
// 补在写侧需要重读前缀做嗅探；补在读侧只看扩展名，历史行还一起修好。
func (f *File) EffectiveContentType() string {
	ct := strings.TrimSpace(f.ContentType)
	if ct != "" && !strings.EqualFold(ct, "application/octet-stream") {
		return ct
	}
	if m := MIMETypeOfURL(f.Filename); m != "" {
		return m
	}
	return ct
}

// ArtifactKind 判产物类型（image/video/audio/text/file）。
//
// 放在 model 层而不是 task 或 logs：这两个包互相不可见（relay→logs→task→relay 会成环），
// 而"产物是什么类型"是数据的属性，不是某个服务的私事。两份判断迟早漂移成
// "image/ 与 image/png 各认一半"，表现是同一条记录在两个页面显示不同图标。
//
// 实现委托 MediaKind，与「同步调用的媒体项」共用同一张表 —— 门户详情里
// 同步产物与任务产物是并排展示的，两张表一定会出现「同一条记录两个图标」。
func ArtifactKind(ct string) string { return MediaKind(ct, "", "") }

// mediaExtMIME 是「扩展名 → MIME」的**唯一**表。
//
// 覆盖范围刻意只到"能在浏览器里直接渲染"的那几类：其余一律 file（下载）。
// 加不进去的类型不要硬塞 —— 判错类型的代价比"不认"更高（会用错标签渲染）。
var mediaExtMIME = map[string]string{
	"png": "image/png", "jpg": "image/jpeg", "jpeg": "image/jpeg", "webp": "image/webp",
	"gif": "image/gif", "avif": "image/avif", "bmp": "image/bmp", "svg": "image/svg+xml",
	"mp4": "video/mp4", "webm": "video/webm", "mov": "video/quicktime",
	"m4v": "video/x-m4v", "mkv": "video/x-matroska",
	"mp3": "audio/mpeg", "wav": "audio/wav", "m4a": "audio/mp4",
	"aac": "audio/aac", "ogg": "audio/ogg", "flac": "audio/flac",
	"txt": "text/plain", "json": "application/json", "csv": "text/csv", "md": "text/markdown",
	"pdf": "application/pdf",
}

// MIMETypeOfURL 从 URL 路径的扩展名推断 MIME；认不出返回空串。
//
// 只看 path，不看 query：上游签名链接的 query 里常带 ".png" 之类的假后缀
// （`?name=a.png&sig=...`），按整串取扩展名会把一段音频判成图片。
func MIMETypeOfURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" || strings.HasPrefix(strings.ToLower(rawURL), "data:") {
		return ""
	}
	if i := strings.IndexAny(rawURL, "?#"); i >= 0 {
		rawURL = rawURL[:i]
	}
	dot := strings.LastIndexByte(rawURL, '.')
	if dot < 0 || dot == len(rawURL)-1 {
		return ""
	}
	return mediaExtMIME[strings.ToLower(rawURL[dot+1:])]
}

// MediaKind 判媒体类型：MIME 优先，其次 URL 扩展名，最后才用端点模态兜底。
//
// 为什么不再直接用端点模态（旧实现）：video 端点返回的封面是 jpg，按模态判成 video
// 会让前端用 <video> 渲染一张图 —— 黑屏 + 控制条，用户看到的就是"预览坏了"。
func MediaKind(mime, rawURL, fallback string) string {
	if k := kindOfMIME(mime); k != "" {
		return k
	}
	if m := MIMETypeOfURL(rawURL); m != "" {
		if k := kindOfMIME(m); k != "" {
			return k
		}
	}
	switch fallback {
	case "image", "video", "audio", "text":
		return fallback
	}
	return "file"
}

// kindOfMIME 把 MIME 归到五个渲染档；空串表示"这个 MIME 我们不认识"。
func kindOfMIME(ct string) string {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	switch {
	// SVG 可内嵌 <script>：内联渲染等于把产物变成同源可执行页面。
	// 与 controller.safeArtifactContentType 的取舍必须一致，否则一边能开一边不能。
	case ct == "image/svg+xml":
		return "file"
	case strings.HasPrefix(ct, "image/"):
		return "image"
	case strings.HasPrefix(ct, "video/"):
		return "video"
	case strings.HasPrefix(ct, "audio/"):
		return "audio"
	case strings.HasPrefix(ct, "text/"):
		return "text"
	// JSON/CSV 这类"文本形态的结构化数据"按文本预览才有意义（原样给 <pre> 更好读）。
	case ct == "application/json", ct == "application/xml", ct == "application/csv":
		return "text"
	}
	return ""
}
