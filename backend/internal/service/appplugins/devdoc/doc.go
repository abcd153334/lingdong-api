// Package devdoc 生成「应用插件」的**开发文档**（markdown）。
//
// 为什么由代码生成，而不是手写一份 md：
//   - 类型矩阵、上限、动作词表、错误码都在代码里。手写文档必然漂 —— 这份能力
//     已经漂过一次：一篇文档说不提供对外读端点，而同一篇文档的 §3.4 就在讲对外只读。
//   - 文档要能从插件管理页**直接下载**，它必须描述"当前部署真正支持什么"，
//     而不是"某次编辑时以为支持什么"。
//
// 分工：正文（prose.go）回答"为什么、怎么用"，由人写；表格（本文件）回答"有什么"，
// 一律从常量生成。`go run ./cmd/gen-plugin-doc -check` 在门禁里比对 docs/ 下的副本。
package devdoc

import (
	"fmt"
	"strings"

	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/service/appplugins"
)

// FileName 是下载时的文件名（也是 docs/ 下的文件名）。
const FileName = "应用插件开发手册.md"

// Markdown 渲染整篇文档。
//
// 结构固定为「正文常量 + 生成的表格」交替：每张表的**列含义与数据**都来自代码，
// 表前后的"为什么/怎么用"来自 prose.go。改顺序时同时改开头的目录。
func Markdown() string {
	var b strings.Builder
	b.WriteString(proseHeader)
	b.WriteString("```json\n" + appplugins.SampleManifest + "\n```\n")
	b.WriteString(proseQuickStartTail)
	b.WriteString(sectionTypes())
	b.WriteString(proseTypesTail)
	b.WriteString(sectionList())
	b.WriteString(proseListTail)
	b.WriteString(sectionActions())
	b.WriteString(proseActionsTail)
	b.WriteString(sectionAPI())
	b.WriteString(proseAPITail)
	b.WriteString(sectionErrors())
	b.WriteString(proseErrorsTail)
	b.WriteString(sectionLimits())
	b.WriteString(proseLimitsTail)
	b.WriteString("```json\n" + appplugins.SampleManifest + "\n```\n")
	return b.String()
}

// ---- 生成段：一眼能看出"这些数字来自代码" ----

func sectionTypes() string {
	rows := make([][]string, 0, len(appplugins.TypeSpecs))
	for _, s := range appplugins.TypeSpecs {
		sortable := "否"
		if s.CanSort {
			sortable = "是"
			if s.SortNeedsShort {
				sortable = fmt.Sprintf("是（maxLen ≤ %d）", appplugins.MaxUniqueLen)
			}
		}
		filter := "否"
		if s.FilterOp != "" {
			filter = filterOpLabel(s.FilterOp)
		}
		cap := "—"
		if s.MaxLenCap > 0 {
			cap = fmt.Sprint(s.MaxLenCap)
		}
		rows = append(rows, []string{
			"`" + s.Type + "`", typeCN(s.Type), s.Control, s.TableCell,
			validationText(s.Type), sortable, filter, cap,
		})
	}
	return mdTable(
		[]string{"type", "含义", "表单控件", "表格呈现", "写入校验", "可排序", "可筛选", "maxLen 上限"},
		rows,
	)
}

func sectionList() string {
	// 排序/筛选的方言来自 pagination.ParseSort 与 parseListQuery，两边同一套参数名。
	rows := [][]string{
		{"`keyword`", "按编号或业务唯一值模糊搜索（大小写不敏感，LIKE 元字符已转义）"},
		{"`sort`", "`<字段>:asc|desc`；字段必须在 `list.sort` 白名单里，否则 400"},
		{"`f_<字段>`", "筛选，字段必须在 `list.filters` 白名单里；算子由字段类型决定"},
		{"`page` / `page_size`", fmt.Sprintf("分页，`page_size` 上限 %d", maxPageSize())},
		{"`entity`", "数据对象 key（必填）"},
	}
	return mdTable([]string{"参数", "语义"}, rows)
}

func sectionActions() string {
	rows := [][]string{
		{"`create`", "新建记录（POST）"},
		{"`update`", "整条替换（PUT，带 `version` 乐观锁）"},
		{"`delete`", "删除单条（DELETE）"},
		{"`export`", "导出 CSV（受筛选与 5 万行上限约束）"},
		{"`batch_delete`", fmt.Sprintf("批量删除（≤%d 条/次，单事务）；依赖 `delete`，不额外授予删除权", appplugins.MaxBatchDelete)},
	}
	return mdTable([]string{"action", "开放的能力"}, rows)
}

func sectionAPI() string {
	rows := [][]string{
		{"`GET`", "`/api/admin/app-plugins`", "目录列表（`q` 搜索 key/name，精确命中排第一）"},
		{"`GET`", "`/api/admin/app-plugins/sample`", "取示例 manifest"},
		{"`GET`", "`/api/admin/app-plugins/" + FileName + "`", "本文档（markdown 下载）"},
		{"`GET`", "`/api/admin/app-plugins/:key`", "单个插件详情（描述 + 原始 manifest + 安装数）"},
		{"`PUT`", "`/api/admin/app-plugins/:key`", "导入/升级（body 见下）"},
		{"`PATCH`", "`/api/admin/app-plugins/:key/status`", "上架/下架目录"},
		{"`DELETE`", "`/api/admin/app-plugins/:key`", "删除目录项（仍有安装时 409）"},
		{"`GET`", "`/api/admin/apps/:id/plugins`", "某应用已安装的插件清单"},
		{"`POST`", "`/api/admin/apps/:id/plugins`", "安装到某应用"},
		{"`GET`", "`/api/admin/apps/:id/plugins/:key`", "安装详情（描述 + 配置 + 漂移 + 记录数）"},
		{"`PATCH`", "`/api/admin/apps/:id/plugins/:key`", "改配置/启停/对外只读"},
		{"`DELETE`", "`/api/admin/apps/:id/plugins/:key`", "卸载（保留数据）"},
		{"`DELETE`", "`/api/admin/apps/:id/plugins/:key/data`", "清除数据（要求已卸载）"},
		{"`GET`", "`/api/admin/apps/:id/plugins/:key/records`", "记录列表（搜索/筛选/排序/分页）"},
		{"`POST`", "`/api/admin/apps/:id/plugins/:key/records`", "新建记录"},
		{"`PUT`", "`/api/admin/apps/:id/plugins/:key/records/:rk`", "整条替换（带 `version`）"},
		{"`DELETE`", "`/api/admin/apps/:id/plugins/:key/records/:rk`", "删除单条"},
		{"`POST`", "`/api/admin/apps/:id/plugins/:key/records/batch-delete`", "批量删除"},
		{"`GET`", "`/api/admin/apps/:id/plugins/:key/records/export`", "导出 CSV（同一份筛选）"},
	}
	return mdTable([]string{"方法", "路径", "用途"}, rows)
}

func sectionErrors() string {
	type item struct {
		def errs.Def
		when string
	}
	items := []item{
		{errs.PluginNotFound, "目录里没有这个 key"},
		{errs.PluginManifestInvalid, "定义不合法：`data.problems[]` 逐条给出 `path` 与 `message`"},
		{errs.PluginAlreadyInstalled, "同一应用重复安装"},
		{errs.PluginNotInstalled, "尚未安装就访问数据"},
		{errs.PluginDisabled, "插件已下架或已停用，写入被关闭"},
		{errs.PluginEntityNotFound, "`entity` 不在当前定义里（升级删掉了对象）"},
		{errs.PluginRecordNotFound, "记录不存在（或已被删除）"},
		{errs.PluginRecordConflict, "业务唯一值重复（`unique` 字段撞了）"},
		{errs.PluginVersionConflict, "`version` 与库中不一致（他人先改了），需重取再改"},
		{errs.PluginLimitExceeded, "超出记录数/导出行数上限"},
		{errs.PluginInUse, "删除目录项时仍有应用安装着"},
	}
	rows := make([][]string, 0, len(items))
	for _, it := range items {
		rows = append(rows, []string{"`" + it.def.Code + "`", fmt.Sprint(it.def.HTTP), it.when})
	}
	return mdTable([]string{"code", "HTTP", "触发场景"}, rows)
}

func sectionLimits() string {
	rows := [][]string{
		{"manifest 体积", fmt.Sprintf("%d KB", appplugins.MaxManifestBytes/1024)},
		{"数据对象（entities）", fmt.Sprint(appplugins.MaxEntities)},
		{"每个对象的字段", fmt.Sprint(appplugins.MaxFieldsPerEntity)},
		{"配置字段（config）", fmt.Sprint(appplugins.MaxConfigFields)},
		{"列表列（list.columns）", fmt.Sprint(appplugins.MaxColumns)},
		{"可排序字段（list.sort）", fmt.Sprint(appplugins.MaxSortKeys)},
		{"可筛选字段（list.filters）", fmt.Sprint(appplugins.MaxFilters)},
		{"表单分段（form.sections）", fmt.Sprint(appplugins.MaxFormSections)},
		{"枚举取值", fmt.Sprint(appplugins.MaxEnumOptions)},
		{"string/text 的 maxLen", fmt.Sprint(appplugins.MaxStringLen)},
		{"link 的 maxLen", fmt.Sprint(appplugins.MaxLinkLen)},
		{"唯一字段 maxLen", fmt.Sprint(appplugins.MaxUniqueLen)},
		{"每个对象的记录数", fmt.Sprintf("默认 %d，可声明到 %d", appplugins.DefaultMaxRecords, appplugins.MaxRecordsCap)},
		{"单次导出", fmt.Sprint(appplugins.MaxExportRows)},
		{"单次批量删除", fmt.Sprint(appplugins.MaxBatchDelete)},
	}
	return mdTable([]string{"项", "上限"}, rows)
}

// ---- 小工具 ----

func mdTable(headers []string, rows [][]string) string {
	var b strings.Builder
	b.WriteString("| " + strings.Join(headers, " | ") + " |\n")
	sep := make([]string, len(headers))
	for i := range sep {
		sep[i] = "---"
	}
	b.WriteString("| " + strings.Join(sep, " | ") + " |\n")
	for _, r := range rows {
		b.WriteString("| " + strings.Join(r, " | ") + " |\n")
	}
	return b.String()
}

func filterOpLabel(op string) string {
	switch op {
	case appplugins.FilterOpEq:
		return "等值"
	case appplugins.FilterOpRange:
		return "区间"
	case appplugins.FilterOpContains:
		return "包含"
	}
	return op
}

func typeCN(t string) string {
	switch t {
	case appplugins.TypeString:
		return "短文本"
	case appplugins.TypeText:
		return "长文本"
	case appplugins.TypeInt:
		return "整数"
	case appplugins.TypeBool:
		return "开关"
	case appplugins.TypeEnum:
		return "枚举"
	case appplugins.TypeDate:
		return "日期"
	case appplugins.TypeLink:
		return "链接"
	}
	return t
}

// validationText 描述写入侧到底拦什么。这段文字必须与 normalizeField 的分支一一对应，
// 不一致就是文档骗人（作者会按文档写数据，然后收到看不懂的 422）。
func validationText(t string) string {
	switch t {
	case appplugins.TypeString:
		return "去首尾空格、不能含换行、不超 maxLen"
	case appplugins.TypeText:
		return "去首尾空格、允许换行、不超 maxLen"
	case appplugins.TypeInt:
		return "整数（|n| ≤ 2^53-1），可加 min/max"
	case appplugins.TypeBool:
		return "true / false"
	case appplugins.TypeEnum:
		return "必须是 options 之一"
	case appplugins.TypeDate:
		return "`YYYY-MM-DD`"
	case appplugins.TypeLink:
		return "`http://` 或 `https://` 开头"
	}
	return "—"
}

// maxPageSize 取自分页包本身：写死一个数字，改上限时文档就会骗人。
func maxPageSize() int { return pagination.MaxPageSize }
