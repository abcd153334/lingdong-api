package storage

import (
	"fmt"
	"strconv"
)

// 凭据封印的**作用域表**：哪些配置键下、哪个位置装着凭据。
//
// 为什么需要一张表：凭据散布在多个配置键下（云存储后端、支付渠道），
// 每加一处就复制一份遍历逻辑，复制的那份几乎必然漏掉某个细节
// （幂等前缀判断 / 解密失败置空 / 数组按 id 定位），而漏掉的表现都不会报错 ——
// 要么凭据明文静静躺在库里，要么密文被当成密钥发出去（上游 401）。
//
// 这里只声明「形状」，判据仍然是 IsCredentialField 的命名约定：
// 新增云厂商或支付协议时不需要改这张表。

// SecretScope 描述一个「装着凭据」的配置键。
//
// **注意入参是「该键的值」本身**，不是整个配置对象：
//
//	storage      值 = {"active":"oss","channels":{"oss":{<field>:<value>}}}
//	              → 容器在 Container="channels" 下，形状为「名字 → 对象」
//	pay_channels 值 = [{"id":"epay","params":{<field>:<value>}}, ...]
//	              → 值本身就是数组，凭据在元素的 Field="params" 子对象里
type SecretScope struct {
	// Key 是 system_config 的顶层键。
	Key string
	// Container 是值内部装凭据的容器键（空 = 值本身就是容器）。
	Container string
	// Array 为 true 表示容器是数组，否则是「名字 → 对象」的映射。
	Array bool
	// Field 是元素内装凭据的子对象键（数组形用；空表示元素本身就是那个对象）。
	Field string
	// NameField 是数组元素里作为路径标识的字段（日志与告警用）。
	NameField string
}

// secretScopes 是凭据位置的**唯一清单**。
//
// 漏登记的代价不是报错而是「明文落库」，所以新增任何带凭据的配置键时，
// 必须同时在这里加一行。
var secretScopes = []SecretScope{
	// 云存储后端：access_key_id / secret_key / secret_id 等。
	{Key: "storage", Container: "channels"},
	// 支付渠道：key / app_secret / api_v3_key / private_key / secret_key / webhook_secret。
	{Key: "pay_channels", Array: true, Field: "params", NameField: "id"},
}

// SecretConfigKeys 返回「含凭据」的配置键（装载与保存两侧据此逐键处理）。
func SecretConfigKeys() []string {
	out := make([]string, 0, len(secretScopes))
	for _, s := range secretScopes {
		out = append(out, s.Key)
	}
	return out
}

// scopeOf 按键取形状声明。
func scopeOf(key string) (SecretScope, bool) {
	for _, s := range secretScopes {
		if s.Key == key {
			return s, true
		}
	}
	return SecretScope{}, false
}

// checkShape 校验「配置键的值」顶层形状是否与声明一致。
//
// 为什么必须报错而不是「找不到容器就原样返回」：形状不对（storage 存成数组、
// pay_channels 存成对象）时静默放行，畸形 JSON 会直接落库，直到读取方解析失败
// 才暴露 —— 那时故障点已经离保存动作很远（表现是「存储不可用 / 支付方式全没了」）。
// 形状在保存期就该 400。
func (s SecretScope) checkShape(v any) error {
	if v == nil {
		return nil // null 视为「未配置」，与各读取方的回落口径一致
	}
	if s.Container == "" {
		if s.Array {
			if _, ok := v.([]any); !ok {
				return fmt.Errorf("%s 必须是数组", s.Key)
			}
			return nil
		}
		if _, ok := v.(map[string]any); !ok {
			return fmt.Errorf("%s 必须是对象", s.Key)
		}
		return nil
	}
	if _, ok := v.(map[string]any); !ok {
		return fmt.Errorf("%s 必须是对象", s.Key)
	}
	return nil
}

// secretTarget 是「一个装着凭据的对象」及其可读路径。
type secretTarget struct {
	label string
	obj   map[string]any
}

// secretTargets 从某个配置键的**值**里收集全部凭据容器。
//
// 只认声明的形状：形状不符（比如有人把 pay_channels 写成对象）时原地跳过，
// 让上层各自的解析错误分支去报 —— 在这里猜结构只会把格式错误变成静默丢弃。
func secretTargets(s SecretScope, v any) []secretTarget {
	if s.Container != "" {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[s.Container]
	}
	if s.Array {
		arr, ok := v.([]any)
		if !ok {
			return nil
		}
		out := make([]secretTarget, 0, len(arr))
		for i, ev := range arr {
			m, ok := ev.(map[string]any)
			if !ok {
				continue
			}
			// 路径用 id 而不是下标：调序/删增渠道时下标会漂移，日志与告警里的人名
			// 必须能直接对上管理台上的那张卡片。
			label := s.Key + "[" + strconv.Itoa(i) + "]"
			if id, ok := m[s.NameField].(string); ok && id != "" {
				label = s.Key + "." + id
			}
			container := m
			if s.Field != "" {
				sub, ok := m[s.Field].(map[string]any)
				if !ok {
					continue
				}
				container = sub
			}
			out = append(out, secretTarget{label: label, obj: container})
		}
		return out
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make([]secretTarget, 0, len(m))
	for name, ev := range m {
		em, ok := ev.(map[string]any)
		if !ok {
			continue
		}
		container := em
		if s.Field != "" {
			sub, ok := em[s.Field].(map[string]any)
			if !ok {
				continue
			}
			container = sub
		}
		out = append(out, secretTarget{label: s.Key + "." + name, obj: container})
	}
	return out
}
