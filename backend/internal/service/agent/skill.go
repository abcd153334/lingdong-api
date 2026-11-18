package agent

import (
	"embed"
	"sort"
	"strconv"
	"strings"
)

// 知识包（SKILL）是**编译期**资产：用 go:embed 打进二进制，随版本一起发布。
//
// 为什么不落库：知识包描述的是"这套代码怎么用"，与代码同源才能保证不分叉。
// 落库会产生第二个真相源（代码改了、库里还是旧说明），而症状是模型照着旧说明
// 写出被拒绝的提案。
//
// 每篇的 front-matter 只有四个键（id/title/summary/priority），不引入第三方解析器。
//
//go:embed skill/*.md
var skillFS embed.FS

type skillDoc struct {
	ID       string
	Title    string
	Summary  string
	Priority int
	Body     string
}

// skillIndexMaxBytes 是提示词里索引的预算上限。
//
// 索引只是"目录"，正文按需 skill_read。超限时丢低优先级条目，
// **绝不截断正文**（半句话比没有更危险）。
//
// 2400 是"13 篇各一行摘要"（实测约 1990 字节）+ 余量：
// 低于实际值，末尾几篇会被整段挤掉，而模型看不到就永远不会读它
// —— 表现为"文档站文案它从不写""出错后不知道读 75"。
// SkillIndex 超限时是 break（该条及之后全部消失、**没有任何提示**），
// 所以余量必须留够；TestSkillIndexHasHeadroom 守着这条线。
// 改大时必须同步看 promptMaxBytes（提示词总上限 8KB），否则整段被丢弃。
const skillIndexMaxBytes = 2400

var skillDocs = loadSkills()

func loadSkills() []skillDoc {
	entries, err := skillFS.ReadDir("skill")
	if err != nil {
		return nil
	}
	out := make([]skillDoc, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		raw, err := skillFS.ReadFile("skill/" + e.Name())
		if err != nil {
			continue
		}
		out = append(out, parseSkill(string(raw)))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out
}

// parseSkill 解析 front-matter（缺失时回退到文件名，保证永远能读）。
func parseSkill(raw string) skillDoc {
	d := skillDoc{Body: strings.TrimSpace(raw)}
	text := raw
	if strings.HasPrefix(text, "---") {
		if end := strings.Index(text[3:], "\n---"); end >= 0 {
			head := text[3 : 3+end]
			body := text[3+end+4:]
			d.Body = strings.TrimSpace(body)
			for _, line := range strings.Split(head, "\n") {
				k, v, ok := strings.Cut(line, ":")
				if !ok {
					continue
				}
				k, v = strings.TrimSpace(k), strings.TrimSpace(v)
				switch k {
				case "id":
					d.ID = v
				case "title":
					d.Title = v
				case "summary":
					d.Summary = v
				case "priority":
					d.Priority, _ = strconv.Atoi(v)
				}
			}
		}
	}
	if d.ID == "" {
		d.ID = "skill"
	}
	if d.Title == "" {
		d.Title = d.ID
	}
	return d
}

// SkillIndex 是塞进提示词的知识包目录（id + 标题 + 一句话）。
func SkillIndex() string {
	var b strings.Builder
	for _, d := range skillDocs {
		line := "- " + d.ID + "：" + d.Title
		if d.Summary != "" {
			line += " —— " + d.Summary
		}
		if b.Len()+len(line)+1 > skillIndexMaxBytes {
			break
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// SkillLen 返回某篇的字节数（提示词索引与错误文案用）。
//
// 按**渲染后**长度算：示例占位符会展开成代码块，用原始长度会低估，
// 模型据此判断"一次能读完"就会读到半截。
func SkillLen(id string) int {
	for _, d := range skillDocs {
		if d.ID == id {
			return len(renderExamples(d.Body))
		}
	}
	return 0
}

// SkillIDs 返回全部 id（工具描述与测试用）。
func SkillIDs() []string {
	out := make([]string, 0, len(skillDocs))
	for _, d := range skillDocs {
		out = append(out, d.ID)
	}
	return out
}

// ReadSkill 按字节区间读一篇知识包。
//
// 与 docs_read 同形（offset/limit/next/eof）：模型对"分段读"已经有肌肉记忆，
// 换一套语义只会让它一次索要全文。
func ReadSkill(id string, offset, limit int) (text string, next int, more bool, ok bool) {
	var d *skillDoc
	for i := range skillDocs {
		if skillDocs[i].ID == id {
			d = &skillDocs[i]
			break
		}
	}
	if d == nil {
		return "", 0, false, false
	}
	if limit <= 0 {
		limit = 4 << 10
	}
	if limit > 8<<10 {
		limit = 8 << 10
	}
	if offset < 0 {
		offset = 0
	}
	// 示例占位符在**读取时**展开：知识包里不存第二份 JSON 副本。
	body := renderExamples(d.Body)
	if offset >= len(body) {
		return "", len(body), false, true
	}
	end := offset + limit
	if end >= len(body) {
		return body[offset:], len(body), false, true
	}
	// 按 UTF-8 边界回退，避免把汉字切一半。
	piece := safeCut(body[offset:end], len(body[offset:end]))
	return piece, offset + len(piece), true, true
}
