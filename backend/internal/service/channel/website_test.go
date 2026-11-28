package channel

import (
	"strings"
	"testing"

	"lingdong-api/internal/model"
)

// 本文件守「渠道官网页」的三条不变式：
//  1. 回落规则只有一条（WebsiteFor）：适配器默认 → openai 的 host 规则 → 空；
//  2. 归一/校验与 BaseURL 完全不同（展示链接允许私网、允许路径，但拒 javascript:/userinfo）；
//  3. 存量回填只填空值、软删行也填、且只跑一次。

func TestWebsiteFor(t *testing.T) {
	cases := []struct{ adapter, base, want string }{
		{"duomiapi", "https://duomiapi.com", "https://duomiapi.com"},
		{"metaso", "https://metaso.cn/v1", "https://metaso.cn"},
		{"autodl", "https://autodl.art", "https://autodl.art"},
		{"volcengine-ark", "https://ark.cn-beijing.volces.com/api/v3", "https://www.volcengine.com/product/ark"},
		// openai 是协议适配器：官网由 base_url 的 host 决定（大小写不敏感）。
		{"openai", "https://api.deepseek.com", "https://www.deepseek.com"},
		{"openai", "https://API.DeepSeek.com/v1", "https://www.deepseek.com"},
		{"openai", "https://api.openai.com/v1", "https://openai.com"},
		{"openai", "https://proxy.example.com", ""},
		{"spec:my-gw", "https://gw.example.com", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := WebsiteFor(c.adapter, c.base); got != c.want {
			t.Errorf("WebsiteFor(%q, %q) = %q，期望 %q", c.adapter, c.base, got, c.want)
		}
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://API.OpenAI.com/", "https://api.openai.com"},
		{"https://api.openai.com:443/v1/", "https://api.openai.com/v1"},
		{"http://example.com:80/a?b=1#c", "http://example.com/a"},
		{"  https://duomiapi.com  ", "https://duomiapi.com"},
		{"https://example.com:8443/api/", "https://example.com:8443/api"},
		{"", ""},
		{"not-a-url", ""},
		{"https://", ""},
	}
	for _, c := range cases {
		if got := NormalizeBaseURL(c.in); got != c.want {
			t.Errorf("NormalizeBaseURL(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestValidateWebsite(t *testing.T) {
	ok := []string{"", "https://duomiapi.com", "http://intranet.local:8080/console", "https://example.com/a?b=1"}
	for _, s := range ok {
		v := s
		if err := validateWebsite(&v); err != nil {
			t.Errorf("%q 应通过校验，实际 %v", s, err)
		}
	}
	if err := validateWebsite(nil); err != nil {
		t.Fatalf("nil 表示不修改，不应报错：%v", err)
	}
	bad := []struct{ in, want string }{
		{"javascript:alert(1)", "http/https"},
		{"ftp://example.com", "http/https"},
		{"//example.com", "http/https"},
		{"https://user:pass@example.com", "用户名或密码"},
		{"https://" + strings.Repeat("a", 600) + ".com", "512"},
	}
	for _, c := range bad {
		v := c.in
		err := validateWebsite(&v)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q 应报「%s」，实际 %v", c.in, c.want, err)
		}
	}
}

func TestEffectiveWebsitePrefersOwn(t *testing.T) {
	own := &model.Channel{AdapterID: "duomiapi", BaseURL: "https://duomiapi.com", Website: "https://mine.example.com"}
	if got := effectiveWebsite(own); got != "https://mine.example.com" {
		t.Fatalf("自己的官网应优先，实际 %q", got)
	}
	fallback := &model.Channel{AdapterID: "duomiapi", BaseURL: "https://duomiapi.com"}
	if got := effectiveWebsite(fallback); got != "https://duomiapi.com" {
		t.Fatalf("未配置时应回落适配器默认，实际 %q", got)
	}
}

func TestCreatePersistsWebsiteAndEffective(t *testing.T) {
	withChannelDB(t)

	own := "https://custom.example.com/console"
	r := channelReq("官网渠道")
	r.Website = &own
	item, err := Create(r, "")
	if err != nil {
		t.Fatalf("创建失败：%v", err)
	}
	if item.Website != own || item.WebsiteURL != own {
		t.Fatalf("官网未正确回显：website=%q website_url=%q", item.Website, item.WebsiteURL)
	}

	// 编辑为 openai + DeepSeek：清空自定义官网后，有效官网应回落到 host 规则。
	empty := ""
	r.AdapterID = "openai"
	r.BaseURL = "https://api.deepseek.com"
	r.Website = &empty
	updated, err := Update(item.ID, r, "")
	if err != nil {
		t.Fatalf("更新失败：%v", err)
	}
	if updated.Website != "" {
		t.Fatalf("清空后 website 应为空，实际 %q", updated.Website)
	}
	if updated.WebsiteURL != "https://www.deepseek.com" {
		t.Fatalf("有效官网应回落到 DeepSeek，实际 %q", updated.WebsiteURL)
	}
}

func TestCreateRejectsBadWebsite(t *testing.T) {
	withChannelDB(t)

	bad := "javascript:alert(1)"
	r := channelReq("坏官网渠道")
	r.Website = &bad
	if _, err := Create(r, ""); err == nil || !strings.Contains(err.Error(), "http/https") {
		t.Fatalf("非 http(s) 官网应被拒，实际 %v", err)
	}
}

func TestPatchMergeKeepsAndOverridesWebsite(t *testing.T) {
	cur := &model.Channel{
		Name: "x", AdapterID: "openai", BaseURL: "https://api.example.com",
		Website: "https://old.example.com", MultiKeyMode: ModeRandom,
		Groups: model.MustJSON([]string{"default"}), Weight: 1, Priority: 1,
	}
	req, err := (Patch{}).Merge(cur)
	if err != nil {
		t.Fatalf("Merge 失败：%v", err)
	}
	if req.Website == nil || *req.Website != cur.Website {
		t.Fatalf("未提交 website 时必须复制库内现值，实际 %v", req.Website)
	}
	next := "https://new.example.com"
	req, err = (Patch{Website: &next}).Merge(cur)
	if err != nil {
		t.Fatalf("Merge 失败：%v", err)
	}
	if req.Website == nil || *req.Website != next {
		t.Fatalf("提交 website 时必须覆盖，实际 %v", req.Website)
	}
}

func TestBackfillWebsites(t *testing.T) {
	gdb := withChannelDB(t)
	if err := gdb.AutoMigrate(&model.DataMigration{}); err != nil {
		t.Fatalf("迁移 data_migrations 失败：%v", err)
	}

	mk := func(name, adapter, base, website string) model.Channel {
		ch := model.Channel{
			Name: name, AdapterID: adapter, BaseURL: base, Website: website,
			Keys: model.MustJSON([]string{}), MultiKeyMode: ModeRandom,
			Groups: model.MustJSON([]string{"default"}), Weight: 1, Priority: 1,
			Status: model.ChannelStatusDisabled,
		}
		if err := gdb.Create(&ch).Error; err != nil {
			t.Fatalf("插入渠道失败：%v", err)
		}
		return ch
	}

	// 软删行也要回填：恢复出厂会把它们拉回来。
	soft := mk("待回填渠道", "duomiapi", "https://duomiapi.com", "")
	if err := gdb.Delete(&model.Channel{}, soft.ID).Error; err != nil {
		t.Fatalf("软删失败：%v", err)
	}
	// 无规则（openai + 私有域名）保持空。
	noRule := mk("无规则渠道", "openai", "https://proxy.example.com", "")
	// 运营手工填过的不覆盖。
	manual := mk("手工官网渠道", "metaso", "https://metaso.cn", "https://mine.example.com")

	if err := BackfillWebsites(gdb); err != nil {
		t.Fatalf("回填失败：%v", err)
	}

	var softRow model.Channel
	if err := gdb.Unscoped().First(&softRow, soft.ID).Error; err != nil {
		t.Fatalf("读取软删渠道失败：%v", err)
	}
	if softRow.Website != "https://duomiapi.com" {
		t.Fatalf("软删渠道应被回填，实际 %q", softRow.Website)
	}
	var noRuleRow model.Channel
	if err := gdb.First(&noRuleRow, noRule.ID).Error; err != nil {
		t.Fatalf("读取无规则渠道失败：%v", err)
	}
	if noRuleRow.Website != "" {
		t.Fatalf("无规则渠道不应被填，实际 %q", noRuleRow.Website)
	}
	var manualRow model.Channel
	if err := gdb.First(&manualRow, manual.ID).Error; err != nil {
		t.Fatalf("读取手工渠道失败：%v", err)
	}
	if manualRow.Website != "https://mine.example.com" {
		t.Fatalf("手工官网不应被覆盖，实际 %q", manualRow.Website)
	}

	// 第二次必须因 marker 直接返回（不再动数据）。
	if err := gdb.Model(&model.Channel{}).Where("id = ?", noRule.ID).UpdateColumn("website", "").Error; err != nil {
		t.Fatalf("重置失败：%v", err)
	}
	if err := BackfillWebsites(gdb); err != nil {
		t.Fatalf("二次回填失败：%v", err)
	}
	var n int64
	if err := gdb.Model(&model.DataMigration{}).Where("name = ?", websiteBackfillMarker).Count(&n).Error; err != nil {
		t.Fatalf("统计标记失败：%v", err)
	}
	if n != 1 {
		t.Fatalf("回填标记应只有一行，实际 %d", n)
	}
}
