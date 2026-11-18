package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// 本文件是**工具参数的唯一解析入口**。
//
// 为什么需要它：模型一次性手写几 KB、十几层嵌套的 JSON 时，最常见的失败是
// 括号数错。而原来的实现是裸 json.Unmarshal + 把 err.Error() 原样回给模型
// （"invalid character '{' looking for beginning of object key string"）——
// 没有位置、没有片段、没有成因判断，模型只能整份重写，连错 3 次撞上循环闸门。
//
// 这里做两件事，且只做这两件：
//  1. **无歧义修复**：去 BOM、去 markdown 围栏、削掉前后噪声。绝不替你补括号、
//     去尾逗号、改引号 —— 那会把"结构错"改成"结构对但内容错"，错误被推到更晚，
//     而且补过之后 offset 失去意义。
//  2. **可定位的失败**：offset（字节与字符双份）、行列、出错点片段、
//     未闭合栈（到哪一层、缺几层）、按成因分档的 hint。

// argSnippetRadius 是出错点前后各取多少字符（模型需要看到上下文，不是整份）。
const argSnippetRadius = 60

// argCode* 是参数类失败的四个码。前三个都算"模型可以自己改"，第四个要改配置。
const (
	argCodeInvalid  = "invalid_arguments"
	argCodeMissing  = "missing_arguments"
	argCodeTruncate = "output_truncated"
)

// Balance 是整份参数的括号统计（不是前缀）：模型据此判断"差几层"。
type Balance struct {
	CurlyOpen   int  `json:"curly_open"`
	CurlyClose  int  `json:"curly_close"`
	SquareOpen  int  `json:"square_open"`
	SquareClose int  `json:"square_close"`
	InString    bool `json:"in_string"`
	// NetCurly/NetSquare 是"还差几个闭合"（正数=少闭合，负数=多闭合）。
	NetCurly  int `json:"net_curly"`
	NetSquare int `json:"net_square"`
}

// Frame 是一个未闭合的容器，Path 是该容器在 JSON 里的位置（如 $.operations[0]）。
type Frame struct {
	Ch   string `json:"ch"`
	Path string `json:"path"`
	Line int    `json:"line"`
	Col  int    `json:"col"`
}

// ArgError 是参数解析失败的结构化回执。
//
// 这些键（offset/line/column/snippet/unclosed/hint）是**承诺给模型的契约**：
// 知识包 20-plan-shape 与 25-writing-json 里写着"按 line/column/snippet 只改那一处"，
// 少给一个键，那句话就变成空话。
type ArgError struct {
	Code       string
	Message    string
	ByteOffset int
	RuneOffset int
	Line       int
	Column     int
	Bytes      int
	Runes      int
	Balance    Balance
	Unclosed   []Frame
	Snippet    struct{ Before, At, After string }
	Hint       string
	Repairs    []string
	Tail       string
}

// Result 把 ArgError 摊成工具回执（含 ok:false，与 toolError 同形）。
func (e *ArgError) Result() map[string]any {
	out := map[string]any{
		"ok": false, "code": e.Code, "field": "arguments",
		"message": e.Message,
	}
	if e.ByteOffset > 0 {
		out["byte_offset"] = e.ByteOffset
		out["offset"] = e.RuneOffset
		out["line"] = e.Line
		out["column"] = e.Column
		out["bytes"] = e.Bytes
		out["runes"] = e.Runes
		out["balance"] = e.Balance
		snip := map[string]any{}
		if e.Snippet.Before != "" {
			snip["before"] = e.Snippet.Before
		}
		if e.Snippet.At != "" {
			snip["at"] = e.Snippet.At
		}
		if e.Snippet.After != "" {
			snip["after"] = e.Snippet.After
		}
		if len(snip) > 0 {
			out["snippet"] = snip
		}
		if len(e.Unclosed) > 0 {
			out["unclosed"] = e.Unclosed
		}
	}
	if e.Hint != "" {
		out["hint"] = e.Hint
	}
	if len(e.Repairs) > 0 {
		out["repairs"] = e.Repairs
	}
	if e.Tail != "" {
		out["tail"] = e.Tail
	}
	return out
}

// ParseToolArgs 解析工具参数（唯一入口）。
//
// truncated 表示上游已声明本轮输出被长度上限截断（finish_reason=length）：
// 那种情况下参数必然不完整，**不必再猜括号**，直接回 output_truncated，
// 否则模型会以为是自己写错，反复重写同样大小的 JSON。
func ParseToolArgs(raw string, truncated bool) (json.RawMessage, *ArgError) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, &ArgError{Code: argCodeMissing, Message: "本次工具调用没有参数"}
	}
	norm, repairs := normalizeToolArgs(raw)
	if truncated {
		e := analyzeArgs(norm, nil)
		e.Code = argCodeTruncate
		e.Repairs = repairs
		e.Message = "本轮输出被长度上限截断，参数不完整（这不是你写错了 JSON）"
		e.Tail = tailRunes(norm, 80)
		e.Hint = "不要原样重试：缩短内容或分块提交——先用 validate_draft(mode=\"shape\") 送 1–2 个 operations，语法过了再补齐。"
		return nil, e
	}
	var out json.RawMessage
	if err := json.Unmarshal([]byte(norm), &out); err != nil {
		e := analyzeArgs(norm, err)
		e.Code = argCodeInvalid
		e.Repairs = repairs
		return nil, e
	}
	return out, nil
}

// normalizeToolArgs 只做**无歧义**的修复，并返回做过的修复项（写进回执，便于观测）。
func normalizeToolArgs(raw string) (string, []string) {
	var repairs []string
	s := raw
	if strings.HasPrefix(s, "\ufeff") {
		s = strings.TrimPrefix(s, "\ufeff")
		repairs = append(repairs, "stripped_bom")
	}
	s = strings.TrimSpace(s)

	// ```json … ``` / ``` … ```
	if strings.HasPrefix(s, "```") {
		if nl := strings.IndexByte(s, '\n'); nl >= 0 {
			if end := strings.LastIndex(s, "```"); end > nl {
				s = strings.TrimSpace(s[nl+1 : end])
				repairs = append(repairs, "stripped_fence")
			}
		}
	}
	// 单行围栏：```{"a":1}```
	if strings.HasPrefix(s, "```") && strings.HasSuffix(s, "```") && len(s) > 6 {
		s = strings.TrimSpace(s[3 : len(s)-3])
		repairs = append(repairs, "stripped_fence_inline")
	}

	// 前后噪声：定位第一个完整 JSON 值，削掉它的前后。
	//
	// 只做**定位**不改造。两条保险（都是真踩过的）：
	//   1. 前缀里出现引号就不削 —— 说明那个 { 或 [ 是 payload 内部的一部分
	//      （典型是全角括号包着的 JSON：削掉前缀会把 `｛"a":[]｝` 变成合法的 `[]`，
	//      于是**错误输入被"修"成了另一个合法输入**，比直接报错危险得多）；
	//   2. 削完必须能扫出一个完整值，否则不动。
	if s != "" && s[0] != '{' && s[0] != '[' {
		if i := strings.IndexAny(s, "{[ "); i >= 0 && !strings.ContainsAny(s[:i], "\"'") {
			cand := strings.TrimSpace(s[i:])
			if _, ok := scanJSONValue(cand); ok {
				s = cand
				repairs = append(repairs, "trimmed_prologue")
			}
		}
	}
	if end, ok := scanJSONValue(s); ok && end < len(s) {
		// 后面还有非空白内容（模型附了说明文字）：只保留 JSON 本体。
		if strings.TrimSpace(s[end:]) != "" {
			s = s[:end]
			repairs = append(repairs, "trimmed_epilogue")
		}
	}
	return s, repairs
}

// scanJSONValue 用状态机找第一个完整 JSON 值的结束位置（不解析内容）。
//
// 只在括号配对平衡且字符串闭合时返回 ok：不平衡说明值本身没写完，
// 此时"截到最后一个 }"是错误的修复方向，必须交给报错去定位。
func scanJSONValue(s string) (int, bool) {
	if s == "" || (s[0] != '{' && s[0] != '[') {
		return 0, false
	}
	var stack []byte
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			stack = append(stack, c)
		case '}', ']':
			if len(stack) == 0 {
				return 0, false
			}
			open := stack[len(stack)-1]
			if (open == '{' && c != '}') || (open == '[' && c != ']') {
				return 0, false
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
}

// analyzeArgs 构造可定位的失败回执。
//
// err 为 nil 表示"还没解析就把判定做完了"（截断场景）。
func analyzeArgs(s string, err error) *ArgError {
	e := &ArgError{Bytes: len(s), Runes: utf8.RuneCountInString(s)}
	e.Balance = scanBalance(s)

	off := 0
	if se, ok := err.(*json.SyntaxError); ok && se.Offset > 0 {
		// Go 的 Offset 是**字节**（1-based），而人和模型都按字符数位置。
		// 只给字节，中文文档里会指到错的地方（本次故障两者差 252）。
		off = int(se.Offset) - 1
		if off > len(s) {
			off = len(s)
		}
	} else if err != nil {
		off = len(s)
	}
	e.ByteOffset = off
	e.RuneOffset = utf8.RuneCountInString(s[:off])
	e.Line, e.Column = lineColOf(s, off)
	e.Snippet = snippetAround(s, off)
	e.Unclosed = unclosedFrames(s, off)

	msg := "参数不是合法 JSON"
	if err != nil {
		msg += "：" + err.Error()
	}
	e.Message = msg
	e.Hint = argHintFor(s, err, e)
	return e
}

// scanBalance 统计整份的括号平衡（不判错，只给数字）。
func scanBalance(s string) Balance {
	var b Balance
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			b.CurlyOpen++
		case '}':
			b.CurlyClose++
		case '[':
			b.SquareOpen++
		case ']':
			b.SquareClose++
		}
	}
	b.InString = inStr
	b.NetCurly = b.CurlyOpen - b.CurlyClose
	b.NetSquare = b.SquareOpen - b.SquareClose
	return b
}

// lineColOf 把字节下标折成 1-based 行列（列按字符计，中文文档里才准）。
func lineColOf(s string, off int) (line, col int) {
	line, col = 1, 1
	for i := 0; i < off && i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == '\n' {
			line++
			col = 1
		} else {
			col++
		}
		i += size
	}
	return line, col
}

func snippetAround(s string, off int) struct{ Before, At, After string } {
	var out struct{ Before, At, After string }
	if off > len(s) {
		off = len(s)
	}
	// 以字符为窗口：先取前 N 个字符，再取后 N 个。
	before := []rune(s[:off])
	if len(before) > argSnippetRadius {
		before = before[len(before)-argSnippetRadius:]
	}
	rest := []rune(s[off:])
	at := ""
	if len(rest) > 0 {
		at = string(rest[0])
		rest = rest[1:]
	}
	if len(rest) > argSnippetRadius {
		rest = rest[:argSnippetRadius]
	}
	out.Before = "…" + string(before)
	out.At = at
	out.After = string(rest) + "…"
	return out
}

// unclosedFrames 返回"到出错点为止"还没闭合的容器（最深 3 层，由内向外的路径）。
//
// 只扫前缀：整份扫出来的平衡数字无法告诉模型"我现在卡在哪一层"，
// 而模型要改的恰恰就是那一层。
func unclosedFrames(s string, off int) []Frame {
	type frame struct {
		ch   byte
		path string
		line int
		col  int
		idx  int
	}
	var stack []*frame
	root := "$"
	line, col := 1, 1
	inStr, esc := false, false
	pendingKey := ""
	advance := func(c byte) {
		if c == '\n' {
			line++
			col = 1
			return
		}
		col++
	}
	for i := 0; i < off && i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			advance(c)
			continue
		}
		switch c {
		case '"':
			// 读完整个字符串，判断它是不是键（后面跟冒号）。
			j := i + 1
			var b strings.Builder
			for j < len(s) {
				if s[j] == '\\' && j+1 < len(s) {
					b.WriteByte(s[j])
					b.WriteByte(s[j+1])
					j += 2
					continue
				}
				if s[j] == '"' {
					break
				}
				b.WriteByte(s[j])
				j++
			}
			k := j + 1
			for k < len(s) && (s[k] == ' ' || s[k] == '\t' || s[k] == '\r' || s[k] == '\n') {
				k++
			}
			if k < len(s) && s[k] == ':' {
				pendingKey = b.String()
			}
			// 推进位置与行列
			for ; i <= j && i < len(s); i++ {
				advance(s[i])
			}
			i--
			continue
		case '{', '[':
			path := root
			if len(stack) > 0 {
				top := stack[len(stack)-1]
				if top.ch == '[' {
					path = fmt.Sprintf("%s[%d]", top.path, top.idx)
				} else if pendingKey != "" {
					path = top.path + "." + pendingKey
				}
			}
			stack = append(stack, &frame{ch: c, path: path, line: line, col: col})
			pendingKey = ""
		case '}', ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case ',':
			if len(stack) > 0 && stack[len(stack)-1].ch == '[' {
				stack[len(stack)-1].idx++
			}
			pendingKey = ""
		}
		advance(c)
	}
	out := make([]Frame, 0, 3)
	for i := len(stack) - 1; i >= 0 && len(out) < 3; i-- {
		out = append(out, Frame{Ch: string(stack[i].ch), Path: stack[i].path, Line: stack[i].line, Col: stack[i].col})
	}
	return out
}

// argHintRules 是 hint 的唯一定义处，按顺序匹配第一条。
//
// 分档的意义：同样是"净差 +1"，可能是"结尾少一个 }"（该补/该拆），
// 也可能是"中间多开一层 {"（该回去核对配对）。两类给同一句提示，
// 模型就会按错的方向修，而它每次修完都只差几个字符，看起来"快对了"。
var argHintRules = []struct {
	match func(s string, err error, e *ArgError) bool
	hint  func(e *ArgError) string
}{
	{
		match: func(s string, err error, e *ArgError) bool {
			return err != nil && strings.Contains(err.Error(), "unexpected end of JSON input") && e.Balance.InString
		},
		hint: func(e *ArgError) string {
			return "字符串没有闭合（缺一个 \"）。注意字符串里不能出现真实换行，要写成 \\n。"
		},
	},
	{
		match: func(s string, err error, e *ArgError) bool {
			return err != nil && strings.Contains(err.Error(), "unexpected end of JSON input")
		},
		hint: func(e *ArgError) string {
			inner := "最外层"
			if len(e.Unclosed) > 0 {
				inner = e.Unclosed[0].Path
			}
			return fmt.Sprintf("结尾被截断：还差 %d 个 } 与 %d 个 ]，最内层未闭合的是 %s。"+
				"如果本来就想写这么多，说明该分块了（mode=\"shape\"）；否则回到那一层补齐，不要整份重写。",
				maxInt(e.Balance.NetCurly, 0), maxInt(e.Balance.NetSquare, 0), inner)
		},
	},
	{
		match: func(s string, err error, e *ArgError) bool {
			return err != nil && strings.Contains(err.Error(), "looking for beginning of object key string")
		},
		hint: func(e *ArgError) string {
			inner := "上一层的对象"
			if len(e.Unclosed) > 0 {
				inner = e.Unclosed[0].Path
			}
			if e.Balance.NetCurly > 0 {
				return fmt.Sprintf("失败点仍然在 %s 内部：这个对象**多开了一层 {**（整份少 %d 个 }）。"+
					"回到该层逐层核对配对，不要直接在末尾补 }。", inner, e.Balance.NetCurly)
			}
			return fmt.Sprintf("对象里出现了非法键：键必须是双引号字符串，键值对之间要有逗号（看 %s 附近）。", inner)
		},
	},
	{
		match: func(s string, err error, e *ArgError) bool {
			return err != nil && (strings.Contains(err.Error(), "after object key:value pair") ||
				strings.Contains(err.Error(), "after array element"))
		},
		hint: func(e *ArgError) string {
			return "末尾多了逗号：JSON 不允许最后一个元素后面还有逗号。"
		},
	},
	{
		match: func(s string, err error, e *ArgError) bool {
			return strings.ContainsAny(s, "｛｝［］，：")
		},
		hint: func(e *ArgError) string {
			return "出现了全角标点（｛｝［］，：）：JSON 只接受半角。"
		},
	},
}

func argHintFor(s string, err error, e *ArgError) string {
	for _, r := range argHintRules {
		if r.match(s, err, e) {
			return r.hint(e)
		}
	}
	inner := "最内层"
	if len(e.Unclosed) > 0 {
		inner = e.Unclosed[0].Path
	}
	return fmt.Sprintf("先把 %s 这一层单独成行、缩进 2 格重写；不要整份重写。", inner)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func tailRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n:])
}

/* ------------------------------ 脱敏 ------------------------------ */

// sensitiveArgKeys 是参数里需要脱敏的键（唯一清单）。
//
// 小写比较：模型写 apiKey / api_key / API-KEY 都要盖住。
var sensitiveArgKeys = []string{
	"api_key", "apikey", "api-key", "token", "access_token", "refresh_token",
	"secret", "client_secret", "password", "authorization", "keys",
}

func isSensitiveKey(k string) bool {
	k = strings.ToLower(strings.TrimSpace(k))
	for _, s := range sensitiveArgKeys {
		if k == s {
			return true
		}
	}
	return false
}

// redactJSONLike 在"可能是坏 JSON"的文本上把敏感值换成 ***。
//
// 必须是词法级而不是 json.Unmarshal 后再 marshal：这段文本出现在**失败**路径上，
// 而失败路径上的参数往往正是坏 JSON —— 能解析的才脱敏，等于最该脱敏的时候不脱。
func redactJSONLike(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] != '"' {
			b.WriteByte(s[i])
			i++
			continue
		}
		// 读一个字符串字面量（含转义）。
		start := i
		j := i + 1
		for j < len(s) {
			if s[j] == '\\' && j+1 < len(s) {
				j += 2
				continue
			}
			if s[j] == '"' {
				break
			}
			j++
		}
		if j >= len(s) {
			b.WriteString(s[start:])
			break
		}
		key := s[start+1 : j]
		b.WriteString(s[start : j+1])
		i = j + 1
		if !isSensitiveKey(key) {
			continue
		}
		// key 之后的冒号
		k := i
		for k < len(s) && (s[k] == ' ' || s[k] == '\t' || s[k] == '\r' || s[k] == '\n') {
			k++
		}
		if k >= len(s) || s[k] != ':' {
			continue
		}
		b.WriteString(s[i : k+1])
		i = k + 1
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\r' || s[i] == '\n') {
			b.WriteByte(s[i])
			i++
		}
		// 吞掉值：字符串 / 数组 / 对象 / 裸值
		if i < len(s) && (s[i] == '"' || s[i] == '[' || s[i] == '{') {
			depth := 0
			for i < len(s) {
				c := s[i]
				if c == '"' {
					i++
					for i < len(s) {
						if s[i] == '\\' && i+1 < len(s) {
							i += 2
							continue
						}
						if s[i] == '"' {
							i++
							break
						}
						i++
					}
					if depth == 0 {
						break
					}
					continue
				}
				if c == '[' || c == '{' {
					depth++
				} else if c == ']' || c == '}' {
					depth--
					if depth == 0 {
						i++
						break
					}
				}
				i++
			}
		} else {
			for i < len(s) && s[i] != ',' && s[i] != '}' && s[i] != ']' && s[i] != '\n' {
				i++
			}
		}
		b.WriteString(`"***"`)
	}
	return b.String()
}

// SafeArgsBrief 生成给 UI/SSE 看的参数摘要（先脱敏再截断，顺序不可换）。
// SafeArgsBrief 生成给 UI/SSE 看的参数摘要：**先脱敏，再截断**（顺序不可换，
// 反过来会把密钥的前若干字节留在摘要里）。
//
// 返回长度保证 ≤ max 字节（clipText 的 "…" 会让结果超 3 字节，这里自己收口）。
func SafeArgsBrief(args string, max int) string {
	if max <= 0 {
		max = 200
	}
	s := redactJSONLike(args)
	if len(s) <= max {
		return s
	}
	return safeCut(s, max-3) + "…"
}

// sensitiveKeysIn 返回文本里出现过的敏感键名（测试与观测用）。
func sensitiveKeysIn(s string) []string {
	out := make([]string, 0, 2)
	seen := map[string]bool{}
	for _, k := range sensitiveArgKeys {
		if strings.Contains(strings.ToLower(s), `"`+k+`"`) && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
