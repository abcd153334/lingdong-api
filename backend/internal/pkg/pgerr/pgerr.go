// Package pgerr 是 PostgreSQL 驱动错误判据的**唯一**实现。
//
// 为什么需要一个包：唯一约束冲突（SQLSTATE 23505）在本项目里出现在至少七处
// —— 注册、建索引、结算流水、下单幂等、套餐重名、任务绑定、应用 slug。
// 改造前每个包各写一份 `strings.Contains(err.Error(), "23505")`，其中三份
// 忘了 nil 判断（nil err 直接 panic）。判据分散的真实代价不是重复几行，
// 而是「有一处认 duplicate key 文案、另一处不认」这种差异只在线上偶发时暴露。
//
// 为什么不直接用 pgx 的 *pgconn.PgError.Code：本包在 internal/pkg 下，
// 引入具体驱动会让 pkg 层耦合驱动实现（pgx 目前还是 indirect 依赖），
// 而 pgx 的 stdlib 包装在 GORM 里已被 err.Error() 展平成文本。
// 字符串判据覆盖 23505 与 "duplicate key"（同一错误的两种呈现），
// 与驱动侧的 Code 判定等价；另外也认 GORM 抽象层翻译出的
// gorm.ErrDuplicatedKey —— 那是开启 TranslateError 后唯一能拿到的形态。
package pgerr

import (
	"errors"
	"strings"

	"gorm.io/gorm"
)

// IsUnique 判定是否为唯一约束冲突（SQLSTATE 23505）。
//
// 三种呈现都算：GORM 开启 TranslateError 时的 gorm.ErrDuplicatedKey、
// 驱动原文里的 SQLSTATE 23505、以及 PG 的 "duplicate key" 文案。
// 只认其中一种就是线上偶发「同一件事这处当幂等、那处当故障」的根因。
//
// 允许传 nil：调用点常在 `if err != nil && ...` 之外直接判，nil 必须返回 false。
func IsUnique(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "23505") || strings.Contains(s, "duplicate key")
}

// IsAlreadyExists 判定对象已存在（SQLSTATE 42P07，建表/建索引时并发启动会在
// 另一实例先建成功后命中）。同样允许传 nil。
func IsAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "42P07") || strings.Contains(s, "already exists")
}

// Unique 返回冲突的**索引名**（拿不到名字时返回空串 + true）。
//
// 为什么需要名字：文档站有 8 个唯一索引，每一处的「人话解释」都不同
// （key 重名 / 协议重复 / 同位置字段重名 / 同状态码变体重复…）。
// 只判「是 23505」再统一回 500，用户看到的是「服务器开小差」，
// 而真正该说的是「这个标识已被占用」。
//
// PG 的原文形如 `duplicate key value violates unique constraint "uq_doc_nodes_key"`；
// GORM 开启 TranslateError 时只剩 gorm.ErrDuplicatedKey（无名字），此时返回空名字。
func Unique(err error) (string, bool) {
	if !IsUnique(err) {
		return "", false
	}
	const marker = `constraint "`
	s := err.Error()
	i := strings.Index(s, marker)
	if i < 0 {
		return "", true
	}
	rest := s[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j <= 0 {
		return "", true
	}
	return rest[:j], true
}
