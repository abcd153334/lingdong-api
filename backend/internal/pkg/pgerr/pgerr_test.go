package pgerr

import (
	"errors"
	"fmt"
	"testing"

	"gorm.io/gorm"
)

func TestIsUnique(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"23505", errors.New(`ERROR: duplicate key value violates unique constraint "uq_api_keys_hosted" (SQLSTATE 23505)`), true},
		{"duplicate key 文案", errors.New("duplicate key value violates unique constraint"), true},
		{"GORM 翻译后的哨兵", gorm.ErrDuplicatedKey, true},
		{"包装后仍是唯一冲突", fmt.Errorf("创建密钥: %w", errors.New("SQLSTATE 23505")), true},
		{"其他错误码", errors.New("SQLSTATE 42P01 relation does not exist"), false},
		{"业务错误", errors.New("余额不足"), false},
	}
	for _, c := range cases {
		if got := IsUnique(c.err); got != c.want {
			t.Fatalf("%s: IsUnique = %v, 期望 %v", c.name, got, c.want)
		}
	}
}

func TestIsAlreadyExists(t *testing.T) {
	if IsAlreadyExists(nil) {
		t.Fatal("nil 必须返回 false")
	}
	if !IsAlreadyExists(errors.New("SQLSTATE 42P07")) {
		t.Fatal("42P07 应判为已存在")
	}
	if !IsAlreadyExists(errors.New(`relation "users" already exists`)) {
		t.Fatal("already exists 文案应判为已存在")
	}
	// 唯一冲突不是「已存在」：两者必须分开，否则建索引会把 23505 当"已建好"跳过。
	if IsAlreadyExists(errors.New("SQLSTATE 23505")) {
		t.Fatal("23505 不能被当成已存在")
	}
}
