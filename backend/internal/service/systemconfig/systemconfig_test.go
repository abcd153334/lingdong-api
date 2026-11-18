package systemconfig

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/uploadcfg"
	"lingdong-api/internal/service/payment"
)

// ---------- 纯函数用例 ----------

// snapshotSystem 在用例结束时还原 system_config 内存镜像。
func snapshotSystem(t *testing.T) {
	t.Helper()
	prev := conf.System().All()
	t.Cleanup(func() { conf.System().Reload(prev) })
}

func TestReadMasksSensitive(t *testing.T) {
	snapshotSystem(t)
	conf.System().Reload(map[string]json.RawMessage{
		"email": json.RawMessage(`{"enabled":true,"username":"n@x.cn","password":"real-secret","smtp_host":"smtp.x.cn","from_addr":"n@x.cn","templates":{"task_done":{"subject":"a","body":"b"},"low_balance":{"subject":"a","body":"b"},"price_change":{"subject":"a","body":"b"},"reset_password":{"subject":"a","body":"b"}}}`),
		"site":  json.RawMessage(`{"site_name":"X","logo_image":""}`),
	})
	out := Read()
	email, _ := out["email"].(map[string]any)
	if email["password"] != Mask {
		t.Fatalf("密码应脱敏为 %s，实际 %v", Mask, email["password"])
	}
	if email["username"] != "n@x.cn" {
		t.Fatalf("非敏感字段不应脱敏: %v", email["username"])
	}
	// 空值不脱敏（管理员可据此判断字段是否已配置）。
	// uploads 回显必须是**补齐后的完整形状**：前端直接读 compress.enabled / limits，
	// 缺字段的存量值会让管理台整页渲染崩溃。
	conf.System().Set("uploads", json.RawMessage(`{"enabled":true}`))
	u, ok := Read()["uploads"].(uploadcfg.Settings)
	if !ok || !u.Enabled || !u.Compress.Enabled || u.Limits[uploadcfg.KindImage] <= 0 || len(u.Types) == 0 {
		t.Fatalf("uploads 回显应为补齐后的完整形状: %#v", Read()["uploads"])
	}
}

// TestValidateRejectsByCard 逐卡校验（16 卡中 S10 所属卡的必填与区间规则）。
func TestValidateRejectsByCard(t *testing.T) {
	cases := []struct {
		name string
		key  string
		val  string
	}{
		{"站点名称为空", "site", `{"site_name":"","api_base_urls":["https://a.cn"],"contact_email":"a@b.cn"}`},
		{"联系邮箱非法", "site", `{"site_name":"X","api_base_urls":["https://a.cn"],"contact_email":"bad"}`},
		{"Logo 超限", "site", `{"site_name":"X","api_base_urls":["https://a.cn"],"contact_email":"a@b.cn","logo_image":"data:image/png;base64,` + strings.Repeat("A", 3_000_001) + `"}`},
		{"图标超限", "site", `{"site_name":"X","api_base_urls":["https://a.cn"],"contact_email":"a@b.cn","favicon_image":"` + strings.Repeat("A", 140_001) + `"}`},
		// 地址必须是带 host 的 http(s)：前缀判断会放过 "https://" 这种残缺值
		{"API 地址无 scheme", "site", `{"site_name":"X","contact_email":"a@b.cn","api_base_urls":["api.example.com"]}`},
		{"API 地址无 host", "site", `{"site_name":"X","contact_email":"a@b.cn","api_base_urls":["https://"]}`},
		{"API 地址协议不对", "site", `{"site_name":"X","contact_email":"a@b.cn","api_base_urls":["ftp://api.example.com"]}`},
		{"API 地址过多", "site", `{"site_name":"X","contact_email":"a@b.cn","api_base_urls":["https://a.cn","https://b.cn","https://c.cn","https://d.cn","https://e.cn","https://f.cn","https://g.cn","https://h.cn","https://i.cn"]}`},
		// 支付回调 = 首条地址 + "/api/pay/" + 渠道ID + "/notify"，网关上限 128 ⇒ 首条 ≤80
		{"首条地址过长", "site", `{"site_name":"X","contact_email":"a@b.cn","api_base_urls":["https://` + strings.Repeat("a", 72) + `.com"]}`},
		// 站点地址参与 X-Forwarded-Host 白名单判定，非法值比不填更危险
		{"站点地址非法", "site", `{"site_name":"X","contact_email":"a@b.cn","site_url":"www.example.com"}`},
		{"缺 default 分组", "group_discount", `{"vip":900000}`},
		{"折扣为零", "group_discount", `{"default":0}`},
		{"折扣写成小数", "group_discount", `{"default":1,"vip":0.9}`},
		{"折扣超过 10 倍", "group_discount", `{"default":10000000,"vip":20000000}`},
		{"余额上限为负", "balance_limit", `-1`},
		{"最低充值非整数", "min_topup", `1.5`},
		{"汇率非正", "cny_per_usd", `0`},
		{"重试次数为负", "retry_times", `-1`},
		{"单文件上限越界", "uploads", `{"enabled":true,"max_size_mb":0,"daily_limit_per_key":200,"retention_hours":24,"user_total_mb":2048,"concurrency":3}`},
		{"邮件启用缺主机", "email", `{"enabled":true,"smtp_host":"","smtp_port":465,"username":"u","password":"p","from_addr":"a@b.cn","templates":{"task_done":{"subject":"s","body":"b"},"low_balance":{"subject":"s","body":"b"},"price_change":{"subject":"s","body":"b"},"reset_password":{"subject":"s","body":"b"}}}`},
		{"邮件模板缺场景", "email", `{"enabled":false,"templates":{"task_done":{"subject":"s","body":"b"},"low_balance":{"subject":"s","body":"b"},"price_change":{"subject":"s","body":"b"}}}`},
		{"邮件模板正文为空", "email", `{"enabled":false,"templates":{"task_done":{"subject":"s","body":""},"low_balance":{"subject":"s","body":"b"},"price_change":{"subject":"s","body":"b"},"reset_password":{"subject":"s","body":"b"}}}`},
		{"短信启用缺渠道参数", "sms", `{"enabled":true,"active":"aliyun","daily_limit":0,"channels":{"aliyun":{"access_key_id":"id","access_key_secret":"sk","sign_name":"","template_code":"T"}}}`},
		{"短信限额为负", "sms", `{"enabled":false,"active":"aliyun","daily_limit":-1,"channels":{}}`},
		{"云存储渠道不支持", "storage", `{"active":"gcs","channels":{}}`},
		{"云存储必填缺失", "storage", `{"active":"s3","channels":{"s3":{"endpoint":"https://s3.cn","bucket":"b"}}}`},
		{"监控阈值越界", "monitor", `{"auto_disable":true,"fail_threshold":1.5,"fail_window":10,"auto_enable":true,"probe_interval_min":5}`},
		{"公告内容为空", "announcements", `{"enabled":true,"items":[{"id":1,"content":"","publishDate":"2026-10-01T00:00:00+08:00","type":"default","extra":""}]}`},
		{"公告类型不支持", "announcements", `{"enabled":true,"items":[{"id":1,"content":"x","publishDate":"2026-10-01T00:00:00+08:00","type":"weird","extra":""}]}`},
		{"公告内容超长", "announcements", `{"enabled":true,"items":[{"id":1,"content":"` + strings.Repeat("中", 501) + `","publishDate":"2026-10-01T00:00:00+08:00","type":"default","extra":""}]}`},
		{"站点内容为空", "about_content", `""`},
		{"返利字段非整数", "aff", `{"enabled":true,"inviter_rate":1.5,"invitee_amount":0,"max_rewards":3,"hold_hours":0,"min_transfer":0}`},
		{"返利比例超 100%", "aff", `{"enabled":true,"inviter_rate":1000001,"invitee_amount":0,"max_rewards":3,"hold_hours":0,"min_transfer":0}`},
		{"返利笔数为零", "aff", `{"enabled":true,"inviter_rate":100000,"invitee_amount":0,"max_rewards":0,"hold_hours":0,"min_transfer":0}`},
		{"返利冷却期越界", "aff", `{"enabled":true,"inviter_rate":100000,"invitee_amount":0,"max_rewards":3,"hold_hours":721,"min_transfer":0}`},
		{"返利缺少比例", "aff", `{"enabled":true,"invitee_amount":0,"max_rewards":3,"hold_hours":0,"min_transfer":0}`},
		{"支付渠道 id 重复", "pay_channels", `[{"id":"epay","name":"易支付"},{"id":"epay","name":"重复"}]`},
		{"支付渠道名为空", "pay_channels", `[{"id":"epay","name":"  "}]`},
		// 名称不能含 '-'：订单快照是「渠道名-支付方式」，按第一个 '-' 拆
		{"支付渠道名含中划线", "pay_channels", `[{"id":"epay","name":"易-支付"}]`},
		// 启用却不选方式：收银台会出现一张只有渠道名的卡
		{"启用渠道未选支付方式", "pay_channels", `[{"id":"epay","name":"易支付","protocol":"epay","enabled":true,"methods":[],"params":{"pay_address":"https://pay.example.com","pid":"1001","key":"k"}}]`},
		// 虎皮椒一条渠道只有一个 APPID，勾两种会让用户选支付宝却进微信通道
		{"虎皮椒勾选两种方式", "pay_channels", `[{"id":"xunhupay","name":"虎皮椒","protocol":"xunhupay","enabled":true,"methods":["wechat","alipay"],"params":{"pay_address":"https://api.xunhupay.com/payment/do.html","app_id":"a1","app_secret":"s1"}}]`},
		{"无法识别的支付方式", "pay_channels", `[{"id":"epay","name":"易支付","protocol":"epay","enabled":false,"methods":["随便写"]}]`},
		// 同协议商户标识重复：回调的商户号一致性校验会把正常订单判成不一致
		{"同协议商户标识重复", "pay_channels", `[{"id":"xunhupay","name":"虎皮椒","protocol":"xunhupay","enabled":false,"methods":["wechat"],"params":{"app_id":"same"}},{"id":"xunhupay-alipay","name":"虎皮椒（支付宝）","protocol":"xunhupay","enabled":false,"methods":["alipay"],"params":{"app_id":"same"}}]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			next := map[string]json.RawMessage{c.key: json.RawMessage(c.val)}
			if err := validate(context.Background(), nil, 0, next, nil); err == nil {
				t.Fatalf("%s 应校验失败", c.key)
			}
		})
	}
}

// TestValidateAccepts 合法取值应通过（含「短信未启用时渠道参数可为空」级联语义）。
func TestValidateAccepts(t *testing.T) {
	ok := []struct {
		key string
		val string
	}{
		{"site", `{"site_name":"灵动 API","api_base_urls":["https://api.x.cn"],"contact_email":"a@b.cn"}`},
		// 空数组是**合法**的：未配置 API 地址时门户回落站点地址，
		// 这正是"没填就用站点域名"的实现前提（曾经空串会被 isHTTPURL 直接拒）。
		{"site（地址留空）", `{"site_name":"灵动 API","api_base_urls":[],"contact_email":"a@b.cn"}`},
		// 连键都没有同样合法：老客户端/部分提交不会带这个键
		{"site（无地址键）", `{"site_name":"灵动 API","contact_email":"a@b.cn"}`},
		// http 首条可以存（本地开发常见），只有"启用微信直连"时才拦
		{"site（http 首条）", `{"site_name":"灵动 API","api_base_urls":["http://127.0.0.1:8080"],"contact_email":"a@b.cn"}`},
		{"group_discount", `{"default":1000000,"vip":900000}`},
		// 冷却期 0 = 不冷却，是合法取值（不是"缺省"），必须能存。
		{"aff（冷却期为 0）", `{"enabled":true,"inviter_rate":100000,"invitee_amount":5000000,"max_rewards":3,"hold_hours":0,"min_transfer":10000000}`},
		// 关闭时数值项可以为空：运营常常先关开关再改字段。
		{"aff（关闭且字段为空）", `{"enabled":false}`},
		{"balance_limit", `0`},
		{"min_topup", `1000000`},
		{"sms", `{"enabled":false,"active":"aliyun","daily_limit":0,"channels":{"aliyun":{}}}`},
		{"storage", `{"active":"none","channels":{}}`},
		{"announcements", `{"enabled":false,"items":[]}`},
		{"email", `{"enabled":false,"templates":{"task_done":{"subject":"s","body":"b"},"low_balance":{"subject":"s","body":"b"},"price_change":{"subject":"s","body":"b"},"reset_password":{"subject":"s","body":"b"}}}`},
	}
	for _, c := range ok {
		if err := validate(context.Background(), nil, 0, map[string]json.RawMessage{c.key: json.RawMessage(c.val)}, nil); err != nil {
			t.Fatalf("%s 应校验通过，实际: %v", c.key, err)
		}
	}
}

// TestMaskRoundTrip 校验敏感字段「回显 Mask → 提交 Mask → 保留原值」链路。
func TestMaskRoundTrip(t *testing.T) {
	prev := map[string]any{"password": "old-secret", "nested": map[string]any{"secret_key": "old-key"}}
	in := map[string]any{"password": Mask, "nested": map[string]any{"secret_key": Mask}, "plain": "keep"}
	out, _ := restoreSensitive(in, prev, "email").(map[string]any)
	if out["password"] != "old-secret" || out["plain"] != "keep" {
		t.Fatalf("还原失败: %v", out)
	}
	if n, _ := out["nested"].(map[string]any); n["secret_key"] != "old-key" {
		t.Fatalf("嵌套还原失败: %v", out["nested"])
	}
	// 原值不存在时置空（避免把占位符写进存储）。
	out2, _ := restoreSensitive(map[string]any{"password": Mask}, map[string]any{}, "email").(map[string]any)
	if out2["password"] != "" {
		t.Fatalf("无原值时应置空: %v", out2["password"])
	}
}

// ---------- 数据库集成用例（需 LD_TEST_DSN；未设置则跳过） ----------

const testSchema = "ld_test_s10"

func withTestDB(t *testing.T) *gorm.DB {
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
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + testSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+testSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	// 含 Package / Order / Transaction / Usage：切币种校验要查「是否已有账务记录」。
	if err := gdb.AutoMigrate(&model.SystemConfig{}, &model.Model{}, &model.Package{},
		&model.Order{}, &model.Transaction{}, &model.Usage{},
		&model.AdminAudit{}, &model.AnnouncementRead{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + testSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

// TestUpdatePersistsAndHotReloads 保存 → 落库 → 内存热更新 → 审计（无需重启即生效）。
func TestUpdatePersistsAndHotReloads(t *testing.T) {
	gdb := withTestDB(t)
	snapshotSystem(t)
	conf.System().Reload(map[string]json.RawMessage{"about_content": json.RawMessage(`"旧正文"`)})

	body := map[string]json.RawMessage{
		"about_content": json.RawMessage(`"新正文"`),
		"site":          json.RawMessage(`{"site_name":"新品牌","logo_text":"新","logo_image":"","site_url":"https://x.cn","api_base_urls":["https://api.x.cn"],"contact_email":"a@b.cn","footer_text":"","icp_beian":""}`),
	}
	if err := Update(context.Background(), gdb, 1, body); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	// 热更新：内存镜像立即可读新值。
	if got := conf.System().String("about_content", ""); got != "新正文" {
		t.Fatalf("内存未热更新: %s", got)
	}
	// 落库可回读。
	var row model.SystemConfig
	if err := gdb.Where("key = ?", "about_content").First(&row).Error; err != nil {
		t.Fatalf("落库失败: %v", err)
	}
	if !strings.Contains(string(row.Value), "新正文") {
		t.Fatalf("落库值不符: %s", row.Value)
	}
	// 废弃字段必须被 patchSite 剥掉：不剥的话，库里残留的 logo_text 会跟着
	// 每次整包回写「复活」，等于这个字段从未删除。
	var siteRow model.SystemConfig
	if err := gdb.Where("key = ?", "site").First(&siteRow).Error; err != nil {
		t.Fatalf("读取 site 行失败: %v", err)
	}
	if strings.Contains(string(siteRow.Value), "logo_text") {
		t.Fatalf("logo_text 应被剔除: %s", siteRow.Value)
	}
	if !strings.Contains(string(siteRow.Value), "api_base_urls") {
		t.Fatalf("归一后的 api_base_urls 必须落库: %s", siteRow.Value)
	}
	// 审计：config.update + 品牌键变更 → config 域各写一条。
	var n int64
	gdb.Model(&model.AdminAudit{}).Where("action = ?", "config.update").Count(&n)
	if n != 1 {
		t.Fatalf("应写 1 条 config.update 审计，实际 %d", n)
	}
	gdb.Model(&model.AdminAudit{}).Where("admin_id = ?", 1).Count(&n)
	if n < 1 {
		t.Fatalf("审计 admin_id 缺失")
	}
}

// TestUpdateNormalizesSite 归一结果必须**落库**。
//
// 只归一不写回的话，界面上看着规整、库里还是「带尾斜杠 + 带 /v1」的原始输入，
// 下游 base+"/f/" 会拼出双斜杠，门户会显示 /v1/v1，而两处都不报错。
func TestUpdateNormalizesSite(t *testing.T) {
	gdb := withTestDB(t)
	snapshotSystem(t)

	body := map[string]json.RawMessage{
		"site": json.RawMessage(`{"site_name":" A ","contact_email":"a@b.cn",
			"api_base_urls":["https://a.cn/","https://b.cn/v1","https://A.cn"]}`),
	}
	if err := Update(context.Background(), gdb, 1, body); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	var row model.SystemConfig
	if err := gdb.Where("key = ?", "site").First(&row).Error; err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	var got struct {
		SiteName    string   `json:"site_name"`
		APIBaseURLs []string `json:"api_base_urls"`
	}
	if err := json.Unmarshal(row.Value, &got); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got.SiteName != "A" {
		t.Fatalf("站名应去两端空白，实际 %q", got.SiteName)
	}
	// 去尾斜杠 + 剥 /v1 + 大小写去重，且保留首次出现的顺序
	if strings.Join(got.APIBaseURLs, " ") != "https://a.cn https://b.cn" {
		t.Fatalf("归一未落库: %v", got.APIBaseURLs)
	}
}

// TestValidateRejectsUnknownKey 键集白名单：未冻结的键一律拒绝。
func TestUpdateRejectsUnknownKey(t *testing.T) {
	gdb := withTestDB(t)
	snapshotSystem(t)
	err := Update(context.Background(), gdb, 1, map[string]json.RawMessage{"evil_key": json.RawMessage(`1`)})
	if def, ok := errs.AsDef(err); !ok || def.Code != errs.CodeInvalidParam {
		t.Fatalf("未知键应返回 invalid_param，实际: %v", err)
	}
}

// TestUpdateKeepsMaskedSecret 提交 Mask 时保留库中原值（敏感字段不会被占位符覆盖）。
func TestUpdateKeepsMaskedSecret(t *testing.T) {
	gdb := withTestDB(t)
	snapshotSystem(t)
	conf.System().Reload(map[string]json.RawMessage{
		"email": json.RawMessage(`{"enabled":false,"password":"real-secret","templates":{"task_done":{"subject":"s","body":"b"},"low_balance":{"subject":"s","body":"b"},"price_change":{"subject":"s","body":"b"},"reset_password":{"subject":"s","body":"b"}}}`),
	})
	body := map[string]json.RawMessage{
		"email": json.RawMessage(`{"enabled":false,"password":"` + Mask + `","templates":{"task_done":{"subject":"s","body":"b"},"low_balance":{"subject":"s","body":"b"},"price_change":{"subject":"s","body":"b"},"reset_password":{"subject":"s","body":"b"}}}`),
	}
	if err := Update(context.Background(), gdb, 1, body); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	var s struct {
		Password string `json:"password"`
	}
	_ = conf.System().Unmarshal("email", &s)
	if s.Password != "real-secret" {
		t.Fatalf("Mask 应保留原值，实际: %q", s.Password)
	}
}

// TestUpdateDefaultModelWhitelist 默认模型兜底（B7）：须为已启用的同模态模型。
func TestUpdateDefaultModelWhitelist(t *testing.T) {
	gdb := withTestDB(t)
	snapshotSystem(t)
	if err := gdb.Create(&model.Model{ID: "m-chat", DisplayName: "M", Vendor: "openai", Modality: "chat", Enabled: true}).Error; err != nil {
		t.Fatalf("造模型失败: %v", err)
	}
	valid := `{"chat":"m-chat","image":"m-chat","audio":"m-chat","video":"m-chat","embedding":"m-chat","rerank":"m-chat","moderation":"m-chat"}`
	if err := Update(context.Background(), gdb, 1, map[string]json.RawMessage{"modality_defaults": json.RawMessage(valid)}); err == nil {
		t.Fatalf("模态不符应校验失败")
	}
	// 不存在的模型 → 失败
	if err := Update(context.Background(), gdb, 1, map[string]json.RawMessage{"modality_defaults": json.RawMessage(`{"chat":"nope","image":"nope","audio":"nope","video":"nope","embedding":"nope","rerank":"nope","moderation":"nope"}`)}); err == nil {
		t.Fatalf("不存在的默认模型应校验失败")
	}
	// 留空 = 自动选择（派生时回落到该模态首个在售模型）：
	// 强制必填会让「这次没配音频默认模型」变成一个保存不了的配置项 ——
	// 运营明明只改了别的页签，却被「默认模型「audio」不能为空」整页拒绝。
	empty := `{"chat":"","image":"","audio":"","video":"","music":"","embedding":"","rerank":"","moderation":""}`
	if err := Update(context.Background(), gdb, 1, map[string]json.RawMessage{"modality_defaults": json.RawMessage(empty)}); err != nil {
		t.Fatalf("默认模型全部留空必须放行，实际: %v", err)
	}
	// 未知模态键要明说：拼错的键既不会生效、也不会被校验，静默丢弃最难查
	if err := Update(context.Background(), gdb, 1, map[string]json.RawMessage{"modality_defaults": json.RawMessage(`{"chta":"m-chat"}`)}); err == nil {
		t.Fatalf("未知模态键应校验失败")
	}
}

// ---------- restoreSensitive：数组按稳定 id 对齐（B0） ----------

func mustDecode(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	return v
}

// TestRestoreSensitiveArrayReorderedById 本次回归核心：卡片 ↑↓ 调序后密钥不得串位。
func TestRestoreSensitiveArrayReorderedById(t *testing.T) {
	prev := mustDecode(t, `[
		{"id":"wx","name":"虎皮椒微信","params":{"app_id":"wx-app","app_secret":"secret-wx"}},
		{"id":"ali","name":"虎皮椒支付宝","params":{"app_id":"ali-app","app_secret":"secret-ali"}}
	]`)
	// 前端回显后调序（ali 在前），两个密钥都是占位符
	next := mustDecode(t, `[
		{"id":"ali","name":"虎皮椒支付宝","params":{"app_id":"ali-app","app_secret":"******"}},
		{"id":"wx","name":"虎皮椒微信","params":{"app_id":"wx-app","app_secret":"******"}}
	]`)
	out := restoreSensitive(next, prev, "pay_channels").([]any)

	first := out[0].(map[string]any)["params"].(map[string]any)
	second := out[1].(map[string]any)["params"].(map[string]any)
	if first["app_secret"] != "secret-ali" {
		t.Fatalf("调序后第一条密钥应为 secret-ali，实际 %v", first["app_secret"])
	}
	if second["app_secret"] != "secret-wx" {
		t.Fatalf("调序后第二条密钥应为 secret-wx，实际 %v", second["app_secret"])
	}
}

// TestRestoreSensitiveNumericIDKeepsIndex 数字 id 的数组仍按下标对齐（announcements 等不得被改语义）。
func TestRestoreSensitiveNumericIDKeepsIndex(t *testing.T) {
	prev := mustDecode(t, `[{"id":1,"password":"p1"},{"id":2,"password":"p2"}]`)
	next := mustDecode(t, `[{"id":1,"password":"******"},{"id":2,"password":"******"}]`)
	out := restoreSensitive(next, prev, "announcements").([]any)
	if out[0].(map[string]any)["password"] != "p1" || out[1].(map[string]any)["password"] != "p2" {
		t.Fatalf("数字 id 数组应按下标还原，实际 %v / %v", out[0], out[1])
	}
}

// TestRestoreSensitiveDuplicateIDFallsBack 原数组 id 重复（脏数据）→ 退回下标语义。
func TestRestoreSensitiveDuplicateIDFallsBack(t *testing.T) {
	prev := mustDecode(t, `[{"id":"dup","password":"a"},{"id":"dup","password":"b"}]`)
	next := mustDecode(t, `[{"id":"dup","password":"******"},{"id":"dup","password":"******"}]`)
	out := restoreSensitive(next, prev, "pay_channels").([]any)
	if out[0].(map[string]any)["password"] != "a" || out[1].(map[string]any)["password"] != "b" {
		t.Fatalf("id 重复时应退回下标语义，实际 %v / %v", out[0], out[1])
	}
}

// TestRestoreSensitiveNewElementMaskEmptied 新增元素带 Mask → 置空，绝不借用同下标的旧值。
func TestRestoreSensitiveNewElementMaskEmptied(t *testing.T) {
	prev := mustDecode(t, `[{"id":"wx","params":{"app_secret":"secret-wx"}}]`)
	next := mustDecode(t, `[
		{"id":"new","params":{"app_secret":"******"}},
		{"id":"wx","params":{"app_secret":"******"}}
	]`)
	out := restoreSensitive(next, prev, "pay_channels").([]any)
	if got := out[0].(map[string]any)["params"].(map[string]any)["app_secret"]; got != "" {
		t.Fatalf("新元素的 Mask 应置空，实际 %v", got)
	}
	if got := out[1].(map[string]any)["params"].(map[string]any)["app_secret"]; got != "secret-wx" {
		t.Fatalf("同 id 元素应保留原值，实际 %v", got)
	}
}

// ---------- validatePayChannels 增量校验（B1） ----------

func payPayload(s string) map[string]json.RawMessage {
	return map[string]json.RawMessage{"pay_channels": json.RawMessage(s)}
}

// TestValidatePayChannelsNullNormalized JSON null 必须落成 []，不能原样入库。
func TestValidatePayChannelsNullNormalized(t *testing.T) {
	next := payPayload(`null`)
	if err := validatePayChannels(context.Background(), nil, next, nil); err != nil {
		t.Fatalf("null 应归一化为空数组，实际报错: %v", err)
	}
	if got := strings.TrimSpace(string(next["pay_channels"])); got != "[]" {
		t.Fatalf("null 应写回 []，实际 %s", got)
	}
}

// TestValidatePayChannelsProtocolImmutable 已实现协议之间不可切换；"" → 有效值属于补全，放行。
func TestValidatePayChannelsProtocolImmutable(t *testing.T) {
	prev := payPayload(`[{"id":"a","name":"甲","protocol":"xunhupay","enabled":false,"methods":[],"params":{}}]`)
	next := payPayload(`[{"id":"a","name":"甲","protocol":"epay","enabled":false,"methods":[],"params":{}}]`)
	if err := validatePayChannels(context.Background(), nil, next, prev); err == nil {
		t.Fatalf("已实现协议之间不允许切换（历史订单的原路退款会按新协议分发）")
	}

	prevEmpty := payPayload(`[{"id":"b","name":"乙","protocol":"","enabled":false,"methods":[],"params":{}}]`)
	nextFilled := payPayload(`[{"id":"b","name":"乙","protocol":"epay","enabled":false,"methods":[],"params":{}}]`)
	if err := validatePayChannels(context.Background(), nil, nextFilled, prevEmpty); err != nil {
		t.Fatalf("空协议补全为有效协议应放行，实际: %v", err)
	}
}

// TestValidatePayChannelsRejectsNewAggregate 只拒「本次新引入」的聚合支付，存量原样放行。
func TestValidatePayChannelsRejectsNewAggregate(t *testing.T) {
	next := payPayload(`[{"id":"hp","name":"虎皮椒","protocol":"xunhupay","enabled":false,"methods":["aggregate"],"params":{}}]`)
	if err := validatePayChannels(context.Background(), nil, next, payPayload(`[]`)); err == nil {
		t.Fatalf("不允许新选聚合支付")
	}

	prev := payPayload(`[{"id":"hp","name":"虎皮椒","protocol":"xunhupay","enabled":false,"methods":["aggregate"],"params":{"app_id":"1"}}]`)
	nextKeep := payPayload(`[{"id":"hp","name":"虎皮椒","protocol":"xunhupay","enabled":false,"methods":["aggregate"],"params":{"app_id":"2"}}]`)
	if err := validatePayChannels(context.Background(), nil, nextKeep, prev); err != nil {
		t.Fatalf("存量聚合渠道改其它字段必须能保存（否则整页配置锁死），实际: %v", err)
	}
}

// TestValidatePayChannelsEmptyProtocolCannotEnable 未实现协议不允许启用（Options 会直接跳过它）。
func TestValidatePayChannelsEmptyProtocolCannotEnable(t *testing.T) {
	prev := payPayload(`[{"id":"w","name":"微信直连","protocol":"","enabled":false,"methods":[],"params":{}}]`)
	next := payPayload(`[{"id":"w","name":"微信直连","protocol":"","enabled":true,"methods":["wechat"],"params":{}}]`)
	if err := validatePayChannels(context.Background(), nil, next, prev); err == nil {
		t.Fatalf("空协议不能启用")
	}
}

// TestValidatePayChannelsUnchangedEnabledSkipsRequired 未改动的启用渠道仍跳过必填（历史空密钥可保存）。
func TestValidatePayChannelsUnchangedEnabledSkipsRequired(t *testing.T) {
	body := `{"id":"epay","name":"易支付","protocol":"epay","enabled":true,"methods":["wechat","alipay"],"params":{"pay_address":"https://pay.x.cn/submit","pid":"1001","key":""}}`
	if err := validatePayChannels(context.Background(), nil, payPayload(`[`+body+`]`), payPayload(`[`+body+`]`)); err != nil {
		t.Fatalf("未改动的启用渠道不应被必填拦住，实际: %v", err)
	}
}

// ---------- 支付渠道字段瘦身（B2） ----------

// TestValidatePayChannelsKeepsLegacyHttpInbound 存量非 https 的入站地址不得阻断保存。
//
// query_address / refund_address 已从管理台移除（改由 pay_address 推导），
// 运营在界面上改不了它们。若继续强制 https，存量里一个 http 值就会让
// 「连改站点名都 400」，而用户没有任何自救手段（与 M7 的 aggregate 事故同型）。
func TestValidatePayChannelsKeepsLegacyHttpInbound(t *testing.T) {
	body := `[{"id":"xunhupay","name":"虎皮椒","protocol":"xunhupay","enabled":false,
		"methods":["wechat"],
		"params":{"pay_address":"https://api.xunhupay.com/payment/do.html",
		          "app_id":"201906147484","app_secret":"s",
		          "query_address":"http://old.example.com/query.html",
		          "refund_address":"http://old.example.com/refund.html"}}]`
	if err := validatePayChannels(context.Background(), nil, payPayload(body), nil); err != nil {
		t.Fatalf("存量非 https 的入站地址不应阻断保存，实际: %v", err)
	}
}

// TestValidatePayChannelsStillRejectsHttpPayAddress 下单网关仍必须 https。
func TestValidatePayChannelsStillRejectsHttpPayAddress(t *testing.T) {
	body := `[{"id":"xunhupay","name":"虎皮椒","protocol":"xunhupay","enabled":false,
		"methods":["wechat"],
		"params":{"pay_address":"http://api.xunhupay.com/payment/do.html",
		          "app_id":"1","app_secret":"s"}}]`
	if err := validatePayChannels(context.Background(), nil, payPayload(body), nil); err == nil {
		t.Fatal("下单网关必须 https")
	}
}

// TestValidatePayChannelsRestoresReservedParams 未提交的 reserved 键必须用旧值补回。
//
// ChannelParams 没有 omitempty，回写会全量 marshal。若客户端裁剪了这些
// 「界面上已移除」的键，一次无关的保存就会把存量值永久抹掉（不可逆）。
func TestValidatePayChannelsRestoresReservedParams(t *testing.T) {
	prev := payPayload(`[{"id":"xunhupay","name":"虎皮椒","protocol":"xunhupay","enabled":false,
		"methods":["wechat"],
		"params":{"pay_address":"https://api.xunhupay.com/payment/do.html",
		          "app_id":"1","app_secret":"s",
		          "query_address":"https://keep.example.com/query.html",
		          "refund_address":"https://keep.example.com/refund.html",
		          "return_url":"https://keep.example.com/done",
		          "callback_url":"https://keep.example.com/notify"}}]`)
	// 新提交完全不带这四个键（模拟旧前端按 schema 提交）
	next := payPayload(`[{"id":"xunhupay","name":"虎皮椒","protocol":"xunhupay","enabled":false,
		"methods":["wechat"],
		"params":{"pay_address":"https://api.xunhupay.com/payment/do.html",
		          "app_id":"1","app_secret":"s"}}]`)

	if err := validatePayChannels(context.Background(), nil, next, prev); err != nil {
		t.Fatalf("校验失败: %v", err)
	}

	var got []struct {
		Params map[string]string `json:"params"`
	}
	if err := json.Unmarshal(next["pay_channels"], &got); err != nil {
		t.Fatalf("解析回写结果失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应回写 1 条渠道，实际 %d", len(got))
	}
	p := got[0].Params
	for _, k := range []string{"query_address", "refund_address", "return_url", "callback_url"} {
		if !strings.Contains(p[k], "keep.example.com") {
			t.Fatalf("reserved 键 %s 应保留旧值，实际 %q", k, p[k])
		}
	}
}

// TestUpdateCurrencySwitchLockedByLedger 已有账务记录时结算币种永久只读。
//
// 币种是「价签单位」：orders / transactions / usages 里存的是**数字**，
// 币种只作为快照列存在。切换后同一笔 100 会被解释成 100 美元而不是 100 元，
// 退款、对账、客诉全部对不上，且没有办法事后还原当时的含义。
func TestUpdateCurrencySwitchLockedByLedger(t *testing.T) {
	gdb := withTestDB(t)
	snapshotSystem(t)

	const siteCNY = `{"site_name":"站点","currency":"CNY","api_base_urls":["https://a.cn"],"contact_email":"a@b.cn"}`
	const siteUSD = `{"site_name":"站点","currency":"USD","api_base_urls":["https://a.cn"],"contact_email":"a@b.cn"}`
	conf.System().Reload(map[string]json.RawMessage{"site": json.RawMessage(siteCNY)})

	if err := gdb.Select("*").Create(&model.Order{
		UserID: 1, OutTradeNo: "T-LOCK-1", AmountCent: 7200, Currency: conf.CurrencyCNY,
		Credited: 72000000, Status: "paid",
	}).Error; err != nil {
		t.Fatalf("造订单失败: %v", err)
	}

	err := Update(context.Background(), gdb, 1, map[string]json.RawMessage{
		"site": json.RawMessage(siteUSD),
	})
	if err == nil {
		t.Fatal("已有账务记录时切币种必须被拒")
	}
	if msg := err.Error(); !strings.Contains(msg, "结算币种不可再修改") {
		t.Fatalf("错误文案应点明币种锁定，实际: %v", err)
	}
	// 拒绝必须是「整次保存失败」，不能半途落库。
	if got := conf.SiteCurrency(); got != conf.CurrencyCNY {
		t.Fatalf("被拒后币种不应变化，实际 %s", got)
	}
}

// TestUpdateCurrencySwitchAllowedWithoutLedger 没有任何账务记录时允许切币种。
//
// 只有「还没开张」的站点才能改单位；改完之后套餐/价目需要运营自己复核数字含义，
// 那不是代码能拦的（拦住会让切换永远做不成）。
func TestUpdateCurrencySwitchAllowedWithoutLedger(t *testing.T) {
	gdb := withTestDB(t)
	snapshotSystem(t)

	const siteCNY = `{"site_name":"站点","currency":"CNY","api_base_urls":["https://a.cn"],"contact_email":"a@b.cn"}`
	const siteUSD = `{"site_name":"站点","currency":"USD","api_base_urls":["https://a.cn"],"contact_email":"a@b.cn"}`
	conf.System().Reload(map[string]json.RawMessage{"site": json.RawMessage(siteCNY)})

	if err := Update(context.Background(), gdb, 1, map[string]json.RawMessage{
		"site": json.RawMessage(siteUSD),
	}); err != nil {
		t.Fatalf("无账务记录时应允许切币种，实际: %v", err)
	}
	if got := conf.SiteCurrency(); got != conf.CurrencyUSD {
		t.Fatalf("切币种应已生效，实际 %s", got)
	}

	// 币种不变（只改站点名）→ 任何时候都不受影响。
	const siteUSDRenamed = `{"site_name":"新名字","currency":"USD","api_base_urls":["https://a.cn"],"contact_email":"a@b.cn"}`
	if err := Update(context.Background(), gdb, 1, map[string]json.RawMessage{
		"site": json.RawMessage(siteUSDRenamed),
	}); err != nil {
		t.Fatalf("币种未变时不应被拦，实际: %v", err)
	}
}

// TestUpdateCurrencySwitchRequiresPayableChannel 切币种后必须至少还有一条能收新币种的渠道。
//
// 渠道能收哪种币种由**协议**决定（易支付/虎皮椒/微信/支付宝固定人民币，
// 只有 Stripe 可选 CNY/USD），收银台按它过滤。
// 「切完币种、一条可用渠道都不剩」= 站点上线却没有任何支付方式，用户点充值只看到空面板，
// 而且这个状态没有任何报错，运营要等用户投诉才会发现。所以这一种情况必须硬拦。
//
// 其余情况（还剩至少一条）只告警：运营很可能「先切币种，再补一条能收新币种的渠道」。
func TestUpdateCurrencySwitchRequiresPayableChannel(t *testing.T) {
	gdb := withTestDB(t)
	snapshotSystem(t)

	const siteCNY = `{"site_name":"站点","currency":"CNY","api_base_urls":["https://a.cn"],"contact_email":"a@b.cn"}`
	const siteUSD = `{"site_name":"站点","currency":"USD","api_base_urls":["https://a.cn"],"contact_email":"a@b.cn"}`
	// 币种能力来自协议：易支付/虎皮椒/微信/支付宝恒为人民币，只有 Stripe 能收美元。
	// 所以「切到美元后还有可用渠道」这个场景只能用 Stripe 构造 —— 试图把易支付
	// 改成 USD 现在会被保存期直接拒掉（见 payment.TestValidateChannelShapeRejectsWrongSettleCurrency），
	// 那种配置会让用户付人民币、系统记美元。
	epayCNY := map[string]any{
		"id": "epay", "name": "易支付", "protocol": "epay", "enabled": true,
		"methods": []any{"微信支付"},
		"params": map[string]any{
			"pay_address": "https://pay.example.com", "pid": "1001", "key": "secret",
		},
	}
	stripeUSD := map[string]any{
		"id": "stripe", "name": "Stripe", "protocol": "stripe", "enabled": true,
		"methods": []any{"card"},
		"params": map[string]any{
			"secret_key": "sk_test", "webhook_secret": "whsec_test",
			"settle_currency": "USD",
		},
	}
	cnyOnly := []any{epayCNY}
	conf.System().Reload(map[string]json.RawMessage{
		"site":         json.RawMessage(siteCNY),
		"pay_channels": json.RawMessage(model.MustJSON(cnyOnly)),
	})

	// ① 唯一渠道只收人民币 → 切美元必须被拒。
	err := Update(context.Background(), gdb, 1, map[string]json.RawMessage{
		"site":         json.RawMessage(siteUSD),
		"pay_channels": json.RawMessage(model.MustJSON(cnyOnly)),
	})
	if err == nil {
		t.Fatal("切币种后没有可用渠道时必须拒绝，否则收银台会一个支付方式都没有")
	}
	if msg := err.Error(); !strings.Contains(msg, "没有任何可用的支付渠道") {
		t.Fatalf("错误文案应点明「没有可用渠道」，实际: %v", err)
	}

	// ② 同批补一条能收美元的渠道 → 通过（不存在死锁：渠道保存不校验站点币种）。
	usdReady := []any{epayCNY, stripeUSD}
	if err := Update(context.Background(), gdb, 1, map[string]json.RawMessage{
		"site":         json.RawMessage(siteUSD),
		"pay_channels": json.RawMessage(model.MustJSON(usdReady)),
	}); err != nil {
		t.Fatalf("同批带着美元渠道时应允许切换，实际: %v", err)
	}
}

// TestUpdateCurrencySwitchAuditsFromTo 切币种必须在审计里留下「从哪个币种切成哪个」。
//
// 审计只写 {keys:["site"]} 的话，事后只能看到「改过」：无法回答
// 「什么时候从人民币切到美元」，而这个事实决定了全部历史金额该按哪种口径解释
// （对账、客诉、退款都依赖它）。
func TestUpdateCurrencySwitchAuditsFromTo(t *testing.T) {
	gdb := withTestDB(t)
	snapshotSystem(t)

	conf.System().Reload(map[string]json.RawMessage{
		"site": json.RawMessage(`{"site_name":"站点","currency":"CNY","api_base_urls":["https://a.cn"],"contact_email":"a@b.cn"}`),
	})
	if err := Update(context.Background(), gdb, 7, map[string]json.RawMessage{
		"site": json.RawMessage(`{"site_name":"站点","currency":"USD","api_base_urls":["https://a.cn"],"contact_email":"a@b.cn"}`),
	}); err != nil {
		t.Fatalf("切币种失败: %v", err)
	}

	var a model.AdminAudit
	if err := gdb.Where("action = ?", "config.update").Order("id DESC").First(&a).Error; err != nil {
		t.Fatalf("应写审计: %v", err)
	}
	// 按 JSON 解析而不是匹配字符串：map 序列化的键序不保证，靠子串会写成脆断言。
	var got struct {
		Currency struct {
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"currency"`
	}
	if err := json.Unmarshal(a.Detail, &got); err != nil {
		t.Fatalf("审计明细不是合法 JSON: %v（%s）", err, string(a.Detail))
	}
	if got.Currency.From != "CNY" || got.Currency.To != "USD" {
		t.Fatalf("币种切换应记 CNY→USD，实际 %s", string(a.Detail))
	}
}

// TestPayChannelSecretsSealedAtRest 支付渠道凭据必须「密文落库、明文进内存」。
//
// 两条断言缺一不可：
//   - 库里是密文：否则一次 `select value from system_config` 就能拿走全部商户号
//     密钥与私钥（那是能直接发起退款、伪造回调的凭据）；
//   - 内存是明文：configsync 每轮整表重载，内存里躺密文的表现是「配置页一切正常，
//     但回调验签全失败 / 上游 401」—— 把密文当密钥去签了。
func TestPayChannelSecretsSealedAtRest(t *testing.T) {
	gdb := withTestDB(t)
	snapshotSystem(t)
	t.Setenv("APP_ENCRYPTION_KEY", "test-master-key-for-pay-secrets")
	conf.Load()

	raw := model.MustJSON([]any{map[string]any{
		"id": "epay", "name": "易支付", "protocol": "epay", "enabled": false,
		"methods": []any{"wechat"},
		"params": map[string]any{
			"pay_address": "https://pay.example.com", "pid": "1000", "key": "EPAY_PLAIN_KEY",
		},
	}})
	if err := Update(context.Background(), gdb, 1, map[string]json.RawMessage{
		"pay_channels": json.RawMessage(raw),
	}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	var row model.SystemConfig
	if err := gdb.Where("key = ?", "pay_channels").First(&row).Error; err != nil {
		t.Fatalf("读库失败: %v", err)
	}
	stored := string(row.Value)
	if strings.Contains(stored, "EPAY_PLAIN_KEY") {
		t.Fatalf("凭据明文落库了：%s", stored)
	}
	if !strings.Contains(stored, "enc:v1:") {
		t.Fatalf("凭据必须带密文前缀：%s", stored)
	}
	// 非凭据字段保持可读：排查问题时要能直接看库确认网关/商户号。
	// 按结构断言而不是匹配字面量：落库 JSON 的空白由序列化实现决定，
	// 拿 `"pid":"1000"` 这种精确串去比会因为多一个空格而假红。
	var got []struct {
		Protocol string `json:"protocol"`
		Enabled  bool   `json:"enabled"`
		Params   struct {
			PayAddress string `json:"pay_address"`
			PID        string `json:"pid"`
			Key        string `json:"key"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(stored), &got); err != nil {
		t.Fatalf("落库值解析失败: %v\n%s", err, stored)
	}
	if len(got) != 1 || got[0].Protocol != "epay" || got[0].Params.PayAddress != "https://pay.example.com" ||
		got[0].Params.PID != "1000" {
		t.Fatalf("非凭据字段不应受影响：%+v", got)
	}
	if !strings.HasPrefix(got[0].Params.Key, "enc:v1:") {
		t.Fatalf("params.key 必须是密文：%q", got[0].Params.Key)
	}

	// ① 热更新后的内存镜像必须是明文（否则下一个回调就验不过）。
	if got := channelParamKey(t); got != "EPAY_PLAIN_KEY" {
		t.Fatalf("内存镜像必须是明文，实际 %q", got)
	}
	// ② 整表重载（configsync / 重启）也必须解封成明文。
	all, err := db.LoadSystemConfig(gdb)
	if err != nil {
		t.Fatalf("整表装载失败: %v", err)
	}
	var reloaded []struct {
		Params struct {
			Key string `json:"key"`
		} `json:"params"`
	}
	if err := json.Unmarshal(all["pay_channels"], &reloaded); err != nil {
		t.Fatalf("重载结果解析失败: %v", err)
	}
	if len(reloaded) != 1 || reloaded[0].Params.Key != "EPAY_PLAIN_KEY" {
		t.Fatalf("整表重载必须解封成明文，实际 %+v", reloaded)
	}
}

// channelParamKey 取当前内存镜像里唯一支付渠道的密钥（空表示没有渠道）。
func channelParamKey(t *testing.T) string {
	t.Helper()
	chs := payment.Channels()
	if len(chs) != 1 {
		t.Fatalf("内存镜像里应恰好一条渠道，实际 %d 条", len(chs))
	}
	return chs[0].Params.Key
}
