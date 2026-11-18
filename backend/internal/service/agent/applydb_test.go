package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/service/docdocs"
	"lingdong-api/internal/service/pricing"
	specsvc "lingdong-api/internal/service/spec"
)

// 本文件覆盖提案 → apply → 回滚的**事务语义**（必须连库：CAS、唯一索引、
// 事务可见性、审计同事务写入都无法用内存替身验证）。
//
// 五个关键用例对应方案 §3.2：
//  1. apply 成功：五类实体落库 + 版本 +1 + 快照 + 审计；
//  2. 版本冲突 → stale（提案基于旧数据生成）；
//  3. 双击 apply → 第二次被状态机挡住（不是"改两次"）；
//  4. 回滚 → 生成**反向提案**（不是魔法撤销）；
//  5. 漂移 → 拒绝自动回滚。

const agentTestSchema = "ld_test_agent"

func withAgentDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("LD_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 LD_TEST_DSN，跳过数据库集成用例")
	}
	silent := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), silent)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := admin.Exec("DROP SCHEMA IF EXISTS " + agentTestSchema + " CASCADE").Error; err != nil {
		t.Fatalf("清理测试 schema 失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA " + agentTestSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+agentTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	// 文档内容表必须一起建：AI 的 doc_* 工具直接读写这几张表。
	// （文档站本身与模型解耦，apply 之后不再触发任何文档重算。）
	if err := gdb.AutoMigrate(
		&model.Vendor{}, &model.AdapterSpec{}, &model.Channel{}, &model.Model{}, &model.ModelBinding{},
		// v7 文档内容（AI 的 doc_* 工具直接读写这几张表）。
		&model.DocSection{}, &model.DocNode{}, &model.DocEndpoint{},
		&model.DocEndpointField{}, &model.DocFieldEnum{}, &model.DocResponseVariant{},
		&model.DocPreset{}, &model.DocEndpointError{},
		&model.AdminAudit{},
		&model.AgentSession{}, &model.AgentMessage{}, &model.AgentProposal{}, &model.AgentRun{},
		&model.AgentSetting{}, &model.AgentDoc{}, &model.AgentAsk{},
		// 探测凭据（跨轮复用）：apply 的闸门直接查这张表。
		&model.AgentDryRun{},
	); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	if err := gdb.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_adapter_specs_slug
		ON adapter_specs (slug) WHERE deleted_at IS NULL`).Error; err != nil {
		t.Fatalf("创建规格唯一索引失败: %v", err)
	}
	// 关掉"必须先探测"的闸门：这些用例考的是 apply 的机制（CAS/快照/回滚），
	// 探测闸门由专项用例覆盖（见 dryrun_test.go），不在这里重复。
	// 夹具的预算必须跟默认值一致：写死 32000 会让"每步固定开销 7–9k"的用例
	// 在第 3 步就撞闸，而失败信息是 max_tokens —— 看起来像业务回归，其实是夹具。
	if err := gdb.Table("agent_settings").Create(map[string]any{
		"id": 1, "enabled": true,
		"max_steps": model.DefaultAgentMaxSteps, "max_tokens_per_run": model.DefaultAgentMaxTokensPerRun,
		"dry_run_required": false,
	}).Error; err != nil {
		t.Fatalf("写设置失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	// 价格快照是**进程级缓存**（pricing.Get 带 TTL）。换库不失效的话，
	// 上一个用例在空库里算出的"没有任何可用模型"会被下一个用例读到，
	// 症状是"单独跑绿、连起来跑报没有可用对话模型"。这是测试隔离问题，
	// 不是业务缺陷——但只有在这一层修才是唯一一处。
	pricing.InvalidatePricingCache()
	// 出厂种子要真跑一遍：生产里 main 在启动时播种（协议篇 / 通用接口 / 说明页），
	// 而"apply 之后模型出现在协议篇的模型清单里"这条断言，前提是那几篇已经存在。
	// 不播的话只会在本文件里得到 record not found，看起来像链路坏了。
	if _, err := docdocs.SeedDocs(); err != nil {
		t.Fatalf("写入出厂种子失败: %v", err)
	}
	// 规格解析器接进注册表（生产由 main 装）。不装的话 apply 建规格渠道时
	// 找不到适配器，用例会以"适配器不可用"失败，看起来像代码错。
	specsvc.Install()
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + agentTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
		pricing.InvalidatePricingCache()
		specsvc.InvalidateLocal("relay-a")
	})
	return gdb
}

// createProposal 是"校验通过并落库"的提案（失败直接 Fatal，用例只关心 apply 之后）。
// testSessionID 是提案夹具的会话 id。
//
// **必须非零**：探测凭据的指纹以 session 当盐，夹具用 0 会让"读写两侧盐不一致"
// 这个致命缺陷被判为通过（两侧都算 0）—— 那正是上一批的假绿。
const testSessionID uint64 = 4201

func createProposal(t *testing.T, plan string) *model.AgentProposal {
	t.Helper()
	p, err := CreateProposal(context.Background(), ProposalInput{
		SessionID: testSessionID, Kind: "change", Plan: json.RawMessage(plan),
	})
	if err != nil {
		t.Fatalf("创建提案失败: %v", err)
	}
	if p.Status != model.ProposalDraft {
		t.Fatalf("新提案状态应为 draft，实际 %s", p.Status)
	}
	return p
}

func TestApplyCreatesEverything(t *testing.T) {
	gdb := withAgentDB(t)
	prop := createProposal(t, happyPlan())

	res, err := Apply(context.Background(), prop.ID, prop.PlanHash, 7)
	if err != nil {
		t.Fatalf("apply 失败: %v", err)
	}
	if res.Ops != 5 {
		t.Fatalf("应执行 5 个操作，实际 %d", res.Ops)
	}

	var spec model.AdapterSpec
	if err := gdb.Where("slug = ?", "relay-a").First(&spec).Error; err != nil {
		t.Fatalf("规格未落库: %v", err)
	}
	var ch model.Channel
	if err := gdb.Where("name = ?", "中转A-国内").First(&ch).Error; err != nil {
		t.Fatalf("渠道未落库: %v", err)
	}
	if ch.AdapterID != "spec:relay-a" {
		t.Fatalf("渠道适配器应为 spec:relay-a，实际 %s", ch.AdapterID)
	}
	var m model.Model
	if err := gdb.Where("id = ?", "relay-chat").First(&m).Error; err != nil {
		t.Fatalf("模型未落库: %v", err)
	}
	if m.Vendor != "relayco" {
		t.Fatalf("模型供应商应为 relayco，实际 %s", m.Vendor)
	}
	var bindings []model.ModelBinding
	if err := gdb.Where("model_id = ?", "relay-chat").Find(&bindings).Error; err != nil {
		t.Fatalf("查询绑定失败: %v", err)
	}
	if len(bindings) != 1 || bindings[0].ChannelID != ch.ID {
		t.Fatalf("绑定不正确: %+v", bindings)
	}

	var after model.AgentProposal
	if err := gdb.First(&after, prop.ID).Error; err != nil {
		t.Fatalf("读取提案失败: %v", err)
	}
	if after.Status != model.ProposalApplied {
		t.Fatalf("提案应为 applied，实际 %s（error=%s）", after.Status, after.Error)
	}
	if len(after.Snapshot) == 0 {
		t.Fatal("apply 必须写快照，回滚完全依赖它")
	}
	var snap applySnapshot
	if err := json.Unmarshal(after.Snapshot, &snap); err != nil {
		t.Fatalf("快照不是合法 JSON: %v", err)
	}
	if _, ok := snap.AfterVersions["model:relay-chat"]; !ok {
		t.Fatalf("快照缺 model 版本：%+v", snap.AfterVersions)
	}

	var audits int64
	if err := gdb.Model(&model.AdminAudit{}).Where("action = ?", "agent.apply").Count(&audits).Error; err != nil {
		t.Fatalf("查询审计失败: %v", err)
	}
	if audits != 1 {
		t.Fatalf("审计应恰好一条 agent.apply，实际 %d", audits)
	}

	// 文档站与模型解耦：apply 之后不需要、也不允许再「重算文档」。
	// 归属是**读取时现算**的：族按 `FamilyDoc.Match` 命中、协议篇按 `AppliesDoc` 派生，
	// 所以这里直接验「派生结果」而不是「表里有没有那行」。
	if got, ok := apidoc.FamilyDocOfModelID("relay-chat"); ok {
		t.Fatalf("对话模型不该属于任何模型族：它只出现在协议篇里，实际命中 %q", got.Key)
	}
}

func TestApplyTwiceIsRejected(t *testing.T) {
	withAgentDB(t)
	prop := createProposal(t, happyPlan())
	if _, err := Apply(context.Background(), prop.ID, prop.PlanHash, 1); err != nil {
		t.Fatalf("首次 apply 失败: %v", err)
	}
	// 第二次必须被状态机挡住：否则会把同一批操作再跑一遍（渠道重名、模型已存在）。
	_, err := Apply(context.Background(), prop.ID, prop.PlanHash, 1)
	if !errors.Is(err, ErrNotApplyable) {
		t.Fatalf("双击 apply 应返回 ErrNotApplyable，实际 %v", err)
	}
}

func TestApplyHashMismatch(t *testing.T) {
	withAgentDB(t)
	prop := createProposal(t, happyPlan())
	_, err := Apply(context.Background(), prop.ID, strings.Repeat("0", 64), 1)
	if !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("hash 不一致应返回 ErrHashMismatch，实际 %v", err)
	}
}

func TestApplyStaleOnVersionConflict(t *testing.T) {
	gdb := withAgentDB(t)
	seedModel(t, gdb, "m1", "原始名称")
	prop := createProposal(t, updateModelPlan("m1", "新名称", 1))

	// 模拟"提案生成后、确认前，别人改了这个模型"。
	if err := gdb.Model(&model.Model{}).Where("id = ?", "m1").
		Update("version", gorm.Expr("version + 1")).Error; err != nil {
		t.Fatalf("制造版本漂移失败: %v", err)
	}
	_, err := Apply(context.Background(), prop.ID, prop.PlanHash, 1)
	if !errors.Is(err, ErrStale) {
		t.Fatalf("版本冲突应返回 ErrStale，实际 %v", err)
	}
	var after model.AgentProposal
	if err := gdb.First(&after, prop.ID).Error; err != nil {
		t.Fatalf("读取提案失败: %v", err)
	}
	// stale 必须落库：否则前端刷新后看到它还是 draft，运营会再点一次同样的失败。
	if after.Status != model.ProposalStale {
		t.Fatalf("失败后提案应为 stale，实际 %s", after.Status)
	}
	var m model.Model
	if err := gdb.Where("id = ?", "m1").First(&m).Error; err != nil {
		t.Fatalf("读取模型失败: %v", err)
	}
	if m.DisplayName != "原始名称" {
		t.Fatalf("stale 提案不得改动数据，display_name 应为原始名称，实际 %s", m.DisplayName)
	}
}

func TestRollbackProducesReverseProposal(t *testing.T) {
	gdb := withAgentDB(t)
	seedModel(t, gdb, "m1", "原始名称")
	prop := createProposal(t, updateModelPlan("m1", "新名称", 1))
	if _, err := Apply(context.Background(), prop.ID, prop.PlanHash, 1); err != nil {
		t.Fatalf("apply 失败: %v", err)
	}
	var m model.Model
	if err := gdb.Where("id = ?", "m1").First(&m).Error; err != nil {
		t.Fatalf("读取模型失败: %v", err)
	}
	if m.DisplayName != "新名称" {
		t.Fatalf("apply 未生效：%s", m.DisplayName)
	}

	rev, err := Rollback(context.Background(), prop.ID, 2)
	if err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	// 回滚产出的是**新提案**，不是立刻改库：反向变更同样要人工确认。
	if rev.ID == prop.ID {
		t.Fatal("回滚必须生成新提案，不能原地改原提案")
	}
	if rev.Status != model.ProposalDraft {
		t.Fatalf("反向提案应为 draft，实际 %s", rev.Status)
	}
	plan, err := DecodePlan(rev.Plan)
	if err != nil {
		t.Fatalf("反向提案的 plan 不合法: %v", err)
	}
	if len(plan.Operations) != 1 {
		t.Fatalf("反向提案应只有一个操作，实际 %d", len(plan.Operations))
	}
	op := plan.Operations[0]
	if op.Entity != EntityModel || op.Op != OpUpdate {
		t.Fatalf("反向操作应为 update model，实际 %s %s", op.Op, op.Entity)
	}
	var d ModelData
	if err := strictUnmarshal(op.Data, &d); err != nil {
		t.Fatalf("反向操作 data 不合法: %v", err)
	}
	if d.DisplayName == nil || *d.DisplayName != "原始名称" {
		t.Fatalf("反向提案应还原为原始名称，实际 %v", d.DisplayName)
	}

	// 模型此时仍是"新名称"（回滚还没被 apply）。
	if err := gdb.Where("id = ?", "m1").First(&m).Error; err != nil {
		t.Fatalf("读取模型失败: %v", err)
	}
	if m.DisplayName != "新名称" {
		t.Fatalf("生成反向提案不得改库，实际 %s", m.DisplayName)
	}
	// 反向提案本身必须可应用（能过校验才算真的可回滚）。
	if _, err := Apply(context.Background(), rev.ID, rev.PlanHash, 2); err != nil {
		t.Fatalf("应用反向提案失败: %v", err)
	}
	if err := gdb.Where("id = ?", "m1").First(&m).Error; err != nil {
		t.Fatalf("读取模型失败: %v", err)
	}
	if m.DisplayName != "原始名称" {
		t.Fatalf("应用反向提案后应回到原始名称，实际 %s", m.DisplayName)
	}
}

func TestRollbackRejectsDrift(t *testing.T) {
	gdb := withAgentDB(t)
	seedModel(t, gdb, "m1", "原始名称")
	prop := createProposal(t, updateModelPlan("m1", "新名称", 1))
	if _, err := Apply(context.Background(), prop.ID, prop.PlanHash, 1); err != nil {
		t.Fatalf("apply 失败: %v", err)
	}
	// 应用之后模型又被人工改了一次：自动回滚会覆盖掉这次人工修改。
	if err := gdb.Model(&model.Model{}).Where("id = ?", "m1").
		Updates(map[string]any{"display_name": "人工改的名称", "version": gorm.Expr("version + 1")}).Error; err != nil {
		t.Fatalf("制造漂移失败: %v", err)
	}
	if _, err := Rollback(context.Background(), prop.ID, 1); !errors.Is(err, ErrDrift) {
		t.Fatalf("漂移应返回 ErrDrift，实际 %v", err)
	}
}

/* ------------------------------ 测试夹具 ------------------------------ */

func seedModel(t *testing.T, gdb *gorm.DB, id, display string) {
	t.Helper()
	// 刻意**不写 endpoints**：这几个用例关心的是版本 CAS 与反向提案，
	// 一旦模型声明了端点，覆盖校验（规则 10）就会因为这个种子模型没有绑定渠道而先失败，
	// 用例会以一句与主题无关的"没有渠道能承接"告终。
	inPrice := int64(500000)
	m := model.Model{
		ID: id, DisplayName: display, Vendor: "relayco",
		Modality: "chat", BillingMode: model.BillingModeTokens, Enabled: true,
		Prices: model.Prices{Input: &inPrice},
	}
	if err := gdb.Create(&m).Error; err != nil {
		t.Fatalf("插入模型失败: %v", err)
	}
	// Create 会因 `default:1` 把 Version 回填成 1；显式确认一次，
	// 否则下面的 expect_version 断言会建立在假设之上。
	if err := gdb.Model(&model.Model{}).Where("id = ?", id).
		Update("version", 1).Error; err != nil {
		t.Fatalf("设置模型版本失败: %v", err)
	}
}

// updateModelPlan 是一条"只改模型显示名"的提案（回滚/漂移/版本冲突都用它）。
func updateModelPlan(id, display string, expectVersion int) string {
	return fmt.Sprintf(`{
		"summary": "把模型显示名改成 %s",
		"operations": [
			{"op":"update","entity":"model","key":%q,"expect_version":%d,
			 "data":{"display_name":%q,"modality":"chat","billing_mode":"tokens"}}
		]
	}`, display, id, expectVersion, display)
}
