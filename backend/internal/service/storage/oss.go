package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"

	"lingdong-api/internal/pkg/netguard"
)

// ---------- 阿里云 OSS ----------

// ossStore 是阿里云 OSS 后端（官方 SDK）。
//
// scheme/host/bucketName 只服务于 PublicURL：SDK 的 Bucket 对象不暴露"对象对外地址"
// （它只提供签名 URL）。公开读场景下地址是纯拼装，所以在这里保留构造时的三个要素，
// 而不是从 SDK 反推 —— 反推不出自定义 endpoint 的真实形态。
type ossStore struct {
	name       string
	scheme     string
	host       string
	bucketName string
	bucket     *oss.Bucket
	// client 只用于桶级调用（GetBucketInfo / GetBucketVersioning）：Bucket 上没有这两个方法。
	client *oss.Client
}

func newOSSStore(name string, ch map[string]interface{}) (Store, error) {
	endpoint := normalizeEndpoint(channelString(ch, "endpoint"))
	bucketName := channelString(ch, "bucket")
	ak := channelString(ch, "access_key_id")
	sk := channelString(ch, "access_key_secret")
	if endpoint == "" || bucketName == "" || ak == "" || sk == "" {
		return nil, fmt.Errorf("%w: %s 缺少 endpoint/bucket/access_key_id/access_key_secret",
			ErrUnavailable, name)
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("%w: %s endpoint 非法: %q", ErrUnavailable, name, endpoint)
	}
	// 凭据是运营可配置的输入，但仍要先过 netguard 的字面量校验：
	// 填成 127.0.0.1 或裸主机名时，报错应当是「地址不被允许」而不是运行期连接超时。
	if err := netguard.ValidateURL(endpoint, netguard.PolicyFromEnv()); err != nil {
		return nil, err
	}
	client, err := oss.New(endpoint, ak, sk, oss.HTTPClient(guardedClient()))
	if err != nil {
		return nil, fmt.Errorf("%w: %s 初始化失败: %v", ErrUnavailable, name, err)
	}
	// SDK 默认 RetryTimes=5。关掉：重试与项目幂等（转存靠 f=0 补、清理靠下轮）
	// 叠在一起会变成「一次超时 × 5 次重试 × 30 分钟」，把 worker 长时间占死。
	client.Config.RetryTimes = 0
	b, err := client.Bucket(bucketName)
	if err != nil {
		return nil, fmt.Errorf("%w: %s 桶 %q 不可用: %v", ErrUnavailable, name, bucketName, err)
	}
	return &ossStore{name: name, scheme: u.Scheme, host: u.Host, bucketName: bucketName, bucket: b, client: client}, nil
}

func (s *ossStore) Name() string { return s.name }

// Bucket 返回该实例实际寻址的桶（写入时落库到 files/uploads/icons 的 storage_bucket 列）。
func (s *ossStore) Bucket() string { return s.bucketName }

// CheckBucket 确认桶可寻址，并报告版本控制状态。
//
// 版本控制必须单独查：开着的时候 DeleteObject 只产生 delete marker，
// GetObjectMeta 看不到旧版本 —— 删除「复核通过」但数据与账单都还在。
func (s *ossStore) CheckBucket(ctx context.Context) (BucketState, error) {
	ctx, cancel := metaContext(ctx)
	defer cancel()
	if _, err := s.client.GetBucketInfo(s.bucketName, oss.WithContext(ctx)); err != nil {
		if errors.Is(s.mapErr(err), ErrObjectNotFound) {
			return BucketState{}, nil // 桶不存在：由调用方按「不可达」处理
		}
		return BucketState{}, s.mapErr(err)
	}
	state := BucketState{Exists: true}
	// 版本控制的查询失败不阻断删除：只有明确 Enabled 才算开启。
	if v, err := s.client.GetBucketVersioning(s.bucketName, oss.WithContext(ctx)); err == nil {
		state.Versioned = strings.EqualFold(strings.TrimSpace(string(v.Status)), "Enabled")
	}
	return state, nil
}

// Head 只取对象元信息（删除复核用）。不存在必须返回 ErrObjectNotFound。
func (s *ossStore) Head(ctx context.Context, key string) (ObjectInfo, error) {
	ctx, cancel := metaContext(ctx)
	defer cancel()
	h, err := s.bucket.GetObjectMeta(key, oss.WithContext(ctx))
	if err != nil {
		return ObjectInfo{}, s.mapErr(err)
	}
	info := ObjectInfo{ContentType: h.Get("Content-Type"), ETag: h.Get("ETag")}
	if n, e := strconv.ParseInt(h.Get("Content-Length"), 10, 64); e == nil {
		info.Size = n
	}
	if t, e := http.ParseTime(h.Get("Last-Modified")); e == nil {
		info.ModTime = t
	}
	return info, nil
}

// List 按前缀分页列举对象（孤儿对账用）。
func (s *ossStore) List(ctx context.Context, prefix, marker string, limit int) (ListPage, error) {
	ctx, cancel := metaContext(ctx)
	defer cancel()
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	opts := []oss.Option{oss.Prefix(prefix), oss.MaxKeys(limit), oss.WithContext(ctx)}
	if marker != "" {
		opts = append(opts, oss.Marker(marker))
	}
	res, err := s.bucket.ListObjects(opts...)
	if err != nil {
		return ListPage{}, s.mapErr(err)
	}
	page := ListPage{}
	for _, o := range res.Objects {
		page.Objects = append(page.Objects, ListedObject{Key: o.Key, Size: o.Size, ModTime: o.LastModified})
	}
	if res.IsTruncated {
		// OSS 只在带 delimiter 时回 NextMarker；不带 delimiter 时必须用最后一个 key 续页。
		page.Next = res.NextMarker
		if page.Next == "" && len(res.Objects) > 0 {
			page.Next = res.Objects[len(res.Objects)-1].Key
		}
	}
	return page, nil
}

// PublicURL 返回 OSS 对象的公开地址：`{scheme}://{bucket}.{endpoint}/{key}`。
//
// endpoint 是 region 级域名（`oss-cn-shanghai.aliyuncs.com`），桶走子域；
// 这条规则只在官方文档的"访问域名"一节出现一次，所以只在这里实现。
func (s *ossStore) PublicURL(key string) (string, bool) {
	if strings.TrimSpace(key) == "" {
		return "", false
	}
	return fmt.Sprintf("%s://%s.%s/%s", s.scheme, s.bucketName, s.host, escapeObjectKey(key)), true
}

// Put 写入对象。
//
// 长度只能靠 reader 类型表达：OSS 的 conn.handleBody 先 GetReaderLen，探测失败时
// req.ContentLength 保持 0，然后**无条件** Set("Content-Length","0") —— 用
// oss.ContentLength 选项也会被这行覆盖，结果是 200 + 0 字节对象。
// GetReaderLen 的第一个 case 正是 *io.LimitedReader，所以 LimitReader 是唯一正确形态。
func (s *ossStore) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if size <= 0 {
		return ErrNeedLength
	}
	ctx, cancel := dataContext(ctx)
	defer cancel()
	opts := []oss.Option{}
	if contentType != "" {
		opts = append(opts, oss.ContentType(contentType))
	}
	if err := s.bucket.PutObject(key, io.LimitReader(r, size), opts...); err != nil {
		return s.mapErr(err)
	}
	return nil
}

func (s *ossStore) Open(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	ctx, cancel := dataContext(ctx)
	res, err := s.bucket.DoGetObject(&oss.GetObjectRequest{ObjectKey: key}, nil)
	if err != nil {
		cancel()
		return nil, 0, s.mapErr(err)
	}
	// 长度取响应头：oss.Response 不是 http.Response，没有 ContentLength 字段。
	// 取不到就返回 -1（未知），由调用方按 -1 处理，绝不猜一个数。
	size := int64(-1)
	if v := res.Response.Headers.Get("Content-Length"); v != "" {
		if n, e := strconv.ParseInt(v, 10, 64); e == nil && n >= 0 {
			size = n
		}
	}
	return readBody(res.Response.Body, cancel), size, nil
}

func (s *ossStore) OpenRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	ctx, cancel := dataContext(ctx)
	res, err := s.bucket.DoGetObject(&oss.GetObjectRequest{ObjectKey: key},
		[]oss.Option{oss.NormalizedRange(rangeHeader(offset, length))})
	if err != nil {
		cancel()
		return nil, s.mapErr(err)
	}
	return readBody(res.Response.Body, cancel), nil
}

func (s *ossStore) Delete(ctx context.Context, key string) error {
	ctx, cancel := metaContext(ctx)
	defer cancel()
	// 契约：对象不存在视为成功。OSS 对不存在的 key 通常回 204，
	// 但桶开版本控制或被策略拦截时会回 404，统一按成功处理。
	if err := s.mapErr(s.bucket.DeleteObject(key)); err != nil && !errors.Is(err, ErrObjectNotFound) {
		return err
	}
	return nil
}

// mapErr 把 OSS 的错误归一为域内哨兵（唯一处）。
func (s *ossStore) mapErr(err error) error {
	if err == nil {
		return nil
	}
	// 地址被安全策略拒绝时原样上抛：管理台探针要显示「地址不被允许」，
	// 而不是含混的「连接失败」。
	if errors.Is(err, netguard.ErrBlocked) {
		return err
	}
	if se, ok := asOSSServiceError(err); ok {
		switch {
		case se.Code == "NoSuchBucket":
			// 桶不存在必须与「对象不存在」分开：前者是配置事故，
			// 混成 ErrObjectNotFound 会让「桶名写错」表现为「对象已经删干净了」。
			return ErrBackendGone
		case se.StatusCode == http.StatusNotFound, se.Code == "NoSuchKey", se.Code == "NoSuchObject":
			return ErrObjectNotFound
		case se.StatusCode == http.StatusForbidden, se.Code == "InvalidAccessKeyId",
			se.Code == "SignatureDoesNotMatch", se.Code == "AccessDenied", se.Code == "SecurityTokenExpired":
			// 只回错误码，不回显 AK/签名原文（错误串会进日志与任务详情）。
			return fmt.Errorf("%w: %s 鉴权失败(%s)", ErrUnavailable, s.name, se.Code)
		default:
			return fmt.Errorf("%w: %s 返回 %d %s", ErrUnavailable, s.name, se.StatusCode, se.Code)
		}
	}
	if IsTimeout(err) {
		return fmt.Errorf("%w: %s: %v", ErrTimeout, s.name, err)
	}
	return fmt.Errorf("%w: %s: %v", ErrUnavailable, s.name, err)
}

// asOSSServiceError 兼容 SDK 返回 ServiceError 的值与指针两种形态。
//
// SDK 内部走 serviceErrFromXML 返回值类型，但调用链上可能被 fmt.Errorf("%w") 包过，
// 也可能在未来版本改成指针 —— 两种都认，避免「404 被当成 5xx」。
func asOSSServiceError(err error) (oss.ServiceError, bool) {
	var se oss.ServiceError
	if errors.As(err, &se) {
		return se, true
	}
	var pse *oss.ServiceError
	if errors.As(err, &pse) && pse != nil {
		return *pse, true
	}
	return oss.ServiceError{}, false
}

// rangeHeader 生成 HTTP Range 头；length<=0 表示「从 offset 到末尾」。
func rangeHeader(offset, length int64) string {
	if length > 0 {
		return fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
	}
	return fmt.Sprintf("bytes=%d-", offset)
}

// normalizeEndpoint 把运营填的 endpoint 规整成 SDK 接受的形态：
// 无 scheme 补 https://，去尾部 `/`。
//
// 刻意不加 region 字段：OSS 的 region 信息已经在 endpoint 里
// （oss-cn-hangzhou.aliyuncs.com），再加一个就是假配置。
func normalizeEndpoint(ep string) string {
	s := strings.TrimSpace(ep)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	return strings.TrimRight(s, "/")
}
