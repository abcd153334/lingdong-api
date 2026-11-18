package systemconfig

import (
	"encoding/json"
	"strings"
	"testing"
)

// validateStorage 是「配置页能选」与「运行期真能构建」之间的唯一闸门。
//
// 历史事故（2026-10-24）：管理台把 storage.active 存成一个**构建不出来**的值后配置页显示
// 「保存成功」，之后每一个任务的产物转存都失败（Succeed 的「全部产物失败」闸门 → 判失败 +
// 全额退款），平台却已经付了上游钱；错误只出现在任务详情里，配置页毫无提示。
// 所以这里的判据必须与运行期共用同一份 builders，不能另写一套「字段是否填全」的检查。

func storageVal(t *testing.T, s string) map[string]json.RawMessage {
	t.Helper()
	if !json.Valid([]byte(s)) {
		t.Fatalf("用例里的 JSON 非法: %s", s)
	}
	return map[string]json.RawMessage{"storage": json.RawMessage(s)}
}

// TestValidateStorageAcceptsAllSchemes 四种云端方案 + 本地磁盘在字段填全时都必须放行。
func TestValidateStorageAcceptsAllSchemes(t *testing.T) {
	cases := []struct{ name, val string }{
		{"本地磁盘", `{"active":"none","channels":{}}`},
		{"s3", `{"active":"s3","channels":{"s3":{"endpoint":"https://s3.example.com","bucket":"lingdong","access_key":"ak","secret_key":"sk"}}}`},
		{"minio", `{"active":"minio","channels":{"minio":{"endpoint":"https://minio.example.com","bucket":"lingdong","access_key":"ak","secret_key":"sk","path_style":"true"}}}`},
		{"oss", `{"active":"oss","channels":{"oss":{"endpoint":"oss-cn-hangzhou.aliyuncs.com","bucket":"lingdong","access_key_id":"ak","access_key_secret":"sk"}}}`},
		{"cos", `{"active":"cos","channels":{"cos":{"region":"ap-guangzhou","bucket":"lingdong-1250000000","secret_id":"id","secret_key":"sk"}}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := validateStorage(storageVal(t, c.val)); err != nil {
				t.Fatalf("%s 应校验通过，实际: %v", c.name, err)
			}
		})
	}
}

// TestValidateStorageRejectsBroken 只要「保存成功之后必然写不进对象」，就必须当场 400。
func TestValidateStorageRejectsBroken(t *testing.T) {
	cases := []struct{ name, val, want string }{
		{"未知方案", `{"active":"gcs","channels":{}}`, "不受支持"},
		{"s3 缺密钥", `{"active":"s3","channels":{"s3":{"endpoint":"https://s3.example.com","bucket":"lingdong"}}}`, "不可用"},
		{"s3 endpoint 非法", `{"active":"s3","channels":{"s3":{"endpoint":"not-a-url","bucket":"lingdong","access_key":"ak","secret_key":"sk"}}}`, "不可用"},
		// 运营可配的 endpoint 是半可信输入：填成环回必须在保存期被拒，
		// 否则后端会替任何人访问内网并把签名头一起发过去。
		{"oss endpoint 指向环回", `{"active":"oss","channels":{"oss":{"endpoint":"http://127.0.0.1:9000","bucket":"lingdong","access_key_id":"ak","access_key_secret":"sk"}}}`, "不可用"},
		{"oss 桶名非法", `{"active":"oss","channels":{"oss":{"endpoint":"oss-cn-hangzhou.aliyuncs.com","bucket":"b","access_key_id":"ak","access_key_secret":"sk"}}}`, "不可用"},
		{"cos 缺地域", `{"active":"cos","channels":{"cos":{"bucket":"lingdong-1250000000","secret_id":"id","secret_key":"sk"}}}`, "不可用"},
		{"配置不是对象", `[]`, "格式不合法"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateStorage(storageVal(t, c.val))
			if err == nil {
				t.Fatalf("%s 应被拒绝", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误文案应含 %q，实际: %v", c.want, err)
			}
		})
	}
}

// TestValidateStorageSkippedWhenAbsent 未提交 storage 键时零副作用（管理台分页签保存）。
func TestValidateStorageSkippedWhenAbsent(t *testing.T) {
	if err := validateStorage(map[string]json.RawMessage{"site": json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("未提交 storage 时不该校验，实际: %v", err)
	}
}
