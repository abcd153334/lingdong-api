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
	"time"

	"github.com/tencentyun/cos-go-sdk-v5"

	"lingdong-api/internal/pkg/netguard"
)

// ---------- 腾讯云 COS ----------

// cosStore 是腾讯云 COS 后端（官方 SDK）。
type cosStore struct {
	name string
	// bucketName 是该实例实际寻址的桶：写入落库、删除复核都以它为准。
	bucketName string
	client     *cos.Client
	// bucketURL 是对象的公开地址前缀（`{bucket}.cos.{region}.myqcloud.com` 或配置的
	// 自定义域名）。PublicURL 直接拼它，不再从 SDK 反推 —— 自定义 endpoint
	// （私有化/CDN 回源域）在 SDK 里没有可读取的等价字段。
	bucketURL string
}

func newCOSStore(name string, ch map[string]interface{}) (Store, error) {
	region := channelString(ch, "region")
	bucket := channelString(ch, "bucket")
	secretID := channelString(ch, "secret_id")
	secretKey := channelString(ch, "secret_key")
	if region == "" || bucket == "" || secretID == "" || secretKey == "" {
		return nil, fmt.Errorf("%w: %s 缺少 region/bucket/secret_id/secret_key", ErrUnavailable, name)
	}
	raw := cosBucketURL(bucket, region, channelString(ch, "endpoint"))
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("%w: %s 访问域名非法: %q", ErrUnavailable, name, raw)
	}
	// 访问域名是运营可配置的半可信输入（支持私有化/加速域名）。
	// 先过 netguard 的字符面校验，填成内网字面量时立即报「地址不被允许」。
	if err := netguard.ValidateURL(raw, netguard.PolicyFromEnv()); err != nil {
		return nil, err
	}
	client := cos.NewClient(&cos.BaseURL{BucketURL: u}, guardedClient())
	// SDK 默认重试 3 次。关掉：Put 传的是非 Seeker 的 *io.LimitedReader，本来就只有
	// 1 次机会；Get/Delete 的重试叠加上层补转存会变成「一次故障重放很多次」，
	// 重试策略统一交给上层（f=0 补转存、清理任务下轮）。
	client.Conf.RetryOpt.Count = 0
	return &cosStore{name: name, bucketName: bucket, client: client, bucketURL: strings.TrimRight(raw, "/")}, nil
}

func (s *cosStore) Name() string { return s.name }

// Bucket 返回该实例实际寻址的桶。
func (s *cosStore) Bucket() string { return s.bucketName }

// CheckBucket 确认桶可寻址，并报告版本控制状态（开启时删除只产生 delete marker）。
func (s *cosStore) CheckBucket(ctx context.Context) (BucketState, error) {
	ctx, cancel := metaContext(ctx)
	defer cancel()
	if _, err := s.client.Bucket.Head(ctx); err != nil {
		if cos.IsNotFoundError(err) {
			return BucketState{}, nil
		}
		return BucketState{}, s.mapErr(err)
	}
	state := BucketState{Exists: true}
	// 查询版本控制失败不阻断删除：只有明确 Enabled 才算开启。
	if v, _, err := s.client.Bucket.GetVersioning(ctx); err == nil && v != nil {
		state.Versioned = strings.EqualFold(strings.TrimSpace(v.Status), "Enabled")
	}
	return state, nil
}

// Head 只取对象元信息（删除复核用）。
func (s *cosStore) Head(ctx context.Context, key string) (ObjectInfo, error) {
	ctx, cancel := metaContext(ctx)
	defer cancel()
	resp, err := s.client.Object.Head(ctx, key, nil)
	if err != nil {
		return ObjectInfo{}, s.mapErr(err)
	}
	h := resp.Header
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
func (s *cosStore) List(ctx context.Context, prefix, marker string, limit int) (ListPage, error) {
	ctx, cancel := metaContext(ctx)
	defer cancel()
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	res, _, err := s.client.Bucket.Get(ctx, &cos.BucketGetOptions{Prefix: prefix, Marker: marker, MaxKeys: limit})
	if err != nil {
		return ListPage{}, s.mapErr(err)
	}
	page := ListPage{}
	for _, o := range res.Contents {
		page.Objects = append(page.Objects, ListedObject{Key: o.Key, Size: o.Size, ModTime: parseCOSTime(o.LastModified)})
	}
	if res.IsTruncated {
		page.Next = res.NextMarker
		if page.Next == "" && len(res.Contents) > 0 {
			page.Next = res.Contents[len(res.Contents)-1].Key
		}
	}
	return page, nil
}

// parseCOSTime 解析 COS 列举结果里的时间串（ISO8601，毫秒可选）。
//
// 解析失败返回零值：对账任务把零值当作「时间未知」并跳过删除
// （把未知当成很久以前会误删新建对象）。
func parseCOSTime(v string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t
		}
	}
	return time.Time{}
}

// PublicURL 返回 COS 对象的公开地址：`{bucketURL}/{key}`。
func (s *cosStore) PublicURL(key string) (string, bool) {
	if strings.TrimSpace(key) == "" {
		return "", false
	}
	return s.bucketURL + "/" + escapeObjectKey(key), true
}

// Put 写入对象。
//
// ContentLength 必须显式给：Object.Put 只在 IsLenReader(r) 为真时自动填，
// 而 IsLenReader 的白名单只有 bytes.Buffer/bytes.Reader/strings.Reader/os.File，
// 不认 *io.LimitedReader。不给就走 chunked 或按 0 处理。
func (s *cosStore) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if size <= 0 {
		return ErrNeedLength
	}
	ctx, cancel := dataContext(ctx)
	defer cancel()
	header := &cos.ObjectPutHeaderOptions{ContentLength: size, ContentType: contentType}
	_, err := s.client.Object.Put(ctx, key, io.LimitReader(r, size),
		&cos.ObjectPutOptions{ObjectPutHeaderOptions: header})
	return s.mapErr(err)
}

func (s *cosStore) Open(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	ctx, cancel := dataContext(ctx)
	res, err := s.client.Object.Get(ctx, key, nil)
	if err != nil {
		cancel()
		return nil, 0, s.mapErr(err)
	}
	size := res.ContentLength
	if size < 0 {
		// 分块传输时 ContentLength 可能是 -1：保留 -1（未知），不猜。
		size = -1
	}
	return readBody(res.Body, cancel), size, nil
}

func (s *cosStore) OpenRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	ctx, cancel := dataContext(ctx)
	res, err := s.client.Object.Get(ctx, key, &cos.ObjectGetOptions{Range: rangeHeader(offset, length)})
	if err != nil {
		cancel()
		return nil, s.mapErr(err)
	}
	return readBody(res.Body, cancel), nil
}

func (s *cosStore) Delete(ctx context.Context, key string) error {
	ctx, cancel := metaContext(ctx)
	defer cancel()
	// 契约：对象不存在视为成功（COS 对不存在的 key 回 204，被策略拦截时回 404）。
	if _, err := s.client.Object.Delete(ctx, key); err != nil {
		if cos.IsNotFoundError(err) {
			return nil
		}
		return s.mapErr(err)
	}
	return nil
}

// mapErr 把 COS 的错误归一为域内哨兵（唯一处）。
func (s *cosStore) mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, netguard.ErrBlocked) {
		return err
	}
	var er *cos.ErrorResponse
	if errors.As(err, &er) {
		code := er.Code
		status := 0
		if er.Response != nil {
			status = er.Response.StatusCode
		}
		switch {
		case code == "NoSuchBucket":
			// 桶不存在是配置事故，不能与「对象不存在」混为一谈。
			return ErrBackendGone
		case status == http.StatusNotFound, code == "NoSuchKey":
			return ErrObjectNotFound
		case status == http.StatusForbidden, code == "AccessDenied",
			code == "InvalidAccessKeyId", code == "SignatureDoesNotMatch":
			// 只回错误码，不回显密钥/签名原文。
			return fmt.Errorf("%w: %s 鉴权失败(%s)", ErrUnavailable, s.name, code)
		default:
			return fmt.Errorf("%w: %s 返回 %d %s", ErrUnavailable, s.name, status, code)
		}
	}
	if IsTimeout(err) {
		return fmt.Errorf("%w: %s: %v", ErrTimeout, s.name, err)
	}
	return fmt.Errorf("%w: %s: %v", ErrUnavailable, s.name, err)
}

// cosBucketURL 拼 COS 访问域名。
//
// 默认 https://{bucket}.cos.{region}.myqcloud.com；配置里给了 endpoint 就用它
// （私有化部署、全球加速域名、自定义 CDN 回源域名的逃生口）。
func cosBucketURL(bucket, region, endpoint string) string {
	if ep := normalizeEndpoint(endpoint); ep != "" {
		return ep
	}
	return fmt.Sprintf("https://%s.cos.%s.myqcloud.com", bucket, region)
}
