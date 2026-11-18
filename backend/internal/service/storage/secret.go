package storage

import (
	"encoding/json"
	"sort"
	"strings"

	"go.uber.org/zap"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/pkg/crypto"
	"lingdong-api/internal/pkg/logger"
)

// sealedPrefix 标记一个字段值已经被 Fernet 加密。
//
// 带前缀而不是"看能否解密"：Fernet token 与随机明文在形态上不可区分，
// 靠尝试解密判断会把「密钥已轮换」误判成「这是明文」，于是把密文当 AK 发出去。
const sealedPrefix = "enc:v1:"

// IsCredentialField 判定字段名是否为访问凭据。
//
// 这是**唯一判据**（命名约定），不分后端也不分用途：云存储的 secret_key、
// 支付渠道的 app_secret / private_key / api_v3_key 都靠它自动命中，
// 不需要再去某个列表里补一行（漏补的表现是凭据明文静静地躺在库里）。
//
// 同一个判据还驱动管理台脱敏（systemconfig.maskSensitive）：
// 「加密落库」与「回显打码」必须是同一批字段，否则打码的字段没加密、
// 或者加密的字段被回显成打码占位符后再提交回来 —— 两种都会丢掉真实凭据。
func IsCredentialField(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return false
	}
	switch {
	case strings.Contains(n, "secret"), strings.Contains(n, "password"),
		strings.Contains(n, "private_key"):
		return true
	case strings.HasPrefix(n, "access_key"), strings.HasSuffix(n, "_api_key"),
		n == "api_v3_key", n == "key":
		return true
	case strings.HasSuffix(n, "_token"):
		return true
	}
	return false
}

// SealSecrets 把某个配置键下所有凭据字段加密后返回（幂等）。
//
// 幂等很重要：配置页会「取回打码值 → 原样提交」，还原后的值可能已经是密文；
// 不加前缀判断就会变成「密文的密文」，即使用正确主密钥也解不回来。
//
// 返回值 failed 是「加密失败的字段路径」：调用方**必须**据此中止保存，
// 因为失败时字段被置空（绝不落明文），照常写库等于把凭据清空。
func SealSecrets(key string, raw json.RawMessage) (json.RawMessage, []string, error) {
	return transformSecrets(key, raw, sealValue)
}

// UnsealSecrets 把某个配置键下所有凭据字段解密后返回（幂等）。
//
// 解密失败（换过 APP_ENCRYPTION_KEY）时把该字段**置空**并保留其余字段：
// 后续使用侧会报「缺少必填」并提示重新填写。
// 绝不能把密文当 AK/密钥继续用 —— 那会得到 403 鉴权失败或回调验签失败，
// 让人以为是权限/配置问题。
//
// 返回的 failed 是 `键.元素.字段` 形式的路径列表：调用方据此把
// 「凭据解不开」与「运营漏填」区分开（前者要检查主密钥，后者要重填）。
func UnsealSecrets(key string, raw json.RawMessage) (json.RawMessage, []string, error) {
	return transformSecrets(key, raw, unsealValue)
}

func sealValue(name, v string) (string, bool, bool) {
	if v == "" || strings.HasPrefix(v, sealedPrefix) {
		return v, false, false
	}
	master := crypto.FernetDerive(conf.EnvConfig().AppEncryptionKey)
	token, err := crypto.FernetEncrypt(master, []byte(v))
	if err != nil {
		// 加密失败**绝不落明文**：这是封印的唯一目的。置空并报 failed，
		// 由调用方中止本次保存；旧实现在这里保留原文并只打 Warn，
		// 等于「加密坏了就退回明文存」，且页面上看不出任何区别。
		logger.L().Error("凭据加密失败，已置空（本次保存将被拒绝，绝不落明文）",
			zap.String("field", name), zap.Error(err))
		return "", true, true
	}
	return sealedPrefix + token, true, false
}

func unsealValue(name, v string) (string, bool, bool) {
	if !strings.HasPrefix(v, sealedPrefix) {
		return v, false, false
	}
	master := crypto.FernetDerive(conf.EnvConfig().AppEncryptionKey)
	plain, err := crypto.FernetDecrypt(master, strings.TrimPrefix(v, sealedPrefix))
	if err != nil {
		logger.L().Error("凭据无法解密（APP_ENCRYPTION_KEY 可能已变更），请重新填写",
			zap.String("field", name), zap.Error(err))
		return "", true, true
	}
	return string(plain), true, false
}

// transformSecrets 是 Seal/Unseal 的共同实现：遍历该配置键**值**里的凭据容器。
//
// 入参 raw 是「该配置键的值」本身（storage 的对象 / pay_channels 的数组），
// 所以这里按 any 解码而不是 map —— 硬解成 map 会让数组形的键（pay_channels）
// 直接报「格式不合法」，而它其实完全合法。
//
// fn 返回 (新值, 是否改动, 是否失败)；失败的字段以 `键.元素.字段` 形式收集，
// 顺序排序以保证日志与测试稳定。
func transformSecrets(key string, raw json.RawMessage, fn func(name, v string) (string, bool, bool)) (json.RawMessage, []string, error) {
	if len(raw) == 0 {
		return raw, nil, nil
	}
	s, ok := scopeOf(key)
	if !ok {
		// 未登记 = 这个键里没有凭据，原样返回（不要用「猜」来兜底）。
		return raw, nil, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw, nil, err
	}
	if err := s.checkShape(v); err != nil {
		return raw, nil, err
	}
	targets := secretTargets(s, v)
	if len(targets) == 0 {
		return raw, nil, nil
	}
	changed := false
	var failed []string
	for _, t := range targets {
		for k, val := range t.obj {
			if !IsCredentialField(k) {
				continue
			}
			str, ok := val.(string)
			if !ok {
				continue
			}
			out, done, bad := fn(k, str)
			if bad {
				failed = append(failed, t.label+"."+k)
			}
			if done {
				t.obj[k] = out
				changed = true
			}
		}
	}
	sort.Strings(failed)
	if !changed {
		return raw, failed, nil
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw, failed, err
	}
	return out, failed, nil
}
