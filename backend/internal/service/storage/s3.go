package storage

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------- S3 兼容后端（s3 / minio） ----------

// s3Store 以 AWS SigV4 直连 S3 兼容端点（无第三方 SDK 依赖）。
// 桶寻址支持 path-style（自建 minio 常用）与 virtual-host（AWS 默认）。
type s3Store struct {
	name      string
	scheme    string
	host      string // 不含桶
	bucket    string
	region    string
	accessKey string
	secretKey string
	pathStyle bool
	client    *http.Client
}

func newS3Store(name string, ch map[string]interface{}) (Store, error) {
	endpoint := channelString(ch, "endpoint")
	bucket := channelString(ch, "bucket")
	accessKey := channelString(ch, "access_key")
	secretKey := channelString(ch, "secret_key")
	if endpoint == "" || bucket == "" || accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("%w: %s 缺少 endpoint/bucket/access_key/secret_key", ErrUnavailable, name)
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("%w: %s endpoint 非法: %q", ErrUnavailable, name, endpoint)
	}
	pathStyle := channelBool(ch, "path_style", name == "minio")
	region := channelString(ch, "region")
	if region == "" {
		region = "us-east-1"
	}
	return &s3Store{
		name: name, scheme: u.Scheme, host: u.Host, bucket: bucket, region: region,
		accessKey: accessKey, secretKey: secretKey, pathStyle: pathStyle,
		client: guardedClient(),
	}, nil
}

func (s *s3Store) Name() string { return s.name }

// Bucket 返回该实例实际寻址的桶。
func (s *s3Store) Bucket() string { return s.bucket }

// bucketTarget 返回桶级 URL 与签名用的 canonical URI（Head/List/Versioning 用）。
func (s *s3Store) bucketTarget() (rawURL, canonicalURI string) {
	if s.pathStyle {
		return fmt.Sprintf("%s://%s/%s", s.scheme, s.host, s.bucket), "/" + s.bucket
	}
	return fmt.Sprintf("%s://%s.%s/", s.scheme, s.bucket, s.host), "/"
}

// Head 只取对象元信息（删除复核用）。
func (s *s3Store) Head(ctx context.Context, key string) (ObjectInfo, error) {
	ctx, cancel := metaContext(ctx)
	defer cancel()
	rawURL, uri := s.target(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return ObjectInfo{}, err
	}
	resp, err := s.do(req, uri)
	if err != nil {
		return ObjectInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ObjectInfo{}, ErrObjectNotFound
	}
	if resp.StatusCode/100 != 2 {
		return ObjectInfo{}, s.failed(resp)
	}
	info := ObjectInfo{
		Size:        resp.ContentLength,
		ContentType: resp.Header.Get("Content-Type"),
		ETag:        resp.Header.Get("ETag"),
	}
	if t, e := http.ParseTime(resp.Header.Get("Last-Modified")); e == nil {
		info.ModTime = t
	}
	return info, nil
}

// CheckBucket 确认桶可寻址，并读取版本控制状态。
//
// S3 的 HEAD 在「桶不存在」与「无权访问」时都回 404/403，因此这里把
// 404 当作「不存在」、403 当作「不可用」：两者都会让 DeleteRefVerified 返回
// Unverified，区别只在日志里的话术。
func (s *s3Store) CheckBucket(ctx context.Context) (BucketState, error) {
	ctx, cancel := metaContext(ctx)
	defer cancel()
	rawURL, uri := s.bucketTarget()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return BucketState{}, err
	}
	resp, err := s.do(req, uri)
	if err != nil {
		return BucketState{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return BucketState{}, nil
	}
	if resp.StatusCode/100 != 2 {
		return BucketState{}, s.failed(resp)
	}
	state := BucketState{Exists: true}
	// 版本控制的查询失败不阻断删除：只有明确 Enabled 才算开启。
	if v, err := s.versioning(ctx); err == nil {
		state.Versioned = v
	}
	return state, nil
}

// versioning 读取桶的版本控制状态（GET ?versioning，查询参数参与签名）。
func (s *s3Store) versioning(ctx context.Context) (bool, error) {
	rawURL, uri := s.bucketTarget()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL+"?versioning", nil)
	if err != nil {
		return false, err
	}
	resp, err := s.doQuery(req, uri, "versioning=")
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return false, s.failed(resp)
	}
	var out struct {
		Status string `xml:"Status"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&out); err != nil {
		return false, err
	}
	return strings.EqualFold(strings.TrimSpace(out.Status), "Enabled"), nil
}

// List 按前缀分页列举对象（孤儿对账用，ListObjectsV2）。
func (s *s3Store) List(ctx context.Context, prefix, marker string, limit int) (ListPage, error) {
	ctx, cancel := metaContext(ctx)
	defer cancel()
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	q := url.Values{}
	q.Set("list-type", "2")
	q.Set("max-keys", strconv.Itoa(limit))
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	if marker != "" {
		q.Set("continuation-token", marker)
	}
	rawURL, uri := s.bucketTarget()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL+"?"+q.Encode(), nil)
	if err != nil {
		return ListPage{}, err
	}
	resp, err := s.doQuery(req, uri, canonicalQuery(q))
	if err != nil {
		return ListPage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ListPage{}, ErrBackendGone
	}
	if resp.StatusCode/100 != 2 {
		return ListPage{}, s.failed(resp)
	}
	var out struct {
		IsTruncated           bool   `xml:"IsTruncated"`
		NextContinuationToken string `xml:"NextContinuationToken"`
		Contents              []struct {
			Key          string `xml:"Key"`
			Size         int64  `xml:"Size"`
			LastModified string `xml:"LastModified"`
		} `xml:"Contents"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ListPage{}, fmt.Errorf("%w: %s 列举结果解析失败: %v", ErrUnavailable, s.name, err)
	}
	page := ListPage{}
	for _, o := range out.Contents {
		item := ListedObject{Key: o.Key, Size: o.Size}
		if t, e := time.Parse(time.RFC3339, o.LastModified); e == nil {
			item.ModTime = t
		}
		page.Objects = append(page.Objects, item)
	}
	if out.IsTruncated {
		page.Next = out.NextContinuationToken
	}
	return page, nil
}

// target 返回对象 URL 与签名用的 canonical URI。
func (s *s3Store) target(key string) (rawURL, canonicalURI string) {
	escaped := escapeObjectKey(key)
	if s.pathStyle {
		return fmt.Sprintf("%s://%s/%s/%s", s.scheme, s.host, s.bucket, escaped),
			"/" + s.bucket + "/" + escaped
	}
	return fmt.Sprintf("%s://%s.%s/%s", s.scheme, s.bucket, s.host, escaped), "/" + escaped
}

// PublicURL 返回对象的公开地址（与 Put/Open 同一个 target，避免两套拼法漂移）。
func (s *s3Store) PublicURL(key string) (string, bool) {
	if strings.TrimSpace(key) == "" {
		return "", false
	}
	raw, _ := s.target(key)
	return raw, true
}

// Put 写入对象。
//
// 长度只有一份真相：调用方给的 size。这里用 io.LimitReader 包一层再交给 http，
// 是因为「以 reader 为准」的话 http 会走 chunked 编码，而部分 S3 兼容实现
// 对 chunked PUT 直接 501；同时 LimitReader 也守住了「声明多少就发多少」。
func (s *s3Store) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if size <= 0 {
		return ErrNeedLength
	}
	ctx, cancel := dataContext(ctx)
	defer cancel()
	rawURL, uri := s.target(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, rawURL, io.LimitReader(r, size))
	if err != nil {
		return err
	}
	req.ContentLength = size
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := s.do(req, uri)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return s.failed(resp)
	}
	return nil
}

func (s *s3Store) Open(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	ctx, cancel := dataContext(ctx)
	rawURL, uri := s.target(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		cancel()
		return nil, 0, err
	}
	resp, err := s.do(req, uri)
	if err != nil {
		cancel()
		return nil, 0, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		cancel()
		return nil, 0, ErrObjectNotFound
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		defer cancel()
		return nil, 0, s.failed(resp)
	}
	return readBody(resp.Body, cancel), resp.ContentLength, nil
}

// OpenRange 按区间读取 S3 对象（GET + Range 头）。
//
// Range 头刻意不加入 SigV4 的 signedHeaders：SigV4 只要求签 host 与全部 x-amz-* 头，
// 把 Range 加进去会让所有不支持该扩展的 S3 兼容实现（部分自建 MinIO 版本）验签失败。
func (s *s3Store) OpenRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	ctx, cancel := dataContext(ctx)
	rawURL, uri := s.target(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	switch {
	case length > 0:
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	case offset > 0:
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := s.do(req, uri)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		cancel()
		return nil, ErrObjectNotFound
	}
	// 206 是正常区间响应；部分实现忽略 Range 直接回 200 全量，也接受（由调用方按长度截断）。
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		defer cancel()
		return nil, s.failed(resp)
	}
	return readBody(resp.Body, cancel), nil
}

func (s *s3Store) Delete(ctx context.Context, key string) error {
	ctx, cancel := metaContext(ctx)
	defer cancel()
	rawURL, uri := s.target(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := s.do(req, uri)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode/100 != 2 {
		return s.failed(resp)
	}
	return nil
}

func (s *s3Store) do(req *http.Request, canonicalURI string) (*http.Response, error) {
	return s.doQuery(req, canonicalURI, "")
}

// doQuery 与 do 的差别只有一条：规范化查询串参与签名（List/Versioning 必需）。
//
// 旧实现把查询串硬编码成空串，所以带参数的请求一律验签失败 —— 这也是
// 「列举能力」一直没做的原因。查询串的构造集中在 canonicalQuery，
// 不散在调用点，避免同一个参数在两处编码不一致。
func (s *s3Store) doQuery(req *http.Request, canonicalURI, canonicalQueryStr string) (*http.Response, error) {
	amzDate := time.Now().UTC().Format("20060102T150405Z")
	dateStamp := amzDate[:8]
	const payloadHash = "UNSIGNED-PAYLOAD"

	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	req.Host = req.URL.Host
	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		req.Host, payloadHash, amzDate)
	canonicalRequest := strings.Join([]string{
		req.Method, canonicalURI, canonicalQueryStr, canonicalHeaders, signedHeaders, payloadHash,
	}, "\n")

	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, s.region)
	sum := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])

	key := hmacSHA256([]byte("AWS4"+s.secretKey), dateStamp)
	key = hmacSHA256(key, s.region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.accessKey, scope, signedHeaders, signature))
	return s.client.Do(req)
}

func (s *s3Store) failed(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("%w: %s 返回 %d %s", ErrUnavailable, s.name, resp.StatusCode,
		strings.TrimSpace(string(body)))
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// canonicalQuery 按 SigV4 规则构造规范化查询串：参数名升序、同名的值升序、
// 逐段 URI 编码、空值保留 `=`（`?versioning` 的规范化形态就是 `versioning=`）。
func canonicalQuery(v url.Values) string {
	if len(v) == 0 {
		return ""
	}
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		vals := append([]string(nil), v[k]...)
		sort.Strings(vals)
		for _, val := range vals {
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(val, true))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode 是 SigV4 的百分号编码：只保留 unreserved 字符，其余用大写十六进制。
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// escapeObjectKey 按 AWS SigV4 规则逐段编码对象 key：非 unreserved 字符一律百分号编码，保留 `/` 分隔。
//
// 三个驱动共用：S3 签名、OSS/COS 的公开地址都必须按同一规则转义，
// 各写一份必然出现「上传成功、直链 404」（路径段编码不一致）。
func escapeObjectKey(key string) string {
	var b strings.Builder
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~', c == '/':
			b.WriteByte(c)
		default:
			b.WriteString(fmt.Sprintf("%%%02X", c))
		}
	}
	return b.String()
}
