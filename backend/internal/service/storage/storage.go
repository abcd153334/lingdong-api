// Package storage 提供文件对象存储的多后端抽象（v2.0 §4.2.6「云存储」+ §4.3.2「文件域」）。
//
// 三个不可违背的约定（改这个包之前先读完）：
//
//  1. **一个入口**：业务层只经 Ref 系列函数（PutActive / OpenRef / OpenRangeRef / DeleteRef /
//     PublicURL）读写删与取地址，不直接持有 Store。路由规则集中在一处，新增读端点不会各自发明回落顺序。
//  2. **一个归属**：每行记录自己的 storage_backend（值 = Store.Name()），读/删**一律按记录路由**，
//     永不按当前激活后端。否则切换后端的一瞬间，历史对象全部变成孤儿 ——
//     对象还在旧桶里，读却 410，清理任务还会用新后端「删成功」并抹掉最后的线索。
//  3. **一个长度**：Put 的 size 是唯一长度真相。驱动内部统一 io.LimitReader(r, size)，
//     size <= 0 直接 ErrNeedLength —— OSS/COS 都不接受未知长度，OSS 更会在探测失败时
//     把 Content-Length 覆盖成 0（写入 0 字节对象且返回 200，账单照扣）。
package storage

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

	"lingdong-api/internal/conf"
)

// Store 是对象存储抽象：key 形如 `files/{user_id}/{file_id}/{name}`（不含根前缀）。
type Store interface {
	// Name 返回后端标识（none/s3/minio/oss/cos），落库到 *_backend 列。
	Name() string
	// Bucket 返回该实例**实际寻址**的桶（本地后端恒为空串），落库到 *_bucket 列。
	//
	// 它必须来自构造参数而不是「当前配置」：读/删都靠它定位，
	// 换桶之后历史对象仍在新实例之外的那个桶里。
	Bucket() string
	// Put 写入对象（覆盖同 key）。size 是真实字节数，实现必须按它截断读取。
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	// Open 读取对象，返回内容流与长度。
	Open(ctx context.Context, key string) (io.ReadCloser, int64, error)
	// Head 只取对象元信息；对象不存在必须返回 ErrObjectNotFound（不能用 nil 表示）。
	//
	// 这是「删除是否真的生效」的唯一判据：Delete 的返回码不参与判定，
	// 因为 404 既可能是「已经不存在」，也可能是「删错了桶/网关拦了」。
	Head(ctx context.Context, key string) (ObjectInfo, error)
	// Delete 删除对象；对象不存在时视为成功（驱动原语，业务层只经 DeleteRefVerified）。
	Delete(ctx context.Context, key string) error
	// CheckBucket 确认桶可寻址（存在且有权限），并报告是否开启版本控制。
	//
	// 版本控制必须报出来：开了之后 Delete 只产生 delete marker，
	// Head 看不到旧版本 —— 不报就等于「以为删了，实际数据和账单都还在」。
	CheckBucket(ctx context.Context) (BucketState, error)
}

// ObjectInfo 是 Head 的结果；字段取不到时零值（不猜）。
type ObjectInfo struct {
	Size        int64
	ContentType string
	ModTime     time.Time
	ETag        string
}

// BucketState 是 CheckBucket 的结果。
type BucketState struct {
	Exists    bool
	Versioned bool
}

// Lister 是**可选**能力：按前缀分页列举对象（孤儿对账 storage_gc 用）。
//
// 刻意不并入 Store：不支持列举是合法降级（对账任务报 unsupported），
// 与 RangeOpener 同一条理由 —— 不让不相关的实现与测试替身被迫补齐。
type Lister interface {
	List(ctx context.Context, prefix, marker string, limit int) (ListPage, error)
}

// ListPage 是一页列举结果；Next 为空表示没有下一页。
type ListPage struct {
	Objects []ListedObject
	Next    string
}

// ListedObject 是列举出的对象摘要。
type ListedObject struct {
	Key     string
	Size    int64
	ModTime time.Time
}

// RangeOpener 是**可选**能力：支持按区间读取对象（HTTP Range / 视频拖动进度条）。
//
// 刻意不并入 Store 接口：并入会让所有实现（含测试替身）必须补齐，
// 而「不支持 Range」本身是合法降级（回落 200 全量）。调用方用 OpenRangeRef，
// 拿到 ErrRangeUnsupported 再走降级分支。
type RangeOpener interface {
	// OpenRange 打开 [offset, offset+length) 区间；length <= 0 表示到对象末尾。
	OpenRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error)
}

// PublicURLer 是**可选**能力：给出对象在该后端的**公开只读地址**。
//
// 为什么放在存储层而不是拼在业务层：地址形状由后端决定（OSS 是
// `{bucket}.{endpoint}`、COS 是 `{bucket}.cos.{region}.myqcloud.com`、S3 分
// path-style/virtual-host），业务层拼一次就等于把三套云厂商的域名规则复制三份。
// 本地后端不实现它 —— 本地对象没有对外地址，调用方据此回落平台域名。
//
// 约定：桶为公开读，因此这里是**永久直链**，不签名、不带有效期；
// 私有桶会在探测阶段被拒（见 registry.ProbeChannel），不会运行期才暴露成 403。
type PublicURLer interface {
	// PublicURL 返回对象的公开地址；key 为空或不可用时返回 ("", false)。
	PublicURL(key string) (string, bool)
}

// settings 是 system_config 的 `storage` 键（唯一解析点）。
type settings struct {
	Active   string                            `json:"active"`
	Channels map[string]map[string]interface{} `json:"channels"`
}

// settingsOf 解析当前生效的 storage 配置。
//
// **缺键不是"用默认值"**：返回 ErrNotConfigured 让写路径拒绝写入（503）。
// 这条判断是整个包唯一的"安全闸"：system_config 行丢失、启动加载失败、
// 新库没跑 seed 时，静默回落本地磁盘会让对象写进容器层 —— 管理台一切正常，
// 一次重建全部消失。宁可 503 让人看见，也不要写一个注定丢失的文件。
//
// JSON 非法仍归 ErrUnavailable（配置坏了是另一回事，运维需要区分）。
func settingsOf() (settings, error) {
	var s settings
	raw, ok := conf.System().Get("storage")
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return s, ErrNotConfigured
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return settings{}, err
	}
	if strings.TrimSpace(s.Active) == "" {
		return s, ErrNotConfigured
	}
	return s, nil
}
