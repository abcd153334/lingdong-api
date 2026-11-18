package adapters

import "testing"

// 本文件覆盖「HTTP 200 + 业务失败信封」在**查询链路**上的识别。
//
// 缺陷现场（2026-11-04 实测）：`/api/gemini/nano-banana/{id}` 对失败任务回
//
//	{"code":400,"msg":"... does not support 2K or 4K resolutions","data":null}
//
// 旧解析器只读 status/state/data，于是状态空、产物空、错误空 → 上层判「还在跑」，
// 一直轮询到任务超时（1h）才退款：上游早已拒绝，用户白等，渠道健康度全程正常。

// TestDuomiPollTopLevelMsgMarksFailed 根级 code/msg 必须直接判失败并带出原话。
func TestDuomiPollTopLevelMsgMarksFailed(t *testing.T) {
	const body = `{"code":400,"msg":"gemini-3.1-flash-lite-image does not support 2K or 4K resolutions","data":null}`
	st, err := parseDuomiImageTask([]byte(body))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if st.Status != "failed" {
		t.Fatalf("应判 failed，实际 %q（会一直轮询到超时）", st.Status)
	}
	if st.Error != "gemini-3.1-flash-lite-image does not support 2K or 4K resolutions" {
		t.Fatalf("原因应为上游原话，实际 %q", st.Error)
	}
}

// TestDuomiPollSuccessCodeIgnoresMsg 成功码时顶层的 msg 不得变成错误。
//
// 大量上游在成功响应里写 `msg:"success"`；把它当原因会让正常出图被判失败。
func TestDuomiPollSuccessCodeIgnoresMsg(t *testing.T) {
	for _, body := range []string{
		`{"code":0,"msg":"success","data":{"status":"succeeded","images":["https://cdn/a.png"]}}`,
		`{"code":200,"message":"SUCCEED","data":{"status":"running","progress":40}}`,
		`{"code":0,"msg":"success","data":{"status":"running"}}`,
	} {
		st, err := parseDuomiImageTask([]byte(body))
		if err != nil {
			t.Fatalf("解析失败：%v", err)
		}
		if st.Error != "" {
			t.Fatalf("%s：成功响应不应带出错误 %q", body, st.Error)
		}
		if st.Status == "failed" {
			t.Fatalf("%s：成功码不应判 failed", body)
		}
	}
}

// TestDuomiPollArtifactsWinOverErrorCode 有产物时即使带业务码也判成功。
//
// 优先级不能反：上游偶尔把已生成好的产物与一个无关业务码放在一起，
// 判失败会让用户拿不到已经付过钱的图。
func TestDuomiPollArtifactsWinOverErrorCode(t *testing.T) {
	const body = `{"code":1201,"msg":"partial failure","data":{"images":["https://cdn/a.png"]}}`
	st, err := parseDuomiImageTask([]byte(body))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if st.Status != "succeeded" {
		t.Fatalf("有产物应判 succeeded，实际 %q", st.Status)
	}
	if len(st.ResultURLs) != 1 {
		t.Fatalf("应收集 1 个产物，实际 %v", st.ResultURLs)
	}
}

// TestDuomiMusicPollTopLevelMsgMarksFailed 音乐链路同样不能把根级失败当「还在跑」。
func TestDuomiMusicPollTopLevelMsgMarksFailed(t *testing.T) {
	const body = `{"code":400,"msg":"insufficient credits","data":null}`
	st, err := parseDuomiMusicTask([]byte(body))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if st.Status != "failed" || st.Error != "insufficient credits" {
		t.Fatalf("应 failed + 原话，实际 %q / %q", st.Status, st.Error)
	}
}

// TestDuomiKlingPollReadsMsgField 可灵各版本把原因写在 message / msg 两处。
func TestDuomiKlingPollReadsMsgField(t *testing.T) {
	const body = `{"code":1201,"msg":"prompt violates policy","data":{}}`
	st, err := parseDuomiKlingTask([]byte(body))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if st.Status != "failed" {
		t.Fatalf("应判 failed，实际 %q", st.Status)
	}
	if st.Error != "prompt violates policy" {
		t.Fatalf("原因应取 msg，实际 %q", st.Error)
	}
}

// TestDuomiSeedancePollReadsTopLevelMsg seedance 的顶级 msg 也必须能取到。
func TestDuomiSeedancePollReadsTopLevelMsg(t *testing.T) {
	const body = `{"code":400,"msg":"invalid duration","status":"failed","data":{}}`
	st, err := parseDuomiSeedanceTask([]byte(body))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if st.Status != "failed" || st.Error != "invalid duration" {
		t.Fatalf("应 failed + 原话，实际 %q / %q", st.Status, st.Error)
	}
}

// TestDuomiEnvelopeOKStillRecognizesBareID 探活判据不能因为重构而收窄。
//
// 只回 `{"id":"..."}` 的响应同样是 JSON 信封：探活要靠它区分
// 「路径对、任务不存在」与「路径写错了」（两者都可能 404）。
func TestDuomiEnvelopeOKStillRecognizesBareID(t *testing.T) {
	yes := []string{
		`{"id":"abc"}`,
		`{"code":0}`,
		`{"data":{}}`,
		`{"status":"running"}`,
		`{"code":400,"msg":"x","data":null}`,
	}
	for _, b := range yes {
		if !duomiEnvelopeOK([]byte(b)) {
			t.Fatalf("%s 应识别为信封", b)
		}
	}
	for _, b := range []string{`<html>404</html>`, ``, `not json`} {
		if duomiEnvelopeOK([]byte(b)) {
			t.Fatalf("%s 不应识别为信封", b)
		}
	}
}

// TestDuomiEnvelopeFailureCodes 业务码分档：只有非成功码才失败。
func TestDuomiEnvelopeFailureCodes(t *testing.T) {
	cases := []struct {
		body   string
		failed bool
	}{
		{`{"code":0}`, false},
		{`{"code":200}`, false},
		{`{"code":"0"}`, false},
		{`{"code":400,"msg":"x"}`, true},
		{`{"code":"401"}`, true},
		{`{"error":{"message":"bad"}}`, true},
		{`{"error":"bad"}`, true},
		{`{"error":null}`, false},
		{`{"code":0,"msg":"success"}`, false},
	}
	for _, c := range cases {
		_, _, failed := duomiEnvelopeOf([]byte(c.body)).failure()
		if failed != c.failed {
			t.Fatalf("%s：failed=%v，期望 %v", c.body, failed, c.failed)
		}
	}
}

// TestDuomiPollEmptyBodyStaysUnknown 空/非 JSON 响应保持「未知」而不是伪造失败。
//
// 轮询偶尔会拿到网关的临时错误页；那既不是成功也不是业务失败，
// 交给 worker 的连败上限兜底，不能在这里编一个失败原因。
func TestDuomiPollEmptyBodyStaysUnknown(t *testing.T) {
	for _, body := range []string{``, `<html>502</html>`} {
		st, err := parseDuomiImageTask([]byte(body))
		if err != nil {
			t.Fatalf("解析失败：%v", err)
		}
		if st.Status != "" || st.Error != "" {
			t.Fatalf("%q 不应产生状态/原因，实际 %q / %q", body, st.Status, st.Error)
		}
	}
}
