package sorting_test

import (
	"errors"
	"strings"
	"testing"

	"lingdong-api/internal/pkg/sorting"
)

// 一份典型白名单：键是**对外字段名**，值是 SQL 列名。
// 两者经常不同名（`amount` → `amount_micro`），所以不能让调用方只传 []string。
var cols = map[string]string{
	"created_at": "created_at",
	"amount":     "amount_micro",
}

func TestParseEmptyIsNotAnError(t *testing.T) {
	// "没传 sort" 是合法状态：默认顺序必须与引入本包之前完全一致，
	// 否则所有列表页会莫名其妙倒过来。
	o, err := sorting.Parse("", cols)
	if err != nil {
		t.Fatalf("空参数不应报错: %v", err)
	}
	if !o.IsZero() {
		t.Fatalf("空参数应返回零值，实际 %+v", o)
	}
	if got := o.Clause(); got != "id DESC" {
		t.Fatalf("零值的默认片段应为 id DESC，实际 %q", got)
	}
}

func TestParseDirection(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"created_at", "created_at ASC, id ASC"},
		{"+created_at", "created_at ASC, id ASC"},
		{"-created_at", "created_at DESC, id DESC"},
		// 空格必须被容忍：从 curl 里复制过来常带空格
		{"  -created_at  ", "created_at DESC, id DESC"},
	}
	for _, tc := range cases {
		o, err := sorting.Parse(tc.raw, cols)
		if err != nil {
			t.Fatalf("%q 解析失败: %v", tc.raw, err)
		}
		if got := o.Clause(); got != tc.want {
			t.Errorf("%q 的片段应为 %q，实际 %q", tc.raw, tc.want, got)
		}
	}
}

func TestClauseAlwaysTieBreaksOnID(t *testing.T) {
	// Postgres 不保证同值行的相对顺序，而分页是 OFFSET/LIMIT：
	// 少了次序键，翻页会重复或漏行 —— 只在有同值、且数据量够大时出现。
	for _, raw := range []string{"created_at", "-created_at", "amount", "-amount"} {
		o, err := sorting.Parse(raw, cols)
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if !strings.Contains(o.Clause(), ", id ") {
			t.Errorf("%q 的片段 %q 缺 id 次序键", raw, o.Clause())
		}
	}
}

func TestParseUsesColumnMapping(t *testing.T) {
	o, err := sorting.Parse("amount", cols)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// 对外叫 amount，SQL 列叫 amount_micro：用户输入永远不能直接当列名用。
	if o.Column != "amount_micro" {
		t.Fatalf("应映射成 amount_micro，实际 %q", o.Column)
	}
}

func TestParseRejectsUnknownField(t *testing.T) {
	_, err := sorting.Parse("password_hash", cols)
	if err == nil {
		t.Fatal("白名单外的字段必须报错（静默回落会让调用方以为排序生效了）")
	}
	if !errors.Is(err, sorting.ErrUnknownField) {
		t.Fatalf("应是 ErrUnknownField，实际 %v", err)
	}
	var fe *sorting.FieldError
	if !errors.As(err, &fe) {
		t.Fatalf("应能取出 FieldError，实际 %T", err)
	}
	// 错误文案要能让调用方自己改：给出可选项，且顺序稳定
	msg := fe.Error()
	for _, want := range []string{"password_hash", "amount", "created_at"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误文案缺 %q：%s", want, msg)
		}
	}
	if strings.Index(msg, "amount") > strings.Index(msg, "created_at") {
		t.Errorf("白名单应按字典序输出，实际 %s", msg)
	}
}

func TestParseRejectsMultiField(t *testing.T) {
	// 多字段需要每个字段各配方向与白名单项，而契约的 Sortable 是一维列表；
	// 静默取第一个会让 `sort=a,-b` 看起来生效了一半。
	for _, raw := range []string{"created_at,-amount", "a,b", ","} {
		_, err := sorting.Parse(raw, cols)
		if err == nil {
			t.Errorf("%q 应被拒绝", raw)
			continue
		}
		if !errors.Is(err, sorting.ErrTooManyFields) {
			t.Errorf("%q 应是 ErrTooManyFields，实际 %v", raw, err)
		}
	}
}

func TestParseRejectsBareSign(t *testing.T) {
	// `sort=-` 与 `sort=+` 解析出空字段名，不能当成"没传"混过去。
	for _, raw := range []string{"-", "+"} {
		if _, err := sorting.Parse(raw, cols); err == nil {
			t.Errorf("%q 应被拒绝", raw)
		}
	}
}

func TestParseEmptyWhitelist(t *testing.T) {
	// 白名单为空 = 不支持排序：任何字段都该被拒，`nil` map 也不能 panic。
	if _, err := sorting.Parse("created_at", nil); err == nil {
		t.Fatal("空白名单时应拒绝任何字段")
	}
	if o, err := sorting.Parse("", nil); err != nil || !o.IsZero() {
		t.Fatalf("空白名单 + 空参数仍应成功且为零值，实际 %+v %v", o, err)
	}
}

func TestParseRejectsNameOnlyInMapWithEmptyColumn(t *testing.T) {
	// 映射里出现空列名（手滑写错）时按未知字段处理：拼出 `  ASC` 是语法错误，
	// 但报错信息会指向 SQL，排查方向完全错了。
	if _, err := sorting.Parse("broken", map[string]string{"broken": ""}); err == nil {
		t.Fatal("空列名应被拒绝")
	}
}
