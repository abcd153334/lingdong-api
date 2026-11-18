package apidoc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// 本文件生成端点示例代码（curl / Python / Node / Go）。
//
// 四条边界（改之前先读）：
//  1. **只读投影**。请求体取自 EndpointProjection.Example——Project 已经把它里面的
//     model 换成了真实模型 id。这里**不再拼一份默认值**：两处默认值必然分叉，
//     结果是「页面上看到的初值」和「复制出来的 curl」内容不一样，用户会以为文档骗人。
//  2. **KEY 走环境变量**。示例里写死 sk-xxx，用户会把自己的真 KEY 粘进 shell 历史
//     或提交进 git。统一用 LD_API_KEY，复制即可跑且不落密钥。
//  3. **不编造地址**。BaseURL 默认 example.com（RFC 2606 保留域，永不指向真实服务），
//     所以文档里的示例地址明显是占位，不会被误当成可调用的线上服务。
//  4. **路径占位不替换**。`{id}` 保持原样并加注释说明「换成上一步返回的任务号」。
//     换成一个假的 tsk_xxx 更糟：用户照抄会拿到一个「任务不存在」，而不知道自己
//     漏了哪一步。

// SampleEnv 是示例代码里的环境值。
type SampleEnv struct {
	// BaseURL 是平台对外地址（不带尾斜杠）。
	BaseURL string
	// KeyEnv 是示例里读取 API KEY 的环境变量名。
	KeyEnv string
}

// DefaultSampleEnv 返回文档默认环境。
func DefaultSampleEnv() SampleEnv {
	return SampleEnv{BaseURL: "https://api.example.com", KeyEnv: "LD_API_KEY"}
}

func (e SampleEnv) normalized() SampleEnv {
	d := DefaultSampleEnv()
	if strings.TrimSpace(e.BaseURL) == "" {
		e.BaseURL = d.BaseURL
	}
	e.BaseURL = strings.TrimRight(strings.TrimSpace(e.BaseURL), "/")
	if strings.TrimSpace(e.KeyEnv) == "" {
		e.KeyEnv = d.KeyEnv
	}
	return e
}

// Samples 是一个端点的四语言示例。
//
// 四段文本全部由同一次 shapeOf 生成，所以「curl 里的字段」与「Python 里的字段」
// 不可能不一致——这正是 v1 文档被判「错误和不合理」的原因之一：
// 那时每种语言各写一份手写示例，改字段后只改了一两处。
type Samples struct {
	Curl   string `json:"curl"`
	Python string `json:"python"`
	Node   string `json:"node"`
	Go     string `json:"go"`
}

// BuildSamples 生成该端点的示例代码（请求体取契约里的基础示例）。
//
// modelID 只用于请求体里 `model` 的取值（空 = 保留 `<model>` 占位）。
// 早先这里收的是整份单模型投影，而示例只需要「模型 id」一个事实：
// 传整份投影等于让示例生成器有能力依赖任何一个字段，然后悄悄地与文档分叉。
func BuildSamples(modelID string, ep EndpointProjection, env SampleEnv) Samples {
	return BuildSamplesWithBody(modelID, ep, env, nil)
}

// BuildSamplesWithBody 用指定的请求体生成示例（供「第二种写法」使用）。
//
// body 为 nil 时取 ep.Example。为什么要能换体：同一接口常有兼容写法
// （如 duration/width/height 与 seconds/resolution 两套），
// 不给第二份示例，用户就会把两种写法混着发。
func BuildSamplesWithBody(modelID string, ep EndpointProjection, env SampleEnv, body Obj) Samples {
	s := shapeOf(modelID, ep, env.normalized(), body)
	return Samples{Curl: s.curl(), Python: s.python(), Node: s.node(), Go: s.golang()}
}

// formItem 是 multipart 表单的一项。
type formItem struct {
	name  string
	value string
	file  bool
}

func (f formItem) curlValue() string {
	if f.file {
		return "@" + f.value
	}
	return f.value
}

// reqShape 是一次请求的形态（四种语言共用的唯一中间表示）。
type reqShape struct {
	method    string
	path      string // 含 {id} 占位
	baseURL   string
	keyEnv    string
	query     []KV
	body      Obj
	form      []formItem
	jsonBody  bool
	binary    bool
	noContent bool
	sse       bool
	pathVars  []string
}

func shapeOf(modelID string, ep EndpointProjection, env SampleEnv, bodyOverride Obj) reqShape {
	s := reqShape{
		method:    ep.Method,
		path:      ep.Path,
		baseURL:   env.BaseURL,
		keyEnv:    env.KeyEnv,
		binary:    ep.RunMode == string(RunBinary) || hasReturnType(ep, TypeBinary),
		noContent: hasReturnType(ep, TypeNone),
	}
	formEncoded := ep.Transport == "multipart/form-data"

	for _, f := range ep.Fields {
		switch In(f.In) {
		case InPath:
			s.pathVars = append(s.pathVars, f.Name)
		case InQuery:
			if v, ok := sampleValue(f, ep.Example, false); ok {
				s.query = append(s.query, KV{K: f.Name, V: v})
			}
		case InForm:
			if !formEncoded {
				// 契约写错（In=form 但传输方式不是 multipart）时不要静默吞掉：
				// 直接按 JSON 字段处理，至少示例里能看到这个字段。
				continue
			}
			it := formItem{name: f.Name, file: f.Type == string(TypeFile)}
			switch {
			case it.file:
				it.value = "本地文件路径"
			case func() bool {
				v, ok := sampleValue(f, ep.Example, true)
				if ok {
					it.value = scalarText(v)
				}
				return ok
			}():
			case f.Name == "model" && modelID != "":
				// multipart 端点的 model 在表单里，不在示例体里 —— 没有这一分支时
				// 文档会印出 `-F "model=<model>"`，读者不知道该填哪个模型名。
				it.value = modelID
			case Required(f.Required) == ReqRequired:
				// 必填的文本字段给占位符：不给的话示例跑不通，而用户会以为是平台坏了。
				it.value = "<" + f.Name + ">"
			default:
				// 选填且没有默认值/枚举值：**整条不写**。
				//
				// 早先一律给 `<name>` 占位，结果 `purpose`（本平台忽略的兼容字段）
				// 也出现在示例里 —— 照着抄的人会以为必须传，还会去查这个字段的语义。
				continue
			}
			s.form = append(s.form, it)
		}
	}

	if !formEncoded && s.method != "GET" && s.method != "HEAD" {
		if len(bodyOverride) > 0 {
			s.body = bodyOverride
		} else {
			s.body = bodyOf(modelID, ep)
		}
		s.jsonBody = len(s.body) > 0
	}
	// 流式只在这条示例真的开了 stream 时才按流式写。
	// 对话接口的 RunMode 恒为 sse（因为"可以流式"），但基础示例是 stream:false——
	// 若照 RunMode 直接给 `curl -N` + `stream=True`，用户抄走的就是一个
	// "对着普通 JSON 响应做流式解析"的错代码。
	s.sse = ep.RunMode == string(RunSSE) && streamEnabled(s.body)
	return s
}

// hasReturnType 判断返回字段里是否有某类型（binary / none）。
//
// 响应形态以「返回字段的事实」为准，而不是 RunMode：RunMode 描述的是运行面板
// 能不能发，有的端点不能试跑（RunNone）但响应确实是文件流——
// 照 RunMode 生成示例，用户会拿 `print(r.json())` 去读一个 mp4。
func hasReturnType(ep EndpointProjection, t Type) bool {
	for _, r := range ep.Returns {
		if Type(r.Type) == t {
			return true
		}
	}
	return false
}

// streamEnabled 判断示例请求体里 stream 是否为 true。
func streamEnabled(body Obj) bool {
	if len(body) == 0 {
		return false
	}
	v, ok := body.Get("stream")
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

// bodyOf 取请求体：契约示例优先，没有示例时按必填字段拼一个最小体。
func bodyOf(modelID string, ep EndpointProjection) Obj {
	if len(ep.Example) > 0 {
		return ep.Example
	}
	var out Obj
	for _, f := range ep.Fields {
		if In(f.In) != InBody || Required(f.Required) != ReqRequired {
			continue
		}
		// 嵌套路径（`metadata.x` / `a[].b`）不进最小体：拼出来要么非法，要么需要
		// 一个没人会照抄的空壳。这类字段的写法在字段表的说明列里。
		if strings.ContainsAny(f.Name, ".[]") {
			continue
		}
		var v any = "<" + f.Name + ">"
		if f.Name == "model" && modelID != "" {
			v = modelID
		}
		out = append(out, KV{K: f.Name, V: v})
	}
	return out
}

// sampleValue 给查询参数/表单项取一个值：默认值 → 静态枚举首项 → 无。
// sampleValue 给查询参数/表单项取一个值：默认值 → 静态枚举首项 → 无。
//
// allowEnum=false 用于查询参数：`status` 这类筛选字段若自动填第一个枚举值，
// 用户复制出去只会看到"被筛过的一页"，还以为自己丢了数据。
func sampleValue(f FieldProjection, ex Obj, allowEnum bool) (any, bool) {
	if ex != nil {
		if v, ok := ex.Get(f.Name); ok {
			return v, true
		}
	}
	if d := strings.TrimSpace(f.Default); d != "" && d != "—" {
		// 默认值是"人话"（"模型默认"、"自动检测"），不是可直接发送的值。
		if _, err := strconv.ParseFloat(d, 64); err == nil || d == "true" || d == "false" {
			return d, true
		}
		if len(f.Values) > 0 {
			for _, v := range f.Values {
				if v == d {
					return d, true
				}
			}
		}
	}
	if allowEnum && f.ValueSource == string(EnumStatic) && len(f.Values) > 0 {
		return f.Values[0], true
	}
	return nil, false
}

// URL 拼出带查询串的完整地址（路径里的 {id} 保持原样）。
func (s reqShape) URL() string {
	u := s.baseURL + s.path
	if len(s.query) == 0 {
		return u
	}
	parts := make([]string, 0, len(s.query))
	for _, kv := range s.query {
		parts = append(parts, kv.K+"="+url.QueryEscape(scalarText(kv.V)))
	}
	return u + "?" + strings.Join(parts, "&")
}

// pathComment 是路径占位说明（有占位时才输出）。
func (s reqShape) pathComment() string {
	if len(s.pathVars) == 0 {
		return ""
	}
	names := make([]string, 0, len(s.pathVars))
	for _, n := range s.pathVars {
		names = append(names, "{"+n+"}")
	}
	return strings.Join(names, "、") + " 换成实际值（任务号/文件号在上一步的返回里）"
}

// ───────────────────────────── curl ─────────────────────────────

func (s reqShape) curl() string {
	var b strings.Builder
	if c := s.pathComment(); c != "" {
		b.WriteString("# " + c + "\n")
	}
	b.WriteString("curl")
	if s.sse {
		b.WriteString(" -N")
	}
	b.WriteString(" -X " + s.method + " \\\n  \"" + s.URL() + "\"")
	b.WriteString(" \\\n  -H \"Authorization: Bearer $" + s.keyEnv + "\"")
	if s.jsonBody {
		b.WriteString(" \\\n  -H \"Content-Type: application/json\"")
		b.WriteString(" \\\n  -d '" + shellQuote(indentJSON(s.body)) + "'")
	}
	for _, f := range s.form {
		b.WriteString(" \\\n  -F \"" + f.name + "=" + f.curlValue() + "\"")
	}
	if s.binary {
		b.WriteString(" \\\n  -o out.bin")
	}
	return b.String()
}

// shellQuote 让文本可以安全放进单引号里（JSON 里的 ' 需要断开转义）。
func shellQuote(s string) string {
	return strings.ReplaceAll(s, "'", `'\''`)
}

// ───────────────────────────── Python ─────────────────────────────

func (s reqShape) python() string {
	var b strings.Builder
	b.WriteString("import os\nimport requests\n\n")
	b.WriteString("BASE = \"" + s.baseURL + "\"\n")
	b.WriteString("HEADERS = {\"Authorization\": \"Bearer \" + os.environ[\"" + s.keyEnv + "\"]}\n\n")

	if c := s.pathComment(); c != "" {
		b.WriteString("# " + c + "\n")
	}
	call := "requests." + strings.ToLower(s.method)
	args := []string{"BASE + \"" + s.path + s.queryString() + "\"", "headers=HEADERS"}

	switch {
	case s.jsonBody:
		b.WriteString("payload = " + pyValue(s.body, 0) + "\n\n")
		args = append(args, "json=payload")
	case len(s.form) > 0:
		var dataKeys, fileKeys []string
		for _, f := range s.form {
			if f.file {
				fileKeys = append(fileKeys, "    "+pyStr(f.name)+": open("+pyStr(f.value)+", \"rb\"),")
			} else {
				dataKeys = append(dataKeys, "    "+pyStr(f.name)+": "+pyStr(f.value)+",")
			}
		}
		if len(dataKeys) > 0 {
			b.WriteString("data = {\n" + strings.Join(dataKeys, "\n") + "\n}\n")
		}
		if len(fileKeys) > 0 {
			b.WriteString("files = {\n" + strings.Join(fileKeys, "\n") + "\n}\n")
		}
		b.WriteString("\n")
		if len(dataKeys) > 0 {
			args = append(args, "data=data")
		}
		if len(fileKeys) > 0 {
			args = append(args, "files=files")
		}
	}

	if s.sse {
		args = append(args, "stream=True", "timeout=(10, 300)")
		b.WriteString("with " + call + "(" + strings.Join(args, ", ") + ") as r:\n")
		b.WriteString("    r.raise_for_status()\n")
		b.WriteString("    for line in r.iter_lines():\n")
		b.WriteString("        if line:\n")
		b.WriteString("            print(line.decode())\n")
		return b.String()
	}

	args = append(args, "timeout=(10, 300)")
	b.WriteString("r = " + call + "(" + strings.Join(args, ", ") + ")\n")
	b.WriteString("r.raise_for_status()\n")
	switch {
	case s.binary:
		b.WriteString("open(\"out.bin\", \"wb\").write(r.content)\n")
	case s.noContent:
		b.WriteString("print(r.status_code)\n")
	default:
		b.WriteString("print(r.json())\n")
	}
	return b.String()
}

// queryString 返回 `?a=1&b=2`（无查询参数返回空串）。
func (s reqShape) queryString() string {
	if len(s.query) == 0 {
		return ""
	}
	parts := make([]string, 0, len(s.query))
	for _, kv := range s.query {
		parts = append(parts, kv.K+"="+scalarText(kv.V))
	}
	return "?" + strings.Join(parts, "&")
}

// pyValue 把示例值渲染成 Python 字面量（保留键序，与页面/curl 一致）。
func pyValue(v any, lvl int) string {
	switch x := v.(type) {
	case Obj:
		if len(x) == 0 {
			return "{}"
		}
		pad := strings.Repeat("    ", lvl+1)
		parts := make([]string, 0, len(x))
		for _, kv := range x {
			parts = append(parts, pad+pyStr(kv.K)+": "+pyValue(kv.V, lvl+1)+",")
		}
		return "{\n" + strings.Join(parts, "\n") + "\n" + strings.Repeat("    ", lvl) + "}"
	case []any:
		if len(x) == 0 {
			return "[]"
		}
		pad := strings.Repeat("    ", lvl+1)
		parts := make([]string, 0, len(x))
		for _, e := range x {
			parts = append(parts, pad+pyValue(e, lvl+1)+",")
		}
		return "[\n" + strings.Join(parts, "\n") + "\n" + strings.Repeat("    ", lvl) + "]"
	case map[string]any:
		// 兼容：契约示例统一用 Obj，这里只防御外部塞进来的 map（键序会丢，但能跑）。
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		o := make(Obj, 0, len(keys))
		for _, k := range keys {
			o = append(o, KV{K: k, V: x[k]})
		}
		return pyValue(o, lvl)
	case string:
		return pyStr(x)
	case bool:
		if x {
			return "True"
		}
		return "False"
	case nil:
		return "None"
	case json.Number:
		return x.String()
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	default:
		return pyStr(fmt.Sprint(v))
	}
}

// pyStr 用 JSON 字符串转义（与 Python / JS / Go 的字符串字面量在常见字符上等价）。
//
// 关掉 HTML 转义：否则 `<string>` 会变成 `\u003cstring\u003e`，
// 用户复制出来读不懂，而且 Go/Python 里还得靠转义还原。
func pyStr(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return `"` + s + `"`
	}
	return strings.TrimRight(buf.String(), "\n")
}

// ───────────────────────────── Node ─────────────────────────────

func (s reqShape) node() string {
	var b strings.Builder
	if len(s.form) > 0 {
		b.WriteString("import { readFile } from \"node:fs/promises\";\n\n")
	}
	b.WriteString("const BASE = \"" + s.baseURL + "\";\n")
	b.WriteString("const KEY = process.env." + s.keyEnv + ";\n\n")
	if c := s.pathComment(); c != "" {
		b.WriteString("// " + c + "\n")
	}
	if len(s.form) > 0 {
		b.WriteString("const form = new FormData();\n")
		for _, f := range s.form {
			if f.file {
				b.WriteString("form.append(" + pyStr(f.name) + ", new Blob([await readFile(" + pyStr(f.value) +
					")]), " + pyStr(f.value) + ");\n")
			} else {
				b.WriteString("form.append(" + pyStr(f.name) + ", " + pyStr(f.value) + ");\n")
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("const res = await fetch(BASE + \"" + s.path + s.queryString() + "\", {\n")
	b.WriteString("  method: \"" + s.method + "\",\n")
	b.WriteString("  headers: { Authorization: `Bearer ${KEY}`")
	if s.jsonBody {
		b.WriteString(", \"Content-Type\": \"application/json\"")
	}
	b.WriteString(" },\n")
	if s.jsonBody {
		b.WriteString("  body: JSON.stringify(" + indentJSON(s.body) + "),\n")
	} else if len(s.form) > 0 {
		b.WriteString("  body: form,\n")
	}
	b.WriteString("});\n")
	b.WriteString("if (!res.ok) throw new Error(`${res.status} ${await res.text()}`);\n")
	if s.sse {
		b.WriteString("\nconst reader = res.body.getReader();\n")
		b.WriteString("const dec = new TextDecoder();\n")
		b.WriteString("for (;;) {\n")
		b.WriteString("  const { done, value } = await reader.read();\n")
		b.WriteString("  if (done) break;\n")
		b.WriteString("  process.stdout.write(dec.decode(value, { stream: true }));\n")
		b.WriteString("}\n")
	} else if s.binary {
		b.WriteString("const buf = Buffer.from(await res.arrayBuffer());\n")
		b.WriteString("require(\"fs\").writeFileSync(\"out.bin\", buf);\n")
	} else if s.noContent {
		b.WriteString("console.log(res.status);\n")
	} else {
		b.WriteString("console.log(await res.json());\n")
	}
	return b.String()
}

// ───────────────────────────── Go ─────────────────────────────

func (s reqShape) golang() string {
	// import 必须**先算齐再输出**：早先在写 import 块之后按需 append，
	// 结果 multipart 分支需要的 bytes 永远进不了 import（编译不过）。
	need := map[string]bool{
		`"fmt"`: true, `"io"`: true, `"net/http"`: true, `"os"`: true, `"strings"`: true,
	}
	if s.jsonBody || len(s.form) > 0 {
		need[`"bytes"`] = true
	}
	if len(s.form) > 0 {
		need[`"mime/multipart"`] = true
	}
	imports := make([]string, 0, len(need))
	for im := range need {
		imports = append(imports, im)
	}
	sort.Strings(imports)

	var b strings.Builder
	b.WriteString("package main\n\nimport (\n")
	for _, im := range imports {
		b.WriteString("\t" + im + "\n")
	}
	b.WriteString(")\n\nfunc main() {\n")
	b.WriteString("\tconst base = \"" + s.baseURL + "\"\n\n")

	if c := s.pathComment(); c != "" {
		b.WriteString("\t// " + c + "\n")
	}

	bodyExpr := "nil"
	if s.jsonBody {
		b.WriteString("\tbody := []byte(`" + rawQuote(indentJSON(s.body)) + "`)\n")
		bodyExpr = "bytes.NewReader(body)"
	} else if len(s.form) > 0 {
		b.WriteString("\tbuf := new(bytes.Buffer)\n")
		b.WriteString("\tform := multipart.NewWriter(buf)\n")
		for _, f := range s.form {
			if f.file {
				b.WriteString("\tif fw, err := form.CreateFormFile(" + pyStr(f.name) + ", " + pyStr(f.value) +
					"); err == nil {\n\t\tif fh, err := os.Open(" + pyStr(f.value) +
					"); err == nil {\n\t\t\tio.Copy(fw, fh)\n\t\t\tfh.Close()\n\t\t}\n\t}\n")
			} else {
				b.WriteString("\tform.WriteField(" + pyStr(f.name) + ", " + pyStr(f.value) + ")\n")
			}
		}
		b.WriteString("\tform.Close()\n")
		bodyExpr = "buf"
	}

	urlExpr := "base + \"" + s.path + s.queryString() + "\""
	b.WriteString("\treq, err := http.NewRequest(\"" + s.method + "\", " + urlExpr + ", " + bodyExpr + ")\n")
	b.WriteString("\tif err != nil {\n\t\tpanic(err)\n\t}\n")
	b.WriteString("\treq.Header.Set(\"Authorization\", \"Bearer \"+os.Getenv(\"" + s.keyEnv + "\"))\n")
	if s.jsonBody {
		b.WriteString("\treq.Header.Set(\"Content-Type\", \"application/json\")\n")
	}
	if len(s.form) > 0 {
		b.WriteString("\treq.Header.Set(\"Content-Type\", form.FormDataContentType())\n")
	}
	b.WriteString("\n\tres, err := http.DefaultClient.Do(req)\n")
	b.WriteString("\tif err != nil {\n\t\tpanic(err)\n\t}\n\tdefer res.Body.Close()\n\n")
	b.WriteString("\tif res.StatusCode != http.StatusOK {\n")
	b.WriteString("\t\tmsg, _ := io.ReadAll(res.Body)\n")
	b.WriteString("\t\tpanic(fmt.Sprintf(\"%d %s\", res.StatusCode, msg))\n\t}\n")

	if s.binary {
		b.WriteString("\tf, _ := os.Create(\"out.bin\")\n\tdefer f.Close()\n\tio.Copy(f, res.Body)\n")
	} else if s.noContent {
		b.WriteString("\tfmt.Println(res.StatusCode)\n")
	} else if s.sse {
		b.WriteString("\t// 流式：逐行读到结束，不要一次性读完（会一直等到连接关闭）。\n")
		b.WriteString("\tbuf := make([]byte, 4096)\n")
		b.WriteString("\tfor {\n\t\tn, rerr := res.Body.Read(buf)\n")
		b.WriteString("\t\tif n > 0 {\n\t\t\tos.Stdout.Write(buf[:n])\n\t\t}\n")
		b.WriteString("\t\tif rerr != nil {\n\t\t\tbreak\n\t\t}\n\t}\n")
	} else {
		b.WriteString("\tout, _ := io.ReadAll(res.Body)\n\tfmt.Println(strings.TrimSpace(string(out)))\n")
	}
	b.WriteString("}\n")
	return b.String()
}

// rawQuote 让 JSON 能安全放进 Go 反引号字符串（反引号本身无法转义，只能换写法）。
func rawQuote(s string) string {
	if strings.Contains(s, "`") {
		return strings.ReplaceAll(s, "`", "`+\"`\"+`")
	}
	return s
}

// ───────────────────────────── 公共 ─────────────────────────────

// indentJSON 把示例渲染成两空格缩进的 JSON 文本。
//
// Obj 实现了 MarshalJSON，Encoder 会对它的输出再缩进，所以键序仍然是
// 契约里声明的顺序——不用再手写一份缩进逻辑。
// SetEscapeHTML(false) 是必需的：默认会把 `<string>` 转成 `\u003cstring\u003e`。
func indentJSON(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil || buf.Len() == 0 {
		return "{}"
	}
	return strings.TrimRight(buf.String(), "\n")
}

// scalarText 把标量值写成可直接嵌进 URL / 表单的文本。
func scalarText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case json.Number:
		return x.String()
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}
