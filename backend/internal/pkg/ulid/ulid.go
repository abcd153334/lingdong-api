package ulid

import (
	"github.com/oklog/ulid/v2"
)

// New 生成 26 位 Crockford Base32 大写的 ULID（供 uploads 主键使用）。
func New() string {
	return ulid.Make().String()
}
