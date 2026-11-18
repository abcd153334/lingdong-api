package model

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// JSON 用于所有 JSONB 列（对象/数组/标量通用），供 system_config 及各 JSON 字段复用。
type JSON json.RawMessage

// NewJSON 将任意 Go 值序列化为 JSON。
func NewJSON(v any) (JSON, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return JSON(b), nil
}

// MustJSON 同 NewJSON，序列化失败时 panic（仅用于常量种子数据）。
func MustJSON(v any) JSON {
	j, err := NewJSON(v)
	if err != nil {
		panic(err)
	}
	return j
}

func (j JSON) Value() (driver.Value, error) {
	if j == nil {
		return nil, nil
	}
	return string(j), nil
}

// MarshalJSON 让 JSONB 列在响应体中原样输出 JSON（否则 []byte 底层类型会被标准库编码为 base64 字符串）。
func (j JSON) MarshalJSON() ([]byte, error) {
	if len(j) == 0 {
		return []byte("null"), nil
	}
	return j, nil
}

// UnmarshalJSON 让请求体里的对象/数组**原样**收进 JSONB 列。
//
// 为什么必须显式实现（这是个真实的接口缺陷）：
// JSON 的底层类型是 []byte，而 `type JSON json.RawMessage` **不会继承**
// json.RawMessage 的 UnmarshalJSON 方法（Go 的方法集不随底层类型走）。
// 标准库于是退回到 []byte 的默认规则 —— 按 **base64 字符串** 解析，
// 结果就是管理台提交 `spec_prices: {...}` / `endpoints: [...]` 一律
// 400「参数不合法」，而服务端自己写的种子数据能正常落库
// （它们只走 MarshalJSON 这一侧，不需要反序列化）。
//
// 字面量 null 归一成 nil，与 Scan 的 nil 分支保持一致：
// 存成 JSONB 的 'null' 与 SQL NULL 在 GORM 的 `spec_prices = null` 判断里是两种东西。
func (j *JSON) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*j = nil
		return nil
	}
	*j = append((*j)[:0], b...)
	return nil
}

func (j *JSON) Scan(value any) error {
	switch v := value.(type) {
	case nil:
		*j = nil
		return nil
	case []byte:
		*j = append((*j)[:0], v...)
		return nil
	case string:
		*j = JSON(v)
		return nil
	default:
		return fmt.Errorf("model.JSON: 无法从 %T 扫描", value)
	}
}

func (JSON) GormDataType() string { return "jsonb" }

func (JSON) GormDBDataType(db *gorm.DB, _ *schema.Field) string {
	if db.Dialector.Name() == "postgres" {
		return "JSONB"
	}
	return "JSON"
}
