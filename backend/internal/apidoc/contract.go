// 本文件是「对外文档契约」里与内容存储无关的三个常量：目录形状版本、
// 内容契约版本、以及节点 key 的生成规则。
//
// 为什么单独一个文件：它们各自原本住在 v6 的目录引擎与标题引擎里
// （`catalog.go` / `group_title.go`），而那两套引擎是「文档内容写在 Go 常量里」
// 时代的产物，已整体删除。这三个东西不同 —— 前端要拿形状版本对赌、
// v7 投影要下发契约版本、播种要按同一规则生成 key，都还有活人用。
package apidoc

import "strings"

// CatalogShape 是目录形状版本。前端按它校验，不匹配时给明确错误而不是渲染出一个空侧栏。
//
// 现行形态（v7）：板块 → 节点 → 接口页。三层，没有 Tab 分组，没有「格式组」。
// 这个数字只在**目录 JSON 的形状真的变了**时才加：
//   - 加一个纯附加字段（前端不看也不影响渲染）不要动它，否则每次加字段都得前后端同发；
//   - 改了既有字段的含义、删了字段、加了一层结构，必须加 —— 否则旧前端会静默渲染错。
const CatalogShape = 7

// ContractVersion 是对外文档内容的契约版本。
//
// 它描述的是**字段表语义**，不是软件版本：改了必填判定、枚举来源、
// 返回值路径的表达方式，就要加。旧值算出的 `facts_hash` 与新值必然不同，
// 正好让存量文案全部落进待复核队列。
//
// 2：`model` 字段的枚举来源从「单模型」改为「文档节点成员」，
// 且 facts_hash 由 FactsSignature 派生（多了能力位与参考素材计价位）。
// 3：v7 落库（九张内容表）。
// 4：字段新增「取值说明组」（Field.Groups）与「能力前置条件」（Field.Requires）；
// 图片端点删掉不成立的 stream / partial_images。
// 5：**内容与货架解耦**。size 改成一张不分模型的换算表；能力收窄从「库里模型
// 的能力并集」改为族静态声明（FamilyDoc.Caps）；投影期不再按模型过滤任何字段说明。
// 从这一版起，模型上下架不会改变任何一篇已发布文档的内容。
const ContractVersion = "5"

// Slug 把任意标题/厂商名压成可用作 URL 片段的小写标识。
//
// 规则：只保留 a-z 0-9 与 `.`，其余（含中文、空格、下划线）折叠成单个 `-`，
// 首尾的 `-` 与 `.` 去掉；全空时返回 "model"。
//
// 不保留中文：节点 key 会出现在 URL、目录键与 AI 工具参数里，
// 中文键在复制粘贴、日志检索与命令行里都会变成 `%E5%9B…`，运营根本认不出是哪一个。
func Slug(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.'
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-.")
	// 连续 `-` 已折叠，但 `.` 与 `-` 相邻会留下 `.-`，这里再收一次。
	for strings.Contains(out, ".-") {
		out = strings.ReplaceAll(out, ".-", "-")
	}
	for strings.Contains(out, "-.") {
		out = strings.ReplaceAll(out, "-.", "-")
	}
	if out == "" {
		return "model"
	}
	return out
}
