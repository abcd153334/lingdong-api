package apidoc

// 响应的结构性描述：**契约侧**写法，只在出厂种子里用。
//
// 它与 `docdocs` 的 `ResponseView` 是两件事：
//   - 这里描述「这个端点会返回哪几种形态」（播种时写进 `doc_response_variants`）；
//   - 那边是「库里那几行怎么下发」（投影层，独一份）。
//
// 播种之后这些结构就不再参与渲染 —— 运营改的是库，不是这里。所以这里**不提供**
// 任何「从契约重新派生响应」的函数：那会成为第二条内容来源（页面与后台各说一套）。

// ResponseKind 是这个状态码下响应体的形态。
type ResponseKind string

const (
	// ResponseJSON 是 default：JSON 对象（可能带统一封套）。
	ResponseJSON ResponseKind = "json"
	// ResponseBinary 是二进制流（图片/音频/文件的裸响应）。
	ResponseBinary ResponseKind = "binary"
	// ResponseSSE 是事件流（`text/event-stream`）。
	ResponseSSE ResponseKind = "sse"
	// ResponseNone 表示这个状态码没有响应体（如 204）。
	ResponseNone ResponseKind = "none"
)

// ResponseSpec 是一个端点的响应描述（0–2 条变体）。
//
// 逐变体的 `Kind`/`Envelope` 优先于这里的顶层值：202 常是裸任务对象，
// 200 二进制是 image/png —— 顶层只能当"新增一条变体时的默认值"。
type ResponseSpec struct {
	Kind        ResponseKind
	Envelope    bool
	ContentType string
	Variants    []ResponseVariant
	// Notes 是整篇响应的公共说明（如「所有 2xx 都走统一封套」）。
	Notes []string
}

// ResponseVariant 是一个状态码下的响应形态。
type ResponseVariant struct {
	Status int
	Label  string
	// ContentType 只在「响应体不是 JSON」时才要写。
	ContentType string
	Kind        ResponseKind
	Envelope    bool
	// Returns 是这个状态码的返回字段；空则沿用 EndpointDoc.Returns。
	Returns []Field
	// Example 是响应示例（保序对象）。
	Example Obj
	// Raw 是 SSE 等非 JSON 响应的原文。
	Raw   string
	Notes []string
}
