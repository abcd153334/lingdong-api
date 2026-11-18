package adapters

import (
	"bytes"
	"encoding/json"
	"mime"
	"mime/multipart"
	"testing"

	"lingdong-api/internal/service/billing"
)

// 本文件守 OpenAI 兼容渠道的**表单**尺寸处理与 JSON 侧同口径。
//
// 缺陷现场：`/v1/images/edits` 只写 `image_size=2K` 时，JSON 侧会把它折成 `size`，
// 表单侧却只把 image_size 删掉 —— 计费按 2K、出图按上游默认尺寸，
// 用户与运营都看不出差别（账单只记档位，不记产物尺寸）。

// TestOpenAIImageFormFoldsAliasIntoSize 表单字段映射：别名折成 size，别名本身删除。
func TestOpenAIImageFormFoldsAliasIntoSize(t *testing.T) {
	fields, err := openAIImageFormFields("gpt-image-1", &billing.ImageSize{Raw: "2K"})
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if got := fields["size"]; got != "2K" {
		t.Fatalf("size 应折叠为 2K，实际 %q", got)
	}
	for _, k := range []string{"resolution", "image_size", "aspect_ratio", "ratio"} {
		if v, ok := fields[k]; !ok || v != "" {
			t.Fatalf("别名 %q 必须显式删除，实际 %q（存在=%v）", k, v, ok)
		}
	}

	// 没声明尺寸（nil / 空 Raw）：不要凭空补一个 size。
	for _, im := range []*billing.ImageSize{nil, {}} {
		fields, err := openAIImageFormFields("gpt-image-1", im)
		if err != nil {
			t.Fatalf("不应报错：%v", err)
		}
		if _, ok := fields["size"]; ok {
			t.Fatalf("未声明尺寸时不应写 size，实际 %q", fields["size"])
		}
	}
}

// TestOpenAIImageFormFoldedSizeReachesUpstream 端到端：改写后的表单里只有 size。
func TestOpenAIImageFormFoldedSizeReachesUpstream(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("model", "gpt-image-1")
	_ = w.WriteField("prompt", "a cat")
	_ = w.WriteField("image_size", "2K")
	_ = w.Close()

	fields, _ := openAIImageFormFields("gpt-image-1", &billing.ImageSize{Raw: "2K"})
	out, ok := rewriteMultipartFields(w.FormDataContentType(), buf.Bytes(), fields)
	if !ok {
		t.Fatal("表单应被改写（有别名需要删除）")
	}

	_, params, err := mime.ParseMediaType(w.FormDataContentType())
	if err != nil {
		t.Fatalf("解析 content-type 失败：%v", err)
	}
	mr := multipart.NewReader(bytes.NewReader(out), params["boundary"])
	got := map[string]string{}
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		var b bytes.Buffer
		_, _ = b.ReadFrom(p)
		got[p.FormName()] = b.String()
	}
	if got["size"] != "2K" {
		t.Fatalf("上游应收到 size=2K，实际 %q（表单=%v）", got["size"], got)
	}
	if _, exists := got["image_size"]; exists {
		t.Fatalf("image_size 别名必须删除，实际表单=%v", got)
	}
	if got["prompt"] != "a cat" {
		t.Fatalf("其它字段必须原样保留，实际 %q", got["prompt"])
	}
}

// TestStripSizeAliasesUsesResolvedRaw 出站折叠必须与计费侧用**同一份解析结果**。
//
// 缺陷现场（本次复核发现）：计费按 size → image_size → resolution 取首个具体意图，
// 出站却按 resolution → image_size 挑。于是 `resolution=1K + image_size=2K` 变成
// 「按 2K 收钱、按 1K 出图」，而账单上只记档位、看不出交付尺寸。
func TestStripSizeAliasesUsesResolvedRaw(t *testing.T) {
	body := []byte(`{"model":"gpt-image-1","prompt":"p","resolution":"1K","image_size":"2K"}`)
	im := billing.ParseImageSize(billing.ImageDecl{Resolution: "1K", ImageSize: "2K"})
	if im.Raw != "2K" {
		t.Fatalf("计费侧应判 2K（size 缺席时 image_size 优先于 resolution），实际 %q", im.Raw)
	}
	out, err := stripPlatformSizeAliases(body, &im)
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("输出不是合法 JSON：%s", out)
	}
	if got := m["size"]; got != "2K" {
		t.Fatalf("出站 size 必须等于计费档 %q，实际 %#v", im.Raw, got)
	}
	for _, k := range []string{"resolution", "image_size", "aspect_ratio", "ratio"} {
		if _, ok := m[k]; ok {
			t.Fatalf("别名 %q 必须删除：%s", k, out)
		}
	}

	// size 已经是胜出值时逐字节保留（不重新序列化值，也不改用户写法）。
	keep := []byte(`{"model":"m","size":"1024x1024","resolution":"1K"}`)
	im2 := billing.ParseImageSize(billing.ImageDecl{Size: "1024x1024", Resolution: "1K"})
	out2, err := stripPlatformSizeAliases(keep, &im2)
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if !bytes.Contains(out2, []byte(`"size":"1024x1024"`)) {
		t.Fatalf("size 应逐字节保留，实际 %s", out2)
	}

	// 只写了 aspect_ratio（没有任何尺寸字段）：不折叠成 size，画幅不推像素。
	body3 := []byte(`{"model":"m","aspect_ratio":"16:9"}`)
	im3 := billing.ParseImageSize(billing.ImageDecl{AspectRatio: "16:9"})
	out3, err := stripPlatformSizeAliases(body3, &im3)
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if bytes.Contains(out3, []byte(`"size"`)) {
		t.Fatalf("画幅不该折成 size，实际 %s", out3)
	}
}
