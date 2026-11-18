package controller

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/assets/brandicon"
	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

// 本文件是「图标库永远加载失败」那次事故的唯一真护栏：
// 它打的是**真 handler + 真 DB + 真存储**，而不是 e2e 里的 page.route 桩件。
//
// 事故成因值得写下来：e2e 的桩件返回的是 envelope(...)，比真后端"更正确"，
// 所以后端漏了信封时 40 多个 e2e 全绿、单测也全绿（hook 被 mock），
// 只有真人点开选择器才会发现。契约测试必须绕过桩件。

const iconContractSchema = "ld_test_icon_admin"

func withIconAdminEnv(t *testing.T) {
	t.Helper()
	dsn := os.Getenv("LD_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 LD_TEST_DSN，跳过数据库集成用例")
	}
	silent := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), silent)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := admin.Exec("DROP SCHEMA IF EXISTS " + iconContractSchema + " CASCADE").Error; err != nil {
		t.Fatalf("清理 schema 失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA " + iconContractSchema).Error; err != nil {
		t.Fatalf("建 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+iconContractSchema), silent)
	if err != nil {
		t.Fatalf("连接 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.Icon{}, &model.AdminAudit{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	// 与生产同一条唯一索引：没有它，"同内容二次上传返回同一个 id"就只是内存行为。
	if err := gdb.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_icons_sha256 ON icons (sha256)`).Error; err != nil {
		t.Fatalf("建去重索引失败: %v", err)
	}

	t.Setenv("STORAGE_DIR", t.TempDir())
	conf.Load()
	prevStorage, hadStorage := conf.System().Get("storage")
	conf.System().Set("storage", []byte(`{"active":"none"}`))
	prevDB := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		if hadStorage {
			conf.System().Set("storage", prevStorage)
		}
		if sqlDB, e := gdb.DB(); e == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + iconContractSchema + " CASCADE").Error
		if sqlDB, e := admin.DB(); e == nil {
			_ = sqlDB.Close()
		}
		db.DB = prevDB
	})
}

// iconAdminEngine 只挂两条控制台路由，不带鉴权/CSRF/限流中间件。
//
// 本文件测的是**响应形状**，不是权限；把中间件也搭进来会让用例因为
// 没有会话而 401，反而测不到真正的契约。
func iconAdminEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/admin/icons", ListAdminIcons)
	r.POST("/api/admin/icons", UploadAdminIcon)
	return r
}

// envelope 是响应信封的最小形状（pkg/response.Body）。
// 这里独立声明而不是复用 response.Body：真要有人把信封改成嵌套 data，
// 本用例必须跟着红，而不是跟着一起变。
type envelope struct {
	Code      int             `json:"code"`
	Message   string          `json:"message"`
	ErrorCode string          `json:"error_code"`
	Data      json.RawMessage `json:"data"`
}

func decodeEnvelope(t *testing.T, body []byte) envelope {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("响应不是 JSON 信封: %v\n%s", err, body)
	}
	return env
}

// pngHeader 造一个「头部合法」的 PNG：签名 + 带真实宽高的 IHDR。
//
// 只填到 IHDR 就够 —— 服务端只按魔数判类型、按 IHDR 判边长，不解码像素。
// 刻意不用固定字节数组：宽高要能参数化（§2.9 的边长上限用例要造 8000×8000）。
func pngHeader(w, h uint32) []byte {
	b := &bytes.Buffer{}
	b.Write([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A})
	_ = binary.Write(b, binary.BigEndian, uint32(13)) // IHDR 长度
	b.WriteString("IHDR")
	_ = binary.Write(b, binary.BigEndian, w)
	_ = binary.Write(b, binary.BigEndian, h)
	b.Write([]byte{8, 6, 0, 0, 0}) // bitDepth=8, colorType=RGBA, 其余 0
	b.Write([]byte{0, 0, 0, 0})    // 占位 CRC（服务端不校验）
	b.WriteString("\x00\x00\x00\x00IEND")
	return b.Bytes()
}

func uploadRequest(t *testing.T, body []byte, filename string) *http.Request {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("构造 multipart 失败: %v", err)
	}
	if _, err := fw.Write(body); err != nil {
		t.Fatalf("写入请求体失败: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("关闭 multipart 失败: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/admin/icons", buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func TestListAdminIconsReturnsEnvelope(t *testing.T) {
	withIconAdminEnv(t)
	w := httptest.NewRecorder()
	iconAdminEngine().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/admin/icons", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", w.Code)
	}
	env := decodeEnvelope(t, w.Body.Bytes())
	if env.Code != 0 {
		t.Fatalf("code = %d，期望 0（缺信封时前端会把 200 判成失败）\n%s", env.Code, w.Body.Bytes())
	}
	if env.Message != "success" {
		t.Fatalf("message = %q，期望 success", env.Message)
	}

	var data struct {
		Builtin []struct {
			Key     string   `json:"key"`
			Name    string   `json:"name"`
			Group   string   `json:"group"`
			Aliases []string `json:"aliases"`
		} `json:"builtin"`
		Uploaded []struct {
			ID        string `json:"id"`
			Icon      string `json:"icon"`
			Size      int64  `json:"size"`
			CreatedAt int64  `json:"created_at"`
		} `json:"uploaded"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("data 解析失败: %v", err)
	}
	if len(data.Builtin) != brandicon.Count() {
		t.Fatalf("内置图标数 = %d，期望 %d（编译进产物的图标集与目录不一致）", len(data.Builtin), brandicon.Count())
	}
	for _, it := range data.Builtin {
		if it.Key == "" || it.Name == "" || it.Group == "" {
			t.Fatalf("内置图标存在空字段: %+v", it)
		}
		if it.Aliases == nil {
			t.Fatalf("内置图标 %s 的 aliases 是 null（前端 .length 会抛错）", it.Key)
		}
	}
	// uploaded 必须是 [] 而不是 null：库里还没有上传记录时最容易被写成 nil。
	if raw := string(env.Data); !strings.Contains(raw, `"uploaded":[]`) {
		t.Fatalf("空图库的 uploaded 必须是 []，实际 data = %s", raw)
	}
}

func TestUploadAdminIconReturnsEnvelopeAndIsIdempotent(t *testing.T) {
	withIconAdminEnv(t)
	body := pngHeader(64, 64)
	eng := iconAdminEngine()

	w := httptest.NewRecorder()
	eng.ServeHTTP(w, uploadRequest(t, body, "logo.png"))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200\n%s", w.Code, w.Body.Bytes())
	}
	env := decodeEnvelope(t, w.Body.Bytes())
	if env.Code != 0 {
		t.Fatalf("code = %d，期望 0\n%s", env.Code, w.Body.Bytes())
	}

	var up struct {
		ID        string `json:"id"`
		Icon      string `json:"icon"`
		Mime      string `json:"mime"`
		Size      int64  `json:"size"`
		CreatedAt int64  `json:"created_at"`
	}
	if err := json.Unmarshal(env.Data, &up); err != nil {
		t.Fatalf("data 解析失败: %v", err)
	}
	if up.Icon != "upload:"+up.ID {
		t.Fatalf("icon = %q，期望 upload:%s（前端靠它直接落库）", up.Icon, up.ID)
	}
	if len(up.ID) != 26 || up.ID != strings.ToLower(up.ID) {
		t.Fatalf("id = %q，期望 26 位小写 ULID（token 正则只认小写）", up.ID)
	}
	if up.Mime != "image/png" {
		t.Fatalf("mime = %q，期望 image/png", up.Mime)
	}
	if up.CreatedAt == 0 {
		t.Fatalf("created_at 为 0：前端要靠它把新图前插进「已上传」分组")
	}

	// 同内容再传：必须命中同一行（sha256 唯一），否则「已上传」分组会出现重复图。
	w2 := httptest.NewRecorder()
	eng.ServeHTTP(w2, uploadRequest(t, body, "logo-copy.png"))
	env2 := decodeEnvelope(t, w2.Body.Bytes())
	var up2 struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(env2.Data, &up2); err != nil {
		t.Fatalf("第二次上传 data 解析失败: %v", err)
	}
	if up2.ID != up.ID {
		t.Fatalf("同内容二次上传得到不同 id：%q vs %q", up2.ID, up.ID)
	}

	// 列表里必须能看到它（上传与列表是同一条读路径）。
	w3 := httptest.NewRecorder()
	eng.ServeHTTP(w3, httptest.NewRequest(http.MethodGet, "/api/admin/icons", nil))
	if raw := w3.Body.String(); !strings.Contains(raw, up.ID) {
		t.Fatalf("上传后的图标没出现在列表里\n%s", raw)
	}
}

func TestUploadAdminIconRejectsNonImage(t *testing.T) {
	withIconAdminEnv(t)
	w := httptest.NewRecorder()
	iconAdminEngine().ServeHTTP(w, uploadRequest(t, []byte("<svg/>not-an-image"), "x.svg"))

	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("状态码 = %d，期望 415\n%s", w.Code, w.Body.Bytes())
	}
	env := decodeEnvelope(t, w.Body.Bytes())
	if env.Code == 0 {
		t.Fatalf("失败响应也必须带非 0 code\n%s", w.Body.Bytes())
	}
	if env.ErrorCode == "" {
		t.Fatalf("失败响应缺 error_code（前端无法区分原因）\n%s", w.Body.Bytes())
	}
	if !strings.Contains(env.Message, "PNG") {
		t.Fatalf("message = %q，期望点名支持格式（运营要能照做）", env.Message)
	}
}
