// Package mediaprobe 通过 HTTP Range 分段读取 mp4/mov 的 moov box，得到媒体时长（秒）。
//
// 用途：参考视频按秒计费需要「上游视频到底多少秒」这个事实，而请求里只有 URL。
// 设计约束（与方案 v2 §2.2 一致）：
//   - 绝不下载整文件：首段 1 MiB + 尾段 512 KiB，单次读取上限 2 MiB；
//   - 失败一律报错，**不降级按 0 计费**（降级等于开放免费额度）；
//   - 出站走 netguard（连接期 IP 校验 + 重定向逐跳校验），与适配器策略一致；
//   - 不跨请求缓存（同一 URL 的内容可被替换，缓存会让计费口径漂移）。
package mediaprobe

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"lingdong-api/internal/pkg/netguard"
)

// 探测参数。数值与方案 §2.2.3 一致：单请求 8s、整体 10s、≤3 并发、单次读 ≤2 MiB。
const (
	firstChunk       = 1 << 20   // 首段申请 1 MiB
	tailChunk        = 512 << 10 // 尾段申请 512 KiB
	maxRead          = 2 << 20   // 单次实际读取上限 2 MiB
	moovLimit        = 8 << 20   // moov 精确区间的读取上限 8 MiB
	requestTimeout   = 8 * time.Second
	batchTimeout     = 10 * time.Second
	batchConcurrency = 3
	maxRedirects     = 3
)

// probePolicy 是「探测调用方提供的素材地址」时使用的出站策略。
//
// 零值 = 拒绝私网 / 回环 / 元数据地址。这里**刻意不读 LD_UPSTREAM_ALLOW_PRIVATE**：
// 那个开关是给「渠道 base_url」用的（自建 vLLM/Ollama），而本包的 URL 完全由
// 调用方控制，一旦继承该开关，用户就能把探测变成内网探针（盲 SSRF：靠
// 「能否读到时长」判定内网可达性）。
//
// 唯一允许改写它的是包内单测（TestMain 指向 httptest 的 127.0.0.1）。
// 生产代码里除 Duration 读取外不得赋值 —— 它不是配置项。
var probePolicy = netguard.Policy{}

// AllowLoopbackForTest 让探测放行回环/私网地址，**只给集成用例**用（指向 httptest）。
//
// 为什么不提供配置开关：本包的全部价值就在「调用方提供的 URL 永不触内网」。
// 一旦做成可配项（环境变量/系统配置），一次误配就把探测变成内网探针。
// 这里只留一个名字带 ForTest、不读任何环境的函数 —— 生产代码调用它等于**显式**
// 在改安全边界，读代码的人一眼能看到。
func AllowLoopbackForTest() {
	probePolicy = netguard.Policy{AllowPrivate: true, AllowLoopback: true}
}

// ResetPolicyForTest 恢复默认策略（同一进程内跑多组用例时用，避免互相污染）。
func ResetPolicyForTest() { probePolicy = netguard.Policy{} }

// ErrUnsupported 表示目标服务器不支持分段下载：在不下整文件的前提下读不到时长。
var ErrUnsupported = errors.New("目标服务器不支持分段下载，无法读取视频时长")

// ErrNoMoov 表示分段可读但没有找到 moov（fragmented MP4 且缺 mehd、或根本不是 mp4/mov）。
var ErrNoMoov = errors.New("不是有效的 MP4/MOV（缺少 moov），或为无索引的分片 MP4")

// Duration 返回 rawURL 指向的 mp4/mov 的时长（秒，向上取整）。
// 任何失败都返回 error，调用方必须转成调用方参数错误，不得按 0 计费。
func Duration(ctx context.Context, rawURL string) (int64, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return 0, errors.New("地址为空")
	}
	if strings.HasPrefix(strings.ToLower(rawURL), "data:") {
		return 0, errors.New("不支持内联 data: 素材，请先用 /v1/files 上传后改用直链")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, fmt.Errorf("地址不合法：%w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return 0, fmt.Errorf("仅支持 http/https 地址，实际为 %q", u.Scheme)
	}

	// 探测对象是**调用方提供的素材地址**，与"渠道 base_url"是两件事：
	// 后者允许内网（自建 vLLM/Ollama），前者永远不该访问私网/回环/元数据地址。
	// 因此这里**刻意不用 PolicyFromEnv()** —— 用它会让一个为自建上游打开的开关
	// 顺带把用户可控 URL 变成内网探针（盲 SSRF：靠"能不能读到时长"判定可达性）。
	pol := probePolicy
	if err := netguard.ValidateURLResolved(ctx, rawURL, pol); err != nil {
		return 0, err
	}
	cli := netguard.ClientWithRedirects(requestTimeout, pol, maxRedirects)
	return probe(ctx, cli, rawURL)
}

// Batch 对同一批 URL 去重后并发探测（≤3），整体受 ctx 与 batchTimeout 约束。
// 任一失败即整体失败：单个素材读不到时长就不能给出准确的冻结额。
func Batch(ctx context.Context, urls []string) (map[string]int64, error) {
	uniq := make([]string, 0, len(urls))
	seen := make(map[string]struct{}, len(urls))
	for _, raw := range urls {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if _, dup := seen[raw]; dup {
			continue
		}
		seen[raw] = struct{}{}
		uniq = append(uniq, raw)
	}
	if len(uniq) == 0 {
		return map[string]int64{}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, batchTimeout)
	defer cancel()

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	out := make(map[string]int64, len(uniq))
	sem := make(chan struct{}, batchConcurrency)

	for _, raw := range uniq {
		wg.Add(1)
		go func(raw string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				mu.Lock()
				if firstErr == nil {
					firstErr = ctx.Err()
				}
				mu.Unlock()
				return
			}
			secs, err := Duration(ctx, raw)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("参考视频 %s %w", MaskURL(raw), err)
				}
				return
			}
			out[raw] = secs
		}(raw)
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// MaskURL 去掉查询串与用户信息，用于错误文案与日志（签名参数不适合外泄）。
func MaskURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<无法解析的地址>"
	}
	u.RawQuery = ""
	u.Fragment = ""
	u.User = nil
	return u.String()
}

// probe 是探测主流程：首段 → （必要时）moov 精确区间 → 尾段回扫。
func probe(ctx context.Context, cli *http.Client, rawURL string) (int64, error) {
	head, meta, err := fetch(ctx, cli, rawURL, fmt.Sprintf("bytes=0-%d", firstChunk-1), maxRead)
	if err != nil {
		return 0, err
	}
	// 服务器忽略 Range：只能看到开头 maxRead 字节，若 moov 完整在其中仍可解析。
	if meta.status == http.StatusOK && meta.total > maxRead {
		if loc, found, _ := scanMoov(head); found && loc.size > 0 && loc.off+loc.size <= int64(len(head)) {
			return parseMoovBounded(head[loc.off : loc.off+loc.size])
		}
		return 0, ErrUnsupported
	}

	loc, found, hitMdat := scanMoov(head)
	if found {
		if loc.size == 0 {
			loc.size = meta.total - loc.off // size==0：box 延伸到文件末尾
		}
		if loc.size > 0 && loc.off+loc.size <= int64(len(head)) {
			return parseMoovBounded(head[loc.off : loc.off+loc.size])
		}
		if loc.size <= 0 || loc.size > moovLimit {
			return 0, fmt.Errorf("%w（moov 大小异常：%d 字节）", ErrNoMoov, loc.size)
		}
		return fetchMoov(ctx, cli, rawURL, loc.off, loc.size)
	}
	_ = hitMdat // moov 在 mdat 之后（或首段被截断），统一走尾段回扫

	return tailScan(ctx, cli, rawURL)
}

// tailScan 在文件尾部回扫 ASCII "moov"，用其前 4 字节的 size 还原 box。
// 这是「moov 在 mdat 之后」这一最常见排布（ffmpeg 默认 faststart 关闭时）的唯一可行路径。
func tailScan(ctx context.Context, cli *http.Client, rawURL string) (int64, error) {
	tail, meta, err := fetch(ctx, cli, rawURL, fmt.Sprintf("bytes=-%d", tailChunk), maxRead)
	if err != nil {
		return 0, err
	}
	// 从尾向前找最后一个 "moov"（避免命中 mdat 里的随机字节）。
	idx := bytes.LastIndex(tail, []byte("moov"))
	for idx >= 4 {
		start := idx - 4
		size := int64(binary.BigEndian.Uint32(tail[start:]))
		if size >= 8 && int64(start)+size <= int64(len(tail)) {
			return parseMoovBounded(tail[start : int64(start)+size])
		}
		if size >= 8 && size <= moovLimit && meta.start >= 0 {
			// moov 起点在尾段之前：用绝对偏移精确取一次。
			return fetchMoov(ctx, cli, rawURL, meta.start+int64(start), size)
		}
		next := bytes.LastIndex(tail[:idx], []byte("moov"))
		if next <= 0 {
			break
		}
		idx = next
	}
	if meta.status == http.StatusOK && meta.total > maxRead {
		return 0, ErrUnsupported
	}
	return 0, ErrNoMoov
}

// fetchMoov 按绝对偏移取整段 moov 后解析。
func fetchMoov(ctx context.Context, cli *http.Client, rawURL string, off, size int64) (int64, error) {
	if size <= 0 || size > moovLimit {
		return 0, fmt.Errorf("%w（moov 大小异常：%d 字节）", ErrNoMoov, size)
	}
	spec := fmt.Sprintf("bytes=%d-%d", off, off+size-1)
	body, _, err := fetch(ctx, cli, rawURL, spec, size)
	if err != nil {
		return 0, err
	}
	if int64(len(body)) < size {
		return 0, fmt.Errorf("%w（moov 读取不完整：期望 %d 字节，实际 %d 字节）", ErrNoMoov, size, len(body))
	}
	return parseMoovBounded(body[:size])
}

// rangeMeta 记录一次 Range 请求的响应语义。
type rangeMeta struct {
	status int
	start  int64 // 实际返回区间的起始绝对偏移；未知为 -1
	total  int64 // 文件总长；未知为 -1
}

// fetch 发起一次带 Range 的 GET，返回至多 limit 字节的响应体与区间元信息。
func fetch(ctx context.Context, cli *http.Client, rawURL, rangeSpec string, limit int64) ([]byte, rangeMeta, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, rangeMeta{}, err
	}
	req.Header.Set("Range", rangeSpec)
	req.Header.Set("Accept-Encoding", "identity") // 压缩会破坏字节偏移

	resp, err := cli.Do(req)
	if err != nil {
		return nil, rangeMeta{}, fmt.Errorf("请求失败：%w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // 只读，关闭错误无意义

	meta := rangeMeta{status: resp.StatusCode, start: -1, total: -1}
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		meta.start, meta.total = parseContentRange(cr)
	}
	if meta.status == http.StatusOK {
		meta.start = 0
		if resp.ContentLength >= 0 {
			meta.total = resp.ContentLength
		}
	}

	switch {
	case meta.status == http.StatusPartialContent, meta.status == http.StatusOK:
		// 正常
	case meta.status == http.StatusRequestedRangeNotSatisfiable:
		return nil, meta, errors.New("视频文件为空或地址返回 416")
	case meta.status == http.StatusUnauthorized, meta.status == http.StatusForbidden:
		return nil, meta, fmt.Errorf("参考视频不可访问（HTTP %d）", meta.status)
	case meta.status == http.StatusNotFound, meta.status == http.StatusGone:
		return nil, meta, fmt.Errorf("参考视频不存在（HTTP %d）", meta.status)
	default:
		return nil, meta, fmt.Errorf("参考视频不可访问（HTTP %d）", meta.status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, meta, fmt.Errorf("读取响应失败：%w", err)
	}
	if meta.status == http.StatusOK && resp.ContentLength > limit {
		// 服务器忽略了 Range 且文件比读取上限大：无法在不下载整文件的前提下解析。
		return body, meta, nil
	}
	return body, meta, nil
}

// parseContentRange 解析 "bytes 0-1023/4096"，返回 (start, total)。
// 非法或 "bytes */N" 时返回 (-1, total)。
func parseContentRange(v string) (int64, int64) {
	s := strings.TrimSpace(v)
	if !strings.HasPrefix(s, "bytes ") {
		return -1, -1
	}
	s = strings.TrimSpace(strings.TrimPrefix(s, "bytes "))
	slash := strings.IndexByte(s, '/')
	if slash < 0 {
		return -1, -1
	}
	total := int64(-1)
	if t := strings.TrimSpace(s[slash+1:]); t != "*" {
		if n, err := strconv.ParseInt(t, 10, 64); err == nil {
			total = n
		}
	}
	rest := s[:slash]
	if rest == "*" {
		return -1, total
	}
	dash := strings.IndexByte(rest, '-')
	if dash < 0 {
		return -1, total
	}
	start, err := strconv.ParseInt(strings.TrimSpace(rest[:dash]), 10, 64)
	if err != nil {
		return -1, total
	}
	return start, total
}

// moovLoc 是 moov box 在文件中的位置。size<=0 表示长度未知（size 字段为 0）。
type moovLoc struct {
	off  int64
	size int64
}

// scanMoov 按顶层 box 顺序在 buf（文件起始处的字节）中查找 moov。
// found=false 表示 buf 里没有；hitMdat=true 表示遇到了 mdat（moov 在其后）。
func scanMoov(buf []byte) (loc moovLoc, found, hitMdat bool) {
	off := 0
	for off+8 <= len(buf) {
		size := int64(binary.BigEndian.Uint32(buf[off:]))
		typ := string(buf[off+4 : off+8])
		hdr := int64(8)
		if size == 1 {
			if off+16 > len(buf) {
				return moovLoc{}, false, false
			}
			size = int64(binary.BigEndian.Uint64(buf[off+8:]))
			hdr = 16
		}
		if typ == "moov" {
			if size == 0 {
				return moovLoc{off: int64(off), size: 0}, true, false
			}
			if size < hdr {
				return moovLoc{}, false, false
			}
			return moovLoc{off: int64(off), size: size}, true, false
		}
		if typ == "mdat" {
			return moovLoc{}, false, true
		}
		if size == 0 || size < hdr {
			return moovLoc{}, false, false // 未知长度或损坏，无法继续按偏移遍历
		}
		if int64(off)+size > int64(len(buf)) {
			return moovLoc{}, false, false
		}
		off += int(size)
	}
	return moovLoc{}, false, false
}

// parseMoovBounded 解析一段完整的 moov box（含 box 头）。
func parseMoovBounded(box []byte) (int64, error) {
	hdr := 8
	if len(box) >= 16 && binary.BigEndian.Uint32(box) == 1 {
		hdr = 16
	}
	if len(box) < hdr {
		return 0, ErrNoMoov
	}
	payload := box[hdr:]

	var (
		mvhdTimescale  uint64
		mvhdDuration   uint64
		mehdDuration   uint64
		hasMehd        bool
		hasValidMvhdTS bool
	)
	off := 0
	for off+8 <= len(payload) {
		size := int64(binary.BigEndian.Uint32(payload[off:]))
		typ := string(payload[off+4 : off+8])
		if size < 8 || int64(off)+size > int64(len(payload)) {
			break
		}
		child := payload[off+8 : int64(off)+size]
		switch typ {
		case "mvhd":
			if ts, dur, ok := parseMvhd(child); ok {
				mvhdTimescale, mvhdDuration, hasValidMvhdTS = ts, dur, ts > 0
			}
		case "mvex":
			if dur, ok := parseMehd(child); ok && dur > 0 {
				mehdDuration, hasMehd = dur, true
			}
		}
		off += int(size)
	}

	if hasValidMvhdTS && mvhdDuration > 0 {
		return ceilDiv(mvhdDuration, mvhdTimescale), nil
	}
	// fragmented MP4：mvhd.duration 为 0，总时长只在 mvex/mehd 里。
	if hasMehd {
		ts := mvhdTimescale
		if ts == 0 {
			ts = 1000 // mehd 无自己的 timescale，按 mvhd；极端情况下按毫秒
		}
		return ceilDiv(mehdDuration, ts), nil
	}
	if hasValidMvhdTS {
		return 0, errors.New("视频时长字段为 0（可能是流式转码产物）")
	}
	return 0, ErrNoMoov
}

// parseMvhd 解析 mvhd：v0 = 4+4+4+4（creation/modification/timescale/duration），v1 = 8+8+4+8。
func parseMvhd(b []byte) (timescale, duration uint64, ok bool) {
	if len(b) < 4 {
		return 0, 0, false
	}
	switch b[0] {
	case 0:
		if len(b) < 20 {
			return 0, 0, false
		}
		return uint64(binary.BigEndian.Uint32(b[12:])), uint64(binary.BigEndian.Uint32(b[16:])), true
	case 1:
		if len(b) < 32 {
			return 0, 0, false
		}
		return uint64(binary.BigEndian.Uint32(b[20:])), binary.BigEndian.Uint64(b[24:]), true
	}
	return 0, 0, false
}

// parseMehd 在 mvex 的子 box 里找 mehd，返回 fragment_duration。
func parseMehd(mvex []byte) (uint64, bool) {
	off := 0
	for off+8 <= len(mvex) {
		size := int64(binary.BigEndian.Uint32(mvex[off:]))
		typ := string(mvex[off+4 : off+8])
		if size < 8 || int64(off)+size > int64(len(mvex)) {
			return 0, false
		}
		if typ == "mehd" {
			b := mvex[off+8 : int64(off)+size]
			if len(b) < 4 {
				return 0, false
			}
			switch b[0] {
			case 0:
				if len(b) < 8 {
					return 0, false
				}
				return uint64(binary.BigEndian.Uint32(b[4:])), true
			case 1:
				if len(b) < 12 {
					return 0, false
				}
				return binary.BigEndian.Uint64(b[4:]), true
			}
			return 0, false
		}
		off += int(size)
	}
	return 0, false
}

// ceilDiv 向上取整除法（duration/timescale → 秒）。
func ceilDiv(dur, ts uint64) int64 {
	if ts == 0 {
		return 0
	}
	return int64((dur + ts - 1) / ts)
}
