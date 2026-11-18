package payment

import (
	"fmt"
	"regexp"
	"strings"

	"go.uber.org/zap"

	"lingdong-api/internal/pkg/logger"
)

// 本文件是「渠道参数自洽性」的唯一校验点。
//
// 分成两档是刻意的，两档的调用时机完全不同：
//
//	ValidateChannelShape    格式是否能被解析（PEM/长度/序列号）。与 enabled 无关，
//	                        只要字段**非空**就校验 —— 运营把掩码改坏、把私钥粘到公钥框，
//	                        保存时立刻报错，不用等到启用或下单。
//	ValidateChannelRequired 必填是否齐全。只在「启用且本次有改动」时调用，
//	                        与 sameChannel 豁免配合，避免存量半空配置锁死整页保存。
//
// 为什么「非空即校验格式」是安全的：systemconfig.Update 的流水线是
// ① restoreSensitive（把 ****** 还原成真实值）→ ② validate。
// 校验看到的是真实 PEM，不会把脱敏占位当密钥解析；而空值直接跳过，
// 存量渠道（新字段全是空串）不受任何影响。

// certSerialPattern 是微信商户证书序列号的宽松校验。
//
// 微信当前的序列号是 40 位十六进制（如 5157F09EFDC096DE15EBE81A47057A7232F1B8E1）。
// 这里刻意放宽到 16–64 位：既挡掉「把整段私钥粘进来」这类明显错误，
// 又不会因为微信将来改长度把运营锁死。
var certSerialPattern = regexp.MustCompile(`^[0-9A-Fa-f]{16,64}$`)

// ValidateChannelShape 校验与「是否启用」无关的格式约束（全部非空才校验）。
func ValidateChannelShape(ch Channel) error {
	p := ParseProtocol(ch.Protocol)
	if !p.Valid() {
		return nil
	}
	name := strings.TrimSpace(ch.Name)
	wrap := func(format string, args ...any) error {
		return fmt.Errorf("支付渠道「%s」"+format, append([]any{name}, args...)...)
	}

	if v := strings.TrimSpace(ch.Params.APIv3Key); v != "" && len(v) != 32 {
		return wrap("的 APIv3 密钥必须是 32 位（当前 %d 位）：它用来解密回调，长度错一位回调就全解不开", len(v))
	}
	if v := strings.TrimSpace(ch.Params.CertSerialNo); v != "" && !certSerialPattern.MatchString(v) {
		return wrap("的商户证书序列号格式不对：应为 16–64 位十六进制（取 apiclient_cert.pem 的 serial）")
	}
	if v := strings.TrimSpace(ch.Params.PublicKeyID); v != "" && !strings.HasPrefix(v, "PUB_KEY_ID_") {
		return wrap("的微信支付公钥 ID 必须以 PUB_KEY_ID_ 开头（从商户平台直接复制，不要手写）")
	}

	// 结算币种：能力来自协议，不是运营的偏好设置。
	//
	// 单币种协议（易支付/虎皮椒/微信/支付宝）的报文里根本没有币种参数，金额恒按
	// CNY 解释。字段虽已从它们的 schema 移除，ChannelParams 仍会全量回写，
	// 所以直接调 API 依然能把 settle_currency 写成 usd —— 那种渠道会在美元站的
	// 收银台上出现，用户付的是人民币金额、系统却按美元记额度。这里把它挡在保存期，
	// 给出明确文案，而不是让它变成一条「静默不生效」的脏数据。
	//
	// 多币种协议（Stripe）才校验取值合法性；不校验「与站点币种一致」，
	// 否则「先切币种、再逐条改渠道」这条正常路径会被堵死（一致性由
	// systemconfig 的切币种前置检查兜底）。
	declared := normCurrency(ch.Params.SettleCurrency)
	if fixed, ok := FixedCurrency(p); ok {
		if declared != "" && declared != fixed {
			return wrap("固定按 %s 结算，不能改成「%s」", fixed, ch.Params.SettleCurrency)
		}
	} else if declared != "" && !CurrencyAllowed(p, declared) {
		return wrap("的结算币种「%s」不支持：只能是 %s", ch.Params.SettleCurrency, strings.Join(CurrenciesOf(p), " 或 "))
	}

	if err := parseIfPresent(ch.Params.PrivateKey, func(s string) error {
		_, e := LoadRSAPrivateKey(s)
		return e
	}); err != nil {
		return wrap("的私钥无法解析：%v", err)
	}
	if err := parseIfPresent(ch.Params.PublicKey, func(s string) error {
		_, e := LoadRSAPublicKey(s)
		return e
	}); err != nil {
		return wrap("的公钥无法解析：%v", err)
	}
	if err := parseIfPresent(ch.Params.PlatformCert, func(s string) error {
		_, e := LoadCertBundle(s)
		return e
	}); err != nil {
		return wrap("的平台证书无法解析：%v", err)
	}
	return nil
}

// ValidateChannelRequired 校验「启用」所需的必填项是否齐全。
//
// 必填清单的唯一真相源是 schema 的 Required 声明，不再按协议写 switch：
// 新增协议时只改 schema，漏写必填不会被静默放过。
func ValidateChannelRequired(ch Channel) error {
	sch, ok := SchemaOf(ParseProtocol(ch.Protocol))
	if !ok {
		return nil // 空协议/未实现协议由调用方拦（它们有自己的错误文案）
	}
	name := strings.TrimSpace(ch.Name)
	missing := make([]string, 0, len(sch.Params))
	for _, spec := range sch.Params {
		// ShowWhen 字段（微信的三个验签材料）在这里跳过，由 validateWxpayVerifyMaterial
		// 统一接管。理由：后端看不到 UI 的 verify_mode，硬按 Required 判会同时要求
		// 公钥与平台证书，报出「缺 2 项」这种误导文案。
		//
		// **不要在这里加「按客户端传来的 mode 过滤」的逻辑** —— 那是把校验开关
		// 交给请求方，等于允许「传 pubkey 就跳过证书校验」。
		if !spec.Required || spec.Hidden || spec.ShowWhen != nil {
			continue
		}
		if strings.TrimSpace(ParamValue(ch, spec.Key)) == "" {
			missing = append(missing, spec.Label)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("启用支付渠道「%s」前需要补齐：%s", name, strings.Join(missing, "、"))
	}

	if ParseProtocol(ch.Protocol) == ProtocolWxpay {
		return validateWxpayVerifyMaterial(name, ch.Params)
	}
	return nil
}

// ChannelUsable 判断一条**已启用**的渠道是否真的能完成「收钱 + 回调入账」。
//
// 为什么不能只看 enabled：配置页允许「未改动且停用」的历史脏数据继续保存
// （否则一条脏数据会把整页配置锁死），于是一条「启用 + 缺密钥」的渠道可以长期存在。
// 这种渠道的收银台按钮必须消失 —— 否则用户付了钱，回调侧因为 fail-closed
// 拒绝处理（密钥空 = 无法验签），订单永远停在 pending，只能人工退款。
func ChannelUsable(ch Channel) bool {
	if !ch.Enabled || !ParseProtocol(ch.Protocol).Valid() {
		return false
	}
	return ValidateChannelRequired(ch) == nil
}

// validateWxpayVerifyMaterial 校验微信的验签材料，只看数据、不看 UI 的验签方式开关。
//
// 允许两组同时存在：出站 usePublicKeyMode()（wxpay_client.go:48）在公钥完整时走公钥，
// 入站 resolveNotifyVerifier()（:186-204）按回调头 Wechatpay-Serial 前缀自适应。
// 两组都配是换证窗口的正确配置 —— 清空任一都会让某一类回调直接 500。
func validateWxpayVerifyMaterial(name string, p ChannelParams) error {
	pub := strings.TrimSpace(p.PublicKey)
	id := strings.TrimSpace(p.PublicKeyID)
	cert := strings.TrimSpace(p.PlatformCert)
	switch {
	case pub == "" && id == "" && cert == "":
		return fmt.Errorf("微信渠道「%s」缺少验签材料：填「微信支付公钥 + 公钥 ID」，老商户改填平台证书", name)
	case (pub == "") != (id == ""):
		return fmt.Errorf("微信渠道「%s」的公钥与公钥 ID 必须成对填写：只填一个无法验签回调", name)
	case pub != "" && id != "" && cert != "":
		logger.L().Info("微信渠道同时配置了公钥与平台证书：出站走公钥、回调按 Wechatpay-Serial 自适应",
			zap.String("channel", name))
	}
	return nil
}

// ParamValue 按 schema 的 json 键读取渠道参数值（读不到返回空串）。
//
// 用显式映射而不是反射：键只有 16 个，switch 能被编译器查漏；
// 反射写错字只会在运行期静默返回空串（正是「必填校验永远通过」那类事故）。
func ParamValue(ch Channel, key string) string {
	switch key {
	case "pay_address":
		return ch.Params.PayAddress
	case "query_address":
		return ch.Params.QueryAddress
	case "refund_address":
		return ch.Params.RefundAddress
	case "pid":
		return ch.Params.PID
	case "key":
		return ch.Params.Key
	case "app_id":
		return ch.Params.AppID
	case "app_secret":
		return ch.Params.AppSecret
	case "return_url":
		return ch.Params.ReturnURL
	case "callback_url":
		return ch.Params.CallbackURL
	case "mch_id":
		return ch.Params.MchID
	case "api_v3_key":
		return ch.Params.APIv3Key
	case "private_key":
		return ch.Params.PrivateKey
	case "cert_serial_no":
		return ch.Params.CertSerialNo
	case "public_key":
		return ch.Params.PublicKey
	case "public_key_id":
		return ch.Params.PublicKeyID
	case "platform_cert":
		return ch.Params.PlatformCert
	case "settle_currency":
		return ch.Params.SettleCurrency
	case "secret_key":
		return ch.Params.SecretKey
	case "webhook_secret":
		return ch.Params.WebhookSecret
	}
	return ""
}

// RestoreReservedParams 用旧值补回「已从管理台移除」的四个字段。
//
// 为什么需要：ChannelParams 没有 omitempty，保存时会把结构体全量 marshal 回库；
// 而这些键运营看不到也改不了，任何「变空」都只可能是客户端裁剪了未显示的字段
// （旧前端只提交 schema 里的键、或第三方直接调 API）。不补回去的话，一次无关的
// 保存就会把存量值永久抹掉 —— 且没有任何界面能让它回来（不可逆）。
//
// 只在「新值为空、旧值非空」时补：真正想清空的场景对这些字段不存在
// （它们要么是推导出来的，要么是生产链路不读的死配置）。
func RestoreReservedParams(next *Channel, prev Channel) {
	if next == nil {
		return
	}
	if next.Params.QueryAddress == "" {
		next.Params.QueryAddress = prev.Params.QueryAddress
	}
	if next.Params.RefundAddress == "" {
		next.Params.RefundAddress = prev.Params.RefundAddress
	}
	if next.Params.ReturnURL == "" {
		next.Params.ReturnURL = prev.Params.ReturnURL
	}
	if next.Params.CallbackURL == "" {
		next.Params.CallbackURL = prev.Params.CallbackURL
	}
}

// parseIfPresent 空值跳过，非空则交给 fn 解析。
func parseIfPresent(raw string, fn func(string) error) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return fn(raw)
}
