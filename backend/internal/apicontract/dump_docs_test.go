package apicontract

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"lingdong-api/internal/pkg/errs"
)

// TestDumpContractForDocs 把 catalog + details 全量导出成 JSON。
//
// 这是**一次性的文档生成辅助**，不参与常规门禁：它只做序列化，不做断言。
// 保留它的价值在于"文档要改时不用再手抄一遍契约"——手抄必然漏字段，
// 而漏掉的字段恰恰是接入方最需要的那些（默认值、枚举、坑）。
func TestDumpContractForDocs(t *testing.T) {
	if os.Getenv("LD_DUMP_CONTRACT") == "" {
		t.Skip("设置 LD_DUMP_CONTRACT=<path> 才导出")
	}

	type pc struct {
		Name         string   `json:"name"`
		In           string   `json:"in"`
		Type         string   `json:"type"`
		Required     string   `json:"required"`
		RequiredNote string   `json:"required_note"`
		Desc         string   `json:"desc"`
		Note         string   `json:"note"`
		Default      string   `json:"default"`
		Enum         []string `json:"enum"`
		Example      any      `json:"example"`
		Constraints  []struct {
			Kind  string  `json:"kind"`
			Value float64 `json:"value"`
			Text  string  `json:"text"`
		} `json:"constraints"`
	}
	type rc struct {
		Name string   `json:"name"`
		Type string   `json:"type"`
		Desc string   `json:"desc"`
		Note string   `json:"note"`
		Enum []string `json:"enum"`
	}
	type pp struct {
		RouteLimit int `json:"route_limit"`
		MaxAge     int `json:"max_age"`
	}

	type ec struct {
		Code string `json:"code"`
		When string `json:"when"`
	}
	type ep struct {
		Key         string         `json:"key"`
		Group       string         `json:"group"`
		Title       string         `json:"title"`
		Method      string         `json:"method"`
		Path        string         `json:"path"`
		Auth        string         `json:"auth"`
		Scope       string         `json:"scope"`
		Media       string         `json:"media"`
		Idempotency string         `json:"idempotency"`
		Notes       string         `json:"notes"`
		Params      []pc           `json:"params"`
		Returns     []rc           `json:"returns"`
		NoData      bool           `json:"no_data"`
		Errors      []ec           `json:"errors"`
		ReqExample  map[string]any `json:"req_example"`
		RespExample map[string]any `json:"resp_example"`
		Sortable    []string       `json:"sortable"`
		SessionWhy  string         `json:"session_why"`
		TrialRisk   string         `json:"trial_risk"`
		Public      *pp            `json:"public,omitempty"`
	}
	var out []ep
	for _, e := range catalog {
		d := e.Doc()
		item := ep{
			Key: e.Key, Group: e.Group, Title: e.Title, Method: e.Method, Path: e.Path,
			Auth: e.Auth.String(), Scope: e.Scope, Media: string(e.Media),
			Idempotency: e.Idempotency, Notes: e.Notes,
			NoData: d.NoData, ReqExample: d.ReqExample, RespExample: d.RespExample,
			Sortable: d.Sortable, SessionWhy: string(d.SessionWhy), TrialRisk: string(d.TrialRisk),
			Params: []pc{}, Returns: []rc{}, Errors: []ec{},
		}
		if e.Public != nil {
			item.Public = &pp{RouteLimit: e.Public.RouteLimit, MaxAge: e.Public.MaxAge}
		}
		for _, p := range d.Params {
			item.Params = append(item.Params, pc{
				Name: p.Name, In: string(p.In), Type: string(p.Type),
				Required: string(p.Required), RequiredNote: p.RequiredNote,
				Desc: p.Desc, Note: p.Note, Default: p.Default,
				Enum: p.Enum, Example: p.Example,
				Constraints: func() []struct {
					Kind  string  `json:"kind"`
					Value float64 `json:"value"`
					Text  string  `json:"text"`
				} {
					out := make([]struct {
						Kind  string  `json:"kind"`
						Value float64 `json:"value"`
						Text  string  `json:"text"`
					}, 0, len(p.Constraints))
					for _, c := range p.Constraints {
						out = append(out, struct {
							Kind  string  `json:"kind"`
							Value float64 `json:"value"`
							Text  string  `json:"text"`
						}{Kind: c.Kind, Value: c.Value, Text: c.Text})
					}
					return out
				}(),
			})
		}
		for _, r := range d.Returns {
			item.Returns = append(item.Returns, rc{
				Name: r.Name, Type: string(r.Type), Desc: r.Desc, Note: r.Note, Enum: r.Enum,
			})
		}
		for _, x := range d.Errors {
			item.Errors = append(item.Errors, ec{Code: x.Code, When: x.When})
		}
		out = append(out, item)
	}

	// 不开放清单与全局错误码一起导出：参考文档的"未开放能力"与"错误码总表"
	// 必须与后端同源，手抄一遍就等着漂移。
	type nx struct {
		Method string `json:"method"`
		Path   string `json:"path"`
		Title  string `json:"title"`
		Reason string `json:"reason"`
	}
	var exposed []nx
	for _, n := range notExposed {
		exposed = append(exposed, nx{Method: n.Method, Path: n.Path, Title: n.Title, Reason: n.Reason})
	}
	type ed struct {
		Code    string `json:"code"`
		HTTP    int    `json:"http"`
		Message string `json:"message"`
	}
	var codes []ed
	for _, d := range errs.All() {
		codes = append(codes, ed{Code: d.Code, HTTP: d.HTTP, Message: d.Message})
	}

	payload := map[string]any{
		"endpoints":   out,
		"not_exposed": exposed,
		"error_codes": codes,
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	path := os.Getenv("LD_DUMP_CONTRACT")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	fmt.Printf("已导出 %d 条端点 -> %s\n", len(out), path)
}
