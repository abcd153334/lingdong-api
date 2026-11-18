package task

import (
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"lingdong-api/internal/model"
	"lingdong-api/internal/service/billing"
)

// ---- 最小 mp4 构造（与 internal/pkg/mediaprobe 的构造保持同一格式） ----

func mp4Box(typ string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out, uint32(8+len(payload)))
	copy(out[4:8], typ)
	copy(out[8:], payload)
	return out
}

// mp4WithSeconds 构造一个时长为 secs 秒的最小 mp4（ftyp + moov/mvhd + mdat）。
func mp4WithSeconds(secs uint32) []byte {
	mvhd := make([]byte, 20)
	binary.BigEndian.PutUint32(mvhd[12:], 1000)
	binary.BigEndian.PutUint32(mvhd[16:], secs*1000)
	ftyp := mp4Box("ftyp", []byte("isom\x00\x00\x00\x00isomiso2"))
	out := append([]byte{}, ftyp...)
	out = append(out, mp4Box("moov", mp4Box("mvhd", mvhd))...)
	out = append(out, mp4Box("mdat", make([]byte, 32))...)
	return out
}

// startVideoServer 起一个支持 Range 的静态文件服务，返回 URL 与命中计数器。
func startVideoServer(t *testing.T, files map[string][]byte) (string, *int64) {
	t.Helper()
	var hits int64
	mux := http.NewServeMux()
	for path, data := range files {
		path, data := path, data
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&hits, 1)
			total := int64(len(data))
			rng := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
			start, end := int64(0), total-1
			if strings.HasPrefix(rng, "-") {
				n, _ := strconv.ParseInt(strings.TrimPrefix(rng, "-"), 10, 64)
				if start = total - n; start < 0 {
					start = 0
				}
			} else if rng != "" {
				parts := strings.SplitN(rng, "-", 2)
				if v, err := strconv.ParseInt(parts[0], 10, 64); err == nil {
					start = v
				}
				if len(parts) == 2 && parts[1] != "" {
					if v, err := strconv.ParseInt(parts[1], 10, 64); err == nil && v < end {
						end = v
					}
				}
			}
			if start >= total {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end, 10)+"/"+strconv.FormatInt(total, 10))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(data[start : end+1])
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL, &hits
}

// refVideoSnap 构造「参考视频按秒计费」的价目快照。
func refVideoSnap(legacyRefVideo bool) billing.Snapshot {
	sp := model.SpecPrices{
		Kind: "per_video",
		SecondPrice: map[string]int64{
			"480p": 9600, "720p": 12000, "1080p": 18000,
		},
		RefVideoSecondPrice: map[string]int64{"720p": 12000},
	}
	if legacyRefVideo {
		// 「按条」价与「按秒」价互斥：这里模拟"只配了按条价"的存量模型。
		sp.RefVideoSecondPrice = nil
		sp.RefVideoPrice = i64ptr(40000)
	}
	return billing.Snapshot{ModelID: "m", BillingMode: model.BillingModeSpec, SpecPrice: sp}
}

// TestProbeRefVideosSkipsWhenNotBySecond 是「按价目短路」的锁定用例：
// 没有按秒价（含只有旧按条价）时一次也不能出站，否则会给存量模型平白加延迟。
func TestProbeRefVideosSkipsWhenNotBySecond(t *testing.T) {
	t.Setenv("LD_UPSTREAM_ALLOW_PRIVATE", "1")
	url, hits := startVideoServer(t, map[string][]byte{"/v.mp4": mp4WithSeconds(4)})

	base := model.SpecPrices{Kind: "per_video", SecondPrice: map[string]int64{"720p": 12000}}
	byCount := base
	byCount.RefVideoPrice = i64ptr(40000)
	zeroSecond := base
	zeroSecond.RefVideoSecondPrice = map[string]int64{"720p": 0}
	cases := map[string]billing.Snapshot{
		"无任何参考视频价": {BillingMode: model.BillingModeSpec, SpecPrice: base},
		// 只有「按条」价：videoCharge 走 refCount(RefVideos)，秒数不参与计算
		"只有按条价":  {BillingMode: model.BillingModeSpec, SpecPrice: byCount},
		"秒价全为 0": {BillingMode: model.BillingModeSpec, SpecPrice: zeroSecond},
	}
	for name, snap := range cases {
		req := &SubmitRequest{
			Resolution: "720p", Seconds: 5,
			Refs:   []RefSummary{{Kind: "video"}},
			Params: params{Resolution: "720p", Seconds: 5, InputRef: []InputRefSummary{{Type: "video", URL: url + "/v.mp4"}}},
		}
		if err := probeRefVideos(context.Background(), req, snap); err != nil {
			t.Fatalf("%s: 不应报错: %v", name, err)
		}
		if req.RefVideoSeconds != 0 {
			t.Fatalf("%s: 秒数应保持 0，实际 %d", name, req.RefVideoSeconds)
		}
	}
	if n := atomic.LoadInt64(hits); n != 0 {
		t.Fatalf("不该产生任何出站请求，实际 %d 次", n)
	}
}

// TestProbeRefVideosSumsAndDedups：网络请求按 URL 去重，**计费秒数按条目累加**。
//
// 两个口径必须分开：去重只省带宽；计费按条数是与旧「按条」口径一致的——
// 用户把同一段 10 秒视频写 3 次，上游就真的收到 3 份素材，只收 1 份会成本倒挂。
func TestProbeRefVideosSumsAndDedups(t *testing.T) {
	t.Setenv("LD_UPSTREAM_ALLOW_PRIVATE", "1")
	url, hits := startVideoServer(t, map[string][]byte{
		"/a.mp4": mp4WithSeconds(4),
		"/b.mp4": mp4WithSeconds(6),
	})
	req := &SubmitRequest{
		Resolution: "720p", Seconds: 5,
		Refs: []RefSummary{{Kind: "video"}, {Kind: "video"}, {Kind: "video"}, {Kind: "image"}},
		Params: params{
			Resolution: "720p", Seconds: 5,
			InputRef: []InputRefSummary{
				{Type: "video", URL: url + "/a.mp4"},
				{Type: "video", URL: url + "/b.mp4"},
				{Type: "video", URL: url + "/a.mp4"}, // 同一 URL 出现两次：只探一次，但计两次
				{Type: "image", URL: url + "/a.mp4"},
			},
		},
	}
	if err := probeRefVideos(context.Background(), req, refVideoSnap(false)); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if req.RefVideoSeconds != 14 {
		t.Fatalf("want 14s（4+6+4，按条目计费），got %d", req.RefVideoSeconds)
	}
	if req.Params.RefVideoSeconds != 14 {
		t.Fatalf("落库快照必须同步：want 14，got %d", req.Params.RefVideoSeconds)
	}
	if n := atomic.LoadInt64(hits); n != 2 {
		t.Fatalf("去重后只应探测 2 个 URL，实际 %d 次请求", n)
	}
}

// TestProbeRefVideosSaturates：秒数累计后饱和到 MaxVideoSeconds（与计费侧 clamp 一致）。
func TestProbeRefVideosSaturates(t *testing.T) {
	t.Setenv("LD_UPSTREAM_ALLOW_PRIVATE", "1")
	url, _ := startVideoServer(t, map[string][]byte{
		"/a.mp4": mp4WithSeconds(3),
		"/b.mp4": mp4WithSeconds(4),
	})
	req := &SubmitRequest{
		Resolution: "720p", Seconds: 5,
		Refs: []RefSummary{{Kind: "video"}, {Kind: "video"}},
		Params: params{
			Resolution: "720p", Seconds: 5,
			InputRef: []InputRefSummary{
				{Type: "video", URL: url + "/a.mp4"},
				{Type: "video", URL: url + "/b.mp4"},
			},
		},
	}
	if err := probeRefVideos(context.Background(), req, refVideoSnap(false)); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if req.RefVideoSeconds != 7 {
		t.Fatalf("want 7, got %d", req.RefVideoSeconds)
	}
}

// TestProbeRefVideosRejectsInlineVideo：内联 data: 视频读不到时长，必须 400 且不静默按 0。
func TestProbeRefVideosRejectsInlineVideo(t *testing.T) {
	req := &SubmitRequest{
		Resolution: "720p", Seconds: 5,
		Refs:           []RefSummary{{Kind: "video"}}, // 计费口径里有 1 个视频
		hasInlineVideo: true,                          // 由 collectRefs 判定（内联体不落 URL）
		Params: params{
			Resolution: "720p", Seconds: 5,
			InputRef:  nil,
			HasInline: true,
		},
	}
	err := probeRefVideos(context.Background(), req, refVideoSnap(false))
	if !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("want ErrInvalidParam, got %v", err)
	}
	if !strings.Contains(err.Error(), "/v1/files") {
		t.Fatalf("文案应引导用户上传：%v", err)
	}
}

// TestProbeRefVideosRejectsAssetVideo：素材库 ID（asset://）读不到时长，按秒计费时必须拒。
//
// 不能"按 0 秒放行"：那是把参考视频费免掉；也不能当普通非法链接报错——用户会去怀疑
// 链接写错了，而真正要做的是改用直链。
func TestProbeRefVideosRejectsAssetVideo(t *testing.T) {
	req := &SubmitRequest{
		Resolution: "720p", Seconds: 5,
		Refs: []RefSummary{{Kind: "video"}},
		Params: params{
			Resolution: "720p", Seconds: 5,
			InputRef: []InputRefSummary{{Type: "video", URL: "asset://vid-abc"}},
		},
	}
	err := probeRefVideos(context.Background(), req, refVideoSnap(false))
	if !errors.Is(err, ErrInvalidParam) || !strings.Contains(err.Error(), "asset://") {
		t.Fatalf("want ErrInvalidParam(asset://)，got %v", err)
	}
	if req.RefVideoSeconds != 0 {
		t.Fatalf("被拒时不该写秒数，实际 %d", req.RefVideoSeconds)
	}
}

// TestProbeRefVideosFailureIsInvalidParam：素材不可访问 → 400（绝不按 0 计费）。
func TestProbeRefVideosFailureIsInvalidParam(t *testing.T) {
	t.Setenv("LD_UPSTREAM_ALLOW_PRIVATE", "1")
	url, _ := startVideoServer(t, map[string][]byte{"/a.mp4": mp4WithSeconds(3)})
	req := &SubmitRequest{
		Resolution: "720p", Seconds: 5,
		Refs: []RefSummary{{Kind: "video"}},
		Params: params{
			Resolution: "720p", Seconds: 5,
			InputRef: []InputRefSummary{{Type: "video", URL: url + "/missing.mp4"}},
		},
	}
	err := probeRefVideos(context.Background(), req, refVideoSnap(false))
	if !errors.Is(err, ErrInvalidParam) || !strings.Contains(err.Error(), "参考视频时长无法读取") {
		t.Fatalf("want ErrInvalidParam/时长无法读取, got %v", err)
	}
	if req.RefVideoSeconds != 0 {
		t.Fatalf("失败时不得回填任何秒数，实际 %d", req.RefVideoSeconds)
	}
}

// TestEnsureRetryRefVideoSecondsFillsLegacyTask：升级前创建的任务缺秒数，重试时必须补上并写回快照。
func TestEnsureRetryRefVideoSecondsFillsLegacyTask(t *testing.T) {
	t.Setenv("LD_UPSTREAM_ALLOW_PRIVATE", "1")
	url, _ := startVideoServer(t, map[string][]byte{"/a.mp4": mp4WithSeconds(9)})

	snap := refVideoSnap(false)
	row := &model.Task{
		Model:           "metaso-h3",
		PricingSnapshot: mustJSON(snap),
		RequestParams: mustJSON(params{
			Model: "metaso-h3", Resolution: "720p", Seconds: 5,
			References: []RefSummary{{Kind: "video"}},
			InputRef:   []InputRefSummary{{Type: "video", URL: url + "/a.mp4"}},
		}),
	}
	if err := ensureRetryRefVideoSeconds(context.Background(), row); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got := parseParams(row.RequestParams).RefVideoSeconds; got != 9 {
		t.Fatalf("want 9, got %d", got)
	}
}

// TestEnsureRetryRefVideoSecondsNoop 覆盖三种「无需补探」的情形。
func TestEnsureRetryRefVideoSecondsNoop(t *testing.T) {
	t.Setenv("LD_UPSTREAM_ALLOW_PRIVATE", "1")
	url, hits := startVideoServer(t, map[string][]byte{"/a.mp4": mp4WithSeconds(9)})

	cases := []struct {
		name   string
		params params
		spec   string
	}{
		{"已有秒数", params{Resolution: "720p", InputRef: []InputRefSummary{{Type: "video", URL: url + "/a.mp4"}}, RefVideoSeconds: 9}, ""},
		{"再生成任务", params{Kind: KindRegeneration, Resolution: "2k", InputRef: []InputRefSummary{{Type: "video", URL: url + "/a.mp4"}}}, ""},
		{"无参考视频", params{Resolution: "720p", InputRef: []InputRefSummary{{Type: "image", URL: url + "/a.mp4"}}}, ""},
		{"价目未按秒计", params{Resolution: "720p", InputRef: []InputRefSummary{{Type: "video", URL: url + "/a.mp4"}}}, "legacy"},
	}
	for _, c := range cases {
		spec := refVideoSnap(c.spec == "legacy")
		row := &model.Task{PricingSnapshot: mustJSON(spec), RequestParams: mustJSON(c.params)}
		if err := ensureRetryRefVideoSeconds(context.Background(), row); err != nil {
			t.Fatalf("%s: 不应报错: %v", c.name, err)
		}
	}
	if n := atomic.LoadInt64(hits); n != 0 {
		t.Fatalf("以上情形都不该出站，实际 %d 次", n)
	}
}

// TestEnsureRetryRefVideoSecondsNotRetryable：探测失败必须是「不可重试」，
// 不能报「参数不合法」——用户这次根本没有传参。
func TestEnsureRetryRefVideoSecondsNotRetryable(t *testing.T) {
	t.Setenv("LD_UPSTREAM_ALLOW_PRIVATE", "1")
	url, _ := startVideoServer(t, map[string][]byte{"/a.mp4": mp4WithSeconds(9)})
	row := &model.Task{
		PricingSnapshot: mustJSON(refVideoSnap(false)),
		RequestParams: mustJSON(params{
			Resolution: "720p",
			References: []RefSummary{{Kind: "video"}},
			InputRef:   []InputRefSummary{{Type: "video", URL: url + "/gone.mp4"}},
		}),
	}
	err := ensureRetryRefVideoSeconds(context.Background(), row)
	if !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("want ErrNotRetryable, got %v", err)
	}
}

func i64ptr(v int64) *int64 { return &v }
