package systemconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/pkg/affcfg"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/ssrf"
	"lingdong-api/internal/pkg/sitecfg"
	"lingdong-api/internal/pkg/uploadcfg"
	"lingdong-api/internal/service/payment"
	"lingdong-api/internal/service/releasecheck"
	"lingdong-api/internal/service/storage"
)

// ---------- 解码 / 脱敏 / 还原 ----------

// decode 把原始 JSON 解成通用值，数字保留字面量（UseNumber）。
//
// 必须 UseNumber：金额是 int64 微，走 float64 会在 2^53 以上悄悄改数值，
// 而这里的结果会被原样重新序列化回库。
func decode(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// maskSensitive 把敏感叶子替换成占位符（空值不脱敏：管理员据此判断字段是否已配置）。
func maskSensitive(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if storage.IsCredentialField(k) {
				if s, ok := val.(string); ok && s != "" {
					out[k] = Mask
					continue
				}
			}
			out[k] = maskSensitive(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = maskSensitive(val)
		}
		return out
	default:
		return v
	}
}

// restoreSensitive 把提交上来的占位符还原成库里的原值。
//
// 数组按**稳定 id**对齐而不是下标：前端支持卡片 ↑↓ 调序，按下标还原会让
// 「虎皮椒微信」拿到「虎皮椒支付宝」的密钥 —— 表现是支付验签随机失败，
// 而两边配置看起来都正常。
func restoreSensitive(next, prev any, path string) any {
	switch t := next.(type) {
	case map[string]any:
		pm, _ := prev.(map[string]any)
		for k, val := range t {
			if s, ok := val.(string); ok && s == Mask {
				if pm != nil {
					if pv, ok := pm[k]; ok {
						t[k] = pv
						continue
					}
				}
				// 唯一能发现「库里的原值丢了 / 前端把占位符用在了新元素上」的信号。
				if storage.IsCredentialField(k) {
					logger.L().Warn("敏感字段占位未匹配到原值，已置空", zap.String("path", path+"."+k))
				}
				t[k] = ""
				continue
			}
			var pv any
			if pm != nil {
				pv = pm[k]
			}
			t[k] = restoreSensitive(val, pv, path+"."+k)
		}
		return t
	case []any:
		pl, _ := prev.([]any)
		byID := alignByID(pl)
		for i, val := range t {
			var pv any
			if byID != nil {
				// id 对不上（新增元素）时 pv 为 nil：占位符会被置空，绝不借用同下标的旧值。
				if id, ok := elemID(val); ok {
					pv = byID[id]
				}
			} else if pl != nil && i < len(pl) {
				pv = pl[i]
			}
			t[i] = restoreSensitive(val, pv, fmt.Sprintf("%s[%d]", path, i))
		}
		return t
	default:
		return next
	}
}

// alignByID 为「全部元素都有唯一非空字符串 id」的数组建立 id→元素索引。
//
// 任一条件不满足就返回 nil，调用方退回下标语义（数字 id 的公告列表、
// 脏数据里重复 id 的渠道列表都属于这种）。
func alignByID(items []any) map[string]any {
	if len(items) == 0 {
		return nil
	}
	out := make(map[string]any, len(items))
	for _, it := range items {
		id, ok := elemID(it)
		if !ok {
			return nil
		}
		if _, dup := out[id]; dup {
			return nil
		}
		out[id] = it
	}
	return out
}

// elemID 取元素的字符串 id（数字 id 或缺失都返回 false）。
func elemID(v any) (string, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return "", false
	}
	s, ok := m["id"].(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", false
	}
	return s, true
}

// ---------- 各配置键的结构 ----------

// bad 是校验失败的统一出口（对外 400 invalid_param + 中文原因）。
func bad(msg string) error { return errs.InvalidParam.WithMessage(msg) }

// storageCfg 是云存储子树的结构（与 storage.settings 逐字段对齐）。
//
// 这里只声明**保存期需要看**的字段：通道参数是自由形态的文本表，
// 具体键由 storageSchemes 与 storage 包的驱动共同决定，不在这里逐个列字段——
// 列了就成了第二份真相源，新增一个云厂商就要改三处。
type storageCfg struct {
	Active   string                            `json:"active"`
	Channels map[string]map[string]interface{} `json:"channels"`
}

type emailTpl struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

type emailCfg struct {
	Enabled    bool                `json:"enabled"`
	SMTPHost   string              `json:"smtp_host"`
	SMTPPort   int                 `json:"smtp_port"`
	Username   string              `json:"username"`
	Password   string              `json:"password"`
	FromAddr   string              `json:"from_addr"`
	SenderName string              `json:"sender_name"`
	Templates  map[string]emailTpl `json:"templates"`
}

type smsCfg struct {
	Enabled    bool                              `json:"enabled"`
	Active     string                            `json:"active"`
	DailyLimit int64                             `json:"daily_limit"`
	Channels   map[string]map[string]interface{} `json:"channels"`
}

type annItem struct {
	ID          int64  `json:"id"`
	Content     string `json:"content"`
	PublishDate string `json:"publishDate"`
	Type        string `json:"type"`
	Extra       string `json:"extra"`
}

type annCfg struct {
	Enabled bool      `json:"enabled"`
	Items   []annItem `json:"items"`
}

type rateLimitCfg struct {
	RPM         int `json:"rpm"`
	Concurrency int `json:"concurrency"`
}

type monitorCfg struct {
	FailThreshold    float64 `json:"fail_threshold"`
	FailWindow       float64 `json:"fail_window"`
	ProbeIntervalMin float64 `json:"probe_interval_min"`
}

// annTypes 是公告类型枚举（与前端 AnnouncementType 一一对应）。
var annTypes = map[string]bool{
	"default": true, "ongoing": true, "success": true, "warning": true, "error": true,
}

// modalitySet 是允许出现在默认模型配置里的模态。
var modalitySet = map[string]bool{
	"chat": true, "image": true, "audio": true, "video": true, "music": true,
	"embedding": true, "rerank": true, "moderation": true,
}

// ---------- 主校验 ----------

// validate 逐键语义校验（只校验本次提交的键，未提交的保持原值不动）。
func validate(ctx context.Context, gdb *gorm.DB, adminID uint64, next, prev map[string]json.RawMessage) error {
	if raw, ok := next["site"]; ok {
		var s sitecfg.Site
		if err := json.Unmarshal(raw, &s); err != nil {
			return bad("站点配置格式不合法")
		}
		// 归一（去尾斜杠 / 剥 /v1 / 去重 / 长度）与字段级校验都在 sitecfg：
		// 那是**唯一**的 site 模型，别在这里再写一遍规则。
		out, err := sitecfg.Normalize(s)
		if err != nil {
			return bad(err.Error())
		}
		if err := sitecfg.Validate(out); err != nil {
			return bad(err.Error())
		}
		// 跨键：微信直连要求回调为 https，而回调的前缀就是**第一条**地址。
		// 放在保存期拦，比让它变成"渠道显示已启用、回调永远收不到"便宜得多。
		if len(out.APIBaseURLs) > 0 && !strings.HasPrefix(out.APIBaseURLs[0], "https://") {
			for _, p := range enabledProtocols(next) {
				if p == string(payment.ProtocolWxpay) {
					return bad("第一条 API 调用地址必须是 https：已启用的微信支付渠道要求回调地址为 https")
				}
			}
		}
		// 归一结果必须写回 next：Update 落库的是这里的字节，不写回就等于
		// 「界面上看着规整了、库里还是带尾斜杠的原始输入」，下游再拼出双斜杠。
		if err := patchSite(next, out); err != nil {
			return bad("站点配置格式不合法")
		}
	}
	if raw, ok := next["group_discount"]; ok {
		var m map[string]int64
		if err := json.Unmarshal(raw, &m); err != nil {
			return bad("分组折扣必须是整数微比例（1.0 = 1000000）")
		}
		if _, ok := m["default"]; !ok {
			return bad("分组折扣必须包含 default 分组")
		}
		for g, v := range m {
			if v <= 0 {
				return bad("分组折扣「" + g + "」必须大于 0")
			}
			if v > 10*microScale {
				return bad("分组折扣「" + g + "」不能超过 10 倍")
			}
		}
	}
	// 发布清单地址：保存期只做**字面量**校验（不解析 DNS）。
	//
	// 为什么不在保存时解析：这台机器可能为了安全而不允许出站，断网时也要能改配置。
	// 真正的边界在抓取前 —— releasecheck 会再做一次含 DNS 的校验（防 rebinding）。
	if raw, ok := next["release_manifest_url"]; ok {
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return bad("发布清单地址格式不合法")
		}
		v = strings.TrimSpace(v)
		// off 是"显式关闭自动检查"的哨兵值，不是地址：归一成小写后直接放行，
		// 不做 https / 公网校验（它压根不会被请求）。
		// 空串表示"用出厂默认清单"，所以这里也不能拒绝空串。
		if strings.EqualFold(v, releasecheck.OffValue) {
			v = releasecheck.OffValue
		} else if v != "" {
			if len(v) > releasecheck.ManifestURLMaxLen {
				return bad("发布清单地址过长")
			}
			// 清单决定"要不要把线上换成另一个版本"：明文链路可被中间人改写，
			// 所以只接受 https，即使目标是公网地址。
			if !strings.HasPrefix(v, "https://") {
				return bad("发布清单地址必须以 https:// 开头（或填 off 关闭检查）")
			}
			if err := ssrf.ValidatePublicURL(v); err != nil {
				return bad("发布清单地址必须是公网地址（不能是内网/环回/本机名）")
			}
		}
		// 归一结果必须写回：Update 落库的是这里的字节。
		if v == "" {
			next["release_manifest_url"] = json.RawMessage(`""`)
		} else {
			enc, _ := json.Marshal(v)
			next["release_manifest_url"] = json.RawMessage(enc)
		}
	}
	if err := checkMicro(next, "balance_limit"); err != nil {
		return err
	}
	if err := checkMicro(next, "min_topup"); err != nil {
		return err
	}
	if raw, ok := next["cny_per_usd"]; ok {
		var v json.Number
		if err := json.Unmarshal(raw, &v); err != nil {
			return bad("汇率格式不合法")
		}
		f, err := v.Float64()
		if err != nil || f <= 0 {
			return bad("汇率必须大于 0")
		}
	}
	if raw, ok := next["retry_times"]; ok {
		var v int
		if err := json.Unmarshal(raw, &v); err != nil || v < 0 || v > 10 {
			return bad("重试次数须在 0–10 之间")
		}
	}
	for _, k := range []string{"file_retention_days", "poll_deadline_min",
		"logs_retention_days", "archive_retention_days", "audit_retention_days"} {
		if raw, ok := next[k]; ok {
			var v int64
			if err := json.Unmarshal(raw, &v); err != nil || v < 1 {
				return bad(retentionLabel[k] + "须为不小于 1 的整数")
			}
		}
	}
	if raw, ok := next["rate_limit_defaults"]; ok {
		var m rateLimitCfg
		if err := json.Unmarshal(raw, &m); err != nil {
			return bad("默认限流格式不合法")
		}
		if m.RPM < 1 {
			return bad("默认限流：每分钟请求数须不小于 1")
		}
		if m.Concurrency < 1 {
			return bad("默认限流：并发数须不小于 1")
		}
	}
	if raw, ok := next["monitor"]; ok {
		var m monitorCfg
		if err := json.Unmarshal(raw, &m); err != nil {
			return bad("渠道监控格式不合法")
		}
		if m.FailThreshold <= 0 || m.FailThreshold > 1 {
			return bad("失败率阈值须在 (0–1] 区间")
		}
		if m.FailWindow < 1 || m.FailWindow != float64(int64(m.FailWindow)) {
			return bad("统计窗口须为不小于 1 的整数")
		}
		if m.ProbeIntervalMin < 1 || m.ProbeIntervalMin != float64(int64(m.ProbeIntervalMin)) {
			return bad("探活间隔须为不小于 1 的整数")
		}
	}
	if err := validateStorage(next); err != nil {
		return err
	}
	if raw, ok := next["uploads"]; ok {
		// max_size_mb 已改为「按类型上限」（limits）。留着它等于同时存在两套体积口径：
		// 一套是全局 50MB，一套是每类 20/100/200——保存成功但哪个生效没人说得清。
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(raw, &probe); err != nil {
			return bad("文件上传配置格式不合法")
		}
		if _, stale := probe["max_size_mb"]; stale {
			return bad("单文件上限已改为按类型配置（limits），请删除 max_size_mb")
		}
		var u uploadcfg.Settings
		if err := json.Unmarshal(raw, &u); err != nil {
			return bad("文件上传配置格式不合法")
		}
		// 字段清单、默认值与区间只有 uploadcfg 一份，这里不再重复。
		if err := u.Validate(); err != nil {
			return bad(err.Error())
		}
	}
	if raw, ok := next["email"]; ok {
		if err := validateEmail(raw); err != nil {
			return err
		}
	}
	if raw, ok := next["sms"]; ok {
		var s smsCfg
		if err := json.Unmarshal(raw, &s); err != nil {
			return bad("短信配置格式不合法")
		}
		if s.DailyLimit < 0 {
			return bad("短信每日限额不能为负")
		}
		if s.Enabled {
			scheme, ok := smsSchemes[s.Active]
			if !ok {
				return bad("短信渠道不受支持")
			}
			if err := checkChannelRequired(s.Channels[s.Active], scheme, "短信"); err != nil {
				return err
			}
		}
	}
	if raw, ok := next["aff"]; ok {
		if err := validateAff(raw); err != nil {
			return err
		}
	}
	if raw, ok := next["announcements"]; ok {
		if err := validateAnnouncements(raw); err != nil {
			return err
		}
	}
	for _, k := range []string{"about_content", "agreement_content"} {
		if raw, ok := next[k]; ok {
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return bad("站点内容格式不合法")
			}
			if strings.TrimSpace(s) == "" {
				return bad("关于页与用户协议正文不能为空")
			}
		}
	}
	if raw, ok := next["modality_defaults"]; ok {
		var m map[string]string
		if err := json.Unmarshal(raw, &m); err != nil {
			return bad("默认模型格式不合法")
		}
		// 默认模型**允许留空**，空值 = 「自动选择」：派生时由 pricing.deriveDefaults
		// 回落到该模态首个在售模型。强制必填是错的 —— 默认模型本身只是个偏好，
		// 运营完全可能想让系统自己挑；而且每新增一个模态（本次是 music），
		// 存量配置就会在下次保存任意一项时被整页拒绝：明明什么都没改，却保存不了。
		for md, rawID := range m {
			if !modalitySet[md] {
				return bad("默认模型含未知模态「" + md + "」")
			}
			id := strings.TrimSpace(rawID)
			if id == "" {
				continue
			}
			if err := checkDefaultModel(ctx, gdb, md, id); err != nil {
				return err
			}
		}
	}
	if _, ok := next["pay_channels"]; ok {
		if err := validatePayChannels(ctx, gdb, next, prev); err != nil {
			return err
		}
	}
	// 布尔开关只做格式校验：非 true/false（"true" / 1 / null）会让运行期
	// Bool(key, def) 回落默认值 —— 表现为「后台显示已关、实际仍开着」。
	for _, b := range []string{"custom_topup_enabled", "portal_enabled"} {
		if raw, ok := next[b]; ok {
			var v bool
			if err := json.Unmarshal(raw, &v); err != nil {
				return bad("开关值格式不合法")
			}
		}
	}
	if err := validateCurrencySwitch(ctx, gdb, next, prev); err != nil {
		return err
	}
	return validateAuth(ctx, gdb, adminID, next)
}

// retentionLabel 是保留天数类键的中文名（错误文案用）。
var retentionLabel = map[string]string{
	"file_retention_days":    "产物保留天数",
	"poll_deadline_min":      "任务轮询超时（分钟）",
	"logs_retention_days":    "日志保留天数",
	"archive_retention_days": "归档保留天数",
	"audit_retention_days":   "审计保留天数",
}

// microScale 是金额/倍率的刻度（1.0 = 1e6）。
const microScale = 1_000_000

// validateEmail 校验邮件配置：启用时 SMTP 必填；四个场景模板缺一不可。
func validateEmail(raw json.RawMessage) error {
	var e emailCfg
	if err := json.Unmarshal(raw, &e); err != nil {
		return bad("邮件配置格式不合法")
	}
	if e.Enabled {
		switch {
		case strings.TrimSpace(e.SMTPHost) == "":
			return bad("SMTP 服务器未填写")
		case e.SMTPPort < 1 || e.SMTPPort > 65535:
			return bad("SMTP 端口须为 1–65535")
		case strings.TrimSpace(e.Username) == "":
			return bad("SMTP 认证用户名未填写")
		case strings.TrimSpace(e.Password) == "":
			return bad("SMTP 认证密码未填写")
		case strings.TrimSpace(e.FromAddr) == "":
			return bad("发件人地址未填写")
		}
	}
	for _, scene := range []string{"task_done", "low_balance", "price_change", "reset_password"} {
		tpl, ok := e.Templates[scene]
		if !ok {
			return bad("邮件模板缺少场景 " + scene)
		}
		if strings.TrimSpace(tpl.Subject) == "" || strings.TrimSpace(tpl.Body) == "" {
			return bad("邮件模板「" + scene + "」主题/正文不能为空")
		}
	}
	return nil
}

// validateAff 校验邀请返利：键与值域都在 pkg/affcfg，这里只做「错误类型转换」。
//
// 不在本包重新声明一份结构体：这份配置曾有四份键表副本，键名只要有一处写歪，
// 表现就是「保存能过、读取恒为 0」—— 开关开着、一分不发、不报错。
func validateAff(raw json.RawMessage) error {
	r, err := affcfg.Decode(raw)
	if err != nil {
		return bad("邀请返利格式不合法")
	}
	if err := r.Validate(); err != nil {
		return bad(err.Error())
	}
	return nil
}

// validateAnnouncements 校验公告：条数、长度、类型、发布时间。
func validateAnnouncements(raw json.RawMessage) error {
	var a annCfg
	if err := json.Unmarshal(raw, &a); err != nil {
		return bad("站点公告格式不合法")
	}
	if len(a.Items) > 100 {
		return bad("公告最多 100 条")
	}
	for _, it := range a.Items {
		if strings.TrimSpace(it.Content) == "" {
			return bad("公告内容不能为空")
		}
		if len([]rune(it.Content)) > 500 {
			return bad("公告内容 ≤500 字符")
		}
		if len([]rune(it.Extra)) > 100 {
			return bad("公告附加信息 ≤100 字符")
		}
		if strings.TrimSpace(it.PublishDate) == "" {
			return bad("公告发布时间不能为空")
		}
		if a.Enabled && !annTypes[it.Type] {
			return bad("公告类型不受支持")
		}
	}
	return nil
}

// validateStorage 校验云存储配置。
//
// 两个判据都必须基于**本次提交**的通道配置：
//  1. 渠道必须在 storageSchemes 里（配置页可选的值 ⊆ 代码认识的方案）；
//  2. 必须真能构建（storage.ValidateChannel 走运行期同一份 builders）。
//
// 只判「字段非空」是不够的：endpoint 填成非法 URL、path_style 填了无法识别的内容，
// 都要到第一次写对象时才炸，而那时任务已经在跑。
func validateStorage(next map[string]json.RawMessage) error {
	raw, ok := next["storage"]
	if !ok {
		return nil
	}
	var s storageCfg
	if err := json.Unmarshal(raw, &s); err != nil {
		return bad("云存储配置格式不合法")
	}
	scheme, known := storageSchemes[s.Active]
	if !known {
		return bad("云存储渠道不受支持")
	}
	if err := storage.ValidateChannel(s.Active, s.Channels[s.Active]); err != nil {
		return bad("云存储「" + SchemeLabel(s.Active) + "」配置不可用：" + err.Error())
	}
	if s.Active != "none" {
		if err := checkChannelRequired(s.Channels[s.Active], scheme, "云存储"); err != nil {
			return err
		}
	}
	return nil
}

// validateCurrencySwitch 校验结算币种切换。
//
// 两个硬闸：
//  1. 有账务记录后不可再改（三张表任一有数据即锁死）；
//  2. 切完之后必须仍有能收新币种的启用渠道 —— 否则收银台一个支付方式都没有，
//     而页面上不会有任何报错，运营要等用户投诉才发现。
func validateCurrencySwitch(ctx context.Context, gdb *gorm.DB, next, prev map[string]json.RawMessage) error {
	diff := billingSwitchDiff(next, prev)
	if diff == nil {
		return nil
	}
	table, n, err := ledgerRecords(ctx, gdb)
	if err != nil {
		return err
	}
	if n > 0 {
		return bad("已有" + table + "记录，结算币种不可再修改")
	}
	if anyEnabled, payable := payableChannelState(next, siteCurrencyOf(next)); anyEnabled && !payable {
		return bad("切换到该币种后没有任何可用的支付渠道，收银台将没有支付方式")
	}
	return nil
}

// patchSite 把归一后的站点字段写回提交包，**保留未识别的键**。
//
// 不能整包重建：`site` 是前端整对象提交的，但别的模块/版本可能往里放过标记键，
// 重建会让它们凭空消失。currency 不在这里写——它有自己的「有账后只读」校验。
func patchSite(next map[string]json.RawMessage, s sitecfg.Site) error {
	var obj map[string]any
	if err := json.Unmarshal(next["site"], &obj); err != nil {
		return err
	}
	// 已删除的字段：不显式删掉的话，库里残留的旧键会被整包回写"复活"
	delete(obj, "logo_text")
	delete(obj, "api_base_url")

	obj["site_name"] = s.SiteName
	obj["logo_image"] = s.LogoImage
	obj["favicon_image"] = s.FaviconImage
	obj["site_url"] = s.SiteURL
	obj["api_base_urls"] = s.APIBaseURLs
	obj["contact_email"] = s.ContactEmail
	obj["footer_text"] = s.FooterText
	obj["icp_beian"] = s.ICPBeian

	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	next["site"] = b
	return nil
}

// enabledProtocols 返回「已启用渠道」的协议集合。
//
// 取本次提交的 pay_channels；没提交就取当前生效配置——保存是按页签分片的，
// 在「通用」页签改站点地址时 next 里根本没有 pay_channels。
func enabledProtocols(next map[string]json.RawMessage) []string {
	raw, ok := next["pay_channels"]
	if !ok {
		v, has := conf.System().Get("pay_channels")
		if !has {
			return nil
		}
		raw = v
	}
	var chs []payment.Channel
	if json.Unmarshal(raw, &chs) != nil {
		return nil
	}
	out := make([]string, 0, len(chs))
	for i := range chs {
		if chs[i].Enabled {
			out = append(out, chs[i].Protocol)
		}
	}
	return out
}

// payableChannelState 报告「是否存在已启用的支付渠道」与「其中是否有能收目标币种的」。
//
// 两个值必须分开，合成一个 bool 会把两种完全不同的状态混成「不可保存」：
//   - 一条已启用渠道都没有 —— 这是「还没配支付」的初始状态。此时切换币种**不该**被拦，
//     否则新站上线第一步（先定币种、再配渠道）就死锁，而运营没有别的入口绕过去；
//   - 有已启用渠道、但没有一条能收新币种 —— 这是真实的配置错误：
//     收银台会一条支付方式都不显示，而页面上没有任何报错，运营要等用户投诉才发现。
//
// 取本次提交的 pay_channels；没提交就取当前生效配置 —— 与 reconcile 的口径一致。
func payableChannelState(next map[string]json.RawMessage, currency string) (anyEnabled, payable bool) {
	raw, ok := next["pay_channels"]
	if !ok {
		v, has := conf.System().Get("pay_channels")
		if !has {
			return false, false
		}
		raw = v
	}
	var chs []payment.Channel
	if err := json.Unmarshal(raw, &chs); err != nil {
		return false, false
	}
	for i := range chs {
		if !chs[i].Enabled {
			continue
		}
		anyEnabled = true
		if chs[i].SettlesIn(currency) {
			payable = true
		}
	}
	return anyEnabled, payable
}
