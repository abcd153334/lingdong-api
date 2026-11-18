package controller

import (
	"regexp"
	"sort"
	"testing"

	"lingdong-api/internal/apicontract"
	"lingdong-api/internal/relay"
)

// sortWhitelists 把"契约里的哪个端点"对应到"哪个 handler 的白名单"。
//
// 新增一个支持 sort 的端点时，这里和 detail.go 的 Sortable 必须同时加：
// 只加契约 = 文档承诺可用但服务端 400；只加白名单 = 服务端偷偷支持了
// 一个文档里没有的能力（接入方永远发现不了，也就没人用）。
var sortWhitelists = []struct {
	key  string
	cols map[string]string
}{
	{"usage.list", relay.UsageSortColumns()},
	{"billing.transactions", transactionSortColumns},
	{"billing.orders_list", orderSortColumns},
	{"tasks.list", taskSortColumns},
	{"files.list", fileSortColumns},
}

// sqlColumnPattern 是白名单"值"的唯一合法形态。
//
// 白名单的值会被**直接拼进 ORDER BY**（pkg/sorting 只拼不参数化，因为列名
// 不能当占位符传）。所以它必须长得像一个裸列名：任何空格、分号、括号都意味着
// 有人在往 SQL 里塞别的东西。
var sqlColumnPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// TestSortableMatchesWhitelist 双向比对 apicontract.Detail.Sortable ↔ handler 白名单。
//
// 为什么这条门禁必须双向：单向（只查"声明的都有白名单"）会漏掉"白名单比文档
// 多"的情形，而那正是越权面 —— 多出来的一列可以被任意排序，配合分页就能把
// 一个本该看不到的字段变成"按它排序后从结果顺序里读出来"。
func TestSortableMatchesWhitelist(t *testing.T) {
	for _, c := range sortWhitelists {
		var decl []string
		found := false
		for _, e := range apicontract.Rules() {
			if e.Key == c.key {
				decl = e.Doc().Sortable
				found = true
				break
			}
		}
		if !found {
			t.Errorf("契约里没有端点 %q（Key 写错或已改名）", c.key)
			continue
		}

		declared := make(map[string]bool, len(decl))
		for _, f := range decl {
			declared[f] = true
		}

		var extra []string
		for name := range c.cols {
			if !declared[name] {
				extra = append(extra, name)
			}
		}
		sort.Strings(extra)
		if len(extra) > 0 {
			t.Errorf("%s：白名单里有契约未声明的字段 %v（服务端支持了文档没写的能力）", c.key, extra)
		}

		var missing []string
		for _, f := range decl {
			if _, ok := c.cols[f]; !ok {
				missing = append(missing, f)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("%s：契约声明了 %v，白名单里没有（文档承诺可用但会 400）", c.key, missing)
		}

		for name, col := range c.cols {
			if !sqlColumnPattern.MatchString(col) {
				t.Errorf("%s：字段 %q 映射到列 %q，不是合法裸列名", c.key, name, col)
			}
		}
	}
}

// TestEverySortableEndpointIsWired 保证"有 Sortable 的端点都被登记进了上面那张对应表"。
//
// 没有这条，新增一个声明了 Sortable 的端点后，上面那条门禁根本不会去看它
// （它只遍历已登记的 5 条），于是漏接线的端点会安静地存在：文档里有 sort，
// handler 不认，传了也不报错、只是顺序不变 —— 比 400 更难发现。
func TestEverySortableEndpointIsWired(t *testing.T) {
	wired := make(map[string]bool, len(sortWhitelists))
	for _, c := range sortWhitelists {
		wired[c.key] = true
	}
	for _, e := range apicontract.Rules() {
		if len(e.Doc().Sortable) > 0 && !wired[e.Key] {
			t.Errorf("%s 声明了 Sortable，但 sortWhitelists 里没有登记它的白名单", e.Key)
		}
	}
}
