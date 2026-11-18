package controller

import (
	"testing"

	"lingdong-api/internal/model"
	"lingdong-api/internal/service/stats"
)

// 本文件守管理台的标签回显：**不过滤过期项**，且日期/过期由服务端算好。

func TestAdminTagsIncludeExpired(t *testing.T) {
	m := &model.Model{Tags: model.MustJSON([]model.ModelTag{
		{Label: "长期", Color: "teal"},
		{Label: "旧活动", Color: "#0EA5E9", Until: "2020-01-01T00:00:00Z"},
	})}

	got := adminTagsOf(m)
	if len(got) != 2 {
		t.Fatalf("管理台应看到全部标签（含过期），实际 %d：%v", len(got), got)
	}
	if got[0].Label != "长期" || got[0].ColorHex != "#0D9488" || got[0].Expired || got[0].UntilDate != "" {
		t.Fatalf("长期标签回显不对：%+v", got[0])
	}
	if !got[1].Expired {
		t.Fatalf("过期标签应标 expired：%+v", got[1])
	}
	if got[1].Color != "#0EA5E9" {
		t.Fatalf("color 应回显入库原值（供色块回填），实际 %q", got[1].Color)
	}
	// 00:00Z = 北京时间 08:00，业务日期仍是当天。
	if want := model.TagUntilDate("2020-01-01T00:00:00Z", stats.Location()); got[1].UntilDate != want {
		t.Fatalf("until_date 应为 %q，实际 %q", want, got[1].UntilDate)
	}
}

func TestAdminTagsEmptyIsArray(t *testing.T) {
	// 空对象必须是 []：前端 tags.map 遇到 null 会崩。
	if got := adminTagsOf(&model.Model{}); got == nil || len(got) != 0 {
		t.Fatalf("无标签时应返回空切片，实际 %#v", got)
	}
}
