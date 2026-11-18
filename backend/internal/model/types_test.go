package model

import (
	"encoding/json"
	"testing"
)

// TestJSONUnmarshalKeepsRawBytes 把「请求体 → JSONB 列」这条链路钉死。
//
// 回归的是真实缺陷：`type JSON json.RawMessage` 不继承 json.RawMessage 的
// UnmarshalJSON，标准库遂按 []byte 的默认规则把输入当 **base64 字符串**，
// 于是 POST /api/admin/models 带 spec_prices / endpoints 一律 400「参数不合法」。
func TestJSONUnmarshalKeepsRawBytes(t *testing.T) {
	cases := []string{
		`{"kind":"per_image","default_price_quota":27778}`,
		`["chat/completions","responses"]`,
		`[]`,
		`{}`,
		`"plain"`,
		`42`,
	}
	for _, in := range cases {
		var j JSON
		if err := json.Unmarshal([]byte(in), &j); err != nil {
			t.Fatalf("反序列化 %s 失败: %v", in, err)
		}
		if string(j) != in {
			t.Fatalf("应原样保留 %s，实际 %s", in, string(j))
		}
		// 再序列化一次必须字节不变（否则「存进去」与「读出来」不是同一个东西）。
		out, err := json.Marshal(j)
		if err != nil {
			t.Fatalf("序列化 %s 失败: %v", in, err)
		}
		if string(out) != in {
			t.Fatalf("往返后应仍为 %s，实际 %s", in, string(out))
		}
	}
}

// TestJSONUnmarshalNullIsNil null 必须归一成 nil。
//
// 理由与 Scan 一致：JSONB 里存字面量 'null' 和存 SQL NULL 在
// `spec_prices IS NULL` / `spec_prices = 'null'` 判断里是两种结果，
// 保留 null 会让「未配置规格价」的模型被判定成「配置了个空值」。
func TestJSONUnmarshalNullIsNil(t *testing.T) {
	var j JSON
	if err := json.Unmarshal([]byte("null"), &j); err != nil {
		t.Fatalf("反序列化 null 失败: %v", err)
	}
	if j != nil {
		t.Fatalf("null 应归一为 nil，实际 %q", string(j))
	}
	// 空输入走 json.Unmarshal 会在进入本方法之前就报 "unexpected end of JSON input"，
	// 因此直接调方法本身验证归一逻辑（GORM 的 Scan 也可能以空切片进来）。
	if err := j.UnmarshalJSON(nil); err != nil {
		t.Fatalf("空输入失败: %v", err)
	}
	if j != nil {
		t.Fatalf("空输入应归一为 nil，实际 %q", string(j))
	}
}

// TestJSONRoundTripInsideStruct 结构体字段里的 JSON 字段也要能收对象/数组。
//
// 这是接口层面的真实形态：adminModelReq.SpecPrices / Endpoints 都是 JSON 类型，
// 单独测 JSON 通过、放进结构体里却退化成 base64 是曾经的真实表现。
func TestJSONRoundTripInsideStruct(t *testing.T) {
	type req struct {
		ID        string `json:"id"`
		SpecPrice JSON   `json:"spec_prices"`
		Endpoints JSON   `json:"endpoints"`
	}
	body := `{"id":"m1","spec_prices":{"kind":"per_task","per_task_price_quota":69444},` +
		`"endpoints":["audio/music/generations"]}`
	var r req
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("结构体反序列化失败: %v", err)
	}
	if r.ID != "m1" {
		t.Fatalf("id 应解析为 m1，实际 %q", r.ID)
	}
	var spec map[string]any
	if err := json.Unmarshal(r.SpecPrice, &spec); err != nil {
		t.Fatalf("spec_prices 不是合法对象: %v（原始 %q）", err, string(r.SpecPrice))
	}
	if spec["kind"] != "per_task" {
		t.Fatalf("spec_prices.kind 应为 per_task，实际 %v", spec["kind"])
	}
	var eps []string
	if err := json.Unmarshal(r.Endpoints, &eps); err != nil {
		t.Fatalf("endpoints 不是合法数组: %v（原始 %q）", err, string(r.Endpoints))
	}
	if len(eps) != 1 || eps[0] != "audio/music/generations" {
		t.Fatalf("endpoints 解析结果不对: %v", eps)
	}
}
