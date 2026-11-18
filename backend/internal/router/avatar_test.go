package router_test

// 头像接口的门禁：类型/体积/形状/投影四件事，以及「删除必须 200 而不是 204」。
//
// 这些断言都对应过真实缺陷形态：
//   - 204 会被前端 parseResponse 判成失败（成功却弹错）；
//   - `avatar_url` 为 null 会让前端 `<img src="null">` 白发一次请求；
//   - 非图片不拦会把 svg（可内嵌脚本）当头像存进 users 行。

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/router"
	"lingdong-api/internal/service/session"
)

// avatarMultipart 拼一个单文件 multipart 请求体。
func avatarMultipart(t *testing.T, field, filename string, content []byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile(field, filename)
	if err != nil {
		t.Fatalf("构造 multipart 失败: %v", err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatalf("写入 multipart 失败: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("关闭 multipart 失败: %v", err)
	}
	return buf.Bytes(), w.FormDataContentType()
}

// avatarDo 发一次带会话/CSRF 的请求。
func avatarDo(t *testing.T, r *gin.Engine, method, path, contentType string, body []byte,
	cookies []*http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, "http://example.com"+path, reader)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Origin", "http://example.com")
	if csrf != "" {
		req.Header.Set(session.CSRFHeaderName, csrf)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{R: 40, G: 150, B: 220, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("构造 png 失败: %v", err)
	}
	return buf.Bytes()
}

func avatarURLOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Data struct {
			AvatarURL string `json:"avatar_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("解析响应失败: %v %s", err, rec.Body.String())
	}
	return env.Data.AvatarURL
}

func TestAvatarUploadAndClear(t *testing.T) {
	authFlowDB(t)
	authFlowRedis(t)
	setupTestCaptcha(t)
	gin.SetMode(gin.TestMode)
	r := router.New()

	seedUser(t, r, "avataruser", true)
	login := postJSON(t, r, "/api/portal/auth/login", map[string]any{
		"account": "avataruser", "password": portalTestPassword,
	}, nil, "")
	if login.Code != http.StatusOK {
		t.Fatalf("登录失败: %d %s", login.Code, login.Body.String())
	}
	sess := cookieOf(login, session.CookieName)
	csrfCookie := cookieOf(login, session.CSRFCookieName)
	if sess == nil || csrfCookie == nil {
		t.Fatal("登录必须同时种会话票与 CSRF 票")
	}
	cookies := []*http.Cookie{sess, csrfCookie}
	csrf := csrfCookie.Value

	// ① 非图片必须 415：svg 能内嵌脚本，gif 会动，两者都不收。
	svgBody, svgType := avatarMultipart(t, "file", "a.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`))
	if rec := avatarDo(t, r, http.MethodPost, "/api/portal/me/avatar", svgType, svgBody, cookies, csrf); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("svg 应 415，实际 %d %s", rec.Code, rec.Body.String())
	} else if code := decodeEnv(t, rec).ErrorCode; code != "file_type_unsupported" {
		t.Fatalf("错误码应为 file_type_unsupported，实际 %q", code)
	}

	// ② 正常 png：200，且入库的是 webp data URL。
	body, ct := avatarMultipart(t, "file", "a.png", pngBytes(t, 200, 120))
	rec := avatarDo(t, r, http.MethodPost, "/api/portal/me/avatar", ct, body, cookies, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("上传应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	saved := avatarURLOf(t, rec)
	if !strings.HasPrefix(saved, "data:image/webp;base64,") {
		t.Fatalf("入库值必须是 webp data URL，实际前缀 %q", saved[:min(len(saved), 32)])
	}

	// ③ 投影：/api/portal/me 必须回同一个值。
	me := adminGET(t, r, "/api/portal/me", sess)
	if me.Code != http.StatusOK {
		t.Fatalf("/me 应 200，实际 %d %s", me.Code, me.Body.String())
	}
	if got := avatarURLOf(t, me); got != saved {
		t.Fatalf("/me 的 avatar_url 应与上传返回值一致")
	}

	// ④ 删除：必须 200 + 空串（204 会被前端判成失败）。
	del := avatarDo(t, r, http.MethodDelete, "/api/portal/me/avatar", "", nil, cookies, csrf)
	if del.Code != http.StatusOK {
		t.Fatalf("DELETE 必须 200（204 会被前端判成失败），实际 %d %s", del.Code, del.Body.String())
	}
	if got := avatarURLOf(t, del); got != "" {
		t.Fatalf("DELETE 回调应回空串，实际 %q", got)
	}

	// ⑤ 删除后投影是空串，不是 null（null 会让前端发一次 src="null" 的请求）。
	me2 := adminGET(t, r, "/api/portal/me", sess)
	if !bytes.Contains(me2.Body.Bytes(), []byte(`"avatar_url":""`)) {
		t.Fatalf("无头像时必须下发空串，实际 %s", me2.Body.String())
	}
}

func TestAvatarRequiresSessionAndFile(t *testing.T) {
	authFlowDB(t)
	authFlowRedis(t)
	setupTestCaptcha(t)
	gin.SetMode(gin.TestMode)
	r := router.New()

	// 无会话：401（portal 组挂 UserAuth）。
	body, ct := avatarMultipart(t, "file", "a.png", pngBytes(t, 10, 10))
	if rec := avatarDo(t, r, http.MethodPost, "/api/portal/me/avatar", ct, body, nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名上传应 401，实际 %d %s", rec.Code, rec.Body.String())
	}

	seedUser(t, r, "avataruser2", true)
	login := postJSON(t, r, "/api/portal/auth/login", map[string]any{
		"account": "avataruser2", "password": portalTestPassword,
	}, nil, "")
	sess := cookieOf(login, session.CookieName)
	csrfCookie := cookieOf(login, session.CSRFCookieName)
	cookies := []*http.Cookie{sess, csrfCookie}

	// 字段名不对：400 file_missing（前端拼错字段名的第一手证据）。
	empty, ct2 := avatarMultipart(t, "avatar", "a.png", pngBytes(t, 10, 10))
	rec := avatarDo(t, r, http.MethodPost, "/api/portal/me/avatar", ct2, empty, cookies, csrfCookie.Value)
	if rec.Code != http.StatusBadRequest || decodeEnv(t, rec).ErrorCode != "file_missing" {
		t.Fatalf("缺 file 字段应 400 file_missing，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// TestUsersAvatarColumnIsNotNullWithDefault 守「可空列」这个坑：
// AutoMigrate 加列若不写 not null/default，既有行读出来是 NULL，
// 经 JSON 变成 `avatar_url: null`，前端 `<img src="null">` 白发一次请求。
func TestUsersAvatarColumnIsNotNullWithDefault(t *testing.T) {
	gdb := authFlowDB(t)
	var nullable, def string
	row := gdb.Raw(`SELECT is_nullable, coalesce(column_default,'') FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'users' AND column_name = 'avatar'`).Row()
	if err := row.Scan(&nullable, &def); err != nil {
		t.Fatalf("读取 users.avatar 列定义失败: %v", err)
	}
	if nullable != "NO" {
		t.Fatalf("users.avatar 必须 NOT NULL，实际 is_nullable=%s", nullable)
	}
	if def != "''::text" {
		t.Fatalf("users.avatar 默认值必须是空串，实际 %q", def)
	}
}
