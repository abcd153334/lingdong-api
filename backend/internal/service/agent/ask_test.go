package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

// 本文件只考"提问"的判断题与 CAS，不考真实等待：
// 等 60 秒的用例会让整个包从秒级变成分钟级，而等待循环的正确性
// （唤醒/轮询/心跳/断线/到期）靠 askAndWait 的结构保证，靠一个 60s 用例证明不了。

func askTestEnv() *toolEnv {
	return &toolEnv{
		Ctx: context.Background(), SessionID: 1, MessageID: 2, RunID: 3,
		Step: 1, MaxSteps: 12, AskReady: true,
	}
}

// 四种"不能问"必须一律 ok:true。
//
// ok:false 会让 FailStreak 累加，三次触发 agent_loop_guard 整轮中止 ——
// 把"人不在电脑前"变成"这一轮报错了"，这是最容易被写错的一处。
func TestAskGatesAlwaysOK(t *testing.T) {
	cases := []struct {
		name  string
		env   *toolEnv
		want  string
		setup func(e *toolEnv)
	}{
		{name: "未就绪", env: nil, want: AskUnavailable},
		{name: "已问满", env: askTestEnv(), want: AskDenied,
			setup: func(e *toolEnv) { e.AskCount = askMaxPerRun }},
		{name: "步数用尽", env: askTestEnv(), want: AskNoTime,
			setup: func(e *toolEnv) { e.Step = e.MaxSteps }},
		{name: "时间不够", env: askTestEnv(), want: AskNoTime,
			setup: func(e *toolEnv) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				e.Ctx = ctx
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := c.env
			if env != nil && c.setup != nil {
				c.setup(env)
			}
			args := json.RawMessage(`{"question":"要不要","why":"不问会做错","options":[{"value":"a","label":"A"},{"value":"b","label":"B"}]}`)
			out, err := toolAskUser(env, args)
			if err != nil {
				t.Fatalf("工具不应返回传输错误: %v", err)
			}
			m, ok := out.(map[string]any)
			if !ok {
				t.Fatalf("结果必须是对象，得到 %T", out)
			}
			if m["ok"] != true {
				t.Fatalf("闸门拒绝也必须 ok:true，得到 %#v", m)
			}
			if m["status"] != c.want {
				t.Fatalf("status=%v，期望 %s", m["status"], c.want)
			}
			if m["note"] == nil || m["note"] == "" {
				t.Fatalf("拒绝时必须给模型一条「该怎么办」的 note：%#v", m)
			}
		})
	}
}

func TestAskOptionsValidation(t *testing.T) {
	env := askTestEnv()
	bad := []string{
		`{"question":"q","why":"w","options":[{"value":"a","label":"A"}]}`,
		`{"question":"q","why":"w","options":[{"value":"a","label":"A"},{"value":"a","label":"B"}]}`,
		`{"question":"q","why":"w","options":[{"value":"a","label":"A"},{"value":"","label":"B"}]}`,
		`{"question":"","why":"w","options":[{"value":"a","label":"A"},{"value":"b","label":"B"}]}`,
	}
	for _, raw := range bad {
		out, err := toolAskUser(env, json.RawMessage(raw))
		if err != nil {
			t.Fatalf("不应返回传输错误: %v", err)
		}
		m := out.(map[string]any)
		if m["ok"] != false || m["code"] != "invalid_arguments" {
			t.Fatalf("非法参数应返回 invalid_arguments：%s → %#v", raw, m)
		}
	}
}

func TestAskTraceSegmentSettled(t *testing.T) {
	env := askTestEnv()
	idx := env.appendAsk(traceItem{
		K: traceAsk, Q: "用哪个档位", W: "决定价格", ST: AskPending,
		O: []AskOption{{Value: "a", Label: "A"}},
	})
	env.resolveAsk(idx, AskAnswered, "a")
	if env.Trace[idx].ST != AskAnswered || env.Trace[idx].ANS != "a" {
		t.Fatalf("结算未回写：%#v", env.Trace[idx])
	}
	// 越界必须是空操作：轨迹在裁剪后下标会失效，不能让结算 panic 打断整轮。
	env.resolveAsk(99, AskExpired, "")
	env.resolveAsk(-1, AskExpired, "")
}

/* ------------------------------ 需要 DB ------------------------------ */

func seedRunningAsk(t *testing.T, gdb *gorm.DB) *model.AgentAsk {
	t.Helper()
	run := &model.AgentRun{SessionID: 1, MessageID: 2, ModelID: "fake-chat", Status: "running"}
	if err := gdb.Create(run).Error; err != nil {
		t.Fatalf("建 run 失败: %v", err)
	}
	opts, _ := json.Marshal([]AskOption{{Value: "post", Label: "按次"}, {Value: "spec", Label: "按规格"}})
	row := &model.AgentAsk{
		SessionID: 1, MessageID: 2, RunID: run.ID, Step: 1,
		Question: "这个模型按次还是按规格计费？", Why: "决定 quota_type 与 spec_prices",
		Options: model.JSON(opts), Multi: false, AllowFree: true,
		Status: AskPending, ExpiresAt: time.Now().Add(askWaitMax),
	}
	if err := gdb.Create(row).Error; err != nil {
		t.Fatalf("建 ask 失败: %v", err)
	}
	return row
}

func TestAnswerAskCAS(t *testing.T) {
	gdb := withAgentDB(t)
	db.DB = gdb
	row := seedRunningAsk(t, gdb)

	got, err := AnswerAsk(context.Background(), row.ID, 7, AskAnswer{Selected: []string{"post"}})
	if err != nil {
		t.Fatalf("首次作答应成功: %v", err)
	}
	if got.Status != AskAnswered || got.AnsweredBy != 7 {
		t.Fatalf("落库状态不对：%#v", got)
	}
	// 双击 / 两个管理员同时提交：第二次必须被 CAS 拦下，且报"已回答"而不是"已结束"。
	if _, err := AnswerAsk(context.Background(), row.ID, 8, AskAnswer{Selected: []string{"spec"}}); !errors.Is(err, ErrAskAnswered) {
		t.Fatalf("重复作答应报 ErrAskAnswered，得到 %v", err)
	}
	// 行必须还是第一个人答的那份，不能被后来者覆盖。
	again, _ := loadAskRow(context.Background(), row.ID)
	var body struct{ Selected []string }
	_ = json.Unmarshal(again.Answer, &body)
	if len(body.Selected) != 1 || body.Selected[0] != "post" {
		t.Fatalf("答案被覆盖了：%#v", body.Selected)
	}
}

func TestAnswerAskRejectsOutsideValues(t *testing.T) {
	gdb := withAgentDB(t)
	db.DB = gdb
	row := seedRunningAsk(t, gdb)

	// 非法在先、合法在后：合法那条会把行答掉，之后再提交一律是"已回答"，
	// 混在一起测就分不清"值域校验生效"还是"被 CAS 拦下"。
	bad := []AskAnswer{
		{Selected: []string{"nope"}},                // 不在选项里
		{Selected: []string{"post", "spec"}},        // 单选给了两个
		{Skipped: true, Selected: []string{"post"}}, // 跳过还带选项
		{FreeText: "   "},                           // 什么都没给（空白不算）
	}
	for i, c := range bad {
		if _, err := AnswerAsk(context.Background(), row.ID, 7, c); !errors.Is(err, ErrAskInvalid) {
			t.Fatalf("第 %d 种应报 ErrAskInvalid，得到 %v", i, err)
		}
	}
	// 值域校验拒绝时不能顺手把行结掉：否则一次手滑就再也答不了。
	if cur, _ := loadAskRow(context.Background(), row.ID); cur.Status != AskPending {
		t.Fatalf("非法提交后行仍应是 pending，实际 %s", cur.Status)
	}
	if _, err := AnswerAsk(context.Background(), row.ID, 7, AskAnswer{Selected: []string{"post"}}); err != nil {
		t.Fatalf("合法提交应成功：%v", err)
	}
}

func TestAnswerAskExpiredWhenRunEnded(t *testing.T) {
	gdb := withAgentDB(t)
	db.DB = gdb
	row := seedRunningAsk(t, gdb)

	if err := gdb.Model(&model.AgentRun{}).Where("id = ?", row.RunID).
		Update("status", "ok").Error; err != nil {
		t.Fatalf("改 run 状态失败: %v", err)
	}
	// run 已结束 = 没人在等这个答案。必须报"已结束"并顺手把行结掉，
	// 否则卡片会一直显示"待回答"，点一次报一次 500。
	if _, err := AnswerAsk(context.Background(), row.ID, 7, AskAnswer{Selected: []string{"post"}}); !errors.Is(err, ErrAskExpired) {
		t.Fatalf("应报 ErrAskExpired，得到 %v", err)
	}
	again, _ := loadAskRow(context.Background(), row.ID)
	if again.Status != AskExpired {
		t.Fatalf("行应被结算为 expired，实际 %s", again.Status)
	}
}

func TestPendingAskAndExpireOnNewRun(t *testing.T) {
	gdb := withAgentDB(t)
	db.DB = gdb
	row := seedRunningAsk(t, gdb)

	cur, err := PendingAsk(context.Background(), 1)
	if err != nil || cur == nil || cur.ID != row.ID {
		t.Fatalf("应能读到待答行：%v %#v", err, cur)
	}
	// 新一轮开场必须把旧的 pending 结掉：一个会话任何时刻最多一张可答卡片。
	expirePendingAsks(context.Background(), 1)
	if cur, err := PendingAsk(context.Background(), 1); err != nil || cur != nil {
		t.Fatalf("旧 pending 应被结算：%v %#v", err, cur)
	}
}
