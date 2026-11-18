package adapters

// 本文件守的是 `arkPumpSSE` 的四条规则（每条都对应一个真实失败模式，
// 注释里已写明；这里只保证"改坏了会被测出来"）：
//  1. `\n` 与 `\r\n` 都接受；
//  2. 累积到空行才派发（多行 data 不得拆帧）；
//  3. `[DONE]` 转发给客户端但立即终止；
//  4. 单帧超 8 MiB 必须报 `ErrUpstreamBodyTooLarge`（而不是把内存打爆）。
//
// 另外守三个"容易被顺手改掉"的语义：
//  - `b64_json` 帧也要算「拿到图」（否则 b64 流被判零交付 → 全额退费）；
//  - 注释行（`: keep-alive`）不产生空帧；
//  - onFrame 返回错误 = 客户端断开，必须立刻中止（不能继续读完上游）。

import (
	"errors"
	"strings"
	"testing"
	"time"

	"lingdong-api/internal/relay"
)

// runArkPump 用一段固定输入驱动 pump，收集所有回调帧。
func runArkPump(t *testing.T, input string, onFrame func(relay.ImageFrame) error) (*relay.ImageStreamResult, error) {
	t.Helper()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	if onFrame == nil {
		onFrame = func(relay.ImageFrame) error { return nil }
	}
	return arkPumpSSE(strings.NewReader(input), timer, time.Hour, onFrame)
}

// ① 多行 data：一条 JSON 被拆成两个 data 行，必须拼成**一帧**。
//
// 按行派发的实现会在这里给出 2 帧、且两帧都不是合法 JSON ——
// 客户端拿到半条 JSON，产物直接损坏。
func TestArkPumpSSEMultiLineDataAccumulates(t *testing.T) {
	input := "event: image_generation.partial_succeeded\n" +
		`data: {"type":"image_generation.partial_succeeded",` + "\n" +
		`data: "url":"https://cdn.example.com/a.png"}` + "\n" +
		"\n"

	var frames []relay.ImageFrame
	res, err := runArkPump(t, input, func(f relay.ImageFrame) error {
		frames = append(frames, f)
		return nil
	})
	if err != nil {
		t.Fatalf("多行 data 不该报错: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("多行 data 必须拼成一帧，实际 %d 帧", len(frames))
	}
	if got := string(frames[0].Data); got != `{"type":"image_generation.partial_succeeded",`+"\n"+`"url":"https://cdn.example.com/a.png"}` {
		t.Fatalf("拼接后的帧内容不对：%q", got)
	}
	if frames[0].URL != "https://cdn.example.com/a.png" {
		t.Fatalf("拼接后应能解析出 url，实际 %q", frames[0].URL)
	}
	if res.Frames != 1 {
		t.Fatalf("Frames 应为 1，实际 %d", res.Frames)
	}
}

// ② `\r\n` 换行：上游改一次换行符就会让帧尾多出 `\r`，json.Unmarshal 当场失败。
func TestArkPumpSSEAcceptsCRLF(t *testing.T) {
	input := `data: {"type":"image_generation.partial_succeeded","url":"https://cdn.example.com/a.png"}` + "\r\n\r\n"

	var frames []relay.ImageFrame
	_, err := runArkPump(t, input, func(f relay.ImageFrame) error {
		frames = append(frames, f)
		return nil
	})
	if err != nil {
		t.Fatalf("CRLF 换行不该报错: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("应收到 1 帧，实际 %d", len(frames))
	}
	if strings.Contains(string(frames[0].Data), "\r") {
		t.Fatalf("帧内容不得残留 \\r：%q", frames[0].Data)
	}
	if frames[0].URL == "" {
		t.Fatal("CRLF 帧应能解析出 url")
	}
}

// ③ `[DONE]`：**转发给客户端**（P3-1）但立即终止，不再读后面的数据。
// 转发的原因：多数 SDK 以 [DONE] 判「流正常结束」，吞掉会让客户端一直等到自己的空闲超时。
// 但 res.Frames 只统计内容帧 —— 零帧判定（「200 但没出图」）依赖这个计数。
func TestArkPumpSSEStopsAtDone(t *testing.T) {
	input := `data: {"type":"image_generation.partial_succeeded","url":"https://cdn.example.com/a.png"}` + "\n\n" +
		"data: [DONE]\n\n" +
		`data: {"type":"image_generation.partial_succeeded","url":"https://cdn.example.com/b.png"}` + "\n\n"

	var got []relay.ImageFrame
	res, err := runArkPump(t, input, func(f relay.ImageFrame) error {
		got = append(got, f)
		return nil
	})
	if err != nil {
		t.Fatalf("[DONE] 不该报错: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应收到「1 个内容帧 + 1 个 [DONE]」，实际 %d 帧", len(got))
	}
	if strings.TrimSpace(string(got[1].Data)) != "[DONE]" {
		t.Fatalf("第二帧应是 [DONE]，实际 %q", got[1].Data)
	}
	if res.Frames != 1 {
		t.Fatalf("Frames 应只统计 [DONE] 之前的内容帧，实际 %d", res.Frames)
	}
}

// ④ 单帧超限：报 ErrUpstreamBodyTooLarge，且**不**把内存里的半帧当成功。
func TestArkPumpSSEFrameTooLarge(t *testing.T) {
	huge := strings.Repeat("a", arkMaxStreamFrameBytes+2048)
	input := "data: " + huge + "\n\n"

	var frames int
	_, err := runArkPump(t, input, func(relay.ImageFrame) error {
		frames++
		return nil
	})
	if !errors.Is(err, relay.ErrUpstreamBodyTooLarge) {
		t.Fatalf("超限必须返回 ErrUpstreamBodyTooLarge，实际 %v", err)
	}
	if frames != 0 {
		t.Fatalf("超限帧不得派发给客户端，实际派发 %d 帧", frames)
	}
}

// ⑤ EOF 时上游没补空行：最后一行与最后一帧仍要派发，否则白丢一张图。
func TestArkPumpSSEDispatchesLastFrameOnEOF(t *testing.T) {
	input := `data: {"type":"image_generation.partial_succeeded","url":"https://cdn.example.com/last.png"}`

	var frames []relay.ImageFrame
	_, err := runArkPump(t, input, func(f relay.ImageFrame) error {
		frames = append(frames, f)
		return nil
	})
	if err != nil {
		t.Fatalf("EOF 收尾不该报错: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("EOF 前的最后一帧必须派发，实际 %d 帧", len(frames))
	}
	if frames[0].URL != "https://cdn.example.com/last.png" {
		t.Fatalf("最后一帧解析错：%q", frames[0].URL)
	}
}

// ⑥ completed 帧：终态、usage 张数、原文（落 call_io 的凭据）、model 都要落位。
func TestArkPumpSSEMapsCompletedFrame(t *testing.T) {
	data := `{"type":"image_generation.completed","model":"doubao-seedream-5-0-260128",` +
		`"url":"https://cdn.example.com/a.png","usage":{"generated_images":2}}`
	input := "event: image_generation.completed\n" + "data: " + data + "\n\n"

	var frames []relay.ImageFrame
	res, err := runArkPump(t, input, func(f relay.ImageFrame) error {
		frames = append(frames, f)
		return nil
	})
	if err != nil {
		t.Fatalf("completed 帧不该报错: %v", err)
	}
	if len(frames) != 1 || !frames[0].Terminal {
		t.Fatalf("completed 必须是终态帧，实际 %+v", frames)
	}
	if frames[0].Event != "image_generation.completed" {
		t.Fatalf("event 行优先，实际 %q", frames[0].Event)
	}
	if res.GeneratedImages != 2 {
		t.Fatalf("usage.generated_images 应为 2，实际 %d", res.GeneratedImages)
	}
	if res.Model != "doubao-seedream-5-0-260128" {
		t.Fatalf("model 未落位：%q", res.Model)
	}
	if string(res.UsageRaw) != data {
		t.Fatalf("UsageRaw 必须是 completed 帧的**原文**（改写会丢掉上游原始 usage）：%q", res.UsageRaw)
	}
	if len(res.Images) != 1 || res.Images[0] != "https://cdn.example.com/a.png" {
		t.Fatalf("Images 应记录上游原链，实际 %v", res.Images)
	}
}

// ⑦ b64_json 帧：没有 url 也算「拿到图」，但**不得**塞进 Images（那是 url 清单）。
func TestArkPumpSSEKeepsB64Frames(t *testing.T) {
	input := `data: {"type":"image_generation.partial_succeeded","b64_json":"iVBORw0KGgo="}` + "\n\n"

	var frames []relay.ImageFrame
	res, err := runArkPump(t, input, func(f relay.ImageFrame) error {
		frames = append(frames, f)
		return nil
	})
	if err != nil {
		t.Fatalf("b64 帧不该报错: %v", err)
	}
	if len(frames) != 1 || !frames[0].HasImage {
		t.Fatalf("b64_json 必须算作携带图片（否则整条 b64 流被判零交付）：%+v", frames)
	}
	if frames[0].URL != "" {
		t.Fatalf("b64 帧没有 url，URL 必须为空：%q", frames[0].URL)
	}
	if len(res.Images) != 0 {
		t.Fatalf("Images 只收 url，b64 不该进来：%v", res.Images)
	}
}

// ⑧ 注释/心跳行不产生空帧。
func TestArkPumpSSEIgnoresComments(t *testing.T) {
	input := ": keep-alive\n\n" +
		`data: {"type":"image_generation.partial_succeeded","url":"https://cdn.example.com/a.png"}` + "\n\n" +
		": keep-alive\n\n"

	var frames int
	res, err := runArkPump(t, input, func(relay.ImageFrame) error {
		frames++
		return nil
	})
	if err != nil {
		t.Fatalf("心跳行不该报错: %v", err)
	}
	if frames != 1 || res.Frames != 1 {
		t.Fatalf("心跳行不该产生帧，实际回调 %d 帧 / Frames=%d", frames, res.Frames)
	}
}

// ⑨ onFrame 返回错误 = 客户端已断开：必须立刻中止，并把错误原样上抛。
func TestArkPumpSSEStopsOnClientError(t *testing.T) {
	gone := errors.New("客户端已断开")
	input := `data: {"type":"image_generation.partial_succeeded","url":"https://cdn.example.com/a.png"}` + "\n\n" +
		`data: {"type":"image_generation.partial_succeeded","url":"https://cdn.example.com/b.png"}` + "\n\n"

	var frames int
	res, err := runArkPump(t, input, func(relay.ImageFrame) error {
		frames++
		return gone
	})
	if !errors.Is(err, gone) {
		t.Fatalf("必须把客户端断开的错误原样上抛，实际 %v", err)
	}
	if frames != 1 {
		t.Fatalf("客户端断开后不得继续派发，实际回调 %d 帧", frames)
	}
	if res == nil || res.Frames != 1 {
		t.Fatalf("已收到的帧数仍要保留（用于按已交付计费）：%+v", res)
	}
}
