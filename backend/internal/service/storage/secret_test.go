package storage

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"lingdong-api/internal/conf"
)

// withEncryptionKey 给用例固定一个主密钥（脱封/解封都按当前配置派生）。
func withEncryptionKey(t *testing.T) {
	t.Helper()
	t.Setenv("APP_ENCRYPTION_KEY", "test-master-key-for-storage-secrets")
	if err := os.Setenv("APP_ENCRYPTION_KEY", "test-master-key-for-storage-secrets"); err != nil {
		t.Fatalf("设置主密钥失败: %v", err)
	}
	conf.Load()
}

func channelField(t *testing.T, raw json.RawMessage, scheme, key string) string {
	t.Helper()
	var cfg struct {
		Channels map[string]map[string]any `json:"channels"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("storage 解析失败: %v", err)
	}
	v, _ := cfg.Channels[scheme][key].(string)
	return v
}

// TestSealUnsealRoundTrip 落库密文、回读明文，且密文带可识别前缀。
func TestSealUnsealRoundTrip(t *testing.T) {
	withEncryptionKey(t)

	plain := json.RawMessage(`{"active":"oss","channels":{"oss":{
		"endpoint":"oss-cn-hangzhou.aliyuncs.com","bucket":"lingdong",
		"access_key_id":"LTAI_RAW_ID","access_key_secret":"RAW_SECRET"}}}`)

	sealed, _, err := SealSecrets("storage", plain)
	if err != nil {
		t.Fatalf("封印失败: %v", err)
	}
	for _, k := range []string{"access_key_id", "access_key_secret"} {
		v := channelField(t, sealed, "oss", k)
		if v == "" || v == "LTAI_RAW_ID" || v == "RAW_SECRET" {
			t.Fatalf("%s 必须被加密，实际 %q", k, v)
		}
		if !strings.HasPrefix(v, sealedPrefix) {
			t.Fatalf("%s 必须带 %q 前缀（否则无法区分密文与明文），实际 %q", k, sealedPrefix, v)
		}
	}
	// 非凭据字段原样保留：加密它们会让 endpoint/bucket 无法阅读，也会破坏构建逻辑。
	if got := channelField(t, sealed, "oss", "bucket"); got != "lingdong" {
		t.Fatalf("bucket 不该被加密，实际 %q", got)
	}
	if got := channelField(t, sealed, "oss", "endpoint"); got != "oss-cn-hangzhou.aliyuncs.com" {
		t.Fatalf("endpoint 不该被加密，实际 %q", got)
	}

	back, _, err := UnsealSecrets("storage", sealed)
	if err != nil {
		t.Fatalf("解封失败: %v", err)
	}
	if got := channelField(t, back, "oss", "access_key_secret"); got != "RAW_SECRET" {
		t.Fatalf("解封后应还原明文，实际 %q", got)
	}
}

// TestSealIsIdempotent 对已封印的值再封印必须原样返回。
//
// 这是最容易踩的一脚：配置页「取回打码值 → 原样提交」时，还原出来的可能已经是密文。
// 不判断前缀就会变成「密文的密文」，即使用正确主密钥也解不回来 ——
// 而现象只是「某天起云端写入全 401」。
func TestSealIsIdempotent(t *testing.T) {
	withEncryptionKey(t)

	plain := json.RawMessage(`{"channels":{"s3":{"access_key":"AK","secret_key":"SK"}}}`)
	once, _, err := SealSecrets("storage", plain)
	if err != nil {
		t.Fatalf("首次封印失败: %v", err)
	}
	twice, _, err := SealSecrets("storage", once)
	if err != nil {
		t.Fatalf("二次封印失败: %v", err)
	}
	if string(once) != string(twice) {
		t.Fatalf("重复封印不得改变内容\n一次: %s\n二次: %s", once, twice)
	}
	// 二次封印后仍能解回原文，才是真正的幂等。
	back, _, err := UnsealSecrets("storage", twice)
	if err != nil {
		t.Fatalf("解封失败: %v", err)
	}
	if got := channelField(t, back, "s3", "secret_key"); got != "SK" {
		t.Fatalf("二次封印后应仍可解回明文，实际 %q", got)
	}
}

// TestUnsealEmptyOnUndecryptable 换过主密钥时必须把字段置空，绝不把密文当 AK 用。
//
// 把密文当 AK 发去上游的结果是 403 —— 排查方向会变成「密钥权限不对」，
// 而真实原因是「主密钥变了」。置空会让构建报「缺少必填」，指向正确答案。
func TestUnsealEmptyOnUndecryptable(t *testing.T) {
	withEncryptionKey(t)
	sealed, _, err := SealSecrets("storage", json.RawMessage(`{"channels":{"cos":{"secret_id":"ID","secret_key":"SK"}}}`))
	if err != nil {
		t.Fatalf("封印失败: %v", err)
	}

	// 换主密钥后再解封。
	os.Setenv("APP_ENCRYPTION_KEY", "another-master-key")
	conf.Load()
	t.Cleanup(func() { withEncryptionKey(t) })

	back, failed, err := UnsealSecrets("storage", sealed)
	if err != nil {
		t.Fatalf("解封不应整体失败（其余字段仍要可用）: %v", err)
	}
	if got := channelField(t, back, "cos", "secret_key"); got != "" {
		t.Fatalf("无法解密时应置空，实际 %q", got)
	}
	// 失败路径必须按「键.元素.字段」报出来：调用方靠它把「解不开」与「漏填」分开。
	// secret_id 同样命中命名约定（含 secret），所以两个字段都会各自报一次。
	want := []string{"storage.cos.secret_id", "storage.cos.secret_key"}
	if len(failed) != len(want) || failed[0] != want[0] || failed[1] != want[1] {
		t.Fatalf("失败路径不合约：%v", failed)
	}
}

// TestTransformSecretsSkipsNonChannelJSON 无 channels 的对象原样返回（不 panic、不改写）；
// 顶层不是对象的输入必须报错 —— 配置层据此返回 400，而不是把畸形值写进库。
func TestTransformSecretsSkipsNonChannelJSON(t *testing.T) {
	withEncryptionKey(t)

	for _, in := range []string{`{"active":"none"}`, `null`, `{"channels":[]}`, `{"channels":{}}`} {
		out, _, err := SealSecrets("storage", json.RawMessage(in))
		if err != nil {
			t.Fatalf("%s 不应报错: %v", in, err)
		}
		if string(out) != in {
			t.Fatalf("%s 应原样返回，实际 %s", in, out)
		}
	}
	for _, in := range []string{`[]`, `{`} {
		if _, _, err := SealSecrets("storage", json.RawMessage(in)); err == nil {
			t.Fatalf("%q 应报错", in)
		}
	}
}

// TestIsCredentialFieldNamingConvention 命名约定是加密与脱敏的唯一判据，必须覆盖各厂商的写法。
func TestIsCredentialFieldNamingConvention(t *testing.T) {
	yes := []string{
		"secret_key", "secret_id", "access_key", "access_key_id", "access_key_secret",
		"api_v3_key", "private_key", "password", "key", "some_api_key", "refresh_token",
		"SecretKey", // 大小写不敏感
	}
	no := []string{"endpoint", "bucket", "region", "path_style", "sign_name", "template_code", ""}
	for _, k := range yes {
		if !IsCredentialField(k) {
			t.Fatalf("%q 应判定为凭据", k)
		}
	}
	for _, k := range no {
		if IsCredentialField(k) {
			t.Fatalf("%q 不应判定为凭据", k)
		}
	}
}

// payChannelParams 取指定渠道的 params 对象（测试断言用）。
//
// 入参是**该配置键的值**（一个数组），不是整个配置对象 —— 与调用方口径一致。
func payChannelParams(t *testing.T, raw json.RawMessage, id string) map[string]any {
	t.Helper()
	var list []struct {
		ID     string         `json:"id"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("pay_channels 解析失败: %v", err)
	}
	for _, c := range list {
		if c.ID == id {
			return c.Params
		}
	}
	t.Fatalf("找不到渠道 %s", id)
	return nil
}

// TestSealCoversPayChannels 支付渠道的凭据同样必须密文落库。
//
// 旧实现的遍历写死在 storage.channels.* 上，pay_channels 的 key / app_secret /
// api_v3_key / private_key 全是明文 —— 一次 `select value from system_config`
// 就能拿走全部商户密钥与私钥（那是能直接发起退款的凭据）。
// 作用域表（scope.go）就是为这件事加的，这里锁住它真的覆盖到了。
func TestSealCoversPayChannels(t *testing.T) {
	withEncryptionKey(t)

	// 配置键的值就是数组（与 system_config 里存的一致），不是 {"pay_channels":[...]}。
	plain := json.RawMessage(`[
		{"id":"epay","name":"易支付","protocol":"epay","enabled":true,
		 "params":{"pay_address":"https://pay.example.com","pid":"1000","key":"EPAY_KEY"}},
		{"id":"wxpay","name":"微信","protocol":"wxpay","enabled":true,
		 "params":{"mch_id":"1600000001","api_v3_key":"01234567890123456789012345678901",
		           "private_key":"-----BEGIN PRIVATE KEY-----\nXX\n-----END PRIVATE KEY-----"}}
	]`)

	sealed, failed, err := SealSecrets("pay_channels", plain)
	if err != nil || len(failed) > 0 {
		t.Fatalf("封印失败: err=%v failed=%v", err, failed)
	}
	epay := payChannelParams(t, sealed, "epay")
	for _, k := range []string{"key"} {
		v, _ := epay[k].(string)
		if !strings.HasPrefix(v, sealedPrefix) {
			t.Fatalf("epay.%s 必须是密文，实际 %q", k, v)
		}
	}
	// 非凭据字段必须原样：加密 pay_address/pid 会让渠道构建与校验全部失效。
	if epay["pay_address"] != "https://pay.example.com" || epay["pid"] != "1000" {
		t.Fatalf("非凭据字段不得加密: %+v", epay)
	}
	wx := payChannelParams(t, sealed, "wxpay")
	for _, k := range []string{"api_v3_key", "private_key"} {
		v, _ := wx[k].(string)
		if !strings.HasPrefix(v, sealedPrefix) {
			t.Fatalf("wxpay.%s 必须是密文，实际 %q", k, v)
		}
	}
	if wx["mch_id"] != "1600000001" {
		t.Fatalf("mch_id 是商户号不是凭据，不应加密: %+v", wx)
	}
	// 数组元素的其他列（id/name/protocol）必须原样：它们是结构性字段。
	if !strings.Contains(string(sealed), `"id":"epay"`) || !strings.Contains(string(sealed), `"protocol":"wxpay"`) {
		t.Fatalf("结构字段被破坏了: %s", sealed)
	}

	// 回读必须还原明文，否则签名与验签会拿密文去算。
	back, _, err := UnsealSecrets("pay_channels", sealed)
	if err != nil {
		t.Fatalf("解封失败: %v", err)
	}
	if got, _ := payChannelParams(t, back, "epay")["key"].(string); got != "EPAY_KEY" {
		t.Fatalf("解封后应还原明文，实际 %q", got)
	}
	// 幂等：配置页「取打码值 → 原样提交」时会二次封印，不能变成密文的密文。
	twice, _, err := SealSecrets("pay_channels", sealed)
	if err != nil {
		t.Fatalf("二次封印失败: %v", err)
	}
	if string(twice) != string(sealed) {
		t.Fatalf("重复封印不得改变内容")
	}
}

// TestSealLeavesUnregisteredKeyUntouched 未登记的配置键原样返回。
//
// 「猜」比不加密更危险：给 email/sms 里的非密文字段套上密文，等于把配置页
// 变成一个永远解析失败的 JSON（而错误信息只会说「格式不合法」）。
// 要加密某个键，就在 scope.go 里显式登记一行。
func TestSealLeavesUnregisteredKeyUntouched(t *testing.T) {
	withEncryptionKey(t)
	in := `{"smtp_password":"p@ss","access_key":"AK"}`
	out, failed, err := SealSecrets("email", json.RawMessage(in))
	if err != nil || len(failed) > 0 {
		t.Fatalf("未登记键不应报错: err=%v failed=%v", err, failed)
	}
	if string(out) != in {
		t.Fatalf("未登记的键必须原样返回，实际 %s", out)
	}
}

// TestSecretConfigKeysAreDistinct 作用域表不得重复登记同一个键。
//
// 重复登记会让同一份 JSON 被连续封印两次（第二次因为前缀判断是幂等的，
// 但路径告警会重复计入 failed），最重要的是「一处配置两套处理」正是要消灭的东西。
func TestSecretConfigKeysAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range SecretConfigKeys() {
		if seen[k] {
			t.Fatalf("配置键 %s 在作用域表中重复登记", k)
		}
		seen[k] = true
	}
	for _, must := range []string{"storage", "pay_channels"} {
		if !seen[must] {
			t.Fatalf("%s 必须登记在凭据作用域里（否则其凭据会明文落库）", must)
		}
	}
}
