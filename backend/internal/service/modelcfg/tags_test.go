package modelcfg

import (
	"errors"
	"strings"
	"testing"

	"lingdong-api/internal/model"
)

// 本文件守模型营销标签的写入规则。全部走 Build（纯函数，不碰库）：
// 校验只发生在这一处，管理台与 AI 提案共用同一条路径。

func tagInput(tags ...model.ModelTag) Input {
	name := "标签测试模型"
	return Input{
		ID: "tag-model", DisplayName: &name,
		Vendor: "v1", Modality: "chat",
		BillingMode: model.BillingModeTokens, BillingModeSet: true,
		Prices:  model.Prices{Input: i64p(1_000_000)},
		Tags:    tags,
		TagsSet: true,
	}
}

func wantTagFieldError(t *testing.T, err error, wantSub string) {
	t.Helper()
	if err == nil {
		t.Fatalf("应被拒绝（期望含 %q）", wantSub)
	}
	var fe *FieldError
	if !errors.As(err, &fe) {
		t.Fatalf("应为 FieldError，实际 %T: %v", err, err)
	}
	if fe.Field != "tags" {
		t.Fatalf("错误字段应为 tags，实际 %q（%s）", fe.Field, fe.Message)
	}
	if !strings.Contains(fe.Message, wantSub) {
		t.Fatalf("错误文案应含 %q，实际 %q", wantSub, fe.Message)
	}
}

func TestBuildNormalizesTags(t *testing.T) {
	m, err := Build(tagInput(
		model.ModelTag{Label: "  限时免费 ", Color: "red"},
		model.ModelTag{Label: "夜间折扣", Color: "#0ea5e9"},
		model.ModelTag{Label: "订阅优先", Color: "violet", Until: "2026-12-31"},
	))
	if err != nil {
		t.Fatalf("合法标签应通过：%v", err)
	}
	tags := model.DecodeTags(m.Tags)
	if len(tags) != 3 {
		t.Fatalf("应有 3 个标签，实际 %d：%v", len(tags), tags)
	}
	// 顺序 = 数组顺序（没有 order 字段）。
	if tags[0].Label != "限时免费" || tags[1].Label != "夜间折扣" || tags[2].Label != "订阅优先" {
		t.Fatalf("标签顺序被改动：%v", tags)
	}
	// 预设色保持 key；自定义色归一为大写 hex。
	if tags[0].Color != "red" {
		t.Fatalf("预设色应保持 key，实际 %q", tags[0].Color)
	}
	if tags[1].Color != "#0EA5E9" {
		t.Fatalf("自定义色应归一为大写，实际 %q", tags[1].Color)
	}
	// 日期按业务时区当日末刻 → UTC。
	if tags[2].Until != "2026-12-31T15:59:59Z" {
		t.Fatalf("until 应归一为 UTC 当日末刻，实际 %q", tags[2].Until)
	}
}

func TestBuildTagsSkipBlankRows(t *testing.T) {
	// 表单里点了「添加标签」还没填就保存：空行视为不存在，不报错。
	m, err := Build(tagInput(
		model.ModelTag{Label: "   ", Color: "red"},
		model.ModelTag{Label: "新人价", Color: "pink"},
	))
	if err != nil {
		t.Fatalf("空行不应导致 400：%v", err)
	}
	if tags := model.DecodeTags(m.Tags); len(tags) != 1 {
		t.Fatalf("空行应被跳过，实际 %v", tags)
	}
}

func TestBuildTagsEmptyWritesArray(t *testing.T) {
	m, err := Build(tagInput())
	if err != nil {
		t.Fatalf("空标签应通过：%v", err)
	}
	if got := string(m.Tags); got != "[]" {
		t.Fatalf("空标签应落成 []，实际 %q", got)
	}
}

func TestBuildTagsRejectsTooMany(t *testing.T) {
	_, err := Build(tagInput(
		model.ModelTag{Label: "一", Color: "red"},
		model.ModelTag{Label: "二", Color: "blue"},
		model.ModelTag{Label: "三", Color: "lime"},
		model.ModelTag{Label: "四", Color: "pink"},
	))
	wantTagFieldError(t, err, "最多 3 个标签")
}

func TestBuildTagsRejectsLongLabel(t *testing.T) {
	_, err := Build(tagInput(model.ModelTag{Label: strings.Repeat("字", model.TagLabelMaxRunes+1), Color: "red"}))
	wantTagFieldError(t, err, "超过 12 字")
}

func TestBuildTagsRejectsDuplicateLabel(t *testing.T) {
	_, err := Build(tagInput(
		model.ModelTag{Label: "限时免费", Color: "red"},
		model.ModelTag{Label: "限时免费", Color: "blue"},
	))
	wantTagFieldError(t, err, "重复")
}

func TestBuildTagsRejectsUnknownColor(t *testing.T) {
	_, err := Build(tagInput(model.ModelTag{Label: "限时免费", Color: "rainbow"}))
	wantTagFieldError(t, err, "不在可选色板内")
}

func TestBuildTagsRejectsBadHex(t *testing.T) {
	for _, c := range []string{"#12", "#GGGGGG"} {
		_, err := Build(tagInput(model.ModelTag{Label: "限时免费", Color: c}))
		wantTagFieldError(t, err, "6 位十六进制")
	}
}

func TestBuildTagsAllowsPastUntil(t *testing.T) {
	// 已过期的存量项必须能随模型整组保存，否则运营无法续期或删除它。
	m, err := Build(tagInput(model.ModelTag{Label: "旧活动", Color: "red", Until: "2020-01-01"}))
	if err != nil {
		t.Fatalf("过去的到期时间应允许保存：%v", err)
	}
	tags := model.DecodeTags(m.Tags)
	if len(tags) != 1 || tags[0].Until == "" {
		t.Fatalf("过期标签应原样保留：%v", tags)
	}
}

func TestBuildTagsRejectsBadUntil(t *testing.T) {
	_, err := Build(tagInput(model.ModelTag{Label: "限时免费", Color: "red", Until: "31/12/2026"}))
	wantTagFieldError(t, err, "到期时间")
}

// TestTagsRoundTripThroughData 守 AI 快照/回滚：Data → Build 后标签完全一致（含过期项）。
func TestTagsRoundTripThroughData(t *testing.T) {
	m, err := Build(tagInput(
		model.ModelTag{Label: "过期活动", Color: "#DB2777", Until: "2020-01-01"},
		model.ModelTag{Label: "长期", Color: "teal"},
	))
	if err != nil {
		t.Fatalf("构建失败：%v", err)
	}
	in := Data(m)
	if !in.TagsSet || len(in.Tags) != 2 {
		t.Fatalf("Data 未还原标签：%+v", in)
	}
	again, err := Build(in)
	if err != nil {
		t.Fatalf("回滚重建失败：%v", err)
	}
	if string(again.Tags) != string(m.Tags) {
		t.Fatalf("回滚后标签不一致：%s vs %s", again.Tags, m.Tags)
	}
}

// TestFillMissingKeepsTags 守「只改价格不清空标签」——全列写的经典事故。
func TestFillMissingKeepsTags(t *testing.T) {
	cur := &model.Model{
		ID: "tag-model", DisplayName: "X", Vendor: "v1", Modality: "chat",
		BillingMode: model.BillingModeTokens,
		Tags:        model.MustJSON([]model.ModelTag{{Label: "限时免费", Color: "red"}}),
	}
	in := Input{ID: "tag-model"}
	FillMissing(&in, cur)
	if !in.TagsSet || len(in.Tags) != 1 || in.Tags[0].Label != "限时免费" {
		t.Fatalf("FillMissing 未回填标签：%+v", in)
	}
}

// TestTagsSurvivePriceOnlyUpdate 是落库层的回归：改一个价格 lane，标签逐字节不变。
// 其中一项已过期 —— 证明"过期只在读出口过滤，不从库里洗掉"。
func TestTagsSurvivePriceOnlyUpdate(t *testing.T) {
	gdb := withCfgDB(t)
	seedVendorChannel(t, gdb)

	in := tagInput(
		model.ModelTag{Label: "限时免费", Color: "red"},
		model.ModelTag{Label: "旧活动", Color: "#0ea5e9", Until: "2020-01-01"},
	)
	created, err := CreateTx(gdb, in, nil)
	if err != nil {
		t.Fatalf("建模型失败：%v", err)
	}

	updated, err := UpdateTx(gdb, Input{ID: in.ID, Prices: model.Prices{Input: i64p(1_500_000)}}, 0)
	if err != nil {
		t.Fatalf("改价失败：%v", err)
	}
	if string(updated.Tags) != string(created.Tags) {
		t.Fatalf("改价把标签改动了：改前 %s，改后 %s", created.Tags, updated.Tags)
	}
	if tags := model.DecodeTags(updated.Tags); len(tags) != 2 {
		t.Fatalf("过期标签不应被洗掉，实际 %v", tags)
	}
}
