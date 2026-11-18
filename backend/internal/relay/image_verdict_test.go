package relay

// 同步图片响应语义判定的用例（image_verdict.go）。
//
// 这一层的存在理由：上游对「整单没出图」的表示是 200 + 顶层 error，
// 而计费兜底是「0 张也按 1 张收」。两者叠加会让「上游明确失败」变成
// 「用户没拿到图、却被收 1 张」。判定错了两个方向都是钱：
//   - 把失败判成成功 → 错收用户；
//   - 把「我们解析漏了」判成失败 → 白送一次上游出图成本。

import (
	"strings"
	"testing"

	"lingdong-api/internal/model"
	"lingdong-api/internal/service/billing"
)

// hasFlag 按逗号切分精确比对：不能用 Contains —— image_n_unknown 与
// image_n_over_delivered 共享前缀，子串匹配会把断言变成永真。
func hasFlag(flags, want string) bool {
	for _, f := range strings.Split(flags, ",") {
		if f == want {
			return true
		}
	}
	return false
}

func TestJudgeImageResponse(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		verdict  imageVerdict
		count    int64
		partial  bool
		perItem  bool
		codeWant string
	}{
		{
			name:    "正常一张（url）",
			body:    `{"created":1,"model":"m","data":[{"url":"https://x/1.png","size":"2048x2048"}],"usage":{"generated_images":1}}`,
			verdict: imageOK, count: 1,
		},
		{
			name:    "正常一张（b64）",
			body:    `{"data":[{"b64_json":"iVBORw0KGgo="}]}`,
			verdict: imageOK, count: 1,
		},
		{
			name:    "组图三张",
			body:    `{"data":[{"url":"a"},{"url":"b"},{"url":"c"}]}`,
			verdict: imageOK, count: 3,
		},
		{
			name:    "审核拦截：200 + 顶层 error",
			body:    `{"error":{"code":"OutputImageSensitiveContentDetected","message":"内容违规"}}`,
			verdict: imageUpstreamRejected,
		},
		{
			name:    "上游内部错：200 + 顶层 error",
			body:    `{"data":[],"error":{"code":"InternalServiceError","message":"boom"}}`,
			verdict: imageUpstreamFailed, codeWant: "InternalServiceError",
		},
		{
			// 文档定义顶层 error = 整单没出图；出现即上游违约，但交付必须照常、金额按实际。
			name:    "已交付但也有顶层 error",
			body:    `{"data":[{"url":"a"}],"error":{"code":"PartialFailed","message":"1 of 2 failed"}}`,
			verdict: imageOK, count: 1, partial: true,
		},
		{
			// 关键：解析不出来是我们的问题，绝不能判上游失败（那是白送一次生成）。
			name:    "空 data 且无 error",
			body:    `{"data":[]}`,
			verdict: imageNoImage,
		},
		{
			name:    "data 缺失",
			body:    `{"created":1}`,
			verdict: imageNoImage,
		},
		{
			name:    "组图逐项全失败",
			body:    `{"data":[{"error":{"code":"OutputImageSensitiveContentDetected","message":"x"}}]}`,
			verdict: imageNoImage, perItem: true,
		},
		{
			name:    "非法 JSON 不得判成上游失败",
			body:    `<html>502 Bad Gateway</html>`,
			verdict: imageNoImage,
		},
		{
			name:    "空体",
			body:    ``,
			verdict: imageNoImage,
		},
		{
			name:    "URL 为空串不算交付",
			body:    `{"data":[{"url":"","b64_json":""}]}`,
			verdict: imageNoImage,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := judgeImageResponse([]byte(c.body))
			if out.Verdict != c.verdict {
				t.Fatalf("Verdict = %v，期望 %v（body=%s）", out.Verdict, c.verdict, c.body)
			}
			if out.Count != c.count {
				t.Fatalf("Count = %d，期望 %d", out.Count, c.count)
			}
			if out.PartialError != c.partial {
				t.Fatalf("PartialError = %v，期望 %v", out.PartialError, c.partial)
			}
			if out.PerImageFailed != c.perItem {
				t.Fatalf("PerImageFailed = %v，期望 %v", out.PerImageFailed, c.perItem)
			}
			if c.codeWant != "" && out.Code != c.codeWant {
				t.Fatalf("Code = %q，期望 %q", out.Code, c.codeWant)
			}
			// 对外文案绝不能带上游原文（可能含内部 code 与参考图 URL）。
			if msg := out.UserMessage(); msg == "" {
				t.Fatal("UserMessage 不能为空")
			}
		})
	}
}

// TestImageFailedVerdictIsNotCharged 守「明确失败 → 0 张」这条资金线。
//
// 走的是真实的 buildBillingUsage：它以前对「0 张 + 没写 n」的请求**连标记都不打**，
// 用户什么都没拿到、被收 1 张，调用记录却看起来完全正常。
func TestImageFailedVerdictIsNotCharged(t *testing.T) {
	// 0 张 + 无 error：维持 1 张兜底，但必须留痕（image_n_unknown）。
	u, _, flags := buildBillingUsage(kindImagesGenerations, UsageInfo{}, promptCompletionEst{},
		model.BillingModeSpec, billing.Usage{}, []byte(`{"data":[]}`), 0, false, false, 0)
	if u.ImageN != 1 {
		t.Fatalf("0 张无 error 仍按 1 张兜底（避免整单白送），实际 %d", u.ImageN)
	}
	if !hasFlag(flags, model.AnomalyImageNUnknown) {
		t.Fatalf("0 张且没写 n 必须打 image_n_unknown，实际标记 %q", flags)
	}

	// 组图逐项全失败：原因是明确的，用另一个标记。
	_, _, flags = buildBillingUsage(kindImagesGenerations, UsageInfo{}, promptCompletionEst{},
		model.BillingModeSpec, billing.Usage{},
		[]byte(`{"data":[{"error":{"code":"OutputImageSensitiveContentDetected","message":"x"}}]}`),
		0, false, false, 0)
	if !hasFlag(flags, model.AnomalyImagePartialAllFailed) {
		t.Fatalf("组图全失败必须打 image_partial_all_failed，实际 %q", flags)
	}

	// 已交付 + 顶层 error：金额不变，但必须留痕。
	u, _, flags = buildBillingUsage(kindImagesGenerations, UsageInfo{}, promptCompletionEst{},
		model.BillingModeSpec, billing.Usage{ImageN: 1},
		[]byte(`{"data":[{"url":"a"},{"url":"b"}],"error":{"code":"X","message":"y"}}`),
		0, false, false, 0)
	if u.ImageN != 2 {
		t.Fatalf("已交付 2 张必须按 2 张计，实际 %d", u.ImageN)
	}
	if !hasFlag(flags, model.AnomalyImagePartialError) {
		t.Fatalf("部分交付同时带 error 必须打 image_partial_error，实际 %q", flags)
	}
}

// TestIsModerationRejectionSharedByTaskAndImage 守「词表只有一份」。
func TestIsModerationRejectionSharedByTaskAndImage(t *testing.T) {
	for _, s := range []string{"内容违规", "Blocked by policy", "NSFW detected", "审核未通过"} {
		if !IsModerationRejection(s) {
			t.Fatalf("%q 应判为审核拒绝", s)
		}
	}
	for _, s := range []string{"", "internal error", "rate limit exceeded"} {
		if IsModerationRejection(s) {
			t.Fatalf("%q 不应判为审核拒绝", s)
		}
	}
	// 任务链路的错误类型必须与之一致（同一个上游动作不能让两条链路给出不同归因）。
	if _, ok := TaskFailure("内容违规", "t1").(*CallerParamError); !ok {
		t.Fatal("TaskFailure 的审核拒绝必须是 CallerParamError（400、不记渠道健康）")
	}
}
