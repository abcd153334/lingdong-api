package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/netguard"
)

// 上游文档副本（agent_docs）的边界与容量。
//
// 为什么有两条上限（256KB 落库 / 2MB 抓取）：
// 抓取上限是"别把整站首页拖进来"的防线，落库上限是"AI 上下文里塞不下"的防线。
// 两者取同一个数字的后果是：抓 2MB 再拒绝，白花一次下载并且报错很晚才出现。
const (
	// MaxDocBytes 是单份文档落库上限（256KB 纯文本）。
	MaxDocBytes = 256 * 1024
	// MaxDocFetchBytes 是单次 URL 抓取的响应体上限（2MB），超了直接放弃。
	MaxDocFetchBytes = 2 << 20
	// DocPreviewBytes 是预览端点返回的前缀长度（8KB）。
	DocPreviewBytes = 8 * 1024
	// docFetchTimeout 是单次抓取的硬超时。
	docFetchTimeout = 15 * time.Second
)

var (
	ErrDocNotFound        = errors.New("agent: 文档不存在")
	ErrDocEmpty           = errors.New("agent: 文档内容为空")
	ErrDocTooLarge        = errors.New("agent: 文档内容超出上限")
	ErrDocTypeUnsupported = errors.New("agent: 文档类型不支持")
	ErrDocFetchFailed     = errors.New("agent: 文档抓取失败")
	// ErrDocBinary 表示内容看起来是二进制（PDF/Office 压缩包等）。
	//
	// 为什么要拒：paste 路径不做类型判断，静默存一段 `%PDF-1.7` 会让模型读到
	// 满屏乱码、还以为"这份文档没内容"。既要"不限制格式"，就必须把
	// "我读不出正文"显式说出来 —— 静默入库比拒绝更糟。
	ErrDocBinary = errors.New("agent: 文档看起来是二进制内容")
)

// DocInput 是新增文档的入参（粘贴或 URL 两种来源）。
type DocInput struct {
	Title      string
	SourceType string
	Content    string
	URL        string
}

// CreateDoc 落库一份文档副本，返回 (文档, 是否新建)。
//
// 同一份内容重复粘贴时**不新建**而是返回已有行（duplicate=false）：
// 运营常见的动作是把同一份厂商文档粘三遍，落三行会让 docs_read 的目录里
// 出现三条一模一样的标题，模型每次都读第一份，其余两份纯占上下文。
func CreateDoc(ctx context.Context, adminID uint64, in DocInput) (*model.AgentDoc, bool, error) {
	text := in.Content
	srcType := strings.TrimSpace(in.SourceType)
	if srcType == "" {
		if strings.TrimSpace(in.URL) != "" {
			srcType = "url"
		} else {
			srcType = "paste"
		}
	}
	if srcType != "paste" && srcType != "url" {
		return nil, false, ErrDocTypeUnsupported
	}
	title := strings.TrimSpace(in.Title)
	if srcType == "url" {
		raw := strings.TrimSpace(in.URL)
		if raw == "" {
			return nil, false, ErrDocEmpty
		}
		fetched, ct, err := fetchDoc(ctx, raw)
		if err != nil {
			return nil, false, err
		}
		// 粘贴内容与抓取内容同时存在时以粘贴为准（用户可能已手工精简过）。
		if strings.TrimSpace(text) == "" {
			text = fetched
		}
		if title == "" {
			title = titleFromURL(raw)
		}
		_ = ct
		in.URL = raw
	}
	text = normalizeText(text)
	if text == "" {
		return nil, false, ErrDocEmpty
	}
	if len(text) > MaxDocBytes {
		return nil, false, ErrDocTooLarge
	}
	if isProbablyBinary(text) {
		return nil, false, ErrDocBinary
	}
	if title == "" {
		title = titleFromContent(text)
	}
	if r := []rune(title); len(r) > 128 {
		title = string(r[:128])
	}
	sum := sha256.Sum256([]byte(text))
	hash := hex.EncodeToString(sum[:])

	var dup model.AgentDoc
	err := db.DB.WithContext(ctx).
		Where("sha256 = ? AND source_type = ?", hash, srcType).
		Order("id ASC").Limit(1).Find(&dup).Error
	if err != nil {
		return nil, false, err
	}
	if dup.ID != 0 {
		return &dup, false, nil
	}

	row := model.AgentDoc{
		Title:      title,
		SourceType: srcType,
		SourceURL:  strings.TrimSpace(in.URL),
		Content:    text,
		SHA256:     hash,
		SizeBytes:  len(text),
		CreatedBy:  adminID,
	}
	if err := db.DB.WithContext(ctx).Create(&row).Error; err != nil {
		return nil, false, err
	}
	return &row, true, nil
}

// ListDocs 列出文档副本（目录用，不含正文）。
func ListDocs(ctx context.Context) ([]model.AgentDoc, error) {
	var rows []model.AgentDoc
	// Select 掉 content：目录只需要标题与大小，正文动辄几十 KB，
	// 列表页一次拉全都等于把整库文档读进内存。
	if err := db.DB.WithContext(ctx).
		Select("id, title, source_type, source_url, sha256, size_bytes, created_by, created_at, deleted_at").
		Order("id DESC").Limit(200).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// GetDoc 取一份文档（含正文）。
func GetDoc(ctx context.Context, id uint64) (*model.AgentDoc, error) {
	var row model.AgentDoc
	if err := db.DB.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrDocNotFound
		}
		return nil, err
	}
	return &row, nil
}

// DeleteDoc 软删一份文档。
func DeleteDoc(ctx context.Context, id uint64) error {
	res := db.DB.WithContext(ctx).Delete(&model.AgentDoc{}, id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return ErrDocNotFound
	}
	return nil
}

// DocSection 是文档里的一个小节（标题 + 字节区间）。
//
// 给模型看的是这份目录，而不是正文：一份 256KB 的文档整份塞进上下文
// 既贵又会让模型"读完前面忘后面"，按需逐段取才可控。
type DocSection struct {
	Index int    `json:"index"`
	Title string `json:"title"`
	Level int    `json:"level"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

/* ------------------------------ 文档结构（与格式无关） ------------------------------ */

// 结构类型只回答"怎么读"，不回答"能不能读"：
// 任何文本都能读，没有标题的文档就按段落切 —— 这正是"素材不限制格式"的落地点。
const (
	DocKindHeadings = "headings" // Markdown ATX / Setext 标题
	DocKindOutline  = "outline"  // 编号 / 项目符号大纲
	DocKindJSON     = "json"     // JSON 键层级
	DocKindTable    = "table"    // CSV / TSV / 管道表格
	DocKindPlain    = "plain"    // 无结构：按空行分块
)

const (
	// maxDocSections 是目录的最大小节数（再多对定位没有帮助，只占上下文）。
	maxDocSections = 60
	// minDocTitleRunes 过滤掉 "1."、"-" 这类没有信息量的"标题"。
	minDocTitleRunes = 2
	// docPlainChunkMax 是无结构文档的分块大小。
	docPlainChunkMax = 2 * 1024
)

// DocProfile 是一份文档的结构画像。
type DocProfile struct {
	Kind     string       `json:"kind"`
	Sections []DocSection `json:"sections"`
	ReadHint string       `json:"read_hint"`
}

// DocProfileOf 生成/命中缓存的结构画像。
//
// 为什么要缓存：一轮编排里 buildDocCatalog 会对最多 5 份 256KB 文档出目录，
// 六套策略逐个试一遍 = 每轮几 MB 的扫描，首字延迟会肉眼可见。
// 缓存键用 (id, size)：行一旦建成正文就不再变，size 变化即视为换了一份。
func DocProfileOf(doc *model.AgentDoc) DocProfile {
	if doc == nil {
		return DocProfile{Kind: DocKindPlain, ReadHint: "没有正文"}
	}
	key := fmt.Sprintf("%d:%d", doc.ID, doc.SizeBytes)
	docProfileCache.mu.Lock()
	if p, ok := docProfileCache.items[key]; ok && time.Since(docProfileCache.at) < docProfileCacheTTL {
		docProfileCache.mu.Unlock()
		return p
	}
	docProfileCache.mu.Unlock()

	p := buildDocProfile(doc.Content)
	docProfileCache.mu.Lock()
	if time.Since(docProfileCache.at) >= docProfileCacheTTL {
		docProfileCache.items = map[string]DocProfile{}
		docProfileCache.at = time.Now()
	}
	docProfileCache.items[key] = p
	docProfileCache.mu.Unlock()
	return p
}

var (
	docProfileCacheTTL = 10 * time.Second
	docProfileCache    struct {
		mu    sync.Mutex
		at    time.Time
		items map[string]DocProfile
	}
)

func init() { docProfileCache.items = map[string]DocProfile{} }

// DocOutline 抽取文档目录（保留旧签名：预览端点与提示词目录都用它）。
//
// 现在是 DocProfileOf 的薄包装 —— 目录策略只允许有一条实现。
func DocOutline(doc *model.AgentDoc) []DocSection { return DocProfileOf(doc).Sections }

// buildDocProfile 按"命中即停"的顺序试六套策略。
func buildDocProfile(text string) DocProfile {
	if secs := finishSections(outlineATX(text), len(text)); len(secs) > 0 {
		return DocProfile{Kind: DocKindHeadings, Sections: secs,
			ReadHint: "有标题结构：按 section 下标读单节，或按 offset 通读"}
	}
	if secs := finishSections(outlineSetext(text), len(text)); len(secs) > 0 {
		return DocProfile{Kind: DocKindHeadings, Sections: secs,
			ReadHint: "有 Setext 标题（下划线式）：按 section 读单节"}
	}
	if secs := finishSections(outlineNumbered(text), len(text)); len(secs) > 0 {
		return DocProfile{Kind: DocKindOutline, Sections: secs,
			ReadHint: "编号/项目符号大纲：按 section 读，编号是原文里的层级"}
	}
	if p, ok := profileJSON(text); ok {
		return p
	}
	if p, ok := profileTable(text); ok {
		return p
	}
	return profilePlain(text)
}

// finishSections 补齐 End 并截断到上限。
func finishSections(secs []DocSection, total int) []DocSection {
	if len(secs) == 0 {
		return nil
	}
	if len(secs) > maxDocSections {
		secs = secs[:maxDocSections]
	}
	for i := range secs {
		if i+1 < len(secs) {
			secs[i].End = secs[i+1].Start
		} else {
			secs[i].End = total
		}
		secs[i].Index = i
	}
	return secs
}

// outlineATX 识别 Markdown ATX 标题（`# 标题`，`#` 后必须紧跟空格）。
func outlineATX(text string) []DocSection {
	var out []DocSection
	offset := 0
	for _, ln := range strings.Split(text, "\n") {
		lineLen := len(ln) + 1
		if strings.HasPrefix(ln, "#") {
			lvl := 0
			for lvl < len(ln) && ln[lvl] == '#' {
				lvl++
			}
			if lvl <= 6 && lvl < len(ln) && ln[lvl] == ' ' {
				title := strings.TrimSpace(ln[lvl:])
				// 显式标题不设最短长度："# A" 也是作者写的结构。
				if title != "" {
					out = append(out, DocSection{Title: title, Level: lvl, Start: offset})
				}
			}
		}
		offset += lineLen
	}
	return out
}

// outlineSetext 识别下划线式标题（标题行 + === / ---）。
func outlineSetext(text string) []DocSection {
	lines := strings.Split(text, "\n")
	var out []DocSection
	offset := 0
	offsets := make([]int, len(lines))
	for i, ln := range lines {
		offsets[i] = offset
		offset += len(ln) + 1
	}
	for i := 1; i < len(lines); i++ {
		underline := strings.TrimSpace(lines[i])
		if len(underline) < 3 {
			continue
		}
		lvl := 0
		switch {
		case strings.Trim(underline, "=") == "":
			lvl = 1
		case strings.Trim(underline, "-") == "":
			lvl = 2
		}
		if lvl == 0 {
			continue
		}
		title := strings.TrimSpace(lines[i-1])
		if title == "" {
			continue
		}
		// Start 指向标题行（不是下划线行），否则按节读会漏掉标题本身。
		out = append(out, DocSection{Title: title, Level: lvl, Start: offsets[i-1]})
	}
	return out
}

// outlineNumbered 识别编号/项目符号大纲：
// `1.` `1.2` `一、` `- ` 开头、行不长、缩进不深、条数够多。
func outlineNumbered(text string) []DocSection {
	var out []DocSection
	offset := 0
	for _, ln := range strings.Split(text, "\n") {
		lineLen := len(ln) + 1
		trimmed := strings.TrimLeft(ln, " \t")
		indent := len(ln) - len(trimmed)
		if indent <= 4 && len([]rune(trimmed)) <= 60 {
			for _, prefix := range []string{"- ", "* ", "+ "} {
				if strings.HasPrefix(trimmed, prefix) {
					title := strings.TrimSpace(trimmed[len(prefix):])
					if len([]rune(title)) >= minDocTitleRunes {
						out = append(out, DocSection{Title: title, Level: 1 + indent/2, Start: offset})
					}
					break
				}
			}
			if num, title, ok := splitNumbered(trimmed); ok {
				out = append(out, DocSection{Title: num + " " + title, Level: 1 + indent/2, Start: offset})
			}
		}
		offset += lineLen
	}
	// 少于 4 条不算大纲（两三条列表当成结构会让目录失去定位价值）。
	if len(out) < 4 {
		return nil
	}
	return out
}

// splitNumbered 解析 "1." / "1.2" / "一、" 这类前缀。
func splitNumbered(s string) (num, title string, ok bool) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i > 0 {
		j := i
		for j < len(s) && (s[j] == '.' || s[j] == ')' || strings.HasPrefix(s[j:], "、")) {
			if strings.HasPrefix(s[j:], "、") {
				j += len("、")
			} else {
				j++
			}
			for k := j; k < len(s) && s[k] >= '0' && s[k] <= '9'; k++ {
				j = k + 1
			}
		}
		if j < len(s) && (s[j] == ' ' || strings.HasPrefix(s[j:], "、")) {
			rest := strings.TrimSpace(strings.TrimLeft(s[j:], "、"))
			if len([]rune(rest)) >= minDocTitleRunes {
				return s[:i], rest, true
			}
		}
	}
	// 中文序号：一、二、三…
	if r := []rune(s); len(r) > 2 && strings.ContainsRune("一二三四五六七八九十", r[0]) && (r[1] == '、' || r[1] == '.') {
		rest := strings.TrimSpace(string(r[2:]))
		if len(rest) >= minDocTitleRunes {
			return string(r[:2]), rest, true
		}
	}
	return "", "", false
}

// profileJSON 识别 JSON 并把键层级当目录。
//
// Start 用 `"key":` 第一次出现的位置近似（对象键在文本里可能重复出现，
// 取首次出现即"该段的起点"最接近）。这条近似写进了方案，并由用例锁边界。
func profileJSON(text string) (DocProfile, bool) {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) < 2 || trimmed[0] != '{' || !json.Valid([]byte(trimmed)) {
		return DocProfile{}, false
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &top); err != nil {
		return DocProfile{}, false
	}
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []DocSection
	for _, k := range keys {
		start := strings.Index(text, `"`+k+`"`)
		if start < 0 {
			start = 0
		}
		out = append(out, DocSection{Title: k, Level: 1, Start: start})
	}
	// 数组首元素的键作为二级（只取第一个数组，避免目录爆炸）。
	for _, k := range keys {
		var arr []map[string]json.RawMessage
		if err := json.Unmarshal(top[k], &arr); err != nil || len(arr) == 0 {
			continue
		}
		sub := make([]string, 0, len(arr[0]))
		for kk := range arr[0] {
			sub = append(sub, kk)
		}
		sort.Strings(sub)
		for _, kk := range sub {
			if idx := strings.Index(text, `"`+kk+`"`); idx >= 0 {
				out = append(out, DocSection{Title: kk, Level: 2, Start: idx})
			}
		}
		break
	}
	return DocProfile{
		Kind:     DocKindJSON,
		Sections: finishSections(sortByStart(out), len(text)),
		ReadHint: "JSON：section 是键名。读接口定义时先找 endpoints/data 一类键，再按 offset 细读",
	}, true
}

// profileTable 识别 CSV / TSV / 管道表格：前三行列数一致即认为是表。
func profileTable(text string) (DocProfile, bool) {
	lines := strings.Split(text, "\n")
	candidates := make([]string, 0, 3)
	for _, ln := range lines {
		if strings.TrimSpace(ln) != "" {
			candidates = append(candidates, ln)
			if len(candidates) == 3 {
				break
			}
		}
	}
	if len(candidates) < 2 {
		return DocProfile{}, false
	}
	if cols := tableCols(candidates[0]); cols >= 2 && tableCols(candidates[1]) == cols {
		return DocProfile{
			Kind:     DocKindTable,
			Sections: []DocSection{{Title: "表格（共 " + fmt.Sprint(cols) + " 列）", Level: 1, Start: 0, End: len(text)}},
			ReadHint: "表格：首行是表头，按 offset 逐段读（一行一条记录）",
		}, true
	}
	return DocProfile{}, false
}

func tableCols(line string) int {
	switch {
	case strings.Contains(line, "\t"):
		return len(strings.Split(line, "\t"))
	case strings.Contains(line, "|"):
		return len(strings.Split(strings.Trim(line, "|"), "|"))
	default:
		return len(strings.Split(line, ","))
	}
}

// profilePlain 兜底：按空行切块。任何文本都能被定位，这是"不限制格式"的地板。
func profilePlain(text string) DocProfile {
	var out []DocSection
	offset := 0
	blockStart := 0
	blockBytes := 0
	flush := func(end int) {
		if blockBytes == 0 {
			return
		}
		out = append(out, DocSection{
			Title: fmt.Sprintf("第 %d 段", len(out)+1), Level: 1,
			Start: blockStart,
		})
		_ = end
		blockBytes = 0
	}
	for _, ln := range strings.Split(text, "\n") {
		lineLen := len(ln) + 1
		if strings.TrimSpace(ln) == "" {
			flush(offset)
			blockStart = offset + lineLen
		} else {
			if blockBytes == 0 {
				blockStart = offset
			}
			blockBytes += lineLen
			// 超长段落先切一刀：一块 2KB 是"一次读得完"的粒度。
			if blockBytes >= docPlainChunkMax {
				flush(offset)
				blockStart = offset + lineLen
			}
		}
		offset += lineLen
	}
	flush(offset)
	return DocProfile{
		Kind:     DocKindPlain,
		Sections: finishSections(out, len(text)),
		ReadHint: "无标题结构（纯文本/日志）：按 section 顺序读完，不要整份索要",
	}
}

func sortByStart(secs []DocSection) []DocSection {
	sort.SliceStable(secs, func(i, j int) bool {
		if secs[i].Start == secs[j].Start {
			return secs[i].Level < secs[j].Level
		}
		return secs[i].Start < secs[j].Start
	})
	return secs
}

/* ------------------------------ 预览与分段读取 ------------------------------ */

// DocPreview 返回文档前缀与目录（管理台预览用，也是 AI 拿到的第一手材料）。
type DocPreview struct {
	ID       uint64       `json:"id"`
	Title    string       `json:"title"`
	Size     int          `json:"size_bytes"`
	Text     string       `json:"text"`
	Sections []DocSection `json:"sections"`
	Kind     string       `json:"kind"`
	ReadHint string       `json:"read_hint"`
	// Truncated 表示只返回了前缀（前端据此显示"还有 N 字未显示"）。
	Truncated bool `json:"truncated"`
}

// PreviewDoc 取文档前 DocPreviewBytes 字节（按 UTF-8 边界截断）。
func PreviewDoc(ctx context.Context, id uint64) (*DocPreview, error) {
	doc, err := GetDoc(ctx, id)
	if err != nil {
		return nil, err
	}
	text := doc.Content
	trunc := false
	if len(text) > DocPreviewBytes {
		text = safeCut(text, DocPreviewBytes)
		trunc = true
	}
	prof := DocProfileOf(doc)
	return &DocPreview{
		ID: doc.ID, Title: doc.Title, Size: doc.SizeBytes,
		Text: text, Sections: prof.Sections, Kind: prof.Kind, ReadHint: prof.ReadHint,
		Truncated: trunc,
	}, nil
}

// DocChunkMax 是单次 docs_read 的正文上限。
//
// 它与工具结果上限（8KB）是配套的：取 16KB 再被 clipResult 砍到 4KB，
// 而 next_offset 已经前进 16KB —— 中间 12KB 模型从未看见，还自以为读过了。
const DocChunkMax = 6 * 1024

// DocReadInput 是一次分段读取的入参。
type DocReadInput struct {
	Offset int
	Limit  int
	// Section 与 Offset 二选一；HasSection 用来区分"section=0"与"没给"。
	Section    int
	HasSection bool
}

// DocReadResult 是一次分段读取的结果（offset/next_offset 都是**字节**）。
type DocReadResult struct {
	Text           string
	Start          int
	Next           int
	EOF            bool
	LimitEffective int
	LimitClamped   bool
	SectionUsed    int
	SectionTitle   string
	HasSection     bool
	// Kind 是文档结构类型（wrap 时写进分隔符，模型据此决定怎么读）。
	Kind string
}

// ErrDocSectionConflict 表示 offset 与 section 同时给了。
var ErrDocSectionConflict = errors.New("agent: offset 与 section 只能二选一")

// DocSectionRangeError 表示 section 下标越界（带可用范围）。
type DocSectionRangeError struct{ Count int }

func (e *DocSectionRangeError) Error() string {
	return fmt.Sprintf("section 下标越界（可用 0-%d）", e.Count-1)
}

// ReadDoc 按字节区间或小节区间取正文。
//
// 三条硬约束：
//  1. limit 不再"静默重置成 16KB"，超过上限会夹紧并回显 limit_clamped；
//  2. Next 永远等于"实际返回正文的末尾"，与 text 长度严格对应；
//  3. 一律按 UTF-8 边界对齐（按字节切会把汉字切成半个）。
func ReadDoc(ctx context.Context, id uint64, in DocReadInput) (*DocReadResult, error) {
	if in.HasSection && in.Offset > 0 {
		return nil, ErrDocSectionConflict
	}
	doc, err := GetDoc(ctx, id)
	if err != nil {
		return nil, err
	}
	total := len(doc.Content)
	start, end := 0, total
	prof := DocProfileOf(doc)
	res := &DocReadResult{Kind: prof.Kind, HasSection: in.HasSection}
	if in.HasSection {
		if in.Section < 0 || in.Section >= len(prof.Sections) {
			return nil, &DocSectionRangeError{Count: len(prof.Sections)}
		}
		sec := prof.Sections[in.Section]
		start, end = sec.Start, sec.End
		res.SectionUsed = in.Section
		res.SectionTitle = sec.Title
	} else if in.Offset > 0 {
		start = in.Offset
	}
	if start > total {
		start = total
	}
	start = alignDown(doc.Content, start)
	if end > total {
		end = total
	}

	limit := in.Limit
	if limit <= 0 {
		limit = DocChunkMax
	}
	if limit > DocChunkMax {
		limit = DocChunkMax
		res.LimitClamped = true
	}
	if start >= end {
		res.Start, res.Next, res.EOF = start, end, true
		res.LimitEffective = limit
		return res, nil
	}
	stop := alignDown(doc.Content, start+limit)
	if stop > end {
		stop = end
	}
	res.Text = doc.Content[start:stop]
	res.Start = start
	res.Next = stop
	res.EOF = stop >= end
	res.LimitEffective = limit
	return res, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func alignDown(s string, i int) int {
	if i <= 0 {
		return 0
	}
	if i >= len(s) {
		return len(s)
	}
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

// safeCut 把字符串截到不超过 n 字节（按 UTF-8 边界回退）。
func safeCut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:alignDown(s, n)]
}

// normalizeText 统一换行、去掉行尾空白与连续空行。
//
// 必须归一化再落库：同一份文档从网页抓一次、从粘贴来一次，
// 若不归一化会算出两个 hash，"已存在"的提示就永远不触发。
func normalizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, ln := range lines {
		ln = strings.TrimRight(ln, " \t")
		if ln == "" {
			blank++
			// 最多留一个空行：连续空行对模型没有信息量，只占 token。
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, ln)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// binaryControlRatio 是判定"这不是文本"的阈值。
const binaryControlRatio = 0.10

// isProbablyBinary 判断内容是不是"读不出正文的东西"。
//
// 只控制字符占比与 UTF-8 合法性：正常中文/JSON/CSV 的控制字符只有换行与制表
// （远低于 10%），而 PDF/Office 压缩包的字节流几乎全是控制字符与非法序列。
// 阈值取 10% 而不是 0：个别二进制文件开头是 ASCII 魔数（%PDF-、PK\x03\x04）。
func isProbablyBinary(s string) bool {
	if s == "" {
		return false
	}
	bad := 0
	for i := 0; i < len(s); {
		b := s[i]
		if b < utf8.RuneSelf {
			if b < 0x09 || (b > 0x0A && b < 0x20) || b == 0x7F {
				bad++
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			bad++
			i++
			continue
		}
		i += size
	}
	return float64(bad)/float64(len(s)) > binaryControlRatio
}

// titleFromContent 取首个非空行（截 64 字）作为标题。
func titleFromContent(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(strings.TrimLeft(ln, "#"))
		if ln != "" {
			if r := []rune(ln); len(r) > 64 {
				return string(r[:64])
			}
			return ln
		}
	}
	return "未命名文档"
}

// titleFromURL 取 URL 最后一段（无路径时退化为 host）。
func titleFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "未命名文档"
	}
	base := path.Base(strings.TrimRight(u.Path, "/"))
	if base == "" || base == "." || base == "/" {
		base = u.Host
	}
	if r := []rune(base); len(r) > 64 {
		return string(r[:64])
	}
	return base
}

// fetchDoc 抓取一份 URL 文档并转纯文本。
//
// 三道约束（缺一个就会被用来打内网）：
//  1. netguard 禁重定向 + 拨号期校验 IP（DNS rebinding 只在拨号期才看得见真实 IP）；
//  2. content-type 白名单（只收文本，避免把二进制当文档落库）；
//  3. 读时用 LimitReader 卡上限（Content-Length 不可信，可能是 -1 或撒谎）。
func fetchDoc(ctx context.Context, raw string) (string, string, error) {
	policy := netguard.PolicyFromEnv()
	if err := netguard.ValidateURL(raw, policy); err != nil {
		return "", "", err
	}
	reqCtx, cancel := context.WithTimeout(ctx, docFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, raw, nil)
	if err != nil {
		return "", "", ErrDocFetchFailed
	}
	req.Header.Set("User-Agent", "lingdong-agent-docs/1.0")
	req.Header.Set("Accept", "text/html,text/plain,application/json;q=0.9,*/*;q=0.1")
	resp, err := netguard.Client(docFetchTimeout, policy).Do(req)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrDocFetchFailed, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("%w: 上游返回 %d", ErrDocFetchFailed, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxDocFetchBytes+1))
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrDocFetchFailed, err)
	}
	if len(body) > MaxDocFetchBytes {
		return "", "", ErrDocTooLarge
	}
	ct := resp.Header.Get("Content-Type")
	base, _, _ := mime.ParseMediaType(ct)
	if ct != "" && !docTypeSupported(base) {
		return "", "", ErrDocTypeUnsupported
	}
	text := string(body)
	if base == "text/html" || base == "application/xhtml+xml" || looksHTML(text) {
		text = htmlToText(text)
	}
	return text, ct, nil
}

func docTypeSupported(mediaType string) bool {
	switch mediaType {
	case "", "text/plain", "text/html", "text/markdown", "text/x-markdown",
		"application/json", "application/xhtml+xml":
		return true
	}
	return strings.HasPrefix(mediaType, "text/")
}

func looksHTML(s string) bool {
	head := strings.ToLower(s)
	if i := strings.Index(head, "<"); i >= 0 && i < 512 {
		head = head[i:]
		return strings.HasPrefix(head, "<!doctype") || strings.HasPrefix(head, "<html") ||
			strings.Contains(head[:minInt(len(head), 512)], "</")
	}
	return false
}

var (
	// 整段丢弃的标签：内容不是正文（脚本/样式/SVG），保留下来会污染上下文。
	//
	// 必须一个标签一条正则：RE2 **不支持反向引用**，写成 `<(...)>...</\1>`
	// 会在 init 期直接 panic（整个进程起不来，而不是"少去一段标签"）。
	reDropBlocks = func() []*regexp.Regexp {
		tags := []string{"script", "style", "noscript", "svg", "canvas", "iframe", "template"}
		out := make([]*regexp.Regexp, 0, len(tags))
		for _, tag := range tags {
			out = append(out, regexp.MustCompile(`(?is)<`+tag+`[^>]*>.*?</`+tag+`>`))
		}
		return out
	}()
	// 标题：转成 Markdown 标题行，这样 DocOutline 能识别出小节。
	// 闭合标签故意写成 `</h[1-6]>` 而不是 `</h\1>`：RE2 没有反向引用。
	reHTMLHeading = regexp.MustCompile(`(?is)<h([1-6])[^>]*>(.*?)</h[1-6]\s*>`)
	reHTMLBreak   = regexp.MustCompile(`(?i)<br\s*/?>`)
	// 块级标签边界：转成换行，否则 `<p>a</p><p>b</p>` 会粘成 `ab`。
	reHTMLBlock = regexp.MustCompile(`(?i)</?(p|div|li|tr|table|section|article|header|footer|h[1-6]|ul|ol|pre|blockquote)[^>]*>`)
	reHTMLTag   = regexp.MustCompile(`(?s)<[^>]*>`)
	reMultiNL   = regexp.MustCompile(`\n{3,}`)
)

// htmlToText 把 HTML 转成近似 Markdown 的纯文本。
//
// 不用第三方库：这里只需要"能读"，不需要完美还原。真正要注意的是
// **丢弃** script/style —— 带进来的 JS 里往往有整段 JSON 与密钥，
// 既浪费上下文又可能把不该给模型看的东西带进去。
func htmlToText(s string) string {
	for _, re := range reDropBlocks {
		s = re.ReplaceAllString(s, "")
	}
	s = reHTMLHeading.ReplaceAllStringFunc(s, func(m string) string {
		sub := reHTMLHeading.FindStringSubmatch(m)
		if len(sub) != 3 {
			return m
		}
		lvl := int(sub[1][0] - '0')
		return "\n\n" + strings.Repeat("#", lvl) + " " +
			strings.TrimSpace(reHTMLTag.ReplaceAllString(sub[2], "")) + "\n\n"
	})
	s = reHTMLBreak.ReplaceAllString(s, "\n")
	s = reHTMLBlock.ReplaceAllString(s, "\n\n")
	s = reHTMLTag.ReplaceAllString(s, "")
	s = htmlUnescape(s)
	s = reMultiNL.ReplaceAllString(s, "\n\n")
	return s
}

// htmlUnescape 只处理最常见的五个实体（够读，不多写）。
func htmlUnescape(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	r := strings.NewReplacer(
		"&nbsp;", " ", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'", "&amp;", "&",
	)
	return r.Replace(s)
}
