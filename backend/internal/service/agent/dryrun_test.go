package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
)

// 本文件覆盖 dry-run 闸门与出站安全。
//
// 闸门的两个失败模式都要防：
//   - 太松：新建渠道/换上游不探测就上线，配置错误要到第一次真实调用才暴露；
//   - 太紧：早期一次探测失败永久钉死后续提案（补探成功后仍然不能应用），
//     那会把运营逼回"随便找个能过的姿势重开会话"。

const e2eBase = "https://api.a.example"

func mustDecode(t *testing.T, raw string) *Plan {
	t.Helper()
	p, err := DecodePlan([]byte(raw))
	if err != nil {
		t.Fatalf("解析计划失败: %v", err)
	}
	return p
}

// needsOf 是测试侧取"需要探测的目标"的口径：与线上同一条路径
// （referencedChannelIDs → loadChannelRefs → probeNeedsOf），不另写一份推导。
func needsOf(t *testing.T, gdb *gorm.DB, sessionID uint64, plan *Plan) []probeNeed {
	t.Helper()
	chans, err := loadChannelRefs(context.Background(), gdb, referencedChannelIDs(plan))
	if err != nil {
		t.Fatalf("取渠道现状失败: %v", err)
	}
	return probeNeedsOf(sessionID, plan, chans)
}

func TestApplyRequiresDryRunForNewChannel(t *testing.T) {
	gdb := withAgentDB(t)
	setDryRunRequired(t, gdb, true)

	prop := createProposal(t, happyPlan())
	if _, err := Apply(context.Background(), prop.ID, prop.PlanHash, 1); !errors.Is(err, ErrDryRunRequired) {
		t.Fatalf("没有探测记录时应被拦下，实际 %v", err)
	}
	// 被拦下不能把提案打成 failed：补探之后还要能用。
	var after model.AgentProposal
	if err := gdb.First(&after, prop.ID).Error; err != nil {
		t.Fatalf("读提案失败: %v", err)
	}
	if after.Status != model.ProposalDraft {
		t.Fatalf("被闸门拦下后状态应保持 draft，实际 %s", after.Status)
	}

	// 补一条合法凭据 → 放行（密钥必须与提案里的 keys 一致：happyPlan 用 sk-1）
	seedProbe(t, gdb, prop.SessionID, "spec:relay-a", e2eBase, "sk-1", true, time.Now(), time.Time{})
	if _, err := Apply(context.Background(), prop.ID, prop.PlanHash, 1); err != nil {
		t.Fatalf("有成功探测记录时应放行，实际 %v", err)
	}
}

func TestDryRunRecordExpires(t *testing.T) {
	gdb := withAgentDB(t)
	setDryRunRequired(t, gdb, true)
	prop := createProposal(t, happyPlan())

	// 25 小时前的记录：上游可能早就换了地址，不能当证据。
	seedProbe(t, gdb, prop.SessionID, "spec:relay-a", e2eBase, "sk-1", true,
		time.Now().Add(-25*time.Hour), time.Now().Add(-time.Hour))
	if _, err := Apply(context.Background(), prop.ID, prop.PlanHash, 1); !errors.Is(err, ErrDryRunRequired) {
		t.Fatalf("过期记录应被拒绝，实际 %v", err)
	}

	// ExpiresAt 为零值：无法判断新鲜度，一律不放行（fail-closed）。
	seedProbe(t, gdb, prop.SessionID, "spec:relay-a", e2eBase, "sk-1", true,
		time.Now(), time.Unix(1, 0))
	if _, err := Apply(context.Background(), prop.ID, prop.PlanHash, 1); !errors.Is(err, ErrDryRunRequired) {
		t.Fatalf("At 为空的记录应被拒绝，实际 %v", err)
	}
}

func TestDryRunUsesLatestPerBaseURL(t *testing.T) {
	gdb := withAgentDB(t)
	setDryRunRequired(t, gdb, true)
	prop := createProposal(t, happyPlan())

	// 先失败后成功：以**最新**一条为准。会话内记录会跨轮累积，
	// 早期失败若一直算数，补探成功后提案依然不可应用 —— 那是死路不是风控。
	seedProbe(t, gdb, prop.SessionID, "spec:relay-a", e2eBase, "sk-1", false,
		time.Now().Add(-2*time.Minute), time.Time{})
	seedProbe(t, gdb, prop.SessionID, "spec:relay-a", e2eBase, "sk-1", true,
		time.Now(), time.Time{})
	if _, err := Apply(context.Background(), prop.ID, prop.PlanHash, 1); err != nil {
		t.Fatalf("应以最新一次（成功）为准，实际 %v", err)
	}
}

func TestDryRunRejectsAdapterMismatch(t *testing.T) {
	gdb := withAgentDB(t)
	setDryRunRequired(t, gdb, true)
	prop := createProposal(t, happyPlan())

	// 同地址但换了协议：探测记录不能混用（这正是"换适配器必须重探"的场景）。
	seedProbe(t, gdb, prop.SessionID, "openai", e2eBase, "sk-1", true, time.Now(), time.Time{})
	if _, err := Apply(context.Background(), prop.ID, prop.PlanHash, 1); !errors.Is(err, ErrDryRunRequired) {
		t.Fatalf("适配器不同的记录应被判无效，实际 %v", err)
	}
}

// TestDryRunBlocksNonPublicTargets 守住 SSRF 面：
// 模型（或被文档注入的模型）可以随手填一个地址，不校验就等于给它一个内网探测器。
func TestDryRunBlocksNonPublicTargets(t *testing.T) {
	cases := []string{
		"http://127.0.0.1:9/",
		"http://localhost:8080/v1",
		"http://169.254.169.254/latest/meta-data",
		"file:///etc/passwd",
		"ftp://example.com/x",
	}
	for _, target := range cases {
		_, err := DryRun(context.Background(), DryRunInput{AdapterID: "openai", BaseURL: target})
		if err == nil {
			t.Fatalf("%s 应被 netguard 拒绝", target)
		}
		if !errors.Is(err, ErrDryRunBlocked) {
			t.Fatalf("%s 的拒绝原因应是 ErrDryRunBlocked，实际 %v", target, err)
		}
	}
}

// TestPlanNeedsProbeCoversAdapterSwitch 守住"换适配器也要探测"。
//
// 换协议（OpenAI 兼容 ↔ 声明式规格）时 base_url 往往一个字符都不变，
// 只看地址的话，最该探测的一次变更反而放行。
func TestPlanNeedsProbeCoversAdapterSwitch(t *testing.T) {
	gdb := withAgentDB(t)
	ch := model.Channel{
		Name: "换协议渠道", AdapterID: "spec:relay-a", BaseURL: "https://api.c.example",
		Status: model.ChannelStatusEnabled, MultiKeyMode: "random", Weight: 5, Priority: 1,
	}
	if err := gdb.Create(&ch).Error; err != nil {
		t.Fatalf("种子渠道失败: %v", err)
	}
	plan := mustDecode(t, fmt.Sprintf(`{"summary":"换协议","operations":[
		{"op":"update","entity":"channel","id":%d,"expect_version":1,
		 "data":{"adapter_id":"spec:relay-b"}}
	]}`, ch.ID))
	need := needsOf(t, gdb, testSessionID, plan)
	if len(need) != 1 {
		t.Fatalf("换适配器应要求探测，实际 %d 条", len(need))
	}
	// base_url 没提交 → 从库里补，否则这条目标永远匹配不到记录。
	if need[0].BaseURL != "https://api.c.example" {
		t.Fatalf("应回填库内地址，实际 %q", need[0].BaseURL)
	}

	// 只改名字：不要求探测（否则改个名还要先点一次连通性测试，是流程负担）。
	rename := mustDecode(t, fmt.Sprintf(`{"summary":"改名","operations":[
		{"op":"update","entity":"channel","id":%d,"expect_version":1,"data":{"name":"新名字"}}
	]}`, ch.ID))
	if got := needsOf(t, gdb, testSessionID, rename); len(got) != 0 {
		t.Fatalf("只改名不该要求探测，实际 %+v", got)
	}
}

// TestProbeGateAcrossTurns 是本次故障的**直接回归**。
//
// 真实时序：第 1 轮只做探测（探完就结束），第 2 轮才 propose_change。
// 早前凭据只存在 run 内存里，第 2 轮的提案上一条都没有 → apply 恒 409，
// 而运营明明刚探过。现在凭据落 agent_dry_runs，跨轮仍然有效。
func TestProbeGateAcrossTurns(t *testing.T) {
	gdb := withAgentDB(t)
	setDryRunRequired(t, gdb, true)

	// 第 1 轮：只有探测（走真实写入路径 RecordDryRun，不手工插行）。
	session := testSessionID
	base, fp := probeKeyOf(session, "spec:relay-a", e2eBase, "sk-1")
	rec := &model.AgentDryRun{
		SessionID: session, RunID: 1, MessageID: 1,
		AdapterID: "spec:relay-a", BaseURL: base, KeyFP: fp,
		OK: true, Status: 200, CreatedAt: time.Now(),
	}
	if _, err := RecordDryRun(context.Background(), rec); err != nil {
		t.Fatalf("写凭据失败: %v", err)
	}

	// 第 2 轮：新建提案（不同 run/message），apply 必须放行。
	prop := createProposal(t, happyPlan())
	if prop.ID == 0 {
		t.Fatal("提案没落库")
	}
	if prop.SessionID == 0 {
		// 零值会话会让"读写两侧盐不一致"这个缺陷被判为通过（两侧都算 0）。
		t.Fatal("夹具的会话 id 不能是 0，否则这条用例是假绿")
	}
	if _, err := Apply(context.Background(), prop.ID, prop.PlanHash, 1); err != nil {
		t.Fatalf("上一轮探过的凭据必须跨轮有效，实际被拒：%v", err)
	}
}

// TestProbeGateRejectsKeyChange 守"换密钥必须重探"。
//
// 用 A 密钥探通、拿 B 密钥上线，结果是每个请求都 401 —— 而闸门看起来是"探过的"。
func TestProbeGateRejectsKeyChange(t *testing.T) {
	gdb := withAgentDB(t)
	setDryRunRequired(t, gdb, true)
	prop := createProposal(t, happyPlan())

	// 凭据是用另一把密钥探出来的。
	seedProbe(t, gdb, prop.SessionID, "spec:relay-a", e2eBase, "sk-OTHER", true, time.Now(), time.Time{})

	_, err := Apply(context.Background(), prop.ID, prop.PlanHash, 1)
	if !errors.Is(err, ErrDryRunRequired) {
		t.Fatalf("换密钥应被要求重探，实际 %v", err)
	}
	// 拒绝理由必须说清是"密钥已更换"，否则运营只会反复重试。
	var de *DryRunRequiredError
	if !errors.As(err, &de) || len(de.Targets) == 0 || de.Targets[0].Reason != "key_mismatch" {
		t.Fatalf("拒绝理由应指向 key_mismatch，实际 %+v", err)
	}
}

// TestProbeTargetsFailsClosedOnUnreadableSettings 守"读不到设置时拒绝"。
//
// 早前是 serr == nil && …：一次 DB 抖动就让风控 fail-open。
func TestProbeTargetsFailsClosedOnUnreadableSettings(t *testing.T) {
	gdb := withAgentDB(t)
	setDryRunRequired(t, gdb, true)
	prop := createProposal(t, happyPlan())

	// 模拟"设置读不到"：把表删掉。**不能**把 db.DB 置 nil —— 那样会在读提案时
	// 就 nil panic，测到的是空指针而不是闸门的 fail-closed 行为。
	if err := gdb.Exec("DROP TABLE agent_settings").Error; err != nil {
		t.Fatalf("删设置表失败: %v", err)
	}
	if _, err := Apply(context.Background(), prop.ID, prop.PlanHash, 1); err == nil {
		t.Fatal("设置读不到时必须拒绝，不能放行（fail-open 等于没验证就能上线）")
	}
}

// TestProbeStatusIgnoresHistoricalExpired 守"补探成功后不能再报过期"。
//
// 真实时序：先探一次（24h 后过期）→ 出问题补探一次成功。库里同时留着
// 「已过期的旧行」与「刚成功的新行」。早前的判定只要发现任意一条过期就报
// expired，于是补探成功仍然点不动"确认应用" —— 症状与"探过了还要求探测"一样。
func TestProbeStatusIgnoresHistoricalExpired(t *testing.T) {
	gdb := withAgentDB(t)
	setDryRunRequired(t, gdb, true)
	prop := createProposal(t, happyPlan())

	// 旧行：两天前探的，已过期。
	seedProbe(t, gdb, prop.SessionID, "spec:relay-a", e2eBase, "sk-1", true,
		time.Now().Add(-49*time.Hour), time.Now().Add(-25*time.Hour))
	// 新行：刚才补探成功。
	seedProbe(t, gdb, prop.SessionID, "spec:relay-a", e2eBase, "sk-1", true, time.Now(), time.Time{})

	if _, err := Apply(context.Background(), prop.ID, prop.PlanHash, 1); err != nil {
		t.Fatalf("最新一条有效就应放行（历史过期行不参与判定），实际 %v", err)
	}
}

// TestProbeStatusReasonsOnlyFromLatest 守"失败原因只能来自最新那条"。
func TestProbeStatusReasonsOnlyFromLatest(t *testing.T) {
	gdb := withAgentDB(t)
	setDryRunRequired(t, gdb, true)
	prop := createProposal(t, happyPlan())
	// 先失败，后成功。
	seedProbe(t, gdb, prop.SessionID, "spec:relay-a", e2eBase, "sk-1", false,
		time.Now().Add(-2*time.Minute), time.Time{})
	seedProbe(t, gdb, prop.SessionID, "spec:relay-a", e2eBase, "sk-1", true, time.Now(), time.Time{})

	st, err := ProbeStatusFor(context.Background(), gdb, prop.SessionID, mustDecode(t, happyPlan()))
	if err != nil {
		t.Fatalf("算探测状态失败: %v", err)
	}
	if !st.Required || !st.Satisfied {
		t.Fatalf("最新一条成功即满足，实际 %+v", st)
	}
	for _, tg := range st.Targets {
		if tg.Reason != "ok" {
			t.Fatalf("原因应取自最新那条（ok），实际 %q", tg.Reason)
		}
	}
}

// TestProbeStatusBatchUsesOneQueryShape 守列表页的批量口径：
// 不计算已终态提案，且必须为"只改名"的提案返回 Required=false。
func TestProbeStatusBatchUsesOneQueryShape(t *testing.T) {
	gdb := withAgentDB(t)
	setDryRunRequired(t, gdb, true)
	draft := createProposal(t, happyPlan())
	done := createProposal(t, happyPlan())
	if err := gdb.Model(&model.AgentProposal{}).Where("id = ?", done.ID).
		Update("status", model.ProposalApplied).Error; err != nil {
		t.Fatalf("改状态失败: %v", err)
	}
	// 必须从库里重读：内存里的 done 还是 draft（Update 不会回写结构体），
	// 直接传它测到的是"我自己构造的入参"，不是真实列表。
	var fresh []model.AgentProposal
	if err := gdb.Where("id IN ?", []uint64{draft.ID, done.ID}).Find(&fresh).Error; err != nil {
		t.Fatalf("重读提案失败: %v", err)
	}
	got, err := ProbeStatusBatch(context.Background(), gdb, fresh)
	if err != nil {
		t.Fatalf("批量算探测状态失败: %v", err)
	}
	if _, has := got[done.ID]; has {
		t.Fatal("已应用提案不该被计算（白解 plan）")
	}
	st, has := got[draft.ID]
	if !has || !st.Required || st.Satisfied {
		t.Fatalf("新建渠道的草稿必须 Required 且未满足，实际 %+v", st)
	}
	if st.Targets == nil {
		t.Fatal("targets 必须是空切片而不是 nil（前端 .length 会抛错）")
	}
}
