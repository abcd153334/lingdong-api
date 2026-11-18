package uploadcfg

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDefaultsValid 守住「出厂配置本身必须合法」。
//
// 这条最容易失守：改默认值时只想着新安装能跑，忘了 Validate 会拿同一份值校验，
// 结果全新库一保存就是 400。
func TestDefaultsValid(t *testing.T) {
	d := Defaults()
	if err := d.Validate(); err != nil {
		t.Fatalf("出厂配置未通过校验: %v", err)
	}
	if len(d.Types) != 19 {
		t.Fatalf("出厂后缀应为 19 个，实际 %d", len(d.Types))
	}
	for _, k := range Kinds {
		if d.Limits[k] <= 0 {
			t.Fatalf("类别 %s 缺出厂上限", k)
		}
	}
	// 请求体门 = 最大类别上限 + 表单开销。
	if want := (d.Limits[KindVideo] << 20) + UploadOverhead; d.RequestCap() != want {
		t.Fatalf("RequestCap 不符: got=%d want=%d", d.RequestCap(), want)
	}
}

// TestDefaultsLenientParse 缺键/坏 JSON 时用出厂值兜住，不让上传整域不可用。
func TestDefaultsLenientParse(t *testing.T) {
	var s Settings
	s.normalize()
	if s.Compress.Quality != Defaults().Compress.Quality {
		t.Fatalf("zero 值应补默认质量，实际 %d", s.Compress.Quality)
	}
	if s.Compress.Format != FormatOriginal {
		t.Fatalf("非法格式应回落 original，实际 %q", s.Compress.Format)
	}
	if len(s.Types) != 19 {
		t.Fatalf("空后缀表应补默认，实际 %d", len(s.Types))
	}
	if s.Limits[KindImage] != Defaults().Limits[KindImage] {
		t.Fatalf("缺类别上限应补默认，实际 %d", s.Limits[KindImage])
	}
}

// TestValidateRejects 逐条覆盖保存期必须挡住的手误与危险配置。
func TestValidateRejects(t *testing.T) {
	mut := func(fn func(*Settings)) Settings {
		s := Defaults()
		fn(&s)
		return s
	}
	cases := []struct {
		name string
		s    Settings
		want string
	}{
		{"空后缀表", mut(func(s *Settings) { s.Types = map[string]string{} }), "至少保留"},
		{"后缀非法字符", mut(func(s *Settings) { s.Types["p n g"] = KindImage }), "不合法"},
		{"后缀超长", mut(func(s *Settings) { s.Types["abcdefghijk"] = KindImage }), "不合法"},
		{"SVG", mut(func(s *Settings) { s.Types["svg"] = KindImage }), "不允许上传"},
		{"HTML", mut(func(s *Settings) { s.Types["html"] = KindText }), "不允许上传"},
		{"JS", mut(func(s *Settings) { s.Types["js"] = KindText }), "不允许上传"},
		{"未知类别", mut(func(s *Settings) { s.Types["bin"] = "binary" }), "类别"},
		{"缺类别上限", mut(func(s *Settings) { delete(s.Limits, KindAudio) }), "缺少 音频"},
		{"上限为 0", mut(func(s *Settings) { s.Limits[KindText] = 0 }), "文本 类上限"},
		{"上限超顶", mut(func(s *Settings) { s.Limits[KindVideo] = maxLimitMB + 1 }), "视频 类上限"},
		{"质量为 0", mut(func(s *Settings) { s.Compress.Quality = 0 }), "压缩质量"},
		{"质量为 101", mut(func(s *Settings) { s.Compress.Quality = 101 }), "压缩质量"},
		{"格式未知", mut(func(s *Settings) { s.Compress.Format = "avif" }), "输出格式"},
		{"JPEG 但未启用 jpg", mut(func(s *Settings) {
			delete(s.Types, "jpg")
			delete(s.Types, "jpeg")
			s.Compress.Format = FormatJPEG
		}), "jpg 或 jpeg"},
		{"WebP 但未启用 webp", mut(func(s *Settings) {
			delete(s.Types, "webp")
			s.Compress.Format = FormatWebP
		}), "webp 后缀"},
		{"日限越界", mut(func(s *Settings) { s.DailyLimitPerKey = 0 }), "日限"},
		{"有效期越界", mut(func(s *Settings) { s.RetentionHours = 96 }), "有效期"},
		{"在存上限越界", mut(func(s *Settings) { s.UserTotalMB = 0 }), "在存上限"},
		{"并发为 0", mut(func(s *Settings) { s.Concurrency = 0 }), "并发"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.s.Validate()
			if err == nil {
				t.Fatalf("应被拒绝，实际通过")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("文案不含 %q：%v", c.want, err)
			}
		})
	}
}

// TestMimeDerivation 守住 MIME 的派生规则（它是 /f/ 的响应头，安全边界）。
func TestMimeDerivation(t *testing.T) {
	s := Defaults()
	cases := map[string]string{
		"png":  "image/png",
		"jpg":  "image/jpeg",
		"jpeg": "image/jpeg",
		"webp": "image/webp",
		"mp3":  "audio/mpeg",
		"m4a":  "audio/mp4",
		"mov":  "video/quicktime",
		"webm": "video/webm",
		"pdf":  "application/pdf",
		"docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"csv":  "text/csv",
		"txt":  "text/plain",
	}
	for ext, want := range cases {
		if got := s.MimeFor(ext); got != want {
			t.Fatalf("%s 的 MIME 应为 %s，实际 %s", ext, want, got)
		}
	}

	// 自定义后缀：图片按 image/<ext> 推导。
	s.Types["heic"] = KindImage
	if got := s.MimeFor("heic"); got != "image/heic" {
		t.Fatalf("heic 应为 image/heic，实际 %s", got)
	}
	// 文本类必须兜底 text/plain，绝不能出现 text/<ext>（可能推导出 text/html）。
	s.Types["md"] = KindText
	if got := s.MimeFor("md"); got != "text/plain" {
		t.Fatalf("md 应兜底 text/plain，实际 %s", got)
	}
	// 未启用后缀：空串（调用方在此之前就会拒）。
	if got := s.MimeFor("exe"); got != "" {
		t.Fatalf("未启用后缀应返回空，实际 %s", got)
	}
}

// TestFamiliesPerKind 守住「类别 → 魔数族」的对应关系。
func TestFamiliesPerKind(t *testing.T) {
	cases := []struct {
		kind  string
		fam   string
		match bool
	}{
		{KindImage, "image", true},
		{KindImage, "ftyp", true}, // heic / avif
		{KindImage, "zip", false},
		{KindAudio, "audio", true},
		{KindAudio, "ftyp", true}, // m4a
		{KindVideo, "webm", true},
		{KindVideo, "audio", false},
		{KindDocument, "pdf", true},
		{KindDocument, "zip", true}, // docx / xlsx
		{KindText, "text", true},
		{KindText, "image", false},
	}
	for _, c := range cases {
		if got := FamiliesForKind(c.kind)[c.fam]; got != c.match {
			t.Fatalf("%s 是否接受 %s：want=%v got=%v", c.kind, c.fam, c.match, got)
		}
	}
}

// TestLimitsAndTypeList 守住按后缀取上限与门户下发的形状。
func TestLimitsAndTypeList(t *testing.T) {
	s := Defaults()
	s.Limits[KindImage] = 7
	if got := s.LimitMB("PNG"); got != 7 {
		t.Fatalf("大小写/点点后缀应归一，实际 %d", got)
	}
	if got := s.LimitBytes("png"); got != 7<<20 {
		t.Fatalf("字节换算不符，实际 %d", got)
	}
	if got := s.LimitMB("exe"); got != 0 {
		t.Fatalf("未启用后缀上限应为 0，实际 %d", got)
	}

	list := s.TypeList()
	if len(list) != len(s.Types) {
		t.Fatalf("TypeList 长度不符: %d", len(list))
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].Ext >= list[i].Ext {
			t.Fatalf("TypeList 必须按后缀升序: %s >= %s", list[i-1].Ext, list[i].Ext)
		}
	}
	if list[0].MaxMB <= 0 || list[0].Kind == "" {
		t.Fatalf("TypeList 行缺少上限或类别: %+v", list[0])
	}
}

// TestSniffableImage gif 必须被排除（重编码会丢动画帧）。
func TestSniffableImage(t *testing.T) {
	for _, ext := range []string{"png", "jpg", "jpeg", "webp", "bmp", "PNG"} {
		if !SniffableImage(ext) {
			t.Fatalf("%s 应可压缩", ext)
		}
	}
	for _, ext := range []string{"gif", "mp4", "pdf", "txt"} {
		if SniffableImage(ext) {
			t.Fatalf("%s 不应进入压缩链路", ext)
		}
	}
}

// TestNormalizeReplacesTypes 守住「运营删掉的后缀不会被出厂值加回来」。
//
// json.Unmarshal 对非 nil map 是**合并**语义：如果先 Defaults() 再反序列化，
// 删掉 gif 保存、回显又出现 gif、上传也仍然收 gif —— 配置看起来生效、实际没生效，
// 而这类问题只在「删」这个动作上出现（改/加都正常），极难自证。
func TestNormalizeReplacesTypes(t *testing.T) {
	s := Normalize(json.RawMessage(`{"types":{"png":"image","pdf":"document"}}`))
	if _, ok := s.Types["gif"]; ok {
		t.Fatal("未列出的后缀不应从出厂值合并回来")
	}
	if len(s.Types) != 2 {
		t.Fatalf("后缀表应替换而非合并，实际 %v", s.Types)
	}
	// 缺失的类别上限仍要补齐（否则上传会因「上限 0」被当作无限或直接被拒）。
	if s.LimitMBOfKind(KindVideo) <= 0 || s.Compress.Quality <= 0 {
		t.Fatalf("缺失字段应用出厂值补齐: %+v", s)
	}
}

// TestNormalizeFallsBackOnBadJSON 坏 JSON 不能让上传整域不可用。
func TestNormalizeFallsBackOnBadJSON(t *testing.T) {
	s := Normalize(json.RawMessage(`{"enabled":`))
	if !s.Enabled || len(s.Types) == 0 {
		t.Fatalf("坏 JSON 应回退出厂值，实际 %+v", s)
	}
}
